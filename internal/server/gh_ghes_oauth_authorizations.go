package bleephub

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type legacyAuthorizationRequest struct {
	Scopes       []string `json:"scopes"`
	AddScopes    []string `json:"add_scopes"`
	RemoveScopes []string `json:"remove_scopes"`
	Note         *string  `json:"note"`
	NoteURL      *string  `json:"note_url"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Fingerprint  *string  `json:"fingerprint"`
}

type legacyAuthorizationRef struct {
	kind     string
	mapKey   string
	pat      *Token
	oauth    *UserToServerToken
	user     *User
	identity string
}

func (s *Server) legacyAuthorizationUser(w http.ResponseWriter, r *http.Request) *User {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
	}
	return user
}

func (s *Server) listLegacyAuthorizationRefs(userID int) []legacyAuthorizationRef {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	refs := make([]legacyAuthorizationRef, 0)
	user := s.store.Users[userID]
	for key, token := range s.store.Tokens {
		if token.UserID != userID {
			continue
		}
		copy := *token
		identity := tokenIdentity(key, token)
		refs = append(refs, legacyAuthorizationRef{
			kind: "pat", mapKey: key, pat: &copy, user: user, identity: identity,
		})
	}
	for key, token := range s.store.UserToServerTokens {
		if token.UserID != userID {
			continue
		}
		copy := cloneUserToServerToken(token)
		refs = append(refs, legacyAuthorizationRef{
			kind: "oauth", mapKey: key, oauth: copy, user: user, identity: key,
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		return authorizationID(refs[i].identity) < authorizationID(refs[j].identity)
	})
	return refs
}

func (s *Server) legacyAuthorizationJSON(ref legacyAuthorizationRef, reveal bool) map[string]interface{} {
	if ref.kind == "pat" {
		return classicAuthorizationJSON(ref.mapKey, ref.pat, ref.user, reveal)
	}
	out := oauthTokenInspectionJSON(s.store, ref.oauth, ref.user)
	if !reveal {
		out["token"] = ""
	}
	out["note"] = nullableString(ref.oauth.Note)
	out["note_url"] = nullableString(ref.oauth.NoteURL)
	out["fingerprint"] = nullableString(ref.oauth.Fingerprint)
	return out
}

func (s *Server) legacyAuthorizationByID(userID, id int) (legacyAuthorizationRef, bool) {
	for _, ref := range s.listLegacyAuthorizationRefs(userID) {
		if authorizationID(ref.identity) == id {
			return ref, true
		}
	}
	return legacyAuthorizationRef{}, false
}

func (s *Server) handleListLegacyAuthorizations(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	refs := s.listLegacyAuthorizationRefs(user.ID)
	out := make([]map[string]interface{}, 0, len(refs))
	for _, ref := range refs {
		out = append(out, s.legacyAuthorizationJSON(ref, false))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleCreateLegacyAuthorization(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	var req legacyAuthorizationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.ClientID != "" {
		s.createLegacyOAuthAuthorization(w, r, user, req.ClientID, req, false)
		return
	}
	token := s.store.CreateToken(user.ID, strings.Join(req.Scopes, ", "))
	s.store.mu.Lock()
	token.Note = valueOrEmpty(req.Note)
	token.NoteURL = valueOrEmpty(req.NoteURL)
	token.Fingerprint = valueOrEmpty(req.Fingerprint)
	s.store.persistTokenLocked(token)
	copy := *token
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, classicAuthorizationJSON(token.Value, &copy, user, true))
}

func (s *Server) handleGetOrCreateLegacyAuthorization(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	var req legacyAuthorizationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if fingerprint := r.PathValue("fingerprint"); fingerprint != "" {
		req.Fingerprint = &fingerprint
	}
	s.createLegacyOAuthAuthorization(w, r, user, r.PathValue("client_id"), req, true)
}

func (s *Server) createLegacyOAuthAuthorization(
	w http.ResponseWriter,
	r *http.Request,
	user *User,
	clientID string,
	req legacyAuthorizationRequest,
	getOrCreate bool,
) {
	if !s.clientCredentialsVerify(clientID, req.ClientSecret) {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	fingerprint := valueOrEmpty(req.Fingerprint)
	for _, ref := range s.listLegacyAuthorizationRefs(user.ID) {
		if ref.oauth == nil || legacyOAuthClientID(s.store, ref.oauth) != clientID {
			continue
		}
		if !getOrCreate || ref.oauth.Fingerprint == fingerprint {
			writeJSON(w, http.StatusOK, s.legacyAuthorizationJSON(ref, false))
			return
		}
	}
	appID := 0
	oauthClientID := clientID
	if app := s.store.GetAppByClientID(clientID); app != nil {
		appID = app.ID
		oauthClientID = ""
	}
	token, _, err := s.store.CreateUserToServerTokenE(
		user.ID, appID, oauthClientID, strings.Join(req.Scopes, " "), 8*time.Hour, false,
	)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.mu.Lock()
	stored := s.store.UserToServerTokens[token.Token]
	stored.Note = valueOrEmpty(req.Note)
	stored.NoteURL = valueOrEmpty(req.NoteURL)
	stored.Fingerprint = fingerprint
	if s.store.persist != nil {
		s.store.persist.MustPut("user_to_server_tokens", stored.Token, stored)
	}
	copy := cloneUserToServerToken(stored)
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, s.legacyAuthorizationJSON(legacyAuthorizationRef{
		kind: "oauth", mapKey: copy.Token, oauth: copy, user: user, identity: copy.Token,
	}, true))
}

func legacyOAuthClientID(st *Store, token *UserToServerToken) string {
	if token.OAuthAppClientID != "" {
		return token.OAuthAppClientID
	}
	if app := st.GetApp(token.AppID); app != nil {
		return app.ClientID
	}
	return ""
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *Server) handleGetLegacyAuthorization(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("authorization_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	ref, ok := s.legacyAuthorizationByID(user.ID, id)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.legacyAuthorizationJSON(ref, false))
}

func (s *Server) handleUpdateLegacyAuthorization(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("authorization_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	ref, ok := s.legacyAuthorizationByID(user.ID, id)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req legacyAuthorizationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.mu.Lock()
	if ref.kind == "pat" {
		stored := s.store.Tokens[ref.mapKey]
		applyLegacyAuthorizationUpdate(&stored.Scopes, &stored.Note, &stored.NoteURL, &stored.Fingerprint, req)
		s.store.persistTokenLocked(stored)
		copy := *stored
		ref.pat = &copy
	} else {
		stored := s.store.UserToServerTokens[ref.mapKey]
		applyLegacyAuthorizationUpdate(&stored.Scopes, &stored.Note, &stored.NoteURL, &stored.Fingerprint, req)
		if s.store.persist != nil {
			s.store.persist.MustPut("user_to_server_tokens", stored.Token, stored)
		}
		ref.oauth = cloneUserToServerToken(stored)
	}
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, s.legacyAuthorizationJSON(ref, false))
}

func applyLegacyAuthorizationUpdate(
	scopes, note, noteURL, fingerprint *string,
	req legacyAuthorizationRequest,
) {
	current := splitScopes(*scopes)
	if req.Scopes != nil {
		current = append([]string(nil), req.Scopes...)
	}
	for _, add := range req.AddScopes {
		if !legacyContainsString(current, add) {
			current = append(current, add)
		}
	}
	for _, remove := range req.RemoveScopes {
		filtered := current[:0]
		for _, scope := range current {
			if scope != remove {
				filtered = append(filtered, scope)
			}
		}
		current = filtered
	}
	*scopes = strings.Join(current, ", ")
	if req.Note != nil {
		*note = *req.Note
	}
	if req.NoteURL != nil {
		*noteURL = *req.NoteURL
	}
	if req.Fingerprint != nil {
		*fingerprint = *req.Fingerprint
	}
}

func legacyContainsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (s *Server) handleDeleteLegacyAuthorization(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("authorization_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	ref, ok := s.legacyAuthorizationByID(user.ID, id)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.mu.Lock()
	if ref.kind == "pat" {
		s.store.deleteTokenMapKeyLocked(ref.mapKey)
	} else {
		delete(s.store.UserToServerTokens, ref.mapKey)
		if s.store.persist != nil {
			s.store.persist.MustDelete("user_to_server_tokens", ref.mapKey)
		}
	}
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

type legacyGrantRef struct {
	ID        int
	ClientID  string
	Name      string
	URL       string
	User      *User
	Scopes    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s *Server) listLegacyGrants(user *User) []legacyGrantRef {
	byClient := map[string]*legacyGrantRef{}
	for _, ref := range s.listLegacyAuthorizationRefs(user.ID) {
		if ref.oauth == nil {
			continue
		}
		clientID := legacyOAuthClientID(s.store, ref.oauth)
		if clientID == "" {
			continue
		}
		name, appURL := "", ""
		if app := s.store.GetOAuthApp(clientID); app != nil {
			name, appURL = app.Name, app.URL
		} else if app := s.store.GetAppByClientID(clientID); app != nil {
			name, appURL = app.Name, app.ExternalURL
		}
		grant := byClient[clientID]
		if grant == nil {
			grant = &legacyGrantRef{
				ID: authorizationID(strconv.Itoa(user.ID) + ":" + clientID), ClientID: clientID,
				Name: name, URL: appURL, User: user, CreatedAt: ref.oauth.CreatedAt,
				UpdatedAt: ref.oauth.CreatedAt,
			}
			byClient[clientID] = grant
		}
		for _, scope := range splitScopes(ref.oauth.Scopes) {
			if !legacyContainsString(grant.Scopes, scope) {
				grant.Scopes = append(grant.Scopes, scope)
			}
		}
		if ref.oauth.CreatedAt.Before(grant.CreatedAt) {
			grant.CreatedAt = ref.oauth.CreatedAt
		}
		if ref.oauth.CreatedAt.After(grant.UpdatedAt) {
			grant.UpdatedAt = ref.oauth.CreatedAt
		}
	}
	grants := make([]legacyGrantRef, 0, len(byClient))
	for _, grant := range byClient {
		sort.Strings(grant.Scopes)
		grants = append(grants, *grant)
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].ID < grants[j].ID })
	return grants
}

func (s *Server) legacyGrantJSON(grant legacyGrantRef, r *http.Request) map[string]interface{} {
	return map[string]interface{}{
		"id": grant.ID, "url": s.baseURL(r) + "/api/v3/applications/grants/" + strconv.Itoa(grant.ID),
		"app":        map[string]interface{}{"client_id": grant.ClientID, "name": grant.Name, "url": grant.URL},
		"created_at": grant.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": grant.UpdatedAt.UTC().Format(time.RFC3339),
		"scopes":     grant.Scopes, "user": userToJSON(grant.User),
	}
}

func (s *Server) handleListLegacyOAuthGrants(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	grants := s.listLegacyGrants(user)
	out := make([]map[string]interface{}, 0, len(grants))
	for _, grant := range grants {
		out = append(out, s.legacyGrantJSON(grant, r))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) legacyGrantFromRequest(w http.ResponseWriter, r *http.Request, user *User) (legacyGrantRef, bool) {
	id, err := strconv.Atoi(r.PathValue("grant_id"))
	if err == nil {
		for _, grant := range s.listLegacyGrants(user) {
			if grant.ID == id {
				return grant, true
			}
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
	return legacyGrantRef{}, false
}

func (s *Server) handleGetLegacyOAuthGrant(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	if grant, ok := s.legacyGrantFromRequest(w, r, user); ok {
		writeJSON(w, http.StatusOK, s.legacyGrantJSON(grant, r))
	}
}

func (s *Server) handleDeleteLegacyOAuthGrant(w http.ResponseWriter, r *http.Request) {
	user := s.legacyAuthorizationUser(w, r)
	if user == nil {
		return
	}
	grant, ok := s.legacyGrantFromRequest(w, r, user)
	if !ok {
		return
	}
	s.store.RevokeUserGrant(grant.ClientID, user.ID)
	w.WriteHeader(http.StatusNoContent)
}
