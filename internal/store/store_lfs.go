package store

import (
	"sort"
	"time"
)

// Git LFS server state. Object bytes live content-addressed in the S3-backed
// ActionsByteStore, shared across repositories; this file keeps only the
// metadata that bucket cannot derive:
//
//   - LFSObjects: which oids each repository holds and their sizes. Membership
//     is per repository so a leaked oid cannot pull a private object through
//     another repository, and so a batch response can report sizes.
//   - LFSLocks: the file locks of the LFS locking API.
//
// Registering an oid is the commit point of an upload: bytes are verified
// against the advertised oid and size before registration, so unverified bytes
// are never served (see gh_repos_lfs.go).

// LFSLock is one Git LFS file lock: an advisory claim on a path, held until the
// owner or a forcing pusher releases it.
type LFSLock struct {
	ID        int       `json:"id"`
	RepoKey   string    `json:"repo_key"`
	Path      string    `json:"path"`
	Ref       string    `json:"ref,omitempty"`
	OwnerID   int       `json:"owner_id"`
	OwnerName string    `json:"owner_name"`
	LockedAt  time.Time `json:"locked_at"`
}

// RegisterLFSObject records that a repository holds an LFS object of the given
// size. Idempotent: a second repository adds a membership row and shares the bytes.
func (st *Store) RegisterLFSObject(repoKey, oid string, size int64) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	if st.LFSObjects[repoKey] == nil {
		st.LFSObjects[repoKey] = map[string]int64{}
	}
	st.LFSObjects[repoKey][oid] = size
	if st.Persist != nil {
		st.Persist.MustPut("lfs_objects", repoKey, st.LFSObjects[repoKey])
	}
}

// LFSObjectSize reports an object's size and whether this repository holds it.
func (st *Store) LFSObjectSize(repoKey, oid string) (int64, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	size, ok := st.LFSObjects[repoKey][oid]
	return size, ok
}

// LFSObjectStoredAnywhere reports whether any repository holds this oid, i.e.
// whether verified bytes are already in the byte store. The upload path checks
// this before writing so a second upload claiming the same oid cannot overwrite
// already-verified bytes.
func (st *Store) LFSObjectStoredAnywhere(oid string) (int64, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, objects := range st.LFSObjects {
		if size, ok := objects[oid]; ok {
			return size, true
		}
	}
	return 0, false
}

// CreateLFSLock locks a path for a user. An already-locked path returns the
// existing lock and false (the locking API reports a 409 naming the holder).
func (st *Store) CreateLFSLock(repoKey, path, ref string, ownerID int, ownerName string) (*LFSLock, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	for _, existing := range st.LFSLocks[repoKey] {
		if existing.Path == path {
			snapshot := *existing
			return &snapshot, false
		}
	}
	lock := &LFSLock{
		ID:        st.NextLFSLockID,
		RepoKey:   repoKey,
		Path:      path,
		Ref:       ref,
		OwnerID:   ownerID,
		OwnerName: ownerName,
		LockedAt:  st.CurrentTime(),
	}
	st.NextLFSLockID++
	if st.LFSLocks[repoKey] == nil {
		st.LFSLocks[repoKey] = map[int]*LFSLock{}
	}
	st.LFSLocks[repoKey][lock.ID] = lock
	if st.Persist != nil {
		st.Persist.MustPut("lfs_locks", repoKey, st.LFSLocks[repoKey])
	}
	snapshot := *lock
	return &snapshot, true
}

// ListLFSLocks returns a repository's locks by id, as detached snapshots (STORE-021).
func (st *Store) ListLFSLocks(repoKey string) []*LFSLock {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	locks := make([]*LFSLock, 0, len(st.LFSLocks[repoKey]))
	for _, lock := range st.LFSLocks[repoKey] {
		locks = append(locks, lock)
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].ID < locks[j].ID })
	return snapshotSlice(locks)
}

// GetLFSLock returns one lock as a detached snapshot (STORE-021), or nil.
func (st *Store) GetLFSLock(repoKey string, id int) *LFSLock {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	lock, ok := st.LFSLocks[repoKey][id]
	if !ok || lock == nil {
		return nil
	}
	snapshot := *lock
	return &snapshot
}

// DeleteLFSLock releases a lock, returning it or nil when no such lock exists.
func (st *Store) DeleteLFSLock(repoKey string, id int) *LFSLock {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	lock, ok := st.LFSLocks[repoKey][id]
	if !ok || lock == nil {
		return nil
	}
	delete(st.LFSLocks[repoKey], id)
	if st.Persist != nil {
		st.Persist.MustPut("lfs_locks", repoKey, st.LFSLocks[repoKey])
	}
	snapshot := *lock
	return &snapshot
}
