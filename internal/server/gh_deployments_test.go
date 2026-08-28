package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(s *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	return w
}

func TestEnvironments_Pagination(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHDeploymentsRoutes()

	user := s.store.UsersByLogin["admin"]
	_ = s.store.CreateRepo(user, "env-page-repo", "", false)

	for _, name := range []string{"staging", "production"} {
		w := do(s, "PUT", "/api/v3/repos/admin/env-page-repo/environments/"+name, []byte("{}"))
		if w.Code != http.StatusOK {
			t.Fatalf("put env %s: %d", name, w.Code)
		}
	}

	w := do(s, "GET", "/api/v3/repos/admin/env-page-repo/environments?per_page=1", nil)
	var page1 map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if page1["total_count"].(float64) != 2 {
		t.Fatalf("page 1 total_count = %v, want 2", page1["total_count"])
	}
	envs1, _ := page1["environments"].([]any)
	if len(envs1) != 1 {
		t.Fatalf("page 1 envs = %d, want 1", len(envs1))
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	w = do(s, "GET", "/api/v3/repos/admin/env-page-repo/environments?per_page=1&page=2", nil)
	var page2 map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &page2)
	if page2["total_count"].(float64) != 2 {
		t.Fatalf("page 2 total_count = %v, want 2", page2["total_count"])
	}
	envs2, _ := page2["environments"].([]any)
	if len(envs2) != 1 {
		t.Fatalf("page 2 envs = %d, want 1", len(envs2))
	}
	name1 := envs1[0].(map[string]any)["name"]
	name2 := envs2[0].(map[string]any)["name"]
	if name1 == name2 {
		t.Fatalf("page 1 and page 2 returned the same environment: %v", name1)
	}
}

func TestDeployments_Lifecycle(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHDeploymentsRoutes()

	user := s.store.UsersByLogin["admin"]
	_ = s.store.CreateRepo(user, "dep-repo", "", false)

	body, _ := json.Marshal(map[string]any{
		"ref":                    "main",
		"environment":            "staging",
		"description":            "smoke deploy",
		"production_environment": false,
	})
	w := do(s, "POST", "/api/v3/repos/admin/dep-repo/deployments", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	depID := int(created["id"].(float64))
	if created["environment"] != "staging" {
		t.Errorf("env = %v", created["environment"])
	}

	w = do(s, "GET", "/api/v3/repos/admin/dep-repo/deployments", nil)
	var list []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Errorf("list len = %d", len(list))
	}

	for _, state := range []string{"pending", "in_progress", "success"} {
		statusBody, _ := json.Marshal(map[string]any{"state": state, "description": state + " step"})
		w = do(s, "POST", "/api/v3/repos/admin/dep-repo/deployments/"+itoa(depID)+"/statuses", statusBody)
		if w.Code != http.StatusCreated {
			t.Errorf("status %s: %d body=%s", state, w.Code, w.Body.String())
		}
	}

	w = do(s, "GET", "/api/v3/repos/admin/dep-repo/deployments/"+itoa(depID)+"/statuses", nil)
	var statusList []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &statusList)
	if len(statusList) != 3 {
		t.Errorf("statuses len = %d", len(statusList))
	}

	// Creating the deployment auto-created its environment.
	w = do(s, "GET", "/api/v3/repos/admin/dep-repo/environments", nil)
	var envs map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &envs)
	if envs["total_count"].(float64) < 1 {
		t.Errorf("env count = %v", envs["total_count"])
	}

	w = do(s, "PUT", "/api/v3/repos/admin/dep-repo/environments/production", []byte("{}"))
	if w.Code != http.StatusOK {
		t.Fatalf("put env: %d", w.Code)
	}
	w = do(s, "GET", "/api/v3/repos/admin/dep-repo/environments/production", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get env: %d", w.Code)
	}

	w = do(s, "DELETE", "/api/v3/repos/admin/dep-repo/environments/production", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("del env: %d", w.Code)
	}

	w = do(s, "DELETE", "/api/v3/repos/admin/dep-repo/deployments/"+itoa(depID), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("del dep: %d", w.Code)
	}
}

// TestUpsertEnvironmentBodyValidation: PUT /environments/{name} accepts an
// absent body (environment with no protection config) but rejects malformed
// JSON with 400 like real GitHub.
func TestUpsertEnvironmentBodyValidation(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHDeploymentsRoutes()

	user := s.store.UsersByLogin["admin"]
	_ = s.store.CreateRepo(user, "env-body-repo", "", false)

	w := do(s, "PUT", "/api/v3/repos/admin/env-body-repo/environments/staging", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("put env with no body: %d body=%s", w.Code, w.Body.String())
	}

	w = do(s, "PUT", "/api/v3/repos/admin/env-body-repo/environments/staging", []byte(`{"wait_timer": `))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("put env with malformed JSON: %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestRepositoryDispatch(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHActionsExtrasRoutes()

	user := s.store.UsersByLogin["admin"]
	_ = s.store.CreateRepo(user, "dispatch-repo", "", false)

	body, _ := json.Marshal(map[string]any{
		"event_type":     "deploy",
		"client_payload": map[string]any{"version": "1.2.3"},
	})
	w := do(s, "POST", "/api/v3/repos/admin/dispatch-repo/dispatches", body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("dispatch: %d body=%s", w.Code, w.Body.String())
	}

	bad, _ := json.Marshal(map[string]any{})
	w = do(s, "POST", "/api/v3/repos/admin/dispatch-repo/dispatches", bad)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing event_type: %d", w.Code)
	}
}
