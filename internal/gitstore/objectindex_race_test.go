package gitstore

import (
	"fmt"
	"sync"
	"testing"
)

// TestLooseAbsentIsRaceFreeAgainstConcurrentWrites exercises the exact
// read-during-write pattern the object index exists for — a clone (read path)
// while a push (write path) is in flight. looseAbsent used to dereference the
// shared roots map and cuckoo filter AFTER releasing i.mu, so a concurrent
// noteLooseWrite mutating them under the lock triggered a fatal concurrent
// map read/write (and a torn filter read that could drop a present object).
// A short freshness window forces every probe down the refresh path where the
// unlocked reads lived. Run under -race, this must stay clean.
func TestLooseAbsentIsRaceFreeAgainstConcurrentWrites(t *testing.T) {
	t.Setenv(packCacheDirEnv, t.TempDir())
	t.Setenv(objectIndexFreshnessEnv, "1ms") // stale immediately → refresh path
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	seedObjects(t, stor, 50)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Readers hammer looseAbsent through HasEncodedObject (no t.* calls in the
	// goroutines, so they are goroutine-safe).
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
	// The writer runs on the test goroutine (writeBlob may call t.Fatalf); each
	// write calls noteLooseWrite, mutating the shared map + filter the readers probe.
	for i := 0; i < 300; i++ {
		writeBlob(t, stor, fmt.Sprintf("race-blob-%d", i))
	}
	close(stop)
	wg.Wait()
}
