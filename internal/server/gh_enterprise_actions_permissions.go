package bleephub

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strconv"
)

const enterpriseArtifactAndLogRetentionMaxDays = 365

func (s *Server) handleGetEnterpriseActionsPermissions(w http.ResponseWriter, r *http.Request) {
	s.store.Mu.RLock()
	settings := s.store.EnterpriseSettings
	enabledOrganizations := settings.ActionsEnabledOrganizations
	allowedActions := settings.ActionsAllowedActions
	shaPinningRequired := settings.ActionsSHAPinningRequired
	s.store.Mu.RUnlock()

	base := s.baseURL(r) + "/api/v3/enterprises/" + r.PathValue("enterprise") + "/actions/permissions"
	out := map[string]interface{}{
		"enabled_organizations": enabledOrganizations,
		"allowed_actions":       allowedActions,
		"sha_pinning_required":  shaPinningRequired,
	}
	if enabledOrganizations == "selected" {
		out["selected_organizations_url"] = base + "/organizations"
	}
	if allowedActions == "selected" {
		out["selected_actions_url"] = base + "/selected-actions"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSetEnterpriseActionsPermissions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnabledOrganizations string `json:"enabled_organizations"`
		AllowedActions       string `json:"allowed_actions"`
		SHAPinningRequired   *bool  `json:"sha_pinning_required"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !slices.Contains([]string{"all", "none", "selected"}, req.EnabledOrganizations) {
		writeGHValidationError(w, "ActionsPermissions", "enabled_organizations", "invalid")
		return
	}
	if req.AllowedActions != "" &&
		!slices.Contains([]string{"all", "local_only", "selected"}, req.AllowedActions) {
		writeGHValidationError(w, "ActionsPermissions", "allowed_actions", "invalid")
		return
	}

	s.store.Mu.Lock()
	settings := s.store.EnterpriseSettings
	settings.ActionsEnabledOrganizations = req.EnabledOrganizations
	if req.AllowedActions != "" {
		settings.ActionsAllowedActions = req.AllowedActions
	}
	if req.SHAPinningRequired != nil {
		settings.ActionsSHAPinningRequired = *req.SHAPinningRequired
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListEnterpriseActionsOrganizations(w http.ResponseWriter, r *http.Request) {
	s.store.Mu.RLock()
	ids := append([]int(nil), s.store.EnterpriseSettings.ActionsSelectedOrganizationIDs...)
	orgs := make([]*Org, 0, len(ids))
	for _, id := range ids {
		if org := s.store.Orgs[id]; org != nil {
			orgs = append(orgs, org)
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(orgs, func(i, j int) bool { return orgs[i].ID < orgs[j].ID })

	page := paginateAndLink(w, r, orgs)
	out := make([]map[string]interface{}, 0, len(page))
	for _, org := range page {
		out = append(out, orgSimpleJSON(org, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":   len(orgs),
		"organizations": out,
	})
}

func (s *Server) handleSetEnterpriseActionsOrganizations(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SelectedOrganizationIDs *[]int `json:"selected_organization_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SelectedOrganizationIDs == nil {
		writeGHValidationError(w, "ActionsPermissions", "selected_organization_ids", "missing_field")
		return
	}
	s.store.Mu.Lock()
	for _, id := range *req.SelectedOrganizationIDs {
		if s.store.Orgs[id] == nil {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	s.store.EnterpriseSettings.ActionsSelectedOrganizationIDs =
		append([]int(nil), (*req.SelectedOrganizationIDs)...)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) enterpriseActionsOrganizationID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("org_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return 0, false
	}
	s.store.Mu.RLock()
	exists := s.store.Orgs[id] != nil
	s.store.Mu.RUnlock()
	if !exists {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return 0, false
	}
	return id, true
}

func (s *Server) handleAddEnterpriseActionsOrganization(w http.ResponseWriter, r *http.Request) {
	id, ok := s.enterpriseActionsOrganizationID(w, r)
	if !ok {
		return
	}
	s.store.Mu.Lock()
	settings := s.store.EnterpriseSettings
	if !slices.Contains(settings.ActionsSelectedOrganizationIDs, id) {
		settings.ActionsSelectedOrganizationIDs = append(settings.ActionsSelectedOrganizationIDs, id)
		s.store.PersistEnterpriseSettings()
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveEnterpriseActionsOrganization(w http.ResponseWriter, r *http.Request) {
	id, ok := s.enterpriseActionsOrganizationID(w, r)
	if !ok {
		return
	}
	s.store.Mu.Lock()
	settings := s.store.EnterpriseSettings
	settings.ActionsSelectedOrganizationIDs = slices.DeleteFunc(
		settings.ActionsSelectedOrganizationIDs,
		func(candidate int) bool { return candidate == id },
	)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEnterpriseAllowedActions(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	var allowed *ActionsAllowed
	if current := s.store.EnterpriseSettings.ActionsAllowed; current != nil {
		copy := *current
		copy.PatternsAllowed = append([]string(nil), current.PatternsAllowed...)
		allowed = &copy
	}
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, allowedActionsJSON(allowed))
}

func (s *Server) handleSetEnterpriseAllowedActions(w http.ResponseWriter, r *http.Request) {
	var req ActionsAllowed
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.PatternsAllowed = append([]string(nil), req.PatternsAllowed...)
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsAllowed = &req
	s.store.EnterpriseSettings.ActionsAllowedActions = "selected"
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEnterpriseWorkflowPermissions(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	var permissions *WorkflowPermissions
	if current := s.store.EnterpriseSettings.ActionsWorkflowPermissions; current != nil {
		copy := *current
		permissions = &copy
	}
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, workflowPermissionsJSON(permissions))
}

func (s *Server) handleSetEnterpriseWorkflowPermissions(w http.ResponseWriter, r *http.Request) {
	var req WorkflowPermissions
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !slices.Contains([]string{"read", "write"}, req.DefaultWorkflowPermissions) {
		writeGHValidationError(w, "WorkflowPermissions", "default_workflow_permissions", "invalid")
		return
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsWorkflowPermissions = &req
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEnterpriseArtifactAndLogRetention(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	days := s.store.EnterpriseSettings.ActionsArtifactRetentionDays
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"days":                 days,
		"maximum_allowed_days": enterpriseArtifactAndLogRetentionMaxDays,
	})
}

func (s *Server) handleSetEnterpriseArtifactAndLogRetention(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days *int `json:"days"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Days == nil || *req.Days < 1 || *req.Days > enterpriseArtifactAndLogRetentionMaxDays {
		writeGHValidationError(w, "ActionsArtifactAndLogRetention", "days", "invalid")
		return
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsArtifactRetentionDays = *req.Days
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEnterpriseForkPRContributorApproval(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	policy := s.store.EnterpriseSettings.ActionsForkPRApprovalPolicy
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"approval_policy": policy})
}

func (s *Server) handleSetEnterpriseForkPRContributorApproval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApprovalPolicy string `json:"approval_policy"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !forkPRApprovalPolicies[req.ApprovalPolicy] {
		writeGHValidationError(w, "ActionsForkPRContributorApproval", "approval_policy", "invalid")
		return
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsForkPRApprovalPolicy = req.ApprovalPolicy
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEnterpriseForkPRWorkflowsPrivateRepos(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	settings := *s.store.EnterpriseSettings.ActionsForkPRWorkflowsPrivate
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleSetEnterpriseForkPRWorkflowsPrivateRepos(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunWorkflowsFromForkPullRequests  *bool `json:"run_workflows_from_fork_pull_requests"`
		SendWriteTokensToWorkflows        *bool `json:"send_write_tokens_to_workflows"`
		SendSecretsAndVariables           *bool `json:"send_secrets_and_variables"`
		RequireApprovalForForkPRWorkflows *bool `json:"require_approval_for_fork_pr_workflows"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.RunWorkflowsFromForkPullRequests == nil {
		writeGHValidationError(w, "ActionsForkPRWorkflowsPrivateRepos", "run_workflows_from_fork_pull_requests", "missing_field")
		return
	}
	s.store.Mu.Lock()
	settings := s.store.EnterpriseSettings.ActionsForkPRWorkflowsPrivate
	settings.RunWorkflowsFromForkPullRequests = *req.RunWorkflowsFromForkPullRequests
	if req.SendWriteTokensToWorkflows != nil {
		settings.SendWriteTokensToWorkflows = *req.SendWriteTokensToWorkflows
	}
	if req.SendSecretsAndVariables != nil {
		settings.SendSecretsAndVariables = *req.SendSecretsAndVariables
	}
	if req.RequireApprovalForForkPRWorkflows != nil {
		settings.RequireApprovalForForkPRWorkflows = *req.RequireApprovalForForkPRWorkflows
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEnterpriseSelfHostedRunnerPermissions(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	disabled := s.store.EnterpriseSettings.ActionsDisableSelfHostedRunners
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"disable_self_hosted_runners_for_all_orgs": disabled,
	})
}

func (s *Server) handleSetEnterpriseSelfHostedRunnerPermissions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disabled *bool `json:"disable_self_hosted_runners_for_all_orgs"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Disabled == nil {
		writeGHValidationError(w, "SelfHostedRunnersPermissions", "disable_self_hosted_runners_for_all_orgs", "missing_field")
		return
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsDisableSelfHostedRunners = *req.Disabled
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnterpriseCacheUsage(w http.ResponseWriter, _ *http.Request) {
	count := 0
	var bytes int64
	s.artifactStore.Mu.RLock()
	for _, entry := range s.artifactStore.Caches {
		if entry.Finalized {
			count++
			bytes += entry.Size
		}
	}
	s.artifactStore.Mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_active_caches_count":         count,
		"total_active_caches_size_in_bytes": bytes,
	})
}

func (s *Server) handleSetEnterpriseOIDCIssuer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IncludeEnterpriseSlug bool `json:"include_enterprise_slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.OIDCIncludeEnterpriseSlug = req.IncludeEnterpriseSlug
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
