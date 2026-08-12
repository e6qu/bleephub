package actions

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/e6qu/bleephub/internal/store"
)

// countingSink records every workflow_job event the drain emits. jobEvents is
// the total; each emission is also signalled on notify so tests can
// synchronize on "the drain has processed event N" without sleeping.
type countingSink struct {
	jobEvents atomic.Int64
	notify    chan struct{}
}

func (s *countingSink) WorkflowRunEvent(*store.Workflow, string) {}
func (s *countingSink) WorkflowJobEvent(*store.Workflow, *store.WorkflowJob, string) {
	s.jobEvents.Add(1)
	s.notify <- struct{}{}
}
func (s *countingSink) CheckRunEvent(string, int64, string)   {}
func (s *countingSink) CheckSuiteEvent(string, int64, string) {}

// newShutdownTestEngine builds an engine whose Go seam tracks every background
// goroutine in a WaitGroup, exactly like the server's shutdown-owned spawner,
// so tests can perform the same join the server performs at shutdown.
func newShutdownTestEngine(sink *countingSink) (*Engine, *sync.WaitGroup) {
	var wg sync.WaitGroup
	e := NewEngine(Config{
		Store:  store.NewStore(),
		Logger: zerolog.Nop(),
		Addr:   "127.0.0.1:0",
		Events: sink,
		MintJobToken: func(scopeID string, wf *store.Workflow, jd *store.JobDef) string {
			return "test-job-token"
		},
		RepoEventPayload: func(repo *store.Repo) map[string]interface{} {
			return map[string]interface{}{"full_name": repo.FullName}
		},
		Now: func() time.Time { return fixedEngineTestTime },
		Go: func(fn func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fn()
			}()
		},
		CompletedJobRetention: 6 * time.Hour,
	})
	return e, &wg
}

// ACT-100: the event drain must be a shutdown-owned goroutine that flushes
// every already-queued event before background.Wait returns — cancelling the
// Start ctx with events still pending must not drop the tail.
func TestActionsEventDrainFlushesQueueOnShutdown(t *testing.T) {
	const queued = 100
	sink := &countingSink{notify: make(chan struct{}, queued)}
	e, wg := newShutdownTestEngine(sink)

	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)

	wf := &store.Workflow{ID: "wf-shutdown-flush", RepoFullName: "owner/repo"}
	job := &store.WorkflowJob{JobID: "job-1", DisplayName: "build"}
	for i := 0; i < queued; i++ {
		e.QueueEvent(EvJobQueued, wf, job)
	}

	// Cancel with (potentially) events still queued, then perform the same
	// join the server's shutdown performs: wait for every tracked goroutine.
	cancel()
	wg.Wait()

	if got := sink.jobEvents.Load(); got != queued {
		t.Fatalf("drain dropped queued events at shutdown: processed %d of %d", got, queued)
	}
}

// ACT-100: a drain that is idle (empty queue) when the Start ctx is cancelled
// must wake from cond.Wait and return promptly, so the server's shutdown join
// cannot deadlock on it.
func TestActionsEventDrainStopsPromptlyWhenIdle(t *testing.T) {
	sink := &countingSink{notify: make(chan struct{}, 1)}
	e, wg := newShutdownTestEngine(sink)

	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)

	// One event lazily starts the drain; receiving its emission proves the
	// drain is past processing and idle (or about to re-check its predicate).
	e.QueueEvent(EvJobQueued, &store.Workflow{ID: "wf-idle", RepoFullName: "owner/repo"}, &store.WorkflowJob{JobID: "job-1"})
	<-sink.notify

	cancel()
	wg.Wait() // must return: the woken drain sees stopped + empty queue and exits

	if got := sink.jobEvents.Load(); got != 1 {
		t.Fatalf("expected exactly 1 processed event, got %d", got)
	}
}
