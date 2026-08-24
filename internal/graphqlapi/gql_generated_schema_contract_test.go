package graphqlapi

import (
	"testing"

	"github.com/graphql-go/graphql/language/ast"

	"github.com/e6qu/bleephub/internal/graphqlschema"
)

// TestGeneratedScalarsMatchTheServedScalars pins the generated GitHub
// schema's custom scalars to the ones this package already serves.
//
// internal/graphqlschema cannot import this package (the dependency runs
// the other way once the serving schema is switched over), so it carries
// its own string-scalar constructor. That makes drift possible in
// principle: a generated URI that serialized differently from the served
// URI would change bytes on the wire the moment a resolver moved onto the
// generated type. This test makes the two constructors' behaviour one
// reviewed decision instead of two independent ones.
func TestGeneratedScalarsMatchTheServedScalars(t *testing.T) {
	// Every custom scalar GitHub declares, plus the subset this package
	// already defines — all of them are transported as JSON strings.
	for _, name := range []string{
		"Base64String", "BigInt", "CustomPropertyValue", "Date", "DateTime",
		"GitObjectID", "GitRefname", "GitSSHRemote", "GitTimestamp", "HTML",
		"PreciseDateTime", "URI", "X509Certificate",
	} {
		served := stringScalar(name)
		generated := graphqlschema.NewStringScalar(name, "")
		if served.Name() != generated.Name() {
			t.Fatalf("scalar name %q != %q", served.Name(), generated.Name())
		}
		for _, value := range []interface{}{
			"https://example.test/octocat",
			"2026-01-02T03:04:05Z",
			42,
			int64(9007199254740993),
			true,
			nil,
		} {
			if want, got := served.Serialize(value), generated.Serialize(value); want != got {
				t.Fatalf("%s.Serialize(%#v) = %#v, served scalar returns %#v", name, value, got, want)
			}
			if want, got := served.ParseValue(value), generated.ParseValue(value); want != got {
				t.Fatalf("%s.ParseValue(%#v) = %#v, served scalar returns %#v", name, value, got, want)
			}
		}
		for _, literal := range []ast.Value{
			ast.NewStringValue(&ast.StringValue{Value: "refs/heads/main"}),
			ast.NewIntValue(&ast.IntValue{Value: "7"}),
			ast.NewBooleanValue(&ast.BooleanValue{Value: true}),
		} {
			if want, got := served.ParseLiteral(literal), generated.ParseLiteral(literal); want != got {
				t.Fatalf("%s.ParseLiteral(%T) = %#v, served scalar returns %#v", name, literal, got, want)
			}
		}
	}
}

// BenchmarkServedSchemaConstruction measures assembling the hand-written
// schema this package serves today. It is the baseline the generated
// schema's construction cost (internal/graphqlschema's
// BenchmarkSchemaConstruction) is judged against when the serving schema is
// switched over: the swap must not turn schema assembly into a startup cost
// anyone notices.
func BenchmarkServedSchemaConstruction(b *testing.B) {
	b.ReportAllocs()
	resolver := newStubbedResolver()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resolver.initGraphQLSchema()
	}
}
