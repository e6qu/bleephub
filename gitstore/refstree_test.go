package gitstore

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// advertise lists every reference, as the start of a fetch or a push does.
func advertise(t *testing.T, stor storer.ReferenceStorer) map[string]string {
	t.Helper()
	iter, err := stor.IterReferences()
	if err != nil {
		t.Fatalf("list references: %v", err)
	}
	refs := map[string]string{}
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		refs[ref.Name().String()] = ref.Hash().String()
		return nil
	}); err != nil {
		t.Fatalf("walk references: %v", err)
	}
	return refs
}

func hashOf(n int) plumbing.Hash { return plumbing.NewHash(fmt.Sprintf("%040x", n+1)) }

// seedReferences writes branches and tags through one replica.
func seedReferences(t *testing.T, stor storer.ReferenceStorer, branches, tags int) map[string]string {
	t.Helper()
	want := map[string]string{}
	set := func(name plumbing.ReferenceName, n int) {
		if err := stor.SetReference(plumbing.NewHashReference(name, hashOf(n))); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
		want[name.String()] = hashOf(n).String()
	}
	for i := range branches {
		set(plumbing.NewBranchReferenceName(fmt.Sprintf("topic/branch-%03d", i)), i)
	}
	for i := range tags {
		set(plumbing.NewTagReferenceName(fmt.Sprintf("v1.%d", i)), 1000+i)
	}
	return want
}

func sameReferences(t *testing.T, when string, got, want map[string]string) {
	t.Helper()
	for name, hash := range want {
		if got[name] != hash {
			t.Fatalf("%s: %s is %q, want %q", when, name, got[name], hash)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok && name != "HEAD" {
			t.Fatalf("%s: advertised %s, which does not exist", when, name)
		}
	}
}

// TestAnAdvertisementIsOneListingHoweverManyReferences pins what listing a
// repository's references costs. Walked as a disk is, it was a LIST for each
// directory under refs/ and a GET for each reference, every time. It is one
// recursive LIST, and a GET only for a reference whose ETag this replica has
// not read — so the second advertisement of sixty references reads none.
func TestAnAdvertisementIsOneListingHoweverManyReferences(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = 20 * time.Millisecond
	writer := testPackedStorage(t, fake)
	want := seedReferences(t, writer, 40, 20)

	reader := testPackedStorage(t, fake)
	// A handle's first read also builds its pack index; that is not this cost.
	_ = reader.HasEncodedObject(plumbing.ZeroHash)
	before := fake.Snapshot()
	sameReferences(t, "first advertisement", advertise(t, reader), want)
	first := fake.Snapshot().Sub(before)
	if first.List != 1 {
		t.Fatalf("the first advertisement took %d listings, want one recursive listing: %s", first.List, first)
	}
	if first.Get < 60 {
		t.Fatalf("premise: the first advertisement read only %d of 60 references: %s", first.Get, first)
	}

	time.Sleep(3 * fake.opts.IndexFreshness)
	before = fake.Snapshot()
	sameReferences(t, "second advertisement", advertise(t, reader), want)
	second := fake.Snapshot().Sub(before)
	// HEAD and packed-refs live outside refs/ and are read as before.
	if second.List != 1 || second.Get > 2 {
		t.Fatalf("an advertisement of unchanged references cost %s, want 1 listing and no reference reads", second)
	}

	// Another replica moves one branch, deletes another and creates a third.
	moved := plumbing.NewBranchReferenceName("topic/branch-007")
	if err := writer.SetReference(plumbing.NewHashReference(moved, hashOf(7777))); err != nil {
		t.Fatalf("move: %v", err)
	}
	want[moved.String()] = hashOf(7777).String()
	gone := plumbing.NewBranchReferenceName("topic/branch-008")
	if err := writer.RemoveReference(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	delete(want, gone.String())
	created := plumbing.NewBranchReferenceName("a-new-branch")
	if err := writer.SetReference(plumbing.NewHashReference(created, hashOf(8888))); err != nil {
		t.Fatalf("create: %v", err)
	}
	want[created.String()] = hashOf(8888).String()

	time.Sleep(3 * fake.opts.IndexFreshness)
	before = fake.Snapshot()
	sameReferences(t, "after another replica's writes", advertise(t, reader), want)
	third := fake.Snapshot().Sub(before)
	if third.List != 1 || third.Get > 4 {
		t.Fatalf("an advertisement after two references changed cost %s, want 1 listing and only their reads", third)
	}
}

// TestAReplicaSeesItsOwnReferenceWritesAtOnce pins that the listing is never
// what hides a write made through the same filesystem: inside the freshness
// bound, with a listing held, a new branch, a moved one and a deleted one are
// all as the writer left them.
func TestAReplicaSeesItsOwnReferenceWritesAtOnce(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
	stor := testPackedStorage(t, fake)
	want := seedReferences(t, stor, 3, 1)
	sameReferences(t, "before", advertise(t, stor), want)

	created := plumbing.NewBranchReferenceName("created")
	if err := stor.SetReference(plumbing.NewHashReference(created, hashOf(1))); err != nil {
		t.Fatalf("create: %v", err)
	}
	want[created.String()] = hashOf(1).String()
	moved := plumbing.NewBranchReferenceName("topic/branch-000")
	if err := stor.SetReference(plumbing.NewHashReference(moved, hashOf(4242))); err != nil {
		t.Fatalf("move: %v", err)
	}
	want[moved.String()] = hashOf(4242).String()
	gone := plumbing.NewBranchReferenceName("topic/branch-001")
	if err := stor.RemoveReference(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	delete(want, gone.String())

	sameReferences(t, "after local writes", advertise(t, stor), want)
	if ref, err := stor.Reference(moved); err != nil || ref.Hash() != hashOf(4242) {
		t.Fatalf("resolving the moved branch: %v, %v", ref, err)
	}
	if _, err := stor.Reference(gone); err == nil {
		t.Fatal("a removed branch still resolves")
	}
}

// TestWithReuseOffAnAdvertisementIsThePlainWalk pins the off switch: with no
// staleness allowed, nothing is answered from a listing taken earlier, and every
// reference is read from the store each time.
func TestWithReuseOffAnAdvertisementIsThePlainWalk(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = -1
	stor := testPackedStorage(t, fake)
	want := seedReferences(t, stor, 5, 0)

	for round := range 2 {
		before := fake.Snapshot()
		sameReferences(t, "plain walk", advertise(t, stor), want)
		if spent := fake.Snapshot().Sub(before); spent.Get < 5 || spent.List < 2 {
			t.Fatalf("round %d: with reuse off the walk cost %s, want a read of every reference", round, spent)
		}
	}
}

// TestAColdAdvertisementReadsItsReferencesTogether pins that a replica with
// nothing held does not read the references it has just listed one after
// another. The requests are the same; what is pinned is that they overlap, so a
// cold advertisement costs a few round trips of waiting rather than one for
// every branch. The store holds the first reference read until a second one
// arrives: reads made one at a time would leave it waiting for good.
func TestAColdAdvertisementReadsItsReferencesTogether(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Minute
	want := seedReferences(t, testPackedStorage(t, fake), 64, 0)

	reader := testPackedStorage(t, fake)
	_ = reader.HasEncodedObject(plumbing.ZeroHash)

	var arrived atomic.Int32
	var oneAtATime atomic.Bool
	overlapped := make(chan struct{})
	var overlap sync.Once
	fake.SetOnRequest(func(method, key string) {
		if method != http.MethodGet || !strings.Contains(key, "/refs/heads/") {
			return
		}
		if arrived.Add(1) > 1 {
			overlap.Do(func() { close(overlapped) })
			return
		}
		select {
		case <-overlapped:
		case <-time.After(10 * time.Second):
			oneAtATime.Store(true)
		}
	})
	t.Cleanup(func() { fake.SetOnRequest(nil) })

	before := fake.Snapshot()
	sameReferences(t, "cold advertisement", advertise(t, reader), want)
	spent := fake.Snapshot().Sub(before)
	if oneAtATime.Load() {
		t.Fatal("the first reference read finished waiting before a second began: they are read one after another")
	}
	if spent.Get < 64 {
		t.Fatalf("premise: a cold advertisement read only %d of 64 references: %s", spent.Get, spent)
	}
	if spent.Get > 64+2 {
		t.Fatalf("a reference was read more than once: %s", spent)
	}
}
