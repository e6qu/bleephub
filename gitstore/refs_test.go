package gitstore

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

const testBranch = plumbing.ReferenceName("refs/heads/main")

func refKey(name plumbing.ReferenceName) string { return "prefix/" + testRepo + "/" + name.String() }

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

// TestResolvingAReferenceCostsOneRequest pins what resolving a branch costs:
// one GET. Read as a file on a disk is — stat it, then open it — it was a HEAD
// and a GET, and a server resolves a branch many times in the course of one
// push or clone.
func TestResolvingAReferenceCostsOneRequest(t *testing.T) {
	fake := newFakeS3(t)
	// No reuse between reads, so that what one resolution costs is what is priced.
	fake.opts.IndexFreshness = -1
	writer := testPackedStorage(t, fake)
	if err := writer.SetReference(plumbing.NewHashReference(testBranch, hashOf(1))); err != nil {
		t.Fatalf("set: %v", err)
	}

	reader := testPackedStorage(t, fake)
	for round := range 2 {
		before := fake.Snapshot()
		resolvesTo(t, "resolve", reader, testBranch, hashOf(1))
		spent := fake.Snapshot().Sub(before)
		if spent.Head != 0 || spent.Get != 1 || spent.Total() != 1 {
			t.Fatalf("round %d: resolving a reference should cost one GET and nothing else: %s", round, spent)
		}
	}
}

// TestAReadMadeInOrderToCompareAlwaysReadsTheStore covers the guard that keeps
// every saving on reference reads from weakening the compare-and-set: whatever
// this handle remembers of a reference, the comparison a push, a create or a
// conditional delete turns on is made against the store. Another replica moves
// the branch inside the freshness bound; a plain read lags it, as the bound
// allows, and none of the three conditional writes may.
func TestAReadMadeInOrderToCompareAlwaysReadsTheStore(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
	fake.clock = newTestClock()
	stor := testPackedStorage(t, fake)
	first := plumbing.NewHashReference(testBranch, hashOf(1))
	if err := stor.SetReference(first); err != nil {
		t.Fatalf("set: %v", err)
	}
	created := plumbing.NewBranchReferenceName("created-elsewhere")
	if _, err := stor.Reference(created); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("premise: %v", err)
	}

	fake.Put(refKey(testBranch), []byte(hashOf(2).String()+"\n"))
	fake.Put(refKey(created), []byte(hashOf(3).String()+"\n"))
	resolvesTo(t, "premise: a plain read inside the bound", stor, testBranch, hashOf(1))
	if _, err := stor.Reference(created); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("premise: a plain read inside the bound saw the other replica's branch: %v", err)
	}

	before := fake.Snapshot()
	next := plumbing.NewHashReference(testBranch, hashOf(9))
	if err := stor.CheckAndSetReference(next, first); !errors.Is(err, gitStorage.ErrReferenceHasChanged) {
		t.Fatalf("a compare-and-set against a value another replica had replaced: %v, want ErrReferenceHasChanged", err)
	}
	if err := RemoveReferenceCAS(stor, first); !errors.Is(err, gitStorage.ErrReferenceHasChanged) {
		t.Fatalf("a conditional delete against a value another replica had replaced: %v, want ErrReferenceHasChanged", err)
	}
	if err := CreateReferenceIfAbsent(stor, plumbing.NewHashReference(created, hashOf(9))); !errors.Is(err, ErrReferenceAlreadyExists) {
		t.Fatalf("creating a branch another replica had created: %v, want ErrReferenceAlreadyExists", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Get < 3 || spent.Put != 0 {
		t.Fatalf("three refused writes cost %s, want a read of the store each and no write", spent)
	}
	if got, _ := fake.Get(refKey(testBranch)); strings.TrimSpace(string(got)) != hashOf(2).String() {
		t.Fatalf("a refused write changed the branch to %q", got)
	}

	// Against the value the store does hold, the same writes go through.
	moved := plumbing.NewHashReference(testBranch, hashOf(2))
	if err := stor.CheckAndSetReference(next, moved); err != nil {
		t.Fatalf("a compare-and-set against the current value: %v", err)
	}
	resolvesTo(t, "after the compare-and-set", stor, testBranch, hashOf(9))
	if err := RemoveReferenceCAS(stor, next); err != nil {
		t.Fatalf("a conditional delete against the current value: %v", err)
	}
	if _, ok := fake.Get(refKey(testBranch)); ok {
		t.Fatal("a conditional delete left the branch in the store")
	}

	// A compare-and-set of a branch that is not there is refused as go-git
	// refuses it, and an unconditional one (no old value) creates it.
	if err := stor.CheckAndSetReference(next, first); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("a compare-and-set of a missing branch: %v", err)
	}
	if err := stor.CheckAndSetReference(next, nil); err != nil {
		t.Fatalf("an unconditional set: %v", err)
	}
	resolvesTo(t, "after the unconditional set", stor, testBranch, hashOf(9))
}

// TestReferenceReadsWithinTheFreshnessBoundAreOneRead covers the reuse a server
// depends on — it resolves a branch a dozen times in one push — and each thing
// that must end it: a local write, and the bound itself.
func TestReferenceReadsWithinTheFreshnessBoundAreOneRead(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
	fake.clock = newTestClock()
	stor := testPackedStorage(t, fake)
	if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(1))); err != nil {
		t.Fatalf("set: %v", err)
	}

	before := fake.Snapshot()
	for range 12 {
		resolvesTo(t, "a branch just written", stor, testBranch, hashOf(1))
	}
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("a branch written through this handle was read back from the store: %s", spent)
	}
	// A reference that is not there is remembered as well: the loose key, and
	// the packed-refs object behind it.
	for range 5 {
		if _, err := stor.Reference("refs/heads/absent"); !errors.Is(err, plumbing.ErrReferenceNotFound) {
			t.Fatalf("an absent branch: %v", err)
		}
	}
	if spent := fake.Snapshot().Sub(before); spent.Get != 2 || spent.Total() != 2 {
		t.Fatalf("five misses should cost one read of the branch and one of packed-refs: %s", spent)
	}

	// A branch this replica did not write costs its first read and no more.
	elsewhere := plumbing.NewBranchReferenceName("elsewhere")
	fake.Put(refKey(elsewhere), []byte(hashOf(7).String()+"\n"))
	before = fake.Snapshot()
	for range 12 {
		resolvesTo(t, "a branch written elsewhere", stor, elsewhere, hashOf(7))
	}
	if spent := fake.Snapshot().Sub(before); spent.Total() != 1 {
		t.Fatalf("twelve resolutions of one branch should cost one read: %s", spent)
	}

	// A local write ends the reuse at once.
	fake.Put(refKey(testBranch), []byte(hashOf(2).String()+"\n"))
	if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(3))); err != nil {
		t.Fatalf("set: %v", err)
	}
	resolvesTo(t, "after a local write", stor, testBranch, hashOf(3))
	if err := stor.RemoveReference(elsewhere); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := stor.Reference(elsewhere); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("a branch removed through this handle still resolves: %v", err)
	}

	// And so does the bound running out.
	fake.Put(refKey(testBranch), []byte(hashOf(4).String()+"\n"))
	resolvesTo(t, "inside the bound", stor, testBranch, hashOf(3))
	fake.clock.Advance(2 * time.Hour)
	resolvesTo(t, "after the bound ran out", stor, testBranch, hashOf(4))
}

// TestAnAdvertisementIsOneListingHoweverManyReferences pins what listing a
// repository's references costs. Walked as a disk is, it was a LIST for each
// directory under refs/ and a GET for each reference, every time. It is one
// recursive LIST, and a GET only for a reference whose version this replica has
// not read — so the second advertisement of sixty references reads none.
func TestAnAdvertisementIsOneListingHoweverManyReferences(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Second
	fake.clock = newTestClock()
	writer := testPackedStorage(t, fake)
	want := seedReferences(t, writer, 40, 20)

	reader := testPackedStorage(t, fake)
	before := fake.Snapshot()
	sameReferences(t, "first advertisement", advertise(t, reader), want)
	first := fake.Snapshot().Sub(before)
	if first.List != 1 {
		t.Fatalf("the first advertisement took %d listings, want one recursive listing: %s", first.List, first)
	}
	if first.Get < 60 {
		t.Fatalf("premise: the first advertisement read only %d of 60 references: %s", first.Get, first)
	}

	// Inside the bound the next client is answered from what the last one learned.
	before = fake.Snapshot()
	sameReferences(t, "an advertisement inside the bound", advertise(t, reader), want)
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("an advertisement inside the bound cost %s", spent)
	}

	fake.clock.Advance(3 * time.Second)
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

	fake.clock.Advance(3 * time.Second)
	before = fake.Snapshot()
	sameReferences(t, "after another replica's writes", advertise(t, reader), want)
	third := fake.Snapshot().Sub(before)
	if third.List != 1 || third.Get > 4 {
		t.Fatalf("an advertisement after two references changed cost %s, want 1 listing and only their reads", third)
	}
	// What the listing showed answers the resolutions that follow it.
	before = fake.Snapshot()
	resolvesTo(t, "after the advertisement", reader, moved, hashOf(7777))
	if _, err := reader.Reference(gone); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("a branch the listing no longer shows still resolves: %v", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("resolving what an advertisement had just read cost %s", spent)
	}
}

// TestAReplicaSeesItsOwnReferenceWritesAtOnce pins that the listing is never
// what hides a write made through the same handle: inside the freshness bound,
// with a listing held, a new branch, a moved one and a deleted one are all as
// the writer left them.
func TestAReplicaSeesItsOwnReferenceWritesAtOnce(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
	fake.clock = newTestClock()
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
	resolvesTo(t, "the moved branch", stor, moved, hashOf(4242))
	if _, err := stor.Reference(gone); err == nil {
		t.Fatal("a removed branch still resolves")
	}
}

// TestWithReuseOffEveryAdvertisementReadsEveryReference pins the off switch:
// with no staleness allowed, nothing is answered from a listing or a read made
// earlier, and every reference comes from the store each time.
func TestWithReuseOffEveryAdvertisementReadsEveryReference(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = -1
	stor := testPackedStorage(t, fake)
	want := seedReferences(t, stor, 5, 0)

	for round := range 2 {
		before := fake.Snapshot()
		sameReferences(t, "advertisement", advertise(t, stor), want)
		if spent := fake.Snapshot().Sub(before); spent.Get < 5 || spent.List < 1 {
			t.Fatalf("round %d: with reuse off the advertisement cost %s, want a listing and a read of every reference", round, spent)
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

// TestPackedReferencesAreReadWithGitsPrecedence covers a repository that arrives
// with a packed-refs file, as one copied in from a disk does. A packed reference
// resolves and is advertised; a loose one of the same name shadows it; and
// removing a reference removes it from both places, because deleting only the
// loose one would bring the packed one back from the dead.
func TestPackedReferencesAreReadWithGitsPrecedence(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = -1
	packed := "# pack-refs with: peeled fully-peeled sorted \n" +
		hashOf(1).String() + " refs/heads/main\n" +
		hashOf(2).String() + " refs/heads/packed-only\n" +
		hashOf(3).String() + " refs/tags/v1\n" +
		"^" + hashOf(4).String() + "\n" +
		hashOf(5).String() + " refs/tags/v2\n"
	fake.Put(refKey(packedRefsName), []byte(packed))
	fake.Put(refKey(testBranch), []byte(hashOf(100).String()+"\n"))
	fake.Put(refKey(plumbing.HEAD), []byte("ref: refs/heads/main\n"))

	stor := testPackedStorage(t, fake)
	resolvesTo(t, "a loose reference shadows a packed one", stor, testBranch, hashOf(100))
	resolvesTo(t, "a packed reference", stor, "refs/heads/packed-only", hashOf(2))
	head, err := stor.Reference(plumbing.HEAD)
	if err != nil || head.Type() != plumbing.SymbolicReference || head.Target() != testBranch {
		t.Fatalf("HEAD: %v, %v", head, err)
	}
	got := advertise(t, stor)
	want := map[string]string{
		"refs/heads/main": hashOf(100).String(), "refs/heads/packed-only": hashOf(2).String(),
		"refs/tags/v1": hashOf(3).String(), "refs/tags/v2": hashOf(5).String(),
	}
	sameReferences(t, "advertisement", got, want)
	if _, ok := got["HEAD"]; !ok {
		t.Fatal("HEAD was not advertised")
	}
	if count, err := stor.CountLooseRefs(); err != nil || count != 1 {
		t.Fatalf("loose references: %d, %v; want the one loose branch", count, err)
	}

	for _, name := range []plumbing.ReferenceName{testBranch, "refs/tags/v1"} {
		if err := stor.RemoveReference(name); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
		if _, err := stor.Reference(name); !errors.Is(err, plumbing.ErrReferenceNotFound) {
			t.Fatalf("%s still resolves after its removal: %v", name, err)
		}
	}
	left, _ := fake.Get(refKey(packedRefsName))
	if wantLeft := "# pack-refs with: peeled fully-peeled sorted \n" +
		hashOf(2).String() + " refs/heads/packed-only\n" +
		hashOf(5).String() + " refs/tags/v2\n"; string(left) != wantLeft {
		t.Fatalf("packed-refs after the removals:\n%s\nwant:\n%s", left, wantLeft)
	}
	if err := stor.PackRefs(); err != nil {
		t.Fatalf("pack refs: %v", err)
	}
}

// TestUnsafeReferenceNamesNeverReachTheBucket covers STORE-049 on the object
// store, where a reference is stored under its own name: a crafted name must
// not reach another repository's keys, nor this repository's other files.
func TestUnsafeReferenceNamesNeverReachTheBucket(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	for _, name := range []plumbing.ReferenceName{
		"refs/heads/../../evil", "refs/heads/x/../../../other/repo/refs/heads/main", "refs/heads/..",
		`refs/heads/back\slash`, "refs/heads/", "config", "objects/pack/pack-1.pack", "packed-refs",
	} {
		ref := plumbing.NewHashReference(name, hashOf(1))
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
	if spent := fake.Snapshot(); spent.Total() != 0 {
		t.Fatalf("refused names reached the store: %s", spent)
	}
}

// countingLocker is a durable lock manager that grants every lock, after
// refusing it once, and keeps count.
type countingLocker struct {
	mu       sync.Mutex
	held     map[string]string
	refused  map[string]bool
	acquired int
}

func (l *countingLocker) AcquireLock(name, owner string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.refused[name] {
		// The first attempt finds the lock held, as it would while another
		// replica had it: the caller must come back for it.
		l.refused[name] = true
		return false, nil
	}
	if _, held := l.held[name]; held {
		return false, nil
	}
	l.held[name] = owner
	l.acquired++
	return true, nil
}

func (l *countingLocker) ReleaseLock(name, owner string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[name] != owner {
		return fmt.Errorf("lock %s is not held by %s", name, owner)
	}
	delete(l.held, name)
	return nil
}

// TestConcurrentCompareAndSetsHaveOneWinner is the guarantee every push depends
// on: of several writers moving one branch from the same tip — through handles
// of their own, as replicas are — exactly one is told it did. It runs with a
// durable locker installed, which is how more than one replica is run.
func TestConcurrentCompareAndSetsHaveOneWinner(t *testing.T) {
	locker := &countingLocker{held: map[string]string{}, refused: map[string]bool{}}
	SetGitObjectLocker(locker)
	t.Cleanup(func() { ClearGitObjectLocker(locker) })

	fake := newFakeS3(t)
	tip := plumbing.NewHashReference(testBranch, hashOf(0))
	if err := testPackedStorage(t, fake).SetReference(tip); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const writers = 8
	start := make(chan struct{})
	results := make(chan error, writers)
	for writer := range writers {
		replica := testPackedStorage(t, fake)
		go func() {
			<-start
			results <- replica.CheckAndSetReference(plumbing.NewHashReference(testBranch, hashOf(writer+1)), tip)
		}()
	}
	close(start)
	moved, refused := 0, 0
	for range writers {
		switch err := <-results; {
		case err == nil:
			moved++
		case errors.Is(err, gitStorage.ErrReferenceHasChanged):
			refused++
		default:
			t.Fatalf("compare-and-set: %v", err)
		}
	}
	if moved != 1 || refused != writers-1 {
		t.Fatalf("%d writers moved the branch and %d were refused, want exactly one winner", moved, refused)
	}
	if locker.acquired < writers {
		t.Fatalf("premise: the durable lock was taken %d times for %d writes", locker.acquired, writers)
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
// read a reference must surface as an error, never as the reference being
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
	if got, _ := fake.Get(refKey(testBranch)); strings.TrimSpace(string(got)) != hashes[len(hashes)-1].String() {
		t.Fatalf("a write made during the outage changed the branch to %q", got)
	}
}
