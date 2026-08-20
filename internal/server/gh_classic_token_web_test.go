package bleephub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func classicTokenWebTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHAppsOAuthMgmtRoutes()
	// A second authenticated /ui-data surface to prove minted (and expired)
	// classic tokens are honored/refused by the shared auth middleware.
	s.registerGHAchievementsRoutes()
	return s
}

func TestClassicTokenWeb_CreateWithExpiryStoredAndReflected(t *testing.T) {
	s := classicTokenWebTestServer(t)
	expiresAt := fixedTestTime.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)

	w := serveTestRequest(s, bearerHeader(adminPAT), "POST", "/ui-data/user/tokens/classic",
		[]byte(`{"note":"ci token","scopes":["repo","read:org"],"expires_at":"`+expiresAt+`"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	value, _ := created["token"].(string)
	if !strings.HasPrefix(value, "ghp_") {
		t.Fatalf("token = %q, want a ghp_-prefixed classic PAT revealed once", value)
	}
	if created["expires_at"] != expiresAt {
		t.Fatalf("expires_at = %v, want %q", created["expires_at"], expiresAt)
	}
	if created["note"] != "ci token" {
		t.Fatalf("note = %v, want %q", created["note"], "ci token")
	}
	scopes, _ := created["scopes"].([]interface{})
	if len(scopes) != 2 || scopes[0] != "repo" || scopes[1] != "read:org" {
		t.Fatalf("scopes = %v, want [repo read:org]", created["scopes"])
	}

	// Stored: the token resolves and carries the expiry.
	token, user := s.store.LookupToken(value)
	if token == nil || user == nil || user.Login != "admin" {
		t.Fatalf("minted token did not resolve to admin (token=%v user=%v)", token, user)
	}
	if token.ExpiresAt == nil || token.ExpiresAt.UTC().Format(time.RFC3339) != expiresAt {
		t.Fatalf("stored ExpiresAt = %v, want %s", token.ExpiresAt, expiresAt)
	}

	// The not-yet-expired token authenticates an API call.
	w = serveTestRequest(s, bearerHeader(value), "GET", "/ui-data/users/admin/achievements", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("fresh classic token refused: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestClassicTokenWeb_ExpiredTokenIsRefused(t *testing.T) {
	s := classicTokenWebTestServer(t)
	// Minted already expired relative to the fixed test clock.
	expired := fixedTestTime.Add(-time.Hour).UTC().Format(time.RFC3339)

	w := serveTestRequest(s, bearerHeader(adminPAT), "POST", "/ui-data/user/tokens/classic",
		[]byte(`{"note":"stale","scopes":["repo"],"expires_at":"`+expired+`"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &created)
	value, _ := created["token"].(string)

	w = serveTestRequest(s, bearerHeader(value), "GET", "/ui-data/users/admin/achievements", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired classic token status = %d, want 401; body = %s", w.Code, w.Body.String())
	}

	// The same request with a live credential succeeds — the refusal above is
	// the expiry, not the route.
	w = serveTestRequest(s, bearerHeader(adminPAT), "GET", "/ui-data/users/admin/achievements", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("control request status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestClassicTokenWeb_NoExpiryUnchanged(t *testing.T) {
	s := classicTokenWebTestServer(t)

	for name, body := range map[string]string{
		"absent": `{"note":"forever","scopes":["repo"]}`,
		"null":   `{"note":"forever","scopes":["repo"],"expires_at":null}`,
	} {
		w := serveTestRequest(s, bearerHeader(adminPAT), "POST", "/ui-data/user/tokens/classic", []byte(body))
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d, body = %s", name, w.Code, w.Body.String())
		}
		var created map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if created["expires_at"] != nil {
			t.Fatalf("%s: expires_at = %v, want null", name, created["expires_at"])
		}
		value, _ := created["token"].(string)
		token, _ := s.store.LookupToken(value)
		if token == nil || token.ExpiresAt != nil {
			t.Fatalf("%s: stored token = %+v, want resolvable with nil ExpiresAt", name, token)
		}
	}
}

func TestClassicTokenWeb_BadExpiryIs422(t *testing.T) {
	s := classicTokenWebTestServer(t)
	w := serveTestRequest(s, bearerHeader(adminPAT), "POST", "/ui-data/user/tokens/classic",
		[]byte(`{"note":"bad","scopes":[],"expires_at":"tomorrow"}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
}
