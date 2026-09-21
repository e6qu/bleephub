package gitstore

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestProbesAreRaceFreeAgainstConcurrentWritesAndFlushes exercises the
// read-during-write pattern a replica lives with — a clone's negotiation (the
// read path) while objects are written through the API and flushed as packs
// (the write path). Readers probe what is pending and the packs of the state
// held while a writer adds to the one and swaps in a new state for the other.
// A short freshness window sends every miss down the revalidation path as
// well. Run under -race, this must stay clean.
func TestProbesAreRaceFreeAgainstConcurrentWritesAndFlushes(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.IndexFreshness = time.Millisecond // stale almost at once: misses revalidate
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 50)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Readers probe through HasEncodedObject (no t.* calls in the goroutines,
	// so they are goroutine-safe).
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = stor.HasEncodedObject(absentHash(n))
				}
			}
		}(i)
	}
	// The writer runs on the test goroutine (writeBlob may call t.Fatalf).
	for i := 0; i < 300; i++ {
		writeBlob(t, stor, fmt.Sprintf("race-blob-%d", i))
		if i%50 == 49 {
			if err := stor.FlushObjects(); err != nil {
				t.Fatalf("flush: %v", err)
			}
		}
	}
	close(stop)
	wg.Wait()
}
