package bleephub

import (
	"net/http"
	"testing"
)

// TestMergePullRequestRequiresPushAccess pins REST-123: merging writes the base
// branch, so GitHub requires push access. A read-only authenticated user (here,
// a non-collaborator on a public repo with no branch protection) used to be able
// to merge because the handler gated only on `user != nil`; it must now be
// refused with 403 while a push-capable user still merges cleanly.
func TestMergePullRequestRequiresPushAccess(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)

	// admin (owner) creates a public repo with an initial commit + a feature branch.
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

	// A read-only authenticated user (no collaborator/push access) must not merge.
	_, readerToken := srv.newUser(t, "merge-authz-reader")
	resp := srv.put(t, "/api/v3/repos/admin/merge-authz/pulls/1/merge", readerToken, map[string]interface{}{"merge_method": "merge"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only user merge = %d, want 403", resp.StatusCode)
	}
	if got := srv.store.GetPullRequestByNumber(repo.ID, 1); got == nil || got.State == "MERGED" {
		t.Fatalf("the forbidden request still merged the PR: %#v", got)
	}

	// The owner (push access) still merges cleanly.
	resp = srv.put(t, "/api/v3/repos/admin/merge-authz/pulls/1/merge", defaultToken, map[string]interface{}{"merge_method": "merge"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner merge = %d, want 200", resp.StatusCode)
	}
}
