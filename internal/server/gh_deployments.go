package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Deployments + Deployment Statuses + Environments.
// Endpoints:
//   POST   /repos/{o}/{r}/deployments
//   GET    /repos/{o}/{r}/deployments
//   GET    /repos/{o}/{r}/deployments/{id}
//   DELETE /repos/{o}/{r}/deployments/{id}
//   POST   /repos/{o}/{r}/deployments/{id}/statuses
//   GET    /repos/{o}/{r}/deployments/{id}/statuses
//   GET    /repos/{o}/{r}/deployments/{id}/statuses/{status_id}
//   GET    /repos/{o}/{r}/environments
//   GET    /repos/{o}/{r}/environments/{env_name}
//   PUT    /repos/{o}/{r}/environments/{env_name}
//   DELETE /repos/{o}/{r}/environments/{env_name}
//
// gh CLI has no top-level deploy command; this surface is used heavily by
// octokit / probot / GitOps controllers reacting to `deployment` and
// `deployment_status` webhook events.

func (s *Server) registerGHDeploymentsRoutes() {
	s.route("POST /api/v3/repos/{owner}/{repo}/deployments",
		s.requirePerm(store.ScopeDeployments, store.PermWrite, s.handleCreateDeployment))
	s.route("GET /api/v3/repos/{owner}/{repo}/deployments",
		s.handleListDeployments)
	s.route("GET /api/v3/repos/{owner}/{repo}/deployments/{deployment_id}",
		s.handleGetDeployment)
	s.route("DELETE /api/v3/repos/{owner}/{repo}/deployments/{deployment_id}",
		s.requirePerm(store.ScopeDeployments, store.PermWrite, s.handleDeleteDeployment))
	s.route("POST /api/v3/repos/{owner}/{repo}/deployments/{deployment_id}/statuses",
		s.requirePerm(store.ScopeDeployments, store.PermWrite, s.handleCreateDeploymentStatus))
	s.route("GET /api/v3/repos/{owner}/{repo}/deployments/{deployment_id}/statuses",
		s.handleListDeploymentStatuses)
	s.route("GET /api/v3/repos/{owner}/{repo}/deployments/{deployment_id}/statuses/{status_id}",
		s.handleGetDeploymentStatus)

	s.route("GET /api/v3/repos/{owner}/{repo}/environments",
		s.handleListEnvironments)
	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}",
		s.handleGetEnvironment)
	s.route("PUT /api/v3/repos/{owner}/{repo}/environments/{env_name}",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleUpsertEnvironment))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/environments/{env_name}",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDeleteEnvironment))
}

func (s *Server) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Ref                   string                 `json:"ref"`
		Task                  string                 `json:"task"`
		AutoMerge             flexBool               `json:"auto_merge"`
		RequiredContexts      []string               `json:"required_contexts"`
		Payload               map[string]interface{} `json:"payload"`
		Environment           string                 `json:"environment"`
		Description           string                 `json:"description"`
		TransientEnvironment  flexBool               `json:"transient_environment"`
		ProductionEnvironment flexBool               `json:"production_environment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Ref == "" {
		store.WriteGHValidationError(w, "Deployment", "ref", "missing_field")
		return
	}
	env := req.Environment
	if env == "" {
		env = "production"
	}
	s.store.Deployments.UpsertEnvironment(repo.ID, env)
	d := s.store.Deployments.CreateDeployment(repo.ID, user.ID, req.Ref, req.Ref, req.Task, env, req.Description, req.Payload, bool(req.ProductionEnvironment), bool(req.TransientEnvironment))
	s.emitWebhookEvent(repo.FullName, "deployment", "created", buildDeploymentEventPayload(repo, d, user, "created", s.baseURL(r)))
	s.recordAuditEvent("deployment.create", user.Login, "", map[string]interface{}{"repo": repo.FullName, "deployment_id": d.ID})
	deployJSON := deploymentToJSON(d, s.store, s.baseURL(r), repo)
	writeJSONCreated(w, jsonStringField(deployJSON, "url"), deployJSON)
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	deployments := filterDeployments(s.store.Deployments.ListDeployments(repo.ID), r.URL.Query())
	page := paginateAndLink(w, r, deployments)
	out := make([]map[string]interface{}, 0, len(page))
	for _, d := range page {
		out = append(out, deploymentToJSON(d, s.store, s.baseURL(r), repo))
	}
	writeJSON(w, http.StatusOK, out)
}

// filterDeployments applies the four documented query filters on the
// deployments listing: sha (the SHA recorded at creation time), ref (the branch,
// tag or SHA name), task, and environment. Each is an exact match on the value
// recorded when the deployment was created, and each is independent — a listing
// narrowed by several of them keeps only the deployments matching all of them.
//
// A filter that was not sent narrows nothing. The published contract writes the
// default of each as the string "none", which reads as a sentinel for "no
// filter" rather than a value to match, so an absent — or empty — parameter
// leaves the listing whole.
func filterDeployments(deployments []*store.Deployment, query url.Values) []*store.Deployment {
	filters := []struct {
		want  string
		field func(*store.Deployment) string
	}{
		{query.Get("sha"), func(d *store.Deployment) string { return d.Sha }},
		{query.Get("ref"), func(d *store.Deployment) string { return d.Ref }},
		{query.Get("task"), func(d *store.Deployment) string { return d.Task }},
		{query.Get("environment"), func(d *store.Deployment) string { return d.Environment }},
	}
	out := make([]*store.Deployment, 0, len(deployments))
	for _, d := range deployments {
		keep := true
		for _, f := range filters {
			if f.want != "" && f.field(d) != f.want {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("deployment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.Deployments.GetDeployment(id)
	if d == nil || d.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, deploymentToJSON(d, s.store, s.baseURL(r), repo))
}

func (s *Server) handleDeleteDeployment(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	id, err := strconv.Atoi(r.PathValue("deployment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.Deployments.GetDeployment(id)
	if d == nil || d.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Deployments.DeleteDeployment(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateDeploymentStatus(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	id, err := strconv.Atoi(r.PathValue("deployment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.Deployments.GetDeployment(id)
	if d == nil || d.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		State          string   `json:"state"`
		LogURL         string   `json:"log_url"`
		Description    string   `json:"description"`
		Environment    string   `json:"environment"`
		EnvironmentURL string   `json:"environment_url"`
		AutoInactive   flexBool `json:"auto_inactive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.State == "" {
		store.WriteGHValidationError(w, "DeploymentStatus", "state", "missing_field")
		return
	}
	env := req.Environment
	if env == "" {
		env = d.Environment
	}
	status, autoInactivated := s.store.Deployments.AddStatus(id, user.ID, req.State, req.Description, "", req.LogURL, req.EnvironmentURL, env, bool(req.AutoInactive))
	s.emitWebhookEvent(repo.FullName, "deployment_status", req.State, buildDeploymentStatusEventPayload(repo, d, status, user, s.baseURL(r)))
	for _, ai := range autoInactivated {
		priorDep := s.store.Deployments.GetDeployment(ai.DeploymentID)
		if priorDep == nil {
			continue
		}
		priorRepo := s.store.GetRepoByID(priorDep.RepoID)
		if priorRepo == nil {
			continue
		}
		s.emitWebhookEvent(priorRepo.FullName, "deployment_status", "inactive", buildDeploymentStatusEventPayload(priorRepo, priorDep, ai.Status, user, s.baseURL(r)))
	}
	statusJSON := deploymentStatusToJSON(status, s.store, s.baseURL(r), repo)
	writeJSONCreated(w, jsonStringField(statusJSON, "url"), statusJSON)
}

func (s *Server) handleListDeploymentStatuses(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("deployment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.Deployments.GetDeployment(id)
	if d == nil || d.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	statuses := s.store.Deployments.ListStatuses(id)
	page := paginateAndLink(w, r, statuses)
	out := make([]map[string]interface{}, 0, len(page))
	for _, st := range page {
		out = append(out, deploymentStatusToJSON(st, s.store, s.baseURL(r), repo))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetDeploymentStatus(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("deployment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.Deployments.GetDeployment(id)
	if d == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !requireRepoOwns(w, repo, d.RepoID) {
		return
	}
	statusID, err := strconv.Atoi(r.PathValue("status_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	status := s.store.Deployments.GetStatus(statusID)
	if status == nil || status.DeploymentID != d.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, deploymentStatusToJSON(status, s.store, s.baseURL(r), repo))
}

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	envs := s.store.Deployments.ListEnvironments(repo.ID)
	out := make([]map[string]interface{}, 0, len(envs))
	for _, e := range envs {
		out = append(out, environmentToJSON(e, s.store, s.baseURL(r), repo))
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":  len(envs),
		"environments": paged,
	})
}

func (s *Server) handleGetEnvironment(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	env := s.store.Deployments.GetEnvironment(repo.ID, r.PathValue("env_name"))
	if env == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, environmentToJSON(env, s.store, s.baseURL(r), repo))
}

func (s *Server) handleUpsertEnvironment(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var body struct {
		WaitTimer *int `json:"wait_timer"`
		Reviewers []struct {
			Type string `json:"type"`
			ID   int    `json:"id"`
		} `json:"reviewers"`
		PreventSelfReview      *bool                         `json:"prevent_self_review"`
		DeploymentBranchPolicy *store.DeploymentBranchPolicy `json:"deployment_branch_policy"`
	}
	// An absent body is valid (environment with no protection config), but
	// malformed JSON is still a 400 like real GitHub.
	if !decodeJSONBodyOptional(w, r, &body) {
		return
	}

	env := s.store.Deployments.UpsertEnvironment(repo.ID, r.PathValue("env_name"))

	if body.WaitTimer != nil || body.Reviewers != nil {
		var reviewers []map[string]interface{}
		for _, rev := range body.Reviewers {
			revType := rev.Type
			if revType == "" {
				revType = "User"
			}
			reviewers = append(reviewers, map[string]interface{}{"type": revType, "id": rev.ID})
		}
		s.store.Deployments.SetEnvironmentProtection(repo.ID, env.Name, body.WaitTimer, reviewers)
	}
	if body.PreventSelfReview != nil {
		s.store.Deployments.SetEnvironmentPreventSelfReview(repo.ID, env.Name, *body.PreventSelfReview)
	}
	s.store.Deployments.SetEnvironmentBranchPolicyConfig(repo.ID, env.Name, body.DeploymentBranchPolicy)
	writeJSON(w, http.StatusOK, environmentToJSON(env, s.store, s.baseURL(r), repo))
}

func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	env := s.store.Deployments.GetEnvironment(repo.ID, r.PathValue("env_name"))
	if env == nil || !s.store.Deployments.DeleteEnvironment(repo.ID, env.Name) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.PruneEnvironmentPolicies(env.ID)
	w.WriteHeader(http.StatusNoContent)
}

func deploymentToJSON(d *store.Deployment, st *store.Store, baseURL string, repo *store.Repo) map[string]interface{} {
	if d == nil {
		return nil
	}
	var creator map[string]interface{}
	st.Mu.RLock()
	if u := st.Users[d.CreatorID]; u != nil {
		creator = store.UserToJSON(u, baseURL)
	}
	st.Mu.RUnlock()
	return map[string]interface{}{
		"id":                     d.ID,
		"node_id":                d.NodeID,
		"sha":                    d.Sha,
		"ref":                    d.Ref,
		"task":                   d.Task,
		"payload":                jsonObject(d.Payload),
		"original_environment":   d.OriginalEnv,
		"environment":            d.Environment,
		"description":            d.Description,
		"creator":                creator,
		"created_at":             d.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":             d.UpdatedAt.UTC().Format(time.RFC3339),
		"statuses_url":           fmt.Sprintf("%s/api/v3/repos/%s/deployments/%d/statuses", baseURL, repo.FullName, d.ID),
		"repository_url":         fmt.Sprintf("%s/api/v3/repos/%s", baseURL, repo.FullName),
		"url":                    fmt.Sprintf("%s/api/v3/repos/%s/deployments/%d", baseURL, repo.FullName, d.ID),
		"transient_environment":  d.TransientEnv,
		"production_environment": d.ProductionEnv,
	}
}

func deploymentStatusToJSON(st *store.DeploymentStatus, stor *store.Store, baseURL string, repo *store.Repo) map[string]interface{} {
	if st == nil {
		return nil
	}
	var creator map[string]interface{}
	stor.Mu.RLock()
	if u := stor.Users[st.CreatorID]; u != nil {
		creator = store.UserToJSON(u, baseURL)
	}
	stor.Mu.RUnlock()
	return map[string]interface{}{
		"id":              st.ID,
		"node_id":         st.NodeID,
		"state":           st.State,
		"creator":         creator,
		"description":     st.Description,
		"environment":     st.Environment,
		"target_url":      st.TargetURL,
		"log_url":         st.LogURL,
		"environment_url": st.EnvironmentURL,
		"created_at":      st.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":      st.UpdatedAt.UTC().Format(time.RFC3339),
		"url":             fmt.Sprintf("%s/api/v3/repos/%s/deployments/%d/statuses/%d", baseURL, repo.FullName, st.DeploymentID, st.ID),
		"deployment_url":  fmt.Sprintf("%s/api/v3/repos/%s/deployments/%d", baseURL, repo.FullName, st.DeploymentID),
		"repository_url":  fmt.Sprintf("%s/api/v3/repos/%s", baseURL, repo.FullName),
	}
}

func environmentToJSON(e *store.Environment, st *store.Store, baseURL string, repo *store.Repo) map[string]interface{} {
	if e == nil {
		return nil
	}
	var branchPolicy interface{}
	if e.DeploymentBranchPolicy != nil {
		branchPolicy = map[string]interface{}{
			"protected_branches":     e.DeploymentBranchPolicy.ProtectedBranches,
			"custom_branch_policies": e.DeploymentBranchPolicy.CustomBranchPolicies,
		}
	}
	out := map[string]interface{}{
		"id":                       e.ID,
		"node_id":                  e.NodeID,
		"name":                     e.Name,
		"url":                      fmt.Sprintf("%s/api/v3/repos/%s/environments/%s", baseURL, repo.FullName, e.Name),
		"html_url":                 fmt.Sprintf("%s/%s/deployments/activity_log?environments_filter=%s", baseURL, repo.FullName, e.Name),
		"created_at":               e.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":               e.UpdatedAt.UTC().Format(time.RFC3339),
		"deployment_branch_policy": branchPolicy,
	}
	rules := []map[string]interface{}{}
	if e.WaitTimer > 0 {
		rules = append(rules, map[string]interface{}{
			"id":         e.ID*10 + 1,
			"node_id":    fmt.Sprintf("GA_kwDO%08d", e.ID*10+1),
			"type":       "wait_timer",
			"wait_timer": e.WaitTimer,
		})
	}
	if len(e.Reviewers) > 0 {
		rules = append(rules, map[string]interface{}{
			"id":        e.ID*10 + 2,
			"node_id":   fmt.Sprintf("GA_kwDO%08d", e.ID*10+2),
			"type":      "required_reviewers",
			"reviewers": environmentReviewersJSON(e, st, baseURL),
		})
	}
	if branchPolicy != nil {
		rules = append(rules, map[string]interface{}{
			"id":      e.ID*10 + 3,
			"node_id": fmt.Sprintf("GA_kwDO%08d", e.ID*10+3),
			"type":    "branch_policy",
		})
	}
	out["protection_rules"] = rules
	return out
}

// environmentReviewersJSON renders the configured reviewers with their
// resolved reviewer objects — the vendored `environment` schema's reviewer is
// a simple-user | team union keyed by the deployment-reviewer-type — the
// shape protection rules and pending deployments share. Must not be called
// with st.Mu held (team resolution takes RLock).
func environmentReviewersJSON(e *store.Environment, st *store.Store, baseURL string) []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, rev := range e.Reviewers {
		revType, _ := rev["type"].(string)
		var id int
		switch v := rev["id"].(type) {
		case int:
			id = v
		case float64:
			id = int(v)
		}
		entry := map[string]interface{}{"type": revType}
		switch revType {
		case "Team":
			if team := st.GetTeamByID(id); team != nil {
				if org := st.GetOrgByID(team.OrgID); org != nil {
					entry["reviewer"] = teamSimpleJSON(team, org, st, baseURL)
				}
			}
		default: // "User"
			st.Mu.RLock()
			if u := st.Users[id]; u != nil {
				entry["reviewer"] = store.UserToJSON(u, baseURL)
			}
			st.Mu.RUnlock()
		}
		out = append(out, entry)
	}
	return out
}

func buildDeploymentEventPayload(repo *store.Repo, d *store.Deployment, sender *store.User, action, baseURL string) map[string]interface{} {
	return attachInstallationBlock(map[string]interface{}{
		"action": action,
		"deployment": map[string]interface{}{
			"id":          d.ID,
			"sha":         d.Sha,
			"ref":         d.Ref,
			"task":        d.Task,
			"environment": d.Environment,
		},
		"repository": repoPayload(repo, baseURL),
		"sender":     senderPayload(sender, baseURL),
	}, nil)
}

func buildDeploymentStatusEventPayload(repo *store.Repo, d *store.Deployment, status *store.DeploymentStatus, sender *store.User, baseURL string) map[string]interface{} {
	return attachInstallationBlock(map[string]interface{}{
		"action": status.State,
		"deployment_status": map[string]interface{}{
			"id":          status.ID,
			"state":       status.State,
			"description": status.Description,
			"environment": status.Environment,
		},
		"deployment": map[string]interface{}{
			"id":          d.ID,
			"environment": d.Environment,
		},
		"repository": repoPayload(repo, baseURL),
		"sender":     senderPayload(sender, baseURL),
	}, nil)
}
