package gitstore

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// References are kept as git keeps them loose: one small object each, under its
// own name, holding a hash or "ref: <target>". A packed-refs object is read,
// with git's precedence — a loose reference shadows a packed one — but never
// written, except to take out a reference that is being removed.
//
// A server resolves a branch a dozen times in the course of one push, and lists
// every reference at the start of every fetch, so what reading costs is decided
// here:
//
//   - Resolving a reference is one GET. Within the freshness bound
//     (Options.IndexFreshness, the bound the object snapshots already work to on
//     how far a read may lag another replica's write) a reference that has been
//     read answers again without a request, and one written through this handle
//     is not read back at all.
//   - Listing them is ONE recursive LIST of refs/, which carries each
//     reference's version token. What was read is kept beside the version it was
//     read at, so a listing that shows the same version is the store's own word
//     that the reference has not moved: an advertisement reads only the
//     references that have, and reads those together rather than one after
//     another.
//
// None of it weakens the compare-and-set. A read made in order to compare never
// takes any of this — it reads the store — and a write through this handle
// discards what it makes stale. With the bound set to nothing, nothing is kept
// and every read is a request.

const (
	// referenceFetchWorkers bounds the concurrent reads of a listing's references.
	referenceFetchWorkers = 16
	// maxKnownReferences bounds the references remembered for one repository.
	// Past it the memory is emptied rather than trimmed: it holds nothing that
	// the next advertisement cannot fetch again.
	maxKnownReferences = 1 << 17

	headReference   = plumbing.HEAD
	packedRefsName  = "packed-refs"
	referencePrefix = "refs/"
)

// knownReference is what a read or a write of one reference found: its value,
// or that it is absent, and the version the store gave for it.
type knownReference struct {
	// ref is nil when the store holds no such key.
	ref     *plumbing.Reference
	version objstore.Version
	at      time.Time
}

// refsListing is one recursive listing of refs/. It is immutable once made.
type refsListing struct {
	at       time.Time
	names    []plumbing.ReferenceName
	versions map[plumbing.ReferenceName]objstore.Version
}

// packedReferences is a parsed packed-refs object. It is immutable once made.
type packedReferences struct {
	at      time.Time
	version objstore.Version
	raw     []byte
	ordered []*plumbing.Reference
	byName  map[plumbing.ReferenceName]*plumbing.Reference
}

// refStore reads and writes one repository's references.
type refStore struct {
	shared *storeShared
	// prefix is the repository's key prefix, ending in "/".
	prefix string

	mu      sync.Mutex
	known   map[plumbing.ReferenceName]knownReference
	listing *refsListing
	packed  *packedReferences
	// writes counts this handle's reference writes, twice each: as one begins
	// and as it ends. A read or a listing that overlapped a write may hold
	// either side of it, so it answers whoever asked and is kept for nobody else.
	writes uint64

	// flight makes concurrent reads of one reference, and concurrent listings,
	// one request.
	flight singleflight.Group
}

func newRefStore(shared *storeShared, prefix string) *refStore {
	return &refStore{shared: shared, prefix: prefix, known: map[plumbing.ReferenceName]knownReference{}}
}

// fresh reports whether something learned at the given time may still answer.
func (r *refStore) fresh(at time.Time) bool {
	freshness := r.shared.opts.IndexFreshness
	return freshness > 0 && r.shared.now().Sub(at) <= freshness
}

// resolve returns a reference by git's precedence: the loose one, else the
// packed one. With fromStore set it is a read made in order to compare, and
// asks the store for everything it answers from.
func (r *refStore) resolve(name plumbing.ReferenceName, fromStore bool) (*plumbing.Reference, error) {
	loose, err := r.loose(name, fromStore)
	if err != nil {
		return nil, err
	}
	if loose != nil {
		return loose, nil
	}
	packed, err := r.packedRefs(fromStore)
	if err != nil {
		return nil, err
	}
	if ref, ok := packed.byName[name]; ok {
		return ref, nil
	}
	return nil, plumbing.ErrReferenceNotFound
}

// loose returns the reference kept under its own key, or nil if there is none.
func (r *refStore) loose(name plumbing.ReferenceName, fromStore bool) (*plumbing.Reference, error) {
	if fromStore {
		found, err := r.fetch(name)
		return found.ref, err
	}
	if ref, answered := r.remembered(name); answered {
		return ref, nil
	}
	fetched, err, _ := r.flight.Do("ref:"+name.String(), func() (any, error) {
		r.mu.Lock()
		generation := r.writes
		r.mu.Unlock()
		found, err := r.fetch(name)
		if err != nil {
			return nil, err
		}
		r.remember(name, found, generation)
		return found, nil
	})
	if err != nil {
		return nil, err
	}
	return fetched.(knownReference).ref, nil
}

// remembered answers a plain read from what this handle already holds: a read
// or write still inside the bound, or a listing inside the bound that shows the
// reference gone, or at the version it was last read at.
func (r *refStore) remembered(name plumbing.ReferenceName) (ref *plumbing.Reference, answered bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	known, held := r.known[name]
	if held && r.fresh(known.at) {
		return known.ref, true
	}
	if r.listing == nil || !r.fresh(r.listing.at) || !strings.HasPrefix(name.String(), referencePrefix) {
		return nil, false
	}
	version, listed := r.listing.versions[name]
	if !listed {
		return nil, true
	}
	if held && known.ref != nil && known.version != "" && known.version == version {
		return known.ref, true
	}
	return nil, false
}

// remember keeps what a read found, unless a write overlapped the read.
func (r *refStore) remember(name plumbing.ReferenceName, found knownReference, generation uint64) {
	if r.shared.opts.IndexFreshness <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writes != generation {
		return
	}
	if len(r.known) >= maxKnownReferences {
		r.known = map[plumbing.ReferenceName]knownReference{}
	}
	r.known[name] = found
}

// fetch reads one reference from the store: one GET.
func (r *refStore) fetch(name plumbing.ReferenceName) (knownReference, error) {
	at := r.shared.now()
	data, info, err := r.shared.getAll(r.shared.baseContext(), r.prefix+name.String())
	if errors.Is(err, objstore.ErrNotFound) {
		return knownReference{at: at}, nil
	}
	if err != nil {
		return knownReference{}, err
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		return knownReference{}, fmt.Errorf("reference %s of %s is empty", name, strings.TrimSuffix(r.prefix, "/"))
	}
	return knownReference{ref: plumbing.NewReferenceFromStrings(name.String(), line), version: info.Version, at: at}, nil
}

// packedRefs returns the parsed packed-refs object; one that is absent parses
// as holding nothing.
func (r *refStore) packedRefs(fromStore bool) (*packedReferences, error) {
	if fromStore {
		return r.fetchPackedRefs()
	}
	r.mu.Lock()
	if packed := r.packed; packed != nil && r.fresh(packed.at) {
		r.mu.Unlock()
		return packed, nil
	}
	r.mu.Unlock()
	fetched, err, _ := r.flight.Do(packedRefsName, func() (any, error) {
		r.mu.Lock()
		generation := r.writes
		r.mu.Unlock()
		packed, err := r.fetchPackedRefs()
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		if r.writes == generation && r.shared.opts.IndexFreshness > 0 {
			r.packed = packed
		}
		r.mu.Unlock()
		return packed, nil
	})
	if err != nil {
		return nil, err
	}
	return fetched.(*packedReferences), nil
}

func (r *refStore) fetchPackedRefs() (*packedReferences, error) {
	packed := &packedReferences{at: r.shared.now(), byName: map[plumbing.ReferenceName]*plumbing.Reference{}}
	data, info, err := r.shared.getAll(r.shared.baseContext(), r.prefix+packedRefsName)
	if errors.Is(err, objstore.ErrNotFound) {
		return packed, nil
	}
	if err != nil {
		return nil, err
	}
	packed.version, packed.raw = info.Version, data
	lines := bufio.NewScanner(bytes.NewReader(data))
	for lines.Scan() {
		ref, err := parsePackedRefsLine(lines.Text())
		if err != nil {
			return nil, fmt.Errorf("%s of %s: %w", packedRefsName, strings.TrimSuffix(r.prefix, "/"), err)
		}
		if ref != nil {
			packed.ordered = append(packed.ordered, ref)
			packed.byName[ref.Name()] = ref
		}
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("%s of %s: %w", packedRefsName, strings.TrimSuffix(r.prefix, "/"), err)
	}
	return packed, nil
}

var errPackedRefsFormat = errors.New("malformed line")

// parsePackedRefsLine reads one line of git's packed-refs format. A comment, and
// the "^" line that gives the commit an annotated tag peels to, name no reference.
func parsePackedRefsLine(line string) (*plumbing.Reference, error) {
	if line == "" || line[0] == '#' || line[0] == '^' {
		return nil, nil
	}
	hash, name, found := strings.Cut(line, " ")
	if !found || strings.Contains(name, " ") {
		return nil, errPackedRefsFormat
	}
	return plumbing.NewReferenceFromStrings(name, hash), nil
}

// list returns the recursive listing of refs/. A plain read reuses one inside
// the bound; with fromStore set the store is always asked, and nothing is kept.
func (r *refStore) list(fromStore bool) (*refsListing, error) {
	if fromStore {
		return r.fetchListing()
	}
	r.mu.Lock()
	if listing := r.listing; listing != nil && r.fresh(listing.at) {
		r.mu.Unlock()
		return listing, nil
	}
	r.mu.Unlock()
	listed, err, _ := r.flight.Do("list", func() (any, error) {
		r.mu.Lock()
		generation := r.writes
		r.mu.Unlock()
		listing, err := r.fetchListing()
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		if r.writes == generation && r.shared.opts.IndexFreshness > 0 {
			r.listing = listing
		}
		r.mu.Unlock()
		return listing, nil
	})
	if err != nil {
		return nil, err
	}
	return listed.(*refsListing), nil
}

func (r *refStore) fetchListing() (*refsListing, error) {
	// Stamped before the listing starts: the bound is on how old what it
	// reports may be, and a long listing is old by the time it ends.
	listing := &refsListing{at: r.shared.now(), versions: map[plumbing.ReferenceName]objstore.Version{}}
	err := r.shared.list(r.shared.baseContext(), r.prefix+referencePrefix, func(entry objstore.Entry) {
		name := plumbing.ReferenceName(strings.TrimPrefix(entry.Key, r.prefix))
		// A key that is not a reference name this package would write is not
		// one it will serve either.
		if !name.IsSafe() {
			return
		}
		listing.names = append(listing.names, name)
		listing.versions[name] = entry.Version
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(listing.names, func(i, j int) bool { return listing.names[i] < listing.names[j] })
	return listing, nil
}

// all returns every reference: HEAD, the loose ones, and the packed ones no
// loose one shadows. It costs one listing, plus a read for each reference this
// handle does not hold at the version the listing shows — read together — plus
// at most HEAD and packed-refs.
func (r *refStore) all() ([]*plumbing.Reference, error) {
	listing, err := r.list(false)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	generation := r.writes
	found := make(map[plumbing.ReferenceName]*plumbing.Reference, len(listing.names))
	var unread []plumbing.ReferenceName
	for _, name := range listing.names {
		known, held := r.known[name]
		version := listing.versions[name]
		if held && known.ref != nil && version != "" && known.version == version && r.shared.opts.IndexFreshness > 0 {
			found[name] = known.ref
			continue
		}
		unread = append(unread, name)
	}
	r.mu.Unlock()

	fetched := make([]knownReference, len(unread))
	var reads errgroup.Group
	reads.SetLimit(referenceFetchWorkers)
	for i, name := range unread {
		reads.Go(func() error {
			var err error
			fetched[i], err = r.fetch(name)
			return err
		})
	}
	if err := reads.Wait(); err != nil {
		return nil, err
	}
	for i, name := range unread {
		// A reference removed since the listing is simply not there any more.
		if fetched[i].ref == nil {
			continue
		}
		found[name] = fetched[i].ref
		r.remember(name, fetched[i], generation)
	}

	var refs []*plumbing.Reference
	head, err := r.loose(headReference, false)
	if err != nil {
		return nil, err
	}
	if head != nil {
		refs = append(refs, head)
	}
	for _, name := range listing.names {
		if ref, ok := found[name]; ok {
			refs = append(refs, ref)
		}
	}
	packed, err := r.packedRefs(false)
	if err != nil {
		return nil, err
	}
	for _, ref := range packed.ordered {
		if _, shadowed := found[ref.Name()]; !shadowed && packed.byName[ref.Name()] == ref {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// beginWrite discards what a write to name is about to make stale. The caller
// holds the reference's lock.
func (r *refStore) beginWrite(name plumbing.ReferenceName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes++
	r.listing = nil
	delete(r.known, name)
}

// endWrite records what the write left in the store, so that the reads which
// follow a reference update — the hooks, the events, the next advertisement —
// do not go back for what this handle has just put there. It discards the
// listing again, for one that was taken while the write was in flight.
func (r *refStore) endWrite(name plumbing.ReferenceName, left knownReference, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes++
	r.listing = nil
	delete(r.known, name)
	if !failed && r.shared.opts.IndexFreshness > 0 {
		r.known[name] = left
	}
}

// set writes a reference: one PUT. The caller holds the reference's lock.
func (r *refStore) set(ref *plumbing.Reference) error {
	var content string
	switch ref.Type() {
	case plumbing.SymbolicReference:
		content = fmt.Sprintf("ref: %s\n", ref.Target())
	case plumbing.HashReference:
		content = ref.Hash().String() + "\n"
	default:
		return fmt.Errorf("reference %s has no value to store", ref.Name())
	}
	r.beginWrite(ref.Name())
	at := r.shared.now()
	version, err := r.shared.put(r.shared.baseContext(), storeWriteTimeout, r.prefix+ref.Name().String(),
		strings.NewReader(content), int64(len(content)), objstore.Always)
	r.endWrite(ref.Name(), knownReference{ref: ref, version: version, at: at}, err != nil)
	return err
}

// remove deletes a reference, loose and packed. The caller holds its lock.
func (r *refStore) remove(name plumbing.ReferenceName) error {
	r.beginWrite(name)
	at := r.shared.now()
	err := r.removeFromStore(name)
	r.endWrite(name, knownReference{at: at}, err != nil)
	return err
}

func (r *refStore) removeFromStore(name plumbing.ReferenceName) error {
	if err := r.shared.deleteObject(r.shared.baseContext(), r.prefix+name.String()); err != nil {
		return err
	}
	// Deleting the loose reference would bring a packed one of the same name
	// back from the dead, so it has to go too.
	packed, err := r.packedRefs(true)
	if err != nil {
		return err
	}
	if _, isPacked := packed.byName[name]; !isPacked {
		return nil
	}
	var kept bytes.Buffer
	dropping := false
	lines := bufio.NewScanner(bytes.NewReader(packed.raw))
	for lines.Scan() {
		line := lines.Text()
		// A "^" line belongs to the reference above it, and goes with it.
		if dropping && strings.HasPrefix(line, "^") {
			continue
		}
		ref, err := parsePackedRefsLine(line)
		if err != nil {
			return err
		}
		dropping = ref != nil && ref.Name() == name
		if !dropping {
			kept.WriteString(line + "\n")
		}
	}
	// Conditional on the version just read: another replica removing another
	// packed reference at this moment must not have its removal overwritten.
	_, err = r.shared.put(r.shared.baseContext(), storeWriteTimeout, r.prefix+packedRefsName,
		bytes.NewReader(kept.Bytes()), int64(kept.Len()), objstore.IfVersion(packed.version))
	r.mu.Lock()
	r.packed = nil
	r.mu.Unlock()
	if err != nil {
		return fmt.Errorf("remove %s from %s: %w", name, packedRefsName, err)
	}
	return nil
}
