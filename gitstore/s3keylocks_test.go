package gitstore

import (
	"sync"
	"testing"
	"time"
)

// TestKeyLocksAreReleasedUnderContention pins that the key-lock table does not
// leak entries, which would otherwise grow without bound on a busy server.
func TestKeyLocksAreReleasedUnderContention(t *testing.T) {
	locks := newS3KeyLocks()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := locks.acquire("git/owner/repo/packed-refs", 5*time.Second); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			locks.release("git/owner/repo/packed-refs")
		}()
	}
	wg.Wait()

	count := 0
	locks.mu.Lock()
	count = len(locks.slots)
	locks.mu.Unlock()
	if count != 0 {
		t.Fatalf("key lock table retained %d entries", count)
	}
}
