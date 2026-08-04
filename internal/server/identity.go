package bleephub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/text/unicode/norm"
)

// oidcHTTPTimeout bounds every outbound OpenID Connect discovery / JWKS fetch.
const oidcHTTPTimeout = 10 * time.Second

// audience decodes an OpenID Connect "aud" claim, which may be a single string
// or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var single string
	if json.Unmarshal(b, &single) == nil {
		*a = audience{single}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(b, &multi); err != nil {
		return err
	}
	*a = multi
	return nil
}

// oidcClientProvidedKey marks a context that already carries an OpenID Connect
// HTTP client (e.g. a test injecting one that trusts a self-signed issuer), so
// oidcClientContext leaves it in place instead of overriding it.
type oidcClientProvidedKey struct{}

// oidcClientContext returns a context whose OpenID Connect HTTP client carries a
// timeout, so a slow or hung identity provider cannot pin a goroutine and socket
// per request — otherwise an anonymous flood of /auth/* requests, each blocking
// on an unbounded outbound fetch, is a fd/goroutine-exhaustion vector. A client
// already provided on the context is preserved.
func (s *Server) oidcClientContext(ctx context.Context) context.Context {
	if ctx.Value(oidcClientProvidedKey{}) != nil {
		return ctx
	}
	// Route OIDC discovery / JWKS / token / end-session fetches through the same
	// SSRF address gate as webhook delivery and source imports. The issuer is
	// operator-configured, but a provider-controlled discovery document can point
	// the JWKS or end_session_endpoint at an internal/metadata address, so gate
	// the actual dial (Dialer.Control), not just the configured host. The timeout
	// still bounds a hung IdP.
	client := &http.Client{
		Timeout:   oidcHTTPTimeout,
		Transport: newAddressCheckedHTTPTransport(s.allowPrivateOutboundTargets, false),
	}
	return oidc.ClientContext(ctx, client)
}

const (
	githubAdminTeam           = "e6qu-org-admins"
	githubDeveloperTeam       = "e6qu-org-members"
	identityStateCookiePrefix = "_bleephub_identity_state_"
	backChannelLogoutEvent    = "http://schemas.openid.net/event/backchannel-logout"
	sessionCookieName         = "_gh_sess"
	secureSessionCookieName   = "__Host-_gh_sess"
)

// isSecureRequest reports whether the request arrived over HTTPS, directly or
// behind a TLS-terminating proxy that sets X-Forwarded-Proto.
func isSecureRequest(r *http.Request) bool {
	return r != nil && (r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https")
}

// secureCookies reports whether cookies must carry the Secure attribute:
// required in production and conditional only for the explicitly supported
// local HTTP development mode.
func (s *Server) secureCookies(r *http.Request) bool {
	return strings.HasPrefix(s.externalURL, "https://") || isSecureRequest(r)
}

// sessionCookieNameFor returns the __Host- prefixed name when the cookie can
// satisfy the prefix rules (Secure, Path=/, no Domain); plain HTTP local
// development keeps the unprefixed name because browsers reject insecure
// __Host- cookies.
func sessionCookieNameFor(secure bool) string {
	if secure {
		return secureSessionCookieName
	}
	return sessionCookieName
}

func (s *Server) sessionCookieFromRequest(r *http.Request) *http.Cookie {
	if cookie, err := r.Cookie(secureSessionCookieName); err == nil {
		return cookie
	}
	// Over HTTPS a real session is carried by the __Host- cookie; an unprefixed
	// _gh_sess present here is a shadow a related-domain or network attacker
	// planted (the unprefixed name has no Secure/Host guarantees), so it is not
	// honored. Plain-HTTP local development keeps the unprefixed cookie.
	if s.secureCookies(r) {
		return nil
	}
	cookie, _ := r.Cookie(sessionCookieName)
	return cookie
}

// clearSessionCookies revokes the session behind every cookie the browser might
// hold under EITHER name and unsets both. Clearing only the name just written
// would leave a shadow _gh_sess (planted before login, or before this logout)
// live, and the next request would silently adopt it.
func (s *Server) clearSessionCookies(w http.ResponseWriter, r *http.Request) error {
	secure := s.secureCookies(r)
	for _, name := range []string{secureSessionCookieName, sessionCookieName} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			if err := s.store.DeleteLoginSession(c.Value); err != nil {
				return err
			}
		}
		// The __Host- deletion must itself carry Secure so the browser accepts
		// the overwrite; the unprefixed one mirrors the deployment's policy.
		// #nosec G124 -- Secure is conditional only for the supported local-HTTP mode.
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
			Secure: secure || name == secureSessionCookieName, SameSite: http.SameSiteLaxMode,
		})
	}
	return nil
}

type oidcProviderMetadata struct {
	EndSessionEndpoint string `json:"end_session_endpoint"`
}

type oidcLogoutClaims struct {
	Subject string                     `json:"sub"`
	SID     string                     `json:"sid"`
	Nonce   json.RawMessage            `json:"nonce"`
	JTI     string                     `json:"jti"`
	Issued  int64                      `json:"iat"`
	Expires int64                      `json:"exp"`
	Events  map[string]json.RawMessage `json:"events"`
}

type identityConfig struct {
	githubClientID      string
	githubClientSecret  string
	shauthIssuer        string
	shauthClientID      string
	shauthClientSecret  string
	shauthPostLogoutURL string
	allowInsecureOIDC   bool
	allowedLogins       map[string]struct{}
}

type identityState struct {
	Provider  string    `json:"provider"`
	State     string    `json:"state"`
	ReturnTo  string    `json:"return_to"`
	Nonce     string    `json:"nonce,omitempty"`
	PKCE      string    `json:"pkce,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

func identityConfigFromEnv() identityConfig {
	return identityConfig{
		githubClientID:      strings.TrimSpace(os.Getenv("BLEEPHUB_GITHUB_OAUTH_CLIENT_ID")),
		githubClientSecret:  strings.TrimSpace(os.Getenv("BLEEPHUB_GITHUB_OAUTH_CLIENT_SECRET")),
		shauthIssuer:        strings.TrimSpace(os.Getenv("BLEEPHUB_SHAUTH_ISSUER")),
		shauthClientID:      strings.TrimSpace(os.Getenv("BLEEPHUB_SHAUTH_CLIENT_ID")),
		shauthClientSecret:  strings.TrimSpace(os.Getenv("BLEEPHUB_SHAUTH_CLIENT_SECRET")),
		shauthPostLogoutURL: strings.TrimSpace(os.Getenv("BLEEPHUB_SHAUTH_POST_LOGOUT_URL")),
		allowInsecureOIDC:   strings.EqualFold(strings.TrimSpace(os.Getenv("BLEEPHUB_ALLOW_INSECURE_OIDC")), "true"),
		allowedLogins:       parseAllowedLogins(os.Getenv("BLEEPHUB_ALLOWED_LOGINS")),
	}
}

// normalizeLogin canonicalizes a username before it is stored or looked up:
// NFKC compatibility normalization collapses lookalike compatibility forms
// (fullwidth, ligatures) and case folding makes "Alice" and "alice" the same
// account. A name that mixes Latin and Cyrillic letters is rejected outright
// as a basic confusable/homoglyph guard, so Cyrillic "аlice" cannot shadow a
// Latin login. The empty string normalizes to itself and carries no error.
func normalizeLogin(login string) (string, error) {
	if login == "" {
		return "", nil
	}
	if mixesLatinAndCyrillic(login) {
		return "", errMixedScriptLogin
	}
	return strings.ToLower(norm.NFKC.String(login)), nil
}

var errMixedScriptLogin = errors.New("login mixes Latin and Cyrillic characters")

// mixesLatinAndCyrillic reports whether the string contains at least one Basic
// Latin letter and at least one Cyrillic letter — the simplest shape of a
// homoglyph spoof. Ranges are checked on the raw runes because they are stable
// under NFKC for these scripts.
func mixesLatinAndCyrillic(login string) bool {
	var hasLatin, hasCyrillic bool
	for _, r := range login {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
			hasLatin = true
		case r >= '\u0400' && r <= '\u04FF':
			hasCyrillic = true
		}
	}
	return hasLatin && hasCyrillic
}

// parseAllowedLogins splits the comma-separated BLEEPHUB_ALLOWED_LOGINS value
// into a normalized lookup set. Entries are canonicalized with normalizeLogin
// so the operator may write them in any case; a blank or mixed-script entry is
// dropped. A nil map means "no allowlist configured" (admit any normalized
// login); an empty non-nil map would admit nothing, so it collapses to nil.
func parseAllowedLogins(value string) map[string]struct{} {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, raw := range strings.Split(value, ",") {
		normalized, err := normalizeLogin(strings.TrimSpace(raw))
		if err != nil || normalized == "" {
			continue
		}
		out[normalized] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// loginAllowed reports whether a login may authenticate or be created. With
// no allowlist configured every login is permitted. The argument is normalized
// defensively so a caller that passes a non-canonical form still matches.
func (c identityConfig) loginAllowed(login string) bool {
	if c.allowedLogins == nil {
		return true
	}
	normalized, err := normalizeLogin(login)
	if err != nil {
		return false
	}
	_, ok := c.allowedLogins[normalized]
	return ok
}

func (s *Server) registerExternalIdentityRoutes() {
	s.route("GET /auth/providers", s.handleIdentityProviders)
	s.route("GET /auth/session", s.handleIdentitySession)
	s.route("GET /auth/github", s.handleGitHubLogin)
	s.route("GET /auth/github/callback", s.handleGitHubCallback)
	s.route("GET /auth/shauth", s.handleShauthLogin)
	s.route("GET /auth/shauth/callback", s.handleShauthCallback)
	s.route("GET /auth/validation", s.handleIdentityValidation)
	s.route("POST /auth/shauth/backchannel-logout", s.handleShauthBackChannelLogout)
	s.route("GET /auth/shauth/frontchannel-logout", s.handleShauthFrontChannelLogout)
	s.route("POST /auth/shauth/frontchannel-logout", s.handleShauthFrontChannelLogout)
	s.route("GET /auth/shauth/logout/complete", s.handleShauthLogoutComplete)
	s.route("GET /ui/signed-out", s.handleIdentitySignedOut)
	s.route("POST /ui/signed-out", s.handleIdentitySignedOut)
	s.route("POST /auth/local", s.rateLimitAuthFlow(s.handleLocalLogin))
	s.route("POST /auth/token", s.rateLimitAuthFlow(s.handleTokenLogin))
	s.route("POST /auth/logout", s.handleIdentityLogout)
	s.route("GET /control", s.handlePrivateControl)
}

func (s *Server) handleIdentityProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"github": s.identity.githubClientID != "" && s.identity.githubClientSecret != "",
		"shauth": s.identity.shauthConfigured(),
		"entra":  false,
		"local":  true,
	})
}

func (c identityConfig) shauthConfigured() bool {
	return c.shauthIssuer != "" && c.shauthClientID != "" && c.shauthClientSecret != "" && c.shauthPostLogoutURL != ""
}

func (c identityConfig) validate() error {
	shauthValues := []string{c.shauthIssuer, c.shauthClientID, c.shauthClientSecret, c.shauthPostLogoutURL}
	set := 0
	for _, value := range shauthValues {
		if value != "" {
			set++
		}
	}
	if set != 0 && set != len(shauthValues) {
		return fmt.Errorf("BLEEPHUB_SHAUTH_ISSUER, BLEEPHUB_SHAUTH_CLIENT_ID, BLEEPHUB_SHAUTH_CLIENT_SECRET, and BLEEPHUB_SHAUTH_POST_LOGOUT_URL must be configured together")
	}
	if c.shauthIssuer != "" {
		issuer, err := url.Parse(c.shauthIssuer)
		if err != nil || !validIdentityURL(issuer, c.allowInsecureOIDC) {
			return fmt.Errorf("BLEEPHUB_SHAUTH_ISSUER must be an HTTPS issuer URL")
		}
	}
	if c.shauthPostLogoutURL != "" {
		postLogoutURL, err := url.Parse(c.shauthPostLogoutURL)
		if err != nil || !validIdentityURL(postLogoutURL, c.allowInsecureOIDC) {
			return fmt.Errorf("BLEEPHUB_SHAUTH_POST_LOGOUT_URL must be an absolute HTTPS URL")
		}
	}
	return nil
}

func validIdentityURL(value *url.URL, allowInsecure bool) bool {
	return value != nil && value.Host != "" && value.User == nil &&
		(value.Scheme == "https" || (allowInsecure && value.Scheme == "http"))
}

func validateShauthExternalURL(config identityConfig, externalURL string) error {
	if !config.shauthConfigured() {
		return nil
	}
	parsed, err := url.Parse(externalURL)
	if err != nil || !validIdentityURL(parsed, config.allowInsecureOIDC) || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("BLEEPHUB_EXTERNAL_URL must be an absolute HTTPS origin when Shauth is configured")
	}
	postLogoutURL, err := url.Parse(config.shauthPostLogoutURL)
	if err != nil || !sameURLOrigin(parsed, postLogoutURL) || postLogoutURL.Path != "/auth/shauth/logout/complete" || postLogoutURL.RawQuery != "" || postLogoutURL.Fragment != "" {
		return fmt.Errorf("BLEEPHUB_SHAUTH_POST_LOGOUT_URL must be %s/auth/shauth/logout/complete", strings.TrimRight(externalURL, "/"))
	}
	return nil
}

func sameURLOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func (s *Server) handleShauthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.identity.shauthConfigured() {
		writeGHError(w, http.StatusServiceUnavailable, "Shauth sign-in is not configured")
		return
	}
	provider, err := oidc.NewProvider(s.oidcClientContext(r.Context()), s.identity.shauthIssuer)
	if err != nil {
		s.logger.Warn().Err(err).Msg("Shauth discovery failed")
		writeGHError(w, http.StatusBadGateway, "Shauth discovery failed")
		return
	}
	state, err := randomIdentityState()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not start Shauth sign-in")
		return
	}
	nonce, err := randomIdentityState()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not start Shauth sign-in")
		return
	}
	pkce, err := randomIdentityState()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not start Shauth sign-in")
		return
	}
	returnTo := safeIdentityReturnTo(r.URL.Query().Get("return_to"))
	if err := s.setIdentityState(w, r, identityState{Provider: "shauth", State: state, ReturnTo: returnTo, Nonce: nonce, PKCE: pkce, ExpiresAt: time.Now().Add(10 * time.Minute)}); err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not start Shauth sign-in")
		return
	}
	endpoint := provider.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	config := oauth2.Config{ClientID: s.identity.shauthClientID, ClientSecret: s.identity.shauthClientSecret, Endpoint: endpoint, RedirectURL: s.externalAuthCallback("shauth"), Scopes: []string{oidc.ScopeOpenID, "profile", "email", "offline_access"}}
	http.Redirect(w, r, config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkce)), http.StatusFound)
}

func (s *Server) handleShauthCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	pending, err := s.consumeIdentityState(w, r, "shauth", state)
	if err != nil || r.URL.Query().Get("code") == "" {
		writeGHError(w, http.StatusBadRequest, "invalid or expired Shauth sign-in state")
		return
	}
	provider, err := oidc.NewProvider(s.oidcClientContext(r.Context()), s.identity.shauthIssuer)
	if err != nil {
		writeGHError(w, http.StatusBadGateway, "Shauth discovery failed")
		return
	}
	endpoint := provider.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	config := oauth2.Config{ClientID: s.identity.shauthClientID, ClientSecret: s.identity.shauthClientSecret, Endpoint: endpoint, RedirectURL: s.externalAuthCallback("shauth")}
	tokens, err := config.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(pending.PKCE))
	if err != nil {
		writeGHError(w, http.StatusUnauthorized, "Shauth authorization-code exchange failed")
		return
	}
	rawIDToken, ok := tokens.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		writeGHError(w, http.StatusUnauthorized, "Shauth did not return an ID token")
		return
	}
	idToken, err := provider.Verifier(&oidc.Config{
		ClientID: s.identity.shauthClientID,
		Now:      s.currentTime,
	}).Verify(s.oidcClientContext(r.Context()), rawIDToken)
	if err != nil {
		writeGHError(w, http.StatusUnauthorized, "Shauth ID token verification failed")
		return
	}
	var claims struct {
		Nonce             string   `json:"nonce"`
		SID               string   `json:"sid"`
		Email             string   `json:"email"`
		PreferredUsername string   `json:"preferred_username"`
		Name              string   `json:"name"`
		Picture           string   `json:"picture"`
		Role              string   `json:"role"`
		Audience          audience `json:"aud"`
		AuthorizedParty   string   `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil {
		writeGHError(w, http.StatusUnauthorized, "Shauth ID token claims were invalid")
		return
	}
	// go-oidc's Verify only checks that aud CONTAINS the client id. OIDC Core
	// §3.1.3.7 additionally requires rejecting a multi-audience token that omits
	// azp, and requiring azp == client id when present, so another registered
	// client cannot replay its token here.
	if len(claims.Audience) > 1 && claims.AuthorizedParty == "" {
		writeGHError(w, http.StatusUnauthorized, "Shauth ID token has multiple audiences without azp")
		return
	}
	if claims.AuthorizedParty != "" && claims.AuthorizedParty != s.identity.shauthClientID {
		writeGHError(w, http.StatusUnauthorized, "Shauth ID token azp is not this client")
		return
	}
	if claims.Nonce != pending.Nonce || claims.SID == "" || (claims.Role != "admin" && claims.Role != "developer") {
		writeGHError(w, http.StatusUnauthorized, "Shauth ID token claims were invalid")
		return
	}
	login := strings.TrimSpace(claims.PreferredUsername)
	if login == "" {
		writeGHError(w, http.StatusUnauthorized, "Shauth ID token did not carry a preferred_username")
		return
	}
	user, err := s.upsertExternalUser(s.identity.shauthIssuer, idToken.Subject, login, strings.TrimSpace(claims.Name), strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Picture), claims.Role == "admin", true)
	if err != nil {
		writeGHError(w, http.StatusForbidden, "Shauth account cannot be provisioned on this instance")
		return
	}
	expiresAt := idToken.Expiry
	if maximum := time.Now().Add(12 * time.Hour); expiresAt.After(maximum) {
		expiresAt = maximum
	}
	if err := s.createOIDCBrowserSession(w, r, user, LoginSession{
		OIDCProvider: "shauth",
		OIDCIssuer:   s.identity.shauthIssuer,
		OIDCSubject:  idToken.Subject,
		OIDCSID:      claims.SID,
		OIDCIDToken:  rawIDToken,
		ExpiresAt:    expiresAt,
	}); err != nil {
		s.logger.Error().Err(err).Msg("create browser session")
		writeGHError(w, http.StatusServiceUnavailable, "browser session is unavailable")
		return
	}
	// Only redirect to a same-origin relative path. Anything else — an absolute
	// URL, a protocol-relative //host, or a backslash trick — collapses to the
	// app root, so a tampered stored ReturnTo can never become an open redirect
	// to an attacker-controlled origin.
	redirectTarget := "/ui/"
	if rt := pending.ReturnTo; strings.HasPrefix(rt, "/") && !strings.HasPrefix(rt, "//") && !strings.Contains(rt, "\\") {
		redirectTarget = rt
	}
	http.Redirect(w, r, redirectTarget, http.StatusFound)
}

func safeIdentityReturnTo(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, `\`) || parsed.IsAbs() || parsed.Host != "" {
		return "/ui/"
	}
	return parsed.RequestURI()
}

func (s *Server) handleIdentitySession(w http.ResponseWriter, r *http.Request) {
	// The response reflects the caller's own identity, so it must never be
	// served from a shared cache to another user.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Cookie")
	session := s.sessionFromRequest(r)
	if session == nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	user := s.store.GetUserByID(session.UserID)
	if user == nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          s.fullUserJSON(user),
	})
}

func (s *Server) handleIdentityValidation(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromRequest(r)
	var user *User
	if session != nil {
		user = s.store.GetUserByID(session.UserID)
	}
	if user == nil {
		// Fail closed to the signed-out page, not back into a fresh
		// authorization. The Shauth SSO validator reloads validation_url after a
		// global provider logout and requires the browser to come to rest exactly
		// at signed_out_url (validator/validate.mjs "verify Shauth provider logout
		// revoked"): redirecting to /auth/shauth would silently re-enter the
		// authorization flow, so the browser never reaches signed_out_url and the
		// app reads as "remained authenticated". /ui/signed-out is this app's
		// registered signed_out_url and also satisfies the anonymous fail-closed
		// check, so it is correct in both the logged-out and never-logged-in cases.
		http.Redirect(w, r, "/ui/signed-out", http.StatusFound)
		return
	}
	// Render the signed-in identity with the data-testid markers the Shauth SSO
	// validator asserts against (validator/validate.mjs assertValidationIdentity):
	// username (the OIDC preferred_username -> our login), email, role, and the
	// running release revision. Built with fmt on this small fragment only -- the
	// page's inline CSS contains % and is composed via a literal marker replace.
	role := "developer"
	if user.SiteAdmin {
		role = "admin"
	}
	identity := fmt.Sprintf(
		`<dl class="validation-identity">`+
			`<dt>Username</dt><dd data-testid="validation-username">%s</dd>`+
			`<dt>Email</dt><dd data-testid="validation-email">%s</dd>`+
			`<dt>Role</dt><dd data-testid="validation-role">%s</dd>`+
			`<dt>Release</dt><dd data-testid="validation-release">%s</dd>`+
			`</dl>`,
		html.EscapeString(user.Login),
		html.EscapeString(user.Email),
		html.EscapeString(role),
		html.EscapeString(s.build.Version),
	)
	page := strings.Replace(identityValidationPage, "<!--VALIDATION-IDENTITY-->", identity, 1)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}

const identityValidationPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Authenticated · Bleephub</title>
<style>
:root{color-scheme:light dark;font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;--canvas:#f6f8fa;--surface:#fff;--fg:#1f2328;--muted:#59636e;--border:#c6d0da;--blue:#006eff;--purple:#8250df;--pink:#d1248f;--focus:#0550ae;--button-fg:#fff;--shadow:rgba(9,105,218,.18)}
*{box-sizing:border-box}body{min-height:100vh;margin:0;display:grid;place-items:center;padding:1.5rem;background:radial-gradient(circle at 5% 5%,color-mix(in srgb,var(--blue) 22%,transparent),transparent 36%),radial-gradient(circle at 95% 0,color-mix(in srgb,var(--pink) 20%,transparent),transparent 34%),linear-gradient(145deg,var(--canvas),color-mix(in srgb,var(--purple) 10%,var(--canvas)));color:var(--fg)}main{width:min(31rem,100%);padding:2rem 1.35rem 1.5rem;border:1px solid color-mix(in srgb,var(--purple) 45%,var(--border));border-radius:1rem;background:var(--surface);box-shadow:0 1.5rem 4rem var(--shadow)}h1{margin:0;font-size:clamp(1.75rem,7vw,2.45rem);line-height:1.12;letter-spacing:-.035em}p{margin:.9rem 0 1.4rem;color:var(--muted)}button{display:inline-flex;min-height:2.75rem;align-items:center;justify-content:center;padding:.65rem 1rem;border:1px solid color-mix(in srgb,var(--purple) 42%,var(--blue));border-radius:.55rem;background:linear-gradient(110deg,var(--blue),var(--purple) 55%,var(--pink));box-shadow:0 .5rem 1.2rem color-mix(in srgb,var(--purple) 28%,transparent);color:var(--button-fg);font:inherit;font-weight:750;cursor:pointer}button:hover{filter:saturate(1.18) brightness(1.04)}button:focus-visible{outline:3px solid var(--focus);outline-offset:3px}@media(prefers-color-scheme:dark){:root{--canvas:#0d1117;--surface:#161b22;--fg:#f0f6fc;--muted:#a8b3c1;--border:#3d4754;--blue:#58a6ff;--purple:#bc8cff;--pink:#ff7bda;--focus:#79c0ff;--button-fg:#0d1117;--shadow:rgba(0,0,0,.55)}}
</style>
</head>
<body>
<main aria-labelledby="authenticated-title">
<h1 id="authenticated-title">Bleephub is authenticated</h1>
<p>Your Bleephub browser session is active.</p>
<!--VALIDATION-IDENTITY-->
<form method="post" action="/auth/logout"><button type="submit">Sign out</button></form>
</main>
</body>
</html>`

func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if s.identity.githubClientID == "" || s.identity.githubClientSecret == "" {
		writeGHError(w, http.StatusServiceUnavailable, "GitHub sign-in is not configured")
		return
	}
	state, err := randomIdentityState()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not start GitHub sign-in")
		return
	}
	returnTo := safeIdentityReturnTo(r.URL.Query().Get("return_to"))
	if err := s.setIdentityState(w, r, identityState{Provider: "github", State: state, ReturnTo: returnTo, ExpiresAt: time.Now().Add(10 * time.Minute)}); err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not start GitHub sign-in")
		return
	}

	redirect := url.URL{Scheme: "https", Host: "github.com", Path: "/login/oauth/authorize"}
	q := redirect.Query()
	q.Set("client_id", s.identity.githubClientID)
	q.Set("redirect_uri", s.externalAuthCallback("github"))
	q.Set("scope", "read:org user:email")
	q.Set("state", state)
	redirect.RawQuery = q.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	pending, err := s.consumeIdentityState(w, r, "github", state)
	if err != nil || r.URL.Query().Get("code") == "" {
		writeGHError(w, http.StatusBadRequest, "invalid or expired GitHub sign-in state")
		return
	}
	accessToken, err := s.exchangeGitHubCode(r.URL.Query().Get("code"))
	if err != nil {
		s.logger.Warn().Err(err).Msg("GitHub OAuth callback failed")
		writeGHError(w, http.StatusUnauthorized, "GitHub sign-in failed")
		return
	}
	profile, err := githubProfile(accessToken)
	if err != nil {
		writeGHError(w, http.StatusUnauthorized, "GitHub account lookup failed")
		return
	}
	admin, developer, err := githubTeamRoles(accessToken, profile.Login)
	if err != nil {
		writeGHError(w, http.StatusForbidden, "GitHub team membership could not be verified")
		return
	}
	if !admin && !developer {
		writeGHError(w, http.StatusForbidden, "GitHub account is not an e6qu-org Bleephub member")
		return
	}
	if profile.Email == "" {
		profile.Email, err = githubPrimaryEmail(accessToken)
		if err != nil {
			writeGHError(w, http.StatusUnauthorized, "GitHub email lookup failed")
			return
		}
	}
	user, err := s.upsertExternalUser(githubExternalIssuer, profile.ExternalSubject(), profile.Login, profile.Name, profile.Email, profile.AvatarURL, admin, false)
	if err != nil {
		writeGHError(w, http.StatusForbidden, "GitHub account cannot be provisioned on this instance")
		return
	}
	if err := s.createBrowserSession(w, r, user); err != nil {
		s.logger.Error().Err(err).Msg("create browser session")
		writeGHError(w, http.StatusServiceUnavailable, "browser session is unavailable")
		return
	}
	// Only redirect to a same-origin relative path. Anything else — an absolute
	// URL, a protocol-relative //host, or a backslash trick — collapses to the
	// app root, so a tampered stored ReturnTo can never become an open redirect
	// to an attacker-controlled origin.
	redirectTarget := "/ui/"
	if rt := pending.ReturnTo; strings.HasPrefix(rt, "/") && !strings.HasPrefix(rt, "//") && !strings.Contains(rt, "\\") {
		redirectTarget = rt
	}
	http.Redirect(w, r, redirectTarget, http.StatusFound)
}

// identityStateCookie names, paths, and secures the per-flow OAuth state cookie.
// When the deployment is HTTPS it carries the __Host- prefix (Secure, Path=/,
// no Domain), which prevents a sibling-subdomain or network attacker from
// transplanting a server-signed state cookie into the victim's jar and logging
// the victim in as the attacker. Plain-HTTP local development keeps the
// unprefixed name because browsers reject an insecure __Host- cookie.
func identityStateCookie(secure bool, state, value string, maxAge int, expires time.Time) *http.Cookie {
	name := identityStateCookiePrefix + state
	path := "/auth/"
	if secure {
		name = "__Host-" + name
		path = "/" // __Host- requires Path=/
	}
	// #nosec G124 -- Secure is conditional only for the supported local-HTTP mode.
	return &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge, Expires: expires, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode}
}

func (s *Server) setIdentityState(w http.ResponseWriter, r *http.Request, pending identityState) error {
	payload, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, s.identityStateKey)
	_, _ = mac.Write(payload)
	value := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// #nosec G124 -- Secure is conditional only because explicitly enabled local
	// HTTP development is supported; production external URLs are HTTPS.
	http.SetCookie(w, identityStateCookie(s.secureCookies(r), pending.State, value, 600, pending.ExpiresAt))
	return nil
}

func (s *Server) consumeIdentityState(w http.ResponseWriter, r *http.Request, provider, state string) (identityState, error) {
	if len(state) != 64 {
		return identityState{}, errors.New("invalid identity state")
	}
	for _, char := range state {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return identityState{}, errors.New("invalid identity state")
		}
	}
	secure := s.secureCookies(r)
	// #nosec G124 -- see setIdentityState; deletion must use the same Secure policy.
	http.SetCookie(w, identityStateCookie(secure, state, "", -1, time.Time{}))
	cookieName := identityStateCookiePrefix + state
	if secure {
		cookieName = "__Host-" + cookieName
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return identityState{}, err
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return identityState{}, errors.New("invalid identity state encoding")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return identityState{}, err
	}
	if base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		return identityState{}, errors.New("invalid identity state encoding")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identityState{}, err
	}
	if base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return identityState{}, errors.New("invalid identity state encoding")
	}
	mac := hmac.New(sha256.New, s.identityStateKey)
	_, _ = mac.Write(payload)
	if subtle.ConstantTimeCompare(signature, mac.Sum(nil)) != 1 {
		return identityState{}, errors.New("invalid identity state signature")
	}
	var pending identityState
	if err := json.Unmarshal(payload, &pending); err != nil {
		return identityState{}, err
	}
	if pending.Provider != provider || pending.State == "" || pending.State != state || time.Now().After(pending.ExpiresAt) {
		return identityState{}, errors.New("invalid or expired identity state")
	}
	return pending, nil
}

// githubExternalIssuer is the stable issuer for accounts brought in by the
// GitHub OAuth flow. The subject is the GitHub user's numeric id, which is
// immutable; the login is not.
const githubExternalIssuer = "https://github.com"

type githubIdentity struct {
	ID                            int64
	Login, Name, Email, AvatarURL string
}

// ExternalSubject returns the stable subject for this GitHub identity — the
// numeric account id — or "" when the profile did not carry one, in which case
// upsertExternalUser falls back to the username index.
func (g githubIdentity) ExternalSubject() string {
	if g.ID == 0 {
		return ""
	}
	return strconv.FormatInt(g.ID, 10)
}

func (s *Server) exchangeGitHubCode(code string) (string, error) {
	form := url.Values{"client_id": {s.identity.githubClientID}, "client_secret": {s.identity.githubClientSecret}, "code": {code}, "redirect_uri": {s.externalAuthCallback("github")}}
	req, err := http.NewRequest(http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", fmt.Errorf("GitHub token exchange: %s", body.Error)
	}
	return body.AccessToken, nil
}

func githubProfile(token string) (githubIdentity, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return githubIdentity{}, err
	}
	defer response.Body.Close()
	var profile struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.NewDecoder(response.Body).Decode(&profile); err != nil {
		return githubIdentity{}, err
	}
	if response.StatusCode != http.StatusOK || profile.Login == "" {
		return githubIdentity{}, fmt.Errorf("GitHub user lookup status %d", response.StatusCode)
	}
	return githubIdentity{ID: profile.ID, Login: profile.Login, Name: profile.Name, Email: profile.Email, AvatarURL: profile.AvatarURL}, nil
}

func githubPrimaryEmail(token string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/user/emails", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(response.Body).Decode(&emails); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub email lookup status %d", response.StatusCode)
	}
	for _, email := range emails {
		if email.Primary && email.Verified && email.Email != "" {
			return email.Email, nil
		}
	}
	return "", fmt.Errorf("GitHub account has no verified primary email")
}

func githubTeamRoles(token, login string) (admin, developer bool, err error) {
	for _, team := range []struct {
		slug  string
		admin bool
	}{{githubAdminTeam, true}, {githubDeveloperTeam, false}} {
		req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/orgs/e6qu-org/teams/"+team.slug+"/memberships/"+url.PathEscape(login), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		response, requestErr := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if requestErr != nil {
			return false, false, requestErr
		}
		if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusOK {
			if team.admin {
				admin = true
			} else {
				developer = true
			}
		} else if response.StatusCode != http.StatusNotFound {
			_ = response.Body.Close()
			return false, false, fmt.Errorf("GitHub team membership lookup for %s status %d", team.slug, response.StatusCode)
		}
		_ = response.Body.Close()
	}
	return admin, developer, nil
}

func (s *Server) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	login, err := normalizeLogin(strings.TrimSpace(request.Login))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "login mixes confusable scripts")
		return
	}
	// A disallowed login and a wrong credential return the identical 401 so an
	// anonymous caller cannot enumerate BLEEPHUB_ALLOWED_LOGINS membership from
	// the status code.
	var user *User
	if s.identity.loginAllowed(login) {
		user = s.browserLoginUser(login, request.Password)
	}
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "invalid local credentials")
		return
	}
	if err := s.createBrowserSession(w, r, user); err != nil {
		s.logger.Error().Err(err).Msg("create browser session")
		writeGHError(w, http.StatusServiceUnavailable, "browser session is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.fullUserJSON(user))
}

// handleTokenLogin exchanges a user access token for an HttpOnly browser
// session. The SPA must not retain a bearer credential in web storage: any
// script executing in the origin could read it and use it outside the browser.
func (s *Server) handleTokenLogin(w http.ResponseWriter, r *http.Request) {
	ctx := s.authenticateRequest(r)
	if invalid, _ := ctx.Value(ctxInvalidCredential).(bool); invalid {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if suspended, _ := ctx.Value(ctxSuspendedUser).(bool); suspended {
		writeGHError(w, http.StatusForbidden, "This account has been suspended")
		return
	}
	user := ghUserFromContext(ctx)
	pat := ghPersonalAccessTokenFromContext(ctx)
	isUserToken := ghUserToServerTokenFromContext(ctx) != nil
	if user == nil || (pat == nil && !isUserToken) {
		writeGHError(w, http.StatusUnauthorized, "A user access token is required")
		return
	}
	// A fine-grained PAT is deliberately narrow (specific repositories and
	// permissions); a browser session carries the account's full authority.
	// Exchanging one for the other would silently widen a scoped credential, so
	// refuse it — the classic-PAT SPA flow is unaffected.
	if pat != nil && pat.FineGrained {
		writeGHError(w, http.StatusForbidden, "A fine-grained token cannot be exchanged for a browser session")
		return
	}
	if err := s.createBrowserSession(w, r, user); err != nil {
		s.logger.Error().Err(err).Msg("create token browser session")
		writeGHError(w, http.StatusServiceUnavailable, "browser session is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.fullUserJSON(user))
}

// externalIdentityKey is the durable key for a federated identity: the
// (issuer, subject) pair the provider guarantees stable, never the mutable
// username. An empty issuer or subject collapses to "" so the caller falls
// back to the username index rather than silently keying on half a pair.
func externalIdentityKey(issuer, subject string) string {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return ""
	}
	return issuer + "\x00" + subject
}

// errFederatedLogin marks a federated sign-in that must not provision or resolve
// a local account. Callers surface it as a 403 without disclosing which of the
// closed conditions applied, so it is not a login-enumeration oracle.
var errFederatedLogin = errors.New("federated login is not permitted on this instance")

// upsertExternalUser resolves (or creates) the local account for a verified
// federated identity. It resolves on the stable (issuer, subject) key first,
// never the mutable provider username. Only the primary IdP (roleAuthoritative)
// may then adopt a same-named LOCAL account — SSO taking ownership of the seeded
// bootstrap account — and privileges always come from the role claim, so a
// principal cannot escalate by claiming "admin": a developer-role login lands on
// a non-SiteAdmin account regardless of what it adopted. A secondary provider,
// and any account already bound to a different federated identity, are refused,
// so no one can seize another's account (preserves AUTH-021).
func (s *Server) upsertExternalUser(issuer, subject, login, name, email, avatarURL string, siteAdmin, roleAuthoritative bool) (*User, error) {
	login, err := normalizeLogin(login)
	if err != nil || login == "" {
		return nil, errFederatedLogin
	}
	if !s.identity.loginAllowed(login) {
		return nil, errFederatedLogin
	}
	externalKey := externalIdentityKey(issuer, subject)
	if externalKey == "" {
		// Without a stable subject there is no safe federated key; adopting by
		// username is exactly what must not happen, so fail closed.
		return nil, errFederatedLogin
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if user := s.store.UsersByExternalID[externalKey]; user != nil {
		user.Name, user.Email, user.AvatarURL, user.UpdatedAt = name, email, avatarURL, time.Now().UTC()
		if roleAuthoritative {
			user.SiteAdmin = siteAdmin
		}
		s.bindExternalIdentityLocked(user, externalKey)
		if s.store.persist != nil {
			s.store.persist.MustPut("users", fmt.Sprint(user.ID), user)
		}
		return user, nil
	}
	// No federated binding yet. Resolve a same-username account carefully. The
	// primary IdP may adopt a purely LOCAL account (e.g. the seeded bootstrap
	// admin) — that is SSO legitimately taking ownership of it — but privileges
	// then come from the role claim below, not from the account being adopted,
	// so a principal cannot escalate by claiming a privileged username. A
	// non-primary provider, or an account that already belongs to a DIFFERENT
	// federated identity, is refused: neither may seize an existing account.
	if existing := s.store.UsersByLogin[login]; existing != nil {
		if !roleAuthoritative || len(existing.ExternalIdentities) > 0 {
			return nil, errFederatedLogin
		}
		existing.Name, existing.Email, existing.AvatarURL, existing.UpdatedAt = name, email, avatarURL, time.Now().UTC()
		existing.SiteAdmin = siteAdmin
		s.bindExternalIdentityLocked(existing, externalKey)
		if s.store.persist != nil {
			s.store.persist.MustPut("users", fmt.Sprint(existing.ID), existing)
		}
		return existing, nil
	}
	now := time.Now().UTC()
	user := &User{ID: s.store.reserveGlobalID("next_user", &s.store.NextUser), NodeID: "U_bleephub_" + login, Login: login, Name: name, Email: email, AvatarURL: avatarURL, Type: "User", SiteAdmin: siteAdmin, StarredRepos: map[string]bool{}, CreatedAt: now, UpdatedAt: now}
	s.store.Users[user.ID], s.store.UsersByLogin[user.Login] = user, user
	s.bindExternalIdentityLocked(user, externalKey)
	if s.store.persist != nil {
		s.store.persist.MustPut("users", fmt.Sprint(user.ID), user)
	}
	return user, nil
}

// bindExternalIdentityLocked records the (issuer, subject) binding on user and
// indexes it. Callers hold st.mu. A binding already present is a no-op, so the
// same provider re-authenticating does not accumulate duplicate entries.
func (s *Server) bindExternalIdentityLocked(user *User, externalKey string) {
	if externalKey == "" || user == nil {
		return
	}
	for _, identity := range user.ExternalIdentities {
		if externalIdentityKey(identity.Issuer, identity.Subject) == externalKey {
			s.store.UsersByExternalID[externalKey] = user
			return
		}
	}
	issuer, subject, _ := strings.Cut(externalKey, "\x00")
	user.ExternalIdentities = append(user.ExternalIdentities, ExternalIdentity{Issuer: issuer, Subject: subject})
	s.store.UsersByExternalID[externalKey] = user
}

func (s *Server) createBrowserSession(w http.ResponseWriter, r *http.Request, user *User) error {
	return s.createOIDCBrowserSession(w, r, user, LoginSession{ExpiresAt: time.Now().Add(12 * time.Hour)})
}

// createOIDCBrowserSession issues the browser session cookie.
//
// The CSRF token is drawn separately from the cookie value. Reusing the cookie
// value made every page that prints an authenticity_token a disclosure of the
// session identifier, which is the one thing HttpOnly exists to withhold.
func (s *Server) createOIDCBrowserSession(w http.ResponseWriter, r *http.Request, user *User, session LoginSession) error {
	if user == nil {
		return errors.New("cannot issue a browser session for a nil user")
	}
	// Rotate the session on every successful authentication. A cookie value an
	// attacker planted before credentials were verified (session fixation) is
	// revoked the moment a real identity is established, and a privilege change
	// is reflected immediately instead of lingering for the old session's TTL.
	if cookie := s.sessionCookieFromRequest(r); cookie != nil {
		if err := s.store.DeleteLoginSession(cookie.Value); err != nil {
			return err
		}
	}
	id, err := randomIdentityState()
	if err != nil {
		return err
	}
	csrf, err := randomIdentityState()
	if err != nil {
		return err
	}
	session.UserID = user.ID
	session.CSRFToken = csrf
	if session.ExpiresAt.IsZero() {
		session.ExpiresAt = time.Now().Add(12 * time.Hour)
	}
	if err := s.store.PutLoginSession(id, &session); err != nil {
		return err
	}
	secure := s.secureCookies(r)
	// #nosec G124 -- Secure is conditional only for explicitly enabled local HTTP.
	http.SetCookie(w, &http.Cookie{Name: sessionCookieNameFor(secure), Value: id, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt})
	return nil
}

func (s *Server) externalAuthCallback(provider string) string {
	return strings.TrimRight(s.externalURL, "/") + "/auth/" + provider + "/callback"
}

// requestOrigin reconstructs the origin a browser computes for this request,
// so same-origin POSTs pass the logout Origin check even when
// BLEEPHUB_EXTERNAL_URL is unset and URLs derive from the request Host.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if isSecureRequest(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) handleIdentityLogout(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.externalURL && origin != requestOrigin(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin logout denied")
		return
	}
	session := s.sessionFromRequest(r)
	// #nosec G124 -- deletion mirrors the session's conditional local-HTTP policy.
	if err := s.clearSessionCookies(w, r); err != nil {
		s.logger.Error().Err(err).Msg("delete browser session")
		writeGHError(w, http.StatusServiceUnavailable, "browser session could not be revoked")
		return
	}
	logoutTarget := ""
	// Only start a global RP-initiated logout when THIS session was itself
	// established through Shauth. A user who signed in locally (or via a token)
	// must not have their shared Shauth SSO session — used by every other
	// relying party — torn down by signing out of this app.
	if s.identity.shauthConfigured() && session != nil && session.OIDCProvider == "shauth" && session.OIDCIDToken != "" {
		provider, err := oidc.NewProvider(s.oidcClientContext(r.Context()), s.identity.shauthIssuer)
		if err != nil {
			writeGHError(w, http.StatusBadGateway, "Shauth discovery failed")
			return
		}
		var metadata oidcProviderMetadata
		if err := provider.Claims(&metadata); err != nil || metadata.EndSessionEndpoint == "" {
			writeGHError(w, http.StatusBadGateway, "Shauth does not advertise RP-Initiated Logout")
			return
		}
		endpoint, err := url.Parse(metadata.EndSessionEndpoint)
		if err != nil || !validIdentityURL(endpoint, s.identity.allowInsecureOIDC) {
			writeGHError(w, http.StatusBadGateway, "Shauth advertised an invalid logout endpoint")
			return
		}
		query := endpoint.Query()
		query.Set("id_token_hint", session.OIDCIDToken)
		query.Set("post_logout_redirect_uri", s.identity.shauthPostLogoutURL)
		endpoint.RawQuery = query.Encode()
		logoutTarget = endpoint.String()
	}
	if logoutTarget != "" {
		http.Redirect(w, r, logoutTarget, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func (s *Server) handleIdentitySignedOut(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// #nosec G124 -- deletion mirrors the session's conditional local-HTTP policy.
		if err := s.clearSessionCookies(w, r); err != nil {
			s.logger.Error().Err(err).Msg("delete browser session on signed-out landing")
			writeGHError(w, http.StatusServiceUnavailable, "browser session could not be revoked")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(identitySignedOutPage))
}

func (s *Server) handleShauthLogoutComplete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.identity.shauthConfigured() {
		http.NotFound(w, r)
		return
	}
	issuer, err := url.Parse(s.identity.shauthIssuer)
	if err != nil || !validIdentityURL(issuer, s.identity.allowInsecureOIDC) {
		writeGHError(w, http.StatusServiceUnavailable, "Shauth logout completion is unavailable")
		return
	}
	target := issuer.ResolveReference(&url.URL{Path: "/oauth/logout/complete"})
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

const identitySignedOutPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Signed out · Bleephub</title>
<style>
:root{color-scheme:light dark;font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;--canvas:#f6f8fa;--surface:#fff;--fg:#1f2328;--muted:#59636e;--border:#c6d0da;--blue:#006eff;--purple:#8250df;--pink:#d1248f;--cyan:#006b7a;--focus:#0550ae;--button-fg:#fff;--shadow:rgba(9,105,218,.18)}
*{box-sizing:border-box}
body{min-height:100vh;margin:0;display:grid;place-items:center;padding:1.5rem;background:radial-gradient(circle at 5% 5%,color-mix(in srgb,var(--blue) 22%,transparent),transparent 36%),radial-gradient(circle at 95% 0,color-mix(in srgb,var(--pink) 20%,transparent),transparent 34%),linear-gradient(145deg,var(--canvas),color-mix(in srgb,var(--purple) 10%,var(--canvas)));color:var(--fg)}
main{width:min(31rem,100%);overflow:hidden;border:1px solid color-mix(in srgb,var(--purple) 45%,var(--border));border-radius:1rem;background:var(--surface);box-shadow:0 1.5rem 4rem var(--shadow)}
.brand{display:flex;align-items:center;gap:.75rem;padding:1.1rem 1.35rem;border-bottom:1px solid var(--border);font-size:1.05rem;font-weight:750}.mark{display:grid;width:2.25rem;height:2.25rem;place-items:center;border-radius:.7rem;background:linear-gradient(145deg,var(--blue),var(--purple) 58%,var(--pink));box-shadow:0 .45rem 1.2rem color-mix(in srgb,var(--purple) 36%,transparent);color:var(--button-fg);font-weight:850}.content{padding:2rem 1.35rem 1.5rem}.eyebrow{margin:0 0 .4rem;color:var(--cyan);font-size:.75rem;font-weight:800;letter-spacing:.12em;text-transform:uppercase}h1{margin:0;font-size:clamp(1.75rem,7vw,2.45rem);line-height:1.12;letter-spacing:-.035em}p{margin:.9rem 0 1.4rem;color:var(--muted)}.actions{display:flex;flex-wrap:wrap;gap:.75rem}.actions form{margin:0}.actions a,.actions button{display:inline-flex;min-height:2.75rem;align-items:center;justify-content:center;padding:.65rem 1rem;border:1px solid color-mix(in srgb,var(--purple) 42%,var(--blue));border-radius:.55rem;font:inherit;font-weight:750;cursor:pointer}.actions a{background:linear-gradient(110deg,var(--blue),var(--purple) 55%,var(--pink));box-shadow:0 .5rem 1.2rem color-mix(in srgb,var(--purple) 28%,transparent);color:var(--button-fg);text-decoration:none}.actions button{background:var(--surface);color:var(--fg)}.actions a:hover,.actions button:hover{filter:saturate(1.18) brightness(1.04)}.actions a:focus-visible,.actions button:focus-visible{outline:3px solid var(--focus);outline-offset:3px}.privacy{margin:1.25rem 0 0;font-size:.78rem}
@media(prefers-color-scheme:dark){:root{--canvas:#0d1117;--surface:#161b22;--fg:#f0f6fc;--muted:#a8b3c1;--border:#3d4754;--blue:#58a6ff;--purple:#bc8cff;--pink:#ff7bda;--cyan:#39d0e8;--focus:#79c0ff;--button-fg:#0d1117;--shadow:rgba(0,0,0,.55)}body{background:radial-gradient(circle at 5% 5%,color-mix(in srgb,var(--blue) 18%,transparent),transparent 36%),radial-gradient(circle at 95% 0,color-mix(in srgb,var(--pink) 16%,transparent),transparent 34%),linear-gradient(145deg,var(--canvas),color-mix(in srgb,var(--purple) 10%,var(--canvas)))}}
@media(prefers-reduced-motion:reduce){*{scroll-behavior:auto!important}}
</style>
</head>
<body>
<main id="main-content" aria-labelledby="signed-out-title">
<header class="brand"><span class="mark" aria-hidden="true">B</span><span>Bleephub</span></header>
<section class="content">
<p class="eyebrow">Session ended</p>
<h1 id="signed-out-title">You are signed out</h1>
<p>Your Bleephub browser session has ended. Continue when you are ready to start a new Shauth sign-in.</p>
<div class="actions">
<a href="/auth/shauth?return_to=%2Fui%2F">Sign in with Shauth</a>
<form method="post" action="/auth/logout"><button type="submit">Sign out</button></form>
</div>
<p class="privacy">Reloading this page will not start a new sign-in session.</p>
</section>
</main>
</body>
</html>`

func (s *Server) handleShauthFrontChannelLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// The OP renders this endpoint in an iframe during front-channel logout,
	// so the issuer origin must be allowed to frame it alongside 'self'.
	frameAncestors := "'self'"
	if s.identity.shauthConfigured() {
		if issuer, err := url.Parse(s.identity.shauthIssuer); err == nil && issuer.Scheme != "" && issuer.Host != "" {
			frameAncestors += " " + issuer.Scheme + "://" + issuer.Host
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors "+frameAncestors)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The OIDC front-channel logout spec delivers this as a GET in an
	// OP-rendered iframe, so revocation cannot be gated on POST. The iss must
	// match the configured issuer exactly and the sid names the OP session —
	// unguessable to a cross-site attacker — so a forged request cannot revoke
	// anything (issue #112).
	if s.identity.shauthConfigured() && r.URL.Query().Get("iss") == s.identity.shauthIssuer && r.URL.Query().Get("sid") != "" {
		if err := s.store.DeleteLoginSessionsForOIDC("shauth", s.identity.shauthIssuer, r.URL.Query().Get("sid"), ""); err != nil {
			s.logger.Error().Err(err).Msg("revoke browser sessions from Shauth front-channel logout")
			writeGHError(w, http.StatusServiceUnavailable, "browser sessions could not be revoked")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Signed out</title></head><body></body></html>`))
}

func (s *Server) handleShauthBackChannelLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid logout request")
		return
	}
	rawLogoutToken := r.PostForm.Get("logout_token")
	if rawLogoutToken == "" {
		writeGHError(w, http.StatusBadRequest, "logout_token is required")
		return
	}
	provider, err := oidc.NewProvider(s.oidcClientContext(r.Context()), s.identity.shauthIssuer)
	if err != nil {
		writeGHError(w, http.StatusBadGateway, "Shauth discovery failed")
		return
	}
	logoutToken, err := provider.Verifier(&oidc.Config{
		ClientID: s.identity.shauthClientID,
		Now:      s.currentTime,
	}).Verify(s.oidcClientContext(r.Context()), rawLogoutToken)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "logout token verification failed")
		return
	}
	var claims oidcLogoutClaims
	if err := logoutToken.Claims(&claims); err != nil || claims.JTI == "" || claims.Issued == 0 || claims.Expires == 0 || len(claims.Nonce) != 0 || (claims.SID == "" && claims.Subject == "") {
		writeGHError(w, http.StatusBadRequest, "logout token claims are invalid")
		return
	}
	if _, ok := claims.Events[backChannelLogoutEvent]; !ok {
		writeGHError(w, http.StatusBadRequest, "logout token event is invalid")
		return
	}
	var eventClaims map[string]json.RawMessage
	if err := json.Unmarshal(claims.Events[backChannelLogoutEvent], &eventClaims); err != nil || eventClaims == nil || len(eventClaims) != 0 {
		writeGHError(w, http.StatusBadRequest, "logout token event is invalid")
		return
	}
	now := s.currentTime()
	issuedAt := time.Unix(claims.Issued, 0)
	if issuedAt.Before(now.Add(-5*time.Minute)) || issuedAt.After(now.Add(time.Minute)) {
		writeGHError(w, http.StatusBadRequest, "logout token is stale")
		return
	}
	claimed, err := s.store.ClaimOIDCLogoutAndDeleteSessions(
		"shauth", s.identity.shauthIssuer, s.identity.shauthClientID, claims.JTI,
		time.Unix(claims.Expires, 0), now, claims.SID, claims.Subject,
	)
	if err != nil {
		s.logger.Error().Err(err).Msg("claim Shauth logout token and revoke browser sessions")
		writeGHError(w, http.StatusServiceUnavailable, "browser sessions could not be revoked")
		return
	}
	if !claimed {
		writeGHError(w, http.StatusBadRequest, "logout token was already used")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handlePrivateControl(w http.ResponseWriter, r *http.Request) {
	if adminHost := strings.TrimSpace(os.Getenv("BLEEPHUB_ADMIN_HOST")); adminHost != "" && !strings.EqualFold(strings.Split(r.Host, ":")[0], adminHost) {
		http.NotFound(w, r)
		return
	}
	session := s.sessionFromRequest(r)
	var user *User
	if session != nil {
		user = s.store.GetUserByID(session.UserID)
	}
	if user == nil || !user.SiteAdmin {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>Bleephub control</title><style>body{margin:0;background:#111827;color:#f8fafc;font:16px system-ui}main{max-width:960px;margin:3rem auto;padding:2rem;background:#1e293b;border:1px solid #334155;border-radius:18px}h1{color:#67e8f9}table{width:100%;border-collapse:collapse}td,th{padding:.7rem;border-bottom:1px solid #334155;text-align:left}form{display:grid;grid-template-columns:1fr 1fr 1fr 1fr auto;gap:.6rem;margin:1.5rem 0}input,select,button{padding:.7rem;border-radius:8px;border:1px solid #475569}button{background:#a855f7;color:white;font-weight:700;cursor:pointer}.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:.7rem}.stat{padding:1rem;background:#0f172a;border:1px solid #334155;border-radius:10px}.stat b{display:block;color:#67e8f9;font-size:1.3rem}</style></head><body><main><h1>Bleephub private control</h1><p>Instance-only identity, storage, and runtime administration. This surface is intentionally separate from GitHub-compatible routes.</p><section><h2>Live service monitoring</h2><div class="stats" id="stats"><div class="stat">Loading…</div></div></section><form id="new-user"><input name="login" placeholder="login" required><input name="email" type="email" placeholder="email" required><input name="password" type="password" placeholder="temporary password" required><select name="role"><option value="developer">developer</option><option value="admin">admin</option></select><button>Create local user</button></form><table><thead><tr><th>Login</th><th>Email</th><th>Role</th></tr></thead><tbody id="users"></tbody></table></main><script>const users=document.querySelector('#users'),stats=document.querySelector('#stats');const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));async function load(){const [u,m,s,t]=await Promise.all([fetch('/internal/users'),fetch('/internal/metrics'),fetch('/internal/status'),fetch('/internal/storage')]);if(u.ok)users.innerHTML=(await u.json()).map(x=>'<tr><td>@'+esc(x.login)+'</td><td>'+esc(x.email)+'</td><td>'+ (x.site_admin?'admin':'developer')+'</td></tr>').join('');if(m.ok&&s.ok&&t.ok){const metrics=await m.json(),status=await s.json(),storage=await t.json();stats.innerHTML='<div class="stat">Workflows<b>'+esc(status.active_workflows)+'</b></div><div class="stat">Connected runners<b>'+esc(status.connected_runners)+'</b></div><div class="stat">Uptime<b>'+esc(status.uptime_seconds)+'s</b></div><div class="stat">Git storage<b>'+esc(storage.git)+'</b></div><div class="stat">Persistence<b>'+esc(storage.persistence)+'</b></div><div class="stat">Active sessions<b>'+esc(metrics.active_sessions)+'</b></div>'}}document.querySelector('#new-user').onsubmit=async e=>{e.preventDefault();const f=new FormData(e.target);const body=Object.fromEntries(f);body.site_admin=body.role==='admin';const r=await fetch('/internal/users',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});if(!r.ok){alert('Could not create local user');return}e.target.reset();load()};load();setInterval(load,15000)</script></body></html>`))
}

func randomIdentityState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
