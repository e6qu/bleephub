package bleephub

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/graphqlschema"
)

// builtinScalarGaps are the only differences a complete generated schema
// may have from GitHub's SDL. GitHub's published schema does not declare
// the five GraphQL built-in scalars (they are implicit in every schema),
// but they are real types in any assembled schema and introspection
// reports them, so the SDL-directed diff necessarily names them. The
// served schema's ratchet allowlists the same five.
var builtinScalarGaps = []string{
	"type\tBoolean\tSCALAR is absent from GitHub",
	"type\tFloat\tSCALAR is absent from GitHub",
	"type\tID\tSCALAR is absent from GitHub",
	"type\tInt\tSCALAR is absent from GitHub",
	"type\tString\tSCALAR is absent from GitHub",
}

// TestGeneratedGraphQLSchemaIsComplete is the completeness oracle for the
// generated GitHub schema (internal/graphqlschema, emitted by
// internal/graphqlschemagen). It assembles the generated types into a real
// schema, introspects it exactly as the served-schema ratchet introspects
// bleephub's own, and requires:
//
//   - every type GitHub publishes to exist, with the same kind;
//   - every field and input field to exist, with GitHub's exact type,
//     including nullability and list nesting;
//   - every argument to exist with GitHub's exact type;
//   - every enum value, interface implementation and union member to
//     match; and
//   - nothing beyond the five built-in scalars to exist that GitHub does
//     not publish.
//
// Zero missing types and zero missing fields is the phase's stated goal;
// this is where the claim is checked rather than asserted.
func TestGeneratedGraphQLSchemaIsComplete(t *testing.T) {
	registry := graphqlschema.New()
	schema, err := registry.Schema()
	if err != nil {
		t.Fatalf("assemble the generated GitHub schema: %v", err)
	}
	generated := introspectionShape(t, schema)
	official := officialGraphQLShape(t)

	report := graphQLCoverageReport(generated, official, nil)
	t.Logf("generated schema: %d types (%d shared with GitHub, %d missing), %d of %d official fields implemented, %d with an exact signature",
		report.BleephubTypes, report.SharedTypes, len(report.MissingTypes),
		report.ImplementedFields, report.OfficialFields, report.SignatureExactFields)

	if len(report.MissingTypes) != 0 {
		t.Errorf("generated schema is missing %d GitHub types: %s",
			len(report.MissingTypes), strings.Join(sample(report.MissingTypes, 20), ", "))
	}
	if len(report.MissingFields) != 0 {
		var lines []string
		for typeName, fields := range report.MissingFields {
			lines = append(lines, typeName+"."+strings.Join(fields, ","))
		}
		sort.Strings(lines)
		t.Errorf("generated schema is missing fields on %d types: %s",
			len(report.MissingFields), strings.Join(sample(lines, 20), " "))
	}
	if report.SharedTypes != report.OfficialTypes {
		t.Errorf("generated schema shares %d of GitHub's %d types", report.SharedTypes, report.OfficialTypes)
	}
	if report.ImplementedFields != report.OfficialFields {
		t.Errorf("generated schema implements %d of GitHub's %d fields", report.ImplementedFields, report.OfficialFields)
	}
	// A field whose type or nullability differs from GitHub's is worse than
	// a missing one: a client trusts the signature it introspects.
	if report.SignatureExactFields != report.OfficialFields {
		t.Errorf("generated schema reproduces %d of GitHub's %d field signatures exactly",
			report.SignatureExactFields, report.OfficialFields)
	}

	// The reverse direction: nothing invented, no wrong argument types, no
	// enum value, interface or union member GitHub does not have.
	gaps := graphQLCompatibilityGaps(generated, official)
	if diff := diffStrings(gaps, builtinScalarGaps); diff != "" {
		t.Errorf("generated schema deviates from GitHub's published schema:\n%s", diff)
	}
}

// TestGeneratedGraphQLSchemaCoversTheServedSchema proves the generated
// universe is a superset of the hand-written one that serves traffic
// today, which is the precondition for swapping the serving schema over in
// a later phase: no served type or field may be absent from the generated
// definitions.
func TestGeneratedGraphQLSchemaCoversTheServedSchema(t *testing.T) {
	registry := graphqlschema.New()
	schema, err := registry.Schema()
	if err != nil {
		t.Fatalf("assemble the generated GitHub schema: %v", err)
	}
	generated := introspectionShape(t, schema)
	served := bleephubIntrospectionShape(t)

	var missing []string
	for typeName, servedType := range served.Types {
		generatedType, ok := generated.Types[typeName]
		if !ok {
			missing = append(missing, "type "+typeName)
			continue
		}
		for fieldName := range servedType.Fields {
			if _, ok := generatedType.Fields[fieldName]; !ok {
				missing = append(missing, "field "+typeName+"."+fieldName)
			}
		}
		for fieldName := range servedType.InputFields {
			if _, ok := generatedType.InputFields[fieldName]; !ok {
				missing = append(missing, "input field "+typeName+"."+fieldName)
			}
		}
	}
	sort.Strings(missing)
	// bleephub deliberately serves one non-GitHub mutation (deleteRepository
	// and its input/payload types, allowlisted by the served-schema
	// ratchet). The generated schema is GitHub's published surface, so it
	// cannot contain them; everything else must be covered.
	expected := []string{
		"field Mutation.deleteRepository",
		"type DeleteRepositoryInput",
		"type DeleteRepositoryPayload",
	}
	if diff := diffStrings(missing, expected); diff != "" {
		t.Errorf("served schema surface is not covered by the generated schema:\n%s", diff)
	}
}

// diffStrings reports the symmetric difference between two sorted string
// slices, or "" when they are equal.
func diffStrings(got, want []string) string {
	present := map[string]bool{}
	for _, value := range want {
		present[value] = true
	}
	var builder strings.Builder
	for _, value := range got {
		if !present[value] {
			fmt.Fprintf(&builder, "  unexpected: %s\n", value)
		}
		delete(present, value)
	}
	remaining := make([]string, 0, len(present))
	for value := range present {
		remaining = append(remaining, value)
	}
	sort.Strings(remaining)
	for _, value := range remaining {
		fmt.Fprintf(&builder, "  no longer present: %s\n", value)
	}
	return builder.String()
}

func sample(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return append(append([]string{}, values[:limit]...), fmt.Sprintf("… and %d more", len(values)-limit))
}
