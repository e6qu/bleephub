package bleephub

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerJobRoutes() {
	// Operator-only job/workflow control plane. GitHub has no equivalent: jobs
	// are children of workflow runs created via workflow_dispatch, push, or
	// repository_dispatch events. These routes are compatibility debt under
	// /internal/exec/, never the GitHub-compatible application programming
	// interface surface.
	s.route("POST /internal/exec/submit", s.handleSubmitJob)
	s.route("GET /internal/exec/jobs/{jobId}", s.handleGetJobStatus)

	// Workflow YAML submission
	s.route("POST /internal/exec/workflow", s.handleSubmitWorkflow)
	s.route("GET /internal/exec/workflows/{workflowId}", s.handleGetWorkflowStatus)

	// Workflow cancellation
	s.route("POST /internal/exec/workflows/{workflowId}/cancel", s.handleCancelWorkflow)

	// ActionDownloadInfo — the worker resolves an action reference to the
	// commit it downloads (handler in actions.go). The answer names the sha a
	// ref resolves to in bleephub git storage, private repositories included,
	// so it is bound to the runtime token of the job whose plan the path
	// carries.
	s.route("POST /_apis/v1/ActionDownloadInfo/{scopeId}/{hubName}/{planId}", s.requirePlanJob(s.handleActionDownloadInfo))

	// Task definitions. Nothing in the path names a job, so the gate is a
	// verified runner credential of either audience.
	s.route("GET /_apis/v1/tasks/{taskId}/{versionString}", s.requireRunnerAuth(s.handleGetTask))
}

func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	var req actions.SubmitRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Image == "" && !req.HostMode {
		http.Error(w, "image or hostMode required", http.StatusBadRequest)
		return
	}

	serverURL := s.baseURL(r)

	jobID := uuid.New().String()
	planID := uuid.New().String()
	timelineID := uuid.New().String()
	requestID := s.actions.NextRequestID()

	// The runtime token is minted here — signing stays in the auth layer —
	// and travels into the engine's message builder.
	scopeID := uuid.New().String()
	jobToken := makeJWT(scopeID, "actions")
	msg := actions.BuildJobMessage(serverURL, jobID, planID, timelineID, requestID, &req, scopeID, jobToken)
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	job := &store.Job{
		ID:          jobID,
		RequestID:   requestID,
		PlanID:      planID,
		TimelineID:  timelineID,
		Status:      "queued",
		Message:     string(msgJSON),
		LockedUntil: time.Now().Add(1 * time.Hour),
	}

	s.store.Mu.Lock()
	s.store.Jobs[jobID] = job
	// Operator-submitted jobs name no repository; the empty repo scope is the
	// narrowest one there is (see repoForJobScope).
	s.store.RegisterDispatchedJobLocked(job, msg, "")
	s.store.RegisterJobLogMasksLocked(planID, msg)
	s.store.Mu.Unlock()

	// Build the envelope message
	envelope := &store.TaskAgentMessage{
		MessageID:   s.actions.NextMessageID(),
		MessageType: "PipelineAgentJobRequest",
		Body:        string(msgJSON),
		JobID:       jobID,
	}

	s.actions.QueueJobMessage(envelope)

	s.logger.Info().Str("jobId", jobID).Int64("requestId", requestID).Msg("job submitted")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobId":     jobID,
		"requestId": requestID,
		"status":    "queued",
	})
}

func (s *Server) handleGetJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")

	s.store.Mu.RLock()
	job, ok := s.store.Jobs[jobID]
	s.store.Mu.RUnlock()

	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobId":  job.ID,
		"status": job.Status,
		"result": job.Result,
	})
}

// WorkflowSubmitRequest is the workflow YAML submission format.
type WorkflowSubmitRequest struct {
	Workflow  string            `json:"workflow"`   // raw YAML
	Image     string            `json:"image"`      // default container image
	HostMode  bool              `json:"hostMode"`   // run jobs on the runner (no container) unless the YAML declares one
	EventName string            `json:"event_name"` // default "push"
	Ref       string            `json:"ref"`        // repository-scoped default is the repository default branch
	Sha       string            `json:"sha"`        // optional explicit commit SHA for repo-less operator submissions
	Repo      string            `json:"repo"`       // optional repository scope, when the workflow belongs to a repo
	Inputs    map[string]string `json:"inputs"`     // workflow_dispatch inputs
}

func (s *Server) handleSubmitWorkflow(w http.ResponseWriter, r *http.Request) {
	var req WorkflowSubmitRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Workflow == "" {
		http.Error(w, "workflow YAML required", http.StatusBadRequest)
		return
	}

	wfDef, err := store.ParseWorkflow([]byte(req.Workflow))
	if err != nil {
		http.Error(w, "parse workflow: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Enforce concurrent workflow limit
	if s.maxConcurrentWorkflows > 0 {
		s.store.Mu.RLock()
		active := 0
		for _, wf := range s.store.Workflows {
			if wf.Status == "running" {
				active++
			}
		}
		s.store.Mu.RUnlock()
		if active >= s.maxConcurrentWorkflows {
			http.Error(w, "too many concurrent workflows", http.StatusTooManyRequests)
			return
		}
	}

	if req.Image == "" && !req.HostMode {
		http.Error(w, "image or hostMode required", http.StatusBadRequest)
		return
	}

	serverURL := s.baseURL(r)

	// Apply defaults for event metadata
	eventName := req.EventName
	if eventName == "" {
		eventName = "push"
	}
	ref := req.Ref
	sha := req.Sha
	repo := req.Repo
	const zeroSha = "0000000000000000000000000000000000000000"

	// Auto-register the WorkflowFile so /api/v3/repos/{o}/{r}/actions/
	// workflows lists this submission. Path defaults to the conventional
	// `.github/workflows/<name>.yml` shape; the YAML body is cached so
	// the dispatch + rerun endpoints can replay it later.
	wfName := wfDef.Name
	if wfName == "" {
		wfName = "workflow"
	}
	wfPath := ".github/workflows/" + wfName + ".yml"
	if repo != "" {
		repoRow := s.store.GetRepoByFullName(repo)
		if repoRow == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		if ref == "" {
			ref = "refs/heads/" + repoRow.DefaultBranch
		}
		if sha == "" {
			parts := strings.SplitN(repo, "/", 2)
			if len(parts) != 2 {
				writeGHError(w, http.StatusUnprocessableEntity, "Invalid repository scope")
				return
			}
			stor := s.store.GetGitStorage(parts[0], parts[1])
			if stor == nil {
				writeGHError(w, http.StatusUnprocessableEntity, "Repository git storage is not available")
				return
			}
			sha = actions.ResolveRefSha(stor, ref)
			if sha == "" || sha == zeroSha {
				writeGHError(w, http.StatusUnprocessableEntity, "No ref found for: "+ref)
				return
			}
		} else if sha == zeroSha {
			writeGHError(w, http.StatusUnprocessableEntity, "No ref found for: "+ref)
			return
		}
		s.store.RegisterWorkflowFile(repo, wfPath, wfName, req.Workflow, "submitted")
	}

	// Expand matrix strategies
	expandedDef := actions.ExpandMatrixJobs(wfDef)

	// Store serverURL for re-dispatch after job completion
	if expandedDef.Env == nil {
		expandedDef.Env = make(map[string]string)
	}
	expandedDef.Env["__serverURL"] = serverURL
	expandedDef.Env["__defaultImage"] = req.Image

	eventMeta := actions.WorkflowEventMeta{
		EventName: eventName,
		Ref:       ref,
		Sha:       sha,
		Repo:      repo,
		Inputs:    req.Inputs,
	}

	workflow, err := s.actions.SubmitWorkflow(r.Context(), serverURL, expandedDef, req.Image, &eventMeta)
	if err != nil {
		http.Error(w, "submit: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.logger.Info().
		Str("workflow_id", workflow.ID).
		Str("workflow_name", workflow.Name).
		Int("jobs", len(workflow.Jobs)).
		Msg("workflow submitted")

	// Build response with job info
	jobs := make(map[string]interface{}, len(workflow.Jobs))
	for key, wfJob := range workflow.Jobs {
		jobs[key] = map[string]interface{}{
			"jobId":  wfJob.JobID,
			"status": wfJob.Status,
			"name":   wfJob.DisplayName,
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workflowId": workflow.ID,
		"jobs":       jobs,
		"status":     workflow.Status,
	})
}

func (s *Server) handleGetWorkflowStatus(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("workflowId")

	s.store.Mu.RLock()
	wf, ok := s.store.Workflows[wfID]
	s.store.Mu.RUnlock()

	if !ok {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	jobs := make(map[string]interface{}, len(wf.Jobs))
	for key, wfJob := range wf.Jobs {
		jobs[key] = map[string]interface{}{
			"jobId":  wfJob.JobID,
			"status": wfJob.Status,
			"result": wfJob.Result,
			"name":   wfJob.DisplayName,
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workflowId": wf.ID,
		"status":     wf.Status,
		"result":     wf.Result,
		"jobs":       jobs,
	})
}

func (s *Server) handleCancelWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("workflowId")

	s.store.Mu.RLock()
	wf, ok := s.store.Workflows[wfID]
	s.store.Mu.RUnlock()

	if !ok {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	if wf.Status == "completed" {
		http.Error(w, "workflow already completed", http.StatusConflict)
		return
	}

	s.actions.CancelWorkflow(wf)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workflowId": wf.ID,
		"status":     wf.Status,
		"result":     wf.Result,
	})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskId")
	s.logger.Debug().Str("taskId", taskID).Msg("task definition requested")
	http.Error(w, "task not found", http.StatusNotFound)
}
