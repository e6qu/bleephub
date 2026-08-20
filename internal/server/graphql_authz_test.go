package bleephub

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// The GraphQL mutation surface authenticated its callers and then acted. Every
// mutation below reached the store on behalf of any account that held a token,
// so the schema was a complete authorization bypass around the REST handlers
// that guard the same records: a stranger closed issues, merged pull requests,
// locked threads, moderated comments and deleted discussions on repositories
// they could not even read.
//
// The cases are driven from a table so that a mutation added later is not
// silently exempt, and the same table is replayed twice — once for a caller
// with no relationship to the repository, who must be refused by all of them,
// and once for the repository's owner, who must still be served. Over-blocking
// has been the recurring regression here, so the positive half is not
// optional.

// gqlAuthzFixture is a private repository with one of everything the mutation
// surface addresses, plus its owner and an unrelated account.
type gqlAuthzFixture struct {
	owner         *store.User
	ownerToken    string
	stranger      *store.User
	strangerToken string
	repo          *store.Repo
	repo2         *store.Repo // second repo under the same owner (transferIssue's destination)
	issue         *store.Issue
	comment       *store.Comment
	label         *store.IssueLabel
	milestone     *store.Milestone
	category      *store.DiscussionCategory
	discussion    *store.Discussion
	discComment   *store.DiscussionComment
	pr            *store.PullRequest
	threadNodeID  string
	reviewNodeID  string
	headSHA       string
}

func newGQLAuthzFixture(t *testing.T, srv *Server, tag string, private bool) *gqlAuthzFixture {
	t.Helper()
	st := srv.store
	now := fixedTestTime.UTC()

	mkUser := func(login string) *store.User {
		st.Mu.Lock()
		defer st.Mu.Unlock()
		u := &store.User{
			ID:        st.NextUser,
			NodeID:    fmt.Sprintf("U_authz%08d", st.NextUser),
			Login:     login,
			Type:      "User",
			CreatedAt: now,
			UpdatedAt: now,
		}
		st.Users[u.ID] = u
		st.UsersByLogin[u.Login] = u
		st.NextUser++
		return u
	}

	f := &gqlAuthzFixture{}
	f.owner = mkUser("gqlauthz-owner-" + tag)
	f.stranger = mkUser("gqlauthz-stranger-" + tag)

	f.repo = st.CreateRepo(f.owner, "gqlauthz-repo", "", private)
	if f.repo == nil {
		t.Fatalf("could not create the fixture repository for %s", tag)
	}
	f.repo2 = st.CreateRepo(f.owner, "gqlauthz-repo-two", "", private)
	if f.repo2 == nil {
		t.Fatalf("could not create the second fixture repository for %s", tag)
	}
	branches := seedPullRequestBranches(t, srv, f.repo, "feature", "spare")
	f.headSHA = branches["feature"]
	if f.headSHA == "" {
		t.Fatalf("fixture %s: feature branch has no head commit", tag)
	}

	f.issue = st.CreateIssue(f.repo.ID, f.owner.ID, "fixture issue", "body", nil, nil, 0)
	f.comment = st.CreateComment(f.issue.ID, f.owner.ID, "fixture comment")
	f.label = st.CreateLabel(f.repo.ID, "authz-label", "", "d73a4a")
	f.milestone = st.CreateMilestone(f.repo.ID, f.owner.ID, "authz milestone", "", "open", nil)
	f.category = st.CreateDiscussionCategory(f.repo.ID, "Q&A", "", "answers welcome", true)
	f.discussion = st.CreateDiscussion(f.repo.ID, f.category.ID, f.owner.ID, "fixture discussion", "body")
	f.discComment = st.CreateDiscussionComment(f.discussion.ID, f.owner.ID, "fixture answer", 0)
	f.pr = st.CreatePullRequest(f.repo.ID, f.owner.ID, "fixture pr", "body", "feature", "main", false, nil, nil, 0)
	if f.issue == nil || f.comment == nil || f.label == nil || f.milestone == nil ||
		f.category == nil || f.discussion == nil || f.discComment == nil || f.pr == nil {
		t.Fatalf("fixture %s: store refused to seed a record", tag)
	}
	root := st.PRReviewComments.CreateRootComment(f.pr.ID, f.owner.ID, "README.md", "nit", "", "RIGHT", 1, 0)
	if root == nil {
		t.Fatalf("fixture %s: could not seed a review thread", tag)
	}
	f.threadNodeID = store.PRReviewThreadNodeID(root.ID)

	// A pending review owned by the repo owner, for the submit/dismiss cases.
	pendingReview := st.CreatePRReview(f.pr.ID, f.owner.ID, "PENDING", "pending body")
	if pendingReview == nil {
		t.Fatalf("fixture %s: could not seed a pending review", tag)
	}
	f.reviewNodeID = pendingReview.NodeID

	ownerTok := st.CreateToken(f.owner.ID, "repo")
	strangerTok := st.CreateToken(f.stranger.ID, "repo")
	if ownerTok == nil || strangerTok == nil {
		t.Fatalf("fixture %s: could not mint tokens", tag)
	}
	f.ownerToken = ownerTok.Value
	f.strangerToken = strangerTok.Value
	return f
}

func gqlAuthzErrors(env map[string]interface{}) []interface{} {
	errs, _ := env["errors"].([]interface{})
	return errs
}

// gqlMutationCase is one row of the mutation surface: the document a client
// sends and the input it carries against the fixture.
type gqlMutationCase struct {
	name  string
	doc   string
	input func(f *gqlAuthzFixture) map[string]interface{}
	// setup, when set, arranges preconditions the mutation's own semantics
	// demand beyond the plain fixture (e.g. enablePullRequestAutoMerge is
	// only legal while something blocks the merge). It runs before the
	// request in both the refusal and the entitled table.
	setup func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture)
}

// gqlMutationCases covers every mutation that names an existing resource.
// createRepository is deliberately absent: it names no repository, and its own
// resolver is what decides which owner the caller may create under.
var gqlMutationCases = []gqlMutationCase{
	{
		name: "deleteRepository",
		doc:  `mutation($input:DeleteRepositoryInput!){deleteRepository(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID}
		},
	},
	{
		name: "createIssue",
		doc:  `mutation($input:CreateIssueInput!){createIssue(input:$input){issue{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID, "title": "from the table"}
		},
	},
	{
		name: "addComment",
		doc:  `mutation($input:AddCommentInput!){addComment(input:$input){commentEdge{node{id}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.issue.NodeID, "body": "from the table"}
		},
	},
	{
		name: "closeIssue",
		doc:  `mutation($input:CloseIssueInput!){closeIssue(input:$input){issue{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "reopenIssue",
		doc:  `mutation($input:ReopenIssueInput!){reopenIssue(input:$input){issue{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "updateIssue",
		doc:  `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.issue.NodeID, "title": "from the table"}
		},
	},
	{
		name: "pinIssue",
		doc:  `mutation($input:PinIssueInput!){pinIssue(input:$input){issue{isPinned}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "unpinIssue",
		doc:  `mutation($input:UnpinIssueInput!){unpinIssue(input:$input){issue{isPinned}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "transferIssue",
		doc:  `mutation($input:TransferIssueInput!){transferIssue(input:$input){issue{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID, "repositoryId": f.repo2.NodeID}
		},
	},
	{
		name: "deleteIssue",
		doc:  `mutation($input:DeleteIssueInput!){deleteIssue(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"issueId": f.issue.NodeID}
		},
	},
	{
		name: "createDiscussion",
		doc:  `mutation($input:CreateDiscussionInput!){createDiscussion(input:$input){discussion{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"categoryId":   store.DiscussionCategoryNodeID(f.category.ID),
				"title":        "from the table",
				"body":         "from the table",
			}
		},
	},
	{
		name: "addDiscussionComment",
		doc:  `mutation($input:AddDiscussionCommentInput!){addDiscussionComment(input:$input){comment{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"discussionId": f.discussion.NodeID, "body": "from the table"}
		},
	},
	{
		name: "addReaction",
		doc:  `mutation($input:AddReactionInput!){addReaction(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.discussion.NodeID, "content": "THUMBS_UP"}
		},
	},
	{
		name: "removeReaction",
		doc:  `mutation($input:RemoveReactionInput!){removeReaction(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.discussion.NodeID, "content": "THUMBS_UP"}
		},
	},
	{
		name: "updateDiscussion",
		doc:  `mutation($input:UpdateDiscussionInput!){updateDiscussion(input:$input){discussion{title}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"discussionId": f.discussion.NodeID, "title": "from the table"}
		},
	},
	{
		name: "deleteDiscussion",
		doc:  `mutation($input:DeleteDiscussionInput!){deleteDiscussion(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.discussion.NodeID}
		},
	},
	{
		name: "updateDiscussionComment",
		doc:  `mutation($input:UpdateDiscussionCommentInput!){updateDiscussionComment(input:$input){comment{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"commentId": f.discComment.NodeID, "body": "from the table"}
		},
	},
	{
		name: "deleteDiscussionComment",
		doc:  `mutation($input:DeleteDiscussionCommentInput!){deleteDiscussionComment(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.discComment.NodeID}
		},
	},
	{
		name: "markDiscussionCommentAsAnswer",
		doc:  `mutation($input:MarkDiscussionCommentAsAnswerInput!){markDiscussionCommentAsAnswer(input:$input){discussion{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.discComment.NodeID}
		},
	},
	{
		name: "unmarkDiscussionCommentAsAnswer",
		doc:  `mutation($input:UnmarkDiscussionCommentAsAnswerInput!){unmarkDiscussionCommentAsAnswer(input:$input){discussion{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.discComment.NodeID}
		},
	},
	{
		name: "addUpvote",
		doc:  `mutation($input:AddUpvoteInput!){addUpvote(input:$input){subject{upvoteCount}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.discussion.NodeID}
		},
	},
	{
		name: "removeUpvote",
		doc:  `mutation($input:RemoveUpvoteInput!){removeUpvote(input:$input){subject{upvoteCount}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.discussion.NodeID}
		},
	},
	{
		name: "minimizeComment",
		doc:  `mutation($input:MinimizeCommentInput!){minimizeComment(input:$input){minimizedComment{isMinimized}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.comment.NodeID, "classifier": "OFF_TOPIC"}
		},
	},
	{
		name: "unminimizeComment",
		doc:  `mutation($input:UnminimizeCommentInput!){unminimizeComment(input:$input){unminimizedComment{isMinimized}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subjectId": f.comment.NodeID}
		},
	},
	{
		name: "lockLockable",
		doc:  `mutation($input:LockLockableInput!){lockLockable(input:$input){lockedRecord{locked}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"lockableId": f.issue.NodeID, "lockReason": "SPAM"}
		},
	},
	{
		name: "unlockLockable",
		doc:  `mutation($input:UnlockLockableInput!){unlockLockable(input:$input){unlockedRecord{locked}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"lockableId": f.issue.NodeID}
		},
	},
	{
		name: "createPullRequest",
		doc:  `mutation($input:CreatePullRequestInput!){createPullRequest(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"title":        "from the table",
				"headRefName":  "spare",
				"baseRefName":  "main",
			}
		},
	},
	{
		name: "addPullRequestReview",
		doc:  `mutation($input:AddPullRequestReviewInput!){addPullRequestReview(input:$input){pullRequestReview{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID, "event": "COMMENT", "body": "from the table"}
		},
	},
	{
		name: "submitPullRequestReview",
		doc:  `mutation($input:SubmitPullRequestReviewInput!){submitPullRequestReview(input:$input){pullRequestReview{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestReviewId": f.reviewNodeID, "event": "APPROVE"}
		},
	},
	{
		name: "dismissPullRequestReview",
		doc:  `mutation($input:DismissPullRequestReviewInput!){dismissPullRequestReview(input:$input){pullRequestReview{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestReviewId": f.reviewNodeID, "message": "stale"}
		},
	},
	{
		name: "closePullRequest",
		doc:  `mutation($input:ClosePullRequestInput!){closePullRequest(input:$input){pullRequest{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "reopenPullRequest",
		doc:  `mutation($input:ReopenPullRequestInput!){reopenPullRequest(input:$input){pullRequest{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "updatePullRequest",
		doc:  `mutation($input:UpdatePullRequestInput!){updatePullRequest(input:$input){pullRequest{title}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID, "title": "from the table"}
		},
	},
	{
		name: "markPullRequestReadyForReview",
		doc:  `mutation($input:MarkPullRequestReadyForReviewInput!){markPullRequestReadyForReview(input:$input){pullRequest{isDraft}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "convertPullRequestToDraft",
		doc:  `mutation($input:ConvertPullRequestToDraftInput!){convertPullRequestToDraft(input:$input){pullRequest{isDraft}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "mergePullRequest",
		doc:  `mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "enablePullRequestAutoMerge",
		doc:  `mutation($input:EnablePullRequestAutoMergeInput!){enablePullRequestAutoMerge(input:$input){pullRequest{autoMergeRequest{mergeMethod}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID, "mergeMethod": "SQUASH"}
		},
		// Enabling auto-merge requires the repo to allow it and the PR to be
		// blocked from merging right now (a clean PR is refused).
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			owner, name, _ := store.SplitRepoFullName(f.repo.FullName)
			s.store.UpdateRepo(owner, name, func(r *store.Repo) { r.AllowAutoMerge = true })
			s.setBranchProtection(f.repo, "main", &store.BranchProtection{
				RequiredStatusChecks: &store.BPStatusChecks{Contexts: []string{"ci"}},
				EnforceAdmins:        &store.BPEnforceAdmins{Enabled: true},
			})
		},
	},
	{
		name: "disablePullRequestAutoMerge",
		doc:  `mutation($input:DisablePullRequestAutoMergeInput!){disablePullRequestAutoMerge(input:$input){pullRequest{autoMergeRequest{mergeMethod}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
		// Disabling requires an armed request to disarm.
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			t.Helper()
			s.store.UpdatePullRequest(f.pr.ID, func(p *store.PullRequest) {
				p.AutoMerge = &store.PullRequestAutoMerge{
					EnabledByID: f.owner.ID,
					MergeMethod: "MERGE",
					EnabledAt:   fixedTestTime.UTC(),
				}
			})
		},
	},
	{
		name: "resolveReviewThread",
		doc:  `mutation($input:ResolveReviewThreadInput!){resolveReviewThread(input:$input){thread{isResolved}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"threadId": f.threadNodeID}
		},
	},
	{
		name: "unresolveReviewThread",
		doc:  `mutation($input:UnresolveReviewThreadInput!){unresolveReviewThread(input:$input){thread{isResolved}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"threadId": f.threadNodeID}
		},
	},
}

func TestGraphQLMutationsRefuseAnUnrelatedAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// A fixture per case: several of these mutations destroy what the next one
	// addresses, and a shared fixture would let the first success mask every
	// refusal after it.
	for _, tc := range gqlMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "stranger-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: an account with no access to the repository was served: %v", tc.name, env)
		}
		// The refusal has to be a refusal, not an error reported after the
		// write landed.
		s.assertGQLFixtureUntouched(t, tc.name, f)
	}
}

func (s *isolatedServer) assertGQLFixtureUntouched(t *testing.T, what string, f *gqlAuthzFixture) {
	t.Helper()
	st := s.store
	if st.GetRepoByFullName(f.repo.FullName) == nil {
		t.Errorf("%s: the repository was deleted by a stranger", what)
		return
	}
	issue := st.GetIssue(f.issue.ID)
	switch {
	case issue == nil:
		t.Errorf("%s: the issue disappeared", what)
	case issue.State != "OPEN":
		t.Errorf("%s: issue state = %q, want OPEN", what, issue.State)
	case issue.Title != "fixture issue":
		t.Errorf("%s: issue title = %q, want the seeded title", what, issue.Title)
	case issue.Locked:
		t.Errorf("%s: the issue was locked by a stranger", what)
	case issue.RepoID != f.repo.ID:
		t.Errorf("%s: the issue was transferred by a stranger", what)
	case issue.PinnedAt != nil:
		t.Errorf("%s: the issue was pinned by a stranger", what)
	}
	if c := st.GetComment(f.comment.ID); c == nil || c.MinimizedReason != "" {
		t.Errorf("%s: the comment was moderated by a stranger: %+v", what, c)
	}
	if d := st.GetDiscussion(f.discussion.ID); d == nil || d.Deleted || d.Title != "fixture discussion" || len(d.UpvoterIDs) != 0 {
		t.Errorf("%s: the discussion was changed by a stranger: %+v", what, d)
	}
	if dc := st.GetDiscussionComment(f.discComment.ID); dc == nil || dc.IsAnswer || dc.Body != "fixture answer" {
		t.Errorf("%s: the discussion comment was changed by a stranger: %+v", what, dc)
	}
	pr := st.GetPullRequest(f.pr.ID)
	switch {
	case pr == nil:
		t.Errorf("%s: the pull request disappeared", what)
	case pr.State != "OPEN":
		t.Errorf("%s: pull request state = %q, want OPEN", what, pr.State)
	case pr.Title != "fixture pr":
		t.Errorf("%s: pull request title = %q, want the seeded title", what, pr.Title)
	}
	if thread := st.PRReviewComments.GetThread(parsedThreadID(t, f.threadNodeID)); thread == nil || thread.IsResolved {
		t.Errorf("%s: the review thread was resolved by a stranger: %+v", what, thread)
	}
}

func parsedThreadID(t *testing.T, nodeID string) int {
	t.Helper()
	id, ok := store.ParsePRReviewThreadNodeID(nodeID)
	if !ok {
		t.Fatalf("thread node id %q does not parse", nodeID)
	}
	return id
}

// TestGraphQLMutationsStillServeTheirEntitledCaller is the positive half: the
// same table, driven by the repository's owner against a fresh fixture each
// time, must succeed. A guard that refuses everybody is not a fix.
func TestGraphQLMutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "owner-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the repository owner was refused: %v", tc.name, errs)
		}
	}
}

// TestGraphQLReadLevelMutationsServeAnOutsideContributor guards the other
// direction: filing an issue, commenting, proposing a pull request and
// reviewing one are how outside contributors participate on a public
// repository, and demanding push for them would wall off the contribution path
// the REST surface deliberately keeps open.
func TestGraphQLReadLevelMutationsServeAnOutsideContributor(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "outside", false)

	for _, tc := range gqlMutationCases {
		switch tc.name {
		case "createIssue", "addComment", "createPullRequest", "addPullRequestReview":
		default:
			continue
		}
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: an outside contributor was refused on a public repository: %v", tc.name, errs)
		}
	}
}

// TestGraphQLDiscussionAnswerMutationsRequireAViewer covers the two mutations
// that skipped authentication outright. The HTTP endpoint refuses an anonymous
// request at the door, so the resolvers are driven through the schema directly
// with a context carrying no viewer — which is the state an unauthenticated
// path would hand them.
func TestGraphQLDiscussionAnswerMutationsRequireAViewer(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "anon", true)

	for _, doc := range []string{
		`mutation($input:MarkDiscussionCommentAsAnswerInput!){markDiscussionCommentAsAnswer(input:$input){discussion{id}}}`,
		`mutation($input:UnmarkDiscussionCommentAsAnswerInput!){unmarkDiscussionCommentAsAnswer(input:$input){discussion{id}}}`,
	} {
		result := graphql.Do(graphql.Params{
			Schema:        s.graphql.Schema(),
			RequestString: doc,
			VariableValues: map[string]interface{}{
				"input": map[string]interface{}{"id": f.discComment.NodeID},
			},
			Context: context.Background(),
		})
		if len(result.Errors) == 0 {
			t.Errorf("a viewerless caller was served: %v", result.Data)
		}
	}
	if dc := s.store.GetDiscussionComment(f.discComment.ID); dc == nil || dc.IsAnswer {
		t.Errorf("a viewerless caller marked the answer: %+v", dc)
	}

	// And the endpoint itself still answers 401 without a credential.
	resp := s.post(t, "/api/graphql", "", map[string]interface{}{
		"query": `mutation{markDiscussionCommentAsAnswer(input:{id:"x"}){discussion{id}}}`,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous /api/graphql status = %d, want 401", resp.StatusCode)
	}
}

// TestGraphQLMergePullRequestHonoursExpectedHeadOid covers the
// --match-head-commit interlock. The argument was accepted and ignored, so a
// client that named the commit it had reviewed merged whatever had landed since.
func TestGraphQLMergePullRequestHonoursExpectedHeadOid(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	doc := `mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`

	stale := newGQLAuthzFixture(t, s.Server, "headoid-stale", true)
	env := s.gqlAuthzPost(t, stale.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"pullRequestId":   stale.pr.NodeID,
			"expectedHeadOid": "0000000000000000000000000000000000000000",
		},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("a stale expectedHeadOid merged anyway: %v", env)
	}
	if pr := s.store.GetPullRequest(stale.pr.ID); pr == nil || pr.State != "OPEN" {
		t.Errorf("pull request state after the refused merge = %+v, want OPEN", pr)
	}

	fresh := newGQLAuthzFixture(t, s.Server, "headoid-fresh", true)
	env = s.gqlAuthzPost(t, fresh.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"pullRequestId":   fresh.pr.NodeID,
			"expectedHeadOid": fresh.headSHA,
		},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the correct expectedHeadOid was refused: %v", errs)
	}
	if pr := s.store.GetPullRequest(fresh.pr.ID); pr == nil || pr.State != "MERGED" {
		t.Errorf("pull request state after the accepted merge = %+v, want MERGED", pr)
	}
}

// TestGraphQLMergePullRequestEnforcesRequiredChecks covers the merge path a
// branch-protection admin bypass opens. canMergePullRequest returns true for a
// repository admin whenever enforce_admins is off, so a resolver that leans on
// it alone merges a red pull request the REST endpoint refuses — GraphQL is
// then still a way around REST, which is the whole point of closing this lane.
// The caller here is the repository's owner, i.e. an admin.
func TestGraphQLMergePullRequestEnforcesRequiredChecks(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "requiredchecks", true)
	repoPath := "/api/v3/repos/" + f.repo.FullName
	resp := s.put(t, repoPath+"/branches/main/protection", f.ownerToken, map[string]interface{}{
		"required_status_checks": map[string]interface{}{"strict": true, "contexts": []string{"ci"}},
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("protecting main = %d", resp.StatusCode)
	}
	resp.Body.Close()

	doc := `mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`
	vars := map[string]interface{}{"input": map[string]interface{}{"pullRequestId": f.pr.NodeID}}

	env := s.gqlAuthzPost(t, f.ownerToken, doc, vars)
	errs := gqlAuthzErrors(env)
	if len(errs) == 0 {
		t.Errorf("a red required check did not stop the merge: %v", env)
	} else if !strings.Contains(fmt.Sprint(errs[0]), "Required status check") {
		t.Errorf("merge refusal = %v, want the required-status-check message", errs[0])
	}
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || pr.State != "OPEN" {
		t.Fatalf("pull request state after the refused merge = %+v, want OPEN", pr)
	}

	// Turn the required check green and the same caller merges.
	resp = s.post(t, repoPath+"/check-runs", f.ownerToken, map[string]interface{}{
		"name":       "ci",
		"head_sha":   f.headSHA,
		"status":     "completed",
		"conclusion": "success",
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("creating the check run = %d", resp.StatusCode)
	}
	resp.Body.Close()

	env = s.gqlAuthzPost(t, f.ownerToken, doc, vars)
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("a green required check still blocked the merge: %v", errs)
	}
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || pr.State != "MERGED" {
		t.Errorf("pull request state after the permitted merge = %+v, want MERGED", pr)
	}
}

// TestGraphQLUpdateIssueAppliesItsTriageArguments covers the payload that
// reported success for work it had not done: labelIds, assigneeIds and
// milestoneId were accepted, dropped, and the unchanged issue returned as if
// they had been applied.
func TestGraphQLUpdateIssueAppliesItsTriageArguments(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "triage", true)
	doc := `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`

	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"id":          f.issue.NodeID,
			"labelIds":    []interface{}{f.label.NodeID},
			"assigneeIds": []interface{}{f.stranger.NodeID},
			"milestoneId": f.milestone.NodeID,
		},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateIssue refused its own arguments: %v", errs)
	}
	issue := s.store.GetIssue(f.issue.ID)
	if issue == nil {
		t.Fatalf("issue disappeared")
	}
	if len(issue.LabelIDs) != 1 || issue.LabelIDs[0] != f.label.ID {
		t.Errorf("issue labels = %v, want [%d]", issue.LabelIDs, f.label.ID)
	}
	if len(issue.AssigneeIDs) != 1 || issue.AssigneeIDs[0] != f.stranger.ID {
		t.Errorf("issue assignees = %v, want [%d]", issue.AssigneeIDs, f.stranger.ID)
	}
	if issue.MilestoneID != f.milestone.ID {
		t.Errorf("issue milestone = %d, want %d", issue.MilestoneID, f.milestone.ID)
	}

	// An id that names nothing is refused rather than dropped.
	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"id":       f.issue.NodeID,
			"labelIds": []interface{}{"LA_kgDOnosuchlabel"},
		},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("an unresolvable labelId was accepted: %v", env)
	}
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || len(issue.LabelIDs) != 1 {
		t.Errorf("the refused update still changed the labels: %+v", issue)
	}
}

// TestGraphQLUpdateIssueValidatesState covers the free-form string that was
// upper-cased and written straight into the store while the IssueState enum sat
// unused in the same file.
func TestGraphQLUpdateIssueValidatesState(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "state", true)
	doc := `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`

	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"id": f.issue.NodeID, "state": "banana"},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("an invalid IssueState was accepted: %v", env)
	}
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || issue.State != "OPEN" {
		t.Errorf("issue state after the refused update = %+v, want OPEN", issue)
	}

	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"id": f.issue.NodeID, "state": "CLOSED"},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("a valid IssueState was refused: %v", errs)
	}
	issue := s.store.GetIssue(f.issue.ID)
	if issue == nil || issue.State != "CLOSED" {
		t.Fatalf("issue state after CLOSED = %+v", issue)
	}
	if issue.ClosedAt == nil || issue.StateReason != "COMPLETED" {
		t.Errorf("closing through updateIssue left the issue half-closed: %+v", issue)
	}
}

func TestGraphQLEveryMutationIsCoveredByThePolicyTable(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	schema := s.graphql.Schema()
	mutation := schema.MutationType()
	if mutation == nil {
		t.Fatalf("the schema exposes no mutation type")
	}
	fields := mutation.Fields()
	// The policy table itself lives in the graphqlapi package (ARCH-003).
	// TestMutationAuthzTableMatchesSchema there asserts schema fields and
	// table rows are identical sets, so iterating the schema's mutation
	// fields here iterates exactly the table's rows.
	if len(fields) == 0 {
		t.Fatalf("the schema exposes no mutations")
	}
	// Every mutation whose subject is an existing repository or project must
	// be exercised by a refusal case in one of the two tables above, so a
	// mutation cannot be authorized on paper and untested in practice.
	// createRepository has no such subject — its entitlement is over an
	// account — and is covered by the account-scoped cases instead. The
	// graphqlapi-side TestMutationAuthzAccountScopedRowsArePinned pins that
	// createRepository is the only account-scoped row, so this exemption
	// list cannot drift silently.
	inCases := map[string]bool{"createRepository": true}
	for _, tc := range gqlMutationCases {
		inCases[tc.name] = true
	}
	for _, tc := range gqlProjectMutationCases {
		inCases[tc.name] = true
	}
	for name := range fields {
		if inCases[name] {
			continue
		}
		t.Errorf("mutation %s is authorized but no refusal case exercises it", name)
	}
}

// --- Projects v2 ---
//
// A project is owned by a user or an organization, so the repository predicates
// say nothing about it. These four mutations authenticated and then acted: any
// signed-in account created projects under anybody's name, added items to
// anybody's project, and — because write access to a project was never read
// access to what went into it — pulled a stranger's private issue into a
// project as a way of reading its title.

type gqlProjectAuthzFixture struct {
	owner         *store.User
	ownerToken    string
	stranger      *store.User
	strangerToken string
	project       *store.ProjectV2
	item          *store.ProjectV2Item
	field         *store.ProjectV2Field
	issue         *store.Issue
	spareIssue    *store.Issue
	strangerIssue *store.Issue
	org           *store.Org
}

func (s *isolatedServer) newGQLProjectAuthzFixture(t *testing.T, tag string) *gqlProjectAuthzFixture {
	t.Helper()
	st := s.store
	now := fixedTestTime.UTC()

	mkUser := func(login string) *store.User {
		st.Mu.Lock()
		defer st.Mu.Unlock()
		u := &store.User{
			ID:        st.NextUser,
			NodeID:    fmt.Sprintf("U_pauthz%08d", st.NextUser),
			Login:     login,
			Type:      "User",
			CreatedAt: now,
			UpdatedAt: now,
		}
		st.Users[u.ID] = u
		st.UsersByLogin[u.Login] = u
		st.NextUser++
		return u
	}

	f := &gqlProjectAuthzFixture{}
	f.owner = mkUser("gqlpauthz-owner-" + tag)
	f.stranger = mkUser("gqlpauthz-stranger-" + tag)

	ownerRepo := st.CreateRepo(f.owner, "gqlpauthz-repo", "", false)
	strangerRepo := st.CreateRepo(f.stranger, "gqlpauthz-secret", "", true)
	if ownerRepo == nil || strangerRepo == nil {
		t.Fatalf("fixture %s: could not create the repositories", tag)
	}
	f.issue = st.CreateIssue(ownerRepo.ID, f.owner.ID, "project fixture issue", "", nil, nil, 0)
	// A second readable issue, because AddItem is idempotent per content: a
	// case that re-adds the item the fixture already holds cannot tell a
	// refusal from a duplicate.
	f.spareIssue = st.CreateIssue(ownerRepo.ID, f.owner.ID, "project spare issue", "", nil, nil, 0)
	f.strangerIssue = st.CreateIssue(strangerRepo.ID, f.stranger.ID, "not yours", "", nil, nil, 0)

	f.project = st.ProjectsV2.CreateProject(f.owner.ID, "User", "fixture project", f.owner.ID)
	if f.project == nil {
		t.Fatalf("fixture %s: could not create the project", tag)
	}
	f.item = st.ProjectsV2.AddItem(f.project.ID, "Issue", f.issue.ID, f.owner.ID)
	f.field = st.ProjectsV2.CreateField(f.project.ID, "Notes", store.ProjectV2FieldText, nil, nil)
	if f.item == nil || f.field == nil {
		t.Fatalf("fixture %s: could not seed the project item or field", tag)
	}

	f.org = st.CreateOrg(f.owner, "gqlpauthz-org-"+tag, "Project Org", "")
	if f.org == nil {
		t.Fatalf("fixture %s: could not create the organization", tag)
	}

	ownerTok := st.CreateToken(f.owner.ID, "repo, project")
	strangerTok := st.CreateToken(f.stranger.ID, "repo, project")
	if ownerTok == nil || strangerTok == nil {
		t.Fatalf("fixture %s: could not mint tokens", tag)
	}
	f.ownerToken = ownerTok.Value
	f.strangerToken = strangerTok.Value
	return f
}

type gqlProjectMutationCase struct {
	name  string
	doc   string
	input func(f *gqlProjectAuthzFixture) map[string]interface{}
}

var gqlProjectMutationCases = []gqlProjectMutationCase{
	{
		name: "createProjectV2",
		doc:  `mutation($input:CreateProjectV2Input!){createProjectV2(input:$input){projectV2{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"ownerId": f.owner.NodeID, "title": "from the table"}
		},
	},
	{
		name: "addProjectV2ItemById",
		doc:  `mutation($input:AddProjectV2ItemByIdInput!){addProjectV2ItemById(input:$input){item{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "contentId": f.spareIssue.NodeID}
		},
	},
	{
		name: "deleteProjectV2Item",
		doc:  `mutation($input:DeleteProjectV2ItemInput!){deleteProjectV2Item(input:$input){deletedItemId}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "itemId": f.item.NodeID}
		},
	},
	{
		name: "createProjectV2Field",
		doc:  `mutation($input:CreateProjectV2FieldInput!){createProjectV2Field(input:$input){projectV2Field{... on ProjectV2FieldCommon{id}}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"projectId": f.project.NodeID, "dataType": "TEXT", "name": "from the table"}
		},
	},
	{
		name: "updateProjectV2ItemFieldValue",
		doc:  `mutation($input:UpdateProjectV2ItemFieldValueInput!){updateProjectV2ItemFieldValue(input:$input){projectV2Item{id}}}`,
		input: func(f *gqlProjectAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"projectId": f.project.NodeID,
				"itemId":    f.item.NodeID,
				"fieldId":   f.field.NodeID,
				"value":     map[string]interface{}{"text": "from the table"},
			}
		},
	},
}

func TestGraphQLProjectV2MutationsRefuseAnUnrelatedAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	for _, tc := range gqlProjectMutationCases {
		f := s.newGQLProjectAuthzFixture(t, "stranger-"+tc.name)
		projects := len(st.ProjectsV2.ListProjectsForOwner(f.owner.ID, "User"))
		items := len(st.ProjectsV2.ListItemsForProject(f.project.ID))
		fields := len(st.ProjectsV2.FieldsForProject(f.project.ID))
		values := len(st.ProjectsV2.GetItem(f.item.ID).FieldValues)

		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: an account with no access to the project was served: %v", tc.name, env)
		}

		if got := len(st.ProjectsV2.ListProjectsForOwner(f.owner.ID, "User")); got != projects {
			t.Errorf("%s: owner project count %d → %d", tc.name, projects, got)
		}
		if got := len(st.ProjectsV2.ListItemsForProject(f.project.ID)); got != items {
			t.Errorf("%s: project item count %d → %d", tc.name, items, got)
		}
		if got := len(st.ProjectsV2.FieldsForProject(f.project.ID)); got != fields {
			t.Errorf("%s: project field count %d → %d", tc.name, fields, got)
		}
		if got := len(st.ProjectsV2.GetItem(f.item.ID).FieldValues); got != values {
			t.Errorf("%s: item field-value count %d → %d", tc.name, values, got)
		}
	}
}

func TestGraphQLProjectV2MutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlProjectMutationCases {
		f := s.newGQLProjectAuthzFixture(t, "owner-"+tc.name)
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the project's owner was refused: %v", tc.name, errs)
		}
	}
}

// TestGraphQLCreateProjectV2HonoursOrganizationMembership covers the owner
// branch the user-owned cases cannot reach: a project under an organization is
// for its members, and membership is what decides — not merely holding a token.
func TestGraphQLCreateProjectV2HonoursOrganizationMembership(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLProjectAuthzFixture(t, "orgowner")
	doc := `mutation($input:CreateProjectV2Input!){createProjectV2(input:$input){projectV2{id}}}`
	input := map[string]interface{}{"ownerId": f.org.NodeID, "title": "org project"}

	env := s.gqlAuthzPost(t, f.strangerToken, doc, map[string]interface{}{"input": input})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("a non-member created a project under the organization: %v", env)
	}
	if got := len(s.store.ProjectsV2.ListProjectsForOwner(f.org.ID, "Organization")); got != 0 {
		t.Fatalf("organization project count after the refusal = %d, want 0", got)
	}

	// The organization's admin, who created it, still may.
	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("an organization admin was refused: %v", errs)
	}
	if got := len(s.store.ProjectsV2.ListProjectsForOwner(f.org.ID, "Organization")); got != 1 {
		t.Errorf("organization project count after the accepted create = %d, want 1", got)
	}
}

// TestGraphQLAddProjectV2ItemRequiresReadingTheContent covers the second half of
// that mutation: the caller owns the project, so project write is satisfied,
// but the content belongs to a private repository they cannot read. Adding it
// would republish its title through the project.
func TestGraphQLAddProjectV2ItemRequiresReadingTheContent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLProjectAuthzFixture(t, "content")
	doc := `mutation($input:AddProjectV2ItemByIdInput!){addProjectV2ItemById(input:$input){item{id}}}`

	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"projectId": f.project.NodeID, "contentId": f.strangerIssue.NodeID},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("an unreadable issue was pulled into a project: %v", env)
	}
	if got := len(s.store.ProjectsV2.ListItemsForIssue(f.strangerIssue.ID)); got != 0 {
		t.Errorf("the private issue is indexed against %d project items, want 0", got)
	}

	// Content the caller can read still goes in.
	second := s.store.CreateIssue(f.issue.RepoID, f.owner.ID, "another readable issue", "", nil, nil, 0)
	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"projectId": f.project.NodeID, "contentId": second.NodeID},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("readable content was refused: %v", errs)
	}
}

// TestGraphQLDeleteProjectV2ItemRemovesTheItem covers the mutation backing
// `gh project item-delete`: the owner deletes an item, the payload echoes the
// item's node id, and the item is gone from the project afterwards. An itemId
// naming an item outside the addressed project is not found.
func TestGraphQLDeleteProjectV2ItemRemovesTheItem(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLProjectAuthzFixture(t, "delitem")
	doc := `mutation($input:DeleteProjectV2ItemInput!){deleteProjectV2Item(input:$input){deletedItemId}}`

	// An item that belongs to another project is not this project's to delete.
	other := s.store.ProjectsV2.CreateProject(f.owner.ID, "User", "other", f.owner.ID)
	otherItem := s.store.ProjectsV2.AddItem(other.ID, "Issue", f.issue.ID, f.owner.ID)
	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"projectId": f.project.NodeID, "itemId": otherItem.NodeID},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("an item from a different project was deletable: %v", env)
	}
	if s.store.ProjectsV2.GetItem(otherItem.ID) == nil {
		t.Errorf("the other project's item was deleted through the wrong project")
	}

	// The owner deletes their own item; the payload echoes its node id.
	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"projectId": f.project.NodeID, "itemId": f.item.NodeID},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the owner was refused deleting their own item: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	payload, _ := data["deleteProjectV2Item"].(map[string]interface{})
	if got := payload["deletedItemId"]; got != f.item.NodeID {
		t.Errorf("deletedItemId = %v, want %q", got, f.item.NodeID)
	}
	if s.store.ProjectsV2.GetItem(f.item.ID) != nil {
		t.Errorf("the item survived the delete mutation")
	}
}

// TestGraphQLSubmitAndDismissPullRequestReview covers the review-lifecycle
// mutations end to end: a pending review is submitted (APPROVE → APPROVED) and
// then dismissed (→ DISMISSED with the message).
func TestGraphQLSubmitAndDismissPullRequestReview(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "gql-review-lifecycle")
	prJSON := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/admin/gql-review-lifecycle/pulls", defaultToken, map[string]interface{}{
		"title": "lifecycle", "head": "feat", "base": "main",
	}), 201)
	prID := int(prJSON["id"].(float64))
	admin := s.store.UsersByLogin["admin"]
	review := s.store.CreatePRReview(prID, admin.ID, "PENDING", "pending")
	if review == nil {
		t.Fatal("could not seed pending review")
	}

	// Submit the pending review as an approval.
	data := s.gqlData(t, `mutation($input:SubmitPullRequestReviewInput!){submitPullRequestReview(input:$input){pullRequestReview{state}}}`,
		map[string]interface{}{"input": map[string]interface{}{"pullRequestReviewId": review.NodeID, "event": "APPROVE"}})
	submitted, _ := data["submitPullRequestReview"].(map[string]interface{})["pullRequestReview"].(map[string]interface{})
	if submitted["state"] != "APPROVED" {
		t.Errorf("submitted review state = %v, want APPROVED", submitted["state"])
	}

	// Dismiss it.
	data = s.gqlData(t, `mutation($input:DismissPullRequestReviewInput!){dismissPullRequestReview(input:$input){pullRequestReview{state}}}`,
		map[string]interface{}{"input": map[string]interface{}{"pullRequestReviewId": review.NodeID, "message": "no longer relevant"}})
	dismissed, _ := data["dismissPullRequestReview"].(map[string]interface{})["pullRequestReview"].(map[string]interface{})
	if dismissed["state"] != "DISMISSED" {
		t.Errorf("dismissed review state = %v, want DISMISSED", dismissed["state"])
	}
}
