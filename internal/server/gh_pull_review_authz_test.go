package bleephub

import (
	"fmt"
	"net/http"
	"testing"
)

// TestUpdatePRReviewRefusesCrossRepoAndNonAuthor pins the fix for a cross-repo
// IDOR: the update handler used to write the review body by global id before
// scoping the review to the PR in the path, so a caller who owned any repo/PR
// could rewrite a review living on someone else's repository, and the late 404
// hid that the write had already landed. The author check was also missing, so
// even a non-author naming the correct repo could edit another user's review.
func TestUpdatePRReviewRefusesCrossRepoAndNonAuthor(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	st := srv.store
	admin := st.LookupUserByLogin("admin")

	// Victim: a review authored by admin on their repo, PR #1.
	victimRepo := st.CreateRepo(admin, "review-idor-victim", "", false)
	if victimRepo == nil {
		t.Fatal("create victim repo")
	}
	seedStorePullRequestBranches(t, st, victimRepo, "feat")
	if st.CreatePullRequest(victimRepo.ID, admin.ID, "victim pr", "", "feat", "main", false, nil, nil, 0) == nil {
		t.Fatal("create victim pr")
	}
	const originalBody = "original review body"
	review := st.CreatePullRequestReview("admin/review-idor-victim", 1, admin.ID, originalBody, "PENDING")
	if review == nil {
		t.Fatal("create victim review")
	}

	// Attacker: their own account, repo, and PR #1.
	attacker, attackerToken := srv.newUser(t, "review-idor-attacker")
	attackerRepo := st.CreateRepo(attacker, "review-idor-attacker-repo", "", false)
	if attackerRepo == nil {
		t.Fatal("create attacker repo")
	}
	seedStorePullRequestBranches(t, st, attackerRepo, "feat")
	if st.CreatePullRequest(attackerRepo.ID, attacker.ID, "attacker pr", "", "feat", "main", false, nil, nil, 0) == nil {
		t.Fatal("create attacker pr")
	}

	assertBodyUnchanged := func(what string) {
		t.Helper()
		if got := st.GetPullRequestReview(review.ID); got == nil || got.Body != originalBody {
			t.Fatalf("%s: review body mutated to %q, want %q", what, got.Body, originalBody)
		}
	}

	// Cross-repo: the attacker names their own repo/PR but the victim's review id.
	crossRepoPath := fmt.Sprintf("/api/v3/repos/%s/review-idor-attacker-repo/pulls/1/reviews/%d",
		attacker.Login, review.ID)
	resp := srv.put(t, crossRepoPath, attackerToken, map[string]interface{}{"body": "defaced cross-repo"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-repo review update = %d, want 404", resp.StatusCode)
	}
	assertBodyUnchanged("cross-repo")

	// Same repo, correct PR, but the attacker is not the review's author.
	sameRepoPath := fmt.Sprintf("/api/v3/repos/admin/review-idor-victim/pulls/1/reviews/%d", review.ID)
	resp = srv.put(t, sameRepoPath, attackerToken, map[string]interface{}{"body": "defaced non-author"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-author review update = %d, want 403", resp.StatusCode)
	}
	assertBodyUnchanged("non-author")

	// Positive control: the author edits their own review through the correct path.
	resp = srv.put(t, sameRepoPath, defaultToken, map[string]interface{}{"body": "author edit"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("author review update = %d, want 200", resp.StatusCode)
	}
	updated := decodeJSON(t, resp)
	if updated["body"] != "author edit" {
		t.Fatalf("author edit body = %v, want %q", updated["body"], "author edit")
	}
	if got := st.GetPullRequestReview(review.ID); got == nil || got.Body != "author edit" {
		t.Fatalf("author edit did not persist: %#v", got)
	}
}
