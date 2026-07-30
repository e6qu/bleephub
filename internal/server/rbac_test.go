package bleephub

import (
	"testing"
)

func TestSiteAdministratorCanAccessOrganizationRepository(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	creator := store.LookupUserByLogin("admin")
	org := store.CreateOrg(creator, "site-admin-access", "Site admin access", "")
	if org == nil {
		t.Fatal("CreateOrg returned nil")
	}
	repo := store.CreateOrgRepo(org, creator, "private-repository", "", true)
	if repo == nil {
		t.Fatal("CreateOrgRepo returned nil")
	}

	store.mu.Lock()
	siteAdmin := &User{
		ID:           store.NextUser,
		Login:        "external-site-admin",
		Type:         "User",
		SiteAdmin:    true,
		StarredRepos: map[string]bool{},
	}
	store.NextUser++
	store.Users[siteAdmin.ID] = siteAdmin
	store.UsersByLogin[siteAdmin.Login] = siteAdmin
	store.mu.Unlock()

	if !canReadRepoAsUser(store, siteAdmin, repo) {
		t.Fatal("site administrator could not read organization repository")
	}
	if !canPushRepo(store, siteAdmin, repo) {
		t.Fatal("site administrator could not push organization repository")
	}
	if !canAdminRepo(store, siteAdmin, repo) {
		t.Fatal("site administrator could not administer organization repository")
	}
}

func TestOrganizationBaseRepositoryPermissionControlsMemberCapabilities(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	creator := store.LookupUserByLogin("admin")
	org := store.CreateOrg(creator, "base-permission", "Base permission", "")
	repo := store.CreateOrgRepo(org, creator, "private-repository", "", true)
	if org == nil || repo == nil {
		t.Fatal("failed to create organization repository")
	}

	store.mu.Lock()
	member := &User{ID: store.NextUser, Login: "ordinary-member", Type: "User"}
	store.NextUser++
	store.Users[member.ID] = member
	store.UsersByLogin[member.Login] = member
	store.mu.Unlock()
	store.SetMembership(org.Login, member.ID, OrgRoleMember, MembershipStateActive)

	tests := []struct {
		base                   string
		read, push, administer bool
	}{
		{base: "none"},
		{base: "", read: true}, // unset is GitHub's default "read"
		{base: "read", read: true},
		{base: "write", read: true, push: true},
		{base: "admin", read: true, push: true, administer: true},
	}
	for _, tt := range tests {
		t.Run("base_"+tt.base, func(t *testing.T) {
			store.UpdateOrg(org.Login, func(current *Org) {
				current.DefaultRepositoryPermission = tt.base
			})
			if got := canReadRepoAsUser(store, member, repo); got != tt.read {
				t.Errorf("canReadRepoAsUser() = %v, want %v", got, tt.read)
			}
			if got := canPushRepo(store, member, repo); got != tt.push {
				t.Errorf("canPushRepo() = %v, want %v", got, tt.push)
			}
			if got := canAdminRepo(store, member, repo); got != tt.administer {
				t.Errorf("canAdminRepo() = %v, want %v", got, tt.administer)
			}
		})
	}
}

func TestOrganizationOwnerRightsIgnoreRestrictiveBasePermission(t *testing.T) {
	store := NewStore()
	store.SeedDefaultUser()
	owner := store.LookupUserByLogin("admin")
	org := store.CreateOrg(owner, "owner-permission", "Owner permission", "")
	repo := store.CreateOrgRepo(org, owner, "private-repository", "", true)
	if org == nil || repo == nil {
		t.Fatal("failed to create organization repository")
	}
	store.UpdateOrg(org.Login, func(current *Org) {
		current.DefaultRepositoryPermission = "none"
	})

	if !canReadRepoAsUser(store, owner, repo) ||
		!canPushRepo(store, owner, repo) ||
		!canAdminRepo(store, owner, repo) {
		t.Fatal("organization owner lost repository rights under base permission none")
	}
}
