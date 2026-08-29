package graphqlapi

// The enterprise account read surface: Enterprise and everything reachable
// from it. Authorization is per field against the viewer's role in *this*
// enterprise (store.EffectiveEnterpriseRole); a principal with no role reads
// only the public profile.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func enterpriseToGraphQL(e *store.Enterprise) map[string]interface{} {
	if e == nil {
		return nil
	}
	website := interface{}(nil)
	if e.WebsiteURL != "" {
		website = e.WebsiteURL
	}
	return map[string]interface{}{
		"__typename":           "Enterprise",
		"nodeID":               e.NodeID,
		"_dbID":                e.ID,
		"databaseId":           e.ID,
		"slug":                 e.Slug,
		"name":                 e.Name,
		"description":          nullableString(e.Description),
		"descriptionHTML":      "<div>" + htmlEscapeText(e.Description) + "</div>",
		"location":             nullableString(e.Location),
		"websiteUrl":           website,
		"avatarUrl":            externalURL(e.AvatarURL),
		"billingEmail":         nullableString(e.BillingEmail),
		"securityContactEmail": nullableString(e.SecurityContactEmail),
		"readme":               nullableString(e.Readme),
		"readmeHTML":           "<div>" + htmlEscapeText(e.Readme) + "</div>",
		"url":                  externalURL("/enterprises/" + e.Slug),
		"resourcePath":         "/enterprises/" + e.Slug,
		"createdAt":            e.CreatedAt.Format(time.RFC3339),
		"updatedAt":            e.UpdatedAt.Format(time.RFC3339),
	}
}

func htmlEscapeText(v string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(v)
}

// enterpriseFromSource resolves the live enterprise row behind a source map.
func (s *Resolver) enterpriseFromSource(source interface{}) *store.Enterprise {
	m, ok := source.(map[string]interface{})
	if !ok {
		return nil
	}
	id, _ := m["_dbID"].(int)
	return s.store.GetEnterpriseByID(id)
}

func (s *Resolver) viewerEnterpriseRole(p graphql.ResolveParams, e *store.Enterprise) store.EnterpriseRole {
	if e == nil {
		return ""
	}
	return s.store.EffectiveEnterpriseRole(e.ID, s.ghUserFromContext(p.Context))
}

func (s *Resolver) viewerIsEnterpriseOwner(p graphql.ResolveParams, e *store.Enterprise) bool {
	return s.viewerEnterpriseRole(p, e) == store.EnterpriseRoleOwner
}

func (s *Resolver) viewerIsEnterpriseMember(p graphql.ResolveParams, e *store.Enterprise) bool {
	switch s.viewerEnterpriseRole(p, e) {
	case store.EnterpriseRoleOwner, store.EnterpriseRoleBillingManager, store.EnterpriseRoleMember:
		return true
	}
	return false
}

// enterpriseForbidden is the refusal a non-member gets for enterprise-private
// data. Same answer whether the data is absent or hidden, so it cannot become a
// membership oracle.
func enterpriseForbidden() error {
	return &ghForbiddenError{message: "You must be a member of the enterprise to view this information."}
}

// enums

// sharedEnum memoizes an enum by name; graphql-go rejects two distinct types
// with one name, so any enum more than one family needs must come from here.
func (s *Resolver) sharedEnum(name string, values ...string) *graphql.Enum {
	if s.graphqlTypes.enums == nil {
		s.graphqlTypes.enums = map[string]*graphql.Enum{}
	}
	if existing := s.graphqlTypes.enums[name]; existing != nil {
		return existing
	}
	config := graphql.EnumValueConfigMap{}
	for _, value := range values {
		config[value] = &graphql.EnumValueConfig{Value: value}
	}
	enum := graphql.NewEnum(graphql.EnumConfig{Name: name, Values: config})
	s.graphqlTypes.enums[name] = enum
	return enum
}

func (s *Resolver) enterpriseEnabledDisabledEnum() *graphql.Enum {
	return s.sharedEnum("EnterpriseEnabledDisabledSettingValue", "ENABLED", "DISABLED", "NO_POLICY")
}

func (s *Resolver) enterpriseEnabledEnum() *graphql.Enum {
	return s.sharedEnum("EnterpriseEnabledSettingValue", "ENABLED", "NO_POLICY")
}

func (s *Resolver) enterpriseAdministratorRoleEnum() *graphql.Enum {
	return s.sharedEnum("EnterpriseAdministratorRole", "OWNER", "BILLING_MANAGER", "UNAFFILIATED")
}

func (s *Resolver) enterpriseUserAccountMembershipRoleEnum() *graphql.Enum {
	return s.sharedEnum("EnterpriseUserAccountMembershipRole", "MEMBER", "OWNER", "UNAFFILIATED")
}

func (s *Resolver) enterpriseMembershipTypeEnum() *graphql.Enum {
	return s.sharedEnum("EnterpriseMembershipType", "ADMIN", "ALL", "BILLING_MANAGER", "ORG_MEMBERSHIP")
}

func (s *Resolver) enterpriseUserDeploymentEnum() *graphql.Enum {
	return s.sharedEnum("EnterpriseUserDeployment", "CLOUD", "SERVER")
}

func (s *Resolver) ipAllowListEnabledEnum() *graphql.Enum {
	return s.sharedEnum("IpAllowListEnabledSettingValue", "ENABLED", "DISABLED")
}

func (s *Resolver) ipAllowListForInstalledAppsEnum() *graphql.Enum {
	return s.sharedEnum("IpAllowListForInstalledAppsEnabledSettingValue", "ENABLED", "DISABLED")
}

func (s *Resolver) ipAllowListUserLevelEnforcementEnum() *graphql.Enum {
	return s.sharedEnum("IpAllowListUserLevelEnforcementEnabledSettingValue", "ENABLED", "DISABLED")
}

// shared connection builders

// enterpriseConnection renders a node list as a Relay connection.
func enterpriseConnection(nodes []map[string]interface{}, args map[string]interface{}) map[string]interface{} {
	return paginateGQLMaps(nodes, args)
}

// enterpriseEdgeAndConnectionTypes mints the {Name}Edge/{Name}Connection pair
// for a node type, with any extra edge/connection fields the schema declares.
func (s *Resolver) enterpriseEdgeAndConnectionTypes(connectionName, edgeName string, nodeType graphql.Output, extraEdgeFields graphql.Fields, extraConnectionFields graphql.Fields) *graphql.Object {
	edgeFields := graphql.Fields{
		"node":   &graphql.Field{Type: nodeType},
		"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	}
	for name, field := range extraEdgeFields {
		edgeFields[name] = field
	}
	edgeType := graphql.NewObject(graphql.ObjectConfig{Name: edgeName, Fields: edgeFields})
	connectionFields := graphql.Fields{
		"nodes":      &graphql.Field{Type: graphql.NewList(nodeType)},
		"edges":      &graphql.Field{Type: graphql.NewList(edgeType)},
		"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
	}
	for name, field := range extraConnectionFields {
		connectionFields[name] = field
	}
	return graphql.NewObject(graphql.ObjectConfig{Name: connectionName, Fields: connectionFields})
}

// the enterprise family

// addEnterpriseFieldsToSchema assembles the enterprise read surface onto Query
// and User and returns the Enterprise object.
func (s *Resolver) addEnterpriseFieldsToSchema(userType, orgType, queryType *graphql.Object, nodeInterface *graphql.Interface, actorInterface *graphql.Interface) *graphql.Object {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	html := s.graphQLStringScalar("HTML")
	certificate := s.graphQLStringScalar("X509Certificate")

	enterpriseType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Enterprise",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return sourceValue(p, "nodeID")
				},
			},
			"databaseId":      &graphql.Field{Type: graphql.Int},
			"slug":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description":     &graphql.Field{Type: graphql.String},
			"descriptionHTML": &graphql.Field{Type: graphql.NewNonNull(html)},
			"location":        &graphql.Field{Type: graphql.String},
			"websiteUrl":      &graphql.Field{Type: uri},
			"readme":          &graphql.Field{Type: graphql.String},
			"readmeHTML":      &graphql.Field{Type: graphql.NewNonNull(html)},
			"url":             &graphql.Field{Type: graphql.NewNonNull(uri)},
			"resourcePath":    &graphql.Field{Type: graphql.NewNonNull(uri)},
			"createdAt":       &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":       &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"avatarUrl": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int}},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return sourceValue(p, "avatarUrl")
				},
			},
			// billingEmail and securityContactEmail are administrative
			// contacts: only owners and billing managers may read them.
			"billingEmail": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					e := s.enterpriseFromSource(p.Source)
					switch s.viewerEnterpriseRole(p, e) {
					case store.EnterpriseRoleOwner, store.EnterpriseRoleBillingManager:
						return nullableString(e.BillingEmail), nil
					}
					return nil, nil
				},
			},
			"securityContactEmail": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					e := s.enterpriseFromSource(p.Source)
					if !s.viewerIsEnterpriseMember(p, e) {
						return nil, nil
					}
					return nullableString(e.SecurityContactEmail), nil
				},
			},
			"viewerIsAdmin": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return s.viewerIsEnterpriseOwner(p, s.enterpriseFromSource(p.Source)), nil
				},
			},
		},
	})
	s.graphqlTypes.enterprise = enterpriseType

	// Build before the outside-collaborator edge, whose repositories field
	// names EnterpriseRepositoryInfoConnection.
	extras := s.buildEnterpriseExtraTypes(enterpriseType, userType, nodeInterface)

	enterpriseUserAccountType := s.addEnterpriseUserAccountType(enterpriseType, userType, orgType, nodeInterface, actorInterface, dateTime, uri)
	memberUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "EnterpriseMember",
		Types: []*graphql.Object{enterpriseUserAccountType, userType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if source, ok := p.Value.(map[string]interface{}); ok {
				if name, _ := source["__typename"].(string); name == "User" {
					return userType
				}
			}
			return enterpriseUserAccountType
		},
	})
	memberConnectionType := s.enterpriseEdgeAndConnectionTypes("EnterpriseMemberConnection", "EnterpriseMemberEdge", memberUnion, nil, nil)

	adminConnectionType := s.enterpriseEdgeAndConnectionTypes("EnterpriseAdministratorConnection", "EnterpriseAdministratorEdge", userType,
		graphql.Fields{"role": &graphql.Field{
			Type: graphql.NewNonNull(s.enterpriseAdministratorRoleEnum()),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return edgeValue(p, "role")
			},
		}}, nil)

	outsideCollaboratorConnectionType := s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseOutsideCollaboratorConnection", "EnterpriseOutsideCollaboratorEdge", userType,
		graphql.Fields{"repositories": &graphql.Field{
			Type: graphql.NewNonNull(extras.enterpriseRepositoryInfoConnection),
			Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
				"orderBy": &graphql.ArgumentConfig{Type: s.gqlRepositoryOrderInput()},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				// Not aggregated on this instance.
				return enterpriseConnection(nil, p.Args), nil
			},
		}}, nil)

	adminInvitationType, adminInvitationConnectionType := s.addEnterpriseAdminInvitationTypes(enterpriseType, userType, nodeInterface, dateTime)
	memberInvitationType, memberInvitationConnectionType := s.addEnterpriseMemberInvitationTypes(enterpriseType, userType, nodeInterface, dateTime)
	identityProviderType := s.addEnterpriseIdentityProviderType(enterpriseType, nodeInterface, uri, certificate)
	ipAllowListEntryType, ipAllowListConnectionType := s.addIPAllowListTypes(enterpriseType, orgType, nodeInterface, dateTime)
	billingInfoType := s.addEnterpriseBillingInfoType()
	ownerInfoType := s.addEnterpriseOwnerInfoType(enterpriseOwnerInfoDeps{
		userType:                    userType,
		adminConnection:             adminConnectionType,
		outsideCollaboratorConn:     outsideCollaboratorConnectionType,
		memberConnection:            memberConnectionType,
		adminInvitationConnection:   adminInvitationConnectionType,
		memberInvitationConnection:  memberInvitationConnectionType,
		identityProvider:            identityProviderType,
		ipAllowListEntryConnection:  ipAllowListConnectionType,
		organizationConnectionType:  s.graphqlTypes.organizationConnection,
		userConnectionTypeForTwoFA:  s.gqlUserConnectionType(userType),
		organizationMembershipConnT: s.addEnterpriseOrganizationMembershipConnection(orgType),
	})

	organizationConnectionType := s.graphqlTypes.organizationConnection
	enterpriseType.AddFieldConfig("organizations", &graphql.Field{
		Type: graphql.NewNonNull(organizationConnectionType),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseMember(p, e) {
				return nil, enterpriseForbidden()
			}
			return enterpriseConnection(s.enterpriseOrganizationNodes(e), p.Args), nil
		},
	})
	enterpriseType.AddFieldConfig("members", &graphql.Field{
		Type: graphql.NewNonNull(memberConnectionType),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"deployment":         &graphql.ArgumentConfig{Type: s.enterpriseUserDeploymentEnum()},
			"organizationLogins": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"query":              &graphql.ArgumentConfig{Type: graphql.String},
			"role":               &graphql.ArgumentConfig{Type: s.enterpriseUserAccountMembershipRoleEnum()},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseMember(p, e) {
				return nil, enterpriseForbidden()
			}
			return enterpriseConnection(s.enterpriseMemberNodes(e, p.Args), p.Args), nil
		},
	})
	enterpriseType.AddFieldConfig("billingInfo", &graphql.Field{
		Type: billingInfoType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			switch s.viewerEnterpriseRole(p, e) {
			case store.EnterpriseRoleOwner, store.EnterpriseRoleBillingManager:
				return optionalObject(s.enterpriseBillingInfo(e)), nil
			}
			return nil, nil
		},
	})
	enterpriseType.AddFieldConfig("ownerInfo", &graphql.Field{
		Type: ownerInfoType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if !s.viewerIsEnterpriseOwner(p, e) {
				return nil, nil
			}
			return optionalObject(enterpriseToGraphQL(e)), nil
		},
	})

	// User.enterprises
	enterpriseConnectionType := s.enterpriseEdgeAndConnectionTypes("EnterpriseConnection", "EnterpriseEdge", enterpriseType, nil, nil)
	userType.AddFieldConfig("enterprises", &graphql.Field{
		Type: enterpriseConnectionType,
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"membershipType": &graphql.ArgumentConfig{Type: s.enterpriseMembershipTypeEnum(), DefaultValue: "ALL"},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, _ := p.Source.(map[string]interface{})
			dbID, _ := source["databaseId"].(int)
			viewer := s.ghUserFromContext(p.Context)
			// Only the account itself sees its enterprise memberships.
			if viewer == nil || viewer.ID != dbID {
				return enterpriseConnection(nil, p.Args), nil
			}
			membershipType, _ := p.Args["membershipType"].(string)
			var nodes []map[string]interface{}
			for _, e := range s.store.ListEnterprisesForUser(viewer.ID) {
				if !enterpriseMembershipTypeMatches(membershipType, s.store.EffectiveEnterpriseRole(e.ID, viewer)) {
					continue
				}
				nodes = append(nodes, enterpriseToGraphQL(e))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	})

	queryType.AddFieldConfig("enterprise", &graphql.Field{
		Type: enterpriseType,
		Args: graphql.FieldConfigArgument{
			"slug":            &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"invitationToken": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			slug, _ := p.Args["slug"].(string)
			e := s.store.GetEnterprise(slug)
			if e == nil {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to an Enterprise with the slug of '%s'.", slug),
				}
			}
			return enterpriseToGraphQL(e), nil
		},
	})
	queryType.AddFieldConfig("enterpriseAdministratorInvitation", &graphql.Field{
		Type: adminInvitationType,
		Args: graphql.FieldConfigArgument{
			"enterpriseSlug": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"role":           &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.enterpriseAdministratorRoleEnum())},
			"userLogin":      &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			slug, _ := p.Args["enterpriseSlug"].(string)
			role, _ := p.Args["role"].(string)
			login, _ := p.Args["userLogin"].(string)
			return s.lookupEnterpriseInvitation(p, slug, login, "admin", role), nil
		},
	})
	queryType.AddFieldConfig("enterpriseMemberInvitation", &graphql.Field{
		Type: memberInvitationType,
		Args: graphql.FieldConfigArgument{
			"enterpriseSlug": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"userLogin":      &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			slug, _ := p.Args["enterpriseSlug"].(string)
			login, _ := p.Args["userLogin"].(string)
			return s.lookupEnterpriseInvitation(p, slug, login, "member", ""), nil
		},
	})

	// bleephub addresses invitations by invitee, not by an opaque token, so
	// these by-token lookups never resolve.
	queryType.AddFieldConfig("enterpriseAdministratorInvitationByToken", &graphql.Field{
		Type: adminInvitationType,
		Args: graphql.FieldConfigArgument{
			"invitationToken": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return nil, nil
		},
	})
	queryType.AddFieldConfig("enterpriseMemberInvitationByToken", &graphql.Field{
		Type: memberInvitationType,
		Args: graphql.FieldConfigArgument{
			"invitationToken": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return nil, nil
		},
	})

	s.addEnterpriseExtraFields(enterpriseType, ownerInfoType, identityProviderType, enterpriseUserAccountType, extras)

	// Second pass: identity/team/invitation completion fields that name types
	// built above and cross-family types (Organization, User, ...).
	s.addEnterpriseIdentityCompletionFields(orgType, userType, nodeInterface, certificate, extras)

	s.graphqlTypes.enterpriseUserAccount = enterpriseUserAccountType
	s.graphqlTypes.enterpriseAdminInvitation = adminInvitationType
	s.graphqlTypes.enterpriseMemberInvite = memberInvitationType
	s.graphqlTypes.enterpriseIdentityProvide = identityProviderType
	s.graphqlTypes.ipAllowListEntry = ipAllowListEntryType
	return enterpriseType
}

// enterpriseMembershipTypeMatches applies User.enterprises(membershipType:).
func enterpriseMembershipTypeMatches(membershipType string, role store.EnterpriseRole) bool {
	switch membershipType {
	case "", "ALL":
		return true
	case "ADMIN":
		return role == store.EnterpriseRoleOwner
	case "BILLING_MANAGER":
		return role == store.EnterpriseRoleBillingManager
	case "ORG_MEMBERSHIP":
		return role == store.EnterpriseRoleMember
	}
	return true
}

// lookupEnterpriseInvitation answers the two invitation root fields. An
// invitation is visible only to its invitee and the enterprise's owners; to
// anyone else it does not exist.
func (s *Resolver) lookupEnterpriseInvitation(p graphql.ResolveParams, slug, userLogin, kind, role string) interface{} {
	e := s.store.GetEnterprise(slug)
	if e == nil {
		return nil
	}
	invitee := s.store.LookupUserByLogin(userLogin)
	if invitee == nil {
		return nil
	}
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil {
		return nil
	}
	if viewer.ID != invitee.ID && !s.store.IsEnterpriseOwner(e.ID, viewer) {
		return nil
	}
	for _, inv := range s.store.ListEnterpriseInvitations(e.ID, kind) {
		if inv.InviteeID != invitee.ID {
			continue
		}
		if role != "" && string(inv.Role) != role {
			continue
		}
		return s.enterpriseInvitationToGraphQL(inv)
	}
	return nil
}

// member / organization node lists

// enterpriseOrganizationNodes renders the organizations an enterprise owns.
func (s *Resolver) enterpriseOrganizationNodes(e *store.Enterprise) []map[string]interface{} {
	if e == nil {
		return nil
	}
	var nodes []map[string]interface{}
	for _, orgID := range s.store.ListEnterpriseOrgIDs(e.ID) {
		if org := s.store.GetOrgByID(orgID); org != nil {
			nodes = append(nodes, orgToGraphQL(org))
		}
	}
	return nodes
}

// enterpriseMemberUsers returns an enterprise's members paired with each one's
// role, ordered by login.
func (s *Resolver) enterpriseMemberUsers(e *store.Enterprise) ([]*store.User, map[int]store.EnterpriseRole) {
	roles := map[int]store.EnterpriseRole{}
	if e == nil {
		return nil, roles
	}
	seen := map[int]bool{}
	var users []*store.User
	add := func(user *store.User) {
		if user == nil || seen[user.ID] {
			return
		}
		role := s.store.EffectiveEnterpriseRole(e.ID, user)
		if role == "" {
			return
		}
		seen[user.ID] = true
		roles[user.ID] = role
		users = append(users, user)
	}
	for _, m := range s.store.ListEnterpriseMemberships(e.ID) {
		add(s.store.GetUserByID(m.UserID))
	}
	for _, orgID := range s.store.ListEnterpriseOrgIDs(e.ID) {
		org := s.store.GetOrgByID(orgID)
		if org == nil {
			continue
		}
		for _, user := range s.store.ListOrgMembers(org.Login) {
			add(user)
		}
	}
	if s.store.PrimaryEnterpriseSlug() == e.Slug {
		for _, user := range s.store.ListUsers() {
			add(user)
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Login < users[j].Login })
	return users, roles
}

// enterpriseMemberNodes renders Enterprise.members, applying the query,
// organizationLogins, role and deployment filters GitHub declares.
func (s *Resolver) enterpriseMemberNodes(e *store.Enterprise, args map[string]interface{}) []map[string]interface{} {
	users, roles := s.enterpriseMemberUsers(e)
	query, _ := args["query"].(string)
	roleFilter, _ := args["role"].(string)
	deployment, _ := args["deployment"].(string)
	// bleephub is one (server) deployment: SERVER matches every member,
	// CLOUD matches none.
	if deployment == "CLOUD" {
		return nil
	}
	orgLogins := map[string]bool{}
	if raw, ok := args["organizationLogins"].([]interface{}); ok {
		for _, value := range raw {
			if login, ok := value.(string); ok {
				orgLogins[strings.ToLower(login)] = true
			}
		}
	}
	var nodes []map[string]interface{}
	for _, user := range users {
		if query != "" && !strings.Contains(strings.ToLower(user.Login+" "+user.Name), strings.ToLower(query)) {
			continue
		}
		if roleFilter != "" && string(roles[user.ID]) != roleFilter {
			continue
		}
		if len(orgLogins) > 0 && !s.userIsInAnyEnterpriseOrg(e, user, orgLogins) {
			continue
		}
		nodes = append(nodes, s.enterpriseUserAccountToGraphQL(e, user, roles[user.ID]))
	}
	return nodes
}

func (s *Resolver) userIsInAnyEnterpriseOrg(e *store.Enterprise, user *store.User, logins map[string]bool) bool {
	for _, orgID := range s.store.ListEnterpriseOrgIDs(e.ID) {
		org := s.store.GetOrgByID(orgID)
		if org == nil || !logins[strings.ToLower(org.Login)] {
			continue
		}
		if m := s.store.GetMembership(org.Login, user.ID); m != nil && m.State == store.MembershipStateActive {
			return true
		}
	}
	return false
}

// helpers

// sourceValue reads a key from a map-shaped resolver source.
func sourceValue(p graphql.ResolveParams, key string) (interface{}, error) {
	source, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
	}
	return source[key], nil
}

// edgeValue reads a key from a connection edge's source map. Edges are built as
// {"node", "cursor"}, so an extra edge field is carried on the node and read
// back through here.
func edgeValue(p graphql.ResolveParams, key string) (interface{}, error) {
	edge, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve edge: unexpected type %T", p.Source)
	}
	if value, present := edge[key]; present {
		return value, nil
	}
	if node, ok := edge["node"].(map[string]interface{}); ok {
		return node[key], nil
	}
	return nil, nil
}

// mergeArgs combines argument maps left to right.
func mergeArgs(maps ...graphql.FieldConfigArgument) graphql.FieldConfigArgument {
	out := graphql.FieldConfigArgument{}
	for _, m := range maps {
		for name, arg := range m {
			out[name] = arg
		}
	}
	return out
}

// enterprisePolicyRefusal refuses when the named setting is DISABLED by the
// policy governing a repository. Enterprise owners are exempt, as in REST.
func (s *Resolver) enterprisePolicyRefusal(p graphql.ResolveParams, repo *store.Repo, setting func(store.EnterprisePolicy) string, message string) error {
	policy, enterprise := s.store.EnterprisePolicyForRepo(repo)
	if s.store.EnterprisePolicyForbids(enterprise, setting(policy), s.ghUserFromContext(p.Context)) {
		return &ghForbiddenError{message: message}
	}
	return nil
}

// enterpriseOrganizationProjectsRefusal refuses creating a project under an
// organization whose enterprise has disabled organization projects. A
// personal-account project is not an organization project, so the policy is
// silent on it.
func (s *Resolver) enterpriseOrganizationProjectsRefusal(p graphql.ResolveParams, ownerType string, ownerID int) error {
	if ownerType != "Organization" {
		return nil
	}
	policy, enterprise := s.store.EnterprisePolicyForOrg(ownerID)
	if s.store.EnterprisePolicyForbids(enterprise, policy.OrganizationProjects, s.ghUserFromContext(p.Context)) {
		return &ghForbiddenError{message: "Organization projects are disabled by an enterprise policy."}
	}
	return nil
}

// enterpriseNodeByID resolves the enterprise family's Node implementors for
// Query.node/nodes, applying each one's visibility rule: an enterprise profile
// is public, an invitation is its invitee's and owners' to see, an IP allow
// list entry is its owner's.
func (s *Resolver) enterpriseNodeByID(ctx context.Context, nodeID string) interface{} {
	viewer := s.ghUserFromContext(ctx)
	if e := store.FindEnterpriseByNodeID(s.store, nodeID); e != nil {
		return enterpriseToGraphQL(s.store.GetEnterpriseByID(e.ID))
	}
	if inv := store.FindEnterpriseInvitationByNodeID(s.store, nodeID); inv != nil {
		if viewer == nil {
			return nil
		}
		if viewer.ID != inv.InviteeID && !s.store.IsEnterpriseOwner(inv.EnterpriseID, viewer) {
			return nil
		}
		return s.enterpriseInvitationToGraphQL(s.store.GetEnterpriseInvitation(inv.ID))
	}
	if entry := store.FindIPAllowListEntryByNodeID(s.store, nodeID); entry != nil {
		if !s.viewerAdministersIPAllowListOwner(viewer, entry.OwnerType, entry.OwnerID) {
			return nil
		}
		return ipAllowListEntryToGraphQL(s.store.ListIPAllowListEntryByID(entry.ID))
	}
	return nil
}

// viewerAdministersIPAllowListOwner reports whether the viewer administers the
// entry's owning enterprise or organization.
func (s *Resolver) viewerAdministersIPAllowListOwner(viewer *store.User, ownerType string, ownerID int) bool {
	if viewer == nil {
		return false
	}
	if ownerType == store.IPAllowListOwnerEnterprise {
		return s.store.IsEnterpriseOwner(ownerID, viewer)
	}
	if ownerType != store.IPAllowListOwnerOrganization {
		// A user's own allow list is not an IpAllowListOwner and has no global
		// id, so no GraphQL read resolves to one.
		return false
	}
	org := s.store.GetOrgByID(ownerID)
	if org == nil {
		return false
	}
	m := s.store.GetMembership(org.Login, viewer.ID)
	return m != nil && m.State == store.MembershipStateActive && m.Role == store.OrgRoleAdmin
}
