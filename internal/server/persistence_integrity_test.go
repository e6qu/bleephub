package bleephub

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// newStagingFSForTest builds an object filesystem whose endpoint refuses
// connections. Every assertion here is about the write-staging map and the
// lock, both of which are resolved before any request leaves the process, so a
// test that reaches the network has already failed its own premise.
func newStagingFSForTest(t *testing.T) *s3FS {
	t.Helper()
	fs, err := newS3FS(context.Background(), "http://127.0.0.1:1", "bleephub-test", "git")
	if err != nil {
		t.Fatalf("build object filesystem: %v", err)
	}
	return fs
}

// TestChrootStagedBytesAreNotSharedAcrossRepositories pins the tenancy
// boundary: two repositories chrooted from the same object filesystem stage
// writes under identical relative names, and neither may observe the other's
// bytes.
func TestChrootStagedBytesAreNotSharedAcrossRepositories(t *testing.T) {
	fs := newStagingFSForTest(t)
	repoA, err := fs.Chroot("owner/repo-a")
	if err != nil {
		t.Fatalf("chroot repo-a: %v", err)
	}
	repoB, err := fs.Chroot("owner/repo-b")
	if err != nil {
		t.Fatalf("chroot repo-b: %v", err)
	}

	fileA, err := repoA.Create("packed-refs")
	if err != nil {
		t.Fatalf("create in repo-a: %v", err)
	}
	if _, err := fileA.Write([]byte("repository A refs")); err != nil {
		t.Fatalf("write repo-a: %v", err)
	}
	fileB, err := repoB.Create("packed-refs")
	if err != nil {
		t.Fatalf("create in repo-b: %v", err)
	}
	if _, err := fileB.Write([]byte("repository B refs")); err != nil {
		t.Fatalf("write repo-b: %v", err)
	}

	openedA, err := repoA.Open("packed-refs")
	if err != nil {
		t.Fatalf("open staged repo-a object: %v", err)
	}
	gotA, err := io.ReadAll(openedA)
	if err != nil {
		t.Fatalf("read staged repo-a object: %v", err)
	}
	openedB, err := repoB.Open("packed-refs")
	if err != nil {
		t.Fatalf("open staged repo-b object: %v", err)
	}
	gotB, err := io.ReadAll(openedB)
	if err != nil {
		t.Fatalf("read staged repo-b object: %v", err)
	}

	if string(gotA) != "repository A refs" {
		t.Errorf("repo-a read %q, want its own staged bytes", gotA)
	}
	if string(gotB) != "repository B refs" {
		t.Errorf("repo-b read %q, want its own staged bytes", gotB)
	}
}

// TestObjectFileLockExcludesConcurrentWriters pins that the lock go-git takes
// for ref compare-and-set actually excludes a second writer of the same key.
func TestObjectFileLockExcludesConcurrentWriters(t *testing.T) {
	fs := newStagingFSForTest(t)
	first, err := fs.Create("refs/heads/main")
	if err != nil {
		t.Fatalf("create first handle: %v", err)
	}
	second, err := fs.Create("refs/heads/main")
	if err != nil {
		t.Fatalf("create second handle: %v", err)
	}

	if err := first.Lock(); err != nil {
		t.Fatalf("lock first handle: %v", err)
	}

	acquired := make(chan error, 1)
	go func() { acquired <- second.Lock() }()

	select {
	case err := <-acquired:
		t.Fatalf("second writer acquired a held lock (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := first.Unlock(); err != nil {
		t.Fatalf("unlock first handle: %v", err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("second writer could not take the released lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second writer never took the released lock")
	}
	if err := second.Unlock(); err != nil {
		t.Fatalf("unlock second handle: %v", err)
	}
}

// TestObjectFileLockIsReleasedByClose pins the ordering go-git depends on: it
// locks, writes, and lets the deferred Close release the lock.
func TestObjectFileLockIsReleasedByClose(t *testing.T) {
	fs := newStagingFSForTest(t)
	first, err := fs.Create("refs/heads/release")
	if err != nil {
		t.Fatalf("create handle: %v", err)
	}
	if err := first.Lock(); err != nil {
		t.Fatalf("lock: %v", err)
	}
	// Close flushes, which fails against the unreachable endpoint; the lock
	// must be released regardless or the key is stranded forever.
	_ = first.Close()

	second, err := fs.Create("refs/heads/release")
	if err != nil {
		t.Fatalf("create second handle: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- second.Lock() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("lock after close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the lock")
	}
	if err := second.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
}

// TestDurableLockIsExclusiveAcrossOwners pins the cross-replica half of the
// lock: the durable store, not the process, decides who holds a key.
func TestDurableLockIsExclusiveAcrossOwners(t *testing.T) {
	p := openTestPersistence(t, t.TempDir())
	defer func() { _ = p.Close() }()

	acquired, err := p.AcquireLock("git-object:git/owner/repo/packed-refs", "replica-one", time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !acquired {
		t.Fatal("first owner did not acquire a free lock")
	}
	acquired, err = p.AcquireLock("git-object:git/owner/repo/packed-refs", "replica-two", time.Minute)
	if err != nil {
		t.Fatalf("acquire contended: %v", err)
	}
	if acquired {
		t.Fatal("second owner acquired a lock the first still holds")
	}
	if err := p.ReleaseLock("git-object:git/owner/repo/packed-refs", "replica-one"); err != nil {
		t.Fatalf("release: %v", err)
	}
	acquired, err = p.AcquireLock("git-object:git/owner/repo/packed-refs", "replica-two", time.Minute)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if !acquired {
		t.Fatal("second owner did not acquire the released lock")
	}
}

// TestExpiredDurableLockIsTakenOver pins that a replica that died holding a
// lock cannot strand the key.
func TestExpiredDurableLockIsTakenOver(t *testing.T) {
	p := openTestPersistence(t, t.TempDir())
	defer func() { _ = p.Close() }()

	if acquired, err := p.AcquireLock("git-object:stale", "dead-replica", time.Nanosecond); err != nil || !acquired {
		t.Fatalf("acquire = %v, %v", acquired, err)
	}
	acquired, err := p.AcquireLock("git-object:stale", "live-replica", time.Minute)
	if err != nil {
		t.Fatalf("take over expired lock: %v", err)
	}
	if !acquired {
		t.Fatal("expired lock was not taken over")
	}
}

// TestRenameRepoRebindsGitStorageToNewPrefix pins that git input and output
// after a rename addresses the moved bytes, not the prefix they left.
func TestRenameRepoRebindsGitStorageToNewPrefix(t *testing.T) {
	gitDir := t.TempDir()
	t.Setenv("BLEEPHUB_S3_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	st := NewStore()
	st.SeedDefaultUser()
	user := st.UsersByLogin["admin"]
	if st.CreateRepo(user, "before", "", false) == nil {
		t.Fatal("CreateRepo returned nil")
	}

	hash := plumbing.NewHash(strings.Repeat("ab", 20))
	original := st.GitStorages["admin/before"]
	if original == nil {
		t.Fatal("repository has no git storage")
	}
	if err := original.SetReference(plumbing.NewHashReference("refs/heads/main", hash)); err != nil {
		t.Fatalf("seed reference: %v", err)
	}

	if !st.RenameRepo("admin", "before", "after") {
		t.Fatal("RenameRepo failed")
	}

	renamed := st.GitStorages["admin/after"]
	if renamed == nil {
		t.Fatal("renamed repository has no git storage")
	}
	ref, err := renamed.Reference("refs/heads/main")
	if err != nil {
		t.Fatalf("read reference through the renamed storer: %v", err)
	}
	if ref.Hash() != hash {
		t.Fatalf("reference hash = %s, want %s", ref.Hash(), hash)
	}
	if err := renamed.SetReference(plumbing.NewHashReference("refs/heads/next", hash)); err != nil {
		t.Fatalf("write reference through the renamed storer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "admin", "after", "refs", "heads", "next")); err != nil {
		t.Fatalf("new reference did not land under the new prefix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "admin", "before")); !os.IsNotExist(err) {
		t.Fatalf("git input and output still addresses the vanished prefix: %v", err)
	}
}

// TestRenameRepoAbortsWhenTheStorageMoveFails pins that a rename whose bytes
// could not be moved leaves the repository addressable under its old name.
func TestRenameRepoAbortsWhenTheStorageMoveFails(t *testing.T) {
	gitDir := t.TempDir()
	t.Setenv("BLEEPHUB_S3_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	st := NewStore()
	st.SeedDefaultUser()
	user := st.UsersByLogin["admin"]
	if st.CreateRepo(user, "movable", "", false) == nil {
		t.Fatal("CreateRepo returned nil")
	}
	// A regular file where the destination directory must go makes rename(2)
	// fail with something other than EEXIST.
	if err := os.WriteFile(filepath.Join(gitDir, "admin", "blocked"), []byte("in the way"), 0o600); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}

	if st.RenameRepo("admin", "movable", "blocked") {
		t.Fatal("RenameRepo reported success although the storage move failed")
	}
	if st.GetRepo("admin", "movable") == nil {
		t.Fatal("repository disappeared after a failed rename")
	}
	if st.GetRepo("admin", "blocked") != nil {
		t.Fatal("repository was registered under the name the move never reached")
	}
}

func TestTransferRepoRebindsGitStorageToNewOwnerPrefix(t *testing.T) {
	gitDir := t.TempDir()
	t.Setenv("BLEEPHUB_S3_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	st := NewStore()
	st.SeedDefaultUser()
	admin := st.UsersByLogin["admin"]
	if st.CreateOrg(admin, "new-owner", "New Owner", "") == nil {
		t.Fatal("CreateOrg returned nil")
	}
	if st.CreateRepo(admin, "transferred", "", false) == nil {
		t.Fatal("CreateRepo returned nil")
	}
	hash := plumbing.NewHash(strings.Repeat("cd", 20))
	if err := st.GitStorages["admin/transferred"].SetReference(
		plumbing.NewHashReference("refs/heads/main", hash),
	); err != nil {
		t.Fatalf("seed reference: %v", err)
	}

	if !st.TransferRepo("admin", "transferred", "new-owner") {
		t.Fatal("TransferRepo failed")
	}
	transferred := st.GitStorages["new-owner/transferred"]
	if transferred == nil {
		t.Fatal("transferred repository has no rebound git storage")
	}
	ref, err := transferred.Reference("refs/heads/main")
	if err != nil || ref.Hash() != hash {
		t.Fatalf("read reference through transferred storer = %v, %v", ref, err)
	}
	if err := transferred.SetReference(
		plumbing.NewHashReference("refs/heads/next", hash),
	); err != nil {
		t.Fatalf("write reference through transferred storer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "new-owner", "transferred", "refs", "heads", "next")); err != nil {
		t.Fatalf("new reference did not land under transferred prefix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "admin", "transferred")); !os.IsNotExist(err) {
		t.Fatalf("old owner prefix still exists after transfer: %v", err)
	}
}

// TestInterruptedRepoDeleteIsFinishedOnRestart pins that a cascade cut short
// mid-flight leaves no half-deleted repository behind.
func TestInterruptedRepoDeleteIsFinishedOnRestart(t *testing.T) {
	dataDir := t.TempDir()
	gitDir := t.TempDir()
	t.Setenv("BLEEPHUB_S3_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	p1 := openTestPersistence(t, dataDir)
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	st1.SeedDefaultUser()
	user := st1.UsersByLogin["admin"]
	repo := st1.CreateRepo(user, "abandoned", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	st1.CreateIssue(repo.ID, user.ID, "orphan", "", nil, nil, 0)

	// Stop exactly where a process death would: the intent is durable, the
	// cascade has not run.
	if err := p1.Put(pendingDeletionsBucket, pendingRepoDeletionKey(repo.FullName), pendingDeletion{
		Kind:      "repo",
		Name:      repo.FullName,
		StartedAt: fixedTestTime.UTC(),
	}); err != nil {
		t.Fatalf("record deletion intent: %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2 := openTestPersistence(t, dataDir)
	defer func() { _ = p2.Close() }()
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload SetPersistence: %v", err)
	}

	if st2.GetRepo("admin", "abandoned") != nil {
		t.Fatal("half-deleted repository survived the restart")
	}
	for _, issue := range st2.Issues {
		if issue.RepoID == repo.ID {
			t.Fatalf("issue of a half-deleted repository survived: %#v", issue)
		}
	}
	if _, err := os.Stat(filepath.Join(gitDir, "admin", "abandoned")); !os.IsNotExist(err) {
		t.Fatalf("git bytes of a half-deleted repository survived: %v", err)
	}
	rows, err := p2.List(pendingDeletionsBucket)
	if err != nil {
		t.Fatalf("list deletion intents: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("deletion intent survived the resumed delete: %#v", rows)
	}
}

// TestInterruptedOrgDeleteDoesNotPoisonBoot pins the repair path for the worst
// shape of a partial cascade: repositories whose owning organization row is
// already gone. Without the recorded intent that row is indistinguishable from
// corruption and every subsequent start fails on it.
func TestInterruptedOrgDeleteDoesNotPoisonBoot(t *testing.T) {
	dataDir := t.TempDir()
	gitDir := t.TempDir()
	t.Setenv("BLEEPHUB_S3_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	p1 := openTestPersistence(t, dataDir)
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	st1.SeedDefaultUser()
	user := st1.UsersByLogin["admin"]
	org := st1.CreateOrg(user, "doomed-org", "Doomed", "")
	if org == nil {
		t.Fatal("CreateOrg returned nil")
	}
	repo := st1.CreateOrgRepo(org, user, "stranded", "", false)
	if repo == nil {
		t.Fatal("CreateOrgRepo returned nil")
	}

	if err := p1.Put(pendingDeletionsBucket, pendingOrgDeletionKey(org.Login), pendingDeletion{
		Kind:      "org",
		Name:      org.Login,
		StartedAt: fixedTestTime.UTC(),
	}); err != nil {
		t.Fatalf("record deletion intent: %v", err)
	}
	// The organization row is gone; its repository row is not.
	if err := p1.Delete("orgs", strconv.Itoa(org.ID)); err != nil {
		t.Fatalf("delete organization row: %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2 := openTestPersistence(t, dataDir)
	defer func() { _ = p2.Close() }()
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("a partial organization cascade poisoned the boot: %v", err)
	}
	if st2.GetOrg("doomed-org") != nil {
		t.Fatal("half-deleted organization survived the restart")
	}
	if st2.GetRepo("doomed-org", "stranded") != nil {
		t.Fatal("repository of a half-deleted organization survived the restart")
	}
	rows, err := p2.List("repos")
	if err != nil {
		t.Fatalf("list repositories: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("stranded repository row survived: %#v", rows)
	}
}

// TestDeleteOrgCascadesToItsRepositories pins the source of that poison pill:
// deleting an organization must take its repositories with it.
func TestDeleteOrgCascadesToItsRepositories(t *testing.T) {
	dataDir := t.TempDir()
	gitDir := t.TempDir()
	t.Setenv("BLEEPHUB_S3_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	p1 := openTestPersistence(t, dataDir)
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	st1.SeedDefaultUser()
	user := st1.UsersByLogin["admin"]
	org := st1.CreateOrg(user, "cascade-org", "Cascade", "")
	if st1.CreateOrgRepo(org, user, "owned", "", false) == nil {
		t.Fatal("CreateOrgRepo returned nil")
	}
	if !st1.DeleteOrg(org.Login) {
		t.Fatal("DeleteOrg failed")
	}
	if st1.GetRepo("cascade-org", "owned") != nil {
		t.Fatal("organization repository survived the organization delete")
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2 := openTestPersistence(t, dataDir)
	defer func() { _ = p2.Close() }()
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload after organization delete: %v", err)
	}
	if st2.GetRepo("cascade-org", "owned") != nil {
		t.Fatal("organization repository came back after reload")
	}
}

// TestUnknownSchemaVersionRefusesToStart pins that a database written by a
// newer build is rejected rather than decoded against a layout this build does
// not know.
func TestUnknownSchemaVersionRefusesToStart(t *testing.T) {
	dataDir := t.TempDir()
	p := openTestPersistence(t, dataDir)
	if err := p.Put("users", "1", map[string]any{"id": 1}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stampSchemaVersion(t, dataDir, currentSchemaVersion+7)

	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dataDir)
	reopened, err := NewPersistence()
	if err == nil {
		_ = reopened.Close()
		t.Fatal("a database stamped with an unknown schema version was accepted")
	}
	if !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("error %q does not name the schema version", err)
	}
}

// TestSchemaVersionIsStampedAndAccepted pins the ordinary path: a database this
// build wrote reopens without complaint.
func TestSchemaVersionIsStampedAndAccepted(t *testing.T) {
	dataDir := t.TempDir()
	p := openTestPersistence(t, dataDir)
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := readSchemaVersion(t, dataDir); got != currentSchemaVersion {
		t.Fatalf("stamped schema version = %d, want %d", got, currentSchemaVersion)
	}
	reopened := openTestPersistence(t, dataDir)
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened: %v", err)
	}
}

// TestDeletedIdentifierIsNotReusedAcrossRestart pins the object-key hazard: an
// identifier that named object bytes must never be handed out again, however
// few rows survive.
func TestDeletedIdentifierIsNotReusedAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()

	p1 := openTestPersistence(t, dataDir)
	if err := p1.Put("attestations", "41", map[string]any{"id": 41}); err != nil {
		t.Fatalf("seed attestation: %v", err)
	}
	if err := p1.Put("package_files", "97", map[string]any{"id": 97}); err != nil {
		t.Fatalf("seed package file: %v", err)
	}
	if err := p1.Delete("attestations", "41"); err != nil {
		t.Fatalf("delete attestation: %v", err)
	}
	if err := p1.Delete("package_files", "97"); err != nil {
		t.Fatalf("delete package file: %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2 := openTestPersistence(t, dataDir)
	defer func() { _ = p2.Close() }()
	st := NewStore()
	if err := st.SetPersistence(p2); err != nil {
		t.Fatalf("reload SetPersistence: %v", err)
	}
	if st.NextAttestationID <= 41 {
		t.Errorf("next attestation identifier = %d, which reuses the object key of a deleted attestation", st.NextAttestationID)
	}
	if st.NextPackageFileID <= 97 {
		t.Errorf("next package file identifier = %d, which reuses the object key of a deleted package file", st.NextPackageFileID)
	}
}

// TestDeletedRepositoryIdentifierIsNotReusedAcrossRestart pins the same
// invariant on the path a request actually takes.
func TestDeletedRepositoryIdentifierIsNotReusedAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	gitDir := t.TempDir()
	t.Setenv("BLEEPHUB_S3_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	p1 := openTestPersistence(t, dataDir)
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	st1.SeedDefaultUser()
	user := st1.UsersByLogin["admin"]
	repo := st1.CreateRepo(user, "recycled", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	deletedID := repo.ID
	if deleted, err := st1.DeleteRepo("admin", "recycled"); err != nil || !deleted {
		t.Fatalf("DeleteRepo = %v, %v", deleted, err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2 := openTestPersistence(t, dataDir)
	defer func() { _ = p2.Close() }()
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload SetPersistence: %v", err)
	}
	recreated := st2.CreateRepo(st2.UsersByLogin["admin"], "recycled", "", false)
	if recreated == nil {
		t.Fatal("recreate after delete failed")
	}
	if recreated.ID <= deletedID {
		t.Fatalf("recreated repository identifier = %d, which the deleted repository already held (%d)", recreated.ID, deletedID)
	}
}

// TestPersistenceBatchIsAtomic pins that a batch either lands whole or not at
// all; a cascade built on it cannot be observed half applied.
func TestPersistenceBatchIsAtomic(t *testing.T) {
	p := openTestPersistence(t, t.TempDir())
	defer func() { _ = p.Close() }()

	if err := p.Put("labels", "1", map[string]any{"id": 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	batch := newPersistBatch(p)
	batch.Delete("labels", "1")
	batch.Put("labels", "2", func() {}) // functions do not marshal
	if err := batch.Commit(); err == nil {
		t.Fatal("a batch with an unencodable record committed")
	}
	rows, err := p.List("labels")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, ok := rows["1"]; !ok {
		t.Fatal("a failed batch applied its delete")
	}
	if _, ok := rows["2"]; ok {
		t.Fatal("a failed batch applied its write")
	}
}

// TestPersistenceWriteFailureDoesNotKillTheProcess pins the reporting contract:
// a failed write aborts the request, not the server.
func TestPersistenceWriteFailureDoesNotKillTheProcess(t *testing.T) {
	p := openTestPersistence(t, t.TempDir())
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		p.MustPut("users", "1", map[string]any{"id": 1})
	}()
	if recovered == nil {
		t.Fatal("a failed write neither returned nor raised")
	}
	failure, ok := recovered.(*persistenceFailure)
	if !ok {
		t.Fatalf("raised %T, want a persistence failure the handler can report", recovered)
	}
	if !strings.Contains(failure.Error(), "users/1") {
		t.Fatalf("failure %q does not name the record", failure)
	}
}

// TestKeyLocksAreReleasedUnderContention pins that the key-lock table does not
// leak entries, which would otherwise grow without bound on a busy server.
func TestKeyLocksAreReleasedUnderContention(t *testing.T) {
	locks := newS3KeyLocks()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := locks.acquire("git/owner/repo/packed-refs", 5*time.Second); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			locks.release("git/owner/repo/packed-refs")
		}()
	}
	wg.Wait()

	count := 0
	locks.mu.Lock()
	count = len(locks.slots)
	locks.mu.Unlock()
	if count != 0 {
		t.Fatalf("key lock table retained %d entries", count)
	}
}

func openTestPersistence(t *testing.T, dataDir string) *Persistence {
	t.Helper()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dataDir)
	p, err := NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	if p == nil {
		t.Fatal("persistence is disabled")
	}
	return p
}

func openRawTestDatabase(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "bleephub.db"))
	if err != nil {
		t.Fatalf("open database directly: %v", err)
	}
	return db
}

func stampSchemaVersion(t *testing.T, dataDir string, version int) {
	t.Helper()
	db := openRawTestDatabase(t, dataDir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(schemaMetaDDL); err != nil {
		t.Fatalf("create schema metadata: %v", err)
	}
	if _, err := db.Exec(sqliteDialect.writeVersion, strconv.Itoa(version)); err != nil {
		t.Fatalf("stamp schema version: %v", err)
	}
}

func readSchemaVersion(t *testing.T, dataDir string) int {
	t.Helper()
	db := openRawTestDatabase(t, dataDir)
	defer func() { _ = db.Close() }()
	var raw string
	if err := db.QueryRow(sqliteDialect.readVersion).Scan(&raw); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("schema version %q is not a number", raw)
	}
	return version
}
