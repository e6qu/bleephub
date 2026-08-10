package bleephub

import "testing"

func TestGraphQLNodeAndNodesRefetchCanonicalObjects(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoName := s.createRepoWriteRepo(t, true)
	repo := s.store.GetRepo("admin", repoName)
	if repo == nil {
		t.Fatal("fixture repository is missing")
	}
	_, issueNumber := s.createIssueForTest(t, repoRef{owner: "admin", name: repoName}, "node refetch")
	issue := s.store.GetIssueByNumber(repo.ID, issueNumber)
	admin := s.store.LookupUserByLogin("admin")

	response := decodeJSONWithStatus(t, s.post(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `query($repo:ID!,$ids:[ID!]!){
			node(id:$repo){id __typename ... on Repository{nameWithOwner}}
			nodes(ids:$ids){
				id
				__typename
				... on User {login}
				... on Issue {number}
			}
		}`,
		"variables": map[string]interface{}{
			"repo": repo.NodeID,
			"ids":  []interface{}{admin.NodeID, issue.NodeID, "missing-node"},
		},
	}), 200)
	if errors := response["errors"]; errors != nil {
		t.Fatalf("GraphQL errors = %v", errors)
	}
	data := response["data"].(map[string]interface{})
	node := data["node"].(map[string]interface{})
	if node["__typename"] != "Repository" || node["nameWithOwner"] != repo.FullName {
		t.Fatalf("node = %v", node)
	}
	nodes := data["nodes"].([]interface{})
	if len(nodes) != 3 ||
		nodes[0].(map[string]interface{})["__typename"] != "User" ||
		nodes[1].(map[string]interface{})["__typename"] != "Issue" ||
		nodes[2] != nil {
		t.Fatalf("nodes = %v", nodes)
	}
}

func TestGraphQLNodeHidesAnUnreadablePrivateRepository(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	fixture := newGQLAuthzFixture(t, s.Server, "node-private", true)
	response := decodeJSONWithStatus(t, s.post(t, "/api/graphql", fixture.strangerToken, map[string]interface{}{
		"query":     `query($id:ID!){node(id:$id){id __typename}}`,
		"variables": map[string]interface{}{"id": fixture.repo.NodeID},
	}), 200)
	if errors := response["errors"]; errors != nil {
		t.Fatalf("GraphQL errors = %v", errors)
	}
	if node := response["data"].(map[string]interface{})["node"]; node != nil {
		t.Fatalf("private repository leaked through node: %v", node)
	}
}
