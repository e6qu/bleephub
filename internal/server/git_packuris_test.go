package bleephub

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/e6qu/bleephub/internal/store"
)

// Everything here runs against the object store, because a packfile URI only
// exists there: the URL is a presigned GET against a real bucket, and a
// deployment without one has no address to hand out. The MinIO server the S3
// tests already share is that bucket.

// newS3GitServerForTest brings up a server whose repositories live in object
// storage, which is the deployment shape packfile-uris and bundle-uri are for.
//
// It is not parallel, and cannot be: the storage backend is chosen from the
// process environment, so a test that changes it changes it for the binary. Go
// runs the sequential tests of a package before it resumes the parallel ones,
// which is what keeps that from reaching another test.
func newS3GitServerForTest(t *testing.T) *isolatedServer {
	t.Helper()
	fs := newS3FSForTest(t)
	t.Setenv("BLEEPHUB_GIT_DIR", "")
	t.Setenv("BLEEPHUB_S3_ENDPOINT", s3ServerEndpoint)
	t.Setenv("BLEEPHUB_S3_BUCKET", fs.Bucket())
	t.Setenv("BLEEPHUB_S3_PREFIX", "git")
	resetS3FSCacheForTest(t)
	return newIsolatedServer(t)
}

// packURIHistoryCommits is how deep the fixture history is.
//
// Compaction publishes a pack once a repository's loose tier holds enough
// objects to be worth one, and each of these commits writes three: a blob, the
// tree above it and the commit itself. A shallower fixture would be served
// entirely out of the loose tier, where there is no pack to hand an address to.
const packURIHistoryCommits = 30

// packURIWorktree is the file the fixture's tip checks out to.
func packURIWorktree() string {
	var content strings.Builder
	for line := 0; line < packURIHistoryCommits; line++ {
		content.WriteString("l" + strconv.Itoa(line) + "\n")
	}
	return content.String()
}

// seedS3Repo builds the fixture history in object storage: one file rewritten
// once per commit, at fixed instants so the object ids — and therefore the pack
// — are the same on every run.
func seedS3Repo(t *testing.T, srv *isolatedServer, name string, private bool) {
	t.Helper()
	admin := srv.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("admin user is missing")
	}
	if srv.store.CreateRepo(admin, name, "packfile uri fixture", private) == nil {
		t.Fatalf("create repo %s", name)
	}
	stor := srv.store.GetGitStorage("admin", name)
	if stor == nil {
		t.Fatalf("repo %s has no git storage", name)
	}
	if _, err := initRepoWithFiles(stor, "main", "root", map[string]string{"f.txt": "l0\n"}, packReuseSignature(0)); err != nil {
		t.Fatalf("seed the root commit: %v", err)
	}
	content := "l0\n"
	for commit := 1; commit < packURIHistoryCommits; commit++ {
		content += "l" + strconv.Itoa(commit) + "\n"
		if _, err := createFileCommit(stor, "main", "f.txt", content, "c"+strconv.Itoa(commit), packReuseSignature(commit)); err != nil {
			t.Fatalf("seed commit c%d: %v", commit, err)
		}
	}
	if err := store.SetGitHeadBranch(stor, "main"); err != nil {
		t.Fatalf("point git HEAD at main: %v", err)
	}
}

// seedPackedS3Repo seeds a repository in object storage and compacts it, so its
// objects are in one published packfile — the state that makes a URI an answer.
// It returns the name of the pack that was published.
func seedPackedS3Repo(t *testing.T, srv *isolatedServer, name string) string {
	t.Helper()
	seedS3Repo(t, srv, name, false)
	return compactS3Repo(t, srv, name)
}

// compactS3Repo packs a repository's loose objects and returns the name of the
// pack that was published.
func compactS3Repo(t *testing.T, srv *isolatedServer, name string) string {
	t.Helper()
	stor := srv.store.GetGitStorage("admin", name)
	if stor == nil {
		t.Fatalf("repo %s has no git storage", name)
	}
	result, err := gitstore.CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact %s: %v", name, err)
	}
	if result.PackName == "" {
		t.Fatalf("compacting %s published no pack", name)
	}
	return result.PackName
}

// gitPackEntryCount reads the number of entries a packfile states in its
// header.
func gitPackEntryCount(t *testing.T, pack []byte) int {
	t.Helper()
	if len(pack) < gitPackHeaderSize || string(pack[:4]) != "PACK" {
		t.Fatalf("reply is not a packfile: %q", pack[:min(len(pack), 16)])
	}
	return int(binary.BigEndian.Uint32(pack[8:12]))
}

// fetchV2WithPackURIs runs one protocol v2 fetch of a branch tip, stating the
// transfer protocols the client is willing to fetch a packfile over, and
// returns the reply's sections and its packfile.
func fetchV2WithPackURIs(t *testing.T, srv *isolatedServer, name, protocols string, extra ...string) ([]string, []byte) {
	t.Helper()
	tip := gitSeedTip(t, srv, name)
	script := (&gitPktScript{}).
		linef("command=fetch\n").
		linef("object-format=sha1\n").
		delim().
		linef("want %s\n", tip.String())
	for _, argument := range extra {
		script = script.linef("%s\n", argument)
	}
	if protocols != "" {
		script = script.linef("packfile-uris %s\n", protocols)
	}
	script = script.linef("done\n").flush()
	body := postGitUploadPack(t, srv, name, script, map[string]string{gitProtocolHeader: "version=2"})
	sections, stream := splitGitV2Fetch(t, body)
	pack, _ := demuxGitSideband(t, stream)
	return sections, pack
}

// gitPackURILines returns the "<oid> <uri>" lines of a packfile-uris section,
// and reports whether the section was there at all.
func gitPackURILines(sections []string) ([]string, bool) {
	for index, line := range sections {
		if line != "packfile-uris" {
			continue
		}
		var uris []string
		for _, following := range sections[index+1:] {
			if following == "packfile" {
				break
			}
			uris = append(uris, following)
		}
		return uris, true
	}
	return nil, false
}

// TestPackfileURIsCarryTheStoredPackOfAFullClone is the central claim: a full
// clone of a compacted repository is answered with the address of the stored
// pack and an empty packfile, and the address serves exactly those bytes to a
// client holding none of this server's credentials.
func TestPackfileURIsCarryTheStoredPackOfAFullClone(t *testing.T) {
	srv := newS3GitServerForTest(t)
	const name = "packuris-full"
	packName := seedPackedS3Repo(t, srv, name)

	sections, pack := fetchV2WithPackURIs(t, srv, name, "http,https")
	uris, offered := gitPackURILines(sections)
	if !offered {
		t.Fatalf("a full clone of a packed repository was answered without a packfile-uris section: %v", sections)
	}
	if len(uris) != 1 {
		t.Fatalf("packfile-uris named %d packs, want the one that was published: %v", len(uris), uris)
	}
	id, uri, split := strings.Cut(uris[0], " ")
	if !split {
		t.Fatalf("packfile-uris line %q is not '<oid> <uri>'", uris[0])
	}
	if want := strings.TrimPrefix(packName, "pack-"); id != want {
		t.Fatalf("packfile-uris names pack %s, want the published %s", id, want)
	}
	// The remainder is empty: every object the plan owes is in the pack the
	// client was told to fetch, so nothing is sent twice.
	if got := gitPackEntryCount(t, pack); got != 0 {
		t.Fatalf("the packfile section carried %d entries alongside the URI, want none", got)
	}

	// The URL is a credential of its own: it carries no bleephub token, and the
	// object store honours it without asking this server anything.
	response, err := http.Get(uri) // #nosec G107 -- the URL under test
	if err != nil {
		t.Fatalf("fetch the offered URI: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the offered URI answered %d to a caller with no bleephub credential", response.StatusCode)
	}
	fetched, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(fetched, []byte("PACK")) {
		t.Fatalf("the offered URI served %d bytes that are not a packfile", len(fetched))
	}

	// The offer is short-lived, because for its lifetime it bypasses every
	// authorization decision this server makes.
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse the offered URI: %v", err)
	}
	if got, want := parsed.Query().Get("X-Amz-Expires"), strconv.Itoa(int(gitPackURIExpiry.Seconds())); got != want {
		t.Fatalf("the offered URI expires in %qs, want %ss", got, want)
	}
}

// TestPackfileURIsAreOnlyOfferedInAProtocolTheClientNamed pins the negotiation:
// a client that cannot fetch the protocol the object store speaks is answered
// with the objects themselves, not with an address it would have to refuse.
func TestPackfileURIsAreOnlyOfferedInAProtocolTheClientNamed(t *testing.T) {
	srv := newS3GitServerForTest(t)
	const name = "packuris-protocol"
	seedPackedS3Repo(t, srv, name)

	sections, pack := fetchV2WithPackURIs(t, srv, name, "ftp")
	if _, offered := gitPackURILines(sections); offered {
		t.Fatalf("a client that named only ftp was offered a URI: %v", sections)
	}
	if got := gitPackEntryCount(t, pack); got == 0 {
		t.Fatal("a client that was offered nothing received an empty packfile")
	}
}

// TestFetchWithoutPackfileURIsIsAnsweredWithTheObjects is the ignore-the-offer
// path: a client that never asked is never told, and the pack it receives is
// the whole answer.
func TestFetchWithoutPackfileURIsIsAnsweredWithTheObjects(t *testing.T) {
	srv := newS3GitServerForTest(t)
	const name = "packuris-unasked"
	seedPackedS3Repo(t, srv, name)

	sections, pack := fetchV2WithPackURIs(t, srv, name, "")
	if _, offered := gitPackURILines(sections); offered {
		t.Fatalf("a client that never asked was sent a packfile-uris section: %v", sections)
	}
	if got := gitPackEntryCount(t, pack); got == 0 {
		t.Fatal("a fetch that was offered no URI received an empty packfile")
	}
}

// TestPackfileURIsAreNotOfferedForAFilteredClone is the no-leak condition the
// pack-reuse path documents, restated for URIs: a partial clone's plan is a
// strict subset of the repository, and a whole-repository pack is not a correct
// answer to it — so no address for one is handed out.
func TestPackfileURIsAreNotOfferedForAFilteredClone(t *testing.T) {
	srv := newS3GitServerForTest(t)
	const name = "packuris-filtered"
	seedPackedS3Repo(t, srv, name)

	sections, _ := fetchV2WithPackURIs(t, srv, name, "http,https", "filter blob:none")
	if uris, offered := gitPackURILines(sections); offered {
		t.Fatalf("a blob:none clone was offered whole-repository packs: %v", uris)
	}
	sections, _ = fetchV2WithPackURIs(t, srv, name, "http,https", "deepen 1")
	if uris, offered := gitPackURILines(sections); offered {
		t.Fatalf("a --depth 1 clone was offered whole-repository packs: %v", uris)
	}
}

// TestPackfileURIsAreNotOfferedWithoutObjectStorage pins the other half of the
// advertisement: a deployment whose repositories are in a local directory has
// no address for its packs, so it neither advertises the capability nor answers
// a client that asks for it anyway with anything but the objects.
func TestPackfileURIsAreNotOfferedWithoutObjectStorage(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const name = "packuris-nostore"
	seedGitShallowRepo(t, srv.Server, name)

	for _, line := range gitV2Advertisement(t, srv, name) {
		if strings.Contains(line, gitPackURIFetchArgument) {
			t.Fatalf("a deployment with no object storage advertised %q: %q", gitPackURIFetchArgument, line)
		}
		if strings.TrimSpace(line) == gitBundleURICommand {
			t.Fatalf("a deployment with no object storage advertised %q", gitBundleURICommand)
		}
	}

	sections, pack := fetchV2WithPackURIs(t, srv, name, "http,https")
	if _, offered := gitPackURILines(sections); offered {
		t.Fatalf("a deployment with no object storage offered a URI: %v", sections)
	}
	if got := gitPackEntryCount(t, pack); got == 0 {
		t.Fatal("a fetch that was offered no URI received an empty packfile")
	}

	// A client that runs the command anyway — one that saw the capability on
	// another deployment, or asks for it unconditionally — is answered with the
	// empty list the protocol defines, not with an error.
	if lines := bundleURIList(t, srv, name); len(lines) != 0 {
		t.Fatalf("a deployment with no object storage answered bundle-uri with %v", lines)
	}
}

// TestObjectStorageDeploymentAdvertisesTheOffloadCommands is the advertisement
// the client reads before it decides whether to ask.
func TestObjectStorageDeploymentAdvertisesTheOffloadCommands(t *testing.T) {
	srv := newS3GitServerForTest(t)
	const name = "packuris-advertised"
	seedS3Repo(t, srv, name, false)

	advertisement := gitV2Advertisement(t, srv, name)
	fetchLine := ""
	for _, line := range advertisement {
		if strings.HasPrefix(strings.TrimSpace(line), "fetch=") {
			fetchLine = strings.TrimSpace(line)
		}
	}
	if !strings.Contains(fetchLine, gitPackURIFetchArgument) {
		t.Fatalf("fetch capability %q omits %q", fetchLine, gitPackURIFetchArgument)
	}
	if !containsGitLine(advertisement, gitBundleURICommand) {
		t.Fatalf("v2 advertisement %v omits %q", advertisement, gitBundleURICommand)
	}
}

// TestPackfileURIsNeedARepositoryTheCallerMayRead pins that the offer sits
// behind the same gate every other byte of a fetch does. A presigned URL is a
// credential, so the request that would have produced one has to be refused
// before any of it is built.
func TestPackfileURIsNeedARepositoryTheCallerMayRead(t *testing.T) {
	srv := newS3GitServerForTest(t)
	const name = "packuris-private"
	seedS3Repo(t, srv, name, true)
	compactS3Repo(t, srv, name)

	script := (&gitPktScript{}).
		linef("command=fetch\n").
		delim().
		linef("want %s\n", gitSeedTip(t, srv, name).String()).
		linef("packfile-uris http,https\n").
		linef("done\n").
		flush()
	request, err := http.NewRequest(http.MethodPost,
		srv.baseURL+"/admin/"+name+".git/git-upload-pack", bytes.NewReader(script.out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(gitProtocolHeader, "version=2")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an anonymous fetch of a private repository = %d, want 401", response.StatusCode)
	}
	if strings.Contains(string(body), "X-Amz-Signature") {
		t.Fatal("a refused fetch was answered with a presigned URL")
	}
}

// TestGitClonesFromAPackfileURI is the whole claim checked against the program
// that has to believe it: the real git client, told it may fetch packfiles
// itself, ends up with a complete fsck-clean repository whose objects arrived
// from the object store rather than through this server.
func TestGitClonesFromAPackfileURI(t *testing.T) {
	requireGitCLI(t)
	srv := newS3GitServerForTest(t)
	const name = "packuris-clone"
	packName := seedPackedS3Repo(t, srv, name)
	cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/" + name + ".git"

	root := t.TempDir()
	git := requireGitCLI(t).withGitConfig(
		[2]string{"protocol.version", "2"},
		[2]string{"fetch.uriprotocols", "http,https"},
	)
	clone := filepath.Join(root, "offloaded")
	trace := git.with("GIT_TRACE_PACKET=1").run(root, "clone", cloneURL, clone)
	if !gitTraceAskedForPackURIs(trace) {
		t.Fatalf("the client never asked for packfile URIs:\n%s", trace)
	}
	if !gitTraceCarriesPackURIsSection(trace) {
		t.Fatalf("the server never answered with a packfile-uris section:\n%s", trace)
	}
	if !strings.Contains(trace, strings.TrimPrefix(packName, "pack-")) {
		t.Fatalf("the packfile-uris section never named the stored pack %s:\n%s", packName, trace)
	}

	// The clone is complete and consistent, and it holds the very pack the
	// server published — which it can only have got by fetching the URI, since
	// the packfile section of the reply was empty.
	requireCommitCount(t, git, clone, "HEAD", packURIHistoryCommits)
	git.run(clone, "fsck", "--no-progress", "--strict")
	worktree, err := os.ReadFile(filepath.Join(clone, "f.txt"))
	if err != nil {
		t.Fatalf("read the checkout: %v", err)
	}
	if got, want := string(worktree), packURIWorktree(); got != want {
		t.Fatalf("the offloaded checkout is %q, want %q", got, want)
	}
	entries, err := os.ReadDir(filepath.Join(clone, ".git", "objects", "pack"))
	if err != nil {
		t.Fatalf("read the clone's pack directory: %v", err)
	}
	downloaded := false
	for _, entry := range entries {
		if entry.Name() == packName+".pack" {
			downloaded = true
		}
	}
	if !downloaded {
		t.Fatalf("the clone does not hold %s.pack; it has %v", packName, entries)
	}

	// A client that does not take the offer gets the same repository the long
	// way round, which is what keeps the side channel from being load-bearing.
	plain := filepath.Join(root, "inline")
	plainGit := requireGitCLI(t).withGitConfig([2]string{"protocol.version", "2"})
	plainTrace := plainGit.with("GIT_TRACE_PACKET=1").run(root, "clone", cloneURL, plain)
	if gitTraceAskedForPackURIs(plainTrace) || gitTraceCarriesPackURIsSection(plainTrace) {
		t.Fatalf("a client that never asked was sent a packfile-uris section:\n%s", plainTrace)
	}
	requireCommitCount(t, plainGit, plain, "HEAD", packURIHistoryCommits)
	plainGit.run(plain, "fsck", "--no-progress", "--strict")

	// And a fetch into the offloaded clone still works afterwards: the client
	// negotiated from tips it learned out of a pack it downloaded itself.
	git.run(clone, "fetch", "origin")
	requireCommitCount(t, git, clone, "origin/main", packURIHistoryCommits)

	// A commit that landed after the compaction is not in any stored pack, so
	// the answer is both halves at once: the address of the pack, and a
	// packfile carrying what the pack does not. The clone that results has to
	// be as complete as the one that took its objects entirely by URI.
	if _, err := createFileCommit(srv.store.GetGitStorage("admin", name), "main", "f.txt",
		packURIWorktree()+"loose\n", "after the compaction", packReuseSignature(packURIHistoryCommits)); err != nil {
		t.Fatalf("commit past the compaction: %v", err)
	}
	mixed := filepath.Join(root, "mixed")
	mixedTrace := git.with("GIT_TRACE_PACKET=1").run(root, "clone", cloneURL, mixed)
	if !gitTraceCarriesPackURIsSection(mixedTrace) {
		t.Fatalf("a clone owing objects outside the stored pack was offered no URI:\n%s", mixedTrace)
	}
	requireCommitCount(t, git, mixed, "HEAD", packURIHistoryCommits+1)
	git.run(mixed, "fsck", "--no-progress", "--strict")
	if got, want := readFixtureFile(t, mixed, "f.txt"), packURIWorktree()+"loose\n"; got != want {
		t.Fatalf("the mixed checkout is %q, want %q", got, want)
	}
}

// gitTraceAskedForPackURIs reports whether a GIT_TRACE_PACKET transcript shows
// the client sending the packfile-uris argument. The direction marker is what
// separates it from the capability advertisement, which names the argument in
// every conversation whether or not the client goes on to use it.
func gitTraceAskedForPackURIs(trace string) bool {
	return strings.Contains(trace, "> "+gitPackURIFetchArgument+" ")
}

// gitTraceCarriesPackURIsSection reports whether the transcript shows the
// server answering with a packfile-uris section. Under sideband-all the section
// header travels on band 1, which git's trace renders as an escaped \1 ahead of
// it.
func gitTraceCarriesPackURIsSection(trace string) bool {
	return strings.Contains(trace, "< \\1"+gitPackURIFetchArgument) ||
		strings.Contains(trace, "< "+gitPackURIFetchArgument+"\n")
}

// withGitConfig returns a runner carrying git configuration for every
// invocation, including the processes git spawns for itself — which is where a
// packfile URI is actually downloaded, since git runs http-fetch as a child.
func (g gitCLI) withGitConfig(settings ...[2]string) gitCLI {
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(settings))}
	for index, setting := range settings {
		position := strconv.Itoa(index)
		env = append(env,
			"GIT_CONFIG_KEY_"+position+"="+setting[0],
			"GIT_CONFIG_VALUE_"+position+"="+setting[1])
	}
	return g.with(env...)
}

// TestPackURIProtocolListParsing pins the wire spelling of the argument: a
// comma-separated list, insensitive to case and to the spaces a client may put
// around its entries.
func TestPackURIProtocolListParsing(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		value string
		want  []string
	}{
		{value: " https,http", want: []string{"https", "http"}},
		{value: " HTTPS , http ", want: []string{"https", "http"}},
		{value: "", want: nil},
		{value: " ", want: nil},
		{value: ",,", want: nil},
	} {
		got := gitParsePackURIProtocols(testCase.value)
		if len(got) != len(testCase.want) {
			t.Fatalf("packfile-uris %q parsed to %v, want %v", testCase.value, got, testCase.want)
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Fatalf("packfile-uris %q parsed to %v, want %v", testCase.value, got, testCase.want)
			}
		}
	}
}

// TestPackNameIsAnObjectIDOrNoURIIsOffered pins that the object id a client is
// told to expect is read out of the pack's own name, and that a name that is
// not one is never turned into a promise.
func TestPackNameIsAnObjectIDOrNoURIIsOffered(t *testing.T) {
	t.Parallel()
	valid := "pack-0123456789abcdef0123456789abcdef01234567"
	if id, ok := gitPackObjectID(valid); !ok || id != strings.TrimPrefix(valid, "pack-") {
		t.Fatalf("gitPackObjectID(%q) = %q, %v", valid, id, ok)
	}
	for _, name := range []string{
		"pack-0123456789ABCDEF0123456789abcdef01234567",
		"pack-0123456789abcdef0123456789abcdef0123456",
		"pack-0123456789abcdef0123456789abcdef012345678",
		"pack-zzzz456789abcdef0123456789abcdef01234567",
		"multi-pack-index",
		"",
	} {
		if _, ok := gitPackObjectID(name); ok {
			t.Fatalf("gitPackObjectID(%q) accepted a name that is not a packfile's", name)
		}
	}
}
