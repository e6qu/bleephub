package bleephub

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

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
