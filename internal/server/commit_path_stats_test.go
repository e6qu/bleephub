package bleephub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// everyChangeHistory makes, with stock git, a history of every kind of change a
// path can go through: nested edits, a mode change, a directory becoming a file
// and back, a rename, a directory deleted whole, and an empty commit.
func everyChangeHistory(t *testing.T) (*filesystem.Storage, *object.Commit) {
	t.Helper()
	git := requireGitCLI(t)
	work := t.TempDir()
	git.run(work, "init", "-q", "-b", "main")
	git.run(work, "config", "user.name", "History")
	git.run(work, "config", "user.email", "history@bleephub.invalid")
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
	remove := func(path string) {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(work, filepath.FromSlash(path))); err != nil {
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
	remove("docs")
	write("docs", "now a file\n")
	commit("a directory becomes a file")
	remove("docs")
	write("docs/again.md", "a directory again\n")
	commit("a file becomes a directory")
	git.run(work, "mv", "src/a/two.go", "src/b/two.go")
	commit("rename")
	remove("src/a")
	commit("a directory deleted whole")
	commit("nothing changed")

	stored := filesystem.NewStorage(osfs.New(filepath.Join(work, ".git")), cache.NewObjectLRUDefault())
	tip, err := object.GetCommit(stored, plumbing.NewHash(strings.TrimSpace(git.run(work, "rev-parse", "HEAD"))))
	if err != nil {
		t.Fatal(err)
	}
	return stored, tip
}

// diffTouchesPath is the reference: a whole-tree diff against the first
// parent, matching the path itself or anything under it.
func diffTouchesPath(t *testing.T, commit *object.Commit, requested string) bool {
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
		for _, name := range []string{change.From.Name, change.To.Name} {
			if name != "" && (name == requested || strings.HasPrefix(name, requested+"/")) {
				return true
			}
		}
	}
	return false
}

// TestCommitTouchesPathAgreesWithAWholeTreeDiff holds the path filter of the
// commit list, which compares the entries at the path in a commit and its
// parent, to a diff of the two whole trees, for every commit of a history of
// every kind of change and every path that exists anywhere in it, plus paths
// that never did.
func TestCommitTouchesPathAgreesWithAWholeTreeDiff(t *testing.T) {
	t.Parallel()
	_, tip := everyChangeHistory(t)
	var commits []*object.Commit
	paths := map[string]bool{"missing": true, "src/missing/deeper": true, "README.md/inside": true}
	if err := object.NewCommitPreorderIter(tip, nil, nil).ForEach(func(c *object.Commit) error {
		commits = append(commits, c)
		tree, err := c.Tree()
		if err != nil {
			return err
		}
		walker := object.NewTreeWalker(tree, true, nil)
		defer walker.Close()
		for {
			name, _, err := walker.Next()
			if err != nil {
				return nil
			}
			paths[name] = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(commits) != 8 || len(paths) < 12 {
		t.Fatalf("premise: %d commits and %d paths", len(commits), len(paths))
	}
	touched := 0
	for _, c := range commits {
		for path := range paths {
			want := diffTouchesPath(t, c, path)
			got, err := store.CommitTouchesPath(c, path)
			if err != nil {
				t.Fatalf("%q, %s: %v", strings.TrimSpace(c.Message), path, err)
			}
			if got != want {
				t.Errorf("%q touches %s: %v, the whole-tree diff says %v", strings.TrimSpace(c.Message), path, got, want)
			}
			if want {
				touched++
			}
		}
	}
	if touched == 0 {
		t.Fatal("premise: no commit touched any path")
	}
}

// TestCommitLineCountsAreRememberedAndStayRight pins the statistics endpoints'
// memo: what it answers is what counting the commit gives, and a full memo
// forgets its oldest commit and nothing else.
func TestCommitLineCountsAreRememberedAndStayRight(t *testing.T) {
	t.Parallel()
	_, tip := everyChangeHistory(t)
	if err := object.NewCommitPreorderIter(tip, nil, nil).ForEach(func(c *object.Commit) error {
		stats, err := c.Stats()
		if err != nil {
			return err
		}
		wantAdds, wantDels := 0, 0
		for _, file := range stats {
			wantAdds += file.Addition
			wantDels += file.Deletion
		}
		for pass := range 2 {
			if adds, dels := commitLineStats(c); adds != wantAdds || dels != wantDels {
				t.Errorf("pass %d: %q counted +%d -%d, want +%d -%d", pass, strings.TrimSpace(c.Message), adds, dels, wantAdds, wantDels)
			}
		}
		if _, remembered := commitLineCounts.Get(c.Hash); !remembered {
			t.Errorf("%q was counted and not remembered", strings.TrimSpace(c.Message))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	memo := store.NewCommitCountMemo(2)
	a, b, c := plumbing.NewHash("aa"), plumbing.NewHash("bb"), plumbing.NewHash("cc")
	memo.Put(a, [3]int{1, 1, 1})
	memo.Put(b, [3]int{2, 2, 2})
	memo.Put(a, [3]int{9, 9, 9})
	memo.Put(c, [3]int{3, 3, 3})
	if _, held := memo.Get(a); held {
		t.Error("a full memo kept its oldest commit")
	}
	for commit, want := range map[plumbing.Hash][3]int{b: {2, 2, 2}, c: {3, 3, 3}} {
		if got, held := memo.Get(commit); !held || got != want {
			t.Errorf("the memo holds %v (%v) for %s, want %v", got, held, commit, want)
		}
	}
}
