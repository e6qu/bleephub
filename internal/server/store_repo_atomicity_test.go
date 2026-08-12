package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestUpdateRepoPublishesOnlyAfterPersistence(t *testing.T) {
	persistence := openTestPersistence(t, t.TempDir())
	st := store.NewStore()
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	st.SeedDefaultUser()
	owner := st.UsersByLogin["admin"]
	repo := st.CreateRepo(owner, "atomic-update", "before", false)
	if repo == nil {
		t.Fatal("create repository")
	}
	if err := persistence.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		st.UpdateRepo(owner.Login, repo.Name, func(candidate *store.Repo) {
			candidate.Description = "after"
			candidate.Topics = append(candidate.Topics, "not-durable")
		})
	}()
	if _, ok := recovered.(*store.PersistenceFailure); !ok {
		t.Fatalf("update raised %T, want *persistenceFailure", recovered)
	}
	got := st.GetRepo(owner.Login, repo.Name)
	if got.Description != "before" || len(got.Topics) != 0 {
		t.Fatalf("failed persistence leaked repository mutation: %#v", got)
	}
}
