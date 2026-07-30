package bleephub

import (
	"strconv"
	"testing"
)

func TestOrganizationGovernanceStatePersistenceReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	t.Setenv("BLEEPHUB_ADMIN_TOKEN", defaultToken)

	p1, err := NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	st1.SeedDefaultUser()
	admin := st1.LookupUserByLogin("admin")
	org := st1.CreateOrg(admin, "governance-reload", "Governance", "")
	repo := st1.CreateOrgRepo(org, admin, "lfs-reload", "", false)
	st1.mu.Lock()
	repo.LFSEnabled = true
	st1.persist.MustPut("repos", strconv.Itoa(repo.ID), repo)
	st1.mu.Unlock()
	now := fixedTestTime
	description := "persists"
	baseRole := "read"
	st1.mu.Lock()
	st1.OrgAnnouncements[org.Login] = &EnterpriseAnnouncement{Announcement: "Persistent banner", UserDismissible: true}
	st1.OrgCustomRepoRoles[org.Login] = map[int]*OrgCustomRepositoryRole{
		1001: {
			ID: 1001, Name: "persistent repo role", Description: &description, BaseRole: "write",
			Permissions: []string{"create_tag"}, OrgLogin: org.Login, CreatedAt: now, UpdatedAt: now,
		},
	}
	st1.OrgCustomRoles[org.Login] = map[int]*OrgCustomOrganizationRole{
		1002: {
			ID: 1002, Name: "persistent org role", Description: &description, BaseRole: &baseRole,
			Permissions: []string{"read_organization_custom_properties"}, OrgLogin: org.Login,
			CreatedAt: now, UpdatedAt: now,
		},
	}
	st1.OrgSCIMUsers[org.Login] = map[string]*EnterpriseSCIMUser{
		"persistent-scim-user": {
			Schemas: []string{scimUserSchema}, ID: "persistent-scim-user",
			ExternalID: "directory-persistent", UserName: "admin", Active: true,
			UserID: admin.ID, CreatedAt: now, UpdatedAt: now,
		},
	}
	st1.OrgExternalGroups[org.Login] = map[string]*OrgExternalIdentityGroup{
		"persistent-group": {
			ID: "persistent-group", NumericID: 17, Name: "Persistent Group",
			Description: "survives reload", MemberIDs: []int{admin.ID}, UpdatedAt: now,
		},
	}
	st1.TeamExternalGroupIDs[71] = []string{"persistent-group"}
	st1.persist.MustPut("org_announcements", org.Login, st1.OrgAnnouncements[org.Login])
	st1.persist.MustPut("org_custom_repo_roles", org.Login, st1.OrgCustomRepoRoles[org.Login])
	st1.persist.MustPut("org_custom_roles", org.Login, st1.OrgCustomRoles[org.Login])
	st1.persist.MustPut("org_scim_users", org.Login, st1.OrgSCIMUsers[org.Login])
	st1.persist.MustPut("org_external_groups", org.Login, st1.OrgExternalGroups[org.Login])
	st1.persist.MustPut("team_external_group_ids", "71", st1.TeamExternalGroupIDs[71])
	st1.mu.Unlock()
	if err := p1.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	p2, err := NewPersistence()
	if err != nil {
		t.Fatalf("reopen persistence: %v", err)
	}
	defer p2.Close()
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload persistence: %v", err)
	}
	if got := st2.OrgAnnouncements[org.Login]; got == nil || got.Announcement != "Persistent banner" {
		t.Fatalf("reloaded announcement = %#v", got)
	}
	if got := st2.OrgCustomRepoRoles[org.Login][1001]; got == nil || got.Name != "persistent repo role" {
		t.Fatalf("reloaded custom repository role = %#v", got)
	}
	if got := st2.OrgCustomRoles[org.Login][1002]; got == nil || got.Name != "persistent org role" {
		t.Fatalf("reloaded custom organization role = %#v", got)
	}
	if got := st2.OrgSCIMUsers[org.Login]["persistent-scim-user"]; got == nil || got.ExternalID != "directory-persistent" {
		t.Fatalf("reloaded organization SCIM user = %#v", got)
	}
	if got := st2.OrgExternalGroups[org.Login]["persistent-group"]; got == nil || got.NumericID != 17 {
		t.Fatalf("reloaded external identity group = %#v", got)
	}
	if got := st2.TeamExternalGroupIDs[71]; len(got) != 1 || got[0] != "persistent-group" {
		t.Fatalf("reloaded team external group mapping = %#v", got)
	}
	if st2.NextOrgExternalGroupID <= 17 {
		t.Fatalf("next external group id = %d, want > 17", st2.NextOrgExternalGroupID)
	}
	if got := st2.GetRepo(org.Login, "lfs-reload"); got == nil || !got.LFSEnabled {
		t.Fatalf("reloaded repository LFS state = %#v", got)
	}
	if st2.NextOrgCustomRoleID <= 1002 {
		t.Fatalf("next custom role id = %d, want > 1002", st2.NextOrgCustomRoleID)
	}
}
