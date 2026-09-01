package bleephub

import (
	"net/http"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestNestedTeamInheritsParentRepoAccess pins that a child team's members
// inherit the repo grants of the parent team (GitHub cascades access down the
// hierarchy). It was not implemented, so a child-team member was denied.
func TestNestedTeamInheritsParentRepoAccess(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "nested-team-org")
	repo := s.seedOrgRepo(t, org, "shared", true) // private
	alice := s.createTestUser(t, "nested-alice")
	s.store.SetMembership(org.Login, alice.ID, store.OrgRoleMember, store.MembershipStateActive)

	parent := s.store.CreateTeam(org.Login, "parent", store.TeamOptions{Permission: store.TeamPermission("push")})
	if parent == nil {
		t.Fatal("CreateTeam(parent) returned nil")
	}
	child := s.store.CreateTeam(org.Login, "child", store.TeamOptions{ParentID: parent.ID})
	if child == nil {
		t.Fatal("CreateTeam(child) returned nil")
	}
	if !s.store.AddTeamRepo(org.Login, parent.Slug, repo.FullName) {
		t.Fatal("AddTeamRepo(parent) failed")
	}
	// Alice is a member ONLY of the child team, which itself has no repo grant.
	s.store.SetTeamMembership(org.Login, child.Slug, alice.ID, store.TeamRoleMember)

	if !store.CanPushRepo(s.store, alice, repo) {
		t.Fatal("a child-team member did not inherit the parent team's push grant")
	}

	// A user in neither team gets no PUSH (org base permission may grant read,
	// but push here comes only from the team hierarchy).
	bob := s.createTestUser(t, "nested-bob")
	s.store.SetMembership(org.Login, bob.ID, store.OrgRoleMember, store.MembershipStateActive)
	if store.CanPushRepo(s.store, bob, repo) {
		t.Fatal("a non-team member gained push access via team inheritance")
	}
}

// TestTeamMaintainerCannotInviteNonMember pins that a team maintainer may only
// add existing org members; inviting an unaffiliated user is an owner-only
// action (GitHub 422s otherwise).
func TestTeamMaintainerCannotInviteNonMember(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "maint-invite-org")
	team := s.store.CreateTeam(org.Login, "eng", store.TeamOptions{})

	maintainer := s.createTestUser(t, "the-maintainer")
	s.store.SetMembership(org.Login, maintainer.ID, store.OrgRoleMember, store.MembershipStateActive)
	s.store.SetTeamMembership(org.Login, team.Slug, maintainer.ID, store.TeamRoleMaintainer)
	maintTok := s.store.CreateToken(maintainer.ID, "admin:org").Value

	outsider := s.createTestUser(t, "unaffiliated-user")

	// The maintainer tries to add an org non-member to the team → 422.
	resp := s.put(t, "/api/v3/orgs/maint-invite-org/teams/eng/memberships/"+outsider.Login, maintTok, map[string]interface{}{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("maintainer inviting a non-member = %d, want 422", resp.StatusCode)
	}

	// An org owner CAN invite (pending).
	ok := s.put(t, "/api/v3/orgs/maint-invite-org/teams/eng/memberships/"+outsider.Login, defaultToken, map[string]interface{}{})
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("owner inviting a non-member = %d, want 200", ok.StatusCode)
	}
}

// TestCacheLookupPrimaryKeyIsExactOnly pins that the primary cache key is
// matched exactly (not as a prefix), so a restore key GitHub would select wins
// over a stale entry under the primary's prefix.
func TestCacheLookupPrimaryKeyIsExactOnly(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := "admin/cache-repo"
	version := "v1"
	if s.artifactStore.Caches == nil {
		s.artifactStore.Caches = map[int64]*store.CacheEntry{}
	}
	if s.artifactStore.CacheIndex == nil {
		s.artifactStore.CacheIndex = map[string]int64{}
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(id int64, key string, created time.Time) {
		e := &store.CacheEntry{ID: id, Repo: repo, Key: key, Version: version, Finalized: true, CreatedAt: created}
		s.artifactStore.Caches[id] = e
		s.artifactStore.CacheIndex[store.CacheLookupKey(repo, key, version)] = id
	}
	add(1, "A-1", base)
	add(2, "B-1", base.Add(time.Hour))

	// keys = [A, B]: no exact "A" (primary), so A must NOT prefix-match A-1;
	// B is a restore key and prefix-matches B-1.
	got := s.lookupFinalizedCacheLocked(repo, []string{"A", "B"}, version)
	if got == nil || got.Key != "B-1" {
		t.Fatalf("cache restore = %v, want B-1 (primary key must not prefix-match)", got)
	}

	// An exact primary hit still wins.
	add(3, "A", base.Add(2*time.Hour))
	if got := s.lookupFinalizedCacheLocked(repo, []string{"A", "B"}, version); got == nil || got.Key != "A" {
		t.Fatalf("exact primary cache hit = %v, want A", got)
	}
}
