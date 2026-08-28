package bleephub

import (
	"context"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestParseConcurrencyString(t *testing.T) {
	yaml := `
name: test
concurrency: deploy-group
jobs:
  build:
    runs-on: self-hosted
    steps:
      - run: echo build
`
	wf, err := store.ParseWorkflow([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if wf.Concurrency == nil {
		t.Fatal("concurrency should be set")
	}
	if wf.Concurrency.Group != "deploy-group" {
		t.Errorf("group = %q, want deploy-group", wf.Concurrency.Group)
	}
	if wf.Concurrency.CancelInProgress {
		t.Error("cancel-in-progress should default to false")
	}
}

func TestParseConcurrencyObject(t *testing.T) {
	yaml := `
name: test
concurrency:
  group: deploy-${{ github.ref }}
  cancel-in-progress: true
jobs:
  build:
    runs-on: self-hosted
    steps:
      - run: echo build
`
	wf, err := store.ParseWorkflow([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if wf.Concurrency == nil {
		t.Fatal("concurrency should be set")
	}
	if wf.Concurrency.Group != "deploy-${{ github.ref }}" {
		t.Errorf("group = %q", wf.Concurrency.Group)
	}
	if !wf.Concurrency.CancelInProgress {
		t.Error("cancel-in-progress should be true")
	}
}

func TestConcurrencyCancelInProgress(t *testing.T) {
	s := newTestServer()
	s.metrics = NewMetrics()

	wf1 := &store.WorkflowDef{
		Name:        "wf1",
		Concurrency: &store.ConcurrencyDef{Group: "deploy", CancelInProgress: true},
		Jobs: map[string]*store.JobDef{
			"build": {Steps: []store.StepDef{{Run: "sleep 999"}}},
		},
	}
	workflow1, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", wf1, "alpine:latest")
	if err != nil {
		t.Fatalf("submit wf1: %v", err)
	}
	if workflow1.Status != "running" {
		t.Fatalf("wf1 status = %q, want running", workflow1.Status)
	}

	wf2 := &store.WorkflowDef{
		Name:        "wf2",
		Concurrency: &store.ConcurrencyDef{Group: "deploy", CancelInProgress: true},
		Jobs: map[string]*store.JobDef{
			"build": {Steps: []store.StepDef{{Run: "echo build"}}},
		},
	}
	workflow2, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", wf2, "alpine:latest")
	if err != nil {
		t.Fatalf("submit wf2: %v", err)
	}

	if workflow1.Status != "completed" || workflow1.Result != "cancelled" {
		t.Errorf("wf1 = %s/%s, want completed/cancelled", workflow1.Status, workflow1.Result)
	}

	if workflow2.Status != "running" {
		t.Errorf("wf2 status = %q, want running", workflow2.Status)
	}
}

func TestConcurrencyGroupIsolation(t *testing.T) {
	s := newTestServer()
	s.metrics = NewMetrics()

	wf1 := &store.WorkflowDef{
		Name:        "wf-a",
		Concurrency: &store.ConcurrencyDef{Group: "group-a", CancelInProgress: true},
		Jobs: map[string]*store.JobDef{
			"build": {Steps: []store.StepDef{{Run: "sleep 999"}}},
		},
	}
	workflow1, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", wf1, "alpine:latest")
	if err != nil {
		t.Fatalf("submit wf-a: %v", err)
	}

	// A workflow in group B must not affect group A.
	wf2 := &store.WorkflowDef{
		Name:        "wf-b",
		Concurrency: &store.ConcurrencyDef{Group: "group-b", CancelInProgress: true},
		Jobs: map[string]*store.JobDef{
			"build": {Steps: []store.StepDef{{Run: "echo build"}}},
		},
	}
	_, err = s.actions.SubmitWorkflow(context.Background(), "http://localhost", wf2, "alpine:latest")
	if err != nil {
		t.Fatalf("submit wf-b: %v", err)
	}

	if workflow1.Status != "running" {
		t.Errorf("wf-a status = %q, want running (different group)", workflow1.Status)
	}
}

func TestConcurrencyQueueWhenNotCancel(t *testing.T) {
	s := newTestServer()
	s.metrics = NewMetrics()

	wf1 := &store.WorkflowDef{
		Name:        "wf1",
		Concurrency: &store.ConcurrencyDef{Group: "serial", CancelInProgress: false},
		Jobs: map[string]*store.JobDef{
			"build": {Steps: []store.StepDef{{Run: "sleep 999"}}},
		},
	}
	workflow1, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", wf1, "alpine:latest")
	if err != nil {
		t.Fatalf("submit wf1: %v", err)
	}
	workflow1.Env = map[string]string{"__serverURL": "http://localhost", "__defaultImage": "alpine:latest"}

	wf2 := &store.WorkflowDef{
		Name:        "wf2",
		Concurrency: &store.ConcurrencyDef{Group: "serial", CancelInProgress: false},
		Jobs: map[string]*store.JobDef{
			"build": {Steps: []store.StepDef{{Run: "echo build"}}},
		},
		Env: map[string]string{"__serverURL": "http://localhost", "__defaultImage": "alpine:latest"},
	}
	workflow2, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", wf2, "alpine:latest")
	if err != nil {
		t.Fatalf("submit wf2: %v", err)
	}

	if workflow1.Status != "running" {
		t.Errorf("wf1 = %q, want running", workflow1.Status)
	}
	if workflow2.Status != "pending_concurrency" {
		t.Errorf("wf2 = %q, want pending_concurrency", workflow2.Status)
	}

	for _, j := range workflow1.Jobs {
		s.actions.OnJobCompleted(context.Background(), j.JobID, "Succeeded")
	}

	if workflow2.Status != "running" {
		t.Errorf("wf2 after wf1 done = %q, want running", workflow2.Status)
	}
}

func TestNoConcurrency(t *testing.T) {
	yaml := `
name: no-concurrency
jobs:
  build:
    runs-on: self-hosted
    steps:
      - run: echo build
`
	wf, err := store.ParseWorkflow([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if wf.Concurrency != nil {
		t.Error("concurrency should be nil when not specified")
	}
}
