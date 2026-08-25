package bleephub

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/graphql-go/graphql"
)

// Typed-nil source ratchet.
//
// A resolver whose source is a map[string]interface{} must leave an absent
// child object *out* of the map (or store an untyped nil interface), never a
// nil map[string]interface{}. graphql-go's isNullish only reports a value as
// null for a nil pointer or a nil interface: a nil map arrives as
// reflect.Map, which is not nullish, so the executor descends into it as a
// real object and every non-null field of the child type — id, login, url —
// fails with "Cannot return null for non-nullable field X.id". The panic
// unwinds through the non-null chain until it reaches a nullable ancestor, so
// one absent optional child does not null one field: it discards a whole
// subtree and puts an error in the response, which is a hard failure for any
// client that treats `errors` as fatal (gh, octokit, graphql-client).
//
// The class has been found twice independently — GitActor.user for a commit
// signed by an address no account owns, and RepositoryVulnerabilityAlert
// .dismisser for an alert nobody dismissed — so the shape gets a ratchet
// rather than a code review habit.
//
// The guard is a runtime assertion driven by the whole server test suite,
// chosen over a static check because the defect is a *value* property (is
// this particular map nil at this particular moment) that no AST pass can
// decide without a whitelist of never-nil constructors, and over a
// synthesized schema-wide execution sweep because most root fields need
// arguments that only a seeded fixture can supply. Wrapping the field
// resolvers instead lets every GraphQL request the suite already makes pay
// for itself: reaching a type audits *all* of that type's members, not only
// the ones the query selected, so a test that asks a vulnerability alert for
// its number still catches a typed-nil dismisser.
//
// Findings are collected process-wide and reported by TestMain after
// m.Run(), the same shape as the OpenAPI response-shape ratchet.

var (
	graphqlSourceAuditMu       sync.Mutex
	graphqlSourceAuditFindings = map[string]string{}
	graphqlSourceAuditWrapped  = map[*graphql.FieldDefinition]bool{}
)

// graphqlSourceAuditMaxDepth bounds the walk into a returned source. Depth 4
// reaches connection → nodes → node → child object, which is as deep as any
// single resolver builds its own source in one go.
const graphqlSourceAuditMaxDepth = 4

// graphqlSourceAuditMaxElems bounds how many elements of one list are walked.
// A typed-nil node is a property of the renderer, not of the element index, so
// the first few elements carry the whole signal.
const graphqlSourceAuditMaxElems = 25

// recordGraphQLSourceViolation notes one typed-nil member. Keys are the
// GraphQL path (Type.field.child), so repeated hits across thousands of
// requests collapse to one line.
func recordGraphQLSourceViolation(key, detail string) {
	graphqlSourceAuditMu.Lock()
	defer graphqlSourceAuditMu.Unlock()
	if _, seen := graphqlSourceAuditFindings[key]; !seen {
		graphqlSourceAuditFindings[key] = detail
	}
}

// graphqlSourceAuditReport renders the collected findings, or "" when clean.
func graphqlSourceAuditReport() string {
	graphqlSourceAuditMu.Lock()
	defer graphqlSourceAuditMu.Unlock()
	if len(graphqlSourceAuditFindings) == 0 {
		return ""
	}
	keys := make([]string, 0, len(graphqlSourceAuditFindings))
	for key := range graphqlSourceAuditFindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "\ngraphql typed-nil source ratchet: %d field(s) put a typed-nil map into an object-typed slot.\n", len(keys))
	b.WriteString("graphql-go treats a nil map as a present object, so the child type's non-null fields fail\n")
	b.WriteString("with \"Cannot return null for non-nullable field …\" and abort the surrounding subtree.\n")
	b.WriteString("Wrap the member with optionalObject / optionalRendered (internal/graphqlapi/gql_optional_source.go).\n")
	for _, key := range keys {
		fmt.Fprintf(&b, "  %s\n    %s\n", key, graphqlSourceAuditFindings[key])
	}
	return b.String()
}

// instrumentGraphQLSourceAudit wraps every object field resolver in schema so
// each resolved value is walked for typed-nil object members. Wrapping is
// idempotent per field definition, so a suite that builds many servers pays
// the wrap once per schema.
func instrumentGraphQLSourceAudit(schema graphql.Schema) {
	graphqlSourceAuditMu.Lock()
	defer graphqlSourceAuditMu.Unlock()
	for _, ttype := range schema.TypeMap() {
		object, ok := ttype.(*graphql.Object)
		if !ok || strings.HasPrefix(object.Name(), "__") {
			continue
		}
		for name, def := range object.Fields() {
			if def == nil || graphqlSourceAuditWrapped[def] {
				continue
			}
			graphqlSourceAuditWrapped[def] = true
			inner := def.Resolve
			if inner == nil {
				inner = graphql.DefaultResolveFn
			}
			label := object.Name() + "." + name
			fieldType := def.Type
			def.Resolve = func(p graphql.ResolveParams) (interface{}, error) {
				value, err := inner(p)
				if err == nil {
					auditGraphQLSourceValue(label, fieldType, value)
				}
				return value, err
			}
		}
	}
}

// auditGraphQLSourceValue walks one resolved value against the GraphQL type it
// was produced for, recording every typed-nil map sitting where an object is
// expected. The walk is type-directed rather than key-directed: only members
// the schema declares as object/interface/union fields are inspected, so a nil
// map behind a JSON-ish custom scalar (which serializes to null perfectly
// well) is not mistaken for the defect, and the resolver's private bookkeeping
// keys are ignored.
func auditGraphQLSourceValue(label string, ttype graphql.Type, value interface{}) {
	auditGraphQLSourceWalk(recordGraphQLSourceViolation, label, "", ttype, value, 0, map[uintptr]bool{})
}

// auditGraphQLSourceWalk takes its sink as a parameter so the walk itself can
// be tested without writing into the process-wide findings.
func auditGraphQLSourceWalk(record func(key, detail string), label, path string, ttype graphql.Type, value interface{}, depth int, seen map[uintptr]bool) {
	if depth > graphqlSourceAuditMaxDepth || ttype == nil || value == nil {
		return
	}
	unwrapped := ttype
	if nonNull, ok := unwrapped.(*graphql.NonNull); ok {
		unwrapped = nonNull.OfType
	}
	if list, ok := unwrapped.(*graphql.List); ok {
		auditGraphQLSourceList(record, label, path, list, value, depth, seen)
		return
	}
	fields, composite := auditGraphQLCompositeFields(unwrapped)
	if !composite {
		return
	}
	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Map {
		return
	}
	if reflected.IsNil() {
		record(label+path, fmt.Sprintf(
			"a nil %s was stored where GraphQL type %s is expected; graphql-go renders it as a present object and the child's non-null fields fail",
			reflected.Type(), unwrapped))
		return
	}
	if seen[reflected.Pointer()] {
		return
	}
	seen[reflected.Pointer()] = true
	source, ok := value.(map[string]interface{})
	if !ok {
		return
	}
	for name, def := range fields {
		if def == nil {
			continue
		}
		member, present := source[name]
		if !present || member == nil {
			continue
		}
		auditGraphQLSourceWalk(record, label, path+"."+name, def.Type, member, depth+1, seen)
	}
}

// auditGraphQLSourceList walks the elements of a list-typed value.
func auditGraphQLSourceList(record func(key, detail string), label, path string, list *graphql.List, value interface{}, depth int, seen map[uintptr]bool) {
	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Slice && reflected.Kind() != reflect.Array {
		return
	}
	limit := reflected.Len()
	if limit > graphqlSourceAuditMaxElems {
		limit = graphqlSourceAuditMaxElems
	}
	for i := 0; i < limit; i++ {
		element := reflected.Index(i)
		if !element.IsValid() || !element.CanInterface() {
			continue
		}
		auditGraphQLSourceWalk(record, label, path+"[]", list.OfType, element.Interface(), depth+1, seen)
	}
}

// auditGraphQLCompositeFields reports the declared fields of an object or
// interface type, and whether the type is composite at all (unions carry no
// fields of their own but a typed nil is still wrong there).
func auditGraphQLCompositeFields(ttype graphql.Type) (graphql.FieldDefinitionMap, bool) {
	switch t := ttype.(type) {
	case *graphql.Object:
		return t.Fields(), true
	case *graphql.Interface:
		return t.Fields(), true
	case *graphql.Union:
		return nil, true
	}
	return nil, false
}

// TestGraphQLSourceAuditIsWired keeps the ratchet from going vacuous: the
// shared harness's schema must be fully instrumented, so deleting the
// instrumentGraphQLSourceAudit call (or adding a server constructor that skips
// it) fails here rather than quietly turning the audit into a no-op.
func TestGraphQLSourceAuditIsWired(t *testing.T) {
	t.Parallel()
	schema := newIsolatedServer(t).graphql.Schema()
	graphqlSourceAuditMu.Lock()
	defer graphqlSourceAuditMu.Unlock()
	for _, ttype := range schema.TypeMap() {
		object, ok := ttype.(*graphql.Object)
		if !ok || strings.HasPrefix(object.Name(), "__") {
			continue
		}
		for name, def := range object.Fields() {
			if def != nil && !graphqlSourceAuditWrapped[def] {
				t.Fatalf("%s.%s is not instrumented for the typed-nil source ratchet", object.Name(), name)
			}
		}
	}
}

// TestGraphQLSourceAuditDetectsATypedNilMember exercises the detector itself
// against a schema of its own, so a refactor that stops recognising the shape
// fails here instead of turning every future run green for the wrong reason.
func TestGraphQLSourceAuditDetectsATypedNilMember(t *testing.T) {
	t.Parallel()
	child := graphql.NewObject(graphql.ObjectConfig{
		Name:   "AuditChild",
		Fields: graphql.Fields{"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)}},
	})
	parent := graphql.NewObject(graphql.ObjectConfig{
		Name: "AuditParent",
		Fields: graphql.Fields{
			"child":   &graphql.Field{Type: child},
			"payload": &graphql.Field{Type: graphql.String},
		},
	})

	var typedNil map[string]interface{}
	cases := map[string]struct {
		source interface{}
		want   []string
	}{
		"typed nil member":     {map[string]interface{}{"child": typedNil}, []string{"Query.parent.child"}},
		"untyped nil member":   {map[string]interface{}{"child": nil}, nil},
		"absent member":        {map[string]interface{}{}, nil},
		"present child":        {map[string]interface{}{"child": map[string]interface{}{"id": "1"}}, nil},
		"typed nil at the top": {typedNil, []string{"Query.parent"}},
		// A nil map behind a leaf field serializes to null perfectly well and
		// must not be reported.
		"typed nil behind a scalar": {map[string]interface{}{"payload": typedNil}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got []string
			auditGraphQLSourceWalk(func(key, _ string) { got = append(got, key) },
				"Query.parent", "", parent, tc.source, 0, map[uintptr]bool{})
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("findings = %v, want %v", got, tc.want)
			}
		})
	}
}
