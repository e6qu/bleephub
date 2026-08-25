package bleephub

// Enterprise policy enforcement: each policy is exercised at the place it
// governs, so a policy that only stored a value would fail here.

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// enterprisePolicyFixture is an organization owner who is an ordinary member
// of the instance's enterprise — the person every policy binds. The site
// administrator cannot stand in: they are an enterprise owner and therefore
// exempt from every policy, which is exactly what the exemption cases check.
type enterprisePolicyFixture struct {
	enterprise *store.Enterprise
	owner      *store.User
	ownerToken string
	org        *store.Org
	repo       *store.Repo
}

func (s *isolatedServer) newEnterprisePolicyFixture(t *testing.T, tag string) *enterprisePolicyFixture {
	t.Helper()
	f := &enterprisePolicyFixture{}
	f.enterprise = s.store.GetEnterprise(s.enterpriseSlug())
	if f.enterprise == nil {
		t.Fatal("the instance's own enterprise account was not seeded")
	}
	f.owner, f.ownerToken = s.newUser(t, "policy"+tag)
	f.org = s.store.CreateOrg(f.owner, "policyorg"+tag, "Policy Org "+tag, "")
	if f.org == nil {
		t.Fatalf("CreateOrg policyorg%s failed", tag)
	}
	f.repo = s.store.CreateOrgRepo(f.org, f.owner, "policyrepo", "", false)
	if f.repo == nil {
		t.Fatal("CreateOrgRepo failed")
	}
	return f
}

func (s *isolatedServer) setEnterprisePolicy(t *testing.T, f *enterprisePolicyFixture, mutate func(*store.EnterprisePolicy)) {
	t.Helper()
	if s.store.UpdateEnterprisePolicy(f.enterprise.ID, mutate) == nil {
		t.Fatal("UpdateEnterprisePolicy failed")
	}
}

func TestEnterprisePolicyForbidsRepositoryDeletion(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "del")

	path := "/api/v3/repos/" + f.org.Login + "/" + f.repo.Name
	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanDeleteRepositories = store.EnterprisePolicyDisabled
	})
	expectStatus(t, s.delete(t, path, f.ownerToken), http.StatusForbidden, "DELETE repo under a disabling policy")
	if s.store.GetRepo(f.org.Login, f.repo.Name) == nil {
		t.Fatal("the refused deletion still removed the repository")
	}

	// An enterprise owner is exempt: the site administrator owns the
	// instance's enterprise.
	expectStatus(t, s.delete(t, path, defaultToken), http.StatusNoContent, "DELETE repo as an enterprise owner")
}

func TestEnterprisePolicyForbidsVisibilityChange(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "vis")
	path := "/api/v3/repos/" + f.org.Login + "/" + f.repo.Name

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanChangeRepositoryVisibility = store.EnterprisePolicyDisabled
	})
	expectStatus(t, s.patch(t, path, f.ownerToken, map[string]interface{}{"private": true}),
		http.StatusForbidden, "PATCH visibility under a disabling policy")
	if repo := s.store.GetRepo(f.org.Login, f.repo.Name); repo == nil || repo.Private {
		t.Errorf("the refused visibility change still applied: %+v", repo)
	}
	// A body that changes something else is untouched by the policy.
	expectStatus(t, s.patch(t, path, f.ownerToken, map[string]interface{}{"description": "still editable"}),
		http.StatusOK, "PATCH description under a visibility policy")
}

func TestEnterprisePolicyForbidsCollaboratorInvitation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "collab")
	outsider, _ := s.newUser(t, "outsidercollab")

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanInviteCollaborators = store.EnterprisePolicyDisabled
	})
	path := "/api/v3/repos/" + f.org.Login + "/" + f.repo.Name + "/collaborators/" + outsider.Login
	expectStatus(t, s.put(t, path, f.ownerToken, map[string]interface{}{"permission": "push"}),
		http.StatusForbidden, "PUT collaborator under a disabling policy")
	if s.store.GetRepoCollaboratorPermission(f.org.Login, f.repo.Name, outsider.Login) != "" {
		t.Error("the refused invitation still granted access")
	}
}

func TestEnterprisePolicyForbidsDeployKeys(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "key")

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.RepositoryDeployKey = store.EnterprisePolicyDisabled
	})
	path := "/api/v3/repos/" + f.org.Login + "/" + f.repo.Name + "/keys"
	expectStatus(t, s.post(t, path, f.ownerToken, map[string]interface{}{
		"title": "ci", "key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAbcdefghijklmnopqrstuvwxyz0123456789ABCD ci",
	}), http.StatusForbidden, "POST deploy key under a disabling policy")
}

func TestEnterprisePolicyForbidsProtectedBranchUpdates(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "bp")

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanUpdateProtectedBranches = store.EnterprisePolicyDisabled
	})
	path := "/api/v3/repos/" + f.org.Login + "/" + f.repo.Name + "/branches/main/protection"
	expectStatus(t, s.put(t, path, f.ownerToken, map[string]interface{}{
		"required_status_checks":        nil,
		"enforce_admins":                nil,
		"required_pull_request_reviews": nil,
		"restrictions":                  nil,
	}), http.StatusForbidden, "PUT branch protection under a disabling policy")
	expectStatus(t, s.delete(t, path, f.ownerToken), http.StatusForbidden,
		"DELETE branch protection under a disabling policy")
}

func TestEnterprisePolicyForbidsRepositoryCreationByVisibility(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "create")
	path := "/api/v3/orgs/" + f.org.Login + "/repos"

	// PRIVATE permits private repositories and refuses public ones.
	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanCreateRepositories = "PRIVATE"
	})
	expectStatus(t, s.post(t, path, f.ownerToken, map[string]interface{}{"name": "refused-public", "private": false}),
		http.StatusForbidden, "POST public repo under a PRIVATE policy")
	expectStatus(t, s.post(t, path, f.ownerToken, map[string]interface{}{"name": "allowed-private", "private": true}),
		http.StatusCreated, "POST private repo under a PRIVATE policy")

	// DISABLED refuses both.
	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanCreateRepositories = store.EnterprisePolicyDisabled
	})
	expectStatus(t, s.post(t, path, f.ownerToken, map[string]interface{}{"name": "refused-private", "private": true}),
		http.StatusForbidden, "POST private repo under a DISABLED policy")

	// The per-visibility booleans narrow an otherwise permissive setting.
	no := false
	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanCreateRepositories = "ALL"
		p.MembersCanCreatePublicRepositories = &no
	})
	expectStatus(t, s.post(t, path, f.ownerToken, map[string]interface{}{"name": "refused-public-2", "private": false}),
		http.StatusForbidden, "POST public repo with public creation switched off")
}

func TestEnterpriseDefaultRepositoryPermissionIsACeiling(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "base")
	member, memberToken := s.newUser(t, "basemember")
	s.store.SetMembership(f.org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)
	s.store.UpdateOrg(f.org.Login, func(o *store.Org) { o.DefaultRepositoryPermission = "admin" })

	// With no enterprise ceiling the organization's own base permission wins:
	// an ordinary member administers the repository.
	repoPath := "/api/v3/repos/" + f.org.Login + "/" + f.repo.Name
	expectStatus(t, s.patch(t, repoPath, memberToken, map[string]interface{}{"description": "member wrote this"}),
		http.StatusOK, "PATCH repo with an admin base permission")

	// A READ ceiling clamps it on the next access check, without rewriting the
	// organization's own setting.
	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) { p.DefaultRepositoryPermission = "READ" })
	expectStatus(t, s.patch(t, repoPath, memberToken, map[string]interface{}{"description": "member tried again"}),
		http.StatusForbidden, "PATCH repo under a READ ceiling")

	// And the organization's settings report what members actually hold.
	body := decodeJSON(t, s.get(t, "/api/v3/orgs/"+f.org.Login, f.ownerToken))
	if got := body["default_repository_permission"]; got != "read" {
		t.Errorf("default_repository_permission = %v, want the clamped \"read\"", got)
	}
	if got := body["two_factor_requirement_enabled"]; got != false {
		t.Errorf("two_factor_requirement_enabled = %v with no enterprise requirement", got)
	}

	// Raising the organization's own setting above the ceiling is refused
	// outright rather than silently clamped.
	expectStatus(t, s.patch(t, "/api/v3/orgs/"+f.org.Login, f.ownerToken,
		map[string]interface{}{"default_repository_permission": "write"}),
		http.StatusForbidden, "PATCH org base permission above the ceiling")
}

func TestEnterprisePolicyGovernsPrivateForking(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "fork")
	private := s.store.CreateOrgRepo(f.org, f.owner, "secret", "", true)
	if private == nil {
		t.Fatal("CreateOrgRepo failed")
	}
	path := "/api/v3/repos/" + f.org.Login + "/" + private.Name + "/forks"

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.AllowPrivateRepositoryForking = store.EnterprisePolicyDisabled
	})
	expectStatus(t, s.post(t, path, f.ownerToken, map[string]interface{}{}),
		http.StatusForbidden, "POST fork of a private repo under a disabling policy")

	// ENABLED with a policy value of USER_ACCOUNTS admits a fork into a
	// personal namespace, which is what this request asks for.
	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.AllowPrivateRepositoryForking = store.EnterprisePolicyEnabled
		p.AllowPrivateRepositoryForkingPolicyValue = "USER_ACCOUNTS"
	})
	expectStatus(t, s.post(t, path, f.ownerToken, map[string]interface{}{}),
		http.StatusAccepted, "POST fork into a user account under USER_ACCOUNTS")
}

func TestEnterprisePolicyForbidsRepositoryProjects(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "proj")

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.RepositoryProjects = store.EnterprisePolicyDisabled
	})
	expectStatus(t, s.post(t, "/api/v3/repos/"+f.org.Login+"/"+f.repo.Name+"/projects", f.ownerToken,
		map[string]interface{}{"name": "Roadmap"}),
		http.StatusForbidden, "POST repository project under a disabling policy")

	// The organization's settings response reports the same policy.
	body := decodeJSON(t, s.get(t, "/api/v3/orgs/"+f.org.Login, f.ownerToken))
	if got := body["has_repository_projects"]; got != false {
		t.Errorf("has_repository_projects = %v under a disabling policy", got)
	}
}

func TestEnterprisePolicyForbidsDependencyInsights(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "dep")

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanViewDependencyInsights = store.EnterprisePolicyDisabled
	})
	expectStatus(t, s.get(t, "/api/v3/repos/"+f.org.Login+"/"+f.repo.Name+"/dependency-graph/compare/main...topic", f.ownerToken),
		http.StatusForbidden, "GET dependency comparison under a disabling policy")
}

func TestEnterprisePolicyForbidsIssueDeletionThroughGraphQL(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "issue")
	issue := s.store.CreateIssue(f.repo.ID, f.owner.ID, "Delete me", "", nil, nil, 0)
	if issue == nil {
		t.Fatal("CreateIssue failed")
	}

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.MembersCanDeleteIssues = store.EnterprisePolicyDisabled
	})
	const doc = `mutation($input:DeleteIssueInput!){deleteIssue(input:$input){clientMutationId}}`
	env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{
		"input": map[string]interface{}{"issueId": issue.NodeID},
	})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Fatalf("deleteIssue succeeded under a disabling policy: %v", env)
	}
	if s.store.GetIssue(issue.ID) == nil {
		t.Error("the refused deleteIssue still removed the issue")
	}
}

func TestEnterpriseIPAllowListGatesTheAPI(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "ip")

	// Enabled with no active entry admits everything: enabling the feature
	// must not lock the enterprise out before it adds a range.
	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.IPAllowListEnabled = store.EnterprisePolicyEnabled
	})
	expectStatus(t, s.get(t, "/api/v3/user", f.ownerToken), http.StatusOK,
		"GET /user with an empty allow list")

	// An active entry that does not cover the caller refuses the request.
	entry := s.store.CreateIPAllowListEntry("Enterprise", f.enterprise.ID, "198.51.100.0/24", "elsewhere", true)
	if entry == nil {
		t.Fatal("CreateIPAllowListEntry failed")
	}
	expectStatus(t, s.get(t, "/api/v3/user", f.ownerToken), http.StatusForbidden,
		"GET /user from an address outside the allow list")

	// Widening the entry to cover the loopback readmits it.
	if s.store.UpdateIPAllowListEntry(entry.ID, "127.0.0.0/8", "loopback", true) == nil {
		t.Fatal("UpdateIPAllowListEntry failed")
	}
	expectStatus(t, s.get(t, "/api/v3/user", f.ownerToken), http.StatusOK,
		"GET /user from an address inside the allow list")

	// Deactivating the only entry leaves no active entry, which admits again.
	if s.store.UpdateIPAllowListEntry(entry.ID, "198.51.100.0/24", "elsewhere", false) == nil {
		t.Fatal("UpdateIPAllowListEntry failed")
	}
	expectStatus(t, s.get(t, "/api/v3/user", f.ownerToken), http.StatusOK,
		"GET /user with only an inactive entry")
}

func TestEnterpriseTwoFactorRequirementBlocksDisabling(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "2fa")

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.TwoFactorRequired = store.EnterprisePolicyEnabled
	})
	expectStatus(t, s.post(t, "/ui-data/user/two-factor/disable", f.ownerToken, map[string]interface{}{"code": "000000"}),
		http.StatusForbidden, "POST two-factor disable under an enterprise requirement")

	// And the organization settings response reports the requirement.
	body := decodeJSON(t, s.get(t, "/api/v3/orgs/"+f.org.Login, f.ownerToken))
	if got := body["two_factor_requirement_enabled"]; got != true {
		t.Errorf("two_factor_requirement_enabled = %v under an enterprise requirement", got)
	}
}
