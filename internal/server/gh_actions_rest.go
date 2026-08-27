package bleephub

// GitHub-shape REST surface exposing the in-memory Workflow/WorkflowJob
// store under the public actions/runs, jobs, and runners paths.

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHActionsRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runs", s.handleListWorkflowRuns)
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}", s.handleGetWorkflowRun)
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}/jobs", s.handleListWorkflowRunJobs)
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}",
		s.requirePerm(store.ScopeActions, store.PermWrite, s.handleDeleteWorkflowRun))
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}/cancel",
		s.requirePerm(store.ScopeActions, store.PermWrite, s.handleCancelWorkflowRun))
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}/rerun",
		s.requirePerm(store.ScopeActions, store.PermWrite, s.handleRerunWorkflowRun))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}/attempts/{attempt_number}", s.handleGetRunAttempt)
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}/attempts/{attempt_number}/jobs", s.handleListRunAttemptJobs)
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/jobs/{job_id}", s.handleGetWorkflowJob)
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/jobs/{job_id}/logs", s.handleGetWorkflowJobLogs)
	s.route("GET /internal/repos/{owner}/{repo}/actions/jobs/{job_id}/summary", s.handleGetWorkflowJobSummary)
	// List/get runners require administration:read on real GitHub.
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runners",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleListRunners))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runners/downloads",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleListRunnerApplications))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runners/{runner_id}",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleGetRunner))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/runners/{runner_id}",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDeleteRunner))

	// bleephub's runner pool is global, so the org scope serves the same
	// agents; only an unknown org 404s.
	s.route("GET /api/v3/orgs/{org}/actions/runners",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleListRunners))
	s.route("GET /api/v3/orgs/{org}/actions/runners/downloads",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleListRunnerApplications))
	s.route("GET /api/v3/orgs/{org}/actions/runners/{runner_id}",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleGetRunner))
	s.route("DELETE /api/v3/orgs/{org}/actions/runners/{runner_id}",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDeleteRunner))
	s.route("POST /api/v3/orgs/{org}/actions/runners/registration-token",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleOrgRegistrationToken))
	s.route("POST /api/v3/orgs/{org}/actions/runners/remove-token",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleOrgRemoveToken))
	s.route("POST /api/v3/orgs/{org}/actions/runners/generate-jitconfig",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleOrgGenerateJITConfig))
}

func (s *Server) handleListRunnerApplications(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.runnerTargetFromRequest(w, r); !ok {
		return
	}
	// bleephub runners ship as container images, not downloadable archives.
	writeJSON(w, http.StatusOK, []map[string]interface{}{})
}

func (s *Server) handleOrgRegistrationToken(w http.ResponseWriter, r *http.Request) {
	if s.store.GetOrg(r.PathValue("org")) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.handleRegistrationToken(w, r)
}

func (s *Server) handleOrgRemoveToken(w http.ResponseWriter, r *http.Request) {
	if s.store.GetOrg(r.PathValue("org")) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.handleRemoveToken(w, r)
}

func (s *Server) handleOrgGenerateJITConfig(w http.ResponseWriter, r *http.Request) {
	if s.store.GetOrg(r.PathValue("org")) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.handleGenerateJITConfig(w, r)
}

// repoFullName returns "owner/repo", the form Workflow.RepoFullName uses.
func repoFullName(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo")
}

// sortRunsNewestFirst orders runs newest-first; without it, map iteration
// order shifts page boundaries between requests.
func sortRunsNewestFirst(runs []*store.Workflow) {
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunID > runs[j].RunID })
}

// stableJobID maps a WorkflowJob UUID to a stable positive id, kept within
// JavaScript's exact integer range for JSON-number consumers.
func stableJobID(uuid string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(uuid))
	return store.JsonSafePositiveID(h.Sum64())
}

// runMatchesStatusFilter implements the `status` parameter, whose enum
// accepts both run-status and conclusion values (e.g. ?status=success).
func runMatchesStatusFilter(wf *store.Workflow, filter string) bool {
	status := runStatus(wf)
	if status == filter {
		return true
	}
	if c, ok := runConclusion(status, string(wf.Result)).(string); ok && c == filter {
		return true
	}
	return false
}

// runStatus maps a Workflow to GitHub's run status. A submitted-but-not-yet-
// started run reports `queued`, not `in_progress`: pollers gating on
// ?status=queued depend on that to see label-stranded runs.
func runStatus(wf *store.Workflow) string {
	switch string(wf.Status) {
	case "completed":
		return "completed"
	case "running":
		for _, j := range wf.Jobs {
			if j.Status == store.JobStatusRunning || j.Status == store.JobStatusCompleted {
				return "in_progress"
			}
		}
		return "queued"
	case "pending_concurrency":
		return "queued"
	case "waiting":
		return "waiting"
	case "action_required":
		return "action_required"
	default:
		return "queued"
	}
}

// runConclusion maps Workflow.Result to GitHub's nullable conclusion; nil
// while in flight.
func runConclusion(status, result string) any {
	if status != "completed" {
		return nil
	}
	if result == "" {
		return "success"
	}
	return result
}

func jobStatus(internal string) string {
	switch internal {
	case "queued":
		return "queued"
	case "waiting":
		return "waiting"
	case "running":
		return "in_progress"
	case "completed", "skipped":
		return "completed"
	default:
		return "queued"
	}
}

func jobConclusion(status, result string) any {
	if status != "completed" {
		return nil
	}
	if result == "" {
		return "success"
	}
	return result
}

// runRepoJSON resolves the `repository`/`head_repository` object embedded in
// workflow-run responses; nil when the run's repo is gone.
func (s *Server) runRepoJSON(fullName, baseURL string) map[string]interface{} {
	repo := s.store.GetRepoByFullName(fullName)
	if repo == nil {
		return nil
	}
	return store.RepoToJSON(repo, s.store, baseURL)
}

func workflowRunJSON(wf *store.Workflow, baseURL, repoName string, repoJSON map[string]interface{}) map[string]any {
	repoPath := repoName
	if wf.RepoFullName != "" {
		repoPath = wf.RepoFullName
	}
	apiBase := fmt.Sprintf("%s/api/v3/repos/%s", baseURL, repoPath)
	htmlBase := fmt.Sprintf("%s/%s", baseURL, repoPath)
	status := runStatus(wf)
	// workflow_id/workflow_url/path reference the originating workflow FILE
	// (stable across runs), never the per-run RunID. Derive deterministically
	// for runs with no backing file (e.g. seeded in tests).
	fileID := wf.WorkflowFileID
	filePath := wf.WorkflowFilePath
	if filePath == "" {
		filePath = ".github/workflows/" + wf.Name + ".yml"
	}
	if fileID == 0 {
		fileID = store.StableWorkflowFileID(wf.RepoFullName, filePath)
	}
	created := wf.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
	run := map[string]any{
		"repository":           repoJSON,
		"head_repository":      repoJSON,
		"id":                   int64(wf.RunID),
		"name":                 wf.Name,
		"node_id":              "WFR_" + wf.ID,
		"head_branch":          headBranchOf(wf),
		"head_sha":             wf.Sha,
		"path":                 filePath,
		"display_title":        wf.RunDisplayTitle(),
		"run_number":           wf.RunNumber,
		"event":                eventOf(wf),
		"status":               status,
		"conclusion":           runConclusion(status, string(wf.Result)),
		"workflow_id":          fileID,
		"check_suite_id":       int64(wf.RunID),
		"check_suite_node_id":  "CS_" + wf.ID,
		"url":                  fmt.Sprintf("%s/actions/runs/%d", apiBase, wf.RunID),
		"html_url":             fmt.Sprintf("%s/actions/runs/%d", htmlBase, wf.RunID),
		"pull_requests":        []any{},
		"created_at":           created,
		"updated_at":           created,
		"run_attempt":          wf.AttemptNumber(),
		"referenced_workflows": []any{},
		"run_started_at":       created,
		"jobs_url":             fmt.Sprintf("%s/actions/runs/%d/jobs", apiBase, wf.RunID),
		"logs_url":             fmt.Sprintf("%s/actions/runs/%d/logs", apiBase, wf.RunID),
		"check_suite_url":      fmt.Sprintf("%s/check-suites/%d", apiBase, wf.RunID),
		"artifacts_url":        fmt.Sprintf("%s/actions/runs/%d/artifacts", apiBase, wf.RunID),
		"cancel_url":           fmt.Sprintf("%s/actions/runs/%d/cancel", apiBase, wf.RunID),
		"rerun_url":            fmt.Sprintf("%s/actions/runs/%d/rerun", apiBase, wf.RunID),
		"workflow_url":         fmt.Sprintf("%s/actions/workflows/%d", apiBase, fileID),
		"head_commit": map[string]any{
			"id":        wf.Sha,
			"tree_id":   wf.Sha,
			"message":   wf.Name,
			"timestamp": created,
			"author":    map[string]any{"name": "bleephub", "email": "actions@bleephub"},
			"committer": map[string]any{"name": "bleephub", "email": "actions@bleephub"},
		},
	}
	// actor/triggering_actor are optional non-nullable simple-user objects;
	// omit the keys (never emit null) when the triggering user is unknown.
	if actor := runActorJSON(wf); actor != nil {
		run["actor"] = actor
		run["triggering_actor"] = actor
	}
	return run
}

// runActorJSON resolves the run's actor from the triggering event's sender
// payload; nil when there is no originating user.
func runActorJSON(wf *store.Workflow) any {
	if wf.EventPayload == nil {
		return nil
	}
	if sender, ok := wf.EventPayload["sender"].(map[string]interface{}); ok && sender != nil {
		return sender
	}
	return nil
}

func headBranchOf(wf *store.Workflow) string {
	if wf.Ref == "" {
		return "main"
	}
	return strings.TrimPrefix(wf.Ref, "refs/heads/")
}

func eventOf(wf *store.Workflow) string {
	if wf.EventName == "" {
		return "workflow_dispatch"
	}
	return wf.EventName
}

// workflowJobJSON converts a WorkflowJob to GitHub's `Job` shape.
func (s *Server) workflowJobJSON(wf *store.Workflow, wfJob *store.WorkflowJob, baseURL, repoName string) map[string]any {
	// The job's mutable fields are written by the engine under store.mu; hold
	// the read lock across the whole render to synchronize with those writes.
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.workflowJobJSONLocked(wf, wfJob, baseURL, repoName)
}

// workflowJobJSONLocked renders the job payload; caller holds store.mu.
func (s *Server) workflowJobJSONLocked(wf *store.Workflow, wfJob *store.WorkflowJob, baseURL, repoName string) map[string]any {
	repoPath := repoName
	if wf.RepoFullName != "" {
		repoPath = wf.RepoFullName
	}
	apiBase := fmt.Sprintf("%s/api/v3/repos/%s", baseURL, repoPath)
	htmlBase := fmt.Sprintf("%s/%s", baseURL, repoPath)
	status := jobStatus(string(wfJob.Status))
	id := stableJobID(wfJob.JobID)
	var startedAt any
	if !wfJob.StartedAt.IsZero() {
		startedAt = wfJob.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	queuedAt := wfJob.QueuedAt
	if queuedAt.IsZero() {
		queuedAt = wfJob.StartedAt
	}
	if queuedAt.IsZero() {
		queuedAt = wf.CreatedAt
	}
	createdAt := queuedAt.UTC().Format("2006-01-02T15:04:05Z")
	// started_at is required and non-nullable; fall back to the queued/created
	// timestamp for a still-queued job rather than emit null.
	if startedAt == nil {
		startedAt = createdAt
	}
	var completedAt any
	if status == "completed" {
		t := wfJob.CompletedAt
		if t.IsZero() {
			t = wfJob.StartedAt
		}
		completedAt = t.UTC().Format("2006-01-02T15:04:05Z")
	}
	return map[string]any{
		"id":                id,
		"run_id":            int64(wf.RunID),
		"workflow_name":     wf.Name,
		"head_branch":       headBranchOf(wf),
		"run_url":           fmt.Sprintf("%s/actions/runs/%d", apiBase, wf.RunID),
		"run_attempt":       wf.AttemptNumber(),
		"node_id":           "JOB_" + wfJob.JobID,
		"head_sha":          wf.Sha,
		"url":               fmt.Sprintf("%s/actions/jobs/%d", apiBase, id),
		"html_url":          fmt.Sprintf("%s/actions/runs/%d/job/%d", htmlBase, wf.RunID, id),
		"status":            status,
		"conclusion":        jobConclusion(status, string(wfJob.Result)),
		"created_at":        createdAt,
		"started_at":        startedAt,
		"completed_at":      completedAt,
		"name":              wfJob.DisplayName,
		"steps":             s.jobStepsJSONLocked(wfJob),
		"check_run_url":     fmt.Sprintf("%s/check-runs/%d", apiBase, id),
		"labels":            labelsForJob(wfJob),
		"runner_id":         nil,
		"runner_name":       nil,
		"runner_group_id":   nil,
		"runner_group_name": nil,
	}
}

// jobStepsJSONLocked renders the `steps` array from the runner-uploaded Task
// timeline records; empty (never fabricated) until the runner reports. Caller
// holds store.mu.
func (s *Server) jobStepsJSONLocked(wfJob *store.WorkflowJob) []map[string]any {
	tasks := s.taskRecordsForJobLocked(wfJob.JobID)
	steps := make([]map[string]any, 0, len(tasks))
	for i, rec := range tasks {
		steps = append(steps, map[string]any{
			"name":         rec.Name,
			"status":       stepStatus(rec.State),
			"conclusion":   stepConclusion(rec.State, rec.Result),
			"number":       i + 1,
			"started_at":   stepTimestamp(rec.StartTime),
			"completed_at": stepTimestamp(rec.FinishTime),
		})
	}
	return steps
}

// taskRecordsForJobLocked returns the job's Task timeline records in Order.
// Caller holds store.mu.
func (s *Server) taskRecordsForJobLocked(jobUUID string) []*store.TimelineRecord {
	planID := ""
	if job := s.store.Jobs[jobUUID]; job != nil {
		planID = job.PlanID
	}
	if planID == "" {
		for _, wf := range s.store.Workflows {
			if wfJob, ok := actions.FindWorkflowJobByID(wf, jobUUID); ok {
				planID = wfJob.PlanID
				break
			}
		}
	}
	if planID == "" {
		return nil
	}
	var tasks []*store.TimelineRecord
	for _, rec := range s.store.TimelineRecords[planID] {
		if rec.Type == "Task" {
			tasks = append(tasks, rec)
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Order < tasks[j].Order })
	return tasks
}

// stepStatus maps a runner timeline-record state to GitHub's step status enum.
func stepStatus(state string) string {
	switch state {
	case "inProgress":
		return "in_progress"
	case "completed":
		return "completed"
	default:
		return "queued"
	}
}

// stepConclusion maps a timeline-record result to GitHub's step conclusion;
// null until the step completes.
func stepConclusion(state, result string) any {
	if state != "completed" {
		return nil
	}
	switch result {
	case "succeeded", "succeededWithIssues":
		return "success"
	case "failed":
		return "failure"
	case "canceled", "abandoned":
		return "cancelled"
	case "skipped":
		return "skipped"
	default:
		return nil
	}
}

// stepTimestamp normalizes a runner ISO-8601 timestamp to second-resolution
// RFC3339; null when unreported, passed through verbatim when it won't parse.
func stepTimestamp(ts string) any {
	if ts == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05Z")
	}
	return ts
}

func labelsForJob(wfJob *store.WorkflowJob) []string {
	// RunsOn is interface{}: YAML allows a scalar or a sequence. Normalize
	// both into the `labels` array.
	if wfJob.Def == nil || wfJob.Def.RunsOn == nil {
		return []string{}
	}
	switch v := wfJob.Def.RunsOn.(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []string:
		if len(v) > 0 {
			return v
		}
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{}
}

// runnerJSON converts a registered Agent to GitHub's `Runner` shape.
func runnerJSON(a *store.Agent, busy bool) map[string]any {
	labels := make([]map[string]any, 0, len(a.Labels))
	for _, l := range a.Labels {
		labelType := "custom"
		if l.Type == "system" {
			labelType = "read-only"
		}
		labels = append(labels, map[string]any{
			"id":   l.ID,
			"name": l.Name,
			"type": labelType,
		})
	}
	return map[string]any{
		"id":              int64(a.ID),
		"runner_group_id": agentGroupID(a),
		"name":            a.Name,
		"os":              actions.OSFromDescription(a.OSDescription),
		"status":          agentStatusForRunner(a.Status),
		"busy":            busy,
		"ephemeral":       false,
		"version":         versionForRunner(a),
		"labels":          labels,
	}
}

// busyAgentIDsLocked returns agents with an assigned, unfinished job. Caller
// holds store.mu.
func (s *Server) busyAgentIDsLocked() map[int]bool {
	busy := map[int]bool{}
	for _, j := range s.store.Jobs {
		if j.AgentID != 0 && j.Status != "completed" {
			busy[j.AgentID] = true
		}
	}
	return busy
}

// versionForRunner reports the agent's version, or nil when it advertised none.
func versionForRunner(a *store.Agent) any {
	if a.Version == "" {
		return nil
	}
	return a.Version
}

func agentStatusForRunner(internal string) string {
	if internal == "online" {
		return "online"
	}
	return "offline"
}

// findWorkflowByRunID looks up a workflow by its GitHub-facing RunID; nil if
// absent.
func (s *Server) findWorkflowByRunID(runID int) *store.Workflow {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	if wf := s.store.WorkflowsByRunID[runID]; wf != nil {
		return wf
	}
	// Fallback scan for directly-seeded stores (tests); the engine maintains
	// the index otherwise.
	for _, wf := range s.store.Workflows {
		if wf.RunID == runID {
			return wf
		}
	}
	return nil
}

func (s *Server) findWorkflowByRunIDInRepo(runID int, repo string) *store.Workflow {
	wf := s.findWorkflowByRunID(runID)
	if wf == nil || !workflowBelongsToRepo(wf, repo) {
		return nil
	}
	return wf
}

func workflowBelongsToRepo(wf *store.Workflow, repo string) bool {
	return wf != nil && wf.RepoFullName == repo
}

// findJobByStableID resolves a stable job ID back to (workflow, job);
// (nil, nil) if none hashes to it.
func (s *Server) findJobByStableID(jobID int64) (*store.Workflow, *store.WorkflowJob) {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, wf := range s.store.Workflows {
		for _, j := range wf.Jobs {
			if stableJobID(j.JobID) == jobID {
				return wf, j
			}
		}
	}
	return nil, nil
}

func (s *Server) findJobByStableIDInRepo(jobID int64, repo string) (*store.Workflow, *store.WorkflowJob) {
	wf, job := s.findJobByStableID(jobID)
	if job == nil || !workflowBelongsToRepo(wf, repo) {
		return nil, nil
	}
	return wf, job
}

// handleListWorkflowRuns serves GET .../actions/runs (?status, ?branch, ?event).
func (s *Server) handleListWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	repo := repoFullName(r)
	statusFilter := r.URL.Query().Get("status")
	branchFilter := r.URL.Query().Get("branch")
	eventFilter := r.URL.Query().Get("event")

	s.store.Mu.RLock()
	matching := []*store.Workflow{}
	for _, wf := range s.store.Workflows {
		if wf.RepoFullName != "" && wf.RepoFullName != repo {
			continue
		}
		if statusFilter != "" && !runMatchesStatusFilter(wf, statusFilter) {
			continue
		}
		if branchFilter != "" && headBranchOf(wf) != branchFilter {
			continue
		}
		if eventFilter != "" && eventOf(wf) != eventFilter {
			continue
		}
		matching = append(matching, wf)
	}
	s.store.Mu.RUnlock()

	sortRunsNewestFirst(matching)
	page := paginateAndLink(w, r, matching)
	base := s.baseURL(r)
	runRepoJSON := s.runRepoJSON(repo, base)
	runs := make([]map[string]any, 0, len(page))
	for _, wf := range page {
		runs = append(runs, workflowRunJSON(wf, base, repo, runRepoJSON))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":   len(matching),
		"workflow_runs": runs,
	})
}

func (s *Server) handleGetWorkflowRun(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	runID, err := strconv.Atoi(r.PathValue("run_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	wf := s.findWorkflowByRunIDInRepo(runID, repoFullName(r))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, workflowRunJSON(wf, base, repoFullName(r), s.runRepoJSON(repoFullName(r), base)))
}

// handleListWorkflowRunJobs serves GET .../actions/runs/{run_id}/jobs. The
// ?filter=latest|all parameter is accepted but ignored (attempts untracked here).
func (s *Server) handleListWorkflowRunJobs(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	runID, err := strconv.Atoi(r.PathValue("run_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	wf := s.findWorkflowByRunIDInRepo(runID, repoFullName(r))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	allJobs := make([]*store.WorkflowJob, 0, len(wf.Jobs))
	for _, j := range wf.Jobs {
		// Synthetic reusable-workflow gate/collector nodes are engine
		// bookkeeping; GitHub lists only the called jobs.
		if j.Hidden {
			continue
		}
		allJobs = append(allJobs, j)
	}
	s.store.Mu.RUnlock()

	page := paginateAndLink(w, r, allJobs)
	base := s.baseURL(r)
	repo := repoFullName(r)
	jobs := make([]map[string]any, 0, len(page))
	for _, j := range page {
		jobs = append(jobs, s.workflowJobJSON(wf, j, base, repo))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(allJobs),
		"jobs":        jobs,
	})
}

func (s *Server) handleGetWorkflowJob(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("job_id"), 10, 64)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid job_id")
		return
	}
	wf, j := s.findJobByStableIDInRepo(jobID, repoFullName(r))
	if j == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.workflowJobJSON(wf, j, s.baseURL(r), repoFullName(r)))
}

// handleGetWorkflowJobLogs serves GET .../actions/jobs/{job_id}/logs as
// text/plain assembled from the runner-uploaded log files.
func (s *Server) handleGetWorkflowJobLogs(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("job_id"), 10, 64)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid job_id")
		return
	}
	_, j := s.findJobByStableIDInRepo(jobID, repoFullName(r))
	if j == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	content, ok, readErr := s.jobLogContent(r.Context(), j.JobID)
	if readErr != nil {
		writeGHError(w, http.StatusInternalServerError, "log byte-store read: "+readErr.Error())
		return
	}
	if !ok {
		writeGHError(w, http.StatusNotFound, "Logs not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) handleGetWorkflowJobSummary(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("job_id"), 10, 64)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid job_id")
		return
	}
	_, job := s.findJobByStableIDInRepo(jobID, repoFullName(r))
	if job == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	summary := job.Summary
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]string{"summary": summary})
}

type jobLogRef struct {
	ID   int
	Name string
}

// jobLogContent assembles the job's log from runner-uploaded log files in
// step Order. Live console capture is never used as a download substitute.
func (s *Server) jobLogContent(ctx context.Context, jobUUID string) ([]byte, bool, error) {
	s.store.Mu.RLock()
	refs := s.jobLogRefsLocked(jobUUID)
	memoryLogs := s.memoryLogFilesForDownloadLocked(refs)
	s.store.Mu.RUnlock()

	var buf bytes.Buffer
	for _, ref := range refs {
		content, ok, err := s.logFileContent(ctx, ref.ID, memoryLogs[ref.ID])
		if err != nil {
			return nil, false, err
		}
		if !ok {
			continue
		}
		buf.Write(content)
		if content[len(content)-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	if buf.Len() > 0 {
		return buf.Bytes(), true, nil
	}
	return nil, false, nil
}

// jobLogRefsLocked returns the job's log references in step order. Caller
// holds store.mu.
func (s *Server) jobLogRefsLocked(jobUUID string) []jobLogRef {
	tasks := s.taskRecordsForJobLocked(jobUUID)
	refs := make([]jobLogRef, 0, len(tasks))
	for _, rec := range tasks {
		if rec.Log == nil {
			continue
		}
		refs = append(refs, jobLogRef{ID: rec.Log.ID, Name: rec.Name})
	}
	return refs
}

// memoryLogFilesForDownloadLocked snapshots log bytes only when no object
// byte store is configured; otherwise logs are read back from the byte store.
func (s *Server) memoryLogFilesForDownloadLocked(refs []jobLogRef) map[int][]byte {
	if s.artifactStore.ByteStore != nil {
		return nil
	}
	out := make(map[int][]byte, len(refs))
	for _, ref := range refs {
		if content := s.store.LogFiles[ref.ID]; len(content) > 0 {
			out[ref.ID] = append([]byte(nil), content...)
		}
	}
	return out
}

func (s *Server) logFileContent(ctx context.Context, logID int, memoryContent []byte) ([]byte, bool, error) {
	if s.artifactStore.ByteStore != nil {
		content, err := s.artifactStore.ByteStore.Get(ctx, store.LogDataKey(logID))
		if err != nil {
			return nil, false, err
		}
		if len(content) == 0 {
			return nil, false, nil
		}
		return content, true, nil
	}
	if len(memoryContent) == 0 {
		return nil, false, nil
	}
	return memoryContent, true, nil
}

func (s *Server) handleCancelWorkflowRun(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.Atoi(r.PathValue("run_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	wf := s.findWorkflowByRunIDInRepo(runID, repoFullName(r))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.actions.CancelWorkflow(wf)
	// 202 with an empty object, not an empty body: `gh run cancel` decodes the
	// body and aborts on a zero-length one.
	writeJSON(w, http.StatusAccepted, map[string]interface{}{})
}

// handleRerunWorkflowRun serves POST .../actions/runs/{run_id}/rerun by
// replaying the run's cached WorkflowFile YAML with its original event
// metadata; 422 when no cached YAML ties the run to a registered file.
func (s *Server) handleRerunWorkflowRun(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.Atoi(r.PathValue("run_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	wf := s.findWorkflowByRunIDInRepo(runID, repoFullName(r))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := wf.RepoFullName
	if repo == "" {
		repo = repoFullName(r)
	}
	match, err := s.cachedWorkflowFileForRun(repo, wf)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	def, perr := store.ParseWorkflow([]byte(match.YAML))
	if perr != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "parse cached YAML: "+perr.Error())
		return
	}
	def = actions.ExpandMatrixJobs(def)
	if def.Env == nil {
		def.Env = map[string]string{}
	}
	serverURL := s.baseURL(r)
	def.Env["__serverURL"] = serverURL
	def.Env["__defaultImage"] = ""
	if err := s.rerunWorkflowAsNewAttempt(r, wf, match, def, serverURL, nil); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "rerun submit: "+err.Error())
		return
	}
	// 201 with an empty object, same reason as cancel above.
	writeJSON(w, http.StatusCreated, map[string]interface{}{})
}

func (s *Server) cachedWorkflowFileForRun(repo string, wf *store.Workflow) (*store.WorkflowFile, error) {
	s.store.DiscoverWorkflowFilesFromGit(repo)
	if wf.WorkflowFileID != 0 {
		if f := s.store.GetWorkflowFile(repo, wf.WorkflowFileID); f != nil && f.YAML != "" {
			return f, nil
		}
	}
	if wf.WorkflowFilePath != "" {
		for _, f := range s.store.ListWorkflowFiles(repo) {
			if f.Path == wf.WorkflowFilePath && f.YAML != "" {
				return f, nil
			}
		}
	}
	var matches []*store.WorkflowFile
	for _, f := range s.store.ListWorkflowFiles(repo) {
		if f.Name == wf.Name && f.YAML != "" {
			matches = append(matches, f)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("no cached workflow YAML for this run (push the workflow file to git or POST /api/v3/bleephub/workflow first)")
	default:
		return nil, fmt.Errorf("ambiguous cached workflow YAML for this run (multiple workflow files are named %q)", wf.Name)
	}
}

// rerunWorkflowAsNewAttempt archives the current run and re-submits under the
// SAME run id with run_attempt+1 (GitHub never mints a new run id for a rerun).
// carryOver pre-completes the listed job keys with the prior attempt's results.
func (s *Server) rerunWorkflowAsNewAttempt(r *http.Request, old *store.Workflow, file *store.WorkflowFile, def *store.WorkflowDef, serverURL string, carryOver map[string]*store.WorkflowJob) error {
	// Archive + remove the old attempt first; restore on submit failure.
	s.store.Mu.Lock()
	s.store.WorkflowAttempts[old.RunID] = append(s.store.WorkflowAttempts[old.RunID], old)
	delete(s.store.Workflows, old.ID)
	s.store.UnindexWorkflowLocked(old)
	s.store.PersistWorkflowAttemptsRecord(old.RunID)
	s.store.DeleteWorkflowRecord(old.ID)
	s.store.Mu.Unlock()
	s.actions.StopTimeoutWatcher(old)

	meta := actions.WorkflowEventMeta{
		EventName:      eventOf(old),
		Ref:            old.Ref,
		Sha:            old.Sha,
		Repo:           old.RepoFullName,
		Inputs:         old.Inputs,
		TypedInputs:    old.TypedInputs,
		Payload:        old.EventPayload,
		ReuseRunID:     old.RunID,
		ReuseRunNumber: old.RunNumber,
		Attempt:        old.AttemptNumber() + 1,
		CarryOverJobs:  carryOver,
	}
	if file != nil {
		meta.WorkflowFileID = file.ID
		meta.WorkflowFilePath = file.Path
	}
	if _, err := s.actions.SubmitWorkflow(r.Context(), serverURL, def, "", &meta); err != nil {
		// Put the old attempt back so the run doesn't vanish.
		s.store.Mu.Lock()
		attempts := s.store.WorkflowAttempts[old.RunID]
		if n := len(attempts); n > 0 && attempts[n-1] == old {
			s.store.WorkflowAttempts[old.RunID] = attempts[:n-1]
		}
		s.store.Workflows[old.ID] = old
		s.store.PersistWorkflowAttemptsRecord(old.RunID)
		s.store.PersistWorkflowRecord(old)
		s.store.Mu.Unlock()
		return err
	}
	return nil
}

// findRunAttempt resolves a run's specific attempt: the live run, else an
// archived attempt.
func (s *Server) findRunAttempt(runID, attempt int, repo string) *store.Workflow {
	current := s.findWorkflowByRunIDInRepo(runID, repo)
	if current != nil && current.AttemptNumber() == attempt {
		return current
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, archived := range s.store.WorkflowAttempts[runID] {
		if archived.AttemptNumber() == attempt && workflowBelongsToRepo(archived, repo) {
			return archived
		}
	}
	return nil
}

func (s *Server) handleGetRunAttempt(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	runID, err := strconv.Atoi(r.PathValue("run_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	attempt, err := strconv.Atoi(r.PathValue("attempt_number"))
	if err != nil || attempt < 1 {
		writeGHError(w, http.StatusBadRequest, "invalid attempt_number")
		return
	}
	wf := s.findRunAttempt(runID, attempt, repoFullName(r))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	base := s.baseURL(r)
	repo := repoFullName(r)
	writeJSON(w, http.StatusOK, workflowRunJSON(wf, base, repo, s.runRepoJSON(repo, base)))
}

func (s *Server) handleListRunAttemptJobs(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	runID, err := strconv.Atoi(r.PathValue("run_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	attempt, err := strconv.Atoi(r.PathValue("attempt_number"))
	if err != nil || attempt < 1 {
		writeGHError(w, http.StatusBadRequest, "invalid attempt_number")
		return
	}
	wf := s.findRunAttempt(runID, attempt, repoFullName(r))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	allJobs := make([]*store.WorkflowJob, 0, len(wf.Jobs))
	for _, j := range wf.Jobs {
		if j.Hidden {
			continue
		}
		allJobs = append(allJobs, j)
	}
	s.store.Mu.RUnlock()
	page := paginateAndLink(w, r, allJobs)
	base := s.baseURL(r)
	repo := repoFullName(r)
	jobs := make([]map[string]any, 0, len(page))
	for _, j := range page {
		jobs = append(jobs, s.workflowJobJSON(wf, j, base, repo))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(allJobs),
		"jobs":        jobs,
	})
}

func (s *Server) handleDeleteWorkflowRun(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.Atoi(r.PathValue("run_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	s.store.Mu.Lock()
	var foundKey string
	for k, wf := range s.store.Workflows {
		if wf.RunID == runID && workflowBelongsToRepo(wf, repoFullName(r)) {
			foundKey = k
			break
		}
	}
	if foundKey == "" {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	deleted := s.store.Workflows[foundKey]
	delete(s.store.Workflows, foundKey)
	s.store.UnindexWorkflowLocked(deleted)
	// Eagerly tear down the run's replica-local job runtime state rather than
	// waiting for the janitor.
	planIDs := s.store.DropWorkflowJobStateLocked(deleted)
	for _, attempt := range s.store.WorkflowAttempts[runID] {
		planIDs = append(planIDs, s.store.DropWorkflowJobStateLocked(attempt)...)
	}
	s.store.DeleteWorkflowRecord(foundKey)
	delete(s.store.WorkflowAttempts, runID)
	s.store.PersistWorkflowAttemptsRecord(runID)
	s.store.Mu.Unlock()
	s.actions.ReleaseJobLogFiles(planIDs)
	w.WriteHeader(http.StatusNoContent)
}

// handleListRunners serves repo, org, and enterprise runner inventories. A
// repo inventory also includes runners shared by its org; org and enterprise
// inventories include only runners registered at that exact scope.
func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerTargetFromRequest(w, r)
	if !ok {
		return
	}
	s.store.Mu.RLock()
	all := make([]*store.Agent, 0, len(s.store.Agents))
	for _, a := range s.store.Agents {
		if runnerVisibleAt(a.Scope, target) {
			all = append(all, a)
		}
	}
	busy := s.busyAgentIDsLocked()
	s.store.Mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	page := paginateAndLink(w, r, all)
	runners := make([]map[string]any, 0, len(page))
	for _, a := range page {
		runners = append(runners, runnerJSON(a, busy[a.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(all),
		"runners":     runners,
	})
}

func (s *Server) handleGetRunner(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerTargetFromRequest(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	a := s.store.Agents[id]
	busy := s.busyAgentIDsLocked()
	s.store.Mu.RUnlock()
	if a == nil || !runnerVisibleAt(a.Scope, target) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, runnerJSON(a, busy[a.ID]))
}

func (s *Server) handleDeleteRunner(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerTargetFromRequest(w, r)
	if !ok {
		return
	}
	runnerID, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid runner_id")
		return
	}
	s.store.Mu.Lock()
	agent := s.store.Agents[runnerID]
	if agent == nil || !runnerVisibleAt(agent.Scope, target) {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(s.store.Agents, runnerID)
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// runnerTargetFromRequest resolves the runner inventory named by a REST path.
func (s *Server) runnerTargetFromRequest(w http.ResponseWriter, r *http.Request) (store.RunnerScope, bool) {
	target, err := s.runnerScopeFromRequest(r)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return store.RunnerScope{}, false
	}
	return target, true
}

func runnerVisibleAt(agentScope, target store.RunnerScope) bool {
	switch {
	case target.Repo != "":
		if strings.EqualFold(agentScope.Repo, target.Repo) {
			return true
		}
		owner, _, ok := strings.Cut(target.Repo, "/")
		return ok && strings.EqualFold(agentScope.Org, owner)
	case target.Org != "":
		return strings.EqualFold(agentScope.Org, target.Org)
	case target.Enterprise != "":
		return strings.EqualFold(agentScope.Enterprise, target.Enterprise)
	default:
		return false
	}
}
