package bleephub

import "testing"

// TestGraphQLSearchRepositoryType pins the three pieces that make
// `search(type: REPOSITORY)` a usable query, all of which the vendored schema
// declares: the REPOSITORY member of SearchType, the repositoryCount member of
// SearchResultItemConnection, and Repository as a member of the
// SearchResultItem union.
//
// They only work together. Without REPOSITORY in the enum the query does not
// parse; without Repository in the union no node can be selected out of the
// result; and without repositoryCount a client cannot tell an empty result from
// a result it has not paged to yet — the count is the only non-null number a
// repository search returns.
func TestGraphQLSearchRepositoryType(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	needle := s.createRepoWriteRepo(t, true)
	// A second repository the query must not match, so a count of 1 means the
	// search filtered rather than that only one repository exists.
	_ = s.createRepoWriteRepo(t, true)

	data := s.gqlData(t, `query($q: String!) {
	  search(query: $q, type: REPOSITORY, first: 10) {
	    repositoryCount
	    nodes { __typename ... on Repository { name nameWithOwner } }
	    edges { node { __typename ... on Repository { name } } }
	    pageInfo { hasNextPage }
	  }
	}`, map[string]interface{}{"q": needle})

	search, _ := data["search"].(map[string]interface{})
	if search == nil {
		t.Fatalf("search returned no result: %v", data)
	}
	count, ok := search["repositoryCount"].(float64)
	if !ok {
		t.Fatalf("repositoryCount = %v, want a number", search["repositoryCount"])
	}
	if int(count) != 1 {
		t.Fatalf("repositoryCount = %d searching for %q, want 1", int(count), needle)
	}

	nodes, _ := search["nodes"].([]interface{})
	if len(nodes) != 1 {
		t.Fatalf("nodes = %v, want exactly the one matching repository", search["nodes"])
	}
	node, _ := nodes[0].(map[string]interface{})
	// __typename is what tells a client which union member it received; a
	// repository answered as an Issue would be selected away to nothing.
	if node["__typename"] != "Repository" {
		t.Errorf("node __typename = %v, want Repository", node["__typename"])
	}
	if node["name"] != needle {
		t.Errorf("node name = %v, want %q", node["name"], needle)
	}
	if node["nameWithOwner"] != "admin/"+needle {
		t.Errorf("nameWithOwner = %v, want admin/%s", node["nameWithOwner"], needle)
	}

	// edges and nodes are two views of the same window.
	edges, _ := search["edges"].([]interface{})
	if len(edges) != 1 {
		t.Fatalf("edges = %v, want 1", search["edges"])
	}
	edge, _ := edges[0].(map[string]interface{})
	edgeNode, _ := edge["node"].(map[string]interface{})
	if edgeNode == nil || edgeNode["name"] != needle {
		t.Errorf("edge node = %v, want the same repository as nodes[0]", edge["node"])
	}

	// A query matching neither repository counts zero rather than everything.
	empty := s.gqlData(t, `{ search(query: "no-repository-has-this-name-xyzzy", type: REPOSITORY, first: 10) {
	  repositoryCount nodes { __typename }
	} }`, nil)
	emptySearch, _ := empty["search"].(map[string]interface{})
	if emptySearch == nil {
		t.Fatalf("empty search returned no result: %v", empty)
	}
	if got, _ := emptySearch["repositoryCount"].(float64); int(got) != 0 {
		t.Errorf("repositoryCount for a non-matching query = %v, want 0", emptySearch["repositoryCount"])
	}
	if nodes, _ := emptySearch["nodes"].([]interface{}); len(nodes) != 0 {
		t.Errorf("nodes for a non-matching query = %v, want none", emptySearch["nodes"])
	}
}
