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

// TestRangedReadsTransferOnlyTheExtentTouched is the read-amplification claim:
// reading one object from a packed repo must cost the extent that holds it, not
// the whole pack, or every blob would drag the whole monorepo across the wire.
// The extent size is turned down so the pack is many extents long; the pinned ratio holds at any scale.
func TestRangedReadsTransferOnlyTheExtentTouched(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.ChunkBytes = 4096
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 4000)
	if _, err := CompactRepository(context.Background(), stor); err != nil {
		t.Fatalf("compact: %v", err)
	}
	packBytes := 0
	for _, key := range packKeys(fake, ".pack") {
		body, _ := fake.Get(key)
		packBytes = len(body)
	}
	if packBytes == 0 {
		t.Fatal("no packfile was published")
	}

	clearPackCache(t, fake.opts.CacheDir)
	fresh := testPackedStorage(t, fake)
	fake.Reset()
	if _, err := fresh.EncodedObject(plumbing.AnyObject, hashes[3]); err != nil {
		t.Fatalf("read one object: %v", err)
	}
	counts := fake.Snapshot()
	if counts.Get != 1 {
		t.Fatalf("reading one object issued %d whole-object GETs, want the manifest and ranged reads only", counts.Get)
	}
	if counts.GetRanged == 0 {
		t.Fatal("reading one object issued no ranged read")
	}
	// The index and membership filter are read whole; only the packfile traffic
	// must not scale with the pack.
	packTraffic := counts.BytesDown - indexAndFilterBytes(fake)
	if packTraffic >= int64(packBytes)/4 {
		t.Fatalf("reading one object pulled %d bytes of a %d byte pack; a ranged read must cost the extent, not the pack",
			packTraffic, packBytes)
	}
	t.Logf("one object out of a %d byte pack transferred %d bytes of packfile (%s)", packBytes, packTraffic, counts)
}

// indexAndFilterBytes is the fixed cost of opening a pack: its index and
// membership filter, both read in full.
func indexAndFilterBytes(fake *fakeS3) int64 {
	total := int64(0)
	for _, extension := range []string{".idx", ".bfilter"} {
		for _, key := range packKeys(fake, extension) {
			body, _ := fake.Get(key)
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
	fake := newFakeS3(t)
	fake.opts.CacheDir = dir
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
	fake.Reset()
	clonePack(t, restarted, hashes)
	counts := fake.Snapshot()
	if counts.GetRanged != 0 || counts.Get != 1 {
		t.Fatalf("a clone after a restart should read the manifest and no pack bytes: %s", counts)
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

// cachedFiles counts the extents in a pack cache directory.
func cachedFiles(t *testing.T, dir string) int {
	t.Helper()
	files := 0
	if err := filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files++
		}
		return err
	}); err != nil {
		t.Fatalf("walk the cache: %v", err)
	}
	return files
}

// TestOnlyContentAddressedKeysAreCached pins the property that makes the cache
// safe with no invalidation at all: a mutable key — a reference, the config — or
// a loose object must never be read through the cached ranged path. Only a
// pack, its index and its filter are, because only their names are the hash of
// what they hold.
func TestOnlyContentAddressedKeysAreCached(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactionTrigger = -1
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 80)
	if err := Init(stor); err != nil {
		t.Fatalf("init: %v", err)
	}

	reader := testPackedStorage(t, fake)
	before := fake.Snapshot()
	if _, err := reader.Reference("refs/heads/main"); err != nil {
		t.Fatalf("reference: %v", err)
	}
	if _, err := reader.Config(); err != nil {
		t.Fatalf("config: %v", err)
	}
	readObjects(t, reader, hashes)
	spent := fake.Snapshot().Sub(before)
	if spent.Get < int64(len(hashes)) {
		t.Fatalf("premise: the loose objects were not read from the store: %s", spent)
	}
	if spent.GetRanged != 0 || cachedFiles(t, fake.opts.CacheDir) != 0 {
		t.Fatalf("mutable keys were read through the pack cache: %s, %d cached files", spent, cachedFiles(t, fake.opts.CacheDir))
	}

	if _, err := CompactRepository(context.Background(), stor); err != nil {
		t.Fatalf("compact: %v", err)
	}
	clearPackCache(t, fake.opts.CacheDir)
	packed := testPackedStorage(t, fake)
	if err := packed.HasEncodedObject(absentHash(1)); err == nil {
		t.Fatal("an absent object was reported present")
	}
	readObjects(t, packed, hashes)
	// The filter the probe read, and the index and the pack the reads did: one
	// extent each at this size.
	if got := cachedFiles(t, fake.opts.CacheDir); got != 3 {
		t.Fatalf("probing and reading a packed repository cached %d extents, want the pack, its index and its filter", got)
	}
}

// TestWritesRequestCompactionWhenTheLooseTierFills pins that the write path
// decides WHEN to flush (only it knows the loose tier filled) while the caller
// decides who runs it. Objects also arrive through the REST git-database
// endpoints, which never push, so an API-built repo depends on this signal
// rather than post-receive scheduling.
func TestWritesRequestCompactionWhenTheLooseTierFills(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactionTrigger = 150
	stor := testPackedStorage(t, fake)

	// The write path only signals; capture the signal and verify it names this
	// repo.
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
		if name != stor.name {
			t.Fatalf("compaction requested for %q, want %q", name, stor.name)
		}
	}
	// Run the handler inline and assert the pack; inline also avoids racing a
	// background goroutine against cleanup.
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
