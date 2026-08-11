package bleephub

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

// planScope is the plan identity of one dispatched job, recorded at dispatch
// time so job-token authentication never has to re-parse the secret-bearing
// job message — and keeps working after that message is cleared.
type planScope struct {
	ScopeID string // plan scopeIdentifier — the job runtime token's sub
	Repo    string // repository the job runs as ("" for operator-submitted jobs)
}

// registerDispatchedJobLocked indexes a freshly dispatched job and records its
// plan scope. msg is the built job message (pre-marshal). Callers hold the
// store write lock.
func (st *Store) registerDispatchedJobLocked(job *Job, msg map[string]interface{}, repo string) {
	if job == nil {
		return
	}
	if job.PlanID != "" {
		st.jobsByPlanID[job.PlanID] = job
	}
	if job.RequestID != 0 {
		st.jobsByRequestID[job.RequestID] = job
	}
	if scopeID := messagePlanScopeID(msg); scopeID != "" && job.PlanID != "" {
		st.planScopes[job.PlanID] = planScope{ScopeID: scopeID, Repo: repo}
		st.planIDByScope[scopeID] = job.PlanID
	}
}

// messagePlanScopeID reads plan.scopeIdentifier from a built (pre-marshal) job
// message.
func messagePlanScopeID(msg map[string]interface{}) string {
	plan, _ := msg["plan"].(map[string]interface{})
	scopeID, _ := plan["scopeIdentifier"].(string)
	return scopeID
}

// jobByPlanIDLocked resolves a job by plan id: index first, falling back to a
// scan for jobs seeded outside the dispatch path. Callers hold the store lock.
func (st *Store) jobByPlanIDLocked(planID string) *Job {
	if planID == "" {
		return nil
	}
	if job := st.jobsByPlanID[planID]; job != nil {
		return job
	}
	for _, job := range st.Jobs {
		if job.PlanID == planID {
			return job
		}
	}
	return nil
}

// jobByRequestIDLocked resolves a job by request id: index first, falling back
// to a scan for jobs seeded outside the dispatch path. Callers hold the store
// lock.
func (st *Store) jobByRequestIDLocked(reqID int64) *Job {
	if job := st.jobsByRequestID[reqID]; job != nil {
		return job
	}
	for _, job := range st.Jobs {
		if job.RequestID == reqID {
			return job
		}
	}
	return nil
}

// planScopeForJobLocked answers the plan scope identity for a job: the
// dispatch-time record first, else the job's message (jobs seeded directly by
// tests). Callers hold the store lock.
func (st *Store) planScopeForJobLocked(job *Job) planScope {
	if job == nil {
		return planScope{}
	}
	if ps, ok := st.planScopes[job.PlanID]; ok {
		return ps
	}
	scopeID, repo := jobMessageScopeAndRepo(job.Message)
	return planScope{ScopeID: scopeID, Repo: repo}
}

// --- workflow indexes ---

// syncWorkflowIndexesLocked reconciles the derived indexes with one workflow's
// current state. It is invoked from persistWorkflowRecord, which every engine
// mutation site already calls under the store write lock, so index maintenance
// cannot drift from the map it mirrors. A workflow that is not (any longer)
// the store's entry for its ID is not indexed.
func (st *Store) syncWorkflowIndexesLocked(wf *Workflow) {
	if wf == nil || st.workflowsByRunID == nil {
		return
	}
	if st.Workflows[wf.ID] != wf {
		return
	}
	if wf.RunID != 0 {
		st.workflowsByRunID[wf.RunID] = wf
	}
	if wf.ConcurrencyGroup != "" {
		if wf.Status == WorkflowStatusCompleted {
			st.removeWorkflowGroupEntryLocked(wf)
		} else {
			group := st.workflowsByConcurrencyGroup[wf.ConcurrencyGroup]
			if group == nil {
				group = make(map[string]*Workflow)
				st.workflowsByConcurrencyGroup[wf.ConcurrencyGroup] = group
			}
			group[wf.ID] = wf
		}
	}
	for _, wfJob := range wf.Jobs {
		st.syncJobConcurrencyEntryLocked(wf, wfJob)
	}
}

// syncJobConcurrencyEntryLocked adds or removes one job's concurrency-group
// index entry according to its current status. Callers hold the store write
// lock.
func (st *Store) syncJobConcurrencyEntryLocked(wf *Workflow, wfJob *WorkflowJob) {
	if wfJob == nil || wfJob.ConcurrencyGroup == "" || st.jobsByConcurrencyGroup == nil {
		return
	}
	if wfJob.Status == JobStatusCompleted || wfJob.Status == JobStatusSkipped {
		if group := st.jobsByConcurrencyGroup[wfJob.ConcurrencyGroup]; group != nil {
			delete(group, wfJob)
			if len(group) == 0 {
				delete(st.jobsByConcurrencyGroup, wfJob.ConcurrencyGroup)
			}
		}
		return
	}
	group := st.jobsByConcurrencyGroup[wfJob.ConcurrencyGroup]
	if group == nil {
		group = make(map[*WorkflowJob]*Workflow)
		st.jobsByConcurrencyGroup[wfJob.ConcurrencyGroup] = group
	}
	group[wfJob] = wf
}

func (st *Store) removeWorkflowGroupEntryLocked(wf *Workflow) {
	if group := st.workflowsByConcurrencyGroup[wf.ConcurrencyGroup]; group != nil {
		delete(group, wf.ID)
		if len(group) == 0 {
			delete(st.workflowsByConcurrencyGroup, wf.ConcurrencyGroup)
		}
	}
}

// unindexWorkflowLocked removes a workflow (being deleted from st.Workflows)
// from every derived index. Callers hold the store write lock.
func (st *Store) unindexWorkflowLocked(wf *Workflow) {
	if wf == nil || st.workflowsByRunID == nil {
		return
	}
	if st.workflowsByRunID[wf.RunID] == wf {
		delete(st.workflowsByRunID, wf.RunID)
	}
	if wf.ConcurrencyGroup != "" {
		st.removeWorkflowGroupEntryLocked(wf)
	}
	for _, wfJob := range wf.Jobs {
		if wfJob.ConcurrencyGroup == "" {
			continue
		}
		if group := st.jobsByConcurrencyGroup[wfJob.ConcurrencyGroup]; group != nil {
			delete(group, wfJob)
			if len(group) == 0 {
				delete(st.jobsByConcurrencyGroup, wfJob.ConcurrencyGroup)
			}
		}
	}
}

// rebuildWorkflowIndexesLocked recomputes every Workflows-derived index from
// scratch. Called wherever the Workflows map itself is replaced or reloaded:
// initial load from persistence and replica snapshot refresh.
func (st *Store) rebuildWorkflowIndexesLocked() {
	st.workflowsByRunID = make(map[int]*Workflow, len(st.Workflows))
	st.workflowsByConcurrencyGroup = make(map[string]map[string]*Workflow)
	st.jobsByConcurrencyGroup = make(map[string]map[*WorkflowJob]*Workflow)
	for _, wf := range st.Workflows {
		st.syncWorkflowIndexesLocked(wf)
	}
}

// workflowConcurrencyPeersLocked snapshots the non-completed workflows in a
// concurrency group, lazily pruning entries that have completed since they
// were indexed. Callers hold the store write lock.
func (st *Store) workflowConcurrencyPeersLocked(group string) []*Workflow {
	entries := st.workflowsByConcurrencyGroup[group]
	if len(entries) == 0 {
		return nil
	}
	peers := make([]*Workflow, 0, len(entries))
	for id, wf := range entries {
		if wf.Status == WorkflowStatusCompleted || st.Workflows[id] != wf {
			delete(entries, id)
			continue
		}
		peers = append(peers, wf)
	}
	if len(entries) == 0 {
		delete(st.workflowsByConcurrencyGroup, group)
	}
	return peers
}

// jobConcurrencyPeer is one (job, owning workflow) pair in a job concurrency
// group.
type jobConcurrencyPeer struct {
	job *WorkflowJob
	wf  *Workflow
}

// jobConcurrencyPeersLocked snapshots the non-terminal jobs in a job
// concurrency group, lazily pruning entries that have reached a terminal state
// since they were indexed. Callers hold the store write lock.
func (st *Store) jobConcurrencyPeersLocked(group string) []jobConcurrencyPeer {
	entries := st.jobsByConcurrencyGroup[group]
	if len(entries) == 0 {
		return nil
	}
	peers := make([]jobConcurrencyPeer, 0, len(entries))
	for wfJob, wf := range entries {
		if wfJob.Status == JobStatusCompleted || wfJob.Status == JobStatusSkipped {
			delete(entries, wfJob)
			continue
		}
		peers = append(peers, jobConcurrencyPeer{job: wfJob, wf: wf})
	}
	if len(entries) == 0 {
		delete(st.jobsByConcurrencyGroup, group)
	}
	return peers
}

// --- garbage collection ---

// clearRunJobMessagesLocked drops the secret-bearing job messages of a
// finalized run and stamps each job's retirement time. Late runner calls
// (completed-job teardown, log flushes) keep authenticating through
// planScopes; reclaimExpiredJobLeases only redelivers non-completed jobs, and
// every job of a finalized run is terminal. Callers hold the store write lock
// and must only call this once the run has completed.
func (st *Store) clearRunJobMessagesLocked(wf *Workflow) {
	now := st.currentTime()
	for _, wfJob := range wf.Jobs {
		job := st.Jobs[wfJob.JobID]
		if job == nil {
			continue
		}
		job.Message = ""
		if job.CompletedAt.IsZero() {
			job.CompletedAt = now
		}
	}
}

// markJobCompletedLocked stamps a job's terminal transition and releases the
// broker's busy bookkeeping for its (non-ephemeral) agent. Callers hold the
// store write lock.
func (st *Store) markJobCompletedLocked(job *Job) {
	if job == nil {
		return
	}
	job.Status = "completed"
	if job.CompletedAt.IsZero() {
		job.CompletedAt = st.currentTime()
	}
	st.clearAgentAssignmentLocked(job)
}

// clearAgentAssignmentLocked clears the AssignedJobID of the agent holding a
// job that no longer binds it. EverAssigned is deliberately left set: it is
// what keeps a used ephemeral agent disqualified after its job's stub is
// swept. Callers hold the store write lock.
func (st *Store) clearAgentAssignmentLocked(job *Job) {
	if job == nil || job.AgentID == 0 {
		return
	}
	agent := st.Agents[job.AgentID]
	if agent != nil && agent.AssignedJobID == job.ID && !agent.Ephemeral {
		agent.AssignedJobID = ""
	}
}

// dropJobStateLocked deletes every piece of replica-local state held for one
// job: the Job stub, its plan-id/request-id index entries, its plan scope, its
// log masks and its captured console lines. The in-memory log FILE bytes are
// keyed by log id and claimed in the ArtifactStore, so the caller collects the
// returned plan id and releases them outside the store lock
// (releaseJobLogFiles). Callers hold the store write lock.
func (st *Store) dropJobStateLocked(job *Job) (planID string) {
	if job == nil {
		return ""
	}
	delete(st.Jobs, job.ID)
	if st.jobsByPlanID[job.PlanID] == job {
		delete(st.jobsByPlanID, job.PlanID)
	}
	if st.jobsByRequestID[job.RequestID] == job {
		delete(st.jobsByRequestID, job.RequestID)
	}
	if ps, ok := st.planScopes[job.PlanID]; ok {
		if st.planIDByScope[ps.ScopeID] == job.PlanID {
			delete(st.planIDByScope, ps.ScopeID)
		}
		delete(st.planScopes, job.PlanID)
	}
	delete(st.LogMasks, job.PlanID)
	delete(st.LogLines, job.ID)
	return job.PlanID
}

// dropWorkflowJobStateLocked eagerly tears down the replica-local job state of
// every job in a run (run deletion, repository deletion). Returns the plan ids
// whose in-memory log bytes should be released via releaseJobLogFiles once the
// store lock is dropped. Callers hold the store write lock.
func (st *Store) dropWorkflowJobStateLocked(wf *Workflow) (planIDs []string) {
	if wf == nil {
		return nil
	}
	for _, wfJob := range wf.Jobs {
		job := st.Jobs[wfJob.JobID]
		if job == nil {
			continue
		}
		if planID := st.dropJobStateLocked(job); planID != "" {
			planIDs = append(planIDs, planID)
		}
	}
	return planIDs
}

// releaseJobLogFiles releases the in-memory log bytes claimed by the given
// plans: the ArtifactStore claim registry maps log ids to the plan that
// reserved them. Durable byte-store log objects are left alone — GC here is
// purely in-memory. Must be called without the store lock held (the
// ArtifactStore has its own mutex).
func (s *Server) releaseJobLogFiles(planIDs []string) {
	if len(planIDs) == 0 || s.artifactStore == nil {
		return
	}
	logIDs := s.artifactStore.releaseLogClaimsForPlans(planIDs)
	if len(logIDs) == 0 {
		return
	}
	s.store.mu.Lock()
	for _, logID := range logIDs {
		delete(s.store.LogFiles, logID)
	}
	s.store.mu.Unlock()
}

// actionsJanitorInterval is how often the janitor sweeps retired job state.
const actionsJanitorInterval = 10 * time.Minute

// completedJobRetention is how long a completed job's replica-local runtime
// state stays addressable. runnerTokenTTL bounds the lifetime of the job's
// runtime token and the agent session token that could still name it, and
// completed-job teardown calls (DELETE AgentRequest, late log flushes) can
// arrive that late — so nothing valid can reach the job afterwards.
const completedJobRetention = runnerTokenTTL

// startActionsJanitor runs the periodic sweep of retired Actions job state for
// the server's lifetime. There is deliberately one janitor per process,
// started with the server and stopped through ctx at shutdown.
func (s *Server) startActionsJanitor(ctx context.Context) {
	s.goBackground(func() {
		ticker := time.NewTicker(actionsJanitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweepRetiredActionsJobs(s.store.currentTime())
			}
		}
	})
}

// sweepRetiredActionsJobs deletes the replica-local state of every job whose
// retirement stamp is older than completedJobRetention. Returns how many jobs
// were swept (for tests and logging).
func (s *Server) sweepRetiredActionsJobs(now time.Time) int {
	var planIDs []string
	s.store.mu.Lock()
	var retired []*Job
	for _, job := range s.store.Jobs {
		if job.CompletedAt.IsZero() {
			continue
		}
		if now.Sub(job.CompletedAt) < completedJobRetention {
			continue
		}
		retired = append(retired, job)
	}
	for _, job := range retired {
		if planID := s.store.dropJobStateLocked(job); planID != "" {
			planIDs = append(planIDs, planID)
		}
	}
	s.store.mu.Unlock()

	s.releaseJobLogFiles(planIDs)

	if len(retired) > 0 {
		s.logger.Info().Int("jobs", len(retired)).Msg("actions janitor swept retired job state")
	}
	return len(retired)
}
