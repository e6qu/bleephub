package bleephub

import (
	"errors"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

func TestCodespaceRuntimeDeletionDoesNotHoldStoreLock(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	codespace := &store.Codespace{
		ID:         st.NextCodespaceID,
		Name:       "lock-free-delete",
		OwnerLogin: "admin",
		State:      "Available",
	}
	st.NextCodespaceID++
	st.Codespaces[codespace.ID] = codespace
	st.CodespacesByName[codespace.Name] = codespace

	started := make(chan struct{})
	release := make(chan struct{})
	st.CodespaceRuntimeDelete = func(*store.Codespace) error {
		close(started)
		<-release
		return nil
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := st.DeleteCodespace(codespace.ID)
		deleted <- err
	}()
	<-started

	readFinished := make(chan *store.Codespace, 1)
	go func() { readFinished <- st.GetCodespace(codespace.ID) }()
	select {
	case snapshot := <-readFinished:
		if snapshot == nil || snapshot.State != "Deleting" {
			t.Fatalf("snapshot during deletion = %#v", snapshot)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary store read blocked behind codespace runtime deletion")
	}
	close(release)
	if err := <-deleted; err != nil {
		t.Fatalf("delete codespace: %v", err)
	}
	if st.GetCodespace(codespace.ID) != nil {
		t.Fatal("codespace remained after runtime deletion")
	}
}

func TestCodespaceRuntimeDeletionFailureRestoresVisibleState(t *testing.T) {
	st := store.NewStore()
	codespace := &store.Codespace{ID: 41, Name: "retryable-delete", State: "Available"}
	st.Codespaces[codespace.ID] = codespace
	st.CodespacesByName[codespace.Name] = codespace
	st.CodespaceRuntimeDelete = func(*store.Codespace) error { return errors.New("runtime unavailable") }

	ok, err := st.DeleteCodespace(codespace.ID)
	if !ok || err == nil {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if got := st.GetCodespace(codespace.ID); got == nil || got.State != "Available" {
		t.Fatalf("failed deletion left codespace unavailable: %#v", got)
	}
}

func TestCodespaceWorkspacePreparationDoesNotHoldStoreLock(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	user := st.GetUserByID(1)
	repo := st.CreateRepo(user, "workspace-lock", "", false)
	if repo == nil {
		t.Fatal("create repo")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	st.CodespaceWorkspacePrepare = func(string, *store.Repo, gitStorage.Storer, string) (string, func(), error) {
		close(started)
		<-release
		return "", func() {}, nil
	}

	created := make(chan error, 1)
	go func() {
		_, _, cleanup, err := st.ReserveCodespace(user.Login, repo.FullName, "main", "", store.CodespaceCreateOptions{})
		if cleanup != nil {
			cleanup()
		}
		created <- err
	}()
	<-started

	readFinished := make(chan *store.Repo, 1)
	go func() { readFinished <- st.GetRepo(user.Login, repo.Name) }()
	select {
	case snapshot := <-readFinished:
		if snapshot == nil || snapshot.ID != repo.ID {
			t.Fatalf("repository snapshot during workspace preparation = %#v", snapshot)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary store read blocked behind codespace workspace preparation")
	}
	close(release)
	if err := <-created; err != nil {
		t.Fatalf("reserve codespace: %v", err)
	}
}

// TestCodespaceSecretStoresValue covers STORE-031: CreateCodespaceSecret must
// retain the decrypted value on create and update; it never returns through the
// API, so it is only observable at the store boundary.
func TestCodespaceSecretStoresValue(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	scope := store.CodespaceSecretScopeKey("user", "admin")

	st.CreateCodespaceSecret(scope, "DEPLOY_KEY", "first-value", "", nil)
	if got := st.GetCodespaceSecret(scope, "DEPLOY_KEY"); got == nil || got.Value != "first-value" {
		t.Fatalf("after create, Value = %q, want %q", codespaceSecretValue(got), "first-value")
	}

	// Updating the same name must overwrite the stored value.
	st.CreateCodespaceSecret(scope, "DEPLOY_KEY", "second-value", "", nil)
	if got := st.GetCodespaceSecret(scope, "DEPLOY_KEY"); got == nil || got.Value != "second-value" {
		t.Fatalf("after update, Value = %q, want %q", codespaceSecretValue(got), "second-value")
	}
}

func codespaceSecretValue(s *store.CodespaceSecret) string {
	if s == nil {
		return "<nil>"
	}
	return s.Value
}
