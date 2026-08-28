package actions

import "testing"

// TestWorkflowRunWorkflowsFilter pins that `on.workflow_run.workflows` fires only for runs of the named workflows.
func TestWorkflowRunWorkflowsFilter(t *testing.T) {
	on, err := ParseWorkflowOn([]byte(`
on:
  workflow_run:
    workflows: ["CI", "Lint"]
    types: [completed]
jobs: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	td := on["workflow_run"]
	if td == nil {
		t.Fatal("workflow_run trigger not parsed")
	}
	if len(td.Workflows) != 2 || td.Workflows[0] != "CI" || td.Workflows[1] != "Lint" {
		t.Fatalf("parsed workflows filter = %#v, want [CI Lint]", td.Workflows)
	}

	cases := []struct {
		workflow string
		want     bool
	}{
		{"CI", true},
		{"Lint", true},
		{"Release", false},
		{"", false},
	}
	for _, tc := range cases {
		ev := TriggerEvent{Type: "workflow_run", Action: "completed", WorkflowName: tc.workflow}
		if got := WorkflowTriggersOn(on, ev); got != tc.want {
			t.Errorf("workflow_run from %q: triggers = %v, want %v", tc.workflow, got, tc.want)
		}
	}
}

// TestWorkflowRunWithoutWorkflowsFilterMatchesAny pins that an unfiltered `workflow_run` listens to every run.
func TestWorkflowRunWithoutWorkflowsFilterMatchesAny(t *testing.T) {
	on, err := ParseWorkflowOn([]byte("on:\n  workflow_run:\n    types: [completed]\njobs: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	ev := TriggerEvent{Type: "workflow_run", Action: "completed", WorkflowName: "anything"}
	if !WorkflowTriggersOn(on, ev) {
		t.Error("unfiltered workflow_run must match any source workflow")
	}
}
