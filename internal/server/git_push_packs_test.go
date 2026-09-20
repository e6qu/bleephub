package bleephub

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/gitbackend"
)

// looseAndPackedKeys splits a repository's object keys in the bucket into the
// loose tier and published packs.
func looseAndPackedKeys(t *testing.T, name string) (loose, packs []string) {
	t.Helper()
	objectStore, err := gitbackend.GetStore(context.Background())
	if err != nil || objectStore == nil {
		t.Fatalf("open the object store: %v", err)
	}
	prefix := "git/admin/" + name + "/objects/"
	for _, key := range listS3RawKeys(t, objectStore, prefix) {
		rest := strings.TrimPrefix(key, prefix)
		switch {
		case strings.HasPrefix(rest, "pack/") && strings.HasSuffix(rest, ".pack"):
			packs = append(packs, rest)
		case !strings.HasPrefix(rest, "pack/"):
			loose = append(loose, rest)
		}
	}
	return loose, packs
}

// pushedSource is large and repetitive enough that git sends a one-line change
// to it as a delta against the revision the server already holds.
func pushedSource(revision string) string {
	var body strings.Builder
	for line := range 400 {
		fmt.Fprintf(&body, "func handler%d() string { return \"value %d\" }\n", line, line)
	}
	body.WriteString("// " + revision + "\n")
	return body.String()
}

// TestStockGitPushesLandAsPacksOnObjectStorage is the end-to-end contract of the
// object-store backend with an unmodified git client: a first push and then an
// ordinary incremental push — which git sends as a thin pack — must both be
// accepted, must land in the bucket as packfiles rather than as one key per
// object, and must clone back intact on a client that shares nothing with the
// pusher.
func TestStockGitPushesLandAsPacksOnObjectStorage(t *testing.T) {
	git := requireGitCLI(t)
	srv := newS3GitServerForTest(t)
	const name = "stock-git-push"
	admin := srv.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("admin user is missing")
	}
	if srv.store.CreateRepo(admin, name, "stock git push fixture", false) == nil {
		t.Fatalf("create repo %s", name)
	}
	remote := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/" + name + ".git"

	root := t.TempDir()
	work := filepath.Join(root, "work")
	git.run(root, "init", "--quiet", "--initial-branch=main", work)
	git.run(work, "config", "user.name", "Pusher")
	git.run(work, "config", "user.email", "pusher@bleephub.invalid")
	commit := func(revision string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "main.go"), []byte(pushedSource(revision)), 0o600); err != nil {
			t.Fatalf("write main.go: %v", err)
		}
		git.run(work, "add", "main.go")
		git.run(work, "commit", "--quiet", "-m", revision)
	}

	for revision := range 5 {
		commit(fmt.Sprintf("first push, revision %d", revision))
	}
	git.run(work, "push", "--quiet", remote, "main")

	loose, packs := looseAndPackedKeys(t, name)
	if len(loose) != 0 {
		t.Fatalf("the first push left %d loose objects in the bucket: %v", len(loose), loose)
	}
	if len(packs) != 1 {
		t.Fatalf("the first push published %d packs, want 1: %v", len(packs), packs)
	}

	commit("second push")
	trace := git.with("GIT_TRACE=1").run(work, "push", remote, "main")
	if !strings.Contains(trace, "--thin") {
		t.Fatalf("premise broken: git did not send a thin pack, so the incremental path is not exercised:\n%s", trace)
	}

	loose, packs = looseAndPackedKeys(t, name)
	if len(loose) != 0 {
		t.Fatalf("the incremental push left %d loose objects in the bucket: %v", len(loose), loose)
	}
	if len(packs) != 2 {
		t.Fatalf("two pushes published %d packs, want 2: %v", len(packs), packs)
	}

	clone := filepath.Join(root, "clone")
	git.run(root, "clone", "--quiet", remote, clone)
	git.run(clone, "fsck", "--no-progress", "--strict")
	requireCommitCount(t, git, clone, "HEAD", 6)
	got, err := os.ReadFile(filepath.Join(clone, "main.go"))
	if err != nil {
		t.Fatalf("read the clone: %v", err)
	}
	if string(got) != pushedSource("second push") {
		t.Fatal("the clone's checkout is not the file that was pushed")
	}
}
