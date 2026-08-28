package bleephub

import (
	"context"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestCancelledGatedJobWithNeedsStillRuns pins the rule that a job's implicit
// `success()` drops as soon as its `if:` names any status function. On
// cancellation, dependency handling must not skip a cancelled()-gated job for
// the very cancellation that made its condition true.
func TestCancelledGatedJobWithNeedsStillRuns(t *testing.T) {
	s := newTestServer()
	wf := &store.WorkflowDef{
		Name: "cancel-gate",
		Jobs: map[string]*store.JobDef{
			"build":   {Steps: []store.StepDef{{Run: "sleep 999"}}},
			"cleanup": {Needs: []string{"build"}, If: "cancelled()", Steps: []store.StepDef{{Run: "echo cleanup"}}},
		},
	}
	workflow, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", wf, "alpine:latest")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	workflow.Env = map[string]string{"__serverURL": "http://localhost", "__defaultImage": "alpine:latest"}

	s.actions.CancelWorkflow(workflow)

	if got := workflow.Jobs["build"].Result; got != store.ResultCancelled {
		t.Fatalf("build result = %q, want cancelled", got)
	}
	if got := workflow.Jobs["cleanup"].Status; got != store.JobStatusQueued {
		t.Errorf("cleanup status = %q (result %q), want queued: an `if: cancelled()` job must not be skipped for the cancellation of its dependency",
			got, workflow.Jobs["cleanup"].Result)
	}
}

// TestFailedDependencyStillSkipsPlainConditionalJob is the other half: an `if:`
// with no status function keeps the implicit success(), so a failed dependency
// still skips the job even when the expression is true.
func TestFailedDependencyStillSkipsPlainConditionalJob(t *testing.T) {
	s := newTestServer()
	wf := &store.WorkflowDef{
		Name: "plain-gate",
		Jobs: map[string]*store.JobDef{
			"build":  {Steps: []store.StepDef{{Run: "exit 1"}}},
			"report": {Needs: []string{"build"}, If: "needs.build.result == 'failure'", Steps: []store.StepDef{{Run: "echo"}}},
		},
	}
	workflow, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", wf, "alpine:latest")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	workflow.Env = map[string]string{"__serverURL": "http://localhost", "__defaultImage": "alpine:latest"}

	s.actions.OnJobCompleted(context.Background(), workflow.Jobs["build"].JobID, "Failed")

	if got := workflow.Jobs["report"].Status; got != store.JobStatusSkipped {
		t.Errorf("report status = %q, want skipped (no status function means success() still applies)", got)
	}
}
