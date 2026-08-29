package graphqlschema

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/graphql-go/graphql"
)

func TestSchemaBuilds(t *testing.T) {
	registry := New()
	schema, err := registry.Schema()
	if err != nil {
		t.Fatalf("build generated schema: %v", err)
	}
	if schema.QueryType() == nil || schema.QueryType().Name() != "Query" {
		t.Fatalf("generated schema has no Query type")
	}
	if schema.MutationType() == nil || schema.MutationType().Name() != "Mutation" {
		t.Fatalf("generated schema has no Mutation type")
	}
	if got := len(registry.TypeNames()); got != generatedTypeCount {
		t.Fatalf("registered %d types, generator recorded %d", got, generatedTypeCount)
	}
	// The type map is what introspection walks; every generated type has to
	// be in it or a client cannot discover the type at all.
	typeMap := schema.TypeMap()
	for _, name := range registry.TypeNames() {
		if typeMap[name] == nil {
			t.Fatalf("generated type %s is missing from the schema type map", name)
		}
	}
}

func TestCyclicTypesResolveThroughThunks(t *testing.T) {
	registry := New()
	if _, err := registry.Schema(); err != nil {
		t.Fatalf("build generated schema: %v", err)
	}
	// Repository → Issue → Repository is the schema's canonical cycle; the
	// thunked field maps must both resolve to the identical type instance.
	repository, ok := registry.Type("Repository").(*graphql.Object)
	if !ok {
		t.Fatalf("Repository is %T, want *graphql.Object", registry.Type("Repository"))
	}
	issueField, ok := repository.Fields()["issue"]
	if !ok {
		t.Fatalf("Repository has no issue field")
	}
	issue, ok := issueField.Type.(*graphql.Object)
	if !ok {
		t.Fatalf("Repository.issue is %T, want *graphql.Object", issueField.Type)
	}
	back, ok := issue.Fields()["repository"]
	if !ok {
		t.Fatalf("Issue has no repository field")
	}
	nonNull, ok := back.Type.(*graphql.NonNull)
	if !ok {
		t.Fatalf("Issue.repository is %T, want *graphql.NonNull", back.Type)
	}
	if nonNull.OfType != graphql.Type(repository) {
		t.Fatalf("Issue.repository points at a different Repository instance")
	}
}

func TestTypenameOfReadsBothDiscriminators(t *testing.T) {
	if got := TypenameOf(map[string]interface{}{TypenameKey: "Bot"}); got != "Bot" {
		t.Fatalf("map discriminator = %q, want Bot", got)
	}
	if got := TypenameOf(map[string]string{TypenameKey: "Organization"}); got != "Organization" {
		t.Fatalf("string-map discriminator = %q, want Organization", got)
	}
	if got := TypenameOf(typedValue{name: "User"}); got != "User" {
		t.Fatalf("Typed discriminator = %q, want User", got)
	}
	if got := TypenameOf(map[string]interface{}{"login": "octocat"}); got != "" {
		t.Fatalf("undiscriminated value = %q, want empty", got)
	}
	if got := TypenameOf(struct{ Login string }{"octocat"}); got != "" {
		t.Fatalf("untyped struct = %q, want empty", got)
	}
}

type typedValue struct{ name string }

func (v typedValue) GraphQLTypename() string { return v.name }

func TestResolveTypeDispatch(t *testing.T) {
	registry := New()
	if _, err := registry.Schema(); err != nil {
		t.Fatalf("build generated schema: %v", err)
	}
	actor, ok := registry.Type("Actor").(*graphql.Interface)
	if !ok {
		t.Fatalf("Actor is %T, want *graphql.Interface", registry.Type("Actor"))
	}
	resolved := actor.ResolveType(graphql.ResolveTypeParams{
		Value: map[string]interface{}{TypenameKey: "Bot"},
	})
	if resolved == nil || resolved.Name() != "Bot" {
		t.Fatalf("Actor dispatch on Bot = %v, want Bot", resolved)
	}
	// Repository is a real object type but not an Actor. Dispatching it
	// through Actor must fail rather than serve a Repository as an Actor.
	if got := actor.ResolveType(graphql.ResolveTypeParams{
		Value: map[string]interface{}{TypenameKey: "Repository"},
	}); got != nil {
		t.Fatalf("Actor dispatch on Repository = %v, want nil", got)
	}
	if got := actor.ResolveType(graphql.ResolveTypeParams{Value: map[string]interface{}{}}); got != nil {
		t.Fatalf("Actor dispatch without a discriminator = %v, want nil", got)
	}

	union, ok := registry.Type("IssueOrPullRequest").(*graphql.Union)
	if !ok {
		t.Fatalf("IssueOrPullRequest is %T, want *graphql.Union", registry.Type("IssueOrPullRequest"))
	}
	if got := union.ResolveType(graphql.ResolveTypeParams{
		Value: typedValue{name: "PullRequest"},
	}); got == nil || got.Name() != "PullRequest" {
		t.Fatalf("union dispatch on PullRequest = %v, want PullRequest", got)
	}
	if got := union.ResolveType(graphql.ResolveTypeParams{
		Value: typedValue{name: "Repository"},
	}); got != nil {
		t.Fatalf("union dispatch on a non-member = %v, want nil", got)
	}
}

func TestCustomScalarsComeFromTheSDL(t *testing.T) {
	registry := New()
	// The thirteen custom scalars are read from the vendored SDL by the generator;
	// pin the set so a schema update that adds or removes one is a reviewed change.
	want := []string{
		"Base64String", "BigInt", "CustomPropertyValue", "Date", "DateTime",
		"GitObjectID", "GitRefname", "GitSSHRemote", "GitTimestamp", "HTML",
		"PreciseDateTime", "URI", "X509Certificate",
	}
	if generatedScalarCount != len(want) {
		t.Fatalf("generator recorded %d custom scalars, want %d", generatedScalarCount, len(want))
	}
	for _, name := range want {
		scalar, ok := registry.Type(name).(*graphql.Scalar)
		if !ok {
			t.Fatalf("%s is %T, want *graphql.Scalar", name, registry.Type(name))
		}
		if got := scalar.Serialize("2026-01-02T03:04:05Z"); got != "2026-01-02T03:04:05Z" {
			t.Fatalf("%s.Serialize = %v, want the string unchanged", name, got)
		}
	}
	// The five built-in scalars stay graphql-go's own instances rather than
	// becoming a second definition.
	for name, builtin := range map[string]*graphql.Scalar{
		"Int": graphql.Int, "Float": graphql.Float, "String": graphql.String,
		"Boolean": graphql.Boolean, "ID": graphql.ID,
	} {
		if registry.Type(name) != graphql.Type(builtin) {
			t.Fatalf("%s is not graphql-go's built-in scalar", name)
		}
	}
}

// BenchmarkRegistryShells measures phase one: constructing all 1,807 named
// type shells with their thunks still deferred.
func BenchmarkRegistryShells(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if got := len(New().TypeNames()); got != generatedTypeCount {
			b.Fatalf("registered %d types", got)
		}
	}
}

// BenchmarkSchemaConstruction measures what a server would pay at startup
// to assemble the full generated schema: the shells plus graphql-go's type
// map walk, thunk resolution and schema validation.
func BenchmarkSchemaConstruction(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		registry := New()
		built, err := registry.Schema()
		if err != nil {
			b.Fatalf("build generated schema: %v", err)
		}
		runtime.KeepAlive(built)
	}
}

// BenchmarkSchemaRetainedHeap reports the live heap one assembled schema
// holds for as long as the process serves it — the number that decides
// whether 1,807 types are affordable in the serving process, which
// allocation totals do not answer. Run it with -benchtime 1x.
func BenchmarkSchemaRetainedHeap(b *testing.B) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	registries := make([]*Registry, 0, b.N)
	schemas := make([]graphql.Schema, 0, b.N)
	for i := 0; i < b.N; i++ {
		registry := New()
		built, err := registry.Schema()
		if err != nil {
			b.Fatalf("build generated schema: %v", err)
		}
		registries = append(registries, registry)
		schemas = append(schemas, built)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/(1<<20)/float64(b.N), "MiB-retained")
	runtime.KeepAlive(registries)
	runtime.KeepAlive(schemas)
}

// TestDefaultValuesAreReproducedAndIntrospectionIsLimited covers SDL default
// values from both sides.
//
// The value graphql-go uses to coerce a missing argument is the Go value the
// generator emitted — exact for every SDL default kind (scalars, enums, lists,
// input objects). Introspection is a different story: graphql-go v0.8.1 renders
// an input value's default via astFromValue(inputVal.DefaultValue, inputVal),
// passing the *Argument where the function expects the argument's *type*
// (introspection.go:265, :272). Every `ttype.(*List)`/`ttype.(*Enum)` check
// therefore fails, and astFromValue also carries an explicit "TODO: implement
// astFromValue from Map to Value" (introspection.go:738). So enum, list and
// input-object defaults print via Go's %v fallback, not as GraphQL literals.
//
// The deviation is confined to the introspected __InputValue.defaultValue
// string; execution reads DefaultValue directly and is unaffected. This test
// pins both halves, so if graphql-go ever fixes the rendering the failure says
// so rather than going unnoticed.
func TestDefaultValuesAreReproducedAndIntrospectionIsLimited(t *testing.T) {
	registry := New()
	schema, err := registry.Schema()
	if err != nil {
		t.Fatalf("build generated schema: %v", err)
	}
	organization, ok := registry.Type("Organization").(*graphql.Object)
	if !ok {
		t.Fatalf("Organization is %T, want *graphql.Object", registry.Type("Organization"))
	}
	packages, ok := organization.Fields()["packages"]
	if !ok {
		t.Fatalf("Organization has no packages field")
	}
	var orderBy *graphql.Argument
	for _, argument := range packages.Args {
		if argument.Name() == "orderBy" {
			orderBy = argument
		}
	}
	if orderBy == nil {
		t.Fatalf("Organization.packages has no orderBy argument")
	}
	// SDL: orderBy: PackageOrder = {field: CREATED_AT, direction: DESC}
	want := map[string]interface{}{"field": "CREATED_AT", "direction": "DESC"}
	got, ok := orderBy.DefaultValue.(map[string]interface{})
	if !ok {
		t.Fatalf("orderBy default is %T, want map[string]interface{}", orderBy.DefaultValue)
	}
	if len(got) != len(want) {
		t.Fatalf("orderBy default = %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("orderBy default[%q] = %v, want %v", key, got[key], value)
		}
	}

	result := graphql.Do(graphql.Params{
		Schema: schema,
		RequestString: `{ __type(name: "Organization") {
			fields(includeDeprecated: true) { name args { name defaultValue } }
		} }`,
	})
	if len(result.Errors) != 0 {
		t.Fatalf("introspection errors: %v", result.Errors)
	}
	rendered := introspectedDefault(t, result.Data, "packages", "orderBy")
	// Known graphql-go limitation, documented above: a GraphQL-literal
	// renderer would print {field: CREATED_AT, direction: DESC}.
	if !strings.HasPrefix(rendered, `"map[`) {
		t.Fatalf("introspected orderBy default = %s; graphql-go appears to render input-object defaults properly now — drop the workaround note in this test and in the package docs", rendered)
	}
}

func introspectedDefault(t *testing.T, data interface{}, fieldName, argName string) string {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Type struct {
			Fields []struct {
				Name string `json:"name"`
				Args []struct {
					Name         string  `json:"name"`
					DefaultValue *string `json:"defaultValue"`
				} `json:"args"`
			} `json:"fields"`
		} `json:"__type"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, field := range envelope.Type.Fields {
		if field.Name != fieldName {
			continue
		}
		for _, argument := range field.Args {
			if argument.Name == argName {
				if argument.DefaultValue == nil {
					t.Fatalf("%s(%s) has no introspected default", fieldName, argName)
				}
				return *argument.DefaultValue
			}
		}
	}
	t.Fatalf("%s(%s) not found in introspection", fieldName, argName)
	return ""
}
