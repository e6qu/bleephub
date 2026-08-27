package bleephub

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// writeOAuthTokenResponse renders an access_token response in the format the client
// negotiated via Accept: JSON for `Accept: application/json`, else GitHub's default
// application/x-www-form-urlencoded. Covers success and error bodies alike.
func writeOAuthTokenResponse(w http.ResponseWriter, r *http.Request, fields map[string]string) {
	// RFC 6749 §5.1: the response carries a credential and must never be cached.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		obj := make(map[string]any, len(fields))
		for k, v := range fields {
			obj[k] = v
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(obj)
		return
	}
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	w.Header().Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(form.Encode()))
}

// parseOAuthRequestParams reads OAuth parameters from either encoding a client may
// send and leaves them in r.Form for downstream r.FormValue reads.
//
// GitHub accepts a JSON body on these endpoints as well as form-encoding — the
// octokit device-flow strategy sends JSON — so reading only the form encoding left
// every value empty and refused the request as bad client credentials.
func parseOAuthRequestParams(w http.ResponseWriter, r *http.Request) bool {
	// ParseForm reads the query string and a form body but leaves a JSON body
	// unread, so it is safe to decode below.
	if err := r.ParseForm(); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing form")
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return true
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	// An empty JSON body carries no parameters; the query string still stands.
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	if r.Form == nil {
		r.Form = url.Values{}
	}
	if r.PostForm == nil {
		r.PostForm = url.Values{}
	}
	for key, value := range body {
		text, ok := oauthParamText(value)
		if !ok {
			continue
		}
		// A body parameter outranks the same name in the query string, as ParseForm orders a form body.
		r.PostForm.Set(key, text)
		r.Form.Set(key, text)
	}
	return true
}

// oauthParamText renders one JSON member as an OAuth parameter string, reporting
// false for a non-scalar member.
func oauthParamText(value interface{}) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		return "", false
	}
}

func (s *Server) registerGHOAuthRoutes() {
	s.route("POST /login/device/code", s.handleDeviceCode)
	s.route("POST /login/oauth/access_token", s.handleOAuthAccessToken)
	s.route("GET /login/device", s.handleDevicePage)
	s.route("POST /login/device", s.handleDeviceApprove)
	// Session login (required before the web-flow authorize step).
	s.route("GET /login", s.handleLoginPage)
	s.route("POST /login", s.rateLimitAuthFlow(s.handleLoginPost))
	// OAuth web flow.
	s.route("GET /login/oauth/authorize", s.handleOAuthAuthorize)
	s.route("POST /login/oauth/authorize", s.handleOAuthAuthorizeApprove)
}

// handleLoginPage starts Shauth sign-in when configured. The legacy PAT form remains
// only when no external identity provider is configured.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// Sanitize return_to to a relative path before it propagates into the
	// /auth/shauth redirect: a `//evil` or absolute value is an open-redirect vector.
	returnTo := r.URL.Query().Get("return_to")
	if returnTo != "" {
		returnTo = safeIdentityReturnTo(returnTo)
	}
	if s.identity.shauthConfigured() {
		query := url.Values{}
		if returnTo != "" {
			query.Set("return_to", returnTo)
		}
		location := "/auth/shauth"
		if encoded := query.Encode(); encoded != "" {
			location += "?" + encoded
		}
		http.Redirect(w, r, location, http.StatusFound)
		return
	}
	page := fmt.Sprintf(`<!DOCTYPE html><html><head><title>Sign in</title></head>
<body style="font-family:system-ui,sans-serif;max-width:340px;margin:48px auto">
<h1>Sign in to bleephub</h1>
<form method="POST" action="/login">
  <input type="hidden" name="return_to" value="%s"/>
  <label>Username<br><input type="text" name="login" autofocus style="width:100%%"/></label><br><br>
  <label>Personal access token<br><input type="password" name="password" style="width:100%%"/></label><br><br>
  <label>Two-factor code <small>(if enrolled)</small><br><input type="text" name="otp" inputmode="numeric" autocomplete="one-time-code" style="width:100%%"/></label><br><br>
  <button type="submit" style="padding:8px 16px;background:#2da44e;color:white;border:0;border-radius:6px;font-size:14px;cursor:pointer">Sign in</button>
</form>
</body></html>`,
		html.EscapeString(returnTo),
	)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

// handleLoginPost authenticates a user and sets a session cookie. Browser sessions
// are backed by the same real credential source as API requests.
func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing form")
		return
	}
	login := r.FormValue("login")
	credential := r.FormValue("password")

	if login == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "login is required")
		return
	}
	if credential == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "personal access token is required")
		return
	}

	user := s.browserLoginUser(login, credential)
	if user == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><p>Incorrect username or password.</p></body></html>`))
		return
	}

	if s.requireSecondFactor(w, user, secondFactorFromRequest(r, strings.TrimSpace(r.FormValue("otp")))) {
		return
	}

	// Revoke any prior session before issuing a new one, so a fixation cookie
	// planted before authentication cannot outlive it.
	if cookie := s.sessionCookieFromRequest(r); cookie != nil {
		if err := s.store.DeleteLoginSession(cookie.Value); err != nil {
			s.logger.Error().Err(err).Msg("revoke prior session on login")
		}
	}
	sessionID := uuid.New().String()
	csrf := uuid.New().String()
	sess := &store.LoginSession{
		UserID:     user.ID,
		CSRFToken:  csrf,
		ExpiresAt:  time.Now().Add(time.Hour),
		Handle:     uuid.New().String(),
		CreatedAt:  s.currentTime(),
		UserAgent:  truncateSessionUserAgent(r.UserAgent()),
		SignedInIP: sessionClientIP(r),
		// Password (and any second factor) were just verified, so the session opens with proof of presence.
		SudoAt:  s.currentTime(),
		SudoMFA: s.store.TwoFactorEnabled(user.ID),
	}
	if err := s.store.PutLoginSession(sessionID, sess); err != nil {
		s.logger.Error().Err(err).Msg("persist browser session")
		writeGHError(w, http.StatusServiceUnavailable, "browser session is unavailable")
		return
	}

	secure := s.secureCookies(r)
	// Secure is always set (honored over http://localhost); the __Host- prefixed name
	// is used only for https origins.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieNameFor(secure),
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})

	// The legacy PAT login does not honor a caller-supplied return_to: it was an
	// open-redirect vector (AUTH-104). Render the signed-in confirmation instead.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><p>Signed in successfully.</p></body></html>`))
}

func (s *Server) browserLoginUser(login, credential string) *store.User {
	// Canonicalize the login (NFKC + case fold) so lookalike scripts cannot resolve
	// to a real account and case differences still authenticate.
	normalized, err := normalizeLogin(login)
	if err != nil {
		return nil
	}
	// A PAT is a scoped API capability, not a password: turning one into a browser
	// session would drop its scopes and grant full account access. Accept only the
	// configured site-admin bootstrap credential here.
	_, user := s.store.LookupToken(credential)
	adminCredential := store.AdminToken()
	if user != nil && user.Login == normalized && user.SiteAdmin && !user.Suspended &&
		subtle.ConstantTimeCompare([]byte(credential), []byte(adminCredential)) == 1 {
		return user
	}
	user = s.store.LookupUserByLogin(normalized)
	if user == nil || user.Suspended || user.PasswordHash == "" || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(credential)) != nil {
		return nil
	}
	return user
}

// oauthClientKind resolves a client_id to token-minting parameters: a GitHub App
// client_id yields a non-zero appID (mints ghu_), an OAuth App yields oauthClientID
// (mints gho_). Unknown client IDs report ok=false.
func (s *Server) oauthClientKind(clientID string) (appID int, oauthClientID string, isGitHubApp bool, ok bool) {
	if clientID != "" {
		if app := s.store.GetAppByClientID(clientID); app != nil {
			return app.ID, "", true, true
		}
		if app := s.store.GetOAuthApp(clientID); app != nil {
			return 0, app.ClientID, false, true
		}
	}
	return 0, "", false, false
}

// registeredCallbackURL returns the client's registered callback, the only
// destination an authorization code may be delivered to. A client that registered
// none is refused rather than defaulted — defaulting is an open-redirect primitive.
func (s *Server) registeredCallbackURL(clientID string) (string, bool) {
	if clientID == "" {
		return "", false
	}
	if app := s.store.GetOAuthApp(clientID); app != nil {
		return app.CallbackURL, true
	}
	if app := s.store.GetAppByClientID(clientID); app != nil {
		return app.CallbackURL, true
	}
	return "", false
}

// redirectURIMatchesRegistration reports whether a requested redirect_uri is allowed:
// GitHub permits only a sub-path of the registered callback — same scheme, host, port
// and query, with the registered path a prefix at a segment boundary.
func redirectURIMatchesRegistration(requested, registered string) bool {
	if registered == "" || requested == "" {
		return false
	}
	if requested == registered {
		return true
	}
	want, err := url.Parse(registered)
	if err != nil {
		return false
	}
	got, err := url.Parse(requested)
	if err != nil {
		return false
	}
	if !strings.EqualFold(got.Scheme, want.Scheme) || !strings.EqualFold(got.Host, want.Host) {
		return false
	}
	if got.RawQuery != want.RawQuery || got.User.String() != want.User.String() {
		return false
	}
	// Compare cleaned paths: "/cb/../../elsewhere" prefix-matches "/cb" as written
	// but resolves to a different destination.
	base := strings.TrimSuffix(path.Clean("/"+want.Path), "/")
	candidate := path.Clean("/" + got.Path)
	return candidate == base || strings.HasPrefix(candidate, base+"/")
}

// requireRegisteredRedirectURI enforces the comparison above, writing a
// GitHub-shaped error and reporting whether to continue.
func (s *Server) requireRegisteredRedirectURI(w http.ResponseWriter, clientID, redirectURI string) bool {
	registered, known := s.registeredCallbackURL(clientID)
	if !known {
		writeGHError(w, http.StatusBadRequest, "incorrect_client_credentials")
		return false
	}
	if !redirectURIMatchesRegistration(redirectURI, registered) {
		writeGHError(w, http.StatusBadRequest, "redirect_uri_mismatch")
		return false
	}
	return true
}

func (s *Server) verifyOAuthClientSecret(clientID, clientSecret string) (appID int, oauthClientID string, isGitHubApp bool, ok bool) {
	if clientID == "" || clientSecret == "" {
		return 0, "", false, false
	}
	if app := s.store.VerifyAppClientSecret(clientID, clientSecret); app != nil {
		return app.ID, "", true, true
	}
	if app := s.store.VerifyOAuthAppSecret(clientID, clientSecret); app != nil {
		return 0, app.ClientID, false, true
	}
	return 0, "", false, false
}

func newDeviceUserCode() string {
	raw := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))
	return raw[:4] + "-" + raw[4:8]
}

func normalizeDeviceUserCode(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
}

// handleDeviceCode initiates the device authorization flow.
func (s *Server) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	if !parseOAuthRequestParams(w, r) {
		return
	}
	scope := r.FormValue("scope")
	clientID := r.FormValue("client_id")

	appID, oauthClientID, _, ok := s.oauthClientKind(clientID)
	if !ok {
		writeOAuthTokenResponse(w, r, map[string]string{"error": "incorrect_client_credentials"})
		return
	}

	s.store.Mu.Lock()
	dc := &store.DeviceCode{
		Code:          uuid.New().String(),
		UserCode:      newDeviceUserCode(),
		ClientID:      clientID,
		Scopes:        scope,
		AppID:         appID,
		OAuthClientID: oauthClientID,
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	s.store.DeviceCodes[dc.Code] = dc
	s.store.Mu.Unlock()

	s.logger.Info().Str("user_code", dc.UserCode).Msg("device code issued")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"device_code":      dc.Code,
		"user_code":        dc.UserCode,
		"verification_uri": s.baseURL(r) + "/login/device",
		"expires_in":       900,
		"interval":         1,
	})
}

// handleOAuthAccessToken serves both OAuth flows on the shared endpoint, keyed by
// the fields present: device_code → device flow, code → web flow. Failure is a
// 200 with an {error} body, as on GitHub.
func (s *Server) handleOAuthAccessToken(w http.ResponseWriter, r *http.Request) {
	if !parseOAuthRequestParams(w, r) {
		return
	}
	if r.FormValue("device_code") != "" {
		s.handleDeviceTokenForm(w, r)
		return
	}
	if r.FormValue("code") != "" {
		s.handleWebFlowTokenForm(w, r)
		return
	}
	writeOAuthTokenResponse(w, r, map[string]string{"error": "unsupported_grant_type"})
}

// handleDeviceTokenForm — device-flow polling leg.
func (s *Server) handleDeviceTokenForm(w http.ResponseWriter, r *http.Request) {
	deviceCode := r.FormValue("device_code")
	clientID := r.FormValue("client_id")
	s.store.Mu.Lock()
	dc, ok := s.store.DeviceCodes[deviceCode]

	if !ok {
		s.store.Mu.Unlock()
		writeOAuthTokenResponse(w, r, map[string]string{"error": "bad_verification_code"})
		return
	}
	if clientID == "" || clientID != dc.ClientID {
		s.store.Mu.Unlock()
		writeOAuthTokenResponse(w, r, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	if time.Now().After(dc.ExpiresAt) {
		delete(s.store.DeviceCodes, deviceCode)
		s.store.Mu.Unlock()
		writeOAuthTokenResponse(w, r, map[string]string{"error": "expired_token"})
		return
	}
	if dc.Token == "" {
		s.store.Mu.Unlock()
		writeOAuthTokenResponse(w, r, map[string]string{"error": "authorization_pending"})
		return
	}
	token := dc.Token
	scopes := dc.Scopes
	delete(s.store.DeviceCodes, deviceCode)
	s.store.Mu.Unlock()

	s.logger.Info().Msg("device token granted")

	writeOAuthTokenResponse(w, r, map[string]string{
		"access_token": token,
		"token_type":   "bearer",
		"scope":        scopes,
	})
}

// handleWebFlowTokenForm exchanges a one-time authorization code for an access token,
// requiring the registered client_id + client_secret.
func (s *Server) handleWebFlowTokenForm(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	clientID := r.FormValue("client_id")
	clientSecret := r.FormValue("client_secret")
	redirectURI := r.FormValue("redirect_uri")

	s.store.Mu.Lock()
	ac, ok := s.store.AuthCodes[code]
	if ok {
		delete(s.store.AuthCodes, code)
	}
	s.store.Mu.Unlock()

	if !ok || time.Now().After(ac.ExpiresAt) {
		writeOAuthTokenResponse(w, r, map[string]string{"error": "bad_verification_code"})
		return
	}
	if clientID == "" || ac.ClientID == "" || clientID != ac.ClientID {
		writeOAuthTokenResponse(w, r, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	if redirectURI != "" && ac.RedirectURI != "" && redirectURI != ac.RedirectURI {
		writeOAuthTokenResponse(w, r, map[string]string{"error": "redirect_uri_mismatch"})
		return
	}
	appID, oauthClientID, _, ok := s.verifyOAuthClientSecret(clientID, clientSecret)
	if !ok {
		writeOAuthTokenResponse(w, r, map[string]string{"error": "incorrect_client_credentials"})
		return
	}

	s.store.Mu.Lock()
	user := s.store.Users[ac.UserID]
	if user == nil {
		s.store.Mu.Unlock()
		writeOAuthTokenResponse(w, r, map[string]string{"error": "server_error"})
		return
	}
	tok, _, err := s.store.CreateUserToServerTokenLocked(user.ID, appID, oauthClientID, ac.Scopes, 8*time.Hour, false)
	if err != nil {
		s.store.Mu.Unlock()
		writeOAuthTokenResponse(w, r, map[string]string{"error": "server_error"})
		return
	}
	s.store.Mu.Unlock()

	s.logger.Info().Int("user_id", user.ID).Msg("web flow token granted")
	writeOAuthTokenResponse(w, r, map[string]string{
		"access_token": tok.Token,
		"token_type":   "bearer",
		"scope":        ac.Scopes,
	})
}

// handleDevicePage renders the browser confirmation form for a device code.
func (s *Server) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		http.Redirect(w, r, "/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	userCode := r.URL.Query().Get("user_code")
	page := fmt.Sprintf(`<!DOCTYPE html><html><head><title>Device activation</title></head>
<body style="font-family:system-ui,sans-serif;max-width:420px;margin:48px auto">
<h1>Device activation</h1>
<form method="POST" action="/login/device">
  <label>Code<br><input type="text" name="user_code" value="%s" autofocus style="width:100%%;text-transform:uppercase"/></label><br><br>
  <button type="submit" style="padding:8px 16px;background:#2da44e;color:white;border:0;border-radius:6px;font-size:14px;cursor:pointer">Authorize</button>
</form>
</body></html>`, html.EscapeString(userCode))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

// handleDeviceApprove binds a pending device code to the signed-in browser user.
func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		http.Redirect(w, r, "/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing form")
		return
	}
	userCode := normalizeDeviceUserCode(r.FormValue("user_code"))
	if userCode == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "user_code is required")
		return
	}

	s.store.Mu.Lock()
	var dc *store.DeviceCode
	for _, candidate := range s.store.DeviceCodes {
		if normalizeDeviceUserCode(candidate.UserCode) == userCode {
			dc = candidate
			break
		}
	}
	if dc == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if time.Now().After(dc.ExpiresAt) {
		delete(s.store.DeviceCodes, dc.Code)
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusGone, "device code expired")
		return
	}
	user := s.store.Users[sess.UserID]
	if user == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	tok, _, err := s.store.CreateUserToServerTokenLocked(user.ID, dc.AppID, dc.OAuthClientID, dc.Scopes, 8*time.Hour, false)
	if err != nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dc.Token = tok.Token
	dc.UserID = user.ID
	dc.ApprovedAt = time.Now().UTC()
	s.store.Mu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><p>Device authorized.</p></body></html>`))
}

// handleOAuthAuthorize requires a browser session, redirecting to /login when absent,
// then renders a consent form carrying an authenticity_token (CSRF) the POST must echo.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	scopes := q.Get("scope")
	state := q.Get("state")
	if clientID == "" || redirectURI == "" {
		writeGHError(w, http.StatusBadRequest, "client_id and redirect_uri are required")
		return
	}

	sess := s.sessionFromRequest(r)

	if sess == nil {
		returnTo := r.URL.RequestURI()
		http.Redirect(w, r, "/login?return_to="+url.QueryEscape(returnTo), http.StatusFound)
		return
	}
	if _, _, _, ok := s.oauthClientKind(clientID); !ok {
		writeGHError(w, http.StatusBadRequest, "incorrect_client_credentials")
		return
	}
	if !s.requireRegisteredRedirectURI(w, clientID, redirectURI) {
		return
	}

	s.store.Mu.RLock()
	user := s.store.Users[sess.UserID]
	s.store.Mu.RUnlock()
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "session user not found")
		return
	}
	csrf, err := s.mintConsentToken(r)
	if err != nil {
		s.logger.Error().Err(err).Msg("mint OAuth consent token")
		writeGHError(w, http.StatusServiceUnavailable, "browser session is unavailable")
		return
	}

	page := fmt.Sprintf(`<!DOCTYPE html><html><head><title>Authorize bleephub</title></head>
<body style="font-family:system-ui,sans-serif;max-width:480px;margin:48px auto">
<h1>Authorize app</h1>
<p>Signed in as <strong>%s</strong>. The app <code>%s</code> is requesting access with scopes <code>%s</code>.</p>
<form method="POST" action="/login/oauth/authorize">
  <input type="hidden" name="authenticity_token" value="%s"/>
  <input type="hidden" name="client_id" value="%s"/>
  <input type="hidden" name="redirect_uri" value="%s"/>
  <input type="hidden" name="scope" value="%s"/>
  <input type="hidden" name="state" value="%s"/>
  <button type="submit" style="padding:8px 16px;background:#2da44e;color:white;border:0;border-radius:6px;font-size:14px;cursor:pointer">Authorize</button>
</form>
</body></html>`,
		html.EscapeString(user.Login),
		html.EscapeString(clientID),
		html.EscapeString(scopes),
		html.EscapeString(csrf),
		html.EscapeString(clientID),
		html.EscapeString(redirectURI),
		html.EscapeString(scopes),
		html.EscapeString(state),
	)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

// handleOAuthAuthorizeApprove handles the consent-form POST: validates the session
// and authenticity_token (CSRF), then issues the auth code and 302s to redirect_uri.
func (s *Server) handleOAuthAuthorizeApprove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing form")
		return
	}

	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeGHError(w, http.StatusUnauthorized, "session required — POST /login first")
		return
	}

	consumed, err := s.consumeConsentToken(r, r.FormValue("authenticity_token"))
	if err != nil {
		s.logger.Error().Err(err).Msg("rotate OAuth consent token")
		writeGHError(w, http.StatusServiceUnavailable, "browser session is unavailable")
		return
	}
	if !consumed {
		writeGHError(w, http.StatusUnprocessableEntity, "Invalid authenticity_token")
		return
	}
	s.store.Mu.RLock()
	user := s.store.Users[sess.UserID]
	s.store.Mu.RUnlock()
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "session user not found")
		return
	}

	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	scopes := r.FormValue("scope")
	state := r.FormValue("state")
	if clientID == "" || redirectURI == "" {
		writeGHError(w, http.StatusBadRequest, "client_id and redirect_uri are required")
		return
	}
	if _, _, _, ok := s.oauthClientKind(clientID); !ok {
		writeGHError(w, http.StatusBadRequest, "incorrect_client_credentials")
		return
	}
	s.completeAuthorize(w, r, user, clientID, redirectURI, scopes, state)
}

// mintConsentToken issues the consent form's authenticity_token: a fresh secret bound
// to the session, never the session identifier — that stays in the HttpOnly cookie so
// page markup cannot leak it.
func (s *Server) mintConsentToken(r *http.Request) (string, error) {
	id, sess, err := s.sessionRecordFromRequest(r)
	if err != nil || sess == nil {
		return "", err
	}
	token, err := randomIdentityState()
	if err != nil {
		return "", err
	}
	sess.CSRFToken = token
	if err := s.store.PutLoginSession(id, sess); err != nil {
		return "", err
	}
	return token, nil
}

// consumeConsentToken verifies a submitted authenticity_token and retires it, so a
// leaked token buys nothing after use.
func (s *Server) consumeConsentToken(r *http.Request, provided string) (bool, error) {
	id, sess, err := s.sessionRecordFromRequest(r)
	if err != nil || sess == nil {
		return false, err
	}
	if provided == "" || sess.CSRFToken == "" || !store.SecretEqual(provided, sess.CSRFToken) {
		return false, nil
	}
	rotated, err := randomIdentityState()
	if err != nil {
		return false, err
	}
	sess.CSRFToken = rotated
	if err := s.store.PutLoginSession(id, sess); err != nil {
		return false, err
	}
	return true, nil
}

// sessionRecordFromRequest returns the session and the cookie value that keys it,
// which the CSRF rotation needs and sessionFromRequest drops.
func (s *Server) sessionRecordFromRequest(r *http.Request) (string, *store.LoginSession, error) {
	cookie := s.sessionCookieFromRequest(r)
	if cookie == nil {
		return "", nil, nil
	}
	sess, err := s.store.GetLoginSession(cookie.Value)
	if err != nil {
		return "", nil, err
	}
	if sess == nil || time.Now().After(sess.ExpiresAt) {
		return "", nil, nil
	}
	return cookie.Value, sess, nil
}

// completeAuthorize mints a one-time auth code bound to user and 302s back to
// redirect_uri with code + state. The destination is re-checked here — the single
// point at which a code becomes deliverable.
func (s *Server) completeAuthorize(w http.ResponseWriter, r *http.Request, user *store.User, clientID, redirectURI, scopes, state string) {
	if !s.requireRegisteredRedirectURI(w, clientID, redirectURI) {
		return
	}
	s.store.Mu.Lock()
	if s.store.AuthCodes == nil {
		s.store.AuthCodes = map[string]*store.AuthCode{}
	}
	code := uuid.New().String()
	// An omitted scope grants read-only public access (GitHub's default), never a
	// silent upgrade to `repo`; the empty string carries that grant downstream.
	s.store.AuthCodes[code] = &store.AuthCode{
		Code:        code,
		ClientID:    clientID,
		RedirectURI: redirectURI,
		Scopes:      scopes,
		State:       state,
		UserID:      user.ID,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	}
	s.store.Mu.Unlock()

	dest, err := url.Parse(redirectURI)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid redirect_uri")
		return
	}
	q := dest.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	dest.RawQuery = q.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

// sessionFromRequest returns the cookie's LoginSession, or nil if absent, expired, or unknown.
func (s *Server) sessionFromRequest(r *http.Request) *store.LoginSession {
	cookie := s.sessionCookieFromRequest(r)
	if cookie == nil {
		return nil
	}
	sess, err := s.store.GetLoginSession(cookie.Value)
	if err != nil {
		s.logger.Error().Err(err).Msg("read browser session")
		return nil
	}
	s.store.Mu.RLock()
	var user *store.User
	if sess != nil {
		user = s.store.Users[sess.UserID]
	}
	s.store.Mu.RUnlock()
	if sess == nil || user == nil || user.Suspended || time.Now().After(sess.ExpiresAt) {
		return nil
	}
	return sess
}
