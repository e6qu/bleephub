package graphqlapi

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"

	"github.com/e6qu/bleephub/internal/store"
)

func stringScalar(name string) *graphql.Scalar {
	return graphql.NewScalar(graphql.ScalarConfig{
		Name:      name,
		Serialize: func(value interface{}) interface{} { return fmt.Sprint(value) },
		ParseValue: func(value interface{}) interface{} {
			return fmt.Sprint(value)
		},
		ParseLiteral: func(valueAST ast.Value) interface{} {
			if value, ok := valueAST.(*ast.StringValue); ok {
				return value.Value
			}
			return nil
		},
	})
}

func (s *Resolver) graphQLStringScalar(name string) *graphql.Scalar {
	if s.graphqlTypes.scalars == nil {
		s.graphqlTypes.scalars = map[string]*graphql.Scalar{}
	}
	if scalar := s.graphqlTypes.scalars[name]; scalar != nil {
		return scalar
	}
	scalar := stringScalar(name)
	s.graphqlTypes.scalars[name] = scalar
	return scalar
}

// addMetaFieldsToSchema implements the small, widely-used root family that
// Octokit and schema-aware clients use for capability discovery.
func (s *Resolver) addMetaFieldsToSchema(queryType *graphql.Object) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	gitObjectID := s.graphQLStringScalar("GitObjectID")

	rateLimitType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RateLimit",
		Fields: graphql.Fields{
			"cost":      &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"limit":     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"nodeCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"remaining": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"resetAt":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"used":      &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	queryType.AddFieldConfig("rateLimit", &graphql.Field{
		Type: rateLimitType,
		Args: graphql.FieldConfigArgument{
			"dryRun": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			rate := s.apiRate(p.Context)
			if rate.Limit == 0 {
				rate = RateSnapshot{
					Limit: 5000, Used: 1, Remaining: 4999,
					Reset: time.Now().UTC().Add(time.Hour).Unix(),
				}
			}
			return map[string]interface{}{
				"cost": 1, "limit": rate.Limit, "nodeCount": 0, "remaining": rate.Remaining,
				"resetAt": time.Unix(rate.Reset, 0).UTC().Format(time.RFC3339), "used": rate.Used,
			}, nil
		},
	})

	metaType := graphql.NewObject(graphql.ObjectConfig{
		Name: "GitHubMetadata",
		Fields: graphql.Fields{
			"gitHubServicesSha":                   &graphql.Field{Type: graphql.NewNonNull(gitObjectID)},
			"gitIpAddresses":                      &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"githubEnterpriseImporterIpAddresses": &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"hookIpAddresses":                     &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"importerIpAddresses":                 &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"isPasswordAuthenticationVerifiable":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"pagesIpAddresses":                    &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
		},
	})
	queryType.AddFieldConfig("meta", &graphql.Field{
		Type: graphql.NewNonNull(metaType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			sha := strings.ToLower(s.buildCommit())
			if len(sha) != 40 {
				sha = strings.Repeat("0", 40)
			}
			return map[string]interface{}{
				"gitHubServicesSha": sha, "gitIpAddresses": []string{},
				"githubEnterpriseImporterIpAddresses": []string{}, "hookIpAddresses": []string{},
				"importerIpAddresses": []string{}, "isPasswordAuthenticationVerifiable": true,
				"pagesIpAddresses": []string{},
			}, nil
		},
	})

	licenseType := s.gqlLicenseType()
	queryType.AddFieldConfig("license", &graphql.Field{
		Type: licenseType,
		Args: graphql.FieldConfigArgument{
			"key": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// Comma-ok: the arg is String! so graphql-go's validation normally
			// guarantees a string here, but never assert a request-derived value
			// unconditionally — a coercion edge case would panic instead of
			// returning a clean null.
			key, ok := p.Args["key"].(string)
			if !ok {
				return nil, nil
			}
			return graphQLLicenseJSON(key), nil
		},
	})
	queryType.AddFieldConfig("licenses", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(licenseType)),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			keys := make([]string, 0, len(licenseTemplates))
			for key := range licenseTemplates {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			out := make([]interface{}, 0, len(keys))
			for _, key := range keys {
				out = append(out, graphQLLicenseJSON(key))
			}
			return out, nil
		},
	})

	codeOfConductType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CodeOfConduct",
		Fields: graphql.Fields{
			"body":         &graphql.Field{Type: graphql.String},
			"id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"key":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"resourcePath": &graphql.Field{Type: uri},
			"url":          &graphql.Field{Type: uri},
		},
	})
	codeOfConductJSON := func(c store.CodeOfConduct) map[string]interface{} {
		return map[string]interface{}{
			"body": c.Body, "id": "COC_" + c.Key, "key": c.Key, "name": c.Name,
			"resourcePath": "/codes_of_conduct/" + c.Key, "url": nil,
		}
	}
	queryType.AddFieldConfig("codeOfConduct", &graphql.Field{
		Type: codeOfConductType,
		Args: graphql.FieldConfigArgument{
			"key": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			key, ok := p.Args["key"].(string)
			if !ok {
				return nil, nil
			}
			for _, c := range store.CodesOfConductCatalog {
				if c.Key == key {
					return codeOfConductJSON(c), nil
				}
			}
			return nil, nil
		},
	})
	queryType.AddFieldConfig("codesOfConduct", &graphql.Field{
		Type: graphql.NewList(codeOfConductType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			out := make([]interface{}, 0, len(store.CodesOfConductCatalog))
			for _, c := range store.CodesOfConductCatalog {
				out = append(out, codeOfConductJSON(c))
			}
			return out, nil
		},
	})

	queryType.AddFieldConfig("id", &graphql.Field{
		Type: graphql.NewNonNull(graphql.ID),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return "Query", nil
		},
	})
	queryType.AddFieldConfig("relay", &graphql.Field{
		Type: graphql.NewNonNull(queryType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return map[string]interface{}{}, nil
		},
	})
}

// gqlLicenseType returns the shared License object type (memoized). Both
// Query.license/licenses and Repository.licenseInfo resolve to this type —
// matching GitHub, where licenseInfo is a full License, not a summary fork.
func (s *Resolver) gqlLicenseType() *graphql.Object {
	if s.graphqlTypes.license != nil {
		return s.graphqlTypes.license
	}
	uri := s.graphQLStringScalar("URI")
	licenseRuleType := graphql.NewObject(graphql.ObjectConfig{
		Name: "LicenseRule",
		Fields: graphql.Fields{
			"description": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"key":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"label":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	s.graphqlTypes.license = graphql.NewObject(graphql.ObjectConfig{
		Name: "License",
		Fields: graphql.Fields{
			"body":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"conditions":     &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(licenseRuleType))},
			"description":    &graphql.Field{Type: graphql.String},
			"featured":       &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"hidden":         &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"implementation": &graphql.Field{Type: graphql.String},
			"key":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"limitations":    &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(licenseRuleType))},
			"name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"nickname":       &graphql.Field{Type: graphql.String},
			"permissions":    &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(licenseRuleType))},
			"pseudoLicense":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"spdxId":         &graphql.Field{Type: graphql.String},
			"url":            &graphql.Field{Type: uri},
		},
	})
	return s.graphqlTypes.license
}

// graphQLLicenseJSON renders the GraphQL source map for a catalog license
// key, or nil when the key names no vendored license template.
func graphQLLicenseJSON(key string) interface{} {
	tmpl, ok := licenseTemplates[strings.ToLower(key)]
	if !ok {
		return nil
	}
	return map[string]interface{}{
		"body": tmpl.Body, "conditions": []interface{}{}, "description": nil,
		"featured": false, "hidden": false, "id": tmpl.NodeID,
		"implementation": nil, "key": strings.ToLower(key), "limitations": []interface{}{},
		"name": tmpl.Name, "nickname": nil, "permissions": []interface{}{},
		"pseudoLicense": false, "spdxId": tmpl.SpdxID,
		"url": "https://choosealicense.com/licenses/" + strings.ToLower(key) + "/",
	}
}
