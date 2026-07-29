package bleephub

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

var leadingUnionMember = regexp.MustCompile(`=\s*\n\s*\|`)
var nullDefaultValue = regexp.MustCompile(`\s*=\s*null\b`)

const (
	githubGraphQLSchemaFile   = "testdata/github-graphql-schema.graphql.gz"
	githubGraphQLVersionFile  = "testdata/github-graphql-schema.VERSION"
	bleephubGraphQLSnapshot   = "testdata/bleephub-graphql-introspection.json"
	graphQLGapAllowlist       = "testdata/graphql-schema-gap-allowlist.txt"
	graphQLCoverageReportFile = "../../specs/graphql-schema-coverage.json"
)

const schemaIntrospectionQuery = `
query BleephubSchemaRatchet {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types {
      kind
      name
      fields(includeDeprecated: true) {
        name
        type { ...TypeRef }
        args { name type { ...TypeRef } }
      }
      inputFields { name type { ...TypeRef } }
      interfaces { name }
      enumValues(includeDeprecated: true) { name }
      possibleTypes { name }
    }
  }
}
fragment TypeRef on __Type {
  kind
  name
  ofType {
    kind
    name
    ofType {
      kind
      name
      ofType {
        kind
        name
        ofType {
          kind
          name
          ofType {
            kind
            name
            ofType { kind name }
          }
        }
      }
    }
  }
}`

type schemaTypeRef struct {
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	OfType *schemaTypeRef `json:"ofType"`
}

func (r *schemaTypeRef) String() string {
	if r == nil {
		return ""
	}
	switch r.Kind {
	case "NON_NULL":
		return r.OfType.String() + "!"
	case "LIST":
		return "[" + r.OfType.String() + "]"
	default:
		return r.Name
	}
}

type introspectionNamed struct {
	Name string `json:"name"`
}

type introspectionInput struct {
	Name string         `json:"name"`
	Type *schemaTypeRef `json:"type"`
}

type introspectionField struct {
	Name string               `json:"name"`
	Type *schemaTypeRef       `json:"type"`
	Args []introspectionInput `json:"args"`
}

type introspectionType struct {
	Kind          string               `json:"kind"`
	Name          string               `json:"name"`
	Fields        []introspectionField `json:"fields"`
	InputFields   []introspectionInput `json:"inputFields"`
	Interfaces    []introspectionNamed `json:"interfaces"`
	EnumValues    []introspectionNamed `json:"enumValues"`
	PossibleTypes []introspectionNamed `json:"possibleTypes"`
}

type introspectionEnvelope struct {
	Schema struct {
		QueryType        *introspectionNamed `json:"queryType"`
		MutationType     *introspectionNamed `json:"mutationType"`
		SubscriptionType *introspectionNamed `json:"subscriptionType"`
		Types            []introspectionType `json:"types"`
	} `json:"__schema"`
}

type schemaFieldShape struct {
	Type string            `json:"type"`
	Args map[string]string `json:"args,omitempty"`
}

type schemaTypeShape struct {
	Kind          string                      `json:"kind"`
	Fields        map[string]schemaFieldShape `json:"fields,omitempty"`
	InputFields   map[string]string           `json:"input_fields,omitempty"`
	Interfaces    []string                    `json:"interfaces,omitempty"`
	EnumValues    []string                    `json:"enum_values,omitempty"`
	PossibleTypes []string                    `json:"possible_types,omitempty"`
}

type schemaShape struct {
	QueryType        string                     `json:"query_type"`
	MutationType     string                     `json:"mutation_type,omitempty"`
	SubscriptionType string                     `json:"subscription_type,omitempty"`
	Types            map[string]schemaTypeShape `json:"types"`
}

func TestVendoredGitHubGraphQLSchemaMatchesRecordedPin(t *testing.T) {
	meta, err := os.ReadFile(githubGraphQLVersionFile)
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for _, line := range strings.Split(string(meta), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if ok {
			pins[key] = strings.TrimSpace(value)
		}
	}
	want := pins["content sha256"]
	if len(want) != 64 {
		t.Fatalf("%s content sha256 is malformed: %q", githubGraphQLVersionFile, want)
	}
	file, err := os.Open(githubGraphQLSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, reader); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Fatalf("GitHub GraphQL schema digest = %s, pin records %s", got, want)
	}
	if pins["source"] != "https://docs.github.com/public/fpt/schema.docs.graphql" {
		t.Fatalf("GraphQL schema source is not GitHub's official public-schema URL: %q", pins["source"])
	}
}

func bleephubIntrospectionShape(t *testing.T) schemaShape {
	t.Helper()
	server := newTestServer()
	server.initGraphQLSchema()
	result := graphql.Do(graphql.Params{
		Schema:        server.graphqlSchema,
		RequestString: schemaIntrospectionQuery,
	})
	if len(result.Errors) != 0 {
		t.Fatalf("introspection errors: %v", result.Errors)
	}
	raw, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	var envelope introspectionEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	shape := schemaShape{Types: map[string]schemaTypeShape{}}
	if envelope.Schema.QueryType != nil {
		shape.QueryType = envelope.Schema.QueryType.Name
	}
	if envelope.Schema.MutationType != nil {
		shape.MutationType = envelope.Schema.MutationType.Name
	}
	if envelope.Schema.SubscriptionType != nil {
		shape.SubscriptionType = envelope.Schema.SubscriptionType.Name
	}
	for _, typ := range envelope.Schema.Types {
		if strings.HasPrefix(typ.Name, "__") {
			continue
		}
		item := schemaTypeShape{Kind: typ.Kind}
		if len(typ.Fields) != 0 {
			item.Fields = map[string]schemaFieldShape{}
			for _, field := range typ.Fields {
				args := map[string]string{}
				for _, arg := range field.Args {
					args[arg.Name] = arg.Type.String()
				}
				item.Fields[field.Name] = schemaFieldShape{Type: field.Type.String(), Args: args}
			}
		}
		if len(typ.InputFields) != 0 {
			item.InputFields = map[string]string{}
			for _, field := range typ.InputFields {
				item.InputFields[field.Name] = field.Type.String()
			}
		}
		for _, value := range typ.Interfaces {
			item.Interfaces = append(item.Interfaces, value.Name)
		}
		for _, value := range typ.EnumValues {
			item.EnumValues = append(item.EnumValues, value.Name)
		}
		for _, value := range typ.PossibleTypes {
			item.PossibleTypes = append(item.PossibleTypes, value.Name)
		}
		sort.Strings(item.Interfaces)
		sort.Strings(item.EnumValues)
		sort.Strings(item.PossibleTypes)
		shape.Types[typ.Name] = item
	}
	return shape
}

func officialGraphQLShape(t *testing.T) schemaShape {
	t.Helper()
	file, err := os.Open(githubGraphQLSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	// github.com/graphql-go/graphql's SDL parser predates the optional leading
	// pipe style the official schema formatter uses for multiline unions and
	// the explicit null default now emitted for nullable inputs. Neither
	// formatting choice changes the type contract this ratchet compares.
	body = leadingUnionMember.ReplaceAll(body, []byte("= "))
	body = nullDefaultValue.ReplaceAll(body, nil)
	document, err := parser.Parse(parser.ParseParams{
		Source: source.NewSource(&source.Source{Body: body, Name: "GitHub public schema"}),
	})
	if err != nil {
		t.Fatalf("parse official GitHub GraphQL schema: %v", err)
	}
	shape := schemaShape{
		QueryType:    "Query",
		MutationType: "Mutation",
		Types:        map[string]schemaTypeShape{},
	}
	for _, definition := range document.Definitions {
		switch node := definition.(type) {
		case *ast.ObjectDefinition:
			item := fieldsFromAST("OBJECT", node.Fields)
			for _, iface := range node.Interfaces {
				item.Interfaces = append(item.Interfaces, iface.Name.Value)
			}
			sort.Strings(item.Interfaces)
			shape.Types[node.Name.Value] = item
		case *ast.InterfaceDefinition:
			shape.Types[node.Name.Value] = fieldsFromAST("INTERFACE", node.Fields)
		case *ast.InputObjectDefinition:
			fields := map[string]string{}
			for _, field := range node.Fields {
				fields[field.Name.Value] = astTypeString(field.Type)
			}
			shape.Types[node.Name.Value] = schemaTypeShape{Kind: "INPUT_OBJECT", InputFields: fields}
		case *ast.EnumDefinition:
			values := make([]string, 0, len(node.Values))
			for _, value := range node.Values {
				values = append(values, value.Name.Value)
			}
			sort.Strings(values)
			shape.Types[node.Name.Value] = schemaTypeShape{Kind: "ENUM", EnumValues: values}
		case *ast.UnionDefinition:
			types := make([]string, 0, len(node.Types))
			for _, typ := range node.Types {
				types = append(types, typ.Name.Value)
			}
			sort.Strings(types)
			shape.Types[node.Name.Value] = schemaTypeShape{Kind: "UNION", PossibleTypes: types}
		case *ast.ScalarDefinition:
			shape.Types[node.Name.Value] = schemaTypeShape{Kind: "SCALAR"}
		}
	}
	for name, typ := range shape.Types {
		for _, iface := range typ.Interfaces {
			item := shape.Types[iface]
			item.PossibleTypes = append(item.PossibleTypes, name)
			sort.Strings(item.PossibleTypes)
			shape.Types[iface] = item
		}
	}
	return shape
}

func fieldsFromAST(kind string, fields []*ast.FieldDefinition) schemaTypeShape {
	item := schemaTypeShape{Kind: kind, Fields: map[string]schemaFieldShape{}}
	for _, field := range fields {
		args := map[string]string{}
		for _, arg := range field.Arguments {
			args[arg.Name.Value] = astTypeString(arg.Type)
		}
		item.Fields[field.Name.Value] = schemaFieldShape{Type: astTypeString(field.Type), Args: args}
	}
	return item
}

func astTypeString(value ast.Type) string {
	switch typ := value.(type) {
	case *ast.Named:
		return typ.Name.Value
	case *ast.List:
		return "[" + astTypeString(typ.Type) + "]"
	case *ast.NonNull:
		return astTypeString(typ.Type) + "!"
	default:
		return ""
	}
}

func graphQLCompatibilityGaps(bleephub, official schemaShape) []string {
	var gaps []string
	for typeName, localType := range bleephub.Types {
		upstreamType, ok := official.Types[typeName]
		if !ok {
			gaps = append(gaps, fmt.Sprintf("type\t%s\t%s is absent from GitHub", typeName, localType.Kind))
			continue
		}
		if localType.Kind != upstreamType.Kind {
			gaps = append(gaps, fmt.Sprintf("type-kind\t%s\t%s\t%s", typeName, localType.Kind, upstreamType.Kind))
			continue
		}
		for fieldName, localField := range localType.Fields {
			upstreamField, ok := upstreamType.Fields[fieldName]
			qualified := typeName + "." + fieldName
			if !ok {
				gaps = append(gaps, "field\t"+qualified+"\tabsent from GitHub")
				continue
			}
			if localField.Type != upstreamField.Type {
				gaps = append(gaps, fmt.Sprintf("field-type\t%s\t%s\t%s", qualified, localField.Type, upstreamField.Type))
			}
			for argName, localArg := range localField.Args {
				upstreamArg, ok := upstreamField.Args[argName]
				if !ok {
					gaps = append(gaps, "argument\t"+qualified+"("+argName+")\tabsent from GitHub")
				} else if localArg != upstreamArg {
					gaps = append(gaps, fmt.Sprintf("argument-type\t%s(%s)\t%s\t%s", qualified, argName, localArg, upstreamArg))
				}
			}
		}
		for fieldName, localField := range localType.InputFields {
			upstreamField, ok := upstreamType.InputFields[fieldName]
			qualified := typeName + "." + fieldName
			if !ok {
				gaps = append(gaps, "input-field\t"+qualified+"\tabsent from GitHub")
			} else if localField != upstreamField {
				gaps = append(gaps, fmt.Sprintf("input-field-type\t%s\t%s\t%s", qualified, localField, upstreamField))
			}
		}
		for _, value := range localType.EnumValues {
			if !containsString(upstreamType.EnumValues, value) {
				gaps = append(gaps, "enum-value\t"+typeName+"."+value+"\tabsent from GitHub")
			}
		}
		for _, value := range localType.Interfaces {
			if !containsString(upstreamType.Interfaces, value) {
				gaps = append(gaps, "interface\t"+typeName+"."+value+"\tabsent from GitHub")
			}
		}
		for _, value := range localType.PossibleTypes {
			if !containsString(upstreamType.PossibleTypes, value) {
				gaps = append(gaps, "possible-type\t"+typeName+"."+value+"\tabsent from GitHub")
			}
		}
	}
	sort.Strings(gaps)
	return gaps
}

func containsString(values []string, want string) bool {
	index := sort.SearchStrings(values, want)
	return index < len(values) && values[index] == want
}

type graphQLCoverage struct {
	OfficialTypes         int                 `json:"official_types"`
	BleephubTypes         int                 `json:"bleephub_types"`
	SharedTypes           int                 `json:"shared_types"`
	OfficialFields        int                 `json:"official_fields"`
	ImplementedFields     int                 `json:"implemented_fields"`
	SignatureExactFields  int                 `json:"signature_exact_fields"`
	CompatibilityGapCount int                 `json:"compatibility_gap_count"`
	MissingTypes          []string            `json:"missing_types"`
	MissingFields         map[string][]string `json:"missing_fields"`
}

func graphQLCoverageReport(bleephub, official schemaShape, gaps []string) graphQLCoverage {
	report := graphQLCoverage{
		OfficialTypes:         len(official.Types),
		BleephubTypes:         len(bleephub.Types),
		CompatibilityGapCount: len(gaps),
		MissingFields:         map[string][]string{},
	}
	for typeName, upstreamType := range official.Types {
		report.OfficialFields += len(upstreamType.Fields) + len(upstreamType.InputFields)
		localType, ok := bleephub.Types[typeName]
		if !ok {
			report.MissingTypes = append(report.MissingTypes, typeName)
			continue
		}
		report.SharedTypes++
		for fieldName, upstreamField := range upstreamType.Fields {
			localField, ok := localType.Fields[fieldName]
			if !ok {
				report.MissingFields[typeName] = append(report.MissingFields[typeName], fieldName)
				continue
			}
			report.ImplementedFields++
			if localField.Type == upstreamField.Type {
				report.SignatureExactFields++
			}
		}
		for fieldName, upstreamField := range upstreamType.InputFields {
			localField, ok := localType.InputFields[fieldName]
			if !ok {
				report.MissingFields[typeName] = append(report.MissingFields[typeName], fieldName)
				continue
			}
			report.ImplementedFields++
			if localField == upstreamField {
				report.SignatureExactFields++
			}
		}
	}
	sort.Strings(report.MissingTypes)
	for _, fields := range report.MissingFields {
		sort.Strings(fields)
	}
	return report
}

func canonicalJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(body, '\n')
}

func TestGraphQLSchemaMatchesIntrospectionAndOfficialRatchets(t *testing.T) {
	bleephub := bleephubIntrospectionShape(t)
	official := officialGraphQLShape(t)
	gaps := graphQLCompatibilityGaps(bleephub, official)
	report := graphQLCoverageReport(bleephub, official, gaps)
	// Snapshot updates are an explicit review action, but they must never be
	// able to bless less coverage or more incompatibility. These monotonic
	// floors make "update the generated files" incapable of hiding a schema
	// regression.
	if report.ImplementedFields < 779 {
		t.Fatalf("GraphQL implemented fields regressed to %d; floor is 779", report.ImplementedFields)
	}
	if report.SignatureExactFields < 543 {
		t.Fatalf("GraphQL exact-signature fields regressed to %d; floor is 543", report.SignatureExactFields)
	}
	if report.CompatibilityGapCount > 319 {
		t.Fatalf("GraphQL compatibility gaps grew to %d; ceiling is 319", report.CompatibilityGapCount)
	}
	snapshot := canonicalJSON(t, bleephub)
	coverage := canonicalJSON(t, report)
	allowlist := []byte(strings.Join(gaps, "\n") + "\n")

	if os.Getenv("BLEEPHUB_UPDATE_GRAPHQL_SCHEMA") == "1" {
		for path, body := range map[string][]byte{
			bleephubGraphQLSnapshot:   snapshot,
			graphQLGapAllowlist:       allowlist,
			graphQLCoverageReportFile: coverage,
		} {
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("updated %s", path)
		}
		return
	}

	assertFileBytes(t, bleephubGraphQLSnapshot, snapshot,
		"the introspectable Bleephub schema changed; review it and run BLEEPHUB_UPDATE_GRAPHQL_SCHEMA=1 go test ./internal/server -run TestGraphQLSchemaMatchesIntrospectionAndOfficialRatchets")
	assertFileBytes(t, graphQLGapAllowlist, allowlist,
		"the GitHub schema compatibility gap set changed; fix new gaps or remove closed ones before updating the ratchet")
	assertFileBytes(t, graphQLCoverageReportFile, coverage,
		"the official GraphQL coverage report changed; review missing and newly implemented surface before updating the ratchet")
}

func assertFileBytes(t *testing.T, path string, got []byte, message string) {
	t.Helper()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v\n%s", path, err, message)
	}
	if string(want) != string(got) {
		t.Fatalf("%s is stale\n%s", path, message)
	}
}
