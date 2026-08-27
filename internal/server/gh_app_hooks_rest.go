package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

// App-level webhook config and deliveries. A GitHub App owns one webhook URL;
// its deliveries are stored separately from per-repo deliveries, keyed by app
// ID. JWT-authenticated.

func (s *Server) registerGHAppHookRoutes() {
	s.route("GET /api/v3/app/hook/config", s.handleGetAppHookConfig)
	s.route("PATCH /api/v3/app/hook/config", s.handleUpdateAppHookConfig)
	s.route("GET /api/v3/app/hook/deliveries", s.handleListAppHookDeliveries)
	s.route("GET /api/v3/app/hook/deliveries/{delivery_id}", s.handleGetAppHookDelivery)
	s.route("POST /api/v3/app/hook/deliveries/{delivery_id}/attempts", s.handleRedeliverAppHookDelivery)
}

func (s *Server) handleGetAppHookConfig(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	writeJSON(w, http.StatusOK, appHookConfigJSON(app))
}

func (s *Server) handleUpdateAppHookConfig(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	var req struct {
		URL         string `json:"url"`
		Secret      string `json:"secret"`
		ContentType string `json:"content_type"`
		InsecureSSL string `json:"insecure_ssl"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.UpdateAppHookConfig(app.ID, func(a *store.App) {
		if req.URL != "" {
			a.WebhookURL = req.URL
		}
		if req.Secret != "" {
			a.WebhookSecret = req.Secret
		}
		if req.ContentType != "" {
			a.WebhookContentType = req.ContentType
		}
		if req.InsecureSSL != "" {
			a.WebhookInsecureSSL = req.InsecureSSL
		}
	})
	app = s.store.GetApp(app.ID)
	writeJSON(w, http.StatusOK, appHookConfigJSON(app))
}

func (s *Server) handleListAppHookDeliveries(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	deliveries := s.store.ListAppDeliveries(app.ID)
	page := paginateAndLink(w, r, deliveries)
	out := make([]map[string]interface{}, 0, len(page))
	for _, d := range page {
		out = append(out, deliveryToJSON(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetAppHookDelivery(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	id, err := strconv.Atoi(r.PathValue("delivery_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.GetAppDelivery(app.ID, id)
	if d == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, deliveryFullJSON(d))
}

func (s *Server) handleRedeliverAppHookDelivery(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	id, err := strconv.Atoi(r.PathValue("delivery_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	d := s.store.GetAppDelivery(app.ID, id)
	if d == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if app.WebhookURL == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "App has no webhook URL configured")
		return
	}
	s.enqueueWebhookJob(appWebhookQueueKey(app), func() { s.redeliverAppWebhook(app, d) })
	w.WriteHeader(http.StatusAccepted)
}

func appHookConfigJSON(app *store.App) map[string]interface{} {
	contentType := app.WebhookContentType
	if contentType == "" {
		contentType = "form" // GitHub's default for app webhooks
	}
	insecureSSL := app.WebhookInsecureSSL
	if insecureSSL == "" {
		insecureSSL = "0"
	}
	return map[string]interface{}{
		"url":          app.WebhookURL,
		"content_type": contentType,
		"insecure_ssl": insecureSSL,
		"secret":       "********", // GitHub redacts the secret
	}
}

func deliveryFullJSON(d *store.WebhookDelivery) map[string]interface{} {
	out := deliveryToJSON(d)
	out["url"] = d.TargetURL
	// request and response are required members; emit them with null members
	// when nothing was captured, per the hook-delivery shape.
	request := map[string]interface{}{"headers": nil, "payload": nil}
	if d.Request != nil {
		request["headers"] = d.Request.Headers
		request["payload"] = d.Request.Payload
	}
	out["request"] = request
	response := map[string]interface{}{"headers": nil, "payload": nil}
	if d.Response != nil {
		response["headers"] = d.Response.Headers
		response["payload"] = d.Response.Body
	}
	out["response"] = response
	return out
}

// redeliverAppWebhook re-runs the delivery against the App's current webhook URL.
func (s *Server) redeliverAppWebhook(app *store.App, original *store.WebhookDelivery) {
	if app.WebhookURL == "" {
		return
	}
	payloadBytes, _ := json.Marshal(original.Request.Payload)
	hook := appWebhookPseudoHook(app)
	delivery := s.doDeliverAttempt(hook, original.Event, original.Action, original.GUID, payloadBytes, true)
	delivery.HookID = -app.ID
	delivery.AppID = app.ID
	delivery.InstallationID = original.InstallationID
	s.store.AddAppDelivery(app.ID, delivery)
}
