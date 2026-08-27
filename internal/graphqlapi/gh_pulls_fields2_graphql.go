package graphqlapi

// Residual connection/back-reference fields of the pull-request type family,
// plus ReviewDismissedEvent.pullRequestCommit, whose owning type the timeline
// builder mints after this family.

import (
	"github.com/graphql-go/graphql"
)

// gqlConnNodes coerces a connection source's "nodes" value ([]interface{} or
// []map[string]interface{} depending on the renderer) to a uniform []interface{}.
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

// gqlConnEdges returns the connection source's edges, preferring a well-formed
// "edges" slice; when the source carries only "nodes", edges are synthesised so
// the `edges` selection is never a typed nil.
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

// gqlConnPageInfo returns the source's pageInfo, or an all-false pageInfo when
// it carries none — never a nil that would break the non-null PageInfo child.
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

// gqlEdgesField builds a connection's `edges` field over the given edge type.
func gqlEdgesField(edgeType *graphql.Object) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(edgeType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return gqlConnEdges(p.Source), nil
		},
	}
}

// gqlPageInfoField builds a connection's non-null `pageInfo` field.
func (s *Resolver) gqlPageInfoField() *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(s.gqlPageInfoType()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return gqlConnPageInfo(p.Source), nil
		},
	}
}

// simpleEdgeType returns a standard Relay edge object (cursor + node), memoized
// by name so a name another family also mints resolves to one instance.
func (s *Resolver) simpleEdgeType(name string, node graphql.Output) *graphql.Object {
	return s.mutationObject(name, graphql.Fields{
		"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"node":   &graphql.Field{Type: node},
	})
}

// pullTimelineMember reaches a PullRequestTimelineItems union member by name,
// navigating from PullRequest.timelineItems — used to reach types the timeline
// builder mints after this family so their PR-specific fields can be completed.
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

// addReviewDismissedEventPullRequestCommit adds ReviewDismissedEvent's back-ref
// into the pull-request family. bleephub does not record which commit staled a
// review, so the nullable field answers null. Wired once from
// addPullRequestSurfaceMutations, after the timeline family mints the type.
func (s *Resolver) addReviewDismissedEventPullRequestCommit() {
	event := s.pullTimelineMember("ReviewDismissedEvent")
	if event == nil {
		return
	}
	event.AddFieldConfig("pullRequestCommit", &graphql.Field{
		Type: s.graphqlTypes.pullRequestCommit,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return srcMap(p)["pullRequestCommit"], nil
		},
	})
}
