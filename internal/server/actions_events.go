package bleephub

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// githubActionsAppID is the well-known app id real GitHub attributes
// Actions check suites/runs to (the "github-actions" app).
const githubActionsAppID = 15368

// actionsEventKind enumerates workflow/job lifecycle transitions that
// drive the checks layer and its webhook events.
type actionsEventKind int

const (
	evRunRequested actionsEventKind = iota
	evRunCompleted
	evJobQueued
	evJobInProgress
	evJobWaiting
	evJobCompleted
)

// actionsEvent is one queued lifecycle transition. The live pointers are used
// only for check-state mutations; webhook payloads render the immutable
// snapshots captured when the transition occurred.
type actionsEvent struct {
	kind        actionsEventKind
	wf          *Workflow
	job         *WorkflowJob
	wfSnapshot  *Workflow
	jobSnapshot *WorkflowJob
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
	// snapshotMu guards the struct-value copies in cloneWorkflowEventSnapshot
	// and cloneWorkflowJobEventSnapshot against concurrent field writes by the
	// drain goroutine (CheckSuiteID on the workflow, CheckRunID on jobs).
	// Lock order is always store.mu before snapshotMu.
	snapshotMu sync.RWMutex
	queue      []actionsEvent
}

func cloneWorkflowEventSnapshot(wf *Workflow) *Workflow {
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

func cloneWorkflowJobEventSnapshot(job *WorkflowJob) *WorkflowJob {
	if job == nil {
		return nil
	}
	snapshot := *job
	snapshot.Needs = append([]string(nil), job.Needs...)
	snapshot.Outputs = cloneStringMap(job.Outputs)
	snapshot.MatrixValues = cloneActionsMap(job.MatrixValues)
	return &snapshot
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

// queueActionsEvent snapshots and enqueues a transition. It is safe to call
// while holding the store lock: admission takes only the event-loop mutex and
// never waits for the consumer.
func (s *Server) queueActionsEvent(kind actionsEventKind, wf *Workflow, job *WorkflowJob) {
	if wf == nil || wf.RepoFullName == "" {
		return // runs without a repository have no checks surface
	}
	if job != nil && job.Hidden {
		return // synthetic reusable-workflow nodes have no checks
	}
	s.actionsEvents.once.Do(func() {
		s.actionsEvents.cond = sync.NewCond(&s.actionsEvents.mu)
		go s.drainActionsEvents()
	})
	s.actionsEvents.snapshotMu.RLock()
	event := actionsEvent{
		kind:        kind,
		wf:          wf,
		job:         job,
		wfSnapshot:  cloneWorkflowEventSnapshot(wf),
		jobSnapshot: cloneWorkflowJobEventSnapshot(job),
	}
	s.actionsEvents.snapshotMu.RUnlock()
	s.actionsEvents.mu.Lock()
	s.actionsEvents.queue = append(s.actionsEvents.queue, event)
	s.actionsEvents.cond.Signal()
	s.actionsEvents.mu.Unlock()
}

func (s *Server) drainActionsEvents() {
	runInProgress := map[string]bool{} // workflow UUID → workflow_run in_progress emitted
	for {
		s.actionsEvents.mu.Lock()
		for len(s.actionsEvents.queue) == 0 {
			s.actionsEvents.cond.Wait()
		}
		ev := s.actionsEvents.queue[0]
		s.actionsEvents.queue[0] = actionsEvent{}
		s.actionsEvents.queue = s.actionsEvents.queue[1:]
		if len(s.actionsEvents.queue) == 0 {
			s.actionsEvents.queue = nil
		}
		s.actionsEvents.mu.Unlock()

		switch ev.kind {
		case evRunRequested:
			s.onActionsRunRequestedSnapshot(ev.wf, ev.wfSnapshot)
		case evRunCompleted:
			delete(runInProgress, ev.wf.ID)
			s.onActionsRunCompletedSnapshot(ev.wf, ev.wfSnapshot)
		case evJobQueued:
			s.emitWorkflowJobEvent(ev.wfSnapshot, ev.jobSnapshot, "queued")
		case evJobWaiting:
			s.emitWorkflowJobEvent(ev.wfSnapshot, ev.jobSnapshot, "waiting")
		case evJobInProgress:
			s.updateJobCheckRun(ev.wf, ev.job, "in_progress", "")
			if !runInProgress[ev.wf.ID] {
				runInProgress[ev.wf.ID] = true
				s.emitWorkflowRunEvent(ev.wfSnapshot, "in_progress")
			}
			s.emitWorkflowJobEvent(ev.wfSnapshot, ev.jobSnapshot, "in_progress")
		case evJobCompleted:
			s.completeJobCheckRun(ev.wf, ev.job)
			s.emitWorkflowJobEvent(ev.wfSnapshot, ev.jobSnapshot, "completed")
		}
	}
}

// onActionsRunRequestedSnapshot creates the run's check suite plus one check
// run per visible job, then emits check_suite + workflow_run "requested"
// events from the immutable transition snapshot queued by the scheduler.
func (s *Server) onActionsRunRequestedSnapshot(wf, snapshot *Workflow) {
	repoKey := wf.RepoFullName
	branch := refShortName(wf.Ref)

	suite := s.store.CreateCheckSuite(repoKey, branch, wf.Sha, githubActionsAppID)
	s.store.UpdateCheckSuite(suite.ID, func(cs *CheckSuite) {
		cs.WorkflowRunID = wf.RunID
		cs.WorkflowRunBackendID = wf.ID
		cs.WorkflowName = wf.Name
		cs.WorkflowFileID = wf.WorkflowFileID
		cs.WorkflowFilePath = wf.WorkflowFilePath
	})
	s.store.mu.Lock()
	s.actionsEvents.snapshotMu.Lock()
	wf.CheckSuiteID = suite.ID
	s.actionsEvents.snapshotMu.Unlock()
	s.store.persistWorkflowRecord(wf)
	s.store.mu.Unlock()

	s.store.mu.RLock()
	jobs := make([]*WorkflowJob, 0, len(wf.Jobs))
	for _, j := range wf.Jobs {
		if !j.Hidden {
			jobs = append(jobs, j)
		}
	}
	s.store.mu.RUnlock()

	for _, j := range jobs {
		cr := s.store.CreateCheckRun(repoKey, wf.Sha, j.DisplayName, githubActionsAppID, suite.ID)
		s.store.mu.RLock()
		jobStatus, jobResult := j.Status, j.Result
		s.store.mu.RUnlock()
		now := time.Now().UTC()
		s.store.UpdateCheckRun(cr.ID, func(c *CheckRun) {
			c.ExternalID = j.JobID
			c.DetailsURL = fmt.Sprintf("http://%s/%s/actions/runs/%d", s.addr, repoKey, wf.RunID)
			// Jobs carried over from a previous attempt arrive already
			// terminal; their check runs reflect that immediately.
			if jobStatus == JobStatusCompleted || jobStatus == JobStatusSkipped {
				c.Status = "completed"
				c.Conclusion = resultToConclusion(jobResult)
				c.CompletedAt = &now
			}
		})
		s.store.mu.Lock()
		s.actionsEvents.snapshotMu.Lock()
		j.CheckRunID = cr.ID
		s.actionsEvents.snapshotMu.Unlock()
		s.store.mu.Unlock()
		s.emitCheckRunEvent(repoKey, cr.ID, "created")
	}

	s.emitCheckSuiteEvent(repoKey, suite.ID, "requested")
	s.emitWorkflowRunEvent(snapshot, "requested")
}

// onActionsRunCompletedSnapshot rolls the suite up and emits completed events
// from the immutable transition snapshot queued by the scheduler.
func (s *Server) onActionsRunCompletedSnapshot(wf, snapshot *Workflow) {
	repoKey := wf.RepoFullName

	s.store.mu.RLock()
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
	s.store.mu.RUnlock()

	if suiteID != 0 {
		now := time.Now().UTC()
		s.store.mu.Lock()
		if suite := s.store.CheckSuites[suiteID]; suite != nil {
			suite.Status = "completed"
			suite.Conclusion = resultToConclusion(result)
			suite.UpdatedAt = now
			if s.store.persist != nil {
				s.store.persist.MustPut("check_suites", strconv.FormatInt(suiteID, 10), suite)
			}
		}
		s.store.mu.Unlock()
		s.emitCheckSuiteEvent(repoKey, suiteID, "completed")
	}
	s.emitWorkflowRunEvent(snapshot, "completed")
}

// updateJobCheckRun moves a job's check run to a new status.
func (s *Server) updateJobCheckRun(wf *Workflow, job *WorkflowJob, status, conclusion string) {
	id := jobCheckRunID(s, job)
	if id == 0 {
		return
	}
	s.store.UpdateCheckRun(id, func(c *CheckRun) {
		c.Status = status
		if conclusion != "" {
			c.Conclusion = conclusion
		}
	})
}

// completeJobCheckRun finishes a job's check run with the job's result
// and emits check_run completed.
func (s *Server) completeJobCheckRun(wf *Workflow, job *WorkflowJob) {
	id := jobCheckRunID(s, job)
	if id == 0 {
		return
	}
	s.store.mu.RLock()
	result := job.Result
	s.store.mu.RUnlock()
	now := time.Now().UTC()
	s.store.UpdateCheckRun(id, func(c *CheckRun) {
		c.Status = "completed"
		c.Conclusion = resultToConclusion(result)
		c.CompletedAt = &now
	})
	s.emitCheckRunEvent(wf.RepoFullName, id, "completed")
}

func jobCheckRunID(s *Server, job *WorkflowJob) int64 {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return job.CheckRunID
}

// resultToConclusion maps an engine Result onto GitHub's check
// conclusion vocabulary.
func resultToConclusion(r Result) string {
	switch r {
	case ResultSuccess:
		return "success"
	case ResultFailure:
		return "failure"
	case ResultCancelled:
		return "cancelled"
	case ResultSkipped:
		return "skipped"
	case ResultStartupFailure:
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

// ── Webhook payload emission ────────────────────────────────────────

func (s *Server) actionsRepoPayload(repoKey string) (map[string]interface{}, *Repo) {
	repo := s.store.GetRepoByFullName(repoKey)
	if repo == nil {
		return nil, nil
	}
	return repoPayload(repo), repo
}

// emitWorkflowRunEvent fires the workflow_run webhook event.
func (s *Server) emitWorkflowRunEvent(wf *Workflow, action string) {
	repoJSON, repo := s.actionsRepoPayload(wf.RepoFullName)
	if repo == nil {
		return
	}
	base := fmt.Sprintf("http://%s", s.addr)
	s.store.mu.RLock()
	runJSON := workflowRunJSON(wf, base, wf.RepoFullName, repoJSON)
	var wfFileJSON map[string]any
	if f := s.store.WorkflowFiles[wf.WorkflowFileID]; f != nil {
		wfFileJSON = workflowFileJSON(f, base, wf.RepoFullName)
	}
	s.store.mu.RUnlock()

	payload := map[string]interface{}{
		"action":       action,
		"workflow_run": runJSON,
		"repository":   repoJSON,
		"sender":       senderPayload(s.workflowSender(wf)),
	}
	if wfFileJSON != nil {
		payload["workflow"] = wfFileJSON
	}
	s.emitWebhookEvent(wf.RepoFullName, "workflow_run", action, payload)
}

// emitWorkflowJobEvent fires the workflow_job webhook event.
func (s *Server) emitWorkflowJobEvent(wf *Workflow, job *WorkflowJob, action string) {
	repoJSON, repo := s.actionsRepoPayload(wf.RepoFullName)
	if repo == nil {
		return
	}
	base := fmt.Sprintf("http://%s", s.addr)
	jobJSON := s.workflowJobJSON(wf, job, base, wf.RepoFullName)
	payload := map[string]interface{}{
		"action":       action,
		"workflow_job": jobJSON,
		"repository":   repoJSON,
		"sender":       senderPayload(s.workflowSender(wf)),
	}
	s.emitWebhookEvent(wf.RepoFullName, "workflow_job", action, payload)
}

// emitCheckRunEvent fires the check_run webhook event.
func (s *Server) emitCheckRunEvent(repoKey string, checkRunID int64, action string) {
	repoJSON, repo := s.actionsRepoPayload(repoKey)
	if repo == nil {
		return
	}
	cr := s.store.GetCheckRun(checkRunID)
	if cr == nil {
		return
	}
	payload := map[string]interface{}{
		"action":     action,
		"check_run":  s.checkRunToJSON(cr, s.externalURL),
		"repository": repoJSON,
		"sender":     ghostSenderPayload(),
	}
	s.emitWebhookEvent(repoKey, "check_run", action, payload)
}

// emitCheckSuiteEvent fires the check_suite webhook event.
func (s *Server) emitCheckSuiteEvent(repoKey string, suiteID int64, action string) {
	repoJSON, repo := s.actionsRepoPayload(repoKey)
	if repo == nil {
		return
	}
	suite := s.store.GetCheckSuite(suiteID)
	if suite == nil {
		return
	}
	payload := map[string]interface{}{
		"action":      action,
		"check_suite": s.checkSuiteToJSON(suite, s.externalURL),
		"repository":  repoJSON,
		"sender":      ghostSenderPayload(),
	}
	s.emitWebhookEvent(repoKey, "check_suite", action, payload)
}

// workflowSender resolves the user behind the run's triggering event.
func (s *Server) workflowSender(wf *Workflow) *User {
	if wf.EventPayload == nil {
		return nil
	}
	sender, _ := wf.EventPayload["sender"].(map[string]interface{})
	if sender == nil {
		return nil
	}
	login, _ := sender["login"].(string)
	if login == "" {
		return nil
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return s.store.UsersByLogin[login]
}

// ── Required status checks (branch protection ∩ check runs) ─────────

// evaluateChecksForMerge inspects the head sha's check runs against the
// base branch's required contexts.
type checksState struct {
	MissingRequired []string // required contexts not green (absent/pending/failed)
	AnyPending      bool
	AnyFailing      bool
}

func (s *Server) evaluateChecksForMerge(repo *Repo, baseBranch, headSha string) checksState {
	state := checksState{}
	if headSha == "" {
		return state
	}
	repoKey := repo.FullName
	runs := s.store.ListCheckRunsForCommit(repoKey, headSha, "", "", 0)
	byName := map[string]*CheckRun{}
	for _, cr := range runs {
		// Latest run per name wins (reruns create new check runs).
		if prev, ok := byName[cr.Name]; !ok || cr.ID > prev.ID {
			byName[cr.Name] = cr
		}
		if cr.Status != "completed" {
			state.AnyPending = true
		} else if cr.Conclusion == "failure" || cr.Conclusion == "timed_out" || cr.Conclusion == "startup_failure" {
			state.AnyFailing = true
		}
	}
	for _, ctx := range s.requiredCheckContexts(repo.ID, baseBranch) {
		cr, ok := byName[ctx]
		if !ok || cr.Status != "completed" || (cr.Conclusion != "success" && cr.Conclusion != "neutral" && cr.Conclusion != "skipped") {
			state.MissingRequired = append(state.MissingRequired, ctx)
		}
	}
	return state
}

// prHeadSha resolves a PR's current head commit.
func (s *Server) prHeadSha(repo *Repo, pr *PullRequest) string {
	stor, _ := pullRequestGitStorage(s.store, repo, pr)
	return resolveBranchSha(stor, pr.HeadRefName)
}
