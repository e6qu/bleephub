package graphqlapi

// Shared construction machinery for the GitHub mutation surface. Because
// graphql-go refuses two objects of the same name, the helpers here mint each
// supporting type once (keyed by GitHub's spelling) and hand the same instance
// to every family that names it. The families live in
// gh_mutations_<family>_graphql.go, assembled by addGitHubMutationSurface after
// the read surface exists.

import (
	"github.com/graphql-go/graphql"
)

// mutationObject memoizes an object type by GitHub's name. The field map is
// eager, not a thunk: AddFieldConfig silently declines to touch a thunked
// object, and registerMutation adds clientMutationId to every payload that way.
// Genuinely cyclic types use mutationObjectLazy instead.
func (s *Resolver) mutationObject(name string, fields graphql.Fields) *graphql.Object {
	if existing := s.memoizedMutationObject(name); existing != nil {
		return existing
	}
	object := graphql.NewObject(graphql.ObjectConfig{Name: name, Fields: fields})
	s.mutationObjects[name] = object
	return object
}

// mutationPayload is mutationObject for a payload type. It writes
// clientMutationId itself because graphql-go refuses an object with no fields,
// and some payloads (deleteLabel's, deleteRef's) declare nothing else.
func (s *Resolver) mutationPayload(name string, fields graphql.Fields) *graphql.Object {
	if _, declared := fields["clientMutationId"]; !declared {
		fields["clientMutationId"] = gqlField(graphql.String)
	}
	return s.mutationObject(name, fields)
}

// mutationObjectLazy is mutationObject for a type whose members cyclically name
// it. build runs once, after the family is registered; nothing may
// AddFieldConfig onto the result.
func (s *Resolver) mutationObjectLazy(name string, build func() graphql.Fields) *graphql.Object {
	if existing := s.memoizedMutationObject(name); existing != nil {
		return existing
	}
	// Record the entry before build runs, so a thunk naming its own type
	// resolves to the instance being built rather than recursing.
	object := graphql.NewObject(graphql.ObjectConfig{Name: name, Fields: graphql.FieldsThunk(build)})
	s.mutationObjects[name] = object
	return object
}

func (s *Resolver) memoizedMutationObject(name string) *graphql.Object {
	if s.mutationObjects == nil {
		s.mutationObjects = map[string]*graphql.Object{}
		return nil
	}
	return s.mutationObjects[name]
}

// mutationActorField is a payload's `actor` member, always the authenticated viewer.
func (s *Resolver) mutationActorField() *graphql.Field {
	return &graphql.Field{
		Type: s.graphqlTypes.actor,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return optionalRendered(s.ghUserFromContext(p.Context), userToGraphQL), nil
		},
	}
}

// registerExtraSchemaType makes a type appear in introspection even when no
// field returns it — the schema-fidelity shells for data this instance does not
// produce. Nil and duplicate entries are ignored.
func (s *Resolver) registerExtraSchemaType(types ...graphql.Type) {
	for _, t := range types {
		if t == nil {
			continue
		}
		dup := false
		for _, existing := range s.extraSchemaTypes {
			if existing.Name() == t.Name() {
				dup = true
				break
			}
		}
		if !dup {
			s.extraSchemaTypes = append(s.extraSchemaTypes, t)
		}
	}
}

// stashNamedObject records an object one family builds so a later-assembled
// family can reference the same instance by name (e.g. the account surface
// reuses the discussion/advisory/mannequin connections built inside their own
// recursive builders).
func (s *Resolver) stashNamedObject(o *graphql.Object) {
	if o == nil {
		return
	}
	if s.mutationObjects == nil {
		s.mutationObjects = map[string]*graphql.Object{}
	}
	if _, exists := s.mutationObjects[o.Name()]; !exists {
		s.mutationObjects[o.Name()] = o
	}
}

// namedObject returns a previously stashed or memoized object type by name.
func (s *Resolver) namedObject(name string) *graphql.Object {
	return s.memoizedMutationObject(name)
}

// mutationInput memoizes an input object type by GitHub's name. The field map
// is eager for the same reason as mutationObject: registerMutation adds
// clientMutationId to every input, and a thunk would record a schema error.
func (s *Resolver) mutationInput(name string, fields graphql.InputObjectConfigFieldMap) *graphql.InputObject {
	if s.mutationInputs == nil {
		s.mutationInputs = map[string]*graphql.InputObject{}
	}
	if existing := s.mutationInputs[name]; existing != nil {
		return existing
	}
	input := graphql.NewInputObject(graphql.InputObjectConfig{Name: name, Fields: fields})
	s.mutationInputs[name] = input
	return input
}

// mutationInterface memoizes an interface type by GitHub's name.
func (s *Resolver) mutationInterface(name string, build func() graphql.Fields, resolveType graphql.ResolveTypeFn) *graphql.Interface {
	if s.mutationInterfaces == nil {
		s.mutationInterfaces = map[string]*graphql.Interface{}
	}
	if existing := s.mutationInterfaces[name]; existing != nil {
		return existing
	}
	iface := graphql.NewInterface(graphql.InterfaceConfig{
		Name:        name,
		Fields:      graphql.FieldsThunk(build),
		ResolveType: resolveType,
	})
	s.mutationInterfaces[name] = iface
	return iface
}

// mutationUnion memoizes a union type by GitHub's name.
func (s *Resolver) mutationUnion(name string, types func() []*graphql.Object, resolveType graphql.ResolveTypeFn) *graphql.Union {
	if s.mutationUnions == nil {
		s.mutationUnions = map[string]*graphql.Union{}
	}
	if existing := s.mutationUnions[name]; existing != nil {
		return existing
	}
	union := graphql.NewUnion(graphql.UnionConfig{
		Name:        name,
		Types:       types(),
		ResolveType: resolveType,
	})
	s.mutationUnions[name] = union
	return union
}

// small field/argument shorthands
//
// Named for the SDL spelling rather than graphql-go's constructors:
// nonNullID() reads as ID! does.

func gqlID() *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.ID}
}

func gqlNonNullID() *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)}
}

func gqlString() *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.String}
}

func gqlNonNullString() *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)}
}

func gqlBool() *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.Boolean}
}

func gqlNonNullBool() *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Boolean)}
}

func gqlInt() *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.Int}
}

func gqlNonNullInt() *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)}
}

func gqlInputOf(t graphql.Input) *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: t}
}

func gqlNonNullInputOf(t graphql.Input) *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(t)}
}

// gqlListOf is `[T!]`, how GitHub spells nearly every id list in an input.
func gqlListOf(t graphql.Input) *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(t))}
}

// gqlNonNullListOf is `[T!]!`.
func gqlNonNullListOf(t graphql.Input) *graphql.InputObjectFieldConfig {
	return &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(t)))}
}

func gqlField(t graphql.Output) *graphql.Field   { return &graphql.Field{Type: t} }
func gqlNonNull(t graphql.Output) *graphql.Field { return &graphql.Field{Type: graphql.NewNonNull(t)} }

// gqlFieldListOf is `[T!]` on an output object.
func gqlFieldListOf(t graphql.Output) *graphql.Field {
	return &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(t))}
}

// gqlConnectionSource materialises a rendered node list into a terminal
// connection (nodes, edges, pageInfo, totalCount), windowed to the default 30.
// Use it only where the result is returned directly; a connection that is later
// re-paged by repaginateConnection must use gqlUnpagedSource instead — feeding a
// pre-windowed source into repaginateConnection caps it at 30 and dead-ends
// paging past the first page.
func gqlConnectionSource(nodes []map[string]interface{}) map[string]interface{} {
	return paginateGQLMaps(nodes, nil)
}

// gqlUnpagedSource wraps the FULL rendered node list as a repaginate-ready
// source (no window applied). It is only valid when fed through
// repaginateConnection, which applies the caller's Relay window.
func gqlUnpagedSource(nodes []map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"nodes": nodes, "totalCount": len(nodes)}
}

// input readers

// gqlInputString reads a String input member, reporting whether the client
// supplied it — an update mutation distinguishes "set to empty" from "leave alone".
func gqlInputString(input map[string]interface{}, key string) (string, bool) {
	value, present := input[key]
	if !present || value == nil {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

// gqlInputBool is gqlInputString for a Boolean member.
func gqlInputBool(input map[string]interface{}, key string) (bool, bool) {
	value, present := input[key]
	if !present || value == nil {
		return false, false
	}
	flag, ok := value.(bool)
	return flag, ok
}

// gqlInputInt is gqlInputString for an Int member. A JSON variable may arrive
// as float64, so both int and float64 are accepted.
func gqlInputInt(input map[string]interface{}, key string) (int, bool) {
	switch value := input[key].(type) {
	case int:
		return value, true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	}
	return 0, false
}

// gqlInputStrings reads a `[String!]` member as a slice, reporting whether the
// client supplied it — an absent and an empty list differ to a replace-the-set mutation.
func gqlInputStrings(input map[string]interface{}, key string) ([]string, bool) {
	value, present := input[key]
	if !present || value == nil {
		return nil, false
	}
	switch items := value.(type) {
	case []interface{}:
		out := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				continue
			}
			out = append(out, text)
		}
		return out, true
	case []string:
		return append([]string(nil), items...), true
	}
	return nil, false
}

// gqlInputObjects reads a list of nested input objects.
func gqlInputObjects(input map[string]interface{}, key string) []map[string]interface{} {
	items, _ := input[key].([]interface{})
	out := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, object)
	}
	return out
}

// gqlInputObject reads a single nested input object.
func gqlInputObject(input map[string]interface{}, key string) (map[string]interface{}, bool) {
	object, ok := input[key].(map[string]interface{})
	return object, ok
}
