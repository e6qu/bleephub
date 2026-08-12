package bleephub

// GitHub Actions permissions + runner-label REST surface.
//
// Org-scoped endpoints mirror the GHES /orgs/{org}/actions/permissions paths.
// Repo-scoped endpoints mirror /repos/{owner}/{repo}/actions/permissions paths.
// Runner labels are exposed at both repo and org scope.
//
// Store types and helpers live alongside the handlers so the surface is
// self-contained; persistence is wired through store.go.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// artifactRetentionMaximumDays is the ceiling GitHub reports beside the
// configured value; the description declares both as required.
const artifactRetentionMaximumDays = 90

// githubTokenDefaultScopes is the standard set of GITHUB_TOKEN API permission
// scopes a workflow receives when it declares no `permissions:` block (or uses
// read-all / write-all): the repo's default workflow-permission level applies
// across all of them. These are the permScopes the REST gate can check for a
// repository-scoped token.
var githubTokenDefaultScopes = []permScope{
	scopeActions, scopeChecks, scopeContents, scopeDeployments, scopeDiscussions,
	scopeIssues, scopePages, scopePullRequests, scopeSecurityEvents, scopeMetadata,
}

// resolveJobTokenPermissions computes the least-privilege GITHUB_TOKEN API
// permission map for one job (ACT-014). GitHub semantics: a job-level
// `permissions:` block fully replaces a workflow-level one, which fully replaces
// the repo default; there is no merge. An undeclared block grants the repo's
// default level (read unless the repo/org setting is write) across the standard
// scope set; `read-all`/`write-all` grant that level across the set; an explicit
// block grants exactly the listed scopes (a `none` value drops the scope). A
// declared-but-empty block (`permissions: {}`) yields metadata:read only, which
// is always granted regardless.
func (s *Server) resolveJobTokenPermissions(wf *Workflow, jd *JobDef) map[string]string {
	var declared PermissionDef
	switch {
	case jd != nil && jd.Permissions != nil:
		declared = jd.Permissions
	case wf != nil:
		declared = wf.Permissions // nil when the workflow declared none
	}

	perms := map[string]string{}
	switch {
	case declared == nil:
		level := "read"
		if wf != nil {
			if wp := s.store.GetRepoActionsPermissions(wf.RepoFullName).WorkflowPermissions; wp != nil && wp.DefaultWorkflowPermissions == "write" {
				level = "write"
			}
		}
		for _, sc := range githubTokenDefaultScopes {
			perms[string(sc)] = level
		}
	default:
		if lvl, ok := declared["*"]; ok { // read-all / write-all
			for _, sc := range githubTokenDefaultScopes {
				perms[string(sc)] = lvl
			}
		} else {
			for k, v := range declared {
				if v == "none" || v == "" {
					continue
				}
				// Permission keys are hyphenated in YAML (pull-requests,
				// security-events); permScope values are underscored.
				perms[strings.ReplaceAll(k, "-", "_")] = v
			}
		}
	}
	// metadata:read is always available to a workflow token.
	if _, ok := perms[string(scopeMetadata)]; !ok {
		perms[string(scopeMetadata)] = "read"
	}
	return perms
}

// orgArtifactAndLogRetentionMaxDays is the maximum artifact/log
// retention GitHub allows an organization to configure.
const orgArtifactAndLogRetentionMaxDays = 400

func (s *Server) registerGHActionsPermissionsRoutes() {
	// Org permissions.
	s.route("GET /api/v3/orgs/{org}/actions/permissions",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleGetOrgActionsPermissions)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleSetOrgActionsPermissions)))
	s.route("GET /api/v3/orgs/{org}/actions/permissions/repositories",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleListOrgSelectedRepos)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/repositories",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleSetOrgSelectedRepos)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/repositories/{repository_id}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleAddOrgSelectedRepo)))
	s.route("DELETE /api/v3/orgs/{org}/actions/permissions/repositories/{repository_id}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleRemoveOrgSelectedRepo)))
	s.route("GET /api/v3/orgs/{org}/actions/permissions/selected-actions",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleGetOrgAllowedActions)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/selected-actions",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleSetOrgAllowedActions)))
	s.route("GET /api/v3/orgs/{org}/actions/permissions/workflow",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleGetOrgWorkflowPermissions)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/workflow",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleSetOrgWorkflowPermissions)))
	s.route("GET /api/v3/orgs/{org}/actions/permissions/artifact-and-log-retention",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleGetOrgArtifactAndLogRetention)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/artifact-and-log-retention",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleSetOrgArtifactAndLogRetention)))
	s.route("GET /api/v3/orgs/{org}/actions/permissions/fork-pr-contributor-approval",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleGetOrgForkPRContributorApproval)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/fork-pr-contributor-approval",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleSetOrgForkPRContributorApproval)))
	s.route("GET /api/v3/orgs/{org}/actions/permissions/fork-pr-workflows-private-repos",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleGetOrgForkPRWorkflowsPrivateRepos)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/fork-pr-workflows-private-repos",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleSetOrgForkPRWorkflowsPrivateRepos)))
	s.route("GET /api/v3/orgs/{org}/actions/permissions/self-hosted-runners",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleGetOrgSelfHostedRunnersSettings)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/self-hosted-runners",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleSetOrgSelfHostedRunnersSettings)))
	s.route("GET /api/v3/orgs/{org}/actions/permissions/self-hosted-runners/repositories",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleListOrgSelfHostedRunnerRepos)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/self-hosted-runners/repositories",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleSetOrgSelfHostedRunnerRepos)))
	s.route("PUT /api/v3/orgs/{org}/actions/permissions/self-hosted-runners/repositories/{repository_id}",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleAddOrgSelfHostedRunnerRepo)))
	s.route("DELETE /api/v3/orgs/{org}/actions/permissions/self-hosted-runners/repositories/{repository_id}",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleRemoveOrgSelfHostedRunnerRepo)))
	s.route("GET /api/v3/orgs/{org}/actions/cache/usage",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleOrgCacheUsage)))
	s.route("GET /api/v3/orgs/{org}/actions/cache/usage-by-repository",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleOrgCacheUsageByRepository)))

	// Org cache policy limits at the /organizations/{org_id} path (the
	// dotcom REST description's path for these settings).
	s.route("GET /api/v3/organizations/{org_id}/actions/cache/retention-limit",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgIDGated(s.handleGetOrgMaxCacheRetention)))
	s.route("PUT /api/v3/organizations/{org_id}/actions/cache/retention-limit",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgIDGated(s.handleSetOrgMaxCacheRetention)))
	s.route("GET /api/v3/organizations/{org_id}/actions/cache/storage-limit",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgIDGated(s.handleGetOrgMaxCacheSize)))
	s.route("PUT /api/v3/organizations/{org_id}/actions/cache/storage-limit",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgIDGated(s.handleSetOrgMaxCacheSize)))

	// Repo permissions.
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/permissions",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoActionsPermissions))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/permissions",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoActionsPermissions))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/permissions/access",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoActionsAccessLevel))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/permissions/access",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoActionsAccessLevel))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/permissions/selected-actions",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoAllowedActions))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/permissions/selected-actions",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoAllowedActions))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/permissions/workflow",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoWorkflowPermissions))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/permissions/workflow",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoWorkflowPermissions))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/permissions/fork-pr-contributor-approval",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoForkPRContributorApproval))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/permissions/fork-pr-contributor-approval",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoForkPRContributorApproval))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/permissions/fork-pr-workflows-private-repos",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoForkPRWorkflowsPrivateRepos))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/permissions/fork-pr-workflows-private-repos",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoForkPRWorkflowsPrivateRepos))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/permissions/artifact-and-log-retention",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoArtifactAndLogRetention))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/permissions/artifact-and-log-retention",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoArtifactAndLogRetention))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/cache/retention-limit",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoCacheRetentionLimit))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/cache/retention-limit",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoCacheRetentionLimit))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/cache/storage-limit",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoCacheStorageLimit))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/cache/storage-limit",
		s.requirePerm(scopeActions, permWrite, s.handleSetRepoCacheStorageLimit))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/cache/usage-policy",
		s.requirePerm(scopeActions, permRead, s.handleGetRepoCacheUsagePolicy))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/actions/cache/usage-policy",
		s.requirePerm(scopeActions, permWrite, s.handleUpdateRepoCacheUsagePolicy))

	// Run logs delete.
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/runs/{run_id}/logs",
		s.requirePerm(scopeActions, permWrite, s.handleDeleteRunLogs))

	// Runner labels.
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/runners/{runner_id}/labels",
		s.requirePerm(scopeAdministration, permRead, s.handleListRunnerLabels))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/runners/{runner_id}/labels",
		s.requirePerm(scopeAdministration, permWrite, s.handleSetRunnerLabels))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/runners/{runner_id}/labels",
		s.requirePerm(scopeAdministration, permWrite, s.handleRemoveAllRunnerLabels))
	s.route("GET /api/v3/orgs/{org}/actions/runners/{runner_id}/labels",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleListRunnerLabels)))
	s.route("PUT /api/v3/orgs/{org}/actions/runners/{runner_id}/labels",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleSetRunnerLabels)))
	s.route("DELETE /api/v3/orgs/{org}/actions/runners/{runner_id}/labels",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleRemoveAllRunnerLabels)))
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/runners/{runner_id}/labels",
		s.requirePerm(scopeAdministration, permWrite, s.handleAddRunnerLabels))
	s.route("POST /api/v3/orgs/{org}/actions/runners/{runner_id}/labels",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleAddRunnerLabels)))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/runners/{runner_id}/labels/{name}",
		s.requirePerm(scopeAdministration, permWrite, s.handleRemoveRunnerLabel))
	s.route("DELETE /api/v3/orgs/{org}/actions/runners/{runner_id}/labels/{name}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleRemoveRunnerLabel)))
}

// --- Org permissions handlers ---

func (s *Server) handleGetOrgActionsPermissions(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	writeJSON(w, http.StatusOK, orgActionsPermissionsJSON(p, s.baseURL(r), org))
}

func (s *Server) handleSetOrgActionsPermissions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnabledRepositories string `json:"enabled_repositories"`
		AllowedActions      string `json:"allowed_actions"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	if req.EnabledRepositories != "" {
		p.EnabledRepositories = req.EnabledRepositories
	}
	if req.AllowedActions != "" {
		p.AllowedActions = req.AllowedActions
	}
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgSelectedRepos(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	ids := s.store.ListOrgSelectedRepos(org)
	base := s.baseURL(r)
	repos := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		s.store.Mu.RLock()
		repo := s.store.Repos[id]
		s.store.Mu.RUnlock()
		if repo != nil {
			repos = append(repos, repoToJSON(repo, s.store, base))
		}
	}
	paged := paginateAndLink(w, r, repos)
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":  len(repos),
		"repositories": paged,
	})
}

func (s *Server) handleSetOrgSelectedRepos(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	org := r.PathValue("org")
	s.store.SetOrgSelectedRepos(org, req.SelectedRepositoryIDs)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddOrgSelectedRepo(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	repoID, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	exists := s.store.Repos[repoID] != nil
	s.store.Mu.RUnlock()
	if !exists {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.AddOrgSelectedRepo(org, repoID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveOrgSelectedRepo(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	repoID, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.RemoveOrgSelectedRepo(org, repoID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetOrgAllowedActions(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	writeJSON(w, http.StatusOK, allowedActionsJSON(p.ActionsAllowed))
}

func (s *Server) handleSetOrgAllowedActions(w http.ResponseWriter, r *http.Request) {
	var req ActionsAllowed
	if !decodeJSONBody(w, r, &req) {
		return
	}
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	p.ActionsAllowed = &req
	p.AllowedActions = "selected"
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetOrgWorkflowPermissions(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	writeJSON(w, http.StatusOK, workflowPermissionsJSON(p.WorkflowPermissions))
}

func (s *Server) handleSetOrgWorkflowPermissions(w http.ResponseWriter, r *http.Request) {
	var req WorkflowPermissions
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.DefaultWorkflowPermissions == "" {
		req.DefaultWorkflowPermissions = "read"
	}
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	p.WorkflowPermissions = &req
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

// --- Org permissions extras ---

func (s *Server) handleGetOrgArtifactAndLogRetention(w http.ResponseWriter, r *http.Request) {
	p := s.store.GetOrgActionsPermissions(r.PathValue("org"))
	writeJSON(w, http.StatusOK, map[string]any{
		"days":                 p.ArtifactAndLogRetentionDays,
		"maximum_allowed_days": orgArtifactAndLogRetentionMaxDays,
	})
}

func (s *Server) handleSetOrgArtifactAndLogRetention(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days *int `json:"days"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Days == nil || *req.Days < 1 || *req.Days > orgArtifactAndLogRetentionMaxDays {
		writeGHValidationError(w, "ActionsArtifactAndLogRetention", "days", "invalid")
		return
	}
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	p.ArtifactAndLogRetentionDays = *req.Days
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

// forkPRApprovalPolicies are the actions-fork-pr-contributor-approval
// approval_policy enum values.
var forkPRApprovalPolicies = map[string]bool{
	"first_time_contributors_new_to_github": true,
	"first_time_contributors":               true,
	"all_external_contributors":             true,
}

func (s *Server) handleGetOrgForkPRContributorApproval(w http.ResponseWriter, r *http.Request) {
	p := s.store.GetOrgActionsPermissions(r.PathValue("org"))
	writeJSON(w, http.StatusOK, map[string]any{
		"approval_policy": p.ForkPRApprovalPolicy,
	})
}

func (s *Server) handleSetOrgForkPRContributorApproval(w http.ResponseWriter, r *http.Request) {
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
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	p.ForkPRApprovalPolicy = req.ApprovalPolicy
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetOrgForkPRWorkflowsPrivateRepos(w http.ResponseWriter, r *http.Request) {
	p := s.store.GetOrgActionsPermissions(r.PathValue("org"))
	settings := p.ForkPRWorkflowsPrivateRepos
	if settings == nil {
		settings = &ForkPRWorkflowsPrivateRepos{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_workflows_from_fork_pull_requests":  settings.RunWorkflowsFromForkPullRequests,
		"send_write_tokens_to_workflows":         settings.SendWriteTokensToWorkflows,
		"send_secrets_and_variables":             settings.SendSecretsAndVariables,
		"require_approval_for_fork_pr_workflows": settings.RequireApprovalForForkPRWorkflows,
	})
}

func (s *Server) handleSetOrgForkPRWorkflowsPrivateRepos(w http.ResponseWriter, r *http.Request) {
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
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	settings := p.ForkPRWorkflowsPrivateRepos
	if settings == nil {
		settings = &ForkPRWorkflowsPrivateRepos{}
		p.ForkPRWorkflowsPrivateRepos = settings
	}
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
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetOrgSelfHostedRunnersSettings(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	out := map[string]any{
		"enabled_repositories": p.SelfHostedRunnersEnabledRepositories,
	}
	if p.SelfHostedRunnersEnabledRepositories == "selected" {
		out["selected_repositories_url"] = fmt.Sprintf(
			"%s/api/v3/orgs/%s/actions/permissions/self-hosted-runners/repositories", s.baseURL(r), org)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSetOrgSelfHostedRunnersSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnabledRepositories string `json:"enabled_repositories"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.EnabledRepositories {
	case "all", "selected", "none":
	default:
		writeGHValidationError(w, "SelfHostedRunnersSettings", "enabled_repositories", "invalid")
		return
	}
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	p.SelfHostedRunnersEnabledRepositories = req.EnabledRepositories
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgSelfHostedRunnerRepos(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	base := s.baseURL(r)
	repos := make([]map[string]any, 0, len(p.SelfHostedRunnersSelectedRepoIDs))
	for _, id := range p.SelfHostedRunnersSelectedRepoIDs {
		s.store.Mu.RLock()
		repo := s.store.Repos[id]
		s.store.Mu.RUnlock()
		if repo != nil {
			repos = append(repos, repoToJSON(repo, s.store, base))
		}
	}
	paged := paginateAndLink(w, r, repos)
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":  len(repos),
		"repositories": paged,
	})
}

func (s *Server) handleSetOrgSelfHostedRunnerRepos(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SelectedRepositoryIDs == nil {
		writeGHValidationError(w, "SelfHostedRunnersSettings", "selected_repository_ids", "missing_field")
		return
	}
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	p.SelfHostedRunnersSelectedRepoIDs = req.SelectedRepositoryIDs
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddOrgSelfHostedRunnerRepo(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	repoID, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	exists := s.store.Repos[repoID] != nil
	s.store.Mu.RUnlock()
	if !exists {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	p := s.store.GetOrgActionsPermissions(org)
	for _, id := range p.SelfHostedRunnersSelectedRepoIDs {
		if id == repoID {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	p.SelfHostedRunnersSelectedRepoIDs = append(p.SelfHostedRunnersSelectedRepoIDs, repoID)
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveOrgSelfHostedRunnerRepo(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	repoID, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	p := s.store.GetOrgActionsPermissions(org)
	kept := p.SelfHostedRunnersSelectedRepoIDs[:0:0]
	for _, id := range p.SelfHostedRunnersSelectedRepoIDs {
		if id != repoID {
			kept = append(kept, id)
		}
	}
	p.SelfHostedRunnersSelectedRepoIDs = kept
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

// --- Org cache usage + policy limits ---

// orgCacheUsageByRepo aggregates the finalized Actions cache entries of
// every repository owned by the org: repo full name → (count, bytes),
// plus the sorted repo names for stable listing.
func (s *Server) orgCacheUsageByRepo(org string) (map[string]struct {
	Count int
	Bytes int64
}, []string) {
	usage := map[string]struct {
		Count int
		Bytes int64
	}{}
	prefix := strings.ToLower(org) + "/"
	s.artifactStore.Mu.RLock()
	for _, entry := range s.artifactStore.Caches {
		if !entry.Finalized || !strings.HasPrefix(strings.ToLower(entry.Repo), prefix) {
			continue
		}
		u := usage[entry.Repo]
		u.Count++
		u.Bytes += entry.Size
		usage[entry.Repo] = u
	}
	s.artifactStore.Mu.RUnlock()
	names := make([]string, 0, len(usage))
	for name := range usage {
		names = append(names, name)
	}
	sort.Strings(names)
	return usage, names
}

func (s *Server) handleOrgCacheUsage(w http.ResponseWriter, r *http.Request) {
	usage, names := s.orgCacheUsageByRepo(r.PathValue("org"))
	count := 0
	var bytes int64
	for _, name := range names {
		count += usage[name].Count
		bytes += usage[name].Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_active_caches_count":         count,
		"total_active_caches_size_in_bytes": bytes,
	})
}

func (s *Server) handleOrgCacheUsageByRepository(w http.ResponseWriter, r *http.Request) {
	usage, names := s.orgCacheUsageByRepo(r.PathValue("org"))
	page := paginateAndLink(w, r, names)
	out := make([]map[string]any, 0, len(page))
	for _, name := range page {
		out = append(out, map[string]any{
			"full_name":                   name,
			"active_caches_size_in_bytes": usage[name].Bytes,
			"active_caches_count":         usage[name].Count,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":             len(names),
		"repository_cache_usages": out,
	})
}

func (s *Server) handleGetOrgMaxCacheRetention(w http.ResponseWriter, r *http.Request) {
	p := s.store.GetOrgActionsPermissions(r.PathValue("org"))
	writeJSON(w, http.StatusOK, map[string]any{
		"max_cache_retention_days": p.MaxCacheRetentionDays,
	})
}

func (s *Server) handleSetOrgMaxCacheRetention(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxCacheRetentionDays *int `json:"max_cache_retention_days"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.MaxCacheRetentionDays == nil || *req.MaxCacheRetentionDays < 1 {
		writeGHError(w, http.StatusBadRequest, "max_cache_retention_days must be a positive integer")
		return
	}
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	p.MaxCacheRetentionDays = *req.MaxCacheRetentionDays
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetOrgMaxCacheSize(w http.ResponseWriter, r *http.Request) {
	p := s.store.GetOrgActionsPermissions(r.PathValue("org"))
	writeJSON(w, http.StatusOK, map[string]any{
		"max_cache_size_gb": p.MaxCacheSizeGB,
	})
}

func (s *Server) handleSetOrgMaxCacheSize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxCacheSizeGB *int `json:"max_cache_size_gb"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.MaxCacheSizeGB == nil || *req.MaxCacheSizeGB < 1 {
		writeGHError(w, http.StatusBadRequest, "max_cache_size_gb must be a positive integer")
		return
	}
	org := r.PathValue("org")
	p := s.store.GetOrgActionsPermissions(org)
	p.MaxCacheSizeGB = *req.MaxCacheSizeGB
	s.store.SetOrgActionsPermissions(org, p)
	w.WriteHeader(http.StatusNoContent)
}

// --- Repo permissions handlers ---

func (s *Server) handleGetRepoActionsPermissions(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	writeJSON(w, http.StatusOK, repoActionsPermissionsJSON(p, s.baseURL(r), repo))
}

func (s *Server) handleSetRepoActionsPermissions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled        *bool  `json:"enabled"`
		AllowedActions string `json:"allowed_actions"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeGHValidationError(w, "ActionsPermissions", "enabled", "missing_field")
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.Enabled = *req.Enabled
	if req.AllowedActions != "" {
		p.AllowedActions = req.AllowedActions
	}
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoActionsAccessLevel(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	writeJSON(w, http.StatusOK, map[string]string{
		"access_level": p.AccessLevel,
	})
}

func (s *Server) handleSetRepoActionsAccessLevel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessLevel string `json:"access_level"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.AccessLevel = req.AccessLevel
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoAllowedActions(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	writeJSON(w, http.StatusOK, allowedActionsJSON(p.ActionsAllowed))
}

func (s *Server) handleSetRepoAllowedActions(w http.ResponseWriter, r *http.Request) {
	var req ActionsAllowed
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.ActionsAllowed = &req
	p.AllowedActions = "selected"
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoWorkflowPermissions(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	writeJSON(w, http.StatusOK, workflowPermissionsJSON(p.WorkflowPermissions))
}

func (s *Server) handleSetRepoWorkflowPermissions(w http.ResponseWriter, r *http.Request) {
	var req WorkflowPermissions
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.DefaultWorkflowPermissions == "" {
		req.DefaultWorkflowPermissions = "read"
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.WorkflowPermissions = &req
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoForkPRContributorApproval(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	writeJSON(w, http.StatusOK, map[string]string{
		"approval_policy": p.ForkPRContributorApproval,
	})
}

func (s *Server) handleSetRepoForkPRContributorApproval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApprovalPolicy string `json:"approval_policy"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.ForkPRContributorApproval = req.ApprovalPolicy
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoForkPRWorkflowsPrivateRepos(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	settings := p.ForkPRWorkflowsPrivateRepos
	if settings == nil {
		settings = &ForkPRWorkflowsPrivateRepos{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_workflows_from_fork_pull_requests":  settings.RunWorkflowsFromForkPullRequests,
		"send_write_tokens_to_workflows":         settings.SendWriteTokensToWorkflows,
		"send_secrets_and_variables":             settings.SendSecretsAndVariables,
		"require_approval_for_fork_pr_workflows": settings.RequireApprovalForForkPRWorkflows,
	})
}

func (s *Server) handleSetRepoForkPRWorkflowsPrivateRepos(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunWorkflowsFromForkPullRequests  *bool `json:"run_workflows_from_fork_pull_requests"`
		SendWriteTokensToWorkflows        *bool `json:"send_write_tokens_to_workflows"`
		SendSecretsAndVariables           *bool `json:"send_secrets_and_variables"`
		RequireApprovalForForkPRWorkflows *bool `json:"require_approval_for_fork_pr_workflows"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	settings := p.ForkPRWorkflowsPrivateRepos
	if settings == nil {
		settings = &ForkPRWorkflowsPrivateRepos{}
	}
	if req.RunWorkflowsFromForkPullRequests != nil {
		settings.RunWorkflowsFromForkPullRequests = *req.RunWorkflowsFromForkPullRequests
	}
	if req.SendWriteTokensToWorkflows != nil {
		settings.SendWriteTokensToWorkflows = *req.SendWriteTokensToWorkflows
	}
	if req.SendSecretsAndVariables != nil {
		settings.SendSecretsAndVariables = *req.SendSecretsAndVariables
	}
	if req.RequireApprovalForForkPRWorkflows != nil {
		settings.RequireApprovalForForkPRWorkflows = *req.RequireApprovalForForkPRWorkflows
	}
	p.ForkPRWorkflowsPrivateRepos = settings
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoArtifactAndLogRetention(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	writeJSON(w, http.StatusOK, map[string]int{
		"days":                 p.ArtifactAndLogRetentionDays,
		"maximum_allowed_days": artifactRetentionMaximumDays,
	})
}

func (s *Server) handleSetRepoArtifactAndLogRetention(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int `json:"days"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.ArtifactAndLogRetentionDays = req.Days
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoCacheRetentionLimit(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	writeJSON(w, http.StatusOK, map[string]int{
		"max_cache_retention_days": p.CacheRetentionLimitDays,
	})
}

func (s *Server) handleSetRepoCacheRetentionLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxCacheRetentionDays int `json:"max_cache_retention_days"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.CacheRetentionLimitDays = req.MaxCacheRetentionDays
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoCacheStorageLimit(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	writeJSON(w, http.StatusOK, map[string]int64{
		"max_cache_size_gb": p.CacheStorageLimitGB,
	})
}

func (s *Server) handleSetRepoCacheStorageLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxCacheSizeGB int64 `json:"max_cache_size_gb"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.CacheStorageLimitGB = req.MaxCacheSizeGB
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoCacheUsagePolicy(w http.ResponseWriter, r *http.Request) {
	p := s.store.GetRepoActionsPermissions(repoFullName(r))
	cacheSizeGB := p.CacheStorageLimitGB
	if cacheSizeGB == 0 {
		s.store.Mu.RLock()
		cacheSizeGB = int64(s.store.EnterpriseSettings.ActionsDefaultCacheSizeGB)
		s.store.Mu.RUnlock()
	}
	writeJSON(w, http.StatusOK, map[string]int64{
		"repo_cache_size_limit_in_gb": cacheSizeGB,
	})
}

func (s *Server) handleUpdateRepoCacheUsagePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RepoCacheSizeLimitGB *int64 `json:"repo_cache_size_limit_in_gb"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.Mu.RLock()
	maxGB := int64(s.store.EnterpriseSettings.ActionsCacheSizeGB)
	s.store.Mu.RUnlock()
	if req.RepoCacheSizeLimitGB == nil || *req.RepoCacheSizeLimitGB <= 0 || *req.RepoCacheSizeLimitGB > maxGB {
		writeGHError(w, http.StatusBadRequest, "Invalid cache usage policy.")
		return
	}
	repo := repoFullName(r)
	p := s.store.GetRepoActionsPermissions(repo)
	p.CacheStorageLimitGB = *req.RepoCacheSizeLimitGB
	s.store.SetRepoActionsPermissions(repo, p)
	w.WriteHeader(http.StatusNoContent)
}

// --- Run logs ---

func (s *Server) handleDeleteRunLogs(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.Atoi(r.PathValue("run_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	wf := s.findWorkflowByRunIDInRepo(runID, repoFullName(r))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var logIDs []int
	var planIDs []string
	s.store.Mu.RLock()
	for _, j := range wf.Jobs {
		planID := j.PlanID
		if job := s.store.Jobs[j.JobID]; job != nil && job.PlanID != "" {
			planID = job.PlanID
		}
		if planID != "" {
			planIDs = append(planIDs, planID)
			if recs, ok := s.store.TimelineRecords[planID]; ok {
				for _, rec := range recs {
					if rec.Log != nil {
						logIDs = append(logIDs, rec.Log.ID)
					}
				}
			}
		}
	}
	s.store.Mu.RUnlock()
	for _, logID := range logIDs {
		if err := s.artifactStore.DeleteLogData(r.Context(), logID); err != nil {
			writeGHError(w, http.StatusInternalServerError, "log byte-store delete: "+err.Error())
			return
		}
	}
	s.store.Mu.Lock()
	for _, j := range wf.Jobs {
		delete(s.store.LogLines, j.JobID)
	}
	for _, planID := range planIDs {
		if recs, ok := s.store.TimelineRecords[planID]; ok {
			for _, rec := range recs {
				if rec.Log != nil {
					delete(s.store.LogFiles, rec.Log.ID)
				}
			}
		}
		delete(s.store.TimelineRecords, planID)
		if s.store.Persist != nil {
			s.store.Persist.MustDelete("timeline_records", planID)
		}
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// --- Runner labels ---

func (s *Server) handleListRunnerLabels(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerTargetFromRequest(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	a := s.store.Agents[id]
	s.store.Mu.RUnlock()
	if a == nil || !runnerVisibleAt(a.Scope, target) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, runnerLabelsJSON(a.Labels))
}

func (s *Server) handleSetRunnerLabels(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerTargetFromRequest(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Labels []string `json:"labels"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.Mu.Lock()
	a := s.store.Agents[id]
	if a != nil && runnerVisibleAt(a.Scope, target) {
		a.SetLabels(req.Labels)
	} else {
		a = nil
	}
	s.store.Mu.Unlock()
	if a == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, runnerLabelsJSON(a.Labels))
}

func (s *Server) handleRemoveAllRunnerLabels(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerTargetFromRequest(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	a := s.store.Agents[id]
	if a != nil && runnerVisibleAt(a.Scope, target) {
		a.ClearLabels()
	} else {
		a = nil
	}
	s.store.Mu.Unlock()
	if a == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, runnerLabelsJSON(a.Labels))
}

// handleAddRunnerLabels — POST .../actions/runners/{runner_id}/labels
// (repo + org scope): appends custom labels to the runner, returning
// the full label set.
func (s *Server) handleAddRunnerLabels(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerTargetFromRequest(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Labels []string `json:"labels"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.Labels) == 0 {
		writeGHValidationErrorSimple(w, "labels is missing")
		return
	}
	s.store.Mu.Lock()
	a := s.store.Agents[id]
	if a != nil && runnerVisibleAt(a.Scope, target) {
		a.AddLabels(req.Labels)
	} else {
		a = nil
	}
	s.store.Mu.Unlock()
	if a == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, runnerLabelsJSON(a.Labels))
}

// handleRemoveRunnerLabel — DELETE .../actions/runners/{runner_id}/labels/{name}
// (repo + org scope): removes one custom label. Read-only (system)
// labels cannot be removed (422); an absent label is 404.
func (s *Server) handleRemoveRunnerLabel(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerTargetFromRequest(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	name := r.PathValue("name")
	s.store.Mu.Lock()
	a := s.store.Agents[id]
	found := false
	readOnly := false
	if a != nil && runnerVisibleAt(a.Scope, target) {
		for _, l := range a.Labels {
			if l.Name == name {
				found = true
				readOnly = l.Type == "system"
				break
			}
		}
		if found && !readOnly {
			a.RemoveLabels([]string{name})
		}
	} else {
		a = nil
	}
	s.store.Mu.Unlock()
	if a == nil || !found {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if readOnly {
		writeGHError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("Label %q is a read-only label and cannot be removed", name))
		return
	}
	writeJSON(w, http.StatusOK, runnerLabelsJSON(a.Labels))
}

// --- JSON helpers ---

func orgActionsPermissionsJSON(p *OrgActionsPermissions, baseURL, org string) map[string]any {
	apiBase := fmt.Sprintf("%s/api/v3/orgs/%s/actions/permissions", baseURL, org)
	out := map[string]any{
		"enabled_repositories": p.EnabledRepositories,
		"allowed_actions":      p.AllowedActions,
	}
	if p.EnabledRepositories == "selected" {
		out["selected_repositories_url"] = apiBase + "/repositories"
	}
	if p.AllowedActions == "selected" {
		out["selected_actions_url"] = apiBase + "/selected-actions"
	}
	return out
}

func repoActionsPermissionsJSON(p *RepoActionsPermissions, baseURL, repo string) map[string]any {
	owner, name, _ := strings.Cut(repo, "/")
	apiBase := fmt.Sprintf("%s/api/v3/repos/%s/%s/actions/permissions", baseURL, owner, name)
	out := map[string]any{
		"enabled":         p.Enabled,
		"allowed_actions": p.AllowedActions,
	}
	if p.AllowedActions == "selected" {
		out["selected_actions_url"] = apiBase + "/selected-actions"
	}
	return out
}

func allowedActionsJSON(a *ActionsAllowed) map[string]any {
	if a == nil {
		return map[string]any{
			"github_owned_allowed": true,
			"verified_allowed":     false,
			"patterns_allowed":     []string{},
		}
	}
	patterns := a.PatternsAllowed
	if patterns == nil {
		patterns = []string{}
	}
	return map[string]any{
		"github_owned_allowed": a.GithubOwnedAllowed,
		"verified_allowed":     a.VerifiedAllowed,
		"patterns_allowed":     patterns,
	}
}

func workflowPermissionsJSON(w *WorkflowPermissions) map[string]any {
	if w == nil {
		return map[string]any{
			"default_workflow_permissions":     "read",
			"can_approve_pull_request_reviews": false,
		}
	}
	return map[string]any{
		"default_workflow_permissions":     w.DefaultWorkflowPermissions,
		"can_approve_pull_request_reviews": w.CanApprovePullRequestReviews,
	}
}

func runnerLabelsJSON(labels []Label) map[string]any {
	out := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		labelType := "custom"
		if l.Type == "system" {
			labelType = "read-only"
		}
		out = append(out, map[string]any{
			"id":   l.ID,
			"name": l.Name,
			"type": labelType,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		idI, _ := out[i]["id"].(int)
		idJ, _ := out[j]["id"].(int)
		return idI < idJ
	})
	return map[string]any{
		"total_count": len(out),
		"labels":      out,
	}
}
