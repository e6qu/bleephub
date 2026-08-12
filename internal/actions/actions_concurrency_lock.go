package actions

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

// Cross-replica concurrency admission (ACT-012).
//
// Workflow- and job-level concurrency admission used to be one PROCESS-LOCAL
// critical section (workflowConcurrencyMu + store.mu), so two replicas of a
// shared-database deployment could both observe an empty group and both admit
// a run. The shared database already provides a TTL'd named lock
// (Persistence.AcquireLock/ReleaseLock, production-used by git storage); the
// admission paths take it around their existing critical sections whenever the
// database is shared, and refresh in-memory state under the lock so the group
// scan sees peer-admitted holders.
//
// Accepted residual exposure: if a replica stalls while holding the lock, the
// TTL expires and another replica can admit concurrently for that window —
// the same bounded exposure the git-object lock accepts. Fencing tokens are
// out of scope.

const (
	// actionsConcurrencyLockTTL bounds a lock stranded by a dead replica. The
	// guarded section is an in-memory group scan plus one record write, so
	// well under a second in the healthy case.
	actionsConcurrencyLockTTL = 5 * time.Second
	// actionsConcurrencyLockWait bounds how long an admission waits for a
	// peer before proceeding with process-local admission only (the lock TTL
	// guarantees the wait can only be exceeded when the database itself
	// misbehaves).
	actionsConcurrencyLockWait = 10 * time.Second
	actionsConcurrencyLockPoll = 25 * time.Millisecond
)

// ActionsConcurrencyLockName names the shared-database lock for one workflow
// concurrency group. Group strings are user-controlled and unbounded, so they
// are digested.
func ActionsConcurrencyLockName(group string) string {
	digest := sha256.Sum256([]byte(group))
	return "actions/concurrency/" + hex.EncodeToString(digest[:])
}

// actionsJobConcurrencyLockName guards job-level concurrency admission. Job
// groups resolve from templates inside the dispatch loop, so their names are
// not known before the lock must be taken; one shared lock is coarse but
// correct, and job-level concurrency is rare.
const actionsJobConcurrencyLockName = "actions/concurrency/job-admission"

// acquireConcurrencyAdmissionLock takes the named shared-database lock when —
// and only when — the store's persistence is shared with peer replicas, then
// refreshes in-memory state so the caller's group scan sees peer-admitted
// holders. The returned release function must be called after the caller's
// state is committed (persistWorkflowRecord writes through before the caller
// unlocks the store). On a single-writer database it returns a no-op release
// without touching the database at all.
//
// A lock that cannot be acquired within actionsConcurrencyLockWait, or a
// database error while acquiring it, degrades to process-local admission with
// a logged warning rather than failing the run: the exposure is the same
// bounded window the TTL already accepts, and refusing the submission would
// turn a database hiccup into a lost run.
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

	// Any peer admission committed before this point advanced the shared
	// revision; pull it in so the group scan sees the peer's holders. The
	// refresh preserves the object identity of runs this process is actively
	// executing (workflowHasLocalRuntimeJob).
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

// acquireJobConcurrencyAdmissionLock is acquireConcurrencyAdmissionLock for
// the job-level admission inside DispatchReadyJobs, plus one repair step: wf
// may have been submitted moments ago with no runtime job dispatched yet, in
// which case the snapshot refresh cannot recognize it as locally owned and
// replaces its map entry with a detached copy. This process is actively
// driving wf, so its object identity is restored before the dispatch loop
// mutates it.
func (s *Engine) acquireJobConcurrencyAdmissionLock(wf *store.Workflow) (release func()) {
	s.store.Mu.RLock()
	persist := s.store.Persist
	s.store.Mu.RUnlock()
	if persist == nil || persist.OwnedExclusively() {
		// Single-writer database: no lock, no refresh, nothing that could have
		// detached wf — zero cost.
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

// workflowHasJobConcurrency reports whether any job of wf declares job-level
// concurrency, i.e. whether dispatch may run a job-group admission that needs
// cross-replica serialization. Only the immutable Def is consulted — a
// resolved ConcurrencyGroup implies Def.Concurrency != nil, and Def is never
// written after submit, so this needs no lock.
func workflowHasJobConcurrency(wf *store.Workflow) bool {
	for _, wfJob := range wf.Jobs {
		if wfJob.Def != nil && wfJob.Def.Concurrency != nil {
			return true
		}
	}
	return false
}
