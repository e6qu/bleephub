package bleephub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/internal/gitbackend"
	"github.com/e6qu/bleephub/internal/server/testutil"
)

// newFakeObjectStoreGitServerForTest brings up a server whose repositories live
// in the in-process fake of one object-store driver, configured from the
// environment as a deployment on that driver is, so the object-store git path is
// exercised wherever the suite runs and not only where a container can be
// started. It cannot be parallel: the storage backend is chosen from the process
// environment.
func newFakeObjectStoreGitServerForTest(t *testing.T, driver string) *isolatedServer {
	t.Helper()
	t.Setenv("BLEEPHUB_GIT_DIR", "")
	testutil.ConfigureFakeObjectStore(t, driver, "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_BUCKET", "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_PREFIX", "git")
	t.Setenv("BLEEPHUB_GITSTORE_CACHE_DIR", t.TempDir())
	resetGitObjectStoreForTest(t)
	return newIsolatedServer(t)
}

// largeIncompressibleFile is bytes zlib cannot shrink, the same on every run, so
// the pack that carries them is as large as they are.
func largeIncompressibleFile(size int) []byte {
	body := make([]byte, 0, size+sha256.Size)
	link := sha256.Sum256([]byte("a large file"))
	for len(body) < size {
		body = append(body, link[:]...)
		link = sha256.Sum256(link[:])
	}
	return body[:size]
}

// TestStockGitRoundTripsALargeFileThroughTheObjectStoreEngine drives the whole
// object-store path with the stock git client: a push whose pack outgrows git's
// post buffer, landing as a pack in the bucket; a fresh clone served from it;
// and a read of the file through the storage engine, which hands an object this
// large out as a stream from its pack rather than as bytes in memory. Each of
// those was a defect or a gap once, and none needs a container to check. It
// runs once for every driver a deployment can choose, because what it protects
// is a property of the server on that driver and not of the engine alone: the
// pack is an upload and the read a ranged request, and each store has its own
// way of saying both.
func TestStockGitRoundTripsALargeFileThroughTheObjectStoreEngine(t *testing.T) {
	for _, driver := range testutil.ObjectStoreDrivers {
		t.Run(driver, func(t *testing.T) {
			stockGitRoundTripsALargeFile(t, driver)
		})
	}
}

func stockGitRoundTripsALargeFile(t *testing.T, driver string) {
	git := requireGitCLI(t)
	srv := newFakeObjectStoreGitServerForTest(t, driver)
	const name = "large-file"
	seedGitShallowRepo(t, srv.Server, name)

	cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/" + name + ".git"
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	git.run(root, "clone", cloneURL, clone)
	git.run(clone, "config", "user.name", "Large File")
	git.run(clone, "config", "user.email", "large@bleephub.invalid")

	// Past the engine's streaming threshold, and so well past the post buffer.
	payload := largeIncompressibleFile(9 << 20)
	if err := os.WriteFile(filepath.Join(clone, "large.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	git.run(clone, "add", "large.bin")
	git.run(clone, "commit", "-m", "a large file")
	pushed := strings.TrimSpace(git.run(clone, "rev-parse", "HEAD"))
	blob := plumbing.NewHash(strings.TrimSpace(git.run(clone, "rev-parse", "HEAD:large.bin")))
	git.run(clone, "push", "origin", "HEAD:main")

	verify := filepath.Join(root, "verify")
	git.run(root, "clone", cloneURL, verify)
	if got := strings.TrimSpace(git.run(verify, "rev-parse", "HEAD")); got != pushed {
		t.Fatalf("a fresh clone is at %s, want the pushed %s", got, pushed)
	}
	landed, err := os.ReadFile(filepath.Join(verify, "large.bin"))
	if err != nil || !bytes.Equal(landed, payload) {
		t.Fatalf("the large file did not survive the round trip (err %v, %d bytes)", err, len(landed))
	}

	stor := srv.store.GetGitStorage("admin", name)
	if stor == nil {
		t.Fatal("the repository has no git storage")
	}
	object, err := stor.EncodedObject(plumbing.BlobObject, blob)
	if err != nil {
		t.Fatalf("read the blob through the engine: %v", err)
	}
	if object.Size() != int64(len(payload)) {
		t.Fatalf("the engine reports the blob as %d bytes, want %d", object.Size(), len(payload))
	}
	reader, err := object.Reader()
	if err != nil {
		t.Fatalf("open the blob: %v", err)
	}
	read, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil || !bytes.Equal(read, payload) {
		t.Fatalf("the engine read the blob back wrong (err %v, %d bytes)", err, len(read))
	}
}

// TestARefusedStockGitPushLeavesItsObjectsInvisible drives the quarantine rule
// with the stock git client. A push to a repository that has been archived is
// refused after its pack has been uploaded — the server decides a push with the
// pushed commits to hand — and what it uploaded must then be part
// of nothing: not a stored pack, not a readable commit, only keys in the bucket
// that no manifest names, for a compaction to sweep. When references and packs
// were published separately the pack was live from the moment it landed.
func TestARefusedStockGitPushLeavesItsObjectsInvisible(t *testing.T) {
	git := requireGitCLI(t)
	srv := newFakeObjectStoreGitServerForTest(t, "s3")
	const name = "quarantine"
	seedGitShallowRepo(t, srv.Server, name)
	cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/" + name + ".git"
	root := t.TempDir()
	commitIn := func(dir, file string) string {
		t.Helper()
		git.run(root, "clone", cloneURL, dir)
		git.run(dir, "config", "user.name", "Quarantine")
		git.run(dir, "config", "user.email", "quarantine@bleephub.invalid")
		if err := os.WriteFile(filepath.Join(dir, file), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		git.run(dir, "add", file)
		git.run(dir, "commit", "-m", file)
		return strings.TrimSpace(git.run(dir, "rev-parse", "HEAD"))
	}
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	winner := commitIn(first, "winner.txt")
	loser := commitIn(second, "loser.txt")
	git.run(first, "push", "origin", "HEAD:main")

	stor, ok := srv.store.GetGitStorage("admin", name).(gitstore.PackSource)
	if !ok {
		t.Fatal("premise: the repository is not in the object store")
	}
	before, err := stor.StoredPacks(context.Background())
	if err != nil {
		t.Fatalf("stored packs: %v", err)
	}
	keysBefore := packKeysInBucket(t)

	repo := srv.store.GetRepo("admin", name)
	if repo == nil {
		t.Fatal("premise: the repository has no record")
	}
	srv.store.Mu.Lock()
	srv.store.Repos[repo.ID].Archived = true
	srv.store.Mu.Unlock()
	output, err := git.tryRun(second, "push", "--force", "origin", "HEAD:main")
	if err == nil || !strings.Contains(output, "archived") {
		t.Fatalf("premise: a push to an archived repository was not refused: %v\n%s", err, output)
	}
	if uploaded := packKeysInBucket(t) - keysBefore; uploaded != 1 {
		t.Fatalf("premise: the refused push uploaded %d packs, want the one that is now an orphan", uploaded)
	}

	after, err := stor.StoredPacks(context.Background())
	if err != nil || len(after) != len(before) {
		t.Fatalf("the refused push changed the stored packs from %v to %v (%v)", before, after, err)
	}
	full := srv.store.GetGitStorage("admin", name)
	if err := full.HasEncodedObject(plumbing.NewHash(loser)); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("the refused push's commit is readable: %v", err)
	}
	if ref, err := full.Reference("refs/heads/main"); err != nil || ref.Hash().String() != winner {
		t.Fatalf("main is %v (%v), want the accepted push's %s", ref, err, winner)
	}
}

// packKeysInBucket counts the .pack keys under the git prefix, named by a
// manifest or not.
func packKeysInBucket(t *testing.T) int {
	t.Helper()
	store, err := gitbackend.GetStore(context.Background())
	if err != nil || store == nil {
		t.Fatalf("the git store: %v", err)
	}
	packs := 0
	if err := store.Bucket().List(context.Background(), store.Prefix()+"/", func(entry objstore.Entry) error {
		if strings.HasSuffix(entry.Key, ".pack") {
			packs++
		}
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	return packs
}

// TestAnObjectTheAPINamesIsInTheStoreWhenItIsNamed pins the durability point of
// a write that moves no reference. An object written through the git database
// API is held by the replica that wrote it until something packs it, and the
// response names it by id — which another client may take to another replica.
// So the replica that answered must have made it durable first: a store opened
// afresh on the same bucket, with nothing held in memory, reads it at once.
func TestAnObjectTheAPINamesIsInTheStoreWhenItIsNamed(t *testing.T) {
	srv := newFakeObjectStoreGitServerForTest(t, "s3")
	const name = "named-objects"
	seedGitShallowRepo(t, srv.Server, name)

	content := "written through the API, and named in the answer\n"
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	w := doMiscReq(srv.Server, http.MethodPost, "/api/v3/repos/admin/"+name+"/git/blobs", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create blob: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// Another replica: the same bucket through a store that has held nothing.
	gitbackend.StoreCache.Mu.Lock()
	serving := gitbackend.StoreCache.Store
	gitbackend.StoreCache.Store, gitbackend.StoreCache.Inited = nil, false
	gitbackend.StoreCache.Mu.Unlock()
	t.Cleanup(func() {
		gitbackend.StoreCache.Mu.Lock()
		gitbackend.StoreCache.Store, gitbackend.StoreCache.Inited = serving, true
		gitbackend.StoreCache.Mu.Unlock()
	})
	other, err := gitbackend.GetStore(context.Background())
	if err != nil || other == nil || other == serving {
		t.Fatalf("premise: no second store over the bucket (%v)", err)
	}
	stor, err := other.Repository("admin/" + name)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := object.GetBlob(stor, plumbing.NewHash(created.SHA))
	if err != nil {
		t.Fatalf("another replica cannot read the blob the API named: %v", err)
	}
	reader, err := blob.Reader()
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil || string(read) != content {
		t.Fatalf("another replica read %q (%v), want %q", read, err, content)
	}
}
