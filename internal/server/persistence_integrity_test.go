package bleephub

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/s3fake"
	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
)

// newFakeObjectStoreForTest returns a git object store on an in-process object
// store, for assertions that need the engine and not a particular server.
func newFakeObjectStoreForTest(t *testing.T) *gitstore.Store {
	t.Helper()
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	bucket := objstore.NewS3WithClient(fake.Client().Client, "bleephub-test", 0)
	return gitstore.Open(bucket, "git", gitstore.Options{CacheDir: t.TempDir()})
}

// TestRepositoriesOfOneStoreDoNotShareReferences pins the tenancy boundary: two
// repositories opened from the same object store write a reference of the same
// name, and neither may observe the other's.
func TestRepositoriesOfOneStoreDoNotShareReferences(t *testing.T) {
	objects := newFakeObjectStoreForTest(t)
	repoA, err := objects.Repository("owner/repo-a")
	if err != nil {
		t.Fatalf("open repo-a: %v", err)
	}
	repoB, err := objects.Repository("owner/repo-b")
	if err != nil {
		t.Fatalf("open repo-b: %v", err)
	}
	name := plumbing.ReferenceName("refs/heads/main")
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := repoA.SetReference(plumbing.NewHashReference(name, hashA)); err != nil {
		t.Fatalf("set reference in repo-a: %v", err)
	}
	if err := repoB.SetReference(plumbing.NewHashReference(name, hashB)); err != nil {
		t.Fatalf("set reference in repo-b: %v", err)
	}

	gotA, err := repoA.Reference(name)
	if err != nil {
		t.Fatalf("read reference of repo-a: %v", err)
	}
	gotB, err := repoB.Reference(name)
	if err != nil {
		t.Fatalf("read reference of repo-b: %v", err)
	}
	if gotA.Hash() != hashA {
		t.Errorf("repo-a reads %s, want its own %s", gotA.Hash(), hashA)
	}
	if gotB.Hash() != hashB {
		t.Errorf("repo-b reads %s, want its own %s", gotB.Hash(), hashB)
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
	t.Setenv("BLEEPHUB_GIT_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	st := store.NewStore()
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
	t.Setenv("BLEEPHUB_GIT_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	st := store.NewStore()
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
	t.Setenv("BLEEPHUB_GIT_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	st := store.NewStore()
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
	t.Setenv("BLEEPHUB_GIT_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	p1 := openTestPersistence(t, dataDir)
	st1 := store.NewStore()
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
	if err := p1.Put(store.PendingDeletionsBucket, store.PendingRepoDeletionKey(repo.FullName), store.PendingDeletion{
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
	st2 := store.NewStore()
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
	rows, err := p2.List(store.PendingDeletionsBucket)
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
	t.Setenv("BLEEPHUB_GIT_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	p1 := openTestPersistence(t, dataDir)
	st1 := store.NewStore()
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

	if err := p1.Put(store.PendingDeletionsBucket, store.PendingOrgDeletionKey(org.Login), store.PendingDeletion{
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
	st2 := store.NewStore()
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
	t.Setenv("BLEEPHUB_GIT_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	p1 := openTestPersistence(t, dataDir)
	st1 := store.NewStore()
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
	st2 := store.NewStore()
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

	stampSchemaVersion(t, dataDir, store.CurrentSchemaVersion+7)

	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dataDir)
	reopened, err := store.NewPersistence()
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
	if got := readSchemaVersion(t, dataDir); got != store.CurrentSchemaVersion {
		t.Fatalf("stamped schema version = %d, want %d", got, store.CurrentSchemaVersion)
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
	st := store.NewStore()
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
	t.Setenv("BLEEPHUB_GIT_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", gitDir)

	p1 := openTestPersistence(t, dataDir)
	st1 := store.NewStore()
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
	st2 := store.NewStore()
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
	batch := store.NewPersistBatch(p)
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
	failure, ok := recovered.(*store.PersistenceFailure)
	if !ok {
		t.Fatalf("raised %T, want a persistence failure the handler can report", recovered)
	}
	if !strings.Contains(failure.Error(), "users/1") {
		t.Fatalf("failure %q does not name the record", failure)
	}
}

func openTestPersistence(t *testing.T, dataDir string) *store.Persistence {
	t.Helper()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dataDir)
	p, err := store.NewPersistence()
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
	if _, err := db.Exec(store.SchemaMetaDDL); err != nil {
		t.Fatalf("create schema metadata: %v", err)
	}
	if _, err := db.Exec(store.SqliteDialect.WriteVersion, strconv.Itoa(version)); err != nil {
		t.Fatalf("stamp schema version: %v", err)
	}
}

func readSchemaVersion(t *testing.T, dataDir string) int {
	t.Helper()
	db := openRawTestDatabase(t, dataDir)
	defer func() { _ = db.Close() }()
	var raw string
	if err := db.QueryRow(store.SqliteDialect.ReadVersion).Scan(&raw); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("schema version %q is not a number", raw)
	}
	return version
}
