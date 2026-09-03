package bleephub

import (
	"net/http"
	"testing"
)

// TestAdminDeleteUserClearsDanglingEdges pins that deleting an account removes
// the graph edges that named it — stars it placed, watch subscriptions it held,
// follow edges in both directions, and block edges in both directions — so no
// surviving edge inflates another account's counts or points at a deleted user.
func TestAdminDeleteUserClearsDanglingEdges(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "popular", false)
	victim, _ := s.newUser(t, "victim")
	other, _ := s.newUser(t, "other")

	s.store.StarRepo(victim.ID, "admin", "popular")
	s.store.SetRepoSubscription(victim.ID, repo.ID, true, false)
	s.store.SetFollow("victim", "admin", true) // victim follows admin
	s.store.SetFollow("other", "victim", true) // other follows victim
	s.store.BlockUser(victim.ID, other.ID)     // victim blocks other
	s.store.BlockUser(other.ID, victim.ID)     // other blocks victim

	// Sanity: the edges exist before deletion.
	if got := s.store.GetRepo("admin", "popular").StargazersCount; got != 1 {
		t.Fatalf("pre-delete stargazers = %d, want 1", got)
	}
	if s.store.CountFollowers("victim") != 1 || s.store.CountFollowing("victim") != 1 {
		t.Fatal("pre-delete follow edges missing")
	}

	resp := s.delete(t, "/api/v3/admin/users/victim", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete user = %d, want 204", resp.StatusCode)
	}

	if got := s.store.GetRepo("admin", "popular").StargazersCount; got != 0 {
		t.Fatalf("post-delete stargazers = %d, want 0", got)
	}
	if got := s.store.ListRepoStargazers("admin", "popular"); len(got) != 0 {
		t.Fatalf("post-delete stargazer listing = %v, want empty", got)
	}
	if s.store.GetRepoSubscription(victim.ID, repo.ID) != nil {
		t.Fatal("post-delete watch subscription survived")
	}
	if s.store.CountFollowing("other") != 0 {
		t.Fatal("post-delete: other still follows the deleted user")
	}
	if s.store.CountFollowers("admin") != 0 {
		t.Fatal("post-delete: admin still has the deleted user as a follower")
	}
	if s.store.IsUserBlocked(other.ID, victim.ID) {
		t.Fatal("post-delete: other still blocks the deleted user")
	}
}

// gitDataTreeSHA creates a tree with one inline file and returns its SHA. POST
// /git/trees is not itself push-protected (only /git/blobs and ref writes are),
// so this seeds blob content without tripping the block under test.
func (s *isolatedServer) gitDataTreeSHA(t *testing.T, repoFullName, path, content string) string {
	t.Helper()
	resp := s.post(t, "/api/v3/repos/"+repoFullName+"/git/trees", defaultToken, map[string]any{
		"tree": []map[string]any{
			{"path": path, "mode": "100644", "type": "blob", "content": content},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create tree: %d", resp.StatusCode)
	}
	return decodeJSON(t, resp)["sha"].(string)
}

// gitDataCommitSHA creates a commit over treeSHA with the given parents and
// returns its SHA.
func (s *isolatedServer) gitDataCommitSHA(t *testing.T, repoFullName, message, treeSHA string, parents ...string) string {
	t.Helper()
	if parents == nil {
		parents = []string{}
	}
	resp := s.post(t, "/api/v3/repos/"+repoFullName+"/git/commits", defaultToken, map[string]any{
		"message": message,
		"tree":    treeSHA,
		"parents": parents,
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create commit: %d", resp.StatusCode)
	}
	return decodeJSON(t, resp)["sha"].(string)
}

// TestSecretScanningPushProtectionScansPushedRange pins that push protection
// blocks a secret introduced by an *intermediate* commit of a multi-commit
// push even when the tip tree no longer contains it. Previously only the tip
// commit's tree was scanned, so a secret added then deleted within one push
// slipped into the repository's permanent history unblocked.
func TestSecretScanningPushProtectionScansPushedRange(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, repo := "ss-range-org", "ss-range-repo"
	full := org + "/" + repo
	s.createSecretScanningOrgRepoViaPublicAPI(t, org, repo)
	s.enableSecretScanningPushProtectionPattern(t, org, "aws")

	secret := "token=" + secretScanningSeedValue("aws_access_key_id") + "\n"

	// Commit A adds the secret; commit B (its child) removes it. B's tree is
	// clean, but A — carried by the same push — introduced the secret.
	treeA := s.gitDataTreeSHA(t, full, "credentials.txt", secret)
	commitA := s.gitDataCommitSHA(t, full, "add credentials", treeA)
	treeB := s.gitDataTreeSHA(t, full, "README.md", "all clean now\n")
	commitB := s.gitDataCommitSHA(t, full, "remove credentials", treeB, commitA)

	// Creating the branch at B pushes the A..B range; the secret in A must block.
	resp := s.post(t, "/api/v3/repos/"+full+"/git/refs", defaultToken, map[string]any{
		"ref": "refs/heads/main",
		"sha": commitB,
	})
	if ph := secretScanningBlockedPlaceholder(t, resp); ph == "" {
		t.Fatal("push of a range whose intermediate commit adds a secret was not blocked")
	}

	// The branch must not have been created.
	resp = s.get(t, "/api/v3/repos/"+full+"/git/ref/heads/main", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("blocked ref create mutated the branch: %d, want 404", resp.StatusCode)
	}
}

// TestSecretScanningPushProtectionAllowsCleanRange pins the converse: a
// multi-commit push with no secret anywhere in its range is accepted — the
// range walk does not over-block.
func TestSecretScanningPushProtectionAllowsCleanRange(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, repo := "ss-clean-org", "ss-clean-repo"
	full := org + "/" + repo
	s.createSecretScanningOrgRepoViaPublicAPI(t, org, repo)
	s.enableSecretScanningPushProtectionPattern(t, org, "aws")

	treeA := s.gitDataTreeSHA(t, full, "a.txt", "first\n")
	commitA := s.gitDataCommitSHA(t, full, "first", treeA)
	treeB := s.gitDataTreeSHA(t, full, "b.txt", "second\n")
	commitB := s.gitDataCommitSHA(t, full, "second", treeB, commitA)

	resp := s.post(t, "/api/v3/repos/"+full+"/git/refs", defaultToken, map[string]any{
		"ref": "refs/heads/main",
		"sha": commitB,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("clean multi-commit push = %d, want 201", resp.StatusCode)
	}
}
