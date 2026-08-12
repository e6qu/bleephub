// Package actions is the GitHub Actions execution engine: workflow
// submission and job dispatch, the run/job state machine, matrix and
// reusable-workflow expansion, expression evaluation, trigger matching,
// cron scheduling, broker message queuing, concurrency admission, the
// checks/webhook event fan-out, and the retired-job janitor.
//
// The package depends on internal/store (data layer) and internal/gitstore
// only — never on internal/server. Everything the engine needs from the
// HTTP layer (webhook payload emission, GITHUB_TOKEN minting, the test
// clock, owned-goroutine tracking) is injected through [Config] seams, so
// the compiler enforces the layering (ARCH-002).
package actions

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/e6qu/bleephub/internal/store"
)

// Metrics is the engine's view of the server's metrics recorder,
// satisfied by *bleephub.Metrics.
type Metrics interface {
	RecordWorkflowSubmit()
	RecordWorkflowComplete()
	RecordJobDispatch()
	RecordJobCompletion(job *store.WorkflowJob)
}

// EventSink receives the engine's lifecycle transitions and renders them
// as webhook events (workflow_run / workflow_job / check_run /
// check_suite). The server implements it with its payload builders —
// which need the repo/user/checks JSON renderers that stay in the HTTP
// layer. Snapshot pointers passed here are immutable copies captured at
// transition time; the sink may read them lock-free.
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
	// Addr is the server's listen address, used where the engine derives a
	// URL for run details / triggered-run submission exactly as the HTTP
	// layer would (http://<addr>).
	Addr string
	// Events receives lifecycle transitions for webhook/check rendering.
	Events EventSink
	// MintJobToken mints the per-job GITHUB_TOKEN. Its implementation
	// stays in the server: minting and verification share the runner MAC
	// key, and signature drift would break every runner job at lease time.
	MintJobToken func(scopeID string, wf *store.Workflow, jd *store.JobDef) string
	// RepoEventPayload renders the webhook `repository` block for the
	// schedule trigger's github.event context (a server-side payload shape).
	RepoEventPayload func(repo *store.Repo) map[string]interface{}
	// Now is the engine's clock (the server's fixture-aware currentTime).
	Now func() time.Time
	// Go runs a goroutine the server tracks for shutdown draining.
	Go func(func())
	// OnScheduleTick hooks run once per minute tick of the schedule
	// dispatcher, before due schedules fire (the server hangs its
	// login-session reaping and org-invitation reconciliation here).
	OnScheduleTick []func(time.Time)
	// CompletedJobRetention is how long a completed job's replica-local
	// runtime state stays addressable (the server passes its runner token
	// TTL: nothing valid can name the job after that).
	CompletedJobRetention time.Duration
}

// Engine owns the Actions execution state machine and its process-local
// state (concurrency admission, timeout watchers, the schedule
// dispatcher's dedup + cache, and the checks/webhook event loop).
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

	scheduleFired         scheduleFiredKeys // cron-firing dedup (on: schedule)
	scheduleIndex         scheduleIndex     // parsed-schedule cache keyed by default-branch tip (on: schedule)
	actionsEvents         actionsEventLoop  // checks/webhook fan-out for run+job transitions
	workflowConcurrencyMu sync.Mutex        // serializes concurrency-group admission and queue promotion
	workflowTimeoutMu     sync.Mutex        // serializes timeout watcher replacement and cancellation
}

// NewEngine builds an engine from its wired dependencies. It panics when a
// required seam is nil: each of these is dereferenced without a nil guard on
// a runtime path (event fan-out, job-token minting at lease time, schedule
// firing, goroutine spawning), so a missing seam would otherwise surface as
// an arbitrary panic mid-dispatch instead of at wiring time. Seams with a
// safe nil zero value stay optional: Metrics and Now are nil-guarded at
// every use, OnScheduleTick is a range-able slice, and CompletedJobRetention
// zero simply retires jobs immediately.
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

// Start launches the engine's background loops — the minute-aligned
// schedule dispatcher and the retired-job janitor — for the server's
// lifetime; ctx cancellation stops both at shutdown.
func (s *Engine) Start(ctx context.Context) {
	s.startScheduleDispatcher(ctx)
	s.startActionsJanitor(ctx)
}

// currentTime is the engine's fixture-aware clock.
func (s *Engine) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// goBackground runs fn as a server-owned goroutine: shutdown waits for it.
func (s *Engine) goBackground(fn func()) {
	s.spawn(fn)
}
