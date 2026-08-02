package bleephub

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PaginationParams holds parsed pagination query parameters.
type PaginationParams struct {
	Page    int
	PerPage int
}

func filterSince[T any](
	w http.ResponseWriter,
	r *http.Request,
	resource string,
	items []T,
	updatedAt func(T) time.Time,
) ([]T, bool) {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		return items, true
	}
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeGHValidationError(w, resource, "since", "invalid")
		return nil, false
	}
	filtered := make([]T, 0, len(items))
	for _, item := range items {
		if !updatedAt(item).Before(since) {
			filtered = append(filtered, item)
		}
	}
	return filtered, true
}

func invalidRESTPaginationQuery(r *http.Request) string {
	for _, name := range []string{"page", "per_page"} {
		value := r.URL.Query().Get(name)
		if value == "" {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 {
			return name
		}
	}
	return ""
}

// parsePagination extracts page/per_page from query string with GitHub defaults.
func parsePagination(r *http.Request) PaginationParams {
	p := PaginationParams{Page: 1, PerPage: 30}
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Page = n
		}
	}
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.PerPage = n
			if p.PerPage > 100 {
				p.PerPage = 100
			}
		}
	}
	return p
}

// paginateAndLink slices items to the current page and sets the Link header.
func paginateAndLink[T any](w http.ResponseWriter, r *http.Request, items []T) []T {
	pp := parsePagination(r)
	total := len(items)

	lastPage := 1
	if total > 0 {
		lastPage = (total + pp.PerPage - 1) / pp.PerPage
	}

	// Guard against integer overflow from an attacker-supplied page: a very
	// large page makes (page-1)*perPage wrap negative, which would produce an
	// out-of-range slice expression. Compute in int64 and clamp.
	start64 := int64(pp.Page-1) * int64(pp.PerPage)
	var start int
	switch {
	case start64 < 0:
		start = 0
	case start64 > int64(total):
		start = total
	default:
		start = int(start64)
	}
	end := start + pp.PerPage
	if end < start || end > total {
		end = total
	}
	page := items[start:end]

	if link := buildLinkHeader(r, pp.Page, pp.PerPage, lastPage); link != "" {
		w.Header().Set("Link", link)
	}

	return page
}

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
func buildConnectionWindow(nodes []map[string]interface{}, startIdx, endIdx, total int) map[string]interface{} {
	// Keep this final rendering boundary safe even when a new caller computes
	// a malformed window. Cursor values are untrusted client input, and both
	// addition and conversion can otherwise produce an out-of-range slice.
	if total < 0 || total > len(nodes) {
		total = len(nodes)
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
	page := nodes[startIdx:endIdx]
	outNodes := make([]map[string]interface{}, 0, len(page))
	edges := make([]map[string]interface{}, 0, len(page))
	for i, n := range page {
		cursor := encodeCursor(startIdx + i)
		outNodes = append(outNodes, n)
		edges = append(edges, map[string]interface{}{"node": n, "cursor": cursor})
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
func paginateGQL[T any](items []T, first int, after string, toGQL func(T) map[string]interface{}) map[string]interface{} {
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
		startIdx = decodeCursor(after) + 1
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
		cursor := encodeCursor(startIdx + i)
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

// buildLinkHeader constructs an RFC 5988 Link header.
func buildLinkHeader(r *http.Request, page, perPage, lastPage int) string {
	if lastPage <= 1 {
		return ""
	}

	// GitHub emits absolute pagination targets, and clients (octokit, gh)
	// auto-follow the Link header WITH the Authorization header attached. The
	// host therefore must not come from a client-supplied X-Forwarded-Host: a
	// spoofed value would redirect the follow-up request — credentials and all —
	// to an attacker's host. Derive the host from the request's own Host header.
	// X-Forwarded-Proto only selects http/https on that same host (no exfil), so
	// it is still honored for correctness behind a TLS-terminating proxy.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	base := (&url.URL{Scheme: scheme, Host: r.Host, Path: r.URL.Path}).String()
	q := r.URL.Query()
	q.Del("page")

	linkURL := func(p int) string {
		qc := make(url.Values)
		for k, v := range q {
			qc[k] = v
		}
		qc.Set("page", strconv.Itoa(p))
		qc.Set("per_page", strconv.Itoa(perPage))
		return fmt.Sprintf("<%s?%s>", base, qc.Encode())
	}

	var parts []string
	if page < lastPage {
		parts = append(parts, linkURL(page+1)+`; rel="next"`)
		parts = append(parts, linkURL(lastPage)+`; rel="last"`)
	}
	if page > 1 {
		parts = append(parts, linkURL(1)+`; rel="first"`)
		parts = append(parts, linkURL(page-1)+`; rel="prev"`)
	}
	return strings.Join(parts, ", ")
}
