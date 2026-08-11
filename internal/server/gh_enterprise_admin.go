package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

	s.route("GET /api/v3/enterprises/{enterprise}/audit-log", s.requireEnterpriseOwner(s.handleEnterpriseAuditLog))
	s.route("GET /api/v3/enterprises/{enterprise}/audit-log/stream-key", s.requireEnterpriseOwner(s.handleEnterpriseAuditLogStreamKey))
	s.route("GET /api/v3/enterprises/{enterprise}/audit-log/streams", s.requireEnterpriseOwner(s.handleListEnterpriseAuditLogStreams))
	s.route("POST /api/v3/enterprises/{enterprise}/audit-log/streams", s.requireEnterpriseOwner(s.handleCreateEnterpriseAuditLogStream))
	s.route("GET /api/v3/enterprises/{enterprise}/audit-log/streams/{stream_id}", s.requireEnterpriseOwner(s.handleGetEnterpriseAuditLogStream))
	s.route("PUT /api/v3/enterprises/{enterprise}/audit-log/streams/{stream_id}", s.requireEnterpriseOwner(s.handleUpdateEnterpriseAuditLogStream))
	s.route("DELETE /api/v3/enterprises/{enterprise}/audit-log/streams/{stream_id}", s.requireEnterpriseOwner(s.handleDeleteEnterpriseAuditLogStream))

	s.route("GET /api/v3/enterprises/{enterprise}/network-configurations", s.requireEnterpriseOwner(s.handleListEnterpriseNetworkConfigurations))
	s.route("POST /api/v3/enterprises/{enterprise}/network-configurations", s.requireEnterpriseOwner(s.handleCreateEnterpriseNetworkConfiguration))
	s.route("GET /api/v3/enterprises/{enterprise}/network-configurations/{network_configuration_id}", s.requireEnterpriseOwner(s.handleGetEnterpriseNetworkConfiguration))
	s.route("PATCH /api/v3/enterprises/{enterprise}/network-configurations/{network_configuration_id}", s.requireEnterpriseOwner(s.handleUpdateEnterpriseNetworkConfiguration))
	s.route("DELETE /api/v3/enterprises/{enterprise}/network-configurations/{network_configuration_id}", s.requireEnterpriseOwner(s.handleDeleteEnterpriseNetworkConfiguration))
	s.route("GET /api/v3/enterprises/{enterprise}/network-settings/{network_settings_id}", s.requireEnterpriseOwner(s.handleGetEnterpriseNetworkSettings))
}

func (s *Server) handleGetEnterpriseAnnouncement(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	announcement := s.store.EnterpriseSettings.Announcement
	if announcement == nil {
		announcement = &EnterpriseAnnouncement{}
	}
	result := *announcement
	s.store.Mu.RUnlock()
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
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.Announcement = announcement
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, announcement)
}

func (s *Server) handleDeleteEnterpriseAnnouncement(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.Announcement = nil
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setEnterpriseAccessRestrictions(w http.ResponseWriter, enabled bool) {
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.AccessRestrictionsEnabled = enabled
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnableEnterpriseAccessRestrictions(w http.ResponseWriter, _ *http.Request) {
	s.setEnterpriseAccessRestrictions(w, true)
}

func (s *Server) handleDisableEnterpriseAccessRestrictions(w http.ResponseWriter, _ *http.Request) {
	s.setEnterpriseAccessRestrictions(w, false)
}

func (s *Server) handleGetEnterpriseCodeSecurityAndAnalysis(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	settings := s.store.EnterpriseSettings.CodeSecurityAndAnalysis
	s.store.Mu.RUnlock()
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
	s.store.Mu.Lock()
	req.apply(&s.store.EnterpriseSettings.CodeSecurityAndAnalysis)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetEnterpriseSecurityFeature(w http.ResponseWriter, r *http.Request) {
	enabled := r.PathValue("enablement") == "enable_all"
	if !enabled && r.PathValue("enablement") != "disable_all" {
		writeGHValidationError(w, "EnterpriseSecurityFeature", "enablement", "invalid")
		return
	}
	s.store.Mu.Lock()
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
		s.store.Mu.Unlock()
		writeGHValidationError(w, "EnterpriseSecurityFeature", "security_product", "invalid")
		return
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
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

func (s *Server) handleEnterpriseAuditLog(w http.ResponseWriter, r *http.Request) {
	order := r.URL.Query().Get("order")
	if order != "" && order != "asc" && order != "desc" {
		writeGHValidationError(w, "AuditLog", "order", "invalid")
		return
	}
	include := r.URL.Query().Get("include")
	if include != "" && include != "web" && include != "git" && include != "all" {
		writeGHValidationError(w, "AuditLog", "include", "invalid")
		return
	}
	s.store.Misc.Mu.RLock()
	entries := make([]*AuditEntry, 0, len(s.store.Misc.AuditLog))
	for _, entry := range s.store.Misc.AuditLog {
		if phrase := r.URL.Query().Get("phrase"); phrase != "" && !auditEntryMatchesPhrase(entry, phrase) {
			continue
		}
		entries = append(entries, entry)
	}
	s.store.Misc.Mu.RUnlock()
	if order == "asc" {
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, entries))
}

func (s *Server) handleEnterpriseAuditLogStreamKey(w http.ResponseWriter, _ *http.Request) {
	s.writeActionsPublicKey(w)
}

func enterpriseAuditLogStreamJSON(stream *EnterpriseAuditLogStream) map[string]interface{} {
	return map[string]interface{}{
		"id":             stream.ID,
		"stream_type":    stream.StreamType,
		"stream_details": stream.StreamDetails,
		"enabled":        stream.Enabled,
		"created_at":     stream.CreatedAt.Format(time.RFC3339),
		"updated_at":     stream.UpdatedAt.Format(time.RFC3339),
		"paused_at":      stream.PausedAt,
	}
}

func (s *Server) handleListEnterpriseAuditLogStreams(w http.ResponseWriter, r *http.Request) {
	s.store.Mu.RLock()
	out := make([]map[string]interface{}, 0, len(s.store.EnterpriseSettings.AuditLogStreams))
	for _, stream := range s.store.EnterpriseSettings.AuditLogStreams {
		out = append(out, enterpriseAuditLogStreamJSON(stream))
	}
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func enterpriseAuditLogStreamID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("stream_id"))
	if err != nil || id < 1 {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return 0, false
	}
	return id, true
}

func (s *Server) enterpriseAuditLogStreamLocked(id int) *EnterpriseAuditLogStream {
	for _, stream := range s.store.EnterpriseSettings.AuditLogStreams {
		if stream.ID == id {
			return stream
		}
	}
	return nil
}

func (s *Server) handleGetEnterpriseAuditLogStream(w http.ResponseWriter, r *http.Request) {
	id, ok := enterpriseAuditLogStreamID(w, r)
	if !ok {
		return
	}
	s.store.Mu.RLock()
	stream := s.enterpriseAuditLogStreamLocked(id)
	if stream == nil {
		s.store.Mu.RUnlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := enterpriseAuditLogStreamJSON(stream)
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

type enterpriseAuditLogStreamRequest struct {
	Enabled        *bool                  `json:"enabled"`
	StreamType     string                 `json:"stream_type"`
	VendorSpecific map[string]interface{} `json:"vendor_specific"`
}

func validEnterpriseAuditLogStreamType(streamType string) bool {
	switch streamType {
	case "Azure Blob Storage", "Azure Event Hubs", "Amazon S3", "Splunk",
		"HTTPS Event Collector", "Google Cloud Storage", "Datadog":
		return true
	default:
		return false
	}
}

func enterpriseAuditLogStreamDetails(streamType string, vendor map[string]interface{}) string {
	for _, field := range []string{"site", "region", "domain", "bucket", "container", "name"} {
		if value, ok := vendor[field].(string); ok && value != "" {
			return value
		}
	}
	return streamType
}

func decodeEnterpriseAuditLogStreamRequest(w http.ResponseWriter, r *http.Request) (enterpriseAuditLogStreamRequest, bool) {
	var req enterpriseAuditLogStreamRequest
	if !decodeJSONBody(w, r, &req) {
		return req, false
	}
	if req.Enabled == nil {
		writeGHValidationError(w, "AuditLogStream", "enabled", "missing_field")
		return req, false
	}
	if !validEnterpriseAuditLogStreamType(req.StreamType) {
		writeGHValidationError(w, "AuditLogStream", "stream_type", "invalid")
		return req, false
	}
	if len(req.VendorSpecific) == 0 {
		writeGHValidationError(w, "AuditLogStream", "vendor_specific", "missing_field")
		return req, false
	}
	return req, true
}

func (s *Server) handleCreateEnterpriseAuditLogStream(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeEnterpriseAuditLogStreamRequest(w, r)
	if !ok {
		return
	}
	now := s.store.CurrentTime()
	stream := &EnterpriseAuditLogStream{
		StreamType: req.StreamType, StreamDetails: enterpriseAuditLogStreamDetails(req.StreamType, req.VendorSpecific),
		Enabled: *req.Enabled, VendorSpecific: req.VendorSpecific, CreatedAt: now, UpdatedAt: now,
	}
	if !stream.Enabled {
		stream.PausedAt = &now
	}
	s.store.Mu.Lock()
	stream.ID = s.store.EnterpriseSettings.NextAuditLogStreamID
	s.store.EnterpriseSettings.NextAuditLogStreamID++
	s.store.EnterpriseSettings.AuditLogStreams = append(s.store.EnterpriseSettings.AuditLogStreams, stream)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, enterpriseAuditLogStreamJSON(stream))
}

func (s *Server) handleUpdateEnterpriseAuditLogStream(w http.ResponseWriter, r *http.Request) {
	id, ok := enterpriseAuditLogStreamID(w, r)
	if !ok {
		return
	}
	req, ok := decodeEnterpriseAuditLogStreamRequest(w, r)
	if !ok {
		return
	}
	now := s.store.CurrentTime()
	s.store.Mu.Lock()
	stream := s.enterpriseAuditLogStreamLocked(id)
	if stream == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	stream.StreamType = req.StreamType
	stream.StreamDetails = enterpriseAuditLogStreamDetails(req.StreamType, req.VendorSpecific)
	stream.VendorSpecific = req.VendorSpecific
	stream.Enabled = *req.Enabled
	stream.UpdatedAt = now
	if stream.Enabled {
		stream.PausedAt = nil
	} else {
		stream.PausedAt = &now
	}
	s.store.PersistEnterpriseSettings()
	out := enterpriseAuditLogStreamJSON(stream)
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteEnterpriseAuditLogStream(w http.ResponseWriter, r *http.Request) {
	id, ok := enterpriseAuditLogStreamID(w, r)
	if !ok {
		return
	}
	s.store.Mu.Lock()
	for i, stream := range s.store.EnterpriseSettings.AuditLogStreams {
		if stream.ID == id {
			s.store.EnterpriseSettings.AuditLogStreams = append(
				s.store.EnterpriseSettings.AuditLogStreams[:i],
				s.store.EnterpriseSettings.AuditLogStreams[i+1:]...,
			)
			s.store.PersistEnterpriseSettings()
			s.store.Mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.store.Mu.Unlock()
	writeGHError(w, http.StatusNotFound, "Not Found")
}

func (s *Server) enterpriseNetworkScope() string {
	return "enterprise:" + s.enterpriseSlug()
}

func (s *Server) handleListEnterpriseNetworkConfigurations(w http.ResponseWriter, r *http.Request) {
	s.handleListNetworkConfigurationsForScope(w, r, s.enterpriseNetworkScope())
}

func (s *Server) handleCreateEnterpriseNetworkConfiguration(w http.ResponseWriter, r *http.Request) {
	s.handleCreateNetworkConfigurationForScope(w, r, s.enterpriseNetworkScope())
}

func (s *Server) handleGetEnterpriseNetworkConfiguration(w http.ResponseWriter, r *http.Request) {
	s.handleGetNetworkConfigurationForScope(w, r, s.enterpriseNetworkScope())
}

func (s *Server) handleUpdateEnterpriseNetworkConfiguration(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateNetworkConfigurationForScope(w, r, s.enterpriseNetworkScope())
}

func (s *Server) handleDeleteEnterpriseNetworkConfiguration(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteNetworkConfigurationForScope(w, r, s.enterpriseNetworkScope())
}

func (s *Server) handleGetEnterpriseNetworkSettings(w http.ResponseWriter, r *http.Request) {
	s.handleGetNetworkSettingsForScope(w, r, s.enterpriseNetworkScope())
}
