package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// WorkflowEventMeta carries event metadata to be set on the workflow before dispatch.
type WorkflowEventMeta struct {
	EventName string
	Ref       string
	Sha       string
	Repo      string
	Inputs    map[string]string
	// Attempt sets the run's 1-based run_attempt (0 = first attempt);
	// reruns pass the incremented value.
	Attempt int
	// ReuseRunID keeps the original run id/number across rerun attempts
	// (real GitHub never mints a new run id for a re-run).
	ReuseRunID     int
	ReuseRunNumber int
	// WorkflowFileID / WorkflowFilePath preserve the originating workflow
	// file across rerun attempts, even when multiple files share the same
	// workflow display name.
	WorkflowFileID   int64
	WorkflowFilePath string
	// CarryOverJobs pre-completes jobs by key with results carried from
	// the previous attempt (rerun-failed-jobs keeps successful jobs).
	CarryOverJobs map[string]*WorkflowJob
	// TypedInputs carries workflow_dispatch inputs with their declared
	// types (boolean/number) for the `inputs` expression context;
	// Inputs keeps the string forms (github.event.inputs).
	TypedInputs map[string]interface{}
	// Payload is the webhook event payload that triggered the run; it
	// becomes the github.event expression/runner context.
	Payload map[string]interface{}
}

// SubmitWorkflow creates a Workflow from a WorkflowDef and begins dispatching jobs.

func (s *Engine) SubmitWorkflow(ctx context.Context, serverURL string, wf *WorkflowDef, defaultImage string, eventMeta ...*WorkflowEventMeta) (*Workflow, error) {
	ctx, span := otel.Tracer("bleephub").Start(ctx, "SubmitWorkflow",
		trace.WithAttributes(attribute.String("workflow.name", wf.Name)))
	defer span.End()
	// Validate no cycles in the job dependency graph
	if err := validateJobGraph(wf); err != nil {
		return nil, err
	}

	// Expand reusable-workflow calls (jobs.<id>.uses) into their called
	// jobs; the repository context resolves "./" references.
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

	workflow := &Workflow{
		ID:                uuid.New().String(),
		Name:              wf.Name,
		DisplayTitle:      wf.Name,
		RunID:             runID,
		Jobs:              make(map[string]*WorkflowJob),
		Env:               wf.Env,
		Permissions:       wf.Permissions,
		Status:            WorkflowStatusRunning,
		CreatedAt:         time.Now(),
		MatrixMaxParallel: make(map[string]int),
	}

	if workflow.Name == "" {
		workflow.Name = "workflow"
	}
	if workflow.DisplayTitle == "" {
		workflow.DisplayTitle = workflow.Name
	}

	// Apply concurrency from WorkflowDef
	if wf.Concurrency != nil {
		workflow.ConcurrencyGroup = wf.Concurrency.Group
		workflow.CancelInProgress = wf.Concurrency.CancelInProgress
	}

	// Create WorkflowJobs for each JobDef
	for key, jd := range wf.Jobs {
		wfJob := &WorkflowJob{
			Key:             key,
			JobID:           uuid.New().String(),
			DisplayName:     key,
			Needs:           jd.Needs,
			Status:          JobStatusPending,
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

	// Apply event metadata before any goroutines can observe the workflow
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

		// Carry results forward from the previous attempt
		// (rerun-failed-jobs re-runs only the failed jobs); applied
		// before the workflow is stored, so dispatch never sees the
		// carried jobs as pending.
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
			"github": s.githubContextMap(workflow),
			"inputs": inputsCtx,
		}})
		if err != nil {
			return nil, fmt.Errorf("run-name: %w", err)
		}
		workflow.DisplayTitle = displayTitle
	}

	// Concurrency groups are template strings on real GitHub
	// (`group: ci-${{ github.ref }}`); resolve them before grouping.
	if workflow.ConcurrencyGroup != "" && strings.Contains(workflow.ConcurrencyGroup, "${{") {
		inputsCtx := make(map[string]interface{}, len(workflow.Inputs))
		for k, v := range workflow.Inputs {
			inputsCtx[k] = v
		}
		group, err := EvalTemplate(workflow.ConcurrencyGroup, &ExprContext{Contexts: map[string]interface{}{
			"github": s.githubContextMap(workflow),
			"inputs": inputsCtx,
		}})
		if err != nil {
			return nil, fmt.Errorf("concurrency.group: %w", err)
		}
		workflow.ConcurrencyGroup = group
	}

	// Resolve the originating workflow FILE so the GitHub-shape run object
	// can reference its stable id + real path (workflow_id / workflow_url /
	// path), which are constant across every run produced from the file.
	workflow.WorkflowFileID, workflow.WorkflowFilePath = s.ResolveWorkflowFileForRun(workflow)
	if len(eventMeta) > 0 && eventMeta[0] != nil && eventMeta[0].ReuseRunNumber > 0 {
		workflow.RunNumber = eventMeta[0].ReuseRunNumber
	} else {
		workflow.RunNumber = s.store.ReserveWorkflowRunNumber(workflow)
	}

	// Fork-PR contributor approval: a run triggered by a pull request
	// whose head repository differs from the base repository holds in
	// action_required until a maintainer approves it (POST
	// .../runs/{run_id}/approve), when the repository policy requires
	// contributor approval — matching real GitHub's fork-PR gating.
	if workflowNeedsForkPRApproval(workflow, s.store) {
		workflow.Status = WorkflowStatusActionRequired
		s.store.Mu.Lock()
		s.store.Workflows[workflow.ID] = workflow
		s.store.PersistWorkflowRecord(workflow)
		s.store.Mu.Unlock()
		s.QueueEvent(EvRunRequested, workflow, nil)
		return workflow, nil
	}

	// Concurrency admission and insertion are one critical section. The prior
	// read-then-write sequence let simultaneous submissions both observe an
	// empty group and start. GitHub also retains only the newest pending run
	// for a group, so stale queued runs are cancelled as a new one arrives.
	// On a shared (multi-replica) database the critical section additionally
	// runs under the group's database lock, with peer-admitted state refreshed
	// in, so two replicas cannot both admit (ACT-012).
	var cancelForConcurrency []*Workflow
	if workflow.ConcurrencyGroup != "" {
		releaseGroupLock := s.acquireConcurrencyAdmissionLock(actionsConcurrencyLockName(workflow.ConcurrencyGroup))
		s.workflowConcurrencyMu.Lock()
		s.store.Mu.Lock()
		var active bool
		for _, existing := range s.store.WorkflowConcurrencyPeersLocked(workflow.ConcurrencyGroup) {
			switch existing.Status {
			case WorkflowStatusRunning, WorkflowStatusWaiting:
				active = true
				if workflow.CancelInProgress {
					cancelForConcurrency = append(cancelForConcurrency, existing)
				}
			case WorkflowStatusPendingConcurrency:
				// At most one pending run is retained, and it is the newest.
				cancelForConcurrency = append(cancelForConcurrency, existing)
			}
		}
		if active && !workflow.CancelInProgress {
			workflow.Status = WorkflowStatusPendingConcurrency
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
	if workflow.Status == WorkflowStatusPendingConcurrency {
		return workflow, nil
	}

	if s.metrics != nil {
		s.metrics.RecordWorkflowSubmit()
	}

	// Start timeout watcher goroutine
	s.StartTimeoutWatcher(workflow)

	// Dispatch root jobs (no dependencies)
	s.DispatchReadyJobs(ctx, workflow, serverURL, defaultImage)

	return workflow, nil
}

// ResolveWorkflowFileForRun finds the registered [WorkflowFile] that
// produced this run and returns its stable id and real path. When no
// backing file is registered yet, it derives a deterministic stable id
// from (repo, conventional-path) and a best-known path so workflow_id /
// path stay constant across reruns of the same workflow even before the
// file lands in git.
func (s *Engine) ResolveWorkflowFileForRun(wf *Workflow) (int64, string) {
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
	// No registered file: derive a stable id from the conventional path so
	// the run still reports a constant workflow_id across reruns.
	path := ".github/workflows/" + wf.Name + ".yml"
	return stableWorkflowFileID(repo, path), path
}

// DispatchReadyJobs finds pending jobs whose dependencies are all satisfied
// and dispatches them to the runner. Loops until stable (skipping cascades).
func (s *Engine) DispatchReadyJobs(ctx context.Context, wf *Workflow, serverURL string, defaultImage string) {
	ctx, span := otel.Tracer("bleephub").Start(ctx, "DispatchReadyJobs",
		trace.WithAttributes(attribute.String("workflow.id", wf.ID)))
	defer span.End()
	// Job-level concurrency admission must be serialized across replicas on a
	// shared database (ACT-012). The lock is scoped to one pass of the loop —
	// acquired before the store lock, released before the recursive dispatch
	// of affected workflows below (which takes the same lock itself).
	jobAdmissionNeedsLock := workflowHasJobConcurrency(wf)
	for {
		releaseJobAdmission := func() {}
		if jobAdmissionNeedsLock {
			releaseJobAdmission = s.acquireJobConcurrencyAdmissionLock(wf)
		}
		// Hold write lock while evaluating and updating job statuses
		s.store.Mu.Lock()
		changed := false
		var toDispatch []*WorkflowJob
		var jobsToCancel []string
		affectedWorkflows := map[*Workflow]bool{}
		for _, wfJob := range wf.Jobs {
			if wfJob.Status != JobStatusPending {
				continue
			}

			// Check all dependencies are completed
			allDepsOk := true
			anyDepFailed := false
			for _, dep := range wfJob.Needs {
				depJob, ok := wf.Jobs[dep]
				if !ok {
					allDepsOk = false
					break
				}
				if depJob.Status == JobStatusCompleted || depJob.Status == JobStatusSkipped {
					if depJob.Result != ResultSuccess && !depJob.ContinueOnError {
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

			// A cancel-requested run only dispatches jobs explicitly
			// gated on always()/cancelled(); everything else cancels.
			if wf.CancelRequested {
				gated := false
				if wfJob.Def != nil {
					hasAlways, _ := ExprContainsStatusFunction(wfJob.Def.If)
					gated = hasAlways || strings.Contains(strings.ToLower(wfJob.Def.If), "cancelled()")
				}
				if !gated {
					wfJob.Status = JobStatusCompleted
					wfJob.Result = ResultCancelled
					wfJob.CompletedAt = time.Now()
					s.QueueEvent(EvJobCompleted, wf, wfJob)
					changed = true
					continue
				}
			}

			// Evaluate job-level if: condition
			if wfJob.Def != nil && wfJob.Def.If != "" {
				hasAlways, hasFailure := ExprContainsStatusFunction(wfJob.Def.If)
				exprCtx, err := s.jobExprContext(wf, wfJob)
				if err != nil {
					wfJob.Status = JobStatusCompleted
					wfJob.Result = ResultFailure
					wfJob.CompletedAt = time.Now()
					s.logger.Warn().Err(err).Str("job", wfJob.Key).
						Msg("job if: context error — failing job")
					changed = true
					continue
				}

				ok, err := EvalExprErr(wfJob.Def.If, exprCtx)
				if err != nil {
					// Real GitHub fails the job (and run) on an invalid
					// expression rather than silently skipping it.
					wfJob.Status = JobStatusCompleted
					wfJob.Result = ResultFailure
					wfJob.CompletedAt = time.Now()
					s.logger.Warn().Err(err).Str("job", wfJob.Key).Str("if", wfJob.Def.If).
						Msg("job if: expression error — failing job")
					changed = true
					continue
				}
				if !ok {
					wfJob.Status = JobStatusSkipped
					wfJob.Result = ResultSkipped
					s.logger.Info().Str("job", wfJob.Key).Str("if", wfJob.Def.If).Msg("skipping job (if: false)")
					s.QueueEvent(EvJobCompleted, wf, wfJob)
					changed = true
					continue
				}

				// If expression contains always() or failure(), override dep-failure skip
				if hasAlways || hasFailure {
					anyDepFailed = false
				}
			}

			// If any dependency failed (and not continue-on-error), skip this job
			if anyDepFailed {
				wfJob.Status = JobStatusSkipped
				wfJob.Result = ResultSkipped
				s.logger.Info().Str("job", wfJob.Key).Msg("skipping job (dependency failed)")
				s.QueueEvent(EvJobCompleted, wf, wfJob)
				changed = true
				continue
			}

			// Synthetic reusable-workflow nodes complete in the engine —
			// gates resolve call inputs, collectors map call outputs —
			// and never dispatch to a runner. (Hidden: no checks events.)
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
							wfJob.Status = JobStatusCompleted
							wfJob.Result = ResultFailure
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
						wfJob.Status = JobStatusCompleted
						wfJob.Result = ResultFailure
						wfJob.CompletedAt = s.currentTime()
						s.QueueEvent(EvJobCompleted, wf, wfJob)
						changed = true
						continue
					}
					// Index immediately: a sibling job in the same group must
					// see this one within the same lock pass, before the
					// workflow record is next persisted.
					s.store.SyncJobConcurrencyEntryLocked(wf, wfJob)
				}
				blocked := false
				for _, peer := range s.store.JobConcurrencyPeersLocked(wfJob.ConcurrencyGroup) {
					other, otherWorkflow := peer.Job, peer.Wf
					if other == wfJob {
						continue
					}
					if other.Status == JobStatusPending {
						currentIsNewer := wf.CreatedAt.After(otherWorkflow.CreatedAt) ||
							(wf.CreatedAt.Equal(otherWorkflow.CreatedAt) && wf.ID > otherWorkflow.ID)
						if !currentIsNewer {
							blocked = true
							continue
						}
						other.Status = JobStatusCompleted
						other.Result = ResultCancelled
						other.CompletedAt = s.currentTime()
						s.QueueEvent(EvJobCompleted, otherWorkflow, other)
						affectedWorkflows[otherWorkflow] = true
						s.store.PersistWorkflowRecord(otherWorkflow)
						changed = true
						continue
					}
					if other.Status != JobStatusQueued && other.Status != JobStatusRunning {
						continue
					}
					if !wfJob.CancelInProgress {
						blocked = true
						continue
					}
					other.Status = JobStatusCompleted
					other.Result = ResultCancelled
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

			// Enforce max-parallel: count running/queued jobs in same matrix group
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
					if (j.Status == JobStatusQueued || j.Status == JobStatusRunning) && j.MatrixGroup == wfJob.MatrixGroup && wfJob.MatrixGroup != "" {
						active++
					}
				}
				if active >= maxParallel {
					continue // Skip dispatch, will retry when a job completes
				}
			}

			// Environment protection: a job targeting an environment
			// with required reviewers waits for a deployment review
			// (approve via POST .../runs/{id}/pending_deployments).
			if envName := jobEnvironmentName(wfJob); envName != "" && !envApproved(wf, envName) {
				if env := s.protectedEnvironmentLocked(wf, envName); env != nil {
					wfJob.Status = JobStatusWaiting
					addPendingDeployment(wf, env)
					if wf.Status == WorkflowStatusRunning {
						wf.Status = WorkflowStatusWaiting
					}
					s.logger.Info().Str("job", wfJob.Key).Str("environment", envName).
						Msg("job waiting for deployment review")
					s.QueueEvent(EvJobWaiting, wf, wfJob)
					changed = true
					continue
				}
			}

			// Mark as queued now so max-parallel checks in this iteration see it
			wfJob.Status = JobStatusQueued
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

		// Dispatch collected jobs outside the lock (dispatchWorkflowJob acquires its own locks)
		for _, wfJob := range toDispatch {
			s.dispatchWorkflowJob(ctx, wf, wfJob, serverURL, defaultImage)
		}

		if !changed {
			break
		}
	}

	// A workflow can reach the all-done state here without any runner
	// completion event (server-completed collector as the final node);
	// finalize is idempotent.
	s.FinalizeWorkflowIfDone(wf)
}

func workflowDispatchCoordinates(wf *Workflow) (serverURL, defaultImage string) {
	if wf != nil && wf.Env != nil {
		serverURL = wf.Env["__serverURL"]
		defaultImage = wf.Env["__defaultImage"]
	}
	return serverURL, defaultImage
}

// dispatchWorkflowJob builds and sends a job message to the runner.
func (s *Engine) dispatchWorkflowJob(ctx context.Context, wf *Workflow, wfJob *WorkflowJob, serverURL, defaultImage string) {
	_, span := otel.Tracer("bleephub").Start(ctx, "dispatchWorkflowJob",
		trace.WithAttributes(
			attribute.String("workflow.id", wf.ID),
			attribute.String("job.key", wfJob.Key)))
	defer span.End()
	planID := uuid.New().String()
	timelineID := uuid.New().String()
	requestID := s.NextRequestID()

	msg, err := s.buildJobMessageFromDef(serverURL, wf, wfJob, planID, timelineID, requestID, defaultImage)
	if err != nil {
		s.store.Mu.Lock()
		wfJob.Status = JobStatusCompleted
		wfJob.Result = ResultFailure
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

	job := &Job{
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

	envelope := &TaskAgentMessage{
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

// OnJobCompleted is called when a job finishes. It updates the workflow
// and dispatches any newly-ready dependent jobs.
func (s *Engine) OnJobCompleted(ctx context.Context, jobID, result string) {
	ctx, span := otel.Tracer("bleephub").Start(ctx, "OnJobCompleted",
		trace.WithAttributes(
			attribute.String("job.id", jobID),
			attribute.String("job.result", result)))
	defer span.End()

	// Find the workflow and job under write lock, update status atomically
	s.store.Mu.Lock()
	var foundWf *Workflow
	var foundJob *WorkflowJob
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

	// The official runner reports a job's completion twice — once via
	// DELETE /_apis/v1/AgentRequest and once via POST /_apis/v1/FinishJob —
	// and both land here. Only the first terminal transition may finalize the
	// job, emit the completion event, and re-drive dispatch; a second call must
	// not re-emit a duplicate workflow_job/check_run webhook, re-run dispatch,
	// or flip an already-recorded conclusion.
	if foundJob.Status == JobStatusCompleted || foundJob.Status == JobStatusSkipped {
		s.store.Mu.Unlock()
		return
	}

	foundJob.Status = JobStatusCompleted
	foundJob.Result = Result(normalizeResult(result))
	foundJob.CompletedAt = time.Now()
	s.QueueEvent(EvJobCompleted, foundWf, foundJob)

	// Matrix fail-fast: if this job failed and it's in a matrix group, cancel
	// siblings.
	//
	// A job marked continue-on-error does not trigger it. Its failure is
	// tolerated by definition, and cancelling the siblings would put the run
	// back on the failing path through the back door: the roll-up excludes the
	// tolerated failure but counts a cancellation, so the same tolerated
	// failure turned a matrix run red while an identical plain job stayed
	// green.
	if foundJob.Result == ResultFailure && foundJob.MatrixGroup != "" && !foundJob.ContinueOnError {
		if foundJob.Def.FailFast() {
			for _, sibling := range foundWf.Jobs {
				if sibling.Key == foundJob.Key {
					continue
				}
				if sibling.MatrixGroup != foundJob.MatrixGroup {
					continue
				}
				if sibling.Status == JobStatusPending || sibling.Status == JobStatusQueued {
					sibling.Status = JobStatusCompleted
					sibling.Result = ResultCancelled
					sibling.CompletedAt = time.Now()
					s.QueueEvent(EvJobCompleted, foundWf, sibling)
					s.logger.Info().
						Str("job", sibling.Key).
						Str("reason", "fail-fast").
						Msg("cancelling matrix sibling")
				}
			}
		}
	}
	s.store.PersistWorkflowRecord(foundWf)
	s.store.Mu.Unlock()

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

	// Dispatch any newly-ready jobs (this may also mark some as skipped)
	if foundWf.Env != nil {
		if serverURL, ok := foundWf.Env["__serverURL"]; ok {
			defaultImage := foundWf.Env["__defaultImage"]
			s.DispatchReadyJobs(ctx, foundWf, serverURL, defaultImage)
		}
	}
	if foundJob.ConcurrencyGroup != "" {
		s.startPendingJobConcurrency(ctx, foundJob.ConcurrencyGroup)
	}

	// Check if all jobs are done (after dispatch, which may skip dependents)
	s.store.Mu.Lock()
	allDone, anyFailed := workflowRollupLocked(foundWf)

	// DispatchReadyJobs may already have finalized the run (a server-
	// completed collector can be the last node); don't double-complete.
	if foundWf.Status == WorkflowStatusCompleted {
		allDone = false
	}

	if allDone {
		foundWf.Status = WorkflowStatusCompleted
		switch {
		case foundWf.CancelRequested:
			foundWf.Result = ResultCancelled
		case anyFailed:
			foundWf.Result = ResultFailure
		default:
			foundWf.Result = ResultSuccess
		}
		// The run is over: its job messages (GITHUB_TOKEN + every secret
		// value) are no longer needed for delivery or redelivery. Late runner
		// calls authenticate through planScopes.
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

		// Check for pending-concurrency workflows in the same group
		if concurrencyGroup != "" {
			s.startPendingConcurrencyWorkflow(concurrencyGroup)
		}
	}
}

func (s *Engine) startPendingJobConcurrency(ctx context.Context, group string) {
	s.store.Mu.Lock()
	pending := make([]*Workflow, 0)
	seen := map[*Workflow]bool{}
	for _, peer := range s.store.JobConcurrencyPeersLocked(group) {
		if peer.Job.Status == JobStatusPending && !seen[peer.Wf] {
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

// jobEnvironmentName resolves a job's target environment, tolerating a
// nil Def (directly-seeded test jobs).
func jobEnvironmentName(wfJob *WorkflowJob) string {
	if wfJob.Def == nil {
		return ""
	}
	return wfJob.Def.EnvironmentName()
}

// envApproved reports whether an approved review covering the
// environment has been submitted for this run.
func envApproved(wf *Workflow, envName string) bool {
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

// protectedEnvironmentLocked returns the run repo's environment when it exists
// and carries required reviewers; environments without reviewers (or
// runs without a repo) don't gate dispatch. Referencing an environment
// auto-creates it, matching real GitHub.
// The caller holds s.store.mu.
func (s *Engine) protectedEnvironmentLocked(wf *Workflow, envName string) *Environment {
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

// addPendingDeployment records the run's wait on an environment exactly once.
func addPendingDeployment(wf *Workflow, env *Environment) {
	for _, p := range wf.PendingDeployments {
		if p.EnvID == env.ID {
			return
		}
	}
	wf.PendingDeployments = append(wf.PendingDeployments, &PendingDeployment{
		EnvID:              env.ID,
		EnvName:            env.Name,
		WaitTimerStartedAt: time.Now().UTC(),
	})
}

// ApplyDeploymentReview resolves a submitted review against the run's
// pending deployments: approved environments release their waiting jobs
// back to pending and dispatch resumes; rejected environments fail their
// waiting jobs and the run finalizes when nothing else is in flight.
// Returns the environment names the review covered.
func (s *Engine) ApplyDeploymentReview(ctx context.Context, wf *Workflow, envIDs []int, state, comment string, reviewer *User) []string {
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
	wf.EnvApprovals = append(wf.EnvApprovals, &EnvApproval{
		State:     state,
		Comment:   comment,
		UserID:    reviewerID,
		EnvIDs:    append([]int(nil), envIDs...),
		EnvNames:  append([]string(nil), names...),
		CreatedAt: time.Now().UTC(),
	})

	for _, wfJob := range wf.Jobs {
		if wfJob.Status != JobStatusWaiting || !covered[jobEnvironmentName(wfJob)] {
			continue
		}
		if state == "approved" {
			wfJob.Status = JobStatusPending
		} else {
			wfJob.Status = JobStatusCompleted
			wfJob.Result = ResultFailure
			wfJob.CompletedAt = time.Now()
			s.QueueEvent(EvJobCompleted, wf, wfJob)
		}
	}
	if len(wf.PendingDeployments) == 0 && wf.Status == WorkflowStatusWaiting {
		wf.Status = WorkflowStatusRunning
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

// FinalizeWorkflowIfDone completes the run when every job has reached a
// terminal state — the same check OnJobCompleted performs after each
// job, needed independently when a rejection fails jobs without any job
// completion event.
func (s *Engine) FinalizeWorkflowIfDone(wf *Workflow) {
	s.store.Mu.Lock()
	allDone, anyFailed := workflowRollupLocked(wf)
	if allDone && wf.Status != WorkflowStatusCompleted {
		wf.Status = WorkflowStatusCompleted
		switch {
		case wf.CancelRequested:
			wf.Result = ResultCancelled
		case anyFailed:
			wf.Result = ResultFailure
		default:
			wf.Result = ResultSuccess
		}
		// See OnJobCompleted: a finalized run's secret-bearing job messages
		// are dropped; auth for late runner calls rides planScopes.
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
			s.startPendingConcurrencyWorkflow(concurrencyGroup)
		}
	}
}

// CancelWorkflow requests cancellation of a run: pending/queued jobs
// cancel immediately (their undelivered messages are purged), RUNNING
// jobs get a JobCancellation broker message so the runner actually
// aborts them, and jobs gated on always()/cancelled() still dispatch —
// matching real GitHub's cancellation semantics. The run finalizes
// (conclusion cancelled) once nothing remains in flight.
func (s *Engine) CancelWorkflow(wf *Workflow) {
	s.store.Mu.Lock()
	wf.CancelRequested = true

	cancelledJobIDs := map[string]bool{}
	var runningJobIDs []string
	for _, wfJob := range wf.Jobs {
		switch wfJob.Status {
		case JobStatusPending, JobStatusWaiting:
			// Jobs gated on always()/cancelled() still run after a
			// cancel on real GitHub — leave them pending; dispatch
			// evaluates their `if:` with cancelled()==true.
			if wfJob.Def != nil {
				if hasAlways, _ := ExprContainsStatusFunction(wfJob.Def.If); hasAlways ||
					strings.Contains(strings.ToLower(wfJob.Def.If), "cancelled()") {
					wfJob.Status = JobStatusPending
					continue
				}
			}
			wfJob.Status = JobStatusCompleted
			wfJob.Result = ResultCancelled
			wfJob.CompletedAt = time.Now()
			cancelledJobIDs[wfJob.JobID] = true
			s.QueueEvent(EvJobCompleted, wf, wfJob)
		case JobStatusQueued, JobStatusRunning:
			// Delivered to (or executing on) a runner: signal the
			// runner. Undelivered queued messages are purged from the
			// pending queue below and the job cancels immediately.
			if job := s.store.Jobs[wfJob.JobID]; job != nil && job.AgentID != 0 && job.Status != "completed" {
				runningJobIDs = append(runningJobIDs, wfJob.JobID)
			} else {
				wfJob.Status = JobStatusCompleted
				wfJob.Result = ResultCancelled
				wfJob.CompletedAt = time.Now()
				cancelledJobIDs[wfJob.JobID] = true
				s.QueueEvent(EvJobCompleted, wf, wfJob)
			}
		}
	}

	// Drop queued-but-undelivered job messages so a runner can't pull a
	// cancelled job later.
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

	// JobCancellation rides the runner's open mid-job poll (the channel
	// push path — job REQUESTS are pull-only; cancellations are exactly
	// what the open poll exists for).
	for _, jobID := range runningJobIDs {
		s.SendJobCancellation(jobID)
	}

	s.logger.Info().
		Str("workflow_id", wf.ID).
		Str("workflow_name", wf.Name).
		Int("signalled_running", len(runningJobIDs)).
		Msg("workflow cancellation requested")

	// Dispatch any always()/cancelled() jobs whose dependencies are
	// already settled, then finalize if nothing remains in flight.
	if serverURL != "" {
		s.DispatchReadyJobs(context.Background(), wf, serverURL, defaultImage)
	} else {
		s.FinalizeWorkflowIfDone(wf)
	}
}

// startPendingConcurrencyWorkflow finds and starts the next pending-concurrency
// workflow in the given concurrency group. On a shared (multi-replica)
// database the promotion runs under the group's database lock so a peer's
// simultaneous submission or promotion cannot double-admit (ACT-012).
func (s *Engine) startPendingConcurrencyWorkflow(group string) {
	// The database lock is released as soon as the promotion decision is
	// committed (before stale-run cancellation, which can recurse into this
	// function for the same group).
	releaseGroupLock := s.acquireConcurrencyAdmissionLock(actionsConcurrencyLockName(group))
	s.workflowConcurrencyMu.Lock()
	s.store.Mu.Lock()
	var pendingWf *Workflow
	var stale []*Workflow
	for _, wf := range s.store.WorkflowConcurrencyPeersLocked(group) {
		if wf.Status == WorkflowStatusRunning || wf.Status == WorkflowStatusWaiting {
			s.store.Mu.Unlock()
			s.workflowConcurrencyMu.Unlock()
			releaseGroupLock()
			return
		}
		if wf.Status == WorkflowStatusPendingConcurrency {
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

	pendingWf.Status = WorkflowStatusRunning
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

// workflowNeedsForkPRApproval reports whether the run must hold in
// action_required for a maintainer's approval: it was triggered by a
// pull_request event whose head repository is a fork of (differs from)
// the base repository, and the base repository's fork-PR
// contributor-approval policy requires approval.
func workflowNeedsForkPRApproval(wf *Workflow, st *Store) bool {
	if wf.EventName != "pull_request" || wf.RepoFullName == "" || wf.EventPayload == nil {
		return false
	}
	if !pullRequestIsFromFork(wf.EventPayload, wf.RepoFullName) {
		return false
	}
	policy := st.GetRepoActionsPermissions(wf.RepoFullName).ForkPRContributorApproval
	return policy != "" && policy != "none"
}

// normalizeResult converts runner result strings to consistent format.
func normalizeResult(result string) string {
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

// StartTimeoutWatcher starts a goroutine that periodically checks for timed-out jobs.
func (s *Engine) StartTimeoutWatcher(wf *Workflow) {
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
				s.checkJobTimeouts(wf)
			}
		}
	}()
}

func (s *Engine) StopTimeoutWatcher(wf *Workflow) {
	s.workflowTimeoutMu.Lock()
	cancel := wf.CancelTimeout
	wf.CancelTimeout = nil
	s.workflowTimeoutMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// jobLeaseDuration is how long a dispatched job stays leased to the runner
// holding it. The runner renews the lease while it works; a lease that lapses
// means the runner is gone.
const jobLeaseDuration = 1 * time.Hour

// reclaimExpiredJobLeases puts jobs whose runner stopped renewing back on the
// broker's queue. Without it the lease was written three times and read
// nowhere, so a runner that died mid-job left the job — and its run — hung
// until the six-hour job timeout cancelled it.
//
// A job still sitting on the pending queue has not been taken by anyone, so
// there is nothing to reclaim; its lease is simply extended. Callers must not
// hold the store lock.
func (s *Engine) reclaimExpiredJobLeases(wf *Workflow) {
	now := s.currentTime()
	var redeliver []*TaskAgentMessage
	var reclaimed []string

	s.store.Mu.Lock()
	queued := make(map[string]bool, len(s.store.PendingMessages))
	for _, msg := range s.store.PendingMessages {
		if msg.JobID != "" {
			queued[msg.JobID] = true
		}
	}
	for _, wfJob := range wf.Jobs {
		if wfJob.Status != JobStatusQueued && wfJob.Status != JobStatusRunning {
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
		// The runner that held the lease is gone; free its agent record so a
		// reconnecting (non-ephemeral) runner can take work again.
		s.store.ClearAgentAssignmentLocked(job)
		job.AgentID = 0
		wfJob.Status = JobStatusQueued
		reclaimed = append(reclaimed, wfJob.Key)
		redeliver = append(redeliver, &TaskAgentMessage{
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

// checkJobTimeouts cancels jobs that have exceeded their timeout, and hands
// back any job whose runner stopped renewing its lease.
func (s *Engine) checkJobTimeouts(wf *Workflow) {
	s.reclaimExpiredJobLeases(wf)

	s.store.Mu.Lock()
	if wf.Status == WorkflowStatusCompleted {
		s.store.Mu.Unlock()
		return
	}
	now := s.currentTime()
	var timedOut bool
	var timedOutJobIDs []string
	for _, wfJob := range wf.Jobs {
		if wfJob.Status != JobStatusRunning {
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
			wfJob.Status = JobStatusCompleted
			wfJob.Result = ResultFailure
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

	// Re-dispatch to handle dependents (outside lock since DispatchReadyJobs acquires locks)
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

// jobExprContext builds the expression-evaluation context for a job-level
// `if:` with the contexts real GitHub makes available there: github,
// needs, vars, and inputs. Callers hold the store write lock.
func (s *Engine) jobExprContext(wf *Workflow, wfJob *WorkflowJob) (*ExprContext, error) {
	// Jobs inside a reusable-workflow call see their workflow's own view:
	// sibling needs under unprefixed keys, the synthetic gate invisible,
	// and the call's resolved inputs as the inputs context.
	var binding *WorkflowCallBinding
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
		WorkflowCancelled: wf.CancelRequested || wf.Result == ResultCancelled,
		Contexts: map[string]interface{}{
			"github": s.githubContextMap(wf),
			"needs":  needsCtx,
			"inputs": inputsCtx,
			"vars":   varsCtx,
		},
	}, nil
}

// githubContextMap assembles the server-side `github` context for
// expression evaluation, mirroring the fields the runner receives in the
// job message's contextData (same defaults as buildJobMessageFromDef).
func (s *Engine) githubContextMap(wf *Workflow) map[string]interface{} {
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
		// PR-triggered runs carry head_ref/base_ref, like real GitHub.
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

// validateJobGraph checks for cycles in the job dependency graph.
func validateJobGraph(wf *WorkflowDef) error {
	// Topological sort via DFS
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

// workflowRollupLocked reports whether every job in a run has reached a
// terminal state, and whether any of them should make the run fail.
//
// A job marked continue-on-error is excluded from the failure roll-up. That is
// the documented purpose of the key — it "prevents a workflow run from failing
// when a job fails" — and the dependency-skip logic already honoured it; only
// the conclusion did not, so a run whose one failure was explicitly tolerated
// still went red. Cancellation is not tolerated: continue-on-error speaks to
// failure, not to a run the operator stopped.
//
// This existed as two byte-identical copies, one in OnJobCompleted and one in
// FinalizeWorkflowIfDone. Both are now this function, so the run conclusion
// cannot depend on which path finalized it.
//
// The caller must hold the store lock.
func workflowRollupLocked(wf *Workflow) (allDone, anyFailed bool) {
	allDone = true
	for _, wfJob := range wf.Jobs {
		if wfJob.Status != JobStatusCompleted && wfJob.Status != JobStatusSkipped {
			allDone = false
		}
		switch {
		case wfJob.Result == ResultCancelled:
			anyFailed = true
		case wfJob.Result == ResultFailure && !wfJob.ContinueOnError:
			anyFailed = true
		}
	}
	return allDone, anyFailed
}
