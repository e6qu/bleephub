package bleephub

import (
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// timelineEvents fetches an issue/PR timeline and returns the set of event
// types present.
func timelineEventTypes(t *testing.T, s *isolatedServer, repo string, number int) map[string]bool {
	t.Helper()
	resp := s.get(t, "/api/v3/repos/admin/"+repo+"/issues/"+itoa(number)+"/timeline", defaultToken)
	items := decodeJSONArray(t, resp)
	got := map[string]bool{}
	for _, it := range items {
		if ev, ok := it["event"].(string); ok {
			got[ev] = true
		}
	}
	return got
}

// C-5: reopening a merged pull request is rejected (422), not a silent no-op.
func TestReopenMergedPullRequestRejected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "reopen-merged")
	s.post(t, "/api/v3/repos/admin/reopen-merged/pulls", defaultToken, map[string]interface{}{
		"title": "m", "head": "feature", "base": "main",
	}).Body.Close()
	requireStatus(t, s.put(t, "/api/v3/repos/admin/reopen-merged/pulls/1/merge", defaultToken, map[string]interface{}{}), 200)

	resp := s.patch(t, "/api/v3/repos/admin/reopen-merged/pulls/1", defaultToken, map[string]interface{}{"state": "open"})
	requireStatus(t, resp, 422)
}

// C-3: locking a pull request records a `locked` event that appears in the PR
// timeline (it was previously parented to "issue" and filtered out).
func TestPullRequestLockAppearsInTimeline(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "pr-lock-tl")
	s.post(t, "/api/v3/repos/admin/pr-lock-tl/pulls", defaultToken, map[string]interface{}{
		"title": "m", "head": "feature", "base": "main",
	}).Body.Close()
	requireStatus(t, s.put(t, "/api/v3/repos/admin/pr-lock-tl/issues/1/lock", defaultToken, map[string]interface{}{
		"lock_reason": "resolved",
	}), 204)

	if !timelineEventTypes(t, s, "pr-lock-tl", 1)["locked"] {
		t.Fatal("PR timeline missing `locked` event after locking the pull request")
	}
}

// C-1/C-2: editing labels and milestone through PATCH /issues/{n} records the
// same timeline events the dedicated sub-resource endpoints do.
func TestUpdateIssueRecordsLabelAndMilestoneEvents(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "iss-edit-events"}).Body.Close()
	s.post(t, "/api/v3/repos/admin/iss-edit-events/labels", defaultToken, map[string]interface{}{"name": "regression"}).Body.Close()
	s.post(t, "/api/v3/repos/admin/iss-edit-events/milestones", defaultToken, map[string]interface{}{"title": "v1"}).Body.Close()
	s.post(t, "/api/v3/repos/admin/iss-edit-events/issues", defaultToken, map[string]interface{}{"title": "an issue"}).Body.Close()

	requireStatus(t, s.patch(t, "/api/v3/repos/admin/iss-edit-events/issues/1", defaultToken, map[string]interface{}{
		"labels":    []string{"regression"},
		"milestone": 1,
	}), 200)

	got := timelineEventTypes(t, s, "iss-edit-events", 1)
	if !got["labeled"] {
		t.Error("issue timeline missing `labeled` event after PATCH with labels")
	}
	if !got["milestoned"] {
		t.Error("issue timeline missing `milestoned` event after PATCH with milestone")
	}
}

// C-4: dismissing a review records a distinct `review_dismissed` timeline event.
func TestReviewDismissedRecordsTimelineEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "rev-dismiss")
	s.post(t, "/api/v3/repos/admin/rev-dismiss/pulls", defaultToken, map[string]interface{}{
		"title": "m", "head": "feature", "base": "main",
	}).Body.Close()
	review := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/admin/rev-dismiss/pulls/1/reviews", defaultToken, map[string]interface{}{
		"body": "LGTM", "event": "APPROVE",
	}), 200)
	reviewID := int(review["id"].(float64))
	requireStatus(t, s.put(t, "/api/v3/repos/admin/rev-dismiss/pulls/1/reviews/"+itoa(reviewID)+"/dismissals", defaultToken, map[string]interface{}{
		"message": "stale",
	}), 200)

	if !timelineEventTypes(t, s, "rev-dismiss", 1)["review_dismissed"] {
		t.Fatal("PR timeline missing `review_dismissed` event after dismissal")
	}
}

// C-7: a freshly created repository carries github's default feature flags.
func TestNewRepoDefaultFeatureFlagsMatchGitHub(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := decodeJSONWithStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "defaults-repo",
	}), 201)
	for field, want := range map[string]bool{"has_wiki": true, "has_projects": true, "has_issues": true, "has_discussions": false} {
		if repo[field] != want {
			t.Errorf("new repo %s = %v, want %v (github default)", field, repo[field], want)
		}
	}
}

// C-REST-1: the commit-list author/committer resolve to a simple-user or null,
// never an empty object.
func TestCommitListAuthorIsUserOrNull(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "commit-author", "auto_init": true}).Body.Close()
	commits := decodeJSONArray(t, s.get(t, "/api/v3/repos/admin/commit-author/commits", defaultToken))
	if len(commits) == 0 {
		t.Fatal("no commits for auto-init repo")
	}
	author := commits[0]["author"]
	if author == nil {
		return // null is valid
	}
	m, ok := author.(map[string]interface{})
	if !ok {
		t.Fatalf("author = %T, want simple-user object or null", author)
	}
	if _, hasLogin := m["login"]; !hasLogin {
		t.Fatalf("author is a non-null object without `login` (the empty-object bug): %v", m)
	}
}

// C-GQL-1: a mutation targeting a missing node returns errors[].type NOT_FOUND.
func TestGraphQLMutationMissingNodeIsNotFound(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/graphql", defaultToken, map[string]string{
		"query": `mutation { closePullRequest(input:{pullRequestId:"PR_kwNONEXISTENT"}){ clientMutationId } }`,
	})
	body := decodeJSONWithStatus(t, resp, 200)
	errs, _ := body["errors"].([]interface{})
	if len(errs) == 0 {
		t.Fatalf("expected a NOT_FOUND error, got %v", body)
	}
	first, _ := errs[0].(map[string]interface{})
	if first["type"] != "NOT_FOUND" {
		t.Fatalf("mutation missing-node error type = %v, want NOT_FOUND", first["type"])
	}
}

// NC-6: the merge gate's per-user "latest review" is chosen by SubmittedAt, not
// map-iteration order, so a later CHANGES_REQUESTED overrides an earlier
// APPROVED deterministically.
func TestLatestReviewStateByTimeNotMapOrder(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "review-order")
	s.post(t, "/api/v3/repos/admin/review-order/pulls", defaultToken, map[string]interface{}{
		"title": "m", "head": "feature", "base": "main",
	}).Body.Close()
	pr := s.store.GetPullRequestByNumber(s.store.GetRepo("admin", "review-order").ID, 1)

	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	s.store.Mu.Lock()
	// Two reviews by the same user: an earlier APPROVED and a later
	// CHANGES_REQUESTED. Insert approved-second so map order would favour it.
	s.store.PRReviews[9001] = &store.PullRequestReview{ID: 9001, PRID: pr.ID, AuthorID: 42, State: "CHANGES_REQUESTED", SubmittedAt: &late}
	s.store.PRReviews[9002] = &store.PullRequestReview{ID: 9002, PRID: pr.ID, AuthorID: 42, State: "APPROVED", SubmittedAt: &early}
	s.store.Mu.Unlock()

	if s.countApprovingReviews(pr.ID) != 0 {
		t.Error("expected 0 approvals: the user's latest review is CHANGES_REQUESTED")
	}
	if !s.hasRequestedChanges(pr.ID) {
		t.Error("expected CHANGES_REQUESTED to be the effective latest review state")
	}
}
