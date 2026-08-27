package actions

import "github.com/e6qu/bleephub/internal/store"

// RunnerVariableValue is one runner-evaluated output value from the
// JobCompleted plan event.
type RunnerVariableValue struct {
	Value    string `json:"value"`
	IsSecret bool   `json:"isSecret"`
}

// CaptureResolvedJobOutputs stores the output values evaluated by
// actions/runner rather than re-resolving step expressions server-side.
func (s *Engine) CaptureResolvedJobOutputs(jobID string, outputs map[string]RunnerVariableValue) {
	if len(outputs) == 0 {
		return
	}

	resolved := make(map[string]string, len(outputs))
	for name, output := range outputs {
		resolved[name] = output.Value
	}

	s.store.Mu.Lock()
	var wfJob *store.WorkflowJob
	var workflow *store.Workflow
	for _, wf := range s.store.Workflows {
		if job, ok := FindWorkflowJobByID(wf, jobID); ok {
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

// FindWorkflowJobByID scans a workflow's jobs for the engine job UUID. Callers
// hold the store lock.
func FindWorkflowJobByID(wf *store.Workflow, jobID string) (*store.WorkflowJob, bool) {
	for _, wfJob := range wf.Jobs {
		if wfJob.JobID == jobID {
			return wfJob, true
		}
	}
	return nil, false
}
