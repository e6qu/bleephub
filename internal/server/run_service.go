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
	// Acquire / renew / complete job requests
	s.route("GET /_apis/v1/AgentRequest/{poolId}/{requestId}", s.handleGetRequest)
	s.route("PATCH /_apis/v1/AgentRequest/{poolId}/{requestId}", s.handleRenewRequest)
	s.route("PUT /_apis/v1/AgentRequest/{poolId}/{requestId}", s.handleRenewRequest)
	s.route("DELETE /_apis/v1/AgentRequest/{poolId}/{requestId}", s.handleCompleteRequest)

	// FinishJob (runner reports job completion)
	s.route("POST /_apis/v1/FinishJob/{scopeId}/{hubName}/{planId}", s.handleFinishJob)

	// Job events (legacy)
	s.route("PUT /_apis/v1/plans/{planId}/events", s.handleJobEvents)

	// CustomerIntelligence (telemetry, accept and discard)
	s.route("POST /_apis/v1/tasks", s.handleTelemetry)
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
	w.Write([]byte(job.Message))
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

	s.store.mu.Lock()
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
	s.store.mu.Unlock()

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

	s.store.mu.Lock()
	job.Status = "completed"
	if result != "" {
		job.Result = result
	}
	// Snapshot under the lock: job fields are concurrently written by the
	// broker (e.g. recordJobAgentLocked sets AgentID), so reads after the
	// unlock must use locals, not the shared *Job.
	jobIDSnap := job.ID
	jobResultSnap := job.Result
	s.store.mu.Unlock()

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

	// Update job status — try both plan ID lookup and job ID lookup
	job := s.lookupJobByPlanID(planID)
	if job == nil && jobID != "" {
		s.store.mu.RLock()
		job = s.store.Jobs[jobID]
		s.store.mu.RUnlock()
	}
	if job != nil {
		if jobID != "" && jobID != job.ID {
			http.Error(w, "JobCompleted event jobId does not match plan", http.StatusBadRequest)
			return
		}
		jobID = job.ID
		// Store the official runner's evaluated outputs before completing the
		// workflow job. Completion can synchronously dispatch a downstream
		// job whose needs context must already contain these values.
		s.captureResolvedJobOutputs(jobID, body.Outputs)

		s.store.mu.Lock()
		job.Status = "completed"
		job.Result = result
		// Snapshot under the lock: the broker concurrently mutates these
		// fields (recordJobAgentLocked writes AgentID), so every read after
		// the unlock must come from a local, not the shared *Job.
		jobIDSnap := job.ID
		jobResultSnap := job.Result
		jobAgentSnap := job.AgentID
		s.store.mu.Unlock()
		s.logger.Info().Str("jobId", jobIDSnap).Str("result", jobResultSnap).Msg("job status updated")

		// Notify workflow engine of job completion
		s.onJobCompleted(r.Context(), jobIDSnap, jobResultSnap)

		// Ephemeral runners exist for exactly one job — real GitHub
		// auto-deregisters them after it completes (the dispatcher's
		// one-runner-per-job model depends on the registration not
		// lingering as an offline zombie).
		s.removeEphemeralAgent(jobAgentSnap)
	} else {
		s.logger.Warn().Str("planId", planID).Msg("could not find job for finish")
	}

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

	s.store.mu.Lock()
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
		s.store.persistWorkflowRecord(workflow)
	}
	s.store.mu.Unlock()

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
		job := s.lookupJobByPlanID(planID)
		jobID := body.JobID
		if job != nil {
			if jobID != "" && jobID != job.ID {
				http.Error(w, "JobCompleted event jobId does not match plan", http.StatusBadRequest)
				return
			}
			jobID = job.ID
		}
		if jobID == "" {
			http.Error(w, "JobCompleted event is missing jobId", http.StatusBadRequest)
			return
		}

		s.captureResolvedJobOutputs(jobID, body.Outputs)
		result, err := runnerJobResult(body.Result)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if job != nil {
			s.store.mu.Lock()
			job.Status = "completed"
			job.Result = result
			s.store.mu.Unlock()
		}
		s.onJobCompleted(r.Context(), jobID, result)
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
