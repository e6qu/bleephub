// Package graphqlschema is the complete GitHub GraphQL type universe,
// generated from the vendored public schema by internal/graphqlschemagen. It
// holds types only (no resolvers), so the generated definitions diff against
// the vendored SDL without runtime data. Import graphql-go only; never import
// internal/graphqlapi or internal/server.
//
// # What graphql-go v0.8.1 cannot express
//
// Three SDL features survive into the generated Go but not through the library.
// None changes a field's name, type, nullability or list nesting, so none is
// visible to the completeness ratchet; two are visible to an introspecting
// client. TestDefaultValuesAreReproducedAndIntrospectionIsLimited pins them.
//
//   - Applied directives (@docsCategory, @possibleTypes, @preview). graphql-go
//     has no place to attach a directive application and introspection cannot
//     report one, so GitHub's own API does not expose them either.
//
//   - Explicit "null" argument defaults (19, e.g.
//     Enterprise.members(hasTwoFactorEnabled: Boolean = null)). graphql-go
//     models "no default" as a nil DefaultValue, indistinguishable from a null
//     default; introspection reports null for both.
//
//   - Non-scalar default values in introspection. astFromValue is passed the
//     input value where it expects its type (graphql-go introspection.go:265,
//     :272), so the *List/*Enum branches never fire and enum/list/input-object
//     defaults print via Go's %v fallback ({field: CREATED_AT} introspects as
//     "map[field:CREATED_AT]"). The coerced DefaultValue is the correct Go
//     value, so execution is unaffected — only the introspected string deviates.
package graphqlschema

import (
	"fmt"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
)

// TypenameKey is the discriminator key a map-shaped resolver result sets to
// name its concrete object type. Spelled "__typename" to match the existing
// internal/graphqlapi resolvers.
const TypenameKey = "__typename"

// Typed is the struct-shaped half of the discriminator: a resolver returning a
// concrete Go value reports its GraphQL type by implementing this. Dispatch
// reads the discriminator through [TypenameOf], never Go reflection, so one Go
// type can back several GraphQL types (*store.User backs both User and Bot).
type Typed interface {
	GraphQLTypename() string
}

// TypenameOf reads the abstract-type discriminator off a resolver result,
// returning "" when the value carries none (ResolveType turns that into a
// "cannot resolve abstract type" error, not a wrong concrete type).
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

// Registry owns every named type of the generated schema and the abstract-type
// membership that interface and union dispatch reads.
//
// Construction is two-phase to handle the schema's cycles without per-type
// special casing: [New] creates every type's shell with its fields/interfaces/
// members as deferred thunks. graphql-go calls no thunk until the schema is
// assembled, by which point every shell is registered, so a thunk naming a
// not-yet-defined type resolves through the map.
type Registry struct {
	types      map[string]graphql.Type
	objects    map[string]*graphql.Object
	interfaces map[string]*graphql.Interface
	// members maps an interface or union name to its satisfying object types,
	// inverted from each object's "implements" list at registration time so the
	// dispatch table cannot disagree with the declared implementations.
	members map[string]map[string]bool
	// order is the registration order (sorted by name), making
	// SchemaConfig.Types deterministic.
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
	// The five built-in scalars are graphql-go's own; do not redefine them.
	r.types["Int"] = graphql.Int
	r.types["Float"] = graphql.Float
	r.types["String"] = graphql.String
	r.types["Boolean"] = graphql.Boolean
	r.types["ID"] = graphql.ID
	r.defineAllTypes()
	return r
}

// Schema assembles the generated types into an executable, fully
// introspectable schema (every field resolves to nil until resolvers bind).
func (r *Registry) Schema() (graphql.Schema, error) {
	// Not every type is reachable from Query or Mutation (preview-only payloads,
	// union-only members, …); list them all so introspection reports the full
	// universe.
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

// Type returns a generated named type, or nil when none has that name.
func (r *Registry) Type(name string) graphql.Type { return r.types[name] }

// TypeNames lists every generated named type in registration order,
// excluding the five built-in scalars.
func (r *Registry) TypeNames() []string {
	names := make([]string, len(r.order))
	copy(names, r.order)
	return names
}

// NewStringScalar builds a string-serialized custom scalar. All thirteen of
// GitHub's custom scalars transport as JSON strings, and this reproduces
// internal/graphqlapi's stringScalar byte-for-byte (a test there pins the two).
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

// registration seams used by the generated definitions

func (r *Registry) register(name string, typ graphql.Type) {
	if _, duplicate := r.types[name]; duplicate {
		panic("graphqlschema: duplicate type definition " + name)
	}
	r.types[name] = typ
	r.order = append(r.order, name)
}

// t resolves a named type inside a generated thunk. A miss is a generator bug,
// not a runtime condition.
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

// resolveType is the only exposure of abstract membership, so it cannot drift
// from the declared implementations.
func (r *Registry) resolveType(abstract string) graphql.ResolveTypeFn {
	return func(p graphql.ResolveTypeParams) *graphql.Object {
		name := TypenameOf(p.Value)
		if name == "" {
			return nil
		}
		// A discriminator naming a type that does not satisfy this abstract type
		// is a resolver bug; nil surfaces it as a GraphQL error rather than
		// serving a value under an unselected type.
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
