package bleephub

import (
	"net/http"
	"strconv"
	"testing"
)

func TestOrganizationAnnouncementAndRoleGovernance(t *testing.T) {
	createOrgViaAdminAPI(t, "governance-org")
	base := "/api/v3/orgs/governance-org"

	empty := decodeJSON(t, ghGet(t, base+"/announcement", defaultToken))
	if empty["announcement"] != "" || empty["expires_at"] != nil || empty["user_dismissible"] != false {
		t.Fatalf("default announcement = %#v", empty)
	}
	set := decodeJSON(t, ghPatch(t, base+"/announcement", defaultToken, map[string]interface{}{
		"announcement":     "Organization maintenance",
		"expires_at":       "2032-04-05T06:07:08Z",
		"user_dismissible": true,
	}))
	if set["announcement"] != "Organization maintenance" || set["expires_at"] != "2032-04-05T06:07:08Z" ||
		set["user_dismissible"] != true {
		t.Fatalf("set announcement = %#v", set)
	}
	expectStatus(t, ghPatch(t, base+"/announcement", defaultToken, map[string]interface{}{
		"announcement": "bad", "expires_at": "tomorrow",
	}), http.StatusUnprocessableEntity, "invalid announcement expiration")

	repositoryPermissions := decodeJSONArray(t, ghGet(t, base+"/repository-fine-grained-permissions", defaultToken))
	legacyPermissions := decodeJSONArray(t, ghGet(t, base+"/fine_grained_permissions", defaultToken))
	organizationPermissions := decodeJSONArray(t, ghGet(t, base+"/organization-fine-grained-permissions", defaultToken))
	if len(repositoryPermissions) == 0 || len(repositoryPermissions) != len(legacyPermissions) || len(organizationPermissions) == 0 {
		t.Fatalf("permission catalogs: repository=%d legacy=%d organization=%d",
			len(repositoryPermissions), len(legacyPermissions), len(organizationPermissions))
	}

	repoRole := decodeJSON(t, ghPost(t, base+"/custom-repository-roles", defaultToken, map[string]interface{}{
		"name": "release manager", "description": "Ships releases", "base_role": "write",
		"permissions": []string{"create_tag", "delete_tag"},
	}))
	repoRoleID := int(repoRole["id"].(float64))
	if repoRole["base_role"] != "write" || repoRole["organization"].(map[string]interface{})["login"] != "governance-org" {
		t.Fatalf("created repository role = %#v", repoRole)
	}
	updatedRepoRole := decodeJSON(t, ghPatch(t, base+"/custom_roles/"+strconv.Itoa(repoRoleID), defaultToken, map[string]interface{}{
		"name": "release captain", "description": nil,
	}))
	if updatedRepoRole["name"] != "release captain" || updatedRepoRole["description"] != nil {
		t.Fatalf("updated repository role = %#v", updatedRepoRole)
	}
	org := testServer.store.GetOrg("governance-org")
	legacyList := decodeJSON(t, ghGet(t, "/api/v3/organizations/"+strconv.Itoa(org.ID)+"/custom_roles", defaultToken))
	if legacyList["total_count"] != float64(1) {
		t.Fatalf("legacy custom role list = %#v", legacyList)
	}
	expectStatus(t, ghPost(t, base+"/custom-repository-roles", defaultToken, map[string]interface{}{
		"name": "invalid", "base_role": "write", "permissions": []string{"invented_permission"},
	}), http.StatusUnprocessableEntity, "unknown repository permission")

	orgRole := decodeJSON(t, ghPost(t, base+"/organization-roles", defaultToken, map[string]interface{}{
		"name": "billing reader", "description": "Reads organization settings",
		"permissions": []string{"read_organization_custom_properties"}, "base_role": "read",
	}))
	orgRoleID := int(orgRole["id"].(float64))
	if orgRole["source"] != "Organization" || orgRole["base_role"] != "read" {
		t.Fatalf("created organization role = %#v", orgRole)
	}
	listing := decodeJSON(t, ghGet(t, base+"/organization-roles", defaultToken))
	if listing["total_count"] != float64(len(predefinedOrgRoles)+1) {
		t.Fatalf("organization role list = %#v", listing)
	}
	updatedOrgRole := decodeJSON(t, ghPatch(t, base+"/organization-roles/"+strconv.Itoa(orgRoleID), defaultToken, map[string]interface{}{
		"name": "billing steward", "base_role": "none",
	}))
	if updatedOrgRole["name"] != "billing steward" || updatedOrgRole["base_role"] != nil {
		t.Fatalf("updated organization role = %#v", updatedOrgRole)
	}

	expectStatus(t, ghDelete(t, base+"/organization-roles/"+strconv.Itoa(orgRoleID), defaultToken),
		http.StatusNoContent, "delete custom organization role")
	expectStatus(t, ghGet(t, base+"/organization-roles/"+strconv.Itoa(orgRoleID), defaultToken),
		http.StatusNotFound, "deleted custom organization role")
	expectStatus(t, ghDelete(t, base+"/custom-repository-roles/"+strconv.Itoa(repoRoleID), defaultToken),
		http.StatusNoContent, "delete custom repository role")
	expectStatus(t, ghDelete(t, base+"/announcement", defaultToken), http.StatusNoContent, "delete announcement")
}
