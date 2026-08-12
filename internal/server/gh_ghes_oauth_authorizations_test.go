package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// legacyAuthorizationSessionRequest drives the authorizations API the way a
// real client must: with a browser session cookie
// (password-equivalent authority), never a bearer token.
func legacyAuthorizationSessionRequest(
	t *testing.T,
	s *Server,
	sessionID string,
	method, path string,
	body map[string]interface{},
) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req.AddCookie(&http.Cookie{Name: "_gh_sess", Value: sessionID})
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(rec, req)
	return rec
}

func TestLegacyOAuthAuthorizationAndGrantJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsOAuthMgmtRoutes()
	admin := s.store.LookupUserByLogin("admin")
	const session = "admin-browser-session"
	s.store.LoginSessions[session] = &store.LoginSession{UserID: admin.ID, ExpiresAt: s.currentTime().Add(time.Hour)}
	req := func(method, path string, body map[string]interface{}) *httptest.ResponseRecorder {
		return legacyAuthorizationSessionRequest(t, s, session, method, path, body)
	}
	app := s.store.CreateOAuthApp(
		admin.ID, "Legacy Octokit", "", "https://octokit.example.test", "https://octokit.example.test/callback",
	)

	appPath := "/api/v3/authorizations/clients/" + app.ClientID + "/workstation"
	rec := req(http.MethodPut, appPath, map[string]interface{}{
		"client_secret": app.ClientSecret, "scopes": []string{"repo", "user"},
		"note": "Octokit workstation",
	})
	authorization := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || authorization["token"] == "" ||
		authorization["fingerprint"] != "workstation" || authorization["note"] != "Octokit workstation" {
		t.Fatalf("create legacy app authorization = %d %#v", rec.Code, authorization)
	}
	authorizationID := int(authorization["id"].(float64))

	rec = req(http.MethodPut, appPath, map[string]interface{}{
		"client_secret": app.ClientSecret, "scopes": []string{"repo"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("get existing legacy app authorization = %d %q", rec.Code, rec.Body.String())
	}
	rec = req(http.MethodPatch,
		"/api/v3/authorizations/"+strconv.Itoa(authorizationID),
		map[string]interface{}{"add_scopes": []string{"read:org"}, "remove_scopes": []string{"user"}})
	updated := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || len(updated["scopes"].([]interface{})) != 2 {
		t.Fatalf("update legacy authorization = %d %#v", rec.Code, updated)
	}

	rec = req(http.MethodGet, "/api/v3/applications/grants", nil)
	grants := decodeGHESRecorderArray(t, rec)
	if rec.Code != http.StatusOK || len(grants) != 1 ||
		grants[0]["app"].(map[string]interface{})["client_id"] != app.ClientID {
		t.Fatalf("list legacy grants = %d %#v", rec.Code, grants)
	}
	grantID := strconv.Itoa(int(grants[0]["id"].(float64)))
	rec = req(http.MethodGet, "/api/v3/applications/grants/"+grantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get legacy grant = %d %q", rec.Code, rec.Body.String())
	}
	rec = req(http.MethodDelete, "/api/v3/applications/grants/"+grantID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete legacy grant = %d %q", rec.Code, rec.Body.String())
	}
	rec = req(http.MethodGet, "/api/v3/applications/grants", nil)
	if got := decodeGHESRecorderArray(t, rec); len(got) != 0 {
		t.Fatalf("grants after revoke = %#v", got)
	}

	rec = req(http.MethodPost, "/api/v3/authorizations",
		map[string]interface{}{"scopes": []string{"repo"}, "note": "automation"})
	pat := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || pat["token"] == "" || pat["note"] != "automation" {
		t.Fatalf("create classic authorization = %d %#v", rec.Code, pat)
	}
	patID := strconv.Itoa(int(pat["id"].(float64)))
	rec = req(http.MethodDelete, "/api/v3/authorizations/"+patID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete classic authorization = %d %q", rec.Code, rec.Body.String())
	}
}

// TestLegacyAuthorizationsRefuseBearerCredentials pins the rule that the API that
// mints/rewrites account credentials must reject a bearer token so a leaked
// scoped PAT cannot POST itself an unrestricted classic PAT.
func TestLegacyAuthorizationsRefuseBearerCredentials(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsOAuthMgmtRoutes()
	admin := s.store.LookupUserByLogin("admin")
	scoped := s.store.CreateToken(admin.ID, "gist")
	tokensBefore := len(s.store.Tokens)

	body, err := json.Marshal(map[string]interface{}{"scopes": []string{"repo", "admin:org", "delete_repo"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v3/authorizations", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+scoped.Value)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PAT-authenticated authorization create = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	// No new token was minted for the account.
	if len(s.store.Tokens) != tokensBefore {
		t.Fatalf("token count changed from %d to %d; an escalated PAT was minted", tokensBefore, len(s.store.Tokens))
	}
}

func TestLegacyAuthorizationMetadataPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	p1, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatal(err)
	}
	st1.SeedDefaultUser()
	admin := st1.LookupUserByLogin("admin")
	app := st1.CreateOAuthApp(admin.ID, "Persistent OAuth", "", "https://example.test", "https://example.test/cb")
	token, _ := st1.CreateUserToServerToken(admin.ID, 0, app.ClientID, "repo", 8*time.Hour, false)
	st1.Mu.Lock()
	token.Note = "persist me"
	token.Fingerprint = "laptop"
	st1.Persist.MustPut("user_to_server_tokens", token.Token, token)
	st1.Mu.Unlock()
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	st2 := store.NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, reloaded := range st2.UserToServerTokens {
		if reloaded.Note == "persist me" && reloaded.Fingerprint == "laptop" {
			found = true
		}
	}
	if !found {
		t.Fatal("legacy authorization metadata did not persist")
	}
}
