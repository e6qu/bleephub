package bleephub

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) requireEnterpriseAppPermission(write bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.resolveEnterprise(w, r) {
			return
		}
		if user := ghUserFromContext(r.Context()); user != nil && user.SiteAdmin {
			next(w, r)
			return
		}
		token := ghInstallationTokenFromContext(r.Context())
		if token == nil {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		required := "read"
		if write {
			required = "write"
		}
		holds := func(permission string) bool {
			level := token.Permissions[permission]
			return level == "write" || (!write && level == required)
		}
		if !holds("enterprise_organization_installations") &&
			!holds("enterprise_organization_installation_repositories") {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		next(w, r)
	}
}

func (s *Server) registerGHEnterpriseAppRoutes() {
	read := func(next http.HandlerFunc) http.HandlerFunc { return s.requireEnterpriseAppPermission(false, next) }
	write := func(next http.HandlerFunc) http.HandlerFunc { return s.requireEnterpriseAppPermission(true, next) }
	s.route("GET /api/v3/enterprises/{enterprise}/apps/installable_organizations", read(s.handleListEnterpriseInstallableOrganizations))
	s.route("GET /api/v3/enterprises/{enterprise}/apps/installable_organizations/{org}/accessible_repositories", read(s.handleListEnterpriseOrganizationAccessibleRepositories))
	s.route("GET /api/v3/enterprises/{enterprise}/apps/organizations/{org}/installations", read(s.handleListEnterpriseOrganizationInstallations))
	s.route("POST /api/v3/enterprises/{enterprise}/apps/organizations/{org}/installations", write(s.handleCreateEnterpriseOrganizationInstallation))
	s.route("DELETE /api/v3/enterprises/{enterprise}/apps/organizations/{org}/installations/{installation_id}", write(s.handleDeleteEnterpriseOrganizationInstallation))
	s.route("GET /api/v3/enterprises/{enterprise}/apps/organizations/{org}/installations/{installation_id}/repositories", read(s.handleListEnterpriseInstallationRepositories))
	s.route("PATCH /api/v3/enterprises/{enterprise}/apps/organizations/{org}/installations/{installation_id}/repositories", write(s.handleSetEnterpriseInstallationRepositories))
	s.route("PATCH /api/v3/enterprises/{enterprise}/apps/organizations/{org}/installations/{installation_id}/repositories/add", write(s.handleAddEnterpriseInstallationRepositories))
	s.route("PATCH /api/v3/enterprises/{enterprise}/apps/organizations/{org}/installations/{installation_id}/repositories/remove", write(s.handleRemoveEnterpriseInstallationRepositories))
}

func (s *Server) handleListEnterpriseInstallableOrganizations(w http.ResponseWriter, r *http.Request) {
	orgs := paginateAndLink(w, r, s.store.ListOrgsAll(0))
	out := make([]map[string]interface{}, len(orgs))
	for i, org := range orgs {
		out[i] = map[string]interface{}{"id": org.ID, "login": org.Login}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) enterpriseAppOrg(w http.ResponseWriter, r *http.Request) *store.Org {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
	return org
}

func shallowEnterpriseRepoJSON(repo *store.Repo) map[string]interface{} {
	return map[string]interface{}{"id": repo.ID, "name": repo.Name, "full_name": repo.FullName}
}

func (s *Server) handleListEnterpriseOrganizationAccessibleRepositories(w http.ResponseWriter, r *http.Request) {
	org := s.enterpriseAppOrg(w, r)
	if org == nil {
		return
	}
	repos := paginateAndLink(w, r, s.store.ListReposByOwner(org.Login))
	out := make([]map[string]interface{}, len(repos))
	for i, repo := range repos {
		out[i] = shallowEnterpriseRepoJSON(repo)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) organizationInstallations(org string) []*store.Installation {
	s.store.Mu.RLock()
	out := make([]*store.Installation, 0)
	for _, installation := range s.store.Installations {
		if installation.TargetType == "Organization" && installation.TargetLogin == org {
			out = append(out, store.CloneInstallation(installation))
		}
	}
	s.store.Mu.RUnlock()
	sortInstallations(out)
	return out
}

func sortInstallations(installations []*store.Installation) {
	sort.Slice(installations, func(i, j int) bool { return installations[i].ID < installations[j].ID })
}

func enterpriseInstallationJSON(installation *store.Installation, enterprise, org, baseURL string) map[string]interface{} {
	value := installationToJSON(installation, baseURL)
	value["repositories_url"] = "/api/v3/enterprises/" + enterprise + "/apps/organizations/" + org +
		"/installations/" + strconv.Itoa(installation.ID) + "/repositories"
	return value
}

func (s *Server) handleListEnterpriseOrganizationInstallations(w http.ResponseWriter, r *http.Request) {
	org := s.enterpriseAppOrg(w, r)
	if org == nil {
		return
	}
	installations := paginateAndLink(w, r, s.organizationInstallations(org.Login))
	out := make([]map[string]interface{}, len(installations))
	for i, installation := range installations {
		out[i] = map[string]interface{}{"value": enterpriseInstallationJSON(installation, s.enterpriseSlug(), org.Login, s.baseURL(r))}
	}
	writeJSON(w, http.StatusOK, out)
}

type enterpriseInstallationRequest struct {
	ClientID            string   `json:"client_id"`
	RepositorySelection string   `json:"repository_selection"`
	Repositories        []string `json:"repositories"`
}

func (s *Server) resolveEnterpriseInstallationRepos(w http.ResponseWriter, org *store.Org, selection string, names []string) ([]int, bool) {
	if selection != "all" && selection != "selected" && selection != "none" {
		store.WriteGHValidationError(w, "Installation", "repository_selection", "invalid")
		return nil, false
	}
	if selection == "selected" && len(names) == 0 {
		store.WriteGHValidationError(w, "Installation", "repositories", "missing_field")
		return nil, false
	}
	if selection != "selected" && len(names) != 0 {
		store.WriteGHValidationError(w, "Installation", "repositories", "invalid")
		return nil, false
	}
	ids := make([]int, 0, len(names))
	seen := map[int]bool{}
	for _, name := range names {
		repo := s.store.GetRepoByFullName(org.Login + "/" + name)
		if repo == nil || seen[repo.ID] {
			store.WriteGHValidationError(w, "Installation", "repositories", "invalid")
			return nil, false
		}
		seen[repo.ID] = true
		ids = append(ids, repo.ID)
	}
	return ids, true
}

func (s *Server) handleCreateEnterpriseOrganizationInstallation(w http.ResponseWriter, r *http.Request) {
	org := s.enterpriseAppOrg(w, r)
	if org == nil {
		return
	}
	var req enterpriseInstallationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	app := s.store.GetAppByClientID(req.ClientID)
	if app == nil {
		store.WriteGHValidationError(w, "Installation", "client_id", "invalid")
		return
	}
	repoIDs, ok := s.resolveEnterpriseInstallationRepos(w, org, req.RepositorySelection, req.Repositories)
	if !ok {
		return
	}
	for _, existing := range s.organizationInstallations(org.Login) {
		if existing.AppID == app.ID {
			s.store.UnsuspendInstallation(existing.ID)
			s.store.SetInstallationRepositorySelection(existing.ID, req.RepositorySelection, repoIDs)
			writeJSON(w, http.StatusOK, enterpriseInstallationJSON(
				s.store.GetInstallation(existing.ID), s.enterpriseSlug(), org.Login, s.baseURL(r)))
			return
		}
	}
	installation := s.store.CreateInstallation(
		app.ID, "Organization", org.ID, org.Login, app.Permissions, app.Events)
	s.store.SetInstallationRepositorySelection(installation.ID, req.RepositorySelection, repoIDs)
	writeJSON(w, http.StatusCreated, enterpriseInstallationJSON(
		s.store.GetInstallation(installation.ID), s.enterpriseSlug(), org.Login, s.baseURL(r)))
}

func (s *Server) enterpriseOrganizationInstallation(w http.ResponseWriter, r *http.Request, org *store.Org) *store.Installation {
	id, err := strconv.Atoi(r.PathValue("installation_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	installation := s.store.GetInstallation(id)
	if installation == nil || installation.TargetType != "Organization" ||
		installation.TargetID != org.ID || installation.TargetLogin != org.Login {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return installation
}

func (s *Server) handleDeleteEnterpriseOrganizationInstallation(w http.ResponseWriter, r *http.Request) {
	org := s.enterpriseAppOrg(w, r)
	if org == nil {
		return
	}
	installation := s.enterpriseOrganizationInstallation(w, r, org)
	if installation == nil {
		return
	}
	s.store.DeleteInstallation(installation.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) enterpriseInstallationRepositories(installation *store.Installation) []*store.Repo {
	return filterInstallationRepos(s.store.ListReposByOwner(installation.TargetLogin), installation)
}

func (s *Server) writeEnterpriseInstallationRepositories(w http.ResponseWriter, r *http.Request, installation *store.Installation) {
	repos := paginateAndLink(w, r, s.enterpriseInstallationRepositories(installation))
	out := make([]map[string]interface{}, len(repos))
	for i, repo := range repos {
		out[i] = shallowEnterpriseRepoJSON(repo)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListEnterpriseInstallationRepositories(w http.ResponseWriter, r *http.Request) {
	org := s.enterpriseAppOrg(w, r)
	if org == nil {
		return
	}
	installation := s.enterpriseOrganizationInstallation(w, r, org)
	if installation == nil {
		return
	}
	s.writeEnterpriseInstallationRepositories(w, r, installation)
}

func (s *Server) handleSetEnterpriseInstallationRepositories(w http.ResponseWriter, r *http.Request) {
	org := s.enterpriseAppOrg(w, r)
	if org == nil {
		return
	}
	installation := s.enterpriseOrganizationInstallation(w, r, org)
	if installation == nil {
		return
	}
	var req enterpriseInstallationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repoIDs, ok := s.resolveEnterpriseInstallationRepos(w, org, req.RepositorySelection, req.Repositories)
	if !ok || req.RepositorySelection == "none" {
		if ok {
			store.WriteGHValidationError(w, "Installation", "repository_selection", "invalid")
		}
		return
	}
	s.store.SetInstallationRepositorySelection(installation.ID, req.RepositorySelection, repoIDs)
	writeJSON(w, http.StatusOK, enterpriseInstallationJSON(
		s.store.GetInstallation(installation.ID), s.enterpriseSlug(), org.Login, s.baseURL(r)))
}

func (s *Server) mutateEnterpriseInstallationRepositories(w http.ResponseWriter, r *http.Request, add bool) {
	org := s.enterpriseAppOrg(w, r)
	if org == nil {
		return
	}
	installation := s.enterpriseOrganizationInstallation(w, r, org)
	if installation == nil {
		return
	}
	if installation.RepositorySelection != "selected" {
		store.WriteGHValidationError(w, "Installation", "repository_selection", "invalid")
		return
	}
	var req struct {
		Repositories []string `json:"repositories"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repoIDs, ok := s.resolveEnterpriseInstallationRepos(w, org, "selected", req.Repositories)
	if !ok || len(repoIDs) > 50 {
		if ok {
			store.WriteGHValidationError(w, "Installation", "repositories", "invalid")
		}
		return
	}
	if !add && len(installation.SelectedRepoIDs)-len(repoIDs) < 1 {
		store.WriteGHValidationError(w, "Installation", "repositories", "invalid")
		return
	}
	for _, repoID := range repoIDs {
		if add {
			s.store.AddInstallationRepo(installation.ID, repoID)
		} else {
			s.store.RemoveInstallationRepo(installation.ID, repoID)
		}
	}
	s.writeEnterpriseInstallationRepositories(w, r, s.store.GetInstallation(installation.ID))
}

func (s *Server) handleAddEnterpriseInstallationRepositories(w http.ResponseWriter, r *http.Request) {
	s.mutateEnterpriseInstallationRepositories(w, r, true)
}

func (s *Server) handleRemoveEnterpriseInstallationRepositories(w http.ResponseWriter, r *http.Request) {
	s.mutateEnterpriseInstallationRepositories(w, r, false)
}
