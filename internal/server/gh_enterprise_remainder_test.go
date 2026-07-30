package bleephub

import (
	"net/http"
	"strconv"
	"testing"
)

func TestEnterpriseRemainderJourneys(t *testing.T) {
	createOrgViaAdminAPI(t, "enterprise-remainder")
	org := testServer.store.GetOrg("enterprise-remainder")

	key := decodeJSON(t, ghPost(t, "/api/v3/user/keys", defaultToken, map[string]interface{}{
		"title": "organization SSO key",
		"key":   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBleephubCredentialAuthorization",
	}))
	if key["id"] == nil {
		t.Fatalf("created SSH key = %#v", key)
	}
	credentials := decodeJSONArray(t, ghGet(t,
		"/api/v3/orgs/enterprise-remainder/credential-authorizations", defaultToken))
	var sshCredentialID int
	for _, credential := range credentials {
		if credential["credential_type"] == "SSH key" {
			sshCredentialID = int(credential["credential_id"].(float64))
			if credential["fingerprint"] == "" || credential["authorized_credential_title"] != "organization SSO key" {
				t.Fatalf("SSH credential authorization = %#v", credential)
			}
		}
	}
	if sshCredentialID == 0 {
		t.Fatalf("credential authorizations did not include SSH key: %#v", credentials)
	}
	expectStatus(t, ghDelete(t, "/api/v3/orgs/enterprise-remainder/credential-authorizations/"+
		strconv.Itoa(sshCredentialID), defaultToken), http.StatusNoContent, "revoke SSH authorization")

	testServer.store.mu.Lock()
	testServer.store.EnterpriseSettings.OrganizationCustomProperties["cost_center"] = &CustomProperty{
		PropertyName: "cost_center", ValueType: "string",
	}
	testServer.store.persistEnterpriseSettings()
	testServer.store.mu.Unlock()
	propertyPath := "/api/v3/organizations/" + strconv.Itoa(org.ID) + "/org-properties/values"
	expectStatus(t, ghPatch(t, propertyPath, defaultToken, map[string]interface{}{
		"properties": []map[string]interface{}{{"property_name": "cost_center", "value": "engineering"}},
	}), http.StatusNoContent, "set organization property values by ID")
	properties := decodeJSONArray(t, ghGet(t, propertyPath, defaultToken))
	if len(properties) != 1 || properties[0]["property_name"] != "cost_center" || properties[0]["value"] != "engineering" {
		t.Fatalf("organization property values = %#v", properties)
	}

	repo := decodeJSON(t, ghPost(t, "/api/v3/orgs/enterprise-remainder/repos", defaultToken,
		map[string]interface{}{"name": "large-assets", "auto_init": true}))
	repoID := int(repo["id"].(float64))
	repoPath := "/api/v3/repos/enterprise-remainder/large-assets"
	expectStatus(t, ghPut(t, repoPath+"/lfs", defaultToken, nil), http.StatusNoContent, "enable LFS")
	if stored := testServer.store.GetRepoByID(repoID); stored == nil || !stored.LFSEnabled {
		t.Fatalf("repository after enabling LFS = %#v", stored)
	}
	expectStatus(t, ghDelete(t, repoPath+"/lfs", defaultToken), http.StatusNoContent, "disable LFS")
	if stored := testServer.store.GetRepoByID(repoID); stored == nil || stored.LFSEnabled {
		t.Fatalf("repository after disabling LFS = %#v", stored)
	}

	testServer.store.mu.Lock()
	configID := testServer.store.NextCodeSecurityConfigID
	testServer.store.NextCodeSecurityConfigID++
	testServer.store.CodeSecurityConfigs[org.Login] = map[int]*CodeSecurityConfiguration{
		configID: {ID: configID, OrgLogin: org.Login, Name: "advanced", AdvancedSecurity: "enabled"},
	}
	testServer.store.CodeSecurityRepoAttachments[org.Login] = map[int]int{repoID: configID}
	testServer.store.mu.Unlock()
	billing := decodeJSON(t, ghGet(t,
		"/api/v3/orgs/enterprise-remainder/settings/billing/advanced-security", defaultToken))
	if billing["total_count"] != float64(1) || len(billing["repositories"].([]interface{})) != 1 {
		t.Fatalf("advanced security billing = %#v", billing)
	}
}
