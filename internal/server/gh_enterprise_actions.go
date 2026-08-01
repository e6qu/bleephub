package bleephub

import (
	"net/http"
)

func (s *Server) registerGHEnterpriseActionsRoutes() {
	s.route("GET /api/v3/enterprises/{enterprise}/actions/cache/retention-limit", s.requireEnterpriseOwner(s.handleGetEnterpriseActionsCacheRetentionLimit))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/cache/retention-limit", s.requireEnterpriseOwner(s.handleSetEnterpriseActionsCacheRetentionLimit))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/cache/storage-limit", s.requireEnterpriseOwner(s.handleGetEnterpriseActionsCacheStorageLimit))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/cache/storage-limit", s.requireEnterpriseOwner(s.handleSetEnterpriseActionsCacheStorageLimit))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/cache/usage-policy", s.requireEnterpriseOwner(s.handleGetEnterpriseActionsCacheUsagePolicy))
	s.route("PATCH /api/v3/enterprises/{enterprise}/actions/cache/usage-policy", s.requireEnterpriseOwner(s.handleUpdateEnterpriseActionsCacheUsagePolicy))

	s.route("GET /api/v3/enterprises/{enterprise}/actions/oidc/customization/properties/repo", s.requireEnterpriseOwner(s.handleListEnterpriseOIDCCustomProperties))
	s.route("POST /api/v3/enterprises/{enterprise}/actions/oidc/customization/properties/repo", s.requireEnterpriseOwner(s.handleCreateEnterpriseOIDCCustomProperty))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/oidc/customization/properties/repo/{custom_property_name}", s.requireEnterpriseOwner(s.handleDeleteEnterpriseOIDCCustomProperty))

	s.route("GET /api/v3/enterprises/{enterprise}/actions/runners", s.requireEnterpriseOwner(s.handleListRunners))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/runners/downloads", s.requireEnterpriseOwner(s.handleListRunnerApplications))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/runners/{runner_id}", s.requireEnterpriseOwner(s.handleGetRunner))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/runners/{runner_id}", s.requireEnterpriseOwner(s.handleDeleteRunner))
	s.route("POST /api/v3/enterprises/{enterprise}/actions/runners/registration-token", s.requireEnterpriseOwner(s.handleRegistrationToken))
	s.route("POST /api/v3/enterprises/{enterprise}/actions/runners/remove-token", s.requireEnterpriseOwner(s.handleRemoveToken))
	s.route("POST /api/v3/enterprises/{enterprise}/actions/runners/generate-jitconfig", s.requireEnterpriseOwner(s.handleGenerateJITConfig))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/runners/{runner_id}/labels", s.requireEnterpriseOwner(s.handleListRunnerLabels))
	s.route("POST /api/v3/enterprises/{enterprise}/actions/runners/{runner_id}/labels", s.requireEnterpriseOwner(s.handleAddRunnerLabels))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/runners/{runner_id}/labels", s.requireEnterpriseOwner(s.handleSetRunnerLabels))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/runners/{runner_id}/labels", s.requireEnterpriseOwner(s.handleRemoveAllRunnerLabels))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/runners/{runner_id}/labels/{name}", s.requireEnterpriseOwner(s.handleRemoveRunnerLabel))

	s.route("GET /api/v3/enterprises/{enterprise}/actions/runner-groups", s.requireEnterpriseOwner(s.handleListRunnerGroups))
	s.route("POST /api/v3/enterprises/{enterprise}/actions/runner-groups", s.requireEnterpriseOwner(s.handleCreateRunnerGroup))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}", s.requireEnterpriseOwner(s.handleGetRunnerGroup))
	s.route("PATCH /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}", s.requireEnterpriseOwner(s.handleUpdateRunnerGroup))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}", s.requireEnterpriseOwner(s.handleDeleteRunnerGroup))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}/organizations", s.requireEnterpriseOwner(s.handleListGroupOrganizations))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}/organizations", s.requireEnterpriseOwner(s.handleSetGroupOrganizations))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}/organizations/{org_id}", s.requireEnterpriseOwner(s.handleAddGroupOrganization))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}/organizations/{org_id}", s.requireEnterpriseOwner(s.handleRemoveGroupOrganization))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}/runners", s.requireEnterpriseOwner(s.handleListGroupRunners))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}/runners", s.requireEnterpriseOwner(s.handleSetGroupRunners))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}/runners/{runner_id}", s.requireEnterpriseOwner(s.handleAddGroupRunner))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/runner-groups/{runner_group_id}/runners/{runner_id}", s.requireEnterpriseOwner(s.handleRemoveGroupRunner))

	s.route("GET /api/v3/enterprises/{enterprise}/actions/permissions", s.requireEnterpriseOwner(s.handleGetEnterpriseActionsPermissions))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions", s.requireEnterpriseOwner(s.handleSetEnterpriseActionsPermissions))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/permissions/organizations", s.requireEnterpriseOwner(s.handleListEnterpriseActionsOrganizations))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions/organizations", s.requireEnterpriseOwner(s.handleSetEnterpriseActionsOrganizations))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions/organizations/{org_id}", s.requireEnterpriseOwner(s.handleAddEnterpriseActionsOrganization))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/permissions/organizations/{org_id}", s.requireEnterpriseOwner(s.handleRemoveEnterpriseActionsOrganization))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/permissions/selected-actions", s.requireEnterpriseOwner(s.handleGetEnterpriseAllowedActions))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions/selected-actions", s.requireEnterpriseOwner(s.handleSetEnterpriseAllowedActions))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/permissions/workflow", s.requireEnterpriseOwner(s.handleGetEnterpriseWorkflowPermissions))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions/workflow", s.requireEnterpriseOwner(s.handleSetEnterpriseWorkflowPermissions))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/permissions/artifact-and-log-retention", s.requireEnterpriseOwner(s.handleGetEnterpriseArtifactAndLogRetention))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions/artifact-and-log-retention", s.requireEnterpriseOwner(s.handleSetEnterpriseArtifactAndLogRetention))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/permissions/fork-pr-contributor-approval", s.requireEnterpriseOwner(s.handleGetEnterpriseForkPRContributorApproval))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions/fork-pr-contributor-approval", s.requireEnterpriseOwner(s.handleSetEnterpriseForkPRContributorApproval))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/permissions/fork-pr-workflows-private-repos", s.requireEnterpriseOwner(s.handleGetEnterpriseForkPRWorkflowsPrivateRepos))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions/fork-pr-workflows-private-repos", s.requireEnterpriseOwner(s.handleSetEnterpriseForkPRWorkflowsPrivateRepos))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/permissions/self-hosted-runners", s.requireEnterpriseOwner(s.handleGetEnterpriseSelfHostedRunnerPermissions))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/permissions/self-hosted-runners", s.requireEnterpriseOwner(s.handleSetEnterpriseSelfHostedRunnerPermissions))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/cache/usage", s.requireEnterpriseOwner(s.handleEnterpriseCacheUsage))
	s.route("PUT /api/v3/enterprises/{enterprise}/actions/oidc/customization/issuer", s.requireEnterpriseOwner(s.handleSetEnterpriseOIDCIssuer))

	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners", s.requireEnterpriseOwner(s.handleListHostedRunners))
	s.route("POST /api/v3/enterprises/{enterprise}/actions/hosted-runners", s.requireEnterpriseOwner(s.handleCreateHostedRunner))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/images/github-owned", s.requireEnterpriseOwner(s.handleHostedRunnerGitHubOwnedImages))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/images/partner", s.requireEnterpriseOwner(s.handleHostedRunnerPartnerImages))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/images/custom", s.requireEnterpriseOwner(s.handleListHostedRunnerCustomImages))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/images/custom/{image_definition_id}", s.requireEnterpriseOwner(s.handleGetHostedRunnerCustomImage))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/hosted-runners/images/custom/{image_definition_id}", s.requireEnterpriseOwner(s.handleDeleteHostedRunnerCustomImage))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/images/custom/{image_definition_id}/versions", s.requireEnterpriseOwner(s.handleListHostedRunnerCustomImageVersions))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/images/custom/{image_definition_id}/versions/{version}", s.requireEnterpriseOwner(s.handleGetHostedRunnerCustomImageVersion))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/hosted-runners/images/custom/{image_definition_id}/versions/{version}", s.requireEnterpriseOwner(s.handleDeleteHostedRunnerCustomImageVersion))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/limits", s.requireEnterpriseOwner(s.handleHostedRunnerLimits))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/machine-sizes", s.requireEnterpriseOwner(s.handleHostedRunnerMachineSizes))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/platforms", s.requireEnterpriseOwner(s.handleHostedRunnerPlatforms))
	s.route("GET /api/v3/enterprises/{enterprise}/actions/hosted-runners/{hosted_runner_id}", s.requireEnterpriseOwner(s.handleGetHostedRunner))
	s.route("PATCH /api/v3/enterprises/{enterprise}/actions/hosted-runners/{hosted_runner_id}", s.requireEnterpriseOwner(s.handleUpdateHostedRunner))
	s.route("DELETE /api/v3/enterprises/{enterprise}/actions/hosted-runners/{hosted_runner_id}", s.requireEnterpriseOwner(s.handleDeleteHostedRunner))
}

// --- GitHub Actions cache limits ---

func (s *Server) handleGetEnterpriseActionsCacheRetentionLimit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	s.store.mu.RLock()
	days := s.store.EnterpriseSettings.ActionsCacheRetentionDays
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"max_cache_retention_days": days,
	})
}

func (s *Server) handleSetEnterpriseActionsCacheRetentionLimit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	var req struct {
		MaxCacheRetentionDays *int `json:"max_cache_retention_days"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.MaxCacheRetentionDays == nil || *req.MaxCacheRetentionDays <= 0 {
		writeGHError(w, http.StatusBadRequest, "Invalid request. max_cache_retention_days must be a positive integer.")
		return
	}
	s.store.SetEnterpriseActionsCacheRetentionDays(*req.MaxCacheRetentionDays)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEnterpriseActionsCacheStorageLimit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	s.store.mu.RLock()
	gb := s.store.EnterpriseSettings.ActionsCacheSizeGB
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"max_cache_size_gb": gb,
	})
}

func (s *Server) handleSetEnterpriseActionsCacheStorageLimit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	var req struct {
		MaxCacheSizeGB *int `json:"max_cache_size_gb"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.MaxCacheSizeGB == nil || *req.MaxCacheSizeGB <= 0 {
		writeGHError(w, http.StatusBadRequest, "Invalid request. max_cache_size_gb must be a positive integer.")
		return
	}
	s.store.SetEnterpriseActionsCacheSizeGB(*req.MaxCacheSizeGB)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEnterpriseActionsCacheUsagePolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	s.store.mu.RLock()
	defaultGB := s.store.EnterpriseSettings.ActionsDefaultCacheSizeGB
	maxGB := s.store.EnterpriseSettings.ActionsCacheSizeGB
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]int{
		"repo_cache_size_limit_in_gb":     defaultGB,
		"max_repo_cache_size_limit_in_gb": maxGB,
	})
}

func (s *Server) handleUpdateEnterpriseActionsCacheUsagePolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	var req struct {
		RepoCacheSizeLimitGB    *int `json:"repo_cache_size_limit_in_gb"`
		MaxRepoCacheSizeLimitGB *int `json:"max_repo_cache_size_limit_in_gb"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.mu.RLock()
	defaultGB := s.store.EnterpriseSettings.ActionsDefaultCacheSizeGB
	maxGB := s.store.EnterpriseSettings.ActionsCacheSizeGB
	s.store.mu.RUnlock()
	if req.RepoCacheSizeLimitGB != nil {
		defaultGB = *req.RepoCacheSizeLimitGB
	}
	if req.MaxRepoCacheSizeLimitGB != nil {
		maxGB = *req.MaxRepoCacheSizeLimitGB
	}
	if defaultGB <= 0 || maxGB <= 0 || defaultGB > maxGB {
		writeGHError(w, http.StatusBadRequest, "Invalid cache usage policy.")
		return
	}
	s.store.SetEnterpriseActionsCacheUsagePolicy(defaultGB, maxGB)
	w.WriteHeader(http.StatusNoContent)
}

// --- GitHub Actions OIDC custom property inclusions ---

func enterpriseOIDCCustomPropertyJSON(name string) map[string]interface{} {
	return map[string]interface{}{
		"custom_property_name": name,
		"inclusion_source":     "enterprise",
	}
}

func (s *Server) handleListEnterpriseOIDCCustomProperties(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	s.store.mu.RLock()
	names := append([]string(nil), s.store.EnterpriseSettings.OIDCCustomProperties...)
	s.store.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		out = append(out, enterpriseOIDCCustomPropertyJSON(name))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateEnterpriseOIDCCustomProperty(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	var req struct {
		CustomPropertyName string `json:"custom_property_name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.CustomPropertyName == "" {
		writeGHValidationError(w, "OIDCCustomPropertyInclusion", "custom_property_name", "missing_field")
		return
	}
	if !s.store.AddEnterpriseOIDCCustomProperty(req.CustomPropertyName) {
		writeGHValidationError(w, "OIDCCustomPropertyInclusion", "custom_property_name", "already_exists")
		return
	}
	writeJSON(w, http.StatusCreated, enterpriseOIDCCustomPropertyJSON(req.CustomPropertyName))
}

func (s *Server) handleDeleteEnterpriseOIDCCustomProperty(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.enterpriseFromRequest(w, r); !ok {
		return
	}
	name := r.PathValue("custom_property_name")
	if !s.store.RemoveEnterpriseOIDCCustomProperty(name) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
