package bleephub

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// goGitFileChanges is what go-git's own tree diff reports, the reference.
func goGitFileChanges(t *testing.T, commit *object.Commit) (added, removed, modified []string) {
	t.Helper()
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	var parentTree *object.Tree
	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		if err != nil {
			t.Fatal(err)
		}
		if parentTree, err = parent.Tree(); err != nil {
			t.Fatal(err)
		}
	}
	changes, err := object.DiffTree(parentTree, tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		switch {
		case change.From.Name == "":
			added = append(added, change.To.Name)
		case change.To.Name == "":
			removed = append(removed, change.From.Name)
		default:
			modified = append(modified, change.To.Name)
		}
	}
	return added, removed, modified
}

func sortedPaths(paths []string) []string {
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	return sorted
}

// TestPushFileChangesAreGoGitsWithoutReadingABlob holds the push payload's
// per-commit file lists to go-git's tree diff over a history stock git made to
// exercise every kind of change — nested edits, a mode change, a file becoming
// a directory and back, a rename, a directory deleted whole, and the root
// commit — and pins that listing them reads no file content.
func TestPushFileChangesAreGoGitsWithoutReadingABlob(t *testing.T) {
	git := requireGitCLI(t)
	work := t.TempDir()
	git.run(work, "init", "-q", "-b", "main")
	git.run(work, "config", "user.name", "Payload")
	git.run(work, "config", "user.email", "payload@bleephub.invalid")
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(work, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) {
		t.Helper()
		git.run(work, "add", "-A")
		git.run(work, "commit", "-q", "--allow-empty", "-m", message)
	}

	write("README.md", "root\n")
	write("src/a/one.go", "one\n")
	write("src/a/two.go", "two\n")
	write("src/b/three.go", "three\n")
	write("docs/guide.md", "guide\n")
	commit("root")
	write("src/a/one.go", "one, changed\n")
	write("src/c/new.go", "new\n")
	commit("nested edit and a new directory")
	git.run(work, "update-index", "--chmod=+x", "src/b/three.go")
	git.run(work, "commit", "-q", "-m", "mode change")
	if err := os.RemoveAll(filepath.Join(work, "docs")); err != nil {
		t.Fatal(err)
	}
	write("docs", "now a file\n")
	commit("a directory becomes a file")
	if err := os.Remove(filepath.Join(work, "docs")); err != nil {
		t.Fatal(err)
	}
	write("docs/again.md", "a directory again\n")
	commit("a file becomes a directory")
	git.run(work, "mv", "src/a/two.go", "src/b/two.go")
	commit("rename")
	if err := os.RemoveAll(filepath.Join(work, "src", "a")); err != nil {
		t.Fatal(err)
	}
	commit("a directory deleted whole")
	commit("nothing changed")

	stored := filesystem.NewStorage(osfs.New(filepath.Join(work, ".git")), cache.NewObjectLRUDefault())
	counted := &blobCountingStorer{Storer: stored}
	head := plumbing.NewHash(strings.TrimSpace(git.run(work, "rev-parse", "HEAD")))
	tip, err := object.GetCommit(stored, head)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	err = object.NewCommitPreorderIter(tip, nil, nil).ForEach(func(c *object.Commit) error {
		wantAdded, wantRemoved, wantModified := goGitFileChanges(t, c)
		added, removed, modified := commitFileChanges(counted, c)
		for kind, pair := range map[string][2][]string{
			"added": {added, wantAdded}, "removed": {removed, wantRemoved}, "modified": {modified, wantModified},
		} {
			if !slices.Equal(sortedPaths(pair[0]), sortedPaths(pair[1])) {
				t.Errorf("%q: %s %v, go-git %v", strings.TrimSpace(c.Message), kind, pair[0], pair[1])
			}
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked != 8 {
		t.Fatalf("premise broken: compared %d commits, want 8", checked)
	}
	if blobs := counted.blobs; blobs != 0 {
		t.Fatalf("listing the changes read %d blobs, want none", blobs)
	}
}
