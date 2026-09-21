package gitstore

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
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

	fresh := testPackedStorage(t, fake)
	// Warm the handle once — the manifest, and the filter every negative answer
	// is drawn from — then measure the questions themselves.
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

// TestAPackWithoutAFilterCannotHideAnObject is the filter invariant under
// test. A pack's filter is allowed to be useless; it is not allowed to be
// wrong, and a pack whose sidecar carries no filter at all — the most useless
// filter there is, which a manifest records as a filter of zero bytes — must
// answer every read and every probe exactly as a filtered one does, the index
// answering what the filter would have.
func TestAPackWithoutAFilterCannotHideAnObject(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 200)
	hashes = append(hashes, flushedBlobs(t, stor, "second", 20)...)
	want := readObjects(t, testPackedStorage(t, fake), hashes)

	// Rewrite every pack's sidecar without its filter, and the manifest to say so.
	stored := storedManifest(t, fake)
	for at, pack := range stored.Packs {
		key := "prefix/" + testRepo + "/objects/pack/" + pack.Name + sidecarSuffix
		sidecar, ok := fake.Get(key)
		if !ok || pack.FilterBytes == 0 {
			t.Fatalf("premise: pack %s has sidecar %v and a filter of %d bytes", pack.Name, ok, pack.FilterBytes)
		}
		stripped := encodeSidecar(sidecar[:pack.IndexBytes], nil, plumbing.NewHash(strings.TrimPrefix(pack.Name, "pack-")))
		fake.Put(key, stripped)
		stored.Packs[at].FilterBytes, stored.Packs[at].SidecarBytes = 0, int64(len(stripped))
	}
	stored.Sequence++
	encoded, err := stored.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fake.Put(manifestKey, encoded)

	// A replica with a disk of its own, so that nothing of the old sidecars is
	// read from the cache the writer seeded.
	apart := newFakeS3(t)
	apart.Server = fake.Server
	unfiltered := testPackedStorage(t, apart)
	for _, hash := range hashes {
		if err := unfiltered.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s reported absent by a pack without a filter: %v", hash, err)
		}
	}
	// The exact index behind the missing filter still has to say no.
	if err := unfiltered.HasEncodedObject(absentHash(9999)); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("an absent object probed through a pack without a filter: %v", err)
	}
	state := unfiltered.manifests.current.Load()
	if len(state.packs) != 2 {
		t.Fatalf("premise: the handle holds %d packs, want 2", len(state.packs))
	}
	for _, pack := range state.packs {
		if pack.filterBytes != 0 || pack.filter.Load() != nil || pack.parsed.Load() == nil {
			t.Fatalf("premise: pack %s was probed through a filter (%d bytes, loaded %v) rather than its index", pack.name, pack.filterBytes, pack.filter.Load() != nil)
		}
	}
	got := readObjects(t, unfiltered, hashes)
	for hash, body := range want {
		if got[hash] != body {
			t.Fatalf("object %s read differently from a pack without a filter", hash)
		}
	}
}

// TestAProbeReadsThePacksFilterAndNotItsIndex pins what the sidecar's layout is
// for. A fetch negotiation asks mostly about objects the repository does not
// have, and a cold replica must answer those from the filter — about a byte an
// object — without reading the index at thirty bytes an object: the filter is
// addressed by its own range of the sidecar, so a probe downloads it and none
// of the index before it.
func TestAProbeReadsThePacksFilterAndNotItsIndex(t *testing.T) {
	fake := newFakeS3(t)
	// Extents small enough that the index and the filter are many apart.
	fake.opts.ChunkBytes = 4096
	writer := testPackedStorage(t, fake)
	hashes := seedObjects(t, writer, 3000)
	stored := storedManifest(t, fake).Packs
	if len(stored) != 1 || stored[0].IndexBytes < 20*int64(fake.opts.ChunkBytes) {
		t.Fatalf("premise: the repository's packs are %+v, want one whose index spans many extents", stored)
	}

	apart := newFakeS3(t)
	apart.Server = fake.Server
	apart.opts.ChunkBytes = fake.opts.ChunkBytes
	cold := testPackedStorage(t, apart)
	before := fake.Snapshot()
	absent := 0
	for i := range 200 {
		if err := cold.HasEncodedObject(absentHash(i)); errors.Is(err, plumbing.ErrObjectNotFound) {
			absent++
		} else if err != nil {
			t.Fatalf("probe: %v", err)
		}
	}
	spent := fake.Snapshot().Sub(before)
	pack := cold.manifests.current.Load().packs[0]
	if pack.filter.Load() == nil {
		t.Fatal("the probes did not read the pack's filter")
	}
	// A false positive, about one probe in 256, sends one probe to the index;
	// that is the filter doing its job, and it is not what is measured here.
	if pack.parsed.Load() != nil {
		t.Skip("a probe of the 200 was a false positive of the filter; the measurement needs none")
	}
	if absent != 200 {
		t.Fatalf("%d of 200 absent objects were reported absent", absent)
	}
	// The manifest whole, and the filter's extents, and the footer's: nothing
	// of the index.
	if spent.BytesDown-int64(len(mustGet(t, fake, manifestKey))) > stored[0].FilterBytes+3*int64(fake.opts.ChunkBytes) {
		t.Fatalf("probes answered by a %d-byte filter downloaded %s, an index of %d bytes is in there", stored[0].FilterBytes, spent, stored[0].IndexBytes)
	}
	for _, hash := range hashes {
		if err := cold.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s reported absent: %v", hash, err)
		}
	}
}

// mustGet returns what the fake holds under key.
func mustGet(t *testing.T, fake *fakeS3, key string) []byte {
	t.Helper()
	data, ok := fake.Get(key)
	if !ok {
		t.Fatalf("the store holds no %s", key)
	}
	return data
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
	if err := writer.FlushObjects(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// The reader holds no manifest yet, so the first answer is taken from one
	// it reads itself.
	if err := reader.HasEncodedObject(hash); err != nil {
		t.Fatalf("an object present in the object store was reported absent: %v", err)
	}

	later := writeBlob(t, writer, "written after the reader took its snapshot")
	if err := writer.FlushObjects(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	pushed := smallPush(t, writer, "pushed after the reader took its snapshot")
	// Inside the bound the snapshot answers by itself: that is the bound's
	// meaning, and what keeps a negotiation from revalidating for every question.
	before := fake.Snapshot()
	for _, absent := range []plumbing.Hash{later, pushed} {
		if err := reader.HasEncodedObject(absent); !errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Fatalf("premise: inside the bound the reader answered %v for an object its manifest does not hold", err)
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

// TestAHandleIsNeverStaleAboutItsOwnWrites pins the part of the staleness
// argument that does not depend on the freshness window at all: a writer is
// never stale about itself. An object it writes is readable at once, from what
// is pending; once flushed it is read from the pack the flush committed, which
// the handle's own commit told it of — with a day-long freshness window
// nothing else would.
func TestAHandleIsNeverStaleAboutItsOwnWrites(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = 24 * time.Hour
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 100)
	if err := stor.HasEncodedObject(absentHash(3)); err == nil {
		t.Fatal("an object that was never written was reported present")
	}

	const body = "written after the manifest was read"
	hash := writeBlob(t, stor, body)
	for _, when := range []string{"pending", "flushed"} {
		if when == "flushed" {
			if err := stor.FlushObjects(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if _, pending := stor.pending.get(hash); pending {
				t.Fatal("premise: the flushed object is still pending")
			}
			stor.objectCache.Clear()
		}
		before := fake.Snapshot()
		if err := stor.HasEncodedObject(hash); err != nil {
			t.Fatalf("%s: this process's own write was reported absent: %v", when, err)
		}
		if got := readObjects(t, stor, []plumbing.Hash{hash})[hash]; !strings.HasPrefix(got, body+"\x00") {
			t.Fatalf("%s: read back %q", when, got)
		}
		if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
			t.Fatalf("%s: reading this process's own write asked the store: %s", when, spent)
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

	// What the filters replace: go-git's in-memory pack index holds a 20-byte
	// object id, a 4-byte CRC and a 4-byte offset for every object.
	exactIndexBytes := million * (20 + 4 + 4)

	t.Logf("per million objects: binary fuse %d bytes (%.2f bits/key), exact pack index for comparison %d bytes",
		fuseBytes, float64(fuse.bits())/million, exactIndexBytes)

	if fuseBytes > 1_400_000 {
		t.Fatalf("binary fuse filter for a million objects is %d bytes, want under 1.4 MB", fuseBytes)
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
// invariant. A filter's "maybe" sends the caller to the pack's index, and when
// the index says no the answer must be the plain "not found" every caller of
// go-git compares against — not some other error, which a fetch would report
// as a failure. The object asked after is one the pack's filter is found to
// say "maybe" to.
func TestAFilterFalsePositiveIsStillNotFound(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Hour
	fake.clock = newTestClock()
	writer := testPackedStorage(t, fake)
	seedObjects(t, writer, 50)
	filter := writer.manifests.current.Load().packs[0].filter.Load()
	if filter == nil {
		t.Fatal("premise: the writing handle does not hold the filter it built")
	}
	impostor := plumbing.ZeroHash
	for i := 0; i < 1<<20 && impostor.IsZero(); i++ {
		if candidate := absentHash(i); filter.contains(oidKeyFrom(candidate[:])) {
			impostor = candidate
		}
	}
	if impostor.IsZero() {
		t.Fatal("premise: no false positive of the filter was found")
	}

	stor := testPackedStorage(t, fake)
	before := fake.Snapshot()
	if err := stor.HasEncodedObject(impostor); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a probe the filter could not rule out: %v, want ErrObjectNotFound", err)
	}
	if stor.manifests.current.Load().packs[0].parsed.Load() == nil {
		t.Fatal("premise: the probe was not sent on to the index")
	}
	if _, err := stor.EncodedObject(plumbing.AnyObject, impostor); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a read the filter could not rule out: %v, want ErrObjectNotFound", err)
	}
	if _, err := stor.EncodedObjectSize(impostor); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a size the filter could not rule out: %v, want ErrObjectNotFound", err)
	}
	// Inside the freshness bound a miss is believed: the manifest is read once,
	// and nothing is listed.
	if spent := fake.Snapshot().Sub(before); spent.List != 0 || spent.Get != 1 {
		t.Fatalf("three false positives cost %s, want the one read of the manifest", spent)
	}
}
