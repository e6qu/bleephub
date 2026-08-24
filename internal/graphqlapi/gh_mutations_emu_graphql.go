package graphqlapi

import (
	"fmt"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// accessUserNamespaceRepository and createAttributionInvitation: the two
// administrative mutations over managed accounts and imported identities.
// The first records a temporary access grant honored inside the repository
// capability lattice itself; the second records the ask that a mannequin's
// work be claimed by a real account.

func init() {
	// accessUserNamespaceRepository's row lives in the enterprise family's
	// registry, whose sweep pins every enterprise-scoped rule in one place.
	for name, rule := range map[string]mutationRule{
		"createAttributionInvitation": attributionInvitationRule{},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// attributionInvitationRule requires ownership of the organization the
// invitation is issued under: attribution rewrites authorship, which is
// governance, not participation.
type attributionInvitationRule struct{}

func (attributionInvitationRule) check() error { return nil }

func (attributionInvitationRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input["ownerId"].(string)
	ownerID, ownerType, ok := resolveProjectOwner(s.store, nodeID)
	if !ok || ownerType != "Organization" {
		return gqlMissingNode("Organization", nodeID)
	}
	org := s.store.GetOrgByID(ownerID)
	if org == nil {
		return gqlMissingNode("Organization", nodeID)
	}
	if !s.viewerCanAdminAccount(p.Context, org.Login) {
		return gqlMissingNode("Organization", nodeID)
	}
	return nil
}

// mannequinSource renders the Mannequin type from its store row.
func (s *Resolver) mannequinSource(m *store.Mannequin) map[string]interface{} {
	if m == nil {
		return nil
	}
	var claimant interface{}
	if m.ClaimantID != 0 {
		claimant = optionalRendered(s.store.GetUserByID(m.ClaimantID), userToGraphQL)
	}
	return map[string]interface{}{
		"__typename":   "Mannequin",
		"id":           m.NodeID,
		"login":        m.Login,
		"email":        nullableString(m.Email),
		"claimant":     claimant,
		"createdAt":    m.CreatedAt.Format(time.RFC3339),
		"updatedAt":    m.UpdatedAt.Format(time.RFC3339),
		"avatarUrl":    externalURL("/avatars/mannequin/" + m.Login),
		"url":          externalURL("/" + m.Login),
		"resourcePath": "/" + m.Login,
		"databaseId":   m.ID,
	}
}

func (s *Resolver) addEMUMutationsToSchema(mutationType *graphql.Object) {
	accessInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AccessUserNamespaceRepositoryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"enterpriseId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"repositoryId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	accessPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "AccessUserNamespaceRepositoryPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"expiresAt":        &graphql.Field{Type: s.graphQLStringScalar("DateTime")},
			"repository":       &graphql.Field{Type: s.graphqlTypes.repository},
		},
	})
	s.registerMutation(mutationType, "accessUserNamespaceRepository", &graphql.Field{
		Type: accessPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(accessInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			enterprise := store.FindEnterpriseByNodeID(s.store, str(input["enterpriseId"]))
			if enterprise == nil {
				return nil, gqlMissingNode("Enterprise", str(input["enterpriseId"]))
			}
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			// The repository must live in a user namespace whose account the
			// enterprise manages. bleephub's managed-account analogue is an
			// enterprise membership under an enterprise-configured identity
			// provider — the closest truthful reading of "managed user" for
			// an instance whose accounts are IdP-provisioned.
			owner, _, ok := store.SplitRepoFullName(repo.FullName)
			if !ok {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			subject := s.store.LookupUserByLogin(owner)
			if subject == nil {
				return nil, fmt.Errorf("the repository is not in a user namespace")
			}
			if enterprise.IdentityProvider == nil {
				return nil, fmt.Errorf("the enterprise does not manage its users through an identity provider")
			}
			if s.store.EffectiveEnterpriseRole(enterprise.ID, subject) == "" {
				return nil, fmt.Errorf("the repository's owner is not managed by this enterprise")
			}
			expires := s.store.GrantUserNamespaceAccess(enterprise.ID, repo.ID, user.ID)
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"expiresAt":        expires.Format(time.RFC3339),
				"repository":       optionalObject(repoToGraphQL(s.store, s.store.GetRepoByID(repo.ID))),
			}, nil
		},
	})

	attributionInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateAttributionInvitationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"ownerId":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"sourceId":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"targetId":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	mannequinType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mannequin",
		Fields: graphql.Fields{
			"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"login":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"email":      &graphql.Field{Type: graphql.String},
			"claimant":   &graphql.Field{Type: s.graphqlTypes.user},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"createdAt":  &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
			"updatedAt":  &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
			"avatarUrl": &graphql.Field{
				Type: graphql.NewNonNull(s.graphQLStringScalar("URI")),
				Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int}},
			},
			"url":          &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("URI"))},
			"resourcePath": &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("URI"))},
		},
	})

	// Claimable is the union of identities whose work can be claimed: the
	// mannequin holding it, or the user it would move to.
	claimable := graphql.NewUnion(graphql.UnionConfig{
		Name:  "Claimable",
		Types: []*graphql.Object{mannequinType, s.graphqlTypes.user},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name == "Mannequin" {
				return mannequinType
			}
			return s.graphqlTypes.user
		},
	})
	attributionPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateAttributionInvitationPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"owner":            &graphql.Field{Type: s.graphqlTypes.organization},
			"source":           &graphql.Field{Type: claimable},
			"target":           &graphql.Field{Type: claimable},
		},
	})
	s.registerMutation(mutationType, "createAttributionInvitation", &graphql.Field{
		Type: attributionPayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(attributionInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			ownerID, ownerType, ok := resolveProjectOwner(s.store, str(input["ownerId"]))
			if !ok || ownerType != "Organization" {
				return nil, gqlMissingNode("Organization", str(input["ownerId"]))
			}
			org := s.store.GetOrgByID(ownerID)
			if org == nil {
				return nil, gqlMissingNode("Organization", str(input["ownerId"]))
			}
			source := store.FindMannequinByNodeID(s.store, str(input["sourceId"]))
			if source == nil || source.OrgID != org.ID {
				return nil, gqlMissingNode("Mannequin", str(input["sourceId"]))
			}
			targetID := str(input["targetId"])
			targetUserID, targetType, ok := resolveProjectOwner(s.store, targetID)
			if !ok || targetType != "User" {
				return nil, gqlMissingNode("User", targetID)
			}
			target := s.store.GetUserByID(targetUserID)
			if target == nil {
				return nil, gqlMissingNode("User", targetID)
			}
			if _, err := s.store.CreateAttributionInvitation(org.ID, source.NodeID, target.NodeID); err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"owner":            optionalObject(orgToGraphQL(org)),
				"source":           optionalObject(s.mannequinSource(source)),
				"target":           optionalObject(userToGraphQL(target)),
			}, nil
		},
	})
}
