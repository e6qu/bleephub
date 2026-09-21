package gitstore

import (
	"errors"
	"io"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// A push is one transaction. Its pack is uploaded first and is part of nothing:
// no manifest names it, so no reader — on this replica or another — can see its
// objects. The server then decides each reference update with the pushed objects
// to hand, through the transaction, which reads the quarantined pack as well as
// the repository. Only then does one commit of the manifest add the pack AND move
// the references, each against the value the pusher expected. That is git's
// quarantine rule: a pack is never visible unless the push's reference updates
// were accepted; a push that is refused leaves an upload nothing refers to,
// which a later compaction sweeps; and a push of several references negotiated
// as atomic moves all of them or none, because it is one write.

// PushTransactor is a repository that can take a push as one transaction. Only
// the object-store backend is one: on a disk go-git's storage has no commit
// point to put a pack and its references behind.
type PushTransactor interface {
	// BeginPush opens a transaction. The caller ends it with Commit or Abort.
	BeginPush() (PushTransaction, error)
}

// PushTransaction is a push in progress. As a storer it reads the repository
// and, as well, the objects of the pack written through its PackfileWriter,
// which nothing else can see until Commit. Its reference methods are the
// repository's own and commit at once; a push moves references with Commit.
type PushTransaction interface {
	gitStorage.Storer
	storer.PackfileWriter
	// Commit adds the transaction's pack and applies the updates in one swap of
	// the manifest. It returns one error for each update, nil for those applied.
	// An update whose reference is not at its Old value is refused with
	// storage.ErrReferenceHasChanged (or ErrReferenceAlreadyExists for a create).
	// With atomic set, one refusal refuses them all, the rest with
	// ErrPushAborted. If no update is applied the pack is not added either. The
	// second result is a failure of the commit itself, after which nothing was
	// changed.
	Commit(updates []ReferenceUpdate, atomic bool) ([]error, error)
	// Abort ends the transaction without changing the repository.
	Abort()
}

// ErrPushAborted is what an update of an atomic push is refused with when
// another update of the same push was the one at fault.
var ErrPushAborted = errors.New("gitstore: another reference update of the atomic push was refused")

// errPushRefused is how a push's mutation tells the committer that it applied
// nothing; Commit reports the refusals themselves.
var errPushRefused = errors.New("gitstore: no reference update of the push was accepted")

// quarantine is the packs of a push that no manifest names yet, and the objects
// decoded out of them so far. Those are kept apart from the repository's own
// object cache, which serves every reader: an object of a push that may yet be
// refused must not be handed to one.
type quarantine struct {
	packs []*storedPack
	cache cache.Object
}

// noQuarantine is what a read outside any push looks through before the
// repository: no packs, and a cache that keeps nothing.
var noQuarantine = &quarantine{cache: noCache{}}

// noCache is an object cache that never holds anything.
type noCache struct{}

func (noCache) Put(plumbing.EncodedObject) {}

func (noCache) Get(plumbing.Hash) (plumbing.EncodedObject, bool) { return nil, false } //nolint:ireturn

func (noCache) Clear() {}

// quarantineView reads a repository together with packs that are not live.
type quarantineView struct {
	*repository
	quarantine *quarantine
}

func (v *quarantineView) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	return v.encodedObject(v.quarantine, kind, hash)
}

func (v *quarantineView) HasEncodedObject(hash plumbing.Hash) error {
	return v.hasEncodedObject(v.quarantine, hash)
}

func (v *quarantineView) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	return v.encodedObjectSize(v.quarantine, hash)
}

type pushTransaction struct {
	*repository
	// decoded is the quarantine's object cache, for the life of the transaction.
	decoded cache.Object

	mu       sync.Mutex
	uploaded []*uploadedPack
	ended    bool
}

var _ PushTransaction = (*pushTransaction)(nil)

// BeginPush opens a push transaction on the repository.
func (r *repository) BeginPush() (PushTransaction, error) { //nolint:ireturn
	return &pushTransaction{repository: r, decoded: cache.NewObjectLRUDefault()}, nil
}

// view is the repository as the transaction reads it now.
func (t *pushTransaction) view() *quarantineView {
	t.mu.Lock()
	defer t.mu.Unlock()
	packs := make([]*storedPack, 0, len(t.uploaded))
	for _, uploaded := range t.uploaded {
		packs = append(packs, uploaded.stored)
	}
	return &quarantineView{repository: t.repository, quarantine: &quarantine{packs: packs, cache: t.decoded}}
}

func (t *pushTransaction) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	return t.view().EncodedObject(kind, hash)
}

func (t *pushTransaction) HasEncodedObject(hash plumbing.Hash) error {
	return t.view().HasEncodedObject(hash)
}

func (t *pushTransaction) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	return t.view().EncodedObjectSize(hash)
}

// PackfileWriter receives the push's pack and uploads it into quarantine.
func (t *pushTransaction) PackfileWriter() (io.WriteCloser, error) {
	ingest, err := t.newPackIngest(func(uploaded *uploadedPack) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.ended {
			t.manifests.withdraw(uploaded.stored.name)
			return errors.New("gitstore: the push transaction has ended")
		}
		t.uploaded = append(t.uploaded, uploaded)
		return nil
	})
	if err != nil {
		return nil, err
	}
	ingest.quarantine = t.view().quarantine
	return ingest, nil
}

// end closes the transaction and returns the packs it uploaded.
func (t *pushTransaction) end() ([]*uploadedPack, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return nil, errors.New("gitstore: the push transaction has ended")
	}
	t.ended = true
	return t.uploaded, nil
}

func (t *pushTransaction) withdraw(uploaded []*uploadedPack) {
	for _, pack := range uploaded {
		t.manifests.withdraw(pack.stored.name)
	}
}

func (t *pushTransaction) Commit(updates []ReferenceUpdate, atomic bool) ([]error, error) {
	if len(updates) == 0 {
		return nil, errors.New("gitstore: a push commits at least one reference update")
	}
	uploaded, err := t.end()
	if err != nil {
		return nil, err
	}
	results := make([]error, len(updates))
	state, err := t.commits.commit(func(d *draft) error {
		applied, refused := 0, 0
		for i, update := range updates {
			if results[i] = update.apply(d); results[i] != nil {
				refused++
				continue
			}
			applied++
		}
		if applied == 0 || (atomic && refused > 0) {
			return errPushRefused
		}
		for _, pack := range uploaded {
			if err := d.addPack(pack.entry(packSourcePush)); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errPushRefused) {
		t.withdraw(uploaded)
		for i := range results {
			if results[i] == nil {
				results[i] = ErrPushAborted
			}
		}
		return results, nil
	}
	if err != nil {
		t.withdraw(uploaded)
		return nil, err
	}
	if len(uploaded) > 0 {
		t.notePackWritten(len(state.packs))
	}
	return results, nil
}

func (t *pushTransaction) Abort() {
	uploaded, err := t.end()
	if err != nil {
		return
	}
	t.withdraw(uploaded)
}
