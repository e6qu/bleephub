package bleephub

import (
	"net/http"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

// Immutable releases: a repo toggle plus an org enforcement policy
// (all/none/selected). The repo check endpoint reflects both.

func (s *Server) registerGHImmutableReleaseRoutes() {
	s.route("GET /api/v3/orgs/{org}/settings/immutable-releases",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetOrgImmutableReleasesSettings)))
	s.route("PUT /api/v3/orgs/{org}/settings/immutable-releases",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleSetOrgImmutableReleasesSettings)))
	s.route("GET /api/v3/orgs/{org}/settings/immutable-releases/repositories",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleListOrgImmutableReleasesRepos)))
	s.route("PUT /api/v3/orgs/{org}/settings/immutable-releases/repositories",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleSetOrgImmutableReleasesRepos)))
	s.route("PUT /api/v3/orgs/{org}/settings/immutable-releases/repositories/{repository_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleAddOrgImmutableReleasesRepo)))
	s.route("DELETE /api/v3/orgs/{org}/settings/immutable-releases/repositories/{repository_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleRemoveOrgImmutableReleasesRepo)))
	s.route("GET /api/v3/repos/{owner}/{repo}/immutable-releases",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleCheckRepoImmutableReleases))
	s.route("PUT /api/v3/repos/{owner}/{repo}/immutable-releases",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleEnableRepoImmutableReleases))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/immutable-releases",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDisableRepoImmutableReleases))
}

func (s *Server) handleGetOrgImmutableReleasesSettings(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	settings := s.store.GetOrgImmutableReleasesSettings(org)
	out := map[string]interface{}{
		"enforced_repositories": settings.EnforcedRepositories,
	}
	if settings.EnforcedRepositories == "selected" {
		out["selected_repositories_url"] = s.baseURL(r) + "/api/v3/orgs/" + org + "/settings/immutable-releases/repositories"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSetOrgImmutableReleasesSettings(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req struct {
		EnforcedRepositories  *string `json:"enforced_repositories"`
		SelectedRepositoryIDs []int   `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.EnforcedRepositories == nil {
		store.WriteGHValidationError(w, "ImmutableReleasesSettings", "enforced_repositories", "missing_field")
		return
	}
	switch *req.EnforcedRepositories {
	case "all", "none", "selected":
	default:
		store.WriteGHValidationError(w, "ImmutableReleasesSettings", "enforced_repositories", "invalid")
		return
	}
	if *req.EnforcedRepositories != "selected" && len(req.SelectedRepositoryIDs) > 0 {
		store.WriteGHValidationError(w, "ImmutableReleasesSettings", "selected_repository_ids", "invalid")
		return
	}
	if !s.validateOrgRepoIDs(w, org, req.SelectedRepositoryIDs) {
		return
	}
	s.store.SetOrgImmutableReleasesSettings(org, *req.EnforcedRepositories, req.SelectedRepositoryIDs)
	w.WriteHeader(http.StatusNoContent)
}

// validateOrgRepoIDs checks every ID names a repo in the org, writing a 404 on
// the first that does not.
func (s *Server) validateOrgRepoIDs(w http.ResponseWriter, org string, ids []int) bool {
	for _, id := range ids {
		repo := s.store.GetRepoByID(id)
		if repo == nil || !s.store.RepoBelongsToOrg(repo, org) {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return false
		}
	}
	return true
}

func (s *Server) handleListOrgImmutableReleasesRepos(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	repos := s.store.ListOrgImmutableReleasesRepos(org)
	total := len(repos)
	repos = paginateAndLink(w, r, repos)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(repos))
	for _, repo := range repos {
		out = append(out, minimalRepoJSON(repo, s.store, base))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":  total,
		"repositories": out,
	})
}

func (s *Server) handleSetOrgImmutableReleasesRepos(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SelectedRepositoryIDs == nil {
		store.WriteGHValidationError(w, "ImmutableReleasesSettings", "selected_repository_ids", "missing_field")
		return
	}
	if s.store.GetOrgImmutableReleasesSettings(org).EnforcedRepositories != "selected" {
		writeGHError(w, http.StatusConflict, "The organization immutable releases policy for enforced_repositories must be configured to selected")
		return
	}
	if !s.validateOrgRepoIDs(w, org, req.SelectedRepositoryIDs) {
		return
	}
	s.store.SetOrgImmutableReleasesSettings(org, "selected", req.SelectedRepositoryIDs)
	w.WriteHeader(http.StatusNoContent)
}

// resolveImmutableReleasesRepoID parses {repository_id} and requires the
// selected enforcement policy, writing the error on failure.
func (s *Server) resolveImmutableReleasesRepoID(w http.ResponseWriter, r *http.Request) (int, bool) {
	org := r.PathValue("org")
	repoID, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return 0, false
	}
	if s.store.GetOrgImmutableReleasesSettings(org).EnforcedRepositories != "selected" {
		writeGHError(w, http.StatusConflict, "The organization immutable releases policy for enforced_repositories must be configured to selected")
		return 0, false
	}
	return repoID, true
}

func (s *Server) handleAddOrgImmutableReleasesRepo(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	repoID, ok := s.resolveImmutableReleasesRepoID(w, r)
	if !ok {
		return
	}
	if !s.validateOrgRepoIDs(w, org, []int{repoID}) {
		return
	}
	s.store.AddOrgImmutableReleasesRepo(org, repoID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveOrgImmutableReleasesRepo(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	repoID, ok := s.resolveImmutableReleasesRepoID(w, r)
	if !ok {
		return
	}
	s.store.RemoveOrgImmutableReleasesRepo(org, repoID)
	w.WriteHeader(http.StatusNoContent)
}

// resolveRepoForImmutableReleases resolves the repo and requires admin access,
// writing a 404 on failure.
func (s *Server) resolveRepoForImmutableReleases(w http.ResponseWriter, r *http.Request) *store.Repo {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

func (s *Server) handleCheckRepoImmutableReleases(w http.ResponseWriter, r *http.Request) {
	repo := s.resolveRepoForImmutableReleases(w, r)
	if repo == nil {
		return
	}
	enabled, enforced := s.store.RepoImmutableReleasesState(repo)
	if !enabled {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":           true,
		"enforced_by_owner": enforced,
	})
}

func (s *Server) handleEnableRepoImmutableReleases(w http.ResponseWriter, r *http.Request) {
	repo := s.resolveRepoForImmutableReleases(w, r)
	if repo == nil {
		return
	}
	s.store.SetRepoImmutableReleases(repo.FullName, true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDisableRepoImmutableReleases(w http.ResponseWriter, r *http.Request) {
	repo := s.resolveRepoForImmutableReleases(w, r)
	if repo == nil {
		return
	}
	if _, enforced := s.store.RepoImmutableReleasesState(repo); enforced {
		writeGHError(w, http.StatusConflict, "Immutable releases are enforced by the repository owner")
		return
	}
	s.store.SetRepoImmutableReleases(repo.FullName, false)
	w.WriteHeader(http.StatusNoContent)
}
