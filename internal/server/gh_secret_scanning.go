package bleephub

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHSecretScanningRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/secret-scanning/custom-patterns",
		s.requirePerm(store.ScopeSecurityEvents, store.PermRead, s.handleListRepoSecretScanningCustomPatterns))
	s.route("POST /api/v3/repos/{owner}/{repo}/secret-scanning/custom-patterns",
		s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleCreateRepoSecretScanningCustomPatterns))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/secret-scanning/custom-patterns",
		s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleDeleteRepoSecretScanningCustomPatterns))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/secret-scanning/custom-patterns/{pattern_id}",
		s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleUpdateRepoSecretScanningCustomPattern))
	s.route("GET /api/v3/repos/{owner}/{repo}/secret-scanning/alerts", s.handleListSecretScanningAlerts)
	s.route("GET /api/v3/repos/{owner}/{repo}/secret-scanning/alerts/{alert_number}", s.handleGetSecretScanningAlert)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/secret-scanning/alerts/{alert_number}", s.handleUpdateSecretScanningAlert)
	s.route("GET /api/v3/repos/{owner}/{repo}/secret-scanning/alerts/{alert_number}/locations", s.handleListSecretScanningAlertLocations)

	// Organization-level alerts and pattern configurations
	s.route("GET /api/v3/orgs/{org}/secret-scanning/alerts",
		s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermRead, s.handleListSecretScanningOrgAlerts))
	s.route("GET /api/v3/orgs/{org}/secret-scanning/pattern-configurations",
		s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermRead, s.handleListSecretScanningPatternConfigurations))
	s.route("PATCH /api/v3/orgs/{org}/secret-scanning/pattern-configurations",
		s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermWrite, s.handleUpdateSecretScanningPatternConfigurations))
	s.route("GET /api/v3/orgs/{org}/secret-scanning/custom-patterns",
		s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermRead, s.handleListOrgSecretScanningCustomPatterns))
	s.route("POST /api/v3/orgs/{org}/secret-scanning/custom-patterns",
		s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermWrite, s.handleCreateOrgSecretScanningCustomPatterns))
	s.route("DELETE /api/v3/orgs/{org}/secret-scanning/custom-patterns",
		s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermWrite, s.handleDeleteOrgSecretScanningCustomPatterns))
	s.route("PATCH /api/v3/orgs/{org}/secret-scanning/custom-patterns/{pattern_id}",
		s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermWrite, s.handleUpdateOrgSecretScanningCustomPattern))

	// Push protection bypasses + scan history
	s.route("POST /api/v3/repos/{owner}/{repo}/secret-scanning/push-protection-bypasses", s.handleCreateSecretScanningPushProtectionBypass)
	s.route("GET /api/v3/repos/{owner}/{repo}/secret-scanning/scan-history", s.handleGetSecretScanningScanHistory)
}

func (s *Server) handleListSecretScanningAlerts(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	state := r.URL.Query().Get("state")
	secretType := r.URL.Query().Get("secret_type")
	resolution := r.URL.Query().Get("resolution")
	sort := r.URL.Query().Get("sort")
	direction := r.URL.Query().Get("direction")

	alerts := s.store.ListSecretScanningAlerts(repo.FullName, state, secretType, resolution, sort, direction)
	page := paginateAndLink(w, r, alerts)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, len(page))
	for i, a := range page {
		out[i] = secretScanningAlertToJSON(a, baseURL, repo)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetSecretScanningAlert(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	a := s.lookupSecretScanningAlert(w, r, repo)
	if a == nil {
		return
	}
	writeJSON(w, http.StatusOK, secretScanningAlertToJSON(a, s.baseURL(r), repo))
}

func (s *Server) handleUpdateSecretScanningAlert(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecurityEvents, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	a := s.lookupSecretScanningAlert(w, r, repo)
	if a == nil {
		return
	}

	var req struct {
		State             string `json:"state"`
		Resolution        string `json:"resolution"`
		ResolutionComment string `json:"resolution_comment"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.State == "" && req.Resolution == "" {
		store.WriteGHValidationError(w, "SecretScanningAlert", "state", "missing_field")
		return
	}
	if err := s.store.UpdateSecretScanningAlert(a, req.State, req.Resolution, req.ResolutionComment); err != nil {
		store.WriteGHValidationError(w, "SecretScanningAlert", "state", "invalid")
		return
	}
	writeJSON(w, http.StatusOK, secretScanningAlertToJSON(a, s.baseURL(r), repo))
}

func (s *Server) handleListSecretScanningAlertLocations(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	a := s.lookupSecretScanningAlert(w, r, repo)
	if a == nil {
		return
	}
	out := make([]map[string]interface{}, len(a.Locations))
	for i, loc := range a.Locations {
		out[i] = secretScanningLocationToJSON(loc)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleListSecretScanningOrgAlerts(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForSecretScanning(w, r)
	if !ok {
		return
	}

	state := r.URL.Query().Get("state")
	secretType := r.URL.Query().Get("secret_type")
	resolution := r.URL.Query().Get("resolution")
	sortField := r.URL.Query().Get("sort")
	direction := r.URL.Query().Get("direction")

	alerts := s.store.ListSecretScanningAlertsByOrg(org.ID, state, secretType, resolution, sortField, direction)
	page := paginateAndLink(w, r, alerts)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(page))
	for _, a := range page {
		repo := s.store.GetRepoByFullName(a.RepoKey)
		if repo == nil {
			continue
		}
		alertJSON := secretScanningAlertToJSON(a, baseURL, repo)
		alertJSON["repository"] = simpleRepoJSON(repo, s.store, baseURL)
		out = append(out, alertJSON)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListSecretScanningPatternConfigurations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.ListSecretScanningPatternConfigurations(r.PathValue("org")))
}

func validPushProtectionSetting(setting string, allowNotSet bool) bool {
	switch setting {
	case "enabled", "disabled":
		return true
	case "not-set":
		return allowNotSet
	}
	return false
}

func (s *Server) handleUpdateSecretScanningPatternConfigurations(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateSecretScanningPatternConfigurationsForScope(w, r, r.PathValue("org"))
}

func (s *Server) handleUpdateSecretScanningPatternConfigurationsForScope(w http.ResponseWriter, r *http.Request, scope string) {
	var req struct {
		PatternConfigVersion    *string `json:"pattern_config_version"`
		ProviderPatternSettings []struct {
			TokenType             string `json:"token_type"`
			PushProtectionSetting string `json:"push_protection_setting"`
		} `json:"provider_pattern_settings"`
		CustomPatternSettings []struct {
			TokenType             string  `json:"token_type"`
			CustomPatternVersion  *string `json:"custom_pattern_version"`
			PushProtectionSetting string  `json:"push_protection_setting"`
		} `json:"custom_pattern_settings"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	provider := map[string]string{}
	for _, setting := range req.ProviderPatternSettings {
		if !store.IsSecretScanningProviderPattern(setting.TokenType) {
			store.WriteGHValidationError(w, "SecretScanningPatternConfiguration", "token_type", "invalid")
			return
		}
		if !validPushProtectionSetting(setting.PushProtectionSetting, true) {
			store.WriteGHValidationError(w, "SecretScanningPatternConfiguration", "push_protection_setting", "invalid")
			return
		}
		provider[setting.TokenType] = setting.PushProtectionSetting
	}
	custom := map[string]string{}
	for _, setting := range req.CustomPatternSettings {
		if setting.TokenType == "" {
			store.WriteGHValidationError(w, "SecretScanningPatternConfiguration", "token_type", "missing_field")
			return
		}
		if !validPushProtectionSetting(setting.PushProtectionSetting, false) {
			store.WriteGHValidationError(w, "SecretScanningPatternConfiguration", "push_protection_setting", "invalid")
			return
		}
		custom[setting.TokenType] = setting.PushProtectionSetting
	}

	newVersion, ok := s.store.UpdateSecretScanningPatternConfig(scope, req.PatternConfigVersion, provider, custom)
	if !ok {
		writeGHError(w, http.StatusConflict, "pattern_config_version does not match the current configuration version")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pattern_config_version": newVersion,
	})
}

func (s *Server) handleCreateSecretScanningPushProtectionBypass(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerHasRepoPermission(r.Context(), repo, store.ScopeSecurityEvents, store.PermWrite) {
		writeGHError(w, http.StatusForbidden, "User does not have enough permissions to perform this action.")
		return
	}

	var req struct {
		Reason        string `json:"reason"`
		PlaceholderID string `json:"placeholder_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.Reason {
	case "false_positive", "used_in_tests", "will_fix_later":
	default:
		store.WriteGHValidationError(w, "SecretScanningPushProtectionBypass", "reason", "invalid")
		return
	}
	if req.PlaceholderID == "" {
		store.WriteGHValidationError(w, "SecretScanningPushProtectionBypass", "placeholder_id", "missing_field")
		return
	}

	bypass := s.store.CreateSecretScanningPushProtectionBypass(repo.FullName, req.PlaceholderID, req.Reason)
	if bypass == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reason":     bypass.Reason,
		"expire_at":  bypass.ExpireAt.UTC().Format(time.RFC3339),
		"token_type": bypass.TokenType,
	})
}

const secretScanningPushProtectionDocs = "https://docs.github.com/code-security/secret-scanning/working-with-secret-scanning-and-push-protection"

// writeSecretScanningRuleViolation emits the documented repository-rule-violation
// body for a push-protection block, with bypass placeholders under
// metadata.secret_scanning.bypass_placeholders (contents PUT 409, git/blobs 422).
func writeSecretScanningRuleViolation(w http.ResponseWriter, status int, ph *store.SecretScanningPushProtectionPlaceholder) {
	writeJSON(w, status, map[string]interface{}{
		"message":           "Push cannot contain secrets.",
		"documentation_url": secretScanningPushProtectionDocs,
		"status":            strconv.Itoa(status),
		"metadata": map[string]interface{}{
			"secret_scanning": map[string]interface{}{
				"bypass_placeholders": []map[string]interface{}{
					{
						"placeholder_id": ph.ID,
						"token_type":     ph.TokenType,
					},
				},
			},
		},
	})
}

// writeSecretScanningPushProtectionBlocked emits the plain validation-error 422
// GitHub documents for the write routes. The bypass placeholder rides in the
// documented errors[] members rather than on members the schema does not declare.
func writeSecretScanningPushProtectionBlocked(w http.ResponseWriter, ph *store.SecretScanningPushProtectionPlaceholder) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
		"message":           "Push cannot contain secrets.",
		"documentation_url": secretScanningPushProtectionDocs,
		"errors": []map[string]interface{}{
			{
				"resource": "SecretScanningPushProtectionBypass",
				"field":    "placeholder_id",
				"code":     "custom",
				"value":    ph.ID,
				"message":  "Bypass this block with POST /repos/{owner}/{repo}/secret-scanning/push-protection-bypasses.",
			},
			{
				"resource": "SecretScanningPushProtectionBypass",
				"field":    "token_type",
				"code":     "custom",
				"value":    ph.TokenType,
				"message":  "The token type the blocked secret matched.",
			},
		},
	})
}

func secretScanningScanRecordsJSON(records []*store.SecretScanningScanRecord) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(records))
	for _, rec := range records {
		out = append(out, map[string]interface{}{
			"type":         rec.Type,
			"status":       rec.Status,
			"started_at":   rec.StartedAt.UTC().Format(time.RFC3339),
			"completed_at": rec.CompletedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func (s *Server) handleGetSecretScanningScanHistory(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	incremental, patternUpdate, backfill := s.store.SecretScanningScanHistory(repo)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"incremental_scans":              secretScanningScanRecordsJSON(incremental),
		"pattern_update_scans":           secretScanningScanRecordsJSON(patternUpdate),
		"backfill_scans":                 secretScanningScanRecordsJSON(backfill),
		"custom_pattern_backfill_scans":  []map[string]interface{}{},
		"generic_secrets_backfill_scans": []map[string]interface{}{},
	})
}

func (s *Server) lookupSecretScanningAlert(w http.ResponseWriter, r *http.Request, repo *store.Repo) *store.SecretScanningAlert {
	number, err := strconv.Atoi(r.PathValue("alert_number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	a := s.store.GetSecretScanningAlert(repo.FullName, number)
	if a == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return a
}

func secretScanningAlertToJSON(a *store.SecretScanningAlert, baseURL string, repo *store.Repo) map[string]interface{} {
	apiURL := fmt.Sprintf("%s/api/v3/repos/%s/secret-scanning/alerts/%d", baseURL, repo.FullName, a.Number)
	htmlURL := fmt.Sprintf("%s/%s/security/secret-scanning/%d", baseURL, repo.FullName, a.Number)
	locationsURL := fmt.Sprintf("%s/locations", apiURL)

	resolvedAt := interface{}(nil)
	if a.ResolvedAt != nil {
		resolvedAt = a.ResolvedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}

	return map[string]interface{}{
		"number":                   a.Number,
		"created_at":               a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"updated_at":               a.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"url":                      apiURL,
		"html_url":                 htmlURL,
		"locations_url":            locationsURL,
		"state":                    a.State,
		"resolution":               nullOrString(string(a.Resolution)),
		"resolved_at":              resolvedAt,
		"resolved_by":              nil,
		"resolution_comment":       nullOrString(a.ResolutionComment),
		"secret_type":              a.SecretType,
		"secret_type_display_name": a.SecretTypeDisplayName,
		// GitHub returns the detected secret + validity; bleephub persists no real
		// token, so it emits a clearly-synthetic placeholder and unknown validity.
		"secret":                      fmt.Sprintf("EXAMPLE-%s-%d", a.SecretType, a.Number),
		"validity":                    "unknown",
		"push_protection_bypassed":    false,
		"push_protection_bypassed_by": nil,
		"push_protection_bypassed_at": nil,
	}
}

func secretScanningLocationToJSON(loc store.SecretScanningLocation) map[string]interface{} {
	return map[string]interface{}{
		"type": loc.Type,
		"details": map[string]interface{}{
			"path":         loc.Details.Path,
			"start_line":   loc.Details.StartLine,
			"end_line":     loc.Details.EndLine,
			"start_column": loc.Details.StartColumn,
			"end_column":   loc.Details.EndColumn,
			"blob_sha":     loc.Details.BlobSHA,
			"blob_url":     loc.Details.BlobURL,
			"commit_sha":   loc.Details.CommitSHA,
			"commit_url":   loc.Details.CommitURL,
			"html_url":     loc.Details.HTMLURL,
		},
	}
}

func nullOrString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// resolveOrgForSecretScanning resolves {org} for secret-scanning handlers.
func (s *Server) resolveOrgForSecretScanning(w http.ResponseWriter, r *http.Request) (*store.Org, bool) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	return org, true
}
