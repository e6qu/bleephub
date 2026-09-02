package bleephub

import (
	"net/http"
	"testing"
)

// TestDismissStaleReviewsOnPush pins that when the base branch enables
// dismiss_stale_reviews, pushing a new commit to a PR head dismisses the prior
// approval — so a stale approval of an earlier commit can't let never-reviewed
// code merge onto a protected branch. The setting was accepted but never enforced.
func TestDismissStaleReviewsOnPush(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "stale-rev")
	repo := s.store.GetRepo("admin", "stale-rev")
	s.put(t, "/api/v3/repos/admin/stale-rev/branches/main/protection", defaultToken, map[string]interface{}{
		"required_pull_request_reviews": map[string]interface{}{
			"required_approving_review_count": 1,
			"dismiss_stale_reviews":           true,
		},
	}).Body.Close()
	s.post(t, "/api/v3/repos/admin/stale-rev/pulls", defaultToken, map[string]interface{}{
		"title": "x", "head": "feat", "base": "main",
	}).Body.Close()
	pr := s.store.GetPullRequestByNumber(repo.ID, 1)

	reviewer := s.createTestUser(t, "stale-reviewer")
	reviewerTok := s.store.CreateToken(reviewer.ID, "repo")
	s.post(t, "/api/v3/repos/admin/stale-rev/pulls/1/reviews", reviewerTok.Value, map[string]interface{}{
		"event": "APPROVE", "body": "LGTM",
	}).Body.Close()

	if got := s.countApprovingReviews(pr.ID, pr.AuthorID); got != 1 {
		t.Fatalf("approving reviews before push = %d, want 1", got)
	}

	// A new commit is pushed to the PR head.
	admin := s.store.LookupUserByLogin("admin")
	s.firePullRequestSynchronize(repo, repo.FullName, "feat", admin, "0000", "1111")

	if got := s.countApprovingReviews(pr.ID, pr.AuthorID); got != 0 {
		t.Fatalf("approving reviews after push = %d, want 0 (stale approval must be dismissed)", got)
	}
	// The dismissed review is recorded as DISMISSED, not silently dropped.
	reviews := s.store.ListPullRequestReviews(repo.FullName, 1)
	if len(reviews) != 1 || reviews[0].State != "DISMISSED" {
		t.Fatalf("review states after push = %+v, want one DISMISSED", reviews)
	}
}

// TestStrictBranchMustBeUpToDate pins that required_status_checks.strict
// ("require branches to be up to date before merging") is enforced: a PR whose
// base has advanced past its head reports mergeable_state "behind" and the merge
// is refused. The flag was accepted but never enforced.
func TestStrictBranchMustBeUpToDate(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "strict-behind")
	repo := s.store.GetRepo("admin", "strict-behind")
	s.post(t, "/api/v3/repos/admin/strict-behind/pulls", defaultToken, map[string]interface{}{
		"title": "x", "head": "feat", "base": "main",
	}).Body.Close()

	// Advance main beyond the PR's base so the PR head is behind.
	stor := s.store.GetGitStorage("admin", "strict-behind")
	if stor == nil {
		t.Fatal("no git storage")
	}
	if _, err := createFileCommit(stor, "main", "advance.txt", "x\n", "advance main", repoSignature("t", "t@t")); err != nil {
		t.Fatalf("advance main: %v", err)
	}

	s.put(t, "/api/v3/repos/admin/strict-behind/branches/main/protection", defaultToken, map[string]interface{}{
		"required_status_checks": map[string]interface{}{"strict": true, "contexts": []string{}},
		// enforce_admins so the admin merge cannot bypass the strict check.
		"enforce_admins": true,
	}).Body.Close()

	got := decodeJSON(t, s.get(t, "/api/v3/repos/admin/strict-behind/pulls/1", defaultToken))
	if got["mergeable_state"] != "behind" {
		t.Fatalf("mergeable_state = %v, want behind", got["mergeable_state"])
	}

	merge := s.put(t, "/api/v3/repos/admin/strict-behind/pulls/1/merge", defaultToken, map[string]interface{}{})
	merge.Body.Close()
	if merge.StatusCode == http.StatusOK {
		t.Fatal("a behind branch was merged despite strict required status checks")
	}
	if pr := s.store.GetPullRequestByNumber(repo.ID, 1); pr.State == "MERGED" {
		t.Fatal("behind PR ended up merged")
	}
}

// TestRequestedReviewerRemovedAfterReview pins that a requested reviewer is moved
// out of requested_reviewers once they submit a review (GitHub does this
// silently). Previously the reviewer stayed listed as still-requested forever.
func TestRequestedReviewerRemovedAfterReview(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "req-rev")
	reviewer := s.createTestUser(t, "req-reviewer")
	s.store.AddRepoCollaborator("admin", "req-rev", reviewer.Login, "push")
	s.post(t, "/api/v3/repos/admin/req-rev/pulls", defaultToken, map[string]interface{}{
		"title": "x", "head": "feat", "base": "main",
	}).Body.Close()
	s.post(t, "/api/v3/repos/admin/req-rev/pulls/1/requested_reviewers", defaultToken,
		map[string]interface{}{"reviewers": []string{reviewer.Login}}).Body.Close()

	if !requestedReviewerListed(t, s, "req-rev", reviewer.Login) {
		t.Fatal("reviewer was not listed in requested_reviewers after the request")
	}

	reviewerTok := s.store.CreateToken(reviewer.ID, "repo")
	s.post(t, "/api/v3/repos/admin/req-rev/pulls/1/reviews", reviewerTok.Value,
		map[string]interface{}{"event": "APPROVE", "body": "ok"}).Body.Close()

	if requestedReviewerListed(t, s, "req-rev", reviewer.Login) {
		t.Fatal("reviewer still listed as requested after submitting a review")
	}
}

func requestedReviewerListed(t *testing.T, s *isolatedServer, repo, login string) bool {
	t.Helper()
	got := decodeJSON(t, s.get(t, "/api/v3/repos/admin/"+repo+"/pulls/1", defaultToken))
	reviewers, _ := got["requested_reviewers"].([]interface{})
	for _, rv := range reviewers {
		if m, ok := rv.(map[string]interface{}); ok && m["login"] == login {
			return true
		}
	}
	return false
}
