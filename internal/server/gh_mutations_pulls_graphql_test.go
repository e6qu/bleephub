package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// The refusal and entitled halves for the pull-request mutation surface.
//
// The repository-scoped rows ride the shared gqlAuthzFixture; updateTeamReviewAssignment
// names a team rather than a repository, so it rides the issue family's
// organization fixture, where the refusing caller owns a different
// organization.

var gqlPullMutationCases = []gqlMutationCase{
	{
		name: "addPullRequestReviewComment",
		doc:  `mutation($input:AddPullRequestReviewCommentInput!){addPullRequestReviewComment(input:$input){comment{body} commentEdge{cursor}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"pullRequestId": f.pr.NodeID, "body": "a note", "path": "README.md", "position": 1,
			}
		},
	},
	{
		name: "addPullRequestReviewThread",
		doc:  `mutation($input:AddPullRequestReviewThreadInput!){addPullRequestReviewThread(input:$input){thread{id}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"pullRequestId": f.pr.NodeID, "body": "a thread", "path": "README.md", "line": 2,
			}
		},
	},
	{
		name: "addPullRequestReviewThreadReply",
		doc:  `mutation($input:AddPullRequestReviewThreadReplyInput!){addPullRequestReviewThreadReply(input:$input){comment{body}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"pullRequestReviewThreadId": f.threadNodeID, "body": "replying",
			}
		},
	},
	{
		name: "updatePullRequestReviewComment",
		doc:  `mutation($input:UpdatePullRequestReviewCommentInput!){updatePullRequestReviewComment(input:$input){pullRequestReviewComment{body}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"pullRequestReviewCommentId": f.reviewCommentNodeID, "body": "edited",
			}
		},
	},
	{
		name: "deletePullRequestReviewComment",
		doc:  `mutation($input:DeletePullRequestReviewCommentInput!){deletePullRequestReviewComment(input:$input){pullRequestReviewComment{body}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.reviewCommentNodeID}
		},
	},
	{
		name: "updatePullRequestReview",
		doc:  `mutation($input:UpdatePullRequestReviewInput!){updatePullRequestReview(input:$input){pullRequestReview{body}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestReviewId": f.reviewNodeID, "body": "revised"}
		},
	},
	{
		name: "deletePullRequestReview",
		doc:  `mutation($input:DeletePullRequestReviewInput!){deletePullRequestReview(input:$input){pullRequestReview{state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestReviewId": f.reviewNodeID}
		},
	},
	{
		name: "requestReviews",
		doc:  `mutation($input:RequestReviewsInput!){requestReviews(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"pullRequestId": f.pr.NodeID, "userIds": []interface{}{f.stranger.NodeID},
			}
		},
	},
	{
		name: "requestReviewsByLogin",
		doc:  `mutation($input:RequestReviewsByLoginInput!){requestReviewsByLogin(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"pullRequestId": f.pr.NodeID, "userLogins": []interface{}{f.stranger.Login},
			}
		},
	},
	{
		name: "markFileAsViewed",
		doc:  `mutation($input:MarkFileAsViewedInput!){markFileAsViewed(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID, "path": "README.md"}
		},
	},
	{
		name: "unmarkFileAsViewed",
		doc:  `mutation($input:UnmarkFileAsViewedInput!){unmarkFileAsViewed(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID, "path": "README.md"}
		},
	},
	{
		name: "updatePullRequestBranch",
		doc:  `mutation($input:UpdatePullRequestBranchInput!){updatePullRequestBranch(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "archivePullRequest",
		doc:  `mutation($input:ArchivePullRequestInput!){archivePullRequest(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "unarchivePullRequest",
		doc:  `mutation($input:UnarchivePullRequestInput!){unarchivePullRequest(input:$input){pullRequest{number}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "enqueuePullRequest",
		doc:  `mutation($input:EnqueuePullRequestInput!){enqueuePullRequest(input:$input){mergeQueueEntry{position state}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"pullRequestId": f.pr.NodeID}
		},
	},
	{
		name: "dequeuePullRequest",
		doc:  `mutation($input:DequeuePullRequestInput!){dequeuePullRequest(input:$input){mergeQueueEntry{position}}}`,
		setup: func(t *testing.T, s *isolatedServer, f *gqlAuthzFixture) {
			if s.store.EnqueuePullRequest(f.pr.ID, false) == nil {
				t.Fatalf("could not queue the fixture pull request")
			}
		},
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.pr.NodeID}
		},
	},
	{
		name: "addPullRequestCreationCapBypassUsers",
		doc:  `mutation($input:AddPullRequestCreationCapBypassUsersInput!){addPullRequestCreationCapBypassUsers(input:$input){repository{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID, "userIds": []interface{}{f.stranger.NodeID},
			}
		},
	},
	{
		name: "removePullRequestCreationCapBypassUsers",
		doc:  `mutation($input:RemovePullRequestCreationCapBypassUsersInput!){removePullRequestCreationCapBypassUsers(input:$input){repository{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID, "userIds": []interface{}{f.stranger.NodeID},
			}
		},
	},
}

func TestGraphQLPullMutationsRefuseAnUnrelatedAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlPullMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "pull-stranger-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: an account with no access to the repository was served: %v", tc.name, env)
		}
		pr := s.store.GetPullRequest(f.pr.ID)
		switch {
		case pr == nil:
			t.Errorf("%s: the pull request disappeared", tc.name)
		case pr.Archived:
			t.Errorf("%s: the pull request was archived by a stranger", tc.name)
		case len(pr.RequestedReviewerIDs) != 0:
			t.Errorf("%s: a reviewer was requested by a stranger: %v", tc.name, pr.RequestedReviewerIDs)
		case len(pr.ViewedFiles) != 0:
			t.Errorf("%s: a file was marked viewed by a stranger: %v", tc.name, pr.ViewedFiles)
		}
		if len(s.store.PRCreationBypassUsers(f.repo.FullName)) != 0 {
			t.Errorf("%s: the bypass list was changed by a stranger", tc.name)
		}
	}
}

func TestGraphQLPullMutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlPullMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "pull-owner-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the repository owner was refused: %v", tc.name, errs)
		}
	}
}

func TestGraphQLUpdateTeamReviewAssignment(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLIssueOrgFixture(t, s, "team-review")
	team := s.store.CreateTeam(f.org.Login, "reviewers", store.TeamOptions{Privacy: store.TeamPrivacyClosed, Permission: store.TeamPermissionPull})
	if team == nil {
		t.Fatalf("could not create the team")
	}
	doc := `mutation($input:UpdateTeamReviewAssignmentInput!){updateTeamReviewAssignment(input:$input){team{slug}}}`
	input := map[string]interface{}{
		"id": team.NodeID, "enabled": true, "algorithm": "LOAD_BALANCE", "teamMemberCount": 2,
		"excludedTeamMemberIds": []interface{}{f.owner.NodeID},
	}

	env := s.gqlAuthzPost(t, f.strangerToken, doc, map[string]interface{}{"input": input})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("the owner of another organization was served: %v", env)
	}
	if stored := s.store.GetTeamByID(team.ID); stored == nil || stored.ReviewAssignment != nil {
		t.Fatalf("the assignment was written by a stranger: %+v", stored)
	}

	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the organization's owner was refused: %v", errs)
	}
	stored := s.store.GetTeamByID(team.ID)
	switch {
	case stored == nil || stored.ReviewAssignment == nil:
		t.Fatalf("the assignment was not written: %+v", stored)
	case !stored.ReviewAssignment.Enabled:
		t.Errorf("the assignment is not enabled")
	case stored.ReviewAssignment.Algorithm != "LOAD_BALANCE":
		t.Errorf("algorithm = %q", stored.ReviewAssignment.Algorithm)
	case stored.ReviewAssignment.TeamMemberCount != 2:
		t.Errorf("member count = %d", stored.ReviewAssignment.TeamMemberCount)
	case len(stored.ReviewAssignment.ExcludedTeamMemberIDs) != 1:
		t.Errorf("excluded members = %v", stored.ReviewAssignment.ExcludedTeamMemberIDs)
	}
}

// behavioural

func TestGraphQLReviewCommentMutationsWriteTheStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "review-comments", false)

	post := func(doc string, input map[string]interface{}) map[string]interface{} {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
		return env
	}

	env := post(`mutation($input:AddPullRequestReviewThreadInput!){addPullRequestReviewThread(input:$input){thread{id path line comments(first:5){nodes{body}}}}}`,
		map[string]interface{}{
			"pullRequestId": f.pr.NodeID, "body": "please rename this", "path": "main.go", "line": 12,
		})
	threadID := nestedString(t, env, "data", "addPullRequestReviewThread", "thread", "id")
	rootID, ok := store.ParsePRReviewThreadNodeID(threadID)
	if !ok {
		t.Fatalf("thread id %q does not parse", threadID)
	}
	root := s.store.PRReviewComments.Get(rootID)
	if root == nil || root.Body != "please rename this" || root.Path != "main.go" {
		t.Fatalf("addPullRequestReviewThread stored nothing usable: %+v", root)
	}

	post(`mutation($input:AddPullRequestReviewThreadReplyInput!){addPullRequestReviewThreadReply(input:$input){comment{body}}}`,
		map[string]interface{}{"pullRequestReviewThreadId": threadID, "body": "agreed"})
	thread := s.store.PRReviewComments.GetThread(rootID)
	if thread == nil || len(thread.Comments) != 2 {
		t.Fatalf("the reply did not join the thread: %+v", thread)
	}

	reply := thread.Comments[1]
	post(`mutation($input:UpdatePullRequestReviewCommentInput!){updatePullRequestReviewComment(input:$input){pullRequestReviewComment{body}}}`,
		map[string]interface{}{"pullRequestReviewCommentId": reply.NodeID, "body": "on reflection, agreed"})
	if updated := s.store.PRReviewComments.Get(reply.ID); updated == nil || updated.Body != "on reflection, agreed" {
		t.Fatalf("the reply body was not written: %+v", updated)
	}

	post(`mutation($input:DeletePullRequestReviewCommentInput!){deletePullRequestReviewComment(input:$input){pullRequestReviewComment{body}}}`,
		map[string]interface{}{"id": reply.NodeID})
	if s.store.PRReviewComments.Get(reply.ID) != nil {
		t.Errorf("the reply is still stored after deletion")
	}

	// A standalone review comment on a path, and a reply to it.
	env = post(`mutation($input:AddPullRequestReviewCommentInput!){addPullRequestReviewComment(input:$input){comment{id body}}}`,
		map[string]interface{}{
			"pullRequestId": f.pr.NodeID, "body": "nit: spacing", "path": "main.go", "position": 3,
		})
	commentID := nestedString(t, env, "data", "addPullRequestReviewComment", "comment", "id")
	comment := store.FindPullRequestReviewCommentByNodeID(s.store, commentID)
	if comment == nil || comment.Body != "nit: spacing" {
		t.Fatalf("addPullRequestReviewComment stored nothing: %+v", comment)
	}
	post(`mutation($input:AddPullRequestReviewCommentInput!){addPullRequestReviewComment(input:$input){comment{body}}}`,
		map[string]interface{}{"inReplyTo": commentID, "body": "fixed"})
	if replies := s.store.PRReviewComments.GetThread(comment.ID); replies == nil || len(replies.Comments) != 2 {
		t.Fatalf("the in-reply-to comment did not join the thread: %+v", replies)
	}
}

func TestGraphQLReviewMutationsWriteTheStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "reviews", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdatePullRequestReviewInput!){updatePullRequestReview(input:$input){pullRequestReview{body}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"pullRequestReviewId": f.reviewNodeID, "body": "a revised summary",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updatePullRequestReview: %v", errs)
	}
	review := store.FindReviewByNodeID(s.store, f.reviewNodeID)
	if review == nil || review.Body != "a revised summary" {
		t.Fatalf("the review body was not written: %+v", review)
	}
	reviewID := review.ID

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeletePullRequestReviewInput!){deletePullRequestReview(input:$input){pullRequestReview{state}}}`,
		map[string]interface{}{"input": map[string]interface{}{"pullRequestReviewId": f.reviewNodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("deletePullRequestReview: %v", errs)
	}
	if s.store.GetPullRequestReview(reviewID) != nil {
		t.Errorf("the pending review is still stored after deletion")
	}
}

func TestGraphQLRequestReviewsReplacesOrUnionsTheReviewerSet(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "request-reviews", false)

	post := func(doc string, input map[string]interface{}) {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
	}

	post(`mutation($input:RequestReviewsInput!){requestReviews(input:$input){pullRequest{number} requestedReviewersEdge{cursor node{login}}}}`,
		map[string]interface{}{
			"pullRequestId": f.pr.NodeID, "userIds": []interface{}{f.stranger.NodeID},
		})
	pr := s.store.GetPullRequest(f.pr.ID)
	if pr == nil || len(pr.RequestedReviewerIDs) != 1 || pr.RequestedReviewerIDs[0] != f.stranger.ID {
		t.Fatalf("requestReviews did not request the reviewer: %+v", pr)
	}

	// Without union the set is replaced, so naming nobody clears it.
	post(`mutation($input:RequestReviewsInput!){requestReviews(input:$input){pullRequest{number}}}`,
		map[string]interface{}{"pullRequestId": f.pr.NodeID, "userIds": []interface{}{}})
	pr = s.store.GetPullRequest(f.pr.ID)
	if pr == nil || len(pr.RequestedReviewerIDs) != 0 {
		t.Fatalf("requestReviews did not replace the set: %+v", pr)
	}

	// By login, and with union so the previous request survives.
	post(`mutation($input:RequestReviewsByLoginInput!){requestReviewsByLogin(input:$input){pullRequest{number}}}`,
		map[string]interface{}{
			"pullRequestId": f.pr.NodeID, "userLogins": []interface{}{f.stranger.Login},
		})
	post(`mutation($input:RequestReviewsByLoginInput!){requestReviewsByLogin(input:$input){pullRequest{number}}}`,
		map[string]interface{}{
			"pullRequestId": f.pr.NodeID, "userLogins": []interface{}{f.owner.Login}, "union": true,
		})
	pr = s.store.GetPullRequest(f.pr.ID)
	if pr == nil || len(pr.RequestedReviewerIDs) != 2 {
		t.Fatalf("union did not add to the set: %+v", pr)
	}
}

func TestGraphQLViewedFilesAndArchivalWriteThePullRequest(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "viewed-archive", false)

	post := func(doc string, input map[string]interface{}) {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
	}

	post(`mutation($input:MarkFileAsViewedInput!){markFileAsViewed(input:$input){pullRequest{number}}}`,
		map[string]interface{}{"pullRequestId": f.pr.NodeID, "path": "main.go"})
	if viewed := s.store.PullRequestViewedFiles(f.pr.ID, f.owner.ID); len(viewed) != 1 || viewed[0] != "main.go" {
		t.Fatalf("markFileAsViewed did not record the path: %v", viewed)
	}
	// The mark is one reviewer's, not the pull request's.
	if viewed := s.store.PullRequestViewedFiles(f.pr.ID, f.stranger.ID); len(viewed) != 0 {
		t.Errorf("the mark leaked to another reviewer: %v", viewed)
	}
	post(`mutation($input:UnmarkFileAsViewedInput!){unmarkFileAsViewed(input:$input){pullRequest{number}}}`,
		map[string]interface{}{"pullRequestId": f.pr.NodeID, "path": "main.go"})
	if viewed := s.store.PullRequestViewedFiles(f.pr.ID, f.owner.ID); len(viewed) != 0 {
		t.Fatalf("unmarkFileAsViewed left the path: %v", viewed)
	}

	post(`mutation($input:ArchivePullRequestInput!){archivePullRequest(input:$input){pullRequest{number}}}`,
		map[string]interface{}{"pullRequestId": f.pr.NodeID})
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || !pr.Archived {
		t.Fatalf("archivePullRequest did not archive: %+v", pr)
	}
	post(`mutation($input:UnarchivePullRequestInput!){unarchivePullRequest(input:$input){pullRequest{number}}}`,
		map[string]interface{}{"pullRequestId": f.pr.NodeID})
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || pr.Archived {
		t.Fatalf("unarchivePullRequest did not unarchive: %+v", pr)
	}
}

func TestGraphQLMergeQueueOrdersItsEntries(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "merge-queue", false)
	second := s.store.CreatePullRequest(f.repo.ID, f.owner.ID, "second pr", "", "spare", "main", false, nil, nil, 0)
	if second == nil {
		t.Fatalf("could not seed the second pull request")
	}
	// Require a status check so the queue holds its entries (enqueuing now forms a
	// merge group and only merges once required checks pass); this test observes
	// ordering, not merging.
	s.store.Mu.Lock()
	s.store.Misc.BranchProtection[store.BpKey(f.repo.ID, "main")] = &store.BranchProtection{
		RequiredStatusChecks: &store.BPStatusChecks{Contexts: []string{"ci"}},
	}
	s.store.Mu.Unlock()

	post := func(doc string, input map[string]interface{}) map[string]interface{} {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
		return env
	}

	post(`mutation($input:EnqueuePullRequestInput!){enqueuePullRequest(input:$input){mergeQueueEntry{position}}}`,
		map[string]interface{}{"pullRequestId": f.pr.NodeID})
	env := post(`mutation($input:EnqueuePullRequestInput!){enqueuePullRequest(input:$input){mergeQueueEntry{position jump mergeQueue{entries(first:10){totalCount}}}}}`,
		map[string]interface{}{"pullRequestId": second.NodeID, "jump": true})
	if got := nestedFloat(t, env, "data", "enqueuePullRequest", "mergeQueueEntry", "position"); got != 1 {
		t.Errorf("the jumping entry landed at position %v, want 1", got)
	}
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || pr.MergeQueuePosition != 2 {
		t.Fatalf("the first entry was not shifted back: %+v", pr)
	}

	post(`mutation($input:DequeuePullRequestInput!){dequeuePullRequest(input:$input){mergeQueueEntry{position}}}`,
		map[string]interface{}{"id": second.NodeID})
	if pr := s.store.GetPullRequest(second.ID); pr == nil || pr.MergeQueuePosition != 0 {
		t.Fatalf("dequeuePullRequest did not remove the entry: %+v", pr)
	}
	if pr := s.store.GetPullRequest(f.pr.ID); pr == nil || pr.MergeQueuePosition != 1 {
		t.Fatalf("the queue did not close the gap: %+v", pr)
	}
}

func TestGraphQLCreationCapBypassListWritesTheStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "bypass", false)

	post := func(doc string, input map[string]interface{}) {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
	}

	post(`mutation($input:AddPullRequestCreationCapBypassUsersInput!){addPullRequestCreationCapBypassUsers(input:$input){repository{name}}}`,
		map[string]interface{}{"repositoryId": f.repo.NodeID, "userIds": []interface{}{f.stranger.NodeID}})
	users := s.store.PRCreationBypassUsers(f.repo.FullName)
	if len(users) != 1 || users[0].Login != f.stranger.Login {
		t.Fatalf("the bypass list was not written: %+v", users)
	}
	post(`mutation($input:RemovePullRequestCreationCapBypassUsersInput!){removePullRequestCreationCapBypassUsers(input:$input){repository{name}}}`,
		map[string]interface{}{"repositoryId": f.repo.NodeID, "userIds": []interface{}{f.stranger.NodeID}})
	if users := s.store.PRCreationBypassUsers(f.repo.FullName); len(users) != 0 {
		t.Fatalf("the bypass entry was not removed: %+v", users)
	}
}

// nestedFloat reads a numeric member out of a GraphQL response envelope.
func nestedFloat(t *testing.T, env map[string]interface{}, path ...string) float64 {
	t.Helper()
	var cursor interface{} = env
	for _, step := range path {
		object, ok := cursor.(map[string]interface{})
		if !ok {
			t.Fatalf("path %v: %q is not an object in %v", path, step, env)
		}
		cursor = object[step]
	}
	value, ok := cursor.(float64)
	if !ok {
		t.Fatalf("path %v is not a number in %v", path, env)
	}
	return value
}
