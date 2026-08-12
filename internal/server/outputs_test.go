package bleephub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

func TestFinishJobCapturesOfficialRunnerOutputsBeforeCompletion(t *testing.T) {
	s := newTestServer()
	s.registerRunServiceRoutes()
	jobID := uuid.New().String()
	planID := uuid.New().String()
	wf := &store.Workflow{
		ID:           uuid.New().String(),
		Name:         "outputs",
		RunID:        42,
		Status:       store.WorkflowStatusRunning,
		RepoFullName: "admin/test",
		Jobs: map[string]*store.WorkflowJob{
			"build": {
				Key: "build", JobID: jobID, PlanID: planID, Status: store.JobStatusRunning,
				Outputs: map[string]string{}, Def: &store.JobDef{},
			},
		},
	}
	s.store.Workflows[wf.ID] = wf
	// FinishJob is gated on the runtime token of the job whose plan it names,
	// so the job needs the dispatched message that token is minted against.
	scopeID := "scope-" + planID
	s.store.Jobs[jobID] = &store.Job{ID: jobID, PlanID: planID, Status: "running", Message: fmt.Sprintf(
		`{"plan":{"scopeIdentifier":%q,"planId":%q},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":"admin/test"}]}}}`,
		scopeID, planID)}

	// Captured from actions/runner v2.321.0's JobCompletedEvent contract:
	// JobServer.RaisePlanEventAsync POSTs this body to the advertised
	// FinishJob service-location route.
	body := fmt.Sprintf(`{"name":"JobCompleted","jobId":%q,"requestId":17,"result":"succeeded","outputs":{"version":{"value":"1.2.3","isSecret":false}}}`, jobID)
	req := httptest.NewRequest(http.MethodPost, "/_apis/v1/FinishJob/"+scopeID+"/free/"+planID, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+makeJWT(scopeID, runnerAudJob))
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
	if job.Status != store.JobStatusCompleted || job.Result != store.ResultSuccess {
		t.Fatalf("job state = %s/%s, want completed/success", job.Status, job.Result)
	}
}

func TestFinishJobRejectsMismatchedJobAndPlan(t *testing.T) {
	s := newTestServer()
	s.registerRunServiceRoutes()
	jobID := uuid.New().String()
	planID := uuid.New().String()
	scopeID := "scope-" + planID
	s.store.Jobs[jobID] = &store.Job{ID: jobID, PlanID: planID, Status: "running", Message: fmt.Sprintf(
		`{"plan":{"scopeIdentifier":%q,"planId":%q},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":"admin/test"}]}}}`,
		scopeID, planID)}

	body := fmt.Sprintf(`{"name":"JobCompleted","jobId":%q,"result":"succeeded","outputs":{"version":{"value":"attacker-controlled"}}}`, uuid.New().String())
	req := httptest.NewRequest(http.MethodPost, "/_apis/v1/FinishJob/"+scopeID+"/free/"+planID, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+makeJWT(scopeID, runnerAudJob))
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
