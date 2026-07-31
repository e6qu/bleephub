package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestEnterpriseAnnouncementLifecycle(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseAdminRoutes()
	base := "/api/v3/enterprises/bleephub/announcement"

	rec := enterpriseActionsRequest(t, s, http.MethodGet, base, nil)
	got := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || got["announcement"] != "" || got["expires_at"] != nil || got["user_dismissible"] != false {
		t.Fatalf("default announcement = %d %#v", rec.Code, got)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base, map[string]interface{}{
		"announcement":     "Maintenance at **midnight**",
		"expires_at":       "2030-01-02T03:04:05Z",
		"user_dismissible": true,
	})
	got = decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || got["announcement"] != "Maintenance at **midnight**" ||
		got["expires_at"] != "2030-01-02T03:04:05Z" || got["user_dismissible"] != true {
		t.Fatalf("set announcement = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base, map[string]interface{}{
		"announcement": "invalid expiry", "expires_at": "next Tuesday",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid expiry = %d %q, want 422", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete announcement = %d %q", rec.Code, rec.Body.String())
	}
	got = decodeRecorderObject(t, enterpriseActionsRequest(t, s, http.MethodGet, base, nil))
	if got["announcement"] != "" || got["expires_at"] != nil {
		t.Fatalf("announcement after delete = %#v", got)
	}
}

func TestEnterpriseLegacySecurityPolicyAndAccessRestrictions(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseAdminRoutes()
	base := "/api/v3/enterprises/bleephub"

	for suffix, want := range map[string]bool{
		"/access-restrictions/enable":  true,
		"/access-restrictions/disable": false,
	} {
		rec := enterpriseActionsRequest(t, s, http.MethodPost, base+suffix, nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("POST %s = %d %q", suffix, rec.Code, rec.Body.String())
		}
		s.store.mu.RLock()
		got := s.store.EnterpriseSettings.AccessRestrictionsEnabled
		s.store.mu.RUnlock()
		if got != want {
			t.Fatalf("POST %s left access restrictions %v, want %v", suffix, got, want)
		}
	}

	policyPath := base + "/code_security_and_analysis"
	rec := enterpriseActionsRequest(t, s, http.MethodPatch, policyPath, map[string]interface{}{
		"advanced_security_enabled_for_new_repositories":                     true,
		"secret_scanning_enabled_for_new_repositories":                       true,
		"secret_scanning_push_protection_custom_link":                        "https://example.test/security",
		"secret_scanning_non_provider_patterns_enabled_for_new_repositories": true,
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update code security = %d %q", rec.Code, rec.Body.String())
	}
	got := decodeRecorderObject(t, enterpriseActionsRequest(t, s, http.MethodGet, policyPath, nil))
	if got["advanced_security_enabled_for_new_repositories"] != true ||
		got["secret_scanning_enabled_for_new_repositories"] != true ||
		got["secret_scanning_push_protection_custom_link"] != "https://example.test/security" {
		t.Fatalf("code security policy = %#v", got)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/dependabot_alerts/enable_all", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("enable security feature = %d %q", rec.Code, rec.Body.String())
	}
	got = decodeRecorderObject(t, enterpriseActionsRequest(t, s, http.MethodGet, policyPath, nil))
	if got["dependabot_alerts_enabled_for_new_repositories"] != true {
		t.Fatalf("dependabot policy = %#v", got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/unknown/enable_all", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown security product = %d %q, want 422", rec.Code, rec.Body.String())
	}
}

func TestEnterpriseCredentialRevocationValidationAndResponses(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseAdminRoutes()
	base := "/api/v3/enterprises/bleephub/credential-authorizations"

	rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/revoke-all", nil)
	got := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusAccepted || got["message"] != "Credential authorization revocation has been queued" {
		t.Fatalf("revoke all = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/revoke-credential-type", map[string]interface{}{
		"credential_type": "fine_grained_pat",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("revoke type = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/revoke-credential-type", map[string]interface{}{
		"credential_type": "password",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid type = %d %q, want 422", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/admin/revoke", nil)
	got = decodeRecorderObject(t, rec)
	if rec.Code != http.StatusAccepted ||
		got["message"] != "Credential authorization revocation for user 'admin' has been queued" {
		t.Fatalf("revoke user = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/missing/revoke", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke missing user = %d %q, want 404", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/revoke-all", map[string]interface{}{
		"revoke_credentials": true,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("EMU-only revoke_credentials = %d %q, want 422", rec.Code, rec.Body.String())
	}
}

func TestEnterpriseAuditLogAndStreamLifecycle(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseAdminRoutes()
	base := "/api/v3/enterprises/bleephub/audit-log"
	s.replaceClockNow(func() time.Time { return fixedTestTime })
	s.recordAuditEvent("repo.create", "admin", "audit-a", map[string]interface{}{"repo": "audit-a/one"})
	s.recordAuditEvent("team.add_member", "admin", "audit-b", nil)

	rec := enterpriseActionsRequest(t, s, http.MethodGet, base+"?phrase=audit-a&order=asc", nil)
	var entries []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode audit log: %v", err)
	}
	if rec.Code != http.StatusOK || len(entries) != 1 || entries[0]["action"] != "repo.create" {
		t.Fatalf("enterprise audit log = %d %#v", rec.Code, entries)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"?include=unknown", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid audit include = %d %q, want 422", rec.Code, rec.Body.String())
	}

	key := decodeRecorderObject(t, enterpriseActionsRequest(t, s, http.MethodGet, base+"/stream-key", nil))
	if key["key_id"] == nil || key["key"] == nil {
		t.Fatalf("audit stream key = %#v", key)
	}

	streams := base + "/streams"
	rec = enterpriseActionsRequest(t, s, http.MethodPost, streams, map[string]interface{}{
		"enabled": false, "stream_type": "Datadog",
		"vendor_specific": map[string]interface{}{"site": "EU1", "key_id": key["key_id"], "encrypted_token": "sealed"},
	})
	stream := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || stream["id"] != float64(1) || stream["stream_details"] != "EU1" ||
		stream["enabled"] != false || stream["paused_at"] == nil {
		t.Fatalf("create audit stream = %d %#v", rec.Code, stream)
	}
	if _, leaked := stream["vendor_specific"]; leaked {
		t.Fatalf("audit stream leaked vendor credentials: %#v", stream)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPut, streams+"/1", map[string]interface{}{
		"enabled": true, "stream_type": "Amazon S3",
		"vendor_specific": map[string]interface{}{"bucket": "audit-bucket", "region": "eu-central-1"},
	})
	stream = decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || stream["stream_type"] != "Amazon S3" ||
		stream["stream_details"] != "eu-central-1" || stream["paused_at"] != nil {
		t.Fatalf("update audit stream = %d %#v", rec.Code, stream)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, streams, nil)
	var listed []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("list audit streams = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, streams+"/1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete audit stream = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, streams+"/1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted audit stream = %d %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestEnterpriseNetworkConfigurationsArePersistentAndScopeIsolated(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseAdminRoutes()
	s.replaceClockNow(func() time.Time { return fixedTestTime })
	scope := s.enterpriseNetworkScope()
	settings, err := s.store.CreateNetworkSettings(scope, "enterprise-private-network", "/subscriptions/enterprise/subnets/actions", "eastus")
	if err != nil {
		t.Fatalf("create enterprise network settings: %v", err)
	}
	orgSettings, err := s.store.CreateNetworkSettings("network-scope-org", "org-private-network", "/subscriptions/org/subnets/actions", "westus")
	if err != nil {
		t.Fatalf("create org network settings: %v", err)
	}
	base := "/api/v3/enterprises/bleephub/network-configurations"
	rec := enterpriseActionsRequest(t, s, http.MethodPost, base, map[string]interface{}{
		"name": "enterprise_network", "compute_service": "actions",
		"network_settings_ids": []string{settings.ID},
	})
	created := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || created["name"] != "enterprise_network" ||
		created["created_on"] != fixedTestTime.Format(time.RFC3339) {
		t.Fatalf("create enterprise network configuration = %d %#v", rec.Code, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("enterprise network configuration has no id: %#v", created)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base, nil)
	listed := decodeRecorderObject(t, rec)
	if listed["total_count"] != float64(1) {
		t.Fatalf("enterprise network configurations = %#v", listed)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/enterprises/bleephub/network-settings/"+settings.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get enterprise settings = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/enterprises/bleephub/network-settings/"+orgSettings.ID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-scope org settings = %d %q, want 404", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base+"/"+id, map[string]interface{}{
		"name": "enterprise_network_updated", "failover_network_enabled": true,
	})
	updated := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || updated["name"] != "enterprise_network_updated" ||
		updated["failover_network_enabled"] != true {
		t.Fatalf("update enterprise network configuration = %d %#v", rec.Code, updated)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete enterprise network configuration = %d %q", rec.Code, rec.Body.String())
	}
}
