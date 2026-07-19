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

func TestIdentitySessionReportsAuthenticationWithoutExpectedNetworkErrors(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	request := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
	response := httptest.NewRecorder()

	s.handleIdentitySession(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("anonymous session status = %d, want %d", response.Code, http.StatusOK)
	}
	var anonymous map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &anonymous); err != nil {
		t.Fatal(err)
	}
	if authenticated, _ := anonymous["authenticated"].(bool); authenticated {
		t.Fatal("anonymous session reported authenticated")
	}

	s.store.LoginSessions["browser-session"] = &LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour)}
	request = httptest.NewRequest(http.MethodGet, "/auth/session", nil)
	request.AddCookie(&http.Cookie{Name: "_gh_sess", Value: "browser-session"})
	response = httptest.NewRecorder()
	s.handleIdentitySession(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated session status = %d, want %d", response.Code, http.StatusOK)
	}
	var authenticated map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &authenticated); err != nil {
		t.Fatal(err)
	}
	if valid, _ := authenticated["authenticated"].(bool); !valid {
		t.Fatalf("authenticated session response = %s", response.Body.String())
	}
	if authenticated["user"] == nil {
		t.Fatalf("authenticated session omitted user: %s", response.Body.String())
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

func TestShauthCallbackUsesClientSecretPost(t *testing.T) {
	var issuer string
	var tokenForm url.Values
	var tokenAuthorization string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 issuer,
				"authorization_endpoint": issuer + "/oauth2/auth",
				"token_endpoint":         issuer + "/oauth2/token",
				"jwks_uri":               issuer + "/.well-known/jwks.json",
			})
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			tokenForm = r.PostForm
			tokenAuthorization = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"access_token":"token","token_type":"Bearer"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	issuer = idp.URL

	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = identityConfig{shauthIssuer: issuer, shauthClientID: "bleephub", shauthClientSecret: "secret"}
	loginResponse := httptest.NewRecorder()
	s.handleShauthLogin(loginResponse, httptest.NewRequest(http.MethodGet, "/auth/shauth", nil))
	redirect, err := url.Parse(loginResponse.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewRequest(http.MethodGet, "/auth/shauth/callback?state="+url.QueryEscape(redirect.Query().Get("state"))+"&code=code", nil)
	for _, cookie := range loginResponse.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	callbackServer := NewServer("127.0.0.1:0", zerolog.Nop())
	callbackServer.externalURL = s.externalURL
	callbackServer.identity = s.identity
	callbackServer.handleShauthCallback(httptest.NewRecorder(), callback)

	if got, want := tokenForm.Get("client_id"), "bleephub"; got != want {
		t.Fatalf("token client_id = %q, want %q", got, want)
	}
	if got, want := tokenForm.Get("client_secret"), "secret"; got != want {
		t.Fatalf("token client_secret = %q, want %q", got, want)
	}
	if tokenAuthorization != "" {
		t.Fatalf("token request used HTTP Basic authentication: %q", tokenAuthorization)
	}
}

func TestIdentityStateIsBrowserBoundAndTamperEvident(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = identityConfig{shauthIssuer: "https://auth.example.test", shauthClientID: "bleephub", shauthClientSecret: "0123456789abcdef0123456789abcdef"}
	response := httptest.NewRecorder()
	state, err := randomIdentityState()
	if err != nil {
		t.Fatal(err)
	}
	pending := identityState{Provider: "shauth", State: state, ReturnTo: "/ui/", ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.setIdentityState(response, pending); err != nil {
		t.Fatal(err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("identity cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	last := "A"
	if cookie.Value[len(cookie.Value)-1:] == last {
		last = "B"
	}
	cookie.Value = cookie.Value[:len(cookie.Value)-1] + last
	request := httptest.NewRequest(http.MethodGet, "/auth/shauth/callback?state="+state, nil)
	request.AddCookie(cookie)
	if _, err := s.consumeIdentityState(httptest.NewRecorder(), request, "shauth", state); err == nil {
		t.Fatal("tampered identity state was accepted")
	}
}

func TestIdentityStateSupportsConcurrentBrowserFlows(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = identityConfig{shauthClientSecret: "0123456789abcdef0123456789abcdef"}
	states := make([]string, 2)
	cookies := make([]*http.Cookie, 2)
	for index := range states {
		state, err := randomIdentityState()
		if err != nil {
			t.Fatal(err)
		}
		states[index] = state
		response := httptest.NewRecorder()
		if err := s.setIdentityState(response, identityState{Provider: "shauth", State: state, ReturnTo: "/ui/", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		cookies[index] = response.Result().Cookies()[0]
	}
	if cookies[0].Name == cookies[1].Name {
		t.Fatalf("concurrent identity flows reused cookie %q", cookies[0].Name)
	}
	for index, state := range states {
		request := httptest.NewRequest(http.MethodGet, "/auth/shauth/callback?state="+state, nil)
		request.AddCookie(cookies[index])
		pending, err := s.consumeIdentityState(httptest.NewRecorder(), request, "shauth", state)
		if err != nil {
			t.Fatalf("consume concurrent state %d: %v", index, err)
		}
		if pending.State != state {
			t.Fatalf("concurrent state %d = %q, want %q", index, pending.State, state)
		}
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
