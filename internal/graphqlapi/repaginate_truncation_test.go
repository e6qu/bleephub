package graphqlapi

import (
	"strconv"
	"testing"
)

// TestUnpagedSourceRepaginatesPastThirty pins the fix for repaginated
// connections silently truncating at 30. A connection that is re-paged by
// repaginateConnection (e.g. Repository.stargazers, UserList.items,
// MergeQueue.entries, the BranchProtectionRule allowances) must store the FULL
// node list via gqlUnpagedSource; feeding a pre-windowed gqlConnectionSource
// into repaginateConnection caps it at the default 30 and dead-ends `after`
// paging past the first page.
func TestUnpagedSourceRepaginatesPastThirty(t *testing.T) {
	const total = 45
	nodes := make([]map[string]interface{}, total)
	for i := range nodes {
		nodes[i] = map[string]interface{}{"id": "N_" + strconv.Itoa(i)}
	}

	// The fix: an un-windowed source repaginates over the full set.
	full := repaginateConnection(gqlUnpagedSource(nodes), map[string]interface{}{"first": 50}).(map[string]interface{})
	got, _ := full["nodes"].([]map[string]interface{})
	if len(got) != total {
		t.Fatalf("repaginate(first:50) over un-windowed source = %d nodes, want %d", len(got), total)
	}
	if tc, _ := full["totalCount"].(int); tc != total {
		t.Fatalf("totalCount = %d, want %d", tc, total)
	}

	// Regression guard: a pre-windowed source (the old builder shape) dead-ends
	// at 30 — the exact truncation the fix removes. This is why the repaginated
	// builders must NOT use gqlConnectionSource.
	capped := repaginateConnection(gqlConnectionSource(nodes), map[string]interface{}{"first": 50}).(map[string]interface{})
	cappedNodes, _ := capped["nodes"].([]map[string]interface{})
	if len(cappedNodes) != 30 {
		t.Fatalf("pre-windowed source repaginated to %d, expected the legacy 30-cap", len(cappedNodes))
	}

	// `after` paging reaches nodes past the first window when the source is full.
	firstPage := repaginateConnection(gqlUnpagedSource(nodes), map[string]interface{}{"first": 40}).(map[string]interface{})
	page1, _ := firstPage["nodes"].([]map[string]interface{})
	if len(page1) != 40 {
		t.Fatalf("first page = %d nodes, want 40", len(page1))
	}
	pageInfo, _ := firstPage["pageInfo"].(map[string]interface{})
	endCursor, _ := pageInfo["endCursor"].(string)
	if hasNext, _ := pageInfo["hasNextPage"].(bool); !hasNext || endCursor == "" {
		t.Fatalf("first page pageInfo = %v, want hasNextPage + endCursor", pageInfo)
	}
	second := repaginateConnection(gqlUnpagedSource(nodes), map[string]interface{}{"first": 40, "after": endCursor}).(map[string]interface{})
	page2, _ := second["nodes"].([]map[string]interface{})
	if len(page2) != total-40 {
		t.Fatalf("second page = %d nodes, want %d (paging past 30 must not dead-end)", len(page2), total-40)
	}
}
