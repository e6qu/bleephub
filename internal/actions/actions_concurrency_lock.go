package actions

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

// Cross-replica concurrency admission (ACT-012): serialize workflow- and job-level admission across replicas of a
// shared-database deployment via the shared TTL'd named lock, refreshing in-memory state under it so the group scan sees
// peer-admitted holders. Residual: a replica stalled while holding the lock lets the TTL expire and a peer admit concurrently for that window.

const (
	// actionsConcurrencyLockTTL bounds a lock stranded by a dead replica.
	actionsConcurrencyLockTTL = 5 * time.Second
	// actionsConcurrencyLockWait bounds the wait for a peer before falling back to process-local admission.
	actionsConcurrencyLockWait = 10 * time.Second
	actionsConcurrencyLockPoll = 25 * time.Millisecond
)

// ActionsConcurrencyLockName names the shared-database lock for one repository's
// workflow concurrency group. Groups are repo-scoped on GitHub, so the repo is
// part of the key; groups are user-controlled and unbounded, so digest them.
func ActionsConcurrencyLockName(repoFullName, group string) string {
	digest := sha256.Sum256([]byte(repoFullName + "\x00" + group))
	return "actions/concurrency/" + hex.EncodeToString(digest[:])
}

// actionsJobConcurrencyLockName guards job-level admission. Job groups resolve from templates inside the dispatch loop,
// so their names are unknown before the lock must be taken; one coarse shared lock is correct.
const actionsJobConcurrencyLockName = "actions/concurrency/job-admission"

// acquireConcurrencyAdmissionLock takes the named lock only when persistence is shared, refreshes in-memory state so the
// caller's group scan sees peer holders, and returns a release to call after the caller commits its state. On a single-writer
// database it returns a no-op release. A lock timeout or database error degrades to process-local admission rather than losing the run.
func (s *Engine) acquireConcurrencyAdmissionLock(name string) (release func()) {
	s.store.Mu.RLock()
	persist := s.store.Persist
	s.store.Mu.RUnlock()
	if persist == nil || persist.OwnedExclusively() {
		return func() {}
	}

	owner := uuid.New().String()
	deadline := time.Now().Add(actionsConcurrencyLockWait)
	for {
		acquired, err := persist.AcquireLock(name, owner, actionsConcurrencyLockTTL)
		if err != nil {
			s.logger.Warn().Err(err).Str("lock", name).
				Msg("concurrency admission lock unavailable — proceeding with process-local admission")
			return func() {}
		}
		if acquired {
			break
		}
		if !time.Now().Before(deadline) {
			s.logger.Warn().Str("lock", name).
				Msg("concurrency admission lock held past deadline — proceeding with process-local admission")
			return func() {}
		}
		time.Sleep(actionsConcurrencyLockPoll)
	}

	// Pull in peer admissions committed before this point; the refresh preserves the object identity of runs this process is actively executing.
	if err := s.store.RefreshFromPersistenceIfStale(); err != nil {
		s.logger.Warn().Err(err).Str("lock", name).
			Msg("state refresh under concurrency admission lock failed")
	}

	return func() {
		if err := persist.ReleaseLock(name, owner); err != nil {
			s.logger.Warn().Err(err).Str("lock", name).Msg("release concurrency admission lock")
		}
	}
}

// acquireJobConcurrencyAdmissionLock wraps acquireConcurrencyAdmissionLock for DispatchReadyJobs, plus a repair step: a
// just-submitted wf with no runtime job yet is not recognized as locally owned, so the refresh detaches its map entry;
// restore wf's object identity before the dispatch loop mutates it.
func (s *Engine) acquireJobConcurrencyAdmissionLock(wf *store.Workflow) (release func()) {
	s.store.Mu.RLock()
	persist := s.store.Persist
	s.store.Mu.RUnlock()
	if persist == nil || persist.OwnedExclusively() {
		// Single-writer database: no refresh, so wf cannot have been detached.
		return func() {}
	}
	release = s.acquireConcurrencyAdmissionLock(actionsJobConcurrencyLockName)
	s.store.Mu.Lock()
	if s.store.Workflows[wf.ID] != wf {
		s.store.Workflows[wf.ID] = wf
		s.store.SyncWorkflowIndexesLocked(wf)
	}
	s.store.Mu.Unlock()
	return release
}

// workflowHasJobConcurrency reports whether any job of wf declares job-level concurrency. Consults only the immutable Def, so it needs no lock.
func workflowHasJobConcurrency(wf *store.Workflow) bool {
	for _, wfJob := range wf.Jobs {
		if wfJob.Def != nil && wfJob.Def.Concurrency != nil {
			return true
		}
	}
	return false
}
