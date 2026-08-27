package graphqlapi

import (
	"sort"
)

// Relay-style GraphQL connection pagination.

// repaginateConnection re-slices an already-built connection source map to
// honor the Relay connection arguments supplied to an embedded connection
// field (e.g. PullRequest.reviews).
func repaginateConnection(src interface{}, args map[string]interface{}) interface{} {
	conn, ok := src.(map[string]interface{})
	if !ok {
		return src
	}
	nodes, ok := conn["nodes"].([]map[string]interface{})
	if !ok {
		// Some connections store []interface{}; leave them untouched.
		return src
	}
	return paginateGQLMaps(nodes, args)
}

// sortGQLNodesByCreatedAt orders nodes oldest first, breaking ties by "_dbID".
// A deterministic order is required: nodes are built from map iteration and
// createdAt is only second-precision, so without it cursor page boundaries
// would shift between requests for the same page.
func sortGQLNodesByCreatedAt(nodes []map[string]interface{}) {
	sort.Slice(nodes, func(a, b int) bool {
		ca, _ := nodes[a]["createdAt"].(string)
		cb, _ := nodes[b]["createdAt"].(string)
		if ca != cb {
			return ca < cb
		}
		ida, _ := nodes[a]["_dbID"].(int)
		idb, _ := nodes[b]["_dbID"].(int)
		return ida < idb
	})
}

// intArg coerces a GraphQL Int argument (int, int64, or float64) to an int.
func intArg(args map[string]interface{}, key string) (int, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// gqlConnItem is one connection element whose node rendering is deferred until
// pagination has chosen the page. Sorting and cursor resolution run on the
// identity alone, so `search(first:1)` renders one node, not every match.
type gqlConnItem struct {
	identity string
	render   func() map[string]interface{}
}

func resolveConnectionIndexForItems(items []gqlConnItem, cursor string, fallbackIdx int) int {
	if id := connectionCursorID(cursor); id != "" {
		for i := range items {
			if items[i].identity == id {
				return i
			}
		}
	}
	return fallbackIdx
}

// paginateGQLItems windows lazy items like paginateGQLMaps, rendering only
// those inside the final window.
func paginateGQLItems(items []gqlConnItem, args map[string]interface{}) map[string]interface{} {
	total := len(items)
	start := 0
	end := total

	if after, ok := args["after"].(string); ok && after != "" {
		afterIndex := resolveConnectionIndexForItems(items, after, decodeCursor(after))
		// Saturate before +1 so cursor:<MaxInt> describes an empty window
		// rather than wrapping start negative and returning page one.
		if afterIndex >= total {
			start = total
		} else {
			start = afterIndex + 1
		}
	}
	if before, ok := args["before"].(string); ok && before != "" {
		end = resolveConnectionIndexForItems(items, before, decodeCursor(before))
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	if end < 0 {
		end = 0
	}
	if end > total {
		end = total
	}
	if end < start {
		end = start
	}

	if last, ok := intArg(args, "last"); ok && last > 0 {
		if last > 100 {
			last = 100
		}
		if end-start > last {
			start = end - last
		}
	}
	// first: 0 is valid (metadata, no nodes); it must not fold into the
	// unspecified default-page path below.
	if first, ok := intArg(args, "first"); ok && first >= 0 {
		if first > 100 {
			first = 100
		}
		if end-start > first {
			end = start + first
		}
	}
	if _, ok := intArg(args, "first"); !ok {
		if last, ok := intArg(args, "last"); !ok || last <= 0 {
			if end-start > 30 {
				end = start + 30
			}
		}
	}

	return buildConnectionWindowLazy(items, start, end, total)
}

func buildConnectionWindowLazy(items []gqlConnItem, startIdx, endIdx, total int) map[string]interface{} {
	// Clamp: cursor values are untrusted client input that can otherwise
	// produce an out-of-range slice below.
	if total < 0 || total > len(items) {
		total = len(items)
	}
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx > total {
		startIdx = total
	}
	if endIdx < startIdx {
		endIdx = startIdx
	}
	if endIdx > total {
		endIdx = total
	}
	page := items[startIdx:endIdx]
	outNodes := make([]map[string]interface{}, 0, len(page))
	edges := make([]map[string]interface{}, 0, len(page))
	for i := range page {
		node := page[i].render()
		cursor := encodeConnectionCursor(startIdx+i, page[i].identity)
		outNodes = append(outNodes, node)
		edges = append(edges, map[string]interface{}{"node": node, "cursor": cursor})
	}
	var startCursor, endCursor interface{}
	if len(edges) > 0 {
		startCursor = edges[0]["cursor"]
		endCursor = edges[len(edges)-1]["cursor"]
	}
	return map[string]interface{}{
		"nodes":      outNodes,
		"edges":      edges,
		"totalCount": total,
		"pageInfo": map[string]interface{}{
			"hasNextPage":     endIdx < total,
			"hasPreviousPage": startIdx > 0,
			"startCursor":     startCursor,
			"endCursor":       endCursor,
		},
	}
}

// identityOr returns the item's stable identity, or "" when no extractor is
// supplied (the cursor then degrades to index-only).
func identityOr[T any](identity func(T) string, item T) string {
	if identity == nil {
		return ""
	}
	return identity(item)
}

// resolveItemCursorIndex returns the current index of the item a cursor
// identifies, or fallbackIdx when the cursor is index-only or the item is gone.
// Keeps forward pagination stable across inserts before the boundary (GQL-019).
func resolveItemCursorIndex[T any](items []T, cursor string, identity func(T) string, fallbackIdx int) int {
	if id := connectionCursorID(cursor); id != "" && identity != nil {
		for i, item := range items {
			if identity(item) == id {
				return i
			}
		}
	}
	return fallbackIdx
}

// paginateGQL is Relay forward (first/after) pagination; toGQL renders each
// node, threading extra state (e.g. *Store) via a closure.
func paginateGQL[T any](items []T, first int, after string, toGQL func(T) map[string]interface{}, identity func(T) string) map[string]interface{} {
	total := len(items)

	// GitHub caps page size at 100 and rejects non-positive values; clamp so a
	// hostile `first` cannot overflow startIdx+first into an out-of-range slice.
	if first <= 0 {
		first = 30
	}
	if first > 100 {
		first = 100
	}

	startIdx := 0
	if after != "" {
		startIdx = resolveItemCursorIndex(items, after, identity, decodeCursor(after)) + 1
	}
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx > total {
		startIdx = total
	}
	endIdx := startIdx + first
	if endIdx < startIdx || endIdx > total {
		endIdx = total
	}

	page := items[startIdx:endIdx]
	nodes := make([]map[string]interface{}, 0, len(page))
	edges := make([]map[string]interface{}, 0, len(page))
	for i, item := range page {
		gql := toGQL(item)
		cursor := encodeConnectionCursor(startIdx+i, identityOr(identity, item))
		nodes = append(nodes, gql)
		edges = append(edges, map[string]interface{}{
			"node":   gql,
			"cursor": cursor,
		})
	}

	var startCursor, endCursor interface{}
	if len(edges) > 0 {
		startCursor = edges[0]["cursor"]
		endCursor = edges[len(edges)-1]["cursor"]
	}
	return map[string]interface{}{
		"nodes":      nodes,
		"edges":      edges,
		"totalCount": total,
		"pageInfo": map[string]interface{}{
			"hasNextPage":     endIdx < total,
			"hasPreviousPage": startIdx > 0,
			"startCursor":     startCursor,
			"endCursor":       endCursor,
		},
	}
}
