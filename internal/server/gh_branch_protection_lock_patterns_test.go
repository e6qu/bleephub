package bleephub

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// TestBranchProtectionLockBranchRoundTripAndEnforcement covers the
// lock_branch / allow_fork_syncing PUT members: they persist, render in GET
// per the protected-branch shape, and a locked branch refuses every ref
// write at the shared chokepoint plus the PR merge into it.
func TestBranchProtectionLockBranchRoundTripAndEnforcement(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "bp-lock")
	repo := s.store.GetRepo("admin", "bp-lock")
	require.NotNil(t, repo)

	resp := s.put(t, "/api/v3/repos/admin/bp-lock/branches/main/protection", defaultToken, map[string]interface{}{
		"lock_branch":        true,
		"allow_fork_syncing": true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	put := decodeJSON(t, resp)
	lock, _ := put["lock_branch"].(map[string]interface{})
	require.Equal(t, true, lock["enabled"])
	sync, _ := put["allow_fork_syncing"].(map[string]interface{})
	require.Equal(t, true, sync["enabled"])

	got := decodeJSON(t, s.get(t, "/api/v3/repos/admin/bp-lock/branches/main/protection", defaultToken))
	lock, _ = got["lock_branch"].(map[string]interface{})
	require.Equal(t, true, lock["enabled"], "GET must render lock_branch: %v", got)

	// The shared ref-write chokepoint refuses every write to the locked
	// branch — even for the repository administrator (unlocking is how you
	// write to a locked branch).
	admin := s.store.UsersByLogin["admin"]
	ctx := contextWithUser(context.Background(), admin)
	stor := s.store.GetGitStorage("admin", "bp-lock")
	require.NotNil(t, stor)
	for _, kind := range []refWriteKind{refFastForward, refForcePush, refDeletion, refCreation} {
		refusal := s.protectedRefWriteRefusal(ctx, repo, stor, plumbing.NewBranchReferenceName("main"), kind, plumbing.ZeroHash)
		require.Contains(t, refusal, "locked branch", "ref write kind %d must be refused", kind)
	}
	// Another branch in the same repository stays writable.
	require.Equal(t, "", s.protectedRefWriteRefusal(ctx, repo, stor, plumbing.NewBranchReferenceName("feat"), refFastForward, plumbing.ZeroHash))

	// A merge writes the base branch, so it is refused too.
	resp = s.post(t, "/api/v3/repos/admin/bp-lock/pulls", defaultToken, map[string]interface{}{
		"title": "into locked", "head": "feat", "base": "main",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, "/api/v3/repos/admin/bp-lock/pulls/1/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	body := decodeJSON(t, resp)
	require.Contains(t, body["message"], "locked branch")
}

// TestBranchProtectionRequireLastPushApproval covers the
// required_pull_request_reviews.require_last_push_approval gate: the merge
// needs an approval from someone other than whoever pushed the head last.
func TestBranchProtectionRequireLastPushApproval(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "bp-lastpush")

	resp := s.put(t, "/api/v3/repos/admin/bp-lastpush/branches/main/protection", defaultToken, map[string]interface{}{
		"required_pull_request_reviews": map[string]interface{}{"require_last_push_approval": true},
		"enforce_admins":                true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	put := decodeJSON(t, resp)
	reviews, _ := put["required_pull_request_reviews"].(map[string]interface{})
	require.Equal(t, true, reviews["require_last_push_approval"], "PUT must persist require_last_push_approval: %v", put)

	// The feat head was pushed (seeded) as admin.
	resp = s.post(t, "/api/v3/repos/admin/bp-lastpush/pulls", defaultToken, map[string]interface{}{
		"title": "needs non-pusher approval", "head": "feat", "base": "main",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// Unapproved: blocked.
	resp = s.put(t, "/api/v3/repos/admin/bp-lastpush/pulls/1/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	body := decodeJSON(t, resp)
	require.Contains(t, body["message"], "most recent push")

	// Approved by the pusher themself: still blocked.
	resp = s.post(t, "/api/v3/repos/admin/bp-lastpush/pulls/1/reviews", defaultToken, map[string]interface{}{
		"body": "self-approval", "event": "APPROVE",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, "/api/v3/repos/admin/bp-lastpush/pulls/1/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp.Body.Close()

	// Approved by somebody else: permitted.
	reviewer := s.createTestUser(t, "lastpush-reviewer")
	require.NotNil(t, reviewer)
	tok := s.store.CreateToken(reviewer.ID, "repo")
	resp = s.post(t, "/api/v3/repos/admin/bp-lastpush/pulls/1/reviews", tok.Value, map[string]interface{}{
		"body": "LGTM", "event": "APPROVE",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, "/api/v3/repos/admin/bp-lastpush/pulls/1/merge", defaultToken, map[string]interface{}{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestBranchProtectionPatternRules covers the web-only /ui-data pattern
// surface: CRUD with admin gating, fnmatch semantics, enforcement of a
// pattern-protected branch that has no exact rule, and exact-rule precedence.
func TestBranchProtectionPatternRules(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "bp-patterns")
	repo := s.store.GetRepo("admin", "bp-patterns")
	require.NotNil(t, repo)
	seedPullRequestBranches(t, s.Server, repo, "release/v1", "release/a/b")

	rules := []map[string]interface{}{
		{
			"pattern": "release/*",
			"protection": map[string]interface{}{
				"required_pull_request_reviews": map[string]interface{}{"required_approving_review_count": 1},
				"enforce_admins":                true,
			},
		},
	}

	// Writes are admin-gated.
	outsider := s.createTestUser(t, "bp-pattern-outsider")
	tok := s.store.CreateToken(outsider.ID, "repo")
	resp := s.put(t, "/ui-data/repos/admin/bp-patterns/branch-protection-patterns", tok.Value, rules)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	resp = s.put(t, "/ui-data/repos/admin/bp-patterns/branch-protection-patterns", defaultToken, rules)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = s.get(t, "/ui-data/repos/admin/bp-patterns/branch-protection-patterns", defaultToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	listed := decodeJSONArray(t, resp)
	require.Len(t, listed, 1)
	require.Equal(t, "release/*", listed[0]["pattern"])

	// Enforcement: a branch with no exact rule but matching the pattern is
	// protected at the shared chokepoint; `*` does not cross `/`.
	admin := s.store.UsersByLogin["admin"]
	ctx := contextWithUser(context.Background(), admin)
	stor := s.store.GetGitStorage("admin", "bp-patterns")
	require.NotNil(t, stor)
	refusal := s.protectedRefWriteRefusal(ctx, repo, stor, plumbing.NewBranchReferenceName("release/v1"), refFastForward, plumbing.ZeroHash)
	require.Contains(t, refusal, "pull request", "release/v1 must be protected by the release/* rule")
	require.Equal(t, "", s.protectedRefWriteRefusal(ctx, repo, stor, plumbing.NewBranchReferenceName("release/a/b"), refFastForward, plumbing.ZeroHash),
		"release/* must not cross a path segment")
	require.Equal(t, "", s.protectedRefWriteRefusal(ctx, repo, stor, plumbing.NewBranchReferenceName("main"), refFastForward, plumbing.ZeroHash))

	// An exact-name rule wins over the matching pattern: lock release/v1 and
	// the refusal becomes the lock, not the pattern's review requirement.
	resp = s.put(t, "/api/v3/repos/admin/bp-patterns/branches/release%2Fv1/protection", defaultToken, map[string]interface{}{
		"lock_branch": true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	refusal = s.protectedRefWriteRefusal(ctx, repo, stor, plumbing.NewBranchReferenceName("release/v1"), refFastForward, plumbing.ZeroHash)
	require.Contains(t, refusal, "locked branch", "the exact rule must win over the pattern rule")

	resp = s.delete(t, "/ui-data/repos/admin/bp-patterns/branch-protection-patterns", defaultToken)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()
	require.Empty(t, s.store.ListBranchProtectionPatterns(repo.ID))
	require.Equal(t, "", s.protectedRefWriteRefusal(ctx, repo, stor, plumbing.NewBranchReferenceName("release/other"), refFastForward, plumbing.ZeroHash))
}

// TestMatchBranchPattern pins the fnmatch dialect GitHub's branch patterns
// use: `*` stays within one path segment, `**` crosses segments, `?` is a
// single non-separator character.
func TestMatchBranchPattern(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, branch string
		want            bool
	}{
		{"release/*", "release/v1", true},
		{"release/*", "release/", true},
		{"release/*", "release/a/b", false},
		{"release/**", "release/a/b", true},
		{"release/**", "release/v1", true},
		{"*", "main", true},
		{"*", "release/v1", false},
		{"**", "release/v1", true},
		{"v?", "v1", true},
		{"v?", "v12", false},
		{"v?", "v/", false},
		{"main", "main", true},
		{"main", "maint", false},
		{"", "main", false},
		{"rel[ease", "rel[ease", true}, // '[' is literal in this dialect
	}
	for _, tc := range cases {
		if got := store.MatchBranchPattern(tc.pattern, tc.branch); got != tc.want {
			t.Errorf("MatchBranchPattern(%q, %q) = %v, want %v", tc.pattern, tc.branch, got, tc.want)
		}
	}
	// Guard against accidental substring matching.
	if store.MatchBranchPattern("release/*", "prerelease/v1") {
		t.Error("pattern must anchor at both ends")
	}
	if !strings.HasPrefix("release/v1", "release/") {
		t.Fatal("sanity")
	}
}
