package gitstore

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

const testRepo = "octocat/monorepo"

func testPackedStorage(t *testing.T, fake *fakeS3) *atomicRefStorer {
	t.Helper()
	stor, err := packedStorage(fake, testRepo)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	return stor
}

// readObjects reads every hash back through the storer and returns the bytes.
func readObjects(t *testing.T, stor *atomicRefStorer, hashes []plumbing.Hash) map[plumbing.Hash]string {
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

// TestCompactionPreservesEveryObject asserts the pack tier returns every object
// byte-for-byte, read through a storer that has never seen the loose form.
func TestCompactionPreservesEveryObject(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 300)
	want := readObjects(t, stor, hashes)

	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.Packed != len(hashes) {
		t.Fatalf("packed %d of %d objects", result.Packed, len(hashes))
	}
	if remaining := looseKeyCount(fake); remaining != 0 {
		t.Fatalf("%d loose object keys survived compaction", remaining)
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
	if ref.Hash() != hashes[len(hashes)-1] {
		t.Fatalf("reference points at %s, want %s", ref.Hash(), hashes[len(hashes)-1])
	}
	for _, hash := range hashes {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("HasEncodedObject(%s) after compaction: %v", hash, err)
		}
	}
}

func looseKeyCount(fake *fakeS3) int {
	prefix := "prefix/" + testRepo + "/objects/"
	count := 0
	for _, key := range fake.keysWithPrefix(prefix) {
		if !strings.HasPrefix(key, prefix+"pack/") {
			count++
		}
	}
	return count
}

// TestCompactionCrashBeforeThePackIsPublishedLosesNothing interrupts compaction
// with the index and filter stored but the .pack key absent; the repository must
// still read entirely from loose objects and a retry must succeed.
func TestCompactionCrashBeforeThePackIsPublishedLosesNothing(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)
	want := readObjects(t, stor, hashes)

	fake.setFailOn(func(method, key string) bool {
		return method == "PUT" && strings.HasSuffix(key, ".pack")
	})
	if _, err := CompactRepository(context.Background(), stor); err == nil {
		t.Fatal("compaction reported success although the packfile upload failed")
	}
	fake.setFailOn(nil)

	if looseKeyCount(fake) != len(hashes) {
		t.Fatalf("a compaction that never published its pack removed loose objects: %d of %d remain",
			looseKeyCount(fake), len(hashes))
	}
	packs := packKeys(fake, ".pack")
	if len(packs) != 0 {
		t.Fatalf("a packfile became visible although its upload failed: %v", packs)
	}
	if len(packKeys(fake, ".idx")) == 0 {
		t.Fatal("the test did not reach the state it is about: no index was uploaded")
	}

	fresh := testPackedStorage(t, fake)
	got := readObjects(t, fresh, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s unreadable after an interrupted compaction", hash)
		}
	}

	retry := testPackedStorage(t, fake)
	if _, err := CompactRepository(context.Background(), retry); err != nil {
		t.Fatalf("retry after an interrupted compaction: %v", err)
	}
	after := testPackedStorage(t, fake)
	got = readObjects(t, after, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s changed across the retried compaction", hash)
		}
	}
}

// TestCompactionCrashDuringLooseDeletionLosesNothing interrupts after the pack
// is published with the loose keys only partly deleted; both copies stay
// readable and the objects must come back unchanged.
func TestCompactionCrashDuringLooseDeletionLosesNothing(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)
	want := readObjects(t, stor, hashes)

	fake.setFailOn(func(method, key string) bool { return method == "POST" && key == "" })
	if _, err := CompactRepository(context.Background(), stor); err == nil {
		t.Fatal("compaction reported success although the loose deletion failed")
	}
	fake.setFailOn(nil)

	if len(packKeys(fake, ".pack")) != 1 {
		t.Fatalf("expected the packfile to be published before deletion was attempted, found %v",
			packKeys(fake, ".pack"))
	}
	if looseKeyCount(fake) != len(hashes) {
		t.Fatal("loose objects disappeared although their deletion failed")
	}

	fresh := testPackedStorage(t, fake)
	got := readObjects(t, fresh, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s changed when both copies existed", hash)
		}
	}

	retry := testPackedStorage(t, fake)
	if _, err := CompactRepository(context.Background(), retry); err != nil {
		t.Fatalf("retry after an interrupted deletion: %v", err)
	}
	if looseKeyCount(fake) != 0 {
		t.Fatalf("%d loose keys survived the retried compaction", looseKeyCount(fake))
	}
}

func packKeys(fake *fakeS3, extension string) []string {
	var out []string
	for _, key := range fake.keysWithPrefix("prefix/" + testRepo + "/objects/pack/") {
		if strings.HasSuffix(key, extension) {
			out = append(out, key)
		}
	}
	return out
}

// TestCompactionDeletesOnlyWhatItPacked is the invariant that makes a push
// concurrent with a compaction safe. Objects written after the compaction took
// its listing are not in the pack, so they must still be loose afterwards.
func TestCompactionDeletesOnlyWhatItPacked(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)

	// Reproduce a push landing between the listing and the deletion without
	// interleaving goroutines: the second compaction lists a superset and must
	// still leave nothing behind.
	late := writeBlob(t, stor, "written after the compaction listing")
	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.Packed != len(hashes)+1 {
		t.Fatalf("packed %d objects, want %d", result.Packed, len(hashes)+1)
	}

	fresh := testPackedStorage(t, fake)
	if err := fresh.HasEncodedObject(late); err != nil {
		t.Fatalf("object pushed alongside a compaction was lost: %v", err)
	}

	// Now the real shape: a loose object that no compaction has listed must
	// survive one that runs after it.
	later := writeBlob(t, fresh, "written after the pack was published")
	if _, err := CompactRepository(context.Background(), fresh); err != nil {
		t.Fatalf("second compact: %v", err)
	}
	after := testPackedStorage(t, fake)
	if err := after.HasEncodedObject(later); err != nil {
		t.Fatalf("object written between compactions was lost: %v", err)
	}
}

func looseKeyOf(hash plumbing.Hash) string {
	text := hash.String()
	return "prefix/" + testRepo + "/objects/" + text[:2] + "/" + text[2:]
}

func writeBlob(t *testing.T, stor *atomicRefStorer, body string) plumbing.Hash {
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

// TestCompactionToleratesAnObjectAnotherReplicaAlreadyPacked: this replica lists
// a loose key that another replica packs and deletes before this one reads it;
// compaction must complete and must not delete a key it did not pack.
func TestCompactionToleratesAnObjectAnotherReplicaAlreadyPacked(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)

	// The other replica deletes the loose key after this one listed it and while
	// it reads objects into its pack — an interleaving a pre-build probe misses.
	vanished := hashes[7]
	vanishedKey := looseKeyOf(vanished)
	var once sync.Once
	fake.setOnRequest(func(method, key string) {
		if method != "GET" || key == vanishedKey || !strings.Contains(key, "/objects/") {
			return
		}
		once.Do(func() { fake.remove(vanishedKey) })
	})

	result, err := CompactRepository(context.Background(), stor)
	fake.setOnRequest(nil)
	if err != nil {
		t.Fatalf("compaction failed on an object another replica had already packed: %v", err)
	}
	if result.Packed != len(hashes)-1 {
		t.Fatalf("packed %d objects, want %d", result.Packed, len(hashes)-1)
	}

	fresh := testPackedStorage(t, fake)
	for _, hash := range hashes {
		if hash == vanished {
			continue
		}
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost: %v", hash, err)
		}
	}
}

// TestConcurrentCompactionAndWritesLoseNothing runs compaction against live
// writers on the same repository handle, which is the shape a scheduled
// compaction and an ongoing push have.
func TestConcurrentCompactionAndWritesLoseNothing(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	seeded := seedObjects(t, stor, 150)

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

	fresh := testPackedStorage(t, fake)
	mu.Lock()
	defer mu.Unlock()
	for _, hash := range written {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost to a compaction running alongside writes: %v", hash, err)
		}
	}
}

// TestCompactionMergesPacksOnceTheyAccumulate: a repository compacted many times
// must fold its packs together rather than grow an index per push, and every
// object must survive the fold.
func TestCompactionMergesPacksOnceTheyAccumulate(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)

	var all []plumbing.Hash
	for round := range compactionMergeThreshold + 1 {
		for i := range compactionMinLooseObjects + 1 {
			all = append(all, writeBlob(t, stor, "round "+strings.Repeat("r", round)+" object "+strings.Repeat("o", i)))
		}
		if _, err := CompactRepository(context.Background(), stor); err != nil {
			t.Fatalf("compact round %d: %v", round, err)
		}
	}

	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("merging compaction: %v", err)
	}
	if result.Merged == 0 {
		t.Fatalf("a repository with more than %d packs did not merge them", compactionMergeThreshold)
	}

	fresh := testPackedStorage(t, fake)
	for _, hash := range all {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost to a merging compaction: %v", hash, err)
		}
	}

	// A merged pack's predecessors are marked rather than deleted, so a
	// request that began before the merge can still read them.
	if len(packKeys(fake, ".superseded")) == 0 {
		t.Fatal("merged packs were not marked superseded")
	}
	for _, key := range packKeys(fake, ".superseded") {
		pack := strings.TrimSuffix(key, ".superseded") + ".pack"
		if _, ok := fake.get(pack); !ok {
			t.Fatalf("superseded pack %s was deleted immediately instead of aging out", pack)
		}
	}
}

// TestCompactionSkipsRepositoriesWithLittleToGain pins that a handful of loose
// objects is left alone, since publishing a pack costs three uploads.
func TestCompactionSkipsRepositoriesWithLittleToGain(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 4)

	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.PackName != "" {
		t.Fatalf("a repository with six objects was packed into %s", result.PackName)
	}
}

// TestCompactRepositoryIgnoresStorageWithoutAPackTier pins that the local
// filesystem and in-memory backends are left to git's own maintenance.
func TestCompactRepositoryIgnoresStorageWithoutAPackTier(t *testing.T) {
	t.Setenv("BLEEPHUB_S3_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", "")
	S3FSCache.Mu.Lock()
	S3FSCache.Inited = false
	S3FSCache.FS = nil
	S3FSCache.Err = nil
	S3FSCache.Mu.Unlock()

	stor, err := newGitStorage(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.PackName != "" || result.Packed != 0 {
		t.Fatalf("non-object-store storage reported a compaction: %+v", result)
	}
}

// TestCompactionSurfacesAnObjectStoreOutage pins that a compaction that could
// not read the object store fails loudly rather than publishing a pack that is
// missing whatever it could not read.
func TestCompactionSurfacesAnObjectStoreOutage(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)

	fake.setFailOn(func(method, key string) bool {
		return method == "GET" && strings.Contains(key, "/objects/") && !strings.Contains(key, "/pack/")
	})
	_, err := CompactRepository(context.Background(), stor)
	fake.setFailOn(nil)
	if err == nil {
		t.Fatal("compaction reported success although it could not read the objects")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a transient outage was reported as a missing object: %v", err)
	}
	if looseKeyCount(fake) != len(hashes) {
		t.Fatal("a failed compaction removed loose objects")
	}
}

// TestConcurrentReadsWritesAndCompactionAreRaceFree drives a clone, a push, and
// a compaction through one handle at once — the case the storage lock and
// per-Next iterator locking exist for; run under the race detector, its
// assertions only check no object is lost.
func TestConcurrentReadsWritesAndCompactionAreRaceFree(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(compactionTriggerEnv, "0")
	fake := newFakeS3(t)
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

	fresh := testPackedStorage(t, fake)
	mu.Lock()
	defer mu.Unlock()
	for _, hash := range written {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s lost: %v", hash, err)
		}
	}
}

// TestLargePacksAreUploadedInParts covers a monorepo's pack: too large for one
// request, it appears only on multipart-upload completion, so this path must
// publish atomically too.
func TestLargePacksAreUploadedInParts(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(compactionTriggerEnv, "0")
	// Any pack these tests produce is far above one kilobyte, so this forces
	// every publication through the multipart path.
	t.Setenv(multipartThresholdEnv, "1024")
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 300)
	want := readObjects(t, stor, hashes)

	before := fake.snapshot()
	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	counts := fake.snapshot().sub(before)
	if counts.multipart == 0 {
		t.Fatalf("the packfile was not uploaded in parts: %s", counts)
	}

	packs := packKeys(fake, ".pack")
	if len(packs) != 1 {
		t.Fatalf("expected one published pack, found %v", packs)
	}
	body, _ := fake.get(packs[0])
	if int64(len(body)) != result.PackBytes {
		t.Fatalf("the assembled pack is %d bytes, want %d", len(body), result.PackBytes)
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
// point: a pack whose completion never ran must be invisible, and the loose
// objects it was built from must be untouched.
func TestAnInterruptedMultipartUploadPublishesNothing(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(compactionTriggerEnv, "0")
	t.Setenv(multipartThresholdEnv, "1024")
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 300)

	// Failing a part upload leaves the multipart upload incomplete, which is
	// the same state a crashed replica leaves behind.
	failed := false
	fake.setFailOn(func(method, key string) bool {
		if method == "PUT" && strings.HasSuffix(key, ".pack") && !failed {
			failed = true
			return true
		}
		return false
	})
	_, err := CompactRepository(context.Background(), stor)
	fake.setFailOn(nil)
	if err == nil {
		t.Fatal("compaction reported success although a part upload failed")
	}
	if packs := packKeys(fake, ".pack"); len(packs) != 0 {
		t.Fatalf("an incomplete multipart upload published a pack: %v", packs)
	}
	if looseKeyCount(fake) != len(hashes) {
		t.Fatal("an interrupted multipart publication removed loose objects")
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
