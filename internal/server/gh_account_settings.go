package bleephub

import (
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

// Account security (2FA) and notification preferences back github.com's
// Settings → "Password and authentication" and "Notifications" pages. GitHub
// exposes neither over REST, so — like pinned repos and the wiki — these live
// under the browser-only /ui-data namespace. `s.route` auto-wraps /ui-data with
// authenticateUIData, so the handlers act on the authenticated viewer.
func (s *Server) registerGHAccountSettingsRoutes() {
	s.route("GET /ui-data/user/account-settings", s.handleGetAccountSettings)
	s.route("PUT /ui-data/user/two-factor", s.handleSetTwoFactor)
	s.route("PUT /ui-data/user/notification-settings", s.handleSetNotificationSettings)
	// Changing the primary email address is web-only on github.com (the REST
	// email endpoints add/remove/list but never re-point primary).
	s.route("PUT /ui-data/user/emails/primary", s.handleSetPrimaryEmail)
}

func (s *Server) accountSettingsJSON(userID int) (map[string]interface{}, bool) {
	twoFactor, notif, ok := s.store.GetAccountSecurity(userID)
	if !ok {
		return nil, false
	}
	return map[string]interface{}{
		"two_factor_enabled":    twoFactor,
		"notification_settings": notif,
	}, true
}

func (s *Server) handleGetAccountSettings(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	payload, ok := s.accountSettingsJSON(viewer.ID)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleSetTwoFactor(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !s.store.SetTwoFactor(viewer.ID, req.Enabled) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	payload, _ := s.accountSettingsJSON(viewer.ID)
	writeJSON(w, http.StatusOK, payload)
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

func (s *Server) handleSetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	var req store.NotificationSettings
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !s.store.SetNotificationSettings(viewer.ID, req) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	payload, _ := s.accountSettingsJSON(viewer.ID)
	writeJSON(w, http.StatusOK, payload)
}
