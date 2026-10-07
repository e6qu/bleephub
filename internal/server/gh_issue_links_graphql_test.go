package bleephub

import (
	"testing"
)

// The relates-to and blocked-by links GraphQL lists are the ones the store
// records: a relates-to link shows on both issues, newest first by default and
// oldest first when asked, and an issue in a repository the viewer cannot read
// is left out of the lists and the dependency counts.
func TestGraphQLIssueLinksListStoredRelationships(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "issue-links", true)
	first := s.store.CreateIssue(f.repo.ID, f.owner.ID, "first related", "", nil, nil, 0)
	second := s.store.CreateIssue(f.repo.ID, f.owner.ID, "second related", "", nil, nil, 0)
	blocker := s.store.CreateIssue(f.repo.ID, f.owner.ID, "open blocker", "", nil, nil, 0)
	if first == nil || second == nil || blocker == nil {
		t.Fatal("could not seed the linked issues")
	}

	add := `mutation($input:AddRelatesToInput!){addRelatesTo(input:$input){issue{number} relatedIssue{number}}}`
	for _, related := range []int{first.ID, second.ID} {
		node := s.store.GetIssue(related).NodeID
		env := s.gqlAuthzPost(t, f.ownerToken, add, map[string]interface{}{"input": map[string]interface{}{"issueId": f.issue.NodeID, "relatedIssueId": node}})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("addRelatesTo refused the owner: %v", errs)
		}
	}
	env := s.gqlAuthzPost(t, f.ownerToken, add, map[string]interface{}{"input": map[string]interface{}{"issueId": f.issue.NodeID, "relatedIssueId": first.NodeID}})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Fatal("relating already related issues was accepted")
	}
	if !s.store.AddIssueBlockedBy(f.issue.ID, blocker.ID) {
		t.Fatal("could not record the blocked-by link")
	}

	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){issue(number:$number){
		newest:relatesTo(first:10){nodes{number}}
		oldest:relatesTo(first:10,orderBy:{field:RELATES_TO_ADDED_AT,direction:ASC}){nodes{number}}
		blockedBy(first:10){nodes{number}}
		issueDependenciesSummary{blockedBy totalBlockedBy blocking totalBlocking}}}}`
	numbers := func(conn interface{}) []float64 {
		nodes, _ := conn.(map[string]interface{})["nodes"].([]interface{})
		out := make([]float64, 0, len(nodes))
		for _, node := range nodes {
			out = append(out, node.(map[string]interface{})["number"].(float64))
		}
		return out
	}
	vars := map[string]interface{}{"owner": f.owner.Login, "name": f.repo.Name, "number": f.issue.Number}
	env = s.gqlAuthzPost(t, f.ownerToken, query, vars)
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("issue links query failed: %v", errs)
	}
	issue := env["data"].(map[string]interface{})["repository"].(map[string]interface{})["issue"].(map[string]interface{})
	if got := numbers(issue["newest"]); len(got) != 2 || got[0] != float64(second.Number) || got[1] != float64(first.Number) {
		t.Errorf("relatesTo newest first = %v, want [%d %d]", got, second.Number, first.Number)
	}
	if got := numbers(issue["oldest"]); len(got) != 2 || got[0] != float64(first.Number) {
		t.Errorf("relatesTo oldest first = %v, want %d first", got, first.Number)
	}
	if got := numbers(issue["blockedBy"]); len(got) != 1 || got[0] != float64(blocker.Number) {
		t.Errorf("blockedBy = %v, want [%d]", got, blocker.Number)
	}
	summary := issue["issueDependenciesSummary"].(map[string]interface{})
	if summary["blockedBy"] != float64(1) || summary["totalBlockedBy"] != float64(1) || summary["blocking"] != float64(0) {
		t.Errorf("issueDependenciesSummary = %v", summary)
	}

	// The relationship has no direction: the related issue lists it back.
	vars["number"] = first.Number
	env = s.gqlAuthzPost(t, f.ownerToken, query, vars)
	back := env["data"].(map[string]interface{})["repository"].(map[string]interface{})["issue"].(map[string]interface{})
	if got := numbers(back["newest"]); len(got) != 1 || got[0] != float64(f.issue.Number) {
		t.Errorf("the related issue lists %v, want [%d]", got, f.issue.Number)
	}

	remove := `mutation($input:RemoveRelatesToInput!){removeRelatesTo(input:$input){issue{number}}}`
	env = s.gqlAuthzPost(t, f.ownerToken, remove, map[string]interface{}{"input": map[string]interface{}{"issueId": f.issue.NodeID, "relatedIssueId": first.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("removeRelatesTo refused the owner: %v", errs)
	}
	if related := s.store.ListIssueRelatesTo(first.ID); len(related) != 0 {
		t.Errorf("the removed relationship is still recorded on the related issue: %v", related)
	}
}
