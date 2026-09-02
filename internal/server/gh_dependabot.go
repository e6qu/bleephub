package bleephub

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHDependabotRoutes() {
	// Alerts
	s.route("GET /api/v3/repos/{owner}/{repo}/dependabot/alerts",
		s.requirePerm(store.ScopeSecurityEvents, store.PermRead, s.handleListDependabotAlerts))
	s.route("GET /api/v3/repos/{owner}/{repo}/dependabot/alerts/{alert_number}",
		s.requirePerm(store.ScopeSecurityEvents, store.PermRead, s.handleGetDependabotAlert))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/dependabot/alerts/{alert_number}",
		s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleUpdateDependabotAlert))

	// Repository-scoped secrets
	s.route("GET /api/v3/repos/{owner}/{repo}/dependabot/secrets",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermRead, s.handleListDependabotRepoSecrets))
	s.route("GET /api/v3/repos/{owner}/{repo}/dependabot/secrets/public-key",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermRead, s.handleGetDependabotRepoSecretsPublicKey))
	s.route("GET /api/v3/repos/{owner}/{repo}/dependabot/secrets/{secret_name}",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermRead, s.handleGetDependabotRepoSecret))
	s.route("PUT /api/v3/repos/{owner}/{repo}/dependabot/secrets/{secret_name}",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermWrite, s.handlePutDependabotRepoSecret))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/dependabot/secrets/{secret_name}",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermWrite, s.handleDeleteDependabotRepoSecret))

	// Organization-scoped secrets
	s.route("GET /api/v3/orgs/{org}/dependabot/secrets",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermRead, s.handleListDependabotOrgSecrets))
	s.route("GET /api/v3/orgs/{org}/dependabot/secrets/public-key",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermRead, s.handleGetDependabotOrgSecretsPublicKey))
	s.route("GET /api/v3/orgs/{org}/dependabot/secrets/{secret_name}",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermRead, s.handleGetDependabotOrgSecret))
	s.route("PUT /api/v3/orgs/{org}/dependabot/secrets/{secret_name}",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermWrite, s.handlePutDependabotOrgSecret))
	s.route("DELETE /api/v3/orgs/{org}/dependabot/secrets/{secret_name}",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermWrite, s.handleDeleteDependabotOrgSecret))
	s.route("GET /api/v3/orgs/{org}/dependabot/secrets/{secret_name}/repositories",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermRead, s.handleListDependabotOrgSecretRepos))
	s.route("PUT /api/v3/orgs/{org}/dependabot/secrets/{secret_name}/repositories",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermWrite, s.handleSetDependabotOrgSecretRepos))
	s.route("PUT /api/v3/orgs/{org}/dependabot/secrets/{secret_name}/repositories/{repository_id}",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermWrite, s.handleAddDependabotOrgSecretRepo))
	s.route("DELETE /api/v3/orgs/{org}/dependabot/secrets/{secret_name}/repositories/{repository_id}",
		s.requirePerm(store.ScopeDependabotSecrets, store.PermWrite, s.handleRemoveDependabotOrgSecretRepo))

	// Organization-level alerts and repository access
	s.route("GET /api/v3/orgs/{org}/dependabot/alerts",
		s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermRead, s.handleListDependabotOrgAlerts))
	s.route("GET /api/v3/orgs/{org}/dependabot/repository-access",
		s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleGetDependabotRepositoryAccess))
	s.route("PATCH /api/v3/orgs/{org}/dependabot/repository-access",
		s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermWrite, s.handleUpdateDependabotRepositoryAccess))
	s.route("PUT /api/v3/orgs/{org}/dependabot/repository-access/default-level",
		s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermWrite, s.handleSetDependabotRepositoryAccessDefaultLevel))
}

// alerts

func (s *Server) handleListDependabotAlerts(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupSecurityReadableRepo(w, r)
	if repo == nil {
		return
	}

	q := r.URL.Query()
	state := q.Get("state")
	severity := q.Get("severity")
	packageName := q.Get("package_name")
	ecosystem := q.Get("ecosystem")
	manifest := q.Get("manifest")
	sort := q.Get("sort")
	direction := q.Get("direction")

	alerts := s.store.ListDependabotAlerts(repo.FullName, state, severity, packageName, ecosystem, manifest, sort, direction)
	page := paginateAndLink(w, r, alerts)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, len(page))
	for i, a := range page {
		out[i] = dependabotAlertToJSON(a, baseURL, repo, s.store)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetDependabotAlert(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupSecurityReadableRepo(w, r)
	if repo == nil {
		return
	}

	a := s.lookupDependabotAlert(w, r, repo)
	if a == nil {
		return
	}
	writeJSON(w, http.StatusOK, dependabotAlertToJSON(a, s.baseURL(r), repo, s.store))
}

func (s *Server) handleUpdateDependabotAlert(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	user := ghUserFromContext(r.Context())
	if !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecurityEvents, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	a := s.lookupDependabotAlert(w, r, repo)
	if a == nil {
		return
	}

	var req struct {
		State            string `json:"state"`
		DismissedReason  string `json:"dismissed_reason"`
		DismissedComment string `json:"dismissed_comment"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.State == "" {
		writeGHValidationErrorSimple(w, "state is missing")
		return
	}
	previousState := a.State
	if err := s.store.UpdateDependabotAlert(a, req.State, req.DismissedReason, req.DismissedComment, user); err != nil {
		writeGHValidationErrorSimple(w, "state is invalid")
		return
	}
	if action := dependabotAlertActionFor(previousState, a.State); action != "" && previousState != a.State {
		s.emitDependabotAlertEvent(repo, a, user, action)
	}
	writeJSON(w, http.StatusOK, dependabotAlertToJSON(a, s.baseURL(r), repo, s.store))
}

func (s *Server) lookupDependabotAlert(w http.ResponseWriter, r *http.Request, repo *store.Repo) *store.DependabotAlert {
	number, err := strconv.Atoi(r.PathValue("alert_number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	a := s.store.GetDependabotAlert(repo.FullName, number)
	if a == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return a
}

func dependabotAlertToJSON(a *store.DependabotAlert, baseURL string, repo *store.Repo, st *store.Store) map[string]interface{} {
	apiURL := fmt.Sprintf("%s/api/v3/repos/%s/dependabot/alerts/%d", baseURL, repo.FullName, a.Number)
	htmlURL := fmt.Sprintf("%s/%s/security/dependabot/%d", baseURL, repo.FullName, a.Number)
	published := a.CreatedAt.UTC().Format(time.RFC3339)
	updated := a.UpdatedAt.UTC().Format(time.RFC3339)

	var dismissedAt, fixedAt, autoDismissedAt interface{} = nil, nil, nil
	if a.DismissedAt != nil {
		dismissedAt = a.DismissedAt.UTC().Format(time.RFC3339)
	}
	if a.FixedAt != nil {
		fixedAt = a.FixedAt.UTC().Format(time.RFC3339)
	}
	if a.AutoDismissedAt != nil {
		autoDismissedAt = a.AutoDismissedAt.UTC().Format(time.RFC3339)
	}

	var firstPatched interface{} = nil
	if a.FirstPatchedVersion != "" {
		firstPatched = map[string]interface{}{"identifier": a.FirstPatchedVersion}
	}

	identifiers := []map[string]interface{}{
		{"type": "GHSA", "value": a.VulnerabilityID},
	}
	if a.CVEID != "" {
		identifiers = append(identifiers, map[string]interface{}{"type": "CVE", "value": a.CVEID})
	}

	pkg := map[string]interface{}{
		"ecosystem": a.PackageEcosystem,
		"name":      a.PackageName,
	}

	return map[string]interface{}{
		"number":     a.Number,
		"state":      a.State,
		"url":        apiURL,
		"html_url":   htmlURL,
		"created_at": published,
		"updated_at": updated,
		"dependency": map[string]interface{}{
			"package":       pkg,
			"manifest_path": a.ManifestPath,
			"scope":         nil,
			"relationship":  nil,
		},
		"security_advisory": map[string]interface{}{
			"ghsa_id":     a.VulnerabilityID,
			"cve_id":      nullOrString(a.CVEID),
			"summary":     a.Summary,
			"description": a.Description,
			"vulnerabilities": []map[string]interface{}{
				{
					"package":                  pkg,
					"severity":                 a.Severity,
					"vulnerable_version_range": a.VulnerableVersionRange,
					"first_patched_version":    firstPatched,
				},
			},
			"severity":    a.Severity,
			"cvss":        map[string]interface{}{"score": 0.0, "vector_string": nil},
			"cwes":        []map[string]interface{}{},
			"identifiers": identifiers,
			"references": []map[string]interface{}{
				{"url": "https://github.com/advisories/" + a.VulnerabilityID},
			},
			"published_at": published,
			"updated_at":   updated,
			"withdrawn_at": nil,
		},
		"security_vulnerability": map[string]interface{}{
			"package":                  pkg,
			"severity":                 a.Severity,
			"vulnerable_version_range": a.VulnerableVersionRange,
			"first_patched_version":    firstPatched,
		},
		"dismissed_at":      dismissedAt,
		"dismissed_by":      dependabotDismisserJSON(a, st, baseURL),
		"dismissed_reason":  nullOrString(a.DismissedReason),
		"dismissed_comment": nullOrString(a.DismissedComment),
		"fixed_at":          fixedAt,
		"auto_dismissed_at": autoDismissedAt,
	}
}

// repository secrets

func (s *Server) handleListDependabotRepoSecrets(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.store.Mu.RLock()
	list := sortedDependabotSecretsJSON(s.store.DependabotSecrets[repo.FullName])
	s.store.Mu.RUnlock()

	paged := paginateAndLink(w, r, list)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(list),
		"secrets":     paged,
	})
}

// handleGetDependabotRepoSecretsPublicKey resolves the repository first: the
// admin gate has nothing to check against a path naming one that does not exist.
func (s *Server) handleGetDependabotRepoSecretsPublicKey(w http.ResponseWriter, r *http.Request) {
	if s.lookupRepoFromPath(r) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeActionsPublicKey(w)
}

func (s *Server) handleGetDependabotRepoSecret(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	s.store.Mu.RLock()
	sec := s.store.DependabotSecrets[repo.FullName][name]
	var body map[string]interface{}
	if sec != nil {
		body = dependabotSecretJSON(sec)
	}
	s.store.Mu.RUnlock()

	if body == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePutDependabotRepoSecret(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rawName := r.PathValue("secret_name")
	if msg := actionsItemNameError("Secret", rawName); msg != "" {
		writeGHError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	name := strings.ToUpper(rawName)

	var body struct {
		EncryptedValue string `json:"encrypted_value"`
		KeyID          string `json:"key_id"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if ok := s.validateDependabotSecretKeyID(w, body.KeyID); !ok {
		return
	}
	if body.EncryptedValue == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "encrypted_value is required")
		return
	}

	created := s.store.UpsertDependabotSecret(repo.FullName, name, body.EncryptedValue, body.KeyID)
	s.recordAuditEvent("dependabot_secret.create", auditActor(r), "", map[string]interface{}{
		"scope": "repository", "repo": repo.FullName, "secret_name": name,
	})
	if created {
		writeJSON(w, http.StatusCreated, map[string]interface{}{})
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleDeleteDependabotRepoSecret(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	if !s.store.DeleteDependabotSecret(repo.FullName, name) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("dependabot_secret.destroy", auditActor(r), "", map[string]interface{}{
		"scope": "repository", "repo": repo.FullName, "secret_name": name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// organization secrets

func (s *Server) resolveOrgForDependabot(w http.ResponseWriter, r *http.Request) (*store.Org, bool) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	return org, true
}

func (s *Server) handleListDependabotOrgSecrets(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	base := s.baseURL(r)

	s.store.Mu.RLock()
	m := s.store.DependabotOrgSecrets[org.Login]
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	list := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		list = append(list, dependabotOrgSecretJSON(m[n], org.Login, base))
	}
	s.store.Mu.RUnlock()

	paged := paginateAndLink(w, r, list)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(list),
		"secrets":     paged,
	})
}

func (s *Server) handleGetDependabotOrgSecretsPublicKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolveOrgForDependabot(w, r); !ok {
		return
	}
	s.writeActionsPublicKey(w)
}

func (s *Server) handleGetDependabotOrgSecret(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))
	base := s.baseURL(r)

	s.store.Mu.RLock()
	sec := s.store.DependabotOrgSecrets[org.Login][name]
	var body map[string]interface{}
	if sec != nil {
		body = dependabotOrgSecretJSON(sec, org.Login, base)
	}
	s.store.Mu.RUnlock()

	if body == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePutDependabotOrgSecret(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	rawName := r.PathValue("secret_name")
	if msg := actionsItemNameError("Secret", rawName); msg != "" {
		writeGHError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	name := strings.ToUpper(rawName)

	var body struct {
		EncryptedValue        string `json:"encrypted_value"`
		KeyID                 string `json:"key_id"`
		Visibility            string `json:"visibility"`
		SelectedRepositoryIDs []int  `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.Visibility == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "visibility is required and must be one of: all, private, selected")
		return
	}
	if !validOrgItemVisibility(body.Visibility) {
		writeGHError(w, http.StatusUnprocessableEntity, "visibility must be one of: all, private, selected")
		return
	}
	if ok := s.validateDependabotSecretKeyID(w, body.KeyID); !ok {
		return
	}
	if body.EncryptedValue == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "encrypted_value is required")
		return
	}

	ids := body.SelectedRepositoryIDs
	for _, id := range ids {
		if s.store.GetRepoByID(id) == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	if bad, ok := s.firstNonOrgRepo(org.Login, ids); !ok {
		writeGHError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("Validation Failed: repository %d does not belong to the organization", bad))
		return
	}

	created := s.store.UpsertDependabotOrgSecret(org.Login, name, body.EncryptedValue, body.KeyID, body.Visibility, ids)
	s.recordAuditEvent("dependabot_secret.create", auditActor(r), org.Login, map[string]interface{}{
		"scope": "organization", "org": org.Login, "secret_name": name, "visibility": body.Visibility,
	})
	if created {
		writeJSON(w, http.StatusCreated, map[string]interface{}{})
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleDeleteDependabotOrgSecret(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	if !s.store.DeleteDependabotOrgSecret(org.Login, name) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("dependabot_secret.destroy", auditActor(r), org.Login, map[string]interface{}{
		"scope": "organization", "org": org.Login, "secret_name": name,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDependabotOrgSecretRepos(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	s.store.Mu.RLock()
	sec := s.store.DependabotOrgSecrets[org.Login][name]
	var ids []int
	if sec != nil {
		ids = append([]int(nil), sec.SelectedRepoIDs...)
	}
	s.store.Mu.RUnlock()

	if sec == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeDependabotSelectedReposResponse(w, r, ids)
}

func (s *Server) handleSetDependabotOrgSecretRepos(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	var body struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}

	s.store.Mu.RLock()
	sec := s.store.DependabotOrgSecrets[org.Login][name]
	s.store.Mu.RUnlock()
	if sec == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, id := range body.SelectedRepositoryIDs {
		if s.store.GetRepoByID(id) == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}

	s.store.SetDependabotOrgSecretSelectedRepos(org.Login, name, body.SelectedRepositoryIDs)
	w.WriteHeader(http.StatusNoContent)
}

// shared helpers

func (s *Server) validateDependabotSecretKeyID(w http.ResponseWriter, keyID string) bool {
	if keyID == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "key_id is required")
		return false
	}
	kp, err := s.store.ActionsKeyPair()
	if err != nil {
		s.logger.Error().Err(err).Msg("dependabot secrets keypair unavailable")
		writeGHError(w, http.StatusInternalServerError, "Internal Server Error")
		return false
	}
	if keyID != kp.KeyID {
		writeGHError(w, http.StatusUnprocessableEntity, "key_id does not match the current public key; fetch the public-key endpoint and re-encrypt")
		return false
	}
	return true
}

func dependabotSecretJSON(sec *store.DependabotSecret) map[string]interface{} {
	return map[string]interface{}{
		"name":       sec.Name,
		"created_at": sec.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": sec.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func dependabotOrgSecretJSON(sec *store.DependabotOrgSecret, orgLogin, baseURL string) map[string]interface{} {
	out := dependabotSecretJSON(&sec.DependabotSecret)
	out["visibility"] = sec.Visibility
	if sec.Visibility == "selected" {
		out["selected_repositories_url"] = baseURL + "/api/v3/orgs/" + orgLogin + "/dependabot/secrets/" + sec.Name + "/repositories"
	}
	return out
}

func sortedDependabotSecretsJSON(m map[string]*store.DependabotSecret) []map[string]interface{} {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		out = append(out, dependabotSecretJSON(m[n]))
	}
	return out
}

func (s *Server) writeDependabotSelectedReposResponse(w http.ResponseWriter, r *http.Request, ids []int) {
	s.store.Mu.RLock()
	repos := make([]*store.Repo, 0, len(ids))
	for _, id := range ids {
		if repo := s.store.Repos[id]; repo != nil {
			repos = append(repos, repo)
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })

	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(repos))
	for _, repo := range repos {
		out = append(out, dependabotMinimalRepoJSON(repo, s.store, base))
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":  len(out),
		"repositories": paged,
	})
}

// dependabotMinimalRepoJSON renders the minimal-repository shape the
// selected-repositories response's OpenAPI schema requires.
func dependabotMinimalRepoJSON(repo *store.Repo, st *store.Store, baseURL string) map[string]interface{} {
	out := store.RepoToJSON(repo, st, baseURL)
	delete(out, "has_pull_requests")
	return out
}

// org alerts and repository access

func (s *Server) handleListDependabotOrgAlerts(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	alerts := s.store.ListDependabotAlertsByOrg(org.ID, q.Get("state"), q.Get("ecosystem"), q.Get("package"), q.Get("sort"), q.Get("direction"))
	page := paginateAndLink(w, r, alerts)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(page))
	for _, a := range page {
		repo := s.store.GetRepoByFullName(a.RepoKey)
		if repo == nil {
			continue
		}
		alertJSON := dependabotAlertToJSON(a, baseURL, repo, s.store)
		alertJSON["repository"] = simpleRepoJSON(repo, s.store, baseURL)
		out = append(out, alertJSON)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetDependabotRepositoryAccess(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	ids := s.store.GetDependabotRepositoryAccess(org.Login)
	repos := s.dependabotAccessibleRepos(r, ids)
	repos = paginateAndLink(w, r, repos)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"default_level":           s.store.GetDependabotRepositoryAccessDefaultLevel(org.Login),
		"accessible_repositories": repos,
	})
}

func (s *Server) handleSetDependabotRepositoryAccessDefaultLevel(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	var body struct {
		DefaultLevel string `json:"default_level"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.DefaultLevel != "public" && body.DefaultLevel != "internal" {
		store.WriteGHValidationError(w, "DependabotRepositoryAccess", "default_level", "invalid")
		return
	}
	s.store.SetDependabotRepositoryAccessDefaultLevel(org.Login, body.DefaultLevel)
	w.WriteHeader(http.StatusNoContent)
}

// dependabotOrgSecretSelectionChange adapts the shared per-repository selection
// core to the org Dependabot secrets table.
func (s *Server) dependabotOrgSecretSelectionChange(w http.ResponseWriter, r *http.Request, add bool) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))
	s.handleOrgSelectionChange(w, r, name, add,
		func() store.OrgScopedItem {
			if sec := s.store.DependabotOrgSecrets[org.Login][name]; sec != nil {
				return sec
			}
			return nil
		},
		func() {
			if s.store.Persist != nil {
				s.store.Persist.MustPut("dependabot_org_secrets", org.Login, s.store.DependabotOrgSecrets[org.Login])
			}
		})
}

func (s *Server) handleAddDependabotOrgSecretRepo(w http.ResponseWriter, r *http.Request) {
	s.dependabotOrgSecretSelectionChange(w, r, true)
}

func (s *Server) handleRemoveDependabotOrgSecretRepo(w http.ResponseWriter, r *http.Request) {
	s.dependabotOrgSecretSelectionChange(w, r, false)
}

func (s *Server) handleUpdateDependabotRepositoryAccess(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForDependabot(w, r)
	if !ok {
		return
	}

	var body struct {
		RepositoryIDsToAdd    []int `json:"repository_ids_to_add"`
		RepositoryIDsToRemove []int `json:"repository_ids_to_remove"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}

	existing := s.store.GetDependabotRepositoryAccess(org.Login)
	set := make(map[int]struct{}, len(existing))
	for _, id := range existing {
		set[id] = struct{}{}
	}
	for _, id := range body.RepositoryIDsToAdd {
		if s.store.GetRepoByID(id) == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		set[id] = struct{}{}
	}
	for _, id := range body.RepositoryIDsToRemove {
		delete(set, id)
	}

	ids := make([]int, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	s.store.SetDependabotRepositoryAccess(org.Login, ids)
	w.WriteHeader(http.StatusNoContent)
}

// dependabotAccessibleRepos renders the given repository IDs, id-sorted,
// omitting any that do not resolve.
func (s *Server) dependabotAccessibleRepos(r *http.Request, ids []int) []map[string]interface{} {
	s.store.Mu.RLock()
	repos := make([]*store.Repo, 0, len(ids))
	for _, id := range ids {
		if repo := s.store.Repos[id]; repo != nil {
			repos = append(repos, repo)
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })

	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(repos))
	for _, repo := range repos {
		out = append(out, simpleRepoJSON(repo, s.store, base))
	}
	return out
}

// dependabotDismisserJSON renders the dismissing account, or null when the alert
// stands or that account is gone.
func dependabotDismisserJSON(a *store.DependabotAlert, st *store.Store, baseURL string) interface{} {
	if a.DismissedByLogin == "" || st == nil {
		return nil
	}
	user := st.LookupUserByLogin(a.DismissedByLogin)
	if user == nil {
		return nil
	}
	return store.UserToJSON(user, baseURL)
}
