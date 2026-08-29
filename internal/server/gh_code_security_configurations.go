package bleephub

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Code security configurations: named bundles of security-feature enablement an
// organization defines and attaches to repositories.

func (s *Server) registerGHCodeSecurityConfigurationRoutes() {
	s.route("GET /api/v3/orgs/{org}/code-security/configurations",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleListCodeSecurityConfigurations)))
	s.route("POST /api/v3/orgs/{org}/code-security/configurations",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleCreateCodeSecurityConfiguration)))
	s.route("GET /api/v3/orgs/{org}/code-security/configurations/defaults",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetDefaultCodeSecurityConfigurations)))
	s.route("DELETE /api/v3/orgs/{org}/code-security/configurations/detach",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleDetachCodeSecurityConfiguration)))
	s.route("GET /api/v3/orgs/{org}/code-security/configurations/{configuration_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetCodeSecurityConfiguration)))
	s.route("PATCH /api/v3/orgs/{org}/code-security/configurations/{configuration_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleUpdateCodeSecurityConfiguration)))
	s.route("DELETE /api/v3/orgs/{org}/code-security/configurations/{configuration_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleDeleteCodeSecurityConfiguration)))
	s.route("POST /api/v3/orgs/{org}/code-security/configurations/{configuration_id}/attach",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleAttachCodeSecurityConfiguration)))
	s.route("PUT /api/v3/orgs/{org}/code-security/configurations/{configuration_id}/defaults",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleSetCodeSecurityConfigurationAsDefault)))
	s.route("GET /api/v3/orgs/{org}/code-security/configurations/{configuration_id}/repositories",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleListCodeSecurityConfigurationRepositories)))
	s.route("GET /api/v3/repos/{owner}/{repo}/code-security-configuration", s.handleGetRepoCodeSecurityConfiguration)
}

func (s *Server) handleListCodeSecurityConfigurations(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	configs := s.store.ListCodeSecurityConfigurations(org)
	if r.URL.Query().Get("target_type") == "global" {
		// bleephub defines no global (GitHub-managed) configurations.
		configs = nil
	}
	configs = paginateAndLink(w, r, configs)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(configs))
	for _, c := range configs {
		out = append(out, codeSecurityConfigurationJSON(c, base))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateCodeSecurityConfiguration(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req store.CodeSecurityConfigurationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == nil || *req.Name == "" {
		store.WriteGHValidationError(w, "CodeSecurityConfiguration", "name", "missing_field")
		return
	}
	if req.Description == nil || *req.Description == "" {
		store.WriteGHValidationError(w, "CodeSecurityConfiguration", "description", "missing_field")
		return
	}
	if !req.ValidateEnums(w) {
		return
	}
	if s.store.GetCodeSecurityConfigurationByName(org, *req.Name) != nil {
		store.WriteGHValidationError(w, "CodeSecurityConfiguration", "name", "already_exists")
		return
	}
	c := s.store.CreateCodeSecurityConfiguration(org, &req)
	cscJSON := codeSecurityConfigurationJSON(c, s.baseURL(r))
	writeJSONCreated(w, jsonStringField(cscJSON, "url"), cscJSON)
}

func (s *Server) handleGetDefaultCodeSecurityConfigurations(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	base := s.baseURL(r)
	out := []map[string]interface{}{}
	for _, c := range s.store.ListCodeSecurityConfigurations(org) {
		if c.DefaultForNewRepos == "" || c.DefaultForNewRepos == "none" {
			continue
		}
		out = append(out, map[string]interface{}{
			"default_for_new_repos": c.DefaultForNewRepos,
			"configuration":         codeSecurityConfigurationJSON(c, base),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveCodeSecurityConfiguration loads the {configuration_id} configuration, or writes 404.
func (s *Server) resolveCodeSecurityConfiguration(w http.ResponseWriter, r *http.Request) *store.CodeSecurityConfiguration {
	id, err := strconv.Atoi(r.PathValue("configuration_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	c := s.store.GetCodeSecurityConfiguration(r.PathValue("org"), id)
	if c == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return c
}

func (s *Server) handleGetCodeSecurityConfiguration(w http.ResponseWriter, r *http.Request) {
	c := s.resolveCodeSecurityConfiguration(w, r)
	if c == nil {
		return
	}
	writeJSON(w, http.StatusOK, codeSecurityConfigurationJSON(c, s.baseURL(r)))
}

func (s *Server) handleUpdateCodeSecurityConfiguration(w http.ResponseWriter, r *http.Request) {
	c := s.resolveCodeSecurityConfiguration(w, r)
	if c == nil {
		return
	}
	var req store.CodeSecurityConfigurationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !req.ValidateEnums(w) {
		return
	}
	updated, changed := s.store.UpdateCodeSecurityConfiguration(c.OrgLogin, c.ID, &req)
	if !changed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, codeSecurityConfigurationJSON(updated, s.baseURL(r)))
}

func (s *Server) handleDeleteCodeSecurityConfiguration(w http.ResponseWriter, r *http.Request) {
	c := s.resolveCodeSecurityConfiguration(w, r)
	if c == nil {
		return
	}
	s.store.DeleteCodeSecurityConfiguration(c.OrgLogin, c.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAttachCodeSecurityConfiguration(w http.ResponseWriter, r *http.Request) {
	c := s.resolveCodeSecurityConfiguration(w, r)
	if c == nil {
		return
	}
	var req struct {
		Scope                 *string `json:"scope"`
		SelectedRepositoryIDs []int   `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Scope == nil {
		store.WriteGHValidationError(w, "CodeSecurityConfiguration", "scope", "missing_field")
		return
	}
	switch *req.Scope {
	case "all", "all_without_configurations", "public", "private_or_internal":
		if len(req.SelectedRepositoryIDs) > 0 {
			store.WriteGHValidationError(w, "CodeSecurityConfiguration", "selected_repository_ids", "invalid")
			return
		}
	case "selected":
		if len(req.SelectedRepositoryIDs) == 0 {
			store.WriteGHValidationError(w, "CodeSecurityConfiguration", "selected_repository_ids", "missing_field")
			return
		}
	default:
		store.WriteGHValidationError(w, "CodeSecurityConfiguration", "scope", "invalid")
		return
	}
	if !s.store.AttachCodeSecurityConfiguration(c.OrgLogin, c.ID, *req.Scope, req.SelectedRepositoryIDs) {
		store.WriteGHValidationError(w, "CodeSecurityConfiguration", "selected_repository_ids", "invalid")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{})
}

func (s *Server) handleDetachCodeSecurityConfiguration(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.SelectedRepositoryIDs) == 0 || len(req.SelectedRepositoryIDs) > 250 {
		writeGHError(w, http.StatusBadRequest, "selected_repository_ids must contain between 1 and 250 repository IDs")
		return
	}
	s.store.DetachCodeSecurityConfigurations(org, req.SelectedRepositoryIDs)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetCodeSecurityConfigurationAsDefault(w http.ResponseWriter, r *http.Request) {
	c := s.resolveCodeSecurityConfiguration(w, r)
	if c == nil {
		return
	}
	var req struct {
		DefaultForNewRepos *string `json:"default_for_new_repos"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.DefaultForNewRepos == nil {
		store.WriteGHValidationError(w, "CodeSecurityConfiguration", "default_for_new_repos", "missing_field")
		return
	}
	switch *req.DefaultForNewRepos {
	case "all", "none", "private_and_internal", "public":
	default:
		store.WriteGHValidationError(w, "CodeSecurityConfiguration", "default_for_new_repos", "invalid")
		return
	}
	updated := s.store.SetCodeSecurityConfigurationAsDefault(c.OrgLogin, c.ID, *req.DefaultForNewRepos)
	if !mutated(w, updated) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"default_for_new_repos": updated.DefaultForNewRepos,
		"configuration":         codeSecurityConfigurationJSON(updated, s.baseURL(r)),
	})
}

func (s *Server) handleListCodeSecurityConfigurationRepositories(w http.ResponseWriter, r *http.Request) {
	c := s.resolveCodeSecurityConfiguration(w, r)
	if c == nil {
		return
	}
	statusFilter := r.URL.Query().Get("status")
	// Attachments apply synchronously, so "attached" is the only status a repo can hold.
	if statusFilter != "" && statusFilter != "all" {
		matched := false
		for _, s := range strings.Split(statusFilter, ",") {
			if strings.TrimSpace(s) == "attached" {
				matched = true
				break
			}
		}
		if !matched {
			writeJSON(w, http.StatusOK, []map[string]interface{}{})
			return
		}
	}
	repos := s.store.ListCodeSecurityConfigurationRepos(c.OrgLogin, c.ID)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(repos))
	for _, repo := range repos {
		out = append(out, map[string]interface{}{
			"status":     "attached",
			"repository": simpleRepoJSON(repo, s.store, base),
		})
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleGetRepoCodeSecurityConfiguration(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.store.GetRepoCodeSecurityConfiguration(owner, repo.ID)
	if c == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "attached",
		"configuration": codeSecurityConfigurationJSON(c, s.baseURL(r)),
	})
}

func codeSecurityConfigurationJSON(c *store.CodeSecurityConfiguration, baseURL string) map[string]interface{} {
	out := map[string]interface{}{
		"id":                                        c.ID,
		"name":                                      c.Name,
		"target_type":                               c.TargetType,
		"description":                               c.Description,
		"advanced_security":                         c.AdvancedSecurity,
		"dependency_graph":                          c.DependencyGraph,
		"dependency_graph_autosubmit_action":        c.DependencyGraphAutosubmitAction,
		"dependabot_alerts":                         c.DependabotAlerts,
		"dependabot_security_updates":               c.DependabotSecurityUpdates,
		"dependabot_delegated_alert_dismissal":      c.DependabotDelegatedAlertDismissal,
		"code_scanning_default_setup":               c.CodeScanningDefaultSetup,
		"code_scanning_delegated_alert_dismissal":   c.CodeScanningDelegatedAlertDismissal,
		"secret_scanning":                           c.SecretScanning,
		"secret_scanning_push_protection":           c.SecretScanningPushProtection,
		"secret_scanning_delegated_bypass":          c.SecretScanningDelegatedBypass,
		"secret_scanning_validity_checks":           c.SecretScanningValidityChecks,
		"secret_scanning_non_provider_patterns":     c.SecretScanningNonProviderPatterns,
		"secret_scanning_generic_secrets":           c.SecretScanningGenericSecrets,
		"secret_scanning_delegated_alert_dismissal": c.SecretScanningDelegatedDismissal,
		"private_vulnerability_reporting":           c.PrivateVulnerabilityReporting,
		"enforcement":                               c.Enforcement,
		"url":                                       baseURL + "/api/v3/orgs/" + c.OrgLogin + "/code-security/configurations/" + strconv.Itoa(c.ID),
		"html_url":                                  baseURL + "/organizations/" + c.OrgLogin + "/settings/security_products/configurations/view/" + strconv.Itoa(c.ID),
		"created_at":                                c.CreatedAt.Format(time.RFC3339),
		"updated_at":                                c.UpdatedAt.Format(time.RFC3339),
	}
	if c.SecretScanningExtendedMetadata != "" {
		out["secret_scanning_extended_metadata"] = c.SecretScanningExtendedMetadata
	}
	if c.CodeScanningAllowAdvanced != nil {
		out["code_scanning_options"] = map[string]interface{}{
			"allow_advanced": *c.CodeScanningAllowAdvanced,
		}
	}
	if c.DependencyGraphAutosubmitLabeled != nil {
		out["dependency_graph_autosubmit_action_options"] = map[string]interface{}{
			"labeled_runners": *c.DependencyGraphAutosubmitLabeled,
		}
	}
	if c.CodeScanningRunnerType != nil || c.CodeScanningRunnerLabel != nil {
		opts := map[string]interface{}{}
		if c.CodeScanningRunnerType != nil {
			opts["runner_type"] = *c.CodeScanningRunnerType
		}
		if c.CodeScanningRunnerLabel != nil {
			opts["runner_label"] = *c.CodeScanningRunnerLabel
		}
		out["code_scanning_default_setup_options"] = opts
	}
	return out
}

// store
