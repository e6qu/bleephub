package bleephub

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestEnterpriseRemainderJourneys(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	srv.createOrg(t, "enterprise-remainder")
	org := srv.store.GetOrg("enterprise-remainder")

	key := decodeJSON(t, srv.post(t, "/api/v3/user/keys", defaultToken, map[string]interface{}{
		"title": "organization SSO key",
		"key":   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBleephubCredentialAuthorization",
	}))
	if key["id"] == nil {
		t.Fatalf("created SSH key = %#v", key)
	}
	credentials := decodeJSONArray(t, srv.get(t,
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
	expectStatus(t, srv.delete(t, "/api/v3/orgs/enterprise-remainder/credential-authorizations/"+
		strconv.Itoa(sshCredentialID), defaultToken), http.StatusNoContent, "revoke SSH authorization")

	srv.store.Mu.Lock()
	srv.store.EnterpriseSettings.OrganizationCustomProperties["cost_center"] = &store.CustomProperty{
		PropertyName: "cost_center", ValueType: "string",
	}
	srv.store.PersistEnterpriseSettings()
	srv.store.Mu.Unlock()
	propertyPath := "/api/v3/organizations/" + strconv.Itoa(org.ID) + "/org-properties/values"
	expectStatus(t, srv.patch(t, propertyPath, defaultToken, map[string]interface{}{
		"properties": []map[string]interface{}{{"property_name": "cost_center", "value": "engineering"}},
	}), http.StatusNoContent, "set organization property values by ID")
	properties := decodeJSONArray(t, srv.get(t, propertyPath, defaultToken))
	if len(properties) != 1 || properties[0]["property_name"] != "cost_center" || properties[0]["value"] != "engineering" {
		t.Fatalf("organization property values = %#v", properties)
	}

	repo := decodeJSON(t, srv.post(t, "/api/v3/orgs/enterprise-remainder/repos", defaultToken,
		map[string]interface{}{"name": "large-assets", "auto_init": true}))
	repoID := int(repo["id"].(float64))
	repoPath := "/api/v3/repos/enterprise-remainder/large-assets"
	expectStatus(t, srv.put(t, repoPath+"/lfs", defaultToken, nil), http.StatusNoContent, "enable LFS")
	if stored := srv.store.GetRepoByID(repoID); stored == nil || !stored.LFSEnabled {
		t.Fatalf("repository after enabling LFS = %#v", stored)
	}
	expectStatus(t, srv.delete(t, repoPath+"/lfs", defaultToken), http.StatusNoContent, "disable LFS")
	if stored := srv.store.GetRepoByID(repoID); stored == nil || stored.LFSEnabled {
		t.Fatalf("repository after disabling LFS = %#v", stored)
	}

	srv.store.Mu.Lock()
	configID := srv.store.NextCodeSecurityConfigID
	srv.store.NextCodeSecurityConfigID++
	srv.store.CodeSecurityConfigs[org.Login] = map[int]*store.CodeSecurityConfiguration{
		configID: {ID: configID, OrgLogin: org.Login, Name: "advanced", AdvancedSecurity: "enabled"},
	}
	srv.store.CodeSecurityRepoAttachments[org.Login] = map[int]int{repoID: configID}
	srv.store.Mu.Unlock()
	billing := decodeJSON(t, srv.get(t,
		"/api/v3/orgs/enterprise-remainder/settings/billing/advanced-security", defaultToken))
	if billing["total_count"] != float64(1) || len(billing["repositories"].([]interface{})) != 1 {
		t.Fatalf("advanced security billing = %#v", billing)
	}
}
