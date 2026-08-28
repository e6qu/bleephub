package bleephub

import (
	"net/http"
	"testing"
)

// REST-123: merging writes the base branch, so it requires push access. A
// read-only authenticated user (once allowed by a `user != nil`-only gate) must
// be refused with 403 while a push-capable user still merges cleanly.
func TestMergePullRequestRequiresPushAccess(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)

	srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "merge-authz", "auto_init": true,
	}).Body.Close()
	repo := srv.store.GetRepo("admin", "merge-authz")
	if repo == nil {
		t.Fatal("repo not created")
	}
	seedPullRequestBranches(t, srv.Server, repo, "feat")

	srv.post(t, "/api/v3/repos/admin/merge-authz/pulls", defaultToken, map[string]interface{}{
		"title": "add feature", "head": "feat", "base": "main",
	}).Body.Close()

	// A read-only user with no collaborator/push access must not merge.
	_, readerToken := srv.newUser(t, "merge-authz-reader")
	resp := srv.put(t, "/api/v3/repos/admin/merge-authz/pulls/1/merge", readerToken, map[string]interface{}{"merge_method": "merge"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only user merge = %d, want 403", resp.StatusCode)
	}
	if got := srv.store.GetPullRequestByNumber(repo.ID, 1); got == nil || got.State == "MERGED" {
		t.Fatalf("the forbidden request still merged the PR: %#v", got)
	}

	resp = srv.put(t, "/api/v3/repos/admin/merge-authz/pulls/1/merge", defaultToken, map[string]interface{}{"merge_method": "merge"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner merge = %d, want 200", resp.StatusCode)
	}
}
