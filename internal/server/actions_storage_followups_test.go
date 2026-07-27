package bleephub

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkflowRunNumbersArePerWorkflowAndRerunsReuseNumber(t *testing.T) {
	s := newTestServer()
	definition := func(name string) *WorkflowDef {
		return &WorkflowDef{Name: name, Jobs: map[string]*JobDef{
			"build": {Steps: []StepDef{{Run: "echo ok"}}},
		}}
	}
	first, err := s.submitWorkflow(t.Context(), "http://localhost", definition("ci"), "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.submitWorkflow(t.Context(), "http://localhost", definition("ci"), "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.submitWorkflow(t.Context(), "http://localhost", definition("deploy"), "")
	if err != nil {
		t.Fatal(err)
	}
	if first.RunNumber != 1 || second.RunNumber != 2 || other.RunNumber != 1 {
		t.Fatalf("run numbers ci=%d,%d deploy=%d, want 1,2,1", first.RunNumber, second.RunNumber, other.RunNumber)
	}
	rerun, err := s.submitWorkflow(t.Context(), "http://localhost", definition("ci"), "", &WorkflowEventMeta{
		ReuseRunID: first.RunID, ReuseRunNumber: first.RunNumber, Attempt: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rerun.RunID != first.RunID || rerun.RunNumber != first.RunNumber || rerun.AttemptNumber() != 2 {
		t.Fatalf("rerun identity = id:%d number:%d attempt:%d", rerun.RunID, rerun.RunNumber, rerun.AttemptNumber())
	}
}

func TestJobMessageCarriesStepExecutionOptionsAndTypedMatrix(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	admin := s.store.LookupUserByLogin("admin")
	if s.store.GetRepo(admin.Login, "step-options") == nil {
		s.store.CreateRepo(admin, "step-options", "", false)
	}
	def := &JobDef{Steps: []StepDef{{
		Name:             "configured",
		Run:              "echo ok",
		If:               "github.ref == 'refs/heads/main'",
		Env:              map[string]string{"MODE": "test"},
		Shell:            "bash",
		WorkingDirectory: "src",
		ContinueOnError:  true,
		TimeoutMinutes:   5,
	}}}
	wf := &Workflow{
		ID: "run", Name: "ci", RunID: 1, RunNumber: 1, RepoFullName: "admin/step-options",
		Ref: "refs/heads/main", Jobs: map[string]*WorkflowJob{},
	}
	job := &WorkflowJob{
		Key: "build", JobID: "job", DisplayName: "build", Def: def,
		MatrixValues: map[string]interface{}{"attempt": 2, "experimental": true},
	}
	wf.Jobs[job.Key] = job
	message, err := s.buildJobMessageFromDef("http://localhost", wf, job, "plan", "timeline", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	steps := message["steps"].([]map[string]interface{})
	step := steps[0]
	if step["condition"] != "success() && (github.ref == 'refs/heads/main')" {
		t.Fatalf("condition = %q", step["condition"])
	}
	if step["environment"] == nil || step["continueOnError"] == nil || step["timeoutInMinutes"] == nil {
		t.Fatalf("step options missing from runner message: %#v", step)
	}
	inputs := step["inputs"].(map[string]interface{})["map"].([]interface{})
	inputNames := map[string]bool{}
	for _, raw := range inputs {
		entry := raw.(map[string]interface{})
		key := entry["Key"].(map[string]interface{})["lit"].(string)
		inputNames[key] = true
	}
	if !inputNames["script"] || !inputNames["shell"] || !inputNames["workingDirectory"] {
		t.Fatalf("script inputs = %v", inputNames)
	}
	contextData := message["contextData"].(map[string]interface{})
	matrix := contextData["matrix"].(map[string]interface{})
	entries := matrix["d"].([]map[string]interface{})
	types := map[string]interface{}{}
	for _, entry := range entries {
		types[entry["k"].(string)] = entry["v"].(map[string]interface{})["t"]
	}
	if types["attempt"] != 4 || types["experimental"] != 3 {
		t.Fatalf("matrix PipelineContextData types = %#v, want number=4 bool=3", types)
	}
}

func TestTimelineSummaryPersistsOnWorkflowJob(t *testing.T) {
	s := newTestServer()
	wf := &Workflow{ID: "summary-run", Jobs: map[string]*WorkflowJob{
		"build": {JobID: "job", PlanID: "plan", Summary: ""},
	}}
	s.store.mu.Lock()
	s.store.Workflows[wf.ID] = wf
	s.store.mu.Unlock()

	req := httptest.NewRequest(http.MethodPut, "/summary", bytes.NewBufferString("## Results\n\nAll green"))
	req.SetPathValue("planId", "plan")
	req.SetPathValue("attachType", "Distributedtask.Core.Summary")
	req.SetPathValue("name", "summary.md")
	response := httptest.NewRecorder()
	s.handleTimelineAttachment(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("summary upload status = %d: %s", response.Code, response.Body.String())
	}
	if got := wf.Jobs["build"].Summary; got != "## Results\n\nAll green" {
		t.Fatalf("stored summary = %q", got)
	}
}

func TestConcurrencyKeepsOnlyNewestPendingRun(t *testing.T) {
	s := newTestServer()
	makeDef := func(name string) *WorkflowDef {
		return &WorkflowDef{
			Name: name, Concurrency: &ConcurrencyDef{Group: "deploy"},
			Env:  map[string]string{"__serverURL": "http://localhost"},
			Jobs: map[string]*JobDef{"build": {Steps: []StepDef{{Run: "echo ok"}}}},
		}
	}
	holder, err := s.submitWorkflow(context.Background(), "http://localhost", makeDef("holder"), "")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := s.submitWorkflow(context.Background(), "http://localhost", makeDef("stale"), "")
	if err != nil {
		t.Fatal(err)
	}
	newest, err := s.submitWorkflow(context.Background(), "http://localhost", makeDef("newest"), "")
	if err != nil {
		t.Fatal(err)
	}
	if stale.Status != WorkflowStatusCompleted || stale.Result != ResultCancelled {
		t.Fatalf("stale pending run = %s/%s, want completed/cancelled", stale.Status, stale.Result)
	}
	if newest.Status != WorkflowStatusPendingConcurrency {
		t.Fatalf("newest run = %s, want pending", newest.Status)
	}
	for _, job := range holder.Jobs {
		s.onJobCompleted(context.Background(), job.JobID, "Succeeded")
	}
	if newest.Status != WorkflowStatusRunning {
		t.Fatalf("newest run after holder completed = %s, want running", newest.Status)
	}
}
