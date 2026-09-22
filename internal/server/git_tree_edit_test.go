package bleephub

import (
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"

	"github.com/e6qu/bleephub/gitstore"
)

// seedTreeEditRepository makes a repository whose main holds a regular file,
// an executable one, and files nested two directories down, written object by
// object so that the executable mode is in it from the start.
func seedTreeEditRepository(t *testing.T, sig *object.Signature) gitStorage.Storer {
	t.Helper()
	stor, err := gitstore.OpenMemory("admin/tree-edit")
	if err != nil {
		t.Fatal(err)
	}
	blob := func(body string) plumbing.Hash {
		t.Helper()
		hash, err := encodeBlob(stor, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	tree := func(entries ...object.TreeEntry) plumbing.Hash {
		t.Helper()
		encoded := stor.NewEncodedObject()
		if err := (&object.Tree{Entries: entries}).Encode(encoded); err != nil {
			t.Fatal(err)
		}
		hash, err := stor.SetEncodedObject(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	inner := tree(
		object.TreeEntry{Name: "one.go", Mode: filemode.Regular, Hash: blob("one\n")},
		object.TreeEntry{Name: "two.go", Mode: filemode.Regular, Hash: blob("two\n")},
	)
	src := tree(object.TreeEntry{Name: "a", Mode: filemode.Dir, Hash: inner})
	lonely := tree(object.TreeEntry{Name: "only.md", Mode: filemode.Regular, Hash: blob("only\n")})
	root := tree(
		object.TreeEntry{Name: "README.md", Mode: filemode.Regular, Hash: blob("readme\n")},
		object.TreeEntry{Name: "docs", Mode: filemode.Dir, Hash: lonely},
		object.TreeEntry{Name: "run.sh", Mode: filemode.Executable, Hash: blob("#!/bin/sh\n")},
		object.TreeEntry{Name: "src", Mode: filemode.Dir, Hash: src},
	)
	commit, err := encodeCommit(stor, &object.Commit{Author: *sig, Committer: *sig, Message: "seed", TreeHash: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := gitstore.InitializeRepositoryReferences(stor, plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commit), true); err != nil {
		t.Fatal(err)
	}
	return stor
}

// TestTreeEditsCommitWhatAWorktreeCommits holds the API's commit helpers,
// which edit trees directly, to the go-git worktree they replaced
// (git_tree_edit_reference_test.go): on the same repository, the same change
// must make the same commit — the same id, and so the same tree to the byte —
// or be refused by both.
func TestTreeEditsCommitWhatAWorktreeCommits(t *testing.T) {
	t.Parallel()
	sig := &object.Signature{Name: "Editor", Email: "editor@bleephub.invalid", When: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	cases := []struct {
		name      string
		additions map[string][]byte
		deletions []string
	}{
		{"update a file", map[string][]byte{"README.md": []byte("changed\n")}, nil},
		{"update an executable", map[string][]byte{"run.sh": []byte("#!/bin/sh\necho\n")}, nil},
		{"add a nested file", map[string][]byte{"src/b/c/new.go": []byte("new\n")}, nil},
		{"add beside others", map[string][]byte{"src/a/three.go": []byte("three\n")}, nil},
		{"delete a file", nil, []string{"src/a/one.go"}},
		{"delete the last file of a directory", nil, []string{"docs/only.md"}},
		{"delete a directory", nil, []string{"src/a"}},
		{"delete what is not there", nil, []string{"src/missing.go"}},
		{"many at once", map[string][]byte{"src/a/one.go": []byte("one, again\n"), "lib/x.go": []byte("x\n"), "a.txt": []byte("a\n")}, []string{"src/a/two.go", "docs/only.md"}},
		{"write through a file", map[string][]byte{"README.md/inside": []byte("no\n")}, nil},
		{"write over a directory", map[string][]byte{"src": []byte("no\n")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reference := seedTreeEditRepository(t, sig)
			edited := seedTreeEditRepository(t, sig)
			want, wantErr := referenceMultiFileCommit(reference, "main", tc.additions, tc.deletions, "edit", sig, plumbing.ZeroHash)
			got, gotErr := multiFileCommit(edited, "main", tc.additions, tc.deletions, "edit", sig, plumbing.ZeroHash)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("the worktree answered %v (err %v), the tree edit %v (err %v)", want, wantErr, got, gotErr)
			}
			if wantErr == nil && got != want {
				t.Fatalf("the tree edit committed %s, the worktree %s", got, want)
			}
		})
	}

	t.Run("a root commit", func(t *testing.T) {
		files := map[string]string{"README.md": "hello\n", "src/a/one.go": "one\n", "src/b.go": "b\n", "z/y/x/w.txt": "deep\n"}
		reference, err := gitstore.OpenMemory("admin/root-reference")
		if err != nil {
			t.Fatal(err)
		}
		edited, err := gitstore.OpenMemory("admin/root-edited")
		if err != nil {
			t.Fatal(err)
		}
		want, err := referenceRootCommit(reference, "main", "init", files, sig, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := commitRootBranchWithFiles(edited, "main", "init", files, sig, true, nil)
		if err != nil || got != want {
			t.Fatalf("the tree edit's root commit is %s (%v), the worktree's %s", got, err, want)
		}
	})

	t.Run("the single-file write and delete", func(t *testing.T) {
		for _, path := range []string{"README.md", "src/a/new.go", "run.sh"} {
			reference := seedTreeEditRepository(t, sig)
			edited := seedTreeEditRepository(t, sig)
			want, wantErr := referenceFileCommit(reference, "main", path, "written\n", "write", sig, plumbing.ZeroHash, nil)
			got, gotErr := createFileCommitExpectedGuarded(edited, "main", path, "written\n", "write", sig, plumbing.ZeroHash, nil)
			if wantErr != nil || gotErr != nil || got != want {
				t.Fatalf("writing %s: the tree edit committed %s (%v), the worktree %s (%v)", path, got, gotErr, want, wantErr)
			}
		}
		for _, path := range []string{"README.md", "src/a/one.go", "docs/only.md", "nothing/here"} {
			reference := seedTreeEditRepository(t, sig)
			edited := seedTreeEditRepository(t, sig)
			want, wantErr := referenceDeleteCommit(reference, "main", path, "delete", sig, plumbing.ZeroHash, nil)
			got, gotErr := deleteFileCommit(edited, "main", path, "delete", sig, plumbing.ZeroHash, nil)
			if (wantErr == nil) != (gotErr == nil) || got != want {
				t.Fatalf("deleting %s: the tree edit committed %s (%v), the worktree %s (%v)", path, got, gotErr, want, wantErr)
			}
		}
	})

	t.Run("a branch that moved", func(t *testing.T) {
		edited := seedTreeEditRepository(t, sig)
		if _, err := multiFileCommit(edited, "main", map[string][]byte{"x": []byte("x")}, nil, "edit", sig, plumbing.NewHash("1111111111111111111111111111111111111111")); !errors.Is(err, gitStorage.ErrReferenceHasChanged) {
			t.Fatalf("a commit onto a head that is not the branch's answered %v, want ErrReferenceHasChanged", err)
		}
	})
}

// TestTreeEditsRefuseWhatGitRefusesInATree pins the path rules of a commit the
// API makes: ".git" in any disguise, at any depth, and control characters are
// refused — an entry named so is an attack on whoever checks the tree out — as
// the worktree the tree edits replaced refused them, and nothing is committed.
// Paths with "." or ".." components or an empty one are refused too, where the
// worktree's in-memory filesystem quietly cleaned them ("a/../b" was written as
// "b"); ordinary paths, awkward ones included, are accepted.
func TestTreeEditsRefuseWhatGitRefusesInATree(t *testing.T) {
	t.Parallel()
	sig := &object.Signature{Name: "Editor", Email: "editor@bleephub.invalid", When: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	refusedByBoth := []string{
		".git/config", "src/.git/hooks/pre-commit", ".GIT/config", "git~1/config",
		".g\u200cit/config", ".git./config", "a\x01b",
	}
	cleanedByTheWorktree := []string{"a/../b", "./a", "a//b"}
	refuse := func(path string, worktreeRefuses bool) {
		t.Helper()
		reference := seedTreeEditRepository(t, sig)
		edited := seedTreeEditRepository(t, sig)
		before, err := edited.Reference(plumbing.NewBranchReferenceName("main"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = referenceMultiFileCommit(reference, "main", map[string][]byte{path: []byte("x")}, nil, "edit", sig, plumbing.ZeroHash)
		if (err != nil) != worktreeRefuses {
			t.Errorf("premise: the worktree answered %v for %q", err, path)
		}
		if _, err := multiFileCommit(edited, "main", map[string][]byte{path: []byte("x")}, nil, "edit", sig, plumbing.ZeroHash); !errors.Is(err, errTreeEditPath) {
			t.Errorf("writing %q answered %v, want errTreeEditPath", path, err)
		}
		if after, _ := edited.Reference(plumbing.NewBranchReferenceName("main")); after.Hash() != before.Hash() {
			t.Errorf("a refused write of %q moved the branch", path)
		}
	}
	for _, path := range refusedByBoth {
		refuse(path, true)
	}
	for _, path := range cleanedByTheWorktree {
		refuse(path, false)
	}
	for _, path := range []string{"git/config", ".github/workflows/ci.yml", "a.git/b", "src/gitignore", "CON", "name with spaces.txt", ".gitignore"} {
		edited := seedTreeEditRepository(t, sig)
		if _, err := multiFileCommit(edited, "main", map[string][]byte{path: []byte("x")}, nil, "edit", sig, plumbing.ZeroHash); err != nil {
			t.Errorf("writing %q was refused: %v", path, err)
		}
	}
}
