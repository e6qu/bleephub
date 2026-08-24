package bleephub

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

type ghesGlobalHookRequest struct {
	Name   string `json:"name"`
	Config *struct {
		URL         string      `json:"url"`
		Secret      string      `json:"secret"`
		ContentType string      `json:"content_type"`
		InsecureSSL interface{} `json:"insecure_ssl"`
	} `json:"config"`
	Events []string  `json:"events"`
	Active *flexBool `json:"active"`
}

func (s *Server) handleListGHESGlobalHooks(w http.ResponseWriter, r *http.Request) {
	s.store.Mu.RLock()
	hooks := make([]*store.Webhook, 0, len(s.store.EnterpriseSettings.GHESGlobalHooks))
	for _, hook := range s.store.EnterpriseSettings.GHESGlobalHooks {
		hooks = append(hooks, store.CloneWebhook(hook))
	}
	s.store.Mu.RUnlock()
	sort.Slice(hooks, func(i, j int) bool { return hooks[i].ID < hooks[j].ID })
	out := make([]map[string]interface{}, 0, len(hooks))
	for _, hook := range hooks {
		out = append(out, s.ghesGlobalHookJSON(hook, r))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleCreateGHESGlobalHook(w http.ResponseWriter, r *http.Request) {
	var req ghesGlobalHookRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name != "web" {
		store.WriteGHValidationError(w, "Hook", "name", "invalid")
		return
	}
	if req.Config == nil || strings.TrimSpace(req.Config.URL) == "" {
		store.WriteGHValidationError(w, "Hook", "url", "missing_field")
		return
	}
	if s.rejectUndeliverableHookURL(w, req.Config.URL) {
		return
	}
	events := req.Events
	if len(events) == 0 {
		events = []string{"user", "organization"}
	}
	active := true
	if req.Active != nil {
		active = bool(*req.Active)
	}
	now := s.currentTime()
	s.store.Mu.Lock()
	hook := &store.Webhook{
		ID: s.store.NextHookID, URL: req.Config.URL, Secret: req.Config.Secret,
		ContentType: req.Config.ContentType, InsecureSSL: normalizeInsecureSSL(req.Config.InsecureSSL),
		Events: append([]string(nil), events...), Active: active, Global: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if hook.ContentType == "" {
		hook.ContentType = "form"
	}
	if hook.InsecureSSL == "" {
		hook.InsecureSSL = "0"
	}
	s.store.NextHookID++
	s.store.EnterpriseSettings.GHESGlobalHooks = append(s.store.EnterpriseSettings.GHESGlobalHooks, hook)
	s.store.PersistEnterpriseSettings()
	rendered := store.CloneWebhook(hook)
	s.store.Mu.Unlock()
	if rendered.Active {
		s.enqueueWebhookDelivery(rendered, "ping", "", mustMarshal(map[string]interface{}{
			"zen": "Keep it logically awesome.", "hook_id": rendered.ID,
		}))
	}
	writeJSON(w, http.StatusCreated, s.ghesGlobalHookJSON(rendered, r))
}

func (s *Server) ghesGlobalHookFromRequest(w http.ResponseWriter, r *http.Request) (*store.Webhook, int) {
	id, err := strconv.Atoi(r.PathValue("hook_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, -1
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for index, hook := range s.store.EnterpriseSettings.GHESGlobalHooks {
		if hook.ID == id {
			return store.CloneWebhook(hook), index
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
	return nil, -1
}

func (s *Server) handleGetGHESGlobalHook(w http.ResponseWriter, r *http.Request) {
	hook, _ := s.ghesGlobalHookFromRequest(w, r)
	if hook != nil {
		writeJSON(w, http.StatusOK, s.ghesGlobalHookJSON(hook, r))
	}
}

func (s *Server) handleUpdateGHESGlobalHook(w http.ResponseWriter, r *http.Request) {
	current, index := s.ghesGlobalHookFromRequest(w, r)
	if current == nil {
		return
	}
	var req ghesGlobalHookRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Config != nil {
		if strings.TrimSpace(req.Config.URL) == "" {
			store.WriteGHValidationError(w, "Hook", "url", "missing_field")
			return
		}
		if s.rejectUndeliverableHookURL(w, req.Config.URL) {
			return
		}
		current.URL = req.Config.URL
		current.ContentType = req.Config.ContentType
		current.InsecureSSL = normalizeInsecureSSL(req.Config.InsecureSSL)
		if req.Config.Secret != "" {
			current.Secret = req.Config.Secret
		}
	}
	if req.Events != nil {
		current.Events = append([]string(nil), req.Events...)
	}
	if req.Active != nil {
		current.Active = bool(*req.Active)
	}
	current.UpdatedAt = s.currentTime()
	s.store.Mu.Lock()
	if index >= len(s.store.EnterpriseSettings.GHESGlobalHooks) ||
		s.store.EnterpriseSettings.GHESGlobalHooks[index].ID != current.ID {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusConflict, "Hook changed concurrently")
		return
	}
	s.store.EnterpriseSettings.GHESGlobalHooks[index] = current
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, s.ghesGlobalHookJSON(current, r))
}

func (s *Server) handleDeleteGHESGlobalHook(w http.ResponseWriter, r *http.Request) {
	hook, index := s.ghesGlobalHookFromRequest(w, r)
	if hook == nil {
		return
	}
	s.store.Mu.Lock()
	if index >= len(s.store.EnterpriseSettings.GHESGlobalHooks) ||
		s.store.EnterpriseSettings.GHESGlobalHooks[index].ID != hook.ID {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusConflict, "Hook changed concurrently")
		return
	}
	s.store.EnterpriseSettings.GHESGlobalHooks = append(
		s.store.EnterpriseSettings.GHESGlobalHooks[:index],
		s.store.EnterpriseSettings.GHESGlobalHooks[index+1:]...,
	)
	delete(s.store.HookDeliveries, hook.ID)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePingGHESGlobalHook(w http.ResponseWriter, r *http.Request) {
	hook, _ := s.ghesGlobalHookFromRequest(w, r)
	if hook == nil {
		return
	}
	s.enqueueWebhookDelivery(hook, "ping", "", mustMarshal(map[string]interface{}{
		"zen": "Keep it logically awesome.", "hook_id": hook.ID,
	}))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ghesGlobalHookJSON(hook *store.Webhook, r *http.Request) map[string]interface{} {
	base := s.baseURL(r) + "/api/v3/admin/hooks/" + strconv.Itoa(hook.ID)
	contentType := hook.ContentType
	if contentType == "" {
		contentType = "form"
	}
	insecureSSL := hook.InsecureSSL
	if insecureSSL == "" {
		insecureSSL = "0"
	}
	return map[string]interface{}{
		"type": "Global", "id": hook.ID, "name": "web", "active": hook.Active,
		"events": append([]string(nil), hook.Events...),
		"config": map[string]interface{}{
			"url": hook.URL, "content_type": contentType, "insecure_ssl": insecureSSL,
		},
		"updated_at": hook.UpdatedAt.UTC().Format(time.RFC3339),
		"created_at": hook.CreatedAt.UTC().Format(time.RFC3339),
		"url":        base, "ping_url": base + "/pings",
	}
}

func (s *Server) handleListGHESPublicKeys(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	s.store.Misc.Mu.RLock()
	keys := make([]*store.UserKey, 0, len(s.store.Misc.UserKeys))
	for _, key := range s.store.Misc.UserKeys {
		copy := *key
		keys = append(keys, &copy)
	}
	s.store.Misc.Mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	out := make([]map[string]interface{}, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]interface{}{
			"id": key.ID, "key": key.Key, "user_id": key.UserID, "repository_id": nil,
			"url":   base + "/api/v3/admin/keys/" + strconv.Itoa(key.ID),
			"title": key.Title, "read_only": false, "verified": key.Verified,
			"created_at": key.CreatedAt.UTC().Format(time.RFC3339), "added_by": nil,
			"last_used": nil, "enabled": true,
		})
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleDeleteGHESPublicKeys(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.PathValue("key_ids"), ",")
	ids := make(map[int]bool, len(parts))
	for _, part := range parts {
		id, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		ids[id] = true
	}
	s.store.Misc.Mu.Lock()
	for id := range ids {
		key := s.store.Misc.UserKeys[id]
		if key == nil {
			s.store.Misc.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		delete(s.store.Misc.UserKeys, id)
		filtered := s.store.Misc.KeysByUser[key.UserID][:0:0]
		for _, candidate := range s.store.Misc.KeysByUser[key.UserID] {
			if candidate.ID != id {
				filtered = append(filtered, candidate)
			}
		}
		s.store.Misc.KeysByUser[key.UserID] = filtered
		if s.store.Misc.Persist != nil {
			s.store.Misc.Persist.MustDelete("user_keys", strconv.Itoa(id))
		}
	}
	s.store.Misc.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGHESPersonalAccessTokens(w http.ResponseWriter, r *http.Request) {
	type row struct {
		key   string
		token store.Token
		user  *store.User
	}
	s.store.Mu.RLock()
	rows := make([]row, 0, len(s.store.Tokens))
	for key, token := range s.store.Tokens {
		copy := *token
		if user := s.store.Users[token.UserID]; user != nil {
			userCopy := *user
			rows = append(rows, row{key: key, token: copy, user: &userCopy})
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(rows, func(i, j int) bool {
		return authorizationID(tokenIdentity(rows[i].key, &rows[i].token)) <
			authorizationID(tokenIdentity(rows[j].key, &rows[j].token))
	})
	out := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		out = append(out, classicAuthorizationJSON(rows[i].key, &rows[i].token, rows[i].user, false, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleDeleteGHESPersonalAccessToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("token_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	for key, token := range s.store.Tokens {
		if authorizationID(tokenIdentity(key, token)) == id {
			s.store.DeleteTokenMapKeyLocked(key)
			s.store.Mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.store.Mu.Unlock()
	writeGHError(w, http.StatusNotFound, "Not Found")
}

func (s *Server) handleCreateGHESImpersonationToken(w http.ResponseWriter, r *http.Request) {
	user := s.store.LookupUserByLogin(r.PathValue("username"))
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Scopes []string `json:"scopes"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.Mu.Lock()
	for key, token := range s.store.Tokens {
		if token.UserID == user.ID && token.Impersonation {
			out := classicAuthorizationJSON(key, token, user, false, s.baseURL(r))
			s.store.Mu.Unlock()
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	token := s.store.CreateTokenLocked(user.ID, strings.Join(req.Scopes, ", "))
	token.Impersonation = true
	s.store.PersistTokenLocked(token)
	out := classicAuthorizationJSON(token.Value, token, user, true, s.baseURL(r))
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleDeleteGHESImpersonationToken(w http.ResponseWriter, r *http.Request) {
	user := s.store.LookupUserByLogin(r.PathValue("username"))
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.Lock()
	found := false
	for key, token := range s.store.Tokens {
		if token.UserID == user.ID && token.Impersonation {
			s.store.DeleteTokenMapKeyLocked(key)
			found = true
		}
	}
	s.store.Mu.Unlock()
	if !found {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func tokenIdentity(key string, token *store.Token) string {
	if token.Value != "" {
		return token.Value
	}
	return key
}

func classicAuthorizationJSON(key string, token *store.Token, user *store.User, revealToken bool, baseURL string) map[string]interface{} {
	identity := tokenIdentity(key, token)
	value := ""
	if revealToken {
		value = token.Value
	}
	var expires interface{}
	if token.ExpiresAt != nil {
		expires = token.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{
		"id": authorizationID(identity), "url": "/api/v3/authorizations/" + strconv.Itoa(authorizationID(identity)),
		"scopes": splitScopes(token.Scopes), "token": value, "token_last_eight": lastEight(identity),
		"hashed_token": hashedToken(identity),
		"app":          map[string]interface{}{"client_id": "00000000000000000000", "name": "GitHub API", "url": "https://github.com"},
		"note":         nullableString(token.Note), "note_url": nullableString(token.NoteURL),
		"updated_at":  token.CreatedAt.UTC().Format(time.RFC3339),
		"created_at":  token.CreatedAt.UTC().Format(time.RFC3339),
		"fingerprint": nullableString(token.Fingerprint), "user": store.UserToJSON(user, baseURL), "installation": nil, "expires_at": expires,
	}
}
