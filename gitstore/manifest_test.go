package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

const testBranch = plumbing.ReferenceName("refs/heads/main")

// manifestKey is where the fake holds the test repository's manifest.
const manifestKey = "prefix/" + testRepo + "/" + manifestName

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

func resolvesTo(t *testing.T, when string, stor storer.ReferenceStorer, name plumbing.ReferenceName, want plumbing.Hash) {
	t.Helper()
	ref, err := stor.Reference(name)
	if err != nil {
		t.Fatalf("%s: resolve %s: %v", when, name, err)
	}
	if ref.Hash() != want {
		t.Fatalf("%s: %s is %s, want %s", when, name, ref.Hash(), want)
	}
}

// storedManifest decodes the manifest the fake holds.
func storedManifest(t *testing.T, fake *fakeS3) *manifest {
	t.Helper()
	data, ok := fake.Get(manifestKey)
	if !ok {
		t.Fatal("the store holds no manifest")
	}
	decoded, err := decodeManifest(data)
	if err != nil {
		t.Fatalf("the stored manifest: %v", err)
	}
	return decoded
}

// countManifestWrites counts the PUTs of the test repository's manifest from
// now on, won or lost.
func countManifestWrites(fake *fakeS3) func() int {
	var mu sync.Mutex
	writes := 0
	fake.SetOnRequest(func(method, key string) {
		if method == "PUT" && key == manifestKey {
			mu.Lock()
			writes++
			mu.Unlock()
		}
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return writes
	}
}

// TestTheManifestIsTheOnlyPlaceAReferenceLives pins the layout: a reference is
// an entry of the manifest and no key of its own. The engine that kept each
// reference as an object had to list refs/ to advertise them and lock to move
// one; any key written there now would be one nothing reads.
func TestTheManifestIsTheOnlyPlaceAReferenceLives(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	if err := Init(stor); err != nil {
		t.Fatalf("init: %v", err)
	}
	want := seedReferences(t, stor, 3, 2)
	for _, key := range fake.KeysWithPrefix("prefix/" + testRepo + "/") {
		if key != manifestKey {
			t.Fatalf("references were written to %s, want nothing but the manifest", key)
		}
	}
	stored := storedManifest(t, fake)
	if stored.Format != manifestFormat || stored.Sequence != 6 {
		t.Fatalf("the manifest is format %d at sequence %d, want format %d and one sequence a commit (6)", stored.Format, stored.Sequence, manifestFormat)
	}
	if len(stored.Refs.Changes) != 6 || stored.Refs.Snapshot != "" {
		t.Fatalf("the manifest holds %d changes and the snapshot %q, want HEAD and five references inline", len(stored.Refs.Changes), stored.Refs.Snapshot)
	}
	sameReferences(t, "a replica that has never seen the repository", advertise(t, testPackedStorage(t, fake)), want)
}

// TestReferenceReadsWithinTheFreshnessBoundCostNothing prices the reads a
// server makes all day. It resolves a branch a dozen times in the course of one
// push and lists every reference at the start of every fetch; with the manifest
// held, all of that is answered from memory inside the bound, and by one
// conditional read — answered "not modified", with no body — once it has passed.
func TestReferenceReadsWithinTheFreshnessBoundCostNothing(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Second
	fake.clock = newTestClock()
	writer := testPackedStorage(t, fake)
	want := seedReferences(t, writer, 4, 0)

	reader := testPackedStorage(t, fake)
	before := fake.Snapshot()
	for range 5 {
		sameReferences(t, "inside the bound", advertise(t, reader), want)
		resolvesTo(t, "inside the bound", reader, "refs/heads/topic/branch-001", hashOf(1))
	}
	if spent := fake.Snapshot().Sub(before); spent.Get != 1 || spent.Total() != 1 {
		t.Fatalf("ten reads inside the bound should cost the one read of the manifest: %s", spent)
	}

	fake.clock.Advance(2 * time.Second)
	before = fake.Snapshot()
	sameReferences(t, "past the bound", advertise(t, reader), want)
	if spent := fake.Snapshot().Sub(before); spent.NotModified != 1 || spent.Total() != 1 || spent.BytesDown != 0 {
		t.Fatalf("a read past the bound should cost one conditional read with no body: %s", spent)
	}

	// Another replica moves a branch; past the bound the reader sees it.
	moved := plumbing.NewBranchReferenceName("topic/branch-001")
	if err := writer.SetReference(plumbing.NewHashReference(moved, hashOf(77))); err != nil {
		t.Fatalf("move: %v", err)
	}
	resolvesTo(t, "inside the bound, after another replica's write", reader, moved, hashOf(1))
	fake.clock.Advance(2 * time.Second)
	resolvesTo(t, "past the bound, after another replica's write", reader, moved, hashOf(77))
}

// TestWithFreshnessOffEveryReferenceReadRevalidates pins IndexFreshness < 0: a
// deployment that cannot tolerate a replica reading a reference another has
// moved asks for every read to be checked with the store, and gets it — at one
// conditional read each, not a read of every reference.
func TestWithFreshnessOffEveryReferenceReadRevalidates(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = -1
	writer := testPackedStorage(t, fake)
	seedReferences(t, writer, 3, 0)
	reader := testPackedStorage(t, fake)
	resolvesTo(t, "the first read", reader, "refs/heads/topic/branch-000", hashOf(0))

	before := fake.Snapshot()
	const reads = 6
	for range reads {
		resolvesTo(t, "with freshness off", reader, "refs/heads/topic/branch-000", hashOf(0))
	}
	if spent := fake.Snapshot().Sub(before); spent.NotModified != reads || spent.Total() != reads {
		t.Fatalf("%d reads with freshness off should each revalidate, and do nothing else: %s", reads, spent)
	}
	if err := writer.SetReference(plumbing.NewHashReference("refs/heads/topic/branch-000", hashOf(9))); err != nil {
		t.Fatalf("move: %v", err)
	}
	resolvesTo(t, "the read after another replica's write", reader, "refs/heads/topic/branch-000", hashOf(9))
}

// TestAWriteReadsNothingFirst pins "the 412 is the verification". A handle that
// holds a manifest commits by writing on the condition that the store's is the
// one it holds; reading first would double the cost of every reference update
// and prove nothing the conditional write does not.
func TestAWriteReadsNothingFirst(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = -1
	stor := testPackedStorage(t, fake)
	seedReferences(t, stor, 1, 0)

	before := fake.Snapshot()
	const writes = 5
	for i := range writes {
		old := plumbing.NewHashReference(testBranch, hashOf(i))
		if i == 0 {
			old = nil
		}
		if err := stor.CheckAndSetReference(plumbing.NewHashReference(testBranch, hashOf(i+1)), old); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if spent := fake.Snapshot().Sub(before); spent.Put != writes || spent.Total() != writes {
		t.Fatalf("%d compare-and-sets on a held manifest should cost %d conditional writes and no read: %s", writes, writes, spent)
	}
}

// TestTwoReplicasRacingOneBranchHaveOneWinner is the guarantee every push leans
// on, with nothing but the object store to arbitrate: the replicas are stores
// of their own that share no lock, in process or out, and the engine has no
// lock service to install. Two pushes that each read the branch at the same
// commit must not both be told they moved it.
func TestTwoReplicasRacingOneBranchHaveOneWinner(t *testing.T) {
	fake := newFakeS3(t)
	seed := testPackedStorage(t, fake)
	if err := seed.SetReference(plumbing.NewHashReference(testBranch, hashOf(0))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const replicas = 8
	old := plumbing.NewHashReference(testBranch, hashOf(0))
	start := make(chan struct{})
	results := make(chan error, replicas)
	for i := range replicas {
		replica := testPackedStorage(t, fake)
		// Each replica has read the branch, as a push has by the time it writes.
		resolvesTo(t, "before the race", replica, testBranch, hashOf(0))
		go func() {
			<-start
			results <- replica.CheckAndSetReference(plumbing.NewHashReference(testBranch, hashOf(100+i)), old)
		}()
	}
	close(start)
	moved, refused := 0, 0
	for range replicas {
		switch err := <-results; {
		case err == nil:
			moved++
		case errors.Is(err, gitStorage.ErrReferenceHasChanged):
			refused++
		default:
			t.Fatalf("compare-and-set: %v", err)
		}
	}
	if moved != 1 || refused != replicas-1 {
		t.Fatalf("%d replicas moved the branch and %d were refused, want exactly one winner", moved, refused)
	}
	if stored := storedManifest(t, fake); stored.Sequence != 2 {
		t.Fatalf("the manifest is at sequence %d, want 2: the losers must have written nothing", stored.Sequence)
	}
}

// TestTwoReplicasMovingDifferentBranchesBothSucceed is the other half: losing
// the swap is not losing the update. A replica whose conditional write is
// refused because ANOTHER branch moved re-reads, re-applies and commits; only a
// change to the reference it compares may refuse it.
func TestTwoReplicasMovingDifferentBranchesBothSucceed(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
	fake.clock = newTestClock()
	first, second := testPackedStorage(t, fake), testPackedStorage(t, fake)
	one, two := plumbing.NewBranchReferenceName("one"), plumbing.NewBranchReferenceName("two")
	for _, name := range []plumbing.ReferenceName{one, two} {
		if err := first.SetReference(plumbing.NewHashReference(name, hashOf(0))); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	resolvesTo(t, "the second replica's read", second, two, hashOf(0))

	if err := first.CheckAndSetReference(plumbing.NewHashReference(one, hashOf(1)), plumbing.NewHashReference(one, hashOf(0))); err != nil {
		t.Fatalf("the first replica: %v", err)
	}
	if held := second.manifests.current.Load().manifest.Sequence; held != 2 {
		t.Fatalf("premise: the second replica holds sequence %d, want the 2 that is now out of date", held)
	}
	writes := countManifestWrites(fake)
	if err := second.CheckAndSetReference(plumbing.NewHashReference(two, hashOf(2)), plumbing.NewHashReference(two, hashOf(0))); err != nil {
		t.Fatalf("the second replica was refused over a branch it did not touch: %v", err)
	}
	fake.SetOnRequest(nil)
	if writes() != 2 {
		t.Fatalf("premise: the second replica wrote the manifest %d times, want a lost swap and a won one", writes())
	}
	reader := testPackedStorage(t, fake)
	resolvesTo(t, "after both", reader, one, hashOf(1))
	resolvesTo(t, "after both", reader, two, hashOf(2))
}

// holdFirstManifestWrite makes the fake hold the first write of the manifest
// until release is called, and reports, through held, that it has arrived.
func holdFirstManifestWrite(fake *fakeS3) (held <-chan struct{}, release func(), writes func() int) {
	arrived, gate := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	count := 0
	fake.SetOnRequest(func(method, key string) {
		if method != "PUT" || key != manifestKey {
			return
		}
		mu.Lock()
		count++
		first := count == 1
		mu.Unlock()
		if first {
			close(arrived)
			<-gate
		}
	})
	return arrived, func() { close(gate) }, func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

// waitForWaiting spins until n commits are queued behind the swap in flight.
func waitForWaiting(stor *repository, n int) {
	for {
		stor.commits.mu.Lock()
		waiting := len(stor.commits.waiting)
		stor.commits.mu.Unlock()
		if waiting >= n {
			return
		}
		runtime.Gosched()
	}
}

// TestCommitsThatArriveTogetherShareASwap pins group commit. One object takes
// about one conditional overwrite a second on Google Cloud Storage, so a busy
// repository whose every reference update made its own swap would queue without
// bound; commits that arrive while a swap is in flight go out together in the
// next one.
func TestCommitsThatArriveTogetherShareASwap(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	if err := Init(stor); err != nil {
		t.Fatalf("init: %v", err)
	}
	held, release, writes := holdFirstManifestWrite(fake)
	t.Cleanup(func() { fake.SetOnRequest(nil) })

	const callers = 24
	results := make(chan error, callers+1)
	go func() { results <- stor.SetReference(plumbing.NewHashReference("refs/heads/leader", hashOf(0))) }()
	<-held
	for i := range callers {
		go func() {
			results <- stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(fmt.Sprintf("follower-%02d", i)), hashOf(i)))
		}()
	}
	waitForWaiting(stor, callers)
	release()
	for range callers + 1 {
		if err := <-results; err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if got := writes(); got != 2 {
		t.Fatalf("%d commits behind one swap in flight wrote the manifest %d times, want 2: the one in flight and one for all the rest", callers, got)
	}
	if refs := advertise(t, testPackedStorage(t, fake)); len(refs) != callers+2 {
		t.Fatalf("the store holds %d references, want HEAD and the %d written", len(refs), callers+1)
	}
}

// TestARefusalInAGroupFailsOnlyItsOwnCaller pins the other rule of group commit:
// the mutations of a group are validated one by one. A compare-and-set that
// loses must not take down the pushes that happened to share its swap, and must
// itself still be told it lost.
func TestARefusalInAGroupFailsOnlyItsOwnCaller(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(0))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	held, release, writes := holdFirstManifestWrite(fake)
	t.Cleanup(func() { fake.SetOnRequest(nil) })

	leader := make(chan error, 1)
	go func() { leader <- stor.SetReference(plumbing.NewHashReference("refs/heads/leader", hashOf(1))) }()
	<-held
	stale, honest := make(chan error, 1), make(chan error, 2)
	go func() {
		stale <- stor.CheckAndSetReference(plumbing.NewHashReference(testBranch, hashOf(5)), plumbing.NewHashReference(testBranch, hashOf(4)))
	}()
	go func() {
		honest <- stor.CheckAndSetReference(plumbing.NewHashReference(testBranch, hashOf(6)), plumbing.NewHashReference(testBranch, hashOf(0)))
	}()
	go func() { honest <- stor.SetReference(plumbing.NewHashReference("refs/heads/bystander", hashOf(7))) }()
	waitForWaiting(stor, 3)
	release()

	if err := <-leader; err != nil {
		t.Fatalf("the commit in flight: %v", err)
	}
	if err := <-stale; !errors.Is(err, gitStorage.ErrReferenceHasChanged) {
		t.Fatalf("the stale compare-and-set answered %v, want ErrReferenceHasChanged", err)
	}
	for range 2 {
		if err := <-honest; err != nil {
			t.Fatalf("a commit sharing a swap with a refused one failed: %v", err)
		}
	}
	if got := writes(); got != 2 {
		t.Fatalf("premise: the manifest was written %d times, want 2, so that the refusal and the others shared a swap", got)
	}
	reader := testPackedStorage(t, fake)
	resolvesTo(t, "after the group", reader, testBranch, hashOf(6))
	resolvesTo(t, "after the group", reader, "refs/heads/bystander", hashOf(7))
}

// TestTheReferenceChangesFoldIntoASnapshot pins what keeps the manifest small.
// Every commit uploads the manifest whole, so a repository with thousands of
// references must not carry them in it: past the bound the changes are folded
// into a snapshot object, written before the manifest that names it and under a
// key nothing has used, and the manifest starts again from an empty list.
func TestTheReferenceChangesFoldIntoASnapshot(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	const references = 5000
	updates := make([]ReferenceUpdate, 0, references)
	want := map[string]string{}
	for i := range references {
		name := plumbing.NewTagReferenceName(fmt.Sprintf("release/v%d.%d.%d", i/100, i%100, i))
		updates = append(updates, ReferenceUpdate{Name: name, New: hashOf(i)})
		want[name.String()] = hashOf(i).String()
	}
	push, err := stor.BeginPush()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if refusals, err := push.Commit(updates, true); err != nil || errors.Join(refusals...) != nil {
		t.Fatalf("create %d references: %v, %v", references, err, errors.Join(refusals...))
	}
	first := storedManifest(t, fake)
	if first.Refs.Snapshot == "" || len(first.Refs.Changes) != 0 {
		t.Fatalf("premise: %d references left the snapshot %q and %d changes inline, want them folded", references, first.Refs.Snapshot, len(first.Refs.Changes))
	}

	// Moving references one commit at a time grows the change list to the bound
	// and folds it again, into another snapshot; the first is never rewritten.
	firstSnapshot, _ := fake.Get("prefix/" + testRepo + "/" + first.Refs.Snapshot)
	largest := 0
	for i := range refChangeBound + 40 {
		name := plumbing.NewTagReferenceName(fmt.Sprintf("release/v%d.%d.%d", i/100, i%100, i))
		if err := stor.SetReference(plumbing.NewHashReference(name, hashOf(references+i))); err != nil {
			t.Fatalf("move %s: %v", name, err)
		}
		want[name.String()] = hashOf(references + i).String()
		data, _ := fake.Get(manifestKey)
		largest = max(largest, len(data))
	}
	if largest > manifestReferenceBytesBound {
		t.Fatalf("the manifest grew to %d bytes with %d references, want it under %d", largest, references, manifestReferenceBytesBound)
	}
	second := storedManifest(t, fake)
	if second.Refs.Snapshot == first.Refs.Snapshot || len(second.Refs.Changes) > refChangeBound {
		t.Fatalf("after %d more changes the manifest names %q with %d changes, want a second fold", refChangeBound+40, second.Refs.Snapshot, len(second.Refs.Changes))
	}
	if again, ok := fake.Get("prefix/" + testRepo + "/" + first.Refs.Snapshot); !ok || !bytes.Equal(again, firstSnapshot) {
		t.Fatal("the first snapshot was rewritten or removed by the fold that replaced it")
	}
	sameReferences(t, "a replica that has never seen the repository", advertise(t, testPackedStorage(t, fake)), want)

	// A tombstone shadows the snapshot, and a reference that only ever lived in
	// the change list leaves nothing behind.
	gone := plumbing.NewTagReferenceName("release/v49.99.4999")
	if err := stor.RemoveReference(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	delete(want, gone.String())
	sameReferences(t, "after a removal", advertise(t, testPackedStorage(t, fake)), want)
}

// TestAColdAdvertisementCostsTheSameHoweverManyReferences prices the start of
// every fetch and push on a replica that has never seen the repository: the
// manifest and the snapshot it names. It was a listing of refs/ and a read of
// every reference.
func TestAColdAdvertisementCostsTheSameHoweverManyReferences(t *testing.T) {
	for _, references := range []int{1000, 3000} {
		fake := newFakeS3(t)
		stor := testPackedStorage(t, fake)
		updates := make([]ReferenceUpdate, 0, references)
		for i := range references {
			updates = append(updates, ReferenceUpdate{Name: plumbing.NewBranchReferenceName(fmt.Sprintf("team-%d/topic-%d", i%7, i)), New: hashOf(i)})
		}
		push, err := stor.BeginPush()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := push.Commit(updates, true); err != nil {
			t.Fatalf("seed: %v", err)
		}

		cold := testPackedStorage(t, fake)
		before := fake.Snapshot()
		if refs := advertise(t, cold); len(refs) != references {
			t.Fatalf("premise: advertised %d references, want %d", len(refs), references)
		}
		if spent := fake.Snapshot().Sub(before); spent.Get != 2 || spent.Total() != 2 {
			t.Fatalf("a cold advertisement of %d references should cost the manifest and the snapshot: %s", references, spent)
		}
	}
}

// TestAManifestOfAnotherFormatIsRefused pins the refusal that stands in for
// migration code. A replica that met a manifest written by a later engine and
// read what it could of it would serve a repository with references or packs
// missing; it must refuse the repository instead, for reads and writes alike.
func TestAManifestOfAnotherFormatIsRefused(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	seedReferences(t, stor, 1, 0)
	data, _ := fake.Get(manifestKey)
	fake.Put(manifestKey, bytes.Replace(data, []byte(`"format":1`), []byte(`"format":2`), 1))
	if later, _ := fake.Get(manifestKey); bytes.Equal(later, data) {
		t.Fatal("premise: the stored manifest's format was not changed")
	}

	reader := testPackedStorage(t, fake)
	failures := map[string]error{}
	_, failures["Reference"] = reader.Reference(testBranch)
	_, failures["IterReferences"] = reader.IterReferences()
	failures["SetReference"] = reader.SetReference(plumbing.NewHashReference(testBranch, hashOf(3)))
	failures["HasEncodedObject"] = reader.HasEncodedObject(hashOf(3))
	_, failures["StoredPacks"] = reader.StoredPacks(context.Background())
	_, failures["Compact"] = reader.Compact(context.Background())
	for operation, err := range failures {
		if !errors.Is(err, ErrManifestFormat) {
			t.Errorf("%s answered %v, want ErrManifestFormat", operation, err)
		}
	}
	for _, garbage := range []string{"", "not json", `{"format":1,"sequence":1,"surprise":true}`, `{"format":1,"packs":[{"name":"../../etc"}]}`,
		`{"format":1,"refs":{"snapshot":"../other/repo/manifest"}}`, `{"format":1,"refs":{"changes":[{"name":"refs/heads/../x","value":"` + hashOf(1).String() + `"}]}}`} {
		if _, err := decodeManifest([]byte(garbage)); !errors.Is(err, ErrManifestFormat) {
			t.Errorf("decoding %q answered %v, want ErrManifestFormat", garbage, err)
		}
	}
}

// TestUnsafeReferenceNamesNeverReachTheBucket covers STORE-049: a crafted
// reference name must be refused before anything is asked of the store, by every
// way of naming a reference.
func TestUnsafeReferenceNamesNeverReachTheBucket(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	for _, name := range []plumbing.ReferenceName{
		"refs/heads/../../evil", "refs/heads/x/../../../other/repo/refs/heads/main", "refs/heads/..",
		`refs/heads/back\slash`, "refs/heads/", "config", "objects/pack/pack-1.pack", "packed-refs", "manifest",
	} {
		ref := plumbing.NewHashReference(name, hashOf(1))
		push, err := stor.BeginPush()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		refusals, pushErr := push.Commit([]ReferenceUpdate{{Name: name, New: hashOf(1)}}, false)
		if pushErr != nil || !errors.Is(refusals[0], ErrUnsafeReferenceName) {
			t.Errorf("a push to %q: %v, %v; want it refused with ErrUnsafeReferenceName", name, refusals, pushErr)
		}
		for operation, err := range map[string]error{
			"SetReference":                   stor.SetReference(ref),
			"CheckAndSetReference":           stor.CheckAndSetReference(ref, nil),
			"CreateReference":                stor.CreateReference(ref),
			"RemoveReference":                stor.RemoveReference(name),
			"RemoveReferenceCAS":             stor.RemoveReferenceCAS(ref),
			"InitializeRepositoryReferences": stor.InitializeRepositoryReferences(ref, false),
		} {
			if !errors.Is(err, ErrUnsafeReferenceName) {
				t.Errorf("%s(%q): %v, want ErrUnsafeReferenceName", operation, name, err)
			}
		}
		if _, err := stor.Reference(name); !errors.Is(err, ErrUnsafeReferenceName) {
			t.Errorf("Reference(%q): %v, want ErrUnsafeReferenceName", name, err)
		}
	}
	if keys := fake.KeysWithPrefix(""); len(keys) != 0 {
		t.Fatalf("refused names reached the store: %v", keys)
	}
}

// TestRepositoryInitializationIsExclusiveOnTheObjectStore covers the first push
// to a new repository arriving twice: one branch is created, HEAD points at it,
// and the loser is told the repository was already initialized.
func TestRepositoryInitializationIsExclusiveOnTheObjectStore(t *testing.T) {
	fake := newFakeS3(t)
	branches := []*plumbing.Reference{
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), hashOf(1)),
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("trunk"), hashOf(2)),
	}
	start := make(chan struct{})
	results := make(chan error, len(branches))
	for _, branch := range branches {
		replica := testPackedStorage(t, fake)
		go func() {
			<-start
			results <- InitializeRepositoryReferences(replica, branch, true)
		}()
	}
	close(start)
	initialized, rejected := 0, 0
	for range branches {
		switch err := <-results; {
		case err == nil:
			initialized++
		case errors.Is(err, ErrReferenceAlreadyExists):
			rejected++
		default:
			t.Fatalf("initialize: %v", err)
		}
	}
	if initialized != 1 || rejected != 1 {
		t.Fatalf("initialized=%d rejected=%d, want one of each", initialized, rejected)
	}

	reader := testPackedStorage(t, fake)
	refs := advertise(t, reader)
	if len(refs) != 2 {
		t.Fatalf("after initialization the repository advertises %v, want HEAD and one branch", refs)
	}
	head, err := reader.Reference(plumbing.HEAD)
	if err != nil || head.Type() != plumbing.SymbolicReference {
		t.Fatalf("HEAD: %v, %v", head, err)
	}
	if _, err := reader.Reference(head.Target()); err != nil {
		t.Fatalf("HEAD points at %s, which does not resolve: %v", head.Target(), err)
	}

	// A second branch may be added to a repository that has one, unless the
	// caller required it empty; the same branch may not be added twice.
	second := plumbing.NewHashReference(plumbing.NewBranchReferenceName("second"), hashOf(3))
	if err := InitializeRepositoryReferences(reader, second, true); !errors.Is(err, ErrReferenceAlreadyExists) {
		t.Fatalf("requiring an empty repository: %v", err)
	}
	if err := InitializeRepositoryReferences(reader, second, false); err != nil {
		t.Fatalf("adding a branch: %v", err)
	}
	if err := InitializeRepositoryReferences(reader, second, false); !errors.Is(err, ErrReferenceAlreadyExists) {
		t.Fatalf("adding the same branch again: %v", err)
	}
	if moved, err := reader.Reference(plumbing.HEAD); err != nil || moved.Target() != head.Target() {
		t.Fatalf("adding a branch moved HEAD to %v (%v)", moved, err)
	}
}

// TestAnOutageIsNeverReportedAsAMissingReference covers STORE-037: a failure to
// read the manifest must surface as an error, never as a reference being
// absent. A push that is told a branch is absent creates it, and would then
// overwrite a live branch — a silent loss of history. The same goes for an
// object: "not found" is a statement about the repository, and an outage is not.
func TestAnOutageIsNeverReportedAsAMissingReference(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.BreakerThreshold = 2
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 3)

	reader := testPackedStorage(t, fake)
	fake.SetFailOn(func(string, string) bool { return true })
	t.Cleanup(func() { fake.SetFailOn(nil) })

	failures := map[string]error{}
	_, failures["Reference"] = reader.Reference(testBranch)
	_, failures["IterReferences"] = reader.IterReferences()
	_, failures["EncodedObject"] = reader.EncodedObject(plumbing.AnyObject, hashes[0])
	failures["HasEncodedObject"] = reader.HasEncodedObject(hashes[0])
	failures["CreateReference"] = reader.CreateReference(plumbing.NewHashReference(testBranch, hashOf(1)))
	failures["CheckAndSetReference"] = reader.CheckAndSetReference(
		plumbing.NewHashReference(testBranch, hashOf(1)), plumbing.NewHashReference(testBranch, hashes[len(hashes)-1]))
	opened := false
	for operation, err := range failures {
		if err == nil {
			t.Errorf("%s succeeded during an outage", operation)
		}
		if errors.Is(err, plumbing.ErrReferenceNotFound) || errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Errorf("%s reported an outage as absence: %v", operation, err)
		}
		opened = opened || errors.Is(err, ErrS3Unavailable)
	}
	if !opened {
		t.Fatal("premise: the run of failures never opened the circuit breaker, so its error was not among those checked")
	}
	fake.SetFailOn(nil)
	resolvesTo(t, "after the outage", testPackedStorage(t, fake), testBranch, hashes[len(hashes)-1])
}

// pushThrough sends a pack through a push transaction and returns it open.
func pushThrough(t *testing.T, stor *repository, pack []byte) PushTransaction {
	t.Helper()
	push, err := stor.BeginPush()
	if err != nil {
		t.Fatalf("begin push: %v", err)
	}
	if err := packfile.UpdateObjectStorage(push, bytes.NewReader(pack)); err != nil {
		t.Fatalf("ingest push: %v", err)
	}
	return push
}

// TestAPushIsOneCommit prices a push and pins its shape: the pack, its index and
// its filter, and ONE conditional write that adds the pack and moves the
// reference together. Until that write nothing of the push is visible, to this
// replica's readers or another's, though the transaction itself reads the pushed
// objects — which is what lets a server decide a push before accepting it.
func TestAPushIsOneCommit(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	if err := Init(stor); err != nil {
		t.Fatalf("init: %v", err)
	}
	pack, hashes := pushPack(t, 40)
	tip := hashes[len(hashes)-1]

	before := fake.Snapshot()
	push := pushThrough(t, stor, pack)
	if _, err := push.EncodedObject(plumbing.CommitObject, tip); err != nil {
		t.Fatalf("the transaction cannot read the commit it was sent: %v", err)
	}
	for who, reader := range map[string]*repository{"the replica that took the push": stor, "another replica": testPackedStorage(t, fake)} {
		if err := reader.HasEncodedObject(tip); !errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Fatalf("%s sees the pushed commit before the push is committed: %v", who, err)
		}
	}
	refusals, err := push.Commit([]ReferenceUpdate{{Name: testBranch, New: tip}}, false)
	if err != nil || refusals[0] != nil {
		t.Fatalf("commit: %v, %v", err, refusals)
	}
	if spent := fake.Snapshot().Sub(before); spent.Put != 4 {
		t.Fatalf("a push should write its pack, index, filter and the manifest once: %s", spent)
	}
	stored := storedManifest(t, fake)
	if len(stored.Packs) != 1 || stored.Packs[0].Source != packSourcePush || stored.Packs[0].Objects != len(hashes) || stored.Sequence != 2 {
		t.Fatalf("the manifest after the push: %+v", stored)
	}
	reader := testPackedStorage(t, fake)
	resolvesTo(t, "after the push", reader, testBranch, tip)
	readObjects(t, reader, hashes)
}

// TestARefusedPushLeavesItsPackInvisible is the quarantine rule. A push whose
// reference update is refused — the branch moved while the pack was uploading —
// must leave nothing a reader can see: before the manifest, the pack was live
// the moment it was uploaded, whatever became of the push. What it does leave is
// an upload no manifest names, for a compaction to sweep.
func TestARefusedPushLeavesItsPackInvisible(t *testing.T) {
	fake := newFakeS3(t)
	fake.clock = newTestClock()
	stor := testPackedStorage(t, fake)
	base := seedObjects(t, stor, 2)
	pack, hashes := pushPack(t, 30)
	tip := hashes[len(hashes)-1]

	push := pushThrough(t, stor, pack)
	stale := ReferenceUpdate{Name: testBranch, Old: hashOf(12345), New: tip}
	refusals, err := push.Commit([]ReferenceUpdate{stale}, false)
	if err != nil || !errors.Is(refusals[0], gitStorage.ErrReferenceHasChanged) {
		t.Fatalf("a push from a stale branch answered %v, %v; want ErrReferenceHasChanged", refusals, err)
	}
	if uploaded := packKeys(fake, ".pack"); len(uploaded) != 1 {
		t.Fatalf("premise: the refused push uploaded %d packs, want 1", len(uploaded))
	}
	for who, reader := range map[string]*repository{"the replica that took the push": stor, "another replica": testPackedStorage(t, fake)} {
		packs, err := reader.StoredPacks(context.Background())
		if err != nil || len(packs) != 0 {
			t.Fatalf("%s lists %v as stored packs (%v), want none", who, packs, err)
		}
		if err := reader.HasEncodedObject(tip); !errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Fatalf("%s can read the refused push's commit: %v", who, err)
		}
		resolvesTo(t, who, reader, testBranch, base[len(base)-1])
	}

	// The upload is swept: listed as a retired orphan once it has lain there a
	// grace period, and deleted a grace period after that, never before.
	sweep := func(when string) CompactionResult {
		t.Helper()
		result, err := stor.Compact(context.Background())
		if err != nil {
			t.Fatalf("compact %s: %v", when, err)
		}
		return result
	}
	if result := sweep("at once"); len(result.Orphans) != 0 || len(packKeys(fake, ".pack")) != 1 {
		t.Fatalf("a sweep took an upload of a moment ago for an orphan: %+v", result)
	}
	fake.clock.Advance(retiredPackGrace + time.Minute)
	if result := sweep("after a grace period"); len(result.Orphans) != 1 || len(result.RetiredPacks) != 0 || len(packKeys(fake, ".pack")) != 1 {
		t.Fatalf("after a grace period the sweep should list the orphan and delete nothing: %+v", result)
	}
	if retired := storedManifest(t, fake).Retired; len(retired) != 1 || !retired[0].Orphan {
		t.Fatalf("the manifest's retired list is %+v, want the orphan", retired)
	}
	fake.clock.Advance(retiredPackGrace + time.Minute)
	if result := sweep("after another"); len(result.RetiredPacks) != 1 {
		t.Fatalf("after its grace period as a retired orphan the upload should be deleted: %+v", result)
	}
	if left := fake.KeysWithPrefix("prefix/" + testRepo + "/objects/pack/"); len(left) != 0 {
		t.Fatalf("the sweep left %v", left)
	}
	if retired := storedManifest(t, fake).Retired; len(retired) != 0 {
		t.Fatalf("the manifest still lists %+v as retired", retired)
	}
}

// TestAnAtomicPushMovesEveryReferenceOrNone pins `atomic`. The server used to
// write the references one by one and undo them on a failure, which left a
// window in which a fetch saw half a push; one write of the manifest has none.
// Without atomic the same push moves what it can and reports the rest.
func TestAnAtomicPushMovesEveryReferenceOrNone(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	one, two := plumbing.NewBranchReferenceName("one"), plumbing.NewBranchReferenceName("two")
	for _, name := range []plumbing.ReferenceName{one, two} {
		if err := stor.SetReference(plumbing.NewHashReference(name, hashOf(0))); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	pack, hashes := pushPack(t, 5)
	tip := hashes[len(hashes)-1]
	updates := []ReferenceUpdate{
		{Name: one, Old: hashOf(0), New: tip},
		{Name: two, Old: hashOf(999), New: tip},
		{Name: plumbing.NewBranchReferenceName("three"), New: tip},
	}

	refusals, err := pushThrough(t, stor, pack).Commit(updates, true)
	if err != nil {
		t.Fatalf("atomic push: %v", err)
	}
	if !errors.Is(refusals[0], ErrPushAborted) || !errors.Is(refusals[1], gitStorage.ErrReferenceHasChanged) || !errors.Is(refusals[2], ErrPushAborted) {
		t.Fatalf("an atomic push with one stale update answered %v", refusals)
	}
	reader := testPackedStorage(t, fake)
	resolvesTo(t, "after the refused atomic push", reader, one, hashOf(0))
	if _, err := reader.Reference(plumbing.NewBranchReferenceName("three")); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("a refused atomic push created a branch: %v", err)
	}
	if err := reader.HasEncodedObject(tip); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a refused atomic push left its objects readable: %v", err)
	}

	refusals, err = pushThrough(t, stor, pack).Commit(updates, false)
	if err != nil || refusals[0] != nil || !errors.Is(refusals[1], gitStorage.ErrReferenceHasChanged) || refusals[2] != nil {
		t.Fatalf("the same push without atomic answered %v, %v", refusals, err)
	}
	reader = testPackedStorage(t, fake)
	resolvesTo(t, "after the push without atomic", reader, one, tip)
	resolvesTo(t, "after the push without atomic", reader, two, hashOf(0))
	resolvesTo(t, "after the push without atomic", reader, plumbing.NewBranchReferenceName("three"), tip)
	readObjects(t, reader, hashes)

	// A create of a reference that exists is its own refusal.
	refusals, err = pushThrough(t, stor, pack).Commit([]ReferenceUpdate{{Name: one, New: tip}}, false)
	if err != nil || !errors.Is(refusals[0], ErrReferenceAlreadyExists) {
		t.Fatalf("creating a branch that exists answered %v, %v", refusals, err)
	}
	if _, err := pushThrough(t, stor, pack).Commit(nil, false); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("a push with no reference update was committed: %v", err)
	}
}
