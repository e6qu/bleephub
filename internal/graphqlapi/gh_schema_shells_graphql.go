package graphqlapi

// Schema-fidelity shells: the types GitHub declares that bleephub does not
// reach through any resolved field — audit-entry subtypes, unmodeled timeline
// events, ordering inputs and the abstract interfaces over them. They exist so
// bleephub's introspected schema matches GitHub's shape; no query returns them
// because this instance does not produce the underlying data.
//
// Each cluster lives in its own file and registers a builder here through
// init(); addSchemaFidelityShells runs them all during schema assembly. A
// builder constructs every type signature-exact and calls
// registerExtraSchemaType so the type appears in introspection. graphql-go
// admits an interface with no possible types, so an abstract interface needs no
// implementer to be published.

// schemaShellBuilders is the set of per-cluster shell installers, populated by
// each cluster file's init(). Appending from init() keeps clusters in separate
// files with no shared edit.
var schemaShellBuilders []func(*Resolver)

func (s *Resolver) addSchemaFidelityShells() {
	for _, build := range schemaShellBuilders {
		build(s)
	}
}
