package gitstore

import (
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
)

// TestCopyingObjectsCopiesPacksAsTheyStand pins what a fork or a cross-fork
// merge costs in an object store: the source's packs and sidecars copied
// server-side and named by one swap of the destination's manifest — nothing
// read, decoded or uploaded — with what the source held only in memory packed
// first, every object readable afterwards from a replica that has seen none of
// it, and a pack the destination already holds not copied again.
func TestCopyingObjectsCopiesPacksAsTheyStand(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	store := fake.store("prefix")
	open := func(name string) *repository {
		t.Helper()
		stor, err := store.Repository(name)
		if err != nil {
			t.Fatal(err)
		}
		return stor.(*repository)
	}
	source, destination := open("octocat/source"), open("octocat/fork")
	hashes := flushedBlobs(t, source, "first", 100)
	hashes = append(hashes, flushedBlobs(t, source, "second", 100)...)
	pending := writeBlob(t, source, "written and not yet packed")
	hashes = append(hashes, pending)
	if err := destination.create(); err != nil {
		t.Fatal(err)
	}

	before := fake.Snapshot()
	if err := CopyObjects(source, destination); err != nil {
		t.Fatalf("copy: %v", err)
	}
	spent := fake.Snapshot().Sub(before)
	// The pending object's own flush: its pack, sidecar and the source's
	// manifest swap. Then three packs, two objects each, copied; and the
	// destination's one swap. The manifests are read to know what to copy.
	if spent.Copy != 6 || spent.Put != 4 || spent.GetRanged != 0 {
		t.Fatalf("copying three packs cost %s, want 6 copies, 4 writes and no ranged read", spent)
	}

	replica, err := fake.store("prefix").Repository("octocat/fork")
	if err != nil {
		t.Fatal(err)
	}
	want := readObjects(t, source, hashes)
	got := readObjects(t, replica.(*repository), hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("the copy of %s reads back other bytes", hash)
		}
	}

	before = fake.Snapshot()
	if err := CopyObjects(source, destination); err != nil {
		t.Fatalf("copy again: %v", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Copy != 0 || spent.Put != 0 {
		t.Fatalf("copying packs the destination holds cost %s, want no copy and no write", spent)
	}

	other, err := fake.store("prefix").Repository("octocat/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyObjects(other, destination); err == nil {
		t.Fatal("a repository of another store handle was copied from as if it were this store's")
	}
	if err := CopyObjects(memory.NewStorage(), destination); err == nil {
		t.Fatal("go-git's own storage was copied from")
	}
	if err := CopyObjects(source, memory.NewStorage()); err == nil || errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("copying into go-git's own storage answered %v, want a refusal", err)
	}
}

// TestCopyingObjectsBetweenMemoryRepositoriesCopiesEachObject pins the other
// backends' copy: every object of the source, one by one.
func TestCopyingObjectsBetweenMemoryRepositoriesCopiesEachObject(t *testing.T) {
	source, err := OpenMemory("octocat/source")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := OpenMemory("octocat/fork")
	if err != nil {
		t.Fatal(err)
	}
	hashes := seedObjects(t, source, 20)
	if err := CopyObjects(source, destination); err != nil {
		t.Fatal(err)
	}
	for _, hash := range hashes {
		if err := destination.HasEncodedObject(hash); err != nil {
			t.Fatalf("%s was not copied: %v", hash, err)
		}
	}
}
