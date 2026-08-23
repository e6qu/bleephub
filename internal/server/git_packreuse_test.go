package bleephub

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// packReuseCommits is how many commits the packed fixture carries. Each one
// rewrites the same file and adds a new one, so the history is deep enough that
// the stored pack holds delta chains rather than a handful of whole objects.
const packReuseCommits = 24

// packReuseSignature is a fixed identity and instant, so the fixture's object
// ids — and therefore the stored pack — are the same on every run.
func packReuseSignature(index int) *object.Signature {
	return &object.Signature{
		Name:  "Pack Reuse Fixture",
		Email: "packs@bleephub.invalid",
		When:  time.Date(2021, time.January, 1, 0, 0, index, 0, time.UTC),
	}
}

// seedPackedGitRepo builds a repository directly on disk, packs it with the
// real git binary, and only then registers it with the server — so the storer
// the server opens sees a repository whose objects are all in one packfile,
// which is the state compaction leaves behind.
//
// The pack is made by git rather than by this package so the bytes the reuse
// path copies are a real packfile and not something written by the same code
// that reads it.
func seedPackedGitRepo(t *testing.T, srv *isolatedServer, name string) (gitStorage.Storer, string) {
	t.Helper()
	fullName := "admin/" + name
	stor, err := gitstore.OpenOrInitGitStorage(context.Background(), fullName)
	if err != nil {
		t.Fatalf("open git storage for %s: %v", fullName, err)
	}
	files := map[string]string{"f.txt": "line 0\n", "README.md": "# fixture\n"}
	if _, err := initRepoWithFiles(stor, "main", "root", files, packReuseSignature(0)); err != nil {
		t.Fatalf("seed root commit: %v", err)
	}
	content := "line 0\n"
	for i := 1; i < packReuseCommits; i++ {
		content += "line " + strconv.Itoa(i) + "\n"
		if _, err := createFileCommit(stor, "main", "f.txt", content, "c"+strconv.Itoa(i), packReuseSignature(i)); err != nil {
			t.Fatalf("seed commit c%d: %v", i, err)
		}
	}
	if err := store.SetGitHeadBranch(stor, "main"); err != nil {
		t.Fatalf("point git HEAD at main: %v", err)
	}

	repoDir, err := gitstore.RepoGitDirPath(gitstore.GitDataDir(), fullName)
	if err != nil {
		t.Fatalf("resolve the repository directory: %v", err)
	}
	git := requireGitCLI(t)
	git.run(repoDir, "--git-dir", repoDir, "repack", "-a", "-d", "-q")

	admin := srv.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("admin user is missing")
	}
	// CreateRepo opens the storage itself, so the storer the server serves
	// from is one that was opened after the pack was published.
	if srv.store.CreateRepo(admin, name, "pack reuse fixture", false) == nil {
		t.Fatalf("create repo %s", name)
	}
	served := srv.store.GetGitStorage("admin", name)
	if served == nil {
		t.Fatalf("repo %s has no git storage", fullName)
	}
	return served, repoDir
}

// storedPackNames lists the packfiles a repository directory holds.
func storedPackNames(t *testing.T, repoDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoDir, "objects", "pack"))
	if err != nil {
		t.Fatalf("read the pack directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".pack") {
			names = append(names, strings.TrimSuffix(entry.Name(), ".pack"))
		}
	}
	return names
}

// fullClonePack asks the fetch path for everything a branch reaches and returns
// the packfile bytes it answered with.
func fullClonePack(t *testing.T, stor storer.Storer, branch string) []byte {
	t.Helper()
	ref, err := stor.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		t.Fatalf("resolve %s: %v", branch, err)
	}
	request := &gitUploadRequest{wants: []plumbing.Hash{ref.Hash()}, done: true, noProgress: true}
	boundary, err := gitFetchBoundaryFor(stor, request)
	if err != nil {
		t.Fatalf("build the fetch boundary: %v", err)
	}
	var pack bytes.Buffer
	if err := sendGitPackfile(stor, &pack, request, boundary, gitSidebandNone); err != nil {
		t.Fatalf("send the packfile: %v", err)
	}
	return pack.Bytes()
}

// TestFullCloneOfAPackedRepositoryCopiesTheStoredPack is the reuse path's
// central claim: the bytes a clone receives are the bytes storage holds, not a
// re-encoding of them.
func TestFullCloneOfAPackedRepositoryCopiesTheStoredPack(t *testing.T) {
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	srv := newIsolatedServer(t)
	stor, repoDir := seedPackedGitRepo(t, srv, "packed-reuse")

	names := storedPackNames(t, repoDir)
	if len(names) != 1 {
		t.Fatalf("the fixture has %d packs, want exactly 1: %v", len(names), names)
	}
	storedPack, err := os.ReadFile(filepath.Join(repoDir, "objects", "pack", names[0]+".pack"))
	if err != nil {
		t.Fatalf("read the stored pack: %v", err)
	}
	entries := storedPack[gitPackHeaderSize : len(storedPack)-gitPackTrailerSize]

	served := fullClonePack(t, gitStorerWithPackReuse(context.Background(), "admin/packed-reuse", stor), "main")
	if !bytes.Contains(served, entries) {
		t.Fatal("the served pack does not contain the stored pack's entries: the fetch re-encoded them")
	}
	// Reuse of the whole repository leaves nothing to append, so the answer is
	// the stored entries between a header of this fetch's own and a checksum
	// over what was actually written.
	if got, want := len(served), gitPackHeaderSize+len(entries)+gitPackTrailerSize; got != want {
		t.Fatalf("the served pack is %d bytes, want %d — it carries objects the stored pack already held", got, want)
	}

	// Without the pack directory attached the same request goes through the
	// encoder, which is the fallthrough every request that cannot reuse takes.
	encoded := fullClonePack(t, stor, "main")
	if bytes.Equal(encoded, served) {
		t.Fatal("the encoder produced the reused pack byte for byte, so this test cannot tell the two paths apart")
	}
}

// TestReusedPackIsAValidPackfile hands the served bytes to git itself, because
// "byte-valid and complete" is git's judgement to make, not this package's.
func TestReusedPackIsAValidPackfile(t *testing.T) {
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	srv := newIsolatedServer(t)
	stor, _ := seedPackedGitRepo(t, srv, "packed-valid")
	served := fullClonePack(t, gitStorerWithPackReuse(context.Background(), "admin/packed-valid", stor), "main")

	git := requireGitCLI(t)
	target := t.TempDir()
	git.run(target, "init", "--bare", "--quiet")
	packPath := filepath.Join(target, "incoming.pack")
	if err := os.WriteFile(packPath, served, 0o600); err != nil {
		t.Fatal(err)
	}
	// index-pack --strict verifies the checksum, every entry header, every
	// delta chain and that the pack is self-contained.
	git.run(target, "--git-dir", target, "index-pack", "--strict", packPath)
	git.run(target, "--git-dir", target, "fsck", "--no-progress", "--strict")
}

// TestPackReuseRefusesPacksTheAnswerDoesNotOwe covers the safety condition
// directly: a pack holding anything outside the plan is not reused, whatever
// narrowed the plan.
func TestPackReuseRefusesPacksTheAnswerDoesNotOwe(t *testing.T) {
	t.Parallel()
	objectID := func(b byte) plumbing.Hash {
		var h plumbing.Hash
		h[0] = b
		return h
	}
	planOf := func(ids ...plumbing.Hash) *gitPackPlan {
		plan := &gitPackPlan{packed: map[plumbing.Hash]bool{}}
		for _, id := range ids {
			plan.objects = append(plan.objects, id)
			plan.packed[id] = true
		}
		return plan
	}
	packOf := func(name string, ids ...plumbing.Hash) *gitStoredPack {
		pack := &gitStoredPack{name: name, objects: map[plumbing.Hash]bool{}}
		for _, id := range ids {
			pack.objects[id] = true
		}
		return pack
	}
	a, b, c, d := objectID(1), objectID(2), objectID(3), objectID(4)

	t.Run("a pack holding an object the plan does not is refused", func(t *testing.T) {
		t.Parallel()
		packs := []*gitStoredPack{packOf("pack-wide", a, b, c)}
		if got := gitSelectReusablePacks(packs, planOf(a, b)); got != nil {
			t.Fatalf("a pack carrying an unwanted object was reused: %v", got[0].name)
		}
	})

	t.Run("a pack the plan wholly owes is reused", func(t *testing.T) {
		t.Parallel()
		packs := []*gitStoredPack{packOf("pack-exact", a, b, c)}
		got := gitSelectReusablePacks(packs, planOf(a, b, c))
		if len(got) != 1 || got[0].name != "pack-exact" {
			t.Fatalf("the pack the plan owes in full was not reused: %v", got)
		}
	})

	t.Run("overlapping packs never both contribute", func(t *testing.T) {
		t.Parallel()
		packs := []*gitStoredPack{packOf("pack-small", a, b), packOf("pack-large", a, b, c)}
		got := gitSelectReusablePacks(packs, planOf(a, b, c, d))
		if len(got) != 1 || got[0].name != "pack-large" {
			t.Fatalf("overlapping packs were not resolved to the largest: %v", got)
		}
	})

	t.Run("packs too small to be worth the uncompressed remainder are refused", func(t *testing.T) {
		t.Parallel()
		plan := planOf(a, b, c, d, objectID(5), objectID(6), objectID(7), objectID(8))
		if got := gitSelectReusablePacks([]*gitStoredPack{packOf("pack-tiny", a)}, plan); got != nil {
			t.Fatalf("a pack covering an eighth of the answer was reused: %v", got[0].name)
		}
	})
}

// TestPackedRepositoryServesEveryFetchShape drives the real git client against
// a packed repository over the whole matrix of requests whose plan is not the
// repository — each one must come back complete and consistent, which is what
// proves reuse either applied correctly or correctly stood aside.
func TestPackedRepositoryServesEveryFetchShape(t *testing.T) {
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	srv := newIsolatedServer(t)
	_, repoDir := seedPackedGitRepo(t, srv, "packed-matrix")
	cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/packed-matrix.git"
	git := requireGitCLI(t)
	root := t.TempDir()

	full := filepath.Join(root, "full")
	git.run(root, "clone", cloneURL, full)
	requireCommitCount(t, git, full, "HEAD", packReuseCommits)
	// The smart-HTTP transport reaches the reuse path too, which is visible in
	// the clone: index-pack writes the stream it received, so the client's
	// packfile carries the server's stored entries byte for byte.
	requireStoredEntriesReached(t, repoDir, full)
	git.run(full, "fsck", "--no-progress", "--strict")
	if got, want := len(strings.Split(strings.TrimSpace(readFixtureFile(t, full, "f.txt")), "\n")), packReuseCommits; got != want {
		t.Fatalf("the cloned working tree has %d lines, want %d", got, want)
	}

	shallow := filepath.Join(root, "shallow")
	git.run(root, "clone", "--depth", "1", cloneURL, shallow)
	requireCommitCount(t, git, shallow, "HEAD", 1)
	git.run(shallow, "fsck", "--no-progress", "--strict")

	partial := filepath.Join(root, "partial")
	git.run(root, "clone", "--filter=blob:none", "--no-checkout", cloneURL, partial)
	requireCommitCount(t, git, partial, "HEAD", packReuseCommits)
	git.run(partial, "fsck", "--no-progress")
	// A blob:none clone that received the whole pack anyway would hold every
	// blob already, and this count would be the repository's rather than zero.
	if blobs := strings.TrimSpace(git.run(partial, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)")); strings.Contains(blobs, "blob") {
		t.Fatal("a --filter=blob:none clone received blobs: the filtered plan was answered with a whole pack")
	}

	// An incremental fetch after a push: the client already holds everything
	// the stored pack does, so the answer is the new objects alone.
	git.run(full, "config", "user.name", "Pack Reuse")
	git.run(full, "config", "user.email", "packs@bleephub.invalid")
	if err := os.WriteFile(filepath.Join(full, "f.txt"), []byte("after the push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git.run(full, "add", "f.txt")
	git.run(full, "commit", "-m", "after the push")
	git.run(full, "push", "origin", "HEAD:main")

	incremental := filepath.Join(root, "shallow")
	git.run(incremental, "fetch", "--unshallow")
	requireCommitCount(t, git, incremental, "origin/main", packReuseCommits+1)
	git.run(incremental, "fsck", "--no-progress", "--strict")

	behind := filepath.Join(root, "behind")
	git.run(root, "clone", cloneURL, behind)
	requireCommitCount(t, git, behind, "HEAD", packReuseCommits+1)
	git.run(behind, "fsck", "--no-progress", "--strict")
}

// requireStoredEntriesReached asserts that a clone's packfile holds the entry
// region of the server's stored pack.
func requireStoredEntriesReached(t *testing.T, repoDir, cloneDir string) {
	t.Helper()
	names := storedPackNames(t, repoDir)
	if len(names) != 1 {
		t.Fatalf("the fixture has %d packs, want exactly 1: %v", len(names), names)
	}
	stored, err := os.ReadFile(filepath.Join(repoDir, "objects", "pack", names[0]+".pack"))
	if err != nil {
		t.Fatalf("read the stored pack: %v", err)
	}
	entries := stored[gitPackHeaderSize : len(stored)-gitPackTrailerSize]
	received := storedPackNames(t, filepath.Join(cloneDir, ".git"))
	for _, name := range received {
		raw, err := os.ReadFile(filepath.Join(cloneDir, ".git", "objects", "pack", name+".pack"))
		if err != nil {
			t.Fatalf("read the received pack: %v", err)
		}
		if bytes.Contains(raw, entries) {
			return
		}
	}
	t.Fatalf("no pack in the clone carries the stored entries: the HTTP fetch re-encoded them (%v)", received)
}

func readFixtureFile(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// benchmarkPackedHistory builds a linear history directly out of plumbing
// objects — a blob, a tree and a commit per revision — because the point of the
// fixture is object count, and going through a worktree per commit would spend
// the whole benchmark building it.
//
// Each revision rewrites one file of a wide tree, so consecutive blobs and
// consecutive trees are near-identical: the shape a delta window is there to
// exploit, and therefore the shape that makes re-encoding expensive.
func benchmarkPackedHistory(b *testing.B, stor gitStorage.Storer, commits, files int) {
	b.Helper()
	write := func(encode func(plumbing.EncodedObject) error) plumbing.Hash {
		obj := stor.NewEncodedObject()
		if err := encode(obj); err != nil {
			b.Fatal(err)
		}
		hash, err := stor.SetEncodedObject(obj)
		if err != nil {
			b.Fatal(err)
		}
		return hash
	}
	blob := func(content string) plumbing.Hash {
		return write(func(obj plumbing.EncodedObject) error {
			obj.SetType(plumbing.BlobObject)
			obj.SetSize(int64(len(content)))
			writer, err := obj.Writer()
			if err != nil {
				return err
			}
			if _, err := io.WriteString(writer, content); err != nil {
				return err
			}
			return writer.Close()
		})
	}

	entries := make([]object.TreeEntry, files)
	revisions := make([]int, files)
	body := func(file, revision int) string {
		var text strings.Builder
		for line := 0; line < 40; line++ {
			text.WriteString("file " + strconv.Itoa(file) + " line " + strconv.Itoa(line) +
				" revision " + strconv.Itoa(revision) + "\n")
		}
		return text.String()
	}
	for i := range entries {
		entries[i] = object.TreeEntry{
			Name: "f" + strconv.Itoa(100000+i) + ".txt",
			Mode: filemode.Regular,
			Hash: blob(body(i, 0)),
		}
	}

	var parents []plumbing.Hash
	for revision := 1; revision <= commits; revision++ {
		changed := revision % files
		revisions[changed]++
		entries[changed].Hash = blob(body(changed, revisions[changed]))
		treeEntries := append([]object.TreeEntry(nil), entries...)
		treeHash := write((&object.Tree{Entries: treeEntries}).Encode)
		commit := &object.Commit{
			Author:       *packReuseSignature(revision),
			Committer:    *packReuseSignature(revision),
			Message:      "revision " + strconv.Itoa(revision) + "\n",
			TreeHash:     treeHash,
			ParentHashes: parents,
		}
		parents = []plumbing.Hash{write(commit.Encode)}
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), parents[0])); err != nil {
		b.Fatal(err)
	}
}

// benchmarkPackedRepository seeds a repository, packs it with git, and returns
// a freshly opened storer for it — one that sees the pack the way a server
// process restarted after a compaction would.
func benchmarkPackedRepository(b *testing.B, commits, files int) gitStorage.Storer {
	b.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git is not installed")
	}
	b.Setenv("BLEEPHUB_GIT_DIR", b.TempDir())
	const fullName = "bench/packs"
	stor, err := gitstore.OpenOrInitGitStorage(context.Background(), fullName)
	if err != nil {
		b.Fatal(err)
	}
	benchmarkPackedHistory(b, stor, commits, files)

	repoDir, err := gitstore.RepoGitDirPath(gitstore.GitDataDir(), fullName)
	if err != nil {
		b.Fatal(err)
	}
	repack := exec.Command("git", "--git-dir", repoDir, "repack", "-a", "-d", "-q")
	repack.Env = append(os.Environ(), hermeticGitTestEnv(b.TempDir())...)
	if output, err := repack.CombinedOutput(); err != nil {
		b.Fatalf("git repack: %v\n%s", err, output)
	}
	reopened, err := gitstore.OpenOrInitGitStorage(context.Background(), fullName)
	if err != nil {
		b.Fatal(err)
	}
	return reopened
}

// BenchmarkFullCloneOfAPackedRepository measures the two answers to the same
// request: the packfile copied out of storage, and the packfile the encoder
// derives by decoding every stored delta and searching for new ones.
func BenchmarkFullCloneOfAPackedRepository(b *testing.B) {
	const (
		commits = 4000
		files   = 64
	)
	stor := benchmarkPackedRepository(b, commits, files)
	reference, err := stor.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		b.Fatal(err)
	}
	clone := func(b *testing.B, from storer.Storer) {
		b.Helper()
		request := &gitUploadRequest{wants: []plumbing.Hash{reference.Hash()}, done: true, noProgress: true}
		boundary, err := gitFetchBoundaryFor(from, request)
		if err != nil {
			b.Fatal(err)
		}
		var served, objects int
		b.ResetTimer()
		before := processCPUNanoseconds(b)
		for i := 0; i < b.N; i++ {
			var pack countingWriter
			if err := sendGitPackfile(from, &pack, request, boundary, gitSidebandNone); err != nil {
				b.Fatal(err)
			}
			served, objects = pack.bytes, pack.objects()
		}
		spent := processCPUNanoseconds(b) - before
		b.StopTimer()
		b.ReportMetric(float64(served), "pack-bytes")
		b.ReportMetric(float64(objects), "objects")
		b.ReportMetric(float64(spent)/float64(b.N), "cpu-ns/op")
	}
	b.Run("reused", func(b *testing.B) {
		clone(b, gitStorerWithPackReuse(context.Background(), "bench/packs", stor))
	})
	b.Run("encoded", func(b *testing.B) {
		clone(b, stor)
	})
}

// countingWriter measures a packfile without keeping it, so the benchmark's
// allocation profile is the pack path's rather than the buffer's. It keeps the
// header, which is where the entry count is.
type countingWriter struct {
	bytes  int
	header []byte
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if len(w.header) < gitPackHeaderSize {
		w.header = append(w.header, p[:min(len(p), gitPackHeaderSize-len(w.header))]...)
	}
	w.bytes += len(p)
	return len(p), nil
}

// objects reads the entry count out of the packfile header.
func (w *countingWriter) objects() int {
	if len(w.header) < gitPackHeaderSize {
		return 0
	}
	return int(binary.BigEndian.Uint32(w.header[8:12]))
}

// processCPUNanoseconds is the CPU this process has burned in user and system
// mode. The pack path runs the delta search across several goroutines, so wall
// clock understates what a clone costs a server answering many at once; this is
// the number that does not.
func processCPUNanoseconds(b *testing.B) int64 {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	nanoseconds := func(t syscall.Timeval) int64 {
		return t.Sec*int64(time.Second) + int64(t.Usec)*int64(time.Microsecond)
	}
	return nanoseconds(usage.Utime) + nanoseconds(usage.Stime)
}

// TestReuseConcatenatesSeveralStoredPacks covers the multi-pack case, which is
// what a repository looks like between compactions: the answer is built from
// two stored entry regions laid end to end, and the offset deltas inside each
// still resolve because each region keeps its own internal order and spacing.
func TestReuseConcatenatesSeveralStoredPacks(t *testing.T) {
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	srv := newIsolatedServer(t)
	const name = "packed-pair"
	stor, repoDir := seedPackedGitRepo(t, srv, name)

	// A second batch of commits, packed on its own: `git repack` without -a
	// packs the objects no existing pack holds, so the two packs are disjoint.
	content := ""
	for i := packReuseCommits; i < packReuseCommits*2; i++ {
		content += "second batch line " + strconv.Itoa(i) + "\n"
		if _, err := createFileCommit(stor, "main", "f.txt", content, "s"+strconv.Itoa(i), packReuseSignature(i)); err != nil {
			t.Fatalf("seed commit s%d: %v", i, err)
		}
	}
	git := requireGitCLI(t)
	git.run(repoDir, "--git-dir", repoDir, "repack", "-d", "-q")
	names := storedPackNames(t, repoDir)
	if len(names) != 2 {
		t.Fatalf("the fixture has %d packs, want exactly 2: %v", len(names), names)
	}

	// The storer is reopened because the pack it must serve from was published
	// after the one the server has been holding was opened.
	reopened, err := gitstore.OpenOrInitGitStorage(context.Background(), "admin/"+name)
	if err != nil {
		t.Fatalf("reopen git storage: %v", err)
	}
	served := fullClonePack(t, gitStorerWithPackReuse(context.Background(), "admin/"+name, reopened), "main")

	stored := 0
	for _, packName := range names {
		raw, err := os.ReadFile(filepath.Join(repoDir, "objects", "pack", packName+".pack"))
		if err != nil {
			t.Fatalf("read the stored pack: %v", err)
		}
		entries := raw[gitPackHeaderSize : len(raw)-gitPackTrailerSize]
		if !bytes.Contains(served, entries) {
			t.Fatalf("the served pack does not contain %s: only one of two packs was reused", packName)
		}
		stored += len(entries)
	}
	if got, want := len(served), gitPackHeaderSize+stored+gitPackTrailerSize; got != want {
		t.Fatalf("the served pack is %d bytes, want %d — it is not exactly the two stored regions", got, want)
	}

	target := t.TempDir()
	git.run(target, "init", "--bare", "--quiet")
	packPath := filepath.Join(target, "incoming.pack")
	if err := os.WriteFile(packPath, served, 0o600); err != nil {
		t.Fatal(err)
	}
	git.run(target, "--git-dir", target, "index-pack", "--strict", packPath)
	git.run(target, "--git-dir", target, "fsck", "--no-progress", "--strict")
}

// TestPackIndexCountMatchesTheDecodedIndex: the cheap count read from a pack
// index's fanout is what decides whether the expensive decode happens at all,
// so it has to be the number the decode would produce.
func TestPackIndexCountMatchesTheDecodedIndex(t *testing.T) {
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	srv := newIsolatedServer(t)
	_, repoDir := seedPackedGitRepo(t, srv, "packed-count")
	fs := osfs.New(repoDir)
	for _, name := range storedPackNames(t, repoDir) {
		count, err := gitPackIndexCount(fs, name)
		if err != nil {
			t.Fatalf("read the fanout of %s: %v", name, err)
		}
		objects, err := gitPackIndexObjects(fs, name)
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if count != len(objects) {
			t.Fatalf("%s: the fanout says %d objects, the index lists %d", name, count, len(objects))
		}
		if count == 0 {
			t.Fatalf("%s holds no objects", name)
		}
	}
}
