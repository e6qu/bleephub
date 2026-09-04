package actions

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/e6qu/bleephub/internal/store"
)

// newClockedTestEngine builds an engine whose clock is the caller's mutable
// time pointer, so a test can advance the wait-timer clock deterministically.
func newClockedTestEngine(now *time.Time) *Engine {
	st := store.NewStore()
	return NewEngine(Config{
		Store:  st,
		Logger: zerolog.Nop(),
		Addr:   "127.0.0.1:0",
		Events: nopSink{},
		MintJobToken: func(scopeID string, wf *store.Workflow, jd *store.JobDef) string {
			return "test-job-token"
		},
		RepoEventPayload: func(repo *store.Repo) map[string]interface{} {
			return map[string]interface{}{"full_name": repo.FullName}
		},
		Now:                   func() time.Time { return *now },
		Go:                    func(fn func()) { go fn() },
		CompletedJobRetention: 6 * time.Hour,
	})
}

// TestWaitTimerOnlyEnvironmentGatesAndReleases pins deployment protection for an
// environment carrying ONLY a wait timer (no required reviewers). Before the fix
// protectedEnvironmentLocked returned nil whenever the reviewer list was empty,
// so a wait-timer-only environment never gated: its job dispatched immediately
// instead of waiting out the timer.
func TestWaitTimerOnlyEnvironmentGatesAndReleases(t *testing.T) {
	now := time.Date(2042, time.July, 15, 12, 0, 0, 0, time.UTC)
	e := newClockedTestEngine(&now)
	st := e.store

	// A repo whose "prod" environment has a 5-minute wait timer and no reviewers.
	st.Mu.Lock()
	owner := &store.User{ID: st.NextUser, Login: "octo", Type: "User", CreatedAt: now, UpdatedAt: now}
	st.NextUser++
	st.Users[owner.ID] = owner
	st.UsersByLogin[owner.Login] = owner
	st.Mu.Unlock()
	repo := st.CreateRepo(owner, "app", "", false)
	st.Deployments.UpsertEnvironment(repo.ID, "prod")
	waitTimer := 5
	st.Deployments.SetEnvironmentProtection(repo.ID, "prod", &waitTimer, nil)

	wf := &store.Workflow{
		ID:           "wf-wait",
		RepoFullName: "octo/app",
		Status:       store.WorkflowStatusRunning,
		Env:          map[string]string{}, // empty __serverURL: release flips status without a runner dispatch
		Jobs: map[string]*store.WorkflowJob{
			"deploy": {
				Key:    "deploy",
				Status: store.JobStatusPending,
				Def:    &store.JobDef{Environment: "prod"},
			},
		},
	}
	st.Mu.Lock()
	st.Workflows[wf.ID] = wf
	st.Mu.Unlock()

	// First admission pass: the wait timer must hold the job, not dispatch it.
	e.DispatchReadyJobs(context.Background(), wf, "", "")

	job := wf.Jobs["deploy"]
	if job.Status != store.JobStatusWaiting {
		t.Fatalf("job status = %q, want waiting (wait-timer-only env must gate the deployment)", job.Status)
	}
	if wf.Status != store.WorkflowStatusWaiting {
		t.Fatalf("workflow status = %q, want waiting", wf.Status)
	}
	if len(wf.PendingDeployments) != 1 {
		t.Fatalf("recorded %d pending deployments, want 1 for the wait-timer env", len(wf.PendingDeployments))
	}
	if got := wf.PendingDeployments[0].WaitTimerStartedAt; !got.Equal(now) {
		t.Fatalf("WaitTimerStartedAt = %v, want the engine clock %v (must use the controllable clock, not time.Now)", got, now)
	}

	// A tick before the timer elapses must NOT release the job.
	now = now.Add(4 * time.Minute)
	e.ReleaseElapsedWaitTimers(context.Background())
	if job.Status != store.JobStatusWaiting {
		t.Fatalf("job released after 4m, want still waiting (timer is 5m)")
	}

	// Past the 5-minute timer, the tick auto-approves the environment and releases the job into admission.
	now = now.Add(2 * time.Minute) // 6m total
	e.ReleaseElapsedWaitTimers(context.Background())
	if job.Status == store.JobStatusWaiting {
		t.Fatalf("job still waiting after the 5m timer elapsed, want released")
	}
	if len(wf.PendingDeployments) != 0 {
		t.Fatalf("pending deployment not cleared after wait-timer release (%d remain)", len(wf.PendingDeployments))
	}
	if !envApproved(wf, "prod") {
		t.Fatalf("elapsed wait timer should record an approved EnvApproval so the gate does not re-hold the job")
	}
}
