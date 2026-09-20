package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"
)

// mergedRepository pushes one large pack and enough small ones to cross the
// merge threshold, then compacts, leaving the small packs superseded but still
// in the bucket.
func mergedRepository(t *testing.T, fake *fakeS3) []plumbing.Hash {
	t.Helper()
	fake.opts.CompactionTrigger = -1
	stor := testPackedStorage(t, fake)
	big, all := pushPack(t, 400)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(big)); err != nil {
		t.Fatalf("big push: %v", err)
	}
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
		all = append(all, hash)
	}
	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.Merged == 0 {
		t.Fatal("fixture did not merge")
	}
	if len(packKeys(fake, ".superseded")) != compactionMergeThreshold+1 {
		t.Fatalf("fixture superseded %d packs, want %d", len(packKeys(fake, ".superseded")), compactionMergeThreshold+1)
	}
	return all
}

// TestANewReaderDoesNotAdoptSupersededPacks pins that the retention window a
// merged-away pack is kept for serves readers already holding it, and costs a
// reader arriving afterwards nothing: it sees the live packs only, yet finds
// every object.
func TestANewReaderDoesNotAdoptSupersededPacks(t *testing.T) {
	fake := newFakeS3(t)
	all := mergedRepository(t, fake)

	reader := testPackedStorage(t, fake)
	listed, err := reader.StoredPacks(context.Background())
	if err != nil {
		t.Fatalf("list packs: %v", err)
	}
	if stored := len(packKeys(fake, ".pack")); len(listed) != 2 || stored != 2+compactionMergeThreshold+1 {
		t.Fatalf("a new reader lists %d packs of the %d stored, want the 2 live ones of %d", len(listed), stored, 2+compactionMergeThreshold+1)
	}

	// A superseded pack is hidden, not gone: a reader that adopted it before the
	// merge still reads it by key.
	superseded := strings.TrimSuffix(packKeys(fake, ".superseded")[0], ".superseded")
	if _, ok := fake.Get(superseded + ".pack"); !ok {
		t.Fatal("a superseded pack is no longer readable by key")
	}

	fresh := testPackedStorage(t, fake)
	before := fake.Snapshot()
	for _, hash := range all {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s is invisible to a new reader: %v", hash, err)
		}
	}
	if err := fresh.HasEncodedObject(absentHash(3)); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("absent object: %v", err)
	}
	spent := fake.Snapshot().Sub(before)
	// Two live packs: an index and a filter each, plus listings. Adopting the
	// superseded ones as well would add two reads for every one of them.
	if reads := spent.Get + spent.GetRanged; reads > 6 {
		t.Fatalf("a new reader made %d reads, so it is still loading superseded packs: %s", reads, spent)
	}
}

// TestConcurrentColdReadsShareOneFetch pins the thundering herd: many clones
// starting against a cold cache want the same extents, and must not each
// download their own copy.
func TestConcurrentColdReadsShareOneFetch(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	pack, hashes := pushPack(t, 200)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}

	// A different store is a different replica: nothing cached, nothing
	// remembered. Every request is slowed so that the readers, which all start
	// at once, are certain to be asking for an extent while another's fetch of
	// it is still in flight.
	cold := newFakeS3(t)
	cold.Server = fake.Server
	const readers = 8
	replica := testPackedStorage(t, cold)
	fake.SetLatency(100 * time.Millisecond)
	t.Cleanup(func() { fake.SetLatency(0) })

	var wg sync.WaitGroup
	errs := make([]error, readers)
	before := fake.Snapshot()
	for reader := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[reader] = replica.EncodedObject(plumbing.AnyObject, hashes[reader])
		}()
	}
	wg.Wait()
	for reader, err := range errs {
		if err != nil {
			t.Fatalf("reader %d: %v", reader, err)
		}
	}

	spent := fake.Snapshot().Sub(before)
	packReads := spent.GetRanged
	// One fetch each for the index and the pack's single extent is the floor; a
	// herd that did not share would make several times that.
	if packReads < 2 {
		t.Fatalf("premise: the readers did not read the pack from the store: %s", spent)
	}
	if packReads > 2 || spent.List != 1 {
		t.Fatalf("%d concurrent cold readers made %d ranged reads and %d listings; they did not share them: %s", readers, packReads, spent.List, spent)
	}
}

// TestRangedReadsHonourTheCircuitBreaker pins that the pack read path fails
// fast during an outage like every other store call, and that what the decoder
// makes of a failed read — it may well call the object missing — never reaches
// the caller in place of what the store said.
func TestRangedReadsHonourTheCircuitBreaker(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.BreakerThreshold = 2
	stor := testPackedStorage(t, fake)
	pack, hashes := pushPack(t, 100)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}

	cold := newFakeS3(t)
	cold.Server = fake.Server
	cold.opts.BreakerThreshold = 2
	replica := testPackedStorage(t, cold)
	// The snapshot, the index and the filter arrive while the store is well; it
	// is the pack's own bytes that the outage catches.
	if err := replica.HasEncodedObject(hashes[0]); err != nil {
		t.Fatalf("probe: %v", err)
	}

	fake.SetFailOn(func(method, key string) bool { return method == "GET" && strings.HasSuffix(key, ".pack") })
	for _, hash := range hashes[:2] {
		_, err := replica.EncodedObject(plumbing.AnyObject, hash)
		if err == nil {
			t.Fatal("a failing ranged read reported success")
		}
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Fatalf("a failing ranged read was reported as a missing object: %v", err)
		}
	}
	before := fake.Snapshot()
	_, err := replica.EncodedObject(plumbing.AnyObject, hashes[2])
	if !errors.Is(err, ErrS3Unavailable) {
		t.Fatalf("after repeated failures a ranged read returned %v, want the breaker's fast failure", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("an open breaker still let a ranged read through: %s", spent)
	}
}

// TestACompactionWithNothingToDoIsCheap prices the common case. A server asks
// for a compaction after every push, and nearly every time there is nothing to
// pack or merge; what that costs is paid on every push.
func TestACompactionWithNothingToDoIsCheap(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactionTrigger = -1
	stor := testPackedStorage(t, fake)
	pack, _ := pushPack(t, 50)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}

	before := fake.Snapshot()
	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.PackName != "" {
		t.Fatalf("a repository of one pack was compacted into %s", result.PackName)
	}
	spent := fake.Snapshot().Sub(before)
	// One listing of objects/ says what is loose and what is packed.
	if spent.List != 1 || spent.Total() != 1 {
		t.Fatalf("finding nothing to do should cost one listing: %s", spent)
	}
}

// TestASupersededPackIsRetiredOnlyOnceItsRetentionWindowHasPassed pins the two
// halves of "delete nothing a reader may still need". A pack merged away less
// than the retention window ago keeps every one of its keys, for the request
// that was reading it when the merge landed. One older than that is removed —
// and its .pack key goes first, so that no reader arriving mid-removal adopts a
// pack whose index has already gone. Age is measured between the store's own
// modification times, never against this replica's clock.
func TestASupersededPackIsRetiredOnlyOnceItsRetentionWindowHasPassed(t *testing.T) {
	fake := newFakeS3(t)
	all := mergedRepository(t, fake)
	stor := testPackedStorage(t, fake)
	listing, err := stor.tiers.list()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// The fake stamps every object with one instant, so the ages are given to
	// the listing by hand: two markers were written long ago, the rest just now.
	now := time.Date(2020, time.January, 1, 12, 0, 0, 0, time.UTC)
	var aged, recent []string
	for name, entry := range listing.packDirectory {
		entry.modified = now
		if pack, isMarker := strings.CutSuffix(name, ".superseded"); isMarker {
			if len(aged) < 2 {
				entry.modified = now.Add(-2 * supersededPackRetention)
				aged = append(aged, pack)
			} else {
				recent = append(recent, pack)
			}
		}
		listing.packDirectory[name] = entry
	}
	if len(aged) != 2 || len(recent) == 0 {
		t.Fatalf("premise: %d aged and %d recent superseded packs", len(aged), len(recent))
	}

	var deleteMu sync.Mutex
	var deleted []string
	fake.SetOnRequest(func(method, key string) {
		if method == "DELETE" {
			deleteMu.Lock()
			deleted = append(deleted, key)
			deleteMu.Unlock()
		}
	})
	retired, err := stor.retireSupersededPacks(context.Background(), listing)
	fake.SetOnRequest(nil)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	sort.Strings(aged)
	if strings.Join(retired, ",") != strings.Join(aged, ",") {
		t.Fatalf("retired %v, want the two packs past their window: %v", retired, aged)
	}

	directory := "prefix/" + testRepo + "/objects/pack/"
	for _, pack := range aged {
		if keys := fake.KeysWithPrefix(directory + pack + "."); len(keys) != 0 {
			t.Fatalf("a retired pack left %v behind", keys)
		}
	}
	for _, pack := range recent {
		if keys := fake.KeysWithPrefix(directory + pack + "."); len(keys) != 4 {
			t.Fatalf("a pack inside its retention window has %v left, want its pack, index, filter and marker", keys)
		}
	}
	packsGone := 0
	for _, key := range deleted {
		if strings.HasSuffix(key, ".pack") {
			packsGone++
		} else if packsGone < len(aged) {
			t.Fatalf("%s was deleted before every retired .pack key was: %v", key, deleted)
		}
	}

	// Nothing a reader needs went with them.
	fresh := testPackedStorage(t, fake)
	for _, hash := range all {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s was lost to the retirement: %v", hash, err)
		}
	}
}
