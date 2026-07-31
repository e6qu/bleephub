package bleephub

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestGHESDirectoryMappingsOrganizationRenameAndReplicaCaches(t *testing.T) {
	s := newTestServer()
	s.registerGHESAdminStatsRoutes()
	fixed := time.Date(2026, time.July, 30, 14, 0, 0, 0, time.UTC)
	s.replaceClockNow(func() time.Time { return fixed })
	s.store.replaceClockNow(func() time.Time { return fixed })
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "directory-old", "Directory", "")
	team := s.store.CreateTeam(org.Login, "Platform", TeamOptions{})
	repo := s.store.CreateOrgRepo(org, admin, "service", "", false)
	if team == nil || repo == nil {
		t.Fatal("seed directory fixtures")
	}

	teamPath := "/api/v3/admin/ldap/teams/" + strconv.Itoa(team.ID)
	rec := enterpriseActionsRequest(t, s, http.MethodPatch, teamPath+"/mapping",
		map[string]interface{}{"ldap_dn": "cn=Platform,ou=teams,dc=example,dc=test"})
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK ||
		got["ldap_dn"] != "cn=Platform,ou=teams,dc=example,dc=test" {
		t.Fatalf("team LDAP mapping = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, teamPath+"/sync", nil)
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusCreated || got["status"] != "queued" {
		t.Fatalf("team LDAP sync = %d %#v", rec.Code, got)
	}

	userPath := "/api/v3/admin/ldap/users/admin"
	rec = enterpriseActionsRequest(t, s, http.MethodPatch, userPath+"/mapping",
		map[string]interface{}{"ldap_dn": "uid=admin,ou=people,dc=example,dc=test"})
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK ||
		got["ldap_dn"] != "uid=admin,ou=people,dc=example,dc=test" {
		t.Fatalf("user LDAP mapping = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, userPath+"/sync", nil)
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusCreated || got["status"] != "queued" {
		t.Fatalf("user LDAP sync = %d %#v", rec.Code, got)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPatch, "/api/v3/admin/organizations/directory-old",
		map[string]interface{}{"login": "directory-new"})
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusAccepted ||
		got["url"] != "http://example.com/api/v3/orgs/directory-new" {
		t.Fatalf("organization rename = %d %#v", rec.Code, got)
	}
	if s.store.GetOrg("directory-old") != nil || s.store.GetOrg("directory-new") == nil {
		t.Fatal("organization login index was not moved")
	}
	if s.store.GetRepo("directory-new", "service") == nil || s.store.GetRepo("directory-old", "service") != nil {
		t.Fatal("organization repository namespace was not moved")
	}
	if s.store.GetTeam("directory-new", team.Slug) == nil {
		t.Fatal("organization team index was not moved")
	}

	t.Setenv("BLEEPHUB_REPLICA_HOST", "cache-1.example.test")
	t.Setenv("BLEEPHUB_REPLICA_LOCATION", "eu-central")
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/repos/directory-new/service/replicas/caches", nil)
	caches := decodeGHESRecorderArray(t, rec)
	if rec.Code != http.StatusOK || len(caches) != 1 ||
		caches[0]["host"] != "cache-1.example.test" || caches[0]["location"] != "eu-central" ||
		caches[0]["git"].(map[string]interface{})["sync_status"] != "in_sync" {
		t.Fatalf("replica caches = %d %#v", rec.Code, caches)
	}
}

func TestGHESLDAPMappingsPersist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	p1, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatal(err)
	}
	st1.mu.Lock()
	st1.EnterpriseSettings.GHESLDAPUserMappings["octocat"] = "uid=octocat,dc=example,dc=test"
	st1.EnterpriseSettings.GHESLDAPTeamMappings[42] = "cn=platform,dc=example,dc=test"
	st1.persistEnterpriseSettings()
	st1.mu.Unlock()
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}
	p2, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatal(err)
	}
	if st2.EnterpriseSettings.GHESLDAPUserMappings["octocat"] != "uid=octocat,dc=example,dc=test" ||
		st2.EnterpriseSettings.GHESLDAPTeamMappings[42] != "cn=platform,dc=example,dc=test" {
		t.Fatalf("reloaded LDAP mappings = %#v %#v",
			st2.EnterpriseSettings.GHESLDAPUserMappings, st2.EnterpriseSettings.GHESLDAPTeamMappings)
	}
}
