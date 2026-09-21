package gitstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage"
)

const testRepo = "octocat/monorepo"

func testPackedStorage(t *testing.T, fake *fakeS3) *repository {
	t.Helper()
	stor, err := packedStorage(fake, testRepo)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	return stor
}

// readObjects reads every hash back through the storer and returns the bytes.
func readObjects(t *testing.T, stor *repository, hashes []plumbing.Hash) map[plumbing.Hash]string {
	t.Helper()
	out := make(map[plumbing.Hash]string, len(hashes))
	for _, hash := range hashes {
		obj, err := stor.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			t.Fatalf("read %s: %v", hash, err)
		}
		reader, err := obj.Reader()
		if err != nil {
			t.Fatalf("reader for %s: %v", hash, err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read body of %s: %v", hash, err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("close reader for %s: %v", hash, err)
		}
		out[hash] = string(body) + "\x00" + obj.Type().String()
	}
	return out
}

// TestCompactionPreservesEveryObject asserts a merge returns every object
// byte-for-byte, read through a storer that has never seen the packs merged.
func TestCompactionPreservesEveryObject(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 300)
	hashes = append(hashes, flushedBlobs(t, stor, "second", 150)...)
	hashes = append(hashes, flushedBlobs(t, stor, "third", 150)...)
	want := readObjects(t, stor, hashes)
	if live := len(storedManifest(t, fake).Packs); live != 3 {
		t.Fatalf("premise: the repository holds %d packs, want 3 to merge", live)
	}

	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.Merged != len(hashes) || result.PackName == "" {
		t.Fatalf("merged %d of %d objects into %q", result.Merged, len(hashes), result.PackName)
	}
	if live := storedManifest(t, fake).Packs; len(live) != 1 || live[0].Name != result.PackName {
		t.Fatalf("after the merge the manifest names %+v, want the one merged pack", live)
	}
	if loose := looseLayoutKeys(fake); len(loose) != 0 {
		t.Fatalf("the store holds loose objects: %v", loose)
	}

	fresh := testPackedStorage(t, fake)
	got := readObjects(t, fresh, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s changed across compaction", hash)
		}
	}

	// The seeded reference must still resolve and every object must still
	// answer HasEncodedObject, which negotiation depends on.
	ref, err := fresh.Reference("refs/heads/main")
	if err != nil {
		t.Fatalf("reference after compaction: %v", err)
	}
	if ref.Hash() != hashes[301] {
		t.Fatalf("reference points at %s, want %s", ref.Hash(), hashes[301])
	}
	for _, hash := range hashes {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("HasEncodedObject(%s) after compaction: %v", hash, err)
		}
	}
}

// looseLayoutKeys returns every key of the test repository where the earlier
// layouts kept a loose object, objects/XX/ and 38 hex digits. The engine writes
// none: every object it stores is in a pack.
func looseLayoutKeys(fake *fakeS3) []string {
	prefix := "prefix/" + testRepo + "/"
	var loose []string
	for _, key := range fake.KeysWithPrefix(prefix + "objects/") {
		if isLooseObjectKey(strings.TrimPrefix(key, prefix)) {
			loose = append(loose, key)
		}
	}
	return loose
}

// flushedBlobs writes n blobs through SetEncodedObject and flushes them, which
// lands them as one pack of their own.
func flushedBlobs(t *testing.T, stor *repository, label string, n int) []plumbing.Hash {
	t.Helper()
	hashes := make([]plumbing.Hash, 0, n)
	for i := range n {
		hashes = append(hashes, writeBlob(t, stor, fmt.Sprintf("%s object %d\n%s", label, i, blobBody(i))))
	}
	if err := stor.FlushObjects(); err != nil {
		t.Fatalf("flush %s: %v", label, err)
	}
	return hashes
}

// livePackNames is the set of packs the stored manifest names as live.
func livePackNames(t *testing.T, fake *fakeS3) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, pack := range storedManifest(t, fake).Packs {
		names[pack.Name] = true
	}
	return names
}

// TestCompactionCrashBeforeThePackIsPublishedLosesNothing interrupts compaction
// with the sidecar stored but the .pack upload failed. The manifest must still
// name the packs that were to be merged, every object must read from them, and
// a retry must succeed.
func TestCompactionCrashBeforeThePackIsPublishedLosesNothing(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)
	hashes = append(hashes, flushedBlobs(t, stor, "second", 100)...)
	want := readObjects(t, stor, hashes)
	liveBefore := livePackNames(t, fake)
	sidecarsBefore := len(packKeys(fake, sidecarSuffix))

	fake.SetFailOn(func(method, key string) bool {
		return method == "PUT" && strings.HasSuffix(key, ".pack")
	})
	if _, err := CompactRepository(context.Background(), stor); err == nil {
		t.Fatal("compaction reported success although the packfile upload failed")
	}
	fake.SetFailOn(nil)

	if len(packKeys(fake, sidecarSuffix)) == sidecarsBefore {
		t.Fatal("the test did not reach the state it is about: no sidecar was uploaded")
	}
	if packs := packKeys(fake, ".pack"); len(packs) != len(liveBefore) {
		t.Fatalf("a packfile became visible although its upload failed: %v", packs)
	}
	if live := livePackNames(t, fake); len(live) != len(liveBefore) {
		t.Fatalf("a compaction that never published its pack changed the live packs: %v, was %v", live, liveBefore)
	}

	fresh := testPackedStorage(t, fake)
	got := readObjects(t, fresh, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s unreadable after an interrupted compaction", hash)
		}
	}

	retry := testPackedStorage(t, fake)
	if result, err := CompactRepository(context.Background(), retry); err != nil || result.PackName == "" {
		t.Fatalf("retry after an interrupted compaction: %+v, %v", result, err)
	}
	after := testPackedStorage(t, fake)
	got = readObjects(t, after, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s changed across the retried compaction", hash)
		}
	}
}

// TestCompactionWhoseCommitFailsLosesNothing interrupts after the merged pack
// and its sidecar are uploaded, at the one write that would make them the
// repository's: the manifest swap. Nothing may change for a reader — the packs
// that were to be merged stay live — and a retry, which uploads the same
// content-named pack again, must succeed.
func TestCompactionWhoseCommitFailsLosesNothing(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)
	hashes = append(hashes, flushedBlobs(t, stor, "second", 100)...)
	want := readObjects(t, stor, hashes)
	liveBefore := livePackNames(t, fake)

	fake.SetFailOn(func(method, key string) bool { return method == "PUT" && key == manifestKey })
	if _, err := CompactRepository(context.Background(), stor); err == nil {
		t.Fatal("compaction reported success although its commit failed")
	}
	fake.SetFailOn(nil)

	if packs := packKeys(fake, ".pack"); len(packs) != len(liveBefore)+1 {
		t.Fatalf("premise: the merged pack was not uploaded before the commit: %v", packs)
	}
	if live := livePackNames(t, fake); len(live) != len(liveBefore) {
		t.Fatalf("a compaction whose commit failed changed the live packs: %v, was %v", live, liveBefore)
	}
	fresh := testPackedStorage(t, fake)
	got := readObjects(t, fresh, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s changed when the commit failed", hash)
		}
	}

	retry := testPackedStorage(t, fake)
	result, err := CompactRepository(context.Background(), retry)
	if err != nil || result.PackName == "" {
		t.Fatalf("retry after a failed commit: %+v, %v", result, err)
	}
	if live := livePackNames(t, fake); len(live) != 1 || !live[result.PackName] {
		t.Fatalf("after the retry the manifest names %v, want only %s", live, result.PackName)
	}
	readObjects(t, testPackedStorage(t, fake), hashes)
}

func packKeys(fake *fakeS3, extension string) []string {
	var out []string
	for _, key := range fake.KeysWithPrefix("prefix/" + testRepo + "/objects/pack/") {
		if strings.HasSuffix(key, extension) {
			out = append(out, key)
		}
	}
	return out
}

func writeBlob(t *testing.T, stor *repository, body string) plumbing.Hash {
	t.Helper()
	obj := stor.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(body)))
	writer, err := obj.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := io.WriteString(writer, body); err != nil {
		t.Fatalf("blob write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("blob close: %v", err)
	}
	hash, err := stor.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("set blob: %v", err)
	}
	return hash
}

// TestConcurrentCompactionAndWritesLoseNothing runs compaction against live
// writers on the same repository handle, which is the shape a scheduled
// compaction and an ongoing stream of API writes have: the writes flush packs
// while the compaction merges others.
func TestConcurrentCompactionAndWritesLoseNothing(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	seeded := seedObjects(t, stor, 150)
	seeded = append(seeded, flushedBlobs(t, stor, "second", 150)...)

	var mu sync.Mutex
	written := append([]plumbing.Hash(nil), seeded...)

	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for i := range 100 {
			hash := writeBlob(t, stor, "concurrent write "+strings.Repeat("x", i))
			mu.Lock()
			written = append(written, hash)
			mu.Unlock()
			if i%20 == 19 {
				if err := stor.FlushObjects(); err != nil {
					t.Errorf("flush during compaction: %v", err)
					return
				}
			}
		}
	}()
	var compactErr error
	go func() {
		defer group.Done()
		_, compactErr = CompactRepository(context.Background(), stor)
	}()
	group.Wait()
	if compactErr != nil {
		t.Fatalf("compaction during writes: %v", compactErr)
	}
	if err := stor.FlushObjects(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	fresh := testPackedStorage(t, fake)
	mu.Lock()
	defer mu.Unlock()
	for _, hash := range written {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost to a compaction running alongside writes: %v", hash, err)
		}
	}
}

// TestCompactionMergesPacksOnceTheyAccumulate: a repository written to many
// times must fold its packs together rather than keep an index per write, and
// every object must survive the fold.
func TestCompactionMergesPacksOnceTheyAccumulate(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)

	var all []plumbing.Hash
	for round := range 5 {
		all = append(all, flushedBlobs(t, stor, "round "+strings.Repeat("r", round), 20)...)
	}
	if live := len(storedManifest(t, fake).Packs); live != 5 {
		t.Fatalf("premise: five flushes left %d packs", live)
	}

	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("merging compaction: %v", err)
	}
	if result.Merged == 0 {
		t.Fatal("a repository of five packs of a size did not merge them")
	}

	fresh := testPackedStorage(t, fake)
	for _, hash := range all {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost to a merging compaction: %v", hash, err)
		}
	}

	// A merged pack's predecessors are retired in the manifest rather than
	// deleted, so a request that began before the merge can still read them.
	retired := storedManifest(t, fake).Retired
	if len(retired) == 0 {
		t.Fatal("merged packs were not retired")
	}
	for _, pack := range retired {
		for _, suffix := range packKeySuffixes {
			if _, ok := fake.Get("prefix/" + testRepo + "/objects/pack/" + pack.Name + suffix); !ok {
				t.Fatalf("retired pack %s lost its %s at once instead of aging out", pack.Name, suffix)
			}
		}
	}
}

// TestCompactionLeavesAGeometricRepositoryAlone pins that a compaction with
// nothing to merge — one pack, which would only be rewritten into itself —
// writes nothing: publishing a pack costs two uploads and a swap.
func TestCompactionLeavesAGeometricRepositoryAlone(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 4)

	before := fake.Snapshot()
	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.PackName != "" || result.Merged != 0 {
		t.Fatalf("a repository of one pack was merged into %s", result.PackName)
	}
	if spent := fake.Snapshot().Sub(before); spent.Put != 0 || spent.Delete != 0 {
		t.Fatalf("a compaction with nothing to do wrote to the store: %s", spent)
	}
}

// TestCompactRepositoryIgnoresStorageWithoutAPackTier pins that the local
// filesystem and in-memory backends are left to git's own maintenance.
func TestCompactRepositoryIgnoresStorageWithoutAPackTier(t *testing.T) {
	memStor, err := OpenMemory(testRepo)
	if err != nil {
		t.Fatalf("memory storage: %v", err)
	}
	dirStor, err := OpenDir(t.TempDir(), testRepo)
	if err != nil {
		t.Fatalf("directory storage: %v", err)
	}
	for _, stor := range []storage.Storer{memStor, dirStor} {
		result, err := CompactRepository(context.Background(), stor)
		if err != nil {
			t.Fatalf("compact: %v", err)
		}
		if result.PackName != "" || result.Merged != 0 {
			t.Fatalf("non-object-store storage reported a compaction: %+v", result)
		}
	}
}

// TestCompactionSurfacesAnObjectStoreOutage pins that a compaction that could
// not read the packs it merges fails loudly rather than publishing a pack that
// is missing whatever it could not read, and changes nothing.
func TestCompactionSurfacesAnObjectStoreOutage(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	writer := testPackedStorage(t, fake)
	hashes := seedObjects(t, writer, 200)
	hashes = append(hashes, flushedBlobs(t, writer, "second", 100)...)
	liveBefore := livePackNames(t, fake)

	// A replica with a disk of its own, so that the packs are read from the
	// store and not from the cache the writer seeded.
	apart := newFakeS3(t)
	apart.Server = fake.Server
	apart.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, apart)

	var failed atomic.Int64
	fake.SetFailOn(func(method, key string) bool {
		if method == "GET" && strings.Contains(key, "/objects/pack/") {
			failed.Add(1)
			return true
		}
		return false
	})
	_, err := CompactRepository(context.Background(), stor)
	fake.SetFailOn(nil)
	if failed.Load() == 0 {
		t.Fatal("premise: the compaction read nothing of the packs it merges")
	}
	if err == nil {
		t.Fatal("compaction reported success although it could not read the objects")
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a transient outage was reported as a missing object: %v", err)
	}
	if live := livePackNames(t, fake); len(live) != len(liveBefore) {
		t.Fatalf("a failed compaction changed the live packs: %v, was %v", live, liveBefore)
	}
	readObjects(t, testPackedStorage(t, apart), hashes)
}

// TestConcurrentReadsWritesAndCompactionAreRaceFree drives a clone, a push, and
// a compaction through one handle at once — the case the storage lock and
// per-Next iterator locking exist for; run under the race detector, its
// assertions only check no object is lost.
func TestConcurrentReadsWritesAndCompactionAreRaceFree(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	seeded := seedObjects(t, stor, 120)

	var mu sync.Mutex
	written := append([]plumbing.Hash(nil), seeded...)

	var group sync.WaitGroup
	group.Add(4)

	go func() {
		defer group.Done()
		for i := range 60 {
			hash := writeBlob(t, stor, "racing write "+strings.Repeat("w", i))
			mu.Lock()
			written = append(written, hash)
			mu.Unlock()
			// Every few writes land as a pack, for the compactions to merge.
			if i%10 == 9 {
				if err := stor.FlushObjects(); err != nil {
					t.Errorf("flush: %v", err)
					return
				}
			}
		}
	}()
	go func() {
		defer group.Done()
		for range 20 {
			for _, hash := range seeded {
				if _, err := stor.EncodedObject(plumbing.AnyObject, hash); err != nil {
					t.Errorf("read during compaction: %v", err)
					return
				}
			}
		}
	}()
	go func() {
		defer group.Done()
		for range 40 {
			iter, err := stor.IterReferences()
			if err != nil {
				t.Errorf("iterate references: %v", err)
				return
			}
			// Reading the storer from inside the callback is what callers in
			// this repository actually do, and it must not deadlock.
			err = iter.ForEach(func(ref *plumbing.Reference) error {
				if ref.Type() != plumbing.HashReference {
					return nil
				}
				_, err := stor.EncodedObject(plumbing.AnyObject, ref.Hash())
				return err
			})
			if err != nil {
				t.Errorf("reference walk: %v", err)
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for range 3 {
			if _, err := CompactRepository(context.Background(), stor); err != nil {
				t.Errorf("compact: %v", err)
				return
			}
		}
	}()
	group.Wait()
	if t.Failed() {
		return
	}
	if err := stor.FlushObjects(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	fresh := testPackedStorage(t, fake)
	mu.Lock()
	defer mu.Unlock()
	for _, hash := range written {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost: %v", hash, err)
		}
	}
}

// smallestPartBytes is the smallest part an S3 upload may be made of, and so the
// lowest the threshold for uploading in parts can be set.
const smallestPartBytes = 5 << 20

// writeIncompressibleBlob writes a blob that zlib cannot shrink, so that the
// pack holding it is at least as large. The bytes are a hash chain from seed:
// fixed from run to run, with no structure to compress, and — from different
// seeds — nothing for a delta to share.
func writeIncompressibleBlob(t *testing.T, stor *repository, seed string, size int) plumbing.Hash {
	t.Helper()
	body := make([]byte, 0, size+sha256.Size)
	link := sha256.Sum256([]byte("incompressible " + seed))
	for len(body) < size {
		body = append(body, link[:]...)
		link = sha256.Sum256(link[:])
	}
	return writeBlob(t, stor, string(body[:size]))
}

// largePackedRepository seeds a repository of three packs — the seeded history
// and two incompressible blobs, each flushed as a pack of its own and each
// smaller than one part of a multipart upload — whose merge is larger than one.
func largePackedRepository(t *testing.T, stor *repository) []plumbing.Hash {
	t.Helper()
	hashes := seedObjects(t, stor, 300)
	for i, size := range []int{smallestPartBytes/2 + 1<<20, smallestPartBytes/2 + 2<<20} {
		hashes = append(hashes, writeIncompressibleBlob(t, stor, fmt.Sprint(i), size))
		if err := stor.FlushObjects(); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	return hashes
}

// TestLargePacksAreUploadedInParts covers a monorepo's pack: too large for one
// request, it appears only on multipart-upload completion, so this path must
// publish atomically too.
func TestLargePacksAreUploadedInParts(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	// The threshold is as low as the protocol allows, and the merge of the
	// blobs' packs takes the merged pack over it.
	fake.opts.MultipartBytes = smallestPartBytes
	stor := testPackedStorage(t, fake)
	hashes := largePackedRepository(t, stor)
	want := readObjects(t, stor, hashes)

	before := fake.Snapshot()
	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	counts := fake.Snapshot().Sub(before)
	if counts.Multipart == 0 {
		t.Fatalf("the packfile was not uploaded in parts: %s", counts)
	}
	if result.PackBytes <= smallestPartBytes {
		t.Fatalf("premise: the pack is %d bytes, not large enough to need parts", result.PackBytes)
	}

	body, ok := fake.Get("prefix/" + testRepo + "/objects/pack/" + result.PackName + ".pack")
	if !ok || int64(len(body)) != result.PackBytes {
		t.Fatalf("the assembled pack is %d bytes (stored: %v), want %d", len(body), ok, result.PackBytes)
	}

	fresh := testPackedStorage(t, fake)
	got := readObjects(t, fresh, hashes)
	for hash, want := range want {
		if got[hash] != want {
			t.Fatalf("object %s changed across a multipart publication", hash)
		}
	}
}

// TestAnInterruptedMultipartUploadPublishesNothing pins the multipart commit
// point: a pack whose completion never ran must be invisible, and the packs it
// was built from must stay live.
func TestAnInterruptedMultipartUploadPublishesNothing(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	fake.opts.MultipartBytes = smallestPartBytes
	stor := testPackedStorage(t, fake)
	hashes := largePackedRepository(t, stor)
	packsBefore := packKeys(fake, ".pack")
	liveBefore := livePackNames(t, fake)

	// Failing a part upload leaves the multipart upload incomplete, which is
	// the same state a crashed replica leaves behind.
	// The parts go up side by side, so more than one may be asking at once.
	var failed atomic.Bool
	fake.SetFailOn(func(method, key string) bool {
		return method == "PUT" && strings.HasSuffix(key, ".pack") && failed.CompareAndSwap(false, true)
	})
	_, err := CompactRepository(context.Background(), stor)
	fake.SetFailOn(nil)
	if err == nil {
		t.Fatal("compaction reported success although a part upload failed")
	}
	if packs := packKeys(fake, ".pack"); len(packs) != len(packsBefore) {
		t.Fatalf("an incomplete multipart upload published a pack: %v, was %v", packs, packsBefore)
	}
	if live := livePackNames(t, fake); len(live) != len(liveBefore) {
		t.Fatalf("an interrupted multipart publication changed the live packs: %v, was %v", live, liveBefore)
	}

	retry := testPackedStorage(t, fake)
	if _, err := CompactRepository(context.Background(), retry); err != nil {
		t.Fatalf("retry after an interrupted multipart upload: %v", err)
	}
	after := testPackedStorage(t, fake)
	for _, hash := range hashes {
		if err := after.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost: %v", hash, err)
		}
	}
}

// TestAFullyPackedRepositoryIsNeverListedOnTheReadPath pins the point of naming
// the packs in the manifest. A LIST is priced like a write and returns a page of
// a thousand keys however few are wanted; the engine that discovered packs by
// listing paid one on every cold open. A clone of a repository whose objects are
// all packed — refs, packs, every object — now reads the manifest and the packs
// and lists nothing, on a cold replica and a warm one.
func TestAFullyPackedRepositoryIsNeverListedOnTheReadPath(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = -1
	writer := testPackedStorage(t, fake)
	pack, hashes := pushPack(t, 120)
	if err := packfile.UpdateObjectStorage(writer, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := writer.SetReference(plumbing.NewHashReference(testBranch, hashes[len(hashes)-1])); err != nil {
		t.Fatalf("set: %v", err)
	}
	if loose := looseLayoutKeys(fake); len(loose) != 0 {
		t.Fatalf("premise: the repository holds loose objects: %v", loose)
	}

	// The reader has its own disk, so nothing of the pack is cached on it.
	apart := newFakeS3(t)
	apart.Server = fake.Server
	apart.opts.IndexFreshness = -1
	reader := testPackedStorage(t, apart)
	before := fake.Snapshot()
	for range 2 {
		resolvesTo(t, "a clone", reader, testBranch, hashes[len(hashes)-1])
		if packs, err := reader.StoredPacks(context.Background()); err != nil || len(packs) != 1 {
			t.Fatalf("stored packs: %v, %v", packs, err)
		}
		clonePack(t, reader, hashes)
		for _, hash := range hashes {
			if err := reader.HasEncodedObject(hash); err != nil {
				t.Fatalf("has %s: %v", hash, err)
			}
		}
	}
	spent := fake.Snapshot().Sub(before)
	if spent.List != 0 {
		t.Fatalf("reading a fully packed repository listed the store: %s", spent)
	}
	if spent.Get == 0 || spent.GetRanged == 0 {
		t.Fatalf("premise: the reads did not reach the store: %s", spent)
	}
}

// TestTwoCompactionsRacingLeaveOneMergeAndNoDebris covers what a lock service
// used to prevent and the manifest now arbitrates. Two replicas merge the same
// packs at once: both upload, one commits, and the other's commit finds the
// packs it merged no longer live. It must refuse, and must not leave its upload
// behind as a pack nothing names — nor delete the winner's, which, holding the
// same objects, has the same name.
func TestTwoCompactionsRacingLeaveOneMergeAndNoDebris(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	writer := testPackedStorage(t, fake)
	var all []plumbing.Hash
	for push := range 4 {
		all = append(all, smallPush(t, writer, "small push "+strings.Repeat("p", push)))
	}

	// The second replica compacts while the first is between its upload and its
	// commit: the hook runs when the first's manifest write arrives.
	second := testPackedStorage(t, fake)
	var raced CompactionResult
	var racedErr error
	var once sync.Once
	fake.SetOnRequest(func(method, key string) {
		if method == "PUT" && key == manifestKey {
			once.Do(func() {
				fake.SetOnRequest(nil)
				raced, racedErr = second.Compact(context.Background())
			})
		}
	})
	t.Cleanup(func() { fake.SetOnRequest(nil) })
	first, err := writer.Compact(context.Background())
	if err != nil || racedErr != nil {
		t.Fatalf("compactions: %v, %v", err, racedErr)
	}
	if raced.PackName == "" || first.PackName != "" {
		t.Fatalf("premise: the replica that committed first reports %q and the one that lost %q, want the second to have won inside the first's commit", raced.PackName, first.PackName)
	}

	stored := storedManifest(t, fake)
	if len(stored.Packs) != 1 || stored.Packs[0].Name != raced.PackName || len(stored.Retired) != len(all) {
		t.Fatalf("after the race the manifest holds %d live and %d retired packs, want the one merge and the %d packs it replaced", len(stored.Packs), len(stored.Retired), len(all))
	}
	if packs := packKeys(fake, ".pack"); len(packs) != len(all)+1 {
		t.Fatalf("the store holds %d packs, want the %d retired and the one live: the loser left its upload, or took the winner's", len(packs), len(all))
	}
	reader := testPackedStorage(t, fake)
	for _, hash := range all {
		if _, err := reader.EncodedObject(plumbing.AnyObject, hash); err != nil {
			t.Fatalf("an object was lost to the race: %v", err)
		}
	}
}

// TestAnOrphanThatIsPushedAgainComesBack covers the one way a swept name can be
// wanted: a pack is named by the digest of its contents, so a push that was
// refused and is made again uploads the same name. While the orphan's entry in
// the retired list is young no sweep can be deleting it, and the push takes the
// name back; its objects must then be safe from the sweep for good.
func TestAnOrphanThatIsPushedAgainComesBack(t *testing.T) {
	fake := newFakeS3(t)
	fake.clock = newTestClock()
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 1)
	pack, hashes := pushPack(t, 20)
	tip := hashes[len(hashes)-1]

	refusals, err := pushThrough(t, stor, pack).Commit([]ReferenceUpdate{{Name: "refs/heads/topic", Old: hashOf(7), New: tip}}, false)
	if err != nil || refusals[0] == nil {
		t.Fatalf("premise: the first push was not refused: %v, %v", refusals, err)
	}
	fake.clock.Advance(retiredPackGrace + time.Minute)
	if result, err := stor.Compact(context.Background()); err != nil || len(result.Orphans) != 1 {
		t.Fatalf("premise: the sweep listed %v as orphans (%v)", result.Orphans, err)
	}

	refusals, err = pushThrough(t, stor, pack).Commit([]ReferenceUpdate{{Name: "refs/heads/topic", New: tip}}, false)
	if err != nil || refusals[0] != nil {
		t.Fatalf("the push made again: %v, %v", refusals, err)
	}
	// Live: the seeded pack and the pushed one, taken back from the retired list.
	if stored := storedManifest(t, fake); len(stored.Retired) != 0 || len(stored.Packs) != 2 {
		t.Fatalf("after the push the manifest holds %d live and %d retired packs, want the pack live again beside the seeded one", len(stored.Packs), len(stored.Retired))
	}
	fake.clock.Advance(3 * retiredPackGrace)
	if result, err := stor.Compact(context.Background()); err != nil || len(result.RetiredPacks) != 0 || len(result.Orphans) != 0 {
		t.Fatalf("a sweep long after took the live pack for debris: %+v, %v", result, err)
	}
	readObjects(t, testPackedStorage(t, fake), hashes)

	// Past half its grace period an orphan's entry may be mid-deletion, and the
	// name is refused rather than handed keys that are about to go.
	other, otherHashes := pushPack(t, 21)
	if refusals, err := pushThrough(t, stor, other).Commit([]ReferenceUpdate{{Name: "refs/heads/other", Old: hashOf(7), New: otherHashes[0]}}, false); err != nil || refusals[0] == nil {
		t.Fatalf("premise: %v, %v", refusals, err)
	}
	fake.clock.Advance(retiredPackGrace + time.Minute)
	if result, err := stor.Compact(context.Background()); err != nil || len(result.Orphans) != 1 {
		t.Fatalf("premise: the sweep listed %v as orphans (%v)", result.Orphans, err)
	}
	fake.clock.Advance(retiredPackGrace/2 + time.Minute)
	if _, err := pushThrough(t, stor, other).Commit([]ReferenceUpdate{{Name: "refs/heads/other", New: otherHashes[len(otherHashes)-1]}}, false); !errors.Is(err, errPackBeingSwept) {
		t.Fatalf("a push of a name that may be mid-deletion answered %v, want it refused", err)
	}
}

// TestASweepRemovesOnlySnapshotsNoManifestNames covers the reference snapshots a
// fold leaves behind. The one the manifest names must never go; the ones it has
// replaced go once they have lain a grace period, and no sooner, since a reader
// that read the manifest a moment before the fold is about to fetch one.
func TestASweepRemovesOnlySnapshotsNoManifestNames(t *testing.T) {
	fake := newFakeS3(t)
	fake.clock = newTestClock()
	stor := testPackedStorage(t, fake)
	fold := func(round int) string {
		t.Helper()
		updates := make([]ReferenceUpdate, 0, refChangeBound+1)
		for i := range refChangeBound + 1 {
			name := plumbing.NewTagReferenceName(fmt.Sprintf("round-%d/v%d", round, i))
			updates = append(updates, ReferenceUpdate{Name: name, New: hashOf(i)})
		}
		push, err := stor.BeginPush()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := push.Commit(updates, true); err != nil {
			t.Fatalf("fold %d: %v", round, err)
		}
		return storedManifest(t, fake).Refs.Snapshot
	}
	first := fold(1)
	second := fold(2)
	snapshots := "prefix/" + testRepo + "/" + refSnapshotDirectory
	if first == second || len(fake.KeysWithPrefix(snapshots)) != 2 {
		t.Fatalf("premise: two folds left the snapshots %v", fake.KeysWithPrefix(snapshots))
	}

	if result, err := stor.Compact(context.Background()); err != nil || result.SweptSnapshots != 0 {
		t.Fatalf("a sweep at once removed %d snapshots (%v)", result.SweptSnapshots, err)
	}
	fake.clock.Advance(retiredPackGrace + time.Minute)
	if result, err := stor.Compact(context.Background()); err != nil || result.SweptSnapshots != 1 {
		t.Fatalf("a sweep after a grace period removed %d snapshots (%v), want the replaced one", result.SweptSnapshots, err)
	}
	if left := fake.KeysWithPrefix(snapshots); len(left) != 1 || left[0] != "prefix/"+testRepo+"/"+second {
		t.Fatalf("the sweep left %v, want the snapshot the manifest names", left)
	}
	if refs := advertise(t, testPackedStorage(t, fake)); len(refs) != 2*(refChangeBound+1) {
		t.Fatalf("after the sweep the repository advertises %d references", len(refs))
	}
}
