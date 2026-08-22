package bleephub

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
)

// gitAdvertisement fetches a smart-HTTP ref advertisement and decodes it into
// the object id advertised per ref name (including the pseudo-ref HEAD) and the
// capability list carried on the first pkt-line.
func gitAdvertisement(t *testing.T, srv *isolatedServer, repoName, service string) (map[string]string, []string) {
	t.Helper()
	resp := srv.get(t, "/admin/"+repoName+".git/info/refs?service="+service, defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info/refs?service=%s = %d, want 200", service, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read advertisement: %v", err)
	}

	refs := map[string]string{}
	var capabilities []string
	scanner := pktline.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSuffix(string(scanner.Bytes()), "\n")
		if line == "" || strings.HasPrefix(line, "# service=") {
			continue
		}
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			capabilities = strings.Fields(line[nul+1:])
			line = line[:nul]
		}
		if id, name, found := strings.Cut(line, " "); found {
			refs[name] = id
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("decode advertisement: %v", err)
	}
	return refs, capabilities
}

func hasCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

// requireAdvertisedDefaultBranch is the whole contract this file guards: the
// advertisement must name the default branch in a symref capability, and the
// object id it advertises for HEAD must be that branch's tip. Together they are
// the only two ways a client can be told which branch to check out.
func requireAdvertisedDefaultBranch(t *testing.T, srv *isolatedServer, repoName, service, branch string) {
	t.Helper()
	refs, capabilities := gitAdvertisement(t, srv, repoName, service)
	want := "symref=HEAD:refs/heads/" + branch
	if !hasCapability(capabilities, want) {
		t.Errorf("%s advertisement capabilities = %v, want one to be %q", service, capabilities, want)
	}
	tip, ok := refs["refs/heads/"+branch]
	if !ok {
		t.Fatalf("%s advertisement has no refs/heads/%s: %v", service, branch, refs)
	}
	if got := refs["HEAD"]; got != tip {
		t.Errorf("%s advertised HEAD = %s, want the %s tip %s", service, got, branch, tip)
	}
}

// requireStoredGitHead asserts the persisted HEAD is a symbolic reference to
// the branch, not a detached hash and not a stale branch.
func requireStoredGitHead(t *testing.T, srv *isolatedServer, repoName, branch string) {
	t.Helper()
	head, err := srv.store.GetGitStorage("admin", repoName).Reference(plumbing.HEAD)
	if err != nil {
		t.Fatalf("read stored HEAD: %v", err)
	}
	if head.Type() != plumbing.SymbolicReference {
		t.Fatalf("stored HEAD is %v (%s), want a symbolic reference to refs/heads/%s", head.Type(), head.Hash(), branch)
	}
	if got, want := head.Target(), plumbing.NewBranchReferenceName(branch); got != want {
		t.Errorf("stored HEAD targets %s, want %s", got, want)
	}
}

func createSeededRepo(t *testing.T, srv *isolatedServer, name string, branches ...string) map[string]string {
	t.Helper()
	resp := srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name, "auto_init": true})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo %s = %d, want 201", name, resp.StatusCode)
	}
	repo := srv.store.GetRepo("admin", name)
	if repo == nil {
		t.Fatalf("repo %s was not created", name)
	}
	return seedPullRequestBranches(t, srv.Server, repo, branches...)
}

// TestGitAdvertisesDefaultBranchSymref pins the fix for clones landing on the
// wrong branch: without symref=HEAD:refs/heads/<default> a client has to guess
// the checkout branch by matching HEAD's object id against the ref list.
func TestGitAdvertisesDefaultBranchSymref(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	createSeededRepo(t, srv, "symref-fresh", "fix/health-race")

	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		requireAdvertisedDefaultBranch(t, srv, "symref-fresh", service, "main")
	}
	requireStoredGitHead(t, srv, "symref-fresh", "main")
}

// TestGitAdvertisedSymrefFollowsDefaultBranchChange pins that PATCHing
// default_branch moves both the advertised symref and the stored HEAD.
func TestGitAdvertisedSymrefFollowsDefaultBranchChange(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	createSeededRepo(t, srv, "symref-moves", "release")
	requireStoredGitHead(t, srv, "symref-moves", "main")

	resp := srv.patch(t, "/api/v3/repos/admin/symref-moves", defaultToken, map[string]interface{}{"default_branch": "release"})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch default_branch = %d, want 200", resp.StatusCode)
	}

	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		requireAdvertisedDefaultBranch(t, srv, "symref-moves", service, "release")
	}
	requireStoredGitHead(t, srv, "symref-moves", "release")
}

// TestGitAdvertisedSymrefWithSharedBranchTips is the exact ambiguity that
// produced the bug: when several branches point at the same commit, matching
// HEAD's object id against the ref list cannot identify the default branch, so
// the symref capability is the only thing standing between a client and an
// arbitrary checkout.
func TestGitAdvertisedSymrefWithSharedBranchTips(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	createSeededRepo(t, srv, "symref-shared")

	stor := srv.store.GetGitStorage("admin", "symref-shared")
	mainRef, err := stor.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	// Sorts ahead of "main", so an id-matching client would reach it first.
	twin := plumbing.NewBranchReferenceName("aardvark")
	if err := stor.SetReference(plumbing.NewHashReference(twin, mainRef.Hash())); err != nil {
		t.Fatalf("create twin branch: %v", err)
	}

	refs, capabilities := gitAdvertisement(t, srv, "symref-shared", "git-upload-pack")
	if refs["refs/heads/aardvark"] != refs["refs/heads/main"] {
		t.Fatalf("test setup: branches do not share a tip: %v", refs)
	}
	if want := "symref=HEAD:refs/heads/main"; !hasCapability(capabilities, want) {
		t.Errorf("capabilities = %v, want one to be %q", capabilities, want)
	}
	if got := refs["HEAD"]; got != refs["refs/heads/main"] {
		t.Errorf("advertised HEAD = %s, want the main tip %s", got, refs["refs/heads/main"])
	}
}

// TestGitAdvertisesDefaultBranchDespiteDriftedStoredHead covers the half of the
// fix the stored-HEAD sync cannot: a repository whose HEAD is already wrong —
// detached at a commit, as go-git's worktree machinery used to leave it, or
// restored from storage that predates the sync — must still advertise the
// default branch. Nothing derived from that HEAD may reach the client.
func TestGitAdvertisesDefaultBranchDespiteDriftedStoredHead(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	tips := createSeededRepo(t, srv, "symref-drifted", "fix/health-race")

	stor := srv.store.GetGitStorage("admin", "symref-drifted")
	drifted := plumbing.NewHash(tips["fix/health-race"])
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.HEAD, drifted)); err != nil {
		t.Fatalf("detach HEAD: %v", err)
	}

	refs, capabilities := gitAdvertisement(t, srv, "symref-drifted", "git-upload-pack")
	if want := "symref=HEAD:refs/heads/main"; !hasCapability(capabilities, want) {
		t.Errorf("capabilities = %v, want one to be %q", capabilities, want)
	}
	if got, want := refs["HEAD"], refs["refs/heads/main"]; got != want {
		t.Errorf("advertised HEAD = %s, want the main tip %s (drifted HEAD was %s)", got, want, drifted)
	}
}

// TestGitCloneChecksOutDefaultBranch drives a real git client end to end, the
// way git_ssh_test.go does, so the protocol assertions above are backed by the
// behavior a user actually sees: the checked-out branch is the default branch.
func TestGitCloneChecksOutDefaultBranch(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	srv := newIsolatedServer(t)
	createSeededRepo(t, srv, "clone-default", "fix/health-race", "release")

	temp := t.TempDir()
	cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/clone-default.git"
	checkedOutBranch := func(dir string) string {
		t.Helper()
		head, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
		if err != nil {
			t.Fatalf("read cloned HEAD: %v", err)
		}
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(head)), "ref: "))
	}
	runClone := func(dir string) {
		t.Helper()
		command := exec.Command("git", "clone", cloneURL, dir)
		command.Dir = temp
		command.Env = append(os.Environ(), hermeticGitTestEnv(temp)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, output)
		}
	}

	first := filepath.Join(temp, "on-main")
	runClone(first)
	if got := checkedOutBranch(first); got != "refs/heads/main" {
		t.Errorf("clone checked out %s, want refs/heads/main", got)
	}

	resp := srv.patch(t, "/api/v3/repos/admin/clone-default", defaultToken, map[string]interface{}{"default_branch": "release"})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch default_branch = %d, want 200", resp.StatusCode)
	}

	second := filepath.Join(temp, "on-release")
	runClone(second)
	if got := checkedOutBranch(second); got != "refs/heads/release" {
		t.Errorf("clone after the default-branch change checked out %s, want refs/heads/release", got)
	}
}

// TestGitAdvertisesDefaultBranchForEmptyRepository pins that a repository with
// no refs still names its default branch. Protocol v0 cannot express an unborn
// HEAD to the client — that needs v2's `ls-refs unborn`, which the go-git
// server transport does not implement — but the advertisement is what a v0
// server can say, and the capabilities^{} sentinel line is where it says it.
func TestGitAdvertisesDefaultBranchForEmptyRepository(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	resp := srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "symref-empty"})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create empty repo = %d, want 201", resp.StatusCode)
	}

	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		refs, capabilities := gitAdvertisement(t, srv, "symref-empty", service)
		// A ref-less advertisement carries its capabilities on the zero-id
		// "capabilities^{}" sentinel line, which the scanner reads as a ref.
		if id, ok := refs["capabilities^{}"]; !ok || id != plumbing.ZeroHash.String() {
			t.Fatalf("%s: empty repository advertised %v, want only the capabilities sentinel", service, refs)
		}
		if len(refs) != 1 {
			t.Fatalf("%s: empty repository advertised refs %v", service, refs)
		}
		if !hasCapability(capabilities, "symref=HEAD:refs/heads/main") {
			t.Fatalf("%s: empty repository did not name its default branch: %v", service, capabilities)
		}
	}
	requireStoredGitHead(t, srv, "symref-empty", "main")
}
