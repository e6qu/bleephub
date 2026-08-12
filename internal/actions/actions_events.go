package actions

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// EventKind enumerates workflow/job lifecycle transitions that
// drive the checks layer and its webhook events.
type EventKind int

const (
	EvRunRequested EventKind = iota
	EvRunCompleted
	EvJobQueued
	EvJobInProgress
	EvJobWaiting
	EvJobCompleted
)

// actionsEvent is one queued lifecycle transition. The live pointers are used
// only for check-state mutations; webhook payloads render the immutable
// snapshots captured when the transition occurred.
type actionsEvent struct {
	kind        EventKind
	wf          *store.Workflow
	job         *store.WorkflowJob
	wfSnapshot  *store.Workflow
	jobSnapshot *store.WorkflowJob
}

// actionsEventLoop fans workflow/job transitions out to the checks
// layer: check suites/runs mirror every visible job, and the
// workflow_run / workflow_job / check_run / check_suite webhook events
// fire exactly where real GitHub fires them. Events queue from inside
// the engine's store-locked sections; the drain goroutine does all
// store/webhook work outside those locks.
type actionsEventLoop struct {
	once sync.Once
	mu   sync.Mutex
	cond *sync.Cond
	// snapshotMu guards the struct-value copies in CloneWorkflowEventSnapshot
	// and CloneWorkflowJobEventSnapshot against concurrent field writes by the
	// drain goroutine (CheckSuiteID on the workflow, CheckRunID on jobs).
	// Lock order is always store.mu before snapshotMu.
	snapshotMu sync.RWMutex
	queue      []actionsEvent
	// stopped (guarded by mu) tells the drain goroutine to exit once the
	// queue is empty. Engine.Start's shutdown watcher sets it on ctx
	// cancellation; the drain flushes every already-queued event first so
	// shutdown never drops a tail of pending check/webhook events (ACT-100).
	stopped bool
}

func CloneWorkflowEventSnapshot(wf *store.Workflow) *store.Workflow {
	if wf == nil {
		return nil
	}
	snapshot := *wf
	snapshot.Jobs = nil
	snapshot.Env = cloneStringMap(wf.Env)
	snapshot.Inputs = cloneStringMap(wf.Inputs)
	snapshot.TypedInputs = cloneActionsMap(wf.TypedInputs)
	snapshot.EventPayload = cloneActionsMap(wf.EventPayload)
	return &snapshot
}

func CloneWorkflowJobEventSnapshot(job *store.WorkflowJob) *store.WorkflowJob {
	if job == nil {
		return nil
	}
	snapshot := *job
	snapshot.Needs = append([]string(nil), job.Needs...)
	snapshot.Outputs = cloneStringMap(job.Outputs)
	snapshot.MatrixValues = cloneActionsMap(job.MatrixValues)
	return &snapshot
}

// cloneStringMap copies a string map so a snapshot cannot alias live state.
func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneActionsMap(values map[string]interface{}) map[string]interface{} {
	if values == nil {
		return nil
	}
	cloned := make(map[string]interface{}, len(values))
	for key, value := range values {
		cloned[key] = cloneActionsValue(value)
	}
	return cloned
}

func cloneActionsValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return cloneActionsMap(typed)
	case []interface{}:
		cloned := make([]interface{}, len(typed))
		for index, item := range typed {
			cloned[index] = cloneActionsValue(item)
		}
		return cloned
	default:
		return typed
	}
}

// QueueEvent snapshots and enqueues a transition. It is safe to call
// while holding the store lock: admission takes only the event-loop mutex and
// never waits for the consumer.
func (s *Engine) QueueEvent(kind EventKind, wf *store.Workflow, job *store.WorkflowJob) {
	if wf == nil || wf.RepoFullName == "" {
		return // runs without a repository have no checks surface
	}
	if job != nil && job.Hidden {
		return // synthetic reusable-workflow nodes have no checks
	}
	s.actionsEvents.once.Do(func() {
		s.actionsEvents.cond = sync.NewCond(&s.actionsEvents.mu)
		s.goBackground(s.drainActionsEvents)
	})
	s.actionsEvents.snapshotMu.RLock()
	event := actionsEvent{
		kind:        kind,
		wf:          wf,
		job:         job,
		wfSnapshot:  CloneWorkflowEventSnapshot(wf),
		jobSnapshot: CloneWorkflowJobEventSnapshot(job),
	}
	s.actionsEvents.snapshotMu.RUnlock()
	s.actionsEvents.mu.Lock()
	s.actionsEvents.queue = append(s.actionsEvents.queue, event)
	s.actionsEvents.cond.Signal()
	s.actionsEvents.mu.Unlock()
}

func (s *Engine) drainActionsEvents() {
	runInProgress := map[string]bool{} // workflow UUID → workflow_run in_progress emitted
	for {
		s.actionsEvents.mu.Lock()
		for len(s.actionsEvents.queue) == 0 && !s.actionsEvents.stopped {
			s.actionsEvents.cond.Wait()
		}
		if len(s.actionsEvents.queue) == 0 && s.actionsEvents.stopped {
			// Shutdown: every already-queued event has been processed.
			s.actionsEvents.mu.Unlock()
			return
		}
		ev := s.actionsEvents.queue[0]
		s.actionsEvents.queue[0] = actionsEvent{}
		s.actionsEvents.queue = s.actionsEvents.queue[1:]
		if len(s.actionsEvents.queue) == 0 {
			s.actionsEvents.queue = nil
		}
		s.actionsEvents.mu.Unlock()

		switch ev.kind {
		case EvRunRequested:
			s.OnActionsRunRequestedSnapshot(ev.wf, ev.wfSnapshot)
		case EvRunCompleted:
			delete(runInProgress, ev.wf.ID)
			s.OnActionsRunCompletedSnapshot(ev.wf, ev.wfSnapshot)
		case EvJobQueued:
			s.sink.WorkflowJobEvent(ev.wfSnapshot, ev.jobSnapshot, "queued")
		case EvJobWaiting:
			s.sink.WorkflowJobEvent(ev.wfSnapshot, ev.jobSnapshot, "waiting")
		case EvJobInProgress:
			s.updateJobCheckRun(ev.wf, ev.job, "in_progress", "")
			if !runInProgress[ev.wf.ID] {
				runInProgress[ev.wf.ID] = true
				s.sink.WorkflowRunEvent(ev.wfSnapshot, "in_progress")
			}
			s.sink.WorkflowJobEvent(ev.wfSnapshot, ev.jobSnapshot, "in_progress")
		case EvJobCompleted:
			s.completeJobCheckRun(ev.wf, ev.job)
			s.sink.WorkflowJobEvent(ev.wfSnapshot, ev.jobSnapshot, "completed")
		}
	}
}

// OnActionsRunRequestedSnapshot creates the run's check suite plus one check
// run per visible job, then emits check_suite + workflow_run "requested"
// events from the immutable transition snapshot queued by the scheduler.
func (s *Engine) OnActionsRunRequestedSnapshot(wf, snapshot *store.Workflow) {
	repoKey := wf.RepoFullName
	branch := refShortName(wf.Ref)

	suite := s.store.CreateCheckSuite(repoKey, branch, wf.Sha, store.GithubActionsAppID)
	s.store.UpdateCheckSuite(suite.ID, func(cs *store.CheckSuite) {
		cs.WorkflowRunID = wf.RunID
		cs.WorkflowRunBackendID = wf.ID
		cs.WorkflowName = wf.Name
		cs.WorkflowFileID = wf.WorkflowFileID
		cs.WorkflowFilePath = wf.WorkflowFilePath
	})
	s.store.Mu.Lock()
	s.actionsEvents.snapshotMu.Lock()
	wf.CheckSuiteID = suite.ID
	s.actionsEvents.snapshotMu.Unlock()
	s.store.PersistWorkflowRecord(wf)
	s.store.Mu.Unlock()

	s.store.Mu.RLock()
	jobs := make([]*store.WorkflowJob, 0, len(wf.Jobs))
	for _, j := range wf.Jobs {
		if !j.Hidden {
			jobs = append(jobs, j)
		}
	}
	s.store.Mu.RUnlock()

	for _, j := range jobs {
		cr := s.store.CreateCheckRun(repoKey, wf.Sha, j.DisplayName, store.GithubActionsAppID, suite.ID)
		s.store.Mu.RLock()
		jobStatus, jobResult := j.Status, j.Result
		s.store.Mu.RUnlock()
		now := time.Now().UTC()
		s.store.UpdateCheckRun(cr.ID, func(c *store.CheckRun) {
			c.ExternalID = j.JobID
			c.DetailsURL = fmt.Sprintf("http://%s/%s/actions/runs/%d", s.addr, repoKey, wf.RunID)
			// Jobs carried over from a previous attempt arrive already
			// terminal; their check runs reflect that immediately.
			if jobStatus == store.JobStatusCompleted || jobStatus == store.JobStatusSkipped {
				c.Status = "completed"
				c.Conclusion = resultToConclusion(jobResult)
				c.CompletedAt = &now
			}
		})
		s.store.Mu.Lock()
		s.actionsEvents.snapshotMu.Lock()
		j.CheckRunID = cr.ID
		s.actionsEvents.snapshotMu.Unlock()
		s.store.Mu.Unlock()
		s.sink.CheckRunEvent(repoKey, cr.ID, "created")
	}

	s.sink.CheckSuiteEvent(repoKey, suite.ID, "requested")
	s.sink.WorkflowRunEvent(snapshot, "requested")
}

// OnActionsRunCompletedSnapshot rolls the suite up and emits completed events
// from the immutable transition snapshot queued by the scheduler.
func (s *Engine) OnActionsRunCompletedSnapshot(wf, snapshot *store.Workflow) {
	repoKey := wf.RepoFullName

	s.store.Mu.RLock()
	suiteID := wf.CheckSuiteID
	for _, j := range wf.Jobs {
		if j.CheckRunID != 0 {
			if cr := s.store.CheckRuns[j.CheckRunID]; cr != nil {
				suiteID = cr.SuiteID
				break
			}
		}
	}
	result := wf.Result
	s.store.Mu.RUnlock()

	if suiteID != 0 {
		now := time.Now().UTC()
		s.store.Mu.Lock()
		if suite := s.store.CheckSuites[suiteID]; suite != nil {
			suite.Status = "completed"
			suite.Conclusion = resultToConclusion(result)
			suite.UpdatedAt = now
			if s.store.Persist != nil {
				s.store.Persist.MustPut("check_suites", strconv.FormatInt(suiteID, 10), suite)
			}
		}
		s.store.Mu.Unlock()
		s.sink.CheckSuiteEvent(repoKey, suiteID, "completed")
	}
	s.sink.WorkflowRunEvent(snapshot, "completed")
}

// updateJobCheckRun moves a job's check run to a new status.
func (s *Engine) updateJobCheckRun(wf *store.Workflow, job *store.WorkflowJob, status, conclusion string) {
	id := jobCheckRunID(s, job)
	if id == 0 {
		return
	}
	s.store.UpdateCheckRun(id, func(c *store.CheckRun) {
		c.Status = status
		if conclusion != "" {
			c.Conclusion = conclusion
		}
	})
}

// completeJobCheckRun finishes a job's check run with the job's result
// and emits check_run completed.
func (s *Engine) completeJobCheckRun(wf *store.Workflow, job *store.WorkflowJob) {
	id := jobCheckRunID(s, job)
	if id == 0 {
		return
	}
	s.store.Mu.RLock()
	result := job.Result
	s.store.Mu.RUnlock()
	now := time.Now().UTC()
	s.store.UpdateCheckRun(id, func(c *store.CheckRun) {
		c.Status = "completed"
		c.Conclusion = resultToConclusion(result)
		c.CompletedAt = &now
	})
	s.sink.CheckRunEvent(wf.RepoFullName, id, "completed")
}

func jobCheckRunID(s *Engine, job *store.WorkflowJob) int64 {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return job.CheckRunID
}

// resultToConclusion maps an engine Result onto GitHub's check
// conclusion vocabulary.
func resultToConclusion(r store.Result) string {
	switch r {
	case store.ResultSuccess:
		return "success"
	case store.ResultFailure:
		return "failure"
	case store.ResultCancelled:
		return "cancelled"
	case store.ResultSkipped:
		return "skipped"
	case store.ResultStartupFailure:
		return "startup_failure"
	default:
		return ""
	}
}

// refShortName trims refs/heads/ / refs/tags/ to the short name.
func refShortName(ref string) string {
	switch {
	case len(ref) > 11 && ref[:11] == "refs/heads/":
		return ref[11:]
	case len(ref) > 10 && ref[:10] == "refs/tags/":
		return ref[10:]
	default:
		return ref
	}
}
