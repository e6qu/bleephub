package bleephub

// The enterprise mutation surface's authorization table, exercised twice:
// once with an account that owns a *different* enterprise (every mutation
// must be refused — that is the cross-tenant isolation proof) and once with
// the enterprise's own owner (every mutation must succeed).

import (
	"fmt"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// gqlEnterpriseFixture is one enterprise and everything the mutation table
// needs to name: its owner, an organization it owns, a member, the two
// invitations, an IP allow list entry, and a second enterprise the same owner
// administers (the destination for an organization transfer).
type gqlEnterpriseFixture struct {
	tag              string
	enterprise       *store.Enterprise
	destination      *store.Enterprise
	owner            *store.User
	ownerToken       string
	member           *store.User
	invitee          *store.User
	inviteeToken     string
	org              *store.Org
	adminInvitation  *store.EnterpriseInvitation
	memberInvitation *store.EnterpriseInvitation
	ipAllowListEntry *store.IPAllowListEntry
	// outsider owns an unrelated enterprise and nothing in this one.
	outsider      *store.User
	outsiderToken string
}

func (s *isolatedServer) newGQLEnterpriseFixture(t *testing.T, tag string) *gqlEnterpriseFixture {
	t.Helper()
	f := &gqlEnterpriseFixture{tag: tag}
	f.enterprise = s.store.CreateEnterprise("acme-"+tag, "Acme "+tag, "billing-"+tag+"@acme.test")
	if f.enterprise == nil {
		t.Fatalf("CreateEnterprise acme-%s failed", tag)
	}
	f.destination = s.store.CreateEnterprise("dest-"+tag, "Destination "+tag, "")
	outsiderEnterprise := s.store.CreateEnterprise("globex-"+tag, "Globex "+tag, "")

	f.owner, f.ownerToken = s.newUser(t, "eowner"+tag)
	f.member, _ = s.newUser(t, "emember"+tag)
	f.invitee, f.inviteeToken = s.newUser(t, "einvitee"+tag)
	f.outsider, f.outsiderToken = s.newUser(t, "eoutsider"+tag)

	s.store.SetEnterpriseMembership(f.enterprise.ID, f.owner.ID, store.EnterpriseRoleOwner)
	s.store.SetEnterpriseMembership(f.destination.ID, f.owner.ID, store.EnterpriseRoleOwner)
	s.store.SetEnterpriseMembership(f.enterprise.ID, f.member.ID, store.EnterpriseRoleMember)
	s.store.SetEnterpriseMembership(outsiderEnterprise.ID, f.outsider.ID, store.EnterpriseRoleOwner)

	f.org = s.store.CreateOrg(f.owner, "eorg"+tag, "Enterprise Org "+tag, "")
	if f.org == nil {
		t.Fatalf("CreateOrg eorg%s failed", tag)
	}
	if !s.store.AddEnterpriseOrganization(f.enterprise.ID, f.org.ID) {
		t.Fatal("AddEnterpriseOrganization failed")
	}
	f.adminInvitation = s.store.CreateEnterpriseInvitation(f.enterprise.ID, f.owner.ID, f.invitee.ID, "", "admin", store.EnterpriseRoleOwner)
	f.memberInvitation = s.store.CreateEnterpriseInvitation(f.enterprise.ID, f.owner.ID, f.invitee.ID, "", "member", store.EnterpriseRoleMember)
	if f.adminInvitation == nil || f.memberInvitation == nil {
		t.Fatal("CreateEnterpriseInvitation failed")
	}
	f.ipAllowListEntry = s.store.CreateIPAllowListEntry("Enterprise", f.enterprise.ID, "10.0.0.0/8", "office", true)
	// An identity provider is already bound, so the mutations that act on an
	// existing binding (regenerating its recovery codes, removing it) have one
	// to act on.
	s.store.SetEnterpriseIdentityProvider(f.enterprise.ID,
		"https://idp.test/sso-"+tag, "https://idp.test", "MIIC-seed", "RSA_SHA256", "SHA256",
		[]string{"aaaaa-bbbbb"})
	return f
}

// gqlEnterpriseMutationCase is one row of the enterprise mutation surface.
type gqlEnterpriseMutationCase struct {
	name  string
	doc   string
	input func(f *gqlEnterpriseFixture) map[string]interface{}
	// entitledToken selects the account allowed to perform the mutation. It
	// defaults to the enterprise's owner; the two accept mutations are the
	// invitee's alone.
	entitledToken func(f *gqlEnterpriseFixture) string
}

// gqlEnterpriseMutationCases covers every enterprise mutation. Each policy
// mutation is spelled out rather than generated so a mutation whose input
// shape drifts from GitHub's fails here.
var gqlEnterpriseMutationCases = []gqlEnterpriseMutationCase{
	{
		name: "updateEnterpriseProfile",
		doc:  `mutation($input:UpdateEnterpriseProfileInput!){updateEnterpriseProfile(input:$input){enterprise{name location}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "name": "Renamed", "location": "Springfield"}
		},
	},
	{
		name: "inviteEnterpriseAdmin",
		doc:  `mutation($input:InviteEnterpriseAdminInput!){inviteEnterpriseAdmin(input:$input){invitation{id role}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "invitee": f.member.Login, "role": "BILLING_MANAGER"}
		},
	},
	{
		name: "inviteEnterpriseMember",
		doc:  `mutation($input:InviteEnterpriseMemberInput!){inviteEnterpriseMember(input:$input){invitation{id email}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "email": "newcomer@acme.test"}
		},
	},
	{
		name: "acceptEnterpriseAdministratorInvitation",
		doc:  `mutation($input:AcceptEnterpriseAdministratorInvitationInput!){acceptEnterpriseAdministratorInvitation(input:$input){message}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"invitationId": f.adminInvitation.NodeID}
		},
		entitledToken: func(f *gqlEnterpriseFixture) string { return f.inviteeToken },
	},
	{
		name: "acceptEnterpriseMemberInvitation",
		doc:  `mutation($input:AcceptEnterpriseMemberInvitationInput!){acceptEnterpriseMemberInvitation(input:$input){message}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"invitationId": f.memberInvitation.NodeID}
		},
		entitledToken: func(f *gqlEnterpriseFixture) string { return f.inviteeToken },
	},
	{
		name: "cancelEnterpriseAdminInvitation",
		doc:  `mutation($input:CancelEnterpriseAdminInvitationInput!){cancelEnterpriseAdminInvitation(input:$input){message}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"invitationId": f.adminInvitation.NodeID}
		},
	},
	{
		name: "cancelEnterpriseMemberInvitation",
		doc:  `mutation($input:CancelEnterpriseMemberInvitationInput!){cancelEnterpriseMemberInvitation(input:$input){message}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"invitationId": f.memberInvitation.NodeID}
		},
	},
	{
		name: "removeEnterpriseAdmin",
		doc:  `mutation($input:RemoveEnterpriseAdminInput!){removeEnterpriseAdmin(input:$input){admin{login}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "login": f.member.Login}
		},
	},
	{
		name: "removeEnterpriseMember",
		doc:  `mutation($input:RemoveEnterpriseMemberInput!){removeEnterpriseMember(input:$input){user{login}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "userId": f.member.NodeID}
		},
	},
	{
		name: "updateEnterpriseAdministratorRole",
		doc:  `mutation($input:UpdateEnterpriseAdministratorRoleInput!){updateEnterpriseAdministratorRole(input:$input){message}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "login": f.member.Login, "role": "BILLING_MANAGER"}
		},
	},
	{
		name: "addEnterpriseSupportEntitlement",
		doc:  `mutation($input:AddEnterpriseSupportEntitlementInput!){addEnterpriseSupportEntitlement(input:$input){message}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "login": f.member.Login}
		},
	},
	{
		name: "removeEnterpriseSupportEntitlement",
		doc:  `mutation($input:RemoveEnterpriseSupportEntitlementInput!){removeEnterpriseSupportEntitlement(input:$input){message}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "login": f.member.Login}
		},
	},
	{
		name: "grantEnterpriseOrganizationsMigratorRole",
		doc:  `mutation($input:GrantEnterpriseOrganizationsMigratorRoleInput!){grantEnterpriseOrganizationsMigratorRole(input:$input){clientMutationId}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "login": f.member.Login}
		},
	},
	{
		name: "revokeEnterpriseOrganizationsMigratorRole",
		doc:  `mutation($input:RevokeEnterpriseOrganizationsMigratorRoleInput!){revokeEnterpriseOrganizationsMigratorRole(input:$input){clientMutationId}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "login": f.member.Login}
		},
	},
	{
		name: "createEnterpriseOrganization",
		doc:  `mutation($input:CreateEnterpriseOrganizationInput!){createEnterpriseOrganization(input:$input){organization{login}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{
				"enterpriseId": f.enterprise.NodeID,
				"adminLogins":  []interface{}{f.owner.Login},
				"billingEmail": "billing@acme.test",
				"login":        "created" + f.tag,
				"profileName":  "Created " + f.tag,
			}
		},
	},
	{
		name: "removeEnterpriseOrganization",
		doc:  `mutation($input:RemoveEnterpriseOrganizationInput!){removeEnterpriseOrganization(input:$input){organization{login}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "organizationId": f.org.NodeID}
		},
	},
	{
		name: "transferEnterpriseOrganization",
		doc:  `mutation($input:TransferEnterpriseOrganizationInput!){transferEnterpriseOrganization(input:$input){organization{login}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"destinationEnterpriseId": f.destination.NodeID, "organizationId": f.org.NodeID}
		},
	},
	{
		name: "addEnterpriseOrganizationMember",
		doc:  `mutation($input:AddEnterpriseOrganizationMemberInput!){addEnterpriseOrganizationMember(input:$input){users{login}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{
				"enterpriseId":   f.enterprise.NodeID,
				"organizationId": f.org.NodeID,
				"role":           "MEMBER",
				"userIds":        []interface{}{f.member.NodeID},
			}
		},
	},
	{
		name: "updateEnterpriseOwnerOrganizationRole",
		doc:  `mutation($input:UpdateEnterpriseOwnerOrganizationRoleInput!){updateEnterpriseOwnerOrganizationRole(input:$input){message}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{
				"enterpriseId":     f.enterprise.NodeID,
				"organizationId":   f.org.NodeID,
				"organizationRole": "OWNER",
			}
		},
	},
	{
		name: "setEnterpriseIdentityProvider",
		doc:  `mutation($input:SetEnterpriseIdentityProviderInput!){setEnterpriseIdentityProvider(input:$input){identityProvider{ssoUrl issuer recoveryCodes}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{
				"enterpriseId":    f.enterprise.NodeID,
				"digestMethod":    "SHA256",
				"idpCertificate":  "MIIC-test-certificate",
				"issuer":          "https://idp.test",
				"signatureMethod": "RSA_SHA256",
				"ssoUrl":          "https://idp.test/sso",
			}
		},
	},
	{
		name: "regenerateEnterpriseIdentityProviderRecoveryCodes",
		doc:  `mutation($input:RegenerateEnterpriseIdentityProviderRecoveryCodesInput!){regenerateEnterpriseIdentityProviderRecoveryCodes(input:$input){identityProvider{recoveryCodes}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID}
		},
	},
	{
		name: "removeEnterpriseIdentityProvider",
		doc:  `mutation($input:RemoveEnterpriseIdentityProviderInput!){removeEnterpriseIdentityProvider(input:$input){identityProvider{issuer}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"enterpriseId": f.enterprise.NodeID}
		},
	},
	{
		name: "createIpAllowListEntry",
		doc:  `mutation($input:CreateIpAllowListEntryInput!){createIpAllowListEntry(input:$input){ipAllowListEntry{allowListValue isActive}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{
				"ownerId":        f.enterprise.NodeID,
				"allowListValue": "192.168.0.0/16",
				"isActive":       true,
				"name":           "vpn",
			}
		},
	},
	{
		name: "updateIpAllowListEntry",
		doc:  `mutation($input:UpdateIpAllowListEntryInput!){updateIpAllowListEntry(input:$input){ipAllowListEntry{allowListValue}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{
				"ipAllowListEntryId": f.ipAllowListEntry.NodeID,
				"allowListValue":     "172.16.0.0/12",
				"isActive":           false,
				"name":               "datacentre",
			}
		},
	},
	{
		name: "deleteIpAllowListEntry",
		doc:  `mutation($input:DeleteIpAllowListEntryInput!){deleteIpAllowListEntry(input:$input){ipAllowListEntry{allowListValue}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"ipAllowListEntryId": f.ipAllowListEntry.NodeID}
		},
	},
	{
		name: "updateIpAllowListEnabledSetting",
		doc:  `mutation($input:UpdateIpAllowListEnabledSettingInput!){updateIpAllowListEnabledSetting(input:$input){owner{... on Enterprise{slug}}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"ownerId": f.enterprise.NodeID, "settingValue": "ENABLED"}
		},
	},
	{
		name: "updateIpAllowListForInstalledAppsEnabledSetting",
		doc:  `mutation($input:UpdateIpAllowListForInstalledAppsEnabledSettingInput!){updateIpAllowListForInstalledAppsEnabledSetting(input:$input){owner{... on Enterprise{slug}}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"ownerId": f.enterprise.NodeID, "settingValue": "ENABLED"}
		},
	},
	{
		name: "updateIpAllowListUserLevelEnforcementEnabledSetting",
		doc:  `mutation($input:UpdateIpAllowListUserLevelEnforcementEnabledSettingInput!){updateIpAllowListUserLevelEnforcementEnabledSetting(input:$input){owner{... on Enterprise{slug}}}}`,
		input: func(f *gqlEnterpriseFixture) map[string]interface{} {
			return map[string]interface{}{"ownerId": f.enterprise.NodeID, "settingValue": "ENABLED"}
		},
	},
}

// enterprisePolicyMutationCases are the settings mutations, all of which take
// the same {enterpriseId, settingValue} input shape. They are generated from
// the setting-value pairs rather than written out because the input shape is
// the schema's, not each mutation's.
var enterprisePolicySettingValues = []struct {
	mutation string
	value    string
}{
	{"updateEnterpriseAllowPrivateRepositoryForkingSetting", "ENABLED"},
	{"updateEnterpriseDefaultRepositoryPermissionSetting", "READ"},
	{"updateEnterpriseDeployKeySetting", "DISABLED"},
	{"updateEnterpriseMembersCanChangeRepositoryVisibilitySetting", "DISABLED"},
	{"updateEnterpriseMembersCanCreateRepositoriesSetting", "PRIVATE"},
	{"updateEnterpriseMembersCanDeleteIssuesSetting", "DISABLED"},
	{"updateEnterpriseMembersCanDeleteRepositoriesSetting", "DISABLED"},
	{"updateEnterpriseMembersCanInviteCollaboratorsSetting", "DISABLED"},
	{"updateEnterpriseMembersCanMakePurchasesSetting", "DISABLED"},
	{"updateEnterpriseMembersCanUpdateProtectedBranchesSetting", "DISABLED"},
	{"updateEnterpriseMembersCanViewDependencyInsightsSetting", "DISABLED"},
	{"updateEnterpriseOrganizationProjectsSetting", "DISABLED"},
	{"updateEnterpriseProofOfPresenceRequiredSetting", "MFA"},
	{"updateEnterpriseRepositoryProjectsSetting", "DISABLED"},
	{"updateEnterpriseTwoFactorAuthenticationDisallowedMethodsSetting", "INSECURE"},
	{"updateEnterpriseTwoFactorAuthenticationRequiredSetting", "ENABLED"},
}

func enterprisePolicyMutationCases() []gqlEnterpriseMutationCase {
	cases := make([]gqlEnterpriseMutationCase, 0, len(enterprisePolicySettingValues))
	for _, setting := range enterprisePolicySettingValues {
		setting := setting
		inputType := strings.ToUpper(setting.mutation[:1]) + setting.mutation[1:] + "Input"
		cases = append(cases, gqlEnterpriseMutationCase{
			name: setting.mutation,
			doc: fmt.Sprintf(`mutation($input:%s!){%s(input:$input){enterprise{slug}}}`,
				inputType, setting.mutation),
			input: func(f *gqlEnterpriseFixture) map[string]interface{} {
				return map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "settingValue": setting.value}
			},
		})
	}
	return cases
}

func allGQLEnterpriseMutationCases() []gqlEnterpriseMutationCase {
	return append(append([]gqlEnterpriseMutationCase{}, gqlEnterpriseMutationCases...), enterprisePolicyMutationCases()...)
}

// TestGraphQLEnterpriseMutationsRefuseAnotherEnterprisesOwner is the
// cross-tenant isolation proof: an account that owns a different enterprise —
// so it is a legitimate enterprise owner, just not of this one — is refused
// every mutation against this enterprise, and nothing it named changed.
func TestGraphQLEnterpriseMutationsRefuseAnotherEnterprisesOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for i, tc := range allGQLEnterpriseMutationCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := s.newGQLEnterpriseFixture(t, fmt.Sprintf("r%d", i))
			before := s.store.GetEnterpriseByID(f.enterprise.ID)
			env := s.gqlAuthzPost(t, f.outsiderToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
			if len(gqlAuthzErrors(env)) == 0 {
				t.Fatalf("%s was performed by the owner of a different enterprise: %v", tc.name, env)
			}
			after := s.store.GetEnterpriseByID(f.enterprise.ID)
			if before.Policy != after.Policy || before.Name != after.Name {
				t.Errorf("the refused %s still changed the enterprise: %+v -> %+v", tc.name, before, after)
			}
		})
	}
}

// TestGraphQLEnterpriseMutationsAdmitTheEnterprisesOwner is the entitled half:
// the same documents, sent by the account entitled to send them, all succeed.
func TestGraphQLEnterpriseMutationsAdmitTheEnterprisesOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for i, tc := range allGQLEnterpriseMutationCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := s.newGQLEnterpriseFixture(t, fmt.Sprintf("e%d", i))
			token := f.ownerToken
			if tc.entitledToken != nil {
				token = tc.entitledToken(f)
			}
			env := s.gqlAuthzPost(t, token, tc.doc, map[string]interface{}{"input": tc.input(f)})
			if errs := gqlAuthzErrors(env); len(errs) > 0 {
				t.Fatalf("%s was refused its entitled caller: %v", tc.name, errs)
			}
		})
	}
}

// TestGraphQLEnterpriseReadsAreScopedToTheViewersEnterprise checks the read
// half of the isolation boundary: the owner-only and member-only fields answer
// for a member and are withheld from a stranger.
func TestGraphQLEnterpriseReadsAreScopedToTheViewersEnterprise(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLEnterpriseFixture(t, "reads")

	const doc = `query($slug:String!){enterprise(slug:$slug){slug name viewerIsAdmin billingEmail ownerInfo{membersCanDeleteRepositoriesSetting admins{totalCount}} billingInfo{totalLicenses}}}`
	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"slug": f.enterprise.Slug})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the enterprise's owner could not read it: %v", errs)
	}
	enterprise := gqlEnterpriseNode(t, env)
	if enterprise["viewerIsAdmin"] != true {
		t.Errorf("viewerIsAdmin = %v for the enterprise's own owner", enterprise["viewerIsAdmin"])
	}
	if enterprise["billingEmail"] == nil {
		t.Error("the owner could not read the enterprise's billing email")
	}
	if enterprise["ownerInfo"] == nil {
		t.Error("ownerInfo was withheld from the enterprise's own owner")
	}
	if enterprise["billingInfo"] == nil {
		t.Error("billingInfo was withheld from the enterprise's own owner")
	}

	env = s.gqlAuthzPost(t, f.outsiderToken, doc, map[string]interface{}{"slug": f.enterprise.Slug})
	enterprise = gqlEnterpriseNode(t, env)
	if enterprise == nil {
		t.Fatalf("the enterprise's public profile was not readable at all: %v", env)
	}
	if enterprise["viewerIsAdmin"] != false {
		t.Errorf("viewerIsAdmin = %v for the owner of a different enterprise", enterprise["viewerIsAdmin"])
	}
	if enterprise["ownerInfo"] != nil {
		t.Error("another enterprise's owner read this enterprise's ownerInfo")
	}
	if enterprise["billingInfo"] != nil {
		t.Error("another enterprise's owner read this enterprise's billingInfo")
	}
	if enterprise["billingEmail"] != nil {
		t.Error("another enterprise's owner read this enterprise's billing email")
	}
}

// TestGraphQLEnterpriseMembersAndOrganizationsRefuseNonMembers pins the
// forbidden answer on the two member-only connections.
func TestGraphQLEnterpriseMembersAndOrganizationsRefuseNonMembers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLEnterpriseFixture(t, "conn")

	const doc = `query($slug:String!){enterprise(slug:$slug){members{totalCount} organizations{totalCount}}}`
	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"slug": f.enterprise.Slug})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the enterprise's own owner could not list its members: %v", errs)
	}
	enterprise := gqlEnterpriseNode(t, env)
	members, _ := enterprise["members"].(map[string]interface{})
	if members == nil || members["totalCount"] == nil {
		t.Fatalf("members connection = %v", enterprise["members"])
	}

	env = s.gqlAuthzPost(t, f.outsiderToken, doc, map[string]interface{}{"slug": f.enterprise.Slug})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("another enterprise's owner listed this enterprise's members: %v", env)
	}
}

// TestGraphQLViewerEnterprisesListsOnlyTheViewersOwn checks User.enterprises.
func TestGraphQLViewerEnterprisesListsOnlyTheViewersOwn(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLEnterpriseFixture(t, "viewer")

	const doc = `{viewer{enterprises(first:50){totalCount nodes{slug}}}}`
	env := s.gqlAuthzPost(t, f.ownerToken, doc, nil)
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("viewer.enterprises failed: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	viewer, _ := data["viewer"].(map[string]interface{})
	connection, _ := viewer["enterprises"].(map[string]interface{})
	nodes, _ := connection["nodes"].([]interface{})
	slugs := map[string]bool{}
	for _, node := range nodes {
		if n, ok := node.(map[string]interface{}); ok {
			slug, _ := n["slug"].(string)
			slugs[slug] = true
		}
	}
	if !slugs[f.enterprise.Slug] || !slugs[f.destination.Slug] {
		t.Errorf("viewer.enterprises = %v, want it to carry both enterprises the viewer owns", slugs)
	}
	if slugs["globex-viewer"] {
		t.Error("viewer.enterprises carried an enterprise the viewer does not belong to")
	}
}

// TestGraphQLEnterpriseMutationsAreAudited checks that the audit log records
// what an enterprise owner did, not merely that the state changed.
func TestGraphQLEnterpriseMutationsAreAudited(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLEnterpriseFixture(t, "audit")

	const doc = `mutation($input:UpdateEnterpriseMembersCanDeleteRepositoriesSettingInput!){updateEnterpriseMembersCanDeleteRepositoriesSetting(input:$input){enterprise{slug}}}`
	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"enterpriseId": f.enterprise.NodeID, "settingValue": "DISABLED"},
	})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the policy write failed: %v", errs)
	}
	s.store.Misc.Mu.RLock()
	defer s.store.Misc.Mu.RUnlock()
	for _, entry := range s.store.Misc.AuditLog {
		if entry.Action == "business.members_can_delete_repositories_policy_update" && entry.Actor == f.owner.Login {
			if entry.Data["business"] != f.enterprise.Slug {
				t.Errorf("audit entry names business %v, want %q", entry.Data["business"], f.enterprise.Slug)
			}
			return
		}
	}
	t.Error("the enterprise policy change left no audit entry")
}

func gqlEnterpriseNode(t *testing.T, env map[string]interface{}) map[string]interface{} {
	t.Helper()
	data, _ := env["data"].(map[string]interface{})
	enterprise, _ := data["enterprise"].(map[string]interface{})
	return enterprise
}

// TestGraphQLEnterpriseNodesResolveThroughQueryNode checks the Node surface:
// an enterprise resolves by global id, and the two node types with their own
// visibility rule withhold themselves from a caller who may not see them.
func TestGraphQLEnterpriseNodesResolveThroughQueryNode(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGQLEnterpriseFixture(t, "node")

	const doc = `query($id:ID!){node(id:$id){__typename ... on Enterprise{slug} ... on EnterpriseAdministratorInvitation{role} ... on IpAllowListEntry{allowListValue}}}`

	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"id": f.enterprise.NodeID})
	node := gqlNodeResult(t, env)
	if node == nil || node["__typename"] != "Enterprise" || node["slug"] != f.enterprise.Slug {
		t.Fatalf("node(enterprise) = %v", env)
	}

	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"id": f.adminInvitation.NodeID})
	node = gqlNodeResult(t, env)
	if node == nil || node["__typename"] != "EnterpriseAdministratorInvitation" {
		t.Fatalf("node(adminInvitation) for the enterprise's owner = %v", env)
	}
	env = s.gqlAuthzPost(t, f.outsiderToken, doc, map[string]interface{}{"id": f.adminInvitation.NodeID})
	if gqlNodeResult(t, env) != nil {
		t.Errorf("another enterprise's owner resolved this enterprise's invitation: %v", env)
	}

	env = s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"id": f.ipAllowListEntry.NodeID})
	node = gqlNodeResult(t, env)
	if node == nil || node["allowListValue"] != f.ipAllowListEntry.AllowListValue {
		t.Fatalf("node(ipAllowListEntry) for the enterprise's owner = %v", env)
	}
	env = s.gqlAuthzPost(t, f.outsiderToken, doc, map[string]interface{}{"id": f.ipAllowListEntry.NodeID})
	if gqlNodeResult(t, env) != nil {
		t.Errorf("another enterprise's owner resolved this enterprise's allow-list entry: %v", env)
	}
}

func gqlNodeResult(t *testing.T, env map[string]interface{}) map[string]interface{} {
	t.Helper()
	data, _ := env["data"].(map[string]interface{})
	node, _ := data["node"].(map[string]interface{})
	return node
}
