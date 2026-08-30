package graphqlapi

// Repository custom-property mutations: the definition CRUD,
// promoteRepositoryCustomProperty (lifts an org definition into the enterprise
// schema, mirroring the REST promote route) and setRepositoryCustomPropertyValues
// (per-repository values through the same validated store write REST drives).

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func init() {
	for name, rule := range map[string]mutationRule{
		"createRepositoryCustomProperty":    customPropertyRule{sourceKey: "sourceId"},
		"updateRepositoryCustomProperty":    customPropertyRule{propertyKey: "repositoryCustomPropertyId"},
		"deleteRepositoryCustomProperty":    customPropertyRule{propertyKey: "id"},
		"promoteRepositoryCustomProperty":   customPropertyRule{propertyKey: "repositoryCustomPropertyId", promote: true},
		"setRepositoryCustomPropertyValues": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// resolvedCustomProperty is a definition located by node id, with its account
// context. org is nil for a definition addressed through the enterprise slug.
type resolvedCustomProperty struct {
	org             *store.Org
	enterprise      *store.Enterprise
	name            string
	definition      *store.CustomProperty
	enterpriseOwned bool
}

// customPropertyByNodeID resolves a "RCP_<login>/<name>" definition id. The
// login half is the organization (or enterprise slug) the definition was listed
// under. A name the org does not define itself falls back to the enterprise-level
// definition of the same name, which is how the org connection lists both.
func (s *Resolver) customPropertyByNodeID(nodeID string) (resolvedCustomProperty, error) {
	missing := gqlMissingNode("RepositoryCustomProperty", nodeID)
	rest, ok := strings.CutPrefix(nodeID, "RCP_")
	if !ok {
		return resolvedCustomProperty{}, missing
	}
	login, name, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		return resolvedCustomProperty{}, missing
	}
	org := s.store.GetOrg(login)
	if org == nil {
		if e := s.store.GetEnterprise(login); e != nil {
			if definition := s.store.GetEnterpriseCustomProperty(name); definition != nil {
				return resolvedCustomProperty{enterprise: e, name: name, definition: definition, enterpriseOwned: true}, nil
			}
		}
		return resolvedCustomProperty{}, missing
	}
	definition := s.store.GetCustomProperty(org.Login, name)
	if definition == nil {
		return resolvedCustomProperty{}, missing
	}
	return resolvedCustomProperty{
		org:             org,
		name:            name,
		definition:      definition,
		enterpriseOwned: !s.store.OrgOwnsCustomProperty(org.Login, name),
	}, nil
}

// renderResolvedCustomProperty renders a resolved definition through the account
// it was addressed by.
func (s *Resolver) renderResolvedCustomProperty(resolved resolvedCustomProperty, definition *store.CustomProperty) map[string]interface{} {
	if resolved.org != nil {
		return s.repositoryCustomPropertySource(resolved.org, definition)
	}
	return s.enterpriseCustomPropertySource(resolved.enterprise, definition)
}

// customPropertyRule is the policy for the definition mutations. An org's
// definitions are its owners' to write; the enterprise schema — and promotion
// into it — belongs to the enterprise owner (a site admin here).
type customPropertyRule struct {
	// sourceKey names the owning account directly; propertyKey names an existing
	// definition whose owner is looked up. Exactly one is set.
	sourceKey   string
	propertyKey string
	// promote demands enterprise-owner standing regardless of current owner,
	// because the write lands in the enterprise schema.
	promote bool
}

func (r customPropertyRule) check() error {
	if (r.sourceKey == "") == (r.propertyKey == "") {
		return fmt.Errorf("exactly one of sourceKey or propertyKey must be set")
	}
	return nil
}

func (r customPropertyRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	if r.sourceKey != "" {
		nodeID, _ := input[r.sourceKey].(string)
		if org := s.orgByNodeID(nodeID); org != nil {
			return s.authorizeOrgAdministration(p, org.Login, store.ScopeOrgAdministration)
		}
		if e := store.FindEnterpriseByNodeID(s.store, nodeID); e != nil {
			if !s.store.IsEnterpriseOwner(e.ID, s.ghUserFromContext(p.Context)) {
				return enterpriseOwnerRequired()
			}
			return nil
		}
		return gqlMissingNode("CustomPropertySource", nodeID)
	}
	nodeID, _ := input[r.propertyKey].(string)
	resolved, err := s.customPropertyByNodeID(nodeID)
	if err != nil {
		return err
	}
	if r.promote || resolved.enterpriseOwned {
		return s.authorizeEnterpriseSchemaOwner(p)
	}
	return s.authorizeOrgAdministration(p, resolved.org.Login, store.ScopeOrgAdministration)
}

// authorizeEnterpriseSchemaOwner demands enterprise-account ownership, which —
// as on GitHub Enterprise Server — a site administrator holds.
func (s *Resolver) authorizeEnterpriseSchemaOwner(p graphql.ResolveParams) error {
	viewer := s.ghUserFromContext(p.Context)
	if e := s.store.GetEnterprise(s.store.PrimaryEnterpriseSlug()); e != nil {
		if s.store.IsEnterpriseOwner(e.ID, viewer) {
			return nil
		}
	} else if viewer.SiteAdmin {
		return nil
	}
	return enterpriseOwnerRequired()
}

// schema

var graphQLCustomPropertyValueTypes = map[string]bool{
	"string": true, "single_select": true, "multi_select": true, "true_false": true, "url": true,
}

// addCustomPropertyMutationsToSchema installs the five custom-property mutations
// and completes RepositoryCustomProperty with the `source` union the read surface
// could not mint before the Enterprise type existed.
func (s *Resolver) addCustomPropertyMutationsToSchema(mutationType *graphql.Object) {
	propertyType := s.gqlRepositoryCustomPropertyType()
	propertyType.AddFieldConfig("source", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlCustomPropertySourceUnion()),
	})

	valueTypeEnum := s.sharedEnum("CustomPropertyValueType", "MULTI_SELECT", "SINGLE_SELECT", "STRING", "TRUE_FALSE", "URL")
	editableByEnum := s.sharedEnum("RepositoryCustomPropertyValuesEditableBy", "ORG_ACTORS", "ORG_AND_REPO_ACTORS")

	s.registerMutation(mutationType, "createRepositoryCustomProperty", &graphql.Field{
		Type: s.mutationPayload("CreateRepositoryCustomPropertyPayload", graphql.Fields{
			"repositoryCustomProperty": gqlField(propertyType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateRepositoryCustomPropertyInput", graphql.InputObjectConfigFieldMap{
				"allowedValues":         gqlListOf(graphql.String),
				"defaultValue":          gqlString(),
				"description":           gqlString(),
				"propertyName":          gqlNonNullString(),
				"regex":                 gqlString(),
				"requireExplicitValues": gqlBool(),
				"required":              gqlBool(),
				"sourceId":              gqlNonNullID(),
				"valueType":             gqlNonNullInputOf(valueTypeEnum),
				"valuesEditableBy":      gqlInputOf(editableByEnum),
			})),
		}},
		Resolve: s.resolveCreateRepositoryCustomProperty,
	})

	s.registerMutation(mutationType, "updateRepositoryCustomProperty", &graphql.Field{
		Type: s.mutationPayload("UpdateRepositoryCustomPropertyPayload", graphql.Fields{
			"repositoryCustomProperty": gqlField(propertyType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateRepositoryCustomPropertyInput", graphql.InputObjectConfigFieldMap{
				"allowedValues":              gqlListOf(graphql.String),
				"defaultValue":               gqlString(),
				"description":                gqlString(),
				"regex":                      gqlString(),
				"repositoryCustomPropertyId": gqlNonNullID(),
				"requireExplicitValues":      gqlBool(),
				"required":                   gqlBool(),
				"valuesEditableBy":           gqlInputOf(editableByEnum),
			})),
		}},
		Resolve: s.resolveUpdateRepositoryCustomProperty,
	})

	s.registerMutation(mutationType, "deleteRepositoryCustomProperty", &graphql.Field{
		Type: s.mutationPayload("DeleteRepositoryCustomPropertyPayload", graphql.Fields{
			"repositoryCustomProperty": gqlField(propertyType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteRepositoryCustomPropertyInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteRepositoryCustomProperty,
	})

	s.registerMutation(mutationType, "promoteRepositoryCustomProperty", &graphql.Field{
		Type: s.mutationPayload("PromoteRepositoryCustomPropertyPayload", graphql.Fields{
			"repositoryCustomProperty": gqlField(propertyType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("PromoteRepositoryCustomPropertyInput", graphql.InputObjectConfigFieldMap{
				"repositoryCustomPropertyId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolvePromoteRepositoryCustomProperty,
	})

	s.registerMutation(mutationType, "setRepositoryCustomPropertyValues", &graphql.Field{
		Type: s.mutationPayload("SetRepositoryCustomPropertyValuesPayload", graphql.Fields{
			"repository": gqlField(s.graphqlTypes.repository),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("SetRepositoryCustomPropertyValuesInput", graphql.InputObjectConfigFieldMap{
				"properties": gqlNonNullListOf(s.mutationInput("CustomPropertyValueInput", graphql.InputObjectConfigFieldMap{
					"propertyName": gqlNonNullString(),
					"value":        gqlInputOf(s.graphQLStringScalar("CustomPropertyValue")),
				})),
				"repositoryId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveSetRepositoryCustomPropertyValues,
	})
}

// gqlCustomPropertySourceUnion is the Enterprise | Organization union a
// definition's `source` names.
func (s *Resolver) gqlCustomPropertySourceUnion() *graphql.Union {
	return s.mutationUnion("CustomPropertySource",
		func() []*graphql.Object {
			return []*graphql.Object{s.graphqlTypes.enterprise, s.graphqlTypes.organization}
		},
		func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if _, isEnterprise := source["slug"]; isEnterprise {
				return s.graphqlTypes.enterprise
			}
			return s.graphqlTypes.organization
		})
}

// input assembly

// validGraphQLCustomPropertyName mirrors the REST name rule: no whitespace, no
// control characters (the name is a URL path segment on the REST schema routes).
func validGraphQLCustomPropertyName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validateCustomPropertyDefinition applies the same shape rules REST enforces.
func validateCustomPropertyDefinition(def *store.CustomProperty) error {
	if !validGraphQLCustomPropertyName(def.PropertyName) {
		return fmt.Errorf("propertyName is invalid")
	}
	if !graphQLCustomPropertyValueTypes[def.ValueType] {
		return fmt.Errorf("valueType is invalid")
	}
	isSelect := def.ValueType == "single_select" || def.ValueType == "multi_select"
	if !isSelect && len(def.AllowedValues) > 0 {
		return fmt.Errorf("allowedValues are only accepted for the select value types")
	}
	if isSelect && len(def.AllowedValues) > 200 {
		return fmt.Errorf("allowedValues may carry at most 200 entries")
	}
	if def.Required && def.DefaultValue == nil {
		return fmt.Errorf("a required property must carry a defaultValue")
	}
	if def.DefaultValue != nil {
		if err := store.ValidateCustomPropertyValue(def, def.DefaultValue); err != nil {
			return fmt.Errorf("defaultValue is invalid: %w", err)
		}
	}
	return nil
}

// applyCustomPropertyDefinitionInput merges the present update members into the
// definition.
func applyCustomPropertyDefinitionInput(def *store.CustomProperty, input map[string]interface{}) {
	if value, ok := gqlInputString(input, "description"); ok {
		def.Description = &value
	}
	if value, ok := gqlInputString(input, "defaultValue"); ok {
		def.DefaultValue = value
	}
	if values, ok := gqlInputStrings(input, "allowedValues"); ok {
		def.AllowedValues = values
	}
	if value, ok := gqlInputString(input, "regex"); ok {
		def.Regex = &value
	}
	if value, ok := gqlInputBool(input, "required"); ok {
		def.Required = value
	}
	if value, ok := gqlInputBool(input, "requireExplicitValues"); ok {
		def.RequireExplicitValues = value
	}
	if value, ok := gqlInputString(input, "valuesEditableBy"); ok {
		def.ValuesEditableBy = strings.ToLower(value)
	}
}

// resolvers

func (s *Resolver) resolveCreateRepositoryCustomProperty(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	name, _ := gqlInputString(input, "propertyName")
	valueType, _ := gqlInputString(input, "valueType")

	def := &store.CustomProperty{
		PropertyName:     name,
		ValueType:        strings.ToLower(valueType),
		ValuesEditableBy: "org_actors",
	}
	applyCustomPropertyDefinitionInput(def, input)
	if err := validateCustomPropertyDefinition(def); err != nil {
		return nil, err
	}

	sourceID, _ := gqlInputString(input, "sourceId")
	if org := s.orgByNodeID(sourceID); org != nil {
		if s.store.GetCustomProperty(org.Login, name) != nil {
			//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
			return nil, fmt.Errorf("Property name has already been taken")
		}
		s.store.UpsertCustomProperty(org.Login, def)
		return map[string]interface{}{
			"repositoryCustomProperty": s.repositoryCustomPropertySource(org, def),
		}, nil
	}
	if e := store.FindEnterpriseByNodeID(s.store, sourceID); e != nil {
		if s.store.GetEnterpriseCustomProperty(name) != nil {
			//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
			return nil, fmt.Errorf("Property name has already been taken")
		}
		s.store.UpsertEnterpriseCustomProperty(def)
		return map[string]interface{}{
			"repositoryCustomProperty": s.enterpriseCustomPropertySource(e, def),
		}, nil
	}
	return nil, gqlMissingNode("CustomPropertySource", sourceID)
}

func (s *Resolver) resolveUpdateRepositoryCustomProperty(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "repositoryCustomPropertyId")
	resolved, err := s.customPropertyByNodeID(nodeID)
	if err != nil {
		return nil, err
	}
	def := resolved.definition
	applyCustomPropertyDefinitionInput(def, input)
	if err := validateCustomPropertyDefinition(def); err != nil {
		return nil, err
	}
	if resolved.enterpriseOwned {
		s.store.UpsertEnterpriseCustomProperty(def)
	} else {
		s.store.UpsertCustomProperty(resolved.org.Login, def)
	}
	return map[string]interface{}{
		"repositoryCustomProperty": s.renderResolvedCustomProperty(resolved, def),
	}, nil
}

func (s *Resolver) resolveDeleteRepositoryCustomProperty(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "id")
	resolved, err := s.customPropertyByNodeID(nodeID)
	if err != nil {
		return nil, err
	}
	// Render the definition before destroying the row; the payload reports it as it was.
	deleted := s.renderResolvedCustomProperty(resolved, resolved.definition)
	if resolved.enterpriseOwned {
		if !s.store.DeleteEnterpriseCustomProperty(resolved.name) {
			return nil, gqlMissingNode("RepositoryCustomProperty", nodeID)
		}
	} else if !s.store.DeleteCustomProperty(resolved.org.Login, resolved.name) {
		return nil, gqlMissingNode("RepositoryCustomProperty", nodeID)
	}
	return map[string]interface{}{"repositoryCustomProperty": deleted}, nil
}

func (s *Resolver) resolvePromoteRepositoryCustomProperty(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "repositoryCustomPropertyId")
	resolved, err := s.customPropertyByNodeID(nodeID)
	if err != nil {
		return nil, err
	}
	if resolved.enterpriseOwned {
		return nil, fmt.Errorf("the property already belongs to the enterprise")
	}
	promoted := s.store.PromoteCustomProperty(resolved.org.Login, resolved.name)
	if promoted == nil {
		return nil, gqlMissingNode("RepositoryCustomProperty", nodeID)
	}
	// The promoted definition now renders with the enterprise as its source.
	rendered := s.repositoryCustomPropertySource(resolved.org, promoted)
	if e := s.store.GetEnterprise(s.store.PrimaryEnterpriseSlug()); e != nil {
		rendered = s.enterpriseCustomPropertySource(e, promoted)
	}
	return map[string]interface{}{"repositoryCustomProperty": rendered}, nil
}

func (s *Resolver) resolveSetRepositoryCustomPropertyValues(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	owner, _, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return nil, gqlMissingNodeType("Repository")
	}
	items := gqlInputObjects(input, "properties")
	values := make([]store.CustomPropertyValuePayload, 0, len(items))
	for _, item := range items {
		name, _ := item["propertyName"].(string)
		definition := s.store.GetCustomProperty(owner, name)
		if definition == nil {
			return nil, fmt.Errorf("no custom property named %q is defined for %s", name, owner)
		}
		value := item["value"]
		if value != nil {
			if err := store.ValidateCustomPropertyValue(definition, value); err != nil {
				return nil, err
			}
		}
		values = append(values, store.CustomPropertyValuePayload{PropertyName: name, Value: value})
	}
	s.store.SetRepoCustomPropertyValues(repo.FullName, values)
	return map[string]interface{}{
		"repository": optionalObject(repoToGraphQL(s.store, repo)),
	}, nil
}
