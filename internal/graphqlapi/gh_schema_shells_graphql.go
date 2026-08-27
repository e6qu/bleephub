package graphqlapi

// Schema-fidelity shells: types GitHub declares that no bleephub query returns
// (audit-entry subtypes, unmodeled timeline events, ordering inputs, their
// abstract interfaces). They exist only so the introspected schema matches
// GitHub's shape. Each cluster registers a builder through init();
// addSchemaFidelityShells runs them all during schema assembly.

// schemaShellBuilders is populated by each cluster file's init().
var schemaShellBuilders []func(*Resolver)

func (s *Resolver) addSchemaFidelityShells() {
	for _, build := range schemaShellBuilders {
		build(s)
	}
}
