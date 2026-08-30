package store

// planScope records a dispatched job's plan identity so job-token auth keeps
// working after the secret-bearing job message is cleared.
type planScope struct {
	ScopeID string // plan scopeIdentifier — the job runtime token's sub
	Repo    string // repository the job runs as ("" for operator-submitted jobs)
}

// RegisterDispatchedJobLocked indexes a dispatched job and records its plan
// scope. msg is the pre-marshal job message. Callers hold the write lock.
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

// JobByPlanIDLocked resolves a job by plan id, falling back to a scan for jobs
// seeded outside the dispatch path. Callers hold the store lock.
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

// JobByRequestIDLocked resolves a job by request id, falling back to a scan for
// jobs seeded outside the dispatch path. Callers hold the store lock.
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

// PlanScopeForJobLocked answers a job's plan scope: the dispatch-time record
// first, else the job's message. Callers hold the store lock.
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

// SyncWorkflowIndexesLocked reconciles the derived indexes with one workflow's
// current state. Callers hold the write lock. A workflow that is no longer the
// store's entry for its ID is not indexed.
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
	if st.workflowsByRepo != nil {
		byID := st.workflowsByRepo[wf.RepoFullName]
		if byID == nil {
			byID = make(map[string]*Workflow)
			st.workflowsByRepo[wf.RepoFullName] = byID
		}
		byID[wf.ID] = wf
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

// SyncJobConcurrencyEntryLocked adds or removes one job's concurrency-group
// index entry per its status. Callers hold the write lock.
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

// UnindexWorkflowLocked removes a deleted workflow from every derived index.
// Callers hold the write lock.
func (st *Store) UnindexWorkflowLocked(wf *Workflow) {
	if wf == nil || st.WorkflowsByRunID == nil {
		return
	}
	if st.WorkflowsByRunID[wf.RunID] == wf {
		delete(st.WorkflowsByRunID, wf.RunID)
	}
	if byID := st.workflowsByRepo[wf.RepoFullName]; byID != nil && byID[wf.ID] == wf {
		delete(byID, wf.ID)
		if len(byID) == 0 {
			delete(st.workflowsByRepo, wf.RepoFullName)
		}
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
// scratch, for when the Workflows map is replaced or reloaded.
func (st *Store) rebuildWorkflowIndexesLocked() {
	st.WorkflowsByRunID = make(map[int]*Workflow, len(st.Workflows))
	st.workflowsByConcurrencyGroup = make(map[string]map[string]*Workflow)
	st.jobsByConcurrencyGroup = make(map[string]map[*WorkflowJob]*Workflow)
	st.workflowsByRepo = make(map[string]map[string]*Workflow)
	for _, wf := range st.Workflows {
		st.SyncWorkflowIndexesLocked(wf)
	}
}

// WorkflowsForRepoLocked returns the workflow runs belonging to a repository —
// the per-repo run-listing index instead of a scan of every run in the instance.
// Operator-submitted runs (empty RepoFullName) match every repository, matching
// the handler's historical filter, so they are folded in. Callers hold the lock;
// the returned slice is a fresh list of the live pointers.
func (st *Store) WorkflowsForRepoLocked(repoFullName string) []*Workflow {
	out := make([]*Workflow, 0, len(st.workflowsByRepo[repoFullName])+len(st.workflowsByRepo[""]))
	for _, wf := range st.workflowsByRepo[repoFullName] {
		out = append(out, wf)
	}
	if repoFullName != "" {
		for _, wf := range st.workflowsByRepo[""] {
			out = append(out, wf)
		}
	}
	return out
}

// WorkflowConcurrencyPeersLocked snapshots the non-completed workflows in a
// concurrency group, lazily pruning entries completed since indexing. Callers
// hold the write lock.
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

// JobConcurrencyPeersLocked snapshots the non-terminal jobs in a job
// concurrency group, lazily pruning entries gone terminal since indexing.
// Callers hold the write lock.
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

// ClearRunJobMessagesLocked drops the secret-bearing job messages of a
// finalized run and stamps each job's retirement time. Late runner calls keep
// authenticating through planScopes. Callers hold the write lock and must call
// this only once the run has completed.
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

// MarkJobCompletedLocked stamps a job's terminal transition and releases the
// broker's busy bookkeeping for its non-ephemeral agent. Callers hold the write
// lock.
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

// ClearAgentAssignmentLocked clears the AssignedJobID of the agent holding a
// job that no longer binds it. EverAssigned stays set: it keeps a used
// ephemeral agent disqualified after its job's stub is swept. Callers hold the
// write lock.
func (st *Store) ClearAgentAssignmentLocked(job *Job) {
	if job == nil || job.AgentID == 0 {
		return
	}
	agent := st.Agents[job.AgentID]
	if agent != nil && agent.AssignedJobID == job.ID && !agent.Ephemeral {
		agent.AssignedJobID = ""
	}
}

// DropJobStateLocked deletes every piece of replica-local state held for one
// job. The caller releases the returned plan id's in-memory log bytes outside
// the store lock (releaseJobLogFiles). Callers hold the write lock.
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

// DropWorkflowJobStateLocked tears down the replica-local job state of every
// job in a run. Returns the plan ids whose in-memory log bytes to release via
// releaseJobLogFiles once the lock is dropped. Callers hold the write lock.
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

// jobConcurrencyPeer is one (job, owning workflow) pair in a concurrency group.
type jobConcurrencyPeer struct {
	Job *WorkflowJob `json:"-"`
	Wf  *Workflow    `json:"-"`
}

// messagePlanScopeID reads plan.scopeIdentifier from a pre-marshal job message.
func messagePlanScopeID(msg map[string]interface{}) string {
	plan, _ := msg["plan"].(map[string]interface{})
	scopeID, _ := plan["scopeIdentifier"].(string)
	return scopeID
}
