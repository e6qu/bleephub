package actions

import (
	"context"
	"time"
)

// This file holds the Actions hot-path indexes and the garbage collection of
// replica-local job runtime state (ACT-044).
//
// Before these indexes existed, every runner long-poll scanned all of
// store.Jobs under the write lock, every artifact/cache call and ~15 REST
// handlers scanned all of store.Workflows, and — worst — every job-token
// request json.Unmarshal'ed each job's full secret-bearing Message. None of
// Jobs / LogMasks / LogLines / LogFiles was ever deleted, so a long-lived
// server held every GITHUB_TOKEN and secret value ever dispatched, forever.
//
// The GC policy implemented here:
//
//  1. At run finalization the run's job Messages (which embed GITHUB_TOKEN and
//     every secret value) are cleared; auth for late runner calls keeps working
//     through planScopes, which carries only {scopeIdentifier, repo}.
//  2. A janitor (startActionsJanitor) deletes a job's remaining replica-local
//     state — the Job stub, plan scope, log masks, captured log lines and
//     in-memory log bytes — once CompletedAt + runnerTokenTTL (6h) has passed:
//     no valid runner credential can address the job after that.
//  3. Run deletion and repository deletion tear the same state down eagerly.
//
// All swept state is replica-local and non-persisted; durable run history
// (Workflows, TimelineRecords, byte-store log objects) is not touched.

// --- workflow indexes ---

// --- garbage collection ---

// ReleaseJobLogFiles releases the in-memory log bytes claimed by the given
// plans: the ArtifactStore claim registry maps log ids to the plan that
// reserved them. Durable byte-store log objects are left alone — GC here is
// purely in-memory. Must be called without the store lock held (the
// ArtifactStore has its own mutex).
func (s *Engine) ReleaseJobLogFiles(planIDs []string) {
	if len(planIDs) == 0 || s.artifactStore == nil {
		return
	}
	logIDs := s.artifactStore.ReleaseLogClaimsForPlans(planIDs)
	if len(logIDs) == 0 {
		return
	}
	s.store.Mu.Lock()
	for _, logID := range logIDs {
		delete(s.store.LogFiles, logID)
	}
	s.store.Mu.Unlock()
}

// actionsJanitorInterval is how often the janitor sweeps retired job state.
const actionsJanitorInterval = 10 * time.Minute

// Engine.completedJobRetention is how long a completed job's replica-local
// runtime state stays addressable. The server wires it to its runner token
// TTL: that bounds the lifetime of the job's runtime token and the agent
// session token that could still name it, and completed-job teardown calls
// (DELETE AgentRequest, late log flushes) can arrive that late — so nothing
// valid can reach the job afterwards.

// startActionsJanitor runs the periodic sweep of retired Actions job state for
// the server's lifetime. There is deliberately one janitor per process,
// started with the server and stopped through ctx at shutdown.
func (s *Engine) startActionsJanitor(ctx context.Context) {
	s.goBackground(func() {
		ticker := time.NewTicker(actionsJanitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.SweepRetiredActionsJobs(s.store.CurrentTime())
			}
		}
	})
}

// SweepRetiredActionsJobs deletes the replica-local state of every job whose
// retirement stamp is older than completedJobRetention. Returns how many jobs
// were swept (for tests and logging).
func (s *Engine) SweepRetiredActionsJobs(now time.Time) int {
	var planIDs []string
	s.store.Mu.Lock()
	var retired []*Job
	for _, job := range s.store.Jobs {
		if job.CompletedAt.IsZero() {
			continue
		}
		if now.Sub(job.CompletedAt) < s.completedJobRetention {
			continue
		}
		retired = append(retired, job)
	}
	for _, job := range retired {
		if planID := s.store.DropJobStateLocked(job); planID != "" {
			planIDs = append(planIDs, planID)
		}
	}
	s.store.Mu.Unlock()

	s.ReleaseJobLogFiles(planIDs)

	if len(retired) > 0 {
		s.logger.Info().Int("jobs", len(retired)).Msg("actions janitor swept retired job state")
	}
	return len(retired)
}
