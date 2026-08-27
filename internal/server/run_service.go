package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerRunServiceRoutes() {
	// Acquire / renew / complete job requests, each bound to the runner the
	// broker handed that {requestId} to.
	s.route("GET /_apis/v1/AgentRequest/{poolId}/{requestId}", s.requireAssignedAgent(s.handleGetRequest))
	s.route("PATCH /_apis/v1/AgentRequest/{poolId}/{requestId}", s.requireAssignedAgent(s.handleRenewRequest))
	s.route("PUT /_apis/v1/AgentRequest/{poolId}/{requestId}", s.requireAssignedAgent(s.handleRenewRequest))
	s.route("DELETE /_apis/v1/AgentRequest/{poolId}/{requestId}", s.requireAssignedAgent(s.handleCompleteRequest))

	// FinishJob and the legacy plan-event route both act on {planId}, bound
	// to that job's runtime token.
	s.route("POST /_apis/v1/FinishJob/{scopeId}/{hubName}/{planId}", s.requirePlanJob(s.handleFinishJob))
	s.route("PUT /_apis/v1/plans/{planId}/events", s.requirePlanJob(s.handleJobEvents))

	// Telemetry names no job, so the gate is a verified runner credential of
	// either audience.
	s.route("POST /_apis/v1/tasks", s.requireRunnerAuth(s.handleTelemetry))
}

// requireAssignedAgent gates a {requestId} route on the agent session of the
// runner that job was dispatched to (recorded by the broker at dispatch), so
// one runner cannot read, renew, or complete another runner's job — whose
// message carries its secrets and runtime token.
func (s *Server) requireAssignedAgent(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAgentSession(func(w http.ResponseWriter, r *http.Request) {
		reqID, err := strconv.ParseInt(r.PathValue("requestId"), 10, 64)
		if err != nil {
			http.Error(w, "invalid request ID", http.StatusBadRequest)
			return
		}
		job := s.actions.LookupJobByRequestID(reqID)
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

// requirePlanJob gates a {planId} route on that job's runtime token: the
// plan's scopeIdentifier (from the dispatched job message) must match the
// verified sub claim. Binding to {planId} — the parameter the handlers act
// on — not {scopeId} stops a job from writing another job's plan.
func (s *Server) requirePlanJob(next http.HandlerFunc) http.HandlerFunc {
	return s.requireJobToken(func(w http.ResponseWriter, r *http.Request) {
		planID := r.PathValue("planId")
		job := s.actions.LookupJobByPlanID(planID)
		if job == nil {
			http.Error(w, "plan not found", http.StatusNotFound)
			return
		}
		// Read the scope recorded at dispatch, avoiding a re-parse of the
		// secret-bearing job message and surviving its clearing at run
		// finalization.
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

	job := s.actions.LookupJobByRequestID(reqID)
	if job == nil {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	s.logger.Debug().Int64("requestId", reqID).Msg("get request")

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

	job := s.actions.LookupJobByRequestID(reqID)
	if job == nil {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	// bleephub consumes no body fields here; drain explicitly.
	_, _ = io.Copy(io.Discard, r.Body)

	s.store.Mu.Lock()
	startedRunning := false
	if job.Status == "queued" {
		job.Status = "running"
		startedRunning = true
	}
	job.LockedUntil = time.Now().Add(1 * time.Hour)
	// Mirror the pickup onto the workflow job so the jobs API and checks
	// layer report in_progress from now.
	if startedRunning {
		for _, wf := range s.store.Workflows {
			if wfJob, ok := actions.FindWorkflowJobByID(wf, job.ID); ok {
				if wfJob.Status == store.JobStatusQueued {
					wfJob.Status = store.JobStatusRunning
					wfJob.StartedAt = time.Now()
					s.store.PersistWorkflowRecord(wf)
					s.actions.QueueEvent(actions.EvJobInProgress, wf, wfJob)
				}
				break
			}
		}
	}
	// Snapshot under the lock; the broker and completion concurrently mutate
	// these fields.
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

	job := s.actions.LookupJobByRequestID(reqID)
	if job == nil {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	s.store.Mu.Lock()
	s.store.MarkJobCompletedLocked(job)
	if result != "" {
		job.Result = result
	}
	// Snapshot under the lock; the broker concurrently writes job fields.
	jobIDSnap := job.ID
	jobResultSnap := job.Result
	s.store.Mu.Unlock()

	s.logger.Info().
		Int64("requestId", reqID).
		Str("job_id", jobIDSnap).
		Str("result", result).
		Msg("job request completed (DELETE)")

	s.actions.OnJobCompleted(r.Context(), jobIDSnap, jobResultSnap)

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleFinishJob(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("planId")

	// FinishJob is the official runner's JobServer RaisePlanEventAsync route;
	// its body is the JobCompletedEvent wire contract, including evaluated
	// job outputs.
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

	// The plan names the job; a body jobId may only confirm it, never select
	// a different one (that would finish another runner's work).
	job := s.actions.LookupJobByPlanID(planID)
	if job == nil {
		http.Error(w, "plan not found", http.StatusNotFound)
		return
	}
	if jobID != "" && jobID != job.ID {
		http.Error(w, "JobCompleted event jobId does not match plan", http.StatusBadRequest)
		return
	}

	// Capture outputs before completing: completion can synchronously
	// dispatch a downstream job whose needs context must already hold them.
	s.actions.CaptureResolvedJobOutputs(job.ID, body.Outputs)

	s.store.Mu.Lock()
	s.store.MarkJobCompletedLocked(job)
	job.Result = result
	// Snapshot under the lock; the broker concurrently mutates job fields.
	jobIDSnap := job.ID
	jobResultSnap := job.Result
	s.store.Mu.Unlock()
	s.logger.Info().Str("jobId", jobIDSnap).Str("result", jobResultSnap).Msg("job status updated")

	s.actions.OnJobCompleted(r.Context(), jobIDSnap, jobResultSnap)

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
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
		// As on FinishJob: the plan names the job; a body jobId only confirms it.
		job := s.actions.LookupJobByPlanID(planID)
		if job == nil {
			http.Error(w, "plan not found", http.StatusNotFound)
			return
		}
		if body.JobID != "" && body.JobID != job.ID {
			http.Error(w, "JobCompleted event jobId does not match plan", http.StatusBadRequest)
			return
		}

		s.actions.CaptureResolvedJobOutputs(job.ID, body.Outputs)
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
		s.actions.OnJobCompleted(r.Context(), jobIDSnap, result)
	}

	w.WriteHeader(http.StatusOK)
}

type runnerJobEvent struct {
	Name      string                                 `json:"name"`
	JobID     string                                 `json:"jobId"`
	RequestID int64                                  `json:"requestId"`
	Result    json.RawMessage                        `json:"result"`
	Outputs   map[string]actions.RunnerVariableValue `json:"outputs"`
}

// runnerJobResult maps the actions/runner TaskResult JSON value (numeric or
// string) onto Bleephub's workflow-engine completion strings.
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
