package bleephub

import (
	"context"
	"testing"

	"github.com/e6qu/bleephub/internal/actions"
)

func concurrentJobWorkflow(group string, cancel bool) *WorkflowDef {
	return &WorkflowDef{
		Name: "job-concurrency",
		Env: map[string]string{
			"__serverURL":    "http://localhost",
			"__defaultImage": "",
		},
		Jobs: map[string]*JobDef{
			"build": {
				RunsOn: "ubuntu-latest",
				Steps:  []StepDef{{Run: "echo build"}},
				Concurrency: &ConcurrencyDef{
					Group:            group,
					CancelInProgress: cancel,
				},
			},
		},
	}
}

func TestJobConcurrencyQueuesAndReleasesAcrossWorkflowRuns(t *testing.T) {
	s := newTestServer()
	testRepo(t, s, "admin", "concurrency-repo", false)
	meta := &actions.WorkflowEventMeta{Repo: "admin/concurrency-repo", Ref: "refs/heads/main", Sha: "abc"}
	first, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", concurrentJobWorkflow("deploy", false), "", meta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", concurrentJobWorkflow("DEPLOY", false), "", meta)
	if err != nil {
		t.Fatal(err)
	}
	if first.Jobs["build"].Status != JobStatusQueued {
		t.Fatalf("first job = %q, want queued", first.Jobs["build"].Status)
	}
	if second.Jobs["build"].Status != JobStatusPending {
		t.Fatalf("second job = %q, want pending", second.Jobs["build"].Status)
	}

	s.actions.OnJobCompleted(context.Background(), first.Jobs["build"].JobID, "Succeeded")
	if second.Jobs["build"].Status != JobStatusQueued {
		t.Fatalf("second job after release = %q, want queued", second.Jobs["build"].Status)
	}
}

func TestJobConcurrencyCancelInProgressCancelsHolder(t *testing.T) {
	s := newTestServer()
	testRepo(t, s, "admin", "concurrency-cancel", false)
	meta := &actions.WorkflowEventMeta{Repo: "admin/concurrency-cancel", Ref: "refs/heads/main", Sha: "def"}
	first, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", concurrentJobWorkflow("deploy", false), "", meta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", concurrentJobWorkflow("deploy", true), "", meta)
	if err != nil {
		t.Fatal(err)
	}
	if first.Jobs["build"].Status != JobStatusCompleted || first.Jobs["build"].Result != ResultCancelled {
		t.Fatalf("first job = %q/%q, want completed/cancelled", first.Jobs["build"].Status, first.Jobs["build"].Result)
	}
	if second.Jobs["build"].Status != JobStatusQueued {
		t.Fatalf("replacement job = %q, want queued", second.Jobs["build"].Status)
	}
}
