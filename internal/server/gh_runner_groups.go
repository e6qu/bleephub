package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const defaultRunnerGroupID = 1

func (s *Server) registerRunnerGroupRoutes() {
	s.route("GET /api/v3/orgs/{org}/actions/runner-groups",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleListRunnerGroups)))
	s.route("POST /api/v3/orgs/{org}/actions/runner-groups",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleCreateRunnerGroup)))
	s.route("GET /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleGetRunnerGroup)))
	s.route("PATCH /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleUpdateRunnerGroup)))
	s.route("DELETE /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleDeleteRunnerGroup)))
	s.route("GET /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/runners",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleListGroupRunners)))
	s.route("PUT /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/runners",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleSetGroupRunners)))
	s.route("PUT /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/runners/{runner_id}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleAddGroupRunner)))
	s.route("DELETE /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/runners/{runner_id}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleRemoveGroupRunner)))
	s.route("GET /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/repositories",
		s.requirePerm(scopeAdministration, permRead, s.orgGated(s.handleListGroupRepos)))
	s.route("PUT /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/repositories",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleSetGroupRepos)))
	s.route("PUT /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/repositories/{repository_id}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleAddGroupRepo)))
	s.route("DELETE /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/repositories/{repository_id}",
		s.requirePerm(scopeAdministration, permWrite, s.orgGated(s.handleRemoveGroupRepo)))
}

// orgGated 404s requests for unknown orgs before the handler runs.
func (s *Server) orgGated(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.store.GetOrg(r.PathValue("org")) == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		h(w, r)
	}
}

// orgIDGated resolves an official numeric {org_id} path parameter and makes
// the organization login available to the existing policy handlers.
func (s *Server) orgIDGated(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("org_id"))
		if err != nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		org := s.store.GetOrgByID(id)
		if org == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		r.SetPathValue("org", org.Login)
		// requirePerm resolved its org target from an empty {org} (this handler
		// sets it only now), so its organization half was skipped. Re-check the
		// credential against the resolved org here — these are org-administration
		// settings — or any authenticated caller could rewrite another org's
		// Actions cache policy.
		if !s.viewerCanAdminOrg(r.Context(), org.Login) {
			writeGHError(w, http.StatusForbidden, "Must have admin access to the organization.")
			return
		}
		h(w, r)
	}
}

// ensureDefaultRunnerGroupLocked materializes the implicit Default group that
// every organization and enterprise has. Callers hold the store lock.
func (s *Server) ensureDefaultRunnerGroupLocked(target runnerScope) *RunnerGroup {
	for _, group := range s.store.RunnerGroups {
		if group.Default && runnerGroupMatchesTarget(group, target) {
			return group
		}
	}

	id := s.store.NextRunnerGroupID
	if len(s.store.RunnerGroups) == 0 {
		id = defaultRunnerGroupID
	}
	if id <= defaultRunnerGroupID && s.store.RunnerGroups[defaultRunnerGroupID] != nil {
		id = defaultRunnerGroupID + 1
	}
	s.store.NextRunnerGroupID = id + 1
	group := &RunnerGroup{
		ID:                       id,
		Name:                     "Default",
		Visibility:               "all",
		Default:                  true,
		AllowsPublicRepositories: true,
		Scope:                    target,
		CreatedAt:                s.store.CurrentTime(),
	}
	s.store.RunnerGroups[id] = group
	s.persistRunnerGroupLocked(group)
	return group
}

func runnerGroupMatchesTarget(group *RunnerGroup, target runnerScope) bool {
	switch {
	case target.Org != "":
		return strings.EqualFold(group.Scope.Org, target.Org)
	case target.Enterprise != "":
		return strings.EqualFold(group.Scope.Enterprise, target.Enterprise)
	default:
		return false
	}
}

func nonNilStrings(values []string) []string {
	return append([]string{}, values...)
}

func repoOwnedByOrg(repo *Repo, org string) bool {
	if repo == nil || repo.OwnerType != "Organization" {
		return false
	}
	owner, _, ok := strings.Cut(repo.FullName, "/")
	return ok && strings.EqualFold(owner, org)
}

func (s *Server) runnerGroupTarget(w http.ResponseWriter, r *http.Request) (runnerScope, bool) {
	target, err := s.runnerScopeFromRequest(r)
	if err != nil || (target.Org == "" && target.Enterprise == "") {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return runnerScope{}, false
	}
	return target, true
}

func runnerGroupJSON(g *RunnerGroup, baseURL string, target runnerScope) map[string]any {
	var apiBase string
	if target.Enterprise != "" {
		apiBase = fmt.Sprintf("%s/api/v3/enterprises/%s/actions/runner-groups/%d", baseURL, target.Enterprise, g.ID)
	} else {
		apiBase = fmt.Sprintf("%s/api/v3/orgs/%s/actions/runner-groups/%d", baseURL, target.Org, g.ID)
	}
	out := map[string]any{
		"id":                              g.ID,
		"name":                            g.Name,
		"visibility":                      g.Visibility,
		"default":                         g.Default,
		"runners_url":                     apiBase + "/runners",
		"inherited":                       false,
		"allows_public_repositories":      g.AllowsPublicRepositories,
		"restricted_to_workflows":         g.RestrictedToWorkflows,
		"selected_workflows":              nonNilStrings(g.SelectedWorkflows),
		"workflow_restrictions_read_only": false,
	}
	if g.Visibility == "selected" {
		if target.Enterprise != "" {
			out["selected_organizations_url"] = apiBase + "/organizations"
		} else {
			out["selected_repositories_url"] = apiBase + "/repositories"
		}
	}
	if g.NetworkConfigurationID != "" {
		out["network_configuration_id"] = g.NetworkConfigurationID
	}
	return out
}

func (s *Server) handleListRunnerGroups(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return
	}
	s.store.Mu.Lock()
	s.ensureDefaultRunnerGroupLocked(target)
	groups := make([]*RunnerGroup, 0, len(s.store.RunnerGroups))
	for _, g := range s.store.RunnerGroups {
		if runnerGroupMatchesTarget(g, target) {
			groups = append(groups, g)
		}
	}
	s.store.Mu.Unlock()
	sortRunnerGroups(groups)

	page := paginateAndLink(w, r, groups)
	base := s.baseURL(r)
	out := make([]map[string]any, 0, len(page))
	for _, g := range page {
		out = append(out, runnerGroupJSON(g, base, target))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":   len(groups),
		"runner_groups": out,
	})
}

func sortRunnerGroups(groups []*RunnerGroup) {
	for i := 1; i < len(groups); i++ {
		for j := i; j > 0 && groups[j-1].ID > groups[j].ID; j-- {
			groups[j-1], groups[j] = groups[j], groups[j-1]
		}
	}
}

func (s *Server) handleCreateRunnerGroup(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Name                     string   `json:"name"`
		Visibility               string   `json:"visibility"`
		SelectedRepositoryIDs    []int    `json:"selected_repository_ids"`
		SelectedOrganizationIDs  []int    `json:"selected_organization_ids"`
		Runners                  []int    `json:"runners"`
		AllowsPublicRepositories *bool    `json:"allows_public_repositories"`
		RestrictedToWorkflows    bool     `json:"restricted_to_workflows"`
		SelectedWorkflows        []string `json:"selected_workflows"`
		NetworkConfigurationID   string   `json:"network_configuration_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeGHValidationError(w, "RunnerGroup", "name", "missing_field")
		return
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = "all"
	}
	switch visibility {
	case "all", "selected", "private":
	default:
		writeGHValidationError(w, "RunnerGroup", "visibility", "invalid")
		return
	}
	if target.Enterprise != "" && visibility == "private" {
		writeGHValidationError(w, "RunnerGroup", "visibility", "invalid")
		return
	}

	s.store.Mu.Lock()
	s.ensureDefaultRunnerGroupLocked(target)
	for _, existing := range s.store.RunnerGroups {
		if runnerGroupMatchesTarget(existing, target) && strings.EqualFold(existing.Name, req.Name) {
			s.store.Mu.Unlock()
			writeGHValidationError(w, "RunnerGroup", "name", "already_exists")
			return
		}
	}
	for _, repoID := range req.SelectedRepositoryIDs {
		if target.Org == "" || !repoOwnedByOrg(s.store.Repos[repoID], target.Org) {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	for _, orgID := range req.SelectedOrganizationIDs {
		if target.Enterprise == "" || s.store.Orgs[orgID] == nil {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	for _, runnerID := range req.Runners {
		agent := s.store.Agents[runnerID]
		if agent == nil || !runnerVisibleAt(agent.Scope, target) {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	id := s.store.NextRunnerGroupID
	if id <= defaultRunnerGroupID {
		id = defaultRunnerGroupID + 1
	}
	s.store.NextRunnerGroupID = id + 1
	g := &RunnerGroup{
		ID:                       id,
		Name:                     req.Name,
		Visibility:               visibility,
		AllowsPublicRepositories: req.AllowsPublicRepositories != nil && *req.AllowsPublicRepositories,
		SelectedRepoIDs:          req.SelectedRepositoryIDs,
		SelectedOrgIDs:           req.SelectedOrganizationIDs,
		RestrictedToWorkflows:    req.RestrictedToWorkflows,
		SelectedWorkflows:        append([]string(nil), req.SelectedWorkflows...),
		NetworkConfigurationID:   req.NetworkConfigurationID,
		Scope:                    target,
		CreatedAt:                s.store.CurrentTime(),
	}
	s.store.RunnerGroups[id] = g
	for _, runnerID := range req.Runners {
		if a := s.store.Agents[runnerID]; a != nil && runnerVisibleAt(a.Scope, target) {
			a.RunnerGroupID = id
		}
	}
	s.persistRunnerGroupLocked(g)
	s.store.Mu.Unlock()

	writeJSON(w, http.StatusCreated, runnerGroupJSON(g, s.baseURL(r), target))
}

func (s *Server) persistRunnerGroupLocked(g *RunnerGroup) {
	if s.store.Persist != nil {
		s.store.Persist.MustPut("runner_groups", strconv.Itoa(g.ID), g)
	}
}

// lookupRunnerGroup resolves the path's runner_group_id; nil + handled
// response when missing.
func (s *Server) lookupRunnerGroup(w http.ResponseWriter, r *http.Request) *RunnerGroup {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return nil
	}
	id, err := strconv.Atoi(r.PathValue("runner_group_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	s.store.Mu.Lock()
	s.ensureDefaultRunnerGroupLocked(target)
	g := s.store.RunnerGroups[id]
	s.store.Mu.Unlock()
	if g == nil || !runnerGroupMatchesTarget(g, target) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return g
}

func (s *Server) handleGetRunnerGroup(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	writeJSON(w, http.StatusOK, runnerGroupJSON(g, s.baseURL(r), g.Scope))
}

func (s *Server) handleUpdateRunnerGroup(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	var req struct {
		Name                     *string   `json:"name"`
		Visibility               *string   `json:"visibility"`
		AllowsPublicRepositories *bool     `json:"allows_public_repositories"`
		RestrictedToWorkflows    *bool     `json:"restricted_to_workflows"`
		SelectedWorkflows        *[]string `json:"selected_workflows"`
		NetworkConfigurationID   *string   `json:"network_configuration_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if req.Name != nil && *req.Name == "" {
		writeGHValidationError(w, "RunnerGroup", "name", "invalid")
		return
	}
	if req.Visibility != nil {
		valid := *req.Visibility == "all" || *req.Visibility == "selected" ||
			(*req.Visibility == "private" && g.Scope.Org != "")
		if !valid {
			writeGHValidationError(w, "RunnerGroup", "visibility", "invalid")
			return
		}
	}
	s.store.Mu.Lock()
	if req.Name != nil {
		for _, existing := range s.store.RunnerGroups {
			if existing.ID != g.ID && runnerGroupMatchesTarget(existing, g.Scope) &&
				strings.EqualFold(existing.Name, *req.Name) {
				s.store.Mu.Unlock()
				writeGHValidationError(w, "RunnerGroup", "name", "already_exists")
				return
			}
		}
		g.Name = *req.Name
	}
	if req.Visibility != nil {
		g.Visibility = *req.Visibility
	}
	if req.AllowsPublicRepositories != nil {
		g.AllowsPublicRepositories = *req.AllowsPublicRepositories
	}
	if req.RestrictedToWorkflows != nil {
		g.RestrictedToWorkflows = *req.RestrictedToWorkflows
	}
	if req.SelectedWorkflows != nil {
		g.SelectedWorkflows = append([]string(nil), (*req.SelectedWorkflows)...)
	}
	if req.NetworkConfigurationID != nil {
		g.NetworkConfigurationID = *req.NetworkConfigurationID
	}
	s.persistRunnerGroupLocked(g)
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, runnerGroupJSON(g, s.baseURL(r), g.Scope))
}

func (s *Server) handleDeleteRunnerGroup(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	if g.Default {
		// Real GitHub refuses to delete the default group.
		writeGHError(w, http.StatusBadRequest, "Cannot delete the default runner group")
		return
	}
	s.store.Mu.Lock()
	delete(s.store.RunnerGroups, g.ID)
	// Members fall back to the default group, like real GitHub.
	fallback := s.ensureDefaultRunnerGroupLocked(g.Scope)
	for _, a := range s.store.Agents {
		if a.RunnerGroupID == g.ID {
			a.RunnerGroupID = fallback.ID
		}
	}
	if s.store.Persist != nil {
		s.store.Persist.MustDelete("runner_groups", strconv.Itoa(g.ID))
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGroupRunners(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	s.store.Mu.RLock()
	members := make([]*Agent, 0)
	for _, a := range s.store.Agents {
		if runnerVisibleAt(a.Scope, g.Scope) &&
			(a.RunnerGroupID == g.ID || (g.Default && a.RunnerGroupID == 0)) {
			members = append(members, a)
		}
	}
	busy := s.busyAgentIDsLocked()
	s.store.Mu.RUnlock()

	page := paginateAndLink(w, r, members)
	runners := make([]map[string]any, 0, len(page))
	for _, a := range page {
		runners = append(runners, runnerJSON(a, busy[a.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(members),
		"runners":     runners,
	})
}

// agentGroupID resolves an agent's group for legacy response shapes. New
// membership logic resolves the owning scope's actual default group because
// every organization and enterprise has a distinct one.
func agentGroupID(a *Agent) int {
	if a.RunnerGroupID == 0 {
		return defaultRunnerGroupID
	}
	return a.RunnerGroupID
}

func (s *Server) handleSetGroupRunners(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	var req struct {
		Runners []int `json:"runners"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	want := map[int]bool{}
	for _, id := range req.Runners {
		want[id] = true
	}
	s.store.Mu.Lock()
	fallback := s.ensureDefaultRunnerGroupLocked(g.Scope)
	for _, a := range s.store.Agents {
		if !runnerVisibleAt(a.Scope, g.Scope) {
			continue
		}
		switch {
		case want[a.ID]:
			a.RunnerGroupID = g.ID
		case a.RunnerGroupID == g.ID || (g.Default && a.RunnerGroupID == 0):
			a.RunnerGroupID = fallback.ID
		}
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddGroupRunner(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	a := s.store.Agents[id]
	if a != nil && runnerVisibleAt(a.Scope, g.Scope) {
		a.RunnerGroupID = g.ID
	} else {
		a = nil
	}
	s.store.Mu.Unlock()
	if a == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveGroupRunner(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	a := s.store.Agents[id]
	if a != nil && runnerVisibleAt(a.Scope, g.Scope) &&
		(a.RunnerGroupID == g.ID || (g.Default && a.RunnerGroupID == 0)) {
		fallback := s.ensureDefaultRunnerGroupLocked(g.Scope)
		a.RunnerGroupID = fallback.ID
	} else if a != nil && !runnerVisibleAt(a.Scope, g.Scope) {
		a = nil
	}
	s.store.Mu.Unlock()
	if a == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGroupRepos(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	base := s.baseURL(r)
	s.store.Mu.RLock()
	ids := append([]int(nil), g.SelectedRepoIDs...)
	s.store.Mu.RUnlock()
	repos := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		s.store.Mu.RLock()
		repo := s.store.Repos[id]
		s.store.Mu.RUnlock()
		if repoOwnedByOrg(repo, g.Scope.Org) {
			repos = append(repos, repoToJSON(repo, s.store, base))
		}
	}
	paged := paginateAndLink(w, r, repos)
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":  len(repos),
		"repositories": paged,
	})
}

func (s *Server) handleSetGroupRepos(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	var req struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	s.store.Mu.Lock()
	for _, id := range req.SelectedRepositoryIDs {
		repo := s.store.Repos[id]
		if !repoOwnedByOrg(repo, g.Scope.Org) {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	g.SelectedRepoIDs = req.SelectedRepositoryIDs
	s.persistRunnerGroupLocked(g)
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddGroupRepo(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	repo := s.store.Repos[id]
	exists := repoOwnedByOrg(repo, g.Scope.Org)
	if exists {
		found := false
		for _, rid := range g.SelectedRepoIDs {
			if rid == id {
				found = true
				break
			}
		}
		if !found {
			g.SelectedRepoIDs = append(g.SelectedRepoIDs, id)
			s.persistRunnerGroupLocked(g)
		}
	}
	s.store.Mu.Unlock()
	if !exists {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveGroupRepo(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	repo := s.store.Repos[id]
	if !repoOwnedByOrg(repo, g.Scope.Org) {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	kept := g.SelectedRepoIDs[:0]
	for _, rid := range g.SelectedRepoIDs {
		if rid != id {
			kept = append(kept, rid)
		}
	}
	g.SelectedRepoIDs = kept
	s.persistRunnerGroupLocked(g)
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGroupOrganizations(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	s.store.Mu.RLock()
	orgs := make([]*Org, 0, len(g.SelectedOrgIDs))
	for _, id := range g.SelectedOrgIDs {
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

func (s *Server) handleSetGroupOrganizations(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	var req struct {
		SelectedOrganizationIDs []int `json:"selected_organization_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	s.store.Mu.Lock()
	for _, id := range req.SelectedOrganizationIDs {
		if s.store.Orgs[id] == nil {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	g.SelectedOrgIDs = append([]int(nil), req.SelectedOrganizationIDs...)
	s.persistRunnerGroupLocked(g)
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddGroupOrganization(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("org_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	if s.store.Orgs[id] == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !slices.Contains(g.SelectedOrgIDs, id) {
		g.SelectedOrgIDs = append(g.SelectedOrgIDs, id)
		s.persistRunnerGroupLocked(g)
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveGroupOrganization(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("org_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	if s.store.Orgs[id] == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	g.SelectedOrgIDs = slices.DeleteFunc(g.SelectedOrgIDs, func(candidate int) bool {
		return candidate == id
	})
	s.persistRunnerGroupLocked(g)
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
