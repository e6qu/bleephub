package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerRunServiceRoutes() {
	// Acquire / renew / complete job requests. The runner listener holds an
	// agent session token here, and every one of these names a single job by
	// its {requestId}, so each is bound to the runner the broker handed that
	// job to.
	s.route("GET /_apis/v1/AgentRequest/{poolId}/{requestId}", s.requireAssignedAgent(s.handleGetRequest))
	s.route("PATCH /_apis/v1/AgentRequest/{poolId}/{requestId}", s.requireAssignedAgent(s.handleRenewRequest))
	s.route("PUT /_apis/v1/AgentRequest/{poolId}/{requestId}", s.requireAssignedAgent(s.handleRenewRequest))
	s.route("DELETE /_apis/v1/AgentRequest/{poolId}/{requestId}", s.requireAssignedAgent(s.handleCompleteRequest))

	// FinishJob (the worker reports job completion) and the legacy plan event
	// route both act on the job named by {planId}, so both are bound to that
	// job's runtime token.
	s.route("POST /_apis/v1/FinishJob/{scopeId}/{hubName}/{planId}", s.requirePlanJob(s.handleFinishJob))
	s.route("PUT /_apis/v1/plans/{planId}/events", s.requirePlanJob(s.handleJobEvents))

	// CustomerIntelligence (telemetry, accept and discard). Nothing in the
	// path names a job, so the gate is a verified runner credential of either
	// audience.
	s.route("POST /_apis/v1/tasks", s.requireRunnerAuth(s.handleTelemetry))
}

// requireAssignedAgent gates a route that names a job by its {requestId} on
// the agent session of the runner that job was dispatched to. The broker
// records the agent when it hands the job message out, so ownership is server
// state rather than a claim on the request: one runner cannot read another
// runner's job message — it carries that job's secrets and runtime token — nor
// renew or complete its request.
func (s *Server) requireAssignedAgent(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAgentSession(func(w http.ResponseWriter, r *http.Request) {
		reqID, err := strconv.ParseInt(r.PathValue("requestId"), 10, 64)
		if err != nil {
			http.Error(w, "invalid request ID", http.StatusBadRequest)
			return
		}
		job := s.lookupJobByRequestID(reqID)
		if job == nil {
			http.Error(w, "request not found", http.StatusNotFound)
			return
		}
		s.store.Mu.RLock()
		assigned := job.AgentID
		s.store.Mu.RUnlock()
		if assigned == 0 || assigned != runnerFromContext(r.Context()).Agent.ID {
			writeGHError(w, http.StatusForbidden, "This job request belongs to another runner")
			return
		}
		next(w, r)
	})
}

// requirePlanJob gates a route that names a job by its {planId} on that job's
// runtime token. Every such handler reads the {planId}, so that is what the
// credential is bound to: the plan's own scopeIdentifier is resolved from the
// dispatched job message and compared against the already-verified sub claim.
// Checking the {scopeId} path segment instead leaves the parameter the
// handlers act on unchecked, which is a job writing another job's plan.
func (s *Server) requirePlanJob(next http.HandlerFunc) http.HandlerFunc {
	return s.requireJobToken(func(w http.ResponseWriter, r *http.Request) {
		planID := r.PathValue("planId")
		job := s.lookupJobByPlanID(planID)
		if job == nil {
			http.Error(w, "plan not found", http.StatusNotFound)
			return
		}
		// The plan scope was recorded at dispatch time, so this neither
		// re-parses the secret-bearing job message on every request nor breaks
		// when that message has been cleared at run finalization (the fallback
		// inside planScopeForJobLocked parses the message for directly-seeded
		// jobs only).
		s.store.Mu.RLock()
		scopeID := s.store.PlanScopeForJobLocked(job).ScopeID
		s.store.Mu.RUnlock()
		if scopeID == "" || scopeID != runnerFromContext(r.Context()).Claims.Sub {
			writeGHError(w, http.StatusForbidden, "Job token does not cover this plan")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	reqID, err := strconv.ParseInt(r.PathValue("requestId"), 10, 64)
	if err != nil {
		http.Error(w, "invalid request ID", http.StatusBadRequest)
		return
	}

	job := s.lookupJobByRequestID(reqID)
	if job == nil {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	s.logger.Debug().Int64("requestId", reqID).Msg("get request")

	// Return the full job message as the request details
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(job.Message))
}

func (s *Server) handleRenewRequest(w http.ResponseWriter, r *http.Request) {
	reqID, err := strconv.ParseInt(r.PathValue("requestId"), 10, 64)
	if err != nil {
		http.Error(w, "invalid request ID", http.StatusBadRequest)
		return
	}

	job := s.lookupJobByRequestID(reqID)
	if job == nil {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	// The body would carry status fields but bleephub doesn't consume
	// them today; drain explicitly so it's obvious there's no decode.
	_, _ = io.Copy(io.Discard, r.Body)

	s.store.Mu.Lock()
	startedRunning := false
	if job.Status == "queued" {
		job.Status = "running"
		startedRunning = true
	}
	job.LockedUntil = time.Now().Add(1 * time.Hour)
	// Mirror the runner pickup onto the workflow job: the jobs API and
	// the checks layer report in_progress from this moment.
	if startedRunning {
		for _, wf := range s.store.Workflows {
			if wfJob, ok := findWorkflowJobByID(wf, job.ID); ok {
				if wfJob.Status == JobStatusQueued {
					wfJob.Status = JobStatusRunning
					wfJob.StartedAt = time.Now()
					s.store.PersistWorkflowRecord(wf)
					s.queueActionsEvent(evJobInProgress, wf, wfJob)
				}
				break
			}
		}
	}
	// Snapshot the fields read below while the lock is held; other
	// goroutines (the broker, completion) mutate job.Status/LockedUntil.
	jobStatusSnap := job.Status
	lockedUntilSnap := job.LockedUntil
	jobPlanID := job.PlanID
	jobIDSnap := job.ID
	s.store.Mu.Unlock()

	s.logger.Info().
		Str("method", r.Method).
		Int64("requestId", reqID).
		Str("status", jobStatusSnap).
		Msg("renew/update request")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"requestId":   reqID,
		"lockedUntil": lockedUntilSnap.UTC().Format(time.RFC3339),
		"planId":      jobPlanID,
		"jobId":       jobIDSnap,
	})
}

func (s *Server) handleCompleteRequest(w http.ResponseWriter, r *http.Request) {
	reqID, err := strconv.ParseInt(r.PathValue("requestId"), 10, 64)
	if err != nil {
		http.Error(w, "invalid request ID", http.StatusBadRequest)
		return
	}

	result := r.URL.Query().Get("result")

	job := s.lookupJobByRequestID(reqID)
	if job == nil {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	s.store.Mu.Lock()
	s.store.MarkJobCompletedLocked(job)
	if result != "" {
		job.Result = result
	}
	// Snapshot under the lock: job fields are concurrently written by the
	// broker (e.g. recordJobAgentLocked sets AgentID), so reads after the
	// unlock must use locals, not the shared *Job.
	jobIDSnap := job.ID
	jobResultSnap := job.Result
	s.store.Mu.Unlock()

	s.logger.Info().
		Int64("requestId", reqID).
		Str("job_id", jobIDSnap).
		Str("result", result).
		Msg("job request completed (DELETE)")

	// Notify workflow engine of job completion
	s.onJobCompleted(r.Context(), jobIDSnap, jobResultSnap)

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleFinishJob(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("planId")

	// FinishJob is the service-location route used by the official runner's
	// JobServer RaisePlanEventAsync call. Its body is the JobCompletedEvent
	// wire contract, including runner-evaluated job outputs.
	var body runnerJobEvent
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.Name != "JobCompleted" {
		http.Error(w, "FinishJob requires a JobCompleted event", http.StatusBadRequest)
		return
	}

	result, err := runnerJobResult(body.Result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	jobID := body.JobID

	s.logger.Info().
		Str("planId", planID).
		Str("jobId", jobID).
		Str("result", result).
		Msg("job finished")

	// The plan names the job. A jobId in the body may confirm it but never
	// select a different one: completing whatever job the body pointed at was
	// a way to finish another runner's work.
	job := s.lookupJobByPlanID(planID)
	if job == nil {
		http.Error(w, "plan not found", http.StatusNotFound)
		return
	}
	if jobID != "" && jobID != job.ID {
		http.Error(w, "JobCompleted event jobId does not match plan", http.StatusBadRequest)
		return
	}

	// Store the official runner's evaluated outputs before completing the
	// workflow job. Completion can synchronously dispatch a downstream
	// job whose needs context must already contain these values.
	s.captureResolvedJobOutputs(job.ID, body.Outputs)

	s.store.Mu.Lock()
	s.store.MarkJobCompletedLocked(job)
	job.Result = result
	// Snapshot under the lock: the broker concurrently mutates these
	// fields (recordJobAgentLocked writes AgentID), so every read after
	// the unlock must come from a local, not the shared *Job.
	jobIDSnap := job.ID
	jobResultSnap := job.Result
	s.store.Mu.Unlock()
	s.logger.Info().Str("jobId", jobIDSnap).Str("result", jobResultSnap).Msg("job status updated")

	// Notify workflow engine of job completion
	s.onJobCompleted(r.Context(), jobIDSnap, jobResultSnap)

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

// captureResolvedJobOutputs stores the output names and values evaluated by
// actions/runner. The JobCompleted event already contains the declared output
// names (for example, "version"), so resolving step expressions a second time
// on the server would discard the official runner result.
func (s *Server) captureResolvedJobOutputs(jobID string, outputs map[string]runnerVariableValue) {
	if len(outputs) == 0 {
		return
	}

	resolved := make(map[string]string, len(outputs))
	for name, output := range outputs {
		resolved[name] = output.Value
	}

	s.store.Mu.Lock()
	var wfJob *WorkflowJob
	var workflow *Workflow
	for _, wf := range s.store.Workflows {
		if job, ok := findWorkflowJobByID(wf, jobID); ok {
			wfJob = job
			workflow = wf
			break
		}
	}
	if wfJob != nil {
		if wfJob.Outputs == nil {
			wfJob.Outputs = make(map[string]string, len(resolved))
		}
		for name, value := range resolved {
			wfJob.Outputs[name] = value
		}
		s.store.PersistWorkflowRecord(workflow)
	}
	s.store.Mu.Unlock()

	if wfJob != nil {
		s.logger.Info().
			Str("jobId", jobID).
			Interface("outputs", resolved).
			Msg("job outputs captured")
	}
}

// findWorkflowJobByID scans a workflow's jobs for the given engine job
// UUID. Callers hold the store lock.
func findWorkflowJobByID(wf *Workflow, jobID string) (*WorkflowJob, bool) {
	for _, wfJob := range wf.Jobs {
		if wfJob.JobID == jobID {
			return wfJob, true
		}
	}
	return nil, false
}

func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("planId")

	var body runnerJobEvent
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid job event body: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.logger.Debug().Str("planId", planID).Str("event", body.Name).Msg("job event")
	if body.Name == "JobCompleted" {
		// As on FinishJob: the plan names the job, and a jobId in the body may
		// only confirm it.
		job := s.lookupJobByPlanID(planID)
		if job == nil {
			http.Error(w, "plan not found", http.StatusNotFound)
			return
		}
		if body.JobID != "" && body.JobID != job.ID {
			http.Error(w, "JobCompleted event jobId does not match plan", http.StatusBadRequest)
			return
		}

		s.captureResolvedJobOutputs(job.ID, body.Outputs)
		result, err := runnerJobResult(body.Result)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.store.Mu.Lock()
		s.store.MarkJobCompletedLocked(job)
		job.Result = result
		jobIDSnap := job.ID
		s.store.Mu.Unlock()
		s.onJobCompleted(r.Context(), jobIDSnap, result)
	}

	w.WriteHeader(http.StatusOK)
}

type runnerVariableValue struct {
	Value    string `json:"value"`
	IsSecret bool   `json:"isSecret"`
}

type runnerJobEvent struct {
	Name      string                         `json:"name"`
	JobID     string                         `json:"jobId"`
	RequestID int64                          `json:"requestId"`
	Result    json.RawMessage                `json:"result"`
	Outputs   map[string]runnerVariableValue `json:"outputs"`
}

// runnerJobResult maps the official actions/runner TaskResult JSON value onto
// the completion strings already consumed by Bleephub's workflow engine.
func runnerJobResult(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", fmt.Errorf("JobCompleted event is missing result")
	}
	var numeric int
	if err := json.Unmarshal(raw, &numeric); err == nil {
		switch numeric {
		case 0:
			return "Succeeded", nil
		case 1:
			return "SucceededWithIssues", nil
		case 2:
			return "Failed", nil
		case 3:
			return "Cancelled", nil
		case 4:
			return "Skipped", nil
		case 5:
			return "Abandoned", nil
		default:
			return "", fmt.Errorf("JobCompleted event has invalid result %d", numeric)
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || text == "" {
		return "", fmt.Errorf("JobCompleted event has invalid result")
	}
	switch strings.ToLower(text) {
	case "succeeded":
		return "Succeeded", nil
	case "succeededwithissues", "succeeded_with_issues":
		return "SucceededWithIssues", nil
	case "failed":
		return "Failed", nil
	case "canceled", "cancelled":
		return "Cancelled", nil
	case "skipped":
		return "Skipped", nil
	case "abandoned":
		return "Abandoned", nil
	default:
		return "", fmt.Errorf("JobCompleted event has invalid result %q", text)
	}
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug().Msg("telemetry event (discarded)")
	w.WriteHeader(http.StatusOK)
}
