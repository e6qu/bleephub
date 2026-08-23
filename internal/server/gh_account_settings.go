package bleephub

import (
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

// Account settings back github.com's Settings → "Password and authentication"
// and "Notifications" pages. GitHub exposes neither over REST, so — like
// pinned repos and the wiki — these live under the browser-only /ui-data
// namespace. `s.route` auto-wraps /ui-data with authenticateUIData, so the
// handlers act on the authenticated viewer.
//
// The second-factor, password and session endpoints are in
// gh_account_security.go.
func (s *Server) registerGHAccountSettingsRoutes() {
	s.route("GET /ui-data/user/notification-settings", s.handleGetNotificationSettings)
	s.route("PUT /ui-data/user/notification-settings", s.handleSetNotificationSettings)
	// Changing the primary email address is web-only on github.com (the REST
	// email endpoints add/remove/list but never re-point primary).
	s.route("PUT /ui-data/user/emails/primary", s.handleSetPrimaryEmail)
	s.registerGHAccountSecurityRoutes()
}

// handleSetPrimaryEmail promotes one of the viewer's verified addresses to
// primary. The address must already be on the account (POST /user/emails) and
// verified; anything else is a 422, mirroring the visibility toggle's errors.
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
// preferences, defaults included, so the settings page renders a complete form
// for an account that has never saved one.
func (s *Server) handleGetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	preferences, ok := s.store.GetNotificationPreferences(viewer.ID)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, preferences)
}

// handleSetNotificationSettings replaces the viewer's preferences. The body is
// the whole document (the page submits every control it renders); the store
// normalizes it, dropping event keys the model does not define.
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
	if !s.store.SetNotificationPreferences(viewer.ID, req, s.currentTime()) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	preferences, _ := s.store.GetNotificationPreferences(viewer.ID)
	writeJSON(w, http.StatusOK, preferences)
}
