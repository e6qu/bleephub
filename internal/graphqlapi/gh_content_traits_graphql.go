package graphqlapi

import (
	"regexp"
	"strings"

	"github.com/graphql-go/graphql"
)

// The content-editing trait shared by every comment and body: the
// UserContentEdit history and the bodyText/bodyHTML projections of a markdown
// body. These live in one place so the comment types — issue comments, review
// comments, commit comments, gist comments, discussion comments, and the issue
// and pull-request bodies themselves — render them identically.

// gqlUserContentEditType is GitHub's UserContentEdit, memoized. bleephub does
// not record a per-edit diff history — it keeps only the last-edited instant —
// so the connection these build is genuinely empty rather than fabricated: an
// instance with no recorded edit history has none to serve, which is a true
// answer, not a stub.
func (s *Resolver) gqlUserContentEditType() *graphql.Object {
	if s.graphqlTypes.userContentEdit != nil {
		return s.graphqlTypes.userContentEdit
	}
	dateTime := s.graphQLStringScalar("DateTime")
	s.graphqlTypes.userContentEdit = graphql.NewObject(graphql.ObjectConfig{
		Name: "UserContentEdit",
		Fields: graphql.Fields{
			"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"createdAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"editedAt":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"deletedAt":  &graphql.Field{Type: dateTime},
			"diff":       &graphql.Field{Type: graphql.String},
			"editor":     &graphql.Field{Type: s.graphqlTypes.user},
			"deletedBy":  &graphql.Field{Type: s.graphqlTypes.user},
		},
	})
	return s.graphqlTypes.userContentEdit
}

// gqlUserContentEditConnectionType is the connection the *.userContentEdits
// fields return.
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

// emptyUserContentEditConnection is the source a userContentEdits field
// resolves to. It is a real, well-formed empty connection because this
// instance records no edit history — never a nil that would break a non-null
// child, and never invented edits.
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

// bodyText projects a markdown body to the plain text GitHub's bodyText
// returns: the readable content with the markup structure removed. It is a
// deliberately light strip — GitHub's own bodyText is the rendered text, not a
// full markdown parse — matching what a client uses it for (search, previews).
func bodyText(markdown string) string {
	stripped := markdownStructureRe.ReplaceAllString(markdown, "")
	lines := strings.Split(stripped, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
