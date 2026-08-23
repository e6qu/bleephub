package bleephub

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/e6qu/bleephub/internal/gitstore"
)

// bundleURIList runs the bundle-uri command over smart HTTP and returns the
// key=value lines of the bundle list it answered with.
func bundleURIList(t *testing.T, srv *isolatedServer, name string) []string {
	t.Helper()
	script := (&gitPktScript{}).
		linef("command=%s\n", gitBundleURICommand).
		linef("object-format=sha1\n").
		flush()
	return gitPktLines(t, postGitUploadPack(t, srv, name, script, map[string]string{gitProtocolHeader: "version=2"}))
}

// bundleListValue reads one key out of a bundle list.
func bundleListValue(lines []string, key string) (string, bool) {
	for _, line := range lines {
		if name, value, split := strings.Cut(line, "="); split && name == key {
			return value, true
		}
	}
	return "", false
}

// bundleListURI reads the single bundle URI a list names, and the id it is
// filed under.
func bundleListURI(t *testing.T, lines []string) (id, uri string) {
	t.Helper()
	for _, line := range lines {
		key, value, split := strings.Cut(line, "=")
		if !split || !strings.HasPrefix(key, "bundle.") || !strings.HasSuffix(key, ".uri") {
			continue
		}
		if id != "" {
			t.Fatalf("bundle list names more than one bundle: %v", lines)
		}
		id = strings.TrimSuffix(strings.TrimPrefix(key, "bundle."), ".uri")
		uri = value
	}
	if id == "" {
		t.Fatalf("bundle list names no bundle: %v", lines)
	}
	return id, uri
}

// TestBundleURIServesABundleOfTheCurrentRefs is the command's central claim:
// the list names a bundle, the bundle is fetchable without any credential of
// this server's, and what comes back is a bundle git itself accepts.
func TestBundleURIServesABundleOfTheCurrentRefs(t *testing.T) {
	requireGitCLI(t)
	srv := newS3GitServerForTest(t)
	const name = "bundleuri-serve"
	seedPackedS3Repo(t, srv, name)

	lines := bundleURIList(t, srv, name)
	if version, named := bundleListValue(lines, "bundle.version"); !named || version != "1" {
		t.Fatalf("bundle list states version %q, want 1: %v", version, lines)
	}
	if mode, named := bundleListValue(lines, "bundle.mode"); !named || mode != "all" {
		t.Fatalf("bundle list states mode %q, want all: %v", mode, lines)
	}
	_, uri := bundleListURI(t, lines)

	response, err := http.Get(uri) // #nosec G107 -- the URL under test
	if err != nil {
		t.Fatalf("fetch the offered bundle: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the offered bundle answered %d to a caller with no bleephub credential", response.StatusCode)
	}
	bundle, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(bundle, []byte(gitBundleSignature)) {
		t.Fatalf("the offered bundle does not open with a bundle signature: %q", bundle[:min(len(bundle), 32)])
	}

	// git's judgement, not this package's: a bundle it verifies and clones from
	// is a bundle, and the clone it produces is the repository.
	root := t.TempDir()
	path := filepath.Join(root, "repo.bundle")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	git := requireGitCLI(t)
	// `git bundle verify` reads the object database to decide whether the
	// prerequisites are satisfiable, so it has to run inside a repository. An
	// empty one is the honest place: it holds nothing, so a bundle it accepts
	// is a bundle that needs nothing.
	scratch := filepath.Join(root, "scratch")
	git.run(root, "init", "--quiet", scratch)
	git.run(scratch, "bundle", "verify", path)
	clone := filepath.Join(root, "from-bundle")
	git.run(root, "clone", path, clone)
	requireCommitCount(t, git, clone, "HEAD", packURIHistoryCommits)
	git.run(clone, "fsck", "--no-progress", "--strict")
}

// TestBundleURIIsRebuiltWhenTheRefsMove pins the staleness rule: the bundle is
// named by the ref state it covers, so a repository whose refs have moved is
// answered with a different bundle, and that bundle carries the new tip.
func TestBundleURIIsRebuiltWhenTheRefsMove(t *testing.T) {
	requireGitCLI(t)
	srv := newS3GitServerForTest(t)
	const name = "bundleuri-stale"
	seedPackedS3Repo(t, srv, name)

	before, _ := bundleListURI(t, bundleURIList(t, srv, name))

	stor := srv.store.GetGitStorage("admin", name)
	if stor == nil {
		t.Fatalf("repo %s has no git storage", name)
	}
	moved, err := createFileCommit(stor, "main", "f.txt", "moved on\n", "after the bundle", packReuseSignature(packURIHistoryCommits))
	if err != nil {
		t.Fatalf("move the branch: %v", err)
	}

	after, uri := bundleListURI(t, bundleURIList(t, srv, name))
	if before == after {
		t.Fatalf("the bundle for a moved branch is still named %s", after)
	}

	response, err := http.Get(uri) // #nosec G107 -- the URL under test
	if err != nil {
		t.Fatalf("fetch the rebuilt bundle: %v", err)
	}
	defer response.Body.Close()
	bundle, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "repo.bundle")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	git := requireGitCLI(t)
	if heads := git.run(root, "bundle", "list-heads", path); !strings.Contains(heads, moved.String()) {
		t.Fatalf("the rebuilt bundle does not carry the new tip %s:\n%s", moved, heads)
	}
	clone := filepath.Join(root, "from-bundle")
	git.run(root, "clone", path, clone)
	requireCommitCount(t, git, clone, "HEAD", packURIHistoryCommits+1)
	git.run(clone, "fsck", "--no-progress", "--strict")

	// The bundle the first request published is still there. A URL for it may
	// have been handed out moments before the branch moved, and it has the rest
	// of its expiry to run; sweeping it the instant it went stale would break a
	// download already under way.
	if entries := storedBundles(t, name); len(entries) != 2 {
		t.Fatalf("the repository holds %d bundles, want the stale one alongside the rebuilt one: %v", len(entries), entries)
	}
}

// storedBundles lists the bundle keys a repository holds in object storage.
func storedBundles(t *testing.T, name string) []string {
	t.Helper()
	objectStore, err := gitstore.GetS3FS(context.Background())
	if err != nil || objectStore == nil {
		t.Fatalf("open the object store: %v", err)
	}
	repoFS, err := objectStore.Chroot("admin/" + name)
	if err != nil {
		t.Fatalf("chroot the repository: %v", err)
	}
	entries, err := repoFS.ReadDir(gitBundleDirectory)
	if err != nil {
		t.Fatalf("list the bundle directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestGitClonesThroughTheBundleURI is the command checked against the client
// that has to use it: git, told to look for bundles, discovers one, unbundles
// it and finishes the clone from what the fetch still owes.
func TestGitClonesThroughTheBundleURI(t *testing.T) {
	requireGitCLI(t)
	srv := newS3GitServerForTest(t)
	const name = "bundleuri-clone"
	seedPackedS3Repo(t, srv, name)
	cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/" + name + ".git"

	root := t.TempDir()
	git := requireGitCLI(t).withGitConfig(
		[2]string{"protocol.version", "2"},
		[2]string{"transfer.bundleURI", "true"},
	)
	clone := filepath.Join(root, "bootstrapped")
	trace := git.with("GIT_TRACE_PACKET=1").run(root, "clone", cloneURL, clone)
	if !strings.Contains(trace, "command="+gitBundleURICommand) {
		t.Fatalf("the client never ran the bundle-uri command:\n%s", trace)
	}
	if !strings.Contains(trace, "bundle.mode=all") {
		t.Fatalf("the server never answered with a bundle list:\n%s", trace)
	}

	requireCommitCount(t, git, clone, "HEAD", packURIHistoryCommits)
	git.run(clone, "fsck", "--no-progress", "--strict")
	refs := git.run(clone, "for-each-ref", "--format=%(refname)", "refs/bundles/")
	if !strings.Contains(refs, "refs/bundles/") || !strings.Contains(refs, "main") {
		t.Fatalf("the clone recorded no bundle ref for main, so it never unbundled:\n%s", refs)
	}
	worktree, err := os.ReadFile(filepath.Join(clone, "f.txt"))
	if err != nil {
		t.Fatalf("read the checkout: %v", err)
	}
	if got, want := string(worktree), packURIWorktree(); got != want {
		t.Fatalf("the bootstrapped checkout is %q, want %q", got, want)
	}
}

// TestBundleURIIsEmptyForARepositoryWithNoRefs pins the one case that has
// nothing to offer: an empty repository has no tips to bundle, and the protocol
// spells "no bundles" as a list with nothing in it.
func TestBundleURIIsEmptyForARepositoryWithNoRefs(t *testing.T) {
	srv := newS3GitServerForTest(t)
	const name = "bundleuri-empty"
	seedGitEmptyRepo(t, srv, name)
	if lines := bundleURIList(t, srv, name); len(lines) != 0 {
		t.Fatalf("an empty repository answered bundle-uri with %v", lines)
	}
}

// TestConcurrentBundleURIRequestsPublishOneBundle exercises the gate that keeps
// a burst of clones arriving between two pushes from each building and
// uploading the same bundle. Every caller must come away with the same bundle,
// and the repository must hold exactly the one.
func TestConcurrentBundleURIRequestsPublishOneBundle(t *testing.T) {
	srv := newS3GitServerForTest(t)
	const name = "bundleuri-concurrent"
	seedPackedS3Repo(t, srv, name)
	stor := gitStorerWithPackReuse(context.Background(), "admin/"+name, srv.store.GetGitStorage("admin", name))

	const callers = 4
	replies := make([]bytes.Buffer, callers)
	failures := make(chan error, callers)
	var release sync.WaitGroup
	release.Add(1)
	for index := range replies {
		go func(index int) {
			release.Wait()
			failures <- serveGitBundleURIV2(context.Background(), stor, nil, &replies[index])
		}(index)
	}
	release.Done()
	for range callers {
		if err := <-failures; err != nil {
			t.Fatalf("concurrent bundle-uri: %v", err)
		}
	}

	first := ""
	for index := range replies {
		id, _ := bundleListURI(t, gitPktLines(t, replies[index].Bytes()))
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("caller %d was offered bundle %s, want the %s every other caller got", index, id, first)
		}
	}
	if stored := storedBundles(t, name); len(stored) != 1 {
		t.Fatalf("the repository holds %d bundles after %d concurrent requests: %v", len(stored), callers, stored)
	}
}
