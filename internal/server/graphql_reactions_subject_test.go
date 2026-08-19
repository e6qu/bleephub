package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestGraphQLReactionsCoverNonDiscussionSubjects verifies addReaction and
// removeReaction resolve issue, pull-request and review subjects. GitHub's
// AddReactionInput.subjectId spans CommitComment/Discussion/DiscussionComment/
// Issue/IssueComment/PullRequest/PullRequestReview/PullRequestReviewComment/
// Release, and the store already backs these rows (the GraphQL read side renders
// their reactionGroups), but the resolver formerly resolved only discussions —
// so you could read a reaction count on an issue yet not add one over GraphQL.
func TestGraphQLReactionsCoverNonDiscussionSubjects(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "reactsubj", true)

	reviewID := store.FindReviewByNodeID(s.store, f.reviewNodeID).ID
	cases := []struct {
		name       string
		nodeID     string
		parentType string
		parentID   int
	}{
		{"issue", f.issue.NodeID, "issue", f.issue.ID},
		{"pull_request", f.pr.NodeID, "pull_request", f.pr.ID},
		{"pull_request_review", f.reviewNodeID, "pull_request_review", reviewID},
	}

	addDoc := `mutation($input:AddReactionInput!){addReaction(input:$input){clientMutationId}}`
	removeDoc := `mutation($input:RemoveReactionInput!){removeReaction(input:$input){clientMutationId}}`
	for _, tc := range cases {
		vars := map[string]interface{}{"input": map[string]interface{}{"subjectId": tc.nodeID, "content": "THUMBS_UP"}}

		env := s.gqlAuthzPost(t, f.ownerToken, addDoc, vars)
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: addReaction refused the owner: %v", tc.name, errs)
		}
		if got := s.store.Reactions.ListReactions(tc.parentType, tc.parentID, "+1"); len(got) != 1 {
			t.Fatalf("%s: store has %d +1 reactions on (%s,%d), want 1", tc.name, len(got), tc.parentType, tc.parentID)
		}

		env = s.gqlAuthzPost(t, f.ownerToken, removeDoc, vars)
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: removeReaction refused the owner: %v", tc.name, errs)
		}
		if got := s.store.Reactions.ListReactions(tc.parentType, tc.parentID, "+1"); len(got) != 0 {
			t.Fatalf("%s: %d +1 reactions remain after removeReaction, want 0", tc.name, len(got))
		}
	}
}

// TestGraphQLAddReactionReturnsReactionGroups verifies the addReaction payload
// exposes reactionGroups (GitHub's [ReactionGroup!]), reflecting the subject's
// reaction state after the mutation without a separate refetch.
func TestGraphQLAddReactionReturnsReactionGroups(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "reactgroups", true)

	doc := `mutation($input:AddReactionInput!){addReaction(input:$input){reaction{id content user{login}} reactionGroups{content viewerHasReacted}}}`
	env := s.gqlAuthzPost(t, f.ownerToken, doc,
		map[string]interface{}{"input": map[string]interface{}{"subjectId": f.issue.NodeID, "content": "HEART"}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("addReaction errored: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	add, _ := data["addReaction"].(map[string]interface{})
	// The payload's reaction is the one just created.
	reaction, _ := add["reaction"].(map[string]interface{})
	if reaction == nil || reaction["content"] != "HEART" {
		t.Errorf("addReaction.reaction = %v, want content HEART", add["reaction"])
	}
	if id, _ := reaction["id"].(string); id == "" {
		t.Errorf("addReaction.reaction.id is empty")
	}
	groups, _ := add["reactionGroups"].([]interface{})
	found := false
	for _, g := range groups {
		gm, _ := g.(map[string]interface{})
		if gm["content"] == "HEART" {
			found = true
			if gm["viewerHasReacted"] != true {
				t.Errorf("HEART viewerHasReacted = %v, want true", gm["viewerHasReacted"])
			}
		}
	}
	if !found {
		t.Errorf("reactionGroups %v missing the HEART group just added", groups)
	}
}

// TestGraphQLPermissionDenialCarriesForbiddenType verifies that a permission
// denial (viewer can read the repo but lacks push standing) surfaces GitHub's
// `"type": "FORBIDDEN"` errors[] member — the sibling of the NOT_FOUND channel
// used when the resource cannot be read at all. Clients discriminate on it.
func TestGraphQLPermissionDenialCarriesForbiddenType(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// A public repo (private=false) the outside contributor can read but not push.
	f := newGQLAuthzFixture(t, s.Server, "forbidden", false)

	// closeIssue is a push-level mutation and the stranger is not the author.
	env := s.gqlAuthzPost(t, f.strangerToken,
		`mutation($input:CloseIssueInput!){closeIssue(input:$input){clientMutationId}}`,
		map[string]interface{}{"input": map[string]interface{}{"issueId": f.issue.NodeID}})

	errs := gqlAuthzErrors(env)
	if len(errs) == 0 {
		t.Fatalf("expected a permission error, got %v", env)
	}
	first, _ := errs[0].(map[string]interface{})
	if first["type"] != "FORBIDDEN" {
		t.Errorf("error type = %v, want FORBIDDEN; full error: %v", first["type"], first)
	}
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || issue.State != "OPEN" {
		t.Errorf("the issue was closed despite the refusal: %+v", issue)
	}
}

// TestGraphQLAddReactionExposesSubjectReactable verifies the addReaction payload's
// subject: Reactable field resolves to the concrete reacted subject (GQL-068):
// selecting a fragment on the subject's concrete type returns its fields.
func TestGraphQLAddReactionExposesSubjectReactable(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "reactsubj-field", true)

	doc := `mutation($input:AddReactionInput!){addReaction(input:$input){subject{__typename ... on Issue { title databaseId } viewerCanReact reactionGroups{content}}}}`
	env := s.gqlAuthzPost(t, f.ownerToken, doc,
		map[string]interface{}{"input": map[string]interface{}{"subjectId": f.issue.NodeID, "content": "ROCKET"}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("addReaction errored: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	add, _ := data["addReaction"].(map[string]interface{})
	subject, _ := add["subject"].(map[string]interface{})
	if subject == nil {
		t.Fatalf("addReaction.subject is nil: %v", add)
	}
	if subject["__typename"] != "Issue" {
		t.Errorf("subject.__typename = %v, want Issue", subject["__typename"])
	}
	if subject["title"] != "fixture issue" {
		t.Errorf("subject.title = %v, want the fixture issue title", subject["title"])
	}
	if subject["viewerCanReact"] != true {
		t.Errorf("subject.viewerCanReact = %v, want true", subject["viewerCanReact"])
	}
}
