package bleephub

import (
	"net/http"
	"testing"
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
