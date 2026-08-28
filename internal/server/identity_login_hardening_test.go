package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// makeLocalPasswordUser inserts a user with a bcrypt password hash keyed under
// the given (already-canonical) login, bypassing the creation handler so the
// login path can be exercised in isolation.
func makeLocalPasswordUser(t *testing.T, s *Server, login, password string) *store.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	if _, exists := s.store.UsersByLogin[login]; exists {
		t.Fatalf("user %q already exists", login)
	}
	u := &store.User{
		ID:           s.store.NextUser,
		Login:        login,
		Type:         "User",
		PasswordHash: string(hash),
		StarredRepos: map[string]time.Time{},
	}
	s.store.Users[u.ID] = u
	s.store.UsersByLogin[u.Login] = u
	s.store.NextUser++
	return u
}

func localLoginRequest(t *testing.T, login, password string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]string{"login": login, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/local", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func createInternalUserRequest(t *testing.T, s *Server, login string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]string{"login": login, "email": "x@local", "password": "pw"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	admin := s.store.LookupUserByLogin("admin")
	req := httptest.NewRequest(http.MethodPost, "/internal/users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(contextWithUser(req.Context(), admin))
}

// TestNormalizeLoginAppliesNFKCAndCaseFolding is the unit-level guard for the
// Username canonicalization: "Alice", "alice", and fullwidth "ＡＬＩＣＥ" must
// collapse to a single canonical key, while a Latin/Cyrillic mix is rejected.
func TestNormalizeLoginAppliesNFKCAndCaseFolding(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"empty", "", "", false},
		{"already canonical", "alice", "alice", false},
		{"ascii uppercase", "ALICE", "alice", false},
		{"mixed case", "Alice", "alice", false},
		{"fullwidth collapses via NFKC", "\uff41\uff4c\uff49\uff43\uff45", "alice", false},
		{"cyrillic only is not mixed", "\u0430\u043b\u0438\u0446\u0435", "\u0430\u043b\u0438\u0446\u0435", false},
		{"latin and cyrillic mix rejected", "\u0430lice", "", true},
		{"trailing latin with cyrillic rejected", "ali\u0441e", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeLogin(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("normalizeLogin(%q) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeLogin(%q) error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("normalizeLogin(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestParseAllowedLogins is case-insensitive and drops mixed-script entries,
// returning nil (admit-all) for an empty or entirely-invalid value.
func TestParseAllowedLogins(t *testing.T) {
	if got := parseAllowedLogins(""); got != nil {
		t.Fatalf("parseAllowedLogins(\"\") = %v, want nil", got)
	}
	if got := parseAllowedLogins("  "); got != nil {
		t.Fatalf("parseAllowedLogins blanks = %v, want nil", got)
	}
	allow := parseAllowedLogins("Alice, bob ,\u0430lice")
	want := map[string]struct{}{"alice": {}, "bob": {}}
	if len(allow) != len(want) {
		t.Fatalf("parseAllowedLogins = %v, want %v", allow, want)
	}
	for k := range want {
		if _, ok := allow[k]; !ok {
			t.Errorf("parseAllowedLogins missing normalized entry %q", k)
		}
	}
	cfg := identityConfig{allowedLogins: allow}
	if !cfg.loginAllowed("ALICE") || !cfg.loginAllowed("bob") {
		t.Error("loginAllowed is not case-insensitive over the normalized set")
	}
	if cfg.loginAllowed("carol") {
		t.Error("loginAllowed admitted a login not on the allowlist")
	}
	if (identityConfig{}).loginAllowed("anyone") == false {
		t.Error("absent allowlist must admit any login")
	}
}

// TestLocalLoginCaseFoldsBeforeLookup proves the account-collapse
// property end to end: an account stored as "alice" authenticates when the
// caller spells the login "ALICE" or with fullwidth letters.
func TestLocalLoginCaseFoldsBeforeLookup(t *testing.T) {
	s := newTestServer()
	const password = "correct horse battery staple"
	makeLocalPasswordUser(t, s, "alice", password)

	for _, supplied := range []string{"alice", "ALICE", "AlIcE", "\uff41\uff4c\uff49\uff43\uff45"} {
		w := httptest.NewRecorder()
		s.handleLocalLogin(w, localLoginRequest(t, supplied, password))
		if w.Code != http.StatusOK {
			t.Fatalf("login %q = %d body=%s, want 200", supplied, w.Code, w.Body.String())
		}
	}
}

// TestLocalLoginRejectsCyrillicConfusable ensures a Latin account cannot be
// shadowed by its Cyrillic lookalike: "аlice" (first letter Cyrillic U+0430)
// is rejected before any lookup, so it neither authenticates nor silently
// creates a sibling identity.
func TestLocalLoginRejectsCyrillicConfusable(t *testing.T) {
	s := newTestServer()
	const password = "correct horse battery staple"
	makeLocalPasswordUser(t, s, "alice", password)

	w := httptest.NewRecorder()
	s.handleLocalLogin(w, localLoginRequest(t, "\u0430lice", password))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cyrillic-confusable login = %d, want 400", w.Code)
	}

	// A wrong-case but valid Latin spelling still fails closed on a bad
	// password rather than leaking that the account exists.
	w = httptest.NewRecorder()
	s.handleLocalLogin(w, localLoginRequest(t, "ALICE", "wrong"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", w.Code)
	}
}

// TestLocalLoginAllowlistRejectsLoginsNotListed enforces the
// BLEEPHUB_ALLOWED_LOGINS gate at the local login path: with an allowlist of
// {alice}, an allowed user signs in while a non-allowlisted user is refused
// before the password is ever checked.
func TestLocalLoginAllowlistRejectsLoginsNotListed(t *testing.T) {
	s := newTestServer()
	const password = "correct horse battery staple"
	makeLocalPasswordUser(t, s, "alice", password)
	makeLocalPasswordUser(t, s, "bob", password)
	s.identity.allowedLogins = map[string]struct{}{"alice": {}}

	w := httptest.NewRecorder()
	s.handleLocalLogin(w, localLoginRequest(t, "alice", password))
	if w.Code != http.StatusOK {
		t.Fatalf("allowlisted login = %d, want 200", w.Code)
	}

	// A non-allowlisted login is refused with the SAME 401 as a wrong password
	// so the status code cannot enumerate allowlist membership.
	w = httptest.NewRecorder()
	s.handleLocalLogin(w, localLoginRequest(t, "bob", password))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-allowlisted login = %d, want 401", w.Code)
	}
	// And a wrong password for an allowlisted login returns the identical 401.
	w = httptest.NewRecorder()
	s.handleLocalLogin(w, localLoginRequest(t, "alice", "wrong-password"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", w.Code)
	}
}

// TestInternalUserCreationFoldsCase rejects duplicate lookalike accounts at the
// creation choke point: after "Alice" is created and stored as "alice", a
// second "alice" collides, and a Cyrillic mix never reaches the store.
func TestInternalUserCreationFoldsCase(t *testing.T) {
	s := newTestServer()

	first := httptest.NewRecorder()
	s.handleCreateUserInternal(first, createInternalUserRequest(t, s, "Alice"))
	if first.Code != http.StatusCreated {
		t.Fatalf("create Alice = %d body=%s, want 201", first.Code, first.Body.String())
	}
	if u := s.store.LookupUserByLogin("alice"); u == nil {
		t.Fatalf("Alice was not stored under its case-folded login %q", "alice")
	}
	// Case-variant lookups intentionally resolve (GitHub parity via the
	// folded index) — canonical storage is proven by the resolved login.
	if u := s.store.LookupUserByLogin("Alice"); u == nil || u.Login != "alice" {
		t.Fatalf("case-variant lookup = %+v, want the canonical alice account", u)
	}

	dup := httptest.NewRecorder()
	s.handleCreateUserInternal(dup, createInternalUserRequest(t, s, "alice"))
	if dup.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate case-folded login = %d, want 422 already_exists", dup.Code)
	}
	if !strings.Contains(dup.Body.String(), "already_exists") {
		t.Fatalf("duplicate body = %s, want already_exists", dup.Body.String())
	}

	confusable := httptest.NewRecorder()
	s.handleCreateUserInternal(confusable, createInternalUserRequest(t, s, "\u0430lice"))
	if confusable.Code != http.StatusBadRequest {
		t.Fatalf("cyrillic-confusable create = %d, want 400", confusable.Code)
	}
}

// TestInternalUserCreationAllowlistEnforced gates /internal/users creation on
// the configured allowlist.
func TestInternalUserCreationAllowlistEnforced(t *testing.T) {
	s := newTestServer()
	s.identity.allowedLogins = map[string]struct{}{"carol": {}}

	ok := httptest.NewRecorder()
	s.handleCreateUserInternal(ok, createInternalUserRequest(t, s, "CAROL"))
	if ok.Code != http.StatusCreated {
		t.Fatalf("allowlisted create = %d body=%s, want 201", ok.Code, ok.Body.String())
	}

	denied := httptest.NewRecorder()
	s.handleCreateUserInternal(denied, createInternalUserRequest(t, s, "dave"))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("non-allowlisted create = %d, want 403", denied.Code)
	}
}

// TestUpsertExternalUserNeverEscalatesByUsername pins that a
// federated identity is resolved on (issuer, subject), and a principal cannot
// gain privileges by claiming a taken username. The primary IdP may adopt a
// LOCAL account (SSO ownership of the seeded bootstrap admin), but the role
// claim — not the account adopted — decides SiteAdmin, so a developer-role
// "admin" lands on a NON-admin account. Non-primary providers and accounts
// already bound to a different federated identity cannot be seized.
func TestUpsertExternalUserNeverEscalatesByUsername(t *testing.T) {
	s := newTestServer() // seeds admin/SiteAdmin, no external identity
	const issuer = "https://auth.example.test"

	// A developer-role principal claiming "admin" adopts the seeded account but
	// is NOT a SiteAdmin — the escalation the finding was about is closed.
	adopted, err := s.upsertExternalUser(issuer, "dev-subject", "admin", "A", "a@x", "", false, true)
	if err != nil || adopted == nil {
		t.Fatalf("primary-IdP adoption of the local admin failed: %+v err=%v", adopted, err)
	}
	if adopted.SiteAdmin {
		t.Fatal("a developer-role federated 'admin' became SiteAdmin — escalation")
	}

	// A SECONDARY provider may not adopt an existing account by username.
	if u, err := s.upsertExternalUser(issuer, "other-subject", "admin", "A", "a@x", "", true, false); err == nil {
		t.Fatalf("a secondary provider adopted an existing account: %+v", u)
	}
	// Nor may anyone seize the account now bound to dev-subject.
	if u, err := s.upsertExternalUser(issuer, "thief-subject", "admin", "A", "a@x", "", true, true); err == nil {
		t.Fatalf("a second identity seized an already-federated account: %+v", u)
	}

	// A fresh federated identity with a free login provisions normally.
	fresh, err := s.upsertExternalUser(issuer, "s-1", "octo", "Octo", "o@x", "", false, true)
	if err != nil || fresh == nil || fresh.SiteAdmin {
		t.Fatalf("fresh federated provisioning failed: user=%+v err=%v", fresh, err)
	}
	// Re-login on the same (issuer, subject) resolves the same account.
	again, err := s.upsertExternalUser(issuer, "s-1", "octo", "Octo Renamed", "o@x", "", false, true)
	if err != nil || again == nil || again.ID != fresh.ID {
		t.Fatalf("re-login did not resolve the same account: %+v/%+v err=%v", fresh, again, err)
	}
}

// TestUpsertExternalUserHonorsAllowlistAndAuthoritativeRole covers
// allowlist enforcement on the SSO path and the role-authority rule:
// the primary IdP writes SiteAdmin on every login (so a demotion takes effect),
// while a secondary provider never reshapes privileges.
func TestUpsertExternalUserHonorsAllowlistAndAuthoritativeRole(t *testing.T) {
	s := newTestServer()
	s.identity.allowedLogins = map[string]struct{}{"alice": {}}
	const issuer = "https://auth.example.test"

	if _, err := s.upsertExternalUser(issuer, "s-bob", "bob", "Bob", "b@x", "", false, true); err == nil {
		t.Fatal("allowlist did not block a federated login on the SSO path")
	}

	// alice is admitted and, as a primary-IdP admin, becomes SiteAdmin.
	alice, err := s.upsertExternalUser(issuer, "s-alice", "alice", "Alice", "a@x", "", true, true)
	if err != nil || alice == nil || !alice.SiteAdmin {
		t.Fatalf("authoritative admin login: user=%+v err=%v", alice, err)
	}
	// Demoted at the IdP: the next authoritative login revokes SiteAdmin.
	if again, err := s.upsertExternalUser(issuer, "s-alice", "alice", "Alice", "a@x", "", false, true); err != nil || again.SiteAdmin {
		t.Fatalf("authoritative demotion not honored: user=%+v err=%v", again, err)
	}
	// A non-authoritative (secondary) provider must never grant it back.
	if again, err := s.upsertExternalUser(issuer, "s-alice", "alice", "Alice", "a@x", "", true, false); err != nil || again.SiteAdmin {
		t.Fatalf("secondary provider reshaped privileges: user=%+v err=%v", again, err)
	}
}

// TestSecureDeploymentIgnoresUnprefixedSessionCookie pins that over HTTPS
// only the __Host- session cookie is honored, so a shadow _gh_sess planted by a
// related-domain or network attacker cannot authenticate — and logout expires
// both names so no shadow survives it.
func TestSecureDeploymentIgnoresUnprefixedSessionCookie(t *testing.T) {
	s := newTestServer()
	s.externalURL = "https://bleephub.example.test" // secureCookies(r) == true
	s.store.LoginSessions["attacker-session"] = &store.LoginSession{UserID: 1, ExpiresAt: s.currentTime().Add(time.Hour)}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "_gh_sess", Value: "attacker-session"})
	if s.sessionCookieFromRequest(req) != nil {
		t.Fatal("unprefixed _gh_sess was honored over HTTPS")
	}
	if s.sessionFromRequest(req) != nil {
		t.Fatal("a planted _gh_sess resolved to a session over HTTPS")
	}

	secureReq := httptest.NewRequest(http.MethodGet, "/", nil)
	secureReq.AddCookie(&http.Cookie{Name: secureSessionCookieName, Value: "attacker-session"})
	if s.sessionCookieFromRequest(secureReq) == nil {
		t.Fatal("__Host- session cookie was not honored over HTTPS")
	}
}
