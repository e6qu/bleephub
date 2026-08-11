package store

import (
	"strconv"
	"time"
)

// WorkflowStatus is the lifecycle state of a [Workflow].
type WorkflowStatus string

const (
	WorkflowStatusRunning            WorkflowStatus = "running"
	WorkflowStatusCompleted          WorkflowStatus = "completed"
	WorkflowStatusPendingConcurrency WorkflowStatus = "pending_concurrency"
	// WorkflowStatusWaiting holds runs whose environment-targeting jobs
	// await a deployment review (required reviewers on the environment).
	WorkflowStatusWaiting WorkflowStatus = "waiting"
	// WorkflowStatusActionRequired holds runs triggered by a pull request
	// from a fork when the repository's fork-PR contributor-approval
	// policy requires a maintainer to approve the run before any job
	// dispatches (POST .../runs/{run_id}/approve releases it).
	WorkflowStatusActionRequired WorkflowStatus = "action_required"
)

// JobStatus is the lifecycle state of a [WorkflowJob].
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusQueued    JobStatus = "queued"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusSkipped   JobStatus = "skipped"
	// JobStatusWaiting holds jobs targeting a reviewer-protected
	// environment until the run's pending deployment is approved.
	JobStatusWaiting JobStatus = "waiting"
)

// Result is the terminal outcome of a workflow or job. The empty value
// means in-flight (no outcome yet).
type Result string

const (
	ResultNone      Result = ""
	ResultSuccess   Result = "success"
	ResultFailure   Result = "failure"
	ResultCancelled Result = "cancelled"
	ResultSkipped   Result = "skipped"
	// ResultStartupFailure marks runs that never produced jobs because
	// the workflow failed at startup (invalid reusable-workflow ref,
	// unparseable definition) — real GitHub's conclusion for these.
	ResultStartupFailure Result = "startup_failure"
)

// Workflow represents a running multi-job workflow.
type Workflow struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	DisplayTitle string                  `json:"displayTitle,omitempty"`
	RunID        int                     `json:"runId"`
	RunNumber    int                     `json:"runNumber"`
	Jobs         map[string]*WorkflowJob `json:"jobs"`
	Env          map[string]string       `json:"env,omitempty"`
	Permissions  PermissionDef           `json:"permissions,omitempty"`
	Status       WorkflowStatus          `json:"status"` // "running", "completed", "pending_concurrency"
	// PendingDeployments holds one record per reviewer-protected
	// environment the run is waiting on; EnvApprovals records every
	// approve/reject review submitted for the run.
	PendingDeployments []*PendingDeployment `json:"pendingDeployments,omitempty"`
	EnvApprovals       []*EnvApproval       `json:"envApprovals,omitempty"`
	Result             Result               `json:"result"` // "success", "failure", "cancelled"
	CreatedAt          time.Time            `json:"createdAt"`
	MaxParallel        int                  `json:"-"` // compatibility fallback for directly-constructed runs
	MatrixMaxParallel  map[string]int       `json:"-"`
	CancelTimeout      func()               `json:"-"` // stops the timeout watcher goroutine
	EventName          string               `json:"eventName,omitempty"`
	Ref                string               `json:"ref,omitempty"`
	Sha                string               `json:"sha,omitempty"`
	RepoFullName       string               `json:"repoFullName,omitempty"`
	Inputs             map[string]string    `json:"inputs,omitempty"`
	ConcurrencyGroup   string               `json:"concurrencyGroup,omitempty"`
	CancelInProgress   bool                 `json:"-"`
	// ConcurrencyAcquiredAt records when this run took its concurrency
	// group's lease (started running while holding the group); zero for
	// runs without a group or still queued behind the group.
	ConcurrencyAcquiredAt time.Time `json:"-"`
	// Attempt is the 1-based run_attempt; zero means first attempt
	// (reruns bump it and archive the prior attempt in
	// Store.WorkflowAttempts).
	Attempt int `json:"attempt,omitempty"`
	// CancelRequested marks a run whose cancellation was requested;
	// in-flight jobs are winding down and always()/cancelled() jobs may
	// still dispatch. The run finalizes with conclusion cancelled.
	CancelRequested bool `json:"-"`
	// EventPayload is the triggering webhook payload (github.event).
	// In-flight runs aren't persisted, so neither is this.
	EventPayload map[string]interface{} `json:"-"`
	// TypedInputs is the typed `inputs` expression context (boolean /
	// number inputs carry real types); Inputs keeps the string forms.
	TypedInputs map[string]interface{} `json:"-"`

	// WorkflowFileID / WorkflowFilePath identify the originating workflow
	// FILE (the YAML on disk), which is stable across every run produced
	// from it. GitHub's WorkflowRun.workflow_id and .path reference the
	// file, not the run, so these must be carried separately from RunID.
	// Populated at submit/dispatch time by resolving the registered
	// [WorkflowFile] for (repo, name); zero/"" when no backing file is
	// known yet (resolved lazily in workflowRunJSON).
	WorkflowFileID   int64  `json:"workflowFileId,omitempty"`
	WorkflowFilePath string `json:"workflowFilePath,omitempty"`
	CheckSuiteID     int64  `json:"checkSuiteId,omitempty"`
}

// WorkflowJob represents a single job within a workflow.
type WorkflowJob struct {
	Key             string                 `json:"key"`   // YAML key
	JobID           string                 `json:"jobId"` // UUID, used as Job.ID
	PlanID          string                 `json:"planId,omitempty"`
	DisplayName     string                 `json:"displayName"`
	Needs           []string               `json:"needs,omitempty"`
	Status          JobStatus              `json:"status"` // "pending", "queued", "running", "completed", "skipped"
	Result          Result                 `json:"result"` // "success", "failure", "cancelled", "skipped"
	Outputs         map[string]string      `json:"outputs,omitempty"`
	MatrixValues    map[string]interface{} `json:"matrix,omitempty"`
	ContinueOnError bool                   `json:"continueOnError,omitempty"`
	QueuedAt        time.Time              `json:"queuedAt,omitempty"`
	StartedAt       time.Time              `json:"startedAt,omitempty"`
	CompletedAt     time.Time              `json:"completedAt,omitempty"`
	MatrixGroup     string                 `json:"matrixGroup,omitempty"`
	Summary         string                 `json:"summary,omitempty"`
	Def             *JobDef                `json:"-"`
	// Hidden marks synthetic reusable-workflow gate/collector nodes the
	// jobs API never lists (real GitHub shows only the called jobs).
	Hidden bool `json:"hidden,omitempty"`
	// CheckRunID links the job to the check run mirroring it.
	CheckRunID int64 `json:"checkRunId,omitempty"`
	// ConcurrencyGroup is the evaluated jobs.<id>.concurrency.group. It is
	// persisted separately from Def so a waiting job survives a restart
	// without re-evaluating against changed dependency outputs.
	ConcurrencyGroup string `json:"concurrencyGroup,omitempty"`
	CancelInProgress bool   `json:"cancelInProgress,omitempty"`
}

func (wf *Workflow) RunDisplayTitle() string {
	if wf.DisplayTitle != "" {
		return wf.DisplayTitle
	}
	return wf.Name
}

func normalizeReloadedWorkflow(wf *Workflow) {
	if wf == nil || wf.Status == WorkflowStatusCompleted {
		return
	}
	now := time.Now().UTC()
	wf.Status = WorkflowStatusCompleted
	wf.Result = ResultCancelled
	wf.CancelRequested = true
	for _, job := range wf.Jobs {
		switch job.Status {
		case JobStatusCompleted, JobStatusSkipped:
			continue
		default:
			job.Status = JobStatusCompleted
			job.Result = ResultCancelled
			if job.CompletedAt.IsZero() {
				job.CompletedAt = now
			}
		}
	}
}

// AttemptNumber returns the 1-based run_attempt (the zero value is the
// first attempt).
func (wf *Workflow) AttemptNumber() int {
	if wf.Attempt < 1 {
		return 1
	}
	return wf.Attempt
}

func (st *Store) PersistWorkflowRecord(wf *Workflow) {
	// Every engine mutation site already funnels through here under the store
	// write lock, so this is also where the derived run-id/concurrency-group
	// indexes are kept in step with the workflow's state.
	st.SyncWorkflowIndexesLocked(wf)
	if st.Persist != nil && wf != nil {
		st.Persist.MustPut("workflows", wf.ID, wf)
	}
}

func (st *Store) DeleteWorkflowRecord(id string) {
	if st.Persist != nil && id != "" {
		st.Persist.MustDelete("workflows", id)
	}
}

func (st *Store) PersistWorkflowAttemptsRecord(runID int) {
	if st.Persist == nil || runID == 0 {
		return
	}
	attempts := st.WorkflowAttempts[runID]
	if len(attempts) == 0 {
		st.Persist.MustDelete("workflow_attempts", strconv.Itoa(runID))
		return
	}
	st.Persist.MustPut("workflow_attempts", strconv.Itoa(runID), attempts)
}

// EnvApproval is one submitted deployment review (approve or reject).
type EnvApproval struct {
	State     string    `json:"state"` // approved | rejected
	Comment   string    `json:"comment"`
	UserID    int       `json:"userId"`
	EnvIDs    []int     `json:"envIds"`
	EnvNames  []string  `json:"envNames"`
	CreatedAt time.Time `json:"createdAt"`
}

// PendingDeployment is one reviewer-protected environment a run waits on.
type PendingDeployment struct {
	EnvID              int       `json:"envId"`
	EnvName            string    `json:"envName"`
	WaitTimerStartedAt time.Time `json:"waitTimerStartedAt"`
}
