package bleephub

import (
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Organization-level webhooks (`/orgs/{org}/hooks`). Org hooks receive the
// org's own events plus every repo event on org-owned repositories. Stored
// separately from repo hooks but sharing the hook ID sequence and deliveries
// table.

func (s *Server) registerGHOrgHookRoutes() {
	s.route("POST /api/v3/orgs/{org}/hooks", s.requirePerm(store.ScopeOrganizationHooks, store.PermWrite, s.handleCreateOrgHook))
	s.route("GET /api/v3/orgs/{org}/hooks", s.requirePerm(store.ScopeOrganizationHooks, store.PermRead, s.handleListOrgHooks))
	s.route("GET /api/v3/orgs/{org}/hooks/{id}", s.requirePerm(store.ScopeOrganizationHooks, store.PermRead, s.handleGetOrgHook))
	s.route("PATCH /api/v3/orgs/{org}/hooks/{id}", s.requirePerm(store.ScopeOrganizationHooks, store.PermWrite, s.handleUpdateOrgHook))
	s.route("DELETE /api/v3/orgs/{org}/hooks/{id}", s.requirePerm(store.ScopeOrganizationHooks, store.PermWrite, s.handleDeleteOrgHook))
	s.route("GET /api/v3/orgs/{org}/hooks/{hook_id}/config", s.requirePerm(store.ScopeOrganizationHooks, store.PermRead, s.handleGetOrgHookConfig))
	s.route("PATCH /api/v3/orgs/{org}/hooks/{hook_id}/config", s.requirePerm(store.ScopeOrganizationHooks, store.PermWrite, s.handleUpdateOrgHookConfig))
	s.route("GET /api/v3/orgs/{org}/hooks/{id}/deliveries", s.requirePerm(store.ScopeOrganizationHooks, store.PermRead, s.handleListOrgHookDeliveries))
	s.route("GET /api/v3/orgs/{org}/hooks/{id}/deliveries/{delivery_id}", s.requirePerm(store.ScopeOrganizationHooks, store.PermRead, s.handleGetOrgHookDelivery))
	s.route("POST /api/v3/orgs/{org}/hooks/{id}/deliveries/{delivery_id}/attempts", s.requirePerm(store.ScopeOrganizationHooks, store.PermWrite, s.handleRedeliverOrgHookDelivery))
	s.route("POST /api/v3/orgs/{org}/hooks/{id}/pings", s.requirePerm(store.ScopeOrganizationHooks, store.PermWrite, s.handlePingOrgHook))
}

// orgHookGate resolves the org and requires org-admin (the analogue of
// GitHub's admin:org_hook scope), writing the error response on failure.
func (s *Server) orgHookGate(w http.ResponseWriter, r *http.Request) *store.Org {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return nil
	}
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return org
}

func (s *Server) handleCreateOrgHook(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
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
	// GitHub requires name=web for org hooks (unlike repo hooks).
	if req.Name != "web" {
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
	hook := s.store.CreateOrgHook(org.Login, req.Config.URL, req.Config.Secret,
		req.Config.ContentType, normalizeInsecureSSL(req.Config.InsecureSSL), events, active)
	s.recordAuditEvent("hook.create", ghUserFromContext(r.Context()).Login, org.Login, map[string]interface{}{"hook_id": hook.ID})

	// GitHub fires a ping automatically on active-hook creation.
	if hook.Active {
		s.enqueueWebhookDelivery(hook, "ping", "", mustMarshal(s.orgPingPayload(org, hook, r)))
	}

	orgHookJSON := orgHookToJSON(hook, org, s.baseURL(r))
	writeJSONCreated(w, jsonStringField(orgHookJSON, "url"), orgHookJSON)
}

func (s *Server) handleListOrgHooks(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
	hooks := s.store.ListOrgHooks(org.Login)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(hooks))
	for _, h := range hooks {
		result = append(result, orgHookToJSON(h, org, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// orgHookFromRequest resolves {id} to a stored org hook, writing 404 and
// returning nil when it doesn't resolve.
func (s *Server) orgHookFromRequest(w http.ResponseWriter, r *http.Request, org *store.Org) *store.Webhook {
	hookID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	hook := s.store.GetOrgHook(org.Login, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return hook
}

func (s *Server) handleGetOrgHook(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
	hook := s.orgHookFromRequest(w, r, org)
	if hook == nil {
		return
	}
	writeJSON(w, http.StatusOK, orgHookToJSON(hook, org, s.baseURL(r)))
}

func (s *Server) handleUpdateOrgHook(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
	hook := s.orgHookFromRequest(w, r, org)
	if hook == nil {
		return
	}
	var req struct {
		Config *struct {
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
	if req.Config != nil && req.Config.URL != "" && s.rejectUndeliverableHookURL(w, req.Config.URL) {
		return
	}
	s.store.UpdateOrgHook(org.Login, hook.ID, func(h *store.Webhook) {
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
		if req.Active != nil {
			h.Active = bool(*req.Active)
		}
	})
	writeJSON(w, http.StatusOK, orgHookToJSON(s.store.GetOrgHook(org.Login, hook.ID), org, s.baseURL(r)))
}

// orgHookFromConfigRequest resolves {org} + {hook_id} for the config
// sub-resource routes.
func (s *Server) orgHookFromConfigRequest(w http.ResponseWriter, r *http.Request) (*store.Org, *store.Webhook) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return nil, nil
	}
	hookID, err := strconv.Atoi(r.PathValue("hook_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	hook := s.store.GetOrgHook(org.Login, hookID)
	if hook == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	return org, hook
}

// orgHookConfigJSON renders the webhook-config shape; the secret is masked,
// as GitHub never surfaces the raw value after creation.
func orgHookConfigJSON(h *store.Webhook) map[string]interface{} {
	contentType := h.ContentType
	if contentType == "" {
		contentType = "form"
	}
	insecureSSL := h.InsecureSSL
	if insecureSSL == "" {
		insecureSSL = "0"
	}
	out := map[string]interface{}{
		"url":          h.URL,
		"content_type": contentType,
		"insecure_ssl": insecureSSL,
	}
	if h.Secret != "" {
		out["secret"] = "********"
	}
	return out
}

func (s *Server) handleGetOrgHookConfig(w http.ResponseWriter, r *http.Request) {
	_, hook := s.orgHookFromConfigRequest(w, r)
	if hook == nil {
		return
	}
	writeJSON(w, http.StatusOK, orgHookConfigJSON(hook))
}

func (s *Server) handleUpdateOrgHookConfig(w http.ResponseWriter, r *http.Request) {
	org, hook := s.orgHookFromConfigRequest(w, r)
	if hook == nil {
		return
	}
	var req struct {
		URL         string      `json:"url"`
		ContentType string      `json:"content_type"`
		Secret      string      `json:"secret"`
		InsecureSSL interface{} `json:"insecure_ssl"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.URL != "" && s.rejectUndeliverableHookURL(w, req.URL) {
		return
	}
	s.store.UpdateOrgHook(org.Login, hook.ID, func(h *store.Webhook) {
		if req.URL != "" {
			h.URL = req.URL
		}
		if req.ContentType != "" {
			h.ContentType = req.ContentType
		}
		if req.Secret != "" {
			h.Secret = req.Secret
		}
		if ssl := normalizeInsecureSSL(req.InsecureSSL); ssl != "" {
			h.InsecureSSL = ssl
		}
	})
	writeJSON(w, http.StatusOK, orgHookConfigJSON(s.store.GetOrgHook(org.Login, hook.ID)))
}

func (s *Server) handleDeleteOrgHook(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
	hook := s.orgHookFromRequest(w, r, org)
	if hook == nil {
		return
	}
	s.store.DeleteOrgHook(org.Login, hook.ID)
	s.recordAuditEvent("hook.destroy", ghUserFromContext(r.Context()).Login, org.Login, map[string]interface{}{"hook_id": hook.ID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgHookDeliveries(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
	hook := s.orgHookFromRequest(w, r, org)
	if hook == nil {
		return
	}
	deliveries := s.store.ListDeliveries(hook.ID)
	page := paginateAndLink(w, r, deliveries)
	result := make([]map[string]interface{}, 0, len(page))
	for _, d := range page {
		result = append(result, deliveryToJSON(d))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetOrgHookDelivery(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
	hook := s.orgHookFromRequest(w, r, org)
	if hook == nil {
		return
	}
	deliveryID, err := strconv.Atoi(r.PathValue("delivery_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, d := range s.store.ListDeliveries(hook.ID) {
		if d.ID == deliveryID {
			writeJSON(w, http.StatusOK, deliveryFullJSON(d))
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

func (s *Server) handleRedeliverOrgHookDelivery(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
	hook := s.orgHookFromRequest(w, r, org)
	if hook == nil {
		return
	}
	deliveryID, err := strconv.Atoi(r.PathValue("delivery_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var original *store.WebhookDelivery
	for _, d := range s.store.ListDeliveries(hook.ID) {
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

func (s *Server) handlePingOrgHook(w http.ResponseWriter, r *http.Request) {
	org := s.orgHookGate(w, r)
	if org == nil {
		return
	}
	hook := s.orgHookFromRequest(w, r, org)
	if hook == nil {
		return
	}
	s.enqueueWebhookDelivery(hook, "ping", "", mustMarshal(s.orgPingPayload(org, hook, r)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) orgPingPayload(org *store.Org, hook *store.Webhook, r *http.Request) map[string]interface{} {
	return map[string]interface{}{
		"zen":          "Keep it logically awesome.",
		"hook_id":      hook.ID,
		"hook":         orgHookToJSON(hook, org, s.baseURL(r)),
		"organization": orgWebhookPayload(org, s.baseURL(r)),
		// GitHub's ping event always carries a top-level sender, like the repo variant.
		"sender": senderPayload(ghUserFromContext(r.Context()), s.baseURL(r)),
	}
}

func orgHookToJSON(h *store.Webhook, org *store.Org, baseURL string) map[string]interface{} {
	hookBase := baseURL + "/api/v3/orgs/" + org.Login + "/hooks/" + strconv.Itoa(h.ID)
	contentType := h.ContentType
	if contentType == "" {
		contentType = "form"
	}
	insecureSSL := h.InsecureSSL
	if insecureSSL == "" {
		insecureSSL = "0"
	}
	return map[string]interface{}{
		"id":     h.ID,
		"type":   "Organization",
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
		"ping_url":       hookBase + "/pings",
		"deliveries_url": hookBase + "/deliveries",
	}
}

func (s *Server) emitOrgWebhookEvent(orgLogin, eventType, action string, payload interface{}) {
	payloadBytes := mustMarshal(payload)
	for _, hook := range s.store.ListOrgHooks(orgLogin) {
		if !hook.Active || !hookMatchesEvent(hook, eventType) {
			continue
		}
		s.enqueueWebhookDelivery(hook, eventType, action, payloadBytes)
	}
}

// emitOrgMembershipEvent fires the `organization` event for membership
// changes. bleephub models invitations as pending memberships, so the
// member_invited payload's invitation object is derived from the membership
// (there is no separate invitation entity).
func (s *Server) emitOrgMembershipEvent(org *store.Org, action string, m *store.Membership, target, sender *store.User) {
	payload := map[string]interface{}{
		"action":       action,
		"organization": orgWebhookPayload(org, s.publicOrigin()),
	}
	if sender != nil {
		payload["sender"] = store.UserToJSON(sender, s.publicOrigin())
	}
	if action == "member_invited" {
		invitationRole := "direct_member"
		if m.Role == store.OrgRoleAdmin {
			invitationRole = "admin"
		}
		payload["user"] = store.UserToJSON(target, s.publicOrigin())
		payload["invitation"] = map[string]interface{}{
			"id":         authorizationID(org.Login + "/" + target.Login),
			"login":      target.Login,
			"email":      nil,
			"role":       invitationRole,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
	} else {
		payload["membership"] = map[string]interface{}{
			"url":              "/api/v3/orgs/" + org.Login + "/memberships/" + target.Login,
			"organization_url": "/api/v3/orgs/" + org.Login,
			"state":            m.State,
			"role":             m.Role,
			"user":             store.UserToJSON(target, s.publicOrigin()),
		}
	}
	s.emitOrgWebhookEvent(org.Login, "organization", action, payload)
}
