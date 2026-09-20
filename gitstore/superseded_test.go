package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

	chrooted, err := fake.fs("bucket", "prefix").Chroot(testRepo)
	if err != nil {
		t.Fatalf("chroot: %v", err)
	}
	entries, err := chrooted.ReadDir("objects/pack")
	if err != nil {
		t.Fatalf("list packs: %v", err)
	}
	listed := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".pack") {
			listed++
		}
	}
	if stored := len(packKeys(fake, ".pack")); listed != 2 || stored != 2+compactionMergeThreshold+1 {
		t.Fatalf("a new reader lists %d packs of the %d stored, want the 2 live ones of %d", listed, stored, 2+compactionMergeThreshold+1)
	}

	// A superseded pack is hidden, not gone: a reader that adopted it before the
	// merge still reads it by key.
	superseded := strings.TrimSuffix(strings.TrimPrefix(packKeys(fake, ".superseded")[0], "prefix/"+testRepo+"/"), ".superseded")
	if _, err := chrooted.Stat(superseded + ".pack"); err != nil {
		t.Fatalf("a superseded pack is no longer readable by key: %v", err)
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

	// A different filesystem is a different replica: nothing cached, nothing
	// remembered. Every request is slowed so that the readers, which all start
	// at once, are certain to be asking for an extent while another's fetch of
	// it is still in flight.
	cold := newFakeS3(t)
	cold.Server = fake.Server
	const readers = 8
	fs := cold.fs("bucket", "prefix")
	fake.SetLatency(100 * time.Millisecond)
	t.Cleanup(func() { fake.SetLatency(0) })

	var wg sync.WaitGroup
	errs := make([]error, readers)
	before := fake.Snapshot()
	for reader := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replica, err := OpenObjectStore(fs, testRepo)
			if err != nil {
				errs[reader] = err
				return
			}
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
	// One fetch each for the index, the filter and the pack's single extent is
	// the floor; a herd that did not share would make several times that.
	if packReads > 4 {
		t.Fatalf("%d concurrent cold readers made %d ranged reads; they did not share fetches: %s", readers, packReads, spent)
	}
}

// TestRangedReadsHonourTheCircuitBreaker pins that the pack read path fails
// fast during an outage like every other S3 call. It used to bypass the breaker
// and wait out its full timeout on every read, holding the repository lock.
func TestRangedReadsHonourTheCircuitBreaker(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.BreakerThreshold = 2
	stor := testPackedStorage(t, fake)
	pack, _ := pushPack(t, 100)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}

	cold := newFakeS3(t)
	cold.Server = fake.Server
	cold.opts.BreakerThreshold = 2
	fs := cold.fs("bucket", "prefix")
	chrooted, err := fs.Chroot(testRepo)
	if err != nil {
		t.Fatalf("chroot: %v", err)
	}
	packName := strings.TrimPrefix(packKeys(fake, ".pack")[0], "prefix/"+testRepo+"/")

	fake.SetFailOn(func(method, key string) bool { return method == "GET" && strings.HasSuffix(key, ".pack") })
	for range 2 {
		if _, err := chrooted.Open(packName); err == nil {
			t.Fatal("a failing ranged read reported success")
		}
	}
	before := fake.Snapshot()
	_, err = chrooted.Open(packName)
	if !errors.Is(err, ErrS3Unavailable) {
		t.Fatalf("after repeated failures a ranged read returned %v, want the breaker's fast failure", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Total() != 0 {
		t.Fatalf("an open breaker still let a ranged read through: %s", spent)
	}
}
