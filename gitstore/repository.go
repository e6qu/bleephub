package gitstore

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// repository is go-git's storage.Storer implemented directly against the object
// store, with no filesystem in between: packs, their indexes and filters and the
// loose objects under git's own key names, and a manifest that says which packs
// are live and what every reference holds (manifest.go).
//
// It is one handle shared by every goroutine that touches the repository for
// the life of the process, and it needs no lock to be so. What it knows of the
// repository — the manifest, the loose tier — it holds as immutable values
// swapped whole (state.go, objectindex.go), so a reader takes a pointer and is
// done. What it reads a pack through — go-git's packfile decoder, which is not
// safe to share — it makes afresh for each call, over a pack index that is
// parsed once and only ever read after that. Writers are arbitrated by the
// store itself: every change is a conditional write of the manifest (commit.go).
type repository struct {
	shared *storeShared
	// name is the repository's full name, owner/repo; prefix is its key prefix,
	// ending in "/".
	name   string
	prefix string

	manifests *manifestStore
	commits   *committer
	loose     *looseTier
	// objectCache holds recently decoded objects, delta bases among them. It
	// locks for itself, and what it hands out is shared read-only.
	objectCache cache.Object

	// looseWrites counts objects written into the loose tier since the last
	// compaction request. Admitting one compaction at a time is the scheduler's
	// job, not this counter's.
	looseWrites atomic.Int64
	// compacting keeps this process to one compaction of the repository at a
	// time. Between replicas nothing does, and nothing needs to: see compact.go.
	compacting sync.Mutex

	modulesMu sync.Mutex
	modules   map[string]*repository
}

var (
	_ gitStorage.Storer     = (*repository)(nil)
	_ storer.PackfileWriter = (*repository)(nil)
	_ Compactor             = (*repository)(nil)
	_ PackSource            = (*repository)(nil)
	_ Addressable           = (*repository)(nil)
	_ PushTransactor        = (*repository)(nil)
)

func newRepository(shared *storeShared, name, prefix string) *repository {
	manifests := &manifestStore{shared: shared, prefix: prefix}
	return &repository{
		shared:      shared,
		name:        name,
		prefix:      prefix,
		manifests:   manifests,
		commits:     &committer{manifests: manifests},
		loose:       &looseTier{shared: shared, prefix: prefix},
		objectCache: cache.NewObjectLRUDefault(),
	}
}

// Objects.

// NewEncodedObject returns an empty object held in memory, for the caller to
// fill and hand to SetEncodedObject.
func (r *repository) NewEncodedObject() plumbing.EncodedObject { //nolint:ireturn
	return &plumbing.MemoryObject{}
}

func looseObjectKey(prefix string, hash plumbing.Hash) string {
	text := hash.String()
	return prefix + path.Join("objects", text[:2], text[2:])
}

// SetEncodedObject writes one object into the loose tier: one PUT. git writes
// to a temporary name and renames, so that the object appears whole; a PUT
// already is atomic. And it probes for the object first, to skip writing what is
// there; the key is the hash of the bytes, so writing it again stores what it
// already holds, and costs less than asking.
func (r *repository) SetEncodedObject(object plumbing.EncodedObject) (plumbing.Hash, error) {
	if object.Type() == plumbing.OFSDeltaObject || object.Type() == plumbing.REFDeltaObject {
		return plumbing.ZeroHash, plumbing.ErrInvalidType
	}
	var encoded bytes.Buffer
	writer := objfile.NewWriter(&encoded)
	if err := writer.WriteHeader(object.Type(), object.Size()); err != nil {
		return plumbing.ZeroHash, err
	}
	content, err := object.Reader()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	_, err = io.Copy(writer, content)
	closeErr := content.Close()
	if err = errors.Join(err, closeErr, writer.Close()); err != nil {
		return plumbing.ZeroHash, err
	}
	hash := writer.Hash()

	key := looseObjectKey(r.prefix, hash)
	if _, err := r.shared.put(r.shared.baseContext(), storeWriteTimeout, key, bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), objstore.Always); err != nil {
		return plumbing.ZeroHash, err
	}
	r.loose.wrote(hash)
	r.noteObjectWritten()
	return hash, nil
}

// lookup answers a question about one object. It looks through the packs of the
// manifest held — and of quarantine, the packs of a push not yet committed —
// and then the loose tier. An object that is found is returned whatever the age
// of what found it, because packs and objects never change. An object that is
// not found is believed absent only of a manifest and a loose listing inside the
// freshness bound: otherwise the one that is out of date is fetched again and
// the object looked for once more, because another replica may have published
// the pack that holds it. Evidence that something held is out of date — a pack
// the manifest names is gone, a loose key the listing showed is gone — fetches
// both again whatever their age.
func lookup[T any](r *repository, quarantine *quarantine, hash plumbing.Hash, probing bool, inPack func(*packHandle, int64) (T, error), inLoose func() (T, error)) (T, error) {
	var none T
	// No object hashes to zero, and that question is not worth a request.
	if hash.IsZero() {
		return none, plumbing.ErrObjectNotFound
	}
	state, err := r.manifests.held()
	if err != nil {
		return none, err
	}
	loose := r.loose.current.Load()
	for attempt := 0; ; attempt++ {
		answer, err := find(r, quarantine, state, loose, hash, probing, inPack, inLoose)
		outdated := errors.Is(err, errStaleSnapshot) || errors.Is(err, errLooseKeyAbsent)
		if err == nil || (!outdated && !errors.Is(err, plumbing.ErrObjectNotFound)) {
			return answer, err
		}
		staleManifest := outdated || !r.manifests.fresh(state.at)
		staleLoose := outdated || !r.loose.fresh(loose)
		if attempt > 0 || (!staleManifest && !staleLoose) {
			// A loose key that a listing taken a moment ago cannot vouch for
			// either is the loose filter's false "maybe". The sentinel goes back
			// bare, as go-git's callers compare it.
			if errors.Is(err, errStaleSnapshot) {
				return none, err
			}
			return none, plumbing.ErrObjectNotFound
		}
		if staleManifest {
			if state, err = r.manifests.revalidate(); err != nil {
				return none, err
			}
		}
		if staleLoose {
			listed, err := r.loose.refresh()
			if err != nil {
				return none, err
			}
			loose = listed.snapshot
		}
	}
}

// find is one pass of lookup over one manifest and one loose snapshot.
func find[T any](r *repository, quarantine *quarantine, state *repoState, loose *looseSnapshot, hash plumbing.Hash, probing bool, inPack func(*packHandle, int64) (T, error), inLoose func() (T, error)) (T, error) {
	var none T
	// Quarantined packs are decoded into a cache of the quarantine's own: what
	// the repository's cache holds, every reader of the repository is served.
	for _, tier := range []struct {
		packs   []*storedPack
		decoded cache.Object
	}{{quarantine.packs, quarantine.cache}, {state.packs, r.objectCache}} {
		for _, pack := range tier.packs {
			index, offset, err := pack.find(hash, probing)
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				continue
			}
			if errors.Is(err, objstore.ErrNotFound) {
				return none, fmt.Errorf("%w: %w", errStaleSnapshot, err)
			}
			if err != nil {
				return none, err
			}
			if probing {
				return none, nil
			}
			handle := openPack(pack, index, tier.decoded)
			answer, err := inPack(handle, offset)
			_ = handle.decoder.Close()
			if err != nil {
				return none, handle.failure(err)
			}
			return answer, nil
		}
	}
	if loose == nil || !loose.mayHold(hash) {
		return none, plumbing.ErrObjectNotFound
	}
	return inLoose()
}

// errLooseKeyAbsent reports that the store holds no loose object under a key the
// snapshot's filter could not rule out. From a snapshot that has been standing
// a while it most likely means a compaction elsewhere packed the object and
// deleted the key, so neither the listing nor the manifest held can say where
// the object now is, and the answer is to fetch both again; from a listing just
// taken it is the filter's rare false "maybe", and the object is not loose.
var errLooseKeyAbsent = errors.New("no loose object under the key")

// packHandle is a decoder over one pack for the length of one call.
type packHandle struct {
	pack    *storedPack
	file    *packFile
	decoder *packfile.Packfile
}

// openPack makes the per-call handle. go-git's decoder keeps a read position
// and scratch state, so it cannot be shared; the parsed index it is given can,
// and is. With no filesystem the decoder returns objects held in memory, which
// is why object reads large objects for themselves: see streamedObject.
func openPack(pack *storedPack, index *idxfile.MemoryIndex, decoded cache.Object) *packHandle {
	file := newPackFile(pack.pack, pack.name+".pack")
	return &packHandle{pack: pack, file: file, decoder: packfile.NewPackfileWithCache(index, nil, file, decoded, 0)}
}

// object reads the object at offset: as a stream if it is large and stored
// whole, and otherwise through the decoder, which resolves deltas and caches.
func (h *packHandle) object(hash plumbing.Hash, offset int64) (plumbing.EncodedObject, error) { //nolint:ireturn
	header, stream, err := streamable(h.pack, offset)
	if err != nil {
		return nil, err
	}
	if stream {
		return &streamedObject{pack: h.pack, offset: offset, hash: hash, kind: header.Type, size: header.Length}, nil
	}
	return h.decoder.Get(hash)
}

// failure reports what reading the pack came to. The decoder reports a failed
// read in its own terms, and may report it as the object being absent; what the
// store said is what the caller has to hear — above all that an outage is an
// outage, and that a pack which is gone means the manifest held is out of date.
func (h *packHandle) failure(err error) error {
	stored := h.file.failure()
	switch {
	case stored == nil:
		return err
	case errors.Is(stored, objstore.ErrNotFound):
		return fmt.Errorf("%w: %w", errStaleSnapshot, stored)
	default:
		return stored
	}
}

// EncodedObject reads one object: from the cache, else from the pack that holds
// it, else from the loose tier.
func (r *repository) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	return r.encodedObject(noQuarantine, kind, hash)
}

func (r *repository) encodedObject(quarantine *quarantine, kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	object, cached := r.objectCache.Get(hash)
	if !cached {
		object, cached = quarantine.cache.Get(hash)
	}
	if !cached {
		var err error
		object, err = lookup(r, quarantine, hash, false,
			func(handle *packHandle, offset int64) (plumbing.EncodedObject, error) {
				return handle.object(hash, offset)
			},
			func() (plumbing.EncodedObject, error) { return r.looseObject(hash) })
		if err != nil {
			return nil, err
		}
	}
	if kind != plumbing.AnyObject && object.Type() != kind {
		return nil, plumbing.ErrObjectNotFound
	}
	return object, nil
}

// looseObject reads an object the snapshot's filter could not rule out of the
// loose tier, or reports errLooseKeyAbsent.
func (r *repository) looseObject(hash plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	data, _, err := r.shared.getAll(r.shared.baseContext(), looseObjectKey(r.prefix, hash))
	if errors.Is(err, objstore.ErrNotFound) {
		return nil, errLooseKeyAbsent
	}
	if err != nil {
		return nil, err
	}
	reader, err := objfile.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("loose object %s: %w", hash, err)
	}
	defer func() { _ = reader.Close() }()
	kind, size, err := reader.Header()
	if err != nil {
		return nil, fmt.Errorf("loose object %s: %w", hash, err)
	}
	object := &plumbing.MemoryObject{}
	object.SetType(kind)
	object.SetSize(size)
	writer, err := object.Writer()
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(writer, reader); err != nil {
		return nil, fmt.Errorf("loose object %s: %w", hash, err)
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if object.Hash() != hash {
		return nil, fmt.Errorf("loose object %s: its content hashes to %s", hash, object.Hash())
	}
	r.objectCache.Put(object)
	return object, nil
}

// HasEncodedObject answers a fetch negotiation's question — do you have this? —
// which for most of what it is asked about is no. The filters answer that from
// memory: a pack's index is loaded, and the store asked, only for an object the
// filters cannot rule out.
func (r *repository) HasEncodedObject(hash plumbing.Hash) error {
	return r.hasEncodedObject(noQuarantine, hash)
}

func (r *repository) hasEncodedObject(quarantine *quarantine, hash plumbing.Hash) error {
	if _, cached := r.objectCache.Get(hash); cached {
		return nil
	}
	_, err := lookup(r, quarantine, hash, true, nil, func() (struct{}, error) {
		_, err := r.shared.head(r.shared.baseContext(), looseObjectKey(r.prefix, hash))
		if errors.Is(err, objstore.ErrNotFound) {
			return struct{}{}, errLooseKeyAbsent
		}
		return struct{}{}, err
	})
	return err
}

// EncodedObjectSize reports an object's inflated size. From a pack it reads the
// object's header, and for a delta the head of the delta, not the object.
func (r *repository) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	return r.encodedObjectSize(noQuarantine, hash)
}

func (r *repository) encodedObjectSize(quarantine *quarantine, hash plumbing.Hash) (int64, error) {
	if object, cached := r.objectCache.Get(hash); cached {
		return object.Size(), nil
	}
	return lookup(r, quarantine, hash, false,
		func(handle *packHandle, offset int64) (int64, error) { return handle.decoder.GetSizeByOffset(offset) },
		func() (int64, error) {
			object, err := r.looseObject(hash)
			if err != nil {
				return 0, err
			}
			return object.Size(), nil
		})
}

// IterEncodedObjects walks every object of a type, pack by pack and then the
// loose tier, each object once however many places hold it. It starts from a
// manifest the store has just vouched for and a listing of its own, since only
// a listing names the loose objects, and reads lazily, so walking a large
// repository never holds it in memory.
func (r *repository) IterEncodedObjects(kind plumbing.ObjectType) (storer.EncodedObjectIter, error) { //nolint:ireturn
	state, err := r.manifests.revalidate()
	if err != nil {
		return nil, err
	}
	listed, err := r.loose.refresh()
	if err != nil {
		return nil, err
	}
	loose := make([]plumbing.Hash, 0, len(listed.listing.loose))
	for _, object := range listed.listing.loose {
		loose = append(loose, object.hash)
	}
	return &objectWalk{repository: r, kind: kind, packs: state.packs, loose: loose, seen: map[plumbing.Hash]struct{}{}}, nil
}

// objectWalk is the iterator IterEncodedObjects returns. Like every go-git
// iterator it belongs to the goroutine that asked for it.
type objectWalk struct {
	repository *repository
	kind       plumbing.ObjectType
	packs      []*storedPack
	loose      []plumbing.Hash
	seen       map[plumbing.Hash]struct{}

	handle  *packHandle
	current storer.EncodedObjectIter
}

func (w *objectWalk) Next() (plumbing.EncodedObject, error) { //nolint:ireturn
	for {
		object, err := w.next()
		if err != nil {
			return nil, err
		}
		if _, duplicate := w.seen[object.Hash()]; duplicate {
			continue
		}
		w.seen[object.Hash()] = struct{}{}
		return object, nil
	}
}

func (w *objectWalk) next() (plumbing.EncodedObject, error) { //nolint:ireturn
	for {
		if w.current != nil {
			object, err := w.current.Next()
			if err == nil {
				return object, nil
			}
			if !errors.Is(err, io.EOF) {
				return nil, w.handle.failure(err)
			}
			w.closePack()
		}
		if len(w.packs) > 0 {
			pack := w.packs[0]
			w.packs = w.packs[1:]
			index, err := pack.loadIndex()
			if err != nil {
				return nil, err
			}
			w.handle = openPack(pack, index, w.repository.objectCache)
			w.current, err = w.handle.decoder.GetByType(w.kind)
			if err != nil {
				w.closePack()
				return nil, err
			}
			continue
		}
		for len(w.loose) > 0 {
			hash := w.loose[0]
			w.loose = w.loose[1:]
			// A pack has already supplied it: no need to fetch the copy.
			if _, met := w.seen[hash]; met {
				continue
			}
			object, err := w.repository.looseObject(hash)
			if errors.Is(err, errLooseKeyAbsent) {
				// Packed and deleted since the listing: the walk of the packs
				// either met it or the pack is newer than this walk.
				continue
			}
			if err != nil {
				return nil, err
			}
			if w.kind == plumbing.AnyObject || object.Type() == w.kind {
				return object, nil
			}
		}
		return nil, io.EOF
	}
}

func (w *objectWalk) closePack() {
	if w.current != nil {
		w.current.Close()
	}
	if w.handle != nil {
		_ = w.handle.decoder.Close()
	}
	w.current, w.handle = nil, nil
}

// ForEach mirrors go-git's storer.ForEachIterator contract.
func (w *objectWalk) ForEach(visit func(plumbing.EncodedObject) error) error {
	defer w.Close()
	for {
		object, err := w.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := visit(object); err != nil {
			if errors.Is(err, storer.ErrStop) {
				return nil
			}
			return err
		}
	}
}

func (w *objectWalk) Close() {
	w.closePack()
	w.packs, w.loose = nil, nil
}

// AddAlternate is not offered: an alternate is a path to another object
// directory on the same disk, and there is no disk.
func (r *repository) AddAlternate(string) error {
	return errors.New("gitstore: an object-store repository cannot have alternates")
}

// References. They are read from the manifest and written by committing to it:
// see manifest.go and commit.go. Nothing here takes a lock. A reference update
// that expects an old value is a mutation that refuses when the draft holds
// another, and the conditional write of the manifest is what makes the
// comparison and the write one step, between goroutines and replicas alike.

// Reference resolves one reference from a manifest inside the freshness bound.
func (r *repository) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if err := checkSafeRefName(name); err != nil {
		return nil, err
	}
	state, err := r.manifests.recent()
	if err != nil {
		return nil, err
	}
	return state.reference(name)
}

// IterReferences lists every reference, as each fetch and push begins by doing:
// from the manifest and the snapshot it names, which a handle that has read
// them holds, and never from a listing. The references are gathered before the
// iterator is returned, so the walk holds nothing and the caller may read and
// write the repository from inside it.
func (r *repository) IterReferences() (storer.ReferenceIter, error) { //nolint:ireturn
	state, err := r.manifests.recent()
	if err != nil {
		return nil, err
	}
	return storer.NewReferenceSliceIter(state.references()), nil
}

// SetReference writes a reference whatever it held before.
func (r *repository) SetReference(ref *plumbing.Reference) error {
	_, err := r.commits.commit(func(d *draft) error { return d.set(ref) })
	return err
}

// CheckAndSetReference moves a reference only if it still holds old: the
// compare-and-set, compared as go-git compares it.
func (r *repository) CheckAndSetReference(next, old *plumbing.Reference) error {
	if err := checkSafeRefName(next.Name()); err != nil {
		return err
	}
	_, err := r.commits.commit(func(d *draft) error {
		if old != nil {
			current := d.reference(old.Name())
			if current == nil {
				return plumbing.ErrReferenceNotFound
			}
			if current.Hash() != old.Hash() {
				return gitStorage.ErrReferenceHasChanged
			}
		}
		return d.set(next)
	})
	return err
}

// CreateReference writes a reference only if there is none of that name.
func (r *repository) CreateReference(ref *plumbing.Reference) error {
	if err := checkSafeRefName(ref.Name()); err != nil {
		return err
	}
	_, err := r.commits.commit(func(d *draft) error {
		if d.reference(ref.Name()) != nil {
			return ErrReferenceAlreadyExists
		}
		return d.set(ref)
	})
	return err
}

// RemoveReference removes a reference whatever it held. Removing one that is
// not there is not an error.
func (r *repository) RemoveReference(name plumbing.ReferenceName) error {
	if err := checkSafeRefName(name); err != nil {
		return err
	}
	_, err := r.commits.commit(func(d *draft) error {
		d.remove(name)
		return nil
	})
	return err
}

// RemoveReferenceCAS removes a reference only if it still holds old.
func (r *repository) RemoveReferenceCAS(old *plumbing.Reference) error {
	if err := checkSafeRefName(old.Name()); err != nil {
		return err
	}
	_, err := r.commits.commit(func(d *draft) error {
		current := d.reference(old.Name())
		if current == nil {
			return plumbing.ErrReferenceNotFound
		}
		if current.Type() != old.Type() || current.String() != old.String() {
			return gitStorage.ErrReferenceHasChanged
		}
		d.remove(old.Name())
		return nil
	})
	return err
}

// InitializeRepositoryReferences creates a repository's first branch and points
// HEAD at it, in one commit: no reader finds the branch without HEAD or HEAD
// without the branch, and no other initialization can interleave with it.
func (r *repository) InitializeRepositoryReferences(branch *plumbing.Reference, requireEmpty bool) error {
	if err := checkSafeRefName(branch.Name()); err != nil {
		return err
	}
	_, err := r.commits.commit(func(d *draft) error {
		alreadyInitialized := false
		for name := range d.references() {
			if name.IsBranch() {
				if name == branch.Name() {
					return ErrReferenceAlreadyExists
				}
				alreadyInitialized = true
			}
		}
		if requireEmpty && alreadyInitialized {
			return ErrReferenceAlreadyExists
		}
		if !alreadyInitialized {
			if err := d.set(plumbing.NewSymbolicReference(plumbing.HEAD, branch.Name())); err != nil {
				return err
			}
		}
		return d.set(branch)
	})
	return err
}

// defaultBranch is the branch HEAD names in a repository that has just been
// created, which is go-git's own choice for one.
const defaultBranch = plumbing.Master

// create makes the repository exist: it commits a manifest whose HEAD names a
// branch yet to be born, which is what an empty git repository is. A repository
// that exists already is left exactly as it is — and one the handle holds a
// manifest of exists, however old the manifest, so nothing is asked about it.
func (r *repository) create() error {
	state, err := r.manifests.held()
	if err != nil {
		return err
	}
	if _, err := state.reference(plumbing.HEAD); err == nil {
		return nil
	}
	_, err = r.commits.commit(func(d *draft) error {
		if d.reference(plumbing.HEAD) != nil {
			return nil
		}
		return d.set(plumbing.NewSymbolicReference(plumbing.HEAD, defaultBranch))
	})
	return err
}

// CountLooseRefs reports no loose references: a reference here is an entry in
// the manifest, not an object of its own, and there is nothing to pack.
func (r *repository) CountLooseRefs() (int, error) { return 0, nil }

// PackRefs does nothing, as go-git's in-memory storage does nothing: folding
// the manifest's reference changes into a snapshot is a commit's business.
func (r *repository) PackRefs() error { return nil }

// The small files: config, index, shallow.

// small reads one of the repository's small files, reporting whether it exists.
func (r *repository) small(name string) (data []byte, exists bool, err error) {
	data, _, err = r.shared.getAll(r.shared.baseContext(), r.prefix+name)
	if errors.Is(err, objstore.ErrNotFound) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (r *repository) setSmall(name string, data []byte) error {
	_, err := r.shared.put(r.shared.baseContext(), storeWriteTimeout, r.prefix+name, bytes.NewReader(data), int64(len(data)), objstore.Always)
	return err
}

// Config reads the repository's config. A repository without one has the
// default for a bare repository, which is the only kind a bucket can hold:
// there is nowhere in it for a work tree.
func (r *repository) Config() (*config.Config, error) {
	data, exists, err := r.small("config")
	if err != nil {
		return nil, err
	}
	if !exists {
		bare := config.NewConfig()
		bare.Core.IsBare = true
		return bare, nil
	}
	return config.ReadConfig(bytes.NewReader(data))
}

// SetConfig validates and writes the repository's config.
func (r *repository) SetConfig(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	return r.setSmall("config", data)
}

// Index reads the staging index; a bare repository has an empty one.
func (r *repository) Index() (*index.Index, error) {
	decoded := &index.Index{Version: 2}
	data, exists, err := r.small("index")
	if err != nil || !exists {
		return decoded, err
	}
	if err := index.NewDecoder(bufio.NewReader(bytes.NewReader(data))).Decode(decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// SetIndex writes the staging index.
func (r *repository) SetIndex(idx *index.Index) error {
	var encoded bytes.Buffer
	if err := index.NewEncoder(&encoded).Encode(idx); err != nil {
		return err
	}
	return r.setSmall("index", encoded.Bytes())
}

// Shallow reads the commits at which the repository's history was cut short.
func (r *repository) Shallow() ([]plumbing.Hash, error) {
	data, exists, err := r.small("shallow")
	if err != nil || !exists {
		return nil, err
	}
	var commits []plumbing.Hash
	for _, line := range strings.Fields(string(data)) {
		commits = append(commits, plumbing.NewHash(line))
	}
	return commits, nil
}

// SetShallow writes the commits at which the repository's history is cut short.
func (r *repository) SetShallow(commits []plumbing.Hash) error {
	var encoded strings.Builder
	for _, commit := range commits {
		encoded.WriteString(commit.String() + "\n")
	}
	return r.setSmall("shallow", []byte(encoded.String()))
}

// Module returns a submodule's repository, kept under modules/<name>/ as git
// keeps it. It is memoized for the reason Store.Repository is: a handle owns
// its snapshots.
func (r *repository) Module(name string) (gitStorage.Storer, error) { //nolint:ireturn
	if name == "" || path.Clean(name) != name || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
		return nil, fmt.Errorf("invalid submodule name %q", name)
	}
	r.modulesMu.Lock()
	defer r.modulesMu.Unlock()
	if module, ok := r.modules[name]; ok {
		return module, nil
	}
	if r.modules == nil {
		r.modules = map[string]*repository{}
	}
	module := newRepository(r.shared, r.name+"/"+name, r.prefix+path.Join("modules", name)+"/")
	r.modules[name] = module
	return module, nil
}
