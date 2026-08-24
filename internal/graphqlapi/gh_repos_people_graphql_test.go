package graphqlapi

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestRepositoryCollaboratorsReportEveryGrant pins that the collaborators
// connection reports the owner, the accounts added directly, and — for an
// organization repository — the members of the teams granted access, each with
// the permission that grant confers.
func TestRepositoryCollaboratorsReportEveryGrant(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	h.user("writer")
	teamMember := h.user("teammate")

	org := h.store.CreateOrg(admin, "peopleorg", "People Org", "")
	if org == nil {
		t.Fatal("organization not created")
	}
	repo := h.store.CreateOrgRepo(org, admin, "shared", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	if !h.store.AddRepoCollaborator("peopleorg", "shared", "writer", "push") {
		t.Fatal("collaborator not added")
	}
	team := h.store.CreateTeam("peopleorg", "Reviewers", store.TeamOptions{
		Privacy: store.TeamPrivacyClosed, Permission: store.TeamPermissionAdmin,
	})
	if team == nil {
		t.Fatal("team not created")
	}
	if h.store.SetMembership("peopleorg", teamMember.ID, store.OrgRoleMember, store.MembershipStateActive) == nil {
		t.Fatal("organization membership not set")
	}
	if !h.store.SetTeamMembership("peopleorg", team.Slug, teamMember.ID, store.TeamRoleMember) {
		t.Fatal("team membership not set")
	}
	if !h.store.AddTeamRepo("peopleorg", team.Slug, repo.FullName) {
		t.Fatal("team repository access not granted")
	}

	document := `{
	  repository(owner:"peopleorg", name:"shared") {
	    collaborators(first:20) {
	      totalCount
	      edges { permission node { login } }
	    }
	  }
	}`
	data := h.query(admin, document, nil)
	edges := at(t, data, "repository", "collaborators", "edges").([]interface{})
	permissions := map[string]string{}
	for _, raw := range edges {
		edge, _ := raw.(map[string]interface{})
		node, _ := edge["node"].(map[string]interface{})
		login, _ := node["login"].(string)
		permission, _ := edge["permission"].(string)
		permissions[login] = permission
	}
	for login, want := range map[string]string{
		"admin":    "ADMIN",
		"writer":   "WRITE",
		"teammate": "ADMIN",
	} {
		if got := permissions[login]; got != want {
			t.Errorf("collaborator %s permission = %q, want %q (all: %#v)", login, got, want, permissions)
		}
	}

	// A stranger with no push standing must not learn who may push.
	stranger := h.user("nobody")
	strangerView := h.query(stranger, document, nil)
	if got := at(t, strangerView, "repository", "collaborators"); got != nil {
		t.Errorf("stranger read collaborators = %#v, want null", got)
	}
}

// TestRepositoryCollaboratorAffiliationNarrowsTheList pins the affiliation
// argument: DIRECT reports only the accounts added to the repository itself,
// OUTSIDE only those who are not members of the owning organization.
func TestRepositoryCollaboratorAffiliationNarrowsTheList(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	h.user("outsider")
	member := h.user("insider")

	org := h.store.CreateOrg(admin, "affilorg", "Affiliation Org", "")
	repo := h.store.CreateOrgRepo(org, admin, "code", "", false)
	if org == nil || repo == nil {
		t.Fatal("fixture not created")
	}
	h.store.SetMembership("affilorg", member.ID, store.OrgRoleMember, store.MembershipStateActive)
	if !h.store.AddRepoCollaborator("affilorg", "code", "outsider", "pull") {
		t.Fatal("outside collaborator not added")
	}
	if !h.store.AddRepoCollaborator("affilorg", "code", "insider", "push") {
		t.Fatal("member collaborator not added")
	}

	logins := func(affiliation string) map[string]bool {
		data := h.query(admin, `query($a:CollaboratorAffiliation!){
		  repository(owner:"affilorg", name:"code") {
		    collaborators(first:20, affiliation:$a) { nodes { login } }
		  }
		}`, map[string]interface{}{"a": affiliation})
		out := map[string]bool{}
		for _, raw := range at(t, data, "repository", "collaborators", "nodes").([]interface{}) {
			node, _ := raw.(map[string]interface{})
			login, _ := node["login"].(string)
			out[login] = true
		}
		return out
	}

	direct := logins("DIRECT")
	if !direct["outsider"] || !direct["insider"] {
		t.Errorf("DIRECT = %#v, want both direct collaborators", direct)
	}
	outside := logins("OUTSIDE")
	if !outside["outsider"] {
		t.Errorf("OUTSIDE = %#v, want the non-member collaborator", outside)
	}
	if outside["insider"] {
		t.Errorf("OUTSIDE = %#v, must not include an organization member", outside)
	}
}

// TestRepositoryRecordConnectionsResolveFromTheStore covers the milestone,
// label, deploy-key, commit-comment and fork members in one query, each
// against a record the store holds.
func TestRepositoryRecordConnectionsResolveFromTheStore(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	repo := h.store.CreateRepo(owner, "records", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	milestone := h.store.CreateMilestone(repo.ID, owner.ID, "v1", "first release", "open", nil)
	if milestone == nil {
		t.Fatal("milestone not created")
	}
	if h.store.CreateRepoDeployKey(repo.ID, "ci", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyForTests ci@bleephub", true) == nil {
		t.Fatal("deploy key not created")
	}
	if h.store.CommitComments.Create(repo.ID, "deadbeef", owner.ID, "looks good", "", nil, nil) == nil {
		t.Fatal("commit comment not created")
	}
	fork := h.store.ForkRepo(h.user("forker"), repo, "records")
	if fork == nil {
		t.Fatal("fork not created")
	}

	data := h.query(owner, `{
	  repository(owner:"admin", name:"records") {
	    milestone(number:1) { title description }
	    label(name:"bug") { name }
	    deployKeys(first:5) { totalCount nodes { title readOnly key enabled } }
	    commitComments(first:5) { totalCount nodes { body author { login } } }
	    forks(first:5) { totalCount nodes { nameWithOwner } }
	  }
	}`, nil)

	if got := at(t, data, "repository", "milestone", "title"); got != "v1" {
		t.Errorf("milestone title = %v, want v1", got)
	}
	if got := at(t, data, "repository", "label", "name"); got != "bug" {
		t.Errorf("label name = %v, want bug", got)
	}
	if got := at(t, data, "repository", "deployKeys", "totalCount"); got != float64(1) {
		t.Errorf("deployKeys totalCount = %v, want 1", got)
	}
	if got := at(t, data, "repository", "commitComments", "totalCount"); got != float64(1) {
		t.Errorf("commitComments totalCount = %v, want 1", got)
	}
	comments := at(t, data, "repository", "commitComments", "nodes").([]interface{})
	comment, _ := comments[0].(map[string]interface{})
	if at(t, comment, "author", "login") != "admin" {
		t.Errorf("commit comment author = %#v", comment["author"])
	}
	if got := at(t, data, "repository", "forks", "totalCount"); got != float64(1) {
		t.Errorf("forks totalCount = %v, want 1", got)
	}
	forks := at(t, data, "repository", "forks", "nodes").([]interface{})
	if node, _ := forks[0].(map[string]interface{}); node["nameWithOwner"] != "forker/records" {
		t.Errorf("fork = %#v", node)
	}

	// A deploy key is a credential: a non-administrator sees none.
	strangerView := h.query(h.user("stranger"), `{
	  repository(owner:"admin", name:"records") { deployKeys(first:5) { totalCount } }
	}`, nil)
	if got := at(t, strangerView, "repository", "deployKeys", "totalCount"); got != float64(0) {
		t.Errorf("stranger read deployKeys totalCount = %v, want 0", got)
	}
}

// TestRepositoryMentionableUsersSpanStandingAndParticipation pins that
// mentionableUsers reports both the accounts with standing on the repository
// and the accounts that have already opened something in it.
func TestRepositoryMentionableUsersSpanStandingAndParticipation(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	reporter := h.user("reporter")
	repo := h.store.CreateRepo(owner, "mentions", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	if h.store.CreateIssue(repo.ID, reporter.ID, "a bug", "", nil, nil, 0) == nil {
		t.Fatal("issue not created")
	}

	data := h.query(owner, `{
	  repository(owner:"admin", name:"mentions") {
	    mentionableUsers(first:20) { nodes { login } }
	  }
	}`, nil)
	seen := map[string]bool{}
	for _, raw := range at(t, data, "repository", "mentionableUsers", "nodes").([]interface{}) {
		node, _ := raw.(map[string]interface{})
		login, _ := node["login"].(string)
		seen[login] = true
	}
	if !seen["admin"] || !seen["reporter"] {
		t.Errorf("mentionableUsers = %#v, want the owner and the issue author", seen)
	}
}
