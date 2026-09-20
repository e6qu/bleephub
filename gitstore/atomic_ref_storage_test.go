package gitstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestAtomicRefStorage_CreateIsExclusive(t *testing.T) {
	stor := WrapAtomicRefStorage("owner/repo", memory.NewStorage())
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
			results <- CreateReferenceIfAbsent(stor, ref)
		}(ref)
	}
	ready.Wait()
	close(start)
	created, rejected := 0, 0
	for range refs {
		switch err := <-results; {
		case err == nil:
			created++
		case errors.Is(err, ErrReferenceAlreadyExists):
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
	stor := WrapAtomicRefStorage("owner/repo", memory.NewStorage())
	name := plumbing.NewBranchReferenceName("main")
	old := plumbing.NewHashReference(name, plumbing.NewHash("1111111111111111111111111111111111111111"))
	if err := stor.SetReference(old); err != nil {
		t.Fatal(err)
	}
	next := plumbing.NewHashReference(name, plumbing.NewHash("2222222222222222222222222222222222222222"))
	if err := stor.CheckAndSetReference(next, old); err != nil {
		t.Fatal(err)
	}
	if err := RemoveReferenceCAS(stor, old); !errors.Is(err, gitStorage.ErrReferenceHasChanged) {
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
	stor := WrapAtomicRefStorage("owner/repo", memory.NewStorage())
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
			results <- InitializeRepositoryReferences(stor, branch, true)
		}(branch)
	}
	ready.Wait()
	close(start)
	initialized, rejected := 0, 0
	for range branches {
		switch err := <-results; {
		case err == nil:
			initialized++
		case errors.Is(err, ErrReferenceAlreadyExists):
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
		stor := WrapAtomicRefStorage("owner/repo", memory.NewStorage())
		ref := plumbing.NewHashReference(name, hash)
		if err := stor.SetReference(ref); !errors.Is(err, ErrUnsafeReferenceName) {
			t.Errorf("SetReference(%q): err = %v, want ErrUnsafeReferenceName", name, err)
		}
		// Nothing may have been written under the crafted name.
		if _, err := stor.Reference(name); err == nil {
			t.Errorf("SetReference(%q) stored a ref despite the rejection", name)
		}
		if err := stor.RemoveReference(name); !errors.Is(err, ErrUnsafeReferenceName) {
			t.Errorf("RemoveReference(%q): err = %v, want ErrUnsafeReferenceName", name, err)
		}
		if err := stor.CheckAndSetReference(ref, nil); !errors.Is(err, ErrUnsafeReferenceName) {
			t.Errorf("CheckAndSetReference(%q): err = %v, want ErrUnsafeReferenceName", name, err)
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
		stor := WrapAtomicRefStorage("owner/repo", memory.NewStorage())
		if err := stor.SetReference(plumbing.NewHashReference(name, hash)); err != nil {
			t.Errorf("SetReference(%q): unexpected error %v", name, err)
		}
	}
}

// storeTestBlob writes a blob whose contents are derived from body and returns
// its hash, exercising the object-write path of the wrapped storer.
func storeTestBlob(t *testing.T, stor gitStorage.Storer, body string) plumbing.Hash {
	t.Helper()
	obj := stor.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(body)))
	w, err := obj.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}
	hash, err := stor.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}
	return hash
}

// TestAtomicRefStorage_ConcurrentReadsAndWritesAreSerialized reproduces the
// production shape of a `git clone` (reference iteration plus object reads on a
// connection goroutine) overlapping a `git push` or a REST git-data write
// (reference and object writes on a request goroutine) against the one storer
// instance the store keeps per repository. Before the storage lock the go-git
// reference and object maps were read and written with no happens-before edge,
// which the race detector flags and which can corrupt a Go map outright.
func TestAtomicRefStorage_ConcurrentReadsAndWritesAreSerialized(t *testing.T) {
	stor := WrapAtomicRefStorage("owner/repo", memory.NewStorage())
	seed := storeTestBlob(t, stor, "seed")
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), seed)); err != nil {
		t.Fatalf("seed ref: %v", err)
	}

	const iterations = 60
	start := make(chan struct{})
	errs := make(chan error, 4)
	var wg sync.WaitGroup

	writer := func(prefix string) {
		defer wg.Done()
		<-start
		for i := range iterations {
			hash := storeTestBlob(t, stor, fmt.Sprintf("%s-%d", prefix, i))
			name := plumbing.NewBranchReferenceName(fmt.Sprintf("%s-%d", prefix, i))
			if err := stor.SetReference(plumbing.NewHashReference(name, hash)); err != nil {
				errs <- err
				return
			}
			if err := stor.RemoveReference(name); err != nil {
				errs <- err
				return
			}
		}
	}
	reader := func() {
		defer wg.Done()
		<-start
		for range iterations {
			refs, err := stor.IterReferences()
			if err != nil {
				errs <- err
				return
			}
			// Reading the storer from inside the walk is what gh_import's
			// commit decoding does; it must not deadlock against a queued
			// writer, so the iterator may not hold the lock across this
			// callback.
			if err := refs.ForEach(func(ref *plumbing.Reference) error {
				if ref.Type() != plumbing.HashReference {
					return nil
				}
				if _, err := stor.EncodedObject(plumbing.AnyObject, ref.Hash()); err != nil &&
					!errors.Is(err, plumbing.ErrObjectNotFound) {
					return err
				}
				return nil
			}); err != nil {
				errs <- err
				return
			}
			objects, err := stor.IterEncodedObjects(plumbing.BlobObject)
			if err != nil {
				errs <- err
				return
			}
			if err := objects.ForEach(func(obj plumbing.EncodedObject) error {
				r, err := obj.Reader()
				if err != nil {
					return err
				}
				return r.Close()
			}); err != nil {
				errs <- err
				return
			}
		}
	}

	wg.Add(4)
	go writer("alpha")
	go writer("beta")
	go reader()
	go reader()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent storer access: %v", err)
	}
}

// TestAtomicRefStorage_WriteDuringIterationDoesNotDeadlock pins the choice not
// to hold the storage lock across an iterator's callback: callers such as
// copyGitStorage and the ref-pruning paths write to a storer from inside its
// own ForEach, and a held write- or read-lock would self-deadlock there.
func TestAtomicRefStorage_WriteDuringIterationDoesNotDeadlock(t *testing.T) {
	stor := WrapAtomicRefStorage("owner/repo", memory.NewStorage())
	hash := storeTestBlob(t, stor, "tip")
	for _, name := range []string{"main", "topic", "release"} {
		if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), hash)); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	done := make(chan error, 1)
	go func() {
		refs, err := stor.IterReferences()
		if err != nil {
			done <- err
			return
		}
		done <- refs.ForEach(func(ref *plumbing.Reference) error {
			if !ref.Name().IsBranch() {
				return nil
			}
			return stor.RemoveReference(ref.Name())
		})
	}()

	if err := <-done; err != nil {
		t.Fatalf("write during iteration: %v", err)
	}
	remaining, err := stor.IterReferences()
	if err != nil {
		t.Fatal(err)
	}
	branches := 0
	if err := remaining.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() {
			branches++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if branches != 0 {
		t.Fatalf("branches remaining = %d, want 0", branches)
	}
}
