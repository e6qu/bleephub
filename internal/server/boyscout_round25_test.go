package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestPredefinedOrgRoleBaseRolesMatchCatalog guards the store-layer enforcement
// map against drift from the server's predefined organization-role catalog: the
// two must agree on the base repository role every predefined role confers.
func TestPredefinedOrgRoleBaseRolesMatchCatalog(t *testing.T) {
	t.Parallel()
	for _, role := range predefinedOrgRoles {
		got, ok := store.PredefinedOrgRoleBaseRoles[role.ID]
		if !ok {
			t.Errorf("predefined org role %d (%s) has no base-role entry in the store enforcement map", role.ID, role.Name)
			continue
		}
		if got != role.BaseRole {
			t.Errorf("predefined org role %d (%s): store base role %q != catalog %q", role.ID, role.Name, got, role.BaseRole)
		}
	}
	catalog := map[int]bool{}
	for _, role := range predefinedOrgRoles {
		catalog[role.ID] = true
	}
	for id := range store.PredefinedOrgRoleBaseRoles {
		if !catalog[id] {
			t.Errorf("store enforcement map has role %d with no matching catalog entry", id)
		}
	}
}

// TestOrgRoleGrantsRepoAccess pins that an organization-role assignment actually
// confers repository access — through the predefined all_repo_* roles and a
// custom role assigned to a team. Previously assignments were stored and listed
// but never consulted in the access decision, so the grant was cosmetic.
func TestOrgRoleGrantsRepoAccess(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createOrg(t, "acme")
	_, repoID := s.createOrgRepoForGovernance(t, "acme")
	repo := s.store.GetRepoByID(repoID)
	if repo == nil {
		t.Fatal("org repo not found")
	}

	// Direct assignment of all_repo_admin.
	member, _ := s.newUser(t, "rolemember")
	s.store.SetMembership("acme", member.ID, store.OrgRoleMember, store.MembershipStateActive)
	if store.CanAdminRepo(s.store, member, repo) {
		t.Fatal("baseline: a plain member must not have admin")
	}
	s.store.AssignOrgRoleToUser("acme", 142, member.ID) // all_repo_admin
	if !store.CanAdminRepo(s.store, member, repo) {
		t.Fatal("all_repo_admin did not confer admin access")
	}

	// Custom base_role=write role assigned to a team the user belongs to.
	teamMember, _ := s.newUser(t, "teamrolemember")
	s.store.SetMembership("acme", teamMember.ID, store.OrgRoleMember, store.MembershipStateActive)
	team := s.store.CreateTeam("acme", "shippers", store.TeamOptions{})
	if team == nil {
		t.Fatal("team creation failed")
	}
	s.store.SetTeamMembership("acme", team.Slug, teamMember.ID, store.TeamRoleMember)

	base := "write"
	s.store.Mu.Lock()
	roleID := s.store.ReserveOrgCustomRoleIDLocked()
	if s.store.OrgCustomRoles["acme"] == nil {
		s.store.OrgCustomRoles["acme"] = map[int]*store.OrgCustomOrganizationRole{}
	}
	s.store.OrgCustomRoles["acme"][roleID] = &store.OrgCustomOrganizationRole{
		ID: roleID, Name: "deployer", BaseRole: &base, OrgLogin: "acme",
	}
	s.store.Mu.Unlock()

	if store.CanPushRepo(s.store, teamMember, repo) {
		t.Fatal("baseline: a plain team member must not have push")
	}
	s.store.AssignOrgRoleToTeam("acme", roleID, team.ID)
	if !store.CanPushRepo(s.store, teamMember, repo) {
		t.Fatal("a custom write role assigned to the team did not confer push")
	}
	if store.CanAdminRepo(s.store, teamMember, repo) {
		t.Fatal("a write base role must not confer admin")
	}
}
