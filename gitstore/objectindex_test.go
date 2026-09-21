package gitstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"strings"
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
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
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
	fake.Reset()
	for i := 1; i <= probes; i++ {
		if err := fresh.HasEncodedObject(absentHash(i)); err == nil {
			t.Fatalf("absent object %d was reported present", i)
		}
	}
	counts := fake.Snapshot()
	// The residual cost is the filters' false positive rate, not the number of
	// questions: a probe the pack filter cannot rule out falls through to the
	// exact index, which is the whole point of a negative-only filter. At about
	// one in 256 that is a couple of dozen lookups for five thousand questions,
	// against five thousand round trips before.
	if counts.Total() > probes/50 {
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
// pack with no filter beside it is recorded, and the loose filters are set
// saturated, which is what happens when a directory outgrows the table it was
// built for. Every read must then return exactly what it returned before. A
// filter is allowed to be useless; it is not allowed to be wrong, and the only
// way it could be wrong is by answering "absent", which saturation makes
// impossible.
func TestSaturatedFilterCannotHideAnObject(t *testing.T) {
	fake := newFakeS3(t)
	// A listing would quietly repair the saturation part way through the reads
	// that follow; with the bound this wide, and a clock that does not move,
	// none is taken.
	fake.opts.IndexFreshness = time.Hour
	fake.clock = newTestClock()
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
	// Take the snapshot, then publish one in which every filter is saturated.
	if err := saturated.HasEncodedObject(absentHash(1)); err == nil {
		t.Fatal("an object that was never written was reported present")
	}
	taken := saturated.tiers.current.Load()
	if len(taken.packs) == 0 {
		t.Fatal("the test did not reach the state it is about: the snapshot holds no pack")
	}
	useless := &tierSnapshot{at: taken.at}
	for _, pack := range taken.packs {
		if pack.filter.Load() == nil {
			t.Fatal("the test did not reach the state it is about: the probe did not read the pack's filter")
		}
		useless.packs = append(useless.packs, &storedPack{name: pack.name, pack: pack.pack, index: pack.index})
	}
	for fanout := range useless.loose {
		useless.loose[fanout] = &cuckooFilter{saturated: true}
	}
	saturated.tiers.current.Store(useless)

	// A filter that matches everything must not change a single answer.
	for _, key := range absentOIDs(64) {
		var hash plumbing.Hash
		copy(hash[:], key[:20])
		if !useless.looseMayHold(hash) {
			t.Fatal("a saturated loose filter gave a negative answer")
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
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Minute
	fake.clock = newTestClock()

	writer := testPackedStorage(t, fake)
	reader := testPackedStorage(t, fake)
	seedObjects(t, writer, 100)

	hash := writeBlob(t, writer, "written by another replica")
	// The reader has no snapshot yet, so the first answer is taken from a
	// listing it makes itself.
	if err := reader.HasEncodedObject(hash); err != nil {
		t.Fatalf("an object present in the object store was reported absent: %v", err)
	}

	later := writeBlob(t, writer, "written after the reader took its snapshot")
	pushed := smallPush(t, writer, "pushed after the reader took its snapshot")
	// Inside the bound the snapshot answers by itself: that is the bound's
	// meaning, and what keeps a negotiation from listing for every question.
	before := fake.Snapshot()
	for _, absent := range []plumbing.Hash{later, pushed} {
		if err := reader.HasEncodedObject(absent); !errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Fatalf("premise: inside the bound the reader answered %v for an object it has not listed", err)
		}
	}
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("a negative answer inside the bound cost %s", spent)
	}

	fake.clock.Advance(2 * time.Minute)
	for _, present := range []plumbing.Hash{later, pushed} {
		if err := reader.HasEncodedObject(present); err != nil {
			t.Fatalf("an object written by another replica never became visible: %v", err)
		}
	}
	if got := readObjects(t, reader, []plumbing.Hash{pushed})[pushed]; !strings.HasPrefix(got, "pushed after") {
		t.Fatalf("the pack another replica pushed read back %q", got)
	}
}

// TestLooseObjectIndexTracksThisProcessesOwnWrites pins the part of the
// staleness argument that does not depend on the freshness window at all: a
// writer is never stale about itself, because every write and every deletion
// updates the index as it happens.
func TestLooseObjectIndexTracksThisProcessesOwnWrites(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = 24 * time.Hour
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

// TestWritingALooseObjectIsOnePut pins what an object written through the API
// costs. git probes for the object, writes it under a temporary name and renames
// it: against a bucket a HEAD, a PUT, a COPY and a DELETE. The key is the hash
// of the bytes and a PUT is atomic, so it is one PUT, and the object is
// readable through the writing handle at once without the store being asked.
func TestWritingALooseObjectIsOnePut(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
	stor := testPackedStorage(t, fake)
	writeBlob(t, stor, "the write that takes the handle's first snapshot")
	if err := stor.HasEncodedObject(absentHash(11)); err == nil {
		t.Fatal("an absent object was reported present")
	}

	before := fake.Snapshot()
	hash := writeBlob(t, stor, "the write that is measured")
	spent := fake.Snapshot().Sub(before)
	if spent.Put != 1 || spent.Total() != 1 {
		t.Fatalf("writing one object cost %s, want exactly one PUT", spent)
	}
	if keys := fake.KeysWithPrefix("prefix/" + testRepo + "/objects/"); len(keys) != 2 {
		t.Fatalf("two writes left %v, want the two objects under their final names and nothing else", keys)
	}
	if _, ok := fake.Get(looseKeyOf(hash)); !ok {
		t.Fatal("the object is not under the key git would look for it at")
	}

	// Writing what is already there stores the same bytes under the same key.
	before = fake.Snapshot()
	if again := writeBlob(t, stor, "the write that is measured"); again != hash {
		t.Fatalf("the same content hashed to %s and then %s", hash, again)
	}
	if spent := fake.Snapshot().Sub(before); spent.Put != 1 || spent.Total() != 1 {
		t.Fatalf("rewriting an object cost %s, want one PUT and no probe", spent)
	}
}

// TestTheZeroHashIsAnsweredWithoutARequest pins the one question whose answer
// needs no store: no object hashes to zero, and go-git asks after it.
func TestTheZeroHashIsAnsweredWithoutARequest(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	if err := stor.HasEncodedObject(plumbing.ZeroHash); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("the zero hash: %v", err)
	}
	if _, err := stor.EncodedObject(plumbing.AnyObject, plumbing.ZeroHash); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("the zero hash: %v", err)
	}
	if spent := fake.Snapshot(); spent.Total() != 0 {
		t.Fatalf("asking after the zero hash cost %s", spent)
	}
}

// TestAFilterFalsePositiveIsStillNotFound pins the far side of the filter
// invariant. A filter's "maybe" sends the caller to the store, and when the
// store says no the answer must be the plain "not found" every caller of go-git
// compares against — not some other error, which a fetch would report as a
// failure, and not after listing the repository over and over. The object asked
// after shares its loose filter's every input with one that exists, so the
// filter is certain to say "maybe".
func TestAFilterFalsePositiveIsStillNotFound(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
	fake.clock = newTestClock()
	stor := testPackedStorage(t, fake)
	present := writeBlob(t, stor, "the object the impostor is mistaken for")
	impostor := present
	impostor[len(impostor)-1] ^= 0xff
	if snapshot := stor.tiers.current.Load(); snapshot == nil || !snapshot.looseMayHold(impostor) {
		// The handle takes its first snapshot on its first read.
		if err := stor.HasEncodedObject(present); err != nil {
			t.Fatalf("premise: %v", err)
		}
	}
	if !stor.tiers.current.Load().looseMayHold(impostor) {
		t.Fatal("premise: the loose filter rules the impostor out, so no false positive is exercised")
	}

	before := fake.Snapshot()
	if err := stor.HasEncodedObject(impostor); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a probe the filter could not rule out: %v, want ErrObjectNotFound", err)
	}
	if _, err := stor.EncodedObject(plumbing.AnyObject, impostor); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a read the filter could not rule out: %v, want ErrObjectNotFound", err)
	}
	if _, err := stor.EncodedObjectSize(impostor); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a size the filter could not rule out: %v, want ErrObjectNotFound", err)
	}
	// Each question asks the store about the key twice — once on the snapshot's
	// word, once on a new listing's — and lists once.
	if spent := fake.Snapshot().Sub(before); spent.List != 3 || spent.Total() != 9 {
		t.Fatalf("three false positives cost %s, want a listing and two lookups each", spent)
	}
}
