package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// WorkflowEventMeta carries event metadata set on the workflow before dispatch.
type WorkflowEventMeta struct {
	EventName string
	Ref       string
	Sha       string
	Repo      string
	Inputs    map[string]string
	// Attempt sets the run's 1-based run_attempt (0 = first attempt).
	Attempt int
	// ReuseRunID keeps the original run id/number across rerun attempts; real GitHub never mints a new run id for a re-run.
	ReuseRunID     int
	ReuseRunNumber int
	// WorkflowFileID / WorkflowFilePath pin the originating workflow file across reruns even when files share a display name.
	WorkflowFileID   int64
	WorkflowFilePath string
	// CarryOverJobs pre-completes jobs by key with results carried from the previous attempt (rerun-failed-jobs keeps successful jobs).
	CarryOverJobs map[string]*store.WorkflowJob
	// TypedInputs carries workflow_dispatch inputs with declared types for the `inputs` context; Inputs keeps the string github.event.inputs forms.
	TypedInputs map[string]interface{}
	// Payload is the triggering webhook event, exposed as github.event.
	Payload map[string]interface{}
}

// SubmitWorkflow creates a Workflow from a WorkflowDef and begins dispatching jobs.

func (s *Engine) SubmitWorkflow(ctx context.Context, serverURL string, wf *store.WorkflowDef, defaultImage string, eventMeta ...*WorkflowEventMeta) (*store.Workflow, error) {
	ctx, span := otel.Tracer("bleephub").Start(ctx, "submitWorkflow",
		trace.WithAttributes(attribute.String("workflow.name", wf.Name)))
	defer span.End()
	if err := ValidateJobGraph(wf); err != nil {
		return nil, err
	}

	// Expand reusable-workflow calls; the repository context resolves "./" refs.
	repoForCalls := ""
	if len(eventMeta) > 0 && eventMeta[0] != nil {
		repoForCalls = eventMeta[0].Repo
	}
	wf, err := s.expandReusableWorkflows(wf, repoForCalls, 1)
	if err != nil {
		return nil, err
	}

	var runID int
	if len(eventMeta) > 0 && eventMeta[0] != nil && eventMeta[0].ReuseRunID > 0 {
		runID = eventMeta[0].ReuseRunID
	} else {
		runID = s.store.ReserveRunID()
	}

	workflow := &store.Workflow{
		ID:                uuid.New().String(),
		Name:              wf.Name,
		DisplayTitle:      wf.Name,
		RunID:             runID,
		Jobs:              make(map[string]*store.WorkflowJob),
		Env:               wf.Env,
		Permissions:       wf.Permissions,
		Status:            store.WorkflowStatusRunning,
		CreatedAt:         time.Now(),
		MatrixMaxParallel: make(map[string]int),
	}

	if workflow.Name == "" {
		workflow.Name = "workflow"
	}
	if workflow.DisplayTitle == "" {
		workflow.DisplayTitle = workflow.Name
	}

	if wf.Concurrency != nil {
		workflow.ConcurrencyGroup = wf.Concurrency.Group
		workflow.CancelInProgress = wf.Concurrency.CancelInProgress
	}

	for key, jd := range wf.Jobs {
		wfJob := &store.WorkflowJob{
			Key:             key,
			JobID:           uuid.New().String(),
			DisplayName:     key,
			Needs:           jd.Needs,
			Status:          store.JobStatusPending,
			Outputs:         make(map[string]string),
			ContinueOnError: jd.ContinueOnError,
			Def:             jd,
			Hidden:          jd.ServerCompleted,
		}
		if jd.Name != "" {
			wfJob.DisplayName = jd.Name
		}

		if len(jd.MatrixValues) > 0 {
			wfJob.MatrixValues = make(map[string]interface{}, len(jd.MatrixValues))
			for matrixKey, matrixValue := range jd.MatrixValues {
				wfJob.MatrixValues[matrixKey] = matrixValue
			}
		}
		wfJob.MatrixGroup = jd.MatrixGroup
		if jd.MatrixGroup != "" && jd.MatrixMaxParallel > 0 {
			workflow.MatrixMaxParallel[jd.MatrixGroup] = jd.MatrixMaxParallel
		}

		workflow.Jobs[key] = wfJob
	}

	// Apply event metadata before any goroutine can observe the workflow.
	if len(eventMeta) > 0 && eventMeta[0] != nil {
		m := eventMeta[0]
		workflow.EventName = m.EventName
		workflow.Ref = m.Ref
		workflow.Sha = m.Sha
		workflow.RepoFullName = m.Repo
		workflow.Inputs = m.Inputs
		workflow.TypedInputs = m.TypedInputs
		workflow.EventPayload = m.Payload
		workflow.Attempt = m.Attempt
		workflow.WorkflowFileID = m.WorkflowFileID
		workflow.WorkflowFilePath = m.WorkflowFilePath

		// Carry results from the previous attempt (rerun-failed-jobs re-runs only failed jobs) before storing, so dispatch never sees a carried job as pending.
		for key, prev := range m.CarryOverJobs {
			wfJob, ok := workflow.Jobs[key]
			if !ok {
				continue
			}
			wfJob.Status = prev.Status
			wfJob.Result = prev.Result
			wfJob.StartedAt = prev.StartedAt
			wfJob.CompletedAt = prev.CompletedAt
			for k, v := range prev.Outputs {
				wfJob.Outputs[k] = v
			}
		}
	}

	if wf.RunName != "" {
		inputsCtx := make(map[string]interface{}, len(workflow.Inputs))
		for key, value := range workflow.Inputs {
			inputsCtx[key] = value
		}
		displayTitle, err := EvalTemplate(wf.RunName, &ExprContext{Contexts: map[string]interface{}{
			"github": s.GithubContextMap(workflow),
			"inputs": inputsCtx,
		}})
		if err != nil {
			return nil, fmt.Errorf("run-name: %w", err)
		}
		workflow.DisplayTitle = displayTitle
	}

	// Concurrency groups are template strings on real GitHub (`group: ci-${{ github.ref }}`); resolve them before grouping.
	if workflow.ConcurrencyGroup != "" && strings.Contains(workflow.ConcurrencyGroup, "${{") {
		inputsCtx := make(map[string]interface{}, len(workflow.Inputs))
		for k, v := range workflow.Inputs {
			inputsCtx[k] = v
		}
		group, err := EvalTemplate(workflow.ConcurrencyGroup, &ExprContext{Contexts: map[string]interface{}{
			"github": s.GithubContextMap(workflow),
			"inputs": inputsCtx,
		}})
		if err != nil {
			return nil, fmt.Errorf("concurrency.group: %w", err)
		}
		workflow.ConcurrencyGroup = group
	}

	// Resolve the originating workflow file for the run's stable id and path, constant across every run produced from the file.
	workflow.WorkflowFileID, workflow.WorkflowFilePath = s.ResolveWorkflowFileForRun(workflow)
	if len(eventMeta) > 0 && eventMeta[0] != nil && eventMeta[0].ReuseRunNumber > 0 {
		workflow.RunNumber = eventMeta[0].ReuseRunNumber
	} else {
		workflow.RunNumber = s.store.ReserveWorkflowRunNumber(workflow)
	}

	// Fork-PR gating: a fork pull_request run holds in action_required until a maintainer approves it, when the base repo requires contributor approval.
	if workflowNeedsForkPRApproval(workflow, s.store) {
		workflow.Status = store.WorkflowStatusActionRequired
		s.store.Mu.Lock()
		s.store.Workflows[workflow.ID] = workflow
		s.store.PersistWorkflowRecord(workflow)
		s.store.Mu.Unlock()
		s.QueueEvent(EvRunRequested, workflow, nil)
		return workflow, nil
	}

	// Concurrency admission and insertion are one critical section, else simultaneous submissions both observe an empty group
	// and start. GitHub retains only the newest pending run per group, so stale queued runs are cancelled as a new one arrives.
	// On a shared database the section runs under the group's database lock so two replicas cannot both admit (ACT-012).
	var cancelForConcurrency []*store.Workflow
	if workflow.ConcurrencyGroup != "" {
		releaseGroupLock := s.acquireConcurrencyAdmissionLock(ActionsConcurrencyLockName(workflow.RepoFullName, workflow.ConcurrencyGroup))
		s.workflowConcurrencyMu.Lock()
		s.store.Mu.Lock()
		var active bool
		for _, existing := range s.store.WorkflowConcurrencyPeersLocked(workflow.RepoFullName, workflow.ConcurrencyGroup) {
			switch existing.Status {
			case store.WorkflowStatusRunning, store.WorkflowStatusWaiting:
				active = true
				if workflow.CancelInProgress {
					cancelForConcurrency = append(cancelForConcurrency, existing)
				}
			case store.WorkflowStatusPendingConcurrency:
				// Retain only the newest pending run.
				cancelForConcurrency = append(cancelForConcurrency, existing)
			}
		}
		if active && !workflow.CancelInProgress {
			workflow.Status = store.WorkflowStatusPendingConcurrency
		} else {
			workflow.ConcurrencyAcquiredAt = time.Now().UTC()
		}
		s.store.Workflows[workflow.ID] = workflow
		s.store.PersistWorkflowRecord(workflow)
		s.store.Mu.Unlock()
		s.workflowConcurrencyMu.Unlock()
		releaseGroupLock()
	} else {
		s.store.Mu.Lock()
		s.store.Workflows[workflow.ID] = workflow
		s.store.PersistWorkflowRecord(workflow)
		s.store.Mu.Unlock()
	}
	s.QueueEvent(EvRunRequested, workflow, nil)
	for _, existing := range cancelForConcurrency {
		s.CancelWorkflow(existing)
	}
	if workflow.Status == store.WorkflowStatusPendingConcurrency {
		return workflow, nil
	}

	if s.metrics != nil {
		s.metrics.RecordWorkflowSubmit()
	}

	s.StartTimeoutWatcher(workflow)
	s.DispatchReadyJobs(ctx, workflow, serverURL, defaultImage)

	return workflow, nil
}

// ResolveWorkflowFileForRun returns the stable id and path of the [WorkflowFile] that produced this run. When no file is
// registered yet it derives a deterministic id from (repo, conventional-path) so workflow_id and path stay constant across reruns even before the file lands in git.
func (s *Engine) ResolveWorkflowFileForRun(wf *store.Workflow) (int64, string) {
	repo := wf.RepoFullName
	if repo != "" {
		s.store.DiscoverWorkflowFilesFromGit(repo)
		if wf.WorkflowFileID != 0 {
			if f := s.store.GetWorkflowFile(repo, wf.WorkflowFileID); f != nil {
				return f.ID, f.Path
			}
		}
		if wf.WorkflowFilePath != "" {
			for _, f := range s.store.ListWorkflowFiles(repo) {
				if f.Path == wf.WorkflowFilePath {
					return f.ID, f.Path
				}
			}
		}
		for _, f := range s.store.ListWorkflowFiles(repo) {
			if f.Name == wf.Name {
				return f.ID, f.Path
			}
		}
	}
	// No registered file: derive a stable id from the conventional path.
	path := ".github/workflows/" + wf.Name + ".yml"
	return store.StableWorkflowFileID(repo, path), path
}

// DispatchReadyJobs dispatches pending jobs whose dependencies are satisfied, looping until stable so skip cascades settle.
func (s *Engine) DispatchReadyJobs(ctx context.Context, wf *store.Workflow, serverURL string, defaultImage string) {
	ctx, span := otel.Tracer("bleephub").Start(ctx, "dispatchReadyJobs",
		trace.WithAttributes(attribute.String("workflow.id", wf.ID)))
	defer span.End()
	// Job-level concurrency admission must serialize across replicas on a shared database (ACT-012). The lock spans one pass,
	// released before the recursive dispatch of affected workflows below (which takes it itself).
	jobAdmissionNeedsLock := workflowHasJobConcurrency(wf)
	for {
		releaseJobAdmission := func() {}
		if jobAdmissionNeedsLock {
			releaseJobAdmission = s.acquireJobConcurrencyAdmissionLock(wf)
		}
		s.store.Mu.Lock()
		changed := false
		var toDispatch []*store.WorkflowJob
		var jobsToCancel []string
		affectedWorkflows := map[*store.Workflow]bool{}
		for _, wfJob := range wf.Jobs {
			if wfJob.Status != store.JobStatusPending {
				continue
			}

			allDepsOk := true
			anyDepFailed := false
			for _, dep := range wfJob.Needs {
				depJob, ok := wf.Jobs[dep]
				if !ok {
					allDepsOk = false
					break
				}
				if depJob.Status == store.JobStatusCompleted || depJob.Status == store.JobStatusSkipped {
					if depJob.Result != store.ResultSuccess && !depJob.ContinueOnError {
						anyDepFailed = true
					}
					continue
				}
				allDepsOk = false
				break
			}

			if !allDepsOk {
				continue
			}

			// A cancel-requested run only dispatches jobs explicitly gated on always()/cancelled(); everything else cancels.
			if wf.CancelRequested {
				gated := wfJob.Def != nil && ExprGatesOnCancellation(wfJob.Def.If)
				if !gated {
					wfJob.Status = store.JobStatusCompleted
					wfJob.Result = store.ResultCancelled
					wfJob.CompletedAt = time.Now()
					s.QueueEvent(EvJobCompleted, wf, wfJob)
					changed = true
					continue
				}
			}

			if wfJob.Def != nil && wfJob.Def.If != "" {
				// A job carries an implicit success(); naming any of success()/always()/cancelled()/failure() replaces it, so the
				// dependency roll-up below must not re-impose it. Checking only always()/failure() left an `if: cancelled()`
				// job skipped by the very cancellation that made its condition true.
				overridesStatusCheck := ExprContainsAnyStatusFunction(wfJob.Def.If)
				exprCtx, err := s.jobExprContext(wf, wfJob)
				if err != nil {
					wfJob.Status = store.JobStatusCompleted
					wfJob.Result = store.ResultFailure
					wfJob.CompletedAt = time.Now()
					s.logger.Warn().Err(err).Str("job", wfJob.Key).
						Msg("job if: context error — failing job")
					changed = true
					continue
				}

				ok, err := EvalExprErr(wfJob.Def.If, exprCtx)
				if err != nil {
					// GitHub fails the job on an invalid expression, not skip it.
					wfJob.Status = store.JobStatusCompleted
					wfJob.Result = store.ResultFailure
					wfJob.CompletedAt = time.Now()
					s.logger.Warn().Err(err).Str("job", wfJob.Key).Str("if", wfJob.Def.If).
						Msg("job if: expression error — failing job")
					changed = true
					continue
				}
				if !ok {
					wfJob.Status = store.JobStatusSkipped
					wfJob.Result = store.ResultSkipped
					s.logger.Info().Str("job", wfJob.Key).Str("if", wfJob.Def.If).Msg("skipping job (if: false)")
					s.QueueEvent(EvJobCompleted, wf, wfJob)
					changed = true
					continue
				}

				if overridesStatusCheck {
					anyDepFailed = false
				}
			}

			if anyDepFailed {
				wfJob.Status = store.JobStatusSkipped
				wfJob.Result = store.ResultSkipped
				s.logger.Info().Str("job", wfJob.Key).Msg("skipping job (dependency failed)")
				s.QueueEvent(EvJobCompleted, wf, wfJob)
				changed = true
				continue
			}

			// Synthetic reusable-workflow nodes complete in the engine (gates resolve call inputs, collectors map outputs) and never dispatch.
			if wfJob.Def != nil && wfJob.Def.ServerCompleted {
				s.completeServerJobLocked(wf, wfJob)
				changed = true
				continue
			}

			if wfJob.Def != nil && wfJob.Def.Concurrency != nil {
				if wfJob.ConcurrencyGroup == "" {
					group := wfJob.Def.Concurrency.Group
					if strings.Contains(group, "${{") {
						exprCtx, err := s.jobExprContext(wf, wfJob)
						if err == nil {
							group, err = EvalTemplate(group, exprCtx)
						}
						if err != nil {
							wfJob.Status = store.JobStatusCompleted
							wfJob.Result = store.ResultFailure
							wfJob.CompletedAt = s.currentTime()
							s.QueueEvent(EvJobCompleted, wf, wfJob)
							s.logger.Warn().Err(err).Str("job", wfJob.Key).
								Msg("job concurrency expression failed")
							changed = true
							continue
						}
					}
					wfJob.ConcurrencyGroup = strings.ToLower(strings.TrimSpace(group))
					wfJob.CancelInProgress = wfJob.Def.Concurrency.CancelInProgress
					changed = true
					if wfJob.ConcurrencyGroup == "" {
						wfJob.Status = store.JobStatusCompleted
						wfJob.Result = store.ResultFailure
						wfJob.CompletedAt = s.currentTime()
						s.QueueEvent(EvJobCompleted, wf, wfJob)
						changed = true
						continue
					}
					// Index now so a sibling in the same group sees this one within the same lock pass.
					s.store.SyncJobConcurrencyEntryLocked(wf, wfJob)
				}
				blocked := false
				for _, peer := range s.store.JobConcurrencyPeersLocked(wf.RepoFullName, wfJob.ConcurrencyGroup) {
					other, otherWorkflow := peer.Job, peer.Wf
					if other == wfJob {
						continue
					}
					if other.Status == store.JobStatusPending {
						currentIsNewer := wf.CreatedAt.After(otherWorkflow.CreatedAt) ||
							(wf.CreatedAt.Equal(otherWorkflow.CreatedAt) && wf.ID > otherWorkflow.ID)
						if !currentIsNewer {
							blocked = true
							continue
						}
						other.Status = store.JobStatusCompleted
						other.Result = store.ResultCancelled
						other.CompletedAt = s.currentTime()
						s.QueueEvent(EvJobCompleted, otherWorkflow, other)
						affectedWorkflows[otherWorkflow] = true
						s.store.PersistWorkflowRecord(otherWorkflow)
						changed = true
						continue
					}
					if other.Status != store.JobStatusQueued && other.Status != store.JobStatusRunning {
						continue
					}
					if !wfJob.CancelInProgress {
						blocked = true
						continue
					}
					other.Status = store.JobStatusCompleted
					other.Result = store.ResultCancelled
					other.CompletedAt = s.currentTime()
					s.QueueEvent(EvJobCompleted, otherWorkflow, other)
					jobsToCancel = append(jobsToCancel, other.JobID)
					affectedWorkflows[otherWorkflow] = true
					s.store.PersistWorkflowRecord(otherWorkflow)
					changed = true
				}
				if blocked {
					continue
				}
				if wfJob.CancelInProgress {
					cancelled := make(map[string]bool, len(jobsToCancel))
					for _, id := range jobsToCancel {
						cancelled[id] = true
					}
					filtered := s.store.PendingMessages[:0:0]
					for _, message := range s.store.PendingMessages {
						if !cancelled[message.JobID] {
							filtered = append(filtered, message)
						}
					}
					s.store.PendingMessages = filtered
				}
			}

			// Enforce max-parallel over the job's matrix group.
			maxParallel := wf.MaxParallel
			if wfJob.MatrixGroup != "" && wf.MatrixMaxParallel[wfJob.MatrixGroup] > 0 {
				maxParallel = wf.MatrixMaxParallel[wfJob.MatrixGroup]
			}
			if maxParallel > 0 {
				active := 0
				for _, j := range wf.Jobs {
					if j.Key == wfJob.Key {
						continue
					}
					if (j.Status == store.JobStatusQueued || j.Status == store.JobStatusRunning) && j.MatrixGroup == wfJob.MatrixGroup && wfJob.MatrixGroup != "" {
						active++
					}
				}
				if active >= maxParallel {
					continue // retries when a job completes
				}
			}

			// A job targeting an environment whose deployment branch policy forbids
			// this ref is blocked — it must not run (and must not receive the
			// environment's secrets/protection context). GitHub fails such a job.
			if envName := jobEnvironmentName(wfJob); envName != "" {
				if repo := s.store.ReposByName[wf.RepoFullName]; repo != nil {
					if env := s.store.Deployments.GetEnvironment(repo.ID, envName); env != nil && !s.environmentAllowsRefLocked(repo.ID, wf, env) {
						wfJob.Status = store.JobStatusCompleted
						wfJob.Result = store.ResultFailure
						wfJob.CompletedAt = time.Now()
						s.logger.Info().Str("job", wfJob.Key).Str("environment", envName).Str("ref", wf.Ref).
							Msg("branch not allowed to deploy to environment")
						s.QueueEvent(EvJobCompleted, wf, wfJob)
						changed = true
						continue
					}
				}
			}

			// A job targeting an environment with required reviewers waits for a deployment review.
			if envName := jobEnvironmentName(wfJob); envName != "" && !envApproved(wf, envName) {
				if env := s.protectedEnvironmentLocked(wf, envName); env != nil {
					wfJob.Status = store.JobStatusWaiting
					addPendingDeployment(wf, env)
					if wf.Status == store.WorkflowStatusRunning {
						wf.Status = store.WorkflowStatusWaiting
					}
					s.logger.Info().Str("job", wfJob.Key).Str("environment", envName).
						Msg("job waiting for deployment review")
					s.QueueEvent(EvJobWaiting, wf, wfJob)
					changed = true
					continue
				}
			}

			// Mark queued now so max-parallel checks this pass count it.
			wfJob.Status = store.JobStatusQueued
			wfJob.QueuedAt = time.Now()
			toDispatch = append(toDispatch, wfJob)
			s.QueueEvent(EvJobQueued, wf, wfJob)
			changed = true
		}
		if changed {
			s.store.PersistWorkflowRecord(wf)
		}
		s.store.Mu.Unlock()
		releaseJobAdmission()

		for _, jobID := range jobsToCancel {
			s.SendJobCancellation(jobID)
		}
		for affected := range affectedWorkflows {
			if affected == wf {
				continue
			}
			serverURL, defaultImage := workflowDispatchCoordinates(affected)
			s.DispatchReadyJobs(ctx, affected, serverURL, defaultImage)
		}

		// Dispatch outside the lock; dispatchWorkflowJob takes its own locks.
		for _, wfJob := range toDispatch {
			s.dispatchWorkflowJob(ctx, wf, wfJob, serverURL, defaultImage)
		}

		if !changed {
			break
		}
	}

	// A run can reach all-done here with no runner completion event (a server-completed collector as the final node); finalize is idempotent.
	s.FinalizeWorkflowIfDone(wf)
}

func workflowDispatchCoordinates(wf *store.Workflow) (serverURL, defaultImage string) {
	if wf != nil && wf.Env != nil {
		serverURL = wf.Env["__serverURL"]
		defaultImage = wf.Env["__defaultImage"]
	}
	return serverURL, defaultImage
}

// dispatchWorkflowJob builds and sends a job message to the runner.
func (s *Engine) dispatchWorkflowJob(ctx context.Context, wf *store.Workflow, wfJob *store.WorkflowJob, serverURL, defaultImage string) {
	_, span := otel.Tracer("bleephub").Start(ctx, "dispatchWorkflowJob",
		trace.WithAttributes(
			attribute.String("workflow.id", wf.ID),
			attribute.String("job.key", wfJob.Key)))
	defer span.End()
	planID := uuid.New().String()
	timelineID := uuid.New().String()
	requestID := s.NextRequestID()

	msg, err := s.BuildJobMessageFromDef(serverURL, wf, wfJob, planID, timelineID, requestID, defaultImage)
	if err != nil {
		s.store.Mu.Lock()
		wfJob.Status = store.JobStatusCompleted
		wfJob.Result = store.ResultFailure
		wfJob.CompletedAt = time.Now()
		s.store.Mu.Unlock()
		s.QueueEvent(EvJobCompleted, wf, wfJob)
		s.logger.Error().Err(err).Str("job", wfJob.Key).Msg("failed to build job message")
		s.FinalizeWorkflowIfDone(wf)
		return
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		s.logger.Error().Err(err).Str("job", wfJob.Key).Msg("failed to marshal job message")
		return
	}

	job := &store.Job{
		ID:          wfJob.JobID,
		RequestID:   requestID,
		PlanID:      planID,
		TimelineID:  timelineID,
		Status:      "queued",
		Message:     string(msgJSON),
		LockedUntil: time.Now().Add(jobLeaseDuration),
	}

	s.store.Mu.Lock()
	s.store.Jobs[wfJob.JobID] = job
	s.store.RegisterDispatchedJobLocked(job, msg, wf.RepoFullName)
	s.store.RegisterJobLogMasksLocked(planID, msg)
	wfJob.PlanID = planID
	s.store.PersistWorkflowRecord(wf)
	s.store.Mu.Unlock()

	envelope := &store.TaskAgentMessage{
		MessageID:   s.NextMessageID(),
		MessageType: "PipelineAgentJobRequest",
		Body:        string(msgJSON),
		Labels:      wfJob.Def.RunsOnLabels(),
		JobID:       wfJob.JobID,
	}

	s.QueueJobMessage(envelope)

	if s.metrics != nil {
		s.metrics.RecordJobDispatch()
	}

	s.logger.Info().
		Str("workflow", wf.ID).
		Str("job", wfJob.Key).
		Str("jobId", wfJob.JobID).
		Msg("workflow job dispatched")
}

// OnJobCompleted records a finished job and dispatches newly-ready dependents.
func (s *Engine) OnJobCompleted(ctx context.Context, jobID, result string) {
	ctx, span := otel.Tracer("bleephub").Start(ctx, "onJobCompleted",
		trace.WithAttributes(
			attribute.String("job.id", jobID),
			attribute.String("job.result", result)))
	defer span.End()

	s.store.Mu.Lock()
	var foundWf *store.Workflow
	var foundJob *store.WorkflowJob
	for _, wf := range s.store.Workflows {
		for _, wfJob := range wf.Jobs {
			if wfJob.JobID == jobID {
				foundWf = wf
				foundJob = wfJob
				break
			}
		}
		if foundWf != nil {
			break
		}
	}

	if foundWf == nil {
		s.store.Mu.Unlock()
		return // Not a workflow job
	}

	// The official runner reports completion twice (DELETE AgentRequest and POST FinishJob), both landing here.
	// Only the first terminal transition may finalize and emit; ignore the rest.
	if foundJob.Status == store.JobStatusCompleted || foundJob.Status == store.JobStatusSkipped {
		s.store.Mu.Unlock()
		return
	}

	foundJob.Status = store.JobStatusCompleted
	foundJob.Result = store.Result(NormalizeResult(result))
	foundJob.CompletedAt = time.Now()
	s.QueueEvent(EvJobCompleted, foundWf, foundJob)

	// Matrix fail-fast cancels siblings on a job failure. A continue-on-error job is excluded: the roll-up excludes its
	// tolerated failure but counts a cancellation, so cancelling siblings would turn the run red anyway.
	var cancelRunningSiblings []string
	if foundJob.Result == store.ResultFailure && foundJob.MatrixGroup != "" && !foundJob.ContinueOnError {
		if foundJob.Def.FailFast() {
			for _, sibling := range foundWf.Jobs {
				if sibling.Key == foundJob.Key {
					continue
				}
				if sibling.MatrixGroup != foundJob.MatrixGroup {
					continue
				}
				switch sibling.Status {
				case store.JobStatusPending, store.JobStatusQueued, store.JobStatusWaiting:
					sibling.Status = store.JobStatusCompleted
					sibling.Result = store.ResultCancelled
					sibling.CompletedAt = time.Now()
					s.QueueEvent(EvJobCompleted, foundWf, sibling)
					s.logger.Info().
						Str("job", sibling.Key).
						Str("reason", "fail-fast").
						Msg("cancelling matrix sibling")
				case store.JobStatusRunning:
					// In-progress on a runner: signal the runner to cancel (it reports
					// the cancelled completion). GitHub fail-fast cancels in-progress
					// siblings too, not just queued/pending ones.
					cancelRunningSiblings = append(cancelRunningSiblings, sibling.JobID)
					s.logger.Info().
						Str("job", sibling.Key).
						Str("reason", "fail-fast").
						Msg("cancelling in-progress matrix sibling")
				}
			}
		}
	}
	s.store.PersistWorkflowRecord(foundWf)
	s.store.Mu.Unlock()

	// Deliver runner cancellations outside the store lock (SendJobCancellation
	// takes its own locks), mirroring the normal cancel path.
	for _, jobID := range cancelRunningSiblings {
		s.SendJobCancellation(jobID)
	}

	if s.metrics != nil {
		s.metrics.RecordJobCompletion(foundJob)
	}

	s.logger.Info().
		Str("workflow_id", foundWf.ID).
		Str("workflow_name", foundWf.Name).
		Str("job_key", foundJob.Key).
		Str("job_id", foundJob.JobID).
		Str("result", string(foundJob.Result)).
		Msg("workflow job completed")

	// Dispatch newly-ready jobs (may also mark some skipped).
	if foundWf.Env != nil {
		if serverURL, ok := foundWf.Env["__serverURL"]; ok {
			defaultImage := foundWf.Env["__defaultImage"]
			s.DispatchReadyJobs(ctx, foundWf, serverURL, defaultImage)
		}
	}
	if foundJob.ConcurrencyGroup != "" {
		s.startPendingJobConcurrency(ctx, foundWf.RepoFullName, foundJob.ConcurrencyGroup)
	}

	s.store.Mu.Lock()
	allDone, anyFailed := workflowRollupLocked(foundWf)

	// DispatchReadyJobs may already have finalized the run; don't double-complete.
	if foundWf.Status == store.WorkflowStatusCompleted {
		allDone = false
	}

	if allDone {
		foundWf.Status = store.WorkflowStatusCompleted
		switch {
		case foundWf.CancelRequested:
			foundWf.Result = store.ResultCancelled
		case anyFailed:
			foundWf.Result = store.ResultFailure
		default:
			foundWf.Result = store.ResultSuccess
		}
		// Run over: drop its secret-bearing job messages. Late runner calls authenticate through planScopes.
		s.store.ClearRunJobMessagesLocked(foundWf)
	}
	concurrencyGroup := foundWf.ConcurrencyGroup
	s.store.PersistWorkflowRecord(foundWf)
	s.store.Mu.Unlock()

	if allDone {
		if s.metrics != nil {
			s.metrics.RecordWorkflowComplete()
		}
		s.StopTimeoutWatcher(foundWf)
		s.QueueEvent(EvRunCompleted, foundWf, nil)
		duration := time.Since(foundWf.CreatedAt)
		s.logger.Info().
			Str("workflow_id", foundWf.ID).
			Str("workflow_name", foundWf.Name).
			Str("result", string(foundWf.Result)).
			Int64("duration_ms", duration.Milliseconds()).
			Msg("workflow completed")

		if concurrencyGroup != "" {
			s.startPendingConcurrencyWorkflow(foundWf.RepoFullName, concurrencyGroup)
		}
	}
}

func (s *Engine) startPendingJobConcurrency(ctx context.Context, repoFullName, group string) {
	s.store.Mu.Lock()
	pending := make([]*store.Workflow, 0)
	seen := map[*store.Workflow]bool{}
	for _, peer := range s.store.JobConcurrencyPeersLocked(repoFullName, group) {
		if peer.Job.Status == store.JobStatusPending && !seen[peer.Wf] {
			seen[peer.Wf] = true
			pending = append(pending, peer.Wf)
		}
	}
	s.store.Mu.Unlock()
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].ID > pending[j].ID
		}
		return pending[i].CreatedAt.After(pending[j].CreatedAt)
	})
	for _, workflow := range pending {
		serverURL, defaultImage := workflowDispatchCoordinates(workflow)
		s.DispatchReadyJobs(ctx, workflow, serverURL, defaultImage)
	}
}

// jobEnvironmentName resolves a job's target environment, tolerating a nil Def.
func jobEnvironmentName(wfJob *store.WorkflowJob) string {
	if wfJob.Def == nil {
		return ""
	}
	return wfJob.Def.EnvironmentName()
}

// envApproved reports whether an approved review covers the environment.
func envApproved(wf *store.Workflow, envName string) bool {
	for _, a := range wf.EnvApprovals {
		if a.State != "approved" {
			continue
		}
		for _, name := range a.EnvNames {
			if name == envName {
				return true
			}
		}
	}
	return false
}

// environmentAllowsRefLocked reports whether a job running on wf's ref may
// deploy to env under its deployment branch policy. A nil policy allows any ref;
// "protected_branches" allows only a protected branch; "custom_branch_policies"
// allows only refs matching the environment's branch/tag patterns. Caller holds
// s.store.Mu (reads env-branch-policy and branch-protection maps directly / via
// the separate Misc mutex, so it never re-locks the store).
func (s *Engine) environmentAllowsRefLocked(repoID int, wf *store.Workflow, env *store.Environment) bool {
	policy := env.DeploymentBranchPolicy
	if policy == nil {
		return true
	}
	ref := wf.Ref
	isTag := strings.HasPrefix(ref, "refs/tags/")
	name := ref
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		name = strings.TrimPrefix(ref, "refs/heads/")
	case isTag:
		name = strings.TrimPrefix(ref, "refs/tags/")
	}
	switch {
	case policy.ProtectedBranches:
		// Only a protected branch may deploy; a tag never qualifies.
		return !isTag && s.store.GetBranchProtection(repoID, name) != nil
	case policy.CustomBranchPolicies:
		for _, p := range s.store.EnvBranchPolicies[env.ID] {
			if (p.Type == store.DeploymentBranchPolicyType("tag")) != isTag {
				continue
			}
			if store.MatchBranchPattern(p.Name, name) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// protectedEnvironmentLocked returns the environment when it carries required reviewers; referencing one auto-creates it. Caller holds s.store.mu.
func (s *Engine) protectedEnvironmentLocked(wf *store.Workflow, envName string) *store.Environment {
	if wf.RepoFullName == "" {
		return nil
	}
	repo := s.store.ReposByName[wf.RepoFullName]
	if repo == nil {
		return nil
	}
	env := s.store.Deployments.GetEnvironment(repo.ID, envName)
	if env == nil {
		env = s.store.Deployments.UpsertEnvironment(repo.ID, envName)
	}
	if len(env.Reviewers) == 0 {
		return nil
	}
	return env
}

// addPendingDeployment records the run's wait on an environment once.
func addPendingDeployment(wf *store.Workflow, env *store.Environment) {
	for _, p := range wf.PendingDeployments {
		if p.EnvID == env.ID {
			return
		}
	}
	wf.PendingDeployments = append(wf.PendingDeployments, &store.PendingDeployment{
		EnvID:              env.ID,
		EnvName:            env.Name,
		WaitTimerStartedAt: time.Now().UTC(),
	})
}

// ApplyDeploymentReview resolves a review against pending deployments: approved environments release their waiting jobs;
// rejected ones fail them. Returns the environment names the review covered.
func (s *Engine) ApplyDeploymentReview(ctx context.Context, wf *store.Workflow, envIDs []int, state, comment string, reviewer *store.User) []string {
	s.store.Mu.Lock()
	covered := map[string]bool{}
	var names []string
	remaining := wf.PendingDeployments[:0]
	for _, p := range wf.PendingDeployments {
		matched := false
		for _, id := range envIDs {
			if p.EnvID == id {
				matched = true
				break
			}
		}
		if matched {
			covered[p.EnvName] = true
			names = append(names, p.EnvName)
		} else {
			remaining = append(remaining, p)
		}
	}
	wf.PendingDeployments = remaining

	reviewerID := 0
	if reviewer != nil {
		reviewerID = reviewer.ID
	}
	wf.EnvApprovals = append(wf.EnvApprovals, &store.EnvApproval{
		State:     state,
		Comment:   comment,
		UserID:    reviewerID,
		EnvIDs:    append([]int(nil), envIDs...),
		EnvNames:  append([]string(nil), names...),
		CreatedAt: time.Now().UTC(),
	})

	for _, wfJob := range wf.Jobs {
		if wfJob.Status != store.JobStatusWaiting || !covered[jobEnvironmentName(wfJob)] {
			continue
		}
		if state == "approved" {
			wfJob.Status = store.JobStatusPending
		} else {
			wfJob.Status = store.JobStatusCompleted
			wfJob.Result = store.ResultFailure
			wfJob.CompletedAt = time.Now()
			s.QueueEvent(EvJobCompleted, wf, wfJob)
		}
	}
	if len(wf.PendingDeployments) == 0 && wf.Status == store.WorkflowStatusWaiting {
		wf.Status = store.WorkflowStatusRunning
	}
	serverURL := ""
	defaultImage := ""
	if wf.Env != nil {
		serverURL = wf.Env["__serverURL"]
		defaultImage = wf.Env["__defaultImage"]
	}
	s.store.PersistWorkflowRecord(wf)
	s.store.Mu.Unlock()

	if state == "approved" && serverURL != "" {
		s.DispatchReadyJobs(ctx, wf, serverURL, defaultImage)
	}
	s.FinalizeWorkflowIfDone(wf)
	return names
}

// FinalizeWorkflowIfDone completes the run once every job is terminal — needed independently of OnJobCompleted
// when a rejection fails jobs without any job completion event.
func (s *Engine) FinalizeWorkflowIfDone(wf *store.Workflow) {
	s.store.Mu.Lock()
	allDone, anyFailed := workflowRollupLocked(wf)
	if allDone && wf.Status != store.WorkflowStatusCompleted {
		wf.Status = store.WorkflowStatusCompleted
		switch {
		case wf.CancelRequested:
			wf.Result = store.ResultCancelled
		case anyFailed:
			wf.Result = store.ResultFailure
		default:
			wf.Result = store.ResultSuccess
		}
		// See OnJobCompleted: drop the finalized run's secret-bearing messages.
		s.store.ClearRunJobMessagesLocked(wf)
	} else {
		allDone = false
	}
	concurrencyGroup := wf.ConcurrencyGroup
	s.store.PersistWorkflowRecord(wf)
	s.store.Mu.Unlock()

	if allDone {
		if s.metrics != nil {
			s.metrics.RecordWorkflowComplete()
		}
		s.StopTimeoutWatcher(wf)
		s.QueueEvent(EvRunCompleted, wf, nil)
		if concurrencyGroup != "" {
			s.startPendingConcurrencyWorkflow(wf.RepoFullName, concurrencyGroup)
		}
	}
}

// CancelWorkflow requests cancellation: pending/queued jobs cancel immediately (their undelivered messages purged),
// running jobs get a JobCancellation broker message, and jobs gated on always()/cancelled() still dispatch. The run finalizes once nothing remains in flight.
func (s *Engine) CancelWorkflow(wf *store.Workflow) {
	s.store.Mu.Lock()
	wf.CancelRequested = true

	cancelledJobIDs := map[string]bool{}
	var runningJobIDs []string
	for _, wfJob := range wf.Jobs {
		switch wfJob.Status {
		case store.JobStatusPending, store.JobStatusWaiting:
			// Jobs gated on always()/cancelled() still run after a cancel; leave them pending for dispatch to evaluate with cancelled()==true.
			if wfJob.Def != nil && ExprGatesOnCancellation(wfJob.Def.If) {
				wfJob.Status = store.JobStatusPending
				continue
			}
			wfJob.Status = store.JobStatusCompleted
			wfJob.Result = store.ResultCancelled
			wfJob.CompletedAt = time.Now()
			cancelledJobIDs[wfJob.JobID] = true
			s.QueueEvent(EvJobCompleted, wf, wfJob)
		case store.JobStatusQueued, store.JobStatusRunning:
			// Delivered to a runner: signal it. Undelivered queued messages are purged below and the job cancels immediately.
			if job := s.store.Jobs[wfJob.JobID]; job != nil && job.AgentID != 0 && job.Status != "completed" {
				runningJobIDs = append(runningJobIDs, wfJob.JobID)
			} else {
				wfJob.Status = store.JobStatusCompleted
				wfJob.Result = store.ResultCancelled
				wfJob.CompletedAt = time.Now()
				cancelledJobIDs[wfJob.JobID] = true
				s.QueueEvent(EvJobCompleted, wf, wfJob)
			}
		}
	}

	// Drop undelivered job messages so no runner pulls a cancelled job later.
	if len(cancelledJobIDs) > 0 {
		kept := s.store.PendingMessages[:0:0]
		for _, msg := range s.store.PendingMessages {
			if !cancelledJobIDs[msg.JobID] {
				kept = append(kept, msg)
			}
		}
		s.store.PendingMessages = kept
	}

	serverURL := ""
	defaultImage := ""
	if wf.Env != nil {
		serverURL = wf.Env["__serverURL"]
		defaultImage = wf.Env["__defaultImage"]
	}
	s.store.Mu.Unlock()

	// JobCancellation rides the runner's open mid-job poll; job requests are pull-only, but cancellations are what the open poll exists for.
	for _, jobID := range runningJobIDs {
		s.SendJobCancellation(jobID)
	}

	s.logger.Info().
		Str("workflow_id", wf.ID).
		Str("workflow_name", wf.Name).
		Int("signalled_running", len(runningJobIDs)).
		Msg("workflow cancellation requested")

	// Dispatch settled always()/cancelled() jobs, then finalize if idle.
	if serverURL != "" {
		s.DispatchReadyJobs(context.Background(), wf, serverURL, defaultImage)
	} else {
		s.FinalizeWorkflowIfDone(wf)
	}
}

// startPendingConcurrencyWorkflow promotes the next pending run in the group. On a shared database the promotion
// runs under the group's database lock so a peer cannot double-admit (ACT-012).
func (s *Engine) startPendingConcurrencyWorkflow(repoFullName, group string) {
	// Release the lock once the promotion is committed, before stale-run cancellation, which can recurse here for the same group.
	releaseGroupLock := s.acquireConcurrencyAdmissionLock(ActionsConcurrencyLockName(repoFullName, group))
	s.workflowConcurrencyMu.Lock()
	s.store.Mu.Lock()
	var pendingWf *store.Workflow
	var stale []*store.Workflow
	for _, wf := range s.store.WorkflowConcurrencyPeersLocked(repoFullName, group) {
		if wf.Status == store.WorkflowStatusRunning || wf.Status == store.WorkflowStatusWaiting {
			s.store.Mu.Unlock()
			s.workflowConcurrencyMu.Unlock()
			releaseGroupLock()
			return
		}
		if wf.Status == store.WorkflowStatusPendingConcurrency {
			if pendingWf == nil || wf.CreatedAt.After(pendingWf.CreatedAt) {
				if pendingWf != nil {
					stale = append(stale, pendingWf)
				}
				pendingWf = wf
			} else {
				stale = append(stale, wf)
			}
		}
	}

	if pendingWf == nil {
		s.store.Mu.Unlock()
		s.workflowConcurrencyMu.Unlock()
		releaseGroupLock()
		return
	}

	pendingWf.Status = store.WorkflowStatusRunning
	pendingWf.ConcurrencyAcquiredAt = time.Now().UTC()
	s.store.PersistWorkflowRecord(pendingWf)
	s.store.Mu.Unlock()
	s.workflowConcurrencyMu.Unlock()
	releaseGroupLock()

	for _, wf := range stale {
		s.CancelWorkflow(wf)
	}
	if s.metrics != nil {
		s.metrics.RecordWorkflowSubmit()
	}
	s.StartTimeoutWatcher(pendingWf)

	if pendingWf.Env != nil {
		if serverURL, ok := pendingWf.Env["__serverURL"]; ok {
			defaultImage := pendingWf.Env["__defaultImage"]
			s.DispatchReadyJobs(context.Background(), pendingWf, serverURL, defaultImage)
		}
	}
}

// workflowNeedsForkPRApproval reports whether the run must hold in action_required for maintainer approval:
// a fork pull_request run whose base repo sets require_approval_for_fork_pr_workflows.
func workflowNeedsForkPRApproval(wf *store.Workflow, st *store.Store) bool {
	if wf.EventName != "pull_request" || wf.RepoFullName == "" || wf.EventPayload == nil {
		return false
	}
	if !pullRequestIsFromFork(wf.EventPayload, wf.RepoFullName) {
		return false
	}
	settings := st.GetRepoActionsPermissions(wf.RepoFullName).ForkPRWorkflowsPrivateRepos
	return settings != nil && settings.RequireApprovalForForkPRWorkflows
}

// NormalizeResult maps runner result strings to the canonical forms.
func NormalizeResult(result string) string {
	switch result {
	case "Succeeded", "succeeded":
		return "success"
	case "Failed", "failed":
		return "failure"
	// The runner's TaskResult uses the US spelling ("Canceled").
	case "Cancelled", "cancelled", "Canceled", "canceled":
		return "cancelled"
	default:
		if result == "" {
			return "failure"
		}
		return result
	}
}

// StartTimeoutWatcher polls for timed-out jobs in a background goroutine.
func (s *Engine) StartTimeoutWatcher(wf *store.Workflow) {
	ctx, cancel := context.WithCancel(context.Background())
	s.workflowTimeoutMu.Lock()
	previous := wf.CancelTimeout
	wf.CancelTimeout = cancel
	s.workflowTimeoutMu.Unlock()
	if previous != nil {
		previous()
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.CheckJobTimeouts(wf)
			}
		}
	}()
}

func (s *Engine) StopTimeoutWatcher(wf *store.Workflow) {
	s.workflowTimeoutMu.Lock()
	cancel := wf.CancelTimeout
	wf.CancelTimeout = nil
	s.workflowTimeoutMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// jobLeaseDuration bounds how long a dispatched job stays leased; the runner renews it while working, and a lapsed lease means the runner is gone.
const jobLeaseDuration = 1 * time.Hour

// reclaimExpiredJobLeases requeues jobs whose runner stopped renewing so a mid-job runner death no longer hangs the run
// until the 6-hour job timeout. A job still on the pending queue is untaken, so its lease is merely extended. Callers must not hold the store lock.
func (s *Engine) reclaimExpiredJobLeases(wf *store.Workflow) {
	now := s.currentTime()
	var redeliver []*store.TaskAgentMessage
	var reclaimed []string

	s.store.Mu.Lock()
	queued := make(map[string]bool, len(s.store.PendingMessages))
	for _, msg := range s.store.PendingMessages {
		if msg.JobID != "" {
			queued[msg.JobID] = true
		}
	}
	for _, wfJob := range wf.Jobs {
		if wfJob.Status != store.JobStatusQueued && wfJob.Status != store.JobStatusRunning {
			continue
		}
		job := s.store.Jobs[wfJob.JobID]
		if job == nil || job.Status == "completed" || job.LockedUntil.IsZero() || job.LockedUntil.After(now) {
			continue
		}
		job.LockedUntil = now.Add(jobLeaseDuration)
		if queued[wfJob.JobID] {
			continue
		}
		job.Status = "queued"
		// Free the gone runner's agent record so a reconnecting non-ephemeral runner can take work again.
		s.store.ClearAgentAssignmentLocked(job)
		job.AgentID = 0
		wfJob.Status = store.JobStatusQueued
		reclaimed = append(reclaimed, wfJob.Key)
		redeliver = append(redeliver, &store.TaskAgentMessage{
			MessageType: "PipelineAgentJobRequest",
			Body:        job.Message,
			Labels:      wfJob.Def.RunsOnLabels(),
			JobID:       wfJob.JobID,
		})
	}
	if len(reclaimed) > 0 {
		s.store.PersistWorkflowRecord(wf)
	}
	s.store.Mu.Unlock()

	for i, msg := range redeliver {
		msg.MessageID = s.NextMessageID()
		s.QueueJobMessage(msg)
		s.logger.Warn().
			Str("workflow_id", wf.ID).
			Str("job_key", reclaimed[i]).
			Str("jobId", msg.JobID).
			Msg("job lease expired — requeued for another runner")
	}
}

// CheckJobTimeouts cancels timed-out jobs and reclaims lapsed leases.
func (s *Engine) CheckJobTimeouts(wf *store.Workflow) {
	s.reclaimExpiredJobLeases(wf)

	s.store.Mu.Lock()
	if wf.Status == store.WorkflowStatusCompleted {
		s.store.Mu.Unlock()
		return
	}
	now := s.currentTime()
	var timedOut bool
	var timedOutJobIDs []string
	for _, wfJob := range wf.Jobs {
		if wfJob.Status != store.JobStatusRunning {
			continue
		}
		if wfJob.StartedAt.IsZero() {
			continue
		}
		timeout := 360 // default 6 hours
		if wfJob.Def != nil && wfJob.Def.TimeoutMinutes > 0 {
			timeout = wfJob.Def.TimeoutMinutes
		}
		if now.Sub(wfJob.StartedAt) > time.Duration(timeout)*time.Minute {
			s.logger.Warn().
				Str("workflow_id", wf.ID).
				Str("job_key", wfJob.Key).
				Int("timeout_minutes", timeout).
				Msg("job timed out, marking failed")
			wfJob.Status = store.JobStatusCompleted
			wfJob.Result = store.ResultFailure
			wfJob.CompletedAt = now
			s.QueueEvent(EvJobCompleted, wf, wfJob)
			timedOutJobIDs = append(timedOutJobIDs, wfJob.JobID)
			timedOut = true
		}
	}
	s.store.Mu.Unlock()

	// A timed-out job may still be executing on its runner — signal it.
	for _, jobID := range timedOutJobIDs {
		s.SendJobCancellation(jobID)
	}

	if timedOut {
		s.store.Mu.Lock()
		s.store.PersistWorkflowRecord(wf)
		s.store.Mu.Unlock()
		if wf.Env != nil {
			if serverURL, ok := wf.Env["__serverURL"]; ok {
				s.DispatchReadyJobs(context.Background(), wf, serverURL, wf.Env["__defaultImage"])
			}
		}
	}
}

// jobExprContext builds the job-level `if:` context (github, needs, vars,
// inputs). Callers hold the store write lock.
func (s *Engine) jobExprContext(wf *store.Workflow, wfJob *store.WorkflowJob) (*ExprContext, error) {
	// Jobs inside a reusable-workflow call see their own view: sibling needs under unprefixed keys, the synthetic gate
	// invisible, and the call's resolved inputs as the inputs context.
	var binding *store.WorkflowCallBinding
	if wfJob.Def != nil && wfJob.Def.Call != nil && wfJob.Def.CallRole == "" {
		binding = wfJob.Def.Call
	}

	deps := make(map[string]string, len(wfJob.Needs))
	needsCtx := make(map[string]interface{}, len(wfJob.Needs))
	for _, dep := range wfJob.Needs {
		depJob, ok := wf.Jobs[dep]
		if !ok {
			continue
		}
		ctxKey := dep
		if binding != nil {
			if dep == binding.CallerKey+"/__call" {
				continue
			}
			ctxKey = strings.TrimPrefix(dep, binding.CallerKey+"/")
		}
		deps[ctxKey] = string(depJob.Result)
		outputs := make(map[string]interface{}, len(depJob.Outputs))
		for k, v := range depJob.Outputs {
			outputs[k] = v
		}
		needsCtx[ctxKey] = map[string]interface{}{
			"result":  string(depJob.Result),
			"outputs": outputs,
		}
	}

	inputsCtx := make(map[string]interface{}, len(wf.Inputs))
	if binding != nil {
		if ri := binding.ResolvedInputs(); ri != nil {
			inputsCtx = ri
		}
	} else {
		for k, v := range wf.Inputs {
			inputsCtx[k] = v
		}
		for k, v := range wf.TypedInputs {
			inputsCtx[k] = v
		}
	}

	varsCtx := make(map[string]interface{})
	if wf.RepoFullName != "" {
		_, vars, err := s.collectJobSecretsAndVarsLocked(wf.RepoFullName, jobEnvironmentName(wfJob))
		if err != nil {
			return nil, err
		}
		for name, v := range vars {
			varsCtx[name] = v
		}
	}

	return &ExprContext{
		DepResults:        deps,
		WorkflowCancelled: wf.CancelRequested || wf.Result == store.ResultCancelled,
		Contexts: map[string]interface{}{
			"github": s.GithubContextMap(wf),
			"needs":  needsCtx,
			"inputs": inputsCtx,
			"vars":   varsCtx,
		},
	}, nil
}

// GithubContextMap assembles the server-side `github` expression context, mirroring the job message's contextData (same defaults as BuildJobMessageFromDef).
func (s *Engine) GithubContextMap(wf *store.Workflow) map[string]interface{} {
	eventName := wf.EventName
	if eventName == "" {
		eventName = "push"
	}
	repoFullName := wf.RepoFullName
	ref := wf.Ref
	if ref == "" && repoFullName != "" {
		ref = "refs/heads/main"
	}
	sha := wf.Sha
	repoOwner := repoFullName
	if idx := strings.Index(repoOwner, "/"); idx >= 0 {
		repoOwner = repoOwner[:idx]
	}
	refName := ref
	refType := "branch"
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		refName = strings.TrimPrefix(ref, "refs/heads/")
	case strings.HasPrefix(ref, "refs/tags/"):
		refName = strings.TrimPrefix(ref, "refs/tags/")
		refType = "tag"
	}
	m := map[string]interface{}{
		"event_name":       eventName,
		"ref":              ref,
		"ref_name":         refName,
		"ref_type":         refType,
		"sha":              sha,
		"repository":       repoFullName,
		"repository_owner": repoOwner,
		"run_id":           strconv.Itoa(wf.RunID),
		"run_number":       strconv.Itoa(wf.RunNumber),
		"run_attempt":      strconv.Itoa(wf.AttemptNumber()),
		"workflow":         wf.Name,
		"workflow_sha":     sha,
	}
	if repoFullName != "" && wf.WorkflowFilePath != "" {
		m["workflow_ref"] = repoFullName + "/" + wf.WorkflowFilePath + "@" + ref
	}
	if wf.EventPayload != nil {
		m["event"] = wf.EventPayload
		if sender, _ := wf.EventPayload["sender"].(map[string]interface{}); sender != nil {
			if login, _ := sender["login"].(string); login != "" {
				m["actor"] = login
			}
		}
		// PR-triggered runs carry head_ref/base_ref.
		if pr, _ := wf.EventPayload["pull_request"].(map[string]interface{}); pr != nil {
			if head, _ := pr["head"].(map[string]interface{}); head != nil {
				if r, _ := head["ref"].(string); r != "" {
					m["head_ref"] = r
				}
			}
			if base, _ := pr["base"].(map[string]interface{}); base != nil {
				if r, _ := base["ref"].(string); r != "" {
					m["base_ref"] = r
				}
			}
		}
	}
	return m
}

// ValidateJobGraph checks for cycles in the job dependency graph.
func ValidateJobGraph(wf *store.WorkflowDef) error {
	visited := make(map[string]int) // 0=unvisited, 1=visiting, 2=visited
	var visit func(key string) error
	visit = func(key string) error {
		if visited[key] == 2 {
			return nil
		}
		if visited[key] == 1 {
			return fmt.Errorf("cycle detected involving job %q", key)
		}
		visited[key] = 1

		jd, ok := wf.Jobs[key]
		if !ok {
			return fmt.Errorf("job %q references unknown dependency", key)
		}
		for _, dep := range jd.Needs {
			if _, ok := wf.Jobs[dep]; !ok {
				return fmt.Errorf("job %q needs unknown job %q", key, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[key] = 2
		return nil
	}

	for key := range wf.Jobs {
		if err := visit(key); err != nil {
			return err
		}
	}
	return nil
}

// workflowRollupLocked reports whether every job is terminal and whether any should fail the run. A continue-on-error job
// is excluded from the failure roll-up (but cancellation still counts — it speaks to failure, not an operator-stopped run). Caller holds the store lock.
func workflowRollupLocked(wf *store.Workflow) (allDone, anyFailed bool) {
	allDone = true
	for _, wfJob := range wf.Jobs {
		if wfJob.Status != store.JobStatusCompleted && wfJob.Status != store.JobStatusSkipped {
			allDone = false
		}
		switch {
		case wfJob.Result == store.ResultCancelled:
			anyFailed = true
		case wfJob.Result == store.ResultFailure && !wfJob.ContinueOnError:
			anyFailed = true
		}
	}
	return allDone, anyFailed
}
