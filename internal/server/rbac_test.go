package bleephub

import (
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func TestSiteAdministratorCanAccessOrganizationRepository(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	creator := st.LookupUserByLogin("admin")
	org := st.CreateOrg(creator, "site-admin-access", "Site admin access", "")
	if org == nil {
		t.Fatal("CreateOrg returned nil")
	}
	repo := st.CreateOrgRepo(org, creator, "private-repository", "", true)
	if repo == nil {
		t.Fatal("CreateOrgRepo returned nil")
	}

	st.Mu.Lock()
	siteAdmin := &store.User{
		ID:           st.NextUser,
		Login:        "external-site-admin",
		Type:         "User",
		SiteAdmin:    true,
		StarredRepos: map[string]time.Time{},
	}
	st.NextUser++
	st.Users[siteAdmin.ID] = siteAdmin
	st.UsersByLogin[siteAdmin.Login] = siteAdmin
	st.Mu.Unlock()

	if !canReadRepoAsUser(st, siteAdmin, repo) {
		t.Fatal("site administrator could not read organization repository")
	}
	if !canPushRepo(st, siteAdmin, repo) {
		t.Fatal("site administrator could not push organization repository")
	}
	if !canAdminRepo(st, siteAdmin, repo) {
		t.Fatal("site administrator could not administer organization repository")
	}
}

func TestOrganizationBaseRepositoryPermissionControlsMemberCapabilities(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	creator := st.LookupUserByLogin("admin")
	org := st.CreateOrg(creator, "base-permission", "Base permission", "")
	repo := st.CreateOrgRepo(org, creator, "private-repository", "", true)
	if org == nil || repo == nil {
		t.Fatal("failed to create organization repository")
	}

	st.Mu.Lock()
	member := &store.User{ID: st.NextUser, Login: "ordinary-member", Type: "User"}
	st.NextUser++
	st.Users[member.ID] = member
	st.UsersByLogin[member.Login] = member
	st.Mu.Unlock()
	st.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)

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
			st.UpdateOrg(org.Login, func(current *store.Org) {
				current.DefaultRepositoryPermission = tt.base
			})
			if got := canReadRepoAsUser(st, member, repo); got != tt.read {
				t.Errorf("canReadRepoAsUser() = %v, want %v", got, tt.read)
			}
			if got := canPushRepo(st, member, repo); got != tt.push {
				t.Errorf("canPushRepo() = %v, want %v", got, tt.push)
			}
			if got := canAdminRepo(st, member, repo); got != tt.administer {
				t.Errorf("canAdminRepo() = %v, want %v", got, tt.administer)
			}
		})
	}
}

func TestOrganizationOwnerRightsIgnoreRestrictiveBasePermission(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	owner := st.LookupUserByLogin("admin")
	org := st.CreateOrg(owner, "owner-permission", "Owner permission", "")
	repo := st.CreateOrgRepo(org, owner, "private-repository", "", true)
	if org == nil || repo == nil {
		t.Fatal("failed to create organization repository")
	}
	st.UpdateOrg(org.Login, func(current *store.Org) {
		current.DefaultRepositoryPermission = "none"
	})

	if !canReadRepoAsUser(st, owner, repo) ||
		!canPushRepo(st, owner, repo) ||
		!canAdminRepo(st, owner, repo) {
		t.Fatal("organization owner lost repository rights under base permission none")
	}
}
