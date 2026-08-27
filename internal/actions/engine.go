// Package actions is the GitHub Actions execution engine: workflow
// submission, the run/job state machine, expression evaluation, cron
// scheduling, concurrency admission, and the checks/webhook event fan-out.
//
// It depends on internal/store and internal/gitstore, never on
// internal/server; HTTP-layer needs arrive through [Config] seams so the
// compiler enforces the layering (ARCH-002).
package actions

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/e6qu/bleephub/internal/store"
)

// Metrics records engine lifecycle counters; satisfied by *bleephub.Metrics.
type Metrics interface {
	RecordWorkflowSubmit()
	RecordWorkflowComplete()
	RecordJobDispatch()
	RecordJobCompletion(job *store.WorkflowJob)
}

// EventSink renders engine lifecycle transitions as webhook events.
// Snapshot pointers are immutable copies captured at transition time; the
// sink may read them lock-free.
type EventSink interface {
	WorkflowRunEvent(wf *store.Workflow, action string)
	WorkflowJobEvent(wf *store.Workflow, job *store.WorkflowJob, action string)
	CheckRunEvent(repoKey string, checkRunID int64, action string)
	CheckSuiteEvent(repoKey string, suiteID int64, action string)
}

// Config carries the engine's dependencies and injected seams.
type Config struct {
	Store     *store.Store
	Artifacts *store.ArtifactStore
	Logger    zerolog.Logger
	Metrics   Metrics
	// Addr is the server's listen address (http://<addr>), used to derive
	// run-detail and triggered-run URLs.
	Addr string
	// Events receives lifecycle transitions for webhook/check rendering.
	Events EventSink
	// MintJobToken mints the per-job GITHUB_TOKEN. It stays in the server
	// because minting and verification share the runner MAC key; signature
	// drift would break every runner job at lease time.
	MintJobToken func(scopeID string, wf *store.Workflow, jd *store.JobDef) string
	// RepoEventPayload renders the webhook `repository` block for the
	// schedule trigger's github.event context.
	RepoEventPayload func(repo *store.Repo) map[string]interface{}
	// Now is the engine's clock.
	Now func() time.Time
	// Go runs a goroutine the server tracks for shutdown draining.
	Go func(func())
	// OnScheduleTick hooks run once per minute tick, before due schedules
	// fire.
	OnScheduleTick []func(time.Time)
	// CompletedJobRetention bounds how long a completed job's
	// replica-local runtime state stays addressable; zero retires jobs
	// immediately.
	CompletedJobRetention time.Duration
}

// Engine owns the Actions state machine and its process-local state.
type Engine struct {
	store                 *store.Store
	artifactStore         *store.ArtifactStore
	logger                zerolog.Logger
	metrics               Metrics
	addr                  string
	sink                  EventSink
	mintJobToken          func(scopeID string, wf *store.Workflow, jd *store.JobDef) string
	repoEventPayload      func(repo *store.Repo) map[string]interface{}
	now                   func() time.Time
	spawn                 func(func())
	onScheduleTick        []func(time.Time)
	completedJobRetention time.Duration

	scheduleFired         scheduleFiredKeys // cron-firing dedup
	scheduleIndex         scheduleIndex     // parsed-schedule cache keyed by default-branch tip
	actionsEvents         actionsEventLoop  // checks/webhook fan-out
	workflowConcurrencyMu sync.Mutex        // serializes concurrency-group admission and promotion
	workflowTimeoutMu     sync.Mutex        // serializes timeout watcher replacement and cancellation
}

// NewEngine builds an engine from its wired dependencies. It panics when a
// required seam is nil, surfacing the wiring error up front rather than as
// an arbitrary panic mid-dispatch; seams with a safe nil zero value stay
// optional.
func NewEngine(cfg Config) *Engine {
	if cfg.Events == nil {
		panic("actions.NewEngine: Config.Events is nil — the engine emits every run/job/check lifecycle transition through it; wire the server's event sink or a no-op stub")
	}
	if cfg.MintJobToken == nil {
		panic("actions.NewEngine: Config.MintJobToken is nil — every dispatched job's GITHUB_TOKEN is minted through it at lease time; wire the server's minter or a stub")
	}
	if cfg.Go == nil {
		panic("actions.NewEngine: Config.Go is nil — every engine background goroutine is spawned through it; wire the server's tracked spawner or func(fn func()) { go fn() }")
	}
	if cfg.RepoEventPayload == nil {
		panic("actions.NewEngine: Config.RepoEventPayload is nil — the schedule trigger renders github.event.repository through it when a cron fires; wire the server's repo payload builder or a stub")
	}
	return &Engine{
		store:                 cfg.Store,
		artifactStore:         cfg.Artifacts,
		logger:                cfg.Logger,
		metrics:               cfg.Metrics,
		addr:                  cfg.Addr,
		sink:                  cfg.Events,
		mintJobToken:          cfg.MintJobToken,
		repoEventPayload:      cfg.RepoEventPayload,
		now:                   cfg.Now,
		spawn:                 cfg.Go,
		onScheduleTick:        cfg.OnScheduleTick,
		completedJobRetention: cfg.CompletedJobRetention,
	}
}

// Start launches the schedule dispatcher and retired-job janitor; ctx
// cancellation stops both at shutdown. It also installs the stop watcher for
// the lazily-started event drain: on cancellation the watcher flags the loop
// stopped and wakes it so the drain flushes and exits instead of leaking
// past background.Wait (ACT-100).
func (s *Engine) Start(ctx context.Context) {
	s.startScheduleDispatcher(ctx)
	s.startActionsJanitor(ctx)
	s.goBackground(func() {
		<-ctx.Done()
		s.actionsEvents.mu.Lock()
		s.actionsEvents.stopped = true
		// cond is nil only if no event was ever queued (nothing to wake).
		if s.actionsEvents.cond != nil {
			s.actionsEvents.cond.Broadcast()
		}
		s.actionsEvents.mu.Unlock()
	})
}

func (s *Engine) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// goBackground runs fn as a server-owned goroutine that shutdown waits for.
func (s *Engine) goBackground(fn func()) {
	s.spawn(fn)
}
