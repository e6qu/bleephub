package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/stretchr/testify/require"
)

func bpTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	s.store.Tokens[adminPAT] = &store.Token{Value: adminPAT, UserID: admin.ID, Scopes: "repo,admin:org"}
	return s
}

func doBPReq(s *Server, token, method, path, body string) *httptest.ResponseRecorder {
	return serveTestRequest(s, "Bearer "+token, method, path, jsonBodyBytes(body))
}

func TestBranchProtection_CRUD(t *testing.T) {
	s := bpTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "bp-crud", "", false)

	w := doBPReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", "")
	require.Equal(t, http.StatusNotFound, w.Code)

	body := `{
		"required_status_checks": {"strict": true, "contexts": ["ci"]},
		"required_pull_request_reviews": {"required_approving_review_count": 2},
		"enforce_admins": true,
		"allow_force_pushes": true,
		"allow_deletions": false
	}`
	w = doBPReq(s, adminPAT, "PUT", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", body)
	require.Equal(t, http.StatusOK, w.Code)

	var bp store.BranchProtection
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bp))
	require.NotNil(t, bp.RequiredStatusChecks)
	require.True(t, bp.RequiredStatusChecks.Strict)
	require.Equal(t, []string{"ci"}, bp.RequiredStatusChecks.Contexts)
	require.NotNil(t, bp.RequiredPullRequestReviews)
	require.Equal(t, 2, bp.RequiredPullRequestReviews.RequiredApprovingReviewCount)
	require.NotNil(t, bp.EnforceAdmins)
	require.True(t, bp.EnforceAdmins.Enabled)
	require.NotNil(t, bp.AllowForcePushes)
	require.True(t, bp.AllowForcePushes.Enabled)
	require.NotNil(t, bp.AllowDeletions)
	require.False(t, bp.AllowDeletions.Enabled)

	w = doBPReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bp))
	require.Equal(t, []string{"ci"}, bp.RequiredStatusChecks.Contexts)

	w = doBPReq(s, adminPAT, "DELETE", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	w = doBPReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestBranchProtection_RequiredStatusChecksSubresource(t *testing.T) {
	s := bpTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "bp-rsc", "", false)

	doBPReq(s, adminPAT, "PUT", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", `{"required_status_checks":{"strict":false,"contexts":["bootstrap"]}}`)

	w := doBPReq(s, adminPAT, "PATCH", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_status_checks", `{"strict": true, "contexts": ["ci", "lint"]}`)
	require.Equal(t, http.StatusOK, w.Code)
	var sc store.BPStatusChecks
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sc))
	require.True(t, sc.Strict)
	require.Equal(t, []string{"ci", "lint"}, sc.Contexts)

	w = doBPReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_status_checks", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sc))
	require.Equal(t, []string{"ci", "lint"}, sc.Contexts)

	w = doBPReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_status_checks/contexts", "")
	require.Equal(t, http.StatusOK, w.Code)
	var contexts []string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &contexts))
	require.Equal(t, []string{"ci", "lint"}, contexts)

	w = doBPReq(s, adminPAT, "POST", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_status_checks/contexts", `["build"]`)
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &contexts))
	require.Equal(t, []string{"ci", "lint", "build"}, contexts)

	w = doBPReq(s, adminPAT, "DELETE", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_status_checks/contexts", `{"contexts":["lint"]}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &contexts))
	require.Equal(t, []string{"ci", "build"}, contexts)

	w = doBPReq(s, adminPAT, "DELETE", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_status_checks", "")
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestBranchProtection_RequiredReviewsSubresource(t *testing.T) {
	s := bpTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "bp-reviews", "", false)

	doBPReq(s, adminPAT, "PUT", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", `{"enforce_admins":false}`)

	w := doBPReq(s, adminPAT, "PATCH", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_pull_request_reviews", `{"required_approving_review_count": 1, "dismiss_stale_reviews": true}`)
	require.Equal(t, http.StatusOK, w.Code)
	var rev store.BPPullRequestReviews
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rev))
	require.Equal(t, 1, rev.RequiredApprovingReviewCount)
	require.True(t, rev.DismissStaleReviews)

	w = doBPReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_pull_request_reviews", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rev))
	require.Equal(t, 1, rev.RequiredApprovingReviewCount)
}

func TestBranchProtection_EnforceAdminsSubresource(t *testing.T) {
	s := bpTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "bp-admins", "", false)

	doBPReq(s, adminPAT, "PUT", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", `{"enforce_admins":false}`)

	w := doBPReq(s, adminPAT, "POST", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/enforce_admins", ``)
	require.Equal(t, http.StatusOK, w.Code)
	var ea store.BPEnforceAdmins
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ea))
	require.True(t, ea.Enabled)

	w = doBPReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/enforce_admins", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ea))
	require.True(t, ea.Enabled)

	w = doBPReq(s, adminPAT, "DELETE", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/enforce_admins", "")
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestBranchProtection_RestrictionsSubresource(t *testing.T) {
	s := bpTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "bp-restrict", "", false)

	doBPReq(s, adminPAT, "PUT", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", `{"enforce_admins":false}`)

	body := `{"restrictions":{"users":[{"login":"admin","id":1,"type":"User"}]}}`
	w := doBPReq(s, adminPAT, "PUT", "/api/v3/repos/"+repo.FullName+"/branches/main/protection", body)
	require.Equal(t, http.StatusOK, w.Code)
	var protection store.BranchProtection
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &protection))
	require.NotNil(t, protection.Restrictions)
	require.Len(t, protection.Restrictions.Users, 1)
	require.Equal(t, "admin", protection.Restrictions.Users[0].Login)

	w = doBPReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/restrictions/users", "")
	require.Equal(t, http.StatusOK, w.Code)
	var users []store.BPActor
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &users))
	require.Len(t, users, 1)

	w = doBPReq(s, adminPAT, "DELETE", "/api/v3/repos/"+repo.FullName+"/branches/main/protection/restrictions", "")
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestBranchProtection_MergeEnforcesRequiredReviews(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "bp-merge-reviews")
	defer func() { s.delete(t, "/api/v3/repos/admin/bp-merge-reviews", defaultToken).Body.Close() }()

	s.put(t, "/api/v3/repos/admin/bp-merge-reviews/branches/main/protection", defaultToken, map[string]interface{}{
		"required_pull_request_reviews": map[string]interface{}{"required_approving_review_count": 1},
		"enforce_admins":                true,
	}).Body.Close()

	s.post(t, "/api/v3/repos/admin/bp-merge-reviews/pulls", defaultToken, map[string]interface{}{
		"title": "To merge", "head": "feat", "base": "main",
	}).Body.Close()

	resp := s.put(t, "/api/v3/repos/admin/bp-merge-reviews/pulls/1/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp.Body.Close()

	resp = s.post(t, "/api/v3/repos/admin/bp-merge-reviews/pulls/1/reviews", defaultToken, map[string]interface{}{
		"body": "LGTM", "event": "APPROVE",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = s.put(t, "/api/v3/repos/admin/bp-merge-reviews/pulls/1/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestBranchProtection_MergeEnforcesRequestedChanges(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "bp-merge-changes")
	defer func() { ghDelete(t, "/api/v3/repos/admin/bp-merge-changes", defaultToken).Body.Close() }()

	s.put(t, "/api/v3/repos/admin/bp-merge-changes/branches/main/protection", defaultToken, map[string]interface{}{
		"enforce_admins": true,
	}).Body.Close()

	s.post(t, "/api/v3/repos/admin/bp-merge-changes/pulls", defaultToken, map[string]interface{}{
		"title": "To merge", "head": "feat", "base": "main",
	}).Body.Close()

	reviewer := &store.User{ID: 1000, Login: "reviewer", Type: "User", CreatedAt: fixedTestTime, UpdatedAt: fixedTestTime}
	s.store.Users[reviewer.ID] = reviewer
	s.store.UsersByLogin[reviewer.Login] = reviewer
	tok := s.store.CreateToken(reviewer.ID, "repo")

	resp := s.post(t, "/api/v3/repos/admin/bp-merge-changes/pulls/1/reviews", tok.Value, map[string]interface{}{
		"body": "nope", "event": "REQUEST_CHANGES",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = s.put(t, "/api/v3/repos/admin/bp-merge-changes/pulls/1/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp.Body.Close()
}
