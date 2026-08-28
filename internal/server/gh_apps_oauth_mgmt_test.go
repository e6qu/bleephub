package bleephub

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// /applications/{client_id}/token family + OAuth App management — token
// inspection, revocation, refresh, and OAuth App create/update against the
// GitHub-compatible app-management surface (uses Basic auth with the OAuth
// App credentials, not bearer tokens).

func TestOAuthAppCreate_AndCheckTokenWithBasicAuth(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHAppsOAuthMgmtRoutes()

	user := s.store.UsersByLogin["admin"]
	oapp := s.store.CreateOAuthApp(user.ID, "Test OAuth App", "", "https://example.test", "https://example.test/cb")
	tok, _ := s.store.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "repo", 8*time.Hour, false)

	body, _ := json.Marshal(map[string]string{"access_token": tok.Token})
	req := httptest.NewRequest("POST", "/api/v3/applications/"+oapp.ClientID+"/token", bytes.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicHeader(oapp.ClientID, oapp.ClientSecret))
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["token"] != tok.Token {
		t.Errorf("inspection echoed wrong token: %v", got["token"])
	}
	if got["token_last_eight"] != tok.Token[len(tok.Token)-8:] {
		t.Errorf("token_last_eight = %v, want %s", got["token_last_eight"], tok.Token[len(tok.Token)-8:])
	}
	sum := sha256.Sum256([]byte(tok.Token))
	if got["hashed_token"] != hex.EncodeToString(sum[:]) {
		t.Errorf("hashed_token = %v, want %s", got["hashed_token"], hex.EncodeToString(sum[:]))
	}
	// An OAuth-App token has no installation, so this field is null.
	if v, present := got["installation"]; !present || v != nil {
		t.Errorf("installation = %v (present=%v), want null for OAuth-App token", v, present)
	}
	appObj, _ := got["app"].(map[string]any)
	if appObj == nil {
		t.Fatalf("missing app object in inspection response")
	}
	if appObj["client_id"] != oapp.ClientID {
		t.Errorf("app.client_id = %v, want %s", appObj["client_id"], oapp.ClientID)
	}
	if appObj["name"] != "Test OAuth App" {
		t.Errorf("app.name = %v, want Test OAuth App", appObj["name"])
	}
}

func TestOAuthCheckToken_BadCreds(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHAppsOAuthMgmtRoutes()

	user := s.store.UsersByLogin["admin"]
	oapp := s.store.CreateOAuthApp(user.ID, "T", "", "", "")
	tok, _ := s.store.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "repo", 8*time.Hour, false)
	body, _ := json.Marshal(map[string]string{"access_token": tok.Token})

	req := httptest.NewRequest("POST", "/api/v3/applications/"+oapp.ClientID+"/token", bytes.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicHeader(oapp.ClientID, "wrong"))
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong-secret status = %d, want 401", w.Code)
	}

	req = httptest.NewRequest("POST", "/api/v3/applications/"+oapp.ClientID+"/token", bytes.NewReader(body))
	w = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-auth status = %d, want 401", w.Code)
	}
}

func TestOAuthResetToken_RotatesAndRevokesOld(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHAppsOAuthMgmtRoutes()

	user := s.store.UsersByLogin["admin"]
	oapp := s.store.CreateOAuthApp(user.ID, "Rotate", "", "", "")
	tok, _ := s.store.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "repo", 8*time.Hour, false)
	oldToken := tok.Token

	body, _ := json.Marshal(map[string]string{"access_token": oldToken})
	req := httptest.NewRequest("PATCH", "/api/v3/applications/"+oapp.ClientID+"/token", bytes.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicHeader(oapp.ClientID, oapp.ClientSecret))
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	newToken, _ := resp["token"].(string)
	if newToken == "" || newToken == oldToken {
		t.Errorf("reset returned same/empty token: old=%q new=%q", oldToken, newToken)
	}
	if got, _ := s.store.LookupUserToServerToken(oldToken); got != nil {
		t.Error("old token still valid after reset")
	}
}

func TestOAuthRevokeToken_204(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHAppsOAuthMgmtRoutes()

	user := s.store.UsersByLogin["admin"]
	oapp := s.store.CreateOAuthApp(user.ID, "Revoke", "", "", "")
	tok, _ := s.store.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "repo", 8*time.Hour, false)

	body, _ := json.Marshal(map[string]string{"access_token": tok.Token})
	req := httptest.NewRequest("DELETE", "/api/v3/applications/"+oapp.ClientID+"/token", bytes.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicHeader(oapp.ClientID, oapp.ClientSecret))
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if got, _ := s.store.LookupUserToServerToken(tok.Token); got != nil {
		t.Error("token still valid after revoke")
	}
}

func TestOAuthRevokeGrant_KillsAllUserToServerTokensForClient(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHAppsOAuthMgmtRoutes()

	user := s.store.UsersByLogin["admin"]
	oapp := s.store.CreateOAuthApp(user.ID, "Grant", "", "", "")
	a, _ := s.store.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "repo", 8*time.Hour, true)
	b, _ := s.store.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "read:org", 8*time.Hour, true)

	body, _ := json.Marshal(map[string]string{"access_token": a.Token})
	req := httptest.NewRequest("DELETE", "/api/v3/applications/"+oapp.ClientID+"/grant", bytes.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicHeader(oapp.ClientID, oapp.ClientSecret))
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	// Revoking the grant kills the whole clientID/userID grant, so both A and B go.
	if got, _ := s.store.LookupUserToServerToken(a.Token); got != nil {
		t.Error("A still valid after grant revoke")
	}
	if got, _ := s.store.LookupUserToServerToken(b.Token); got != nil {
		t.Error("B still valid after grant revoke")
	}
}

func TestOAuthScopeToken_MintsFreshNarrowedToken(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHAppsOAuthMgmtRoutes()
	s.registerGHRepoRoutes()
	s.registerGHIssueRoutes()

	user := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(user, "scoped-token-repo", "", true)
	outside := s.store.CreateRepo(user, "scoped-token-outside", "", true)
	appPermissions := map[string]string{"contents": "write", "issues": "write"}
	app := s.store.CreateApp(user.ID, "Scoped App", "", appPermissions, nil)
	inst := s.store.CreateInstallation(app.ID, "User", user.ID, "admin", appPermissions, nil)
	// GitHub-App user-to-server token (ghu_).
	tok, _ := s.store.CreateUserToServerToken(user.ID, app.ID, "", "", 8*time.Hour, false)

	body, _ := json.Marshal(map[string]any{
		"access_token": tok.Token,
		"target":       "admin",
		"permissions":  map[string]string{"contents": "read"},
		"repositories": []string{repo.Name},
	})
	req := httptest.NewRequest("POST", "/api/v3/applications/"+app.ClientID+"/token/scoped", bytes.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicHeader(app.ClientID, app.ClientSecret))
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	newToken, _ := got["token"].(string)
	if newToken == "" || newToken == tok.Token {
		t.Errorf("scope returned same/empty token: old=%q new=%q", tok.Token, newToken)
	}
	if !strings.HasPrefix(newToken, "ghu_") {
		t.Errorf("scoped token = %q, want ghu_ prefix", newToken)
	}
	fresh, u := s.store.LookupUserToServerToken(newToken)
	if fresh == nil || u == nil {
		t.Error("scoped token not valid in store")
	} else {
		if len(fresh.InstallationIDs) != 1 || fresh.InstallationIDs[0] != inst.ID {
			t.Errorf("installation scope = %v, want [%d]", fresh.InstallationIDs, inst.ID)
		}
		if len(fresh.RepositoryIDs) != 1 || fresh.RepositoryIDs[0] != repo.ID {
			t.Errorf("repository scope = %v, want [%d]", fresh.RepositoryIDs, repo.ID)
		}
		if len(fresh.Permissions) != 1 || fresh.Permissions["contents"] != "read" {
			t.Errorf("permission scope = %v, want contents:read", fresh.Permissions)
		}
	}
	// GitHub leaves the original token intact after scoping.
	if orig, _ := s.store.LookupUserToServerToken(tok.Token); orig == nil {
		t.Error("original token was revoked; scope must leave it intact")
	}
	// A GitHub-App token carries an installation object, unlike the OAuth case above.
	if got["installation"] == nil {
		t.Error("expected installation object for GitHub-App scoped token")
	}

	if allowed := tokenRequest(s, http.MethodGet, "/api/v3/repos/"+repo.FullName, newToken); allowed.Code != http.StatusOK {
		t.Errorf("selected repository read = %d: %s", allowed.Code, allowed.Body.String())
	}
	if denied := tokenRequest(s, http.MethodGet, "/api/v3/repos/"+outside.FullName, newToken); denied.Code != http.StatusNotFound {
		t.Errorf("unselected private repository read = %d: %s", denied.Code, denied.Body.String())
	}
	if denied := tokenRequest(s, http.MethodPost, "/api/v3/repos/"+repo.FullName+"/issues", newToken); denied.Code != http.StatusForbidden {
		t.Errorf("unrequested issues permission = %d: %s", denied.Code, denied.Body.String())
	}

	widenBody, _ := json.Marshal(map[string]any{
		"access_token": newToken,
		"target":       "admin",
		"permissions":  map[string]string{"contents": "write"},
	})
	widenReq := httptest.NewRequest("POST", "/api/v3/applications/"+app.ClientID+"/token/scoped", bytes.NewReader(widenBody))
	widenReq.Header.Set("Authorization", "Basic "+basicHeader(app.ClientID, app.ClientSecret))
	widen := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(widen, widenReq)
	if widen.Code != http.StatusUnprocessableEntity {
		t.Errorf("permission widening = %d: %s", widen.Code, widen.Body.String())
	}

	for name, invalidBody := range map[string]map[string]any{
		"conflicting target identity": {
			"access_token": tok.Token,
			"target":       "admin",
			"target_id":    user.ID + 1,
		},
		"both repository selectors present": {
			"access_token":   tok.Token,
			"target":         "admin",
			"repositories":   []string{},
			"repository_ids": []int{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(invalidBody)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/api/v3/applications/"+app.ClientID+"/token/scoped", bytes.NewReader(body))
			req.Header.Set("Authorization", "Basic "+basicHeader(app.ClientID, app.ClientSecret))
			got := httptest.NewRecorder()
			s.requestHandler().ServeHTTP(got, req)
			if got.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d: %s", got.Code, got.Body.String())
			}
		})
	}
}

func TestOAuthAppBrowserSettingsCreateAndList(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsOAuthMgmtRoutes()

	form := "name=Browser+OAuth&description=settings&url=https%3A%2F%2Fexample.test&callback_url=https%3A%2F%2Fexample.test%2Fcb"
	req := httptest.NewRequest("POST", "/settings/oauth-apps/new", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "token "+store.AdminToken())
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("settings create status = %d body = %s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created["client_id"] == "" || created["client_secret"] == "" {
		t.Fatalf("created OAuth App missing one-time credentials: %v", created)
	}

	req = httptest.NewRequest("GET", "/settings/oauth-apps", nil)
	req.Header.Set("Authorization", "token "+store.AdminToken())
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("settings list status = %d body = %s", w.Code, w.Body.String())
	}
	var list []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["client_id"] != created["client_id"] {
		t.Fatalf("settings list = %+v, want created client id %v", list, created["client_id"])
	}
	if _, leaked := list[0]["client_secret"]; leaked {
		t.Fatal("settings list leaked OAuth App client_secret")
	}
}

func basicHeader(clientID, clientSecret string) string {
	return base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
}
