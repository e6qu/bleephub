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
// merge threshold, then compacts, leaving the small packs retired but still in
// the bucket.
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
	if retired := len(storedManifest(t, fake).Retired); retired != compactionMergeThreshold+1 {
		t.Fatalf("fixture retired %d packs, want %d", retired, compactionMergeThreshold+1)
	}
	return all
}

// TestANewReaderDoesNotAdoptRetiredPacks pins that the grace period a
// merged-away pack is kept for serves readers already holding it, and costs a
// reader arriving afterwards nothing: it sees the live packs only, yet finds
// every object.
func TestANewReaderDoesNotAdoptRetiredPacks(t *testing.T) {
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

	// A retired pack is hidden, not gone: a reader that held it before the merge
	// still reads it by key.
	retired := storedManifest(t, fake).Retired[0].Name
	if _, ok := fake.Get("prefix/" + testRepo + "/objects/pack/" + retired + ".pack"); !ok {
		t.Fatal("a retired pack is no longer readable by key")
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
	// The manifest, and two live packs: an index and a filter each. Adopting the
	// retired ones as well would add two reads for every one of them.
	if reads := spent.Get + spent.GetRanged; reads > 6 {
		t.Fatalf("a new reader made %d reads, so it is still loading retired packs: %s", reads, spent)
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
	// One fetch each for the index and the pack's single extent is the floor, and
	// one read of the manifest; a herd that did not share would make several
	// times that. Nothing is listed: every object asked for is in the pack.
	if packReads < 2 {
		t.Fatalf("premise: the readers did not read the pack from the store: %s", spent)
	}
	if packReads > 2 || spent.Get != 1 || spent.List != 0 {
		t.Fatalf("%d concurrent cold readers made %d ranged reads, %d reads of the manifest and %d listings; they did not share them: %s", readers, packReads, spent.Get, spent.List, spent)
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
	// One listing of objects/ says what is loose and what lies unnamed in the
	// pack directory, and one conditional read, answered "not modified", says
	// the manifest held is the one the listing is to be judged against.
	if spent.List != 1 || spent.NotModified != 1 || spent.Total() != 2 {
		t.Fatalf("finding nothing to do should cost one listing and one revalidation: %s", spent)
	}
}

// TestARetiredPackIsDeletedOnlyOnceItsGracePeriodHasPassed pins the two halves
// of "delete nothing a reader may still need". A pack merged away less than the
// grace period ago keeps every one of its keys, for the request that was
// reading it when the merge landed. One older than that is removed — its .pack
// key first, so that nothing arriving mid-removal finds a pack whose index has
// already gone — and only then dropped from the manifest. A live pack is never
// touched, however old.
func TestARetiredPackIsDeletedOnlyOnceItsGracePeriodHasPassed(t *testing.T) {
	fake := newFakeS3(t)
	fake.clock = newTestClock()
	fake.SetClock(fake.clock.Now)
	all := mergedRepository(t, fake)
	stor := testPackedStorage(t, fake)
	directory := "prefix/" + testRepo + "/objects/pack/"
	merged := storedManifest(t, fake)
	if len(merged.Packs) != 2 {
		t.Fatalf("premise: %d live packs, want the big one and the merged one", len(merged.Packs))
	}

	fake.clock.Advance(retiredPackGrace - time.Minute)
	result, err := stor.Compact(context.Background())
	if err != nil || len(result.RetiredPacks) != 0 {
		t.Fatalf("inside the grace period a compaction deleted %v (err %v)", result.RetiredPacks, err)
	}
	for _, pack := range merged.Retired {
		if keys := fake.KeysWithPrefix(directory + pack.Name + "."); len(keys) != 3 {
			t.Fatalf("a pack inside its grace period has %v left, want its pack, index and filter", keys)
		}
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
	fake.clock.Advance(2 * time.Minute)
	result, err = stor.Compact(context.Background())
	fake.SetOnRequest(nil)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	var want []string
	for _, pack := range merged.Retired {
		want = append(want, pack.Name)
	}
	sort.Strings(want)
	if strings.Join(result.RetiredPacks, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted %v, want the packs past their grace period: %v", result.RetiredPacks, want)
	}
	for _, pack := range merged.Retired {
		if keys := fake.KeysWithPrefix(directory + pack.Name + "."); len(keys) != 0 {
			t.Fatalf("a deleted pack left %v behind", keys)
		}
	}
	for i := 0; i+2 < len(deleted); i += 3 {
		if !strings.HasSuffix(deleted[i], ".pack") {
			t.Fatalf("%s was deleted before its pack was: %v", deleted[i], deleted)
		}
	}
	after := storedManifest(t, fake)
	if len(after.Retired) != 0 || len(after.Packs) != 2 {
		t.Fatalf("the manifest after the deletion lists %d retired and %d live packs, want 0 and 2", len(after.Retired), len(after.Packs))
	}
	for _, pack := range after.Packs {
		if keys := fake.KeysWithPrefix(directory + pack.Name + "."); len(keys) != 3 {
			t.Fatalf("the live pack %s has %v left", pack.Name, keys)
		}
	}

	// Nothing a reader needs went with them.
	fresh := testPackedStorage(t, fake)
	for _, hash := range all {
		if err := fresh.HasEncodedObject(hash); err != nil {
			t.Fatalf("object %s was lost to the deletion: %v", hash, err)
		}
	}
}
