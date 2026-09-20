package gitstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// TestAStoreHandsOutOneHandlePerRepository pins what makes one request's listing
// answer the next: a repository's snapshots belong to its handle, so the store
// gives every caller the same one. A second handle would pay again to learn what
// the first already knew.
func TestAStoreHandsOutOneHandlePerRepository(t *testing.T) {
	fake := newFakeS3(t)
	store := fake.store("prefix")
	first, err := store.Repository(testRepo)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	if err := packfile.UpdateObjectStorage(first, bytes.NewReader(firstOf(pushPack(t, 20)))); err != nil {
		t.Fatalf("push: %v", err)
	}
	again, err := store.Repository(testRepo)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	if again != first {
		t.Fatal("the store handed out a second handle on one repository")
	}
	other, err := store.Repository("octocat/other")
	if err != nil || other == first {
		t.Fatalf("another repository shares the handle: %v", err)
	}
	if store.Prefix() != "prefix" || store.Bucket().Name() != "bucket" {
		t.Fatalf("store reports %q in %q", store.Prefix(), store.Bucket().Name())
	}

	for _, name := range []string{"", "octocat", "octocat/repo/extra", "../octocat/repo", "/octocat/repo", `octocat\repo`, "octocat/.."} {
		if _, err := store.Repository(name); err == nil {
			t.Errorf("a repository was opened under the unsafe name %q", name)
		}
	}
}

func firstOf(pack []byte, _ []plumbing.Hash) []byte { return pack }

// TestASubStoreSharesTheConnectionAndNotTheKeys covers the sibling prefix an
// application keeps its other bytes under: same bucket, same breaker, same base
// context, and a key space of its own.
func TestASubStoreSharesTheConnectionAndNotTheKeys(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.BreakerThreshold = 1
	store := fake.store("git")
	sibling := store.Sub("bytes")
	if sibling.Prefix() != "bytes" || sibling.Bucket() != store.Bucket() {
		t.Fatalf("the sibling is %q on %v", sibling.Prefix(), sibling.Bucket())
	}
	for _, each := range []*Store{store, sibling} {
		stor, err := each.Repository(testRepo)
		if err != nil {
			t.Fatalf("repository: %v", err)
		}
		if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(1))); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	if keys := strings.Join(fake.KeysWithPrefix(""), ","); keys != "bytes/"+testRepo+"/refs/heads/main,git/"+testRepo+"/refs/heads/main" {
		t.Fatalf("the two stores wrote %s", keys)
	}

	// One failure through the sibling opens the breaker both share.
	fake.SetFailOn(func(string, string) bool { return true })
	stor, _ := sibling.Repository(testRepo)
	if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(2))); err == nil {
		t.Fatal("a write succeeded during an outage")
	}
	fake.SetFailOn(nil)
	stor, _ = store.Repository(testRepo)
	if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(2))); !errors.Is(err, ErrS3Unavailable) {
		t.Fatalf("the sibling's outage did not open this store's breaker: %v", err)
	}
}

// TestCancellingTheBaseContextStopsStoreCalls pins that every request derives
// from the context the server installs, so a process that is shutting down does
// not leave object-store I/O running behind it.
func TestCancellingTheBaseContextStopsStoreCalls(t *testing.T) {
	fake := newFakeS3(t)
	store := fake.store("prefix")
	stor, err := store.Repository(testRepo)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(1))); err != nil {
		t.Fatalf("premise: a write before shutdown: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	store.SetBaseContext(ctx)
	cancel()
	before := fake.Snapshot()
	if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(2))); !errors.Is(err, context.Canceled) {
		t.Fatalf("a write after shutdown: %v, want the cancellation", err)
	}
	if _, err := stor.Reference("refs/heads/unread"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a read after shutdown: %v, want the cancellation", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Put != 0 {
		t.Fatalf("a write after shutdown reached the store: %s", spent)
	}
}

// TestARepositoryIsCopiedRenamedAndDeletedWhole covers the repository lifecycle
// an application drives: a fork or rename copies every key, the source survives
// a copy and not a rename, and a handle opened afterwards sees what is there now
// rather than what its predecessor remembered.
func TestARepositoryIsCopiedRenamedAndDeletedWhole(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactionTrigger = -1
	store := fake.store("prefix")
	source, err := store.Repository("octocat/source")
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	pack, hashes := pushPack(t, 40)
	if err := packfile.UpdateObjectStorage(source, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}
	loose := storeBlob(t, source, "a loose object rides along")
	tip := hashes[len(hashes)-1]
	if err := source.SetReference(plumbing.NewHashReference(testBranch, tip)); err != nil {
		t.Fatalf("set: %v", err)
	}
	sourceKeys := len(fake.KeysWithPrefix("prefix/octocat/source/"))

	holds := func(name string) error {
		stor, err := store.Repository(name)
		if err != nil {
			return err
		}
		ref, err := stor.Reference(testBranch)
		if err != nil {
			return err
		}
		if ref.Hash() != tip {
			t.Fatalf("%s: branch is %s, want %s", name, ref.Hash(), tip)
		}
		for _, hash := range append([]plumbing.Hash{loose}, hashes...) {
			if _, err := stor.EncodedObject(plumbing.AnyObject, hash); err != nil {
				return err
			}
		}
		return nil
	}

	// A handle on the destination from before the copy has seen it empty.
	stale, _ := store.Repository("octocat/copy")
	if err := stale.HasEncodedObject(tip); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("premise: %v", err)
	}
	if err := store.CopyRepository("octocat/source", "octocat/copy"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	for _, name := range []string{"octocat/source", "octocat/copy"} {
		if err := holds(name); err != nil {
			t.Fatalf("after the copy, %s: %v", name, err)
		}
	}
	if got := len(fake.KeysWithPrefix("prefix/octocat/copy/")); got != sourceKeys {
		t.Fatalf("the copy has %d keys, the source %d", got, sourceKeys)
	}

	if err := store.RenameRepository("octocat/copy", "octocat/renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := holds("octocat/renamed"); err != nil {
		t.Fatalf("after the rename: %v", err)
	}
	if keys := fake.KeysWithPrefix("prefix/octocat/copy/"); len(keys) != 0 {
		t.Fatalf("the rename left %v behind", keys)
	}
	gone, _ := store.Repository("octocat/copy")
	if _, err := gone.Reference(testBranch); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("the renamed-away repository still resolves its branch: %v", err)
	}

	if err := store.DeleteRepository("octocat/source"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if keys := fake.KeysWithPrefix("prefix/octocat/source/"); len(keys) != 0 {
		t.Fatalf("the delete left %v behind", keys)
	}
	if err := holds("octocat/renamed"); err != nil {
		t.Fatalf("deleting the source damaged the renamed copy: %v", err)
	}
	for _, unsafe := range []string{"../escape", "octocat"} {
		if store.DeleteRepository(unsafe) == nil || store.CopyRepository(unsafe, "octocat/x") == nil || store.CopyRepository("octocat/renamed", unsafe) == nil {
			t.Fatalf("a lifecycle operation accepted the unsafe name %q", unsafe)
		}
	}
}

// TestAListingFollowsEveryPage proves the engine reads a listing to its end. The
// fake pages at a thousand keys as the real service does, which is also what
// makes a listing's request count comparable to it.
func TestAListingFollowsEveryPage(t *testing.T) {
	fake := newFakeS3(t)
	const references = 2500
	for i := range references {
		fake.Put(refKey(plumbing.NewBranchReferenceName(padHex(i))), []byte(hashOf(i).String()+"\n"))
	}
	stor := testPackedStorage(t, fake)
	before := fake.Snapshot()
	count, err := stor.CountLooseRefs()
	if err != nil || count != references {
		t.Fatalf("counted %d references (err %v), want %d", count, err, references)
	}
	if spent := fake.Snapshot().Sub(before); spent.List != 3 || spent.Total() != 3 {
		t.Fatalf("listing %d keys cost %s, want 3 pages of 1000", references, spent)
	}
}

func padHex(i int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 8)
	for pos := 7; pos >= 0; pos-- {
		out[pos] = digits[i&0xf]
		i >>= 4
	}
	return string(out)
}

// TestOpenS3ReachesAnEndpointByURL covers the constructor an application uses:
// an endpoint URL, a region and credentials, which is all the fake needs too.
func TestOpenS3ReachesAnEndpointByURL(t *testing.T) {
	fake := newFakeS3(t)
	store, err := OpenS3(context.Background(), fake.URL(), "bucket", "prefix", Options{
		Credentials: credentials.NewStaticV4("fake", "fake", ""),
		CacheDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	stor, err := store.Repository(testRepo)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(testBranch, hashOf(1))); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, ok := fake.Get(refKey(testBranch)); !ok || strings.TrimSpace(string(got)) != hashOf(1).String() {
		t.Fatalf("the branch in the bucket is %q", got)
	}
	if _, err := OpenS3(context.Background(), "http://bad host/", "bucket", "prefix", Options{}); err == nil {
		t.Fatal("an endpoint that is not a URL was accepted")
	}
}
