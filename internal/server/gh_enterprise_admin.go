package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// registerGHEnterpriseAdminRoutes mounts the enterprise administration APIs
// that are shared by GHES and GHEC. They intentionally use the enterprise
// owner gate rather than an organization-owner check: an enterprise policy
// spans every organization on this single-enterprise instance.
func (s *Server) registerGHEnterpriseAdminRoutes() {
	s.route("GET /api/v3/enterprises/{enterprise}/announcement", s.requireEnterpriseOwner(s.handleGetEnterpriseAnnouncement))
	s.route("PATCH /api/v3/enterprises/{enterprise}/announcement", s.requireEnterpriseOwner(s.handleSetEnterpriseAnnouncement))
	s.route("DELETE /api/v3/enterprises/{enterprise}/announcement", s.requireEnterpriseOwner(s.handleDeleteEnterpriseAnnouncement))

	s.route("POST /api/v3/enterprises/{enterprise}/access-restrictions/enable", s.requireEnterpriseOwner(s.handleEnableEnterpriseAccessRestrictions))
	s.route("POST /api/v3/enterprises/{enterprise}/access-restrictions/disable", s.requireEnterpriseOwner(s.handleDisableEnterpriseAccessRestrictions))

	s.route("GET /api/v3/enterprises/{enterprise}/code_security_and_analysis", s.requireEnterpriseOwner(s.handleGetEnterpriseCodeSecurityAndAnalysis))
	s.route("PATCH /api/v3/enterprises/{enterprise}/code_security_and_analysis", s.requireEnterpriseOwner(s.handleUpdateEnterpriseCodeSecurityAndAnalysis))
	s.route("POST /api/v3/enterprises/{enterprise}/{security_product}/{enablement}", s.requireEnterpriseOwner(s.handleSetEnterpriseSecurityFeature))

	s.route("POST /api/v3/enterprises/{enterprise}/credential-authorizations/revoke-all", s.requireEnterpriseOwner(s.handleRevokeEnterpriseCredentials))
	s.route("POST /api/v3/enterprises/{enterprise}/credential-authorizations/revoke-credential-type", s.requireEnterpriseOwner(s.handleRevokeEnterpriseCredentialType))
	s.route("POST /api/v3/enterprises/{enterprise}/credential-authorizations/{username}/revoke", s.requireEnterpriseOwner(s.handleRevokeUserCredentials))
	s.route("POST /api/v3/enterprises/{enterprise}/credential-authorizations/{username}/revoke-credential-type", s.requireEnterpriseOwner(s.handleRevokeUserCredentialType))
}

func (s *Server) handleGetEnterpriseAnnouncement(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.RLock()
	announcement := s.store.EnterpriseSettings.Announcement
	if announcement == nil {
		announcement = &EnterpriseAnnouncement{}
	}
	result := *announcement
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, result)
}

type enterpriseAnnouncementRequest struct {
	Announcement    *string `json:"announcement"`
	ExpiresAt       *string `json:"expires_at"`
	UserDismissible *bool   `json:"user_dismissible"`
}

func (s *Server) handleSetEnterpriseAnnouncement(w http.ResponseWriter, r *http.Request) {
	var req enterpriseAnnouncementRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Announcement == nil {
		writeGHValidationError(w, "EnterpriseAnnouncement", "announcement", "missing_field")
		return
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, *req.ExpiresAt); err != nil {
			writeGHValidationError(w, "EnterpriseAnnouncement", "expires_at", "invalid")
			return
		}
	}
	announcement := &EnterpriseAnnouncement{Announcement: *req.Announcement}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		expires := *req.ExpiresAt
		announcement.ExpiresAt = &expires
	}
	if req.UserDismissible != nil {
		announcement.UserDismissible = *req.UserDismissible
	}
	s.store.mu.Lock()
	s.store.EnterpriseSettings.Announcement = announcement
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, announcement)
}

func (s *Server) handleDeleteEnterpriseAnnouncement(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.Lock()
	s.store.EnterpriseSettings.Announcement = nil
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setEnterpriseAccessRestrictions(w http.ResponseWriter, enabled bool) {
	s.store.mu.Lock()
	s.store.EnterpriseSettings.AccessRestrictionsEnabled = enabled
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnableEnterpriseAccessRestrictions(w http.ResponseWriter, _ *http.Request) {
	s.setEnterpriseAccessRestrictions(w, true)
}

func (s *Server) handleDisableEnterpriseAccessRestrictions(w http.ResponseWriter, _ *http.Request) {
	s.setEnterpriseAccessRestrictions(w, false)
}

func (s *Server) handleGetEnterpriseCodeSecurityAndAnalysis(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.RLock()
	settings := s.store.EnterpriseSettings.CodeSecurityAndAnalysis
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, settings)
}

type enterpriseCodeSecurityRequest struct {
	AdvancedSecurityEnabledForNewRepositories                  *bool   `json:"advanced_security_enabled_for_new_repositories"`
	AdvancedSecurityEnabledNewUserNamespaceRepos               *bool   `json:"advanced_security_enabled_new_user_namespace_repos"`
	DependabotAlertsEnabledForNewRepositories                  *bool   `json:"dependabot_alerts_enabled_for_new_repositories"`
	SecretScanningEnabledForNewRepositories                    *bool   `json:"secret_scanning_enabled_for_new_repositories"`
	SecretScanningPushProtectionEnabledForNewRepositories      *bool   `json:"secret_scanning_push_protection_enabled_for_new_repositories"`
	SecretScanningPushProtectionCustomLink                     *string `json:"secret_scanning_push_protection_custom_link"`
	SecretScanningNonProviderPatternsEnabledForNewRepositories *bool   `json:"secret_scanning_non_provider_patterns_enabled_for_new_repositories"`
}

func (req enterpriseCodeSecurityRequest) apply(settings *EnterpriseCodeSecurity) {
	setBool := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	setBool(&settings.AdvancedSecurityEnabledForNewRepositories, req.AdvancedSecurityEnabledForNewRepositories)
	setBool(&settings.AdvancedSecurityEnabledNewUserNamespaceRepos, req.AdvancedSecurityEnabledNewUserNamespaceRepos)
	setBool(&settings.DependabotAlertsEnabledForNewRepositories, req.DependabotAlertsEnabledForNewRepositories)
	setBool(&settings.SecretScanningEnabledForNewRepositories, req.SecretScanningEnabledForNewRepositories)
	setBool(&settings.SecretScanningPushProtectionEnabledForNewRepositories, req.SecretScanningPushProtectionEnabledForNewRepositories)
	setBool(&settings.SecretScanningNonProviderPatternsEnabledForNewRepositories, req.SecretScanningNonProviderPatternsEnabledForNewRepositories)
	if req.SecretScanningPushProtectionCustomLink != nil {
		link := strings.TrimSpace(*req.SecretScanningPushProtectionCustomLink)
		if link == "" {
			settings.SecretScanningPushProtectionCustomLink = nil
		} else {
			settings.SecretScanningPushProtectionCustomLink = &link
		}
	}
}

func (s *Server) handleUpdateEnterpriseCodeSecurityAndAnalysis(w http.ResponseWriter, r *http.Request) {
	var req enterpriseCodeSecurityRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.mu.Lock()
	req.apply(&s.store.EnterpriseSettings.CodeSecurityAndAnalysis)
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetEnterpriseSecurityFeature(w http.ResponseWriter, r *http.Request) {
	enabled := r.PathValue("enablement") == "enable_all"
	if !enabled && r.PathValue("enablement") != "disable_all" {
		writeGHValidationError(w, "EnterpriseSecurityFeature", "enablement", "invalid")
		return
	}
	s.store.mu.Lock()
	settings := &s.store.EnterpriseSettings.CodeSecurityAndAnalysis
	switch r.PathValue("security_product") {
	case "advanced_security":
		settings.AdvancedSecurityEnabledForNewRepositories = enabled
	case "advanced_security_user_namespace":
		settings.AdvancedSecurityEnabledNewUserNamespaceRepos = enabled
	case "dependabot_alerts":
		settings.DependabotAlertsEnabledForNewRepositories = enabled
	case "secret_scanning":
		settings.SecretScanningEnabledForNewRepositories = enabled
	case "secret_scanning_push_protection":
		settings.SecretScanningPushProtectionEnabledForNewRepositories = enabled
	case "secret_scanning_non_provider_patterns":
		settings.SecretScanningNonProviderPatternsEnabledForNewRepositories = enabled
	default:
		s.store.mu.Unlock()
		writeGHValidationError(w, "EnterpriseSecurityFeature", "security_product", "invalid")
		return
	}
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

type enterpriseCredentialRevocationRequest struct {
	CredentialType    string `json:"credential_type"`
	RevokeCredentials bool   `json:"revoke_credentials"`
}

func decodeEnterpriseCredentialRevocation(w http.ResponseWriter, r *http.Request, credentialTypeRequired bool) (enterpriseCredentialRevocationRequest, bool) {
	var req enterpriseCredentialRevocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return req, false
	}
	if credentialTypeRequired && !validEnterpriseCredentialType(req.CredentialType) {
		writeGHValidationError(w, "CredentialAuthorization", "credential_type", "invalid")
		return req, false
	}
	// bleephub models a regular GHES enterprise rather than an EMU enterprise.
	if req.RevokeCredentials {
		writeGHValidationError(w, "CredentialAuthorization", "revoke_credentials", "invalid")
		return req, false
	}
	return req, true
}

func validEnterpriseCredentialType(v string) bool {
	switch v {
	case "classic_pat", "fine_grained_pat", "ssh_key", "oauth_app_token":
		return true
	default:
		return false
	}
}

func (s *Server) handleRevokeEnterpriseCredentials(w http.ResponseWriter, r *http.Request) {
	if _, ok := decodeEnterpriseCredentialRevocation(w, r, false); !ok {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": "Credential authorization revocation has been queued"})
}

func (s *Server) handleRevokeEnterpriseCredentialType(w http.ResponseWriter, r *http.Request) {
	if _, ok := decodeEnterpriseCredentialRevocation(w, r, true); !ok {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": "Credential type revocation has been queued"})
}

func (s *Server) enterpriseCredentialUser(w http.ResponseWriter, username string) *User {
	user := s.store.LookupUserByLogin(username)
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
	return user
}

func (s *Server) handleRevokeUserCredentials(w http.ResponseWriter, r *http.Request) {
	if _, ok := decodeEnterpriseCredentialRevocation(w, r, false); !ok {
		return
	}
	username := r.PathValue("username")
	if s.enterpriseCredentialUser(w, username) == nil {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"message": fmt.Sprintf("Credential authorization revocation for user '%s' has been queued", username),
	})
}

func (s *Server) handleRevokeUserCredentialType(w http.ResponseWriter, r *http.Request) {
	if _, ok := decodeEnterpriseCredentialRevocation(w, r, true); !ok {
		return
	}
	username := r.PathValue("username")
	if s.enterpriseCredentialUser(w, username) == nil {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"message": fmt.Sprintf("Credential type revocation for user '%s' has been queued", username),
	})
}
