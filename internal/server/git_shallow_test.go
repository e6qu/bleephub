package bleephub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"
)

// The shallow protocol is only worth testing against the program that speaks it,
// so everything below drives the real git CLI at a running bleephub. The same
// matrix runs over smart HTTP and SSH: the two transports share one upload-pack
// implementation, and the point of these tests is that they cannot drift apart.

// gitShallowHistoryCommits is the number of commits seedGitShallowRepo builds:
// a root plus five dated commits.
const gitShallowHistoryCommits = 6

// gitShallowExcludeBranch points at the third commit, so a clone that excludes
// it keeps exactly the three commits after it.
const gitShallowExcludeBranch = "base"

// gitShallowSince splits the seeded history between its third and fourth
// commit, so a --shallow-since clone of it keeps exactly two.
const gitShallowSince = "2020-03-15"

// seedGitShallowRepo builds a repository with a deterministic linear history: a
// root commit plus five commits dated one month apart through 2020, and a "base"
// branch at the third of them. Dates are fixed rather than relative to now so
// --shallow-since asserts on an exact commit count.
func seedGitShallowRepo(t *testing.T, s *Server, name string) {
	t.Helper()
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("admin user is missing")
	}
	if s.store.CreateRepo(admin, name, "shallow clone fixture", false) == nil {
		t.Fatalf("create repo %s", name)
	}
	stor := s.store.GetGitStorage("admin", name)
	if stor == nil {
		t.Fatalf("repo %s has no git storage", name)
	}
	signatureAt := func(month time.Month) *object.Signature {
		return &object.Signature{
			Name:  "Shallow Fixture",
			Email: "shallow@bleephub.invalid",
			When:  time.Date(2020, month, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	if _, err := initRepoWithFiles(stor, "main", "root", map[string]string{"f.txt": "l0\n"}, signatureAt(time.January)); err != nil {
		t.Fatalf("seed root commit: %v", err)
	}
	content := "l0\n"
	var third plumbing.Hash
	for i := 1; i <= 5; i++ {
		content += "l" + strconv.Itoa(i) + "\n"
		hash, err := createFileCommit(stor, "main", "f.txt", content, "c"+strconv.Itoa(i), signatureAt(time.Month(i)))
		if err != nil {
			t.Fatalf("seed commit c%d: %v", i, err)
		}
		if i == 2 {
			third = hash
		}
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(gitShallowExcludeBranch), third)); err != nil {
		t.Fatalf("seed %s branch: %v", gitShallowExcludeBranch, err)
	}
	if err := store.SetGitHeadBranch(stor, "main"); err != nil {
		t.Fatalf("point git HEAD at main: %v", err)
	}
}

// gitCLI runs the real git binary with whatever environment a transport needs.
type gitCLI struct {
	t   *testing.T
	env []string
}

// requireGitCLI builds a runner, skipping the test when git is not installed —
// the same guard TestGitCloneChecksOutDefaultBranch uses.
func requireGitCLI(t *testing.T, env ...string) gitCLI {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := append(os.Environ(), hermeticGitTestEnv(t.TempDir())...)
	return gitCLI{t: t, env: append(base, env...)}
}

func (g gitCLI) run(dir string, args ...string) string {
	g.t.Helper()
	output, err := g.tryRun(dir, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func (g gitCLI) tryRun(dir string, args ...string) (string, error) {
	g.t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = g.env
	output, err := command.CombinedOutput()
	return string(output), err
}

// commitCount counts the commits reachable from a revision in a clone. In a
// shallow clone the boundary stops the walk, so this is exactly the depth the
// client ended up with.
func (g gitCLI) commitCount(dir, revision string) int {
	g.t.Helper()
	count, err := strconv.Atoi(strings.TrimSpace(g.run(dir, "rev-list", "--count", revision)))
	if err != nil {
		g.t.Fatalf("parse commit count: %v", err)
	}
	return count
}

func requireCommitCount(t *testing.T, git gitCLI, dir, revision string, want int) {
	t.Helper()
	if got := git.commitCount(dir, revision); got != want {
		t.Fatalf("%s in %s has %d commits, want %d", revision, filepath.Base(dir), got, want)
	}
}

func requireShallowFile(t *testing.T, dir string, want bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, ".git", "shallow"))
	if want && err != nil {
		t.Fatalf("%s has no .git/shallow: %v", filepath.Base(dir), err)
	}
	if !want && err == nil {
		t.Fatalf("%s still has a .git/shallow", filepath.Base(dir))
	}
}

// runGitShallowMatrix is the whole shallow contract, exercised against one
// clone URL. Both transports run it so neither can regress alone.
func runGitShallowMatrix(t *testing.T, git gitCLI, cloneURL string) {
	t.Helper()
	root := t.TempDir()

	// A plain clone still returns the complete history. This is the common
	// path the shallow work must not disturb, so it is asserted first.
	full := filepath.Join(root, "full")
	git.run(root, "clone", cloneURL, full)
	requireCommitCount(t, git, full, "HEAD", gitShallowHistoryCommits)
	requireShallowFile(t, full, false)
	git.run(full, "fsck", "--no-progress")

	// --depth 1: exactly the tip, recorded as a boundary, with a correct
	// checkout. The working tree matters: a pack that carried the commit but
	// not its tree would still produce a one-commit log.
	depthOne := filepath.Join(root, "depth-1")
	git.run(root, "clone", "--depth", "1", cloneURL, depthOne)
	requireCommitCount(t, git, depthOne, "HEAD", 1)
	requireShallowFile(t, depthOne, true)
	git.run(depthOne, "fsck", "--no-progress")
	worktree, err := os.ReadFile(filepath.Join(depthOne, "f.txt"))
	if err != nil {
		t.Fatalf("read checked-out file: %v", err)
	}
	if got, want := string(worktree), "l0\nl1\nl2\nl3\nl4\nl5\n"; got != want {
		t.Fatalf("shallow checkout is %q, want %q", got, want)
	}
	// git log has to work inside a shallow clone, which means the boundary
	// commit's parents must be absent rather than dangling.
	if logOutput := git.run(depthOne, "log", "--oneline"); strings.Count(strings.TrimSpace(logOutput), "\n") != 0 {
		t.Fatalf("git log in a --depth 1 clone printed more than one commit:\n%s", logOutput)
	}

	// --depth 3 on a six-commit history keeps three.
	depthThree := filepath.Join(root, "depth-3")
	git.run(root, "clone", "--depth", "3", cloneURL, depthThree)
	requireCommitCount(t, git, depthThree, "HEAD", 3)

	// Deepening an existing shallow clone, then removing the boundary
	// altogether. Both go through the client's own shallow lines, so this is
	// the unshallow path as well as the deepen one.
	git.run(depthThree, "fetch", "--depth", "5")
	requireCommitCount(t, git, depthThree, "origin/main", 5)
	requireShallowFile(t, depthThree, true)
	git.run(depthThree, "fetch", "--unshallow")
	requireCommitCount(t, git, depthThree, "origin/main", gitShallowHistoryCommits)
	requireShallowFile(t, depthThree, false)
	git.run(depthThree, "fsck", "--no-progress")

	// --shallow-since and --shallow-exclude reach the same boundary shape
	// through different predicates.
	since := filepath.Join(root, "since")
	git.run(root, "clone", "--shallow-since="+gitShallowSince, cloneURL, since)
	requireCommitCount(t, git, since, "HEAD", 2)
	requireShallowFile(t, since, true)

	exclude := filepath.Join(root, "exclude")
	git.run(root, "clone", "--shallow-exclude="+gitShallowExcludeBranch, cloneURL, exclude)
	requireCommitCount(t, git, exclude, "HEAD", 3)
	requireShallowFile(t, exclude, true)

	// A boundary predicate that selects nothing is refused with a reason the
	// client can print, not with a pack that silently omits the wants.
	output, err := git.tryRun(root, "clone", "--shallow-exclude=main", cloneURL, filepath.Join(root, "empty-boundary"))
	if err == nil {
		t.Fatalf("--shallow-exclude of the wanted branch succeeded:\n%s", output)
	}
	if !strings.Contains(output, "no commits selected for shallow requests") {
		t.Fatalf("--shallow-exclude of the wanted branch did not explain itself:\n%s", output)
	}

	// A --shallow-exclude naming nothing is a request error, and git's own
	// wording for it reaches the user rather than a bare transport failure.
	output, err = git.tryRun(root, "clone", "--shallow-exclude=no-such-ref", cloneURL, filepath.Join(root, "unknown-exclude"))
	if err == nil {
		t.Fatalf("--shallow-exclude of a missing ref succeeded:\n%s", output)
	}
	if !strings.Contains(output, "deepen-not is not a ref") {
		t.Fatalf("--shallow-exclude of a missing ref did not explain itself:\n%s", output)
	}

	// An ordinary fetch into a shallow clone must leave the boundary where it
	// is rather than quietly filling in the history behind it.
	git.run(depthOne, "fetch")
	requireShallowFile(t, depthOne, true)
	requireCommitCount(t, git, depthOne, "HEAD", 1)

	// A second boundary, so the push below carries more than one shallow line:
	// that is the count go-git's reference-update decoder cannot represent, and
	// the shape that used to fail the push outright.
	git.run(depthOne, "fetch", "--depth", "1", "origin", gitShallowExcludeBranch+":refs/remotes/origin/"+gitShallowExcludeBranch)
	boundaries, err := os.ReadFile(filepath.Join(depthOne, ".git", "shallow"))
	if err != nil {
		t.Fatalf("read shallow boundaries: %v", err)
	}
	if got := len(strings.Fields(string(boundaries))); got != 2 {
		t.Fatalf("shallow clone records %d boundaries, want 2", got)
	}

	// Pushing from a shallow clone: the classic breakage, because the client
	// prefixes its reference-update request with those boundary lines.
	git.run(depthOne, "config", "user.name", "Shallow Pusher")
	git.run(depthOne, "config", "user.email", "shallow@bleephub.invalid")
	if err := os.WriteFile(filepath.Join(depthOne, "f.txt"), []byte("pushed from a shallow clone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git.run(depthOne, "add", "f.txt")
	git.run(depthOne, "commit", "-m", "pushed from a shallow clone")
	git.run(depthOne, "push", "origin", "HEAD:main")

	// The push landed, and a full clone still walks the whole history through
	// the commit a shallow client produced.
	afterPush := filepath.Join(root, "after-push")
	git.run(root, "clone", cloneURL, afterPush)
	requireCommitCount(t, git, afterPush, "HEAD", gitShallowHistoryCommits+1)
	git.run(afterPush, "fsck", "--no-progress")
	if subject := strings.TrimSpace(git.run(afterPush, "log", "-1", "--format=%s")); subject != "pushed from a shallow clone" {
		t.Fatalf("tip after the shallow push is %q", subject)
	}
}

// gitProtocolVersions are the two wire protocols this server speaks. The whole
// shallow matrix runs under each of them, because a client picks the version
// and both have to reach the same boundary.
var gitProtocolVersions = []int{0, 2}

func TestGitShallowCloneOverHTTP(t *testing.T) {
	t.Parallel()
	requireGitCLI(t)
	srv := newIsolatedServer(t)
	for _, version := range gitProtocolVersions {
		t.Run("protocol-v"+strconv.Itoa(version), func(t *testing.T) {
			name := "shallow-http-v" + strconv.Itoa(version)
			seedGitShallowRepo(t, srv.Server, name)
			cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/" + name + ".git"
			runGitShallowMatrix(t, requireGitCLI(t).atProtocol(version), cloneURL)
		})
	}
}

func TestGitShallowCloneOverSSH(t *testing.T) {
	// Not parallel: bringing up an SSH listener goes through the process
	// environment, the way the shutdown test does.
	requireGitCLI(t)
	srv := newIsolatedServer(t)
	keyPath := startIsolatedGitSSH(t, srv)
	for _, version := range gitProtocolVersions {
		t.Run("protocol-v"+strconv.Itoa(version), func(t *testing.T) {
			git := requireGitCLI(t).
				with("GIT_SSH_COMMAND=ssh -i " + keyPath + " -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null").
				atProtocol(version)
			name := "shallow-ssh-v" + strconv.Itoa(version)
			seedGitShallowRepo(t, srv.Server, name)
			runGitShallowMatrix(t, git, "ssh://git@"+os.Getenv("BLEEPHUB_SSH_ADDR")+"/admin/"+name+".git")
		})
	}
}

// startIsolatedGitSSH brings up this server's own SSH Git listener on a free
// port with a freshly generated host key, registers a client key on the admin
// account, and returns the path to that key's private half.
//
// Keys are generated rather than fixtured because a skipped test proves nothing,
// and the listener is this server's rather than the package-wide one so the test
// stays isolated (TEST-008).
func startIsolatedGitSSH(t *testing.T, srv *isolatedServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a host key: %v", err)
	}
	hostDER, err := x509.MarshalPKCS8PrivateKey(hostKey)
	if err != nil {
		t.Fatalf("marshal the host key: %v", err)
	}
	t.Setenv("BLEEPHUB_SSH_ADDR", addr)
	t.Setenv("BLEEPHUB_SSH_HOST_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: hostDER})))

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	if err := srv.startGitSSH(ctx); err != nil {
		t.Fatalf("start the SSH Git transport: %v", err)
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("the SSH listener never came up: %v", err)
	}
	conn.Close()

	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a client key: %v", err)
	}
	public, err := ssh.NewPublicKey(clientKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	response := srv.post(t, "/api/v3/user/keys", defaultToken, map[string]string{
		"title": "shallow clone test",
		"key":   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))),
	})
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("register SSH key = %d", response.StatusCode)
	}
	keyBlock, err := ssh.MarshalPrivateKey(clientKey, "bleephub shallow test")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(keyBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

// TestGitUploadPackRejectsAMalformedRequest keeps the one failure mode that can
// still carry an HTTP status: a request refused before any reply is written is a
// 400, not a 200 with a body no client can parse.
func TestGitUploadPackRejectsAMalformedRequest(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "malformed-upload-pack"
	seedGitShallowRepo(t, srv.Server, name)

	request, err := http.NewRequest(http.MethodPost, srv.baseURL+"/admin/"+name+".git/git-upload-pack", strings.NewReader("not a pkt-line"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "token "+defaultToken)
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed upload-pack request = %d, want 400", response.StatusCode)
	}
}
