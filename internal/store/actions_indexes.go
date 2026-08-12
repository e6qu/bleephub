package store

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
func (st *Store) RegisterDispatchedJobLocked(job *Job, msg map[string]interface{}, repo string) {
	if job == nil {
		return
	}
	if job.PlanID != "" {
		st.JobsByPlanID[job.PlanID] = job
	}
	if job.RequestID != 0 {
		st.jobsByRequestID[job.RequestID] = job
	}
	if scopeID := messagePlanScopeID(msg); scopeID != "" && job.PlanID != "" {
		st.PlanScopes[job.PlanID] = planScope{ScopeID: scopeID, Repo: repo}
		st.PlanIDByScope[scopeID] = job.PlanID
	}
}

// jobByPlanIDLocked resolves a job by plan Id: index first, falling back to a
// scan for jobs seeded outside the dispatch path. Callers hold the store lock.
func (st *Store) JobByPlanIDLocked(planID string) *Job {
	if planID == "" {
		return nil
	}
	if job := st.JobsByPlanID[planID]; job != nil {
		return job
	}
	for _, job := range st.Jobs {
		if job.PlanID == planID {
			return job
		}
	}
	return nil
}

// jobByRequestIDLocked resolves a job by request Id: index first, falling back
// to a scan for jobs seeded outside the dispatch path. Callers hold the store
// lock.
func (st *Store) JobByRequestIDLocked(reqID int64) *Job {
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

// planScopeForJobLocked answers the plan scope identity for a Job: the
// dispatch-time record first, else the job's message (jobs seeded directly by
// tests). Callers hold the store lock.
func (st *Store) PlanScopeForJobLocked(job *Job) planScope {
	if job == nil {
		return planScope{}
	}
	if ps, ok := st.PlanScopes[job.PlanID]; ok {
		return ps
	}
	scopeID, repo := JobMessageScopeAndRepo(job.Message)
	return planScope{ScopeID: scopeID, Repo: repo}
}

// syncWorkflowIndexesLocked reconciles the derived indexes with one workflow's
// current state. It is invoked from persistWorkflowRecord, which every engine
// mutation site already calls under the store write lock, so index maintenance
// cannot drift from the map it mirrors. A workflow that is not (any longer)
// the store's entry for its ID is not indexed.
func (st *Store) SyncWorkflowIndexesLocked(wf *Workflow) {
	if wf == nil || st.WorkflowsByRunID == nil {
		return
	}
	if st.Workflows[wf.ID] != wf {
		return
	}
	if wf.RunID != 0 {
		st.WorkflowsByRunID[wf.RunID] = wf
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
		st.SyncJobConcurrencyEntryLocked(wf, wfJob)
	}
}

// syncJobConcurrencyEntryLocked adds or removes one job's concurrency-group
// index entry according to its current status. Callers hold the store write
// lock.
func (st *Store) SyncJobConcurrencyEntryLocked(wf *Workflow, wfJob *WorkflowJob) {
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
func (st *Store) UnindexWorkflowLocked(wf *Workflow) {
	if wf == nil || st.WorkflowsByRunID == nil {
		return
	}
	if st.WorkflowsByRunID[wf.RunID] == wf {
		delete(st.WorkflowsByRunID, wf.RunID)
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
	st.WorkflowsByRunID = make(map[int]*Workflow, len(st.Workflows))
	st.workflowsByConcurrencyGroup = make(map[string]map[string]*Workflow)
	st.jobsByConcurrencyGroup = make(map[string]map[*WorkflowJob]*Workflow)
	for _, wf := range st.Workflows {
		st.SyncWorkflowIndexesLocked(wf)
	}
}

// workflowConcurrencyPeersLocked snapshots the non-completed workflows in a
// concurrency group, lazily pruning entries that have completed since they
// were indexed. Callers hold the store write lock.
func (st *Store) WorkflowConcurrencyPeersLocked(group string) []*Workflow {
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

// jobConcurrencyPeersLocked snapshots the non-terminal jobs in a job
// concurrency group, lazily pruning entries that have reached a terminal state
// since they were indexed. Callers hold the store write lock.
func (st *Store) JobConcurrencyPeersLocked(group string) []jobConcurrencyPeer {
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
		peers = append(peers, jobConcurrencyPeer{Job: wfJob, Wf: wf})
	}
	if len(entries) == 0 {
		delete(st.jobsByConcurrencyGroup, group)
	}
	return peers
}

// clearRunJobMessagesLocked drops the secret-bearing job messages of a
// finalized run and stamps each job's retirement time. Late runner calls
// (completed-job teardown, log flushes) keep authenticating through
// planScopes; reclaimExpiredJobLeases only redelivers non-completed jobs, and
// every job of a finalized run is terminal. Callers hold the store write lock
// and must only call this once the run has completed.
func (st *Store) ClearRunJobMessagesLocked(wf *Workflow) {
	now := st.CurrentTime()
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
func (st *Store) MarkJobCompletedLocked(job *Job) {
	if job == nil {
		return
	}
	job.Status = "completed"
	if job.CompletedAt.IsZero() {
		job.CompletedAt = st.CurrentTime()
	}
	st.ClearAgentAssignmentLocked(job)
}

// clearAgentAssignmentLocked clears the AssignedJobID of the agent holding a
// job that no longer binds it. EverAssigned is deliberately left set: it is
// what keeps a used ephemeral agent disqualified after its job's stub is
// swept. Callers hold the store write lock.
func (st *Store) ClearAgentAssignmentLocked(job *Job) {
	if job == nil || job.AgentID == 0 {
		return
	}
	agent := st.Agents[job.AgentID]
	if agent != nil && agent.AssignedJobID == job.ID && !agent.Ephemeral {
		agent.AssignedJobID = ""
	}
}

// dropJobStateLocked deletes every piece of replica-local state held for one
// Job: the Job stub, its plan-id/request-id index entries, its plan scope, its
// log masks and its captured console lines. The in-memory log FILE bytes are
// keyed by log id and claimed in the ArtifactStore, so the caller collects the
// returned plan id and releases them outside the store lock
// (releaseJobLogFiles). Callers hold the store write lock.
func (st *Store) DropJobStateLocked(job *Job) (planID string) {
	if job == nil {
		return ""
	}
	delete(st.Jobs, job.ID)
	if st.JobsByPlanID[job.PlanID] == job {
		delete(st.JobsByPlanID, job.PlanID)
	}
	if st.jobsByRequestID[job.RequestID] == job {
		delete(st.jobsByRequestID, job.RequestID)
	}
	if ps, ok := st.PlanScopes[job.PlanID]; ok {
		if st.PlanIDByScope[ps.ScopeID] == job.PlanID {
			delete(st.PlanIDByScope, ps.ScopeID)
		}
		delete(st.PlanScopes, job.PlanID)
	}
	delete(st.LogMasks, job.PlanID)
	delete(st.LogLines, job.ID)
	return job.PlanID
}

// dropWorkflowJobStateLocked eagerly tears down the replica-local job state of
// every job in a run (run deletion, repository deletion). Returns the plan ids
// whose in-memory log bytes should be released via releaseJobLogFiles once the
// store lock is dropped. Callers hold the store write lock.
func (st *Store) DropWorkflowJobStateLocked(wf *Workflow) (planIDs []string) {
	if wf == nil {
		return nil
	}
	for _, wfJob := range wf.Jobs {
		job := st.Jobs[wfJob.JobID]
		if job == nil {
			continue
		}
		if planID := st.DropJobStateLocked(job); planID != "" {
			planIDs = append(planIDs, planID)
		}
	}
	return planIDs
}

// jobConcurrencyPeer is one (job, owning workflow) pair in a job concurrency
// group.
type jobConcurrencyPeer struct {
	Job *WorkflowJob `json:"-"`
	Wf  *Workflow    `json:"-"`
}

// messagePlanScopeID reads plan.scopeIdentifier from a built (pre-marshal) job
// message.
func messagePlanScopeID(msg map[string]interface{}) string {
	plan, _ := msg["plan"].(map[string]interface{})
	scopeID, _ := plan["scopeIdentifier"].(string)
	return scopeID
}
