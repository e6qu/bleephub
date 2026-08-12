package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestGistMutationPublishesMemoryOnlyAfterPersistence(t *testing.T) {
	persistence := openTestPersistence(t, t.TempDir())
	st := store.NewStore()
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	st.SeedDefaultUser()
	owner := st.UsersByLogin["admin"]
	gist, err := st.CreateGistE(owner, "durable", true, map[string]*store.GistFile{
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
		st.DeleteGist(gist.ID)
	}()
	if _, ok := recovered.(*store.PersistenceFailure); !ok {
		t.Fatalf("delete raised %T, want *persistenceFailure", recovered)
	}
	if got := st.GetGist(gist.ID); got == nil || got.Description != "durable" {
		t.Fatalf("failed persistence changed the live gist: %#v", got)
	}
}

func TestGistReadsReturnDefensiveSnapshots(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	owner := st.UsersByLogin["admin"]
	gist, err := st.CreateGistE(owner, "original", true, map[string]*store.GistFile{
		"README.md": {Filename: "README.md", Content: "original"},
	})
	if err != nil {
		t.Fatalf("create gist: %v", err)
	}

	read := st.GetGist(gist.ID)
	read.Description = "mutated"
	read.Files["README.md"].Content = "mutated"
	read.ForkIDs = append(read.ForkIDs, "shadow")

	again := st.GetGist(gist.ID)
	if again.Description != "original" ||
		again.Files["README.md"].Content != "original" ||
		len(again.ForkIDs) != 0 {
		t.Fatalf("caller mutation escaped the store boundary: %#v", again)
	}
}
