package gitstore

import (
	"fmt"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Everything written is a pack. An object written one at a time — through
// SetEncodedObject, by the API, the wiki, a template's copy — is not given a key
// of its own: it is held as pending, readable by this replica at once, and lands
// in a pack at one of three moments:
//
//   - the next commit of the repository's references, which carries the pack in
//     the same swap of the manifest, so that an object and the reference that
//     names it become visible together;
//   - FlushObjects, which is what an application calls before it tells anyone an
//     object's id: the git database API answering with the id of a blob it has
//     just written, for one;
//   - as soon as what is pending passes pendingFlushBytes, so that a long run of
//     writes holds no more than that in memory.
//
// A pack a flush uploads is part of the repository only once the swap that
// names it succeeds; until then its objects stay pending, and the upload of a
// flush that failed is an orphan for a compaction to sweep. A write of one blob
// is therefore two uploads — pack and sidecar — and one swap, where a loose
// object was one upload but was then found by other replicas only by listing.

// pendingFlushBytes is how much written and unpacked content a repository holds
// in memory before it packs it.
const pendingFlushBytes = 8 << 20

// pendingObjects are the objects written to a repository and not yet in a pack.
type pendingObjects struct {
	mu      sync.Mutex
	objects map[plumbing.Hash]plumbing.EncodedObject
	bytes   int64
}

func (p *pendingObjects) add(object plumbing.EncodedObject) (held int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.objects == nil {
		p.objects = map[plumbing.Hash]plumbing.EncodedObject{}
	}
	if _, present := p.objects[object.Hash()]; !present {
		p.objects[object.Hash()] = object
		p.bytes += object.Size()
	}
	return p.bytes
}

func (p *pendingObjects) get(hash plumbing.Hash) (plumbing.EncodedObject, bool) { //nolint:ireturn
	p.mu.Lock()
	defer p.mu.Unlock()
	object, held := p.objects[hash]
	return object, held
}

// empty reports whether nothing is pending.
func (p *pendingObjects) empty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.objects) == 0
}

// snapshot returns what is pending now, in no particular order.
func (p *pendingObjects) snapshot() []plumbing.EncodedObject {
	p.mu.Lock()
	defer p.mu.Unlock()
	objects := make([]plumbing.EncodedObject, 0, len(p.objects))
	for _, object := range p.objects {
		objects = append(objects, object)
	}
	return objects
}

// packed forgets objects that a committed pack now holds. Objects written since
// the snapshot that pack was built from stay pending.
func (p *pendingObjects) packed(objects []plumbing.EncodedObject) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, object := range objects {
		if _, held := p.objects[object.Hash()]; held {
			delete(p.objects, object.Hash())
			p.bytes -= object.Size()
		}
	}
}

// pendingPack is the pending objects of one moment, packed and uploaded, and
// waiting for the swap that makes them part of the repository.
type pendingPack struct {
	objects  []plumbing.EncodedObject
	uploaded *uploadedPack
}

// packPending builds and uploads a pack of whatever is pending, or returns nil
// when nothing is. The caller holds r.flushing, and commits the pack.
func (r *repository) packPending() (*pendingPack, error) {
	objects := r.pending.snapshot()
	if len(objects) == 0 {
		return nil, nil
	}
	source := memory.NewStorage()
	hashes := make([]plumbing.Hash, 0, len(objects))
	for _, object := range objects {
		if _, err := source.SetEncodedObject(object); err != nil {
			return nil, err
		}
		hashes = append(hashes, object.Hash())
	}
	built, err := r.buildPackFrom(source, hashes)
	if err != nil {
		return nil, fmt.Errorf("pack %d written objects of %s: %w", len(objects), r.name, err)
	}
	defer built.cleanup()
	uploaded, err := r.uploadPack(r.shared.baseContext(), built)
	if err != nil {
		return nil, err
	}
	return &pendingPack{objects: objects, uploaded: uploaded}, nil
}

// commit commits mutate, carrying in the same swap a pack of whatever objects
// are pending. A commit that refuses leaves them pending, and its upload is an
// orphan for a compaction to sweep.
func (r *repository) commit(mutate mutation) (*repoState, error) {
	if r.pending.empty() {
		return r.commits.commit(mutate)
	}
	r.flushing.Lock()
	defer r.flushing.Unlock()
	batch, err := r.packPending()
	if err != nil {
		return nil, err
	}
	if batch == nil {
		return r.commits.commit(mutate)
	}
	state, err := r.commits.commit(func(d *draft) error {
		if err := d.addPack(batch.uploaded.entry(packSourceWrite)); err != nil {
			return err
		}
		return mutate(d)
	})
	if err != nil {
		r.manifests.withdraw(batch.uploaded.stored.name)
		return nil, err
	}
	r.pending.packed(batch.objects)
	r.notePackWritten(len(state.packs))
	return state, nil
}

// FlushObjects packs and commits every object written to the repository and
// not yet in a pack, so that it is durable and every replica can read it. An
// application calls it before it tells anyone the id of an object it has written
// on its own, without moving a reference to it.
func (r *repository) FlushObjects() error {
	r.flushing.Lock()
	defer r.flushing.Unlock()
	batch, err := r.packPending()
	if err != nil || batch == nil {
		return err
	}
	state, err := r.commits.commit(func(d *draft) error { return d.addPack(batch.uploaded.entry(packSourceWrite)) })
	if err != nil {
		r.manifests.withdraw(batch.uploaded.stored.name)
		return err
	}
	r.pending.packed(batch.objects)
	r.notePackWritten(len(state.packs))
	return nil
}

// ObjectFlusher is git storage whose object writes may be held before they are
// durable. Every backend of this package is one; on a disk or in memory an
// object is written by the time SetEncodedObject returns, and there flushing
// has nothing to do.
type ObjectFlusher interface {
	FlushObjects() error
}

var (
	_ ObjectFlusher = (*repository)(nil)
	_ ObjectFlusher = (*atomicRefStorer)(nil)
)

// FlushObjects makes every object written to stor durable and readable by every
// replica. It refuses storage that is not this package's: an application that
// is about to tell someone an object's id must not be told it may.
func FlushObjects(stor gitStorage.Storer) error {
	flusher, ok := stor.(ObjectFlusher)
	if !ok {
		return fmt.Errorf("gitstore: a %T cannot say that the objects written to it are durable", stor)
	}
	return flusher.FlushObjects()
}
