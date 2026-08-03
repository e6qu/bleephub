package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func decodeGHESRecorderArray(t *testing.T, rec *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	var value []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return value
}

// TestGHESAdminSurfaceRejectsNarrowCredentials pins the rule that a site admin's
// fine-grained PAT or OAuth/GitHub-App user token must NOT administer the
// appliance, even though the user record is a SiteAdmin. Only broad credentials
// (a browser session or a classic PAT) may.
func TestGHESAdminSurfaceRejectsNarrowCredentials(t *testing.T) {
	s := newTestServer()
	s.registerGHESAdminStatsRoutes()
	admin := s.store.LookupUserByLogin("admin") // SiteAdmin

	// A fine-grained PAT owned by the site admin.
	fg := s.store.CreateToken(admin.ID, "")
	s.store.mu.Lock()
	fg.FineGrained = true
	s.store.mu.Unlock()

	// An OAuth-App user-to-server token owned by the site admin.
	app := s.store.CreateOAuthApp(admin.ID, "Narrow", "", "https://x.test", "https://x.test/cb")
	uts, _ := s.store.CreateUserToServerToken(admin.ID, 0, app.ClientID, "", time.Hour, false)

	for _, tc := range []struct{ name, auth string }{
		{"fine-grained PAT", "token " + fg.Value},
		{"user-to-server token", "Bearer " + uts.Token},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v3/admin/hooks", nil)
		req.Header.Set("Authorization", tc.auth)
		rec := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s reached the admin surface: %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

func TestGHESGlobalHooksAndAdministrativeCredentials(t *testing.T) {
	s := newTestServer()
	s.registerGHESAdminStatsRoutes()
	fixed := time.Date(2026, time.July, 30, 9, 15, 0, 0, time.UTC)
	restoreServer := s.replaceClockNow(func() time.Time { return fixed })
	restoreStore := s.store.replaceClockNow(func() time.Time { return fixed })
	t.Cleanup(func() {
		s.replaceClockNow(restoreServer)
		s.store.replaceClockNow(restoreStore)
	})

	rec := enterpriseActionsRequest(t, s, http.MethodPost, "/api/v3/admin/hooks", map[string]interface{}{
		"name": "web", "active": false,
		"config": map[string]interface{}{"url": "https://hooks.example.test/global", "content_type": "json"},
	})
	hook := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || hook["type"] != "Global" || hook["created_at"] != fixed.Format(time.RFC3339) {
		t.Fatalf("create global hook = %d %#v", rec.Code, hook)
	}
	hookID := strconv.Itoa(int(hook["id"].(float64)))
	rec = enterpriseActionsRequest(t, s, http.MethodPatch, "/api/v3/admin/hooks/"+hookID,
		map[string]interface{}{"events": []string{"organization"}, "active": false})
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK ||
		len(got["events"].([]interface{})) != 1 || got["events"].([]interface{})[0] != "organization" {
		t.Fatalf("update global hook = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/admin/hooks", nil)
	if got := decodeGHESRecorderArray(t, rec); rec.Code != http.StatusOK || len(got) != 1 {
		t.Fatalf("list global hooks = %d %#v", rec.Code, got)
	}

	admin := s.store.LookupUserByLogin("admin")
	s.store.Misc.mu.Lock()
	key := &UserKey{
		ID: 41, Key: "ssh-ed25519 AAAAGHESAdmin", Title: "site-admin",
		Verified: true, UserID: admin.ID, CreatedAt: fixed,
	}
	s.store.Misc.userKeys[key.ID] = key
	s.store.Misc.keysByUser[admin.ID] = []*UserKey{key}
	s.store.Misc.mu.Unlock()
	rec = enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/admin/keys", nil)
	if got := decodeGHESRecorderArray(t, rec); rec.Code != http.StatusOK || len(got) != 1 ||
		got[0]["user_id"] != float64(admin.ID) {
		t.Fatalf("list public keys = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, "/api/v3/admin/keys/41", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete public key = %d %q", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost, "/api/v3/admin/users/admin/authorizations",
		map[string]interface{}{"scopes": []string{"repo", "admin:org"}})
	authorization := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || authorization["token"] == "" ||
		len(authorization["scopes"].([]interface{})) != 2 {
		t.Fatalf("create impersonation token = %d %#v", rec.Code, authorization)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, "/api/v3/admin/users/admin/authorizations",
		map[string]interface{}{"scopes": []string{"repo"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent impersonation token = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/admin/tokens", nil)
	tokens := decodeGHESRecorderArray(t, rec)
	if rec.Code != http.StatusOK || len(tokens) < 2 {
		t.Fatalf("list personal access tokens = %d %#v", rec.Code, tokens)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, "/api/v3/admin/users/admin/authorizations", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete impersonation token = %d %q", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, "/api/v3/admin/hooks/"+hookID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete global hook = %d %q", rec.Code, rec.Body.String())
	}
}

func TestGHESGlobalHooksAndImpersonationTokensPersist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	fixed := time.Date(2026, time.July, 30, 11, 0, 0, 0, time.UTC)

	p1, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	st1 := NewStore()
	st1.replaceClockNow(func() time.Time { return fixed })
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatal(err)
	}
	st1.SeedDefaultUser()
	st1.mu.Lock()
	st1.EnterpriseSettings.GHESGlobalHooks = []*Webhook{{
		ID: 77, URL: "https://hooks.example.test/persisted", Events: []string{"user"},
		Active: true, Global: true, CreatedAt: fixed, UpdatedAt: fixed,
	}}
	st1.NextHookID = 78
	token := st1.createTokenLocked(st1.UsersByLogin["admin"].ID, "repo")
	token.Impersonation = true
	st1.persistTokenLocked(token)
	st1.persistEnterpriseSettings()
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
	if len(st2.EnterpriseSettings.GHESGlobalHooks) != 1 ||
		!st2.EnterpriseSettings.GHESGlobalHooks[0].Global || st2.NextHookID < 78 {
		t.Fatalf("reloaded global hooks = %#v next=%d", st2.EnterpriseSettings.GHESGlobalHooks, st2.NextHookID)
	}
	found := false
	for _, persisted := range st2.Tokens {
		found = found || persisted.Impersonation
	}
	if !found {
		t.Fatal("impersonation marker did not survive persistence")
	}
}
