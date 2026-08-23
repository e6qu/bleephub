package gitstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// absentHash is an object id no repository in these tests contains.
func absentHash(n int) plumbing.Hash {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(n)^0xa5a5a5a5)
	digest := sha256.Sum256(buf[:])
	var hash plumbing.Hash
	copy(hash[:], digest[:20])
	return hash
}

// TestMembershipIndexAnswersAbsenceWithoutARoundTrip is the win the filters
// exist for. A fetch negotiation asks whether the repository has an object it
// does not have, and that used to be an S3 GET that returned a 404 for every
// question.
func TestMembershipIndexAnswersAbsenceWithoutARoundTrip(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(objectIndexFreshnessEnv, "1h")
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)
	if _, err := CompactRepository(context.Background(), stor); err != nil {
		t.Fatalf("compact: %v", err)
	}

	fresh := testPackedStorage(t, fake)
	// Warm the index once, which is the listing every negative answer is
	// backed by, then measure the questions themselves.
	if err := fresh.HasEncodedObject(absentHash(0)); err == nil {
		t.Fatal("an object that was never written was reported present")
	}

	const probes = 5000
	fake.reset()
	for i := 1; i <= probes; i++ {
		if err := fresh.HasEncodedObject(absentHash(i)); err == nil {
			t.Fatalf("absent object %d was reported present", i)
		}
	}
	counts := fake.snapshot()
	// The residual cost is the filters' false positive rate, not the number of
	// questions: a probe the pack filter cannot rule out falls through to the
	// exact index, which is the whole point of a negative-only filter. At about
	// one in 256 that is a couple of dozen lookups for five thousand questions,
	// against five thousand round trips before.
	if counts.total() > probes/50 {
		t.Fatalf("%d negative answers cost %s, want far fewer than one request each", probes, counts)
	}
	t.Logf("%d negative answers cost %s", probes, counts)

	// And the objects that are present must still be found, so the fast path
	// has not simply started answering "no" to everything.
	for _, hash := range hashes {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s reported absent: %v", hash, err)
		}
	}
}

// TestSaturatedFilterCannotHideAnObject is the filter invariant under test.
//
// Both membership structures are driven into the state where they answer
// "present" to every possible key: the pack filters are dropped, which is how a
// pack whose filter cannot be read is recorded, and the loose filters are set
// saturated, which is what happens when a directory outgrows the table it was
// built for. Every read must then return exactly what it returned before. A
// filter is allowed to be useless; it is not allowed to be wrong, and the only
// way it could be wrong is by answering "absent", which saturation makes
// impossible.
func TestSaturatedFilterCannotHideAnObject(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)
	if _, err := CompactRepository(context.Background(), stor); err != nil {
		t.Fatalf("compact: %v", err)
	}
	// Leave some objects in the loose tier as well, so both structures are on
	// the path being tested.
	loose := make([]plumbing.Hash, 0, 8)
	for i := range 8 {
		loose = append(loose, writeBlob(t, stor, "loose object "+string(rune('a'+i))))
	}
	hashes = append(hashes, loose...)

	baseline := testPackedStorage(t, fake)
	want := readObjects(t, baseline, hashes)

	saturated := testPackedStorage(t, fake)
	// Populate the index, then saturate everything it holds.
	if err := saturated.HasEncodedObject(absentHash(1)); err == nil {
		t.Fatal("an object that was never written was reported present")
	}
	index := saturated.fs.repoIndexFor()
	index.mu.Lock()
	if len(index.packs) == 0 {
		index.mu.Unlock()
		t.Fatal("the test did not reach the state it is about: no pack filter was loaded")
	}
	for name := range index.packs {
		index.packs[name] = nil
	}
	for _, snapshot := range index.fanouts {
		snapshot.filter.saturated = true
	}
	// The freshness window must not quietly repair the saturation part way
	// through the reads that follow: the snapshots were all taken a moment ago
	// by the probe above, so widening the window keeps them fresh throughout.
	index.freshness = time.Hour
	index.mu.Unlock()

	// A filter that matches everything must not change a single answer.
	for _, key := range absentOIDs(64) {
		var hash plumbing.Hash
		copy(hash[:], key[:20])
		if !index.maybePresent(saturated.fs, oidKeyFrom(hash[:])) {
			t.Fatal("a saturated index gave a negative answer")
		}
	}
	got := readObjects(t, saturated, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s read differently through a saturated filter", hash)
		}
	}
	for _, hash := range hashes {
		if err := saturated.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s reported absent through a saturated filter: %v", hash, err)
		}
	}
	// The exact index behind the saturated filter still has to say no.
	if err := saturated.HasEncodedObject(absentHash(9999)); err == nil {
		t.Fatal("a saturated filter turned an absent object into a present one")
	}
}

// TestNegativeAnswersAreRefreshedFromTheObjectStore pins the staleness bound.
// An object another replica writes must become visible once the snapshot the
// negative answer is drawn from has aged past the freshness window.
func TestNegativeAnswersAreRefreshedFromTheObjectStore(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(objectIndexFreshnessEnv, "1ms")
	fake := newFakeS3(t)

	writer := testPackedStorage(t, fake)
	reader := testPackedStorage(t, fake)
	seedObjects(t, writer, 100)

	hash := writeBlob(t, writer, "written by another replica")
	// The reader's index has never been built, so the first answer is taken
	// from a listing it makes itself.
	if err := reader.HasEncodedObject(hash); err != nil {
		t.Fatalf("an object present in the object store was reported absent: %v", err)
	}

	later := writeBlob(t, writer, "written after the reader took its snapshot")
	visible := false
	for range 2000 {
		if reader.HasEncodedObject(later) == nil {
			visible = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !visible {
		t.Fatal("an object written by another replica never became visible")
	}
}

// TestLooseObjectIndexTracksThisProcessesOwnWrites pins the part of the
// staleness argument that does not depend on the freshness window at all: a
// writer is never stale about itself, because every write and every deletion
// updates the index as it happens.
func TestLooseObjectIndexTracksThisProcessesOwnWrites(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(objectIndexFreshnessEnv, "24h")
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 100)

	// Build the snapshot, then write through the same handle. With a day-long
	// freshness window nothing will re-list, so only the write path's own
	// bookkeeping can make the object visible.
	if err := stor.HasEncodedObject(absentHash(3)); err == nil {
		t.Fatal("an object that was never written was reported present")
	}
	hash := writeBlob(t, stor, "written after the snapshot was taken")
	if err := stor.HasEncodedObject(hash); err != nil {
		t.Fatalf("this process's own write was reported absent: %v", err)
	}
	obj, err := stor.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		t.Fatalf("this process's own write was unreadable: %v", err)
	}
	reader, err := obj.Reader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = reader.Close()
	if string(body) != "written after the snapshot was taken" {
		t.Fatalf("read back %q", body)
	}
}

// TestLooseObjectPathRecognisesOnlyObjectPaths keeps the fast path off
// everything that is not a loose object: a ref, the config, the pack directory.
// Answering "absent" for one of those from a membership filter would be a
// category error.
func TestLooseObjectPathRecognisesOnlyObjectPaths(t *testing.T) {
	valid := "objects/ab/c0ffee0123456789abcdef0123456789abcdef"
	if _, _, ok := looseObjectPath(valid); !ok {
		t.Fatalf("%q was not recognised as a loose object path", valid)
	}
	for _, name := range []string{
		"config",
		"HEAD",
		"refs/heads/main",
		"objects/pack/pack-0123456789abcdef0123456789abcdef01234567.pack",
		"objects/info/packs",
		"objects/ab",
		"objects/ab/not-hexadecimal-at-all-not-hexadecimal",
		"objects/abc/0123456789abcdef0123456789abcdef012345",
	} {
		if _, _, ok := looseObjectPath(name); ok {
			t.Fatalf("%q was treated as a loose object path", name)
		}
	}
}

// TestMembershipStructureSizePerMillionObjects records the resident cost of the
// filters, which is the budget that decides how many repositories a replica can
// keep answers for.
func TestMembershipStructureSizePerMillionObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement run")
	}
	const million = 1000000
	keys := deterministicOIDs(million)

	fuse, err := newBinaryFuseFilter(keys)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	fuseBytes := fuse.bits() / 8

	// The loose tier spreads over 256 fanout directories, so the comparable
	// figure is 256 cuckoo filters each holding a 256th of the objects.
	cuckooBytes := 0
	for range 256 {
		cuckooBytes += cuckooResidentBits(newCuckooFilter(million/256)) / 8
	}

	// What the filters replace: go-git's in-memory pack index holds a 20-byte
	// object id, a 4-byte CRC and a 4-byte offset for every object.
	exactIndexBytes := million * (20 + 4 + 4)

	t.Logf("per million objects: binary fuse (packs) %d bytes (%.2f bits/key), "+
		"cuckoo (loose, 256 fanouts) %d bytes (%.2f bits/key), exact pack index for comparison %d bytes",
		fuseBytes, float64(fuse.bits())/million,
		cuckooBytes, float64(cuckooBytes*8)/million,
		exactIndexBytes)

	if fuseBytes > 1_400_000 {
		t.Fatalf("binary fuse filter for a million objects is %d bytes, want under 1.4 MB", fuseBytes)
	}
}
