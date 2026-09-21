package gitstore

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

// pushPack builds n objects in a client-side repository and returns them as the
// packfile a push would send.
func pushPack(t *testing.T, n int) ([]byte, []plumbing.Hash) {
	t.Helper()
	client := memory.NewStorage()
	hashes := seedObjects(t, client, n)
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, client, false).Encode(hashes, gitPackWindow); err != nil {
		t.Fatalf("encode push: %v", err)
	}
	return pack.Bytes(), hashes
}

// TestAPushedPackIsStoredAsAPack pins what a push costs. Before the pack was
// kept, each pushed object became a loose key at several requests apiece, and a
// push of a few hundred objects cost thousands of round trips.
func TestAPushedPackIsStoredAsAPack(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	pack, hashes := pushPack(t, 300)

	before := fake.Snapshot()
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("ingest push: %v", err)
	}
	spent := fake.Snapshot().Sub(before)

	if loose := looseKeyCount(fake); loose != 0 {
		t.Fatalf("a pushed pack left %d loose objects", loose)
	}
	for _, extension := range []string{".pack", ".idx", ".bfilter"} {
		if keys := packKeys(fake, extension); len(keys) != 1 {
			t.Fatalf("push published %d %s keys, want 1: %v", len(keys), extension, keys)
		}
	}
	// The pack is content-named: what is stored is what the client sent.
	stored, ok := fake.Get(packKeys(fake, ".pack")[0])
	if !ok || !bytes.Equal(stored, pack) {
		t.Fatal("the stored pack is not the pack that was pushed")
	}
	if spent.Put != 4 || spent.Copy != 0 || spent.Delete != 0 {
		t.Fatalf("a push should cost four writes (index, filter, pack, and the manifest that makes them the repository's) and no staging: %s", spent)
	}
	if spent.Total() > 12 {
		t.Fatalf("a push of %d objects cost %d requests; it must not scale with the object count: %s", len(hashes), spent.Total(), spent)
	}

	// The pushing handle sees its own pack at once, as the ref update that
	// follows a push requires.
	for _, hash := range hashes {
		if err := stor.HasEncodedObject(hash); err != nil {
			t.Fatalf("pushing handle cannot see %s: %v", hash, err)
		}
	}
	fresh := testPackedStorage(t, fake)
	if got, want := readObjects(t, fresh, hashes), readObjects(t, stor, hashes); len(got) != len(want) {
		t.Fatalf("a fresh replica read %d of %d objects", len(got), len(want))
	}
	if err := fresh.HasEncodedObject(absentHash(1)); err == nil {
		t.Fatal("an object that was never pushed was reported present")
	}
}

// TestAnInterruptedPushPublishesNothing pins the ordering: the .pack key is the
// commit point and is written last, so a push that dies on it leaves no pack a
// reader could list.
func TestAnInterruptedPushPublishesNothing(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	pack, hashes := pushPack(t, 100)

	fake.SetFailOn(func(method, key string) bool {
		return method == "PUT" && strings.HasSuffix(key, ".pack")
	})
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err == nil {
		t.Fatal("a push whose pack upload failed reported success")
	}
	fake.SetFailOn(nil)

	if keys := packKeys(fake, ".pack"); len(keys) != 0 {
		t.Fatalf("a failed push published %v", keys)
	}
	if err := testPackedStorage(t, fake).HasEncodedObject(hashes[0]); err == nil {
		t.Fatal("an object of a failed push is visible")
	}

	// The retry is the same content, so the same names: it completes the push.
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	fresh := testPackedStorage(t, fake)
	for _, hash := range hashes {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s missing after the retried push: %v", hash, err)
		}
	}
}

// TestOtherBackendsStillIngestAPush pins that the memory and directory backends,
// which have no pack tier, take a push exactly as they did.
func TestOtherBackendsStillIngestAPush(t *testing.T) {
	memStor, err := OpenMemory(testRepo)
	if err != nil {
		t.Fatalf("memory storage: %v", err)
	}
	dirStor, err := OpenDir(t.TempDir(), testRepo)
	if err != nil {
		t.Fatalf("directory storage: %v", err)
	}
	pack, hashes := pushPack(t, 50)
	for name, stor := range map[string]gitStorage.Storer{"memory": memStor, "directory": dirStor} {
		if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
			t.Fatalf("%s: ingest push: %v", name, err)
		}
		for _, hash := range hashes {
			if err := stor.HasEncodedObject(hash); err != nil {
				t.Fatalf("%s: object %s missing: %v", name, hash, err)
			}
		}
		if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack[:len(pack)/2])); err == nil {
			t.Fatalf("%s: a truncated pack was accepted", name)
		}
	}
}

// gitClient runs the real git in a scratch repository. The thin-pack test needs
// it because a thin pack is something a stock client produces, and a pack
// hand-built to look like one would only prove the test agrees with itself.
type gitClient struct {
	t   *testing.T
	dir string
}

func newGitClient(t *testing.T) *gitClient {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	client := &gitClient{t: t, dir: t.TempDir()}
	client.run(nil, "init", "--quiet", "--initial-branch=main")
	return client
}

func (c *gitClient) run(stdin []byte, args ...string) []byte {
	c.t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // fixed arguments, scratch directory
	cmd.Dir = c.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@gitstore.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@gitstore.invalid",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		c.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return out
}

func (c *gitClient) commit(name, body, message string) plumbing.Hash {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(body), 0o600); err != nil {
		c.t.Fatalf("write %s: %v", name, err)
	}
	c.run(nil, "add", name)
	c.run(nil, "commit", "--quiet", "-m", message)
	return plumbing.NewHash(strings.TrimSpace(string(c.run(nil, "rev-parse", "HEAD"))))
}

// sourceFile is large and repetitive enough that git stores a one-line change to
// it as a delta rather than whole.
func sourceFile(revision string) string {
	var body strings.Builder
	for line := range 400 {
		fmt.Fprintf(&body, "func handler%d() string { return \"value %d\" }\n", line, line)
	}
	body.WriteString("// " + revision + "\n")
	return body.String()
}

// TestAThinPackFromAStockGitClientIsCompleted pins the case every ordinary
// `git push` after the first hits: the client sends a thin pack, whose deltas
// lean on objects it knows the server has. It cannot be stored as it stands, so
// it must be completed — and must still cost no per-object request.
func TestAThinPackFromAStockGitClientIsCompleted(t *testing.T) {
	client := newGitClient(t)
	first := client.commit("main.go", sourceFile("first"), "first")
	second := client.commit("main.go", sourceFile("second"), "second")

	fullPack := client.run([]byte(first.String()+"\n"), "pack-objects", "--stdout", "--revs", "-q")
	thinPack := client.run([]byte(second.String()+"\n^"+first.String()+"\n"), "pack-objects", "--stdout", "--revs", "--thin", "-q")

	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)

	// Premise: the second pack really is thin. If git ever stops deltifying
	// this fixture, the test must fail here rather than pass without having
	// exercised the completion path.
	spool, probe, err := stor.stagePack("premise-*.pack")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := spool.Write(thinPack); err != nil {
		t.Fatalf("spool: %v", err)
	}
	describeErr := probe.describe(spool)
	_ = spool.Close()
	probe.cleanup()
	if describeErr == nil {
		t.Fatal("premise broken: git produced a self-contained pack, so the thin-pack path is not exercised")
	}

	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(fullPack)); err != nil {
		t.Fatalf("first push: %v", err)
	}
	before := fake.Snapshot()
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(thinPack)); err != nil {
		t.Fatalf("thin push: %v", err)
	}
	spent := fake.Snapshot().Sub(before)

	if loose := looseKeyCount(fake); loose != 0 {
		t.Fatalf("a thin push left %d loose objects", loose)
	}
	if packs := packKeys(fake, ".pack"); len(packs) != 2 {
		t.Fatalf("two pushes published %d packs, want 2", len(packs))
	}
	if spent.Put != 4 || spent.Copy != 0 {
		t.Fatalf("a thin push should still cost four writes: %s", spent)
	}

	// Every stored pack must be readable on its own: a fresh replica resolves
	// the pushed file without the client's help.
	fresh := testPackedStorage(t, fake)
	blobHash := plumbing.NewHash(strings.TrimSpace(string(client.run(nil, "rev-parse", second.String()+":main.go"))))
	got := readObjects(t, fresh, []plumbing.Hash{blobHash})[blobHash]
	if want := sourceFile("second") + "\x00blob"; got != want {
		t.Fatalf("the file pushed thin read back wrong (%d bytes, want %d)", len(got), len(want))
	}
	for _, key := range packKeys(fake, ".pack") {
		body, _ := fake.Get(key)
		spool, probe, err := stor.stagePack("verify-*.pack")
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		if _, err := spool.Write(body); err != nil {
			t.Fatalf("spool: %v", err)
		}
		err = probe.describe(spool)
		_ = spool.Close()
		probe.cleanup()
		if err != nil {
			t.Fatalf("stored pack %s is not self-contained: %v", key, err)
		}
	}
}

func TestPacksToMergeKeepsTheSizesGeometric(t *testing.T) {
	pack := func(name string, size int64) livePack { return livePack{name: name, size: size} }
	cases := []struct {
		name  string
		packs []livePack
		want  string
	}{
		{"equal packs all merge", []livePack{pack("a", 10), pack("b", 10), pack("c", 10)}, "a,b,c"},
		{"a geometric run is left alone", []livePack{pack("a", 1), pack("b", 2), pack("c", 6), pack("d", 18)}, ""},
		{"small pushes merge and the big pack is not rewritten",
			[]livePack{pack("big", 1000), pack("p1", 3), pack("p2", 3), pack("p3", 3), pack("p4", 3)}, "p1,p2,p3,p4"},
		{"a pack too small for what is below it is rolled up",
			[]livePack{pack("big", 1000), pack("mid", 12), pack("p1", 5), pack("p2", 5)}, "mid,p1,p2"},
		{"one pack is never merged into itself", []livePack{pack("a", 10)}, ""},
		{"nothing to merge", nil, ""},
	}
	for _, tc := range cases {
		if got := strings.Join(packsToMerge(tc.packs), ","); got != tc.want {
			t.Errorf("%s: merged %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSmallPushesMergeWithoutRewritingTheRepository drives the policy end to
// end: a large first push, then enough small ones to cross the merge threshold.
// The merge must fold the small packs together and leave the large one alone,
// and a second compaction straight after must find nothing to do — before
// retired packs were excluded, it re-merged everything on every run until
// their grace period closed.
func TestSmallPushesMergeWithoutRewritingTheRepository(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactionTrigger = -1
	stor := testPackedStorage(t, fake)

	big, bigHashes := pushPack(t, 600)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(big)); err != nil {
		t.Fatalf("big push: %v", err)
	}
	bigKey := packKeys(fake, ".pack")[0]

	all := append([]plumbing.Hash(nil), bigHashes...)
	for push := range compactionMergeThreshold + 1 {
		client := memory.NewStorage()
		var hashes []plumbing.Hash
		for i := range 3 {
			hashes = append(hashes, storeBlob(t, client, fmt.Sprintf("push %d object %d", push, i)))
		}
		var pack bytes.Buffer
		if _, err := packfile.NewEncoder(&pack, client, false).Encode(hashes, gitPackWindow); err != nil {
			t.Fatalf("encode push %d: %v", push, err)
		}
		if err := packfile.UpdateObjectStorage(stor, &pack); err != nil {
			t.Fatalf("push %d: %v", push, err)
		}
		all = append(all, hashes...)
	}

	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.Merged != 3*(compactionMergeThreshold+1) {
		t.Fatalf("merged %d objects, want only the %d from the small pushes", result.Merged, 3*(compactionMergeThreshold+1))
	}
	retired := storedManifest(t, fake).Retired
	if len(retired) != compactionMergeThreshold+1 {
		t.Fatalf("premise: the merge retired %d packs, want the %d small ones", len(retired), compactionMergeThreshold+1)
	}
	for _, pack := range retired {
		if strings.HasSuffix(bigKey, "/"+pack.Name+".pack") {
			t.Fatal("the large pack was rewritten by a merge of small pushes")
		}
	}

	again, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if again.Merged != 0 || again.PackName != "" {
		t.Fatalf("a compaction straight after a merge rewrote %d objects into %q", again.Merged, again.PackName)
	}

	fresh := testPackedStorage(t, fake)
	for _, hash := range all {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost: %v", hash, err)
		}
	}
}

func storeBlob(t *testing.T, stor gitStorage.Storer, body string) plumbing.Hash {
	t.Helper()
	obj := stor.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(body)))
	writer, err := obj.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatalf("blob write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("blob close: %v", err)
	}
	hash, err := stor.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}
	return hash
}

// TestARunOfSmallPushesRequestsCompaction pins the second trigger. Small pushes
// never add up to the object trigger, but each leaves a pack, and it is the
// pack count that every packed lookup pays for.
func TestARunOfSmallPushesRequestsCompaction(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)

	var requestMu sync.Mutex
	requested := 0
	SetCompactionRequestHandler(func(string, gitStorage.Storer) {
		requestMu.Lock()
		defer requestMu.Unlock()
		requested++
	})
	t.Cleanup(func() { SetCompactionRequestHandler(nil) })

	for push := range compactionMergeThreshold + 1 {
		client := memory.NewStorage()
		hash := storeBlob(t, client, fmt.Sprintf("small push %d", push))
		var pack bytes.Buffer
		if _, err := packfile.NewEncoder(&pack, client, false).Encode([]plumbing.Hash{hash}, gitPackWindow); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := packfile.UpdateObjectStorage(stor, &pack); err != nil {
			t.Fatalf("push %d: %v", push, err)
		}
		requestMu.Lock()
		got := requested
		requestMu.Unlock()
		if want := 0; push < compactionMergeThreshold && got != want {
			t.Fatalf("compaction requested after only %d pushes", push+1)
		}
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	if requested != 1 {
		t.Fatalf("%d pushes requested %d compactions, want 1", compactionMergeThreshold+1, requested)
	}
}

// smallPush publishes one single-blob pack, as a small push does.
func smallPush(t *testing.T, stor gitStorage.Storer, body string) plumbing.Hash {
	t.Helper()
	client := memory.NewStorage()
	hash := storeBlob(t, client, body)
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, client, false).Encode([]plumbing.Hash{hash}, gitPackWindow); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := packfile.UpdateObjectStorage(stor, &pack); err != nil {
		t.Fatalf("push %q: %v", body, err)
	}
	return hash
}

// countCompactionRequests installs a handler that counts.
func countCompactionRequests(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	requested := 0
	SetCompactionRequestHandler(func(string, gitStorage.Storer) {
		mu.Lock()
		defer mu.Unlock()
		requested++
	})
	t.Cleanup(func() { SetCompactionRequestHandler(nil) })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return requested
	}
}

// TestAPushToAWarmReplicaCostsItsUploadsAndNothingElse pins what a push spends
// on the object store: the pack, its index and its filter, and the conditional
// write of the manifest that makes them part of the repository. The handle holds
// the manifest, so it reads nothing before it writes. It
// used to spend a listing of the pack directory to adopt the pack, before that
// a listing of the loose tier and a second of the pack directory rebuilding a
// membership index it had thrown away, and a read of the index it had just
// uploaded — and the object pushed must still be readable without any of them.
func TestAPushToAWarmReplicaCostsItsUploadsAndNothingElse(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	smallPush(t, stor, "the push that warms the replica")

	before := fake.Snapshot()
	hash := smallPush(t, stor, "the push that is measured")
	spent := fake.Snapshot().Sub(before)
	if spent.Put != 4 || spent.Total() != 4 {
		t.Fatalf("a push spent %s, want its three uploads, the commit of the manifest, and nothing else", spent)
	}

	before = fake.Snapshot()
	if got := readObjects(t, stor, []plumbing.Hash{hash})[hash]; !strings.HasPrefix(got, "the push that is measured") {
		t.Fatalf("read back %q", got)
	}
	if err := stor.HasEncodedObject(plumbing.NewHash("1111111111111111111111111111111111111111")); err == nil {
		t.Fatal("an object nobody pushed was reported present")
	}
	if spent := fake.Snapshot().Sub(before); spent.Get+spent.GetRanged != 0 {
		t.Fatalf("reading the pushed object back downloaded what was just uploaded: %s", spent)
	}
}

// TestPacksLeftByAnEarlierProcessCountTowardCompaction pins that the pack count
// a compaction is due at is the repository's, not this process's. A server that
// restarted every few pushes never reached the threshold by its own tally, and
// its repositories gathered packs without limit.
func TestPacksLeftByAnEarlierProcessCountTowardCompaction(t *testing.T) {
	fake := newFakeS3(t)
	earlier := testPackedStorage(t, fake)
	requested := countCompactionRequests(t)
	for push := range compactionMergeThreshold {
		smallPush(t, earlier, fmt.Sprintf("before the restart %d", push))
	}
	if got := requested(); got != 0 {
		t.Fatalf("premise: %d compactions requested below the threshold", got)
	}

	restarted := testPackedStorage(t, fake)
	smallPush(t, restarted, "the first push after the restart")
	if got := requested(); got != 1 {
		t.Fatalf("the push that crossed the threshold requested %d compactions, want 1", got)
	}
}

// TestAPushFoldsInALooseTierWorthPacking pins the other reason a push asks for
// a compaction. Objects written through the API land loose, and stay below the
// write trigger for a long time; a push is when they are packed, as they were
// when every push ran a compaction.
func TestAPushFoldsInALooseTierWorthPacking(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	requested := countCompactionRequests(t)

	smallPush(t, stor, "a push with nothing loose behind it")
	if got := requested(); got != 0 {
		t.Fatalf("a push to a tidy repository requested %d compactions", got)
	}
	for i := range compactionMinLooseObjects {
		storeBlob(t, stor, fmt.Sprintf("written through the API %d", i))
	}
	smallPush(t, stor, "a push with a loose tier behind it")
	if got := requested(); got != 1 {
		t.Fatalf("a push behind %d loose objects requested %d compactions, want 1", compactionMinLooseObjects, got)
	}
}
