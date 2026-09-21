package gitstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

// readsOnlyFromMemory asserts that everything a handle can be asked about an
// object it holds pending is answered, and answered without the store.
func readsOnlyFromMemory(t *testing.T, fake *fakeS3, stor *repository, hash plumbing.Hash, body string) {
	t.Helper()
	before := fake.Snapshot()
	if err := stor.HasEncodedObject(hash); err != nil {
		t.Fatalf("the writing handle cannot find its own write: %v", err)
	}
	if got := readObjects(t, stor, []plumbing.Hash{hash})[hash]; got != body+"\x00blob" {
		t.Fatalf("the writing handle read back %q", got)
	}
	if size, err := stor.EncodedObjectSize(hash); err != nil || size != int64(len(body)) {
		t.Fatalf("the writing handle sizes its own write at %d (%v), want %d", size, err, len(body))
	}
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("reading an object held pending asked the store: %s", spent)
	}
	walk, err := stor.IterEncodedObjects(plumbing.BlobObject)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	found := false
	if err := walk.ForEach(func(object plumbing.EncodedObject) error {
		found = found || object.Hash() == hash
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !found {
		t.Fatal("a walk of the writing handle's blobs missed its own pending write")
	}
}

// TestAWrittenObjectIsPendingUntilFlushed pins the write path of an object
// written one at a time, as the API writes one. SetEncodedObject asks nothing
// of the store: the object is held by the handle that wrote it and readable
// there at once, and by no other replica — it is in no pack the manifest names.
// FlushObjects makes it durable and every replica's, for three writes: its pack,
// the pack's sidecar, and the manifest swap that names them. A flush with
// nothing pending asks nothing.
func TestAWrittenObjectIsPendingUntilFlushed(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 3)

	const body = "written through the API"
	before := fake.Snapshot()
	hash := writeBlob(t, stor, body)
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("writing an object asked the store: %s", spent)
	}
	readsOnlyFromMemory(t, fake, stor, hash, body)

	if err := testPackedStorage(t, fake).HasEncodedObject(hash); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("another replica sees an object that was never flushed: %v", err)
	}

	packsBefore := len(storedManifest(t, fake).Packs)
	before = fake.Snapshot()
	if err := FlushObjects(stor); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Put != 3 || spent.Total() != 3 {
		t.Fatalf("flushing one object cost %s, want its pack, its sidecar and the manifest swap", spent)
	}
	if packs := len(storedManifest(t, fake).Packs); packs != packsBefore+1 {
		t.Fatalf("the flush left %d live packs, want %d", packs, packsBefore+1)
	}
	if !stor.pending.empty() {
		t.Fatal("the flushed object is still pending")
	}
	if got := readObjects(t, testPackedStorage(t, fake), []plumbing.Hash{hash})[hash]; got != body+"\x00blob" {
		t.Fatalf("another replica read the flushed object as %q", got)
	}

	before = fake.Snapshot()
	if err := stor.FlushObjects(); err != nil {
		t.Fatalf("an empty flush: %v", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("a flush with nothing pending asked the store: %s", spent)
	}

	// The package function flushes this package's storage and has nothing to do
	// on go-git's own, which writes an object as it is given one; storage it
	// cannot vouch for it refuses.
	memoryStor, err := OpenMemory(testRepo)
	if err != nil {
		t.Fatalf("memory storage: %v", err)
	}
	if err := FlushObjects(memoryStor); err != nil {
		t.Fatalf("flushing the memory backend: %v", err)
	}
	if err := FlushObjects(memory.NewStorage()); err == nil {
		t.Fatal("storage of another package was said to have flushed")
	}
}

// TestAReferenceCommitCarriesThePendingObjects pins why an object written
// through the API costs no flush of its own in the common case: the reference
// commit that names it carries the pack of whatever is pending in the SAME swap
// of the manifest. There is then no moment at which a replica sees the
// reference and not the objects, and one conditional write where there would
// be two. Every way of committing a reference does it.
func TestAReferenceCommitCarriesThePendingObjects(t *testing.T) {
	commits := map[string]func(stor *repository, tip plumbing.Hash) error{
		"SetReference": func(stor *repository, tip plumbing.Hash) error {
			return stor.SetReference(plumbing.NewHashReference(testBranch, tip))
		},
		"CheckAndSetReference": func(stor *repository, tip plumbing.Hash) error {
			return stor.CheckAndSetReference(plumbing.NewHashReference(testBranch, tip), plumbing.NewHashReference(testBranch, hashOf(1)))
		},
		"CreateReference": func(stor *repository, tip plumbing.Hash) error {
			return stor.CreateReference(plumbing.NewHashReference(testBranch, tip))
		},
		"InitializeRepositoryReferences": func(stor *repository, tip plumbing.Hash) error {
			return stor.InitializeRepositoryReferences(plumbing.NewHashReference(testBranch, tip), true)
		},
	}
	for name, commit := range commits {
		t.Run(name, func(t *testing.T) {
			fake := newFakeS3(t)
			fake.opts.CompactAfterPacks = -1
			stor := testPackedStorage(t, fake)
			if name == "CheckAndSetReference" {
				if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(1))); err != nil {
					t.Fatalf("seed: %v", err)
				}
			} else if _, err := stor.manifests.held(); err != nil {
				t.Fatalf("read the manifest: %v", err)
			}
			packsBefore := len(stor.manifests.current.Load().manifest.Packs)
			// A blob, a tree naming it, and a commit naming the tree.
			tree := &treeBuilder{}
			blob := writeBlob(t, stor, "a file written through the API")
			tree.add("file.txt", blob)
			treeHash, err := tree.store(stor)
			if err != nil {
				t.Fatalf("tree: %v", err)
			}
			tip, err := storeCommit(stor, treeHash)
			if err != nil {
				t.Fatalf("commit: %v", err)
			}

			writes := countManifestWrites(fake)
			t.Cleanup(func() { fake.SetOnRequest(nil) })
			if err := commit(stor, tip); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got := writes(); got != 1 {
				t.Fatalf("%s carrying pending objects wrote the manifest %d times, want once", name, got)
			}
			stored := storedManifest(t, fake)
			if len(stored.Packs) != packsBefore+1 {
				t.Fatalf("%s left %d live packs, want the %d before and the pack of what was pending", name, len(stored.Packs), packsBefore)
			}
			for _, pack := range stored.Packs {
				if pack.Source == packSourceWrite && pack.Objects != 3 {
					t.Fatalf("the pack %s carried holds %d objects, want the 3 written", name, pack.Objects)
				}
			}
			if !stor.pending.empty() {
				t.Fatalf("after %s objects are still pending", name)
			}

			fresh := testPackedStorage(t, fake)
			resolvesTo(t, "after "+name, fresh, testBranch, tip)
			readObjects(t, fresh, []plumbing.Hash{blob, treeHash, tip})
		})
	}
}

// TestARefusedReferenceCommitLeavesTheObjectsPending pins the other half of
// carrying the pack in the reference's swap: when the reference commit refuses —
// the branch moved — the pack goes with it, invisible to every replica, and the
// objects stay pending on the handle that wrote them, still readable there, for
// a later commit or flush to land.
func TestARefusedReferenceCommitLeavesTheObjectsPending(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	base := seedObjects(t, stor, 2)
	tip := base[len(base)-1]
	packsBefore := len(storedManifest(t, fake).Packs)

	const body = "written for a branch that moved"
	hash := writeBlob(t, stor, body)
	stale := plumbing.NewHashReference(testBranch, hashOf(12345))
	if err := stor.CheckAndSetReference(plumbing.NewHashReference(testBranch, hash), stale); !errors.Is(err, gitStorage.ErrReferenceHasChanged) {
		t.Fatalf("a compare-and-set from a stale value answered %v, want ErrReferenceHasChanged", err)
	}
	if len(packKeys(fake, ".pack")) != packsBefore+1 {
		t.Fatalf("premise: the refused commit uploaded no pack (%d stored), so carrying one was not exercised", len(packKeys(fake, ".pack")))
	}
	if _, pending := stor.pending.get(hash); !pending {
		t.Fatal("a refused commit dropped the objects it carried")
	}
	readsOnlyFromMemory(t, fake, stor, hash, body)
	fresh := testPackedStorage(t, fake)
	if err := fresh.HasEncodedObject(hash); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("another replica sees the objects of a refused commit: %v", err)
	}
	resolvesTo(t, "after the refused commit", fresh, testBranch, tip)
	if packs := len(storedManifest(t, fake).Packs); packs != packsBefore {
		t.Fatalf("a refused commit left %d live packs, want %d", packs, packsBefore)
	}

	if err := stor.FlushObjects(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := readObjects(t, testPackedStorage(t, fake), []plumbing.Hash{hash})[hash]; got != body+"\x00blob" {
		t.Fatalf("after the flush another replica read %q", got)
	}
}

// TestPendingContentPastTheBoundIsPacked pins the bound on what a handle holds
// in memory. A long run of writes with no reference commit and no flush — an
// import, say — must not hold all of it: once what is pending passes
// pendingFlushBytes, the write that took it past packs everything pending, and
// every replica can read it without anyone having asked.
func TestPendingContentPastTheBoundIsPacked(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 1)

	const each = pendingFlushBytes/3 + 1<<10
	var written []plumbing.Hash
	for i := range 2 {
		written = append(written, writeIncompressibleBlob(t, stor, fmt.Sprintf("bound %d", i), each))
	}
	if stor.pending.empty() || testPackedStorage(t, fake).HasEncodedObject(written[0]) == nil {
		t.Fatalf("premise: %d bytes, under the bound of %d, were packed", 2*each, pendingFlushBytes)
	}
	packsBefore := len(storedManifest(t, fake).Packs)

	written = append(written, writeIncompressibleBlob(t, stor, "bound 2", each))
	if !stor.pending.empty() {
		t.Fatalf("%d bytes written, past the bound of %d, are still pending", 3*each, pendingFlushBytes)
	}
	if packs := len(storedManifest(t, fake).Packs); packs != packsBefore+1 {
		t.Fatalf("passing the bound left %d live packs, want one more than %d", packs, packsBefore)
	}
	fresh := testPackedStorage(t, fake)
	for _, hash := range written {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("an object packed at the bound is not every replica's: %v", err)
		}
	}
}

// TestNoWritePathLeavesALooseObject pins the layout: every object the engine
// stores is in a pack the manifest names, and nothing is ever written under
// objects/XX/, the fanout the earlier layouts kept loose objects in — nothing
// reads there any more, so anything written there would be lost. Every way an
// object reaches the store is driven: a write and a flush, a reference commit
// carrying what is pending, a write past the pending bound, a pack written
// through PackfileWriter, a push transaction, and a compaction.
func TestNoWritePathLeavesALooseObject(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	var all []plumbing.Hash
	steps := []struct {
		name string
		run  func()
	}{
		{"a write and a flush", func() { all = append(all, flushedBlobs(t, stor, "flushed", 5)...) }},
		{"a reference commit carrying what is pending", func() { all = append(all, seedObjects(t, stor, 5)...) }},
		{"a write past the pending bound", func() {
			for i := range 3 {
				all = append(all, writeIncompressibleBlob(t, stor, fmt.Sprintf("loose-free %d", i), pendingFlushBytes/3+1<<10))
			}
		}},
		{"a pack written through PackfileWriter", func() { all = append(all, smallPush(t, stor, "a pack written whole")) }},
		{"a push transaction", func() {
			pack, hashes := pushPack(t, 7)
			if refusals, err := pushThrough(t, stor, pack).Commit([]ReferenceUpdate{{Name: "refs/heads/pushed", New: hashes[len(hashes)-1]}}, false); err != nil || refusals[0] != nil {
				t.Fatalf("push: %v, %v", refusals, err)
			}
			all = append(all, hashes...)
		}},
		{"a compaction", func() {
			if result, err := stor.Compact(context.Background()); err != nil || result.PackName == "" {
				t.Fatalf("premise: the compaction merged nothing: %+v, %v", result, err)
			}
		}},
	}
	for _, step := range steps {
		step.run()
		if loose := looseLayoutKeys(fake); len(loose) != 0 {
			t.Fatalf("%s left loose objects: %v", step.name, loose)
		}
		for _, key := range fake.KeysWithPrefix("prefix/" + testRepo + "/objects/") {
			relative := strings.TrimPrefix(key, "prefix/"+testRepo+"/")
			pack := strings.HasPrefix(relative, "objects/pack/pack-") && (strings.HasSuffix(relative, ".pack") || strings.HasSuffix(relative, sidecarSuffix))
			if !pack && !strings.HasPrefix(relative, refSnapshotDirectory) {
				t.Fatalf("%s wrote %s, which is neither a pack, a sidecar nor a reference snapshot", step.name, relative)
			}
		}
	}
	fresh := testPackedStorage(t, fake)
	for _, hash := range all {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s is not every replica's: %v", hash, err)
		}
	}
}

// TestFlushingConcurrentlyWithWritesLosesNothing drives writes, reference
// commits and flushes through one handle from several goroutines at once. A
// flush packs a snapshot of what is pending and forgets only that; an object
// written while it was being packed must stay pending for the next. Run under
// -race, it must also be clean.
func TestFlushingConcurrentlyWithWritesLosesNothing(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	if err := Init(stor); err != nil {
		t.Fatalf("init: %v", err)
	}
	const writers, each = 4, 25
	written := make([][]plumbing.Hash, writers)
	errs := make(chan error, writers)
	for writer := range writers {
		go func() {
			for i := range each {
				object := stor.NewEncodedObject()
				object.SetType(plumbing.BlobObject)
				body := []byte(fmt.Sprintf("writer %d object %d", writer, i))
				object.SetSize(int64(len(body)))
				w, _ := object.Writer()
				_, _ = w.Write(body)
				_ = w.Close()
				hash, err := stor.SetEncodedObject(object)
				if err != nil {
					errs <- err
					return
				}
				written[writer] = append(written[writer], hash)
				switch i % 5 {
				case 2:
					err = stor.FlushObjects()
				case 4:
					err = stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(fmt.Sprintf("writer-%d", writer)), hash))
				}
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for range writers {
		if err := <-errs; err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := stor.FlushObjects(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	fresh := testPackedStorage(t, fake)
	for _, hashes := range written {
		for _, hash := range hashes {
			if err := fresh.HasEncodedObject(hash); err != nil {
				t.Fatalf("object %s was lost between concurrent flushes: %v", hash, err)
			}
		}
	}
}
