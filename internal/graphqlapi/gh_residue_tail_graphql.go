package graphqlapi

// The final tail of GraphQL fields bleephub does not model. Each field's type
// transcribes the vendored SDL exactly so it validates, but nothing backs it,
// so it resolves a truthful null or empty connection/list. Runs after every
// family it draws on is assembled.

import (
	"github.com/graphql-go/graphql"
)

// addResidueTailFields adds every residual field in this file.
func (s *Resolver) addResidueTailFields() {
	s.addRuleConditionsResidueFields()
	s.addRulesetBypassActorResidueFields()
	s.addCollaboratorPermissionSourcesField()
	s.addRepositoryPinnedDiscussionsField()
}

// addRuleConditionsResidueFields completes RepositoryRuleConditions with the
// property and id/name condition targets beside refName. Only ref_name
// conditions are stored, so each resolves null.
func (s *Resolver) addRuleConditionsResidueFields() {
	conditions := s.namedObject("RepositoryRuleConditions")
	if conditions == nil {
		return
	}

	orgPropertyDefinition := graphql.NewObject(graphql.ObjectConfig{
		Name: "OrganizationPropertyTargetDefinition",
		Fields: graphql.Fields{
			"name":           gqlNonNull(graphql.String),
			"propertyValues": gqlNonNullFieldListOf(graphql.String),
		},
	})
	orgProperty := graphql.NewObject(graphql.ObjectConfig{
		Name: "OrganizationPropertyConditionTarget",
		Fields: graphql.Fields{
			"exclude": gqlNonNullFieldListOf(orgPropertyDefinition),
			"include": gqlNonNullFieldListOf(orgPropertyDefinition),
		},
	})
	repositoryID := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryIdConditionTarget",
		Fields: graphql.Fields{
			"repositoryIds": gqlNonNullFieldListOf(graphql.ID),
		},
	})
	repositoryName := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryNameConditionTarget",
		Fields: graphql.Fields{
			"exclude":   gqlNonNullFieldListOf(graphql.String),
			"include":   gqlNonNullFieldListOf(graphql.String),
			"protected": gqlNonNull(graphql.Boolean),
		},
	})
	propertyDefinition := graphql.NewObject(graphql.ObjectConfig{
		Name: "PropertyTargetDefinition",
		Fields: graphql.Fields{
			"name":           gqlNonNull(graphql.String),
			"propertyValues": gqlNonNullFieldListOf(graphql.String),
			"source":         gqlField(graphql.String),
		},
	})
	repositoryProperty := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryPropertyConditionTarget",
		Fields: graphql.Fields{
			"exclude": gqlNonNullFieldListOf(propertyDefinition),
			"include": gqlNonNullFieldListOf(propertyDefinition),
		},
	})

	conditions.AddFieldConfig("organizationProperty", &graphql.Field{Type: orgProperty, Resolve: nilResolver})
	conditions.AddFieldConfig("repositoryId", &graphql.Field{Type: repositoryID, Resolve: nilResolver})
	conditions.AddFieldConfig("repositoryName", &graphql.Field{Type: repositoryName, Resolve: nilResolver})
	conditions.AddFieldConfig("repositoryProperty", &graphql.Field{Type: repositoryProperty, Resolve: nilResolver})
}

// addRulesetBypassActorResidueFields completes RepositoryRulesetBypassActor
// with `actor` and the `repositoryRuleset` back-reference. Neither concrete
// object is stored, so both resolve null.
func (s *Resolver) addRulesetBypassActorResidueFields() {
	actorObject := s.namedObject("RepositoryRulesetBypassActor")
	if actorObject == nil {
		return
	}

	bypassActor := s.mutationUnion("BypassActor", func() []*graphql.Object {
		return []*graphql.Object{
			s.namedObject("App"),
			s.namedObject("EnterpriseTeam"),
			s.graphqlTypes.team,
			s.graphqlTypes.user,
		}
	}, func(graphql.ResolveTypeParams) *graphql.Object {
		// Always resolves null, so no member is chosen.
		return nil
	})
	actorObject.AddFieldConfig("actor", &graphql.Field{Type: bypassActor, Resolve: nilResolver})

	if ruleset := s.accountSurfaceRegistry().ruleset; ruleset != nil {
		actorObject.AddFieldConfig("repositoryRuleset", &graphql.Field{Type: ruleset, Resolve: nilResolver})
	}
}

// addCollaboratorPermissionSourcesField adds
// RepositoryCollaboratorEdge.permissionSources. Only effective permission is
// resolved, not the per-source breakdown, so the (nullable) list is empty.
func (s *Resolver) addCollaboratorPermissionSourcesField() {
	connection := s.accountSurfaceRegistry().connections["RepositoryCollaborator"]
	if connection == nil {
		return
	}
	edgesDef := connection.Fields()["edges"]
	if edgesDef == nil {
		return
	}
	edge, ok := graphql.GetNamed(edgesDef.Type).(*graphql.Object)
	if !ok {
		return
	}

	permission := s.sharedEnum("DefaultRepositoryPermissionField", "ADMIN", "NONE", "READ", "WRITE")
	granter := s.mutationUnion("PermissionGranter", func() []*graphql.Object {
		return []*graphql.Object{
			s.namedObject("EnterpriseTeam"),
			s.graphqlTypes.organization,
			s.graphqlTypes.repository,
			s.graphqlTypes.team,
		}
	}, func(graphql.ResolveTypeParams) *graphql.Object {
		// The list resolves empty, so no member is chosen.
		return nil
	})
	permissionSource := s.mutationObject("PermissionSource", graphql.Fields{
		"organization": gqlNonNull(s.graphqlTypes.organization),
		"permission":   gqlNonNull(permission),
		"roleName":     gqlField(graphql.String),
		"source":       gqlNonNull(granter),
	})

	edge.AddFieldConfig("permissionSources", &graphql.Field{
		Type: graphql.NewList(graphql.NewNonNull(permissionSource)),
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			return []interface{}{}, nil
		},
	})
}

// addRepositoryPinnedDiscussionsField adds Repository.pinnedDiscussions. Pin
// display metadata (gradient, pattern, actor) is unmodeled, so the (non-null)
// connection resolves empty.
func (s *Resolver) addRepositoryPinnedDiscussionsField() {
	repository := s.graphqlTypes.repository
	if repository == nil {
		return
	}
	dateTime := s.graphQLStringScalar("DateTime")
	pattern := s.graphQLEnum("PinnedDiscussionPattern",
		"CHEVRON_UP", "DOT", "DOT_FILL", "HEART_FILL", "PLUS", "ZAP")
	gradient := s.graphQLEnum("PinnedDiscussionGradient",
		"BLUE_MINT", "BLUE_PURPLE", "PINK_BLUE", "PURPLE_CORAL", "RED_ORANGE")

	pinnedDiscussion := s.mutationObject("PinnedDiscussion", graphql.Fields{
		"createdAt":             gqlNonNull(dateTime),
		"databaseId":            gqlField(graphql.Int),
		"discussion":            gqlNonNull(s.graphqlTypes.discussion),
		"gradientStopColors":    gqlNonNullFieldListOf(graphql.String),
		"id":                    gqlNonNull(graphql.ID),
		"pattern":               gqlNonNull(pattern),
		"pinnedBy":              gqlNonNull(s.graphqlTypes.actor),
		"preconfiguredGradient": gqlField(gradient),
		"repository":            gqlNonNull(repository),
		"updatedAt":             gqlNonNull(dateTime),
	})
	connection := s.gqlConnectionType("PinnedDiscussion", pinnedDiscussion)

	repository.AddFieldConfig("pinnedDiscussions", &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return paginateGQLItems(nil, p.Args), nil
		},
	})
}
