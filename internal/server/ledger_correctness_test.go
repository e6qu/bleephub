package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestReviewerRequestResolvesPRThroughTheIndex covers STORE-046: the
// reviewer-request mutations used to resolve a pull request with a linear scan
// of every PR in the instance, which returns an arbitrary match in map order,
// while every read/merge path used the PullsByRepo index. The scan is now the
// same index lookup, so the reviewer path and the read path resolve the exact
// same object.
func TestReviewerRequestResolvesPRThroughTheIndex(t *testing.T) {
	s, admin, repo := pullsTestServer(t)
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "index", "", "feature", "feat", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("PR not created")
	}

	if !s.store.RequestReviewers(repo.FullName, pr.Number, []int{admin.ID}, admin.ID) {
		t.Fatal("RequestReviewers did not resolve the PR")
	}

	got := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
	if got == nil || len(got.RequestedReviewerIDs) != 1 || got.RequestedReviewerIDs[0] != admin.ID {
		t.Fatalf("reviewer not recorded on the indexed PR: %#v", got)
	}

	// Force the exact desync the bug is about: leave the real PR in the
	// PullsByRepo index but replace its slot in the flat st.PullRequests
	// collection with a stale record sharing (repoID, number). The old linear
	// scan of st.PullRequests would resolve the stale record; the index resolves
	// the real one. The reviewer path must follow the index.
	stale := &store.PullRequest{ID: pr.ID + 100000, RepoID: repo.ID, Number: pr.Number, Title: "stale"}
	s.store.Mu.Lock()
	delete(s.store.PullRequests, pr.ID)
	s.store.PullRequests[stale.ID] = stale
	viaHelper := s.store.FindPRByRepoNumberLocked(repo.FullName, pr.Number)
	s.store.Mu.Unlock()
	if viaHelper == nil || viaHelper.ID != pr.ID {
		gotID := 0
		if viaHelper != nil {
			gotID = viaHelper.ID
		}
		t.Fatalf("STORE-046: reviewer path resolved PR %d, want the indexed PR %d (a scan would return the stale record)", gotID, pr.ID)
	}
}
