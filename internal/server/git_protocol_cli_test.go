package bleephub

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// Drive the real git binary against a running bleephub, checking the protocol
// claims of git_protocol_test.go against the program that must believe them.
// The same matrix runs over smart HTTP and SSH so the two transports, which
// share one upload-pack implementation, cannot drift apart.

// gitProtocolTagName is the annotated tag seedGitProtocolRepo puts on the tip,
// so include-tag has something to volunteer.
const gitProtocolTagName = "v1"

// seedGitProtocolRepo builds the fixture the matrix runs against: the linear
// dated history seedGitShallowRepo creates, plus an annotated tag on its tip.
func seedGitProtocolRepo(t *testing.T, srv *isolatedServer, name string) {
	t.Helper()
	seedGitShallowRepo(t, srv.Server, name)
	seedGitAnnotatedTag(t, srv, name, gitProtocolTagName, gitSeedTip(t, srv, name))
}

// seedGitEmptyRepo creates a repository with no commits at all and reports the
// branch its first commit is meant to land on.
func seedGitEmptyRepo(t *testing.T, srv *isolatedServer, name string) string {
	t.Helper()
	admin := srv.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("admin user is missing")
	}
	if srv.store.CreateRepo(admin, name, "empty fixture", false) == nil {
		t.Fatalf("create repo %s", name)
	}
	repo := srv.store.GetRepo("admin", name)
	if repo == nil || repo.DefaultBranch == "" {
		t.Fatalf("repo %s has no default branch", name)
	}
	return repo.DefaultBranch
}

// with returns a runner carrying extra environment.
func (g gitCLI) with(env ...string) gitCLI {
	next := gitCLI{t: g.t}
	next.env = append(append([]string{}, g.env...), env...)
	return next
}

// atProtocol pins every git invocation, including the ones git spawns for
// itself such as a partial clone's lazy fetch, to one wire protocol version.
func (g gitCLI) atProtocol(version int) gitCLI {
	return g.with(
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=protocol.version",
		"GIT_CONFIG_VALUE_0="+strconv.Itoa(version),
	)
}

// tracing returns a runner whose output also carries every pkt-line git read or
// wrote, which is how a test proves which protocol version was really spoken.
func (g gitCLI) tracing() gitCLI {
	return g.with("GIT_TRACE_PACKET=1")
}

// gitEnumeratedObjects reads the object count out of the progress the server
// sent on band 2, which is exactly how many objects the pack carried.
func gitEnumeratedObjects(t *testing.T, output string) int {
	t.Helper()
	const marker = "Enumerating objects: "
	index := strings.Index(output, marker)
	if index < 0 {
		t.Fatalf("no enumeration progress in:\n%s", output)
	}
	rest := output[index+len(marker):]
	end := strings.IndexAny(rest, ",\r\n")
	if end < 0 {
		t.Fatalf("malformed enumeration progress in:\n%s", output)
	}
	count, err := strconv.Atoi(strings.TrimSpace(rest[:end]))
	if err != nil {
		t.Fatalf("malformed enumeration progress %q", rest[:end])
	}
	return count
}

// runGitProtocolMatrix is the whole upload-pack contract, exercised against one
// clone URL with the real git binary.
func runGitProtocolMatrix(t *testing.T, git gitCLI, cloneURL, emptyCloneURL, emptyBranch string) {
	t.Helper()
	root := t.TempDir()
	v2 := git.atProtocol(2)
	v0 := git.atProtocol(0)

	// Protocol v2 is negotiated, used, and delivers the same history: the
	// server answers info/refs with "version 2", the client then asks its two
	// commands, and the working tree that comes out is the right one.
	v2Clone := filepath.Join(root, "v2")
	trace := v2.tracing().run(root, "clone", cloneURL, v2Clone)
	for _, want := range []string{"version 2", "command=ls-refs", "command=fetch"} {
		if !strings.Contains(trace, want) {
			t.Fatalf("a protocol v2 clone never exchanged %q; git fell back to v0", want)
		}
	}
	requireCommitCount(t, git, v2Clone, "HEAD", gitShallowHistoryCommits)
	git.run(v2Clone, "fsck", "--no-progress")
	worktree, err := os.ReadFile(filepath.Join(v2Clone, "f.txt"))
	if err != nil {
		t.Fatalf("read the v2 checkout: %v", err)
	}
	if got, want := string(worktree), "l0\nl1\nl2\nl3\nl4\nl5\n"; got != want {
		t.Fatalf("v2 checkout is %q, want %q", got, want)
	}
	// ls-refs carried the tags too, and the annotated tag peels to the tip.
	if got := strings.TrimSpace(git.run(v2Clone, "cat-file", "-t", "refs/tags/"+gitProtocolTagName)); got != "tag" {
		t.Fatalf("refs/tags/%s in a v2 clone is a %s, want a tag object", gitProtocolTagName, got)
	}

	// An empty repository can finally state its default branch, because v2's
	// ls-refs has a way to name a branch that has no commits yet.
	emptyClone := filepath.Join(root, "v2-empty")
	trace = v2.tracing().run(root, "clone", emptyCloneURL, emptyClone)
	if !strings.Contains(trace, "unborn HEAD") {
		t.Fatalf("cloning an empty repository over v2 never saw an unborn HEAD:\n%s", trace)
	}
	if got := strings.TrimSpace(git.run(emptyClone, "symbolic-ref", "--short", "HEAD")); got != emptyBranch {
		t.Fatalf("empty v2 clone checked out %q, want %q", got, emptyBranch)
	}

	// side-band: with progress asked for, the counting and totals the server
	// computed arrive interleaved with the pack and are printed as "remote:".
	for _, runner := range []struct {
		name string
		git  gitCLI
	}{{"v0", v0}, {"v2", v2}} {
		loud := filepath.Join(root, "progress-"+runner.name)
		output := runner.git.run(root, "clone", "--progress", cloneURL, loud)
		for _, want := range []string{"remote: Counting objects", "remote: Total"} {
			if !strings.Contains(output, want) {
				t.Fatalf("%s clone --progress never printed %q:\n%s", runner.name, want, output)
			}
		}
		// no-progress: git asks for silence whenever it is not driving a
		// terminal, and the server honours it.
		quiet := filepath.Join(root, "quiet-"+runner.name)
		output = runner.git.run(root, "clone", cloneURL, quiet)
		if strings.Contains(output, "remote: Counting objects") {
			t.Fatalf("%s clone without --progress still received band 2 text:\n%s", runner.name, output)
		}
	}

	// deepen-relative: `git fetch --deepen` extends an existing shallow clone
	// by a number of commits counted from the boundary it already has, rather
	// than from the tip.
	for _, runner := range []struct {
		name string
		git  gitCLI
	}{{"v0", v0}, {"v2", v2}} {
		deepen := filepath.Join(root, "deepen-"+runner.name)
		runner.git.run(root, "clone", "--depth", "1", cloneURL, deepen)
		requireCommitCount(t, git, deepen, "HEAD", 1)
		runner.git.run(deepen, "fetch", "--deepen=2")
		requireCommitCount(t, git, deepen, "HEAD", 3)
		requireShallowFile(t, deepen, true)
		runner.git.run(deepen, "fetch", "--deepen=2")
		requireCommitCount(t, git, deepen, "HEAD", 5)
		git.run(deepen, "fsck", "--no-progress")
	}

	// A shallow clone over v2 goes through the shallow-info section rather than
	// the v0 shallow update, and reaches the same boundary.
	v2Shallow := filepath.Join(root, "v2-shallow")
	v2.run(root, "clone", "--depth", "2", cloneURL, v2Shallow)
	requireCommitCount(t, git, v2Shallow, "HEAD", 2)
	requireShallowFile(t, v2Shallow, true)
	v2.run(v2Shallow, "fetch", "--unshallow")
	requireCommitCount(t, git, v2Shallow, "origin/main", gitShallowHistoryCommits)
	requireShallowFile(t, v2Shallow, false)

	// Partial clone: the blobs really are absent, and the one the working tree
	// later needs is fetched on demand.
	partial := filepath.Join(root, "partial")
	v2.run(root, "clone", "--filter=blob:none", "--no-checkout", cloneURL, partial)
	if got := strings.TrimSpace(git.run(partial, "config", "remote.origin.promisor")); got != "true" {
		t.Fatalf("a --filter clone did not record a promisor remote (%q), so the server never advertised filter", got)
	}
	missing := gitMissingObjects(t, git, partial)
	if len(missing) == 0 {
		t.Fatal("a --filter=blob:none clone is missing no objects, so the filter was ignored")
	}
	content := git.run(partial, "cat-file", "-p", missing[0])
	if !strings.Contains(content, "l0\nl1\n") {
		t.Fatalf("the lazily fetched blob reads %q", content)
	}
	for _, id := range gitMissingObjects(t, git, partial) {
		if id == missing[0] {
			t.Fatalf("object %s is still missing after it was read", id)
		}
	}

	// include-tag: a fetch of one branch also brings the annotated tags that
	// point into it.
	tagged := filepath.Join(root, "include-tag")
	git.run(root, "init", tagged)
	git.run(tagged, "remote", "add", "origin", cloneURL)
	// The refspec names a destination, which is what puts git in tag-following
	// mode and therefore makes it ask for include-tag.
	trace = v2.tracing().run(tagged, "fetch", "origin", "main:refs/remotes/origin/main")
	if !strings.Contains(trace, "include-tag") {
		t.Fatalf("a branch fetch never asked for include-tag:\n%s", trace)
	}
	if got := strings.TrimSpace(git.run(tagged, "cat-file", "-t", "refs/tags/"+gitProtocolTagName)); got != "tag" {
		t.Fatalf("refs/tags/%s after a branch fetch is a %s, want a tag object", gitProtocolTagName, got)
	}

	// An incremental fetch transfers the delta and nothing else. The count is
	// the server's own enumeration, read back off the progress band.
	pusher := filepath.Join(root, "pusher")
	git.run(root, "clone", cloneURL, pusher)
	git.run(pusher, "config", "user.name", "Protocol Fixture")
	git.run(pusher, "config", "user.email", "protocol@bleephub.invalid")
	if err := os.WriteFile(filepath.Join(pusher, "f.txt"), []byte("l0\nl1\nl2\nl3\nl4\nl5\nl6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git.run(pusher, "add", "f.txt")
	git.run(pusher, "commit", "-m", "one more line")
	git.run(pusher, "push", "origin", "HEAD:main")

	body := "l0\nl1\nl2\nl3\nl4\nl5\nl6\n"
	commits := gitShallowHistoryCommits + 1
	for _, runner := range []struct {
		name string
		git  gitCLI
	}{{"v0", v0}, {"v2", v2}} {
		behind := filepath.Join(root, "behind-"+runner.name)
		runner.git.run(root, "clone", cloneURL, behind)
		body += "another line for " + runner.name + "\n"
		if err := os.WriteFile(filepath.Join(pusher, "f.txt"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git.run(pusher, "add", "f.txt")
		git.run(pusher, "commit", "-m", "another line for "+runner.name)
		git.run(pusher, "push", "origin", "HEAD:main")
		commits++

		output := runner.git.run(behind, "fetch", "--progress")
		if count := gitEnumeratedObjects(t, output); count > 4 {
			t.Fatalf("%s incremental fetch enumerated %d objects, want only the new commit, tree and blob:\n%s",
				runner.name, count, output)
		}
		requireCommitCount(t, git, behind, "origin/main", commits)
		git.run(behind, "fsck", "--no-progress")
	}
}

// gitMissingObjects lists the object ids a partial clone knows about but does
// not hold.
func gitMissingObjects(t *testing.T, git gitCLI, dir string) []string {
	t.Helper()
	var missing []string
	for _, line := range strings.Split(git.run(dir, "rev-list", "--objects", "--all", "--missing=print"), "\n") {
		if id, found := strings.CutPrefix(strings.TrimSpace(line), "?"); found {
			missing = append(missing, id)
		}
	}
	return missing
}

func TestGitProtocolOverHTTP(t *testing.T) {
	t.Parallel()
	git := requireGitCLI(t)
	srv := newIsolatedServer(t)
	const name = "protocol-http"
	const emptyName = "protocol-http-empty"
	seedGitProtocolRepo(t, srv, name)
	emptyBranch := seedGitEmptyRepo(t, srv, emptyName)
	base := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1)
	runGitProtocolMatrix(t, git, base+"/admin/"+name+".git", base+"/admin/"+emptyName+".git", emptyBranch)
}

func TestGitProtocolOverSSH(t *testing.T) {
	// Not parallel: bringing up an SSH listener goes through the process
	// environment, the way the shutdown test does.
	git := requireGitCLI(t)
	srv := newIsolatedServer(t)
	keyPath := startIsolatedGitSSH(t, srv)
	git = git.with("GIT_SSH_COMMAND=ssh -i " + keyPath + " -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null")
	const name = "protocol-ssh"
	const emptyName = "protocol-ssh-empty"
	seedGitProtocolRepo(t, srv, name)
	emptyBranch := seedGitEmptyRepo(t, srv, emptyName)
	base := "ssh://git@" + os.Getenv("BLEEPHUB_SSH_ADDR")
	runGitProtocolMatrix(t, git, base+"/admin/"+name+".git", base+"/admin/"+emptyName+".git", emptyBranch)
}

// TestGitUploadPackAdvertisesExactlyWhatItImplements pins the advertisement
// itself, without a git binary: every capability listed here is a promise the
// negotiation keeps, and the list is exhaustive so a capability cannot be
// advertised without a test being updated to say what honours it.
func TestGitUploadPackAdvertisesExactlyWhatItImplements(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "advertised-caps-exact"
	seedGitShallowRepo(t, srv.Server, name)

	advertisement, err := gitUploadPackAdvertisement(srv.store.GetGitStorage("admin", name))
	if err != nil {
		t.Fatalf("build advertisement: %v", err)
	}
	want := []string{
		"multi_ack",
		"multi_ack_detailed",
		"no-done",
		"thin-pack",
		"side-band",
		"side-band-64k",
		"ofs-delta",
		"shallow",
		"deepen-since",
		"deepen-not",
		"deepen-relative",
		"no-progress",
		"include-tag",
		"allow-tip-sha1-in-want",
		"allow-reachable-sha1-in-want",
		"filter",
		"object-format=sha1",
		// The symbolic HEAD reference carries its own capability, which is how
		// a client learns which branch to check out.
		"symref=HEAD:refs/heads/main",
	}
	var got []string
	for _, field := range strings.Fields(advertisement.Capabilities.String()) {
		if strings.HasPrefix(field, "agent=") {
			continue
		}
		got = append(got, field)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("advertised capabilities:\n got %v\nwant %v", got, want)
	}
	if agent := advertisement.Capabilities.Get("agent"); len(agent) != 1 {
		t.Errorf("advertisement carries %v agents, want exactly one", agent)
	}
	if _, ok := advertisement.References[plumbing.NewBranchReferenceName("main").String()]; !ok {
		t.Error("advertisement omits refs/heads/main")
	}
	if advertisement.Head == nil {
		t.Error("advertisement omits HEAD")
	}
}
