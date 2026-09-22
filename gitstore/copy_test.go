package gitstore

import (
	"errors"
	"fmt"
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

// TestSettingManyReferencesIsOneWrite pins what a fork of a repository of many
// references costs: the references set together, in one swap of the manifest
// (and the snapshot a change list that long is folded into), where one at a
// time they were a conditional write each.
func TestSettingManyReferencesIsOneWrite(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	if err := stor.create(); err != nil {
		t.Fatal(err)
	}
	target := plumbing.NewHash("1111111111111111111111111111111111111111")
	refs := make([]*plumbing.Reference, 0, 1000)
	for i := range 1000 {
		refs = append(refs, plumbing.NewHashReference(plumbing.NewTagReferenceName(fmt.Sprintf("v%d", i)), target))
	}
	before := fake.Snapshot()
	if err := SetReferences(stor, refs); err != nil {
		t.Fatal(err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Put != 2 {
		t.Fatalf("setting 1,000 references cost %s, want the manifest and its snapshot", spent)
	}
	replica := testPackedStorage(t, fake)
	for _, ref := range refs {
		got, err := replica.Reference(ref.Name())
		if err != nil || got.Hash() != target {
			t.Fatalf("%s reads as %v (%v) on another replica", ref.Name(), got, err)
		}
	}
	if err := SetReferences(memory.NewStorage(), refs); err == nil {
		t.Fatal("go-git's own storage was asked to set references together")
	}
}
