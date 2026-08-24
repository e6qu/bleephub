package graphqlapi

// Optional object sources.
//
// Every GraphQL source in this package is a map[string]interface{}. An
// optional child object that does not exist must be held as an untyped nil
// interface (or left out of the map) — never as a nil
// map[string]interface{}.
//
// The distinction is not cosmetic. graphql-go decides nullness with
// isNullish, which answers "null" for a nil pointer or a nil interface but
// not for a nil map: a map arrives as reflect.Map and is reported present.
// The executor then completes the child type against an empty shell, so its
// non-null fields — id, login, url — fail with "Cannot return null for
// non-nullable field X.id". That failure panics up through the non-null chain
// until it reaches a nullable ancestor, so a single missing optional child
// does not null one field: it discards a whole subtree and puts an error in
// the response, which any client treating `errors` as fatal (gh, octokit)
// reports as the whole query having failed.
//
// The two helpers below exist so a renderer never has to hold that in mind,
// and so the shortest way to write the field is also the correct one. A
// regression is caught by the typed-nil source ratchet, which walks every
// source map the server test suite produces
// (internal/server/graphql_source_audit_test.go).

// optionalObject converts an already-rendered child source into the value a
// source-map member — or a resolver's own return — may safely hold: the map
// when the child exists, an untyped nil interface when it does not.
//
//	"dismisser": optionalObject(dismisser),
//	return optionalObject(s.projectV2ByNumber(…)), nil
func optionalObject(child map[string]interface{}) interface{} {
	if child == nil {
		return nil
	}
	return child
}

// optionalRendered renders an optional store record with a renderer that
// dereferences it, answering an untyped nil interface when the record is
// absent. It removes the declare-then-conditionally-assign dance that is
// where the typed-nil defect keeps being introduced, because the natural
// one-line form is the safe one:
//
//	"author": optionalRendered(st.Users[issue.AuthorID], userToGraphQL),
//
// A missing map key yields the nil pointer directly, so a comma-ok lookup is
// not needed either.
func optionalRendered[T any](record *T, render func(*T) map[string]interface{}) interface{} {
	if record == nil {
		return nil
	}
	return render(record)
}
