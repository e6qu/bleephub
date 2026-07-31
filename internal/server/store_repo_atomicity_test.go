package bleephub

import "testing"

func TestUpdateRepoPublishesOnlyAfterPersistence(t *testing.T) {
	persistence := openTestPersistence(t, t.TempDir())
	store := NewStore()
	if err := store.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	store.SeedDefaultUser()
	owner := store.UsersByLogin["admin"]
	repo := store.CreateRepo(owner, "atomic-update", "before", false)
	if repo == nil {
		t.Fatal("create repository")
	}
	if err := persistence.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		store.UpdateRepo(owner.Login, repo.Name, func(candidate *Repo) {
			candidate.Description = "after"
			candidate.Topics = append(candidate.Topics, "not-durable")
		})
	}()
	if _, ok := recovered.(*persistenceFailure); !ok {
		t.Fatalf("update raised %T, want *persistenceFailure", recovered)
	}
	got := store.GetRepo(owner.Login, repo.Name)
	if got.Description != "before" || len(got.Topics) != 0 {
		t.Fatalf("failed persistence leaked repository mutation: %#v", got)
	}
}
