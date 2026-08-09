package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// Real GitHub returns 400 with the body `{"message":"Problems parsing JSON",...}`
// when a write endpoint receives malformed JSON. Earlier bleephub silently
// dropped decode errors and ack'd the request as success, so a runner shipping
// a broken body would never learn its mistake. These tests pin the strict-decode
// contract on every misc-endpoint write surface.

const adminPAT = "bleephub-admin-token-00000000000000000000"

func miscEndpointsTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHMiscEndpoints()
	s.registerGHBranchProtectionRoutes()
	s.registerGHIssueRoutes()
	return s
}

func doMiscReq(s *Server, method, path string, body string) *httptest.ResponseRecorder {
	return serveTestRequest(s, "token "+adminPAT, method, path, jsonBodyBytes(body))
}

func assertProblemsParsingJSON(t *testing.T, w *httptest.ResponseRecorder, surface string) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("%s malformed-JSON status = %d, want 400; body = %s", surface, w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%s response not JSON: %v", surface, err)
	}
	msg, _ := got["message"].(string)
	if !strings.Contains(msg, "Problems parsing JSON") {
		t.Errorf("%s message = %q, want \"Problems parsing JSON\"", surface, msg)
	}
}

func TestOIDCCustomSubPut_RejectsMalformedJSON(t *testing.T) {
	s := miscEndpointsTestServer(t)
	ensureSeededRepo(s, "admin/oidc-repo")
	w := doMiscReq(s, "PUT", "/api/v3/repos/admin/oidc-repo/actions/oidc/customization/sub", `{"include_claim_keys":`)
	assertProblemsParsingJSON(t, w, "OIDC custom-sub PUT")
}

func TestOIDCCustomSubPut_EmptyBodyOK(t *testing.T) {
	// Empty body has been historical bleephub behaviour and matches the laxer
	// "missing field defaults to []" reading; the strict-decode fix should
	// only reject *malformed* JSON, not absent bodies.
	s := miscEndpointsTestServer(t)
	ensureSeededRepo(s, "admin/oidc-repo")
	w := doMiscReq(s, "PUT", "/api/v3/repos/admin/oidc-repo/actions/oidc/customization/sub", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("empty-body status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
}

// TestOIDCCustomSubIsScopedPerRepository pins that OIDC subject
// customization is stored per repository, so one repo's admin cannot clobber or
// read another repository's include_claim_keys.
func TestOIDCCustomSubIsScopedPerRepository(t *testing.T) {
	s := miscEndpointsTestServer(t)
	ensureSeededRepo(s, "admin/repo-a")
	ensureSeededRepo(s, "admin/repo-b")

	if w := doMiscReq(s, "PUT", "/api/v3/repos/admin/repo-a/actions/oidc/customization/sub", `{"include_claim_keys":["repo","ref"]}`); w.Code != http.StatusCreated {
		t.Fatalf("PUT repo-a = %d %s", w.Code, w.Body.String())
	}
	if w := doMiscReq(s, "PUT", "/api/v3/repos/admin/repo-b/actions/oidc/customization/sub", `{"include_claim_keys":["environment"]}`); w.Code != http.StatusCreated {
		t.Fatalf("PUT repo-b = %d %s", w.Code, w.Body.String())
	}

	get := func(repo string) []interface{} {
		w := doMiscReq(s, "GET", "/api/v3/repos/admin/"+repo+"/actions/oidc/customization/sub", "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", repo, w.Code, w.Body.String())
		}
		var body map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		keys, _ := body["include_claim_keys"].([]interface{})
		return keys
	}
	if a := get("repo-a"); len(a) != 2 {
		t.Fatalf("repo-a keys = %v, want 2 (its own)", a)
	}
	if b := get("repo-b"); len(b) != 1 || b[0] != "environment" {
		t.Fatalf("repo-b keys = %v, want [environment] — repo-a must not have clobbered it", b)
	}
}

func TestPagesCreate_RejectsMalformedJSON(t *testing.T) {
	s := miscEndpointsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "pages-malformed", "", false)
	w := doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/pages", `{"source": {`)
	assertProblemsParsingJSON(t, w, "Pages create")
}

func TestBranchProtectionPut_RejectsMalformedJSON(t *testing.T) {
	s := miscEndpointsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "bp-malformed", "", false)
	w := doMiscReq(s, "PUT", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", `{"required_status_checks":`)
	assertProblemsParsingJSON(t, w, "branch protection PUT")
}

func TestIssueLock_RejectsMalformedJSON(t *testing.T) {
	s := miscEndpointsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "lock-malformed", "", false)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "title", "body", nil, nil, 0)
	w := doMiscReq(s, "PUT", "/api/v3/repos/"+repo.FullName+"/issues/"+strconv.Itoa(issue.Number)+"/lock", `{"lock_reason":`)
	assertProblemsParsingJSON(t, w, "issue lock")
}

func TestIssueLock_EmptyBodyOK(t *testing.T) {
	// Real GH accepts `gh issue lock` with no body (lock with no reason). The
	// strict-decode fix must preserve that path: io.EOF on Decode is not an
	// error, only a malformed body is.
	s := miscEndpointsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "lock-empty", "", false)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "title", "body", nil, nil, 0)
	w := doMiscReq(s, "PUT", "/api/v3/repos/"+repo.FullName+"/issues/"+strconv.Itoa(issue.Number)+"/lock", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("empty-body lock status = %d, want 204; body = %s", w.Code, w.Body.String())
	}
}
