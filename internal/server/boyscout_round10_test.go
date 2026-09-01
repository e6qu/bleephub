package bleephub

import (
	"fmt"
	"net/http"
	"testing"
)

// TestPullRequestAssigneesViaSharedEndpoint pins that assignees can be added to
// a pull request through the shared /issues/{n}/assignees endpoint (a PR is an
// issue on GitHub). It previously 404'd because the handler resolved only issues.
func TestPullRequestAssigneesViaSharedEndpoint(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "pr-assign")
	s.post(t, "/api/v3/repos/admin/pr-assign/pulls", defaultToken, map[string]interface{}{
		"title": "x", "head": "feat", "base": "main",
	}).Body.Close()

	resp := s.post(t, "/api/v3/repos/admin/pr-assign/issues/1/assignees", defaultToken,
		map[string]interface{}{"assignees": []string{"admin"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add assignees to a PR = %d, want 201 (must not 404)", resp.StatusCode)
	}
	got := decodeJSON(t, resp)
	if !assigneeListed(got, "admin") {
		t.Fatalf("PR assignees do not include admin: %v", got["assignees"])
	}

	// Removing works too.
	del := s.do(t, http.MethodDelete, "/api/v3/repos/admin/pr-assign/issues/1/assignees", defaultToken,
		map[string]interface{}{"assignees": []string{"admin"}})
	defer del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("remove PR assignees = %d, want 200", del.StatusCode)
	}
	if assigneeListed(decodeJSON(t, del), "admin") {
		t.Fatal("admin still assigned after removal")
	}
}

// TestAssigneeAssignabilityAndCap pins that a non-assignable (non-collaborator)
// user is silently dropped and that the total assignees are capped at 10.
func TestAssigneeAssignabilityAndCap(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "assign-cap"}).Body.Close()
	s.post(t, "/api/v3/repos/admin/assign-cap/issues", defaultToken,
		map[string]interface{}{"title": "an issue"}).Body.Close()

	// A user who is not a collaborator is not assignable and is dropped.
	outsider := s.createTestUser(t, "assign-outsider")
	_ = outsider
	got := decodeJSON(t, s.post(t, "/api/v3/repos/admin/assign-cap/issues/1/assignees", defaultToken,
		map[string]interface{}{"assignees": []string{"assign-outsider"}}))
	if assigneeListed(got, "assign-outsider") {
		t.Fatal("a non-collaborator was assigned (must be dropped)")
	}

	// Assigning 12 collaborators caps at 10.
	logins := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		login := fmt.Sprintf("collab-%d", i)
		u := s.createTestUser(t, login)
		s.store.AddRepoCollaborator("admin", "assign-cap", u.Login, "push")
		logins = append(logins, login)
	}
	got = decodeJSON(t, s.post(t, "/api/v3/repos/admin/assign-cap/issues/1/assignees", defaultToken,
		map[string]interface{}{"assignees": logins}))
	assignees, _ := got["assignees"].([]interface{})
	if len(assignees) != 10 {
		t.Fatalf("assignees after assigning 12 = %d, want the 10-cap", len(assignees))
	}
}

// TestRequestedReviewerRejectsPhantomID pins that a review request with an
// unverified numeric id does not pollute requested_reviewers with a phantom user.
func TestRequestedReviewerRejectsPhantomID(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "phantom-rev")
	s.post(t, "/api/v3/repos/admin/phantom-rev/pulls", defaultToken, map[string]interface{}{
		"title": "x", "head": "feat", "base": "main",
	}).Body.Close()

	s.post(t, "/api/v3/repos/admin/phantom-rev/pulls/1/requested_reviewers", defaultToken,
		map[string]interface{}{"reviewers": []map[string]interface{}{{"id": 999999}}}).Body.Close()

	got := decodeJSON(t, s.get(t, "/api/v3/repos/admin/phantom-rev/pulls/1", defaultToken))
	reviewers, _ := got["requested_reviewers"].([]interface{})
	if len(reviewers) != 0 {
		t.Fatalf("requested_reviewers = %v, want empty (phantom id must be dropped)", reviewers)
	}
}

func assigneeListed(obj map[string]interface{}, login string) bool {
	assignees, _ := obj["assignees"].([]interface{})
	for _, a := range assignees {
		if m, ok := a.(map[string]interface{}); ok && m["login"] == login {
			return true
		}
	}
	return false
}
