package graphqlapi

// The enterprise account mutation surface. Enterprise mutations name no
// repository, so they carry their own authorization rule types. Every rule
// resolves the enterprise from the input, so owning one enterprise never
// authorizes a write against another.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// --- authorization rules ---------------------------------------------------

// enterpriseOwnerRule requires the viewer to own the enterprise named by idKey.
type enterpriseOwnerRule struct {
	idKey string
}

func (r enterpriseOwnerRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no enterprise id input key")
	}
	return nil
}

func (r enterpriseOwnerRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.idKey].(string)
	e := store.FindEnterpriseByNodeID(s.store, nodeID)
	if e == nil {
		return gqlMissingNode("Enterprise", nodeID)
	}
	if !s.store.IsEnterpriseOwner(e.ID, s.ghUserFromContext(p.Context)) {
		return enterpriseOwnerRequired()
	}
	return nil
}

func enterpriseOwnerRequired() error {
	return &ghForbiddenError{message: "You must be an owner of the enterprise to perform this action."}
}

// enterpriseInvitationRule is the policy for the four invitation mutations:
// accept admits only the invitee, cancel only an owner of the issuing enterprise.
type enterpriseInvitationRule struct {
	accept bool
	// kind is "admin" or "member": an admin invitation accepted through the
	// member mutation would launder the role.
	kind string
}

func (r enterpriseInvitationRule) check() error {
	if r.kind != "admin" && r.kind != "member" {
		return fmt.Errorf("invitation kind must be admin or member")
	}
	return nil
}

func (r enterpriseInvitationRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input["invitationId"].(string)
	inv := store.FindEnterpriseInvitationByNodeID(s.store, nodeID)
	if inv == nil || inv.Kind != r.kind {
		return gqlMissingNode(enterpriseInvitationTypeName(r.kind), nodeID)
	}
	viewer := s.ghUserFromContext(p.Context)
	if r.accept {
		if viewer == nil || inv.InviteeID == 0 || viewer.ID != inv.InviteeID {
			// Same answer as "no such invitation", so it does not leak to a
			// stranger that somebody was invited.
			return gqlMissingNode(enterpriseInvitationTypeName(r.kind), nodeID)
		}
		return nil
	}
	if !s.store.IsEnterpriseOwner(inv.EnterpriseID, viewer) {
		return enterpriseOwnerRequired()
	}
	return nil
}

func enterpriseInvitationTypeName(kind string) string {
	if kind == "admin" {
		return "EnterpriseAdministratorInvitation"
	}
	return "EnterpriseMemberInvitation"
}

// enterpriseTransferRule is the policy for transferEnterpriseOrganization, whose
// effect spans two enterprises: the viewer must own both the destination and the
// enterprise the organization is leaving.
type enterpriseTransferRule struct{}

func (enterpriseTransferRule) check() error { return nil }

func (enterpriseTransferRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	viewer := s.ghUserFromContext(p.Context)
	destinationID, _ := input["destinationEnterpriseId"].(string)
	destination := store.FindEnterpriseByNodeID(s.store, destinationID)
	if destination == nil {
		return gqlMissingNode("Enterprise", destinationID)
	}
	if !s.store.IsEnterpriseOwner(destination.ID, viewer) {
		return enterpriseOwnerRequired()
	}
	orgNodeID, _ := input["organizationId"].(string)
	org := s.orgByNodeID(orgNodeID)
	if org == nil {
		return gqlMissingNode("Organization", orgNodeID)
	}
	sourceID := s.store.EnterpriseIDForOrg(org.ID)
	if sourceID == 0 || !s.store.IsEnterpriseOwner(sourceID, viewer) {
		return enterpriseOwnerRequired()
	}
	return nil
}

// ipAllowListOwnerRule authorizes an IP allow list mutation against the list's
// owner (an enterprise or organization).
type ipAllowListOwnerRule struct {
	// Exactly one is set: ownerKey names the owner directly, entryKey names an
	// existing entry whose owner is looked up.
	ownerKey string
	entryKey string
}

func (r ipAllowListOwnerRule) check() error {
	if (r.ownerKey == "") == (r.entryKey == "") {
		return fmt.Errorf("exactly one of ownerKey or entryKey must be set")
	}
	return nil
}

func (r ipAllowListOwnerRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	ownerType, ownerID, nodeID, err := r.resolveOwner(s, input)
	if err != nil {
		return err
	}
	viewer := s.ghUserFromContext(p.Context)
	switch ownerType {
	case "Enterprise":
		if !s.store.IsEnterpriseOwner(ownerID, viewer) {
			return enterpriseOwnerRequired()
		}
		return nil
	case "Organization":
		org := s.store.GetOrgByID(ownerID)
		if org == nil {
			return gqlMissingNode("IpAllowListOwner", nodeID)
		}
		if m := s.store.GetMembership(org.Login, viewer.ID); m == nil || m.Role != store.OrgRoleAdmin || m.State != store.MembershipStateActive {
			return &ghForbiddenError{message: "You must be an owner of the organization to perform this action."}
		}
		return nil
	}
	return gqlMissingNode("IpAllowListOwner", nodeID)
}

func (r ipAllowListOwnerRule) resolveOwner(s *Resolver, input map[string]interface{}) (string, int, string, error) {
	if r.entryKey != "" {
		nodeID, _ := input[r.entryKey].(string)
		entry := store.FindIPAllowListEntryByNodeID(s.store, nodeID)
		if entry == nil {
			return "", 0, nodeID, gqlMissingNode("IpAllowListEntry", nodeID)
		}
		return entry.OwnerType, entry.OwnerID, nodeID, nil
	}
	nodeID, _ := input[r.ownerKey].(string)
	if e := store.FindEnterpriseByNodeID(s.store, nodeID); e != nil {
		return "Enterprise", e.ID, nodeID, nil
	}
	if org := s.orgByNodeID(nodeID); org != nil {
		return "Organization", org.ID, nodeID, nil
	}
	return "", 0, nodeID, gqlMissingNode("IpAllowListOwner", nodeID)
}

func (s *Resolver) orgByNodeID(nodeID string) *store.Org {
	if nodeID == "" {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, org := range s.store.Orgs {
		if org.NodeID == nodeID {
			candidate := *org
			return &candidate
		}
	}
	return nil
}

// enterpriseMutationAuthzRows is the enterprise family's half of the mutation
// authorization table.
func enterpriseMutationAuthzRows() map[string]mutationRule {
	ownerRule := func(key string) mutationRule { return enterpriseOwnerRule{idKey: key} }
	rows := map[string]mutationRule{
		"acceptEnterpriseAdministratorInvitation":             enterpriseInvitationRule{accept: true, kind: "admin"},
		"acceptEnterpriseMemberInvitation":                    enterpriseInvitationRule{accept: true, kind: "member"},
		"cancelEnterpriseAdminInvitation":                     enterpriseInvitationRule{kind: "admin"},
		"cancelEnterpriseMemberInvitation":                    enterpriseInvitationRule{kind: "member"},
		"transferEnterpriseOrganization":                      enterpriseTransferRule{},
		"accessUserNamespaceRepository":                       ownerRule("enterpriseId"),
		"createIpAllowListEntry":                              ipAllowListOwnerRule{ownerKey: "ownerId"},
		"updateIpAllowListEntry":                              ipAllowListOwnerRule{entryKey: "ipAllowListEntryId"},
		"deleteIpAllowListEntry":                              ipAllowListOwnerRule{entryKey: "ipAllowListEntryId"},
		"updateIpAllowListEnabledSetting":                     ipAllowListOwnerRule{ownerKey: "ownerId"},
		"updateIpAllowListForInstalledAppsEnabledSetting":     ipAllowListOwnerRule{ownerKey: "ownerId"},
		"updateIpAllowListUserLevelEnforcementEnabledSetting": ipAllowListOwnerRule{ownerKey: "ownerId"},
	}
	for _, name := range enterpriseOwnerGatedMutations() {
		rows[name] = ownerRule("enterpriseId")
	}
	return rows
}

// enterpriseOwnerGatedMutations name the enterprise directly and are owner-only.
func enterpriseOwnerGatedMutations() []string {
	return []string{
		"addEnterpriseOrganizationMember",
		"addEnterpriseSupportEntitlement",
		"createEnterpriseOrganization",
		"grantEnterpriseOrganizationsMigratorRole",
		"inviteEnterpriseAdmin",
		"inviteEnterpriseMember",
		"regenerateEnterpriseIdentityProviderRecoveryCodes",
		"removeEnterpriseAdmin",
		"removeEnterpriseIdentityProvider",
		"removeEnterpriseMember",
		"removeEnterpriseOrganization",
		"removeEnterpriseSupportEntitlement",
		"revokeEnterpriseOrganizationsMigratorRole",
		"setEnterpriseIdentityProvider",
		"updateEnterpriseAdministratorRole",
		"updateEnterpriseAllowPrivateRepositoryForkingSetting",
		"updateEnterpriseDefaultRepositoryPermissionSetting",
		"updateEnterpriseDeployKeySetting",
		"updateEnterpriseMembersCanChangeRepositoryVisibilitySetting",
		"updateEnterpriseMembersCanCreateRepositoriesSetting",
		"updateEnterpriseMembersCanDeleteIssuesSetting",
		"updateEnterpriseMembersCanDeleteRepositoriesSetting",
		"updateEnterpriseMembersCanInviteCollaboratorsSetting",
		"updateEnterpriseMembersCanMakePurchasesSetting",
		"updateEnterpriseMembersCanUpdateProtectedBranchesSetting",
		"updateEnterpriseMembersCanViewDependencyInsightsSetting",
		"updateEnterpriseOrganizationProjectsSetting",
		"updateEnterpriseOwnerOrganizationRole",
		"updateEnterpriseProfile",
		"updateEnterpriseProofOfPresenceRequiredSetting",
		"updateEnterpriseRepositoryProjectsSetting",
		"updateEnterpriseTwoFactorAuthenticationDisallowedMethodsSetting",
		"updateEnterpriseTwoFactorAuthenticationRequiredSetting",
	}
}

func init() {
	for name, rule := range enterpriseMutationAuthzRows() {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic("graphql mutation " + name + " already has an authorization row")
		}
		graphqlMutationAuthz[name] = rule
	}
}

// --- audit -----------------------------------------------------------------

// recordEnterpriseAudit writes one enterprise audit-log entry.
func (s *Resolver) recordEnterpriseAudit(p graphql.ResolveParams, e *store.Enterprise, action string, data map[string]interface{}) {
	actor := ""
	if user := s.ghUserFromContext(p.Context); user != nil {
		actor = user.Login
	}
	if data == nil {
		data = map[string]interface{}{}
	}
	if e != nil {
		data["business"] = e.Slug
		data["business_id"] = e.ID
	}
	s.store.RecordAuditEntry(action, actor, "", data)
}

// --- schema assembly -------------------------------------------------------

func (s *Resolver) addEnterpriseMutationsToSchema(mutationType *graphql.Object) {
	enterpriseType := s.graphqlTypes.enterprise
	userType := s.graphqlTypes.user
	orgType := s.graphqlTypes.organization

	s.addEnterprisePolicyMutations(mutationType, enterpriseType)
	s.addEnterpriseProfileMutation(mutationType, enterpriseType)
	s.addEnterpriseMembershipMutations(mutationType, enterpriseType, userType)
	s.addEnterpriseOrganizationMutations(mutationType, enterpriseType, orgType, userType)
	s.addEnterpriseIdentityProviderMutations(mutationType)
	s.addIPAllowListMutations(mutationType)
}

// enterpriseSettingPayload mints the {enterprise, message} payload most policy
// mutations return.
func (s *Resolver) enterpriseSettingPayload(name string, enterpriseType *graphql.Object) *graphql.Object {
	return graphql.NewObject(graphql.ObjectConfig{
		Name: name,
		Fields: graphql.Fields{
			"enterprise": &graphql.Field{Type: enterpriseType},
			"message":    &graphql.Field{Type: graphql.String},
		},
	})
}

// enterprisePolicyMutation is one policy setting's whole definition.
type enterprisePolicyMutation struct {
	name             string
	enum             *graphql.Enum
	apply            func(policy *store.EnterprisePolicy, value string)
	extraInputFields graphql.InputObjectConfigFieldMap
	// settingValueOptional marks the one setting whose settingValue GitHub
	// declares nullable (members-can-create-repositories, whose booleans can
	// carry the change alone).
	settingValueOptional bool
	applyExtras          func(policy *store.EnterprisePolicy, input map[string]interface{})
	auditAction          string
}

func (s *Resolver) addEnterprisePolicyMutations(mutationType *graphql.Object, enterpriseType *graphql.Object) {
	enabledDisabled := s.enterpriseEnabledDisabledEnum()
	setting := func(name string, enum *graphql.Enum, apply func(*store.EnterprisePolicy, string), auditAction string) enterprisePolicyMutation {
		return enterprisePolicyMutation{name: name, enum: enum, apply: apply, auditAction: auditAction}
	}
	mutations := []enterprisePolicyMutation{
		setting("updateEnterpriseAllowPrivateRepositoryForkingSetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.AllowPrivateRepositoryForking = v },
			"business.private_repository_forking_policy_update"),
		setting("updateEnterpriseDefaultRepositoryPermissionSetting",
			s.sharedEnum("EnterpriseDefaultRepositoryPermissionSettingValue", "ADMIN", "NONE", "NO_POLICY", "READ", "WRITE"),
			func(p *store.EnterprisePolicy, v string) { p.DefaultRepositoryPermission = v },
			"business.default_repository_permission_update"),
		setting("updateEnterpriseDeployKeySetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.RepositoryDeployKey = v },
			"business.repository_deploy_key_policy_update"),
		setting("updateEnterpriseMembersCanChangeRepositoryVisibilitySetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.MembersCanChangeRepositoryVisibility = v },
			"business.members_can_change_repository_visibility_policy_update"),
		setting("updateEnterpriseMembersCanDeleteIssuesSetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.MembersCanDeleteIssues = v },
			"business.members_can_delete_issues_policy_update"),
		setting("updateEnterpriseMembersCanDeleteRepositoriesSetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.MembersCanDeleteRepositories = v },
			"business.members_can_delete_repositories_policy_update"),
		setting("updateEnterpriseMembersCanInviteCollaboratorsSetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.MembersCanInviteCollaborators = v },
			"business.members_can_invite_collaborators_policy_update"),
		setting("updateEnterpriseMembersCanMakePurchasesSetting",
			s.sharedEnum("EnterpriseMembersCanMakePurchasesSettingValue", "DISABLED", "ENABLED"),
			func(p *store.EnterprisePolicy, v string) { p.MembersCanMakePurchases = v },
			"business.members_can_make_purchases_policy_update"),
		setting("updateEnterpriseMembersCanUpdateProtectedBranchesSetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.MembersCanUpdateProtectedBranches = v },
			"business.members_can_update_protected_branches_policy_update"),
		setting("updateEnterpriseMembersCanViewDependencyInsightsSetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.MembersCanViewDependencyInsights = v },
			"business.members_can_view_dependency_insights_policy_update"),
		setting("updateEnterpriseOrganizationProjectsSetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.OrganizationProjects = v },
			"business.organization_projects_policy_update"),
		setting("updateEnterpriseRepositoryProjectsSetting", enabledDisabled,
			func(p *store.EnterprisePolicy, v string) { p.RepositoryProjects = v },
			"business.repository_projects_policy_update"),
		setting("updateEnterpriseProofOfPresenceRequiredSetting",
			s.sharedEnum("ProofOfPresenceRequirement", "MFA", "NO_POLICY", "REAUTH"),
			func(p *store.EnterprisePolicy, v string) { p.ProofOfPresenceRequired = v },
			"business.proof_of_presence_required_policy_update"),
		setting("updateEnterpriseTwoFactorAuthenticationRequiredSetting", s.enterpriseEnabledEnum(),
			func(p *store.EnterprisePolicy, v string) { p.TwoFactorRequired = v },
			"business.two_factor_requirement_update"),
		setting("updateEnterpriseTwoFactorAuthenticationDisallowedMethodsSetting",
			s.sharedEnum("EnterpriseDisallowedMethodsSettingValue", "INSECURE", "NO_POLICY"),
			func(p *store.EnterprisePolicy, v string) { p.TwoFactorDisallowedMethods = v },
			"business.two_factor_disallowed_methods_update"),
	}

	forking := &mutations[0]
	forking.extraInputFields = graphql.InputObjectConfigFieldMap{
		"policyValue": &graphql.InputObjectFieldConfig{Type: s.sharedEnum("EnterpriseAllowPrivateRepositoryForkingPolicyValue",
			"ENTERPRISE_ORGANIZATIONS", "ENTERPRISE_ORGANIZATIONS_USER_ACCOUNTS", "EVERYWHERE",
			"SAME_ORGANIZATION", "SAME_ORGANIZATION_USER_ACCOUNTS", "USER_ACCOUNTS")},
	}
	forking.applyExtras = func(p *store.EnterprisePolicy, input map[string]interface{}) {
		if value, ok := input["policyValue"].(string); ok {
			p.AllowPrivateRepositoryForkingPolicyValue = value
		}
	}

	mutations = append(mutations, enterprisePolicyMutation{
		name:                 "updateEnterpriseMembersCanCreateRepositoriesSetting",
		enum:                 s.sharedEnum("EnterpriseMembersCanCreateRepositoriesSettingValue", "ALL", "DISABLED", "NO_POLICY", "PRIVATE", "PUBLIC"),
		settingValueOptional: true,
		apply:                func(p *store.EnterprisePolicy, v string) { p.MembersCanCreateRepositories = v },
		auditAction:          "business.members_can_create_repositories_policy_update",
		extraInputFields: graphql.InputObjectConfigFieldMap{
			"membersCanCreateInternalRepositories":      &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"membersCanCreatePrivateRepositories":       &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"membersCanCreatePublicRepositories":        &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"membersCanCreateRepositoriesPolicyEnabled": &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
		},
		applyExtras: func(p *store.EnterprisePolicy, input map[string]interface{}) {
			assign := func(key string, dst **bool) {
				if value, ok := input[key].(bool); ok {
					v := value
					*dst = &v
				}
			}
			assign("membersCanCreateInternalRepositories", &p.MembersCanCreateInternalRepositories)
			assign("membersCanCreatePrivateRepositories", &p.MembersCanCreatePrivateRepositories)
			assign("membersCanCreatePublicRepositories", &p.MembersCanCreatePublicRepositories)
			// A disabled policy means the enterprise imposes nothing here.
			if enabled, ok := input["membersCanCreateRepositoriesPolicyEnabled"].(bool); ok && !enabled {
				p.MembersCanCreateRepositories = store.EnterprisePolicyNoPolicy
			}
		},
	})

	for _, mutation := range mutations {
		s.registerEnterprisePolicyMutation(mutationType, enterpriseType, mutation)
	}
}

func (s *Resolver) registerEnterprisePolicyMutation(mutationType, enterpriseType *graphql.Object, m enterprisePolicyMutation) {
	inputName := strings.ToUpper(m.name[:1]) + m.name[1:] + "Input"
	payloadName := strings.ToUpper(m.name[:1]) + m.name[1:] + "Payload"
	settingType := graphql.Type(graphql.NewNonNull(m.enum))
	if m.settingValueOptional {
		settingType = m.enum
	}
	fields := graphql.InputObjectConfigFieldMap{
		"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		"settingValue": &graphql.InputObjectFieldConfig{Type: settingType},
	}
	for name, field := range m.extraInputFields {
		fields[name] = field
	}
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{Name: inputName, Fields: fields})
	payloadType := s.enterpriseSettingPayload(payloadName, enterpriseType)
	apply := m.apply
	applyExtras := m.applyExtras
	auditAction := m.auditAction
	s.registerMutation(mutationType, m.name, &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			value, hasValue := input["settingValue"].(string)
			updated := s.store.UpdateEnterprisePolicy(e.ID, func(policy *store.EnterprisePolicy) {
				if hasValue {
					apply(policy, value)
				}
				if applyExtras != nil {
					applyExtras(policy, input)
				}
			})
			if updated == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			s.recordEnterpriseAudit(p, updated, auditAction, map[string]interface{}{"setting_value": value})
			return map[string]interface{}{
				"enterprise": enterpriseToGraphQL(updated),
				"message":    nil,
			}, nil
		},
	})
}

func (s *Resolver) addEnterpriseProfileMutation(mutationType, enterpriseType *graphql.Object) {
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateEnterpriseProfileInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"description":          &graphql.InputObjectFieldConfig{Type: graphql.String},
			"location":             &graphql.InputObjectFieldConfig{Type: graphql.String},
			"name":                 &graphql.InputObjectFieldConfig{Type: graphql.String},
			"securityContactEmail": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"websiteUrl":           &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name:   "UpdateEnterpriseProfilePayload",
		Fields: graphql.Fields{"enterprise": &graphql.Field{Type: enterpriseType}},
	})
	s.registerMutation(mutationType, "updateEnterpriseProfile", &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			read := func(key string) *string {
				if value, ok := input[key].(string); ok {
					return &value
				}
				return nil
			}
			updated := s.store.UpdateEnterpriseProfile(e.ID,
				read("name"), read("description"), read("location"), read("websiteUrl"), read("securityContactEmail"), nil)
			if updated == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			s.recordEnterpriseAudit(p, updated, "business.update_profile", nil)
			return map[string]interface{}{"enterprise": enterpriseToGraphQL(updated)}, nil
		},
	})
}

// --- membership, administrators and invitations ----------------------------

func (s *Resolver) addEnterpriseMembershipMutations(mutationType, enterpriseType, userType *graphql.Object) {
	adminInvitationType := s.graphqlTypes.enterpriseAdminInvitation
	memberInvitationType := s.graphqlTypes.enterpriseMemberInvite
	adminRoleEnum := s.enterpriseAdministratorRoleEnum()

	// inviteEnterpriseAdmin / inviteEnterpriseMember
	s.registerEnterpriseInviteMutation(mutationType, "inviteEnterpriseAdmin", "admin", adminInvitationType, adminRoleEnum)
	s.registerEnterpriseInviteMutation(mutationType, "inviteEnterpriseMember", "member", memberInvitationType, nil)

	// accept / cancel, one pair per invitation kind.
	s.registerEnterpriseInvitationLifecycleMutation(mutationType, "acceptEnterpriseAdministratorInvitation", "AcceptEnterpriseAdministratorInvitationInput", "AcceptEnterpriseAdministratorInvitationPayload", adminInvitationType, true, "admin")
	s.registerEnterpriseInvitationLifecycleMutation(mutationType, "acceptEnterpriseMemberInvitation", "AcceptEnterpriseMemberInvitationInput", "AcceptEnterpriseMemberInvitationPayload", memberInvitationType, true, "member")
	s.registerEnterpriseInvitationLifecycleMutation(mutationType, "cancelEnterpriseAdminInvitation", "CancelEnterpriseAdminInvitationInput", "CancelEnterpriseAdminInvitationPayload", adminInvitationType, false, "admin")
	s.registerEnterpriseInvitationLifecycleMutation(mutationType, "cancelEnterpriseMemberInvitation", "CancelEnterpriseMemberInvitationInput", "CancelEnterpriseMemberInvitationPayload", memberInvitationType, false, "member")

	// removeEnterpriseAdmin
	removeAdminInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RemoveEnterpriseAdminInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"login":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	removeAdminPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "RemoveEnterpriseAdminPayload",
		Fields: graphql.Fields{
			"admin":      &graphql.Field{Type: userType},
			"enterprise": &graphql.Field{Type: enterpriseType},
			"message":    &graphql.Field{Type: graphql.String},
			"viewer":     &graphql.Field{Type: userType},
		},
	})
	s.registerMutation(mutationType, "removeEnterpriseAdmin", &graphql.Field{
		Type: removeAdminPayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(removeAdminInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			e, admin, err := s.enterpriseAndLogin(input, "login")
			if err != nil {
				return nil, err
			}
			// Demote rather than delete: a removed admin stays a member if their
			// organizations keep them one.
			s.store.SetEnterpriseMembership(e.ID, admin.ID, store.EnterpriseRoleMember)
			s.recordEnterpriseAudit(p, e, "business.remove_admin", map[string]interface{}{"user": admin.Login})
			return map[string]interface{}{
				"admin":      userToGraphQL(admin),
				"enterprise": optionalObject(enterpriseToGraphQL(s.store.GetEnterpriseByID(e.ID))),
				"message":    nil,
				"viewer":     userToGraphQL(s.ghUserFromContext(p.Context)),
			}, nil
		},
	})

	// removeEnterpriseMember
	removeMemberInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RemoveEnterpriseMemberInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"userId":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	removeMemberPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "RemoveEnterpriseMemberPayload",
		Fields: graphql.Fields{
			"enterprise": &graphql.Field{Type: enterpriseType},
			"user":       &graphql.Field{Type: userType},
			"viewer":     &graphql.Field{Type: userType},
		},
	})
	s.registerMutation(mutationType, "removeEnterpriseMember", &graphql.Field{
		Type: removeMemberPayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(removeMemberInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			enterpriseNodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, enterpriseNodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", enterpriseNodeID)
			}
			userNodeID, _ := input["userId"].(string)
			user := store.FindUserByNodeID(s.store, userNodeID)
			if user == nil {
				return nil, gqlMissingNode("User", userNodeID)
			}
			// An enterprise membership is the sum of its org memberships, so
			// removal must also drop the person from every org the enterprise owns.
			s.store.RemoveEnterpriseMembership(e.ID, user.ID)
			for _, orgID := range s.store.ListEnterpriseOrgIDs(e.ID) {
				if org := s.store.GetOrgByID(orgID); org != nil {
					s.store.RemoveMembership(org.Login, user.ID)
				}
			}
			s.recordEnterpriseAudit(p, e, "business.remove_member", map[string]interface{}{"user": user.Login})
			return map[string]interface{}{
				"enterprise": optionalObject(enterpriseToGraphQL(s.store.GetEnterpriseByID(e.ID))),
				"user":       userToGraphQL(user),
				"viewer":     userToGraphQL(s.ghUserFromContext(p.Context)),
			}, nil
		},
	})

	// updateEnterpriseAdministratorRole
	updateRoleInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateEnterpriseAdministratorRoleInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"login":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"role":         &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(adminRoleEnum)},
		},
	})
	updateRolePayload := graphql.NewObject(graphql.ObjectConfig{
		Name:   "UpdateEnterpriseAdministratorRolePayload",
		Fields: graphql.Fields{"message": &graphql.Field{Type: graphql.String}},
	})
	s.registerMutation(mutationType, "updateEnterpriseAdministratorRole", &graphql.Field{
		Type: updateRolePayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateRoleInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			e, admin, err := s.enterpriseAndLogin(input, "login")
			if err != nil {
				return nil, err
			}
			role, _ := input["role"].(string)
			if !store.ValidEnterpriseAdministratorRole(role) {
				return nil, fmt.Errorf("%q is not a valid EnterpriseAdministratorRole", role)
			}
			if s.store.SetEnterpriseMembership(e.ID, admin.ID, store.EnterpriseRole(role)) == nil {
				return nil, gqlMissingNode("Enterprise", e.NodeID)
			}
			s.recordEnterpriseAudit(p, e, "business.update_admin_role", map[string]interface{}{"user": admin.Login, "role": role})
			return map[string]interface{}{
				"message": admin.Login + " is now " + strings.ToLower(strings.ReplaceAll(role, "_", " ")) + " of " + e.Slug,
			}, nil
		},
	})

	// add / remove support entitlement
	s.registerEnterpriseSupportEntitlementMutation(mutationType, "addEnterpriseSupportEntitlement", true)
	s.registerEnterpriseSupportEntitlementMutation(mutationType, "removeEnterpriseSupportEntitlement", false)

	// grant / revoke the organizations migrator role
	s.registerEnterpriseMigratorRoleMutation(mutationType, "grantEnterpriseOrganizationsMigratorRole", true)
	s.registerEnterpriseMigratorRoleMutation(mutationType, "revokeEnterpriseOrganizationsMigratorRole", false)
}

// enterpriseAndLogin resolves the enterprise the input names plus the user its
// login names.
func (s *Resolver) enterpriseAndLogin(input map[string]interface{}, loginKey string) (*store.Enterprise, *store.User, error) {
	nodeID, _ := input["enterpriseId"].(string)
	e := store.FindEnterpriseByNodeID(s.store, nodeID)
	if e == nil {
		return nil, nil, gqlMissingNode("Enterprise", nodeID)
	}
	login, _ := input[loginKey].(string)
	user := s.store.LookupUserByLogin(login)
	if user == nil {
		return nil, nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a User with the login of '%s'.", login)}
	}
	return e, user, nil
}

func (s *Resolver) registerEnterpriseInviteMutation(mutationType *graphql.Object, name, kind string, invitationType *graphql.Object, roleEnum *graphql.Enum) {
	inputName := strings.ToUpper(name[:1]) + name[1:] + "Input"
	payloadName := strings.ToUpper(name[:1]) + name[1:] + "Payload"
	fields := graphql.InputObjectConfigFieldMap{
		"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		"email":        &graphql.InputObjectFieldConfig{Type: graphql.String},
		"invitee":      &graphql.InputObjectFieldConfig{Type: graphql.String},
	}
	if roleEnum != nil {
		fields["role"] = &graphql.InputObjectFieldConfig{Type: roleEnum}
	}
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{Name: inputName, Fields: fields})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name:   payloadName,
		Fields: graphql.Fields{"invitation": &graphql.Field{Type: invitationType}},
	})
	s.registerMutation(mutationType, name, &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			email, _ := input["email"].(string)
			inviteeLogin, _ := input["invitee"].(string)
			if email == "" && inviteeLogin == "" {
				return nil, fmt.Errorf("an invitation needs either an invitee or an email address")
			}
			inviteeID := 0
			if inviteeLogin != "" {
				invitee := s.store.LookupUserByLogin(inviteeLogin)
				if invitee == nil {
					return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a User with the login of '%s'.", inviteeLogin)}
				}
				inviteeID = invitee.ID
			}
			role := store.EnterpriseRoleOwner
			if kind == "admin" {
				if value, ok := input["role"].(string); ok && value != "" {
					if !store.ValidEnterpriseAdministratorRole(value) {
						return nil, fmt.Errorf("%q is not a valid EnterpriseAdministratorRole", value)
					}
					role = store.EnterpriseRole(value)
				}
			} else {
				role = store.EnterpriseRoleMember
			}
			inviter := s.ghUserFromContext(p.Context)
			inv := s.store.CreateEnterpriseInvitation(e.ID, inviter.ID, inviteeID, email, kind, role)
			if inv == nil {
				return nil, fmt.Errorf("an invitation to this enterprise is already outstanding for that recipient")
			}
			s.recordEnterpriseAudit(p, e, "business.invite_"+kind, map[string]interface{}{
				"invitee": inviteeLogin, "email": email, "role": string(role),
			})
			return map[string]interface{}{"invitation": s.enterpriseInvitationToGraphQL(inv)}, nil
		},
	})
}

func (s *Resolver) registerEnterpriseInvitationLifecycleMutation(mutationType *graphql.Object, name, inputName, payloadName string, invitationType *graphql.Object, accept bool, kind string) {
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: inputName,
		Fields: graphql.InputObjectConfigFieldMap{
			"invitationId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: payloadName,
		Fields: graphql.Fields{
			"invitation": &graphql.Field{Type: invitationType},
			"message":    &graphql.Field{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, name, &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["invitationId"].(string)
			inv := store.FindEnterpriseInvitationByNodeID(s.store, nodeID)
			if inv == nil || inv.Kind != kind {
				return nil, gqlMissingNode(enterpriseInvitationTypeName(kind), nodeID)
			}
			e := s.store.GetEnterpriseByID(inv.EnterpriseID)
			rendered := s.enterpriseInvitationToGraphQL(inv)
			message := ""
			if accept {
				viewer := s.ghUserFromContext(p.Context)
				if s.store.AcceptEnterpriseInvitation(inv.ID, viewer.ID) == nil {
					return nil, gqlMissingNode(enterpriseInvitationTypeName(kind), nodeID)
				}
				message = viewer.Login + " joined " + e.Slug
				s.recordEnterpriseAudit(p, e, "business.accept_"+kind+"_invitation", map[string]interface{}{"user": viewer.Login})
			} else {
				if !s.store.DeleteEnterpriseInvitation(inv.ID) {
					return nil, gqlMissingNode(enterpriseInvitationTypeName(kind), nodeID)
				}
				message = "Invitation cancelled"
				s.recordEnterpriseAudit(p, e, "business.cancel_"+kind+"_invitation", map[string]interface{}{"invitation_id": inv.ID})
			}
			return map[string]interface{}{"invitation": rendered, "message": message}, nil
		},
	})
}

func (s *Resolver) registerEnterpriseSupportEntitlementMutation(mutationType *graphql.Object, name string, grant bool) {
	inputName := strings.ToUpper(name[:1]) + name[1:] + "Input"
	payloadName := strings.ToUpper(name[:1]) + name[1:] + "Payload"
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: inputName,
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"login":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name:   payloadName,
		Fields: graphql.Fields{"message": &graphql.Field{Type: graphql.String}},
	})
	s.registerMutation(mutationType, name, &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			e, user, err := s.enterpriseAndLogin(input, "login")
			if err != nil {
				return nil, err
			}
			// A support entitlement belongs to a membership, so grant creates a
			// member row first rather than let the entitlement dangle.
			if s.store.GetEnterpriseMembership(e.ID, user.ID) == nil {
				if !grant {
					return nil, fmt.Errorf("%s has no support entitlement in %s", user.Login, e.Slug)
				}
				s.store.SetEnterpriseMembership(e.ID, user.ID, store.EnterpriseRoleMember)
			}
			if !s.store.SetEnterpriseSupportEntitlement(e.ID, user.ID, grant) {
				return nil, gqlMissingNode("Enterprise", e.NodeID)
			}
			action, message := "business.add_support_entitlee", "Support entitlement granted to "+user.Login
			if !grant {
				action, message = "business.remove_support_entitlee", "Support entitlement removed from "+user.Login
			}
			s.recordEnterpriseAudit(p, e, action, map[string]interface{}{"user": user.Login})
			return map[string]interface{}{"message": message}, nil
		},
	})
}

func (s *Resolver) registerEnterpriseMigratorRoleMutation(mutationType *graphql.Object, name string, grant bool) {
	inputName := strings.ToUpper(name[:1]) + name[1:] + "Input"
	payloadName := strings.ToUpper(name[:1]) + name[1:] + "Payload"
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: inputName,
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"login":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	// The payload names every organization the grant reached — the whole
	// enterprise — so the caller learns the blast radius.
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: payloadName,
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"organizations": &graphql.Field{
				Type: s.graphqlTypes.organizationConnection,
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, _ := p.Source.(map[string]interface{})
					nodes, _ := source["_organizations"].([]map[string]interface{})
					return paginateGQLMaps(nodes, p.Args), nil
				},
			},
		},
	})
	s.registerMutation(mutationType, name, &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			e, user, err := s.enterpriseAndLogin(input, "login")
			if err != nil {
				return nil, err
			}
			if !s.store.SetEnterpriseMigratorRole(e.ID, user.Login, grant) {
				return nil, gqlMissingNode("Enterprise", e.NodeID)
			}
			action := "business.revoke_migrator_role"
			if grant {
				action = "business.grant_migrator_role"
			}
			s.recordEnterpriseAudit(p, e, action, map[string]interface{}{"user": user.Login})
			reached := make([]map[string]interface{}, 0)
			for _, orgID := range s.store.ListEnterpriseOrgIDs(e.ID) {
				if org := s.store.GetOrgByID(orgID); org != nil {
					reached = append(reached, orgToGraphQL(org))
				}
			}
			return map[string]interface{}{"_organizations": reached}, nil
		},
	})
}

// --- organizations ---------------------------------------------------------

func (s *Resolver) addEnterpriseOrganizationMutations(mutationType, enterpriseType, orgType, userType *graphql.Object) {
	// createEnterpriseOrganization
	createInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateEnterpriseOrganizationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"adminLogins":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
			"billingEmail": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"login":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"profileName":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	createPayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateEnterpriseOrganizationPayload",
		Fields: graphql.Fields{
			"enterprise":   &graphql.Field{Type: enterpriseType},
			"organization": &graphql.Field{Type: orgType},
		},
	})
	s.registerMutation(mutationType, "createEnterpriseOrganization", &graphql.Field{
		Type: createPayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			login, _ := input["login"].(string)
			profileName, _ := input["profileName"].(string)
			billingEmail, _ := input["billingEmail"].(string)
			admins := stringListArg(input["adminLogins"])
			if len(admins) == 0 {
				return nil, fmt.Errorf("an enterprise organization needs at least one administrator")
			}
			adminUsers := make([]*store.User, 0, len(admins))
			for _, adminLogin := range admins {
				admin := s.store.LookupUserByLogin(adminLogin)
				if admin == nil {
					return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a User with the login of '%s'.", adminLogin)}
				}
				adminUsers = append(adminUsers, admin)
			}
			if s.store.GetOrg(login) != nil {
				return nil, fmt.Errorf("an account named %q already exists", login)
			}
			org := s.store.CreateOrg(adminUsers[0], login, profileName, "")
			if org == nil {
				return nil, fmt.Errorf("could not create the organization %q", login)
			}
			s.store.UpdateOrg(org.Login, func(o *store.Org) { o.BillingEmail = billingEmail })
			for _, admin := range adminUsers[1:] {
				s.store.SetMembership(org.Login, admin.ID, store.OrgRoleAdmin, store.MembershipStateActive)
			}
			if !s.store.AddEnterpriseOrganization(e.ID, org.ID) {
				return nil, fmt.Errorf("the organization %q already belongs to another enterprise", login)
			}
			s.recordEnterpriseAudit(p, e, "business.create_organization", map[string]interface{}{"org": org.Login})
			return map[string]interface{}{
				"enterprise":   optionalObject(enterpriseToGraphQL(s.store.GetEnterpriseByID(e.ID))),
				"organization": orgToGraphQL(s.store.GetOrg(org.Login)),
			}, nil
		},
	})

	// removeEnterpriseOrganization
	removeInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RemoveEnterpriseOrganizationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"organizationId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	removePayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "RemoveEnterpriseOrganizationPayload",
		Fields: graphql.Fields{
			"enterprise":   &graphql.Field{Type: enterpriseType},
			"organization": &graphql.Field{Type: orgType},
			"viewer":       &graphql.Field{Type: userType},
		},
	})
	s.registerMutation(mutationType, "removeEnterpriseOrganization", &graphql.Field{
		Type: removePayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(removeInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			orgNodeID, _ := input["organizationId"].(string)
			org := s.orgByNodeID(orgNodeID)
			if org == nil {
				return nil, gqlMissingNode("Organization", orgNodeID)
			}
			if !s.store.RemoveEnterpriseOrganization(e.ID, org.ID) {
				return nil, fmt.Errorf("%s does not belong to %s", org.Login, e.Slug)
			}
			s.recordEnterpriseAudit(p, e, "business.remove_organization", map[string]interface{}{"org": org.Login})
			return map[string]interface{}{
				"enterprise":   optionalObject(enterpriseToGraphQL(s.store.GetEnterpriseByID(e.ID))),
				"organization": orgToGraphQL(org),
				"viewer":       userToGraphQL(s.ghUserFromContext(p.Context)),
			}, nil
		},
	})

	// transferEnterpriseOrganization
	transferInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "TransferEnterpriseOrganizationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"destinationEnterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"organizationId":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	transferPayload := graphql.NewObject(graphql.ObjectConfig{
		Name:   "TransferEnterpriseOrganizationPayload",
		Fields: graphql.Fields{"organization": &graphql.Field{Type: orgType}},
	})
	s.registerMutation(mutationType, "transferEnterpriseOrganization", &graphql.Field{
		Type: transferPayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(transferInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			destNodeID, _ := input["destinationEnterpriseId"].(string)
			destination := store.FindEnterpriseByNodeID(s.store, destNodeID)
			if destination == nil {
				return nil, gqlMissingNode("Enterprise", destNodeID)
			}
			orgNodeID, _ := input["organizationId"].(string)
			org := s.orgByNodeID(orgNodeID)
			if org == nil {
				return nil, gqlMissingNode("Organization", orgNodeID)
			}
			if !s.store.TransferEnterpriseOrganization(org.ID, destination.ID) {
				return nil, fmt.Errorf("%s does not belong to an enterprise", org.Login)
			}
			s.recordEnterpriseAudit(p, destination, "business.transfer_organization", map[string]interface{}{"org": org.Login})
			return map[string]interface{}{"organization": orgToGraphQL(org)}, nil
		},
	})

	// addEnterpriseOrganizationMember
	addMemberInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AddEnterpriseOrganizationMemberInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"organizationId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"role":           &graphql.InputObjectFieldConfig{Type: s.sharedEnum("OrganizationMemberRole", "ADMIN", "MEMBER")},
			"userIds":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.ID)))},
		},
	})
	addMemberPayload := graphql.NewObject(graphql.ObjectConfig{
		Name:   "AddEnterpriseOrganizationMemberPayload",
		Fields: graphql.Fields{"users": &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(userType))}},
	})
	s.registerMutation(mutationType, "addEnterpriseOrganizationMember", &graphql.Field{
		Type: addMemberPayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addMemberInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			orgNodeID, _ := input["organizationId"].(string)
			org := s.orgByNodeID(orgNodeID)
			if org == nil {
				return nil, gqlMissingNode("Organization", orgNodeID)
			}
			// The org must be one this enterprise owns.
			if s.store.EnterpriseIDForOrg(org.ID) != e.ID {
				return nil, fmt.Errorf("%s does not belong to %s", org.Login, e.Slug)
			}
			role := store.OrgRoleMember
			if value, _ := input["role"].(string); value == "ADMIN" {
				role = store.OrgRoleAdmin
			}
			var added []interface{}
			for _, userNodeID := range stringListArg(input["userIds"]) {
				user := store.FindUserByNodeID(s.store, userNodeID)
				if user == nil {
					return nil, gqlMissingNode("User", userNodeID)
				}
				s.store.SetMembership(org.Login, user.ID, role, store.MembershipStateActive)
				added = append(added, userToGraphQL(user))
			}
			s.recordEnterpriseAudit(p, e, "business.add_organization_member", map[string]interface{}{
				"org": org.Login, "count": len(added), "role": string(role),
			})
			return map[string]interface{}{"users": added}, nil
		},
	})

	// updateEnterpriseOwnerOrganizationRole
	ownerRoleInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateEnterpriseOwnerOrganizationRoleInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"organizationId":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"organizationRole": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.sharedEnum("RoleInOrganization", "DIRECT_MEMBER", "OWNER", "UNAFFILIATED"))},
		},
	})
	ownerRolePayload := graphql.NewObject(graphql.ObjectConfig{
		Name:   "UpdateEnterpriseOwnerOrganizationRolePayload",
		Fields: graphql.Fields{"message": &graphql.Field{Type: graphql.String}},
	})
	s.registerMutation(mutationType, "updateEnterpriseOwnerOrganizationRole", &graphql.Field{
		Type: ownerRolePayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(ownerRoleInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			orgNodeID, _ := input["organizationId"].(string)
			org := s.orgByNodeID(orgNodeID)
			if org == nil || s.store.EnterpriseIDForOrg(org.ID) != e.ID {
				return nil, gqlMissingNode("Organization", orgNodeID)
			}
			role, _ := input["organizationRole"].(string)
			viewer := s.ghUserFromContext(p.Context)
			switch role {
			case "OWNER":
				s.store.SetMembership(org.Login, viewer.ID, store.OrgRoleAdmin, store.MembershipStateActive)
			case "DIRECT_MEMBER":
				s.store.SetMembership(org.Login, viewer.ID, store.OrgRoleMember, store.MembershipStateActive)
			case "UNAFFILIATED":
				s.store.RemoveMembership(org.Login, viewer.ID)
			}
			s.recordEnterpriseAudit(p, e, "business.update_owner_organization_role", map[string]interface{}{
				"org": org.Login, "role": role,
			})
			return map[string]interface{}{
				"message": viewer.Login + " is now " + strings.ToLower(strings.ReplaceAll(role, "_", " ")) + " in " + org.Login,
			}, nil
		},
	})
}

// stringListArg coerces a GraphQL [String!]!/[ID!]! argument to a Go slice.
func stringListArg(value interface{}) []string {
	raw, ok := value.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- identity provider -----------------------------------------------------

func (s *Resolver) addEnterpriseIdentityProviderMutations(mutationType *graphql.Object) {
	identityProviderType := s.graphqlTypes.enterpriseIdentityProvide
	uri := s.graphQLStringScalar("URI")

	setInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "SetEnterpriseIdentityProviderInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"digestMethod":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.sharedEnum("SamlDigestAlgorithm", "SHA1", "SHA256", "SHA384", "SHA512"))},
			"idpCertificate":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"issuer":          &graphql.InputObjectFieldConfig{Type: graphql.String},
			"signatureMethod": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.sharedEnum("SamlSignatureAlgorithm", "RSA_SHA1", "RSA_SHA256", "RSA_SHA384", "RSA_SHA512"))},
			"ssoUrl":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(uri)},
		},
	})
	identityPayload := func(name string) *graphql.Object {
		return graphql.NewObject(graphql.ObjectConfig{
			Name:   name,
			Fields: graphql.Fields{"identityProvider": &graphql.Field{Type: identityProviderType}},
		})
	}
	s.registerMutation(mutationType, "setEnterpriseIdentityProvider", &graphql.Field{
		Type: identityPayload("SetEnterpriseIdentityProviderPayload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(setInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			ssoURL, _ := input["ssoUrl"].(string)
			issuer, _ := input["issuer"].(string)
			certificate, _ := input["idpCertificate"].(string)
			signature, _ := input["signatureMethod"].(string)
			digest, _ := input["digestMethod"].(string)
			codes := enterpriseRecoveryCodes()
			if existing := s.store.GetEnterpriseByID(e.ID); existing != nil && existing.IdentityProvider != nil {
				// Rebinding keeps the existing recovery codes; only
				// regenerateEnterpriseIdentityProviderRecoveryCodes replaces them.
				codes = existing.IdentityProvider.RecoveryCodes
			}
			if s.store.SetEnterpriseIdentityProvider(e.ID, ssoURL, issuer, certificate, signature, digest, codes) == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			s.recordEnterpriseAudit(p, e, "business.set_identity_provider", map[string]interface{}{"sso_url": ssoURL, "issuer": issuer})
			return map[string]interface{}{
				"identityProvider": optionalObject(enterpriseIdentityProviderToGraphQL(s.store.GetEnterpriseByID(e.ID), false)),
			}, nil
		},
	})

	removeInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RemoveEnterpriseIdentityProviderInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	s.registerMutation(mutationType, "removeEnterpriseIdentityProvider", &graphql.Field{
		Type: identityPayload("RemoveEnterpriseIdentityProviderPayload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(removeInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			removed := s.store.RemoveEnterpriseIdentityProvider(e.ID)
			if removed == nil {
				return map[string]interface{}{"identityProvider": nil}, nil
			}
			s.recordEnterpriseAudit(p, e, "business.remove_identity_provider", nil)
			return map[string]interface{}{"identityProvider": map[string]interface{}{
				"__typename":      "EnterpriseIdentityProvider",
				"nodeID":          removed.NodeID,
				"_enterprise":     e.ID,
				"ssoUrl":          nullableString(removed.SSOURL),
				"issuer":          nullableString(removed.Issuer),
				"idpCertificate":  nullableString(removed.IDPCertificate),
				"signatureMethod": nullableString(removed.SignatureMethod),
				"digestMethod":    nullableString(removed.DigestMethod),
				"recoveryCodes":   nil,
			}}, nil
		},
	})

	regenerateInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RegenerateEnterpriseIdentityProviderRecoveryCodesInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"enterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	s.registerMutation(mutationType, "regenerateEnterpriseIdentityProviderRecoveryCodes", &graphql.Field{
		Type: identityPayload("RegenerateEnterpriseIdentityProviderRecoveryCodesPayload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(regenerateInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["enterpriseId"].(string)
			e := store.FindEnterpriseByNodeID(s.store, nodeID)
			if e == nil {
				return nil, gqlMissingNode("Enterprise", nodeID)
			}
			if s.store.RegenerateEnterpriseIdentityProviderRecoveryCodes(e.ID, enterpriseRecoveryCodes()) == nil {
				return nil, fmt.Errorf("%s has no identity provider", e.Slug)
			}
			s.recordEnterpriseAudit(p, e, "business.regenerate_identity_provider_recovery_codes", nil)
			return map[string]interface{}{
				"identityProvider": optionalObject(enterpriseIdentityProviderToGraphQL(s.store.GetEnterpriseByID(e.ID), false)),
			}, nil
		},
	})
}

// enterpriseRecoveryCodes mints the ten single-use crypto/rand codes GitHub
// issues when an enterprise binds an identity provider.
func enterpriseRecoveryCodes() []string {
	codes := make([]string, 10)
	for i := range codes {
		buf := make([]byte, 5)
		if _, err := rand.Read(buf); err != nil {
			// An unreadable entropy source is fatal, never a weaker code.
			panic("enterprise recovery codes: " + err.Error())
		}
		encoded := hex.EncodeToString(buf)
		codes[i] = encoded[:5] + "-" + encoded[5:]
	}
	sort.Strings(codes)
	return codes
}

// --- IP allow list ---------------------------------------------------------

func (s *Resolver) addIPAllowListMutations(mutationType *graphql.Object) {
	entryType := s.graphqlTypes.ipAllowListEntry
	ownerUnion := s.graphqlTypes.ipAllowListOwner

	entryPayload := func(name string) *graphql.Object {
		return graphql.NewObject(graphql.ObjectConfig{
			Name:   name,
			Fields: graphql.Fields{"ipAllowListEntry": &graphql.Field{Type: entryType}},
		})
	}

	createInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateIpAllowListEntryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"allowListValue": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"isActive":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Boolean)},
			"name":           &graphql.InputObjectFieldConfig{Type: graphql.String},
			"ownerId":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	s.registerMutation(mutationType, "createIpAllowListEntry", &graphql.Field{
		Type: entryPayload("CreateIpAllowListEntryPayload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			ownerType, ownerID, _, err := (ipAllowListOwnerRule{ownerKey: "ownerId"}).resolveOwner(s, input)
			if err != nil {
				return nil, err
			}
			value, _ := input["allowListValue"].(string)
			if !validIPAllowListValue(value) {
				return nil, fmt.Errorf("%q is not an IP address or CIDR range", value)
			}
			name, _ := input["name"].(string)
			isActive, _ := input["isActive"].(bool)
			entry := s.store.CreateIPAllowListEntry(ownerType, ownerID, value, name, isActive)
			if ownerType == "Enterprise" {
				s.recordEnterpriseAudit(p, s.store.GetEnterpriseByID(ownerID), "business.create_ip_allow_list_entry",
					map[string]interface{}{"allow_list_value": value})
			}
			return map[string]interface{}{"ipAllowListEntry": ipAllowListEntryToGraphQL(entry)}, nil
		},
	})

	updateInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateIpAllowListEntryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"allowListValue":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"ipAllowListEntryId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"isActive":           &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Boolean)},
			"name":               &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "updateIpAllowListEntry", &graphql.Field{
		Type: entryPayload("UpdateIpAllowListEntryPayload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["ipAllowListEntryId"].(string)
			existing := store.FindIPAllowListEntryByNodeID(s.store, nodeID)
			if existing == nil {
				return nil, gqlMissingNode("IpAllowListEntry", nodeID)
			}
			value, _ := input["allowListValue"].(string)
			if !validIPAllowListValue(value) {
				return nil, fmt.Errorf("%q is not an IP address or CIDR range", value)
			}
			name, _ := input["name"].(string)
			isActive, _ := input["isActive"].(bool)
			entry := s.store.UpdateIPAllowListEntry(existing.ID, value, name, isActive)
			return map[string]interface{}{"ipAllowListEntry": ipAllowListEntryToGraphQL(entry)}, nil
		},
	})

	deleteInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DeleteIpAllowListEntryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"ipAllowListEntryId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	s.registerMutation(mutationType, "deleteIpAllowListEntry", &graphql.Field{
		Type: entryPayload("DeleteIpAllowListEntryPayload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(deleteInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["ipAllowListEntryId"].(string)
			existing := store.FindIPAllowListEntryByNodeID(s.store, nodeID)
			if existing == nil {
				return nil, gqlMissingNode("IpAllowListEntry", nodeID)
			}
			removed := s.store.DeleteIPAllowListEntry(existing.ID)
			return map[string]interface{}{"ipAllowListEntry": ipAllowListEntryToGraphQL(removed)}, nil
		},
	})

	// The three owner-level IP allow list toggles.
	s.registerIPAllowListSettingMutation(mutationType, ownerUnion,
		"updateIpAllowListEnabledSetting", s.ipAllowListEnabledEnum(),
		func(policy *store.EnterprisePolicy, value string) { policy.IPAllowListEnabled = value })
	s.registerIPAllowListSettingMutation(mutationType, ownerUnion,
		"updateIpAllowListForInstalledAppsEnabledSetting", s.ipAllowListForInstalledAppsEnum(),
		func(policy *store.EnterprisePolicy, value string) { policy.IPAllowListForInstalledAppsEnabled = value })
	s.registerIPAllowListSettingMutation(mutationType, ownerUnion,
		"updateIpAllowListUserLevelEnforcementEnabledSetting", s.ipAllowListUserLevelEnforcementEnum(),
		func(policy *store.EnterprisePolicy, value string) {
			policy.IPAllowListUserLevelEnforcementEnabled = value
		})
}

func (s *Resolver) registerIPAllowListSettingMutation(mutationType *graphql.Object, ownerUnion *graphql.Union, name string, enum *graphql.Enum, apply func(*store.EnterprisePolicy, string)) {
	inputName := strings.ToUpper(name[:1]) + name[1:] + "Input"
	payloadName := strings.ToUpper(name[:1]) + name[1:] + "Payload"
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: inputName,
		Fields: graphql.InputObjectConfigFieldMap{
			"ownerId":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"settingValue": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(enum)},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name:   payloadName,
		Fields: graphql.Fields{"owner": &graphql.Field{Type: ownerUnion}},
	})
	s.registerMutation(mutationType, name, &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			ownerType, ownerID, nodeID, err := (ipAllowListOwnerRule{ownerKey: "ownerId"}).resolveOwner(s, input)
			if err != nil {
				return nil, err
			}
			value, _ := input["settingValue"].(string)
			if ownerType != "Enterprise" {
				// bleephub stores the toggle on the enterprise that owns the org.
				org := s.store.GetOrgByID(ownerID)
				if org == nil {
					return nil, gqlMissingNode("IpAllowListOwner", nodeID)
				}
				enterpriseID := s.store.EnterpriseIDForOrg(org.ID)
				if enterpriseID == 0 {
					return nil, fmt.Errorf("%s does not belong to an enterprise", org.Login)
				}
				ownerID = enterpriseID
			}
			updated := s.store.UpdateEnterprisePolicy(ownerID, func(policy *store.EnterprisePolicy) { apply(policy, value) })
			if updated == nil {
				return nil, gqlMissingNode("IpAllowListOwner", nodeID)
			}
			s.recordEnterpriseAudit(p, updated, "business."+name, map[string]interface{}{"setting_value": value})
			return map[string]interface{}{"owner": enterpriseToGraphQL(updated)}, nil
		},
	})
}

// validIPAllowListValue reports whether a value is an IP address or CIDR range,
// rejecting an entry that could never match at write time.
func validIPAllowListValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "/") {
		_, _, err := net.ParseCIDR(value)
		return err == nil
	}
	return net.ParseIP(value) != nil
}
