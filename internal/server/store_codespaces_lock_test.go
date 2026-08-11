package bleephub

import (
	"errors"
	"testing"
	"time"

	gitStorage "github.com/go-git/go-git/v5/storage"
)

func TestCodespaceRuntimeDeletionDoesNotHoldStoreLock(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	codespace := &Codespace{
		ID:         store.NextCodespaceID,
		Name:       "lock-free-delete",
		OwnerLogin: "admin",
		State:      "Available",
	}
	store.NextCodespaceID++
	store.Codespaces[codespace.ID] = codespace
	store.CodespacesByName[codespace.Name] = codespace

	started := make(chan struct{})
	release := make(chan struct{})
	store.CodespaceRuntimeDelete = func(*Codespace) error {
		close(started)
		<-release
		return nil
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := store.DeleteCodespace(codespace.ID)
		deleted <- err
	}()
	<-started

	readFinished := make(chan *Codespace, 1)
	go func() { readFinished <- store.GetCodespace(codespace.ID) }()
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
	if store.GetCodespace(codespace.ID) != nil {
		t.Fatal("codespace remained after runtime deletion")
	}
}

func TestCodespaceRuntimeDeletionFailureRestoresVisibleState(t *testing.T) {
	store := NewStore()
	codespace := &Codespace{ID: 41, Name: "retryable-delete", State: "Available"}
	store.Codespaces[codespace.ID] = codespace
	store.CodespacesByName[codespace.Name] = codespace
	store.CodespaceRuntimeDelete = func(*Codespace) error { return errors.New("runtime unavailable") }

	ok, err := store.DeleteCodespace(codespace.ID)
	if !ok || err == nil {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if got := store.GetCodespace(codespace.ID); got == nil || got.State != "Available" {
		t.Fatalf("failed deletion left codespace unavailable: %#v", got)
	}
}

func TestCodespaceWorkspacePreparationDoesNotHoldStoreLock(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	user := store.GetUserByID(1)
	repo := store.CreateRepo(user, "workspace-lock", "", false)
	if repo == nil {
		t.Fatal("create repo")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	store.CodespaceWorkspacePrepare = func(string, *Repo, gitStorage.Storer, string) (string, func(), error) {
		close(started)
		<-release
		return "", func() {}, nil
	}

	created := make(chan error, 1)
	go func() {
		_, _, cleanup, err := store.ReserveCodespace(user.Login, repo.FullName, "main", "", codespaceCreateOptions{})
		if cleanup != nil {
			cleanup()
		}
		created <- err
	}()
	<-started

	readFinished := make(chan *Repo, 1)
	go func() { readFinished <- store.GetRepo(user.Login, repo.Name) }()
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
// retain the decrypted value it is handed, both when creating and when
// updating an existing secret. The value is never returned through the API,
// so it is only observable at the store boundary.
func TestCodespaceSecretStoresValue(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	scope := codespaceSecretScopeKey("user", "admin")

	store.CreateCodespaceSecret(scope, "DEPLOY_KEY", "first-value", "", nil)
	if got := store.GetCodespaceSecret(scope, "DEPLOY_KEY"); got == nil || got.Value != "first-value" {
		t.Fatalf("after create, Value = %q, want %q", codespaceSecretValue(got), "first-value")
	}

	// Updating the same name must overwrite the stored value.
	store.CreateCodespaceSecret(scope, "DEPLOY_KEY", "second-value", "", nil)
	if got := store.GetCodespaceSecret(scope, "DEPLOY_KEY"); got == nil || got.Value != "second-value" {
		t.Fatalf("after update, Value = %q, want %q", codespaceSecretValue(got), "second-value")
	}
}

func codespaceSecretValue(s *CodespaceSecret) string {
	if s == nil {
		return "<nil>"
	}
	return s.Value
}
