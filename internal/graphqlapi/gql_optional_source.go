package graphqlapi

// Optional object sources.
//
// A missing optional child must be an untyped nil interface (or absent from
// the map), never a nil map[string]interface{}: graphql-go's isNullish reports
// a nil map as present, then completing its non-null fields fails and the error
// panics up to the nearest nullable ancestor, discarding a whole subtree.
// Regressions are caught by the typed-nil source ratchet
// (internal/server/graphql_source_audit_test.go).

// optionalObject returns child as an interface, or an untyped nil when absent.
//
//	"dismisser": optionalObject(dismisser),
//	return optionalObject(s.projectV2ByNumber(…)), nil
func optionalObject(child map[string]interface{}) interface{} {
	if child == nil {
		return nil
	}
	return child
}

// optionalRendered renders an optional store record, or an untyped nil when the
// record (a nil pointer) is absent — the safe one-line form for optional fields.
//
//	"author": optionalRendered(st.Users[issue.AuthorID], userToGraphQL),
func optionalRendered[T any](record *T, render func(*T) map[string]interface{}) interface{} {
	if record == nil {
		return nil
	}
	return render(record)
}
