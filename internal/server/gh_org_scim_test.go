package bleephub

import (
	"net/http"
	"testing"
)

func TestOrganizationSCIMUserLifecycle(t *testing.T) {
	createOrgViaAdminAPI(t, "org-scim")
	base := "/api/v3/scim/v2/organizations/org-scim/Users"
	created := ghPost(t, base, defaultToken, map[string]interface{}{
		"schemas":     []string{scimUserSchema},
		"externalId":  "directory-42",
		"userName":    "org.scim.user",
		"displayName": "Organization SCIM User",
		"active":      true,
		"emails": []map[string]interface{}{{
			"value": "org.scim.user@example.test", "primary": true,
		}},
	})
	if created.StatusCode != http.StatusCreated || created.Header.Get("Content-Type") != "application/scim+json" {
		created.Body.Close()
		t.Fatalf("create SCIM user = %d content-type=%q", created.StatusCode, created.Header.Get("Content-Type"))
	}
	user := decodeJSON(t, created)
	id := user["id"].(string)
	if user["userName"] != "org-scim-user" || user["active"] != true {
		t.Fatalf("created SCIM user = %#v", user)
	}
	isActiveMember := func(userID int) bool {
		membership := testServer.store.GetMembership("org-scim", userID)
		return membership != nil && membership.State == MembershipStateActive
	}
	backing := testServer.store.LookupUserByLogin("org-scim-user")
	if backing == nil || !isActiveMember(backing.ID) {
		t.Fatalf("backing user/membership was not provisioned: %#v", backing)
	}

	listed := decodeJSON(t, ghGet(t, base+"?filter=externalId%20eq%20%22directory-42%22", defaultToken))
	if listed["totalResults"] != float64(1) || len(listed["Resources"].([]interface{})) != 1 {
		t.Fatalf("filtered SCIM users = %#v", listed)
	}

	patched := ghPatch(t, base+"/"+id, defaultToken, map[string]interface{}{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []map[string]interface{}{
			{"op": "replace", "path": "displayName", "value": "Renamed SCIM User"},
			{"op": "replace", "path": "active", "value": false},
		},
	})
	if patched.StatusCode != http.StatusOK {
		patched.Body.Close()
		t.Fatalf("patch SCIM user = %d", patched.StatusCode)
	}
	user = decodeJSON(t, patched)
	if user["displayName"] != "Renamed SCIM User" || user["active"] != false {
		t.Fatalf("patched SCIM user = %#v", user)
	}
	if isActiveMember(backing.ID) {
		t.Fatal("inactive SCIM identity retained organization membership")
	}

	replaced := ghPut(t, base+"/"+id, defaultToken, map[string]interface{}{
		"schemas": []string{scimUserSchema}, "externalId": "directory-42",
		"userName": "org.scim.user", "displayName": "Active Again", "active": true,
	})
	if replaced.StatusCode != http.StatusOK {
		replaced.Body.Close()
		t.Fatalf("replace SCIM user = %d", replaced.StatusCode)
	}
	replaced.Body.Close()
	if !isActiveMember(backing.ID) {
		t.Fatal("reactivated SCIM identity did not restore organization membership")
	}

	expectStatus(t, ghDelete(t, base+"/"+id, defaultToken), http.StatusNoContent, "delete SCIM user")
	if isActiveMember(backing.ID) {
		t.Fatal("deleted SCIM identity retained organization membership")
	}
	expectStatus(t, ghGet(t, base+"/"+id, defaultToken), http.StatusNotFound, "deleted SCIM user")

	// Keep the OpenAPI shape ratchet non-vacuous when this GHEC-only route
	// family is selected in isolation.
	expectStatus(t, ghGet(t, "/api/v3/user", defaultToken), http.StatusOK, "shape-ratchet control")
}

// TestOrganizationSCIMCannotHijackExistingAccount pins AUTH-103: an org's SCIM
// may not bind to, force-enroll, or rewrite a global account it does not
// manage. Provisioning a SCIM user whose userName collides with a pre-existing
// account is a conflict, and the victim is neither renamed nor made a member.
func TestOrganizationSCIMCannotHijackExistingAccount(t *testing.T) {
	createOrgViaAdminAPI(t, "evilcorp")

	// A pre-existing, non-SCIM account.
	testServer.store.mu.Lock()
	victim := &User{ID: testServer.store.NextUser, Login: "victim", Name: "Victim", Email: "victim@real.test", Type: "User", StarredRepos: map[string]bool{}}
	testServer.store.NextUser++
	testServer.store.Users[victim.ID] = victim
	testServer.store.UsersByLogin["victim"] = victim
	testServer.store.mu.Unlock()

	base := "/api/v3/scim/v2/organizations/evilcorp/Users"
	resp := ghPost(t, base, defaultToken, map[string]interface{}{
		"schemas": []string{scimUserSchema}, "userName": "victim",
		"displayName": "Pwned", "active": true,
		"emails": []map[string]interface{}{{"value": "attacker@evil.test", "primary": true}},
	})
	if resp.StatusCode != http.StatusConflict {
		resp.Body.Close()
		t.Fatalf("SCIM provisioning of an existing account = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	after := testServer.store.LookupUserByLogin("victim")
	if after == nil || after.ID != victim.ID || after.Name != "Victim" || after.Email != "victim@real.test" {
		t.Fatalf("victim account was mutated by SCIM: %#v", after)
	}
	if m := testServer.store.GetMembership("evilcorp", victim.ID); m != nil && m.State == MembershipStateActive {
		t.Fatal("victim was force-enrolled into the org via SCIM")
	}
}
