package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Permission enforcement decorator — requirePerm wrappers gate ghs_ tokens
// against the installation's granted permission set, returning 403 with a
// GitHub-shaped error envelope when the requested perm is missing.

func TestRequirePerm_GhsToken_PermsGate(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHRepoRoutes()
	s.registerGHIssueRoutes()

	user := s.store.UsersByLogin["admin"]
	app := s.store.CreateApp(user.ID, "Perms App", "", map[string]string{
		"issues":   "read",
		"contents": "read",
	}, nil)
	inst := s.store.CreateInstallation(app.ID, "User", user.ID, user.Login, app.Permissions, nil)

	// Mint a ghs_ token carrying the installation's perms (contents:read, issues:read only).
	tok := s.store.CreateInstallationToken(inst.ID, app.ID, app.Permissions, nil)

	repo := s.store.CreateRepo(user, "perms-target", "", false)
	_ = repo

	body, _ := json.Marshal(map[string]string{"title": "test"})
	req := httptest.NewRequest("POST", "/api/v3/repos/admin/perms-target/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (issues:read can't create issues), got %d body=%s", w.Code, w.Body.String())
	}

	// A minted ghs_ token's perms are immutable (real GitHub behaviour), so an
	// installation upgrade to issues:write only takes effect on a fresh token.
	s.store.Mu.Lock()
	inst.Permissions["issues"] = "write"
	s.store.Mu.Unlock()
	fresh := s.store.CreateInstallationToken(inst.ID, app.ID, inst.Permissions, nil)

	req = httptest.NewRequest("POST", "/api/v3/repos/admin/perms-target/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fresh.Token)
	w = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 with issues:write, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRequirePerm_PATBypass(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHRepoRoutes()
	s.registerGHIssueRoutes()

	user := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(user, "pat-target", "", false)

	// Use the seeded admin PAT (bph_-prefixed via Tokens map) — should bypass.
	body, _ := json.Marshal(map[string]string{"title": "test"})
	req := httptest.NewRequest("POST", "/api/v3/repos/admin/pat-target/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("PAT bypass failed: %d body=%s", w.Code, w.Body.String())
	}
}

func TestRequirePerm_GhuToken_AppInstallationPerms(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHRepoRoutes()
	s.registerGHIssueRoutes()

	user := s.store.UsersByLogin["admin"]
	app := s.store.CreateApp(user.ID, "Ghu App", "", nil, nil)
	s.store.CreateInstallation(app.ID, "User", user.ID, user.Login, map[string]string{"issues": "write"}, nil)
	s.store.CreateRepo(user, "ghu-target", "", false)

	tok, _ := s.store.CreateUserToServerToken(user.ID, app.ID, "", "", 8*time.Hour, false)

	body, _ := json.Marshal(map[string]string{"title": "test"})
	req := httptest.NewRequest("POST", "/api/v3/repos/admin/ghu-target/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("ghu_ token with issues:write installation expected 201, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRequirePerm_GhoToken_ClassicScopesMap(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsOAuthMgmtRoutes()
	s.registerGHRepoRoutes()
	s.registerGHIssueRoutes()

	user := s.store.UsersByLogin["admin"]
	oapp := s.store.CreateOAuthApp(user.ID, "ScopeMap", "", "", "")
	tokRepo, _ := s.store.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "repo", 8*time.Hour, false)
	tokReadOrg, _ := s.store.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "read:org", 8*time.Hour, false)

	s.store.CreateRepo(user, "gho-target", "", false)
	body, _ := json.Marshal(map[string]string{"title": "test"})

	// "repo" classic scope covers issues:write → 201
	req := httptest.NewRequest("POST", "/api/v3/repos/admin/gho-target/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokRepo.Token)
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("repo scope expected 201, got %d body=%s", w.Code, w.Body.String())
	}

	// "read:org" does NOT cover issues:write → 403
	req = httptest.NewRequest("POST", "/api/v3/repos/admin/gho-target/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokReadOrg.Token)
	w = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("read:org expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestValidateRequestedPermissions pins the downscoping contract for
// installation-token minting: every requested scope must be granted at >=
// the requested level; metadata:read is implicit; level strings outside
// read/write/admin are invalid.
func TestValidateRequestedPermissions(t *testing.T) {
	granted := map[string]string{"contents": "read", "issues": "write", "administration": "admin"}

	cases := []struct {
		name      string
		requested map[string]string
		ok        bool
	}{
		{"equal level", map[string]string{"contents": "read"}, true},
		{"downscope", map[string]string{"issues": "read"}, true},
		{"admin implies write", map[string]string{"administration": "write"}, true},
		{"escalate read to write", map[string]string{"contents": "write"}, false},
		{"escalate write to admin", map[string]string{"issues": "admin"}, false},
		{"ungranted scope", map[string]string{"deployments": "read"}, false},
		{"implicit metadata read", map[string]string{"metadata": "read"}, true},
		{"metadata write not implicit", map[string]string{"metadata": "write"}, false},
		{"invalid level string", map[string]string{"contents": "sudo"}, false},
		{"empty request", map[string]string{}, true},
		{"mixed valid and invalid", map[string]string{"issues": "read", "contents": "write"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := validateRequestedPermissions(tc.requested, granted)
			if ok != tc.ok {
				t.Errorf("validateRequestedPermissions(%v) ok = %v, want %v", tc.requested, ok, tc.ok)
			}
		})
	}
}

// TestInstallationPermissionsSerializeInGitHubsVocabulary: bleephub's
// authorization model has a level above write, and app-permissions declares
// ["read","write","admin"] for only four of its members. The serialization
// narrows the level for the members whose enum stops at write; the stored
// level, which is what every authorization decision reads, is untouched.
func TestInstallationPermissionsSerializeInGitHubsVocabulary(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store

	user := st.LookupUserByLogin("admin")
	if user == nil {
		t.Fatal("no admin user")
	}
	granted := map[string]string{
		"metadata":                    "read",
		"contents":                    "write",
		"administration":              "admin",
		"organization_administration": "admin",
		"organization_hooks":          "admin",
		"organization_projects":       "admin",
	}
	app := st.CreateApp(user.ID, "Admin Level App", "", granted, nil)
	if app == nil {
		t.Fatal("could not create the app")
	}
	inst := st.CreateInstallation(app.ID, "User", user.ID, user.Login, granted, nil)
	if inst == nil {
		t.Fatal("could not install the app")
	}

	body := decodeJSONWithStatus(t, s.get(t, "/api/v3/user/installations", defaultToken), 200)
	listed, _ := body["installations"].([]interface{})
	var served map[string]interface{}
	for _, raw := range listed {
		entry, _ := raw.(map[string]interface{})
		if id, _ := entry["id"].(float64); int(id) == inst.ID {
			served, _ = entry["permissions"].(map[string]interface{})
		}
	}
	if served == nil {
		t.Fatalf("installation %d is not listed: %v", inst.ID, body)
	}
	// The three members whose documented enum stops at read/write.
	for _, scope := range []string{"administration", "organization_administration", "organization_hooks"} {
		if served[scope] != "write" {
			t.Errorf("%s serialized as %v, want write (app-permissions declares only read/write)", scope, served[scope])
		}
	}
	// organization_projects is one of the four whose enum does carry admin,
	// so narrowing it would lose information GitHub models.
	if served["organization_projects"] != "admin" {
		t.Errorf("organization_projects serialized as %v, want admin", served["organization_projects"])
	}
	if served["contents"] != "write" || served["metadata"] != "read" {
		t.Errorf("unrelated levels changed: %v", served)
	}

	// The authorization side is unchanged: the stored grant is still admin,
	// and the downscoping gate — the thing that separates a write grant from
	// an admin one — still lets an admin-level request through, which a write
	// grant would refuse.
	stored := st.GetInstallation(inst.ID)
	if stored == nil {
		t.Fatal("installation vanished")
	}
	for _, scope := range []string{"administration", "organization_administration", "organization_hooks"} {
		if stored.Permissions[scope] != "admin" {
			t.Errorf("stored %s = %q, want admin; the wire narrowing must not reach the store", scope, stored.Permissions[scope])
		}
		if _, ok := validateRequestedPermissions(map[string]string{scope: "admin"}, stored.Permissions); !ok {
			t.Errorf("an admin-level request for %s was refused; the authorization level was weakened", scope)
		}
	}
	if _, ok := validateRequestedPermissions(map[string]string{"contents": "admin"}, stored.Permissions); ok {
		t.Error("a write grant satisfied an admin-level request; the levels are no longer distinct")
	}
}
