package bleephub

import (
	"context"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestRepositoryStorageInitializationDoesNotHoldStoreLock(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	owner := st.UsersByLogin["admin"]
	started := make(chan struct{})
	release := make(chan struct{})
	st.RepoStorageOpen = func(context.Context, string) (gitStorage.Storer, error) {
		close(started)
		<-release
		return memory.NewStorage(), nil
	}

	created := make(chan *store.Repo, 1)
	go func() { created <- st.CreateRepo(owner, "unlocked-create", "", false) }()
	<-started

	readFinished := make(chan *store.User, 1)
	go func() { readFinished <- st.GetUserByID(owner.ID) }()
	select {
	case user := <-readFinished:
		if user == nil || user.ID != owner.ID {
			t.Fatalf("unrelated read returned %#v", user)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary store read blocked behind repository storage initialization")
	}
	if duplicate := st.CreateRepo(owner, "unlocked-create", "", false); duplicate != nil {
		t.Fatalf("duplicate creation bypassed pending-name reservation: %#v", duplicate)
	}
	close(release)
	if repo := <-created; repo == nil {
		t.Fatal("repository was not created after storage initialization")
	}
}

func TestRepositoryForkCopyDoesNotHoldStoreLock(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	owner := st.UsersByLogin["admin"]
	source := st.CreateRepo(owner, "fork-source", "", false)
	if source == nil {
		t.Fatal("create source repository")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	st.RepoStorageOpen = func(context.Context, string) (gitStorage.Storer, error) {
		close(started)
		<-release
		return memory.NewStorage(), nil
	}

	created := make(chan *store.Repo, 1)
	go func() { created <- st.ForkRepo(owner, source, "unlocked-fork") }()
	<-started

	readFinished := make(chan *store.User, 1)
	go func() { readFinished <- st.GetUserByID(owner.ID) }()
	select {
	case user := <-readFinished:
		if user == nil || user.ID != owner.ID {
			t.Fatalf("unrelated read returned %#v", user)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary store read blocked behind repository fork storage copy")
	}
	if duplicate := st.CreateRepo(owner, "unlocked-fork", "", false); duplicate != nil {
		t.Fatalf("duplicate creation bypassed fork name reservation: %#v", duplicate)
	}
	close(release)
	if fork := <-created; fork == nil || fork.ParentID != source.ID {
		t.Fatalf("repository fork = %#v", fork)
	}
}
