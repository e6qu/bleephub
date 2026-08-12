package bleephub

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// registerGHAppSettingsRoutes exposes the browser-owned settings lifecycle.
// These are deliberately separate from /api/v3/app, whose authentication and
// wire contract are the public GitHub App REST API.
func (s *Server) registerGHAppSettingsRoutes() {
	s.route("GET /settings/apps/{app_slug}", s.handleGetBrowserGitHubApp)
	s.route("PATCH /settings/apps/{app_slug}", s.handleUpdateBrowserGitHubApp)
	s.route("DELETE /settings/apps/{app_slug}", s.handleDeleteBrowserGitHubApp)
	s.route("POST /settings/apps/{app_slug}/client-secret", s.handleRotateBrowserGitHubAppSecret)
	s.route("POST /settings/apps/{app_slug}/private-key", s.handleRotateBrowserGitHubAppKey)

	s.route("GET /settings/oauth-apps/{client_id}", s.handleGetBrowserOAuthApp)
	s.route("PATCH /settings/oauth-apps/{client_id}", s.handleUpdateBrowserOAuthApp)
	s.route("DELETE /settings/oauth-apps/{client_id}", s.handleDeleteBrowserOAuthApp)
	s.route("POST /settings/oauth-apps/{client_id}/client-secret", s.handleRotateBrowserOAuthAppSecret)

	s.route("GET /settings/connections/applications", s.handleListBrowserOAuthGrants)
	s.route("DELETE /settings/connections/applications/{client_id}", s.handleDeleteBrowserOAuthGrant)
}

func (s *Server) handleListBrowserOAuthGrants(w http.ResponseWriter, r *http.Request) {
	_, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	type grant struct {
		ClientID  string   `json:"client_id"`
		Name      string   `json:"name"`
		Type      string   `json:"type"`
		URL       string   `json:"url"`
		Scopes    []string `json:"scopes"`
		CreatedAt string   `json:"created_at"`
	}
	byClient := map[string]*grant{}
	s.store.Mu.RLock()
	for _, token := range s.store.UserToServerTokens {
		if token.UserID != user.ID || time.Now().After(token.ExpiresAt) {
			continue
		}
		clientID, name, kind, appURL := token.OAuthAppClientID, "", "OAuthApp", ""
		if token.AppID > 0 {
			if app := s.store.Apps[token.AppID]; app != nil {
				clientID, name, kind, appURL = app.ClientID, app.Name, "GitHubApp", app.ExternalURL
			}
		} else if app := s.store.OAuthApps[token.OAuthAppClientID]; app != nil {
			name, appURL = app.Name, app.URL
		}
		if clientID == "" {
			continue
		}
		current := byClient[clientID]
		if current == nil || token.CreatedAt.Before(parseGrantCreatedAt(current.CreatedAt)) {
			byClient[clientID] = &grant{
				ClientID: clientID, Name: name, Type: kind, URL: appURL,
				Scopes: splitScopes(token.Scopes), CreatedAt: token.CreatedAt.UTC().Format(time.RFC3339),
			}
		}
	}
	s.store.Mu.RUnlock()
	out := make([]*grant, 0, len(byClient))
	for _, item := range byClient {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func parseGrantCreatedAt(raw string) time.Time {
	value, _ := time.Parse(time.RFC3339, raw)
	return value
}

func (s *Server) handleDeleteBrowserOAuthGrant(w http.ResponseWriter, r *http.Request) {
	r, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	clientID := r.PathValue("client_id")
	if s.store.GetOAuthApp(clientID) == nil && s.store.GetAppByClientID(clientID) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.RevokeUserGrant(clientID, user.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) browserOwnedApp(w http.ResponseWriter, r *http.Request) (*App, bool) {
	r, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return nil, false
	}
	app := s.store.GetAppBySlug(r.PathValue("app_slug"))
	if app == nil || app.OwnerID != user.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	return app, true
}

func (s *Server) browserOwnedOAuthApp(w http.ResponseWriter, r *http.Request) (*OAuthApp, bool) {
	r, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return nil, false
	}
	app := s.store.GetOAuthApp(r.PathValue("client_id"))
	if app == nil || app.OwnerID != user.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	return app, true
}

func appSettingsJSON(st *Store, app *App) map[string]interface{} {
	out := appToJSON(st, app, false)
	out["callback_url"] = app.CallbackURL
	out["webhook_url"] = app.WebhookURL
	out["webhook_active"] = app.WebhookActive
	out["webhook_content_type"] = app.WebhookContentType
	return out
}

func (s *Server) handleGetBrowserGitHubApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedApp(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, appSettingsJSON(s.store, app))
}

type githubAppSettingsRequest struct {
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	URL                string            `json:"url"`
	CallbackURL        string            `json:"callback_url"`
	WebhookURL         string            `json:"webhook_url"`
	WebhookActive      bool              `json:"webhook_active"`
	WebhookContentType string            `json:"webhook_content_type"`
	Permissions        map[string]string `json:"permissions"`
	Events             []string          `json:"events"`
}

func (s *Server) handleUpdateBrowserGitHubApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedApp(w, r)
	if !ok {
		return
	}
	var req githubAppSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeGHValidationError(w, "GitHubApp", "name", "missing_field")
		return
	}
	req.CallbackURL = strings.TrimSpace(req.CallbackURL)
	if err := validateClientCallbackURL(req.CallbackURL); err != nil {
		writeGHValidationError(w, "GitHubApp", "callback_url", "invalid")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeGHValidationError(w, "GitHubApp", "url", "missing_field")
		return
	}
	if err := validateClientCallbackURL(req.URL); err != nil {
		writeGHValidationError(w, "GitHubApp", "url", "invalid")
		return
	}
	req.WebhookURL = strings.TrimSpace(req.WebhookURL)
	if err := validateClientCallbackURL(req.WebhookURL); err != nil {
		writeGHValidationError(w, "GitHubApp", "webhook_url", "invalid")
		return
	}
	if req.WebhookContentType != "json" && req.WebhookContentType != "form" {
		writeGHValidationError(w, "GitHubApp", "webhook_content_type", "invalid")
		return
	}
	for scope, level := range req.Permissions {
		if !validPermLevelString(level) {
			writeGHValidationError(w, "GitHubApp", "permissions."+scope, "invalid")
			return
		}
	}
	s.store.UpdateApp(app.ID, func(current *App) {
		current.Name = req.Name
		current.Description = req.Description
		current.ExternalURL = req.URL
		current.CallbackURL = req.CallbackURL
		current.WebhookURL = req.WebhookURL
		current.WebhookActive = req.WebhookActive
		current.WebhookContentType = req.WebhookContentType
		current.Permissions = copyInstallationPermissions(req.Permissions)
		current.Events = append([]string(nil), req.Events...)
		current.WebhookEvents = append([]string(nil), req.Events...)
	})
	writeJSON(w, http.StatusOK, appSettingsJSON(s.store, s.store.GetApp(app.ID)))
}

func (s *Server) handleRotateBrowserGitHubAppSecret(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedApp(w, r)
	if !ok {
		return
	}
	secret, err := s.store.RotateAppClientSecret(app.ID)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"client_secret": secret})
}

func (s *Server) handleRotateBrowserGitHubAppKey(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedApp(w, r)
	if !ok {
		return
	}
	privateKey, err := s.store.RotateAppPrivateKey(app.ID)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"pem": privateKey})
}

func (s *Server) handleDeleteBrowserGitHubApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedApp(w, r)
	if !ok {
		return
	}
	if listing := s.store.GetMarketplaceListing(app.Slug); listing != nil {
		if err := s.store.DeleteMarketplaceListing(app.Slug); err != nil {
			writeGHError(w, http.StatusConflict, err.Error())
			return
		}
	}
	s.store.DeleteApp(app.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetBrowserOAuthApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedOAuthApp(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, oauthAppToJSON(app, false))
}

func (s *Server) handleUpdateBrowserOAuthApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedOAuthApp(w, r)
	if !ok {
		return
	}
	req, valid := decodeOAuthAppSettingsRequest(w, r)
	if !valid {
		return
	}
	s.store.UpdateOAuthApp(app.ClientID, func(current *OAuthApp) {
		current.Name = req.Name
		current.Description = req.Description
		current.URL = req.URL
		current.CallbackURL = req.CallbackURL
	})
	writeJSON(w, http.StatusOK, oauthAppToJSON(s.store.GetOAuthApp(app.ClientID), false))
}

func (s *Server) handleRotateBrowserOAuthAppSecret(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedOAuthApp(w, r)
	if !ok {
		return
	}
	secret, err := s.store.RotateOAuthAppClientSecret(app.ClientID)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"client_secret": secret})
}

func (s *Server) handleDeleteBrowserOAuthApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.browserOwnedOAuthApp(w, r)
	if !ok {
		return
	}
	for _, listing := range s.store.ListMarketplaceListings(false) {
		if listing.OAuthAppClientID == app.ClientID {
			if err := s.store.DeleteMarketplaceListing(listing.Slug); err != nil {
				writeGHError(w, http.StatusConflict, err.Error())
				return
			}
		}
	}
	s.store.DeleteOAuthApp(app.ClientID)
	w.WriteHeader(http.StatusNoContent)
}
