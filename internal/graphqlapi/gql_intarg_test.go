package graphqlapi

import "testing"

// TestGraphQLIntArgsAcceptVariableNumericTypes covers GQL-048: GraphQL Int
// arguments arrive as int for inline literals but as float64 (or int64) when
// supplied through query variables (JSON-decoded). The connection paginators
// read `first`/`last`/`number` through intArg, which accepts all three, instead
// of a bare `.(int)` assertion that silently dropped a variable-supplied value
// and fell back to the default page.
func TestGraphQLIntArgsAcceptVariableNumericTypes(t *testing.T) {
	nodes := []map[string]interface{}{{"a": 1}, {"a": 2}, {"a": 3}, {"a": 4}, {"a": 5}}
	page := paginateGQLMaps(nodes, map[string]interface{}{"first": float64(2)})
	got, ok := page["nodes"].([]map[string]interface{})
	if !ok {
		t.Fatalf("nodes has unexpected type %T", page["nodes"])
	}
	if len(got) != 2 {
		t.Fatalf("GQL-048: first=float64(2) yielded %d nodes, want 2 (a variable-supplied int must be honored)", len(got))
	}

	for _, v := range []interface{}{int(3), int64(3), float64(3)} {
		if n, ok := intArg(map[string]interface{}{"n": v}, "n"); !ok || n != 3 {
			t.Fatalf("intArg(%T) = %d,%v; want 3,true", v, n, ok)
		}
	}
	if _, ok := intArg(map[string]interface{}{"n": "3"}, "n"); ok {
		t.Fatal("intArg accepted a string argument")
	}
}
