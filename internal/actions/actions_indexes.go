package actions

import (
	"context"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Actions hot-path indexes and GC of replica-local job runtime state (ACT-044).
// A completed job's Message embeds GITHUB_TOKEN and secret values, so its
// replica-local state is torn down once no valid runner credential can still
// reach it. Swept state is non-persisted; durable run history is untouched.

// ReleaseJobLogFiles frees the in-memory log bytes claimed by the given plans.
// Call without the store lock held (the ArtifactStore has its own mutex).
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

const actionsJanitorInterval = 10 * time.Minute

// startActionsJanitor runs the periodic sweep of retired Actions job state,
// stopping through ctx at shutdown.
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

// SweepRetiredActionsJobs deletes the replica-local state of every job
// completed longer ago than completedJobRetention, returning the count swept.
func (s *Engine) SweepRetiredActionsJobs(now time.Time) int {
	var planIDs []string
	s.store.Mu.Lock()
	var retired []*store.Job
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
