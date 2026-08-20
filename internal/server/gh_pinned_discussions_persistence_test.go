package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestPersistenceReload_PinnedDiscussionsAndAdvisoryCredits pins the new
// persisted state: the pinned_discussions bucket reloads in order, and an
// advisory's credits survive a restart.
func TestPersistenceReload_PinnedDiscussionsAndAdvisoryCredits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	p1, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	st1.SeedDefaultUser()
	user := st1.UsersByLogin["admin"]
	repo := st1.CreateRepo(user, "pinned-disc-repo", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	cat := st1.CreateDiscussionCategory(repo.ID, "General2", ":speech_balloon:", "", false)
	d1 := st1.CreateDiscussion(repo.ID, cat.ID, user.ID, "first", "b")
	d2 := st1.CreateDiscussion(repo.ID, cat.ID, user.ID, "second", "b")
	st1.SetPinnedDiscussions(repo.ID, []int{d2.ID, d1.ID})

	adv, err := st1.CreateSecurityAdvisoryE(repo.ID, user.ID, store.CreateAdvisoryReq{
		Summary:  "credited",
		Severity: "low",
		Credits:  []store.SecurityAdvisoryCredit{{Login: "admin", Type: "finder"}},
	})
	if err != nil || adv == nil {
		t.Fatalf("create advisory: %v %v", adv, err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	st2 := store.NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("re-SetPersistence: %v", err)
	}
	defer p2.Close()

	pins := st2.ListPinnedDiscussions(repo.ID)
	if len(pins) != 2 || pins[0] != d2.ID || pins[1] != d1.ID {
		t.Errorf("reloaded pins = %v, want [%d %d] in order", pins, d2.ID, d1.ID)
	}
	reloaded := st2.GetSecurityAdvisoryByGHSA(repo.ID, adv.GHSAID)
	if reloaded == nil {
		t.Fatalf("advisory %s did not reload", adv.GHSAID)
	}
	if len(reloaded.Credits) != 1 || reloaded.Credits[0].Login != "admin" || reloaded.Credits[0].Type != "finder" {
		t.Errorf("reloaded credits = %v, want [{admin finder}]", reloaded.Credits)
	}
}
