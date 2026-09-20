package gitstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// GitObjectLocker grants exclusive use of one named lock across replicas. An
// object store has no advisory locking, so the reference compare-and-set and
// the single-writer rule of compaction borrow the durable store that already
// serializes the application's shared state.
type GitObjectLocker interface {
	AcquireLock(name, owner string, ttl time.Duration) (bool, error)
	ReleaseLock(name, owner string) error
}

var (
	gitObjectLockerMu sync.RWMutex
	gitObjectLockerV  GitObjectLocker
)

// SetGitObjectLocker installs the durable lock manager. Until one is installed there is no shared durable state, so the process-local lock is the whole lock.
func SetGitObjectLocker(l GitObjectLocker) {
	gitObjectLockerMu.Lock()
	defer gitObjectLockerMu.Unlock()
	gitObjectLockerV = l
}

func currentGitObjectLocker() GitObjectLocker {
	gitObjectLockerMu.RLock()
	defer gitObjectLockerMu.RUnlock()
	return gitObjectLockerV
}

// ClearGitObjectLocker uninstalls l if it is the currently installed manager. A closed locker left installed would fail every ref update.
func ClearGitObjectLocker(l GitObjectLocker) {
	gitObjectLockerMu.Lock()
	defer gitObjectLockerMu.Unlock()
	if gitObjectLockerV == l {
		gitObjectLockerV = nil
	}
}

const (
	gitObjectLockTTL  = 2 * time.Minute
	gitObjectLockWait = 30 * time.Second
	gitObjectLockPoll = 50 * time.Millisecond
)

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

// withLockName runs mutate holding the named lock: the process-local one always,
// and the durable one on top of it when a locker is installed. The local lock
// comes first so that goroutines of one process queue here rather than poll the
// durable store against each other.
func withLockName(name string, mutate func() error) error {
	if err := refMutationLocks.acquire(name, gitObjectLockWait); err != nil {
		return err
	}
	defer refMutationLocks.release(name)
	locker := currentGitObjectLocker()
	if locker == nil {
		return mutate()
	}
	owner := uuid.New().String()
	deadline := time.Now().Add(gitObjectLockWait)
	for {
		acquired, err := locker.AcquireLock(name, owner, gitObjectLockTTL)
		if err != nil {
			return err
		}
		if acquired {
			mutationErr := mutate()
			releaseErr := locker.ReleaseLock(name, owner)
			if mutationErr != nil {
				return mutationErr
			}
			if releaseErr != nil {
				return fmt.Errorf("release lock %s: %w", name, releaseErr)
			}
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("lock %s: another replica still holds it after %s", name, gitObjectLockWait)
		}
		time.Sleep(gitObjectLockPoll)
	}
}

// lockNameFor derives the name of a lock from the repository it guards and what
// within it is being guarded. The digest keeps an arbitrary reference name out
// of the lock table's key space, and the kind prefix keeps a reference lock from
// ever colliding with a compaction lock.
func lockNameFor(kind, repo, subject string) string {
	digest := sha256.Sum256([]byte(repo + "\x00" + subject))
	return kind + ":" + hex.EncodeToString(digest[:])
}
