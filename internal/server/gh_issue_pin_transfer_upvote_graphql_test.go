package bleephub

import (
	"strings"
	"testing"
)

// These tests cover what the pin/transfer/delete and upvote mutations do — the
// three-pin cap, target-repo number allocation, the delete cascade, and the
// idempotent upvote sets — while the authz table covers who may call them.

// gqlMutationData digs the named mutation's payload out of a GraphQL envelope,
// failing the test on any envelope errors.
func gqlMutationData(t *testing.T, env map[string]interface{}, mutation string) map[string]interface{} {
	t.Helper()
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("%s errors: %v", mutation, errs)
	}
	data, _ := env["data"].(map[string]interface{})
	payload, _ := data[mutation].(map[string]interface{})
	if payload == nil {
		t.Fatalf("%s payload missing: %v", mutation, env)
	}
	return payload
}

func TestGraphQLPinIssueCapsAtThreeAndSurfacesPinnedState(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "pin", true)
	st := s.store

	extra := make([]string, 0, 3)
	for _, title := range []string{"second", "third", "fourth"} {
		issue := st.CreateIssue(f.repo.ID, f.owner.ID, title, "", nil, nil, 0)
		if issue == nil {
			t.Fatalf("could not seed issue %q", title)
		}
		extra = append(extra, issue.NodeID)
	}

	pinDoc := `mutation($input:PinIssueInput!){pinIssue(input:$input){issue{number isPinned}}}`
	for _, nodeID := range []string{f.issue.NodeID, extra[0], extra[1]} {
		payload := gqlMutationData(t, s.gqlAuthzPost(t, f.ownerToken, pinDoc,
			map[string]interface{}{"input": map[string]interface{}{"issueId": nodeID}}), "pinIssue")
		issue, _ := payload["issue"].(map[string]interface{})
		if pinned, _ := issue["isPinned"].(bool); !pinned {
			t.Fatalf("pinIssue payload isPinned = %v, want true", issue["isPinned"])
		}
	}

	// The fourth pin must refuse: GitHub caps pinned issues at three per repo.
	env := s.gqlAuthzPost(t, f.ownerToken, pinDoc,
		map[string]interface{}{"input": map[string]interface{}{"issueId": extra[2]}})
	errs := gqlAuthzErrors(env)
	if len(errs) == 0 {
		t.Fatalf("a fourth pinned issue was accepted: %v", env)
	}
	if first, _ := errs[0].(map[string]interface{}); !strings.Contains(first["message"].(string), "3") {
		t.Errorf("cap refusal does not name the limit: %v", errs)
	}

	// Repository.pinnedIssues lists the three, in pin order, with the pinner.
	listDoc := `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){pinnedIssues(first:10){totalCount nodes{issue{number} pinnedBy{login} repository{name}}}}}`
	env = s.gqlAuthzPost(t, f.ownerToken, listDoc, map[string]interface{}{
		"owner": f.owner.Login, "name": f.repo.Name,
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("pinnedIssues errors: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	repo, _ := data["repository"].(map[string]interface{})
	conn, _ := repo["pinnedIssues"].(map[string]interface{})
	if got, _ := conn["totalCount"].(float64); got != 3 {
		t.Fatalf("pinnedIssues totalCount = %v, want 3", conn["totalCount"])
	}
	nodes, _ := conn["nodes"].([]interface{})
	if len(nodes) != 3 {
		t.Fatalf("pinnedIssues nodes = %d, want 3", len(nodes))
	}
	firstNode, _ := nodes[0].(map[string]interface{})
	if pinnedBy, _ := firstNode["pinnedBy"].(map[string]interface{}); pinnedBy["login"] != f.owner.Login {
		t.Errorf("pinnedBy.login = %v, want %s", pinnedBy["login"], f.owner.Login)
	}

	// Unpinning frees a slot (payload carries the unpinned PinnedIssue id) and
	// unpinning again is a no-op with a null id, like removeReaction.
	unpinDoc := `mutation($input:UnpinIssueInput!){unpinIssue(input:$input){id issue{isPinned}}}`
	payload := gqlMutationData(t, s.gqlAuthzPost(t, f.ownerToken, unpinDoc,
		map[string]interface{}{"input": map[string]interface{}{"issueId": f.issue.NodeID}}), "unpinIssue")
	if payload["id"] == nil {
		t.Errorf("unpinIssue payload id = nil, want the PinnedIssue id")
	}
	issue, _ := payload["issue"].(map[string]interface{})
	if pinned, _ := issue["isPinned"].(bool); pinned {
		t.Errorf("issue still pinned after unpinIssue")
	}
	payload = gqlMutationData(t, s.gqlAuthzPost(t, f.ownerToken, unpinDoc,
		map[string]interface{}{"input": map[string]interface{}{"issueId": f.issue.NodeID}}), "unpinIssue")
	if payload["id"] != nil {
		t.Errorf("second unpinIssue returned id %v, want null", payload["id"])
	}

	// The freed slot admits the issue the cap refused.
	gqlMutationData(t, s.gqlAuthzPost(t, f.ownerToken, pinDoc,
		map[string]interface{}{"input": map[string]interface{}{"issueId": extra[2]}}), "pinIssue")
}

func TestGraphQLTransferIssueMovesToTheSameOwnersRepo(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "transfer", true)
	st := s.store

	// A pre-existing issue in the target proves the moved issue takes the
	// target's next number, not its own.
	if st.CreateIssue(f.repo2.ID, f.owner.ID, "target already has one", "", nil, nil, 0) == nil {
		t.Fatal("could not seed the target repository's issue")
	}
	// A label on the moving issue, absent from the target, exercises
	// createLabelsIfMissing.
	if !st.AddIssueLabels(f.repo.FullName, f.issue.Number, []int{f.label.ID}) {
		t.Fatal("could not label the fixture issue")
	}

	doc := `mutation($input:TransferIssueInput!){transferIssue(input:$input){issue{number isPinned}}}`
	payload := gqlMutationData(t, s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{
			"issueId":               f.issue.NodeID,
			"repositoryId":          f.repo2.NodeID,
			"createLabelsIfMissing": true,
		},
	}), "transferIssue")
	issue, _ := payload["issue"].(map[string]interface{})
	if got, _ := issue["number"].(float64); got != 2 {
		t.Errorf("transferred issue number = %v, want 2 (the target's next)", issue["number"])
	}

	moved := st.GetIssue(f.issue.ID)
	switch {
	case moved == nil:
		t.Fatal("the issue disappeared during transfer")
	case moved.RepoID != f.repo2.ID:
		t.Errorf("issue RepoID = %d, want the target repo %d", moved.RepoID, f.repo2.ID)
	case moved.Number != 2:
		t.Errorf("issue number = %d, want 2", moved.Number)
	}
	if st.GetIssueByNumber(f.repo.ID, f.issue.Number) != nil {
		t.Errorf("the source repository still resolves the issue's old number")
	}
	// The label followed by name into the target repository.
	if len(moved.LabelIDs) != 1 {
		t.Fatalf("moved issue labels = %v, want one re-matched label", moved.LabelIDs)
	}
	if lbl := st.GetLabel(moved.LabelIDs[0]); lbl == nil || lbl.RepoID != f.repo2.ID || lbl.Name != f.label.Name {
		t.Errorf("re-matched label = %+v, want %q in the target repo", lbl, f.label.Name)
	}
	// History moved with the issue: the timeline in the target repo carries
	// both the original "opened" and the new "transferred" event.
	events := map[string]bool{}
	for _, e := range st.ListIssueEvents(f.repo2.ID, f.issue.ID) {
		events[e.Event] = true
	}
	if !events["opened"] || !events["transferred"] {
		t.Errorf("target-repo timeline events = %v, want opened and transferred", events)
	}

	// A repo under another owner is refused even when the caller has push on both
	// sides: GitHub only transfers between same-owner repos.
	foreign := st.CreateRepo(f.stranger, "foreign-target", "", true)
	if foreign == nil {
		t.Fatal("could not create the foreign repository")
	}
	if !st.AddRepoCollaborator(f.stranger.Login, foreign.Name, f.owner.Login, "push") {
		t.Fatal("could not grant the owner push on the foreign repository")
	}
	second := st.CreateIssue(f.repo.ID, f.owner.ID, "stays home", "", nil, nil, 0)
	if second == nil {
		t.Fatal("could not seed the second issue")
	}
	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"issueId": second.NodeID, "repositoryId": foreign.NodeID},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Fatalf("a cross-owner transfer was served: %v", env)
	}
	if after := st.GetIssue(second.ID); after == nil || after.RepoID != f.repo.ID {
		t.Errorf("the issue moved despite the refusal: %+v", after)
	}
}

func TestGraphQLDeleteIssueRemovesTheIssueAndItsChildren(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "delete", true)
	st := s.store

	doc := `mutation($input:DeleteIssueInput!){deleteIssue(input:$input){repository{name}}}`
	payload := gqlMutationData(t, s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"issueId": f.issue.NodeID},
	}), "deleteIssue")
	repo, _ := payload["repository"].(map[string]interface{})
	if repo["name"] != f.repo.Name {
		t.Errorf("deleteIssue repository.name = %v, want %s", repo["name"], f.repo.Name)
	}

	if st.GetIssue(f.issue.ID) != nil {
		t.Errorf("the issue survived deleteIssue")
	}
	if st.GetComment(f.comment.ID) != nil {
		t.Errorf("the issue's comment survived deleteIssue")
	}
	if events := st.ListIssueEvents(f.repo.ID, f.issue.ID); len(events) != 0 {
		t.Errorf("%d timeline events survived deleteIssue", len(events))
	}

	// A second delete of the same node refuses cleanly.
	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"issueId": f.issue.NodeID},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("deleting a deleted issue was served: %v", env)
	}
}

func TestGraphQLUpvotesOnDiscussionsAndComments(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Public repo: upvoting is read-level participation, so the unrelated account
	// exercises the second voter.
	f := newGQLAuthzFixture(t, s.Server, "upvote", false)

	addDoc := `mutation($input:AddUpvoteInput!){addUpvote(input:$input){subject{upvoteCount viewerHasUpvoted}}}`
	removeDoc := `mutation($input:RemoveUpvoteInput!){removeUpvote(input:$input){subject{upvoteCount viewerHasUpvoted}}}`

	subjectAfter := func(env map[string]interface{}, mutation string) (float64, bool) {
		t.Helper()
		payload := gqlMutationData(t, env, mutation)
		subject, _ := payload["subject"].(map[string]interface{})
		count, _ := subject["upvoteCount"].(float64)
		voted, _ := subject["viewerHasUpvoted"].(bool)
		return count, voted
	}

	for _, subjectID := range []string{f.discussion.NodeID, f.discComment.NodeID} {
		input := map[string]interface{}{"input": map[string]interface{}{"subjectId": subjectID}}

		count, voted := subjectAfter(s.gqlAuthzPost(t, f.ownerToken, addDoc, input), "addUpvote")
		if count != 1 || !voted {
			t.Fatalf("%s: after owner addUpvote count=%v voted=%v, want 1/true", subjectID, count, voted)
		}
		// Upvoting twice is a no-op, not a second vote.
		count, _ = subjectAfter(s.gqlAuthzPost(t, f.ownerToken, addDoc, input), "addUpvote")
		if count != 1 {
			t.Fatalf("%s: a repeated addUpvote double-counted: %v", subjectID, count)
		}
		// A second account's vote counts separately.
		count, voted = subjectAfter(s.gqlAuthzPost(t, f.strangerToken, addDoc, input), "addUpvote")
		if count != 2 || !voted {
			t.Fatalf("%s: after second voter count=%v voted=%v, want 2/true", subjectID, count, voted)
		}
		// Removing one vote leaves the other, and the remover's viewer flag clears.
		count, voted = subjectAfter(s.gqlAuthzPost(t, f.ownerToken, removeDoc, input), "removeUpvote")
		if count != 1 || voted {
			t.Fatalf("%s: after removeUpvote count=%v voted=%v, want 1/false", subjectID, count, voted)
		}
		// Removing an absent vote is a no-op.
		count, _ = subjectAfter(s.gqlAuthzPost(t, f.ownerToken, removeDoc, input), "removeUpvote")
		if count != 1 {
			t.Fatalf("%s: a repeated removeUpvote changed the count: %v", subjectID, count)
		}
	}

	// The persisted count survives a fresh read, and the Votable interface
	// dispatches to the concrete Discussion type.
	env := s.gqlAuthzPost(t, f.ownerToken,
		`query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){discussion(number:$number){upvoteCount viewerHasUpvoted}}}`,
		map[string]interface{}{"owner": f.owner.Login, "name": f.repo.Name, "number": f.discussion.Number})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("discussion re-read errors: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	repo, _ := data["repository"].(map[string]interface{})
	d, _ := repo["discussion"].(map[string]interface{})
	if count, _ := d["upvoteCount"].(float64); count != 1 {
		t.Errorf("persisted discussion upvoteCount = %v, want 1", d["upvoteCount"])
	}
	if voted, _ := d["viewerHasUpvoted"].(bool); voted {
		t.Errorf("owner still reads viewerHasUpvoted=true after removing their vote")
	}
}
