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
