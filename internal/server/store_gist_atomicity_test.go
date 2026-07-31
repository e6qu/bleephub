package bleephub

import (
	"testing"
)

func TestGistMutationPublishesMemoryOnlyAfterPersistence(t *testing.T) {
	persistence := openTestPersistence(t, t.TempDir())
	store := NewStore()
	if err := store.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	store.SeedDefaultUser()
	owner := store.UsersByLogin["admin"]
	gist, err := store.CreateGistE(owner, "durable", true, map[string]*GistFile{
		"README.md": {Filename: "README.md", Content: "before"},
	})
	if err != nil {
		t.Fatalf("create gist: %v", err)
	}
	if err := persistence.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		store.DeleteGist(gist.ID)
	}()
	if _, ok := recovered.(*persistenceFailure); !ok {
		t.Fatalf("delete raised %T, want *persistenceFailure", recovered)
	}
	if got := store.GetGist(gist.ID); got == nil || got.Description != "durable" {
		t.Fatalf("failed persistence changed the live gist: %#v", got)
	}
}

func TestGistReadsReturnDefensiveSnapshots(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	owner := store.UsersByLogin["admin"]
	gist, err := store.CreateGistE(owner, "original", true, map[string]*GistFile{
		"README.md": {Filename: "README.md", Content: "original"},
	})
	if err != nil {
		t.Fatalf("create gist: %v", err)
	}

	read := store.GetGist(gist.ID)
	read.Description = "mutated"
	read.Files["README.md"].Content = "mutated"
	read.ForkIDs = append(read.ForkIDs, "shadow")

	again := store.GetGist(gist.ID)
	if again.Description != "original" ||
		again.Files["README.md"].Content != "original" ||
		len(again.ForkIDs) != 0 {
		t.Fatalf("caller mutation escaped the store boundary: %#v", again)
	}
}
