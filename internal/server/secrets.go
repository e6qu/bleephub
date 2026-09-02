package bleephub

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

// actionsItemNameRe is GitHub's name rule for Actions secrets and variables:
// alphanumeric or underscores, not starting with a digit.
var actionsItemNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// actionsItemNameError validates a secret/variable name against GitHub's rules.
// kind is "Secret" or "Variable" for the message. Returns "" when valid.
func actionsItemNameError(kind, name string) string {
	if !actionsItemNameRe.MatchString(name) {
		return kind + " names can only contain alphanumeric characters or underscores and must not start with a number."
	}
	if strings.HasPrefix(strings.ToUpper(name), "GITHUB_") {
		return kind + " names must not start with the GITHUB_ prefix."
	}
	return ""
}

// validOrgItemVisibility reports whether v is a legal org-level visibility.
func validOrgItemVisibility(v string) bool {
	switch v {
	case "all", "private", "selected":
		return true
	}
	return false
}

// auditActor names the acting user for audit events; installation tokens have no
// user, so it may be empty.
func auditActor(r *http.Request) string {
	if u := ghUserFromContext(r.Context()); u != nil {
		return u.Login
	}
	return ""
}

func (s *Server) registerSecretsRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/secrets", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleListSecrets))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/secrets/public-key", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleGetRepoSecretsPublicKey))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleGetSecret))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handlePutSecret))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handleDeleteSecret))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/organization-secrets", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleListRepoOrgSecrets))

	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}/secrets", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleListEnvSecrets))
	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}/secrets/public-key", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleGetEnvSecretsPublicKey))
	s.route("GET /api/v3/repos/{owner}/{repo}/environments/{env_name}/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleGetEnvSecret))
	s.route("PUT /api/v3/repos/{owner}/{repo}/environments/{env_name}/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handlePutEnvSecret))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/environments/{env_name}/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handleDeleteEnvSecret))

	// Environment secrets addressed by repository id. go-github's only typed
	// environment-secret methods build this path, so without it an unmodified
	// go-github cannot read or write an environment secret. Served by rewriting to
	// the canonical owner/name path, so an id-addressed request cannot acquire
	// access the named form would refuse.
	for _, suffix := range []string{"", "/public-key", "/{secret_name}"} {
		s.route("GET /api/v3/repositories/{repository_id}/environments/{env_name}/secrets"+suffix, s.rewriteRepositoryIDPath)
	}
	s.route("PUT /api/v3/repositories/{repository_id}/environments/{env_name}/secrets/{secret_name}", s.rewriteRepositoryIDPath)
	s.route("DELETE /api/v3/repositories/{repository_id}/environments/{env_name}/secrets/{secret_name}", s.rewriteRepositoryIDPath)

	s.route("GET /api/v3/orgs/{org}/actions/secrets", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleListOrgSecrets))
	s.route("GET /api/v3/orgs/{org}/actions/secrets/public-key", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleGetOrgSecretsPublicKey))
	s.route("GET /api/v3/orgs/{org}/actions/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleGetOrgSecret))
	s.route("PUT /api/v3/orgs/{org}/actions/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handlePutOrgSecret))
	s.route("DELETE /api/v3/orgs/{org}/actions/secrets/{secret_name}", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handleDeleteOrgSecret))
	s.route("GET /api/v3/orgs/{org}/actions/secrets/{secret_name}/repositories", s.requirePerm(store.ScopeSecrets, store.PermRead, s.handleListOrgSecretRepos))
	s.route("PUT /api/v3/orgs/{org}/actions/secrets/{secret_name}/repositories", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handleSetOrgSecretRepos))
	s.route("PUT /api/v3/orgs/{org}/actions/secrets/{secret_name}/repositories/{repository_id}", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handleAddOrgSecretRepo))
	s.route("DELETE /api/v3/orgs/{org}/actions/secrets/{secret_name}/repositories/{repository_id}", s.requirePerm(store.ScopeSecrets, store.PermWrite, s.handleRemoveOrgSecretRepo))

	s.registerVariablesRoutes()
}

// writeActionsPublicKey serves the sealed-box public key. Every scope's
// public-key endpoint returns the same pair: bleephub has one Actions key.
func (s *Server) writeActionsPublicKey(w http.ResponseWriter) {
	kp, err := s.store.ActionsKeyPair()
	if err != nil {
		s.logger.Error().Err(err).Msg("actions secrets keypair unavailable")
		writeGHError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key_id": kp.KeyID,
		"key":    kp.PublicKey,
	})
}

// decryptSealedSecret enforces the secrets wire contract: key_id must name the
// current key and encrypted_value must be a libsodium sealed box against it.
// Writes a 422 and returns false on any violation.
func (s *Server) decryptSealedSecret(w http.ResponseWriter, encryptedValue, keyID string) (string, bool) {
	if encryptedValue == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "encrypted_value is required")
		return "", false
	}
	if keyID == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "key_id is required")
		return "", false
	}
	kp, err := s.store.ActionsKeyPair()
	if err != nil {
		s.logger.Error().Err(err).Msg("actions secrets keypair unavailable")
		writeGHError(w, http.StatusInternalServerError, "Internal Server Error")
		return "", false
	}
	if keyID != kp.KeyID {
		writeGHError(w, http.StatusUnprocessableEntity, "key_id does not match the current public key; fetch the public-key endpoint and re-encrypt")
		return "", false
	}
	plain, err := s.store.OpenSealedSecret(encryptedValue)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "encrypted_value could not be decrypted: it must be a libsodium sealed box for the announced public key")
		return "", false
	}
	return plain, true
}

func secretJSON(sec *store.Secret) map[string]interface{} {
	return map[string]interface{}{
		"name":       sec.Name,
		"created_at": sec.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": sec.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// sortedSecretsJSON renders a scope's secrets sorted by name. Call with the
// store lock held.
func sortedSecretsJSON(m map[string]*store.Secret) []map[string]interface{} {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		out = append(out, secretJSON(m[n]))
	}
	return out
}

// sealedSecretBody is the PUT body for repository and environment secrets.
type sealedSecretBody struct {
	EncryptedValue string `json:"encrypted_value"`
	KeyID          string `json:"key_id"`
}

// upsertSecret creates or updates a secret in one scope map, persisting the
// collection. Returns true when the secret was created.
func (s *Server) upsertSecret(table map[string]map[string]*store.Secret, bucket, key, name, value string) bool {
	now := time.Now().UTC()
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	m := table[key]
	if m == nil {
		m = make(map[string]*store.Secret)
		table[key] = m
	}
	existing := m[name]
	if existing != nil {
		existing.Value = value
		existing.UpdatedAt = now
	} else {
		m[name] = &store.Secret{Name: name, CreatedAt: now, UpdatedAt: now, Value: value}
	}
	if s.store.Persist != nil {
		s.store.Persist.MustPut(bucket, key, m)
	}
	return existing == nil
}

// deleteSecret removes a secret from one scope map, persisting the remainder (or
// deleting the row when empty). Returns whether the secret existed.
func (s *Server) deleteSecret(table map[string]map[string]*store.Secret, bucket, key, name string) bool {
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	m, ok := table[key]
	if !ok || m[name] == nil {
		return false
	}
	delete(m, name)
	if s.store.Persist != nil {
		if len(m) > 0 {
			s.store.Persist.MustPut(bucket, key, m)
		} else {
			s.store.Persist.MustDelete(bucket, key)
		}
	}
	return true
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.requireRepoAdmin(w, r)
	if !ok {
		return
	}

	s.store.Mu.RLock()
	list := sortedSecretsJSON(s.store.RepoSecrets[repo.FullName])
	s.store.Mu.RUnlock()

	paged := paginateAndLink(w, r, list)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(list),
		"secrets":     paged,
	})
}

func (s *Server) handleGetRepoSecretsPublicKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRepoAdmin(w, r); !ok {
		return
	}
	s.writeActionsPublicKey(w)
}

func (s *Server) handleGetSecret(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.requireRepoAdmin(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	s.store.Mu.RLock()
	sec := s.store.RepoSecrets[repo.FullName][name]
	var body map[string]interface{}
	if sec != nil {
		body = secretJSON(sec)
	}
	s.store.Mu.RUnlock()

	if body == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.requireRepoAdmin(w, r)
	if !ok {
		return
	}
	rawName := r.PathValue("secret_name")
	if msg := actionsItemNameError("Secret", rawName); msg != "" {
		writeGHError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	name := strings.ToUpper(rawName)

	var body sealedSecretBody
	if !decodeJSONBody(w, r, &body) {
		return
	}
	plain, ok := s.decryptSealedSecret(w, body.EncryptedValue, body.KeyID)
	if !ok {
		return
	}

	created := s.upsertSecret(s.store.RepoSecrets, "repo_secrets", repo.FullName, name, plain)
	s.recordAuditEvent("secret.create", auditActor(r), "", map[string]interface{}{
		"scope": "repository", "repo": repo.FullName, "secret_name": name,
	})
	if created {
		writeJSON(w, http.StatusCreated, map[string]interface{}{})
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.requireRepoAdmin(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	if !s.deleteSecret(s.store.RepoSecrets, "repo_secrets", repo.FullName, name) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("secret.destroy", auditActor(r), "", map[string]interface{}{
		"scope": "repository", "repo": repo.FullName, "secret_name": name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleListRepoOrgSecrets lists the org secrets visible to a repository. The
// documented item shape is the plain actions-secret; visibility metadata stays
// on the org endpoints.
func (s *Server) handleListRepoOrgSecrets(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.requireRepoAdmin(w, r)
	if !ok {
		return
	}

	list := make([]map[string]interface{}, 0)
	if org := s.store.GetOrg(r.PathValue("owner")); org != nil {
		s.store.Mu.RLock()
		visible := make(map[string]*store.Secret)
		for name, sec := range s.store.OrgSecrets[org.Login] {
			if actions.OrgItemVisibleToRepo(sec.Visibility, sec.SelectedRepoIDs, repo) {
				visible[name] = &sec.Secret
			}
		}
		list = sortedSecretsJSON(visible)
		s.store.Mu.RUnlock()
	}

	paged := paginateAndLink(w, r, list)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(list),
		"secrets":     paged,
	})
}

// resolveEnvScope resolves an environment-scoped secrets/variables request to
// its repo key and scope key, writing a 404 when the repo or environment does
// not exist. GitHub never auto-creates an environment here (a PUT against a
// missing one is a 404). Demands the same repository admin rights as the repo
// surface.
func (s *Server) resolveEnvScope(w http.ResponseWriter, r *http.Request) (repoKey, scopeKey, envName string, ok bool) {
	repo, ok := s.requireRepoAdmin(w, r)
	if !ok {
		return "", "", "", false
	}
	envName = r.PathValue("env_name")
	if s.store.Deployments.GetEnvironment(repo.ID, envName) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return "", "", "", false
	}
	return repo.FullName, store.EnvScopeKey(repo.FullName, envName), envName, true
}

func (s *Server) handleListEnvSecrets(w http.ResponseWriter, r *http.Request) {
	_, scopeKey, _, ok := s.resolveEnvScope(w, r)
	if !ok {
		return
	}

	s.store.Mu.RLock()
	list := sortedSecretsJSON(s.store.EnvSecrets[scopeKey])
	s.store.Mu.RUnlock()

	paged := paginateAndLink(w, r, list)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(list),
		"secrets":     paged,
	})
}

func (s *Server) handleGetEnvSecretsPublicKey(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := s.resolveEnvScope(w, r); !ok {
		return
	}
	s.writeActionsPublicKey(w)
}

func (s *Server) handleGetEnvSecret(w http.ResponseWriter, r *http.Request) {
	_, scopeKey, _, ok := s.resolveEnvScope(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	s.store.Mu.RLock()
	sec := s.store.EnvSecrets[scopeKey][name]
	var body map[string]interface{}
	if sec != nil {
		body = secretJSON(sec)
	}
	s.store.Mu.RUnlock()

	if body == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePutEnvSecret(w http.ResponseWriter, r *http.Request) {
	repoKey, scopeKey, envName, ok := s.resolveEnvScope(w, r)
	if !ok {
		return
	}
	rawName := r.PathValue("secret_name")
	if msg := actionsItemNameError("Secret", rawName); msg != "" {
		writeGHError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	name := strings.ToUpper(rawName)

	var body sealedSecretBody
	if !decodeJSONBody(w, r, &body) {
		return
	}
	plain, ok := s.decryptSealedSecret(w, body.EncryptedValue, body.KeyID)
	if !ok {
		return
	}

	created := s.upsertSecret(s.store.EnvSecrets, "env_secrets", scopeKey, name, plain)
	s.recordAuditEvent("secret.create", auditActor(r), "", map[string]interface{}{
		"scope": "environment", "repo": repoKey, "environment": envName, "secret_name": name,
	})
	writeSecretUpsert(w, created)
}

// writeSecretUpsert writes the documented secret-PUT response: 201 with an
// empty-object body when new, 204 when it replaced one. 201 carries a body — a
// zero-length one is not an empty object.
func writeSecretUpsert(w http.ResponseWriter, created bool) {
	if created {
		writeJSON(w, http.StatusCreated, map[string]interface{}{})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteEnvSecret(w http.ResponseWriter, r *http.Request) {
	repoKey, scopeKey, envName, ok := s.resolveEnvScope(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	if !s.deleteSecret(s.store.EnvSecrets, "env_secrets", scopeKey, name) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("secret.destroy", auditActor(r), "", map[string]interface{}{
		"scope": "environment", "repo": repoKey, "environment": envName, "secret_name": name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// resolveOrgForActions resolves {org} or writes a 404.
func (s *Server) resolveOrgForActions(w http.ResponseWriter, r *http.Request) (*store.Org, bool) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	return org, true
}

// orgSecretJSON renders the organization-actions-secret shape;
// selected_repositories_url appears only for visibility "selected".
func orgSecretJSON(sec *store.OrgSecret, orgLogin, baseURL string) map[string]interface{} {
	out := secretJSON(&sec.Secret)
	out["visibility"] = sec.Visibility
	if sec.Visibility == "selected" {
		out["selected_repositories_url"] = baseURL + "/api/v3/orgs/" + orgLogin + "/actions/secrets/" + sec.Name + "/repositories"
	}
	return out
}

func (s *Server) handleListOrgSecrets(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForActions(w, r)
	if !ok {
		return
	}
	base := s.baseURL(r)

	s.store.Mu.RLock()
	m := s.store.OrgSecrets[org.Login]
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	list := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		list = append(list, orgSecretJSON(m[n], org.Login, base))
	}
	s.store.Mu.RUnlock()

	paged := paginateAndLink(w, r, list)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(list),
		"secrets":     paged,
	})
}

func (s *Server) handleGetOrgSecretsPublicKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolveOrgForActions(w, r); !ok {
		return
	}
	s.writeActionsPublicKey(w)
}

func (s *Server) handleGetOrgSecret(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForActions(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))
	base := s.baseURL(r)

	s.store.Mu.RLock()
	sec := s.store.OrgSecrets[org.Login][name]
	var body map[string]interface{}
	if sec != nil {
		body = orgSecretJSON(sec, org.Login, base)
	}
	s.store.Mu.RUnlock()

	if body == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePutOrgSecret(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForActions(w, r)
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
		sealedSecretBody
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
	plain, ok := s.decryptSealedSecret(w, body.EncryptedValue, body.KeyID)
	if !ok {
		return
	}

	ids := body.SelectedRepositoryIDs
	if body.Visibility != "selected" {
		ids = nil
	}
	if bad, ok := s.firstNonOrgRepo(org.Login, ids); !ok {
		writeGHError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("Validation Failed: repository %d does not belong to the organization", bad))
		return
	}

	now := time.Now().UTC()
	s.store.Mu.Lock()
	m := s.store.OrgSecrets[org.Login]
	if m == nil {
		m = make(map[string]*store.OrgSecret)
		s.store.OrgSecrets[org.Login] = m
	}
	existing := m[name]
	if existing != nil {
		existing.Value = plain
		existing.UpdatedAt = now
		existing.Visibility = body.Visibility
		existing.SelectedRepoIDs = ids
	} else {
		m[name] = &store.OrgSecret{
			Secret:          store.Secret{Name: name, CreatedAt: now, UpdatedAt: now, Value: plain},
			Visibility:      body.Visibility,
			SelectedRepoIDs: ids,
		}
	}
	if s.store.Persist != nil {
		s.store.Persist.MustPut("org_secrets", org.Login, m)
	}
	s.store.Mu.Unlock()

	s.recordAuditEvent("secret.create", auditActor(r), org.Login, map[string]interface{}{
		"scope": "organization", "org": org.Login, "secret_name": name, "visibility": body.Visibility,
	})
	if existing == nil {
		writeJSON(w, http.StatusCreated, map[string]interface{}{})
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleDeleteOrgSecret(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForActions(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	s.store.Mu.Lock()
	m := s.store.OrgSecrets[org.Login]
	existed := m[name] != nil
	if existed {
		delete(m, name)
		if s.store.Persist != nil {
			if len(m) > 0 {
				s.store.Persist.MustPut("org_secrets", org.Login, m)
			} else {
				s.store.Persist.MustDelete("org_secrets", org.Login)
			}
		}
	}
	s.store.Mu.Unlock()

	if !existed {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("secret.destroy", auditActor(r), org.Login, map[string]interface{}{
		"scope": "organization", "org": org.Login, "secret_name": name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// writeSelectedReposResponse renders {total_count, repositories} for the org
// selected-repositories endpoints, dropping ids whose repository is gone.
func (s *Server) writeSelectedReposResponse(w http.ResponseWriter, r *http.Request, ids []int) {
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
		out = append(out, store.RepoToJSON(repo, s.store, base))
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":  len(out),
		"repositories": paged,
	})
}

// repoIDPathValue parses {repository_id}; a non-numeric id is a 404.
func repoIDPathValue(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return 0, false
	}
	return id, true
}

func (s *Server) handleListOrgSecretRepos(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForActions(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))

	s.store.Mu.RLock()
	sec := s.store.OrgSecrets[org.Login][name]
	var ids []int
	if sec != nil {
		ids = append([]int(nil), sec.SelectedRepoIDs...)
	}
	s.store.Mu.RUnlock()

	if sec == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeSelectedReposResponse(w, r, ids)
}

func (s *Server) handleSetOrgSecretRepos(w http.ResponseWriter, r *http.Request) {
	org, ok := s.resolveOrgForActions(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))
	s.setOrgItemSelectedRepos(w, r, name, false,
		func() store.OrgScopedItem {
			if sec := s.store.OrgSecrets[org.Login][name]; sec != nil {
				return sec
			}
			return nil
		},
		func() {
			if s.store.Persist != nil {
				s.store.Persist.MustPut("org_secrets", org.Login, s.store.OrgSecrets[org.Login])
			}
		})
}

// setOrgItemSelectedRepos implements the set-selected-repositories endpoints
// (PUT .../{name}/repositories) shared by org secrets and variables: 404 for a
// missing item or unknown repository id, and — where the surface documents it
// (variables) — 409 unless visibility is "selected". lookup and persistLocked
// run under the store write lock.
// repoOwnedByOrgLocked reports whether repository id exists and is owned by
// orgLogin. Caller holds s.store.Mu (read or write). An org's selected-repository
// set may only name repos the org owns; a foreign id would let an org admin
// enumerate arbitrary private repositories through the selected-repos listing.
func (s *Server) repoOwnedByOrgLocked(orgLogin string, id int) bool {
	repo := s.store.Repos[id]
	if repo == nil {
		return false
	}
	owner, _, _ := strings.Cut(repo.FullName, "/")
	return strings.EqualFold(owner, orgLogin)
}

// firstNonOrgRepo returns the first selected repository id that is not a repo
// owned by orgLogin (nonexistent or owned by another account); ok is false when
// such an id exists. GitHub rejects those with 422. Caller must NOT hold s.store.Mu.
func (s *Server) firstNonOrgRepo(orgLogin string, ids []int) (int, bool) {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, id := range ids {
		if !s.repoOwnedByOrgLocked(orgLogin, id) {
			return id, false
		}
	}
	return 0, true
}

func (s *Server) setOrgItemSelectedRepos(w http.ResponseWriter, r *http.Request, name string, requireSelected bool,
	lookup func() store.OrgScopedItem, persistLocked func()) {
	var body struct {
		SelectedRepositoryIDs []int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}

	s.store.Mu.Lock()
	item := lookup()
	if item == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if requireSelected && item.ItemVisibility() != "selected" {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusConflict, "Conflict: visibility of "+name+" is not set to selected")
		return
	}
	for _, id := range body.SelectedRepositoryIDs {
		if s.store.Repos[id] == nil {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		if !s.repoOwnedByOrgLocked(r.PathValue("org"), id) {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed: repository does not belong to the organization")
			return
		}
	}
	item.SetSelectedIDs(append([]int(nil), body.SelectedRepositoryIDs...))
	item.TouchUpdated(time.Now().UTC())
	persistLocked()
	s.store.Mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// handleOrgSelectionChange implements the per-repository add/remove endpoints
// (PUT/DELETE .../{name}/repositories/{repository_id}) for org secrets and
// variables: 404 for a missing item or (on add) unknown repository, 409 unless
// visibility is "selected". lookup and persistLocked run under the store write
// lock.
func (s *Server) handleOrgSelectionChange(w http.ResponseWriter, r *http.Request, name string, add bool,
	lookup func() store.OrgScopedItem, persistLocked func()) {
	id, ok := repoIDPathValue(w, r)
	if !ok {
		return
	}

	s.store.Mu.Lock()
	item := lookup()
	if item == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if item.ItemVisibility() != "selected" {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusConflict, "Conflict: visibility of "+name+" is not set to selected")
		return
	}
	if add {
		if s.store.Repos[id] == nil {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		if !s.repoOwnedByOrgLocked(r.PathValue("org"), id) {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed: repository does not belong to the organization")
			return
		}
	}

	ids := item.SelectedIDs()
	changed := false
	if add {
		present := false
		for _, existing := range ids {
			if existing == id {
				present = true
				break
			}
		}
		if !present {
			ids = append(ids, id)
			changed = true
		}
	} else {
		kept := ids[:0]
		for _, existing := range ids {
			if existing != id {
				kept = append(kept, existing)
			}
		}
		if len(kept) != len(ids) {
			ids = kept
			changed = true
		}
	}
	if changed {
		item.SetSelectedIDs(ids)
		item.TouchUpdated(time.Now().UTC())
		persistLocked()
	}
	s.store.Mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) orgSecretSelectionChange(w http.ResponseWriter, r *http.Request, add bool) {
	org, ok := s.resolveOrgForActions(w, r)
	if !ok {
		return
	}
	name := strings.ToUpper(r.PathValue("secret_name"))
	s.handleOrgSelectionChange(w, r, name, add,
		func() store.OrgScopedItem {
			if sec := s.store.OrgSecrets[org.Login][name]; sec != nil {
				return sec
			}
			return nil
		},
		func() {
			if s.store.Persist != nil {
				s.store.Persist.MustPut("org_secrets", org.Login, s.store.OrgSecrets[org.Login])
			}
		})
}

func (s *Server) handleAddOrgSecretRepo(w http.ResponseWriter, r *http.Request) {
	s.orgSecretSelectionChange(w, r, true)
}

func (s *Server) handleRemoveOrgSecretRepo(w http.ResponseWriter, r *http.Request) {
	s.orgSecretSelectionChange(w, r, false)
}

// rewriteRepositoryIDPath serves a /repositories/{repository_id}/… request by
// rewriting it to the equivalent /repos/{owner}/{repo}/… path and re-dispatching
// through the same mux, so an id-addressed request can never acquire access the
// named form would refuse and cannot drift from the named route.
func (s *Server) rewriteRepositoryIDPath(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := s.store.GetRepoByID(id)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Only the addressing segment is replaced; everything after it carries across
	// untouched.
	rest := strings.TrimPrefix(r.URL.Path, "/api/v3/repositories/"+r.PathValue("repository_id"))
	rewritten := r.Clone(r.Context())
	rewritten.URL.Path = "/api/v3/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + rest
	rewritten.RequestURI = ""
	s.mux.ServeHTTP(w, rewritten)
}
