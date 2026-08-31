package graphqlapi

// Activity and account-policy mutations: stars, the watch subscription, the
// three interaction limits, outside-collaborator removal and two organization
// settings toggles. Each writes through the same store primitive its REST route
// uses, so the two surfaces stay consistent.

import (
	"fmt"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Resolver) addActivityMutations(mutationType *graphql.Object) {
	userType := s.graphqlTypes.user
	organizationType := s.graphqlTypes.organization
	repositoryType := s.graphqlTypes.repository
	limitEnum := s.repositoryInteractionLimitEnum()
	expiryEnum := s.sharedEnum("RepositoryInteractionLimitExpiry",
		"ONE_DAY", "ONE_MONTH", "ONE_WEEK", "SIX_MONTHS", "THREE_DAYS")

	// starring

	s.registerMutation(mutationType, "addStar", &graphql.Field{
		Type: s.mutationPayload("AddStarPayload", graphql.Fields{
			"starrable": gqlField(s.starrableInterface()),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("AddStarInput", graphql.InputObjectConfigFieldMap{
				"starrableId": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveStar(p, true)
		},
	})

	s.registerMutation(mutationType, "removeStar", &graphql.Field{
		Type: s.mutationPayload("RemoveStarPayload", graphql.Fields{
			"starrable": gqlField(s.starrableInterface()),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("RemoveStarInput", graphql.InputObjectConfigFieldMap{
				"starrableId": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveStar(p, false)
		},
	})

	// watching

	s.registerMutation(mutationType, "updateSubscription", &graphql.Field{
		Type: s.mutationPayload("UpdateSubscriptionPayload", graphql.Fields{
			"subscribable": gqlField(s.subscribableInterface()),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateSubscriptionInput", graphql.InputObjectConfigFieldMap{
				"state":          gqlNonNullInputOf(s.sharedEnum("SubscriptionState", "IGNORED", "SUBSCRIBED", "UNSUBSCRIBED")),
				"subscribableId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveUpdateSubscription,
	})

	// interaction limits

	s.registerMutation(mutationType, "setRepositoryInteractionLimit", &graphql.Field{
		Type: s.mutationPayload("SetRepositoryInteractionLimitPayload", graphql.Fields{
			"repository": gqlField(repositoryType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("SetRepositoryInteractionLimitInput", graphql.InputObjectConfigFieldMap{
				"expiry":       gqlInputOf(expiryEnum),
				"limit":        gqlNonNullInputOf(limitEnum),
				"repositoryId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveSetRepositoryInteractionLimit,
	})

	s.registerMutation(mutationType, "setOrganizationInteractionLimit", &graphql.Field{
		Type: s.mutationPayload("SetOrganizationInteractionLimitPayload", graphql.Fields{
			"organization": gqlField(organizationType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("SetOrganizationInteractionLimitInput", graphql.InputObjectConfigFieldMap{
				"expiry":         gqlInputOf(expiryEnum),
				"limit":          gqlNonNullInputOf(limitEnum),
				"organizationId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveSetOrganizationInteractionLimit,
	})

	s.registerMutation(mutationType, "setUserInteractionLimit", &graphql.Field{
		Type: s.mutationPayload("SetUserInteractionLimitPayload", graphql.Fields{
			"user": gqlField(userType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("SetUserInteractionLimitInput", graphql.InputObjectConfigFieldMap{
				"expiry": gqlInputOf(expiryEnum),
				"limit":  gqlNonNullInputOf(limitEnum),
				"userId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveSetUserInteractionLimit,
	})

	// organization membership and settings

	s.registerMutation(mutationType, "removeOutsideCollaborator", &graphql.Field{
		Type: s.mutationPayload("RemoveOutsideCollaboratorPayload", graphql.Fields{
			"removedUser": gqlField(userType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("RemoveOutsideCollaboratorInput", graphql.InputObjectConfigFieldMap{
				"organizationId": gqlNonNullID(),
				"userId":         gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveRemoveOutsideCollaborator,
	})

	s.registerMutation(mutationType, "updateOrganizationAllowPrivateRepositoryForkingSetting", &graphql.Field{
		Type: s.mutationPayload("UpdateOrganizationAllowPrivateRepositoryForkingSettingPayload", graphql.Fields{
			"message":      gqlField(graphql.String),
			"organization": gqlField(organizationType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateOrganizationAllowPrivateRepositoryForkingSettingInput", graphql.InputObjectConfigFieldMap{
				"forkingEnabled": gqlNonNullBool(),
				"organizationId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveUpdateOrganizationForkingSetting,
	})

	s.registerMutation(mutationType, "updateOrganizationWebCommitSignoffSetting", &graphql.Field{
		Type: s.mutationPayload("UpdateOrganizationWebCommitSignoffSettingPayload", graphql.Fields{
			"message":      gqlField(graphql.String),
			"organization": gqlField(organizationType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateOrganizationWebCommitSignoffSettingInput", graphql.InputObjectConfigFieldMap{
				"organizationId":           gqlNonNullID(),
				"webCommitSignoffRequired": gqlNonNullBool(),
			})),
		}},
		Resolve: s.resolveUpdateOrganizationWebCommitSignoff,
	})
}

// repositoryInteractionLimitEnum is shared by interactionAbility and the three
// set-limit mutations.
func (s *Resolver) repositoryInteractionLimitEnum() *graphql.Enum {
	return s.sharedEnum("RepositoryInteractionLimit",
		"COLLABORATORS_ONLY", "CONTRIBUTORS_ONLY", "EXISTING_USERS", "NO_LIMIT")
}

// Starrable and Subscribable

// starrableInterface is the payload interface for the star mutations.
// Repository is the only implementation bleephub's star store records
// stargazers for.
func (s *Resolver) starrableInterface() *graphql.Interface {
	return s.mutationInterface("Starrable", func() graphql.Fields {
		return graphql.Fields{
			"id":             gqlNonNull(graphql.ID),
			"stargazerCount": gqlNonNull(graphql.Int),
			"stargazers": &graphql.Field{
				Type: graphql.NewNonNull(s.gqlStargazerConnectionType()),
				Args: s.stargazerConnectionArgs(),
			},
			"viewerHasStarred": gqlNonNull(graphql.Boolean),
		}
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		return s.graphqlTypes.repository
	})
}

// subscribableInterface is updateSubscription's payload interface; Repository
// is the only subject bleephub records watch subscriptions for.
func (s *Resolver) subscribableInterface() *graphql.Interface {
	return s.mutationInterface("Subscribable", func() graphql.Fields {
		return graphql.Fields{
			"id":                 gqlNonNull(graphql.ID),
			"viewerCanSubscribe": gqlNonNull(graphql.Boolean),
			"viewerSubscription": gqlField(s.sharedEnum("SubscriptionState", "IGNORED", "SUBSCRIBED", "UNSUBSCRIBED")),
		}
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		return s.graphqlTypes.repository
	})
}

// stargazerConnectionArgs are the four pagination arguments plus a StarOrder.
func (s *Resolver) stargazerConnectionArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"after":  &graphql.ArgumentConfig{Type: graphql.String},
		"before": &graphql.ArgumentConfig{Type: graphql.String},
		"first":  &graphql.ArgumentConfig{Type: graphql.Int},
		"last":   &graphql.ArgumentConfig{Type: graphql.Int},
		"orderBy": &graphql.ArgumentConfig{Type: s.mutationInput("StarOrder", graphql.InputObjectConfigFieldMap{
			"direction": gqlNonNullInputOf(s.sharedEnum("OrderDirection", "ASC", "DESC")),
			"field":     gqlNonNullInputOf(s.sharedEnum("StarOrderField", "STARRED_AT")),
		})},
	}
}

func (s *Resolver) gqlStargazerConnectionType() *graphql.Object {
	return s.mutationObject("StargazerConnection", graphql.Fields{
		"edges":      gqlField(graphql.NewList(s.gqlStargazerEdgeType())),
		"nodes":      gqlField(graphql.NewList(s.graphqlTypes.user)),
		"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
		"totalCount": gqlNonNull(graphql.Int),
	})
}

func (s *Resolver) gqlStargazerEdgeType() *graphql.Object {
	return s.mutationObject("StargazerEdge", graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   gqlNonNull(s.graphqlTypes.user),
		"starredAt": &graphql.Field{
			// Edges are minted as {node, cursor}, so starredAt rides on
			// the node.
			Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime")),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				edge, ok := p.Source.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
				}
				node, ok := edge["node"].(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("stargazer edge has no node")
				}
				return node["starredAt"], nil
			},
		},
	})
}

// stargazerConnectionSource renders a repository's stargazers as a connection,
// each node carrying the instant that user starred the repo.
func (s *Resolver) stargazerConnectionSource(repo *store.Repo) map[string]interface{} {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return gqlUnpagedSource(nil)
	}
	starredAt := s.store.RepoStargazersAt(owner, name)
	ids := s.store.ListRepoStargazers(owner, name)
	nodes := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		user := s.store.GetUserByID(id)
		if user == nil {
			continue
		}
		node := userToGraphQL(user)
		at := starredAt[id]
		if at.IsZero() {
			// starredAt is DateTime!; a star with no recorded timestamp
			// falls back to the repo's creation instant.
			at = repo.CreatedAt
		}
		node["starredAt"] = at.UTC().Format(time.RFC3339)
		nodes = append(nodes, node)
	}
	return gqlUnpagedSource(nodes)
}

// star resolvers

func (s *Resolver) resolveStar(p graphql.ResolveParams, star bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "starrableId")
	if err != nil {
		return nil, err
	}
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return nil, gqlMissingNodeType("Repository")
	}
	// Both mutations are idempotent on github.com. The store primitives report
	// "nothing to do" and "no such repository" with the same false, so read
	// current state first and write only a real change.
	viewer := s.ghUserFromContext(p.Context)
	starred := s.store.IsRepoStarredBy(viewer.ID, owner, name)
	changed := false
	switch {
	case star && !starred:
		if !s.store.StarRepo(viewer.ID, owner, name) {
			return nil, gqlMissingNodeType("Repository")
		}
		changed = true
	case !star && starred:
		if !s.store.UnstarRepo(viewer.ID, owner, name) {
			return nil, gqlMissingNodeType("Repository")
		}
		changed = true
	}
	updated := s.store.GetRepoByID(repo.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Repository")
	}
	if star && changed {
		// Starring fires the `watch` event (action "started"); the event name
		// is historical.
		s.emitWebhookEvent(updated.FullName, "watch", "started", map[string]interface{}{
			"action":     "started",
			"repository": s.repoPayload(updated),
			"sender":     s.senderPayload(viewer),
		})
	}
	return map[string]interface{}{"starrable": optionalObject(repoToGraphQL(s.store, updated))}, nil
}

// subscription resolver

func (s *Resolver) resolveUpdateSubscription(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "subscribableId")
	if err != nil {
		return nil, err
	}
	viewer := s.ghUserFromContext(p.Context)
	state, _ := gqlInputString(input, "state")
	switch state {
	case "SUBSCRIBED":
		s.store.SetRepoSubscription(viewer.ID, repo.ID, true, false)
	case "IGNORED":
		s.store.SetRepoSubscription(viewer.ID, repo.ID, false, true)
	case "UNSUBSCRIBED":
		s.store.DeleteRepoSubscription(viewer.ID, repo.ID)
	default:
		return nil, fmt.Errorf("subscription state %q is not one of IGNORED, SUBSCRIBED, UNSUBSCRIBED", state)
	}
	updated := s.store.GetRepoByID(repo.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Repository")
	}
	return map[string]interface{}{"subscribable": optionalObject(repoToGraphQL(s.store, updated))}, nil
}

// interaction-limit resolvers

func (s *Resolver) resolveSetRepositoryInteractionLimit(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	limit, expiry, err := s.interactionLimitFromInput(input)
	if err != nil {
		return nil, err
	}
	if !s.store.SetRepoInteractionLimit(repo.ID, limit, expiry) {
		return nil, gqlMissingNodeType("Repository")
	}
	updated := s.store.GetRepoByID(repo.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Repository")
	}
	return map[string]interface{}{"repository": optionalObject(repoToGraphQL(s.store, updated))}, nil
}

func (s *Resolver) resolveSetOrganizationInteractionLimit(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	org, err := s.mutationOrgFromInput(input, "organizationId")
	if err != nil {
		return nil, err
	}
	limit, expiry, err := s.interactionLimitFromInput(input)
	if err != nil {
		return nil, err
	}
	if limit == "" || expiry == nil {
		s.store.DeleteOrgInteractionLimit(org.Login)
	} else {
		s.store.SetOrgInteractionLimit(org.Login, limit, *expiry)
	}
	updated := s.store.GetOrgByID(org.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Organization")
	}
	return map[string]interface{}{"organization": optionalRendered(updated, orgToGraphQL)}, nil
}

func (s *Resolver) resolveSetUserInteractionLimit(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "userId")
	user := store.FindUserByNodeID(s.store, nodeID)
	if user == nil {
		return nil, gqlMissingNode("User", nodeID)
	}
	limit, expiry, err := s.interactionLimitFromInput(input)
	if err != nil {
		return nil, err
	}
	if !s.store.SetUserInteractionLimit(user.ID, limit, expiry) {
		return nil, gqlMissingNode("User", nodeID)
	}
	updated := s.store.GetUserByID(user.ID)
	if updated == nil {
		return nil, gqlMissingNode("User", nodeID)
	}
	return map[string]interface{}{"user": optionalRendered(updated, userToGraphQL)}, nil
}

// interactionLimitFromInput translates the limit/expiry enums into the stored
// limit name and expiry instant. NO_LIMIT means "remove the limit", returned as
// an empty limit with no expiry.
func (s *Resolver) interactionLimitFromInput(input map[string]interface{}) (string, *time.Time, error) {
	limit, _ := gqlInputString(input, "limit")
	if limit == "NO_LIMIT" {
		return "", nil, nil
	}
	stored := strings.ToLower(limit)
	if !store.IsInteractionGroup(stored) {
		return "", nil, fmt.Errorf("interaction limit %q is not one of COLLABORATORS_ONLY, CONTRIBUTORS_ONLY, EXISTING_USERS, NO_LIMIT", limit)
	}
	expiry, _ := gqlInputString(input, "expiry")
	expiresAt, ok := store.InteractionLimitExpiry(strings.ToLower(expiry), s.store.CurrentTime())
	if !ok {
		return "", nil, fmt.Errorf("interaction limit expiry %q is not one of ONE_DAY, THREE_DAYS, ONE_WEEK, ONE_MONTH, SIX_MONTHS", expiry)
	}
	return stored, &expiresAt, nil
}

// organization resolvers

func (s *Resolver) resolveRemoveOutsideCollaborator(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	org, err := s.mutationOrgFromInput(input, "organizationId")
	if err != nil {
		return nil, err
	}
	nodeID, _ := gqlInputString(input, "userId")
	user := store.FindUserByNodeID(s.store, nodeID)
	if user == nil {
		return nil, gqlMissingNode("User", nodeID)
	}
	s.store.RemoveOutsideCollaborator(org.Login, user.Login)
	return map[string]interface{}{"removedUser": optionalRendered(user, userToGraphQL)}, nil
}

func (s *Resolver) resolveUpdateOrganizationForkingSetting(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	enabled, _ := gqlInputBool(input, "forkingEnabled")
	updated, err := s.updateOrgRow(input, func(o *store.Org) {
		o.MembersCanForkPrivateRepositories = store.BoolPointer(enabled)
	})
	if err != nil {
		return nil, err
	}
	message := "private repository forking is disabled"
	if enabled {
		message = "private repository forking is enabled"
	}
	return map[string]interface{}{
		"message":      message,
		"organization": optionalRendered(updated, orgToGraphQL),
	}, nil
}

func (s *Resolver) resolveUpdateOrganizationWebCommitSignoff(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	required, _ := gqlInputBool(input, "webCommitSignoffRequired")
	updated, err := s.updateOrgRow(input, func(o *store.Org) {
		o.WebCommitSignoffRequired = required
	})
	if err != nil {
		return nil, err
	}
	message := "web commit signoff is not required"
	if required {
		message = "web commit signoff is required"
	}
	return map[string]interface{}{
		"message":      message,
		"organization": optionalRendered(updated, orgToGraphQL),
	}, nil
}

// mutationOrgFromInput resolves the organization a mutation input names.
func (s *Resolver) mutationOrgFromInput(input map[string]interface{}, key string) (*store.Org, error) {
	nodeID, _ := gqlInputString(input, key)
	org := s.orgByNodeID(nodeID)
	if org == nil {
		return nil, gqlMissingNode("Organization", nodeID)
	}
	return org, nil
}

// updateOrgRow applies a settings write through UpdateOrg and returns the
// resulting detached row.
func (s *Resolver) updateOrgRow(input map[string]interface{}, apply func(*store.Org)) (*store.Org, error) {
	org, err := s.mutationOrgFromInput(input, "organizationId")
	if err != nil {
		return nil, err
	}
	now := s.store.CurrentTime()
	if !s.store.UpdateOrg(org.Login, func(o *store.Org) {
		apply(o)
		o.UpdatedAt = now
	}) {
		return nil, gqlMissingNodeType("Organization")
	}
	updated := s.store.GetOrgByID(org.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Organization")
	}
	return updated, nil
}
