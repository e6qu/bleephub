package gitstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// TestRangedReadsTransferOnlyTheExtentTouched is the read-amplification claim,
// and it is the reason packing is a win rather than a catastrophe. Reading one
// object out of a packed repository must cost the bytes of the extent that
// holds it, not the bytes of the pack — otherwise every blob served from a
// monorepo would drag the whole monorepo across the wire.
//
// The extent size is turned down so the pack is many extents long without the
// test having to build a gigabyte one; the ratio is what is being pinned, and
// it is the same ratio at any scale.
func TestRangedReadsTransferOnlyTheExtentTouched(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(packChunkBytesEnv, "4096")
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 4000)
	if _, err := CompactRepository(context.Background(), stor); err != nil {
		t.Fatalf("compact: %v", err)
	}
	packBytes := 0
	for _, key := range packKeys(fake, ".pack") {
		body, _ := fake.get(key)
		packBytes = len(body)
	}
	if packBytes == 0 {
		t.Fatal("no packfile was published")
	}

	clearPackCache(t, os.Getenv(packCacheDirEnv))
	fresh := testPackedStorage(t, fake)
	fake.reset()
	if _, err := fresh.EncodedObject(plumbing.AnyObject, hashes[3]); err != nil {
		t.Fatalf("read one object: %v", err)
	}
	counts := fake.snapshot()
	if counts.get != 0 {
		t.Fatalf("reading one object issued %d whole-object GETs, want ranged reads only", counts.get)
	}
	if counts.getRanged == 0 {
		t.Fatal("reading one object issued no ranged read")
	}
	// The index and the membership filter are fetched whole because they are
	// read whole; what must not scale with the pack is the packfile traffic.
	packTraffic := counts.bytesDown - indexAndFilterBytes(fake)
	if packTraffic >= int64(packBytes)/4 {
		t.Fatalf("reading one object pulled %d bytes of a %d byte pack; a ranged read must cost the extent, not the pack",
			packTraffic, packBytes)
	}
	t.Logf("one object out of a %d byte pack transferred %d bytes of packfile (%s)", packBytes, packTraffic, counts)
}

// indexAndFilterBytes is the fixed cost of opening a pack at all: its index and
// its membership filter, both of which are read in full whatever is asked of
// the pack afterwards.
func indexAndFilterBytes(fake *fakeS3) int64 {
	total := int64(0)
	for _, extension := range []string{".idx", ".bfilter"} {
		for _, key := range packKeys(fake, extension) {
			body, _ := fake.get(key)
			total += int64(len(body))
		}
	}
	return total
}

// TestPackCacheSurvivesARestart pins that the local tier is durable. A replica
// that restarts must not have to fetch back the packs it already holds, which
// is what makes the object store the cold tier rather than the only tier.
func TestPackCacheSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(packCacheDirEnv, dir)
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 300)
	if _, err := CompactRepository(context.Background(), stor); err != nil {
		t.Fatalf("compact: %v", err)
	}

	warm := testPackedStorage(t, fake)
	clonePack(t, warm, hashes)

	// A restart drops every in-process structure — the storer, the filesystem,
	// the size memo and the cache's own recency index — but leaves the cache
	// directory on disk. Dropping the memoized cache reproduces that.
	packCaches.Delete(dir)
	restarted := testPackedStorage(t, fake)
	fake.reset()
	clonePack(t, restarted, hashes)
	counts := fake.snapshot()
	if counts.getRanged != 0 || counts.get != 0 {
		t.Fatalf("a clone after a restart re-fetched pack bytes: %s", counts)
	}
	t.Logf("clone after restart cost %s", counts)
}

// TestPackCacheEvictsToItsBudget pins that the local tier is bounded. A cache
// that grew without limit would fill the replica's disk with the packs of every
// repository it has ever served.
func TestPackCacheEvictsToItsBudget(t *testing.T) {
	dir := t.TempDir()
	cache := newPackDiskCache(dir, 4096)
	chunk := make([]byte, 1024)
	for i := range 16 {
		cache.store("bucket", "objects/pack/pack-"+strings.Repeat("a", i+1)+".pack", defaultPackChunkSize, 0, chunk)
	}
	cache.mu.Lock()
	resident := cache.bytes
	entries := len(cache.entries)
	cache.mu.Unlock()
	if resident > 4096 {
		t.Fatalf("cache holds %d bytes against a 4096 byte budget", resident)
	}
	if entries == 0 {
		t.Fatal("the cache evicted everything")
	}

	files := 0
	_ = filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files++
		}
		return nil
	})
	if files != entries {
		t.Fatalf("%d files on disk against %d cache entries: eviction did not remove the bytes", files, entries)
	}
}

// TestPackCacheDiscardsUnfinishedWrites pins that a chunk file that was being
// written when the process died is never served. A truncated extent decoded as
// a packfile is a corrupt read, and the cache has no checksum to catch it —
// the guard is that an unfinished write never has the name of a cache entry.
func TestPackCacheDiscardsUnfinishedWrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ab"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stray := filepath.Join(dir, "ab", "tmp-half-written")
	if err := os.WriteFile(stray, []byte("truncated"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cache := newPackDiskCache(dir, 1<<20)
	cache.mu.Lock()
	err := cache.initLocked()
	entries := len(cache.entries)
	cache.mu.Unlock()
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if entries != 0 {
		t.Fatalf("an unfinished write was adopted as a cache entry")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("an unfinished write was left behind")
	}
}

// TestOnlyContentAddressedKeysAreCached pins the property that makes the cache
// safe with no invalidation at all: a mutable key — a ref, the config — must
// never be read through the cached ranged path.
func TestOnlyContentAddressedKeysAreCached(t *testing.T) {
	pack := "objects/pack/pack-0123456789abcdef0123456789abcdef01234567"
	for _, name := range []string{pack + ".pack", pack + ".idx", pack + ".bfilter"} {
		if !isImmutablePackKey(name) {
			t.Fatalf("%q was not recognised as content addressed", name)
		}
	}
	for _, name := range []string{
		"config",
		"HEAD",
		"packed-refs",
		"refs/heads/main",
		"objects/ab/0123456789abcdef0123456789abcdef012345",
		"objects/pack/tmp_pack_123",
		pack + ".superseded",
		"objects/info/packs",
	} {
		if isImmutablePackKey(name) {
			t.Fatalf("%q was treated as content addressed and would be cached without invalidation", name)
		}
	}
}

// TestWritesRequestCompactionWhenTheLooseTierFills pins that the write path
// still decides WHEN to flush — the loose tier is a memtable and only the
// write path knows it has filled — while the caller that has a lifetime to
// bound a goroutine with decides who runs it. This matters beyond tidiness:
// objects also arrive through the REST git-database endpoints, which never go
// through a push, so a repository built entirely through the API depends on
// this signal rather than on post-receive scheduling.
func TestWritesRequestCompactionWhenTheLooseTierFills(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(compactionTriggerEnv, "150")
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)

	// The write path signals that the loose tier has filled; it no longer runs
	// the compaction itself, because this package has no lifetime to bound a
	// goroutine with. Capture the signal and verify it names this repository.
	var requestMu sync.Mutex
	var requested []string
	SetCompactionRequestHandler(func(repo string, _ gitStorage.Storer) {
		requestMu.Lock()
		defer requestMu.Unlock()
		requested = append(requested, repo)
	})
	t.Cleanup(func() { SetCompactionRequestHandler(nil) })

	hashes := seedObjects(t, stor, 200)
	requestMu.Lock()
	defer requestMu.Unlock()

	if len(requested) == 0 {
		t.Fatal("writing past the compaction trigger never requested a compaction")
	}
	for _, name := range requested {
		if name != stor.repo {
			t.Fatalf("compaction requested for %q, want %q", name, stor.repo)
		}
	}
	// The handler is what performs the work now, so run it here and assert the
	// pack it publishes. Doing it inline also means the test never races a
	// background goroutine against its own cleanup.
	if _, err := CompactRepository(context.Background(), stor); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(packKeys(fake, ".pack")) == 0 {
		t.Fatal("the requested compaction published no pack")
	}

	fresh := testPackedStorage(t, fake)
	for _, hash := range hashes {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost to an automatic compaction: %v", hash, err)
		}
	}
}
