package store

import (
	"sort"
	"time"
)

// Git LFS server state.
//
// The object *bytes* never live here: they go to the S3-backed
// ActionsByteStore under LFSObjectDataKey, which is content-addressed by the
// object's oid and therefore shared by every repository that holds the same
// content. What this file keeps is the two pieces of metadata an LFS server
// cannot derive from a content-addressed bucket:
//
//   - LFSObjects: which oids each repository actually holds, and how large
//     they are. Membership is per repository because the key is not: without
//     it, anyone who learned an oid could pull a private repository's object
//     through a repository of their own, and a batch response could not report
//     a size for an object the client did not already describe.
//   - LFSLocks: the file locks of the LFS locking API.
//
// Registering an oid is also the *commit point* of an upload: the transfer
// handler streams bytes to the byte store, verifies them against the advertised
// oid and size, and only then registers. Bytes that fail verification are never
// registered, so they are never served — see gh_repos_lfs.go.

// LFSLock is one Git LFS file lock: an advisory claim on a path in a
// repository, held by a user until they (or a pusher forcing it) release it.
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
// size. It is idempotent: re-uploading or re-pushing the same object from a
// second repository adds a membership row and shares the stored bytes.
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

// LFSObjectSize reports the stored size of an object held by this repository,
// and whether the repository holds it at all.
func (st *Store) LFSObjectSize(repoKey, oid string) (int64, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	size, ok := st.LFSObjects[repoKey][oid]
	return size, ok
}

// LFSObjectStoredAnywhere reports whether any repository already holds this
// oid, i.e. whether verified bytes for it are already in the byte store. The
// upload path asks before writing: bytes under a content-addressed key that is
// already registered were verified when they were written, and must not be
// overwritten by a second upload claiming the same oid.
//
// It scans the per-repository membership rather than keeping a second index:
// the map is one entry per (repository, object), and the alternative is a
// refcount that has to stay correct across repository deletion and rename.
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

// CreateLFSLock locks a path for a user. When the path is already locked it
// returns the existing lock and false, which the locking API reports as a 409
// naming the current holder.
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

// ListLFSLocks returns a repository's locks ordered by id, as detached
// snapshots (STORE-021).
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

// GetLFSLock returns one lock as a detached snapshot, or nil.
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

// DeleteLFSLock releases a lock, returning the released lock or nil when the
// repository has no such lock.
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
