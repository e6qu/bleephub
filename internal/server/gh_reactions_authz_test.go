package bleephub

import (
	"fmt"
	"net/http"
	"testing"
)

// TestReactions_CrossRepoParentsAreNotReachable pins the fix for a cross-repo
// IDOR: resolveReactionParent used to trust the raw path id for every non-issue
// parent type (issue_comment, commit_comment, pull_request_review_comment,
// release), so a caller who could write to any repository could create — and,
// through the list endpoint, read — reactions on a comment or release living in
// another (private) repository. Every parent is now re-scoped to the repo in
// the path, so naming a foreign repo is a 404 and never touches the parent.
func TestReactions_CrossRepoParentsAreNotReachable(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	st := srv.store
	admin := st.LookupUserByLogin("admin")

	// Victim repository with one of every reactable parent type.
	victim := st.CreateRepo(admin, "reactions-idor-victim", "", false)
	issue := st.CreateIssue(victim.ID, admin.ID, "victim issue", "", nil, nil, 0)
	issueComment := st.CreateComment(issue.ID, admin.ID, "victim issue comment")
	commitComment := st.CommitComments.Create(victim.ID, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", admin.ID, "victim commit comment", "", nil, nil)
	seedStorePullRequestBranches(t, st, victim, "feat")
	pr := st.CreatePullRequest(victim.ID, admin.ID, "victim pr", "", "feat", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("create victim pr")
	}
	prComment := st.PRReviewComments.CreateRootComment(pr.ID, admin.ID, "a.txt", "victim pr review comment", "sha", "RIGHT", 1, 0)
	release := st.Releases.Create(victim.ID, admin.ID, "v1.0.0", "main", "v1.0.0", "", false, false, false)

	// Attacker's own repository, named in every path below.
	if st.CreateRepo(admin, "reactions-idor-attacker", "", false) == nil {
		t.Fatal("create attacker repo")
	}

	// (store parent type, attacker path targeting the victim's parent id)
	cases := []struct {
		name       string
		parentType string
		parentID   int
		createPath string
		listPath   string
	}{
		{"issue_comment", "issue_comment", issueComment.ID,
			"/api/v3/repos/admin/reactions-idor-attacker/issues/comments/%d/reactions",
			"/api/v3/repos/admin/reactions-idor-attacker/issues/comments/%d/reactions"},
		{"commit_comment", "commit_comment", commitComment.ID,
			"/api/v3/repos/admin/reactions-idor-attacker/comments/%d/reactions",
			"/api/v3/repos/admin/reactions-idor-attacker/comments/%d/reactions"},
		{"pull_request_review_comment", "pull_request_review_comment", prComment.ID,
			"/api/v3/repos/admin/reactions-idor-attacker/pulls/comments/%d/reactions", ""},
		{"release", "release", release.ID,
			"/api/v3/repos/admin/reactions-idor-attacker/releases/%d/reactions",
			"/api/v3/repos/admin/reactions-idor-attacker/releases/%d/reactions"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := srv.post(t, fmt.Sprintf(tc.createPath, tc.parentID), defaultToken, map[string]interface{}{"content": "rocket"})
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("cross-repo react on %s = %d, want 404", tc.name, resp.StatusCode)
			}
			if tc.listPath != "" {
				lresp := srv.get(t, fmt.Sprintf(tc.listPath, tc.parentID), defaultToken)
				lresp.Body.Close()
				if lresp.StatusCode != http.StatusNotFound {
					t.Fatalf("cross-repo list %s = %d, want 404", tc.name, lresp.StatusCode)
				}
			}
			// The victim's parent must carry no reaction from the foreign path.
			if got := st.Reactions.ListReactions(tc.parentType, tc.parentID, ""); len(got) != 0 {
				t.Fatalf("%s: %d reaction(s) leaked onto the victim's parent, want 0", tc.name, len(got))
			}
		})
	}
}
