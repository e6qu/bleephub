package graphqlapi

import (
	"regexp"
	"strings"

	"github.com/graphql-go/graphql"
)

// The content-editing trait shared by every comment and body: the
// UserContentEdit history and the bodyText/bodyHTML projections of a markdown body.

// gqlUserContentEditType returns GitHub's UserContentEdit (memoized). bleephub
// records no per-edit diff history, so the connection built from it is a true
// empty rather than a stub.
func (s *Resolver) gqlUserContentEditType() *graphql.Object {
	if s.graphqlTypes.userContentEdit != nil {
		return s.graphqlTypes.userContentEdit
	}
	dateTime := s.graphQLStringScalar("DateTime")
	s.graphqlTypes.userContentEdit = graphql.NewObject(graphql.ObjectConfig{
		Name: "UserContentEdit",
		// Signature-exact with GitHub's UserContentEdit: Node only, no
		// databaseId, editor/deletedBy are Actor. The ratchet enforces the shape.
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"editedAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"deletedAt": &graphql.Field{Type: dateTime},
			"diff":      &graphql.Field{Type: graphql.String},
			"editor":    &graphql.Field{Type: s.graphqlTypes.actor},
			"deletedBy": &graphql.Field{Type: s.graphqlTypes.actor},
		},
	})
	return s.graphqlTypes.userContentEdit
}

func (s *Resolver) gqlUserContentEditConnectionType() *graphql.Object {
	if s.graphqlTypes.userContentEditConnection != nil {
		return s.graphqlTypes.userContentEditConnection
	}
	edit := s.gqlUserContentEditType()
	edge := graphql.NewObject(graphql.ObjectConfig{
		Name: "UserContentEditEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: edit},
		},
	})
	s.graphqlTypes.userContentEditConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "UserContentEditConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(edge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(edit)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	return s.graphqlTypes.userContentEditConnection
}

// emptyUserContentEditConnection is the well-formed empty connection a
// userContentEdits field resolves to — never a nil that would break a non-null child.
func emptyUserContentEditConnection() map[string]interface{} {
	return map[string]interface{}{
		"edges":      []interface{}{},
		"nodes":      []interface{}{},
		"totalCount": 0,
		"pageInfo": map[string]interface{}{
			"hasNextPage":     false,
			"hasPreviousPage": false,
			"startCursor":     nil,
			"endCursor":       nil,
		},
	}
}

var markdownStructureRe = regexp.MustCompile(`(?m)^[#>\-\*\+\s]+|` + "`+")

// bodyText strips markdown structure to the plain text GitHub's bodyText
// returns — a deliberately light strip, not a full markdown parse.
func bodyText(markdown string) string {
	stripped := markdownStructureRe.ReplaceAllString(markdown, "")
	lines := strings.Split(stripped, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
