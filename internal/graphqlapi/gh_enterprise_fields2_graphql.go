package graphqlapi

// Second-pass completion of the enterprise identity / team / invitation read
// surface. These fields are installed after buildEnterpriseExtraTypes because
// they name types that pass builds (the OrganizationInvitation, ExternalIdentity
// and EnterpriseServerInstallation nodes) as well as cross-family types
// (Organization, User) and the OrganizationIdentityProvider the account surface
// memoizes later.
//
// Where bleephub keeps real data the field reads it: an enterprise team's
// assigned organizations and members are store-backed and rendered live. Where
// the single-instance broker provisions nothing — SCIM/SAML external identities,
// GitHub Enterprise Server installations, cross-org invitation aggregation — the
// field is declared for GitHub parity and answers a truthful-empty connection or
// null rather than an invented value.

import (
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addEnterpriseIdentityCompletionFields installs the remaining missing fields on
// OrganizationInvitation, EnterpriseTeam, ExternalIdentity,
// OrganizationIdentityProvider, EnterpriseServerInstallation and OIDCProvider.
func (s *Resolver) addEnterpriseIdentityCompletionFields(
	orgType, userType *graphql.Object,
	nodeInterface *graphql.Interface,
	certificate *graphql.Scalar,
	extras *enterpriseExtraTypes,
) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	reg := s.accountSurfaceRegistry()

	// --- attribute sub-objects the external-identity node names --------------
	userEmailMetadata := graphql.NewObject(graphql.ObjectConfig{
		Name: "UserEmailMetadata",
		Fields: graphql.Fields{
			"primary": &graphql.Field{Type: graphql.Boolean},
			"type":    &graphql.Field{Type: graphql.String},
			"value":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	externalIdentityAttribute := graphql.NewObject(graphql.ObjectConfig{
		Name: "ExternalIdentityAttribute",
		Fields: graphql.Fields{
			"metadata": &graphql.Field{Type: graphql.String},
			"name":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"value":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	samlAttributes := graphql.NewObject(graphql.ObjectConfig{
		Name: "ExternalIdentitySamlAttributes",
		Fields: graphql.Fields{
			"attributes": &graphql.Field{Type: graphql.NewNonNull(
				graphql.NewList(graphql.NewNonNull(externalIdentityAttribute)))},
			"emails":     &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(userEmailMetadata))},
			"familyName": &graphql.Field{Type: graphql.String},
			"givenName":  &graphql.Field{Type: graphql.String},
			"groups":     &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"nameId":     &graphql.Field{Type: graphql.String},
			"username":   &graphql.Field{Type: graphql.String},
		},
	})
	scimAttributes := graphql.NewObject(graphql.ObjectConfig{
		Name: "ExternalIdentityScimAttributes",
		Fields: graphql.Fields{
			"emails":     &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(userEmailMetadata))},
			"familyName": &graphql.Field{Type: graphql.String},
			"givenName":  &graphql.Field{Type: graphql.String},
			"groups":     &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"username":   &graphql.Field{Type: graphql.String},
		},
	})

	// --- OrganizationInvitation ---------------------------------------------
	// The node both enterprise invitation connections carry. No producer on this
	// instance aggregates cross-org invitations, so the node is never realized;
	// the fields read the invitation source map by name (default resolver) so a
	// populated node would render correctly.
	extras.orgInvitationType.AddFieldConfig("invitationSource", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("OrganizationInvitationSource", "MEMBER", "SCIM", "UNKNOWN")),
	})
	extras.orgInvitationType.AddFieldConfig("invitationType", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("OrganizationInvitationType", "EMAIL", "USER")),
	})
	extras.orgInvitationType.AddFieldConfig("invitee", &graphql.Field{Type: userType})
	extras.orgInvitationType.AddFieldConfig("inviter", &graphql.Field{Type: graphql.NewNonNull(userType)})
	extras.orgInvitationType.AddFieldConfig("inviterActor", &graphql.Field{Type: userType})
	extras.orgInvitationType.AddFieldConfig("organization", &graphql.Field{Type: graphql.NewNonNull(orgType)})
	extras.orgInvitationType.AddFieldConfig("role", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("OrganizationInvitationRole",
			"ADMIN", "BILLING_MANAGER", "DIRECT_MEMBER", "REINSTATE")),
	})

	// --- ExternalIdentity ----------------------------------------------------
	// SCIM/SAML provisioning is not performed by the single-instance broker, so
	// each attribute block and the linked invitation are null.
	extras.externalIdentityType.AddFieldConfig("organizationInvitation", &graphql.Field{Type: extras.orgInvitationType})
	extras.externalIdentityType.AddFieldConfig("samlIdentity", &graphql.Field{Type: samlAttributes})
	extras.externalIdentityType.AddFieldConfig("scimIdentity", &graphql.Field{Type: scimAttributes})

	// --- EnterpriseTeam ------------------------------------------------------
	assignedOrgConnection := s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseTeamAssignedOrganizationConnection", "EnterpriseTeamAssignedOrganizationEdge", orgType, nil, nil)
	teamMemberConnection := s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseTeamMemberConnection", "EnterpriseTeamMemberEdge", userType, nil, nil)

	extras.enterpriseTeamType.AddFieldConfig("privacy", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("TeamPrivacy", "SECRET", "VISIBLE")),
	})
	extras.enterpriseTeamType.AddFieldConfig("assignedOrganizations", &graphql.Field{
		Type: graphql.NewNonNull(assignedOrgConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(reg,
				"EnterpriseTeamOrganizationOrder", "EnterpriseTeamOrganizationOrderField",
				"CREATED_AT", "ID", "LOGIN")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team := s.enterpriseTeamFromSource(p.Source)
			if team == nil {
				return enterpriseConnection(nil, p.Args), nil
			}
			var orgs []*store.Org
			switch strings.ToLower(team.OrganizationSelectionType) {
			case "all":
				orgs = s.store.ListOrgsAll(0)
			case "selected":
				for _, login := range team.SelectedOrgLogins {
					if o := s.store.GetOrg(login); o != nil {
						orgs = append(orgs, o)
					}
				}
			}
			var nodes []map[string]interface{}
			for _, o := range orgs {
				nodes = append(nodes, orgToGraphQL(o))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	})
	extras.enterpriseTeamType.AddFieldConfig("enterpriseTeamMembers", &graphql.Field{
		Type: graphql.NewNonNull(teamMemberConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(reg,
				"EnterpriseTeamMemberOrder", "EnterpriseTeamMemberOrderField",
				"CREATED_AT", "ID", "LOGIN")},
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team := s.enterpriseTeamFromSource(p.Source)
			if team == nil {
				return enterpriseConnection(nil, p.Args), nil
			}
			query, _ := p.Args["query"].(string)
			var nodes []map[string]interface{}
			for _, u := range s.store.ListEnterpriseTeamMembers(team) {
				if query != "" && !strings.Contains(strings.ToLower(u.Login), strings.ToLower(query)) &&
					!strings.Contains(strings.ToLower(u.Name), strings.ToLower(query)) {
					continue
				}
				nodes = append(nodes, userToGraphQL(u))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	})

	// --- EnterpriseServerInstallation ---------------------------------------
	// No GitHub Enterprise Server installation exists under a single instance, so
	// both connections are constant-empty; the node types are declared for the
	// connection's nodes/edges to name.
	serverUserAccount := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseServerUserAccount",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":              &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"createdAt":       &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":       &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"isSiteAdmin":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"login":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"profileName":     &graphql.Field{Type: graphql.String},
			"remoteCreatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"remoteUserId":    &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	serverUpload := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseServerUserAccountsUpload",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"name":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"syncState": &graphql.Field{Type: graphql.NewNonNull(
				s.sharedEnum("EnterpriseServerUserAccountsUploadSyncState", "FAILURE", "PENDING", "SUCCESS"))},
		},
	})
	// The account's emails, back-references and the upload's enterprise/
	// installation members. No GHES installation exists under a single
	// instance, so userAccounts/userAccountsUploads never realize a node and
	// these resolvers never run; the fields are declared for schema parity, the
	// non-null back-references reading their (never-produced) source.
	serverUserAccountEmail := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseServerUserAccountEmail",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"createdAt":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"email":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"isPrimary":   &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"userAccount": &graphql.Field{Type: graphql.NewNonNull(serverUserAccount)},
		},
	})
	serverUserAccountEmailConnection := s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseServerUserAccountEmailConnection", "EnterpriseServerUserAccountEmailEdge", serverUserAccountEmail, nil, nil)
	serverUserAccount.AddFieldConfig("emails", &graphql.Field{
		Type: graphql.NewNonNull(serverUserAccountEmailConnection),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return enterpriseConnection(nil, p.Args), nil
		},
	})
	serverUserAccount.AddFieldConfig("enterpriseServerInstallation", &graphql.Field{
		Type: graphql.NewNonNull(extras.serverInstallationType),
	})
	serverUpload.AddFieldConfig("enterprise", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.enterprise),
	})
	serverUpload.AddFieldConfig("enterpriseServerInstallation", &graphql.Field{
		Type: graphql.NewNonNull(extras.serverInstallationType),
	})

	serverUserAccountConnection := s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseServerUserAccountConnection", "EnterpriseServerUserAccountEdge", serverUserAccount, nil, nil)
	serverUploadConnection := s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseServerUserAccountsUploadConnection", "EnterpriseServerUserAccountsUploadEdge", serverUpload, nil, nil)

	extras.serverInstallationType.AddFieldConfig("userAccounts", &graphql.Field{
		Type: graphql.NewNonNull(serverUserAccountConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(reg,
				"EnterpriseServerUserAccountOrder", "EnterpriseServerUserAccountOrderField",
				"LOGIN", "REMOTE_CREATED_AT")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return enterpriseConnection(nil, p.Args), nil
		},
	})
	extras.serverInstallationType.AddFieldConfig("userAccountsUploads", &graphql.Field{
		Type: graphql.NewNonNull(serverUploadConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(reg,
				"EnterpriseServerUserAccountsUploadOrder", "EnterpriseServerUserAccountsUploadOrderField",
				"CREATED_AT")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return enterpriseConnection(nil, p.Args), nil
		},
	})

	// --- OIDCProvider --------------------------------------------------------
	// Authentication is brokered by an external SSO provider, not a
	// GitHub-managed OIDC tenant, so no external identities are provisioned.
	extras.oidcProviderType.AddFieldConfig("externalIdentities", &graphql.Field{
		Type: graphql.NewNonNull(extras.externalIdentityConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"login":       &graphql.ArgumentConfig{Type: graphql.String},
			"membersOnly": &graphql.ArgumentConfig{Type: graphql.Boolean},
			"userName":    &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return enterpriseConnection(nil, p.Args), nil
		},
	})

	// --- OrganizationIdentityProvider ---------------------------------------
	// Pre-register the complete type (memoized by name) before the account
	// surface builds its subset, so externalIdentities and idpCertificate are
	// present. SAML binds at the enterprise on this instance, never at the
	// organization, so no external identities are provisioned and no IdP
	// certificate is stored.
	s.mutationObject("OrganizationIdentityProvider", graphql.Fields{
		"id":              &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"issuer":          &graphql.Field{Type: graphql.String},
		"ssoUrl":          &graphql.Field{Type: uri},
		"digestMethod":    &graphql.Field{Type: uri},
		"signatureMethod": &graphql.Field{Type: uri},
		"organization":    &graphql.Field{Type: orgType},
		"idpCertificate":  &graphql.Field{Type: certificate},
		"externalIdentities": &graphql.Field{
			Type: graphql.NewNonNull(extras.externalIdentityConnection),
			Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
				"login":       &graphql.ArgumentConfig{Type: graphql.String},
				"membersOnly": &graphql.ArgumentConfig{Type: graphql.Boolean},
				"userName":    &graphql.ArgumentConfig{Type: graphql.String},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return enterpriseConnection(nil, p.Args), nil
			},
		},
	})
}

// enterpriseTeamFromSource re-reads the enterprise team an EnterpriseTeam source
// map names by its slug, returning nil when the source is not a realized team.
func (s *Resolver) enterpriseTeamFromSource(source interface{}) *store.EnterpriseTeam {
	m, ok := source.(map[string]interface{})
	if !ok {
		return nil
	}
	slug, _ := m["slug"].(string)
	if slug == "" {
		return nil
	}
	return s.store.GetEnterpriseTeam(slug)
}
