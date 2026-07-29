package bleephub

import (
	"errors"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestAtomicRefStorage_CreateIsExclusive(t *testing.T) {
	stor := wrapAtomicRefStorage("owner/repo", memory.NewStorage())
	name := plumbing.NewBranchReferenceName("main")
	refs := []*plumbing.Reference{
		plumbing.NewHashReference(name, plumbing.NewHash("1111111111111111111111111111111111111111")),
		plumbing.NewHashReference(name, plumbing.NewHash("2222222222222222222222222222222222222222")),
	}
	start := make(chan struct{})
	results := make(chan error, len(refs))
	var ready sync.WaitGroup
	ready.Add(len(refs))
	for _, ref := range refs {
		go func(ref *plumbing.Reference) {
			ready.Done()
			<-start
			results <- createReferenceIfAbsent(stor, ref)
		}(ref)
	}
	ready.Wait()
	close(start)
	created, rejected := 0, 0
	for range refs {
		switch err := <-results; {
		case err == nil:
			created++
		case errors.Is(err, errReferenceAlreadyExists):
			rejected++
		default:
			t.Fatalf("create ref: %v", err)
		}
	}
	if created != 1 || rejected != 1 {
		t.Fatalf("created=%d rejected=%d, want one of each", created, rejected)
	}
}

func TestAtomicRefStorage_StaleDeleteCannotRemoveNewValue(t *testing.T) {
	stor := wrapAtomicRefStorage("owner/repo", memory.NewStorage())
	name := plumbing.NewBranchReferenceName("main")
	old := plumbing.NewHashReference(name, plumbing.NewHash("1111111111111111111111111111111111111111"))
	if err := stor.SetReference(old); err != nil {
		t.Fatal(err)
	}
	next := plumbing.NewHashReference(name, plumbing.NewHash("2222222222222222222222222222222222222222"))
	if err := stor.CheckAndSetReference(next, old); err != nil {
		t.Fatal(err)
	}
	if err := removeReferenceCAS(stor, old); !errors.Is(err, gitStorage.ErrReferenceHasChanged) {
		t.Fatalf("stale delete error = %v, want ErrReferenceHasChanged", err)
	}
	got, err := stor.Reference(name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash() != next.Hash() {
		t.Fatalf("stale delete changed ref to %s, want %s", got.Hash(), next.Hash())
	}
}

func TestAtomicRefStorage_RepositoryInitializationIsExclusiveAcrossBranches(t *testing.T) {
	stor := wrapAtomicRefStorage("owner/repo", memory.NewStorage())
	branches := []*plumbing.Reference{
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash("1111111111111111111111111111111111111111")),
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("trunk"), plumbing.NewHash("2222222222222222222222222222222222222222")),
	}
	start := make(chan struct{})
	results := make(chan error, len(branches))
	var ready sync.WaitGroup
	ready.Add(len(branches))
	for _, branch := range branches {
		go func(branch *plumbing.Reference) {
			ready.Done()
			<-start
			results <- initializeRepositoryReferences(stor, branch, true)
		}(branch)
	}
	ready.Wait()
	close(start)
	initialized, rejected := 0, 0
	for range branches {
		switch err := <-results; {
		case err == nil:
			initialized++
		case errors.Is(err, errReferenceAlreadyExists):
			rejected++
		default:
			t.Fatalf("initialize repository refs: %v", err)
		}
	}
	if initialized != 1 || rejected != 1 {
		t.Fatalf("initialized=%d rejected=%d, want one of each", initialized, rejected)
	}
	refs, err := stor.IterReferences()
	if err != nil {
		t.Fatal(err)
	}
	defer refs.Close()
	branchCount := 0
	if err := refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() {
			branchCount++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if branchCount != 1 {
		t.Fatalf("branch count = %d, want 1", branchCount)
	}
	head, err := stor.Reference(plumbing.HEAD)
	if err != nil {
		t.Fatal(err)
	}
	if head.Type() != plumbing.SymbolicReference {
		t.Fatalf("HEAD type = %v, want symbolic", head.Type())
	}
}

func TestGitPushCommandsHonorWireOldObjectID(t *testing.T) {
	stor := wrapAtomicRefStorage("owner/repo", memory.NewStorage())
	name := plumbing.NewBranchReferenceName("main")
	current := plumbing.NewHash("2222222222222222222222222222222222222222")
	if err := stor.SetReference(plumbing.NewHashReference(name, current)); err != nil {
		t.Fatal(err)
	}
	stale := plumbing.NewHash("1111111111111111111111111111111111111111")
	next := plumbing.NewHash("3333333333333333333333333333333333333333")
	for _, command := range []*packp.Command{
		{Name: name, Old: stale, New: next},
		{Name: name, Old: stale, New: plumbing.ZeroHash},
		{Name: name, Old: plumbing.ZeroHash, New: next},
	} {
		if err := applyPushCommandAtomic(stor, command); err == nil {
			t.Fatalf("%s with stale/duplicate precondition unexpectedly succeeded", command.Action())
		}
		got, err := stor.Reference(name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Hash() != current {
			t.Fatalf("%s changed ref to %s, want %s", command.Action(), got.Hash(), current)
		}
	}
}
