package graphqlapi

// The five verifiable-domain mutations and the VerifiableDomain object their
// payloads return. A domain belongs to an enterprise or an organization (the
// VerifiableDomainOwner union) and writes through the same store ledger the
// /ui-data surface and the notification-delivery restriction read.

import (
	"fmt"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func init() {
	for name, rule := range map[string]mutationRule{
		"addVerifiableDomain":             verifiableDomainOwnerRule{ownerKey: "ownerId"},
		"approveVerifiableDomain":         verifiableDomainOwnerRule{domainKey: "id"},
		"deleteVerifiableDomain":          verifiableDomainOwnerRule{domainKey: "id"},
		"regenerateVerifiableDomainToken": verifiableDomainOwnerRule{domainKey: "id"},
		"verifyVerifiableDomain":          verifiableDomainOwnerRule{domainKey: "id"},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// verifiableDomainOwnerRule authorizes the domain mutations against the
// domain's owner (enterprise or organization).
type verifiableDomainOwnerRule struct {
	// ownerKey names the owner directly; domainKey names an existing domain
	// whose owner is looked up. Exactly one is set.
	ownerKey  string
	domainKey string
}

func (r verifiableDomainOwnerRule) check() error {
	if (r.ownerKey == "") == (r.domainKey == "") {
		return fmt.Errorf("exactly one of ownerKey or domainKey must be set")
	}
	return nil
}

func (r verifiableDomainOwnerRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	ownerType, ownerID, nodeID, err := r.resolveOwner(s, input)
	if err != nil {
		return err
	}
	viewer := s.ghUserFromContext(p.Context)
	switch ownerType {
	case store.VerifiableDomainOwnerEnterprise:
		if !s.store.IsEnterpriseOwner(ownerID, viewer) {
			return enterpriseOwnerRequired()
		}
		return nil
	case store.VerifiableDomainOwnerOrganization:
		org := s.store.GetOrgByID(ownerID)
		if org == nil {
			return gqlMissingNode("VerifiableDomainOwner", nodeID)
		}
		return s.authorizeOrgAdministration(p, org.Login, store.ScopeOrgAdministration)
	}
	return gqlMissingNode("VerifiableDomainOwner", nodeID)
}

func (r verifiableDomainOwnerRule) resolveOwner(s *Resolver, input map[string]interface{}) (string, int, string, error) {
	if r.domainKey != "" {
		nodeID, _ := input[r.domainKey].(string)
		domain := store.FindVerifiableDomainByNodeID(s.store, nodeID)
		if domain == nil {
			return "", 0, nodeID, gqlMissingNode("VerifiableDomain", nodeID)
		}
		return domain.OwnerType, domain.OwnerID, nodeID, nil
	}
	nodeID, _ := input[r.ownerKey].(string)
	if e := store.FindEnterpriseByNodeID(s.store, nodeID); e != nil {
		return store.VerifiableDomainOwnerEnterprise, e.ID, nodeID, nil
	}
	if org := s.orgByNodeID(nodeID); org != nil {
		return store.VerifiableDomainOwnerOrganization, org.ID, nodeID, nil
	}
	return "", 0, nodeID, gqlMissingNode("VerifiableDomainOwner", nodeID)
}

// --- type and rendering ------------------------------------------------------

// gqlVerifiableDomainType returns the VerifiableDomain object (memoized).
func (s *Resolver) gqlVerifiableDomainType() *graphql.Object {
	if existing := s.memoizedMutationObject("VerifiableDomain"); existing != nil {
		return existing
	}
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	domainType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "VerifiableDomain",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.Fields{
			"id":                             gqlNonNull(graphql.ID),
			"databaseId":                     gqlField(graphql.Int),
			"createdAt":                      gqlNonNull(dateTime),
			"updatedAt":                      gqlNonNull(dateTime),
			"dnsHostName":                    gqlField(uri),
			"domain":                         gqlNonNull(uri),
			"punycodeEncodedDomain":          gqlNonNull(uri),
			"hasFoundHostName":               gqlNonNull(graphql.Boolean),
			"hasFoundVerificationToken":      gqlNonNull(graphql.Boolean),
			"isApproved":                     gqlNonNull(graphql.Boolean),
			"isRequiredForPolicyEnforcement": gqlNonNull(graphql.Boolean),
			"isVerified":                     gqlNonNull(graphql.Boolean),
			"owner":                          gqlNonNull(s.gqlVerifiableDomainOwnerUnion()),
			"tokenExpirationTime":            gqlField(dateTime),
			"verificationToken":              gqlField(graphql.String),
		},
	})
	s.mutationObjects["VerifiableDomain"] = domainType
	return domainType
}

// verifiableDomainToGQL renders one domain row. DNS lookup is simulated, so the
// two hasFound members mirror the verified state.
func (s *Resolver) verifiableDomainToGQL(domain *store.VerifiableDomain) map[string]interface{} {
	if domain == nil {
		return nil
	}
	owner := s.verifiableDomainOwnerSource(domain.OwnerType, domain.OwnerID)
	return map[string]interface{}{
		"id":                             domain.NodeID,
		"databaseId":                     domain.ID,
		"createdAt":                      domain.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":                      domain.UpdatedAt.UTC().Format(time.RFC3339),
		"dnsHostName":                    "_github-challenge-" + verifiableDomainOwnerLabel(owner) + "." + domain.Domain,
		"domain":                         domain.Domain,
		"punycodeEncodedDomain":          domain.Domain,
		"hasFoundHostName":               domain.IsVerified,
		"hasFoundVerificationToken":      domain.IsVerified,
		"isApproved":                     domain.IsApproved,
		"isRequiredForPolicyEnforcement": s.verifiableDomainRequiredForPolicy(domain),
		"isVerified":                     domain.IsVerified,
		"owner":                          optionalObject(owner),
		"tokenExpirationTime":            domain.TokenExpiresAt.UTC().Format(time.RFC3339),
		"verificationToken":              domain.VerificationToken,
	}
}

func verifiableDomainOwnerLabel(owner map[string]interface{}) string {
	if owner == nil {
		return "owner"
	}
	if slug, ok := owner["slug"].(string); ok && slug != "" {
		return slug
	}
	if login, ok := owner["login"].(string); ok && login != "" {
		return login
	}
	return "owner"
}

// verifiableDomainRequiredForPolicy reports whether the owner's
// notification-delivery restriction is on and this domain counts toward it.
func (s *Resolver) verifiableDomainRequiredForPolicy(domain *store.VerifiableDomain) bool {
	if !domain.IsVerified && !domain.IsApproved {
		return false
	}
	switch domain.OwnerType {
	case store.VerifiableDomainOwnerEnterprise:
		e := s.store.GetEnterpriseByID(domain.OwnerID)
		return e != nil && e.Policy.NotificationDeliveryRestrictionEnabled == store.EnterprisePolicyEnabled
	case store.VerifiableDomainOwnerOrganization:
		org := s.store.GetOrgByID(domain.OwnerID)
		return org != nil && org.NotificationDeliveryRestrictionEnabled
	}
	return false
}

// --- schema -----------------------------------------------------------------

// addVerifiableDomainMutationsToSchema installs the five domain mutations.
func (s *Resolver) addVerifiableDomainMutationsToSchema(mutationType *graphql.Object) {
	domainType := s.gqlVerifiableDomainType()
	uri := s.graphQLStringScalar("URI")

	s.registerMutation(mutationType, "addVerifiableDomain", &graphql.Field{
		Type: s.mutationPayload("AddVerifiableDomainPayload", graphql.Fields{
			"domain": gqlField(domainType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("AddVerifiableDomainInput", graphql.InputObjectConfigFieldMap{
				"domain":  gqlNonNullInputOf(uri),
				"ownerId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveAddVerifiableDomain,
	})

	s.registerMutation(mutationType, "approveVerifiableDomain", &graphql.Field{
		Type: s.mutationPayload("ApproveVerifiableDomainPayload", graphql.Fields{
			"domain": gqlField(domainType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("ApproveVerifiableDomainInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveApproveVerifiableDomain,
	})

	s.registerMutation(mutationType, "deleteVerifiableDomain", &graphql.Field{
		Type: s.mutationPayload("DeleteVerifiableDomainPayload", graphql.Fields{
			"owner": gqlField(s.gqlVerifiableDomainOwnerUnion()),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteVerifiableDomainInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteVerifiableDomain,
	})

	s.registerMutation(mutationType, "regenerateVerifiableDomainToken", &graphql.Field{
		Type: s.mutationPayload("RegenerateVerifiableDomainTokenPayload", graphql.Fields{
			"verificationToken": gqlField(graphql.String),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("RegenerateVerifiableDomainTokenInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveRegenerateVerifiableDomainToken,
	})

	s.registerMutation(mutationType, "verifyVerifiableDomain", &graphql.Field{
		Type: s.mutationPayload("VerifyVerifiableDomainPayload", graphql.Fields{
			"domain": gqlField(domainType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("VerifyVerifiableDomainInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveVerifyVerifiableDomain,
	})
}

// --- resolvers ---------------------------------------------------------------

func (s *Resolver) resolveAddVerifiableDomain(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	ownerType, ownerID, _, err := (verifiableDomainOwnerRule{ownerKey: "ownerId"}).resolveOwner(s, input)
	if err != nil {
		return nil, err
	}
	domainName, _ := gqlInputString(input, "domain")
	created, err := s.store.CreateVerifiableDomain(ownerType, ownerID, domainName)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"domain": optionalObject(s.verifiableDomainToGQL(created))}, nil
}

// verifiableDomainFromInput resolves the domain an id-carrying input names.
func (s *Resolver) verifiableDomainFromInput(input map[string]interface{}) (*store.VerifiableDomain, error) {
	nodeID, _ := gqlInputString(input, "id")
	domain := store.FindVerifiableDomainByNodeID(s.store, nodeID)
	if domain == nil {
		return nil, gqlMissingNode("VerifiableDomain", nodeID)
	}
	return domain, nil
}

func (s *Resolver) resolveApproveVerifiableDomain(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	domain, err := s.verifiableDomainFromInput(input)
	if err != nil {
		return nil, err
	}
	approved := s.store.ApproveVerifiableDomain(domain.ID)
	if approved == nil {
		return nil, gqlMissingNodeType("VerifiableDomain")
	}
	return map[string]interface{}{"domain": optionalObject(s.verifiableDomainToGQL(approved))}, nil
}

func (s *Resolver) resolveVerifyVerifiableDomain(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	domain, err := s.verifiableDomainFromInput(input)
	if err != nil {
		return nil, err
	}
	verified, err := s.store.VerifyVerifiableDomain(domain.ID)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"domain": optionalObject(s.verifiableDomainToGQL(verified))}, nil
}

func (s *Resolver) resolveRegenerateVerifiableDomainToken(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	domain, err := s.verifiableDomainFromInput(input)
	if err != nil {
		return nil, err
	}
	regenerated := s.store.RegenerateVerifiableDomainToken(domain.ID)
	if regenerated == nil {
		return nil, gqlMissingNodeType("VerifiableDomain")
	}
	return map[string]interface{}{"verificationToken": regenerated.VerificationToken}, nil
}

func (s *Resolver) resolveDeleteVerifiableDomain(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	domain, err := s.verifiableDomainFromInput(input)
	if err != nil {
		return nil, err
	}
	removed := s.store.DeleteVerifiableDomain(domain.ID)
	if removed == nil {
		return nil, gqlMissingNodeType("VerifiableDomain")
	}
	return map[string]interface{}{"owner": optionalObject(s.verifiableDomainOwnerSource(removed.OwnerType, removed.OwnerID))}, nil
}

// verifiableDomainOwnerSource renders the owning account as its union member.
func (s *Resolver) verifiableDomainOwnerSource(ownerType string, ownerID int) map[string]interface{} {
	switch ownerType {
	case store.VerifiableDomainOwnerEnterprise:
		if e := s.store.GetEnterpriseByID(ownerID); e != nil {
			return enterpriseToGraphQL(e)
		}
	case store.VerifiableDomainOwnerOrganization:
		if org := s.store.GetOrgByID(ownerID); org != nil {
			return orgToGraphQL(org)
		}
	}
	return nil
}
