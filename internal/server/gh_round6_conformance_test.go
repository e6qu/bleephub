package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestRepoSubscriptionHonorsIgnored covers that PUT /repos/{o}/{r}/subscription
// persists and returns the `ignored` flag (repo-watch "ignore"), not a hardcoded
// false.
func TestRepoSubscriptionHonorsIgnored(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "sub-repo"}).Body.Close()

	got := decodeJSONWithStatus(t, s.put(t, "/api/v3/repos/admin/sub-repo/subscription", defaultToken, map[string]interface{}{
		"subscribed": false, "ignored": true,
	}), 200)
	if got["ignored"] != true {
		t.Errorf("ignored = %v, want true", got["ignored"])
	}
}

// TestMetaExposesSSHHostKeyMembers covers that GET /meta carries the
// ssh_key_fingerprints and ssh_keys members clients read to seed known_hosts.
func TestMetaExposesSSHHostKeyMembers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	meta := decodeJSONWithStatus(t, s.get(t, "/api/v3/meta", defaultToken), 200)
	if _, ok := meta["ssh_key_fingerprints"].(map[string]interface{}); !ok {
		t.Errorf("meta.ssh_key_fingerprints missing/not an object: %v", meta["ssh_key_fingerprints"])
	}
	if _, ok := meta["ssh_keys"].([]interface{}); !ok {
		t.Errorf("meta.ssh_keys missing/not an array: %v", meta["ssh_keys"])
	}
}

// TestLicenseDetailCarriesChoosealicenseMetadata covers that GET /licenses/{key}
// returns real permission/condition/limitation slugs and a description, not
// empty arrays.
func TestLicenseDetailCarriesChoosealicenseMetadata(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	mit := decodeJSONWithStatus(t, s.get(t, "/api/v3/licenses/mit", defaultToken), 200)
	perms, _ := mit["permissions"].([]interface{})
	if len(perms) == 0 || perms[0] != "commercial-use" {
		t.Errorf("mit permissions = %v, want non-empty starting commercial-use", mit["permissions"])
	}
	if mit["description"] == "" || mit["description"] == "MIT License" {
		t.Errorf("mit description not populated: %v", mit["description"])
	}
	if mit["featured"] != true {
		t.Errorf("mit featured = %v, want true", mit["featured"])
	}
}

// TestLicenseListFeaturedFilter covers that GET /licenses returns only featured
// licenses by default and the full catalog for ?featured=false.
func TestLicenseListFeaturedFilter(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	keysOf := func(path string) map[string]bool {
		out := map[string]bool{}
		for _, l := range decodeJSONArray(t, s.get(t, path, defaultToken)) {
			out[l["key"].(string)] = true
		}
		return out
	}
	def := keysOf("/api/v3/licenses")
	if !def["mit"] || def["bsd-2-clause"] {
		t.Errorf("default /licenses = %v, want featured only (mit yes, bsd-2-clause no)", def)
	}
	all := keysOf("/api/v3/licenses?featured=false")
	if !all["bsd-2-clause"] {
		t.Errorf("/licenses?featured=false = %v, want the full catalog incl bsd-2-clause", all)
	}
}

// TestGPGKeyShapeUsesSpecFields covers that a created GPG key serializes with
// github's field names/required set (can_encrypt_comms, not the bogus
// can_encrypt_commits; primary_key_id/raw_key/subkeys/revoked present).
func TestGPGKeyShapeUsesSpecFields(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	created := decodeJSONWithStatus(t, s.post(t, "/api/v3/user/gpg_keys", defaultToken, map[string]interface{}{
		"armored_public_key": "-----BEGIN PGP PUBLIC KEY BLOCK-----\ntest\n-----END PGP PUBLIC KEY BLOCK-----",
		"name":               "k",
	}), 201)
	if _, bogus := created["can_encrypt_commits"]; bogus {
		t.Errorf("gpg key emits the non-spec can_encrypt_commits field")
	}
	for _, field := range []string{"can_encrypt_comms", "can_encrypt_storage", "primary_key_id", "raw_key", "subkeys", "revoked", "emails"} {
		if _, ok := created[field]; !ok {
			t.Errorf("gpg key missing required field %q: %v", field, created)
		}
	}
}

// TestGetGPGKeyRequiresAuthAndOwnership covers that GET /user/gpg_keys/{id}
// demands a caller and never discloses another account's key.
func TestGetGPGKeyRequiresAuthAndOwnership(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	created := decodeJSONWithStatus(t, s.post(t, "/api/v3/user/gpg_keys", defaultToken, map[string]interface{}{
		"armored_public_key": "-----BEGIN PGP PUBLIC KEY BLOCK-----\nx\n-----END PGP PUBLIC KEY BLOCK-----",
	}), 201)
	id := itoa(int(created["id"].(float64)))

	// Anonymous → 401.
	if resp := s.get(t, "/api/v3/user/gpg_keys/"+id, ""); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Errorf("anonymous get = %d, want 401", resp.StatusCode)
	}
	// Another user → 404 (never disclosed).
	other := s.createTestUser(t, "gpg-stranger")
	otherToken := s.store.CreateToken(other.ID, "read:gpg_key").Value
	if resp := s.get(t, "/api/v3/user/gpg_keys/"+id, otherToken); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Errorf("stranger get = %d, want 404", resp.StatusCode)
	}
}

// TestOrgMembersRoleFilter covers ?role on GET /orgs/{org}/members.
func TestOrgMembersRoleFilter(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "acme6", "Acme", "")
	member := s.createTestUser(t, "member6")
	s.store.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)

	logins := func(path string) map[string]bool {
		out := map[string]bool{}
		for _, u := range decodeJSONArray(t, s.get(t, path, defaultToken)) {
			out[u["login"].(string)] = true
		}
		return out
	}
	admins := logins("/api/v3/orgs/acme6/members?role=admin")
	if !admins["admin"] || admins["member6"] {
		t.Errorf("?role=admin = %v, want admin only", admins)
	}
	members := logins("/api/v3/orgs/acme6/members?role=member")
	if !members["member6"] || members["admin"] {
		t.Errorf("?role=member = %v, want member6 only", members)
	}
	if resp := s.get(t, "/api/v3/orgs/acme6/members?role=bogus", defaultToken); resp.StatusCode != http.StatusUnprocessableEntity {
		resp.Body.Close()
		t.Errorf("?role=bogus = %d, want 422", resp.StatusCode)
	}
}

// TestTeamMembersRoleFilter covers ?role on GET /orgs/{org}/teams/{slug}/members.
func TestTeamMembersRoleFilter(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "acme6t", "Acme", "")
	team := s.store.CreateTeam(org.Login, "Platform", store.TeamOptions{})
	maint := s.createTestUser(t, "maint6")
	mem := s.createTestUser(t, "mem6")
	s.store.SetTeamMembership(org.Login, team.Slug, maint.ID, store.TeamRoleMaintainer)
	s.store.SetTeamMembership(org.Login, team.Slug, mem.ID, store.TeamRoleMember)

	logins := func(path string) map[string]bool {
		out := map[string]bool{}
		for _, u := range decodeJSONArray(t, s.get(t, path, defaultToken)) {
			out[u["login"].(string)] = true
		}
		return out
	}
	base := "/api/v3/orgs/acme6t/teams/" + team.Slug + "/members"
	maints := logins(base + "?role=maintainer")
	if !maints["maint6"] || maints["mem6"] {
		t.Errorf("?role=maintainer = %v, want maint6 only", maints)
	}
}
