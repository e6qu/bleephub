package graphqlapi

// Shared construction machinery for the GitHub mutation surface.
//
// GitHub's schema declares one input object and one payload object per
// mutation, and both routinely name supporting types other families also
// name: CheckRunOutput belongs to createCheckRun and updateCheckRun,
// ProjectColumn is returned by six classic-project mutations, RefUpdate is
// written by updateRefs alone but read by the ref family's payloads.
// graphql-go refuses a schema holding two objects of the same name, so the
// helpers here mint each type once, keyed by GitHub's own spelling, and hand
// the same instance to every family that names it.
//
// The families themselves live in gh_mutations_<family>_graphql.go and are
// assembled by addGitHubMutationSurface, which the schema builder calls once
// the read surface exists — every payload here returns an object the read
// surface already defines (Repository, Issue, PullRequest, Label, Team, …).

import (
	"github.com/graphql-go/graphql"
)

// mutationObject memoizes an object type by GitHub's name.
//
// The field map is eager rather than a thunk because graphql-go's
// AddFieldConfig silently declines to touch a thunked object, and every
// payload type registerMutation installs has clientMutationId added to it
// that way — a thunked payload would be published without the member GitHub
// declares on it. Types whose members are genuinely cyclic use
// mutationObjectLazy below, which no payload is.
func (s *Resolver) mutationObject(name string, fields graphql.Fields) *graphql.Object {
	if existing := s.memoizedMutationObject(name); existing != nil {
		return existing
	}
	object := graphql.NewObject(graphql.ObjectConfig{Name: name, Fields: fields})
	s.mutationObjects[name] = object
	return object
}

// mutationPayload is mutationObject for a mutation's payload type. It writes
// clientMutationId in itself rather than leaving it to registerMutation's
// fill-in, because graphql-go refuses to construct an object with no fields at
// all and several of GitHub's payloads — deleteLabel's, deleteRef's — declare
// nothing else.
func (s *Resolver) mutationPayload(name string, fields graphql.Fields) *graphql.Object {
	if _, declared := fields["clientMutationId"]; !declared {
		fields["clientMutationId"] = gqlField(graphql.String)
	}
	return s.mutationObject(name, fields)
}

// mutationObjectLazy is mutationObject for a type whose members name types
// that in turn name it — a column holding a card connection whose cards name
// their column. build is called once, after the whole family is registered,
// so the cycle resolves; nothing may AddFieldConfig onto the result.
func (s *Resolver) mutationObjectLazy(name string, build func() graphql.Fields) *graphql.Object {
	if existing := s.memoizedMutationObject(name); existing != nil {
		return existing
	}
	// The entry is recorded before build runs, so a thunk that names its own
	// type resolves to the instance being built rather than recursing.
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

// mutationInput memoizes an input object type by GitHub's name. Like
// mutationObject the field map is eager: registerMutation adds
// clientMutationId to every mutation input, and graphql-go records a schema
// error rather than a field when asked to add one to a thunk.
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

// --- small field/argument shorthands ---------------------------------------
//
// Every input and payload below is a transcription of GitHub's SDL, so the
// shorthands are named for what the SDL says rather than for graphql-go's
// constructors: `nonNullID()` reads as `ID!` does.

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

// gqlListOf is `[T!]` — a nullable list of non-null members, which is how
// GitHub spells nearly every id list in an input object.
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

// gqlConnectionSource materialises an already-rendered node list into the
// connection source shape the rest of the package uses: nodes, edges with
// cursors, pageInfo and totalCount. A field resolver hands the result to
// repaginateConnection with its own arguments, so a connection built here
// pages exactly as every other connection does.
func gqlConnectionSource(nodes []map[string]interface{}) map[string]interface{} {
	return paginateGQLMaps(nodes, nil)
}

// --- input readers ----------------------------------------------------------

// gqlInputString reads a String member of a mutation input, reporting whether
// the client supplied it at all. A GitHub update mutation distinguishes "set
// this to the empty string" from "leave it alone", and every resolver below
// depends on that distinction.
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

// gqlInputInt is gqlInputString for an Int member. graphql-go hands an Int
// through as a Go int, but a variable that arrived as JSON may still be a
// float64, so both are accepted.
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

// gqlInputStrings reads a `[String!]` member as a Go slice, reporting whether
// the client supplied the list at all — an absent list and an empty one mean
// different things to a replace-the-set mutation.
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
