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
// store: git's own layout, one object-store object per git file, with no
// filesystem in between.
//
// It is one handle shared by every goroutine that touches the repository for
// the life of the process, and it needs no lock to be so. What it knows of the
// repository — the live packs, the loose tier, the references — it holds as
// immutable snapshots swapped whole (objectindex.go, refs.go), so a reader takes
// a pointer and is done. What it reads a pack through — go-git's packfile
// decoder, which is not safe to share — it makes afresh for each call, over a
// pack index that is parsed once and only ever read after that. The one thing
// that needs arbitration between writers, moving a reference, takes the
// reference's lock across goroutines and replicas (locks.go).
type repository struct {
	shared *storeShared
	// name is the repository's full name, owner/repo, which lock names derive
	// from; prefix is its key prefix, ending in "/".
	name   string
	prefix string

	tiers *objectTiers
	refs  *refStore
	// objectCache holds recently decoded objects, delta bases among them. It
	// locks for itself, and what it hands out is shared read-only.
	objectCache cache.Object

	// looseWrites counts objects written into the loose tier since the last
	// compaction request. Admitting one compaction at a time is the scheduler's
	// job, not this counter's.
	looseWrites atomic.Int64

	modulesMu sync.Mutex
	modules   map[string]*repository
}

var (
	_ gitStorage.Storer     = (*repository)(nil)
	_ storer.PackfileWriter = (*repository)(nil)
	_ Compactor             = (*repository)(nil)
	_ PackSource            = (*repository)(nil)
	_ Addressable           = (*repository)(nil)
)

func newRepository(shared *storeShared, name, prefix string) *repository {
	return &repository{
		shared:      shared,
		name:        name,
		prefix:      prefix,
		tiers:       &objectTiers{shared: shared, prefix: prefix},
		refs:        newRefStore(shared, prefix),
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
	r.tiers.apply(tierChange{loose: hash})
	r.noteObjectWritten()
	return hash, nil
}

// lookup answers a question about one object from the current snapshot, and
// looks again after a fresh listing before it believes the object is absent:
// another replica may have published the pack that holds it. An answer of
// "absent" from a snapshot inside the freshness bound stands. Evidence that the
// snapshot is out of date — it named something the store no longer holds —
// lists again whatever the snapshot's age.
func lookup[T any](r *repository, hash plumbing.Hash, find func(*tierSnapshot) (T, error)) (T, error) {
	var none T
	// No object hashes to zero, and that question is not worth a request.
	if hash.IsZero() {
		return none, plumbing.ErrObjectNotFound
	}
	if snapshot := r.tiers.current.Load(); snapshot != nil {
		found, err := find(snapshot)
		outdated := errors.Is(err, errStaleSnapshot) || errors.Is(err, errLooseKeyAbsent)
		if err == nil || (!outdated && !errors.Is(err, plumbing.ErrObjectNotFound)) {
			return found, err
		}
		if !outdated && r.tiers.fresh(snapshot) {
			return none, plumbing.ErrObjectNotFound
		}
	}
	listed, err := r.tiers.refresh()
	if err != nil {
		return none, err
	}
	found, err := find(listed.snapshot)
	// A loose key that a listing taken a moment ago cannot vouch for either is
	// the loose filter's false "maybe": the packs have been looked through and
	// the store has been asked, and the object is in neither. The sentinel goes
	// back bare, as go-git's callers compare it.
	if errors.Is(err, plumbing.ErrObjectNotFound) || errors.Is(err, errLooseKeyAbsent) {
		return none, plumbing.ErrObjectNotFound
	}
	return found, err
}

// errLooseKeyAbsent reports that the store holds no loose object under a key the
// snapshot's filter could not rule out. From a snapshot that has been standing
// a while it most likely means a compaction elsewhere packed the object and
// deleted the key, so the snapshot cannot say where the object now is, and the
// answer is to list again; from a listing just taken it is the filter's rare
// false "maybe", and the object is not loose.
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
func (r *repository) openPack(pack *storedPack, index *idxfile.MemoryIndex) *packHandle {
	file := newPackFile(pack.pack, pack.name+".pack")
	return &packHandle{pack: pack, file: file, decoder: packfile.NewPackfileWithCache(index, nil, file, r.objectCache, 0)}
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
// outage, and that a pack which is gone means the snapshot is out of date.
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

// inPacks finds the object in the snapshot's packs and runs read on the pack
// that holds it. With no read to run it is a probe, which is answered from the
// packs' filters where it can be: see storedPack.find.
func inPacks[T any](r *repository, snapshot *tierSnapshot, hash plumbing.Hash, read func(*packHandle, int64) (T, error)) (T, error) {
	var none T
	for _, pack := range snapshot.packs {
		index, offset, err := pack.find(hash, read == nil)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			continue
		}
		if errors.Is(err, objstore.ErrNotFound) {
			return none, fmt.Errorf("%w: %w", errStaleSnapshot, err)
		}
		if err != nil {
			return none, err
		}
		if read == nil {
			return none, nil
		}
		handle := r.openPack(pack, index)
		found, err := read(handle, offset)
		_ = handle.decoder.Close()
		if err != nil {
			return none, handle.failure(err)
		}
		return found, nil
	}
	return none, plumbing.ErrObjectNotFound
}

// EncodedObject reads one object: from the cache, else from the pack that holds
// it, else from the loose tier.
func (r *repository) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	object, cached := r.objectCache.Get(hash)
	if !cached {
		var err error
		object, err = lookup(r, hash, func(snapshot *tierSnapshot) (plumbing.EncodedObject, error) {
			found, err := inPacks(r, snapshot, hash, func(handle *packHandle, offset int64) (plumbing.EncodedObject, error) {
				return handle.object(hash, offset)
			})
			if !errors.Is(err, plumbing.ErrObjectNotFound) {
				return found, err
			}
			if !snapshot.looseMayHold(hash) {
				return nil, plumbing.ErrObjectNotFound
			}
			return r.looseObject(hash)
		})
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
	if _, cached := r.objectCache.Get(hash); cached {
		return nil
	}
	_, err := lookup(r, hash, func(snapshot *tierSnapshot) (struct{}, error) {
		_, err := inPacks[struct{}](r, snapshot, hash, nil)
		if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return struct{}{}, err
		}
		if !snapshot.looseMayHold(hash) {
			return struct{}{}, plumbing.ErrObjectNotFound
		}
		_, err = r.shared.head(r.shared.baseContext(), looseObjectKey(r.prefix, hash))
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
	if object, cached := r.objectCache.Get(hash); cached {
		return object.Size(), nil
	}
	return lookup(r, hash, func(snapshot *tierSnapshot) (int64, error) {
		size, err := inPacks(r, snapshot, hash, func(handle *packHandle, offset int64) (int64, error) {
			return handle.decoder.GetSizeByOffset(offset)
		})
		if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return size, err
		}
		if !snapshot.looseMayHold(hash) {
			return 0, plumbing.ErrObjectNotFound
		}
		object, err := r.looseObject(hash)
		if err != nil {
			return 0, err
		}
		return object.Size(), nil
	})
}

// IterEncodedObjects walks every object of a type, pack by pack and then the
// loose tier, each object once however many places hold it. It starts from a
// listing of its own, since only a listing names the loose objects, and reads
// lazily, so walking a large repository never holds it in memory.
func (r *repository) IterEncodedObjects(kind plumbing.ObjectType) (storer.EncodedObjectIter, error) { //nolint:ireturn
	listed, err := r.tiers.refresh()
	if err != nil {
		return nil, err
	}
	loose := make([]plumbing.Hash, 0, len(listed.listing.loose))
	for _, object := range listed.listing.loose {
		loose = append(loose, object.hash)
	}
	return &objectWalk{repository: r, kind: kind, packs: listed.snapshot.packs, loose: loose, seen: map[plumbing.Hash]struct{}{}}, nil
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
			w.handle = w.repository.openPack(pack, index)
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

// References.

func (r *repository) withRefLock(name plumbing.ReferenceName, mutate func() error) error {
	return withLockName(lockNameFor("git-ref", r.name, name.String()), mutate)
}

// Reference resolves one reference: a plain read, which may be answered from
// what this handle read or wrote inside the freshness bound. See refs.go.
func (r *repository) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if err := checkSafeRefName(name); err != nil {
		return nil, err
	}
	return r.refs.resolve(name, false)
}

// IterReferences lists every reference, as each fetch and push begins by doing.
// The references are gathered before the iterator is returned, so the walk
// holds nothing and the caller may read and write the repository from inside it.
func (r *repository) IterReferences() (storer.ReferenceIter, error) { //nolint:ireturn
	refs, err := r.refs.all()
	if err != nil {
		return nil, err
	}
	return storer.NewReferenceSliceIter(refs), nil
}

// SetReference writes a reference whatever it held before.
func (r *repository) SetReference(ref *plumbing.Reference) error {
	if err := checkSafeRefName(ref.Name()); err != nil {
		return err
	}
	return r.withRefLock(ref.Name(), func() error { return r.refs.set(ref) })
}

// CheckAndSetReference moves a reference only if it still holds old: the
// compare-and-set every push depends on. Under the reference's lock it reads
// the store — never what this handle remembers — and compares as go-git does.
func (r *repository) CheckAndSetReference(next, old *plumbing.Reference) error {
	if err := checkSafeRefName(next.Name()); err != nil {
		return err
	}
	return r.withRefLock(next.Name(), func() error {
		if old != nil {
			current, err := r.refs.resolve(old.Name(), true)
			if err != nil {
				return err
			}
			if current.Hash() != old.Hash() {
				return gitStorage.ErrReferenceHasChanged
			}
		}
		return r.refs.set(next)
	})
}

// CreateReference writes a reference only if there is none of that name.
func (r *repository) CreateReference(ref *plumbing.Reference) error {
	if err := checkSafeRefName(ref.Name()); err != nil {
		return err
	}
	return r.withRefLock(ref.Name(), func() error {
		if _, err := r.refs.resolve(ref.Name(), true); err == nil {
			return ErrReferenceAlreadyExists
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		return r.refs.set(ref)
	})
}

// RemoveReference removes a reference whatever it held. Removing one that is
// not there is not an error.
func (r *repository) RemoveReference(name plumbing.ReferenceName) error {
	if err := checkSafeRefName(name); err != nil {
		return err
	}
	return r.withRefLock(name, func() error { return r.refs.remove(name) })
}

// RemoveReferenceCAS removes a reference only if it still holds old.
func (r *repository) RemoveReferenceCAS(old *plumbing.Reference) error {
	if err := checkSafeRefName(old.Name()); err != nil {
		return err
	}
	return r.withRefLock(old.Name(), func() error {
		current, err := r.refs.resolve(old.Name(), true)
		if err != nil {
			return err
		}
		if current.Type() != old.Type() || current.String() != old.String() {
			return gitStorage.ErrReferenceHasChanged
		}
		return r.refs.remove(old.Name())
	})
}

// InitializeRepositoryReferences creates a repository's first branch and points
// HEAD at it, as one step no other initialization can interleave with.
func (r *repository) InitializeRepositoryReferences(branch *plumbing.Reference, requireEmpty bool) error {
	if err := checkSafeRefName(branch.Name()); err != nil {
		return err
	}
	return withLockName(lockNameFor("git-ref", r.name, "repository-initialization"), func() error {
		// Read in order to decide, so from the store.
		listing, err := r.refs.list(true)
		if err != nil {
			return err
		}
		packed, err := r.refs.packedRefs(true)
		if err != nil {
			return err
		}
		names := append([]plumbing.ReferenceName(nil), listing.names...)
		for _, ref := range packed.ordered {
			names = append(names, ref.Name())
		}
		alreadyInitialized, branchExists := false, false
		for _, name := range names {
			if name.IsBranch() {
				alreadyInitialized = true
				branchExists = branchExists || name == branch.Name()
			}
		}
		if branchExists || (requireEmpty && alreadyInitialized) {
			return ErrReferenceAlreadyExists
		}
		// HEAD first. Until the branch exists HEAD names a branch yet to be
		// born, which is what an empty repository looks like and what this one
		// looked like a moment ago; written the other way round, a reader
		// between the two writes would find a branch in a repository whose HEAD
		// points somewhere else. If the branch then cannot be written the
		// repository is still the empty one it was, so there is nothing to undo.
		if !alreadyInitialized {
			if err := r.refs.set(plumbing.NewSymbolicReference(plumbing.HEAD, branch.Name())); err != nil {
				return err
			}
		}
		return r.refs.set(branch)
	})
}

// CountLooseRefs counts the references kept as objects of their own, which is
// all of them but any that arrived in a packed-refs file.
func (r *repository) CountLooseRefs() (int, error) {
	listing, err := r.refs.list(false)
	if err != nil {
		return 0, err
	}
	return len(listing.names), nil
}

// PackRefs does nothing, as go-git's in-memory storage does nothing: a
// reference here is an object of its own, which is what makes moving one a
// single write, and there is no packed form this package writes.
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

// Config reads the repository's config; a repository without one has the default.
func (r *repository) Config() (*config.Config, error) {
	data, exists, err := r.small("config")
	if err != nil {
		return nil, err
	}
	if !exists {
		return config.NewConfig(), nil
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
