package graphqlapi

// The final tail of GitHub GraphQL fields whose subject bleephub does not model
// at all. Each field's type is a faithful, signature-exact transcription of the
// vendored SDL so the field exists and validates, but there is no stored record
// behind it, so it resolves a truthful null (nullable fields) or an empty
// connection/list (non-null connections and nullable lists). Nothing here
// invents data.
//
// addResidueTailFields is wired once from initGraphQLSchema, after every family
// it draws on is assembled: the ruleset read surface (RepositoryRuleConditions,
// RepositoryRulesetBypassActor, RepositoryRuleset), the enterprise family
// (EnterpriseTeam), the branch-protection family (App), the account surface
// (Organization, Repository, the collaborators connection) and the discussion
// family (Discussion). The condition/bypass/collaborator objects are reached by
// GitHub name through the stash the ruleset and account families record.

import (
	"github.com/graphql-go/graphql"
)

// addResidueTailFields adds every residual field in this file. It is the single
// hook initGraphQLSchema calls; the helpers below own the type construction.
func (s *Resolver) addResidueTailFields() {
	s.addRuleConditionsResidueFields()
	s.addRulesetBypassActorResidueFields()
	s.addCollaboratorPermissionSourcesField()
	s.addRepositoryPinnedDiscussionsField()
}

// addRuleConditionsResidueFields completes RepositoryRuleConditions with the
// organization/repository property and id/name condition targets GitHub
// declares beside refName. bleephub stores only ref_name conditions, so each of
// these resolves null (no such condition is ever recorded); their container
// types carry the exact non-null inner list fields the SDL declares, which are
// never reached because the parent field resolves null.
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

// addRulesetBypassActorResidueFields completes RepositoryRulesetBypassActor with
// the `actor` (the App/EnterpriseTeam/Team/User that may bypass) and the
// `repositoryRuleset` back-reference. bleephub records the boolean discriminator
// flags for a bypass actor but not the concrete actor object, so `actor`
// resolves null; the source map carries no ruleset back-reference, so
// `repositoryRuleset` resolves null too. Both are nullable in the SDL.
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
		// The field always resolves null, so a concrete member is never chosen.
		return nil
	})
	actorObject.AddFieldConfig("actor", &graphql.Field{Type: bypassActor, Resolve: nilResolver})

	if ruleset := s.accountSurfaceRegistry().ruleset; ruleset != nil {
		actorObject.AddFieldConfig("repositoryRuleset", &graphql.Field{Type: ruleset, Resolve: nilResolver})
	}
}

// addCollaboratorPermissionSourcesField adds RepositoryCollaboratorEdge
// .permissionSources: the per-source breakdown of how a collaborator's access
// was granted. bleephub resolves a collaborator's effective permission but not
// the source breakdown, so the list resolves empty; it is nullable, so an empty
// list is truthful. PermissionSource, its DefaultRepositoryPermissionField enum
// and its PermissionGranter union are built here to GitHub's SDL.
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

	// DefaultRepositoryPermissionField is already minted by the enterprise
	// family; sharedEnum returns that one instance.
	permission := s.sharedEnum("DefaultRepositoryPermissionField", "ADMIN", "NONE", "READ", "WRITE")
	granter := s.mutationUnion("PermissionGranter", func() []*graphql.Object {
		return []*graphql.Object{
			s.namedObject("EnterpriseTeam"),
			s.graphqlTypes.organization,
			s.graphqlTypes.repository,
			s.graphqlTypes.team,
		}
	}, func(graphql.ResolveTypeParams) *graphql.Object {
		// The list resolves empty, so a concrete member is never chosen.
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

// addRepositoryPinnedDiscussionsField adds Repository.pinnedDiscussions. Pinning
// a discussion carries display metadata (a gradient, a background pattern, the
// pinning actor) bleephub has no model for, so the connection resolves empty.
// The field is a non-null connection, so it returns a concrete empty connection
// rather than null; the PinnedDiscussion node and its two enums are built to
// GitHub's SDL for the connection's element type.
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
