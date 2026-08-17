package bleephub

import (
	"net/http"
	"testing"
)

// TestPRReviewBodyRequiredForCommentAndChanges covers that github requires a
// review body when the event is COMMENT or REQUEST_CHANGES, while APPROVE and a
// still-pending review may omit it.
func TestPRReviewBodyRequiredForCommentAndChanges(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "pr-review-body")
	s.post(t, "/api/v3/repos/admin/pr-review-body/pulls", defaultToken, map[string]interface{}{
		"title": "Body PR", "head": "feat", "base": "main",
	}).Body.Close()
	path := "/api/v3/repos/admin/pr-review-body/pulls/1/reviews"

	for _, event := range []string{"COMMENT", "REQUEST_CHANGES"} {
		resp := s.post(t, path, defaultToken, map[string]interface{}{"event": event})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			resp.Body.Close()
			t.Errorf("%s with empty body = %d, want 422", event, resp.StatusCode)
			continue
		}
		resp.Body.Close()
	}

	// APPROVE with no body is accepted.
	resp := s.post(t, path, defaultToken, map[string]interface{}{"event": "APPROVE"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("APPROVE with empty body = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPRReviewDismissRequiresMessage covers github's required `message` on the
// dismiss endpoint.
func TestPRReviewDismissRequiresMessage(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "pr-review-dismiss")
	s.post(t, "/api/v3/repos/admin/pr-review-dismiss/pulls", defaultToken, map[string]interface{}{
		"title": "Dismiss PR", "head": "feat", "base": "main",
	}).Body.Close()
	review := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/admin/pr-review-dismiss/pulls/1/reviews", defaultToken, map[string]interface{}{
		"body": "changes please", "event": "REQUEST_CHANGES",
	}), 200)
	reviewID := int(review["id"].(float64))

	// Missing message → 422.
	resp := s.put(t, "/api/v3/repos/admin/pr-review-dismiss/pulls/1/reviews/"+itoa(reviewID)+"/dismissals", defaultToken, map[string]interface{}{})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		resp.Body.Close()
		t.Fatalf("dismiss with no message = %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()

	// With a message → 200.
	requireStatus(t, s.put(t, "/api/v3/repos/admin/pr-review-dismiss/pulls/1/reviews/"+itoa(reviewID)+"/dismissals", defaultToken, map[string]interface{}{
		"message": "stale",
	}), 200)
}

// TestPRReviewAuthorAssociationReflectsRelationship covers that a review's
// author_association is the reviewer's real relationship to the repo (here a
// push collaborator → COLLABORATOR), not the OWNER-or-CONTRIBUTOR approximation
// that mislabeled every non-owner as CONTRIBUTOR.
func TestPRReviewAuthorAssociationReflectsRelationship(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "pr-review-assoc")
	s.post(t, "/api/v3/repos/admin/pr-review-assoc/pulls", defaultToken, map[string]interface{}{
		"title": "Assoc PR", "head": "feat", "base": "main",
	}).Body.Close()

	reviewer := s.createTestUser(t, "collab-reviewer")
	s.store.Mu.Lock()
	if s.store.RepoCollaborators["admin/pr-review-assoc"] == nil {
		s.store.RepoCollaborators["admin/pr-review-assoc"] = map[string]string{}
	}
	s.store.RepoCollaborators["admin/pr-review-assoc"]["collab-reviewer"] = "push"
	s.store.Mu.Unlock()
	token := s.store.CreateToken(reviewer.ID, "repo").Value

	review := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/admin/pr-review-assoc/pulls/1/reviews", token, map[string]interface{}{
		"body": "looks good", "event": "APPROVE",
	}), 200)
	if review["author_association"] != "COLLABORATOR" {
		t.Errorf("author_association = %v, want COLLABORATOR", review["author_association"])
	}
}
