package bleephub

import (
	"fmt"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestPersistencePermanentErrorClassification covers the STORE-036 classifier:
// a permanentPersistenceError (and anything wrapping one) is recognized, while
// an ordinary transient error is not — so MustNewPersistence fails fast on
// misconfiguration and only retries genuine "waiting for quorum" failures.
func TestPersistencePermanentErrorClassification(t *testing.T) {
	if !store.IsPermanentPersistenceError(store.PermanentErrf("bad config")) {
		t.Fatal("permanentErrf was not classified as permanent")
	}
	if !store.IsPermanentPersistenceError(fmt.Errorf("startup: %w", store.PermanentErrf("bad config"))) {
		t.Fatal("a wrapped permanent error was not detected through the chain")
	}
	if store.IsPermanentPersistenceError(fmt.Errorf("ping dqlite: connection refused")) {
		t.Fatal("a transient error was misclassified as permanent")
	}
	if store.IsPermanentPersistenceError(nil) {
		t.Fatal("nil was classified as permanent")
	}
}

// TestMalformedDqliteConfigIsPermanent covers STORE-036: a malformed dqlite
// address map is a configuration error no retry can fix, so NewPersistence must
// surface it as permanent (MustNewPersistence then fails fast instead of looping
// forever "waiting for dqlite quorum").
func TestMalformedDqliteConfigIsPermanent(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DQLITE_SERVERS", "1.2.3.4:9000")
	t.Setenv("BLEEPHUB_DQLITE_ADDRESS_MAP", "garbage-with-no-equals-sign")

	_, err := store.NewPersistence()
	if err == nil {
		t.Fatal("NewPersistence accepted a malformed dqlite address map")
	}
	if !store.IsPermanentPersistenceError(err) {
		t.Fatalf("STORE-036: a malformed dqlite address map must be permanent, got: %v", err)
	}
}

// TestDiscussionNumberCounterSurvivesReload covers STORE-059: per-repo
// discussion numbers now come from a high-water counter instead of a scan of
// every (tombstoned) discussion. The counter must be restored on reload, or a
// restarted process would reset numbering to 1 and collide with existing
// discussions.
func TestDiscussionNumberCounterSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	p1, err := store.NewPersistence()
	if err != nil || p1 == nil {
		t.Fatalf("persistence: %v", err)
	}
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	st1.SeedDefaultUser()
	admin := st1.UsersByLogin["admin"]
	repo := st1.CreateRepo(admin, "disc-repo", "", false)

	d1 := st1.CreateDiscussion(repo.ID, 0, admin.ID, "one", "")
	d2 := st1.CreateDiscussion(repo.ID, 0, admin.ID, "two", "")
	if d1.Number != 1 || d2.Number != 2 {
		t.Fatalf("initial numbers = %d,%d want 1,2", d1.Number, d2.Number)
	}
	st1.DeleteDiscussion(d2.ID) // tombstone; the number stays reserved
	_ = p1.Close()

	// Reload into a fresh store.
	p2, err := store.NewPersistence()
	if err != nil || p2 == nil {
		t.Fatalf("reopen persistence: %v", err)
	}
	defer func() { _ = p2.Close() }()
	st2 := store.NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload persistence: %v", err)
	}

	d3 := st2.CreateDiscussion(repo.ID, 0, admin.ID, "three", "")
	if d3.Number != 3 {
		t.Fatalf("STORE-059: discussion number after reload = %d, want 3 (the counter must survive reload and stay monotonic across the tombstone, not reset to 1)", d3.Number)
	}
}
