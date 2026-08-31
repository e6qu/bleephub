package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestPrivateRepoSubIssueReadsAreGated pins that the sub-issue / dependency /
// label GET routes (which share issueFromNumberPath) honor private-repo
// visibility, rather than leaking a private repo's issue data to unauthorized
// viewers.
func TestPrivateRepoSubIssueReadsAreGated(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "priv-subissue", true)
	admin := s.store.LookupUserByLogin("admin")
	issue := s.store.CreateIssue(repo.ID, admin.ID, "secret", "hush", nil, nil, 0)
	if issue == nil {
		t.Fatal("CreateIssue failed")
	}
	path := "/api/v3/repos/admin/priv-subissue/issues/" + itoa(issue.Number) + "/sub_issues"

	// Anonymous and a stranger both get 404 (hidden), not 200 with the private
	// issue's sub-issue list.
	requireStatus(t, s.get(t, path, ""), http.StatusNotFound)
	stranger := s.createTestUser(t, "subissue-stranger")
	strangerTok := s.store.CreateToken(stranger.ID, "repo")
	requireStatus(t, s.get(t, path, strangerTok.Value), http.StatusNotFound)

	// The owner still reads it.
	requireStatus(t, s.get(t, path, defaultToken), http.StatusOK)
}

// TestUserSubscriptionsOmitPrivateReposForStrangers pins that
// GET /users/{u}/subscriptions filters repos the viewer can't see.
func TestUserSubscriptionsOmitPrivateReposForStrangers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	priv := s.seedRepo(t, "watched-private", true)
	s.store.SetRepoSubscription(admin.ID, priv.ID, true, false)

	resp := s.get(t, "/api/v3/users/admin/subscriptions", "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSONArray(t, resp)
	for _, row := range body {
		if row["full_name"] == "admin/watched-private" {
			t.Fatal("anonymous /subscriptions leaked a private watched repo")
		}
	}
}

// TestUpdatePullRequestRejectsUnknownBase pins that retargeting a PR to a
// non-existent base branch 422s (GitHub parity) instead of silently accepting.
func TestUpdatePullRequestRejectsUnknownBase(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "pr-base")
	s.post(t, "/api/v3/repos/admin/pr-base/pulls", defaultToken, map[string]interface{}{
		"title": "x", "head": "feature", "base": "main",
	}).Body.Close()

	requireStatus(t, s.patch(t, "/api/v3/repos/admin/pr-base/pulls/1", defaultToken,
		map[string]interface{}{"base": "no-such-branch"}), http.StatusUnprocessableEntity)
	// A real seeded branch is accepted.
	requireStatus(t, s.patch(t, "/api/v3/repos/admin/pr-base/pulls/1", defaultToken,
		map[string]interface{}{"base": "fix"}), http.StatusOK)
}

// TestAsyncMergeRejectsDisabledMethod pins that the async merge endpoint honors
// the repository's disabled merge methods with a 405, like the sync endpoint.
func TestAsyncMergeRejectsDisabledMethod(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "pr-async")
	s.store.UpdateRepo("admin", "pr-async", func(r *store.Repo) {
		r.AllowMergeCommit = false
		r.AllowSquashMerge = true
		r.AllowRebaseMerge = true
	})
	s.post(t, "/api/v3/repos/admin/pr-async/pulls", defaultToken, map[string]interface{}{
		"title": "x", "head": "feature", "base": "main",
	}).Body.Close()

	requireStatus(t, s.put(t, "/api/v3/repos/admin/pr-async/pulls/1/merge-async", defaultToken,
		map[string]interface{}{"merge_method": "merge"}), http.StatusMethodNotAllowed)
}

// TestCreateIssueRejectsUnknownMilestone pins that creating an issue with a
// non-existent milestone 422s (matching the PATCH path and GitHub) rather than
// silently dropping it.
func TestCreateIssueRejectsUnknownMilestone(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "iss-ms", false)
	requireStatus(t, s.post(t, "/api/v3/repos/admin/iss-ms/issues", defaultToken,
		map[string]interface{}{"title": "x", "milestone": 999}), http.StatusUnprocessableEntity)
}
