package bleephub

import (
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestRenameRepoObjectCopyDoesNotHoldStoreLock proves the STORE-013 property for
// rename: the slow object-prefix copy runs outside the global store lock, an
// unrelated read proceeds while it blocks, and the metadata swap + old-prefix
// purge + intent clearing all happen correctly once the copy completes.
func TestRenameRepoObjectCopyDoesNotHoldStoreLock(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	admin := st.UsersByLogin["admin"]
	if st.CreateRepo(admin, "rename-lock", "", false) == nil {
		t.Fatal("create repository")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var copied [2]string
	st.RepoPrefixCopy = func(oldFull, newFull string) error {
		copied[0], copied[1] = oldFull, newFull
		close(started)
		<-release
		return nil
	}
	purged := make(chan string, 1)
	st.RepoPrefixDelete = func(fullName string) error {
		purged <- fullName
		return nil
	}

	done := make(chan bool, 1)
	go func() { done <- st.RenameRepo("admin", "rename-lock", "renamed") }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("rename never reached the object-prefix copy")
	}

	// The copy is blocked; the store lock must be free for unrelated work.
	readDone := make(chan struct{})
	go func() {
		_ = st.LookupUserByLogin("admin")
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("object-prefix copy retained the global store lock")
	}

	close(release)
	if !<-done {
		t.Fatal("rename returned false")
	}

	if copied[0] != "admin/rename-lock" || copied[1] != "admin/renamed" {
		t.Fatalf("copied prefixes = %v, want [admin/rename-lock admin/renamed]", copied)
	}
	st.Mu.Lock()
	_, oldStillThere := st.ReposByName["admin/rename-lock"]
	live, newThere := st.ReposByName["admin/renamed"]
	reserved := st.PendingRepoCreations["admin/renamed"]
	st.Mu.Unlock()
	if oldStillThere {
		t.Fatal("old name still registered after rename")
	}
	if !newThere || live.Name != "renamed" || live.FullName != "admin/renamed" {
		t.Fatalf("new name not registered: %+v", live)
	}
	if reserved {
		t.Fatal("target reservation not released after rename")
	}
	select {
	case p := <-purged:
		if p != "admin/rename-lock" {
			t.Fatalf("purged prefix = %q, want the old name", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old object prefix was not purged after the swap")
	}
	if raw, _ := st.Persist.Get(store.PendingRenamesBucket, store.PendingRepoRenameKey("admin/renamed")); len(raw) != 0 {
		t.Fatal("rename intent was not cleared")
	}
}

// TestInterruptedRepoRenameRecoveryPurgesUnpublishedPrefix proves recovery: an
// intent whose target never became live (crash mid-copy) has its partial new
// prefix purged and its intent cleared at startup.
func TestInterruptedRepoRenameRecoveryPurgesUnpublishedPrefix(t *testing.T) {
	dataDir := t.TempDir()
	p1 := openTestPersistence(t, dataDir)
	if err := p1.Put(store.PendingRenamesBucket, store.PendingRepoRenameKey("admin/newname"), store.PendingRename{
		From:      "admin/oldname",
		To:        "admin/newname",
		StartedAt: fixedTestTime,
	}); err != nil {
		t.Fatalf("record rename intent: %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close first persistence: %v", err)
	}

	p2 := openTestPersistence(t, dataDir)
	defer func() { _ = p2.Close() }()
	st := store.NewStore()
	purged := make(chan string, 1)
	st.RepoPrefixDelete = func(fullName string) error {
		purged <- fullName
		return nil
	}
	if err := st.SetPersistence(p2); err != nil {
		t.Fatalf("resume rename: %v", err)
	}

	select {
	case p := <-purged:
		if p != "admin/newname" {
			t.Fatalf("recovery purged %q, want the unpublished new prefix admin/newname", p)
		}
	default:
		t.Fatal("recovery did not purge the leftover prefix")
	}
	if raw, _ := p2.Get(store.PendingRenamesBucket, store.PendingRepoRenameKey("admin/newname")); len(raw) != 0 {
		t.Fatal("rename intent was not cleared by recovery")
	}
}
