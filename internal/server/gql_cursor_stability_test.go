package bleephub

import (
	"reflect"
	"testing"
)

func gqlNode(id string) map[string]interface{} { return map[string]interface{}{"nodeID": id} }

func gqlConnNodeIDs(conn map[string]interface{}) []string {
	nodes, _ := conn["nodes"].([]map[string]interface{})
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n["nodeID"].(string))
	}
	return out
}

// A connection cursor identifies its node, so inserting an item before a page
// boundary must not shift the next page (no skip, no duplicate) — GQL-019.
func TestGQLConnectionCursorsStableAcrossInserts(t *testing.T) {
	nodes := []map[string]interface{}{gqlNode("a"), gqlNode("b"), gqlNode("c"), gqlNode("d")}
	page1 := paginateGQLMaps(nodes, map[string]interface{}{"first": 2})
	if got := gqlConnNodeIDs(page1); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("page 1 = %v, want [a b]", got)
	}
	endCursor := page1["pageInfo"].(map[string]interface{})["endCursor"].(string)

	// An item is inserted at the front, shifting every index by one.
	shifted := []map[string]interface{}{gqlNode("z"), gqlNode("a"), gqlNode("b"), gqlNode("c"), gqlNode("d")}
	page2 := paginateGQLMaps(shifted, map[string]interface{}{"first": 2, "after": endCursor})
	// The cursor points at node "b" (now index 2), so the next page is [c, d] —
	// with the old index cursor it would have wrongly returned [b, c].
	if got := gqlConnNodeIDs(page2); !reflect.DeepEqual(got, []string{"c", "d"}) {
		t.Fatalf("page 2 after insert = %v, want [c d] (stable cursor)", got)
	}
}

// A cursor whose node was deleted falls back to its recorded index, and a plain
// legacy index cursor still works.
func TestGQLConnectionCursorFallbacks(t *testing.T) {
	nodes := []map[string]interface{}{gqlNode("a"), gqlNode("b"), gqlNode("c")}
	// Legacy index-only cursor "after index 0" -> [b, c].
	legacy := encodeCursor(0)
	page := paginateGQLMaps(nodes, map[string]interface{}{"first": 5, "after": legacy})
	if got := gqlConnNodeIDs(page); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("legacy index cursor = %v, want [b c]", got)
	}
	// Identity cursor whose node no longer exists falls back to the index.
	gone := encodeConnectionCursor(0, "removed")
	page = paginateGQLMaps(nodes, map[string]interface{}{"first": 5, "after": gone})
	if got := gqlConnNodeIDs(page); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("deleted-node cursor fallback = %v, want [b c]", got)
	}
}
