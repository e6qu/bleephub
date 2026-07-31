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
	store.codespaceRuntimeDelete = func(*Codespace) error {
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
	store.codespaceRuntimeDelete = func(*Codespace) error { return errors.New("runtime unavailable") }

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
	store.codespaceWorkspacePrepare = func(string, *Repo, gitStorage.Storer, string) (string, func(), error) {
		close(started)
		<-release
		return "", func() {}, nil
	}

	created := make(chan error, 1)
	go func() {
		_, _, cleanup, err := store.reserveCodespace(user.Login, repo.FullName, "main", "", "")
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
