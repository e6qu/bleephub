package bleephub

import (
	"net/http"
	"testing"
	"time"
)

// TestDeploymentStatusRejectsInvalidState pins that POST deployment-status
// validates the state enum (GitHub returns 422 for anything outside the fixed
// set), matching the commit-status sibling. It previously accepted any
// non-empty string.
func TestDeploymentStatusRejectsInvalidState(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "dep-enum"})
	dep := decodeJSON(t, s.post(t, "/api/v3/repos/admin/dep-enum/deployments", defaultToken, map[string]interface{}{
		"ref":               "main",
		"environment":       "staging",
		"required_contexts": []string{},
	}))
	id, ok := dep["id"].(float64)
	if !ok {
		t.Fatalf("deployment not created: %#v", dep)
	}
	base := "/api/v3/repos/admin/dep-enum/deployments/" + itoa(int(id)) + "/statuses"

	bad := s.post(t, base, defaultToken, map[string]interface{}{"state": "deployed"})
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid deployment-status state = %d, want 422", bad.StatusCode)
	}

	good := s.post(t, base, defaultToken, map[string]interface{}{"state": "success"})
	good.Body.Close()
	if good.StatusCode != http.StatusCreated {
		t.Fatalf("valid deployment-status state = %d, want 201", good.StatusCode)
	}
}

// TestTokenExchangeRejectsUserToServerToken pins that /auth/token — which mints
// a full-authority browser session — only accepts a classic PAT. A narrow
// ghu_ user-to-server token (bounded by installation permissions) must not be
// widened into a full session.
func TestTokenExchangeRejectsUserToServerToken(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	app := s.store.CreateApp(admin.ID, "Round7 App", "",
		map[string]string{"metadata": "read", "contents": "read"}, nil)
	if app == nil {
		t.Fatal("could not register app")
	}
	s.store.CreateInstallation(app.ID, "User", admin.ID, admin.Login,
		map[string]string{"metadata": "read", "contents": "read"}, nil)
	uts, _ := s.store.CreateUserToServerToken(admin.ID, app.ID, "", "", time.Hour, false)
	if uts == nil {
		t.Fatal("could not mint ghu_ token")
	}

	// A ghu_ user-to-server token must be refused (403), not widened.
	blocked := s.post(t, "/auth/token", uts.Token, nil)
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusForbidden {
		t.Fatalf("ghu_ token exchange = %d, want 403 (must not widen a narrow credential)", blocked.StatusCode)
	}

	// A classic PAT still exchanges for a session.
	ok := s.post(t, "/auth/token", defaultToken, nil)
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("classic PAT exchange = %d, want 200", ok.StatusCode)
	}
}
