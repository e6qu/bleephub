package graphqlapi

// The remaining enterprise-family read surface: connection fields on
// Enterprise, EnterpriseOwnerInfo, EnterpriseIdentityProvider and
// EnterpriseUserAccount beyond the members/organizations core.
//
// Store-backed models (enterprise teams, repository custom properties,
// verifiable domains) return live rows; features this single-instance
// simulator does not model return truthful-empty connections or null.

import (
	"strconv"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// enterpriseExtraTypes collects the connection objects the extra fields name.
type enterpriseExtraTypes struct {
	enterpriseTeamType                 *graphql.Object
	enterpriseTeamConnection           *graphql.Object
	repositoryCustomPropertyType       *graphql.Object
	repositoryCustomPropertyConnection *graphql.Object
	userNamespaceRepositoryConnection  *graphql.Object
	rulesetType                        *graphql.Object
	rulesetConnection                  *graphql.Object
	domainConnection                   *graphql.Object
	serverInstallationConnection       *graphql.Object
	failedInvitationConnection         *graphql.Object
	pendingMemberInvitationConnection  *graphql.Object
	repositoryInvitationConnection     *graphql.Object
	externalIdentityConnection         *graphql.Object
	serverInstallationMembershipConn   *graphql.Object
	enterpriseRepositoryInfoConnection *graphql.Object
	oidcProviderType                   *graphql.Object
	announcementBannerType             *graphql.Object
	// Node types the second-pass installer (gh_enterprise_fields2_graphql.go)
	// hangs cross-referencing fields on.
	orgInvitationType      *graphql.Object
	externalIdentityType   *graphql.Object
	serverInstallationType *graphql.Object
}

// buildEnterpriseExtraTypes mints the node and connection types the extra
// enterprise fields return. Runs after the Enterprise object exists but before
// the outside-collaborator connection (that edge names EnterpriseRepositoryInfoConnection).
func (s *Resolver) buildEnterpriseExtraTypes(enterpriseType, userType *graphql.Object, nodeInterface *graphql.Interface) *enterpriseExtraTypes {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	bigInt := s.graphQLStringScalar("BigInt")
	pageInfo := s.gqlPageInfoType()

	extras := &enterpriseExtraTypes{}

	// --- EnterpriseTeam (real: store-backed) --------------------------------
	teamType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseTeam",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"combinedSlug":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"createdAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"description":    &graphql.Field{Type: graphql.String},
			"enterprise":     &graphql.Field{Type: enterpriseType},
			"fullDatabaseId": &graphql.Field{Type: bigInt},
			"isViewerMember": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"notificationSetting": &graphql.Field{Type: graphql.NewNonNull(
				s.sharedEnum("TeamNotificationSetting", "NOTIFICATIONS_DISABLED", "NOTIFICATIONS_ENABLED"))},
			"organizationSelectionType": &graphql.Field{Type: graphql.NewNonNull(
				s.sharedEnum("EnterpriseTeamOrganizationSelectionType", "ALL", "DISABLED", "SELECTED"))},
			"slug":                &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"viewerCanAdminister": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
	})
	extras.enterpriseTeamType = teamType
	// Stashed by name so the residual BypassActor/PermissionGranter unions reach
	// this one EnterpriseTeam instance rather than minting a rejected duplicate.
	s.stashNamedObject(teamType)
	extras.enterpriseTeamConnection = advisoryConnectionType("EnterpriseTeam", teamType, pageInfo)

	// --- RepositoryCustomProperty (real: enterprise schema) -----------------
	// Shared (memoized) with the organization read surface.
	extras.repositoryCustomPropertyType = s.gqlRepositoryCustomPropertyType()
	extras.repositoryCustomPropertyConnection = s.accountConnectionType(
		s.accountSurfaceRegistry(), "RepositoryCustomProperty", extras.repositoryCustomPropertyType, false, nil)

	// --- UserNamespaceRepository (EMU only; empty here) ---------------------
	userNamespaceRepoType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "UserNamespaceRepository",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":            &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"name":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"nameWithOwner": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"owner":         &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.repositoryOwner)},
		},
	})
	extras.userNamespaceRepositoryConnection = advisoryConnectionType("UserNamespaceRepository", userNamespaceRepoType, pageInfo)

	// --- RepositoryRuleset (shared with the account surface) ----------------
	extras.rulesetType = s.accountSurfaceRegistry().ruleset
	extras.rulesetConnection = s.accountSurfaceRegistry().rulesetConnection

	// --- VerifiableDomain (real: store-backed) ------------------------------
	// Reuse the organization surface's memoized VerifiableDomainConnection.
	extras.domainConnection = s.accountConnectionType(
		s.accountSurfaceRegistry(), "VerifiableDomain", s.gqlVerifiableDomainType(), false, nil)

	// --- EnterpriseServerInstallation (no GHES here; empty) -----------------
	serverInstallationType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseServerInstallation",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"createdAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"customerName": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"hostName":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"isConnected":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
	})
	extras.serverInstallationType = serverInstallationType
	extras.serverInstallationConnection = advisoryConnectionType("EnterpriseServerInstallation", serverInstallationType, pageInfo)

	// EnterpriseServerInstallationMembershipConnection: a membership role on each edge.
	membershipEdge := graphql.NewObject(graphql.ObjectConfig{
		Name: "EnterpriseServerInstallationMembershipEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: serverInstallationType},
			"role":   &graphql.Field{Type: graphql.NewNonNull(s.enterpriseUserAccountMembershipRoleEnum())},
		},
	})
	extras.serverInstallationMembershipConn = graphql.NewObject(graphql.ObjectConfig{
		Name: "EnterpriseServerInstallationMembershipConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(membershipEdge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(serverInstallationType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(pageInfo)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	// --- OrganizationInvitation (node of the two invitation connections) ----
	orgInvitationType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "OrganizationInvitation",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"email":     &graphql.Field{Type: graphql.String},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		},
	})
	extras.orgInvitationType = orgInvitationType
	extras.failedInvitationConnection = uniqueUserInvitationConnection(
		"EnterpriseFailedInvitation", orgInvitationType, pageInfo)
	extras.pendingMemberInvitationConnection = uniqueUserInvitationConnection(
		"EnterprisePendingMemberInvitation", orgInvitationType, pageInfo)

	// --- RepositoryInvitation (node of pendingCollaboratorInvitations) ------
	repoInvitationType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "RepositoryInvitation",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"email":     &graphql.Field{Type: graphql.String},
			"invitee":   &graphql.Field{Type: userType},
			"inviter":   &graphql.Field{Type: graphql.NewNonNull(userType)},
			"permalink": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"permission": &graphql.Field{Type: graphql.NewNonNull(
				s.sharedEnum("RepositoryPermission", "ADMIN", "MAINTAIN", "READ", "TRIAGE", "WRITE"))},
			"repository": &graphql.Field{
				Type: s.repositoryInfoInterface(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					repoID, _ := src["repoID"].(int)
					if repo := s.store.GetRepoByID(repoID); repo != nil {
						return repoToGraphQL(s.store, repo), nil
					}
					return nil, nil
				},
			},
		},
	})
	extras.repositoryInvitationConnection = advisoryConnectionType("RepositoryInvitation", repoInvitationType, pageInfo)

	// --- ExternalIdentity (SCIM/SAML provisioning; empty here) --------------
	externalIdentityType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "ExternalIdentity",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"guid": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"user": &graphql.Field{Type: userType},
		},
	})
	extras.externalIdentityType = externalIdentityType
	extras.externalIdentityConnection = advisoryConnectionType("ExternalIdentity", externalIdentityType, pageInfo)

	// --- EnterpriseRepositoryInfo (outside-collaborator edge) ---------------
	enterpriseRepositoryInfoType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseRepositoryInfo",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":            &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"isPrivate":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"name":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"nameWithOwner": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	extras.enterpriseRepositoryInfoConnection = advisoryConnectionType("EnterpriseRepositoryInfo", enterpriseRepositoryInfoType, pageInfo)

	// --- OIDCProvider (external SSO; null here) -----------------------------
	extras.oidcProviderType = graphql.NewObject(graphql.ObjectConfig{
		Name:       "OIDCProvider",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"enterprise":   &graphql.Field{Type: enterpriseType},
			"providerType": &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum("OIDCProviderType", "AAD"))},
			"tenantId":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	// --- AnnouncementBanner (shared with the organization surface) ----------
	extras.announcementBannerType = s.gqlAnnouncementBannerType(dateTime)

	return extras
}

// uniqueUserInvitationConnection builds an enterprise invitation connection
// (OrganizationInvitation nodes) with the extra totalUniqueUserCount member.
func uniqueUserInvitationConnection(name string, nodeType, pageInfo *graphql.Object) *graphql.Object {
	edge := graphql.NewObject(graphql.ObjectConfig{
		Name: name + "Edge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: nodeType},
		},
	})
	return graphql.NewObject(graphql.ObjectConfig{
		Name: name + "Connection",
		Fields: graphql.Fields{
			"edges":                &graphql.Field{Type: graphql.NewList(edge)},
			"nodes":                &graphql.Field{Type: graphql.NewList(nodeType)},
			"pageInfo":             &graphql.Field{Type: graphql.NewNonNull(pageInfo)},
			"totalCount":           &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"totalUniqueUserCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
}

// uniqueUserConnection wraps enterpriseConnection with totalUniqueUserCount.
func uniqueUserConnection(nodes []map[string]interface{}, args map[string]interface{}) map[string]interface{} {
	conn := enterpriseConnection(nodes, args)
	conn["totalUniqueUserCount"] = len(nodes)
	return conn
}

// addEnterpriseExtraFields installs the extra fields on Enterprise,
// EnterpriseOwnerInfo, EnterpriseIdentityProvider, EnterpriseUserAccount.
func (s *Resolver) addEnterpriseExtraFields(
	enterpriseType, ownerInfoType, identityProviderType, userAccountType *graphql.Object,
	extras *enterpriseExtraTypes,
) {
	// ----- Enterprise -------------------------------------------------------

	enterpriseType.AddFieldConfig("announcementBanner", &graphql.Field{
		Type: extras.announcementBannerType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseMember(p, e) {
				return nil, nil
			}
			// Not surfaced: bleephub's announcement model has no creation
			// timestamp, and AnnouncementBanner.createdAt is non-null.
			return nil, nil
		},
	})

	// innersourceVulnerabilities — served empty: bleephub runs no
	// cross-repository innersource scan.
	if vulnConn := s.namedObject("SecurityVulnerabilityConnection"); vulnConn != nil {
		enterpriseType.AddFieldConfig("innersourceVulnerabilities", &graphql.Field{
			Type: graphql.NewNonNull(vulnConn),
			Args: connectionArgs(graphql.FieldConfigArgument{
				"package": &graphql.ArgumentConfig{Type: graphql.String},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				_ = s.enterpriseFromSource(p.Source)
				return paginateGQLItems(nil, p.Args), nil
			},
		})
	}

	enterpriseType.AddFieldConfig("enterpriseTeam", &graphql.Field{
		Type: extras.enterpriseTeamType,
		Args: graphql.FieldConfigArgument{
			"slug": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseMember(p, e) {
				return nil, nil
			}
			slug, _ := p.Args["slug"].(string)
			team := s.store.GetEnterpriseTeam(slug)
			if team == nil {
				return nil, nil
			}
			return s.enterpriseTeamToGraphQL(p, e, team), nil
		},
	})

	enterpriseType.AddFieldConfig("enterpriseTeams", &graphql.Field{
		Type: graphql.NewNonNull(extras.enterpriseTeamConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(s.accountSurfaceRegistry(),
				"EnterpriseTeamOrder", "EnterpriseTeamOrderField", "CREATED_AT", "ID", "NAME")},
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseMember(p, e) {
				return enterpriseConnection(nil, p.Args), nil
			}
			query, _ := p.Args["query"].(string)
			var nodes []map[string]interface{}
			for _, team := range s.store.ListEnterpriseTeams() {
				if query != "" && !strings.Contains(strings.ToLower(team.Name), strings.ToLower(query)) &&
					!strings.Contains(strings.ToLower(team.Slug), strings.ToLower(query)) {
					continue
				}
				nodes = append(nodes, s.enterpriseTeamToGraphQL(p, e, team))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	})

	enterpriseType.AddFieldConfig("repositoryCustomProperties", &graphql.Field{
		Type: extras.repositoryCustomPropertyConnection,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseMember(p, e) {
				return nil, nil
			}
			var nodes []map[string]interface{}
			for _, def := range s.store.ListEnterpriseCustomProperties() {
				nodes = append(nodes, s.enterpriseCustomPropertySource(e, def))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	})

	enterpriseType.AddFieldConfig("repositoryCustomProperty", &graphql.Field{
		Type: extras.repositoryCustomPropertyType,
		Args: graphql.FieldConfigArgument{
			"propertyName": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseMember(p, e) {
				return nil, nil
			}
			name, _ := p.Args["propertyName"].(string)
			def := s.store.GetEnterpriseCustomProperty(name)
			if def == nil {
				return nil, nil
			}
			return s.enterpriseCustomPropertySource(e, def), nil
		},
	})

	enterpriseType.AddFieldConfig("ruleset", &graphql.Field{
		Type: extras.rulesetType,
		Args: graphql.FieldConfigArgument{
			"databaseId": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// Not rendered: the served RuleSource union carries only Organization
			// and Repository, so an enterprise-sourced ruleset has no expressible source.
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseOwner(p, e) {
				return nil, nil
			}
			return nil, nil
		},
	})

	enterpriseType.AddFieldConfig("rulesets", &graphql.Field{
		Type: extras.rulesetConnection,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// See ruleset: no enterprise-source ruleset is expressible, so empty.
			return enterpriseConnection(nil, p.Args), nil
		},
	})

	enterpriseType.AddFieldConfig("userNamespaceRepositories", &graphql.Field{
		Type: graphql.NewNonNull(extras.userNamespaceRepositoryConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlRepositoryOrderInput()},
			"query":   &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// User-namespace repositories exist only under Enterprise Managed Users.
			return enterpriseConnection(nil, p.Args), nil
		},
	})

	// ----- EnterpriseOwnerInfo ---------------------------------------------

	ownerInfoType.AddFieldConfig("domains", &graphql.Field{
		Type: graphql.NewNonNull(extras.domainConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"isApproved": &graphql.ArgumentConfig{Type: graphql.Boolean},
			"isVerified": &graphql.ArgumentConfig{Type: graphql.Boolean},
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(s.accountSurfaceRegistry(),
				"VerifiableDomainOrder", "VerifiableDomainOrderField", "CREATED_AT", "DOMAIN")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			isApproved, hasApproved := p.Args["isApproved"].(bool)
			isVerified, hasVerified := p.Args["isVerified"].(bool)
			var nodes []map[string]interface{}
			for _, d := range s.store.ListVerifiableDomains("Enterprise", e.ID) {
				if hasApproved && d.IsApproved != isApproved {
					continue
				}
				if hasVerified && d.IsVerified != isVerified {
					continue
				}
				nodes = append(nodes, s.verifiableDomainToGQL(d))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	})

	ownerInfoType.AddFieldConfig("enterpriseServerInstallations", &graphql.Field{
		Type: graphql.NewNonNull(extras.serverInstallationConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"connectedOnly": &graphql.ArgumentConfig{Type: graphql.Boolean},
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(s.accountSurfaceRegistry(),
				"EnterpriseServerInstallationOrder", "EnterpriseServerInstallationOrderField",
				"CREATED_AT", "CUSTOMER_NAME", "HOST_NAME")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return enterpriseConnection(nil, p.Args), nil
		},
	})

	ownerInfoType.AddFieldConfig("failedInvitations", &graphql.Field{
		Type: graphql.NewNonNull(extras.failedInvitationConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return uniqueUserConnection(nil, p.Args), nil
		},
	})

	ownerInfoType.AddFieldConfig("oidcProvider", &graphql.Field{
		Type: extras.oidcProviderType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// bleephub authenticates through an external SSO broker, not a
			// GitHub-managed OIDC tenant.
			return nil, nil
		},
	})

	ownerInfoType.AddFieldConfig("pendingCollaboratorInvitations", &graphql.Field{
		Type: graphql.NewNonNull(extras.repositoryInvitationConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(s.accountSurfaceRegistry(),
				"RepositoryInvitationOrder", "RepositoryInvitationOrderField", "CREATED_AT")},
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// Served empty: no enterprise-wide aggregation of per-repository
			// collaborator invitations is modeled.
			return enterpriseConnection(nil, p.Args), nil
		},
	})

	ownerInfoType.AddFieldConfig("pendingMemberInvitations", &graphql.Field{
		Type: graphql.NewNonNull(extras.pendingMemberInvitationConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"invitationSource":   &graphql.ArgumentConfig{Type: s.sharedEnum("OrganizationInvitationSource", "MEMBER", "SCIM", "UNKNOWN")},
			"organizationLogins": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"query":              &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return uniqueUserConnection(nil, p.Args), nil
		},
	})

	// ----- EnterpriseIdentityProvider --------------------------------------

	identityProviderType.AddFieldConfig("externalIdentities", &graphql.Field{
		Type: graphql.NewNonNull(extras.externalIdentityConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"login":       &graphql.ArgumentConfig{Type: graphql.String},
			"membersOnly": &graphql.ArgumentConfig{Type: graphql.Boolean},
			"userName":    &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// SAML external identities are not provisioned on this instance.
			return enterpriseConnection(nil, p.Args), nil
		},
	})

	// ----- EnterpriseUserAccount -------------------------------------------

	userAccountType.AddFieldConfig("enterpriseInstallations", &graphql.Field{
		Type: graphql.NewNonNull(extras.serverInstallationMembershipConn),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlOrderInput(s.accountSurfaceRegistry(),
				"EnterpriseServerInstallationOrder", "EnterpriseServerInstallationOrderField",
				"CREATED_AT", "CUSTOMER_NAME", "HOST_NAME")},
			"query": &graphql.ArgumentConfig{Type: graphql.String},
			"role":  &graphql.ArgumentConfig{Type: s.enterpriseUserAccountMembershipRoleEnum()},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return enterpriseConnection(nil, p.Args), nil
		},
	})
}

// enterpriseTeamToGraphQL renders one enterprise team, baking the per-viewer
// membership and administer flags.
func (s *Resolver) enterpriseTeamToGraphQL(p graphql.ResolveParams, e *store.Enterprise, t *store.EnterpriseTeam) map[string]interface{} {
	isMember := false
	if viewer := s.ghUserFromContext(p.Context); viewer != nil {
		isMember = s.store.IsEnterpriseTeamMember(t, viewer.ID)
	}
	return map[string]interface{}{
		"id":                        "ET_" + enterpriseNodeSuffix(e.ID, t.ID),
		"combinedSlug":              e.Slug + "/" + t.Slug,
		"createdAt":                 t.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":                 t.UpdatedAt.UTC().Format(time.RFC3339),
		"description":               nullableString(t.Description),
		"enterprise":                enterpriseToGraphQL(e),
		"fullDatabaseId":            strconv.Itoa(t.ID),
		"isViewerMember":            isMember,
		"name":                      t.Name,
		"notificationSetting":       strings.ToUpper(t.NotificationSetting),
		"organizationSelectionType": strings.ToUpper(t.OrganizationSelectionType),
		// Constant: the store has no per-team privacy, and enterprise teams are
		// visible within the enterprise.
		"privacy":             "VISIBLE",
		"slug":                t.Slug,
		"viewerCanAdminister": s.viewerIsEnterpriseOwner(p, e),
	}
}
