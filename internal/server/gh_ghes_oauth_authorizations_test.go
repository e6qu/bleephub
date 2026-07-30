package bleephub

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestLegacyOAuthAuthorizationAndGrantJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsOAuthMgmtRoutes()
	admin := s.store.LookupUserByLogin("admin")
	app := s.store.CreateOAuthApp(
		admin.ID, "Legacy Octokit", "", "https://octokit.example.test", "https://octokit.example.test/callback",
	)

	appPath := "/api/v3/authorizations/clients/" + app.ClientID + "/workstation"
	rec := enterpriseActionsRequest(t, s, http.MethodPut, appPath, map[string]interface{}{
		"client_secret": app.ClientSecret, "scopes": []string{"repo", "user"},
		"note": "Octokit workstation",
	})
	authorization := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || authorization["token"] == "" ||
		authorization["fingerprint"] != "workstation" || authorization["note"] != "Octokit workstation" {
		t.Fatalf("create legacy app authorization = %d %#v", rec.Code, authorization)
	}
	authorizationID := int(authorization["id"].(float64))

	rec = enterpriseActionsRequest(t, s, http.MethodPut, appPath, map[string]interface{}{
		"client_secret": app.ClientSecret, "scopes": []string{"repo"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("get existing legacy app authorization = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPatch,
		"/api/v3/authorizations/"+strconv.Itoa(authorizationID),
		map[string]interface{}{"add_scopes": []string{"read:org"}, "remove_scopes": []string{"user"}})
	updated := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || len(updated["scopes"].([]interface{})) != 2 {
		t.Fatalf("update legacy authorization = %d %#v", rec.Code, updated)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/applications/grants", nil)
	grants := decodeGHESRecorderArray(t, rec)
	if rec.Code != http.StatusOK || len(grants) != 1 ||
		grants[0]["app"].(map[string]interface{})["client_id"] != app.ClientID {
		t.Fatalf("list legacy grants = %d %#v", rec.Code, grants)
	}
	grantID := strconv.Itoa(int(grants[0]["id"].(float64)))
	rec = enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/applications/grants/"+grantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get legacy grant = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, "/api/v3/applications/grants/"+grantID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete legacy grant = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/applications/grants", nil)
	if got := decodeGHESRecorderArray(t, rec); len(got) != 0 {
		t.Fatalf("grants after revoke = %#v", got)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost, "/api/v3/authorizations",
		map[string]interface{}{"scopes": []string{"repo"}, "note": "automation"})
	pat := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || pat["token"] == "" || pat["note"] != "automation" {
		t.Fatalf("create classic authorization = %d %#v", rec.Code, pat)
	}
	patID := strconv.Itoa(int(pat["id"].(float64)))
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, "/api/v3/authorizations/"+patID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete classic authorization = %d %q", rec.Code, rec.Body.String())
	}
}

func TestLegacyAuthorizationMetadataPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	p1, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatal(err)
	}
	st1.SeedDefaultUser()
	admin := st1.LookupUserByLogin("admin")
	app := st1.CreateOAuthApp(admin.ID, "Persistent OAuth", "", "https://example.test", "https://example.test/cb")
	token, _ := st1.CreateUserToServerToken(admin.ID, 0, app.ClientID, "repo", 8*time.Hour, false)
	st1.mu.Lock()
	token.Note = "persist me"
	token.Fingerprint = "laptop"
	st1.persist.MustPut("user_to_server_tokens", token.Token, token)
	st1.mu.Unlock()
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	st2 := NewStore()
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
