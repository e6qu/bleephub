package bleephub

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHCodespacesRoutes() {
	s.route("GET /api/v3/user/codespaces", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleListUserCodespaces))
	s.route("POST /api/v3/user/codespaces", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleCreateUserCodespace))
	s.route("GET /api/v3/user/codespaces/{codespace_name}", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleGetUserCodespace))
	s.route("PATCH /api/v3/user/codespaces/{codespace_name}", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handlePatchUserCodespace))
	s.route("DELETE /api/v3/user/codespaces/{codespace_name}", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleDeleteUserCodespace))
	s.route("POST /api/v3/user/codespaces/{codespace_name}/start", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleStartUserCodespace))
	s.route("POST /api/v3/user/codespaces/{codespace_name}/stop", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleStopUserCodespace))

	s.route("GET /api/v3/repos/{owner}/{repo}/codespaces", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleListRepoCodespaces))
	s.route("POST /api/v3/repos/{owner}/{repo}/codespaces", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleCreateRepoCodespace))
	s.route("GET /api/v3/repos/{owner}/{repo}/codespaces/devcontainers", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleListRepoDevcontainers))
	s.route("GET /api/v3/repos/{owner}/{repo}/codespaces/new", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleGetCodespaceDefaults))
	s.route("GET /api/v3/repos/{owner}/{repo}/codespaces/permissions_check", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleCodespacePermissionsCheck))
	s.route("GET /api/v3/repos/{owner}/{repo}/codespaces/machines", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleListCodespaceMachines))

	s.route("GET /api/v3/orgs/{org}/members/{username}/codespaces", s.requireOrgAdminOrCodespaceScope(s.handleListOrgMemberCodespaces))
	s.route("DELETE /api/v3/orgs/{org}/members/{username}/codespaces/{codespace_name}", s.requireOrgAdminOrCodespaceScope(s.handleDeleteOrgMemberCodespace))
	s.route("POST /api/v3/orgs/{org}/members/{username}/codespaces/{codespace_name}/stop", s.requireOrgAdminOrCodespaceScope(s.handleStopOrgMemberCodespace))

	s.route("GET /api/v3/user/codespaces/secrets", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleListUserCodespaceSecrets))
	s.route("GET /api/v3/user/codespaces/secrets/public-key", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleGetCodespacePublicKey))
	s.route("GET /api/v3/user/codespaces/secrets/{secret_name}", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleGetUserCodespaceSecret))
	s.route("PUT /api/v3/user/codespaces/secrets/{secret_name}", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handlePutUserCodespaceSecret))
	s.route("DELETE /api/v3/user/codespaces/secrets/{secret_name}", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleDeleteUserCodespaceSecret))

	s.route("GET /api/v3/user/codespaces/secrets/{secret_name}/repositories", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleListUserCodespaceSecretRepos))
	s.route("PUT /api/v3/user/codespaces/secrets/{secret_name}/repositories", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleSetUserCodespaceSecretRepos))
	s.route("PUT /api/v3/user/codespaces/secrets/{secret_name}/repositories/{repository_id}", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleAddUserCodespaceSecretRepo))
	s.route("DELETE /api/v3/user/codespaces/secrets/{secret_name}/repositories/{repository_id}", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleRemoveUserCodespaceSecretRepo))

	// Go 1.22 ServeMux rejects GET /user/codespaces/{codespace_name}/machines
	// alongside the literal GET /user/codespaces/secrets/{secret_name} (they
	// overlap with neither more specific), so both GET shapes dispatch through
	// wildcards; the more-specific secrets routes above still win.
	s.route("GET /api/v3/user/codespaces/{codespace_name}/{sub}", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleUserCodespaceTwoSegGetDispatch))
	s.route("GET /api/v3/user/codespaces/{codespace_name}/{sub}/{export_id}", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleUserCodespaceThreeSegGetDispatch))

	s.route("POST /api/v3/user/codespaces/{codespace_name}/exports", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleExportUserCodespace))
	s.route("POST /api/v3/user/codespaces/{codespace_name}/publish", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handlePublishUserCodespace))

	s.route("POST /api/v3/repos/{owner}/{repo}/pulls/{pull_number}/codespaces", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleCreatePullRequestCodespace))

	s.route("GET /api/v3/repos/{owner}/{repo}/codespaces/secrets", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleListRepoCodespaceSecrets))
	s.route("GET /api/v3/repos/{owner}/{repo}/codespaces/secrets/public-key", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleGetCodespacePublicKey))
	s.route("GET /api/v3/repos/{owner}/{repo}/codespaces/secrets/{secret_name}", s.requirePerm(store.ScopeCodespaces, store.PermRead, s.handleGetRepoCodespaceSecret))
	s.route("PUT /api/v3/repos/{owner}/{repo}/codespaces/secrets/{secret_name}", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handlePutRepoCodespaceSecret))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/codespaces/secrets/{secret_name}", s.requirePerm(store.ScopeCodespaces, store.PermWrite, s.handleDeleteRepoCodespaceSecret))

	s.route("GET /api/v3/orgs/{org}/codespaces", s.requireOrgAdminOrCodespaceScope(s.handleListOrgCodespaces))
	s.route("PUT /api/v3/orgs/{org}/codespaces/access", s.requireOrgAdminOrCodespaceScope(s.handleSetOrgCodespacesAccess))
	s.route("POST /api/v3/orgs/{org}/codespaces/access/selected_users", s.requireOrgAdminOrCodespaceScope(s.handleAddOrgCodespacesAccessUsers))
	s.route("DELETE /api/v3/orgs/{org}/codespaces/access/selected_users", s.requireOrgAdminOrCodespaceScope(s.handleRemoveOrgCodespacesAccessUsers))

	s.route("GET /api/v3/orgs/{org}/codespaces/secrets", s.requireOrgAdminOrCodespaceScope(s.handleListOrgCodespaceSecrets))
	s.route("GET /api/v3/orgs/{org}/codespaces/secrets/public-key", s.requireOrgAdminOrCodespaceScope(s.handleGetCodespacePublicKey))
	s.route("GET /api/v3/orgs/{org}/codespaces/secrets/{secret_name}", s.requireOrgAdminOrCodespaceScope(s.handleGetOrgCodespaceSecret))
	s.route("PUT /api/v3/orgs/{org}/codespaces/secrets/{secret_name}", s.requireOrgAdminOrCodespaceScope(s.handlePutOrgCodespaceSecret))
	s.route("DELETE /api/v3/orgs/{org}/codespaces/secrets/{secret_name}", s.requireOrgAdminOrCodespaceScope(s.handleDeleteOrgCodespaceSecret))
	s.route("GET /api/v3/orgs/{org}/codespaces/secrets/{secret_name}/repositories", s.requireOrgAdminOrCodespaceScope(s.handleListOrgCodespaceSecretRepos))
	s.route("PUT /api/v3/orgs/{org}/codespaces/secrets/{secret_name}/repositories", s.requireOrgAdminOrCodespaceScope(s.handleSetOrgCodespaceSecretRepos))
	s.route("PUT /api/v3/orgs/{org}/codespaces/secrets/{secret_name}/repositories/{repository_id}", s.requireOrgAdminOrCodespaceScope(s.handleAddOrgCodespaceSecretRepo))
	s.route("DELETE /api/v3/orgs/{org}/codespaces/secrets/{secret_name}/repositories/{repository_id}", s.requireOrgAdminOrCodespaceScope(s.handleRemoveOrgCodespaceSecretRepo))
}

func (s *Server) handleListRepoDevcontainers(w http.ResponseWriter, r *http.Request) {
	if s.lookupReadableRepoFromPath(w, r) == nil {
		return
	}
	devcontainers := []map[string]interface{}{}
	paged := paginateAndLink(w, r, devcontainers)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":   len(devcontainers),
		"devcontainers": paged,
	})
}

func (s *Server) handleGetCodespaceDefaults(w http.ResponseWriter, r *http.Request) {
	if s.lookupReadableRepoFromPath(w, r) == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"billable_owner": store.UserToJSON(ghUserFromContext(r.Context()), s.baseURL(r)),
		"defaults": map[string]interface{}{
			"location":          "EastUs",
			"devcontainer_path": nil,
		},
	})
}

func (s *Server) handleCodespacePermissionsCheck(w http.ResponseWriter, r *http.Request) {
	if s.lookupReadableRepoFromPath(w, r) == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"accepted": true})
}

// auth helpers

func (s *Server) requireOrgAdminOrCodespaceScope(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := ghUserFromContext(r.Context())
		if user == nil {
			writeGHError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		org := s.store.GetOrg(r.PathValue("org"))
		if org == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		if !s.viewerCanAdminOrg(r.Context(), org.Login) {
			writeGHError(w, http.StatusForbidden, "Must have admin rights to Organization.")
			return
		}
		next(w, r)
	}
}

func (s *Server) resolveCodespace(w http.ResponseWriter, r *http.Request, ownerLogin, repoKey string) *store.Codespace {
	name := r.PathValue("codespace_name")
	cs := s.store.GetCodespaceByName(name)
	if cs == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if ownerLogin != "" && cs.OwnerLogin != ownerLogin {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if repoKey != "" && cs.RepoKey != repoKey {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return cs
}

// user codespace handlers

func (s *Server) handleListUserCodespaces(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	list := s.store.ListCodespacesByOwner(user.Login)
	out := make([]map[string]interface{}, len(list))
	for i, cs := range list {
		out[i] = s.codespaceToJSON(cs, s.baseURL(r))
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{"codespaces": paged, "total_count": len(out)})
}

func (s *Server) handleCreateUserCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	var req codespaceCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateCodespaceCreate(w, req) {
		return
	}
	hasRepository := req.RepositoryID > 0
	hasPullRequest := req.PullRequest != nil
	if hasRepository == hasPullRequest {
		store.WriteGHValidationError(w, "Codespace", "repository_id or pull_request", "missing_field")
		return
	}
	repositoryID := req.RepositoryID
	gitRef := req.Ref
	if hasPullRequest {
		repositoryID = req.PullRequest.RepositoryID
		if repositoryID <= 0 || req.PullRequest.PullRequestNumber <= 0 {
			store.WriteGHValidationError(w, "Codespace", "pull_request", "invalid")
			return
		}
	}
	repo := s.store.GetRepoByID(repositoryID)
	if repo == nil {
		field := "repository_id"
		if hasPullRequest {
			field = "pull_request"
		}
		store.WriteGHValidationError(w, "Codespace", field, "invalid")
		return
	}
	if hasPullRequest {
		pr := s.store.GetPullRequestByNumber(repo.ID, req.PullRequest.PullRequestNumber)
		if pr == nil {
			store.WriteGHValidationError(w, "Codespace", "pull_request", "invalid")
			return
		}
		gitRef = pr.HeadRefName
	}
	repoKey := repo.FullName
	opts := store.CodespaceCreateOptions{
		MachineName:            req.Machine,
		DisplayName:            req.DisplayName,
		WorkingDirectory:       req.WorkingDirectory,
		DevcontainerPath:       req.DevcontainerPath,
		Geolocation:            req.Geolocation,
		IdleTimeoutMinutes:     req.IdleTimeoutMinutes,
		RetentionPeriodMinutes: req.RetentionPeriodMinutes,
	}
	cs, err := s.store.CreateCodespace(user.Login, repoKey, gitRef, req.Location, opts)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "codespace create failed: "+err.Error())
		return
	}
	csJSON := s.codespaceToJSON(cs, s.baseURL(r))
	writeJSONCreated(w, jsonStringField(csJSON, "url"), csJSON)
}

func (s *Server) handleGetUserCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	cs := s.resolveCodespace(w, r, user.Login, "")
	if cs == nil {
		return
	}
	_ = s.store.RefreshCodespaceState(cs.ID)
	writeJSON(w, http.StatusOK, s.codespaceToJSON(cs, s.baseURL(r)))
}

func (s *Server) handlePatchUserCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	cs := s.resolveCodespace(w, r, user.Login, "")
	if cs == nil {
		return
	}
	var req codespacePatchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	cs, ok := s.store.UpdateCodespace(cs.ID, req.DisplayName, req.Machine, req.RetentionPeriodMinutes)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	_ = s.store.RefreshCodespaceState(cs.ID)
	writeJSON(w, http.StatusOK, s.codespaceToJSON(cs, s.baseURL(r)))
}

func (s *Server) handleDeleteUserCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	cs := s.resolveCodespace(w, r, user.Login, "")
	if cs == nil {
		return
	}
	ok, err := s.store.DeleteCodespace(cs.ID)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "codespace delete failed: "+err.Error())
		return
	}
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleStartUserCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	cs := s.resolveCodespace(w, r, user.Login, "")
	if cs == nil {
		return
	}
	if err := s.startCodespace(cs); err != nil {
		writeGHError(w, http.StatusInternalServerError, "codespace start failed: "+err.Error())
		return
	}
	cs = s.store.GetCodespace(cs.ID)
	writeJSON(w, http.StatusOK, s.codespaceToJSON(cs, s.baseURL(r)))
}

func (s *Server) handleStopUserCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	cs := s.resolveCodespace(w, r, user.Login, "")
	if cs == nil {
		return
	}
	if err := s.stopCodespace(cs); err != nil {
		writeGHError(w, http.StatusInternalServerError, "codespace stop failed: "+err.Error())
		return
	}
	cs = s.store.GetCodespace(cs.ID)
	writeJSON(w, http.StatusOK, s.codespaceToJSON(cs, s.baseURL(r)))
}

// repo codespace handlers

func (s *Server) handleListRepoCodespaces(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	list := s.store.ListCodespacesByRepo(repo.FullName)
	out := make([]map[string]interface{}, len(list))
	for i, cs := range list {
		out[i] = s.codespaceToJSON(cs, s.baseURL(r))
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{"codespaces": paged, "total_count": len(out)})
}

func (s *Server) handleCreateRepoCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req codespaceCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.RepositoryID == 0 {
		req.RepositoryID = repo.ID
	} else if req.RepositoryID != repo.ID {
		store.WriteGHValidationError(w, "Codespace", "repository_id", "invalid")
		return
	}
	opts := store.CodespaceCreateOptions{
		MachineName:            req.Machine,
		DisplayName:            req.DisplayName,
		WorkingDirectory:       req.WorkingDirectory,
		DevcontainerPath:       req.DevcontainerPath,
		Geolocation:            req.Geolocation,
		IdleTimeoutMinutes:     req.IdleTimeoutMinutes,
		RetentionPeriodMinutes: req.RetentionPeriodMinutes,
	}
	cs, err := s.store.CreateCodespace(user.Login, repo.FullName, req.Ref, req.Location, opts)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "codespace create failed: "+err.Error())
		return
	}
	csJSON := s.codespaceToJSON(cs, s.baseURL(r))
	writeJSONCreated(w, jsonStringField(csJSON, "url"), csJSON)
}

// machines

// codespaceMachineJSON renders one catalog machine. bleephub has no prebuild
// pipeline, so prebuild_availability is "none".
func codespaceMachineJSON(m store.CodespaceMachine) map[string]interface{} {
	return map[string]interface{}{
		"name":                  m.Name,
		"display_name":          m.DisplayName,
		"operating_system":      "linux",
		"storage_in_bytes":      m.StorageBytes,
		"memory_in_bytes":       m.MemoryBytes,
		"cpus":                  m.CPUs,
		"prebuild_availability": "none",
	}
}

func codespaceMachinesListJSON() map[string]interface{} {
	machines := make([]map[string]interface{}, len(store.CodespaceMachines))
	for i, m := range store.CodespaceMachines {
		machines[i] = codespaceMachineJSON(m)
	}
	return map[string]interface{}{"machines": machines, "total_count": len(machines)}
}

func (s *Server) handleListCodespaceMachines(w http.ResponseWriter, r *http.Request) {
	if s.lookupRepoFromPath(r) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, codespaceMachinesListJSON())
}

// handleUserCodespaceTwoSegGetDispatch fans GET /user/codespaces/{name}/{sub}
// out to its sub-resource: machines.
func (s *Server) handleUserCodespaceTwoSegGetDispatch(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("sub") != "machines" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	user := ghUserFromContext(r.Context())
	cs := s.resolveCodespace(w, r, user.Login, "")
	if cs == nil {
		return
	}
	writeJSON(w, http.StatusOK, codespaceMachinesListJSON())
}

// handleUserCodespaceThreeSegGetDispatch fans GET
// /user/codespaces/{name}/{sub}/{export_id} out to exports/{export_id}.
func (s *Server) handleUserCodespaceThreeSegGetDispatch(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("sub") != "exports" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	user := ghUserFromContext(r.Context())
	cs := s.snapshotCodespace(s.resolveCodespace(w, r, user.Login, ""))
	if cs == nil {
		return
	}
	if cs.LatestExport == nil || r.PathValue("export_id") != cs.LatestExport.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.codespaceExportJSON(cs, cs.LatestExport, s.baseURL(r)))
}

// exports + publish

func (s *Server) codespaceExportJSON(live *store.Codespace, liveExport *store.CodespaceExport, baseURL string) map[string]interface{} {
	cs, export := s.snapshotCodespaceExport(live, liveExport)
	out := map[string]interface{}{
		"state":        export.State,
		"completed_at": export.CompletedAt.UTC().Format(time.RFC3339),
		"branch":       export.Branch,
		"sha":          export.SHA,
		"id":           export.ID,
		"export_url":   fmt.Sprintf("%s/api/v3/user/codespaces/%s/exports/%s", baseURL, cs.Name, export.ID),
	}
	if cs.RepoKey != "" {
		out["html_url"] = fmt.Sprintf("%s/%s/tree/%s", baseURL, cs.RepoKey, export.Branch)
	} else {
		out["html_url"] = nil
	}
	return out
}

func (s *Server) handleExportUserCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	cs := s.resolveCodespace(w, r, user.Login, "")
	if cs == nil {
		return
	}
	export, err := s.store.ExportCodespace(cs.ID)
	switch {
	case err == store.ErrCodespaceNoRepository:
		store.WriteGHValidationError(w, "CodespaceExport", "repository", "missing")
		return
	case err != nil:
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.codespaceExportJSON(cs, export, s.baseURL(r)))
}

func (s *Server) handlePublishUserCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	cs := s.resolveCodespace(w, r, user.Login, "")
	if cs == nil {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Private bool   `json:"private"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	published, err := s.store.PublishCodespace(cs.ID, user, req.Name, req.Private)
	switch err {
	case nil:
	case store.ErrCodespacePublished:
		store.WriteGHValidationError(w, "Codespace", "codespace", "already_exists")
		return
	case store.ErrRepoNameTaken:
		store.WriteGHValidationError(w, "Repository", "name", "already_exists")
		return
	default:
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	baseURL := s.baseURL(r)
	out := s.codespaceToJSON(published, baseURL)
	// codespace-with-full-repository embeds the full-repository shape.
	if repo := s.store.GetRepoByFullName(published.RepoKey); repo != nil {
		out["repository"] = fullRepoJSON(repo, s.store, baseURL)
	}
	writeJSONCreated(w, jsonStringField(out, "url"), out)
}

// pull-request codespaces

func (s *Server) handleCreatePullRequestCodespace(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(r.PathValue("pull_number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req codespaceCreateRequest
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	opts := store.CodespaceCreateOptions{
		MachineName:            req.Machine,
		DisplayName:            req.DisplayName,
		WorkingDirectory:       req.WorkingDirectory,
		DevcontainerPath:       req.DevcontainerPath,
		Geolocation:            req.Geolocation,
		IdleTimeoutMinutes:     req.IdleTimeoutMinutes,
		RetentionPeriodMinutes: req.RetentionPeriodMinutes,
	}
	cs, err := s.store.CreateCodespace(user.Login, repo.FullName, pr.HeadRefName, req.Location, opts)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "codespace create failed: "+err.Error())
		return
	}
	prJSON := s.codespaceToJSON(cs, s.baseURL(r))
	writeJSONCreated(w, jsonStringField(prJSON, "url"), prJSON)
}

// user-secret selected repositories

func (s *Server) handleListUserCodespaceSecretRepos(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	sec := s.getCodespaceSecret(r, "user", user.Login)
	if sec == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	baseURL := s.baseURL(r)
	repos := make([]map[string]interface{}, 0, len(sec.SelectedRepoIDs))
	for _, id := range sec.SelectedRepoIDs {
		if repo := s.store.GetRepoByID(id); repo != nil {
			repos = append(repos, minimalRepoJSON(repo, s.store, baseURL))
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"repositories": repos, "total_count": len(repos)})
}

func (s *Server) handleSetUserCodespaceSecretRepos(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	var req struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	for _, id := range req.SelectedRepositoryIDs {
		if s.store.GetRepoByID(id) == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	scope := store.CodespaceSecretScopeKey("user", user.Login)
	if !s.store.SetCodespaceSecretSelectedRepos(scope, r.PathValue("secret_name"), req.SelectedRepositoryIDs) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddUserCodespaceSecretRepo(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	repoID, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil || s.store.GetRepoByID(repoID) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	scope := store.CodespaceSecretScopeKey("user", user.Login)
	if !s.store.AddCodespaceSecretSelectedRepo(scope, r.PathValue("secret_name"), repoID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveUserCodespaceSecretRepo(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	repoID, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil || s.store.GetRepoByID(repoID) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	scope := store.CodespaceSecretScopeKey("user", user.Login)
	if !s.store.RemoveCodespaceSecretSelectedRepo(scope, r.PathValue("secret_name"), repoID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// organization-member codespace handlers

// resolveOrgMemberForCodespaces resolves {org}/{username}: the user must hold an
// active membership. The caller was already vetted as an org owner.
func (s *Server) resolveOrgMemberForCodespaces(w http.ResponseWriter, r *http.Request) (*store.Org, *store.User) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	member := s.store.LookupUserByLogin(r.PathValue("username"))
	if member == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	m := s.store.GetMembership(org.Login, member.ID)
	if m == nil || m.State != store.MembershipStateActive {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	return org, member
}

// handleListOrgMemberCodespaces lists the member's codespaces on the org's repos.
func (s *Server) handleListOrgMemberCodespaces(w http.ResponseWriter, r *http.Request) {
	org, member := s.resolveOrgMemberForCodespaces(w, r)
	if org == nil {
		return
	}
	prefix := org.Login + "/"
	base := s.baseURL(r)
	out := []map[string]interface{}{}
	for _, cs := range s.store.ListCodespacesByOwner(member.Login) {
		if !strings.HasPrefix(cs.RepoKey, prefix) {
			continue
		}
		out = append(out, s.codespaceToJSON(cs, base))
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{"codespaces": paged, "total_count": len(out)})
}

// resolveOrgMemberCodespace resolves {codespace_name} to a codespace the member
// owns on one of the org's repos.
func (s *Server) resolveOrgMemberCodespace(w http.ResponseWriter, r *http.Request, org *store.Org, member *store.User) *store.Codespace {
	cs := s.store.GetCodespaceByName(r.PathValue("codespace_name"))
	if cs == nil || cs.OwnerLogin != member.Login || !strings.HasPrefix(cs.RepoKey, org.Login+"/") {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return cs
}

func (s *Server) handleDeleteOrgMemberCodespace(w http.ResponseWriter, r *http.Request) {
	org, member := s.resolveOrgMemberForCodespaces(w, r)
	if org == nil {
		return
	}
	cs := s.resolveOrgMemberCodespace(w, r, org, member)
	if cs == nil {
		return
	}
	ok, err := s.store.DeleteCodespace(cs.ID)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "codespace delete failed: "+err.Error())
		return
	}
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleStopOrgMemberCodespace answers 200 with the codespace, unlike the
// user-scoped stop (202).
func (s *Server) handleStopOrgMemberCodespace(w http.ResponseWriter, r *http.Request) {
	org, member := s.resolveOrgMemberForCodespaces(w, r)
	if org == nil {
		return
	}
	cs := s.resolveOrgMemberCodespace(w, r, org, member)
	if cs == nil {
		return
	}
	if err := s.stopCodespace(cs); err != nil {
		writeGHError(w, http.StatusInternalServerError, "codespace stop failed: "+err.Error())
		return
	}
	cs = s.store.GetCodespace(cs.ID)
	writeJSON(w, http.StatusOK, s.codespaceToJSON(cs, s.baseURL(r)))
}

// secrets handlers

func (s *Server) handleListUserCodespaceSecrets(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	scope := store.CodespaceSecretScopeKey("user", user.Login)
	secs := s.snapshotCodespaceSecrets(s.store.ListCodespaceSecrets(scope))
	paged := paginateAndLink(w, r, secs)
	writeJSON(w, http.StatusOK, codespaceSecretsListJSON(paged, len(secs), s.baseURL(r)))
}

func (s *Server) handleGetUserCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	sec := s.getCodespaceSecret(r, "user", user.Login)
	if sec == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, codespaceUserSecretJSON(sec, s.baseURL(r)))
}

func (s *Server) handlePutUserCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	name := r.PathValue("secret_name")
	value, ok := s.readSealedCodespaceSecret(w, r)
	if !ok {
		return
	}
	created := s.store.GetCodespaceSecret(store.CodespaceSecretScopeKey("user", user.Login), name) == nil
	s.store.CreateCodespaceSecret(store.CodespaceSecretScopeKey("user", user.Login), name, value, "", nil)
	writeSecretUpsert(w, created)
}

func (s *Server) handleDeleteUserCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if !s.store.DeleteCodespaceSecret(store.CodespaceSecretScopeKey("user", user.Login), r.PathValue("secret_name")) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListRepoCodespaceSecrets(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	scope := store.CodespaceSecretScopeKey("repo", repo.FullName)
	secs := s.snapshotCodespaceSecrets(s.store.ListCodespaceSecrets(scope))
	out := make([]map[string]interface{}, len(secs))
	for i, sec := range secs {
		out[i] = codespaceRepoSecretJSON(sec)
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{"secrets": paged, "total_count": len(out)})
}

func (s *Server) handleGetRepoCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	sec := s.getCodespaceSecret(r, "repo", repo.FullName)
	if sec == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, codespaceRepoSecretJSON(sec))
}

func (s *Server) handlePutRepoCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	name := r.PathValue("secret_name")
	value, ok := s.readSealedCodespaceSecret(w, r)
	if !ok {
		return
	}
	created := s.store.GetCodespaceSecret(store.CodespaceSecretScopeKey("repo", repo.FullName), name) == nil
	s.store.CreateCodespaceSecret(store.CodespaceSecretScopeKey("repo", repo.FullName), name, value, "", nil)
	writeSecretUpsert(w, created)
}

func (s *Server) handleDeleteRepoCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeleteCodespaceSecret(store.CodespaceSecretScopeKey("repo", repo.FullName), r.PathValue("secret_name")) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgCodespaceSecrets(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	scope := store.CodespaceSecretScopeKey("org", org)
	secs := s.snapshotCodespaceSecrets(s.store.ListCodespaceSecrets(scope))
	out := make([]map[string]interface{}, len(secs))
	for i, sec := range secs {
		out[i] = codespaceOrgSecretJSON(sec, org, s.baseURL(r))
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{"secrets": paged, "total_count": len(out)})
}

func (s *Server) handleGetOrgCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	sec := s.getCodespaceSecret(r, "org", r.PathValue("org"))
	if sec == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, codespaceOrgSecretJSON(sec, r.PathValue("org"), s.baseURL(r)))
}

func (s *Server) handlePutOrgCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	name := r.PathValue("secret_name")
	var req struct {
		EncryptedValue        string `json:"encrypted_value"`
		KeyID                 string `json:"key_id"`
		Visibility            string `json:"visibility"`
		SelectedRepositoryIDs []int  `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	plain, ok := s.decryptSealedSecret(w, req.EncryptedValue, req.KeyID)
	if !ok {
		return
	}
	if req.Visibility == "" {
		if len(req.SelectedRepositoryIDs) > 0 {
			req.Visibility = "selected"
		} else {
			req.Visibility = "all"
		}
	}
	created := s.store.GetCodespaceSecret(store.CodespaceSecretScopeKey("org", org), name) == nil
	s.store.CreateCodespaceSecret(store.CodespaceSecretScopeKey("org", org), name, plain, req.Visibility, req.SelectedRepositoryIDs)
	writeSecretUpsert(w, created)
}

func (s *Server) handleDeleteOrgCodespaceSecret(w http.ResponseWriter, r *http.Request) {
	if !s.store.DeleteCodespaceSecret(store.CodespaceSecretScopeKey("org", r.PathValue("org")), r.PathValue("secret_name")) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgCodespaceSecretRepos(w http.ResponseWriter, r *http.Request) {
	sec := s.getCodespaceSecret(r, "org", r.PathValue("org"))
	if sec == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repos := make([]map[string]interface{}, 0, len(sec.SelectedRepoIDs))
	for _, id := range sec.SelectedRepoIDs {
		if repo := s.store.GetRepoByID(id); repo != nil {
			repos = append(repos, store.RepoToJSON(repo, s.store, s.baseURL(r)))
		}
	}
	paged := paginateAndLink(w, r, repos)
	writeJSON(w, http.StatusOK, map[string]interface{}{"repositories": paged, "total_count": len(repos)})
}

func (s *Server) handleSetOrgCodespaceSecretRepos(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	name := r.PathValue("secret_name")
	var req struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !s.store.SetCodespaceSecretSelectedRepos(store.CodespaceSecretScopeKey("org", org), name, req.SelectedRepositoryIDs) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetCodespacePublicKey(w http.ResponseWriter, r *http.Request) {
	s.writeActionsPublicKey(w)
}

// getCodespaceSecret resolves {secret_name} as a snapshot so a concurrent
// selection change is not rendered half-applied.
func (s *Server) getCodespaceSecret(r *http.Request, kind, key string) *store.CodespaceSecret {
	return s.snapshotCodespaceSecret(s.store.GetCodespaceSecret(store.CodespaceSecretScopeKey(kind, key), r.PathValue("secret_name")))
}

func (s *Server) readSealedCodespaceSecret(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req sealedSecretBody
	if !decodeJSONBody(w, r, &req) {
		return "", false
	}
	return s.decryptSealedSecret(w, req.EncryptedValue, req.KeyID)
}

// lifecycle helpers

func (s *Server) startCodespace(cs *store.Codespace) error {
	if cs.ContainerID == "" {
		if cs.Runtime == "workspace" {
			s.store.SetCodespaceState(cs.ID, "Available", true)
			return nil
		}
		return fmt.Errorf("no container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.CodespaceDockerLifecycleTimeout)
	defer cancel()
	if err := store.DockerStartContainer(ctx, cs.ContainerID); err != nil {
		return err
	}
	s.store.SetCodespaceState(cs.ID, store.DockerStateToCodespaceState(cs.ContainerID), true)
	return nil
}

func (s *Server) stopCodespace(cs *store.Codespace) error {
	if cs.ContainerID == "" {
		if cs.Runtime == "workspace" {
			s.store.SetCodespaceState(cs.ID, "Shutdown", false)
			return nil
		}
		return fmt.Errorf("no container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.CodespaceDockerLifecycleTimeout)
	defer cancel()
	if err := store.DockerStopContainer(ctx, cs.ContainerID); err != nil {
		return err
	}
	s.store.SetCodespaceState(cs.ID, store.DockerStateToCodespaceState(cs.ContainerID), false)
	return nil
}

// request/response shapes

type codespaceCreateRequest struct {
	RepositoryID           int                   `json:"repository_id"`
	PullRequest            *codespacePullRequest `json:"pull_request"`
	Ref                    string                `json:"ref"`
	Machine                string                `json:"machine"`
	DisplayName            string                `json:"display_name"`
	Location               string                `json:"location"`
	WorkingDirectory       string                `json:"working_directory"`
	DevcontainerPath       string                `json:"devcontainer_path"`
	Geolocation            string                `json:"geolocation"`
	IdleTimeoutMinutes     int                   `json:"idle_timeout_minutes"`
	RetentionPeriodMinutes int                   `json:"retention_period_minutes"`
}

// validateCodespaceCreate applies shared create checks, writing the error and
// returning false when rejected.
func validateCodespaceCreate(w http.ResponseWriter, req codespaceCreateRequest) bool {
	if req.Machine != "" && !store.CodespaceMachineExists(req.Machine) {
		store.WriteGHValidationError(w, "Codespace", "machine", "invalid")
		return false
	}
	return true
}

type codespacePullRequest struct {
	PullRequestNumber int `json:"pull_request_number"`
	RepositoryID      int `json:"repository_id"`
}

type codespacePatchRequest struct {
	DisplayName            string `json:"display_name"`
	Machine                string `json:"machine"`
	RetentionPeriodMinutes int    `json:"retention_period_minutes"`
}

// snapshotCodespace copies a stored codespace under the store lock; lifecycle
// transitions mutate it concurrently, and a serializer walking the live record
// could publish a half-applied one.
func (s *Server) snapshotCodespace(cs *store.Codespace) *store.Codespace {
	if cs == nil {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return store.CloneCodespace(cs)
}

// snapshotCodespaceExport copies a codespace and one export under a single lock
// acquisition, so the rendered pair is one consistent instant.
func (s *Server) snapshotCodespaceExport(cs *store.Codespace, export *store.CodespaceExport) (*store.Codespace, *store.CodespaceExport) {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return store.CloneCodespace(cs), store.ClonePointer(export)
}

// snapshotCodespaceSecret copies a secret under the store lock, including the
// selected-repository list a concurrent write replaces.
func (s *Server) snapshotCodespaceSecret(sec *store.CodespaceSecret) *store.CodespaceSecret {
	if sec == nil {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	view := *sec
	view.SelectedRepoIDs = append([]int(nil), sec.SelectedRepoIDs...)
	return &view
}

func (s *Server) snapshotCodespaceSecrets(secs []*store.CodespaceSecret) []*store.CodespaceSecret {
	out := make([]*store.CodespaceSecret, len(secs))
	for i, sec := range secs {
		out[i] = s.snapshotCodespaceSecret(sec)
	}
	return out
}

func (s *Server) codespaceToJSON(live *store.Codespace, baseURL string) map[string]interface{} {
	cs := s.snapshotCodespace(live)
	owner := s.store.LookupUserByLogin(cs.OwnerLogin)
	ownerJSON := map[string]interface{}(nil)
	if owner != nil {
		ownerJSON = store.UserToJSON(owner, baseURL)
	}
	var repoJSON map[string]interface{}
	if cs.RepoKey != "" {
		if owner, repoName, ok := store.SplitRepoFullName(cs.RepoKey); ok {
			if repo := s.store.GetRepo(owner, repoName); repo != nil {
				repoJSON = store.RepoToJSON(repo, s.store, baseURL)
			}
		}
	}

	url := fmt.Sprintf("%s/api/v3/user/codespaces/%s", baseURL, cs.Name)
	return map[string]interface{}{
		"id":                       cs.ID,
		"name":                     cs.Name,
		"display_name":             cs.DisplayName,
		"environment_id":           fmt.Sprintf("%d", cs.ID),
		"owner":                    ownerJSON,
		"billable_owner":           ownerJSON,
		"repository":               repoJSON,
		"machine":                  codespaceMachineJSON(store.CodespaceMachineByName(cs.MachineName)),
		"created_at":               cs.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":               cs.UpdatedAt.UTC().Format(time.RFC3339),
		"last_used_at":             cs.LastUsedAt.UTC().Format(time.RFC3339),
		"state":                    cs.State,
		"url":                      url,
		"web_url":                  fmt.Sprintf("%s/ui/codespaces/%s", baseURL, cs.Name),
		"git_status":               map[string]interface{}{"ahead": 0, "behind": 0, "has_uncommitted_changes": false, "ref": cs.GitRef},
		"devcontainer_path":        cs.DevcontainerPath,
		"retention_period_minutes": cs.RetentionPeriodMinutes,
		"idle_timeout_minutes":     cs.IdleTimeoutMinutes,
		// A record without a location (pre-fix row) still reports a valid region (PAR-010).
		"location":       store.CoalesceStr(cs.Location, "EastUs"),
		"machines_url":   url + "/machines",
		"prebuild":       false,
		"pulls_url":      url + "/pulls",
		"recent_folders": []string{},
		"start_url":      url + "/start",
		"stop_url":       url + "/stop",
	}
}

func codespaceUserSecretJSON(sec *store.CodespaceSecret, baseURL string) map[string]interface{} {
	visibility := sec.Visibility
	if visibility == "" {
		visibility = "all"
	}
	return map[string]interface{}{
		"name":                      sec.Name,
		"created_at":                sec.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":                sec.UpdatedAt.UTC().Format(time.RFC3339),
		"visibility":                visibility,
		"selected_repositories_url": baseURL + "/api/v3/user/codespaces/secrets/" + sec.Name + "/repositories",
	}
}

func codespaceRepoSecretJSON(sec *store.CodespaceSecret) map[string]interface{} {
	return map[string]interface{}{
		"name":       sec.Name,
		"created_at": sec.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": sec.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func codespaceOrgSecretJSON(sec *store.CodespaceSecret, orgLogin, baseURL string) map[string]interface{} {
	out := codespaceUserSecretJSON(sec, baseURL)
	out["selected_repositories_url"] = baseURL + "/api/v3/orgs/" + orgLogin + "/codespaces/secrets/" + sec.Name + "/repositories"
	return out
}

func codespaceSecretsListJSON(secs []*store.CodespaceSecret, total int, baseURL string) map[string]interface{} {
	out := make([]map[string]interface{}, len(secs))
	for i, sec := range secs {
		out[i] = codespaceUserSecretJSON(sec, baseURL)
	}
	return map[string]interface{}{"secrets": out, "total_count": total}
}

// ─── organization codespaces + access controls ───────────────────────────

var orgCodespacesAccessVisibilities = map[string]bool{
	"disabled":                              true,
	"selected_members":                      true,
	"all_members":                           true,
	"all_members_and_outside_collaborators": true,
}

func (s *Server) handleListOrgCodespaces(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Gather under the read lock; render outside it — codespaceToJSON takes
	// store locks itself.
	s.store.Mu.RLock()
	memberLogins := map[string]bool{}
	for _, m := range s.store.Memberships {
		if m.OrgID == org.ID && m.State == store.MembershipStateActive {
			if u := s.store.Users[m.UserID]; u != nil {
				memberLogins[u.Login] = true
			}
		}
	}
	var list []*store.Codespace
	for _, cs := range s.store.Codespaces {
		if memberLogins[cs.OwnerLogin] {
			list = append(list, cs)
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	base := s.baseURL(r)
	out := make([]map[string]interface{}, len(list))
	for i, cs := range list {
		out[i] = s.codespaceToJSON(cs, base)
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{"codespaces": paged, "total_count": len(out)})
}

func (s *Server) handleSetOrgCodespacesAccess(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Visibility        string   `json:"visibility"`
		SelectedUsernames []string `json:"selected_usernames"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !orgCodespacesAccessVisibilities[req.Visibility] {
		store.WriteGHValidationError(w, "OrgCodespacesAccess", "visibility", "invalid")
		return
	}
	if len(req.SelectedUsernames) > 100 {
		store.WriteGHValidationError(w, "OrgCodespacesAccess", "selected_usernames", "invalid")
		return
	}
	if len(req.SelectedUsernames) > 0 && req.Visibility != "selected_members" {
		store.WriteGHValidationError(w, "OrgCodespacesAccess", "selected_usernames", "invalid")
		return
	}
	if invalid := s.store.OrgCodespacesInvalidUsers(org, req.SelectedUsernames); len(invalid) > 0 {
		writeGHError(w, http.StatusBadRequest, "Users are neither members nor collaborators of this organization.")
		return
	}
	s.store.SetOrgCodespacesAccess(org.Login, req.Visibility, req.SelectedUsernames)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) modifyOrgCodespacesAccessUsers(w http.ResponseWriter, r *http.Request, add bool) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		SelectedUsernames []string `json:"selected_usernames"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.SelectedUsernames) == 0 || len(req.SelectedUsernames) > 100 {
		store.WriteGHValidationError(w, "OrgCodespacesAccess", "selected_usernames", "invalid")
		return
	}
	if invalid := s.store.OrgCodespacesInvalidUsers(org, req.SelectedUsernames); len(invalid) > 0 {
		writeGHError(w, http.StatusBadRequest, "Users are neither members nor collaborators of this organization.")
		return
	}
	if !s.store.ModifyOrgCodespacesAccessUsers(org.Login, add, req.SelectedUsernames) {
		store.WriteGHValidationError(w, "OrgCodespacesAccess", "visibility", "invalid")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddOrgCodespacesAccessUsers(w http.ResponseWriter, r *http.Request) {
	s.modifyOrgCodespacesAccessUsers(w, r, true)
}

func (s *Server) handleRemoveOrgCodespacesAccessUsers(w http.ResponseWriter, r *http.Request) {
	s.modifyOrgCodespacesAccessUsers(w, r, false)
}

// ─── org codespaces secret selected-repository add/remove ────────────────

// orgCodespaceSecretSelectionChange adapts the shared per-repo selection core to
// org codespace secrets.
func (s *Server) orgCodespaceSecretSelectionChange(w http.ResponseWriter, r *http.Request, add bool) {
	scope := store.CodespaceSecretScopeKey("org", r.PathValue("org"))
	name := r.PathValue("secret_name")
	s.handleOrgSelectionChange(w, r, name, add,
		func() store.OrgScopedItem {
			if sec := s.store.CodespaceSecrets[scope][name]; sec != nil {
				return sec
			}
			return nil
		},
		func() { s.store.PersistCodespaceSecretScopeLocked(scope) })
}

func (s *Server) handleAddOrgCodespaceSecretRepo(w http.ResponseWriter, r *http.Request) {
	s.orgCodespaceSecretSelectionChange(w, r, true)
}

func (s *Server) handleRemoveOrgCodespaceSecretRepo(w http.ResponseWriter, r *http.Request) {
	s.orgCodespaceSecretSelectionChange(w, r, false)
}
