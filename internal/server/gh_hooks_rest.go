package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHHookRoutes() {
	adminHookRoute := func(pattern string, level store.PermLevel, next http.HandlerFunc) {
		s.route(pattern, s.requirePerm(store.ScopeAdministration, level, next))
	}
	adminHookRoute("POST /api/v3/repos/{owner}/{repo}/hooks", store.PermWrite, s.handleCreateHook)
	adminHookRoute("GET /api/v3/repos/{owner}/{repo}/hooks", store.PermRead, s.handleListHooks)
	adminHookRoute("GET /api/v3/repos/{owner}/{repo}/hooks/{id}", store.PermRead, s.handleGetHook)
	adminHookRoute("PATCH /api/v3/repos/{owner}/{repo}/hooks/{id}", store.PermWrite, s.handleUpdateHook)
	adminHookRoute("DELETE /api/v3/repos/{owner}/{repo}/hooks/{id}", store.PermWrite, s.handleDeleteHook)
	adminHookRoute("GET /api/v3/repos/{owner}/{repo}/hooks/{id}/deliveries", store.PermRead, s.handleListHookDeliveries)
	adminHookRoute("GET /api/v3/repos/{owner}/{repo}/hooks/{id}/deliveries/{delivery_id}", store.PermRead, s.handleGetHookDelivery)
	adminHookRoute("POST /api/v3/repos/{owner}/{repo}/hooks/{id}/deliveries/{delivery_id}/attempts", store.PermWrite, s.handleRedeliverHookDelivery)
	adminHookRoute("POST /api/v3/repos/{owner}/{repo}/hooks/{id}/pings", store.PermWrite, s.handlePingHook)
	adminHookRoute("GET /api/v3/repos/{owner}/{repo}/hooks/{id}/config", store.PermRead, s.handleGetHookConfig)
	adminHookRoute("PATCH /api/v3/repos/{owner}/{repo}/hooks/{id}/config", store.PermWrite, s.handleUpdateHookConfig)
	adminHookRoute("POST /api/v3/repos/{owner}/{repo}/hooks/{id}/tests", store.PermWrite, s.handleTestHook)
}

// hookRepo resolves the repository the hook routes name, or writes 404.
func (s *Server) hookRepo(w http.ResponseWriter, r *http.Request) *store.Repo {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

// rejectUndeliverableHookURL writes a validation error for a target deliveries must never reach.
func (s *Server) rejectUndeliverableHookURL(w http.ResponseWriter, target string) bool {
	if err := validateWebhookTargetURL(target); err != nil {
		s.logger.Warn().Err(err).Msg("webhook target rejected at configuration time")
		store.WriteGHValidationError(w, "Hook", "config.url", "invalid")
		return true
	}
	return false
}

func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	repoKey := repo.FullName

	var req struct {
		Name   string `json:"name"`
		Config struct {
			URL         string      `json:"url"`
			Secret      string      `json:"secret"`
			ContentType string      `json:"content_type"`
			InsecureSSL interface{} `json:"insecure_ssl"`
		} `json:"config"`
		Events []string  `json:"events"`
		Active *flexBool `json:"active"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// GitHub's REST API supports only the "web" hook type.
	if req.Name != "" && req.Name != "web" {
		store.WriteGHValidationError(w, "Hook", "name", "invalid")
		return
	}

	if req.Config.URL == "" {
		store.WriteGHValidationError(w, "Hook", "url", "missing_field")
		return
	}
	if s.rejectUndeliverableHookURL(w, req.Config.URL) {
		return
	}

	events := req.Events
	if len(events) == 0 {
		events = []string{"push"}
	}
	active := true
	if req.Active != nil {
		active = bool(*req.Active)
	}

	hook := s.store.CreateHook(repoKey, req.Config.URL, req.Config.Secret,
		req.Config.ContentType, normalizeInsecureSSL(req.Config.InsecureSSL), events, active)
	s.recordAuditEvent("hook.create", user.Login, "", map[string]interface{}{"repo": repoKey, "hook_id": hook.ID})

	// GitHub fires a ping automatically on creating an active hook; inactive hooks get none.
	if hook.Active {
		s.enqueueWebhookDelivery(hook, "ping", "", mustMarshal(buildPingPayload(repo, hook, user, s.baseURL(r))))
	}

	hookJSON := s.hookToJSON(hook, s.store.HookLastResp(hook), r, r.PathValue("owner"), r.PathValue("repo"))
	writeJSONCreated(w, jsonStringField(hookJSON, "url"), hookJSON)
}

func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	hooks := s.store.ListHooks(repo.FullName)

	ownerName, repoName := r.PathValue("owner"), r.PathValue("repo")
	result := make([]map[string]interface{}, 0, len(hooks))
	for _, h := range hooks {
		result = append(result, s.hookToJSON(h, s.store.HookLastResp(h), r, ownerName, repoName))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleGetHook(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	hook := s.store.GetHook(repo.FullName, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	writeJSON(w, http.StatusOK, s.hookToJSON(hook, s.store.HookLastResp(hook), r, r.PathValue("owner"), r.PathValue("repo")))
}

func (s *Server) handleUpdateHook(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	repoKey := repo.FullName
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Name   string `json:"name"`
		Config *struct {
			URL         string      `json:"url"`
			Secret      string      `json:"secret"`
			ContentType string      `json:"content_type"`
			InsecureSSL interface{} `json:"insecure_ssl"`
		} `json:"config"`
		Events       []string  `json:"events"`
		AddEvents    []string  `json:"add_events"`
		RemoveEvents []string  `json:"remove_events"`
		Active       *flexBool `json:"active"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Name != "" && req.Name != "web" {
		store.WriteGHValidationError(w, "Hook", "name", "invalid")
		return
	}
	// Existence before validity: an unknown hook is 404 whatever the body says.
	if s.store.GetHook(repoKey, hookID) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.Config != nil && req.Config.URL != "" && s.rejectUndeliverableHookURL(w, req.Config.URL) {
		return
	}

	found := s.store.UpdateHook(repoKey, hookID, func(h *store.Webhook) {
		if req.Config != nil {
			if req.Config.URL != "" {
				h.URL = req.Config.URL
			}
			if req.Config.Secret != "" {
				h.Secret = req.Config.Secret
			}
			if req.Config.ContentType != "" {
				h.ContentType = req.Config.ContentType
			}
			if ssl := normalizeInsecureSSL(req.Config.InsecureSSL); ssl != "" {
				h.InsecureSSL = ssl
			}
		}
		if req.Events != nil {
			h.Events = req.Events
		}
		// add_events / remove_events adjust the existing subscription, applied after
		// a wholesale events replacement; GitHub's update-hook accepts all three.
		if len(req.AddEvents) > 0 {
			h.Events = unionEvents(h.Events, req.AddEvents)
		}
		if len(req.RemoveEvents) > 0 {
			h.Events = removeEvents(h.Events, req.RemoveEvents)
		}
		if req.Active != nil {
			h.Active = bool(*req.Active)
		}
	})

	if !found {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	hook := s.store.GetHook(repoKey, hookID)
	writeJSON(w, http.StatusOK, s.hookToJSON(hook, s.store.HookLastResp(hook), r, r.PathValue("owner"), r.PathValue("repo")))
}

// unionEvents returns existing plus any of add not already present, preserving
// order and de-duplicating — the add_events semantics of a hook update.
func unionEvents(existing, add []string) []string {
	seen := make(map[string]bool, len(existing))
	out := make([]string, 0, len(existing)+len(add))
	for _, e := range existing {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	for _, e := range add {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// removeEvents returns existing with every member of remove dropped — the
// remove_events semantics of a hook update.
func removeEvents(existing, remove []string) []string {
	drop := make(map[string]bool, len(remove))
	for _, e := range remove {
		drop[e] = true
	}
	out := make([]string, 0, len(existing))
	for _, e := range existing {
		if !drop[e] {
			out = append(out, e)
		}
	}
	return out
}

func (s *Server) handleDeleteHook(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	repoKey := repo.FullName
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if !s.store.DeleteHook(repoKey, hookID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.recordAuditEvent("hook.destroy", user.Login, "", map[string]interface{}{"repo": repoKey, "hook_id": hookID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListHookDeliveries(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	hook := s.store.GetHook(repo.FullName, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	deliveries := paginateAndLink(w, r, s.store.ListDeliveries(hookID))
	result := make([]map[string]interface{}, 0, len(deliveries))
	for _, d := range deliveries {
		result = append(result, deliveryToJSON(d))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handlePingHook(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	hook := s.store.GetHook(repo.FullName, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.enqueueWebhookDelivery(hook, "ping", "", mustMarshal(buildPingPayload(repo, hook, ghUserFromContext(r.Context()), s.baseURL(r))))

	w.WriteHeader(http.StatusNoContent)
}

// hookConfigJSON renders a webhook's config sub-object. GitHub masks a configured
// secret as "********" rather than echoing it in cleartext.
func hookConfigJSON(h *store.Webhook) map[string]interface{} {
	config := map[string]interface{}{
		"url":          h.URL,
		"content_type": store.CoalesceStr(h.ContentType, "form"),
		"insecure_ssl": store.CoalesceStr(h.InsecureSSL, "0"),
	}
	if h.Secret != "" {
		config["secret"] = "********"
	}
	return config
}

func (s *Server) handleGetHookConfig(w http.ResponseWriter, r *http.Request) {
	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	hook := s.store.GetHook(repo.FullName, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, hookConfigJSON(hook))
}

// handleUpdateHookConfig updates the config sub-view: present members replace the
// stored value, absent members are left unchanged.
func (s *Server) handleUpdateHookConfig(w http.ResponseWriter, r *http.Request) {
	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	repoKey := repo.FullName
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		URL         *string     `json:"url"`
		ContentType *string     `json:"content_type"`
		Secret      *string     `json:"secret"`
		InsecureSSL interface{} `json:"insecure_ssl"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// Existence before validity: an unknown hook is 404 whatever the body says.
	if s.store.GetHook(repoKey, hookID) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.URL != nil && s.rejectUndeliverableHookURL(w, *req.URL) {
		return
	}
	found := s.store.UpdateHook(repoKey, hookID, func(h *store.Webhook) {
		if req.URL != nil {
			h.URL = *req.URL
		}
		if req.ContentType != nil {
			h.ContentType = *req.ContentType
		}
		if req.Secret != nil {
			h.Secret = *req.Secret
		}
		if ssl := normalizeInsecureSSL(req.InsecureSSL); ssl != "" {
			h.InsecureSSL = ssl
		}
	})
	if !found {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, hookConfigJSON(s.store.GetHook(repoKey, hookID)))
}

// handleTestHook triggers the hook with a push event for the default branch head.
// A hook not subscribed to push generates no delivery but still answers 204, as on GitHub.
func (s *Server) handleTestHook(w http.ResponseWriter, r *http.Request) {
	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	hook := s.store.GetHook(repo.FullName, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if hookMatchesEvent(hook, "push") {
		sender := ghUserFromContext(r.Context())
		branch := repo.DefaultBranch
		headSha := store.ResolveBranchSha(s.store.GetGitStorage(r.PathValue("owner"), r.PathValue("repo")), branch)
		if headSha == "" {
			writeGHError(w, http.StatusUnprocessableEntity, "No default branch commit found")
			return
		}
		payload := buildPushPayload(s.store, repo, sender, "refs/heads/"+branch, headSha, headSha, s.baseURL(r))
		s.enqueueWebhookDelivery(hook, "push", "", mustMarshal(payload))
	}
	w.WriteHeader(http.StatusNoContent)
}

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("mustMarshal: " + err.Error())
	}
	return b
}

// handleGetHookDelivery returns the full delivery with request and response payloads.
func (s *Server) handleGetHookDelivery(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	deliveryID, err := strconv.Atoi(r.PathValue("delivery_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	hook := s.store.GetHook(repo.FullName, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, d := range s.store.ListDeliveries(hookID) {
		if d.ID == deliveryID {
			writeJSON(w, http.StatusOK, deliveryFullJSON(d))
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

func (s *Server) handleRedeliverHookDelivery(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.hookRepo(w, r)
	if repo == nil {
		return
	}
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	deliveryID, err := strconv.Atoi(r.PathValue("delivery_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	hook := s.store.GetHook(repo.FullName, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var original *store.WebhookDelivery
	for _, d := range s.store.ListDeliveries(hookID) {
		if d.ID == deliveryID {
			original = d
			break
		}
	}
	if original == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	payloadBytes := mustMarshal(original.Request.Payload)
	s.enqueueWebhookJob(webhookQueueKey(hook), func() {
		s.redeliverWebhook(hook, original.Event, original.Action, original.GUID, payloadBytes)
	})
	w.WriteHeader(http.StatusAccepted)
}

// hookToJSON serialises a Webhook to GitHub's published hook object shape.
func (s *Server) hookToJSON(h *store.Webhook, lastResp *store.HookLastResponse, r *http.Request, owner, repo string) map[string]interface{} {
	base := s.baseURL(r)
	hookBase := base + "/api/v3/repos/" + owner + "/" + repo + "/hooks/" + strconv.Itoa(h.ID)

	contentType := h.ContentType
	if contentType == "" {
		contentType = "form"
	}
	insecureSSL := h.InsecureSSL
	if insecureSSL == "" {
		insecureSSL = "0"
	}

	return map[string]interface{}{
		"type":   "Repository",
		"id":     h.ID,
		"name":   "web",
		"active": h.Active,
		"events": h.Events,
		"config": map[string]interface{}{
			"url":          h.URL,
			"content_type": contentType,
			"insecure_ssl": insecureSSL,
		},
		"updated_at":     h.UpdatedAt.UTC().Format(time.RFC3339),
		"created_at":     h.CreatedAt.UTC().Format(time.RFC3339),
		"url":            hookBase,
		"test_url":       hookBase + "/test",
		"ping_url":       hookBase + "/pings",
		"deliveries_url": hookBase + "/deliveries",
		"last_response":  hookLastResponseJSON(lastResp),
	}
}

// hookLastResponseJSON renders last_response. Before any delivery GitHub returns
// {code:null,status:"unused",message:null}.
func hookLastResponseJSON(lr *store.HookLastResponse) map[string]interface{} {
	if lr == nil {
		return map[string]interface{}{
			"code":    nil,
			"status":  "unused",
			"message": nil,
		}
	}
	return map[string]interface{}{
		"code":    lr.Code,
		"status":  lr.Status,
		"message": lr.Message,
	}
}

// normalizeInsecureSSL coerces insecure_ssl (sent as string "0"/"1" or number 0/1)
// to the canonical string form, returning "" when unset so callers keep the stored value.
func normalizeInsecureSSL(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == 1 {
			return "1"
		}
		return "0"
	case bool:
		if t {
			return "1"
		}
		return "0"
	default:
		return ""
	}
}

// deliveryStatus returns GitHub's human-readable delivery status string.
func deliveryStatus(statusCode int) string {
	if statusCode >= 200 && statusCode < 300 {
		return "OK"
	}
	if statusCode == 0 {
		return "failed to connect"
	}
	return strconv.Itoa(statusCode) + " " + http.StatusText(statusCode)
}

func deliveryToJSON(d *store.WebhookDelivery) map[string]interface{} {
	out := map[string]interface{}{
		"id":              d.ID,
		"guid":            d.GUID,
		"delivered_at":    d.DeliveredAt.UTC().Format(time.RFC3339),
		"redelivery":      d.Redelivery,
		"duration":        d.Duration,
		"status":          deliveryStatus(d.StatusCode),
		"status_code":     d.StatusCode,
		"event":           d.Event,
		"action":          nullableString(d.Action),
		"installation_id": nullableInt(d.InstallationID),
		"repository_id":   nullableInt(d.RepositoryID),
		"throttled_at":    nil,
	}
	if d.ThrottledAt != nil {
		out["throttled_at"] = d.ThrottledAt.UTC().Format(time.RFC3339)
	}
	return out
}

// nullableInt renders 0 as JSON null, matching GitHub's unset nullable ids.
func nullableInt(v int) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// nullableString renders "" as JSON null.
func nullableString(v string) interface{} {
	if v == "" {
		return nil
	}
	return v
}
