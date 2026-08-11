package bleephub

import (
	"net/http"
	"sort"
	"strconv"
)

// Deployment branch/tag policies and custom deployment protection rules on
// repository environments.
// Endpoints:
//
//	GET    /repos/{o}/{r}/environments/{env}/deployment-branch-policies
//	POST   /repos/{o}/{r}/environments/{env}/deployment-branch-policies
//	GET    /repos/{o}/{r}/environments/{env}/deployment-branch-policies/{branch_policy_id}
//	PUT    /repos/{o}/{r}/environments/{env}/deployment-branch-policies/{branch_policy_id}
//	DELETE /repos/{o}/{r}/environments/{env}/deployment-branch-policies/{branch_policy_id}
//	GET    /repos/{o}/{r}/environments/{env}/deployment_protection_rules
//	POST   /repos/{o}/{r}/environments/{env}/deployment_protection_rules
//	GET    /repos/{o}/{r}/environments/{env}/deployment_protection_rules/apps
//	GET    /repos/{o}/{r}/environments/{env}/deployment_protection_rules/{protection_rule_id}
//	DELETE /repos/{o}/{r}/environments/{env}/deployment_protection_rules/{protection_rule_id}

const (
	BranchPolicyBranch DeploymentBranchPolicyType = "branch"
	BranchPolicyTag    DeploymentBranchPolicyType = "tag"
)

func (s *Server) registerGHEnvironmentPolicyRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment-branch-policies", s.handleListDeploymentBranchPolicies)
	s.route("POST /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment-branch-policies",
		s.requirePerm(scopeAdministration, permWrite, s.handleCreateDeploymentBranchPolicy))
	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment-branch-policies/{branch_policy_id}", s.handleGetDeploymentBranchPolicy)
	s.route("PUT /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment-branch-policies/{branch_policy_id}",
		s.requirePerm(scopeAdministration, permWrite, s.handleUpdateDeploymentBranchPolicy))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment-branch-policies/{branch_policy_id}",
		s.requirePerm(scopeAdministration, permWrite, s.handleDeleteDeploymentBranchPolicy))

	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment_protection_rules", s.handleListEnvProtectionRules)
	s.route("POST /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment_protection_rules",
		s.requirePerm(scopeAdministration, permWrite, s.handleCreateEnvProtectionRule))
	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment_protection_rules/apps", s.handleListEnvProtectionRuleApps)
	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment_protection_rules/{protection_rule_id}", s.handleGetEnvProtectionRule)
	s.route("DELETE /api/v3/repos/{owner}/{repo}/environments/{env_name}/deployment_protection_rules/{protection_rule_id}",
		s.requirePerm(scopeAdministration, permWrite, s.handleDeleteEnvProtectionRule))
}

// environmentFromPath resolves {owner}/{repo}/environments/{env_name},
// writing a 404 and returning nils when either does not exist.
func (s *Server) environmentFromPath(w http.ResponseWriter, r *http.Request) (*Repo, *Environment) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	env := s.store.Deployments.GetEnvironment(repo.ID, r.PathValue("env_name"))
	if env == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	return repo, env
}

// --- Store: deployment branch policies ---

// --- Store: custom deployment protection rules ---

// --- Handlers: deployment branch policies ---

func branchPolicyToJSON(p *DeploymentBranchPolicyRule) map[string]interface{} {
	return map[string]interface{}{
		"id":      p.ID,
		"node_id": p.NodeID,
		"name":    p.Name,
		"type":    p.Type,
	}
}

func (s *Server) handleListDeploymentBranchPolicies(w http.ResponseWriter, r *http.Request) {
	_, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	policies := s.store.ListEnvBranchPolicies(env.ID)
	page := paginateAndLink(w, r, policies)
	out := make([]map[string]interface{}, 0, len(page))
	for _, p := range page {
		out = append(out, branchPolicyToJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":     len(policies),
		"branch_policies": out,
	})
}

func (s *Server) handleCreateDeploymentBranchPolicy(w http.ResponseWriter, r *http.Request) {
	repo, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	// Branch policies only apply to environments configured with custom
	// branch policies; real GitHub hides the collection otherwise.
	if env.DeploymentBranchPolicy == nil || !env.DeploymentBranchPolicy.CustomBranchPolicies {
		writeGHError(w, http.StatusNotFound, "Deployment branch policies can only be created for environments with custom_branch_policies enabled.")
		return
	}
	var req struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeGHValidationError(w, "DeploymentBranchPolicy", "name", "missing_field")
		return
	}
	policyType := coalesceStr(req.Type, "branch")
	if policyType != "branch" && policyType != "tag" {
		writeGHValidationError(w, "DeploymentBranchPolicy", "type", "invalid")
		return
	}
	created, existing := s.store.CreateEnvBranchPolicy(env.ID, req.Name, policyType)
	if existing != nil {
		w.Header().Set("Location", s.baseURL(r)+"/api/v3/repos/"+repo.FullName+"/environments/"+env.Name+"/deployment-branch-policies/"+strconv.Itoa(existing.ID))
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, branchPolicyToJSON(created))
}

func (s *Server) handleGetDeploymentBranchPolicy(w http.ResponseWriter, r *http.Request) {
	_, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("branch_policy_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	p := s.store.GetEnvBranchPolicy(env.ID, id)
	if p == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, branchPolicyToJSON(p))
}

func (s *Server) handleUpdateDeploymentBranchPolicy(w http.ResponseWriter, r *http.Request) {
	_, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("branch_policy_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeGHValidationError(w, "DeploymentBranchPolicy", "name", "missing_field")
		return
	}
	p := s.store.UpdateEnvBranchPolicy(env.ID, id, req.Name)
	if p == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, branchPolicyToJSON(p))
}

func (s *Server) handleDeleteDeploymentBranchPolicy(w http.ResponseWriter, r *http.Request) {
	_, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("branch_policy_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeleteEnvBranchPolicy(env.ID, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Handlers: custom deployment protection rules ---

func (s *Server) envProtectionRuleJSON(rule *EnvCustomProtectionRule, baseURL string) map[string]interface{} {
	s.store.Mu.RLock()
	app := s.store.Apps[rule.AppID]
	s.store.Mu.RUnlock()
	var appJSON map[string]interface{}
	if app != nil {
		appJSON = customDeploymentRuleAppJSON(app, baseURL)
	}
	return map[string]interface{}{
		"id":      rule.ID,
		"node_id": rule.NodeID,
		"enabled": rule.Enabled,
		"app":     appJSON,
	}
}

func customDeploymentRuleAppJSON(app *App, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"id":              app.ID,
		"slug":            app.Slug,
		"integration_url": baseURL + "/api/v3/apps/" + app.Slug,
		"node_id":         app.NodeID,
	}
}

func (s *Server) handleListEnvProtectionRules(w http.ResponseWriter, r *http.Request) {
	_, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	rules := s.store.ListEnvProtectionRules(env.ID)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(rules))
	for _, rule := range rules {
		out = append(out, s.envProtectionRuleJSON(rule, base))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":                        len(rules),
		"custom_deployment_protection_rules": out,
	})
}

func (s *Server) handleCreateEnvProtectionRule(w http.ResponseWriter, r *http.Request) {
	_, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	var req struct {
		IntegrationID *int `json:"integration_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.IntegrationID == nil {
		writeGHValidationError(w, "DeploymentProtectionRule", "integration_id", "missing_field")
		return
	}
	s.store.Mu.RLock()
	app := s.store.Apps[*req.IntegrationID]
	s.store.Mu.RUnlock()
	if app == nil {
		writeGHValidationError(w, "DeploymentProtectionRule", "integration_id", "invalid")
		return
	}
	rule := s.store.CreateEnvProtectionRule(env.ID, app.ID)
	if rule == nil {
		writeGHValidationError(w, "DeploymentProtectionRule", "integration_id", "already_exists")
		return
	}
	writeJSON(w, http.StatusCreated, s.envProtectionRuleJSON(rule, s.baseURL(r)))
}

// handleListEnvProtectionRuleApps lists the GitHub App integrations that are
// available to provide custom protection rules for the environment: the apps
// with an installation on the repository's owner account.
func (s *Server) handleListEnvProtectionRuleApps(w http.ResponseWriter, r *http.Request) {
	repo, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	ownerLogin, _, _ := splitRepoFullName(repo.FullName)
	s.store.Mu.RLock()
	appIDs := map[int]bool{}
	for _, inst := range s.store.Installations {
		if inst.TargetLogin == ownerLogin {
			appIDs[inst.AppID] = true
		}
	}
	apps := make([]*App, 0, len(appIDs))
	for id := range appIDs {
		if app := s.store.Apps[id]; app != nil {
			apps = append(apps, app)
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })

	base := s.baseURL(r)
	page := paginateAndLink(w, r, apps)
	out := make([]map[string]interface{}, 0, len(page))
	for _, app := range page {
		out = append(out, customDeploymentRuleAppJSON(app, base))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(apps),
		"available_custom_deployment_protection_rule_integrations": out,
	})
}

func (s *Server) handleGetEnvProtectionRule(w http.ResponseWriter, r *http.Request) {
	_, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("protection_rule_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rule := s.store.GetEnvProtectionRule(env.ID, id)
	if rule == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.envProtectionRuleJSON(rule, s.baseURL(r)))
}

func (s *Server) handleDeleteEnvProtectionRule(w http.ResponseWriter, r *http.Request) {
	_, env := s.environmentFromPath(w, r)
	if env == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("protection_rule_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeleteEnvProtectionRule(env.ID, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
