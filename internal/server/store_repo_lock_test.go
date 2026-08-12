package bleephub

import (
	"context"
	"testing"
	"time"

	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestRepositoryStorageInitializationDoesNotHoldStoreLock(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	owner := store.UsersByLogin["admin"]
	started := make(chan struct{})
	release := make(chan struct{})
	store.RepoStorageOpen = func(context.Context, string) (gitStorage.Storer, error) {
		close(started)
		<-release
		return memory.NewStorage(), nil
	}

	created := make(chan *Repo, 1)
	go func() { created <- store.CreateRepo(owner, "unlocked-create", "", false) }()
	<-started

	readFinished := make(chan *User, 1)
	go func() { readFinished <- store.GetUserByID(owner.ID) }()
	select {
	case user := <-readFinished:
		if user == nil || user.ID != owner.ID {
			t.Fatalf("unrelated read returned %#v", user)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary store read blocked behind repository storage initialization")
	}
	if duplicate := store.CreateRepo(owner, "unlocked-create", "", false); duplicate != nil {
		t.Fatalf("duplicate creation bypassed pending-name reservation: %#v", duplicate)
	}
	close(release)
	if repo := <-created; repo == nil {
		t.Fatal("repository was not created after storage initialization")
	}
}

func TestRepositoryForkCopyDoesNotHoldStoreLock(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	owner := store.UsersByLogin["admin"]
	source := store.CreateRepo(owner, "fork-source", "", false)
	if source == nil {
		t.Fatal("create source repository")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	store.RepoStorageOpen = func(context.Context, string) (gitStorage.Storer, error) {
		close(started)
		<-release
		return memory.NewStorage(), nil
	}

	created := make(chan *Repo, 1)
	go func() { created <- store.ForkRepo(owner, source, "unlocked-fork") }()
	<-started

	readFinished := make(chan *User, 1)
	go func() { readFinished <- store.GetUserByID(owner.ID) }()
	select {
	case user := <-readFinished:
		if user == nil || user.ID != owner.ID {
			t.Fatalf("unrelated read returned %#v", user)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary store read blocked behind repository fork storage copy")
	}
	if duplicate := store.CreateRepo(owner, "unlocked-fork", "", false); duplicate != nil {
		t.Fatalf("duplicate creation bypassed fork name reservation: %#v", duplicate)
	}
	close(release)
	if fork := <-created; fork == nil || fork.ParentID != source.ID {
		t.Fatalf("repository fork = %#v", fork)
	}
}
