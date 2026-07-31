package bleephub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Checks API parity — check-run + check-suite CRUD against the
// GitHub-compatible /repos/{}/check-runs surface.

func TestCheckRunLifecycle(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHRepoRoutes()
	s.registerGHChecksRoutes()

	user := s.store.UsersByLogin["admin"]
	app := s.store.CreateApp(user.ID, "Checks App", "", map[string]string{"checks": "write"}, nil)
	inst := s.store.CreateInstallation(app.ID, "User", user.ID, user.Login, map[string]string{"checks": "write"}, nil)
	tok := s.store.CreateInstallationToken(inst.ID, app.ID, map[string]string{"checks": "write"}, nil)

	s.store.CreateRepo(user, "checks-target", "", false)
	headSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	doReq := func(method, path string, body []byte) *httptest.ResponseRecorder {
		var bodyR *bytes.Reader
		if body != nil {
			bodyR = bytes.NewReader(body)
		}
		var req *http.Request
		if bodyR != nil {
			req = httptest.NewRequest(method, path, bodyR)
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}

	// CREATE check run
	body, _ := json.Marshal(map[string]any{
		"name":        "go test",
		"head_sha":    headSHA,
		"status":      "in_progress",
		"details_url": "https://example.test/run/1",
	})
	w := doReq("POST", "/api/v3/repos/admin/checks-target/check-runs", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	runID := int(created["id"].(float64))
	suiteID := created["check_suite"].(map[string]any)["id"]
	if suiteID.(float64) == 0 {
		t.Error("create did not associate a check_suite")
	}

	// GET check run
	w = doReq("GET", fmt.Sprintf("/api/v3/repos/admin/checks-target/check-runs/%d", runID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d body = %s", w.Code, w.Body.String())
	}

	// PATCH check run → completed
	completedAt := fixedTestTime
	body, _ = json.Marshal(map[string]any{
		"status":       "completed",
		"conclusion":   "success",
		"completed_at": completedAt,
		"output": map[string]any{
			"title":   "all green",
			"summary": "5 passed, 0 failed",
		},
	})
	w = doReq("PATCH", fmt.Sprintf("/api/v3/repos/admin/checks-target/check-runs/%d", runID), body)
	if w.Code != http.StatusOK {
		t.Fatalf("patch status = %d body = %s", w.Code, w.Body.String())
	}
	var patched map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &patched)
	if patched["status"] != "completed" || patched["conclusion"] != "success" {
		t.Errorf("patch did not update status/conclusion: %v / %v", patched["status"], patched["conclusion"])
	}

	// LIST by commit
	w = doReq("GET", fmt.Sprintf("/api/v3/repos/admin/checks-target/commits/%s/check-runs", headSHA), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list by commit status = %d body = %s", w.Code, w.Body.String())
	}
	var listResp struct {
		TotalCount int              `json:"total_count"`
		CheckRuns  []map[string]any `json:"check_runs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if listResp.TotalCount != 1 {
		t.Errorf("expected 1 check run, got %d", listResp.TotalCount)
	}

	// LIST suites by commit
	w = doReq("GET", fmt.Sprintf("/api/v3/repos/admin/checks-target/commits/%s/check-suites", headSHA), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list suites status = %d body = %s", w.Code, w.Body.String())
	}
}

func TestLatestCheckRunsFilter(t *testing.T) {
	runs := []*CheckRun{
		{ID: 1, SuiteID: 10},
		{ID: 2, SuiteID: 20},
		{ID: 3, SuiteID: 10},
	}
	latest := latestCheckRuns(runs)
	if len(latest) != 2 || latest[0].ID != 2 || latest[1].ID != 3 {
		t.Fatalf("latestCheckRuns = %#v, want run ids [2 3]", latest)
	}
}

func TestCheckRunRequiresChecksScope(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHRepoRoutes()
	s.registerGHChecksRoutes()

	user := s.store.UsersByLogin["admin"]
	// Installation has issues:write but NOT checks.
	app := s.store.CreateApp(user.ID, "Wrong Scope App", "", map[string]string{"issues": "write"}, nil)
	inst := s.store.CreateInstallation(app.ID, "User", user.ID, user.Login, map[string]string{"issues": "write"}, nil)
	tok := s.store.CreateInstallationToken(inst.ID, app.ID, map[string]string{"issues": "write"}, nil)

	s.store.CreateRepo(user, "scope-target", "", false)

	body, _ := json.Marshal(map[string]any{
		"name":     "t",
		"head_sha": "0000000000000000000000000000000000000000",
	})
	req := httptest.NewRequest("POST", "/api/v3/repos/admin/scope-target/check-runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without checks:write, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestCheckSuitePreferencesIncludeRepository(t *testing.T) {
	s := newTestServer()
	s.registerGHRepoRoutes()
	s.registerGHChecksRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "check-preferences", "", false)

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v3/repos/admin/check-preferences/check-suites/preferences",
		bytes.NewBufferString(`{"auto_trigger_checks":[]}`))
	req.Header.Set("Authorization", "Bearer "+AdminToken())
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update preferences = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Repository struct {
			ID       int    `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Repository.ID != repo.ID || response.Repository.FullName != repo.FullName {
		t.Fatalf("repository = %#v, want %d %q", response.Repository, repo.ID, repo.FullName)
	}
}

func setupChecksPaginationServer(t *testing.T) (*Server, func(method, path string, body []byte) *httptest.ResponseRecorder, string) {
	t.Helper()
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHRepoRoutes()
	s.registerGHChecksRoutes()

	user := s.store.UsersByLogin["admin"]
	app := s.store.CreateApp(user.ID, "Pagination App", "", map[string]string{"checks": "write"}, nil)
	inst := s.store.CreateInstallation(app.ID, "User", user.ID, user.Login, map[string]string{"checks": "write"}, nil)
	tok := s.store.CreateInstallationToken(inst.ID, app.ID, map[string]string{"checks": "write"}, nil)

	s.store.CreateRepo(user, "checks-pg", "", false)
	headSHA := "cafebabecafebabecafebabecafebabecafebabe"

	doReq := func(method, path string, body []byte) *httptest.ResponseRecorder {
		var req *http.Request
		if body != nil {
			req = httptest.NewRequest(method, path, bytes.NewReader(body))
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}
	return s, doReq, headSHA
}

func TestListCheckRunAnnotationsPagination(t *testing.T) {
	_, doReq, headSHA := setupChecksPaginationServer(t)

	body, _ := json.Marshal(map[string]any{
		"name":     "lint",
		"head_sha": headSHA,
		"output": map[string]any{
			"title":   "lint",
			"summary": "2 issues",
			"annotations": []map[string]any{
				{"path": "a.go", "start_line": 1, "end_line": 1, "annotation_level": "warning", "message": "first"},
				{"path": "b.go", "start_line": 2, "end_line": 2, "annotation_level": "failure", "message": "second"},
			},
		},
	})
	w := doReq("POST", "/api/v3/repos/admin/checks-pg/check-runs", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	runID := int(created["id"].(float64))

	w = doReq("GET", fmt.Sprintf("/api/v3/repos/admin/checks-pg/check-runs/%d/annotations?per_page=1", runID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("annotations page 1 status = %d body = %s", w.Code, w.Body.String())
	}
	var page1 []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1) != 1 {
		t.Fatalf("expected 1 annotation on page 1, got %d", len(page1))
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("expected rel=next in Link, got %s", link)
	}

	w = doReq("GET", fmt.Sprintf("/api/v3/repos/admin/checks-pg/check-runs/%d/annotations?per_page=1&page=2", runID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("annotations page 2 status = %d body = %s", w.Code, w.Body.String())
	}
	var page2 []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2) != 1 || page2[0]["message"] != "second" {
		t.Fatalf("expected second annotation on page 2, got %+v", page2)
	}
}

func TestListCheckSuitesForCommitPagination(t *testing.T) {
	s, doReq, headSHA := setupChecksPaginationServer(t)

	s.store.CreateCheckSuite("admin/checks-pg", "", headSHA, 1)
	s.store.CreateCheckSuite("admin/checks-pg", "", headSHA, 2)

	path := fmt.Sprintf("/api/v3/repos/admin/checks-pg/commits/%s/check-suites", headSHA)
	w := doReq("GET", path+"?per_page=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("suites page 1 status = %d body = %s", w.Code, w.Body.String())
	}
	var page1 struct {
		TotalCount  int              `json:"total_count"`
		CheckSuites []map[string]any `json:"check_suites"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if page1.TotalCount != 2 {
		t.Fatalf("expected total_count=2 on page 1, got %d", page1.TotalCount)
	}
	if len(page1.CheckSuites) != 1 {
		t.Fatalf("expected 1 check suite on page 1, got %d", len(page1.CheckSuites))
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("expected rel=next in Link, got %s", link)
	}

	w = doReq("GET", path+"?per_page=1&page=2", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("suites page 2 status = %d body = %s", w.Code, w.Body.String())
	}
	var page2 struct {
		TotalCount  int              `json:"total_count"`
		CheckSuites []map[string]any `json:"check_suites"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page2)
	if page2.TotalCount != 2 {
		t.Fatalf("expected total_count=2 on page 2, got %d", page2.TotalCount)
	}
	if len(page2.CheckSuites) != 1 {
		t.Fatalf("expected 1 check suite on page 2, got %d", len(page2.CheckSuites))
	}
	if page2.CheckSuites[0]["id"] == page1.CheckSuites[0]["id"] {
		t.Fatalf("expected distinct suites across pages, got %v twice", page1.CheckSuites[0]["id"])
	}
}
