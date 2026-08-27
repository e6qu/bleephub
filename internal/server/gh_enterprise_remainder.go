package bleephub

import (
	"crypto/sha256"
	"encoding/base64"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHEnterpriseRemainderRoutes() {
	s.registerGHSecurityReviewRoutes()

	s.route("GET /api/v3/orgs/{org}/credential-authorizations",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleListOrgCredentialAuthorizations))
	s.route("DELETE /api/v3/orgs/{org}/credential-authorizations/{credential_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleDeleteOrgCredentialAuthorization))

	s.route("GET /api/v3/organizations/{organization_id}/org-properties/values",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleGetOrganizationPropertyValuesByID))
	s.route("PATCH /api/v3/organizations/{organization_id}/org-properties/values",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleSetOrganizationPropertyValuesByID))

	s.route("GET /api/v3/orgs/{org}/settings/billing/advanced-security",
		s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleOrgAdvancedSecurityBilling))

	s.route("PUT /api/v3/repos/{owner}/{repo}/lfs",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleEnableRepoLFS))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/lfs",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDisableRepoLFS))
}

func stableCredentialAuthorizationID(kind, key string) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(kind))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(key))
	return int(hash.Sum32()%900_000_000) + 1
}

type credentialAuthorizationRecord struct {
	ID      int
	Kind    string
	MapKey  string
	Token   *store.Token
	User    *store.User
	UserKey *store.UserKey
}

func (s *Server) orgCredentialAuthorizationRecords(org *store.Org) []credentialAuthorizationRecord {
	s.store.Mu.RLock()
	memberIDs := map[int]bool{}
	users := map[int]*store.User{}
	for key, membership := range s.store.Memberships {
		if strings.HasPrefix(key, org.Login+"/") && membership.State == store.MembershipStateActive {
			memberIDs[membership.UserID] = true
		}
	}
	records := make([]credentialAuthorizationRecord, 0)
	for mapKey, token := range s.store.Tokens {
		if token == nil || !memberIDs[token.UserID] {
			continue
		}
		if user := s.store.Users[token.UserID]; user != nil {
			tokenCopy, userCopy := *token, *user
			records = append(records, credentialAuthorizationRecord{
				ID: stableCredentialAuthorizationID("token", mapKey), Kind: "token",
				MapKey: mapKey, Token: &tokenCopy, User: &userCopy,
			})
		}
	}
	for userID := range memberIDs {
		if user := s.store.Users[userID]; user != nil {
			copyUser := *user
			users[userID] = &copyUser
		}
	}
	s.store.Mu.RUnlock()

	s.store.Misc.Mu.RLock()
	for _, key := range s.store.Misc.UserKeys {
		if key == nil || !memberIDs[key.UserID] {
			continue
		}
		if user := users[key.UserID]; user != nil {
			keyCopy := *key
			records = append(records, credentialAuthorizationRecord{
				ID: 1_000_000_000 + key.ID, Kind: "ssh", UserKey: &keyCopy, User: user,
			})
		}
	}
	s.store.Misc.Mu.RUnlock()
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records
}

func tokenLastEight(token *store.Token, mapKey string) string {
	value := token.Value
	if value == "" {
		value = strings.TrimPrefix(mapKey, store.OpaquePersistenceKeyPrefix)
	}
	if len(value) > 8 {
		return value[len(value)-8:]
	}
	return value
}

func credentialAuthorizationJSON(record credentialAuthorizationRecord) map[string]interface{} {
	result := map[string]interface{}{
		"login": record.User.Login, "credential_id": record.ID,
		"credential_accessed_at": nil, "authorized_credential_id": nil,
	}
	if record.Kind == "ssh" {
		digest := sha256.Sum256([]byte(record.UserKey.Key))
		result["credential_type"] = "SSH key"
		result["credential_authorized_at"] = record.UserKey.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
		result["fingerprint"] = "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:])
		result["authorized_credential_id"] = record.UserKey.ID
		result["authorized_credential_title"] = record.UserKey.Title
		return result
	}
	scopes := append([]string(nil), strings.Fields(strings.ReplaceAll(record.Token.Scopes, ",", " "))...)
	result["credential_type"] = "personal access token"
	result["credential_authorized_at"] = record.Token.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
	result["token_last_eight"] = tokenLastEight(record.Token, record.MapKey)
	result["scopes"] = scopes
	if record.Token.FineGrainedID != 0 {
		result["authorized_credential_id"] = record.Token.FineGrainedID
	}
	result["authorized_credential_note"] = record.Token.Name
	if record.Token.ExpiresAt != nil {
		result["authorized_credential_expires_at"] = record.Token.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
	} else {
		result["authorized_credential_expires_at"] = nil
	}
	return result
}

func (s *Server) handleListOrgCredentialAuthorizations(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	records := s.orgCredentialAuthorizationRecords(org)
	result := make([]map[string]interface{}, 0, len(records))
	login := r.URL.Query().Get("login")
	for _, record := range records {
		if login == "" || strings.EqualFold(login, record.User.Login) {
			result = append(result, credentialAuthorizationJSON(record))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleDeleteOrgCredentialAuthorization(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("credential_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var selected *credentialAuthorizationRecord
	for _, record := range s.orgCredentialAuthorizationRecords(org) {
		if record.ID == id {
			copyRecord := record
			selected = &copyRecord
			break
		}
	}
	if selected == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if selected.Kind == "ssh" {
		s.store.Misc.Mu.Lock()
		delete(s.store.Misc.UserKeys, selected.UserKey.ID)
		keys := s.store.Misc.KeysByUser[selected.UserKey.UserID]
		for i, key := range keys {
			if key.ID == selected.UserKey.ID {
				s.store.Misc.KeysByUser[selected.UserKey.UserID] = append(keys[:i], keys[i+1:]...)
				break
			}
		}
		if s.store.Misc.Persist != nil {
			s.store.Misc.Persist.MustDelete("user_keys", strconv.Itoa(selected.UserKey.ID))
		}
		s.store.Misc.Mu.Unlock()
	} else {
		s.store.Mu.Lock()
		s.store.DeleteTokenMapKeyLocked(selected.MapKey)
		s.store.Mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resolveOrganizationByNumericPath(w http.ResponseWriter, r *http.Request) *store.Org {
	id, err := strconv.Atoi(r.PathValue("organization_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	org := s.store.GetOrgByID(id)
	if org == nil || !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return org
}

func (s *Server) handleGetOrganizationPropertyValuesByID(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrganizationByNumericPath(w, r)
	if org == nil {
		return
	}
	s.store.Mu.RLock()
	values := s.store.EnterpriseSettings.OrganizationPropertyValues[org.Login]
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		result = append(result, map[string]interface{}{
			"property_name": name, "value": store.CloneCustomPropertyValue(values[name]),
		})
	}
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSetOrganizationPropertyValuesByID(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrganizationByNumericPath(w, r)
	if org == nil {
		return
	}
	var req struct {
		Properties *[]store.CustomPropertyValuePayload `json:"properties"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Properties == nil {
		store.WriteGHValidationError(w, "CustomPropertyValues", "properties", "missing_field")
		return
	}
	s.store.Mu.Lock()
	for _, value := range *req.Properties {
		definition := s.store.EnterpriseSettings.OrganizationCustomProperties[value.PropertyName]
		if definition == nil || validateCustomPropertyValue(definition, value.Value) != nil {
			s.store.Mu.Unlock()
			store.WriteGHValidationError(w, "CustomPropertyValues", "properties", "invalid")
			return
		}
	}
	values := s.store.EnterpriseSettings.OrganizationPropertyValues[org.Login]
	if values == nil {
		values = map[string]interface{}{}
		s.store.EnterpriseSettings.OrganizationPropertyValues[org.Login] = values
	}
	for _, value := range *req.Properties {
		if value.Value == nil {
			delete(values, value.PropertyName)
		} else {
			values[value.PropertyName] = store.CloneCustomPropertyValue(value.Value)
		}
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOrgAdvancedSecurityBilling(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repos := s.store.ListReposForOrg(org.Login, store.RepoListOptions{})
	s.store.Mu.RLock()
	repositories := make([]map[string]interface{}, 0)
	uniqueCommitters := map[int]bool{}
	maximum := 0
	for _, repo := range repos {
		maximum++
		configID := s.store.CodeSecurityRepoAttachments[org.Login][repo.ID]
		config := s.store.CodeSecurityConfigs[org.Login][configID]
		if config == nil || config.AdvancedSecurity == "disabled" {
			continue
		}
		breakdown := []map[string]interface{}{}
		if repo.Owner != nil {
			uniqueCommitters[repo.Owner.ID] = true
			breakdown = append(breakdown, map[string]interface{}{
				"user_login":        repo.Owner.Login,
				"last_pushed_date":  repo.PushedAt.UTC().Format("2006-01-02"),
				"last_pushed_email": repo.Owner.Email,
			})
		}
		repositories = append(repositories, map[string]interface{}{
			"name": repo.FullName, "advanced_security_committers": len(breakdown),
			"advanced_security_committers_breakdown": breakdown,
		})
	}
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_advanced_security_committers": len(uniqueCommitters),
		"total_count":                        len(repositories), "maximum_advanced_security_committers": maximum,
		"purchased_advanced_security_committers": maximum, "repositories": repositories,
	})
}

func (s *Server) setRepoLFS(w http.ResponseWriter, r *http.Request, enabled bool) {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeAdministration, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}
	// repo is a detached snapshot; mutate through UpdateRepo so the change is
	// observed and persisted.
	s.store.UpdateRepo(r.PathValue("owner"), r.PathValue("repo"), func(rp *store.Repo) {
		rp.LFSEnabled = enabled
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnableRepoLFS(w http.ResponseWriter, r *http.Request) {
	s.setRepoLFS(w, r, true)
}

func (s *Server) handleDisableRepoLFS(w http.ResponseWriter, r *http.Request) {
	s.setRepoLFS(w, r, false)
}
