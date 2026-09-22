package bleephub

import (
	"fmt"
	"strings"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

// The commit helpers as they were before they edited trees directly: through a
// go-git worktree checked out in memory. Kept here, and only here, as the
// reference the tree edits are held to (git_tree_edit_test.go).

type referenceHeadStorer struct {
	gitStorage.Storer
	head *plumbing.Reference
}

func newReferenceHeadStorer(stor gitStorage.Storer) *referenceHeadStorer {
	return &referenceHeadStorer{
		Storer: stor,
		// git.Open rejects a storer with no HEAD; the first Checkout replaces
		// this placeholder.
		head: plumbing.NewHashReference(plumbing.HEAD, plumbing.ZeroHash),
	}
}

func (s *referenceHeadStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if name == plumbing.HEAD {
		return s.head, nil
	}
	return s.Storer.Reference(name)
}

func (s *referenceHeadStorer) SetReference(ref *plumbing.Reference) error {
	if ref.Name() == plumbing.HEAD {
		s.head = ref
		return nil
	}
	return s.Storer.SetReference(ref)
}

func (s *referenceHeadStorer) CheckAndSetReference(next, old *plumbing.Reference) error {
	if next.Name() == plumbing.HEAD {
		s.head = next
		return nil
	}
	return s.Storer.CheckAndSetReference(next, old)
}

func referenceRootCommit(stor gitStorage.Storer, branch, message string, files map[string]string, sig *object.Signature, requireEmpty bool, guard func(plumbing.Hash) error) (plumbing.Hash, error) {
	fs := memfs.New()
	// Build the unborn-branch commit in an isolated storer: Worktree.Commit
	// advances refs/heads/master as a side effect, which would expose a
	// provisional ref before the atomic initialization boundary and let
	// concurrent first-commit requests overwrite each other.
	source := memory.NewStorage()
	repo, err := git.Init(source, fs)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git init: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: %w", err)
	}
	if err := referenceWriteFiles(fs, wt, files); err != nil {
		return plumbing.ZeroHash, err
	}
	commitHash, err := wt.Commit(message, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	for _, objectType := range []plumbing.ObjectType{plumbing.BlobObject, plumbing.TreeObject, plumbing.CommitObject} {
		objects, err := source.IterEncodedObjects(objectType)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("iterate initial %s objects: %w", objectType, err)
		}
		copyErr := objects.ForEach(func(encoded plumbing.EncodedObject) error {
			return copyEncodedObject(stor, encoded)
		})
		objects.Close()
		if copyErr != nil {
			return plumbing.ZeroHash, fmt.Errorf("store initial %s objects: %w", objectType, copyErr)
		}
	}
	branchRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), commitHash)
	if guard != nil {
		if err := guard(commitHash); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if err := gitstore.InitializeRepositoryReferences(stor, branchRef, requireEmpty); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("initialize refs: %w", err)
	}
	return commitHash, nil
}

func referenceFileCommit(stor gitStorage.Storer, branch, path, content, message string, sig *object.Signature, expectedParent plumbing.Hash, guard func(plumbing.Hash) error) (plumbing.Hash, error) {
	fs := memfs.New()
	repo, err := git.Open(newReferenceHeadStorer(stor), fs)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git open: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: %w", err)
	}

	branchRef := plumbing.NewBranchReferenceName(branch)
	ref, err := repo.Storer.Reference(branchRef)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	parentHash := ref.Hash()
	if !expectedParent.IsZero() && parentHash != expectedParent {
		return plumbing.ZeroHash, gitStorage.ErrReferenceHasChanged
	}

	if err := wt.Checkout(&git.CheckoutOptions{Hash: parentHash, Force: true}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("checkout: %w", err)
	}

	if err := referenceWriteFile(fs, wt, path, content); err != nil {
		return plumbing.ZeroHash, err
	}

	commitHash, err := wt.Commit(message, &git.CommitOptions{
		Author:    sig,
		Committer: sig,
		Parents:   []plumbing.Hash{parentHash},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	if guard != nil {
		if err := guard(commitHash); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if err := repo.Storer.CheckAndSetReference(plumbing.NewHashReference(branchRef, commitHash), ref); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set ref: %w", err)
	}
	return commitHash, nil
}

func referenceDeleteCommit(stor gitStorage.Storer, branch, path, message string, sig *object.Signature, expectedParent plumbing.Hash, guard func(plumbing.Hash) error) (plumbing.Hash, error) {
	fs := memfs.New()
	repo, err := git.Open(newReferenceHeadStorer(stor), fs)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git open: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: %w", err)
	}

	branchRef := plumbing.NewBranchReferenceName(branch)
	ref, err := repo.Storer.Reference(branchRef)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	parentHash := ref.Hash()
	if !expectedParent.IsZero() && parentHash != expectedParent {
		return plumbing.ZeroHash, gitStorage.ErrReferenceHasChanged
	}

	if err := wt.Checkout(&git.CheckoutOptions{Hash: parentHash, Force: true}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("checkout: %w", err)
	}

	if _, err := fs.Stat(path); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("path does not exist: %s", path)
	}

	if _, err := wt.Remove(path); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git remove %s: %w", path, err)
	}

	commitHash, err := wt.Commit(message, &git.CommitOptions{
		Author:    sig,
		Committer: sig,
		Parents:   []plumbing.Hash{parentHash},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	if guard != nil {
		if err := guard(commitHash); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if err := repo.Storer.CheckAndSetReference(plumbing.NewHashReference(branchRef, commitHash), ref); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set ref: %w", err)
	}
	return commitHash, nil
}

func referenceMultiFileCommit(stor gitStorage.Storer, branch string, additions map[string][]byte, deletions []string,
	message string, sig *object.Signature, expectedParent plumbing.Hash) (plumbing.Hash, error) {
	fs := memfs.New()
	repo, err := git.Open(newReferenceHeadStorer(stor), fs)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git open: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: %w", err)
	}
	branchRef := plumbing.NewBranchReferenceName(branch)
	ref, err := repo.Storer.Reference(branchRef)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	parentHash := ref.Hash()
	if !expectedParent.IsZero() && parentHash != expectedParent {
		return plumbing.ZeroHash, gitStorage.ErrReferenceHasChanged
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: parentHash, Force: true}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("checkout: %w", err)
	}
	for path, body := range additions {
		if err := referenceWriteFile(fs, wt, path, string(body)); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	for _, path := range deletions {
		if _, err := fs.Stat(path); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("path does not exist: %s", path)
		}
		if _, err := wt.Remove(path); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("git remove %s: %w", path, err)
		}
	}
	commitHash, err := wt.Commit(message, &git.CommitOptions{
		Author:    sig,
		Committer: sig,
		Parents:   []plumbing.Hash{parentHash},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	if err := repo.Storer.CheckAndSetReference(plumbing.NewHashReference(branchRef, commitHash), ref); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set ref: %w", err)
	}
	return commitHash, nil
}

func referenceWriteFiles(fs billy.Filesystem, wt *git.Worktree, files map[string]string) error {
	for path, body := range files {
		if err := referenceWriteFile(fs, wt, path, body); err != nil {
			return err
		}
	}
	return nil
}

func referenceWriteFile(fs billy.Filesystem, wt *git.Worktree, path, body string) error {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		if err := fs.MkdirAll(path[:idx], 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", path[:idx], err)
		}
	}
	f, err := fs.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write([]byte(body)); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if _, err := wt.Add(path); err != nil {
		return fmt.Errorf("git add %s: %w", path, err)
	}
	return nil
}
