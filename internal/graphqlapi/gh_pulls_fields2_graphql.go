package graphqlapi

// This file completes the residual GitHub GraphQL surface of the pull-request
// type family — the connection `edges`/`pageInfo` members, the identity and
// back-reference fields on PullRequestCommit / ReviewRequest / the stray
// Bot and SearchResultItem* types, and the one cross-family field
// (ReviewDismissedEvent.pullRequestCommit) whose owning type is minted by the
// timeline builder that runs after the pull-request family.
//
// Every field is either backed by the real store/git data the surrounding
// renderers already carry, or answers the truthful empty/null where bleephub
// genuinely models nothing (a stacked-PR graph, a stale-commit link, the
// unused Bot reviewer stub). Nothing is fabricated.
//
// The only installer wired from a pull-request family function is
// addReviewDismissedEventPullRequestCommit, called once from
// addPullRequestSurfaceMutations (which runs after the timeline family, so the
// ReviewDismissedEvent object already exists). Every other field on this file's
// types is attached at the type's own definition site in the pulls files.

import (
	"github.com/graphql-go/graphql"
)

// gqlConnNodes coerces a connection source's "nodes" value (stored as either
// []interface{} or []map[string]interface{} depending on the renderer) to a
// uniform []interface{}.
func gqlConnNodes(v interface{}) []interface{} {
	switch n := v.(type) {
	case []interface{}:
		return n
	case []map[string]interface{}:
		out := make([]interface{}, len(n))
		for i := range n {
			out[i] = n[i]
		}
		return out
	}
	return nil
}

// gqlConnEdges returns the connection source's edges. When the source was built
// by the pagination helpers it already carries a well-formed "edges" slice
// (with cursors consistent with its pageInfo); that is preferred. When the
// source only carries "nodes" (an eagerly embedded connection whose reviewer
// nodes are []interface{} and so bypass repaginateConnection), edges are
// synthesised so the connection's `edges` selection is never a typed nil.
func gqlConnEdges(src interface{}) interface{} {
	m, ok := src.(map[string]interface{})
	if !ok {
		return []interface{}{}
	}
	switch e := m["edges"].(type) {
	case []interface{}:
		if e != nil {
			return e
		}
	case []map[string]interface{}:
		out := make([]interface{}, len(e))
		for i := range e {
			out[i] = e[i]
		}
		return out
	}
	nodes := gqlConnNodes(m["nodes"])
	edges := make([]interface{}, 0, len(nodes))
	for i, n := range nodes {
		id := ""
		if nm, ok := n.(map[string]interface{}); ok {
			id = gqlNodeIdentity(nm)
		}
		edges = append(edges, map[string]interface{}{
			"node":   n,
			"cursor": encodeConnectionCursor(i, id),
		})
	}
	return edges
}

// gqlConnPageInfo returns the connection source's pageInfo, or a well-formed
// empty (all-false) pageInfo when the source carries none — never a nil that
// would break the non-null PageInfo child.
func gqlConnPageInfo(src interface{}) interface{} {
	if m, ok := src.(map[string]interface{}); ok {
		if pi, ok := m["pageInfo"]; ok && pi != nil {
			return pi
		}
	}
	return map[string]interface{}{
		"hasNextPage":     false,
		"hasPreviousPage": false,
		"startCursor":     nil,
		"endCursor":       nil,
	}
}

// gqlEdgesField builds a connection's `edges` field over the given edge type,
// resolving from whatever connection source map the connection field produced.
func gqlEdgesField(edgeType *graphql.Object) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(edgeType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return gqlConnEdges(p.Source), nil
		},
	}
}

// gqlPageInfoField builds a connection's non-null `pageInfo` field, resolving
// from the connection source map (or a synthesised empty pageInfo).
func (s *Resolver) gqlPageInfoField() *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(s.gqlPageInfoType()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return gqlConnPageInfo(p.Source), nil
		},
	}
}

// simpleEdgeType returns the standard Relay edge object (cursor + node) GitHub
// declares for a connection whose edges carry no extra fields, memoized by name
// through the shared mutation-object registry so a name another family also
// mints (e.g. PullRequestReviewCommentEdge) resolves to one instance rather
// than a duplicate the schema rejects.
func (s *Resolver) simpleEdgeType(name string, node graphql.Output) *graphql.Object {
	return s.mutationObject(name, graphql.Fields{
		"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"node":   &graphql.Field{Type: node},
	})
}

// pullTimelineMember reaches a member object of the PullRequestTimelineItems
// union by name, navigating from the memoized PullRequest type through its
// timelineItems connection. Used to reach types the timeline builder mints
// after the pull-request family (it runs later), so the pulls family can still
// complete their pull-request-specific fields.
func (s *Resolver) pullTimelineMember(name string) *graphql.Object {
	pr := s.graphqlTypes.pullRequest
	if pr == nil {
		return nil
	}
	timelineDef := pr.Fields()["timelineItems"]
	if timelineDef == nil {
		return nil
	}
	conn, ok := graphql.GetNamed(timelineDef.Type).(*graphql.Object)
	if !ok {
		return nil
	}
	nodesDef := conn.Fields()["nodes"]
	if nodesDef == nil {
		return nil
	}
	union, ok := graphql.GetNamed(nodesDef.Type).(*graphql.Union)
	if !ok {
		return nil
	}
	for _, member := range union.Types() {
		if member != nil && member.Name() == name {
			return member
		}
	}
	return nil
}

// addReviewDismissedEventPullRequestCommit completes ReviewDismissedEvent, whose
// remaining field points back into the pull-request family. bleephub does not
// record which commit staled a dismissed review, so the (nullable) field
// answers the truthful null; adding it keeps the type's GraphQL surface
// complete. Wired once from addPullRequestSurfaceMutations, which runs after the
// timeline family has minted the ReviewDismissedEvent object.
func (s *Resolver) addReviewDismissedEventPullRequestCommit() {
	event := s.pullTimelineMember("ReviewDismissedEvent")
	if event == nil {
		return
	}
	event.AddFieldConfig("pullRequestCommit", &graphql.Field{
		Type: s.graphqlTypes.pullRequestCommit,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// The commit that stales a review is not modelled; nullable field.
			return srcMap(p)["pullRequestCommit"], nil
		},
	})
}
