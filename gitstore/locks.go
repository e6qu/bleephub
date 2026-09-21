package gitstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// gitObjectLockWait is how long a writer waits for another in this process to
// let go of a lock before it gives up.
const gitObjectLockWait = 30 * time.Second

// s3KeyLocks serializes holders of one lock name within this process. Entries are reference counted so an idle name costs nothing.
type s3KeyLocks struct {
	mu    sync.Mutex
	slots map[string]*s3KeySlot
}

type s3KeySlot struct {
	ch   chan struct{}
	refs int
}

func newS3KeyLocks() *s3KeyLocks {
	return &s3KeyLocks{slots: map[string]*s3KeySlot{}}
}

func (l *s3KeyLocks) acquire(key string, wait time.Duration) error {
	l.mu.Lock()
	slot := l.slots[key]
	if slot == nil {
		slot = &s3KeySlot{ch: make(chan struct{}, 1)}
		l.slots[key] = slot
	}
	slot.refs++
	l.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case slot.ch <- struct{}{}:
		return nil
	case <-timer.C:
		l.drop(key)
		return fmt.Errorf("lock %s: another writer in this process still holds it after %s", key, wait)
	}
}

func (l *s3KeyLocks) release(key string) {
	l.mu.Lock()
	slot := l.slots[key]
	l.mu.Unlock()
	if slot == nil {
		return
	}
	<-slot.ch
	l.drop(key)
}

func (l *s3KeyLocks) drop(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot := l.slots[key]
	if slot == nil {
		return
	}
	slot.refs--
	if slot.refs <= 0 {
		delete(l.slots, key)
	}
}

// withLockName runs mutate holding the named process-local lock. It serves the
// local-directory and memory backends, whose storage is go-git's own and has no
// compare-and-swap of its own to offer. The object-store backend takes no lock
// at all: the store arbitrates its writers, in this process and across replicas,
// by conditional write of the repository's manifest (commit.go).
func withLockName(name string, mutate func() error) error {
	if err := refMutationLocks.acquire(name, gitObjectLockWait); err != nil {
		return err
	}
	defer refMutationLocks.release(name)
	return mutate()
}

// lockNameFor derives the name of a lock from the repository it guards and what
// within it is being guarded. The digest gives every name one bounded shape,
// whatever the reference is called.
func lockNameFor(kind, repo, subject string) string {
	digest := sha256.Sum256([]byte(repo + "\x00" + subject))
	return kind + ":" + hex.EncodeToString(digest[:])
}
