package bleephub

import (
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

// Account settings back github.com's password/notifications pages, which GitHub
// exposes over no REST route, so they live under browser-only /ui-data.
func (s *Server) registerGHAccountSettingsRoutes() {
	s.route("GET /ui-data/user/notification-settings", s.handleGetNotificationSettings)
	s.route("PUT /ui-data/user/notification-settings", s.handleSetNotificationSettings)
	s.route("PUT /ui-data/user/emails/primary", s.handleSetPrimaryEmail)
	s.registerGHAccountSecurityRoutes()
	s.registerGHSudoModeRoutes()
	s.registerGHUserIPAllowListRoutes()
}

// handleSetPrimaryEmail promotes one of the viewer's verified addresses to
// primary. An address not already on the account, or unverified, is a 422.
func (s *Server) handleSetPrimaryEmail(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Email == "" {
		store.WriteGHValidationError(w, "Email", "email", "missing_field")
		return
	}
	emails, result := s.store.SetPrimaryUserEmail(viewer.ID, req.Email)
	switch result {
	case store.SetPrimaryEmailUnknown:
		store.WriteGHValidationError(w, "Email", "email", "invalid")
		return
	case store.SetPrimaryEmailUnverified:
		store.WriteGHValidationError(w, "Email", "email", "unverified")
		return
	case store.SetPrimaryEmailOK:
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := make([]map[string]interface{}, len(emails))
	for i, e := range emails {
		out[i] = userEmailJSON(e)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetNotificationSettings returns the viewer's per-type notification
// preferences, defaults included.
func (s *Server) handleGetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	// Apply the enterprise delivery restriction so the answer reflects what
	// would actually be delivered.
	preferences, ok := s.store.EffectiveNotificationPreferences(viewer.ID)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, preferences)
}

// handleSetNotificationSettings replaces the viewer's preferences with the
// posted document; the store drops event keys the model does not define.
func (s *Server) handleSetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	var req store.NotificationPreferences
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// Refuse a request for email delivery when the enterprise restricts it to
	// verified domains this account's address is not in — recording a promise
	// the instance cannot keep is worse than a 403.
	if allowed, restricted := s.store.NotificationEmailDeliveryAllowed(viewer.ID); restricted && !allowed && req.SelectsEmailDelivery() {
		writeGHError(w, http.StatusForbidden,
			"Email notification delivery is restricted to the enterprise's verified domains, "+
				"and this account's address is not in one of them.")
		return
	}
	if !s.store.SetNotificationPreferences(viewer.ID, req, s.currentTime()) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	preferences, _ := s.store.EffectiveNotificationPreferences(viewer.ID)
	writeJSON(w, http.StatusOK, preferences)
}
