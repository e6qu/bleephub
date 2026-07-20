package bleephub

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/rs/zerolog"
)

type shaAuthTestProvider struct {
	server      *httptest.Server
	signer      jose.Signer
	key         jose.JSONWebKey
	nonce       string
	issuer      string
	tokenIssuer string
}

func newShaAuthTestProvider(t *testing.T) *shaAuthTestProvider {
	return newShaAuthTestProviderWithIssuerSuffix(t, "")
}

func newShaAuthTestProviderWithIssuerSuffix(t *testing.T, suffix string) *shaAuthTestProvider {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: privateKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &shaAuthTestProvider{signer: signer, key: jose.JSONWebKey{Key: &privateKey.PublicKey, KeyID: "test-key", Algorithm: string(jose.RS256), Use: "sig"}, nonce: "expected-nonce"}
	provider.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeJSON(w, http.StatusOK, map[string]any{
				"issuer": provider.issuer, "authorization_endpoint": provider.server.URL + "/oauth2/auth",
				"token_endpoint": provider.server.URL + "/oauth2/token", "jwks_uri": provider.server.URL + "/jwks",
				"end_session_endpoint": provider.server.URL + "/oauth2/sessions/logout",
			})
		case "/jwks":
			writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{provider.key}})
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil || r.PostForm.Get("code") == "" || r.PostForm.Get("code_verifier") == "" {
				http.Error(w, "invalid token request", http.StatusBadRequest)
				return
			}
			tokenIssuer := provider.tokenIssuer
			if tokenIssuer == "" {
				tokenIssuer = provider.issuer
			}
			rawIDToken := provider.sign(t, map[string]any{
				"iss": tokenIssuer, "aud": "bleephub", "sub": "subject-1", "sid": "sid-1",
				"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": provider.nonce,
				"preferred_username": "octocat", "name": "Octo Cat", "email": "octocat@example.com",
				"picture": "https://avatars.example.com/octocat.png", "role": "developer",
			})
			writeJSON(w, http.StatusOK, map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "id_token": rawIDToken})
		default:
			http.NotFound(w, r)
		}
	}))
	provider.issuer = provider.server.URL + suffix
	t.Cleanup(provider.server.Close)
	return provider
}

func (provider *shaAuthTestProvider) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := jwt.Signed(provider.signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (provider *shaAuthTestProvider) withClient(request *http.Request) *http.Request {
	return request.WithContext(oidc.ClientContext(request.Context(), provider.server.Client()))
}

func completeShauthIdentityConfig(issuer string) identityConfig {
	return identityConfig{
		shauthIssuer: issuer, shauthClientID: "bleephub", shauthClientSecret: "secret",
		shauthPostLogoutURL: "https://bleephub.example.test/ui/signed-out",
	}
}

func TestShauthIdentityConfigAllowsHTTPOnlyWhenExplicitlyEnabled(t *testing.T) {
	config := completeShauthIdentityConfig("http://localhost:4444")
	config.shauthPostLogoutURL = "http://localhost:15555/ui/signed-out"
	if err := config.validate(); err == nil {
		t.Fatal("HTTP OpenID Connect coordinates were accepted without the explicit local-test switch")
	}
	config.allowInsecureOIDC = true
	if err := config.validate(); err != nil {
		t.Fatalf("explicit local HTTP OpenID Connect coordinates: %v", err)
	}
	if err := validateShauthExternalURL(config, "http://localhost:15555"); err != nil {
		t.Fatalf("explicit local HTTP Bleephub origin: %v", err)
	}
}

func TestShauthIdentityConfigRejectsPartialCoordinates(t *testing.T) {
	for _, cfg := range []identityConfig{
		{shauthIssuer: "https://auth.example.test"},
		{shauthClientID: "bleephub"},
		{shauthClientSecret: "secret"},
		{shauthPostLogoutURL: "https://auth.example.test/apps"},
	} {
		if err := cfg.validate(); err == nil {
			t.Fatal("partial Shauth configuration unexpectedly validated")
		}
	}
	if err := (identityConfig{}).validate(); err != nil {
		t.Fatalf("disabled Shauth configuration: %v", err)
	}
	if err := completeShauthIdentityConfig("https://auth.example.test").validate(); err != nil {
		t.Fatalf("complete Shauth configuration: %v", err)
	}
}

func TestShauthIdentityConfigPreservesExactIssuer(t *testing.T) {
	t.Setenv("BLEEPHUB_SHAUTH_ISSUER", "https://auth.example.test/")
	if got := identityConfigFromEnv().shauthIssuer; got != "https://auth.example.test/" {
		t.Fatalf("Shauth issuer = %q, want exact trailing slash", got)
	}
}

func TestShauthRequiresHTTPSExternalOrigin(t *testing.T) {
	config := completeShauthIdentityConfig("https://auth.example.test")
	for _, value := range []string{"", "http://bleephub.example.test", "https://user@bleephub.example.test", "https://bleephub.example.test/path", "https://bleephub.example.test?query=1"} {
		if err := validateShauthExternalURL(config, value); err == nil {
			t.Errorf("external URL %q was accepted", value)
		}
	}
	if err := validateShauthExternalURL(config, "https://bleephub.example.test"); err != nil {
		t.Fatalf("valid external origin: %v", err)
	}
	config.shauthPostLogoutURL = "https://auth.example.test/apps"
	if err := validateShauthExternalURL(config, "https://bleephub.example.test"); err == nil {
		t.Fatal("cross-origin post-logout redirect was accepted")
	}
	config.shauthPostLogoutURL = "https://bleephub.example.test/ui/login"
	if err := validateShauthExternalURL(config, "https://bleephub.example.test"); err == nil {
		t.Fatal("auto-login post-logout redirect was accepted")
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

func TestIdentityValidationRequiresAuthenticationAndExposesSignOut(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig("https://auth.example.test")

	request := httptest.NewRequest(http.MethodGet, "/auth/validation", nil)
	response := httptest.NewRecorder()
	s.handleIdentityValidation(response, request)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/auth/shauth?return_to=%2Fauth%2Fvalidation" {
		t.Fatalf("anonymous validation = %d location %q", response.Code, response.Header().Get("Location"))
	}

	if err := s.store.PutLoginSession("browser-session", &LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/auth/validation", nil)
	request.AddCookie(&http.Cookie{Name: "_gh_sess", Value: "browser-session"})
	response = httptest.NewRecorder()
	s.handleIdentityValidation(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated validation = %d, want 200", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`<h1 id="authenticated-title">Bleephub is authenticated</h1>`,
		`<form method="post" action="/auth/logout"><button type="submit">Sign out</button></form>`,
		`@media(prefers-color-scheme:dark)`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("validation body omitted %q: %s", expected, body)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("validation security headers = %#v", response.Header())
	}
}

func TestShauthLogoutClearsLocalSessionAndStartsIssuerLogout(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	s.store.LoginSessions["browser-session"] = &LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour), OIDCProvider: "shauth", OIDCIssuer: provider.server.URL, OIDCSubject: "subject-1", OIDCSID: "sid-1", OIDCIDToken: "signed.id.token"}
	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Origin", s.externalURL)
	request.AddCookie(&http.Cookie{Name: "_gh_sess", Value: "browser-session"})
	request = provider.withClient(request)
	response := httptest.NewRecorder()

	s.handleIdentityLogout(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil || location.Path != "/oauth2/sessions/logout" {
		t.Fatalf("logout location = %q (%v)", response.Header().Get("Location"), err)
	}
	if location.Query().Get("id_token_hint") != "signed.id.token" || location.Query().Get("post_logout_redirect_uri") != "https://bleephub.example.test/ui/signed-out" {
		t.Fatalf("logout query = %v", location.Query())
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

func TestShauthLogoutRevokesLocalSessionBeforeDiscoveryFailure(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	provider.server.Close()
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	if err := s.store.PutLoginSession("browser-session", &LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour), OIDCProvider: "shauth"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Origin", s.externalURL)
	request.AddCookie(&http.Cookie{Name: "_gh_sess", Value: "browser-session"})
	request = provider.withClient(request)
	response := httptest.NewRecorder()
	s.handleIdentityLogout(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("logout discovery failure = %d, want 502", response.Code)
	}
	if session, err := s.store.GetLoginSession("browser-session"); err != nil || session != nil {
		t.Fatalf("local browser session survived provider failure: session=%#v err=%v", session, err)
	}
	foundExpired := false
	for _, cookie := range response.Result().Cookies() {
		foundExpired = foundExpired || (cookie.Name == "_gh_sess" && cookie.MaxAge < 0)
	}
	if !foundExpired {
		t.Fatal("provider failure response did not expire the local browser cookie")
	}
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
	s.identity = completeShauthIdentityConfig(issuer)
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
	s.identity = completeShauthIdentityConfig(issuer)
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

func TestShauthCallbackPersistsVerifiedOIDCSession(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	state, err := randomIdentityState()
	if err != nil {
		t.Fatal(err)
	}
	pending := identityState{Provider: "shauth", State: state, ReturnTo: "/ui/repositories", Nonce: provider.nonce, PKCE: "verifier", ExpiresAt: time.Now().Add(time.Minute)}
	stateResponse := httptest.NewRecorder()
	if err := s.setIdentityState(stateResponse, pending); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/shauth/callback?state="+url.QueryEscape(state)+"&code=code", nil)
	request.AddCookie(stateResponse.Result().Cookies()[0])
	request = provider.withClient(request)
	response := httptest.NewRecorder()
	s.handleShauthCallback(response, request)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/ui/repositories" {
		t.Fatalf("callback = %d %q: %s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	var browserCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "_gh_sess" && cookie.MaxAge >= 0 {
			browserCookie = cookie
		}
	}
	if browserCookie == nil {
		t.Fatal("callback omitted browser session cookie")
	}
	session, err := s.store.GetLoginSession(browserCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.OIDCProvider != "shauth" || session.OIDCIssuer != provider.server.URL || session.OIDCSubject != "subject-1" || session.OIDCSID != "sid-1" || session.OIDCIDToken == "" {
		t.Fatalf("verified OpenID Connect session = %#v", session)
	}
}

func TestShauthCallbackRejectsOIDCArtifactFromAnotherIssuer(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	provider.tokenIssuer = "https://attacker.example.test"
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = completeShauthIdentityConfig(provider.server.URL)

	login := httptest.NewRecorder()
	s.handleShauthLogin(login, provider.withClient(httptest.NewRequest(http.MethodGet, "/auth/shauth", nil)))
	redirect, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewRequest(http.MethodGet, "/auth/shauth/callback?state="+url.QueryEscape(redirect.Query().Get("state"))+"&code=code", nil)
	for _, cookie := range login.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	s.handleShauthCallback(response, provider.withClient(callback))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("foreign-issuer callback = %d, want 401: %s", response.Code, response.Body.String())
	}
	s.store.mu.RLock()
	sessions := len(s.store.LoginSessions)
	s.store.mu.RUnlock()
	if sessions != 0 {
		t.Fatalf("foreign-issuer callback created %d browser sessions", sessions)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "_gh_sess" && cookie.Value != "" && cookie.MaxAge >= 0 {
			t.Fatalf("foreign-issuer callback set browser session cookie %#v", cookie)
		}
	}
}

func TestShauthBackChannelLogoutVerifiesAndRevokesSessions(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	base := LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour), OIDCProvider: "shauth", OIDCIssuer: provider.server.URL, OIDCSubject: "subject-1"}
	revoked := base
	revoked.OIDCSID = "sid-1"
	kept := base
	kept.OIDCSID = "sid-2"
	if err := s.store.PutLoginSession("revoked", &revoked); err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutLoginSession("kept", &kept); err != nil {
		t.Fatal(err)
	}
	logoutExpiry := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	logoutToken := provider.sign(t, map[string]any{
		"iss": provider.server.URL, "aud": "bleephub", "sub": "subject-1", "sid": "sid-1",
		"iat": time.Now().Unix(), "exp": logoutExpiry.Unix(), "jti": "logout-sid",
		"events": map[string]any{backChannelLogoutEvent: map[string]any{}},
	})
	form := url.Values{"logout_token": {logoutToken}}
	request := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request = provider.withClient(request)
	response := httptest.NewRecorder()
	s.handleShauthBackChannelLogout(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("back-channel logout = %d: %s", response.Code, response.Body.String())
	}
	replayKey := oidcLogoutReplayKey("shauth", provider.server.URL, "bleephub", "logout-sid")
	if replayExpiry := s.store.OIDCLogoutClaims[replayKey]; !replayExpiry.Equal(logoutExpiry) {
		t.Fatalf("replay expiry = %s, want logout-token expiry %s", replayExpiry, logoutExpiry)
	}
	if session, _ := s.store.GetLoginSession("revoked"); session != nil {
		t.Fatal("sid-matched session remained")
	}
	if session, _ := s.store.GetLoginSession("kept"); session == nil {
		t.Fatal("unrelated sid session was revoked")
	}

	subject1 := base
	subject1.OIDCSID = "sid-subject-1"
	subject2 := base
	subject2.OIDCSID = "sid-subject-2"
	if err := s.store.PutLoginSession("subject-1", &subject1); err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutLoginSession("subject-2", &subject2); err != nil {
		t.Fatal(err)
	}
	subjectToken := provider.sign(t, map[string]any{
		"iss": provider.server.URL, "aud": "bleephub", "sub": "subject-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "jti": "logout-subject",
		"events": map[string]any{backChannelLogoutEvent: map[string]any{}},
	})
	subjectForm := url.Values{"logout_token": {subjectToken}}
	subjectRequest := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(subjectForm.Encode()))
	subjectRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	subjectRequest = provider.withClient(subjectRequest)
	subjectResponse := httptest.NewRecorder()
	s.handleShauthBackChannelLogout(subjectResponse, subjectRequest)
	if subjectResponse.Code != http.StatusNoContent {
		t.Fatalf("subject logout = %d: %s", subjectResponse.Code, subjectResponse.Body.String())
	}
	for _, id := range []string{"kept", "subject-1", "subject-2"} {
		if session, _ := s.store.GetLoginSession(id); session != nil {
			t.Fatalf("subject-matched session %q remained", id)
		}
	}

	replay := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(subjectForm.Encode()))
	replay.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replay = provider.withClient(replay)
	replayResponse := httptest.NewRecorder()
	s.handleShauthBackChannelLogout(replayResponse, replay)
	if replayResponse.Code != http.StatusBadRequest {
		t.Fatalf("logout token replay = %d, want 400", replayResponse.Code)
	}
}

func TestShauthBackChannelLogoutRejectsNonObjectEventValues(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	for name, event := range map[string]any{
		"null":   nil,
		"array":  []any{},
		"string": "",
	} {
		t.Run(name, func(t *testing.T) {
			logoutToken := provider.sign(t, map[string]any{
				"iss": provider.server.URL, "aud": "bleephub", "sub": "subject-1", "sid": "sid-1",
				"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "jti": "invalid-event-" + name,
				"events": map[string]any{backChannelLogoutEvent: event},
			})
			form := url.Values{"logout_token": {logoutToken}}
			request := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			s.handleShauthBackChannelLogout(response, provider.withClient(request))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid event value response = %d, want 400", response.Code)
			}
		})
	}
}

func TestShauthBackChannelLogoutRequiresAndValidatesExpiry(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	for name, expiry := range map[string]any{"missing": nil, "expired": time.Now().Add(-time.Minute).Unix()} {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{
				"iss": provider.server.URL, "aud": "bleephub", "sub": "subject-1", "sid": "sid-1",
				"iat": time.Now().Unix(), "jti": "expiry-" + name,
				"events": map[string]any{backChannelLogoutEvent: map[string]any{}},
			}
			if expiry != nil {
				claims["exp"] = expiry
			}
			logoutToken := provider.sign(t, claims)
			form := url.Values{"logout_token": {logoutToken}}
			request := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			s.handleShauthBackChannelLogout(response, provider.withClient(request))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid expiry response = %d, want 400", response.Code)
			}
		})
	}
}

func TestShauthBackChannelLogoutRejectsNonEmptyLogoutEvent(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	logoutToken := provider.sign(t, map[string]any{
		"iss": provider.server.URL, "aud": "bleephub", "sub": "subject-1", "sid": "sid-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "jti": "event-members",
		"events": map[string]any{
			backChannelLogoutEvent:     map[string]any{"extension": true},
			"https://example.test/evt": map[string]any{},
		},
	})
	form := url.Values{"logout_token": {logoutToken}}
	request := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	s.handleShauthBackChannelLogout(response, provider.withClient(request))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-empty logout event response = %d, want 400: %s", response.Code, response.Body.String())
	}
}

func TestShauthFrontChannelLogoutRevokesOnlyTrustedSession(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig("https://auth.example.test")
	for id, sid := range map[string]string{"revoked": "sid-1", "kept": "sid-2"} {
		if err := s.store.PutLoginSession(id, &LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour), OIDCProvider: "shauth", OIDCIssuer: s.identity.shauthIssuer, OIDCSID: sid}); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/shauth/frontchannel-logout?iss=https%3A%2F%2Fattacker.example&sid=sid-1", nil)
	response := httptest.NewRecorder()
	s.handleShauthFrontChannelLogout(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("untrusted front-channel response = %d headers=%v", response.Code, response.Header())
	}
	if session, _ := s.store.GetLoginSession("revoked"); session == nil {
		t.Fatal("untrusted front-channel request revoked a session")
	}

	request = httptest.NewRequest(http.MethodGet, "/auth/shauth/frontchannel-logout?iss=https%3A%2F%2Fauth.example.test&sid=sid-1", nil)
	response = httptest.NewRecorder()
	s.handleShauthFrontChannelLogout(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("trusted front-channel response = %d", response.Code)
	}
	if session, _ := s.store.GetLoginSession("revoked"); session != nil {
		t.Fatal("trusted sid-matched session remained")
	}
	if session, _ := s.store.GetLoginSession("kept"); session == nil {
		t.Fatal("unrelated provider session was revoked")
	}
}

func TestShauthFrontChannelLogoutDoesNothingWhenShauthIsDisabled(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	if err := s.store.PutLoginSession("kept", &LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour), OIDCProvider: "shauth", OIDCSID: "sid-1"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/shauth/frontchannel-logout?iss=&sid=sid-1", nil)
	response := httptest.NewRecorder()
	s.handleShauthFrontChannelLogout(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("disabled front-channel response = %d", response.Code)
	}
	if session, _ := s.store.GetLoginSession("kept"); session == nil {
		t.Fatal("disabled Shauth configuration revoked a session")
	}
}

func TestShauthBackChannelLogoutRejectsNonceByPresence(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	for name, nonce := range map[string]any{"empty": "", "null": nil} {
		t.Run(name, func(t *testing.T) {
			logoutToken := provider.sign(t, map[string]any{
				"iss": provider.server.URL, "aud": "bleephub", "sub": "subject-1", "sid": "sid-1",
				"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "jti": "nonce-" + name,
				"nonce": nonce, "events": map[string]any{backChannelLogoutEvent: map[string]any{}},
			})
			form := url.Values{"logout_token": {logoutToken}}
			request := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			s.handleShauthBackChannelLogout(response, provider.withClient(request))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("nonce-bearing logout token response = %d, want 400", response.Code)
			}
		})
	}
}

func TestShauthBackChannelLogoutPreservesTrailingSlashIssuer(t *testing.T) {
	provider := newShaAuthTestProviderWithIssuerSuffix(t, "/")
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig(provider.issuer)
	session := &LoginSession{
		UserID: 1, ExpiresAt: time.Now().Add(time.Hour), OIDCProvider: "shauth",
		OIDCIssuer: provider.issuer, OIDCSubject: "subject-1", OIDCSID: "sid-1",
	}
	if err := s.store.PutLoginSession("trailing-slash", session); err != nil {
		t.Fatal(err)
	}
	logoutToken := provider.sign(t, map[string]any{
		"iss": provider.issuer, "aud": "bleephub", "sub": "subject-1", "sid": "sid-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "jti": "trailing-slash",
		"events": map[string]any{backChannelLogoutEvent: map[string]any{}},
	})
	form := url.Values{"logout_token": {logoutToken}}
	request := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	s.handleShauthBackChannelLogout(response, provider.withClient(request))
	if response.Code != http.StatusNoContent {
		t.Fatalf("trailing-slash issuer logout = %d: %s", response.Code, response.Body.String())
	}
	if got, err := s.store.GetLoginSession("trailing-slash"); err != nil || got != nil {
		t.Fatalf("exact-issuer session survived: session=%#v err=%v", got, err)
	}
}

func TestShauthBackChannelLogoutRejectsTamperedSignature(t *testing.T) {
	provider := newShaAuthTestProvider(t)
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.identity = completeShauthIdentityConfig(provider.server.URL)
	logoutToken := provider.sign(t, map[string]any{
		"iss": provider.server.URL, "aud": "bleephub", "sub": "subject-1", "sid": "sid-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "jti": "tampered",
		"events": map[string]any{backChannelLogoutEvent: map[string]any{}},
	})
	parts := strings.Split(logoutToken, ".")
	if len(parts) != 3 || len(parts[2]) == 0 {
		t.Fatalf("signed logout token has %d parts", len(parts))
	}
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	form := url.Values{"logout_token": {strings.Join(parts, ".")}}
	request := httptest.NewRequest(http.MethodPost, "/auth/shauth/backchannel-logout", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request = provider.withClient(request)
	response := httptest.NewRecorder()
	s.handleShauthBackChannelLogout(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("tampered logout token = %d, want 400", response.Code)
	}
}

func TestSignedOutLandingRevokesLocalSessionWithoutStartingLogin(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = completeShauthIdentityConfig("https://auth.example.test")
	if err := s.store.PutLoginSession("browser-session", &LoginSession{UserID: 1, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/ui/signed-out", nil)
	request.AddCookie(&http.Cookie{Name: "_gh_sess", Value: "browser-session"})
	response := httptest.NewRecorder()
	s.handleIdentitySignedOut(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Location") != "" {
		t.Fatalf("signed-out landing = %d location %q", response.Code, response.Header().Get("Location"))
	}
	if session, err := s.store.GetLoginSession("browser-session"); err != nil || session != nil {
		t.Fatalf("signed-out landing retained session %#v: %v", session, err)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`<span>Bleephub</span>`,
		`<h1 id="signed-out-title">You are signed out</h1>`,
		`<a href="/auth/shauth?return_to=%2Fui%2F">Sign in with Shauth</a>`,
		`<form method="post" action="/auth/logout"><button type="submit">Sign out</button></form>`,
		`@media(prefers-color-scheme:dark)`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("signed-out body omitted %q: %s", expected, body)
		}
	}
	if strings.Contains(body, "window.location") || strings.Contains(strings.ToLower(body), "access token") {
		t.Fatalf("signed-out body unexpectedly starts sign-in: %s", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("signed-out security headers = %#v", response.Header())
	}
	if cookies := response.Result().Cookies(); len(cookies) != 1 || cookies[0].Name != "_gh_sess" || cookies[0].MaxAge >= 0 {
		t.Fatalf("signed-out cookies = %#v", cookies)
	}

	reload := httptest.NewRecorder()
	s.handleIdentitySignedOut(reload, httptest.NewRequest(http.MethodGet, "/ui/signed-out", nil))
	if reload.Code != http.StatusOK || reload.Header().Get("Location") != "" || reload.Body.String() != body {
		t.Fatalf("signed-out reload was not stable: status=%d location=%q", reload.Code, reload.Header().Get("Location"))
	}
}

func TestShauthLogoutRejectsCrossOriginPost(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = completeShauthIdentityConfig("https://auth.example.test")
	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	s.handleIdentityLogout(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout = %d, want 403", response.Code)
	}
}

func TestIdentityStateIsBrowserBoundAndTamperEvident(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"
	s.identity = completeShauthIdentityConfig("https://auth.example.test")
	s.identity.shauthClientSecret = "0123456789abcdef0123456789abcdef"
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
	s.identity = completeShauthIdentityConfig("https://auth.example.test")
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

func TestSafeIdentityReturnToRejectsExternalDestinations(t *testing.T) {
	for _, value := range []string{"", "https://attacker.example", "//attacker.example/path", `/\attacker.example/path`, "not-a-path"} {
		if got := safeIdentityReturnTo(value); got != "/ui/" {
			t.Errorf("safeIdentityReturnTo(%q) = %q", value, got)
		}
	}
	if got := safeIdentityReturnTo("/ui/repos?tab=mine"); got != "/ui/repos?tab=mine" {
		t.Fatalf("safe local destination = %q", got)
	}
}
