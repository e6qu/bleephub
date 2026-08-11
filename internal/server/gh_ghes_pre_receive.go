package bleephub

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerGHESPreReceiveRoutes(admin func(http.HandlerFunc) http.HandlerFunc) {
	s.route("GET /api/v3/admin/pre-receive-environments", admin(s.handleListGHESPreReceiveEnvironments))
	s.route("POST /api/v3/admin/pre-receive-environments", admin(s.handleCreateGHESPreReceiveEnvironment))
	s.route("GET /api/v3/admin/pre-receive-environments/{pre_receive_environment_id}", admin(s.handleGetGHESPreReceiveEnvironment))
	s.route("PATCH /api/v3/admin/pre-receive-environments/{pre_receive_environment_id}", admin(s.handleUpdateGHESPreReceiveEnvironment))
	s.route("DELETE /api/v3/admin/pre-receive-environments/{pre_receive_environment_id}", admin(s.handleDeleteGHESPreReceiveEnvironment))
	s.route("POST /api/v3/admin/pre-receive-environments/{pre_receive_environment_id}/downloads", admin(s.handleStartGHESPreReceiveEnvironmentDownload))
	s.route("GET /api/v3/admin/pre-receive-environments/{pre_receive_environment_id}/downloads/latest", admin(s.handleGetGHESPreReceiveEnvironmentDownload))
	s.route("GET /api/v3/admin/pre-receive-hooks", admin(s.handleListGHESPreReceiveHooks))
	s.route("POST /api/v3/admin/pre-receive-hooks", admin(s.handleCreateGHESPreReceiveHook))
	s.route("GET /api/v3/admin/pre-receive-hooks/{pre_receive_hook_id}", admin(s.handleGetGHESPreReceiveHook))
	s.route("PATCH /api/v3/admin/pre-receive-hooks/{pre_receive_hook_id}", admin(s.handleUpdateGHESPreReceiveHook))
	s.route("DELETE /api/v3/admin/pre-receive-hooks/{pre_receive_hook_id}", admin(s.handleDeleteGHESPreReceiveHook))

	s.route("GET /api/v3/orgs/{org}/pre-receive-hooks", s.handleListGHESOrgPreReceiveHooks)
	s.route("GET /api/v3/orgs/{org}/pre-receive-hooks/{pre_receive_hook_id}", s.handleGetGHESOrgPreReceiveHook)
	s.route("PATCH /api/v3/orgs/{org}/pre-receive-hooks/{pre_receive_hook_id}", s.handleUpdateGHESOrgPreReceiveHook)
	s.route("DELETE /api/v3/orgs/{org}/pre-receive-hooks/{pre_receive_hook_id}", s.handleDeleteGHESOrgPreReceiveHook)
	s.route("GET /api/v3/repos/{owner}/{repo}/pre-receive-hooks", s.handleListGHESRepoPreReceiveHooks)
	s.route("GET /api/v3/repos/{owner}/{repo}/pre-receive-hooks/{pre_receive_hook_id}", s.handleGetGHESRepoPreReceiveHook)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/pre-receive-hooks/{pre_receive_hook_id}", s.handleUpdateGHESRepoPreReceiveHook)
	s.route("DELETE /api/v3/repos/{owner}/{repo}/pre-receive-hooks/{pre_receive_hook_id}", s.handleDeleteGHESRepoPreReceiveHook)
}

func preReceiveID(r *http.Request, name string) (int, bool) {
	id, err := strconv.Atoi(r.PathValue(name))
	return id, err == nil && id > 0
}

func validPreReceiveEnforcement(value string) bool {
	return value == "enabled" || value == "disabled" || value == "testing"
}

func (s *Server) preReceiveEnvironmentJSON(env *GHESPreReceiveEnvironment, r *http.Request, hooksCount int) map[string]interface{} {
	base := s.baseURL(r) + "/api/v3/admin/pre-receive-environments/" + strconv.Itoa(env.ID)
	return map[string]interface{}{
		"id": env.ID, "name": env.Name, "image_url": env.ImageURL,
		"url": base, "html_url": s.baseURL(r) + "/admin/pre-receive-environments/" + strconv.Itoa(env.ID),
		"default_environment": env.DefaultEnvironment,
		"created_at":          env.CreatedAt.UTC().Format(time.RFC3339),
		"hooks_count":         hooksCount,
		"download":            s.preReceiveDownloadJSON(env, r),
	}
}

func (s *Server) preReceiveDownloadJSON(env *GHESPreReceiveEnvironment, r *http.Request) map[string]interface{} {
	state := "not_started"
	var downloadedAt interface{}
	var message interface{}
	if env.Download != nil {
		state = env.Download.State
		if env.Download.DownloadedAt != nil {
			downloadedAt = env.Download.DownloadedAt.UTC().Format(time.RFC3339)
		}
		if env.Download.Message != nil {
			message = *env.Download.Message
		}
	}
	return map[string]interface{}{
		"url": s.baseURL(r) + "/api/v3/admin/pre-receive-environments/" +
			strconv.Itoa(env.ID) + "/downloads/latest",
		"state": state, "downloaded_at": downloadedAt, "message": message,
	}
}

func (s *Server) snapshotPreReceiveEnvironment(id int) (*GHESPreReceiveEnvironment, int) {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	env := s.store.EnterpriseSettings.GHESPreReceiveEnvironments[id]
	if env == nil {
		return nil, 0
	}
	copy := *env
	if env.Download != nil {
		download := *env.Download
		copy.Download = &download
	}
	count := 0
	for _, hook := range s.store.EnterpriseSettings.GHESPreReceiveHooks {
		if hook.EnvironmentID == id {
			count++
		}
	}
	return &copy, count
}

func (s *Server) requirePreReceiveEnvironment(w http.ResponseWriter, r *http.Request) (*GHESPreReceiveEnvironment, int) {
	id, ok := preReceiveID(r, "pre_receive_environment_id")
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, 0
	}
	env, count := s.snapshotPreReceiveEnvironment(id)
	if env == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
	return env, count
}

func (s *Server) handleListGHESPreReceiveEnvironments(w http.ResponseWriter, r *http.Request) {
	s.store.Mu.RLock()
	ids := make([]int, 0, len(s.store.EnterpriseSettings.GHESPreReceiveEnvironments))
	for id := range s.store.EnterpriseSettings.GHESPreReceiveEnvironments {
		ids = append(ids, id)
	}
	s.store.Mu.RUnlock()
	sort.Ints(ids)
	out := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		env, count := s.snapshotPreReceiveEnvironment(id)
		out = append(out, s.preReceiveEnvironmentJSON(env, r, count))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleCreateGHESPreReceiveEnvironment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		ImageURL string `json:"image_url"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.ImageURL) == "" {
		writeGHValidationError(w, "PreReceiveEnvironment", "name", "missing_field")
		return
	}
	s.store.Mu.Lock()
	for _, env := range s.store.EnterpriseSettings.GHESPreReceiveEnvironments {
		if strings.EqualFold(env.Name, req.Name) {
			s.store.Mu.Unlock()
			writeGHValidationError(w, "PreReceiveEnvironment", "name", "already_exists")
			return
		}
	}
	env := &GHESPreReceiveEnvironment{
		ID:   s.store.EnterpriseSettings.NextGHESPreReceiveEnvironmentID,
		Name: req.Name, ImageURL: req.ImageURL, CreatedAt: s.currentTime(),
		Download: &GHESPreReceiveDownload{State: "not_started"},
	}
	s.store.EnterpriseSettings.NextGHESPreReceiveEnvironmentID++
	s.store.EnterpriseSettings.GHESPreReceiveEnvironments[env.ID] = env
	s.store.PersistEnterpriseSettings()
	copy := *env
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusCreated, s.preReceiveEnvironmentJSON(&copy, r, 0))
}

func (s *Server) handleGetGHESPreReceiveEnvironment(w http.ResponseWriter, r *http.Request) {
	env, count := s.requirePreReceiveEnvironment(w, r)
	if env != nil {
		writeJSON(w, http.StatusOK, s.preReceiveEnvironmentJSON(env, r, count))
	}
}

func (s *Server) handleUpdateGHESPreReceiveEnvironment(w http.ResponseWriter, r *http.Request) {
	env, _ := s.requirePreReceiveEnvironment(w, r)
	if env == nil {
		return
	}
	var req struct {
		Name     *string `json:"name"`
		ImageURL *string `json:"image_url"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeGHValidationError(w, "PreReceiveEnvironment", "name", "invalid")
		return
	}
	s.store.Mu.Lock()
	stored := s.store.EnterpriseSettings.GHESPreReceiveEnvironments[env.ID]
	if req.Name != nil {
		for id, candidate := range s.store.EnterpriseSettings.GHESPreReceiveEnvironments {
			if id != env.ID && strings.EqualFold(candidate.Name, *req.Name) {
				s.store.Mu.Unlock()
				writeGHValidationError(w, "PreReceiveEnvironment", "name", "already_exists")
				return
			}
		}
		stored.Name = *req.Name
	}
	if req.ImageURL != nil {
		stored.ImageURL = *req.ImageURL
		stored.Download = &GHESPreReceiveDownload{State: "not_started"}
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	updated, count := s.snapshotPreReceiveEnvironment(env.ID)
	writeJSON(w, http.StatusOK, s.preReceiveEnvironmentJSON(updated, r, count))
}

func (s *Server) handleDeleteGHESPreReceiveEnvironment(w http.ResponseWriter, r *http.Request) {
	env, count := s.requirePreReceiveEnvironment(w, r)
	if env == nil {
		return
	}
	if count > 0 || env.DefaultEnvironment {
		writeGHValidationError(w, "PreReceiveEnvironment", "id", "unprocessable")
		return
	}
	s.store.Mu.Lock()
	delete(s.store.EnterpriseSettings.GHESPreReceiveEnvironments, env.ID)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStartGHESPreReceiveEnvironmentDownload(w http.ResponseWriter, r *http.Request) {
	env, _ := s.requirePreReceiveEnvironment(w, r)
	if env == nil {
		return
	}
	now := s.currentTime()
	s.store.Mu.Lock()
	stored := s.store.EnterpriseSettings.GHESPreReceiveEnvironments[env.ID]
	stored.Download = &GHESPreReceiveDownload{State: "success", DownloadedAt: &now}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	updated, _ := s.snapshotPreReceiveEnvironment(env.ID)
	writeJSON(w, http.StatusAccepted, s.preReceiveDownloadJSON(updated, r))
}

func (s *Server) handleGetGHESPreReceiveEnvironmentDownload(w http.ResponseWriter, r *http.Request) {
	env, _ := s.requirePreReceiveEnvironment(w, r)
	if env != nil {
		writeJSON(w, http.StatusOK, s.preReceiveDownloadJSON(env, r))
	}
}

type preReceiveHookRequest struct {
	Name                         *string                `json:"name"`
	Script                       *string                `json:"script"`
	ScriptRepository             map[string]interface{} `json:"script_repository"`
	Environment                  map[string]interface{} `json:"environment"`
	Enforcement                  *string                `json:"enforcement"`
	AllowDownstreamConfiguration *bool                  `json:"allow_downstream_configuration"`
}

func numberMember(values map[string]interface{}, name string) int {
	switch value := values[name].(type) {
	case float64:
		return int(value)
	case json.Number:
		id, _ := strconv.Atoi(value.String())
		return id
	case int:
		return value
	}
	return 0
}

func (s *Server) resolvePreReceiveScriptRepo(values map[string]interface{}) *Repo {
	if id := numberMember(values, "id"); id > 0 {
		return s.store.GetRepoByID(id)
	}
	fullName, _ := values["full_name"].(string)
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return nil
	}
	return s.store.GetRepo(owner, name)
}

func (s *Server) preReceiveHookJSON(hook *GHESPreReceiveHook, r *http.Request) map[string]interface{} {
	repo := s.store.GetRepoByID(hook.ScriptRepositoryID)
	env, count := s.snapshotPreReceiveEnvironment(hook.EnvironmentID)
	var repoJSON interface{}
	if repo != nil {
		repoJSON = map[string]interface{}{
			"id": repo.ID, "full_name": repo.FullName,
			"url":      s.baseURL(r) + "/api/v3/repos/" + repo.FullName,
			"html_url": s.baseURL(r) + "/" + repo.FullName,
		}
	}
	var envJSON interface{}
	if env != nil {
		envJSON = s.preReceiveEnvironmentJSON(env, r, count)
	}
	return map[string]interface{}{
		"id": hook.ID, "name": hook.Name, "enforcement": hook.Enforcement,
		"script": hook.Script, "script_repository": repoJSON, "environment": envJSON,
		"allow_downstream_configuration": hook.AllowDownstreamConfiguration,
	}
}

func (s *Server) snapshotPreReceiveHook(id int) *GHESPreReceiveHook {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	hook := s.store.EnterpriseSettings.GHESPreReceiveHooks[id]
	if hook == nil {
		return nil
	}
	copy := *hook
	return &copy
}

func (s *Server) requirePreReceiveHook(w http.ResponseWriter, r *http.Request) *GHESPreReceiveHook {
	id, ok := preReceiveID(r, "pre_receive_hook_id")
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	hook := s.snapshotPreReceiveHook(id)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
	return hook
}

func (s *Server) handleListGHESPreReceiveHooks(w http.ResponseWriter, r *http.Request) {
	s.store.Mu.RLock()
	ids := make([]int, 0, len(s.store.EnterpriseSettings.GHESPreReceiveHooks))
	for id := range s.store.EnterpriseSettings.GHESPreReceiveHooks {
		ids = append(ids, id)
	}
	s.store.Mu.RUnlock()
	sort.Ints(ids)
	out := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.preReceiveHookJSON(s.snapshotPreReceiveHook(id), r))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleCreateGHESPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	var req preReceiveHookRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" || req.Script == nil || *req.Script == "" {
		writeGHValidationError(w, "PreReceiveHook", "name", "missing_field")
		return
	}
	repo := s.resolvePreReceiveScriptRepo(req.ScriptRepository)
	envID := numberMember(req.Environment, "id")
	env, _ := s.snapshotPreReceiveEnvironment(envID)
	if repo == nil || env == nil {
		writeGHValidationError(w, "PreReceiveHook", "script_repository", "invalid")
		return
	}
	enforcement := "disabled"
	if req.Enforcement != nil {
		enforcement = *req.Enforcement
	}
	if !validPreReceiveEnforcement(enforcement) {
		writeGHValidationError(w, "PreReceiveHook", "enforcement", "invalid")
		return
	}
	allow := false
	if req.AllowDownstreamConfiguration != nil {
		allow = *req.AllowDownstreamConfiguration
	}
	s.store.Mu.Lock()
	hook := &GHESPreReceiveHook{
		ID: s.store.EnterpriseSettings.NextGHESPreReceiveHookID, Name: *req.Name,
		Script: *req.Script, ScriptRepositoryID: repo.ID, EnvironmentID: env.ID,
		Enforcement: enforcement, AllowDownstreamConfiguration: allow,
	}
	s.store.EnterpriseSettings.NextGHESPreReceiveHookID++
	s.store.EnterpriseSettings.GHESPreReceiveHooks[hook.ID] = hook
	s.store.PersistEnterpriseSettings()
	copy := *hook
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusCreated, s.preReceiveHookJSON(&copy, r))
}

func (s *Server) handleGetGHESPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	hook := s.requirePreReceiveHook(w, r)
	if hook != nil {
		writeJSON(w, http.StatusOK, s.preReceiveHookJSON(hook, r))
	}
}

func (s *Server) handleUpdateGHESPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	hook := s.requirePreReceiveHook(w, r)
	if hook == nil {
		return
	}
	var req preReceiveHookRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Enforcement != nil && !validPreReceiveEnforcement(*req.Enforcement) {
		writeGHValidationError(w, "PreReceiveHook", "enforcement", "invalid")
		return
	}
	var repo *Repo
	if req.ScriptRepository != nil {
		repo = s.resolvePreReceiveScriptRepo(req.ScriptRepository)
		if repo == nil {
			writeGHValidationError(w, "PreReceiveHook", "script_repository", "invalid")
			return
		}
	}
	envID := 0
	if req.Environment != nil {
		envID = numberMember(req.Environment, "id")
		if env, _ := s.snapshotPreReceiveEnvironment(envID); env == nil {
			writeGHValidationError(w, "PreReceiveHook", "environment", "invalid")
			return
		}
	}
	s.store.Mu.Lock()
	stored := s.store.EnterpriseSettings.GHESPreReceiveHooks[hook.ID]
	if req.Name != nil {
		stored.Name = *req.Name
	}
	if req.Script != nil {
		stored.Script = *req.Script
	}
	if repo != nil {
		stored.ScriptRepositoryID = repo.ID
	}
	if envID > 0 {
		stored.EnvironmentID = envID
	}
	if req.Enforcement != nil {
		stored.Enforcement = *req.Enforcement
	}
	if req.AllowDownstreamConfiguration != nil {
		stored.AllowDownstreamConfiguration = *req.AllowDownstreamConfiguration
	}
	s.store.PersistEnterpriseSettings()
	copy := *stored
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, s.preReceiveHookJSON(&copy, r))
}

func (s *Server) handleDeleteGHESPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	hook := s.requirePreReceiveHook(w, r)
	if hook == nil {
		return
	}
	s.store.Mu.Lock()
	delete(s.store.EnterpriseSettings.GHESPreReceiveHooks, hook.ID)
	for _, overrides := range s.store.EnterpriseSettings.GHESOrgPreReceiveOverrides {
		delete(overrides, hook.ID)
	}
	for _, overrides := range s.store.EnterpriseSettings.GHESRepoPreReceiveOverrides {
		delete(overrides, hook.ID)
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requirePreReceiveOrgAdmin(w http.ResponseWriter, r *http.Request) *Org {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil || !s.viewerCanAdminOrg(r.Context(), r.PathValue("org")) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return org
}

func (s *Server) requirePreReceiveRepoAdmin(w http.ResponseWriter, r *http.Request) *Repo {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil || !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

func (s *Server) effectiveOrgPreReceive(hook *GHESPreReceiveHook, org string) GHESPreReceiveOverride {
	result := GHESPreReceiveOverride{
		Enforcement: hook.Enforcement, AllowDownstreamConfiguration: hook.AllowDownstreamConfiguration,
	}
	s.store.Mu.RLock()
	if override := s.store.EnterpriseSettings.GHESOrgPreReceiveOverrides[org][hook.ID]; override != nil {
		result = *override
	}
	s.store.Mu.RUnlock()
	return result
}

func (s *Server) orgPreReceiveJSON(hook *GHESPreReceiveHook, org string, r *http.Request) map[string]interface{} {
	effective := s.effectiveOrgPreReceive(hook, org)
	return map[string]interface{}{
		"id": hook.ID, "name": hook.Name, "enforcement": effective.Enforcement,
		"configuration_url":              s.baseURL(r) + "/api/v3/orgs/" + org + "/pre-receive-hooks/" + strconv.Itoa(hook.ID),
		"allow_downstream_configuration": effective.AllowDownstreamConfiguration,
	}
}

func (s *Server) handleListGHESOrgPreReceiveHooks(w http.ResponseWriter, r *http.Request) {
	org := s.requirePreReceiveOrgAdmin(w, r)
	if org == nil {
		return
	}
	s.store.Mu.RLock()
	ids := make([]int, 0, len(s.store.EnterpriseSettings.GHESPreReceiveHooks))
	for id := range s.store.EnterpriseSettings.GHESPreReceiveHooks {
		ids = append(ids, id)
	}
	s.store.Mu.RUnlock()
	sort.Ints(ids)
	out := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.orgPreReceiveJSON(s.snapshotPreReceiveHook(id), org.Login, r))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleGetGHESOrgPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	org := s.requirePreReceiveOrgAdmin(w, r)
	if org == nil {
		return
	}
	hook := s.requirePreReceiveHook(w, r)
	if hook != nil {
		writeJSON(w, http.StatusOK, s.orgPreReceiveJSON(hook, org.Login, r))
	}
}

func (s *Server) handleUpdateGHESOrgPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	org := s.requirePreReceiveOrgAdmin(w, r)
	if org == nil {
		return
	}
	hook := s.requirePreReceiveHook(w, r)
	if hook == nil {
		return
	}
	if !hook.AllowDownstreamConfiguration {
		writeGHError(w, http.StatusUnprocessableEntity, "Downstream configuration is not allowed")
		return
	}
	var req struct {
		Enforcement                  string `json:"enforcement"`
		AllowDownstreamConfiguration *bool  `json:"allow_downstream_configuration"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validPreReceiveEnforcement(req.Enforcement) {
		writeGHValidationError(w, "PreReceiveHook", "enforcement", "invalid")
		return
	}
	allow := hook.AllowDownstreamConfiguration
	if req.AllowDownstreamConfiguration != nil {
		allow = *req.AllowDownstreamConfiguration
	}
	s.store.Mu.Lock()
	if s.store.EnterpriseSettings.GHESOrgPreReceiveOverrides[org.Login] == nil {
		s.store.EnterpriseSettings.GHESOrgPreReceiveOverrides[org.Login] = map[int]*GHESPreReceiveOverride{}
	}
	s.store.EnterpriseSettings.GHESOrgPreReceiveOverrides[org.Login][hook.ID] = &GHESPreReceiveOverride{
		Enforcement: req.Enforcement, AllowDownstreamConfiguration: allow,
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, s.orgPreReceiveJSON(hook, org.Login, r))
}

func (s *Server) handleDeleteGHESOrgPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	org := s.requirePreReceiveOrgAdmin(w, r)
	if org == nil {
		return
	}
	hook := s.requirePreReceiveHook(w, r)
	if hook == nil {
		return
	}
	s.store.Mu.Lock()
	delete(s.store.EnterpriseSettings.GHESOrgPreReceiveOverrides[org.Login], hook.ID)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, s.orgPreReceiveJSON(hook, org.Login, r))
}

func (s *Server) effectiveRepoPreReceive(hook *GHESPreReceiveHook, repo *Repo) GHESPreReceiveOverride {
	parent := GHESPreReceiveOverride{
		Enforcement: hook.Enforcement, AllowDownstreamConfiguration: hook.AllowDownstreamConfiguration,
	}
	if repo.OwnerType == "Organization" {
		orgLogin, _, _ := strings.Cut(repo.FullName, "/")
		parent = s.effectiveOrgPreReceive(hook, orgLogin)
	}
	s.store.Mu.RLock()
	if override := s.store.EnterpriseSettings.GHESRepoPreReceiveOverrides[repo.FullName][hook.ID]; override != nil {
		parent.Enforcement = override.Enforcement
	}
	s.store.Mu.RUnlock()
	return parent
}

func (s *Server) repoPreReceiveJSON(hook *GHESPreReceiveHook, repo *Repo, r *http.Request) map[string]interface{} {
	effective := s.effectiveRepoPreReceive(hook, repo)
	return map[string]interface{}{
		"id": hook.ID, "name": hook.Name, "enforcement": effective.Enforcement,
		"configuration_url": s.baseURL(r) + "/api/v3/repos/" + repo.FullName +
			"/pre-receive-hooks/" + strconv.Itoa(hook.ID),
	}
}

func (s *Server) handleListGHESRepoPreReceiveHooks(w http.ResponseWriter, r *http.Request) {
	repo := s.requirePreReceiveRepoAdmin(w, r)
	if repo == nil {
		return
	}
	s.store.Mu.RLock()
	ids := make([]int, 0, len(s.store.EnterpriseSettings.GHESPreReceiveHooks))
	for id := range s.store.EnterpriseSettings.GHESPreReceiveHooks {
		ids = append(ids, id)
	}
	s.store.Mu.RUnlock()
	sort.Ints(ids)
	out := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.repoPreReceiveJSON(s.snapshotPreReceiveHook(id), repo, r))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleGetGHESRepoPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	repo := s.requirePreReceiveRepoAdmin(w, r)
	if repo == nil {
		return
	}
	hook := s.requirePreReceiveHook(w, r)
	if hook != nil {
		writeJSON(w, http.StatusOK, s.repoPreReceiveJSON(hook, repo, r))
	}
}

func (s *Server) handleUpdateGHESRepoPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	repo := s.requirePreReceiveRepoAdmin(w, r)
	if repo == nil {
		return
	}
	hook := s.requirePreReceiveHook(w, r)
	if hook == nil {
		return
	}
	parent := GHESPreReceiveOverride{
		Enforcement: hook.Enforcement, AllowDownstreamConfiguration: hook.AllowDownstreamConfiguration,
	}
	if repo.OwnerType == "Organization" {
		orgLogin, _, _ := strings.Cut(repo.FullName, "/")
		parent = s.effectiveOrgPreReceive(hook, orgLogin)
	}
	if !parent.AllowDownstreamConfiguration {
		writeGHError(w, http.StatusUnprocessableEntity, "Downstream configuration is not allowed")
		return
	}
	var req struct {
		Enforcement string `json:"enforcement"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validPreReceiveEnforcement(req.Enforcement) {
		writeGHValidationError(w, "PreReceiveHook", "enforcement", "invalid")
		return
	}
	s.store.Mu.Lock()
	if s.store.EnterpriseSettings.GHESRepoPreReceiveOverrides[repo.FullName] == nil {
		s.store.EnterpriseSettings.GHESRepoPreReceiveOverrides[repo.FullName] = map[int]*GHESPreReceiveOverride{}
	}
	s.store.EnterpriseSettings.GHESRepoPreReceiveOverrides[repo.FullName][hook.ID] =
		&GHESPreReceiveOverride{Enforcement: req.Enforcement}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, s.repoPreReceiveJSON(hook, repo, r))
}

func (s *Server) handleDeleteGHESRepoPreReceiveHook(w http.ResponseWriter, r *http.Request) {
	repo := s.requirePreReceiveRepoAdmin(w, r)
	if repo == nil {
		return
	}
	hook := s.requirePreReceiveHook(w, r)
	if hook == nil {
		return
	}
	s.store.Mu.Lock()
	delete(s.store.EnterpriseSettings.GHESRepoPreReceiveOverrides[repo.FullName], hook.ID)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, s.repoPreReceiveJSON(hook, repo, r))
}
