package bleephub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// authedReqScheme drives a request through the /api middleware with the given
// raw Authorization header value, returning the recorder.
func authedReqScheme(s *Server, method, path, authHeader, body string) *httptest.ResponseRecorder {
	return serveTestRequest(s, authHeader, method, path, jsonBodyBytes(body))
}

// TestAuthScheme_CaseInsensitive guards octokit-style lowercase auth schemes:
// HTTP auth schemes are case-insensitive (RFC 7235), so "bearer"/"TOKEN" must
// authenticate exactly like "Bearer"/"token". The bug surfaced as `gh`/octokit
// GraphQL calls failing with "authentication required" on `Authorization:
// bearer …`.
func TestAuthScheme_CaseInsensitive(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	for _, h := range []string{
		"token " + defaultToken,
		"Bearer " + defaultToken,
		"bearer " + defaultToken,
		"TOKEN " + defaultToken,
		"BEARER " + defaultToken,
	} {
		w := authedReqScheme(s, "GET", "/api/v3/user", h, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET /user with %q = %d, want 200 (scheme must be case-insensitive)", h, w.Code)
		}
	}
	// A bogus token is still rejected regardless of scheme case.
	if w := authedReqScheme(s, "GET", "/api/v3/user", "bearer not-a-token", ""); w.Code == http.StatusOK {
		t.Error("GET /user with a bogus token resolved a user")
	}
}

// TestAddIssueLabels_AcceptsBothBodyForms guards the fix for go-github /
// real-GitHub sending the add-labels body as a bare array. The endpoint must
// accept BOTH ["triage"] and {"labels":["triage"]}.
func TestAddIssueLabels_AcceptsBothBodyForms(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "labels-repo", "", false)
	s.store.CreateLabel(repo.ID, "triage", "", "ff0000")
	issue := s.store.CreateIssue(repo.ID, admin.ID, "needs label", "", nil, nil, 0)

	path := "/api/v3/repos/admin/labels-repo/issues/" + itoa(issue.Number) + "/labels"
	auth := "token " + defaultToken

	if w := authedReqScheme(s, "POST", path, auth, `["triage"]`); w.Code != http.StatusOK {
		t.Errorf("bare-array body = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w := authedReqScheme(s, "POST", path, auth, `{"labels":["triage"]}`); w.Code != http.StatusOK {
		t.Errorf("object body = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
