package graphqlapi

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestOrganizationProfileFieldsResolveFromTheStoredRow pins the profile and
// settings members against the organization row the REST organization routes
// write.
func TestOrganizationProfileFieldsResolveFromTheStoredRow(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	org := h.store.CreateOrg(admin, "profileorg", "Profile Org", "A **profiled** org")
	if org == nil {
		t.Fatal("organization not created")
	}
	forking := true
	h.store.UpdateOrg("profileorg", func(o *store.Org) {
		o.Location = "Lisbon"
		o.Blog = "https://profileorg.example.test"
		o.TwitterUsername = "profileorg"
		o.BillingEmail = "billing@profileorg.example.test"
		o.WebCommitSignoffRequired = true
		o.MembersCanForkPrivateRepositories = &forking
	})

	document := `{
	  organization(login:"profileorg") {
	    location
	    websiteUrl
	    twitterUsername
	    descriptionHTML
	    organizationBillingEmail
	    webCommitSignoffRequired
	    membersCanForkPrivateRepositories
	    requiresTwoFactorAuthentication
	    isVerified
	    archivedAt
	    teamsResourcePath
	    teamsUrl
	    newTeamResourcePath
	    viewerCanAdminister
	    viewerIsAMember
	    viewerCanCreateTeams
	    viewerCanCreateRepositories
	  }
	}`
	data := h.query(admin, document, nil)
	orgData, _ := at(t, data, "organization").(map[string]interface{})
	for field, want := range map[string]interface{}{
		"location":                          "Lisbon",
		"websiteUrl":                        "https://profileorg.example.test",
		"twitterUsername":                   "profileorg",
		"organizationBillingEmail":          "billing@profileorg.example.test",
		"webCommitSignoffRequired":          true,
		"membersCanForkPrivateRepositories": true,
		"requiresTwoFactorAuthentication":   false,
		"isVerified":                        false,
		"archivedAt":                        nil,
		"teamsResourcePath":                 "/orgs/profileorg/teams",
		"newTeamResourcePath":               "/orgs/profileorg/teams/new",
		"viewerCanAdminister":               true,
		"viewerIsAMember":                   true,
		"viewerCanCreateTeams":              true,
		"viewerCanCreateRepositories":       true,
	} {
		if got := orgData[field]; got != want {
			t.Errorf("%s = %#v, want %#v", field, got, want)
		}
	}
	if html, _ := orgData["descriptionHTML"].(string); html == "" || html == org.Description {
		t.Errorf("descriptionHTML = %q, want the rendered markdown", html)
	}

	// The billing address and the administrative flags are not public.
	strangerView := h.query(h.user("passerby"), document, nil)
	strangerData, _ := at(t, strangerView, "organization").(map[string]interface{})
	for field, want := range map[string]interface{}{
		"organizationBillingEmail":    nil,
		"viewerCanAdminister":         false,
		"viewerIsAMember":             false,
		"viewerCanCreateTeams":        false,
		"viewerCanCreateRepositories": false,
	} {
		if got := strangerData[field]; got != want {
			t.Errorf("stranger %s = %#v, want %#v", field, got, want)
		}
	}
}

// TestOrganizationMembershipConnectionsHidePrivateMembership is the
// authorization test for the membership graph: a non-member sees only the
// publicized memberships, and only an owner sees who is still pending or
// whether an account has two-factor authentication on.
func TestOrganizationMembershipConnectionsHidePrivateMembership(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	public := h.user("publicmember")
	private := h.user("privatemember")
	invited := h.user("invitee")

	org := h.store.CreateOrg(admin, "memberorg", "Member Org", "")
	if org == nil {
		t.Fatal("organization not created")
	}
	h.store.SetMembership("memberorg", public.ID, store.OrgRoleMember, store.MembershipStateActive)
	h.store.SetMembership("memberorg", private.ID, store.OrgRoleMember, store.MembershipStateActive)
	if !h.store.SetMembershipPublic("memberorg", public.ID, true) {
		t.Fatal("membership not publicized")
	}
	if invitation, _ := h.store.CreateOrgInvitation(org, admin, invited, "", "direct_member", nil); invitation == nil {
		t.Fatal("invitation not created")
	}

	document := `{
	  organization(login:"memberorg") {
	    membersWithRole(first:20) { totalCount edges { role hasTwoFactorEnabled node { login } } }
	    pendingMembers(first:20) { totalCount nodes { login } }
	  }
	}`

	ownerView := h.query(admin, document, nil)
	roles := map[string]string{}
	twoFactorReported := 0
	for _, raw := range at(t, ownerView, "organization", "membersWithRole", "edges").([]interface{}) {
		edge, _ := raw.(map[string]interface{})
		node, _ := edge["node"].(map[string]interface{})
		login, _ := node["login"].(string)
		role, _ := edge["role"].(string)
		roles[login] = role
		if edge["hasTwoFactorEnabled"] != nil {
			twoFactorReported++
		}
	}
	if roles["admin"] != "ADMIN" || roles["publicmember"] != "MEMBER" || roles["privatemember"] != "MEMBER" {
		t.Errorf("owner sees roles %#v, want the whole roster", roles)
	}
	if twoFactorReported != len(roles) {
		t.Errorf("owner saw hasTwoFactorEnabled on %d of %d edges, want all", twoFactorReported, len(roles))
	}
	if got := at(t, ownerView, "organization", "pendingMembers", "totalCount"); got != float64(1) {
		t.Errorf("owner pendingMembers totalCount = %v, want 1", got)
	}

	strangerView := h.query(h.user("outsideorg"), document, nil)
	strangerLogins := map[string]bool{}
	for _, raw := range at(t, strangerView, "organization", "membersWithRole", "edges").([]interface{}) {
		edge, _ := raw.(map[string]interface{})
		node, _ := edge["node"].(map[string]interface{})
		login, _ := node["login"].(string)
		strangerLogins[login] = true
		if edge["hasTwoFactorEnabled"] != nil {
			t.Errorf("stranger saw hasTwoFactorEnabled for %s", login)
		}
	}
	if !strangerLogins["publicmember"] {
		t.Errorf("stranger sees %#v, want the publicized membership", strangerLogins)
	}
	if strangerLogins["privatemember"] {
		t.Errorf("stranger sees %#v, which leaks a private membership", strangerLogins)
	}
	if got := at(t, strangerView, "organization", "pendingMembers", "totalCount"); got != float64(0) {
		t.Errorf("stranger pendingMembers totalCount = %v, want 0", got)
	}
}

// TestOrganizationTeamsHideSecretTeams is the authorization test for the team
// graph: a secret team and its roster are invisible outside the organization
// and outside the team itself.
func TestOrganizationTeamsHideSecretTeams(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	member := h.user("teamviewer")
	org := h.store.CreateOrg(admin, "teamorg", "Team Org", "")
	if org == nil {
		t.Fatal("organization not created")
	}
	h.store.SetMembership("teamorg", member.ID, store.OrgRoleMember, store.MembershipStateActive)

	visible := h.store.CreateTeam("teamorg", "Visible", store.TeamOptions{Privacy: store.TeamPrivacyClosed})
	secret := h.store.CreateTeam("teamorg", "Secret", store.TeamOptions{Privacy: store.TeamPrivacySecret})
	if visible == nil || secret == nil {
		t.Fatal("teams not created")
	}
	if !h.store.SetTeamMembership("teamorg", visible.Slug, member.ID, store.TeamRoleMaintainer) {
		t.Fatal("team membership not set")
	}

	document := `{
	  organization(login:"teamorg") {
	    teams(first:20) {
	      totalCount
	      nodes {
	        slug name privacy combinedSlug notificationSetting resourcePath
	        viewerCanAdminister
	        members(first:10) { totalCount edges { role node { login } } }
	      }
	    }
	    secretTeam: team(slug:"secret") { slug }
	    visibleTeam: team(slug:"visible") { slug description }
	  }
	}`

	ownerView := h.query(admin, document, nil)
	if got := at(t, ownerView, "organization", "teams", "totalCount"); got != float64(2) {
		t.Errorf("owner teams totalCount = %v, want 2", got)
	}
	if got := at(t, ownerView, "organization", "secretTeam", "slug"); got != "secret" {
		t.Errorf("owner cannot see the secret team: %v", got)
	}
	teams := at(t, ownerView, "organization", "teams", "nodes").([]interface{})
	byslug := map[string]map[string]interface{}{}
	for _, raw := range teams {
		team, _ := raw.(map[string]interface{})
		slug, _ := team["slug"].(string)
		byslug[slug] = team
	}
	if byslug["visible"]["privacy"] != "VISIBLE" || byslug["secret"]["privacy"] != "SECRET" {
		t.Errorf("team privacy = %#v / %#v", byslug["visible"]["privacy"], byslug["secret"]["privacy"])
	}
	if byslug["visible"]["combinedSlug"] != "teamorg/visible" {
		t.Errorf("combinedSlug = %v", byslug["visible"]["combinedSlug"])
	}
	if byslug["visible"]["resourcePath"] != "/orgs/teamorg/teams/visible" {
		t.Errorf("resourcePath = %v", byslug["visible"]["resourcePath"])
	}
	memberEdges := at(t, byslug["visible"], "members", "edges").([]interface{})
	if len(memberEdges) != 1 {
		t.Fatalf("visible team members = %#v, want one", memberEdges)
	}
	if edge, _ := memberEdges[0].(map[string]interface{}); edge["role"] != "MAINTAINER" {
		t.Errorf("team member role = %#v, want MAINTAINER", edge)
	}

	// A member of the organization who is not on the secret team cannot see it.
	memberView := h.query(member, document, nil)
	if got := at(t, memberView, "organization", "teams", "totalCount"); got != float64(1) {
		t.Errorf("member teams totalCount = %v, want only the visible team", got)
	}
	memberData, _ := at(t, memberView, "organization").(map[string]interface{})
	if memberData["secretTeam"] != nil {
		t.Errorf("member read the secret team = %#v", memberData["secretTeam"])
	}

	// A stranger sees no teams at all.
	strangerView := h.query(h.user("teamstranger"), document, nil)
	if got := at(t, strangerView, "organization", "teams", "totalCount"); got != float64(0) {
		t.Errorf("stranger teams totalCount = %v, want 0", got)
	}
}

// TestOrganizationGovernanceRecordsRequireStanding pins the IP allow list,
// rulesets and repository custom properties against the rows the REST
// governance routes write, and that each is gated on the standing GitHub
// requires.
func TestOrganizationGovernanceRecordsRequireStanding(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	member := h.user("govmember")
	org := h.store.CreateOrg(admin, "govorg", "Governance Org", "")
	if org == nil {
		t.Fatal("organization not created")
	}
	h.store.SetMembership("govorg", member.ID, store.OrgRoleMember, store.MembershipStateActive)

	if h.store.CreateIPAllowListEntry(store.IPAllowListOwnerOrganization, org.ID, "203.0.113.0/24", "office", true) == nil {
		t.Fatal("allow list entry not created")
	}
	description := "which team owns it"
	h.store.UpsertCustomProperty("govorg", &store.CustomProperty{
		PropertyName:     "owner-team",
		ValueType:        "single_select",
		Required:         true,
		DefaultValue:     "platform",
		Description:      &description,
		AllowedValues:    []string{"platform", "product"},
		ValuesEditableBy: "org_actors",
	})

	document := `{
	  organization(login:"govorg") {
	    ipAllowListEnabledSetting
	    ipAllowListEntries(first:5) { totalCount nodes { allowListValue name isActive owner { ... on Organization { login } } } }
	    repositoryCustomProperties(first:5) {
	      totalCount
	      nodes { propertyName valueType required description allowedValues valuesEditableBy }
	    }
	    repositoryCustomProperty(propertyName:"owner-team") { propertyName defaultValue }
	  }
	}`

	ownerView := h.query(admin, document, nil)
	if got := at(t, ownerView, "organization", "ipAllowListEntries", "totalCount"); got != float64(1) {
		t.Fatalf("owner ipAllowListEntries totalCount = %v, want 1", got)
	}
	entries := at(t, ownerView, "organization", "ipAllowListEntries", "nodes").([]interface{})
	entry, _ := entries[0].(map[string]interface{})
	if entry["allowListValue"] != "203.0.113.0/24" || entry["name"] != "office" || entry["isActive"] != true {
		t.Errorf("allow list entry = %#v", entry)
	}
	if got := at(t, entry, "owner", "login"); got != "govorg" {
		t.Errorf("allow list entry owner = %v, want govorg", got)
	}
	if got := at(t, ownerView, "organization", "repositoryCustomProperties", "totalCount"); got != float64(1) {
		t.Fatalf("repositoryCustomProperties totalCount = %v, want 1", got)
	}
	properties := at(t, ownerView, "organization", "repositoryCustomProperties", "nodes").([]interface{})
	property, _ := properties[0].(map[string]interface{})
	if property["propertyName"] != "owner-team" || property["valueType"] != "SINGLE_SELECT" ||
		property["required"] != true || property["valuesEditableBy"] != "ORG_ACTORS" {
		t.Errorf("custom property = %#v", property)
	}
	if got := at(t, ownerView, "organization", "repositoryCustomProperty", "propertyName"); got != "owner-team" {
		t.Errorf("repositoryCustomProperty = %v", got)
	}

	// A member sees the property definitions (they govern their repositories)
	// but not the network policy.
	memberView := h.query(member, document, nil)
	if got := at(t, memberView, "organization", "repositoryCustomProperties", "totalCount"); got != float64(1) {
		t.Errorf("member repositoryCustomProperties totalCount = %v, want 1", got)
	}
	if got := at(t, memberView, "organization", "ipAllowListEntries", "totalCount"); got != float64(0) {
		t.Errorf("member read ipAllowListEntries totalCount = %v, want 0", got)
	}

	// A stranger sees neither.
	strangerView := h.query(h.user("govstranger"), document, nil)
	strangerData, _ := at(t, strangerView, "organization").(map[string]interface{})
	if strangerData["repositoryCustomProperties"] != nil {
		t.Errorf("stranger read repositoryCustomProperties = %#v, want null", strangerData["repositoryCustomProperties"])
	}
	if strangerData["repositoryCustomProperty"] != nil {
		t.Errorf("stranger read repositoryCustomProperty = %#v, want null", strangerData["repositoryCustomProperty"])
	}
}
