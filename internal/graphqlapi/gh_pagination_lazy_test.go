package graphqlapi

import (
	"strconv"
	"testing"
)

// TestPaginateGQLItemsRendersOnlyTheWindow covers GQL-026: a connection over
// many matches must render only the nodes the pagination window keeps, not the
// whole set. A search(first:1) over a large instance therefore renders one
// node, not every issue and pull request.
func TestPaginateGQLItemsRendersOnlyTheWindow(t *testing.T) {
	const total = 100
	rendered := 0
	items := make([]gqlConnItem, total)
	for i := range items {
		i := i
		items[i] = gqlConnItem{
			identity: "N_" + strconv.Itoa(i),
			render: func() map[string]interface{} {
				rendered++
				return map[string]interface{}{"id": "N_" + strconv.Itoa(i)}
			},
		}
	}

	result := paginateGQLItems(items, map[string]interface{}{"first": 1})

	nodes, _ := result["nodes"].([]map[string]interface{})
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if rendered != 1 {
		t.Fatalf("rendered %d items, want 1 (only the window is rendered)", rendered)
	}
	if got, _ := result["totalCount"].(int); got != total {
		t.Fatalf("totalCount = %d, want %d", got, total)
	}
	if nodes[0]["id"] != "N_0" {
		t.Fatalf("first node id = %v, want N_0", nodes[0]["id"])
	}
}

// TestPaginateGQLItemsMatchesEagerWindowing pins that the lazy path returns the
// same window (nodes + cursors + pageInfo shape) as the eager paginateGQLMaps
// wrapper, so the two share one cursor contract.
func TestPaginateGQLItemsMatchesEagerWindowing(t *testing.T) {
	const total = 20
	nodes := make([]map[string]interface{}, total)
	for i := range nodes {
		nodes[i] = map[string]interface{}{"id": "N_" + strconv.Itoa(i)}
	}
	args := map[string]interface{}{"first": 5}

	eager := paginateGQLMaps(nodes, args)
	lazyItems := make([]gqlConnItem, total)
	for i := range nodes {
		n := nodes[i]
		lazyItems[i] = gqlConnItem{identity: gqlNodeIdentity(n), render: func() map[string]interface{} { return n }}
	}
	lazy := paginateGQLItems(lazyItems, args)

	eagerNodes := eager["nodes"].([]map[string]interface{})
	lazyNodes := lazy["nodes"].([]map[string]interface{})
	if len(eagerNodes) != 5 || len(lazyNodes) != 5 {
		t.Fatalf("window sizes = eager %d, lazy %d, want 5", len(eagerNodes), len(lazyNodes))
	}
	for i := range eagerNodes {
		if eagerNodes[i]["id"] != lazyNodes[i]["id"] {
			t.Fatalf("node %d: eager %v, lazy %v", i, eagerNodes[i]["id"], lazyNodes[i]["id"])
		}
	}
	eagerEdges := eager["edges"].([]map[string]interface{})
	lazyEdges := lazy["edges"].([]map[string]interface{})
	for i := range eagerEdges {
		if eagerEdges[i]["cursor"] != lazyEdges[i]["cursor"] {
			t.Fatalf("edge %d cursor: eager %v, lazy %v", i, eagerEdges[i]["cursor"], lazyEdges[i]["cursor"])
		}
	}
}
