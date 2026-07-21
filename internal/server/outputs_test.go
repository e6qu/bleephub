package bleephub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestJobOutputsTokenUsesRunnerMappingShape(t *testing.T) {
	got := jobOutputsToken(map[string]string{
		"version": "${{ steps.ver.outputs.version }}",
		"channel": "stable",
	})
	want := mappingToken([]interface{}{
		mappingEntry("channel", templateToken("stable")),
		mappingEntry("version", templateToken("${{ steps.ver.outputs.version }}")),
	})
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal job outputs token: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal expected job outputs token: %v", err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("jobOutputs = %s, want %s", gotJSON, wantJSON)
	}
	if got := jobOutputsToken(nil); got != nil {
		t.Fatalf("empty jobOutputs = %#v, want nil", got)
	}
}

func TestFinishJobCapturesOfficialRunnerOutputsBeforeCompletion(t *testing.T) {
	s := newTestServer()
	s.registerRunServiceRoutes()
	jobID := uuid.New().String()
	planID := uuid.New().String()
	wf := &Workflow{
		ID:           uuid.New().String(),
		Name:         "outputs",
		RunID:        42,
		Status:       WorkflowStatusRunning,
		RepoFullName: "admin/test",
		Jobs: map[string]*WorkflowJob{
			"build": {
				Key: "build", JobID: jobID, PlanID: planID, Status: JobStatusRunning,
				Outputs: map[string]string{}, Def: &JobDef{},
			},
		},
	}
	s.store.Workflows[wf.ID] = wf
	s.store.Jobs[jobID] = &Job{ID: jobID, PlanID: planID, Status: "running"}

	// Captured from actions/runner v2.321.0's JobCompletedEvent contract:
	// JobServer.RaisePlanEventAsync POSTs this body to the advertised
	// FinishJob service-location route.
	body := fmt.Sprintf(`{"name":"JobCompleted","jobId":%q,"requestId":17,"result":"succeeded","outputs":{"version":{"value":"1.2.3","isSecret":false}}}`, jobID)
	req := httptest.NewRequest(http.MethodPost, "/_apis/v1/FinishJob/scope/free/"+planID, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("JobCompleted event status = %d, body = %s", w.Code, w.Body.String())
	}
	job := wf.Jobs["build"]
	if got := job.Outputs["version"]; got != "1.2.3" {
		t.Fatalf("captured version = %q, want 1.2.3", got)
	}
	if got := s.store.Jobs[jobID].Result; got != "Succeeded" {
		t.Fatalf("broker job result = %q, want canonical Succeeded", got)
	}
	if job.Status != JobStatusCompleted || job.Result != ResultSuccess {
		t.Fatalf("job state = %s/%s, want completed/success", job.Status, job.Result)
	}
}

func TestFinishJobRejectsMismatchedJobAndPlan(t *testing.T) {
	s := newTestServer()
	s.registerRunServiceRoutes()
	jobID := uuid.New().String()
	planID := uuid.New().String()
	s.store.Jobs[jobID] = &Job{ID: jobID, PlanID: planID, Status: "running"}

	body := fmt.Sprintf(`{"name":"JobCompleted","jobId":%q,"result":"succeeded","outputs":{"version":{"value":"attacker-controlled"}}}`, uuid.New().String())
	req := httptest.NewRequest(http.MethodPost, "/_apis/v1/FinishJob/scope/free/"+planID, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("mismatched JobCompleted event status = %d, want 400", w.Code)
	}
	if s.store.Jobs[jobID].Status != "running" {
		t.Fatalf("mismatched event changed plan job status to %q", s.store.Jobs[jobID].Status)
	}
}

func TestRunnerJobResultRejectsUnknownText(t *testing.T) {
	if _, err := runnerJobResult(json.RawMessage(`"unknown"`)); err == nil {
		t.Fatal("unknown official-runner result was accepted")
	}
}
