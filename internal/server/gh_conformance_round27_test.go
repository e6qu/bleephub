package bleephub

import (
	"encoding/json"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// notificationReasonsFor returns the set of thread reasons GET /notifications
// shows the given token's user.
func notificationReasonsFor(t *testing.T, s *isolatedServer, token string) map[string]bool {
	t.Helper()
	resp := s.get(t, "/api/v3/notifications", token)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /notifications status = %d, want 200", resp.StatusCode)
	}
	var threads []struct {
		Reason string `json:"reason"`
	}
	json.NewDecoder(resp.Body).Decode(&threads)
	out := map[string]bool{}
	for _, th := range threads {
		out[th.Reason] = true
	}
	return out
}

// Round-2 conformance: merging with a repo-disabled merge method 405s (github).
func TestMergeMethodDisabledReturns405(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "merge-method")
	// Disable squash + rebase; leave a normal merge commit allowed.
	s.store.UpdateRepo("admin", "merge-method", func(r *store.Repo) {
		r.AllowSquashMerge = false
		r.AllowRebaseMerge = false
		r.AllowMergeCommit = true
	})
	s.post(t, "/api/v3/repos/admin/merge-method/pulls", defaultToken, map[string]interface{}{
		"title": "m", "head": "feature", "base": "main",
	}).Body.Close()

	requireStatus(t, s.put(t, "/api/v3/repos/admin/merge-method/pulls/1/merge", defaultToken, map[string]interface{}{
		"merge_method": "squash",
	}), 405)
	requireStatus(t, s.put(t, "/api/v3/repos/admin/merge-method/pulls/1/merge", defaultToken, map[string]interface{}{
		"merge_method": "rebase",
	}), 405)
	// The enabled method still merges.
	requireStatus(t, s.put(t, "/api/v3/repos/admin/merge-method/pulls/1/merge", defaultToken, map[string]interface{}{
		"merge_method": "merge",
	}), 200)
}

// A requested reviewer gets a notification thread with reason "review_requested",
// and an @-mentioned user gets one with reason "mention" — even without watching
// the repo (round-2: these reasons were never produced).
func TestNotificationReasonsReviewRequestedAndMention(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "notif-reasons", "", false)
	carol := s.createTestUser(t, "carol")
	bob := s.createTestUser(t, "bob")
	carolTok := s.store.CreateToken(carol.ID, "repo")
	bobTok := s.store.CreateToken(bob.ID, "repo")

	// review_requested: a PR with carol as a requested reviewer.
	s.createTestPRRepo(t, "notif-pr")
	s.post(t, "/api/v3/repos/admin/notif-pr/pulls", defaultToken, map[string]interface{}{
		"title": "review me", "head": "feature", "base": "main",
	}).Body.Close()
	if !s.store.RequestReviewers("admin/notif-pr", 1, []int{carol.ID}, admin.ID) {
		t.Fatal("request carol as reviewer")
	}
	if !notificationReasonsFor(t, s, carolTok.Value)["review_requested"] {
		t.Error("requested reviewer carol has no review_requested notification")
	}

	// mention: an issue body that @-mentions bob.
	s.store.CreateIssue(repo.ID, admin.ID, "ping", "hey @bob take a look", nil, nil, 0)
	if !notificationReasonsFor(t, s, bobTok.Value)["mention"] {
		t.Error("mentioned user bob has no mention notification")
	}
}
