// Package graphqlschema is the complete GitHub GraphQL type universe,
// generated from the vendored public schema
// (third_party/github-graphql-schema.graphql.gz) by
// internal/graphqlschemagen.
//
// The package holds types only: every object, interface, union, enum,
// input object and custom scalar GitHub publishes, with GitHub's exact
// field names, argument names, argument types, default values,
// nullability, list nesting, deprecations, interface implementations and
// descriptions. It deliberately carries no field resolvers — binding
// resolvers is a later phase, and a resolver-free schema is what lets the
// generated definitions be diffed against the vendored SDL without any
// runtime data.
//
// The generated definitions are ordinary Go, not a runtime SDL parse: the
// contract is checked by the compiler, the type expressions are visible in
// review, and no schema text has to ship in the binary.
//
// It depends on graphql-go only. Nothing in this package may import
// internal/graphqlapi or internal/server — the dependency runs the other
// way once the serving schema is switched over.
//
// # What graphql-go v0.8.1 cannot express
//
// Three things in GitHub's SDL survive into the generated Go but cannot be
// carried all the way through the library. None changes a field's name,
// type, nullability or list nesting, so none is visible to the
// completeness ratchet — they are recorded here because a client that
// introspects the schema can see two of them.
//
//   - Applied directives. GitHub annotates 274 fields and 48 unions with
//     @docsCategory, 373 input fields with @possibleTypes, and declares
//     @preview. graphql-go's type system has no place to attach a
//     directive application, and GraphQL introspection has no way to
//     report one either, so GitHub's own API does not expose them.
//     Documentation-only; dropped deliberately.
//
//   - Explicit "null" argument defaults (19 of them, e.g.
//     Enterprise.members(hasTwoFactorEnabled: Boolean = null)). The
//     vendored parser has no NullValue AST node, and graphql-go models
//     "no default" as a nil DefaultValue with no way to distinguish it
//     from a default of null. Introspection reports null for both.
//
//   - Non-scalar default values in introspection.
//     __InputValue.defaultValue is rendered by
//     astFromValue(inputValue.DefaultValue, inputValue), which passes the
//     input value where the function expects its type
//     (graphql-go introspection.go:265, :272), so the *List and *Enum
//     branches never fire; astFromValue also carries an explicit
//     "TODO: implement astFromValue from Map to Value" (introspection.go:738).
//     Enum, list and input-object defaults are therefore printed by Go's
//     %v fallback — {field: CREATED_AT, direction: DESC} introspects as
//     "map[direction:DESC field:CREATED_AT]". The DefaultValue graphql-go
//     coerces a missing argument to is the correct Go value, so execution
//     is unaffected; only the introspected string deviates.
//     TestDefaultValuesAreReproducedAndIntrospectionIsLimited pins both
//     halves.
package graphqlschema

import (
	"fmt"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
)

// TypenameKey is the discriminator key the resolver layer sets on a
// map-shaped resolver result so an interface or union field knows which
// concrete object type the value is. It is spelled "__typename" because
// that is the key the hand-written resolvers in internal/graphqlapi
// already set, so resolvers moved onto the generated schema dispatch
// through the same channel they do today.
const TypenameKey = "__typename"

// Typed is the struct-shaped half of the same discriminator: a resolver
// that returns a concrete Go value rather than a map reports its GraphQL
// type by implementing this interface. Interface and union dispatch reads
// the discriminator through [TypenameOf], never through Go reflection on
// the value's dynamic type, so the resolver layer stays free to return the
// same Go type for several GraphQL types (the store's *store.User backs
// both User and, for a bot login, Bot).
type Typed interface {
	GraphQLTypename() string
}

// TypenameOf reads the abstract-type discriminator off a resolver result.
// It returns "" when the value carries none, which the generated
// ResolveType renders as a GraphQL "cannot resolve abstract type" error
// rather than a silently wrong concrete type.
func TypenameOf(value interface{}) string {
	switch typed := value.(type) {
	case Typed:
		return typed.GraphQLTypename()
	case map[string]interface{}:
		name, _ := typed[TypenameKey].(string)
		return name
	case map[string]string:
		return typed[TypenameKey]
	}
	return ""
}

// Registry owns every named type of the generated schema and the abstract
// type membership that interface and union dispatch reads.
//
// Construction is two-phase, which is how the schema's cycles are handled
// without any per-type special casing: [New] first creates every named
// type's shell (a *graphql.Object, *graphql.Interface, … with its name and
// description), passing its fields, interfaces and union members as
// deferred thunks. graphql-go does not call a thunk until the schema is
// assembled, by which point every shell is registered, so a thunk that
// names Repository from Issue from User from Repository resolves through
// the map instead of needing a definition order that cannot exist.
type Registry struct {
	types      map[string]graphql.Type
	objects    map[string]*graphql.Object
	interfaces map[string]*graphql.Interface
	// members maps an interface or union name to the object types that
	// satisfy it. Interface membership is inverted from each object's
	// "implements" list at registration time, so the dispatch table cannot
	// disagree with the declared implementations.
	members map[string]map[string]bool
	// order is the registration order (the generator emits definitions
	// sorted by name), which makes SchemaConfig.Types deterministic.
	order []string
}

// New builds the registry: every generated type shell, ready for [Schema].
func New() *Registry {
	r := &Registry{
		types:      make(map[string]graphql.Type, generatedTypeCount+5),
		objects:    make(map[string]*graphql.Object, generatedObjectCount),
		interfaces: make(map[string]*graphql.Interface, generatedInterfaceCount),
		members:    make(map[string]map[string]bool, generatedAbstractCount),
		order:      make([]string, 0, generatedTypeCount),
	}
	// The five built-in scalars are graphql-go's own; the schema must not
	// define a second copy of them.
	r.types["Int"] = graphql.Int
	r.types["Float"] = graphql.Float
	r.types["String"] = graphql.String
	r.types["Boolean"] = graphql.Boolean
	r.types["ID"] = graphql.ID
	r.defineAllTypes()
	return r
}

// Schema assembles the generated types into an executable schema. Every
// field resolves to nil until a later phase binds resolvers; the schema is
// nonetheless fully introspectable, which is what the completeness ratchet
// checks.
func (r *Registry) Schema() (graphql.Schema, error) {
	// Most of the 1,800 types are reachable from Query or Mutation, but
	// not all are (payload types for mutations that only appear behind a
	// preview, abstract members reached solely through a union, …).
	// Listing them explicitly puts every generated type in the schema's
	// type map, so introspection reports the full universe.
	types := make([]graphql.Type, 0, len(r.order))
	for _, name := range r.order {
		types = append(types, r.types[name])
	}
	return graphql.NewSchema(graphql.SchemaConfig{
		Query:    r.obj("Query"),
		Mutation: r.obj("Mutation"),
		Types:    types,
	})
}

// Type returns a generated named type, or nil when the schema has none by
// that name.
func (r *Registry) Type(name string) graphql.Type { return r.types[name] }

// TypeNames lists every generated named type in registration order
// (sorted by name), excluding the five built-in scalars.
func (r *Registry) TypeNames() []string {
	names := make([]string, len(r.order))
	copy(names, r.order)
	return names
}

// NewStringScalar builds a string-serialized custom scalar.
//
// All thirteen of GitHub's custom scalars are transported as JSON strings
// (URI, DateTime, HTML, GitObjectID, Base64String, GitTimestamp,
// PreciseDateTime, X509Certificate, GitSSHRemote, GitRefname, BigInt,
// Date, CustomPropertyValue), and this reproduces exactly what
// internal/graphqlapi's stringScalar does for the subset bleephub already
// serves, so moving a resolver onto a generated scalar cannot change a
// single byte on the wire. graphqlapi's copy stays the definition of
// record until the swap; a test there pins the two to identical behaviour.
func NewStringScalar(name, description string) *graphql.Scalar {
	return graphql.NewScalar(graphql.ScalarConfig{
		Name:        name,
		Description: description,
		Serialize:   func(value interface{}) interface{} { return fmt.Sprint(value) },
		ParseValue:  func(value interface{}) interface{} { return fmt.Sprint(value) },
		ParseLiteral: func(valueAST ast.Value) interface{} {
			if value, ok := valueAST.(*ast.StringValue); ok {
				return value.Value
			}
			return nil
		},
	})
}

// --- registration seams used by the generated definitions -----------------

func (r *Registry) register(name string, typ graphql.Type) {
	if _, duplicate := r.types[name]; duplicate {
		panic("graphqlschema: duplicate type definition " + name)
	}
	r.types[name] = typ
	r.order = append(r.order, name)
}

// t resolves a named type inside a generated thunk. A miss is a generator
// bug, not a runtime condition: the SDL is closed over its own type names.
func (r *Registry) t(name string) graphql.Type {
	typ, ok := r.types[name]
	if !ok {
		panic("graphqlschema: undefined type " + name)
	}
	return typ
}

func (r *Registry) obj(name string) *graphql.Object {
	typ, ok := r.objects[name]
	if !ok {
		panic("graphqlschema: undefined object type " + name)
	}
	return typ
}

func (r *Registry) iface(name string) *graphql.Interface {
	typ, ok := r.interfaces[name]
	if !ok {
		panic("graphqlschema: undefined interface type " + name)
	}
	return typ
}

func (r *Registry) addMember(abstract, concrete string) {
	set := r.members[abstract]
	if set == nil {
		set = map[string]bool{}
		r.members[abstract] = set
	}
	set[concrete] = true
}

// Members lists nothing publicly; abstract membership is exposed only
// through resolveType so it cannot drift from the declared implementations.
func (r *Registry) resolveType(abstract string) graphql.ResolveTypeFn {
	return func(p graphql.ResolveTypeParams) *graphql.Object {
		name := TypenameOf(p.Value)
		if name == "" {
			return nil
		}
		// A discriminator naming a type that does not satisfy this
		// abstract type is a resolver bug. Returning nil surfaces it as a
		// GraphQL error; returning the object anyway would serve a value
		// under a type the client's fragment never selected.
		if !r.members[abstract][name] {
			return nil
		}
		return r.objects[name]
	}
}

func (r *Registry) object(name, description string, interfaceNames []string, fields graphql.FieldsThunk) {
	for _, abstract := range interfaceNames {
		r.addMember(abstract, name)
	}
	config := graphql.ObjectConfig{
		Name:        name,
		Description: description,
		Fields:      fields,
	}
	if len(interfaceNames) != 0 {
		config.Interfaces = graphql.InterfacesThunk(func() []*graphql.Interface {
			list := make([]*graphql.Interface, 0, len(interfaceNames))
			for _, abstract := range interfaceNames {
				list = append(list, r.iface(abstract))
			}
			return list
		})
	}
	typ := graphql.NewObject(config)
	r.objects[name] = typ
	r.register(name, typ)
}

func (r *Registry) interfaceType(name, description string, fields graphql.FieldsThunk) {
	typ := graphql.NewInterface(graphql.InterfaceConfig{
		Name:        name,
		Description: description,
		Fields:      fields,
		ResolveType: r.resolveType(name),
	})
	r.interfaces[name] = typ
	r.register(name, typ)
}

func (r *Registry) union(name, description string, memberNames []string) {
	for _, concrete := range memberNames {
		r.addMember(name, concrete)
	}
	r.register(name, graphql.NewUnion(graphql.UnionConfig{
		Name:        name,
		Description: description,
		ResolveType: r.resolveType(name),
		Types: graphql.UnionTypesThunk(func() []*graphql.Object {
			list := make([]*graphql.Object, 0, len(memberNames))
			for _, concrete := range memberNames {
				list = append(list, r.obj(concrete))
			}
			return list
		}),
	}))
}

func (r *Registry) enum(name, description string, values graphql.EnumValueConfigMap) {
	r.register(name, graphql.NewEnum(graphql.EnumConfig{
		Name:        name,
		Description: description,
		Values:      values,
	}))
}

func (r *Registry) input(name, description string, fields graphql.InputObjectConfigFieldMapThunk) {
	r.register(name, graphql.NewInputObject(graphql.InputObjectConfig{
		Name:        name,
		Description: description,
		Fields:      fields,
	}))
}

func (r *Registry) scalar(name, description string) {
	r.register(name, NewStringScalar(name, description))
}
