package graphqlapi

// The enterprise account family's supporting types: EnterpriseUserAccount,
// the two invitation types, EnterpriseIdentityProvider, the IP allow list,
// EnterpriseBillingInfo and the owner-only EnterpriseOwnerInfo.

import (
	"sort"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// EnterpriseUserAccount

// enterpriseUserAccountToGraphQL renders one user's membership in one
// enterprise. The node id carries the membership, not the user, so the same
// person in two enterprises is two distinct accounts.
func (s *Resolver) enterpriseUserAccountToGraphQL(e *store.Enterprise, user *store.User, role store.EnterpriseRole) map[string]interface{} {
	if e == nil || user == nil {
		return nil
	}
	membershipID := user.ID
	if m := s.store.GetEnterpriseMembership(e.ID, user.ID); m != nil {
		membershipID = m.ID
	}
	createdAt := user.CreatedAt
	if m := s.store.GetEnterpriseMembership(e.ID, user.ID); m != nil {
		createdAt = m.CreatedAt
	}
	return map[string]interface{}{
		"__typename":   "EnterpriseUserAccount",
		"nodeID":       store.EnterpriseUserAccountNodeIDPrefix + enterpriseNodeSuffix(e.ID, membershipID),
		"_dbID":        user.ID,
		"_enterprise":  e.ID,
		"login":        user.Login,
		"name":         nullableString(user.Name),
		"role":         string(role),
		"avatarUrl":    user.AvatarURL,
		"url":          externalURL("/enterprises/" + e.Slug + "/people/" + user.Login),
		"resourcePath": "/enterprises/" + e.Slug + "/people/" + user.Login,
		"createdAt":    createdAt.Format(time.RFC3339),
		"updatedAt":    user.UpdatedAt.Format(time.RFC3339),
	}
}

// enterpriseNodeSuffix packs an enterprise id and a row id into the node-id
// digits so two enterprises never mint the same EnterpriseUserAccount id.
func enterpriseNodeSuffix(enterpriseID, rowID int) string {
	return padNodeDigits(enterpriseID) + padNodeDigits(rowID)
}

func padNodeDigits(n int) string {
	digits := []byte("00000000")
	for i := len(digits) - 1; i >= 0 && n > 0; i-- {
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits)
}

func (s *Resolver) addEnterpriseUserAccountType(enterpriseType, userType, orgType *graphql.Object, nodeInterface *graphql.Interface, actorInterface *graphql.Interface, dateTime, uri *graphql.Scalar) *graphql.Object {
	orgMembershipConnection := s.addEnterpriseOrganizationMembershipConnection(orgType)
	accountType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseUserAccount",
		Interfaces: []*graphql.Interface{nodeInterface, actorInterface},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) { return sourceValue(p, "nodeID") },
			},
			"login": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":  &graphql.Field{Type: graphql.String},
			"avatarUrl": &graphql.Field{
				Type:    graphql.NewNonNull(uri),
				Args:    graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int}},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) { return sourceValue(p, "avatarUrl") },
			},
			"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
			"resourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"createdAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"enterprise": &graphql.Field{
				Type: graphql.NewNonNull(enterpriseType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, _ := p.Source.(map[string]interface{})
					id, _ := source["_enterprise"].(int)
					return optionalObject(enterpriseToGraphQL(s.store.GetEnterpriseByID(id))), nil
				},
			},
			"user": &graphql.Field{
				Type: userType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, _ := p.Source.(map[string]interface{})
					id, _ := source["_dbID"].(int)
					user := s.store.GetUserByID(id)
					if user == nil {
						return nil, nil
					}
					return userToGraphQL(user), nil
				},
			},
			"organizations": &graphql.Field{
				Type: graphql.NewNonNull(orgMembershipConnection),
				Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
					"query": &graphql.ArgumentConfig{Type: graphql.String},
					"role":  &graphql.ArgumentConfig{Type: s.enterpriseUserAccountMembershipRoleEnum()},
				}),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, _ := p.Source.(map[string]interface{})
					enterpriseID, _ := source["_enterprise"].(int)
					userID, _ := source["_dbID"].(int)
					e := s.store.GetEnterpriseByID(enterpriseID)
					viewer := s.ghUserFromContext(p.Context)
					if e == nil || !s.store.IsEnterpriseMember(e.ID, viewer) {
						return nil, enterpriseForbidden()
					}
					roleFilter, _ := p.Args["role"].(string)
					query, _ := p.Args["query"].(string)
					var nodes []map[string]interface{}
					for _, orgID := range s.store.ListEnterpriseOrgIDs(e.ID) {
						org := s.store.GetOrgByID(orgID)
						if org == nil {
							continue
						}
						m := s.store.GetMembership(org.Login, userID)
						if m == nil || m.State != store.MembershipStateActive {
							continue
						}
						role := "MEMBER"
						if m.Role == store.OrgRoleAdmin {
							role = "OWNER"
						}
						if roleFilter != "" && roleFilter != role {
							continue
						}
						if query != "" && !strings.Contains(strings.ToLower(org.Login), strings.ToLower(query)) {
							continue
						}
						node := orgToGraphQL(org)
						node["role"] = role
						nodes = append(nodes, node)
					}
					return enterpriseConnection(nodes, p.Args), nil
				},
			},
		},
	})
	return accountType
}

// addEnterpriseOrganizationMembershipConnection mints the connection whose
// edges carry the member's role in each organization (memoized).
func (s *Resolver) addEnterpriseOrganizationMembershipConnection(orgType *graphql.Object) *graphql.Object {
	if s.enterpriseOrgMembershipConnMemo != nil {
		return s.enterpriseOrgMembershipConnMemo
	}
	s.enterpriseOrgMembershipConnMemo = s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseOrganizationMembershipConnection", "EnterpriseOrganizationMembershipEdge", orgType,
		graphql.Fields{"role": &graphql.Field{
			Type:    graphql.NewNonNull(s.enterpriseUserAccountMembershipRoleEnum()),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) { return edgeValue(p, "role") },
		}}, nil)
	return s.enterpriseOrgMembershipConnMemo
}

// invitations

// enterpriseInvitationToGraphQL renders an admin or member invitation.
func (s *Resolver) enterpriseInvitationToGraphQL(inv *store.EnterpriseInvitation) map[string]interface{} {
	if inv == nil {
		return nil
	}
	typeName := "EnterpriseMemberInvitation"
	if inv.Kind == "admin" {
		typeName = "EnterpriseAdministratorInvitation"
	}
	return map[string]interface{}{
		"__typename":   typeName,
		"nodeID":       inv.NodeID,
		"_dbID":        inv.ID,
		"_enterprise":  inv.EnterpriseID,
		"_inviterID":   inv.InviterID,
		"_inviteeID":   inv.InviteeID,
		"email":        nullableString(inv.Email),
		"role":         string(inv.Role),
		"createdAt":    inv.CreatedAt.Format(time.RFC3339),
		"resourcePath": "/enterprises/invitations/" + inv.NodeID,
	}
}

func (s *Resolver) enterpriseInvitationFields(enterpriseType, userType *graphql.Object, dateTime *graphql.Scalar) graphql.Fields {
	return graphql.Fields{
		"id": &graphql.Field{
			Type:    graphql.NewNonNull(graphql.ID),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) { return sourceValue(p, "nodeID") },
		},
		"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		"email":     &graphql.Field{Type: graphql.String},
		"enterprise": &graphql.Field{
			Type: graphql.NewNonNull(enterpriseType),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				source, _ := p.Source.(map[string]interface{})
				id, _ := source["_enterprise"].(int)
				return optionalObject(enterpriseToGraphQL(s.store.GetEnterpriseByID(id))), nil
			},
		},
		"invitee": &graphql.Field{
			Type: userType,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				source, _ := p.Source.(map[string]interface{})
				id, _ := source["_inviteeID"].(int)
				user := s.store.GetUserByID(id)
				if user == nil {
					return nil, nil
				}
				return userToGraphQL(user), nil
			},
		},
		"inviter": &graphql.Field{
			Type: userType,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				source, _ := p.Source.(map[string]interface{})
				id, _ := source["_inviterID"].(int)
				user := s.store.GetUserByID(id)
				if user == nil {
					return nil, nil
				}
				return userToGraphQL(user), nil
			},
		},
	}
}

func (s *Resolver) addEnterpriseAdminInvitationTypes(enterpriseType, userType *graphql.Object, nodeInterface *graphql.Interface, dateTime *graphql.Scalar) (*graphql.Object, *graphql.Object) {
	fields := s.enterpriseInvitationFields(enterpriseType, userType, dateTime)
	fields["role"] = &graphql.Field{Type: graphql.NewNonNull(s.enterpriseAdministratorRoleEnum())}
	invitationType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseAdministratorInvitation",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields:     fields,
	})
	connectionType := s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseAdministratorInvitationConnection", "EnterpriseAdministratorInvitationEdge", invitationType, nil, nil)
	return invitationType, connectionType
}

func (s *Resolver) addEnterpriseMemberInvitationTypes(enterpriseType, userType *graphql.Object, nodeInterface *graphql.Interface, dateTime *graphql.Scalar) (*graphql.Object, *graphql.Object) {
	invitationType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseMemberInvitation",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields:     s.enterpriseInvitationFields(enterpriseType, userType, dateTime),
	})
	connectionType := s.enterpriseEdgeAndConnectionTypes(
		"EnterpriseMemberInvitationConnection", "EnterpriseMemberInvitationEdge", invitationType, nil, nil)
	return invitationType, connectionType
}

// identity provider

// enterpriseIdentityProviderToGraphQL renders an enterprise's SAML binding.
// recoveryCodes are secrets; the caller passes redacted = true to withhold them.
func enterpriseIdentityProviderToGraphQL(e *store.Enterprise, redacted bool) map[string]interface{} {
	if e == nil || e.IdentityProvider == nil {
		return nil
	}
	idp := e.IdentityProvider
	var codes interface{}
	if !redacted && len(idp.RecoveryCodes) > 0 {
		values := make([]interface{}, len(idp.RecoveryCodes))
		for i, code := range idp.RecoveryCodes {
			values[i] = code
		}
		codes = values
	}
	return map[string]interface{}{
		"__typename":      "EnterpriseIdentityProvider",
		"nodeID":          idp.NodeID,
		"_enterprise":     e.ID,
		"ssoUrl":          nullableString(idp.SSOURL),
		"issuer":          nullableString(idp.Issuer),
		"idpCertificate":  nullableString(idp.IDPCertificate),
		"signatureMethod": nullableString(idp.SignatureMethod),
		"digestMethod":    nullableString(idp.DigestMethod),
		"recoveryCodes":   codes,
	}
}

func (s *Resolver) addEnterpriseIdentityProviderType(enterpriseType *graphql.Object, nodeInterface *graphql.Interface, uri, certificate *graphql.Scalar) *graphql.Object {
	return graphql.NewObject(graphql.ObjectConfig{
		Name:       "EnterpriseIdentityProvider",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) { return sourceValue(p, "nodeID") },
			},
			"ssoUrl":          &graphql.Field{Type: uri},
			"issuer":          &graphql.Field{Type: graphql.String},
			"idpCertificate":  &graphql.Field{Type: certificate},
			"recoveryCodes":   &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"signatureMethod": &graphql.Field{Type: s.sharedEnum("SamlSignatureAlgorithm", "RSA_SHA1", "RSA_SHA256", "RSA_SHA384", "RSA_SHA512")},
			"digestMethod":    &graphql.Field{Type: s.sharedEnum("SamlDigestAlgorithm", "SHA1", "SHA256", "SHA384", "SHA512")},
			"enterprise": &graphql.Field{
				Type: enterpriseType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, _ := p.Source.(map[string]interface{})
					id, _ := source["_enterprise"].(int)
					return optionalObject(enterpriseToGraphQL(s.store.GetEnterpriseByID(id))), nil
				},
			},
		},
	})
}

// IP allow list

func ipAllowListEntryToGraphQL(entry *store.IPAllowListEntry) map[string]interface{} {
	if entry == nil {
		return nil
	}
	return map[string]interface{}{
		"__typename":     "IpAllowListEntry",
		"nodeID":         entry.NodeID,
		"_dbID":          entry.ID,
		"_ownerType":     entry.OwnerType,
		"_ownerID":       entry.OwnerID,
		"allowListValue": entry.AllowListValue,
		"name":           nullableString(entry.Name),
		"isActive":       entry.IsActive,
		"createdAt":      entry.CreatedAt.Format(time.RFC3339),
		"updatedAt":      entry.UpdatedAt.Format(time.RFC3339),
	}
}

func (s *Resolver) addIPAllowListTypes(enterpriseType, orgType *graphql.Object, nodeInterface *graphql.Interface, dateTime *graphql.Scalar) (*graphql.Object, *graphql.Object) {
	ownerUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "IpAllowListOwner",
		Types: []*graphql.Object{enterpriseType, orgType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if source, ok := p.Value.(map[string]interface{}); ok {
				if name, _ := source["__typename"].(string); name == "Organization" {
					return orgType
				}
			}
			return enterpriseType
		},
	})
	s.graphqlTypes.ipAllowListOwner = ownerUnion
	entryType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "IpAllowListEntry",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) { return sourceValue(p, "nodeID") },
			},
			"allowListValue": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":           &graphql.Field{Type: graphql.String},
			"isActive":       &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"createdAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"owner": &graphql.Field{
				Type: graphql.NewNonNull(ownerUnion),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, _ := p.Source.(map[string]interface{})
					ownerType, _ := source["_ownerType"].(string)
					ownerID, _ := source["_ownerID"].(int)
					if ownerType == "Organization" {
						org := s.store.GetOrgByID(ownerID)
						if org == nil {
							return nil, nil
						}
						node := orgToGraphQL(org)
						node["__typename"] = "Organization"
						return node, nil
					}
					return optionalObject(enterpriseToGraphQL(s.store.GetEnterpriseByID(ownerID))), nil
				},
			},
		},
	})
	connectionType := s.enterpriseEdgeAndConnectionTypes("IpAllowListEntryConnection", "IpAllowListEntryEdge", entryType, nil, nil)
	// Organization.ipAllowListEntries reuses this connection type; the org
	// surface is assembled later, so record it rather than re-mint it.
	s.accountSurfaceRegistry().ipAllowListEntryConnection = connectionType
	return entryType, connectionType
}

// billing

// enterpriseBillingInfo measures the enterprise against its provisioned
// entitlements, counting usage from what the instance actually stores.
func (s *Resolver) enterpriseBillingInfo(e *store.Enterprise) map[string]interface{} {
	if e == nil {
		return nil
	}
	licensable := s.enterpriseLicensableUserCount(e)
	storageUsage := s.enterpriseStorageUsageGB(e)
	bandwidthUsage := s.enterpriseBandwidthUsageGB(e)
	available := e.BillingTotalLicenses - licensable
	if available < 0 {
		available = 0
	}
	return map[string]interface{}{
		"allLicensableUsersCount":  licensable,
		"assetPacks":               0,
		"bandwidthQuota":           e.BillingBandwidthQuotaGB,
		"bandwidthUsage":           bandwidthUsage,
		"bandwidthUsagePercentage": percentageOf(bandwidthUsage, e.BillingBandwidthQuotaGB),
		"storageQuota":             e.BillingStorageQuotaGB,
		"storageUsage":             storageUsage,
		"storageUsagePercentage":   percentageOf(storageUsage, e.BillingStorageQuotaGB),
		"totalAvailableLicenses":   available,
		"totalLicenses":            e.BillingTotalLicenses,
	}
}

func percentageOf(usage, quota float64) int {
	if quota <= 0 {
		return 0
	}
	return int(usage / quota * 100)
}

// enterpriseLicensableUserCount counts the enterprise's members, excluding
// suspended accounts.
func (s *Resolver) enterpriseLicensableUserCount(e *store.Enterprise) int {
	users, _ := s.enterpriseMemberUsers(e)
	count := 0
	for _, user := range users {
		if !user.Suspended {
			count++
		}
	}
	return count
}

// enterpriseStorageUsageGB reports the release assets and packages the
// enterprise's organizations hold, in gigabytes.
func (s *Resolver) enterpriseStorageUsageGB(e *store.Enterprise) float64 {
	return bytesToGB(s.store.EnterpriseStorageBytes(enterpriseOrgIDSet(s, e)))
}

// enterpriseBandwidthUsageGB reports the release-asset download egress the
// enterprise's organizations served, in gigabytes.
func (s *Resolver) enterpriseBandwidthUsageGB(e *store.Enterprise) float64 {
	return bytesToGB(s.store.EnterpriseBandwidthBytes(enterpriseOrgIDSet(s, e)))
}

func enterpriseOrgIDSet(s *Resolver, e *store.Enterprise) map[int]bool {
	if e == nil {
		return nil
	}
	set := map[int]bool{}
	for _, orgID := range s.store.ListEnterpriseOrgIDs(e.ID) {
		set[orgID] = true
	}
	if s.store.PrimaryEnterpriseSlug() == e.Slug {
		// The primary enterprise owns every organization no other enterprise
		// has claimed, the way a GHES enterprise owns the whole appliance.
		for _, org := range s.store.ListOrgsAll(0) {
			if s.store.EnterpriseIDForOrg(org.ID) == 0 {
				set[org.ID] = true
			}
		}
	}
	return set
}

func bytesToGB(n int64) float64 {
	return float64(n) / (1024 * 1024 * 1024)
}

func (s *Resolver) addEnterpriseBillingInfoType() *graphql.Object {
	return graphql.NewObject(graphql.ObjectConfig{
		Name: "EnterpriseBillingInfo",
		Fields: graphql.Fields{
			"allLicensableUsersCount":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"assetPacks":               &graphql.Field{Type: graphql.NewNonNull(graphql.Int), DeprecationReason: "`assetPacks` will be removed. Removed as of April 1, 2019."},
			"bandwidthQuota":           &graphql.Field{Type: graphql.NewNonNull(graphql.Float)},
			"bandwidthUsage":           &graphql.Field{Type: graphql.NewNonNull(graphql.Float)},
			"bandwidthUsagePercentage": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"storageQuota":             &graphql.Field{Type: graphql.NewNonNull(graphql.Float)},
			"storageUsage":             &graphql.Field{Type: graphql.NewNonNull(graphql.Float)},
			"storageUsagePercentage":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"totalAvailableLicenses":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"totalLicenses":            &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
}

// EnterpriseOwnerInfo

// enterpriseOwnerInfoDeps carries the types EnterpriseOwnerInfo's fields return.
type enterpriseOwnerInfoDeps struct {
	userType                    *graphql.Object
	adminConnection             *graphql.Object
	outsideCollaboratorConn     *graphql.Object
	memberConnection            *graphql.Object
	adminInvitationConnection   *graphql.Object
	memberInvitationConnection  *graphql.Object
	identityProvider            *graphql.Object
	ipAllowListEntryConnection  *graphql.Object
	organizationConnectionType  *graphql.Object
	userConnectionTypeForTwoFA  *graphql.Object
	organizationMembershipConnT *graphql.Object
}

// enterprisePolicySettingFields maps each policy field to the enum it serves
// and the accessor that reads its stored value.
func (s *Resolver) enterprisePolicySettingFields() map[string]struct {
	enum *graphql.Enum
	read func(store.EnterprisePolicy) string
} {
	enabledDisabled := s.enterpriseEnabledDisabledEnum()
	return map[string]struct {
		enum *graphql.Enum
		read func(store.EnterprisePolicy) string
	}{
		"allowPrivateRepositoryForkingSetting":        {enabledDisabled, func(p store.EnterprisePolicy) string { return p.AllowPrivateRepositoryForking }},
		"membersCanChangeRepositoryVisibilitySetting": {enabledDisabled, func(p store.EnterprisePolicy) string { return p.MembersCanChangeRepositoryVisibility }},
		"membersCanDeleteIssuesSetting":               {enabledDisabled, func(p store.EnterprisePolicy) string { return p.MembersCanDeleteIssues }},
		"membersCanDeleteRepositoriesSetting":         {enabledDisabled, func(p store.EnterprisePolicy) string { return p.MembersCanDeleteRepositories }},
		"membersCanInviteCollaboratorsSetting":        {enabledDisabled, func(p store.EnterprisePolicy) string { return p.MembersCanInviteCollaborators }},
		"membersCanUpdateProtectedBranchesSetting":    {enabledDisabled, func(p store.EnterprisePolicy) string { return p.MembersCanUpdateProtectedBranches }},
		"membersCanViewDependencyInsightsSetting":     {enabledDisabled, func(p store.EnterprisePolicy) string { return p.MembersCanViewDependencyInsights }},
		"organizationProjectsSetting":                 {enabledDisabled, func(p store.EnterprisePolicy) string { return p.OrganizationProjects }},
		"repositoryProjectsSetting":                   {enabledDisabled, func(p store.EnterprisePolicy) string { return p.RepositoryProjects }},
		"repositoryDeployKeySetting":                  {enabledDisabled, func(p store.EnterprisePolicy) string { return p.RepositoryDeployKey }},
		"teamDiscussionsSetting":                      {enabledDisabled, func(p store.EnterprisePolicy) string { return p.TeamDiscussions }},
	}
}

// enterprisePolicyOverrideConnection describes one "…SettingOrganizations"
// field. The `value` filter's type is per-field: a Boolean for an on/off
// policy, an enum for a policy with more than two states.
type enterprisePolicyOverrideConnection struct {
	read      func(store.EnterprisePolicy) string
	valueType func(s *Resolver) graphql.Input
	matches   func(setting string, value interface{}) bool
}

// booleanPolicyFilter matches a Boolean `value` against an
// ENABLED/DISABLED/NO_POLICY setting.
func booleanPolicyFilter(setting string, value interface{}) bool {
	want, ok := value.(bool)
	if !ok {
		return true
	}
	return want == (setting == store.EnterprisePolicyEnabled)
}

// enumPolicyFilter matches an enum `value` against the stored setting.
func enumPolicyFilter(setting string, value interface{}) bool {
	want, ok := value.(string)
	if !ok || want == "" {
		return true
	}
	return want == setting
}

func enterprisePolicyOverrideConnections() map[string]enterprisePolicyOverrideConnection {
	boolean := func(read func(store.EnterprisePolicy) string) enterprisePolicyOverrideConnection {
		return enterprisePolicyOverrideConnection{
			read:      read,
			valueType: func(s *Resolver) graphql.Input { return graphql.NewNonNull(graphql.Boolean) },
			matches:   booleanPolicyFilter,
		}
	}
	enum := func(read func(store.EnterprisePolicy) string, name string, values ...string) enterprisePolicyOverrideConnection {
		return enterprisePolicyOverrideConnection{
			read: read,
			valueType: func(s *Resolver) graphql.Input {
				return graphql.NewNonNull(s.sharedEnum(name, values...))
			},
			matches: enumPolicyFilter,
		}
	}
	return map[string]enterprisePolicyOverrideConnection{
		"allowPrivateRepositoryForkingSettingOrganizations":        boolean(func(p store.EnterprisePolicy) string { return p.AllowPrivateRepositoryForking }),
		"membersCanChangeRepositoryVisibilitySettingOrganizations": boolean(func(p store.EnterprisePolicy) string { return p.MembersCanChangeRepositoryVisibility }),
		"membersCanDeleteIssuesSettingOrganizations":               boolean(func(p store.EnterprisePolicy) string { return p.MembersCanDeleteIssues }),
		"membersCanDeleteRepositoriesSettingOrganizations":         boolean(func(p store.EnterprisePolicy) string { return p.MembersCanDeleteRepositories }),
		"membersCanInviteCollaboratorsSettingOrganizations":        boolean(func(p store.EnterprisePolicy) string { return p.MembersCanInviteCollaborators }),
		"membersCanUpdateProtectedBranchesSettingOrganizations":    boolean(func(p store.EnterprisePolicy) string { return p.MembersCanUpdateProtectedBranches }),
		"membersCanViewDependencyInsightsSettingOrganizations":     boolean(func(p store.EnterprisePolicy) string { return p.MembersCanViewDependencyInsights }),
		"organizationProjectsSettingOrganizations":                 boolean(func(p store.EnterprisePolicy) string { return p.OrganizationProjects }),
		"repositoryDeployKeySettingOrganizations":                  boolean(func(p store.EnterprisePolicy) string { return p.RepositoryDeployKey }),
		"repositoryProjectsSettingOrganizations":                   boolean(func(p store.EnterprisePolicy) string { return p.RepositoryProjects }),
		"twoFactorRequiredSettingOrganizations":                    boolean(func(p store.EnterprisePolicy) string { return p.TwoFactorRequired }),
		"defaultRepositoryPermissionSettingOrganizations": enum(
			func(p store.EnterprisePolicy) string { return p.DefaultRepositoryPermission },
			"DefaultRepositoryPermissionField", "ADMIN", "NONE", "READ", "WRITE"),
		"membersCanCreateRepositoriesSettingOrganizations": enum(
			func(p store.EnterprisePolicy) string { return p.MembersCanCreateRepositories },
			"OrganizationMembersCanCreateRepositoriesSettingValue", "ALL", "DISABLED", "INTERNAL", "PRIVATE"),
		// The SAML connection reports which organizations the binding covers,
		// so its filter is the binding's state, not a policy value.
		"samlIdentityProviderSettingOrganizations": {
			read: func(store.EnterprisePolicy) string { return "" },
			valueType: func(s *Resolver) graphql.Input {
				return graphql.NewNonNull(s.sharedEnum("IdentityProviderConfigurationState", "CONFIGURED", "ENFORCED", "UNCONFIGURED"))
			},
			matches: enumPolicyFilter,
		},
	}
}

func (s *Resolver) addEnterpriseOwnerInfoType(deps enterpriseOwnerInfoDeps) *graphql.Object {
	fields := graphql.Fields{}

	policyField := func(enum *graphql.Enum, read func(store.EnterprisePolicy) string) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(enum),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				e := s.enterpriseFromSource(p.Source)
				if e == nil {
					return store.EnterprisePolicyNoPolicy, nil
				}
				return read(e.Policy), nil
			},
		}
	}
	for name, spec := range s.enterprisePolicySettingFields() {
		fields[name] = policyField(spec.enum, spec.read)
	}
	fields["defaultRepositoryPermissionSetting"] = policyField(
		s.sharedEnum("EnterpriseDefaultRepositoryPermissionSettingValue", "ADMIN", "NONE", "NO_POLICY", "READ", "WRITE"),
		func(p store.EnterprisePolicy) string { return p.DefaultRepositoryPermission })
	fields["membersCanMakePurchasesSetting"] = policyField(
		s.sharedEnum("EnterpriseMembersCanMakePurchasesSettingValue", "DISABLED", "ENABLED"),
		func(p store.EnterprisePolicy) string { return p.MembersCanMakePurchases })
	fields["twoFactorRequiredSetting"] = policyField(s.enterpriseEnabledEnum(),
		func(p store.EnterprisePolicy) string { return p.TwoFactorRequired })
	fields["twoFactorDisallowedMethodsSetting"] = policyField(
		s.sharedEnum("EnterpriseDisallowedMethodsSettingValue", "INSECURE", "NO_POLICY"),
		func(p store.EnterprisePolicy) string { return p.TwoFactorDisallowedMethods })
	fields["notificationDeliveryRestrictionEnabledSetting"] = policyField(
		s.sharedEnum("NotificationRestrictionSettingValue", "DISABLED", "ENABLED"),
		func(p store.EnterprisePolicy) string { return p.NotificationDeliveryRestrictionEnabled })
	fields["ipAllowListEnabledSetting"] = policyField(s.ipAllowListEnabledEnum(),
		func(p store.EnterprisePolicy) string { return p.IPAllowListEnabled })
	fields["ipAllowListForInstalledAppsEnabledSetting"] = policyField(s.ipAllowListForInstalledAppsEnum(),
		func(p store.EnterprisePolicy) string { return p.IPAllowListForInstalledAppsEnabled })
	fields["ipAllowListUserLevelEnforcementEnabledSetting"] = policyField(s.ipAllowListUserLevelEnforcementEnum(),
		func(p store.EnterprisePolicy) string { return p.IPAllowListUserLevelEnforcementEnabled })

	// Nullable: GitHub reports null when the enterprise leaves repository
	// creation to its organizations.
	fields["membersCanCreateRepositoriesSetting"] = &graphql.Field{
		Type: s.sharedEnum("EnterpriseMembersCanCreateRepositoriesSettingValue", "ALL", "DISABLED", "NO_POLICY", "PRIVATE", "PUBLIC"),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if e == nil || e.Policy.MembersCanCreateRepositories == "" {
				return nil, nil
			}
			return e.Policy.MembersCanCreateRepositories, nil
		},
	}
	boolPolicy := func(read func(store.EnterprisePolicy) *bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.Boolean,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				e := s.enterpriseFromSource(p.Source)
				if e == nil {
					return nil, nil
				}
				if value := read(e.Policy); value != nil {
					return *value, nil
				}
				return nil, nil
			},
		}
	}
	fields["membersCanCreatePublicRepositoriesSetting"] = boolPolicy(func(p store.EnterprisePolicy) *bool { return p.MembersCanCreatePublicRepositories })
	fields["membersCanCreatePrivateRepositoriesSetting"] = boolPolicy(func(p store.EnterprisePolicy) *bool { return p.MembersCanCreatePrivateRepositories })
	fields["membersCanCreateInternalRepositoriesSetting"] = boolPolicy(func(p store.EnterprisePolicy) *bool { return p.MembersCanCreateInternalRepositories })

	fields["allowPrivateRepositoryForkingSettingPolicyValue"] = &graphql.Field{
		Type: s.sharedEnum("EnterpriseAllowPrivateRepositoryForkingPolicyValue",
			"ENTERPRISE_ORGANIZATIONS", "ENTERPRISE_ORGANIZATIONS_USER_ACCOUNTS", "EVERYWHERE",
			"SAME_ORGANIZATION", "SAME_ORGANIZATION_USER_ACCOUNTS", "USER_ACCOUNTS"),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			if e == nil || e.Policy.AllowPrivateRepositoryForkingPolicyValue == "" {
				return nil, nil
			}
			return e.Policy.AllowPrivateRepositoryForkingPolicyValue, nil
		},
	}

	// Roll-out flags. bleephub applies a policy to every organization inside
	// the mutation that sets it, so no roll-out is ever outstanding.
	rollout := func(read func(store.EnterprisePolicy) bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				e := s.enterpriseFromSource(p.Source)
				if e == nil {
					return false, nil
				}
				return read(e.Policy), nil
			},
		}
	}
	fields["isUpdatingDefaultRepositoryPermission"] = rollout(func(p store.EnterprisePolicy) bool { return p.IsUpdatingDefaultRepositoryPermission })
	fields["isUpdatingTwoFactorRequirement"] = rollout(func(p store.EnterprisePolicy) bool { return p.IsUpdatingTwoFactorRequirement })

	// The per-organization override connections.
	for name, override := range enterprisePolicyOverrideConnections() {
		override := override
		fields[name] = &graphql.Field{
			Type: graphql.NewNonNull(deps.organizationConnectionType),
			Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
				"value": &graphql.ArgumentConfig{Type: override.valueType(s)},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				e := s.enterpriseFromSource(p.Source)
				return enterpriseConnection(s.enterprisePolicyOrganizations(e, override, p.Args), p.Args), nil
			},
		}
	}

	fields["admins"] = &graphql.Field{
		Type: graphql.NewNonNull(deps.adminConnection),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"query":                   &graphql.ArgumentConfig{Type: graphql.String},
			"role":                    &graphql.ArgumentConfig{Type: s.enterpriseAdministratorRoleEnum()},
			"organizationLogins":      &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"hasTwoFactorEnabled":     &graphql.ArgumentConfig{Type: graphql.Boolean},
			"twoFactorMethodSecurity": &graphql.ArgumentConfig{Type: s.sharedEnum("TwoFactorCredentialSecurityType", "DISABLED", "INSECURE", "SECURE")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			roleFilter, _ := p.Args["role"].(string)
			query, _ := p.Args["query"].(string)
			users, roles := s.enterpriseMemberUsers(e)
			var nodes []map[string]interface{}
			for _, user := range users {
				role := roles[user.ID]
				if role != store.EnterpriseRoleOwner && role != store.EnterpriseRoleBillingManager {
					continue
				}
				if roleFilter != "" && string(role) != roleFilter {
					continue
				}
				if query != "" && !strings.Contains(strings.ToLower(user.Login), strings.ToLower(query)) {
					continue
				}
				node := userToGraphQL(user)
				node["role"] = string(role)
				nodes = append(nodes, node)
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	}

	fields["outsideCollaborators"] = &graphql.Field{
		Type: graphql.NewNonNull(deps.outsideCollaboratorConn),
		Args: mergeArgs(relayConnectionArgs(), graphql.FieldConfigArgument{
			"login":              &graphql.ArgumentConfig{Type: graphql.String},
			"query":              &graphql.ArgumentConfig{Type: graphql.String},
			"organizationLogins": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"visibility":         &graphql.ArgumentConfig{Type: s.sharedEnum("RepositoryVisibility", "INTERNAL", "PRIVATE", "PUBLIC")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			login, _ := p.Args["login"].(string)
			query, _ := p.Args["query"].(string)
			var nodes []map[string]interface{}
			for _, user := range s.enterpriseOutsideCollaborators(e) {
				if login != "" && !strings.EqualFold(user.Login, login) {
					continue
				}
				if query != "" && !strings.Contains(strings.ToLower(user.Login), strings.ToLower(query)) {
					continue
				}
				nodes = append(nodes, userToGraphQL(user))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	}

	fields["supportEntitlements"] = &graphql.Field{
		Type: graphql.NewNonNull(deps.memberConnection),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			var nodes []map[string]interface{}
			for _, m := range s.store.ListEnterpriseMemberships(e.ID) {
				if !m.SupportEntitlement {
					continue
				}
				if user := s.store.GetUserByID(m.UserID); user != nil {
					nodes = append(nodes, s.enterpriseUserAccountToGraphQL(e, user, m.Role))
				}
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	}

	fields["pendingAdminInvitations"] = s.enterpriseInvitationConnectionField(deps.adminInvitationConnection, "admin",
		graphql.FieldConfigArgument{
			"query":   &graphql.ArgumentConfig{Type: graphql.String},
			"role":    &graphql.ArgumentConfig{Type: s.enterpriseAdministratorRoleEnum()},
			"orderBy": &graphql.ArgumentConfig{Type: s.enterpriseInvitationOrderInput("EnterpriseAdministratorInvitationOrder", "EnterpriseAdministratorInvitationOrderField")},
		})
	fields["pendingUnaffiliatedMemberInvitations"] = s.enterpriseInvitationConnectionField(deps.memberInvitationConnection, "member",
		graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: s.enterpriseInvitationOrderInput("EnterpriseMemberInvitationOrder", "EnterpriseMemberInvitationOrderField")},
		})

	fields["samlIdentityProvider"] = &graphql.Field{
		Type: deps.identityProvider,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			return optionalObject(enterpriseIdentityProviderToGraphQL(e, false)), nil
		},
	}

	fields["ipAllowListEntries"] = &graphql.Field{
		Type: graphql.NewNonNull(deps.ipAllowListEntryConnection),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			var nodes []map[string]interface{}
			for _, entry := range s.store.ListIPAllowListEntries("Enterprise", e.ID) {
				nodes = append(nodes, ipAllowListEntryToGraphQL(entry))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	}

	fields["affiliatedUsersWithTwoFactorDisabled"] = &graphql.Field{
		Type: graphql.NewNonNull(deps.userConnectionTypeForTwoFA),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			var nodes []map[string]interface{}
			for _, user := range s.enterpriseUsersWithoutTwoFactor(e) {
				nodes = append(nodes, userToGraphQL(user))
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	}
	fields["affiliatedUsersWithTwoFactorDisabledExist"] = &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			return len(s.enterpriseUsersWithoutTwoFactor(e)) > 0, nil
		},
	}

	return graphql.NewObject(graphql.ObjectConfig{Name: "EnterpriseOwnerInfo", Fields: fields})
}

func (s *Resolver) enterpriseInvitationOrderInput(name, fieldEnumName string) *graphql.InputObject {
	if s.enterpriseOrderInputs == nil {
		s.enterpriseOrderInputs = map[string]*graphql.InputObject{}
	}
	if existing := s.enterpriseOrderInputs[name]; existing != nil {
		return existing
	}
	input := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: name,
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.sharedEnum("OrderDirection", "ASC", "DESC"))},
			"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.sharedEnum(fieldEnumName, "CREATED_AT"))},
		},
	})
	s.enterpriseOrderInputs[name] = input
	return input
}

func (s *Resolver) enterpriseInvitationConnectionField(connectionType *graphql.Object, kind string, extraArgs graphql.FieldConfigArgument) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(connectionType),
		Args: mergeArgs(relayConnectionArgs(), extraArgs),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			e := s.enterpriseFromSource(p.Source)
			roleFilter, _ := p.Args["role"].(string)
			query, _ := p.Args["query"].(string)
			var nodes []map[string]interface{}
			for _, inv := range s.store.ListEnterpriseInvitations(e.ID, kind) {
				if roleFilter != "" && string(inv.Role) != roleFilter {
					continue
				}
				if query != "" {
					invitee := s.store.GetUserByID(inv.InviteeID)
					login := inv.Email
					if invitee != nil {
						login = invitee.Login
					}
					if !strings.Contains(strings.ToLower(login), strings.ToLower(query)) {
						continue
					}
				}
				nodes = append(nodes, s.enterpriseInvitationToGraphQL(inv))
			}
			if order, ok := p.Args["orderBy"].(map[string]interface{}); ok {
				if direction, _ := order["direction"].(string); direction == "DESC" {
					for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
						nodes[i], nodes[j] = nodes[j], nodes[i]
					}
				}
			}
			return enterpriseConnection(nodes, p.Args), nil
		},
	}
}

// enterprisePolicyOrganizations lists the organizations the enterprise's
// policy governs. NO_POLICY governs nothing, so the connection is empty.
func (s *Resolver) enterprisePolicyOrganizations(e *store.Enterprise, override enterprisePolicyOverrideConnection, args map[string]interface{}) []map[string]interface{} {
	if e == nil {
		return nil
	}
	setting := override.read(e.Policy)
	if setting == "" || setting == store.EnterprisePolicyNoPolicy {
		return nil
	}
	if value, filtered := args["value"]; filtered && !override.matches(setting, value) {
		return nil
	}
	return s.enterpriseOrganizationNodes(e)
}

// enterpriseOutsideCollaborators lists accounts collaborating on an enterprise
// organization's repositories without belonging to it, computed rather than stored.
func (s *Resolver) enterpriseOutsideCollaborators(e *store.Enterprise) []*store.User {
	if e == nil {
		return nil
	}
	seen := map[int]bool{}
	var out []*store.User
	for _, orgID := range s.store.ListEnterpriseOrgIDs(e.ID) {
		org := s.store.GetOrgByID(orgID)
		if org == nil {
			continue
		}
		for _, user := range s.store.ListOutsideCollaborators(org.Login) {
			if seen[user.ID] {
				continue
			}
			seen[user.ID] = true
			out = append(out, user)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Login < out[j].Login })
	return out
}

// enterpriseUsersWithoutTwoFactor lists the enterprise's members who have not
// enrolled a second factor.
func (s *Resolver) enterpriseUsersWithoutTwoFactor(e *store.Enterprise) []*store.User {
	users, _ := s.enterpriseMemberUsers(e)
	var out []*store.User
	for _, user := range users {
		if !s.store.TwoFactorEnabled(user.ID) {
			out = append(out, user)
		}
	}
	return out
}
