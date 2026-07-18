package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestShauthIdentityConfigRejectsPartialCoordinates(t *testing.T) {
	for _, cfg := range []identityConfig{
		{shauthIssuer: "https://auth.example.test"},
		{shauthClientID: "bleephub"},
		{shauthClientSecret: "secret"},
	} {
		if err := cfg.validate(); err == nil {
			t.Fatal("partial Shauth configuration unexpectedly validated")
		}
	}
	if err := (identityConfig{}).validate(); err != nil {
		t.Fatalf("disabled Shauth configuration: %v", err)
	}
	if err := (identityConfig{shauthIssuer: "https://auth.example.test", shauthClientID: "bleephub", shauthClientSecret: "secret"}).validate(); err != nil {
		t.Fatalf("complete Shauth configuration: %v", err)
	}
}

func TestShauthLogoutClearsLocalSessionAndStartsIssuerLogout(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = identityConfig{shauthIssuer: "https://auth.example.test", shauthClientID: "bleephub", shauthClientSecret: "secret"}
	s.store.LoginSessions["browser-session"] = &LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour)}
	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: "_gh_sess", Value: "browser-session"})
	response := httptest.NewRecorder()

	s.handleIdentityLogout(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	if got, want := response.Header().Get("Location"), "https://auth.example.test/oauth2/sessions/logout"; got != want {
		t.Fatalf("logout location = %q, want %q", got, want)
	}
	if _, ok := s.store.LoginSessions["browser-session"]; ok {
		t.Fatal("local browser session remained after logout")
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "_gh_sess" && cookie.MaxAge < 0 {
			return
		}
	}
	t.Fatal("logout did not expire the local browser cookie")
}

func TestShauthLoginUsesDiscoveredAuthorizationEndpointAndPKCE(t *testing.T) {
	var issuer string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/oauth2/auth",
			"token_endpoint":         issuer + "/oauth2/token",
			"jwks_uri":               issuer + "/.well-known/jwks.json",
		})
	}))
	defer idp.Close()
	issuer = idp.URL

	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = identityConfig{shauthIssuer: issuer, shauthClientID: "bleephub", shauthClientSecret: "secret"}
	request := httptest.NewRequest(http.MethodGet, "/auth/shauth?return_to=%2Fui%2Frepositories", nil)
	response := httptest.NewRecorder()
	s.handleShauthLogin(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("Shauth login status = %d, body=%s", response.Code, response.Body.String())
	}
	redirect, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Path != "/oauth2/auth" || redirect.Query().Get("client_id") != "bleephub" {
		t.Fatalf("unexpected authorization redirect: %s", redirect)
	}
	if redirect.Query().Get("code_challenge_method") != "S256" || redirect.Query().Get("code_challenge") == "" || redirect.Query().Get("nonce") == "" || redirect.Query().Get("state") == "" {
		t.Fatalf("authorization request omitted PKCE, nonce, or state: %s", redirect)
	}
	if redirect.Query().Get("redirect_uri") != "https://bleephub.example.test/auth/shauth/callback" {
		t.Fatalf("redirect URI = %q", redirect.Query().Get("redirect_uri"))
	}
}

func TestLoginPageUsesShauthInsteadOfPersonalAccessTokenForm(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = identityConfig{shauthIssuer: "https://auth.example.test", shauthClientID: "bleephub", shauthClientSecret: "secret"}
	request := httptest.NewRequest(http.MethodGet, "/login?return_to=%2Fui%2Frepositories", nil)
	response := httptest.NewRecorder()
	s.handleLoginPage(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("Shauth login page status = %d, body=%s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/auth/shauth?return_to=%2Fui%2Frepositories" {
		t.Fatalf("Shauth login location = %q", location)
	}
}
