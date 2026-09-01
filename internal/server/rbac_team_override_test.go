package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestTeamRepoPermissionOverrideEnforced pins the per-repo team permission
// override: a team whose default is admin but whose access to one repo was
// explicitly downgraded to pull must NOT grant its members push/admin on that
// repo (the escalation the lattice previously allowed), and an upgrade override
// must grant it.
func TestTeamRepoPermissionOverrideEnforced(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "rbac-override-org")
	repo := s.seedOrgRepo(t, org, "downgraded", true)
	member := s.createTestUser(t, "rbac-override-member")
	// Team members are active org members; team grants only apply once the org
	// membership is active (a team member cannot be a mere pending invitee).
	s.store.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)

	team := s.store.CreateTeam(org.Login, "eng", store.TeamOptions{Permission: store.TeamPermission("admin")})
	if team == nil {
		t.Fatal("CreateTeam returned nil")
	}
	s.store.SetTeamMembership(org.Login, team.Slug, member.ID, store.TeamRoleMember)
	if !s.store.AddTeamRepo(org.Login, team.Slug, repo.FullName) {
		t.Fatal("AddTeamRepo failed")
	}

	// Downgrade THIS repo to pull; the team default stays admin.
	if !s.store.SetTeamRepoPermission(org.Login, team.Slug, repo.FullName, store.TeamPermission("pull")) {
		t.Fatal("SetTeamRepoPermission(pull) failed")
	}
	if store.CanPushRepo(s.store, member, repo) {
		t.Fatal("push granted despite a pull override on this repo (privilege escalation)")
	}
	if !store.CanReadRepoAsUser(s.store, member, repo) {
		t.Fatal("a pull override must still allow read")
	}

	// Upgrade the override to admin: push is now allowed.
	if !s.store.SetTeamRepoPermission(org.Login, team.Slug, repo.FullName, store.TeamPermission("admin")) {
		t.Fatal("SetTeamRepoPermission(admin) failed")
	}
	if !store.CanPushRepo(s.store, member, repo) {
		t.Fatal("an admin override should allow push")
	}
}
