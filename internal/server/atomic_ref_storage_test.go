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

// TestAtomicRefStorage_RejectsUnsafeRefNames covers STORE-049: a reference name
// that could escape the per-repository storage path (a `..` segment or a
// backslash) is refused at the storer boundary, before any backend composes it
// into a filesystem/S3 key. Legitimate refs are unaffected.
func TestAtomicRefStorage_RejectsUnsafeRefNames(t *testing.T) {
	hash := plumbing.NewHash("1111111111111111111111111111111111111111")

	unsafe := []plumbing.ReferenceName{
		"refs/heads/../../evil",
		"refs/heads/x/../../../other-repo/refs/heads/main",
		"refs/heads/..",
		`refs/heads/back\slash`,
		"refs/heads/", // trailing empty segment
	}
	for _, name := range unsafe {
		stor := wrapAtomicRefStorage("owner/repo", memory.NewStorage())
		ref := plumbing.NewHashReference(name, hash)
		if err := stor.SetReference(ref); !errors.Is(err, errUnsafeReferenceName) {
			t.Errorf("SetReference(%q): err = %v, want errUnsafeReferenceName", name, err)
		}
		// Nothing may have been written under the crafted name.
		if _, err := stor.Reference(name); err == nil {
			t.Errorf("SetReference(%q) stored a ref despite the rejection", name)
		}
		if err := stor.RemoveReference(name); !errors.Is(err, errUnsafeReferenceName) {
			t.Errorf("RemoveReference(%q): err = %v, want errUnsafeReferenceName", name, err)
		}
		if err := stor.CheckAndSetReference(ref, nil); !errors.Is(err, errUnsafeReferenceName) {
			t.Errorf("CheckAndSetReference(%q): err = %v, want errUnsafeReferenceName", name, err)
		}
	}

	// Every legitimate ref bleephub stores must still be accepted.
	safe := []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName("main"),
		"refs/pull/1/merge",
		"refs/tags/v1.0.0",
		plumbing.HEAD,
	}
	for _, name := range safe {
		stor := wrapAtomicRefStorage("owner/repo", memory.NewStorage())
		if err := stor.SetReference(plumbing.NewHashReference(name, hash)); err != nil {
			t.Errorf("SetReference(%q): unexpected error %v", name, err)
		}
	}
}
