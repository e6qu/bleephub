package graphqlapi

import (
	"sort"
)

// Relay-style GraphQL connection pagination, moved from the server package
// with the resolver layer (ARCH-003).

// repaginateConnection re-slices an already-built connection source map to
// honor the Relay connection arguments (first/after, last/before) supplied to
// an embedded connection field (e.g. PullRequest.reviews, Issue.comments).
//
// Embedded connections are materialised eagerly into a source map whose
// "nodes" holds the full, already-rendered node list. The field resolver
// passes that source map plus its own p.Args here; this reuses paginateGQL so
// the slice, pageInfo (hasNextPage/hasPreviousPage), startCursor/endCursor and
// totalCount match the top-level connections exactly.
//
// `last`/`before` page from the end: select the window of size `last` ending
// just before `before` (or at the end), mirroring GitHub's backward
// pagination. When no connection args are present the full list is returned
// unsliced with a correct (all-false) pageInfo and cursors spanning the list.
func repaginateConnection(src interface{}, args map[string]interface{}) interface{} {
	conn, ok := src.(map[string]interface{})
	if !ok {
		return src
	}
	nodes, ok := conn["nodes"].([]map[string]interface{})
	if !ok {
		// Some connections store []interface{} (empty or heterogeneous); leave
		// them untouched rather than guess at a node shape.
		return src
	}
	// Keep one implementation of the four Relay window arguments. The old
	// forward-only path dropped `before` whenever `first` was present, so
	// clients walking a bounded window received nodes outside that window.
	return paginateGQLMaps(nodes, args)
}

// sortGQLNodesByCreatedAt orders already-rendered connection nodes oldest
// first, with the numeric "_dbID" as a stable tiebreaker. Embedded
// connections are built by iterating a Go map (nondeterministic order) and
// formatting createdAt to second precision (so equal-second nodes collide);
// without a deterministic order, cursor pagination boundaries would shift
// between requests for the same page. "_dbID" is a private key the GraphQL
// schema never exposes.
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

// intArg coerces a GraphQL Int argument (which may arrive as int, int64, or
// float64 depending on the variable/literal path) to an int.
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

// buildConnectionWindow renders nodes[startIdx:endIdx] into a connection map
// with edges/cursors and a correct pageInfo, matching paginateGQL's shape.
// gqlConnItem is one element of a GraphQL connection whose expensive node
// rendering is deferred until pagination has chosen the page. Sorting and
// cursor resolution run on the lightweight identity, so a `search(first:1)`
// over a large instance renders a single node instead of every match (GQL-026).
type gqlConnItem struct {
	// identity is the stable node id (what gqlNodeIdentity would return for the
	// rendered node), used for identity-cursor resolution.
	identity string
	// render is invoked only for items inside the returned window.
	render func() map[string]interface{}
}

// resolveConnectionIndexForItems mirrors resolveConnectionIndex but matches on
// the lightweight identity so no node is rendered to resolve a cursor.
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

// paginateGQLItems applies the same cursor/first/last windowing as
// paginateGQLMaps, but over lazy items: only the items inside the final window
// are rendered.
func paginateGQLItems(items []gqlConnItem, args map[string]interface{}) map[string]interface{} {
	total := len(items)
	start := 0
	end := total

	if after, ok := args["after"].(string); ok && after != "" {
		afterIndex := resolveConnectionIndexForItems(items, after, decodeCursor(after))
		// Saturate before adding one: cursor:<MaxInt> must describe an empty
		// window, not wrap start negative and unexpectedly return page one.
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
	if first, ok := intArg(args, "first"); ok && first > 0 {
		if first > 100 {
			first = 100
		}
		if end-start > first {
			end = start + first
		}
	}
	if first, ok := intArg(args, "first"); !ok || first <= 0 {
		if last, ok := intArg(args, "last"); !ok || last <= 0 {
			if end-start > 30 {
				end = start + 30
			}
		}
	}

	return buildConnectionWindowLazy(items, start, end, total)
}

func buildConnectionWindowLazy(items []gqlConnItem, startIdx, endIdx, total int) map[string]interface{} {
	// Keep this final rendering boundary safe even when a new caller computes
	// a malformed window. Cursor values are untrusted client input, and both
	// addition and conversion can otherwise produce an out-of-range slice.
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

// paginateGQL implements Relay-style cursor pagination for GraphQL connections.
// toGQL converts each item into a map[string]interface{} that becomes the
// GraphQL node. Use a closure to thread extra state (e.g. *Store) into toGQL.
// resolveItemCursorIndex returns the current index of the item a connection
// cursor identifies (by its stable identity), or the recorded fallback index
// when the cursor is legacy/index-only or the item is gone. Keeps forward
// pagination stable when items are inserted before the boundary (GQL-019).
// identityOr returns the item's stable identity, or "" when no extractor is
// supplied (the cursor then degrades to an index-only cursor).
func identityOr[T any](identity func(T) string, item T) string {
	if identity == nil {
		return ""
	}
	return identity(item)
}

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

func paginateGQL[T any](items []T, first int, after string, toGQL func(T) map[string]interface{}, identity func(T) string) map[string]interface{} {
	total := len(items)

	// Real GitHub caps connection page size at 100 and rejects non-positive
	// values; clamp defensively so an attacker-supplied `first` (negative or
	// huge enough to overflow startIdx+first) can never produce an
	// out-of-range slice expression below.
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
