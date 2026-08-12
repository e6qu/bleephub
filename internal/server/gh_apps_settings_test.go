package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func appSettingsRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "token "+store.AdminToken())
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	s.mux.ServeHTTP(response, req)
	return response
}

func TestBrowserGitHubAppSettingsLifecycle(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	admin := s.store.UsersByLogin["admin"]
	app := s.store.CreateApp(admin.ID, "Settings lifecycle", "old", map[string]string{"contents": "read"}, []string{"push"})
	installation := s.store.CreateInstallation(app.ID, "User", admin.ID, admin.Login, app.Permissions, app.Events)
	installationToken := s.store.CreateInstallationToken(installation.ID, app.ID, app.Permissions, nil)
	userToken, _ := s.store.CreateUserToServerToken(admin.ID, app.ID, "", "repo", time.Hour, false)
	s.store.AuthCodes["pending-app"] = &store.AuthCode{Code: "pending-app", ClientID: app.ClientID}
	s.store.DeviceCodes["pending-app"] = &store.DeviceCode{Code: "pending-app", ClientID: app.ClientID, AppID: app.ID}
	oldSecret := app.ClientSecret
	oldKey := app.PEMPrivateKey

	update := appSettingsRequest(s, http.MethodPatch, "/settings/apps/"+app.Slug, `{
		"name":"Updated app",
		"description":"new description",
		"url":"https://example.test/app",
		"callback_url":"https://example.test/callback",
		"webhook_url":"https://example.test/hooks",
		"webhook_active":true,
		"webhook_content_type":"json",
		"permissions":{"issues":"write"},
		"events":["issues"]
	}`)
	if update.Code != http.StatusOK {
		t.Fatalf("PATCH app settings = %d: %s", update.Code, update.Body.String())
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(update.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated["name"] != "Updated app" || updated["callback_url"] != "https://example.test/callback" {
		t.Fatalf("updated app = %#v", updated)
	}
	if _, leaked := updated["client_secret"]; leaked {
		t.Fatal("settings response leaked client secret")
	}

	secret := appSettingsRequest(s, http.MethodPost, "/settings/apps/"+app.Slug+"/client-secret", `{}`)
	if secret.Code != http.StatusCreated {
		t.Fatalf("rotate secret = %d: %s", secret.Code, secret.Body.String())
	}
	if s.store.VerifyAppClientSecret(app.ClientID, oldSecret) != nil {
		t.Fatal("old GitHub App client secret still works after rotation")
	}

	key := appSettingsRequest(s, http.MethodPost, "/settings/apps/"+app.Slug+"/private-key", `{}`)
	if key.Code != http.StatusCreated {
		t.Fatalf("rotate key = %d: %s", key.Code, key.Body.String())
	}
	if s.store.GetApp(app.ID).PEMPrivateKey == oldKey {
		t.Fatal("private key did not rotate")
	}

	remove := appSettingsRequest(s, http.MethodDelete, "/settings/apps/"+app.Slug, "")
	if remove.Code != http.StatusNoContent {
		t.Fatalf("DELETE app = %d: %s", remove.Code, remove.Body.String())
	}
	if s.store.GetApp(app.ID) != nil || s.store.GetInstallation(installation.ID) != nil {
		t.Fatal("app deletion left app or installation behind")
	}
	if token, _ := s.store.LookupInstallationToken(installationToken.Token); token != nil {
		t.Fatal("app deletion left installation token valid")
	}
	if token, _ := s.store.LookupUserToServerToken(userToken.Token); token != nil {
		t.Fatal("app deletion left user-to-server token valid")
	}
	if s.store.AuthCodes["pending-app"] != nil || s.store.DeviceCodes["pending-app"] != nil {
		t.Fatal("app deletion left pending OAuth authorization state")
	}
}

func TestBrowserGitHubAppSettingsRequireHomepage(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	admin := s.store.UsersByLogin["admin"]
	app := s.store.CreateApp(admin.ID, "Required homepage", "", nil, nil)
	s.store.UpdateApp(app.ID, func(current *store.App) {
		current.ExternalURL = "https://example.test/app"
	})

	resp := appSettingsRequest(s, http.MethodPatch, "/settings/apps/"+app.Slug, `{
		"name":"Required homepage",
		"url":"",
		"webhook_content_type":"json",
		"permissions":{},
		"events":[]
	}`)
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH without homepage = %d: %s", resp.Code, resp.Body.String())
	}
	if got := s.store.GetApp(app.ID); got.ExternalURL != "https://example.test/app" {
		t.Fatalf("invalid update mutated homepage to %q", got.ExternalURL)
	}
}

func TestBrowserOAuthAppSettingsLifecycleAndOwnership(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppSettingsRoutes()
	admin := s.store.UsersByLogin["admin"]
	app := s.store.CreateOAuthApp(admin.ID, "OAuth settings", "old", "https://old.test", "https://old.test/cb")
	token, _ := s.store.CreateUserToServerToken(admin.ID, 0, app.ClientID, "repo", time.Hour, false)
	s.store.AuthCodes["pending-oauth"] = &store.AuthCode{Code: "pending-oauth", ClientID: app.ClientID}
	s.store.DeviceCodes["pending-oauth"] = &store.DeviceCode{Code: "pending-oauth", ClientID: app.ClientID, OAuthClientID: app.ClientID}
	oldSecret := app.ClientSecret

	grants := appSettingsRequest(s, http.MethodGet, "/settings/connections/applications", "")
	if grants.Code != http.StatusOK || !strings.Contains(grants.Body.String(), `"client_id":"`+app.ClientID+`"`) {
		t.Fatalf("OAuth grants = %d: %s", grants.Code, grants.Body.String())
	}
	revoke := appSettingsRequest(s, http.MethodDelete, "/settings/connections/applications/"+app.ClientID, "")
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke OAuth grant = %d: %s", revoke.Code, revoke.Body.String())
	}
	if current, _ := s.store.LookupUserToServerToken(token.Token); current != nil {
		t.Fatal("browser grant revocation left its token valid")
	}
	token, _ = s.store.CreateUserToServerToken(admin.ID, 0, app.ClientID, "repo", time.Hour, false)

	update := appSettingsRequest(s, http.MethodPatch, "/settings/oauth-apps/"+app.ClientID, `{
		"name":"Updated OAuth",
		"description":"new",
		"url":"https://new.test",
		"callback_url":"https://new.test/callback"
	}`)
	if update.Code != http.StatusOK {
		t.Fatalf("PATCH OAuth settings = %d: %s", update.Code, update.Body.String())
	}
	if got := s.store.GetOAuthApp(app.ClientID); got.Name != "Updated OAuth" || got.CallbackURL != "https://new.test/callback" {
		t.Fatalf("updated OAuth App = %#v", got)
	}

	rotate := appSettingsRequest(s, http.MethodPost, "/settings/oauth-apps/"+app.ClientID+"/client-secret", `{}`)
	if rotate.Code != http.StatusCreated {
		t.Fatalf("rotate OAuth secret = %d: %s", rotate.Code, rotate.Body.String())
	}
	if s.store.VerifyOAuthAppSecret(app.ClientID, oldSecret) != nil {
		t.Fatal("old OAuth App client secret still works after rotation")
	}

	remove := appSettingsRequest(s, http.MethodDelete, "/settings/oauth-apps/"+app.ClientID, "")
	if remove.Code != http.StatusNoContent {
		t.Fatalf("DELETE OAuth App = %d: %s", remove.Code, remove.Body.String())
	}
	if s.store.GetOAuthApp(app.ClientID) != nil {
		t.Fatal("OAuth App was not deleted")
	}
	if current, _ := s.store.LookupUserToServerToken(token.Token); current != nil {
		t.Fatal("OAuth App deletion left its grant token valid")
	}
	if s.store.AuthCodes["pending-oauth"] != nil || s.store.DeviceCodes["pending-oauth"] != nil {
		t.Fatal("OAuth App deletion left pending authorization state")
	}
}
