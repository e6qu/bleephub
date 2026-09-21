package bleephub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/internal/gitbackend"
	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	s3ServerOnce      sync.Once
	s3ServerEndpoint  string
	s3ServerContainer string
	s3ServerErr       error
)

// s3TestOwnerLabel stamps the MinIO container with its starting test binary's
// pid. A killed or timed-out suite never runs the removal, so recording the
// owner lets a later run reap an abandoned container without pulling the server
// out from under a concurrently running suite.
const s3TestOwnerLabel = "bleephub-test-s3-owner"

const (
	dockerProbeTimeout  = 5 * time.Second
	dockerRemoveTimeout = 30 * time.Second
)

// s3ServerRunArgs is the docker argument vector that starts the shared MinIO
// server, split out so the reaper's owner label can be asserted without a
// running container.
func s3ServerRunArgs(addr string) []string {
	return []string{
		"run", "--detach", "--rm",
		"--label", fmt.Sprintf("%s=%d", s3TestOwnerLabel, os.Getpid()),
		"--publish", addr + ":9000",
		"--env", "MINIO_ROOT_USER=bleephub-test",
		"--env", "MINIO_ROOT_PASSWORD=bleephub-test-secret",
		"quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z", "server", "/data",
	}
}

// reapAbandonedS3Servers removes MinIO containers whose owning test binary is
// gone. It is deliberately silent: reaping is opportunistic cleanup, and a
// docker that cannot answer is reported by the run that actually needs a
// server, not by this one.
func reapAbandonedS3Servers() {
	listed, err := boundedDockerCleanupOutput("ps", "--all", "--quiet", "--filter", "label="+s3TestOwnerLabel)
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(listed)) {
		owner, err := boundedDockerCleanupOutput("inspect", "--format",
			"{{index .Config.Labels \""+s3TestOwnerLabel+"\"}}", id)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(owner)))
		if err != nil || testBinaryAlive(pid) {
			continue
		}
		_, _ = removeDockerTestContainer(id)
	}
}

func boundedDockerCleanupOutput(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerProbeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

func removeDockerTestContainer(container string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerRemoveTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "rm", "--force", container).CombinedOutput()
}

// testBinaryAlive reports whether a process with this id still exists. Signal 0
// performs the permission and existence checks without delivering anything;
// EPERM means the process exists and is not ours, which still counts as alive.
func testBinaryAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func resetGitObjectStoreForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		gitbackend.StoreCache.Mu.Lock()
		gitbackend.StoreCache.Store = nil
		gitbackend.StoreCache.Inited = false
		gitbackend.StoreCache.Mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// newGitObjectStoreForTest returns a store on a bucket of its own, under the
// "git" prefix the server uses. The bucket is made through a raw client because
// making one is no part of the interface the server runs on.
func newGitObjectStoreForTest(t *testing.T) *gitstore.Store {
	t.Helper()
	endpoint := startS3ServerForTest(t)

	tmp := t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "bleephub-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "bleephub-test-secret")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(tmp, "aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(tmp, "aws-credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_required")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_required")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bucket := fmt.Sprintf("bleephub-test-%d", testutil.NextTestID())
	client, err := minio.New(strings.TrimPrefix(endpoint, "http://"), &minio.Options{
		Creds:  credentials.NewEnvAWS(),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("S3 client: %v", err)
	}
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	return deriveObjectStoreForTest(t, bucket, "git")
}

// newObjectByteStoreForTest returns the service byte store of a fresh bucket,
// and the store it keeps its objects in so a test can look at them directly.
func newObjectByteStoreForTest(t *testing.T) (*gitstore.Store, store.ActionsByteStore) {
	t.Helper()
	storedObjects := newGitObjectStoreForTest(t).Sub("objects")
	return storedObjects, &store.S3ActionsByteStore{Objects: storedObjects}
}

// deriveObjectStoreForTest opens a store on a named bucket of the shared server,
// which is how a test reaches a bucket that does not exist.
func deriveObjectStoreForTest(t *testing.T, bucket, prefix string) *gitstore.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := gitbackend.NewStore(ctx, s3ServerEndpoint, bucket, prefix)
	if err != nil {
		t.Fatalf("open object store: %v", err)
	}
	return opened
}

func startS3ServerForTest(t *testing.T) string {
	t.Helper()
	s3ServerOnce.Do(func() {
		addr := testutil.FreeLocalAddr(t)
		s3ServerEndpoint = "http://" + addr
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, "docker", s3ServerRunArgs(addr)...)
		output, err := command.CombinedOutput()
		if err != nil {
			s3ServerErr = fmt.Errorf("start MinIO S3 server: %w\n%s", err, output)
			return
		}
		s3ServerContainer = strings.TrimSpace(string(output))
		if testutil.TestEventually(30*time.Second, 100*time.Millisecond, func() bool {
			response, err := http.Get(s3ServerEndpoint + "/minio/health/ready") // #nosec G107 -- local test server
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return true
				}
			}
			return false
		}) {
			return
		}
		s3ServerErr = fmt.Errorf("MinIO S3 server did not become healthy at %s", s3ServerEndpoint)
	})
	if s3ServerErr != nil {
		t.Fatal(s3ServerErr)
	}
	return s3ServerEndpoint
}

func putS3RawObject(t *testing.T, objects *gitstore.Store, key string, content []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := objects.Bucket().Put(ctx, key, bytes.NewReader(content), int64(len(content)), objstore.Always, nil); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func getS3RawObject(t *testing.T, objects *gitstore.Store, key string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, _, err := objects.Bucket().Get(ctx, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return data
}

func listS3RawKeys(t *testing.T, objects *gitstore.Store, prefix string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var keys []string
	if err := objects.Bucket().List(ctx, prefix, func(entry objstore.Entry) error {
		keys = append(keys, entry.Key)
		return nil
	}); err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	return keys
}

// readStoredObjectForTest reads what the service byte store keeps under name,
// straight from the bucket rather than through the byte store, so a test sees
// where the bytes landed and that they are exactly the content. The SHA-256 the
// byte store keeps beside them is checked against it.
func readStoredObjectForTest(t *testing.T, storedObjects *gitstore.Store, name string) []byte {
	t.Helper()
	key := path.Join(storedObjects.Prefix(), name)
	content := getS3RawObject(t, storedObjects, key)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := storedObjects.Bucket().Head(ctx, key)
	if err != nil {
		t.Fatalf("head %s: %v", name, err)
	}
	digest := sha256.Sum256(content)
	if want := base64.RawStdEncoding.EncodeToString(digest[:]); info.Metadata[store.ObjectChecksumMetadataName] != want {
		t.Fatalf("stored object %s has %q beside it, want the SHA-256 of its content %q", name, info.Metadata[store.ObjectChecksumMetadataName], want)
	}
	return content
}

// storedObjectExistsForTest reports whether the service byte store keeps
// anything under name.
func storedObjectExistsForTest(t *testing.T, storedObjects *gitstore.Store, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := storedObjects.Bucket().Head(ctx, path.Join(storedObjects.Prefix(), name))
	if err != nil && !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("head %s: %v", name, err)
	}
	return err == nil
}

// TestDeleteRepositoryRemovesEveryObjectOfOneRepository pins that deleting a
// repository empties its prefix past a listing page (a thousand keys) and
// touches nothing of the repository beside it.
func TestDeleteRepositoryRemovesEveryObjectOfOneRepository(t *testing.T) {
	objects := newGitObjectStoreForTest(t)

	for i := 0; i < 1005; i++ {
		putS3RawObject(t, objects, fmt.Sprintf("git/owner/repo/objects/%04d", i), []byte("data"))
	}
	putS3RawObject(t, objects, "git/owner/other/keep", []byte("data"))

	if err := objects.DeleteRepository("owner/repo"); err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}

	keys := listS3RawKeys(t, objects, "git/owner/")
	for _, k := range keys {
		if strings.HasPrefix(k, "git/owner/repo/") {
			t.Fatalf("object %q not deleted", k)
		}
	}
	if len(keys) != 1 || keys[0] != "git/owner/other/keep" {
		t.Fatal("object outside the repo prefix was deleted")
	}
}

// TestRenameRepositoryMovesEveryObjectToTheNewName pins that a rename leaves
// nothing under the old name, everything byte for byte under the new one, and a
// neighbouring repository alone.
func TestRenameRepositoryMovesEveryObjectToTheNewName(t *testing.T) {
	objects := newGitObjectStoreForTest(t)

	putS3RawObject(t, objects, "git/owner/repo/objects/pack/a.pack", []byte("pack-a"))
	putS3RawObject(t, objects, "git/owner/repo/refs/heads/main", []byte("sha-main"))
	putS3RawObject(t, objects, "git/owner/other/refs/heads/main", []byte("keep"))

	if err := objects.RenameRepository("owner/repo", "new-owner/new-repo"); err != nil {
		t.Fatalf("RenameRepository: %v", err)
	}

	oldKeys := listS3RawKeys(t, objects, "git/owner/repo/")
	if len(oldKeys) != 0 {
		t.Fatalf("old repo keys survived rename: %v", oldKeys)
	}
	newKeys := listS3RawKeys(t, objects, "git/new-owner/new-repo/")
	wantNew := []string{
		"git/new-owner/new-repo/objects/pack/a.pack",
		"git/new-owner/new-repo/refs/heads/main",
	}
	if strings.Join(newKeys, "\n") != strings.Join(wantNew, "\n") {
		t.Fatalf("new repo keys = %v, want %v", newKeys, wantNew)
	}
	kept := listS3RawKeys(t, objects, "git/owner/other/")
	if len(kept) != 1 || kept[0] != "git/owner/other/refs/heads/main" {
		t.Fatalf("unrelated repo keys = %v, want owner/other preserved", kept)
	}
	if got := string(getS3RawObject(t, objects, "git/new-owner/new-repo/refs/heads/main")); got != "sha-main" {
		t.Fatalf("renamed ref content = %q, want sha-main", got)
	}
}

// TestDeleteRepositoryReturnsTheListingError pins that a store that cannot be
// listed fails the deletion, rather than reporting an empty prefix as deleted.
func TestDeleteRepositoryReturnsTheListingError(t *testing.T) {
	newGitObjectStoreForTest(t)
	objects := deriveObjectStoreForTest(t, "missing-bucket", "git")

	if err := objects.DeleteRepository("owner/repo"); err == nil {
		t.Fatal("DeleteRepository returned nil, want the listing error")
	}
}
