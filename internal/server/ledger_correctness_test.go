package bleephub

import (
	"fmt"
	"testing"
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
	stale := &PullRequest{ID: pr.ID + 100000, RepoID: repo.ID, Number: pr.Number, Title: "stale"}
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

// TestIssueFieldValuesConnectionCountsBeyond100 covers GQL-022: the
// issueFieldValues sub-connection pre-paginated with paginateGQL, whose page
// size is clamped to 100, and the field resolver then re-paginated that
// already-truncated slice — so an issue with more than 100 field values
// reported totalCount 100 and hid the remainder. The connection now returns the
// full node set and lets the resolver apply the single, correct page window.
func TestIssueFieldValuesConnectionCountsBeyond100(t *testing.T) {
	s := newTestServer()
	st := s.store
	admin := st.UsersByLogin["admin"]
	org := st.CreateOrg(admin, "fieldorg", "Field Org", "")
	repo := st.CreateOrgRepo(org, admin, "fieldrepo", "", false)
	issue := st.CreateIssue(repo.ID, admin.ID, "issue with many fields", "", nil, nil, 0)
	if issue == nil {
		t.Fatal("issue not created")
	}

	const total = 105
	values := make(map[int]interface{}, total)
	for i := 0; i < total; i++ {
		f := st.CreateIssueField(org.Login, fmt.Sprintf("field-%03d", i), nil, "text", "all", nil)
		values[f.ID] = "v"
	}
	st.SetIssueFieldValues(issue.ID, values)

	st.Mu.RLock()
	conn := issueFieldValuesConnectionLocked(st, issue)
	st.Mu.RUnlock()

	// The Issue.issueFieldValues resolver hands the built connection to
	// repaginateConnection with the client's page args; totalCount must be the
	// true count, not the clamped page size.
	repaged, ok := repaginateConnection(conn, map[string]interface{}{}).(map[string]interface{})
	if !ok {
		t.Fatalf("repaginateConnection returned %T, want map", repaged)
	}
	if got := repaged["totalCount"].(int); got != total {
		t.Fatalf("totalCount = %d, want %d (GQL-022: pre-pagination clamped it to 100)", got, total)
	}
}
