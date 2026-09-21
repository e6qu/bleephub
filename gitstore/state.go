package gitstore

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sync/singleflight"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// What a handle knows of its repository is one immutable value, repoState: the
// manifest as the store last gave it, the version it was at, the reference
// snapshot it names, and the live packs as this process reads them. A reader
// takes the pointer and works from a consistent picture with no lock; whatever
// learns of a newer manifest — a revalidation, a commit of this handle's own —
// builds a new state and swaps it in whole.
//
// A state answers reads of references for Options.IndexFreshness after the store
// last vouched for it. Past that, a reader revalidates with a conditional read:
// one small request, which the store usually answers "not modified". An object
// found in one of the state's packs is returned whatever the state's age, since
// a pack never changes; only a miss, which another replica's push could have
// turned into a hit, is checked against a revalidated manifest before it is
// believed. A write reads nothing first: see commit.go.

// repoState is one manifest as a handle holds it. It is never modified.
type repoState struct {
	// at is when the read that last vouched for the manifest began.
	at time.Time
	// version is the manifest object's, or empty when the store holds none: the
	// repository has not been created, and the first commit creates it.
	version  objstore.Version
	manifest *manifest
	// base is the parsed snapshot the manifest names, nil when it names none.
	base *refSnapshot
	// changes indexes the manifest's change list by name.
	changes map[plumbing.ReferenceName]refChange
	// packs are the live packs, largest first, since that is where an object
	// most likely is.
	packs []*storedPack
}

// exists reports whether the store holds a manifest for the repository.
func (s *repoState) exists() bool { return s.version != "" }

// reference resolves one reference: the change list shadows the snapshot.
func (s *repoState) reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if change, changed := s.changes[name]; changed {
		ref, err := change.reference()
		if err != nil {
			return nil, err
		}
		if ref == nil {
			return nil, plumbing.ErrReferenceNotFound
		}
		return ref, nil
	}
	if ref := s.base.lookup(name); ref != nil {
		return ref, nil
	}
	return nil, plumbing.ErrReferenceNotFound
}

// references returns every reference, HEAD first and the rest in name order,
// which is the order an advertisement wants them in.
func (s *repoState) references() []*plumbing.Reference {
	merged := mergedReferences(s.base, s.manifest.Refs.Changes)
	refs := make([]*plumbing.Reference, 0, len(merged))
	for _, ref := range merged {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if first, second := refs[i].Name() == plumbing.HEAD, refs[j].Name() == plumbing.HEAD; first != second {
			return first
		}
		return refs[i].Name() < refs[j].Name()
	})
	return refs
}

// pack returns the live pack of that name, or nil.
func (s *repoState) pack(name string) *storedPack {
	for _, pack := range s.packs {
		if pack.name == name {
			return pack
		}
	}
	return nil
}

// manifestStore reads, holds and revalidates one repository's manifest.
type manifestStore struct {
	shared *storeShared
	// prefix is the repository's key prefix, ending in "/".
	prefix string

	current atomic.Pointer[repoState]
	// flight makes concurrent loads and revalidations one request, which is what
	// keeps the first clones to reach a replica that has just started from each
	// fetching the manifest for themselves.
	flight singleflight.Group

	// published holds packs this process uploaded, by name, until a manifest that
	// names them is built: their indexes and filters are already parsed, and a
	// state built from the manifest alone would read them back from the store.
	publishedMu sync.Mutex
	published   map[string]*storedPack
}

func (m *manifestStore) key() string { return m.prefix + manifestName }

// fresh reports whether something the store vouched for at the given time may
// still answer without asking again.
func (m *manifestStore) fresh(at time.Time) bool {
	freshness := m.shared.opts.IndexFreshness
	return freshness > 0 && m.shared.now().Sub(at) <= freshness
}

// held returns the state the handle holds, reading the manifest if it holds
// none. It is for reads that stay right however old the state is.
func (m *manifestStore) held() (*repoState, error) {
	if current := m.current.Load(); current != nil {
		return current, nil
	}
	return m.revalidate()
}

// recent returns a state the store vouched for inside the freshness bound,
// revalidating the one held if it is older. It is what reads of references use.
func (m *manifestStore) recent() (*repoState, error) {
	if current := m.current.Load(); current != nil && m.fresh(current.at) {
		return current, nil
	}
	return m.revalidate()
}

// revalidate asks the store for the manifest: conditionally when one is held,
// so that the usual answer costs no body, and whole otherwise.
func (m *manifestStore) revalidate() (*repoState, error) {
	loaded, err, _ := m.flight.Do("manifest", func() (any, error) {
		started := m.current.Load()
		state, err := m.read(started)
		if err != nil {
			return nil, err
		}
		// Only if nothing newer arrived meanwhile: a commit of this handle's own
		// publishes what it wrote, and a read that began before it must not put
		// the older manifest back.
		if !m.current.CompareAndSwap(started, state) {
			return m.current.Load(), nil
		}
		return state, nil
	})
	if err != nil {
		return nil, err
	}
	return loaded.(*repoState), nil
}

// read fetches the manifest, given the state held (or nil), and returns the
// state that results. It publishes nothing. A manifest whose snapshot has gone
// was replaced while it was being read — a commit folded the references into a
// new snapshot and a sweep removed the old one — so it is read again, once.
func (m *manifestStore) read(held *repoState) (*repoState, error) {
	state, err := m.readOnce(held)
	if errors.Is(err, errSnapshotGone) {
		return m.readOnce(nil)
	}
	return state, err
}

func (m *manifestStore) readOnce(held *repoState) (*repoState, error) {
	// Stamped before the request: the bound is on how old the answer may be.
	at := m.shared.now()
	ctx := m.shared.baseContext()
	var data []byte
	var info objstore.Info
	var err error
	if held != nil && held.exists() {
		data, info, err = m.shared.getAllIfChanged(ctx, m.key(), held.version)
		if errors.Is(err, objstore.ErrNotModified) {
			vouched := *held
			vouched.at = at
			return &vouched, nil
		}
	} else {
		data, info, err = m.shared.getAll(ctx, m.key())
	}
	if errors.Is(err, objstore.ErrNotFound) {
		return &repoState{at: at, manifest: &manifest{Format: manifestFormat}}, nil
	}
	if err != nil {
		return nil, err
	}
	decoded, err := decodeManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.key(), err)
	}
	var base *refSnapshot
	if held != nil {
		base = held.base
	}
	return m.build(decoded, info.Version, at, base, held)
}

// build makes the state for a manifest. known is a snapshot already parsed,
// used if it is the one the manifest names; previous is the state being
// replaced, whose packs are carried over with whatever has been read of them.
func (m *manifestStore) build(decoded *manifest, version objstore.Version, at time.Time, known *refSnapshot, previous *repoState) (*repoState, error) {
	state := &repoState{at: at, version: version, manifest: decoded, changes: make(map[plumbing.ReferenceName]refChange, len(decoded.Refs.Changes))}
	for _, change := range decoded.Refs.Changes {
		state.changes[plumbing.ReferenceName(change.Name)] = change
	}
	switch {
	case decoded.Refs.Snapshot == "":
	case known != nil && known.key == decoded.Refs.Snapshot:
		state.base = known
	default:
		base, err := m.readSnapshot(decoded.Refs.Snapshot)
		if err != nil {
			return nil, err
		}
		state.base = base
	}

	m.publishedMu.Lock()
	defer m.publishedMu.Unlock()
	for _, pack := range decoded.Packs {
		var stored *storedPack
		if previous != nil {
			stored = previous.pack(pack.Name)
		}
		if stored == nil {
			stored = m.published[pack.Name]
		}
		if stored == nil {
			stored = m.describe(pack)
		}
		delete(m.published, pack.Name)
		state.packs = append(state.packs, stored)
	}
	sortPacks(state.packs)
	return state, nil
}

// errSnapshotGone reports that the snapshot a manifest names is not in the
// store: the manifest is not the newest, and a sweep has since removed what the
// newest no longer names.
var errSnapshotGone = errors.New("the reference snapshot the manifest names is gone")

func (m *manifestStore) readSnapshot(key string) (*refSnapshot, error) {
	data, _, err := m.shared.getAll(m.shared.baseContext(), m.prefix+key)
	if errors.Is(err, objstore.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", m.prefix+key, errSnapshotGone)
	}
	if err != nil {
		return nil, err
	}
	return decodeRefSnapshot(key, data)
}

// describe addresses a pack from what the manifest says of it.
func (m *manifestStore) describe(pack manifestPack) *storedPack {
	return &storedPack{
		name:          pack.Name,
		objects:       pack.Objects,
		pack:          m.extents(pack.Name+".pack", pack.Bytes),
		index:         m.extents(pack.Name+".idx", pack.IndexBytes),
		filtered:      pack.FilterBytes > 0,
		filterExtents: m.extents(pack.Name+".bfilter", pack.FilterBytes),
	}
}

// extents addresses one artefact of the pack directory.
func (m *manifestStore) extents(name string, size int64) packExtents {
	return packExtents{shared: m.shared, key: m.prefix + path.Join("objects", "pack", name), size: size}
}

// offer keeps a pack this process has just uploaded for the state that will
// name it; withdraw forgets one whose commit was refused.
func (m *manifestStore) offer(pack *storedPack) {
	m.publishedMu.Lock()
	defer m.publishedMu.Unlock()
	if m.published == nil {
		m.published = map[string]*storedPack{}
	}
	m.published[pack.name] = pack
}

func (m *manifestStore) withdraw(name string) {
	m.publishedMu.Lock()
	defer m.publishedMu.Unlock()
	delete(m.published, name)
}
