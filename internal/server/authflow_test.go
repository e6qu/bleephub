package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
)

// --- shared fixtures ---

var authflowSeq int64

func authflowName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, int64(testutil.NextTestID()), atomic.AddInt64(&authflowSeq, 1))
}

// authflowStranger seeds a user with no relationship to anything and returns
// the user together with a classic token for it.
func authflowStranger(t *testing.T, s *Server, login string) (*store.User, string) {
	t.Helper()
	s.store.Mu.Lock()
	now := fixedTestTime.UTC()
	user := &store.User{ID: s.store.NextUser, Login: login, Type: "User", StarredRepos: map[string]bool{}, CreatedAt: now, UpdatedAt: now}
	s.store.Users[user.ID] = user
	s.store.UsersByLogin[login] = user
	s.store.NextUser++
	s.store.Mu.Unlock()
	return user, s.store.CreateToken(user.ID, "repo,workflow").Value
}

// withUser attaches a resolved user to a request the way the auth middleware
// does, so a handler can be exercised without its route decorator. Several of
// the fixes below are deliberately redundant with the route gate; calling the
// handler directly is the only way to show the handler itself refuses.
func withUser(r *http.Request, user *store.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxUser, user))
}

// --- redirect_uri must match the client's registration ---

func TestOAuthAuthorizeRefusesUnregisteredRedirectURI(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	app := createOAuthTestApp(t, s, "https://app.test/callback")
	s.registerGHOAuthRoutes()

	jar := doLogin(t, s, "admin")
	authorize := func(redirectURI string) *httptest.ResponseRecorder {
		return requestWithJar(s, "GET", "/login/oauth/authorize?client_id="+url.QueryEscape(app.ClientID)+
			"&redirect_uri="+url.QueryEscape(redirectURI)+"&scope=repo&state=S", "", "", jar)
	}

	for _, hostile := range []string{
		"https://evil.test/steal",
		"https://app.test.evil.test/callback",
		"http://app.test/callback",
		"https://app.test/callback/../../other",
		"https://app.test/callbackother",
	} {
		w := authorize(hostile)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET authorize redirect_uri=%q status = %d, want 400; body=%s", hostile, w.Code, w.Body.String())
		}
	}

	// The registered callback and a sub-path of it are the two GitHub accepts.
	for _, allowed := range []string{"https://app.test/callback", "https://app.test/callback/deep"} {
		if w := authorize(allowed); w.Code != http.StatusOK {
			t.Errorf("GET authorize redirect_uri=%q status = %d, want 200; body=%s", allowed, w.Code, w.Body.String())
		}
	}
}

// TestOAuthApproveRefusesUnregisteredRedirectURI covers the leg that actually
// mints the code: a consent form is fetched honestly and the hidden field is
// then rewritten, which is exactly what an interception attempt looks like.
func TestOAuthApproveRefusesUnregisteredRedirectURI(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	app := createOAuthTestApp(t, s, "https://app.test/callback")
	s.registerGHOAuthRoutes()

	jar := doLogin(t, s, "admin")
	w := requestWithJar(s, "GET", "/login/oauth/authorize?client_id="+url.QueryEscape(app.ClientID)+
		"&redirect_uri="+url.QueryEscape("https://app.test/callback")+"&scope=repo&state=S", "", "", jar)
	if w.Code != http.StatusOK {
		t.Fatalf("GET authorize status = %d, want 200", w.Code)
	}
	csrf := extractCSRF(t, w.Body.String())

	form := url.Values{}
	form.Set("authenticity_token", csrf)
	form.Set("client_id", app.ClientID)
	form.Set("redirect_uri", "https://evil.test/steal")
	form.Set("scope", "repo")
	form.Set("state", "S")
	w2 := requestWithJar(s, "POST", "/login/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", jar)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("POST authorize with a rewritten redirect_uri status = %d, want 400; body=%s", w2.Code, w2.Body.String())
	}
	if loc := w2.Header().Get("Location"); loc != "" {
		t.Fatalf("POST authorize redirected to %q; no code may leave for an unregistered destination", loc)
	}
}

// --- a GitHub App's registered callback ---

// registerGitHubAppViaManifest submits the app manifest the way the browser
// form does and returns the created app. callbackURLs is passed through
// verbatim so a malformed registration can be exercised too.
func registerGitHubAppViaManifest(t *testing.T, s *Server, name string, callbackURLs []string) (*store.App, int, string) {
	t.Helper()
	manifest := map[string]interface{}{
		"name":         name,
		"url":          "https://ghapp.test/home",
		"redirect_url": "https://ghapp.test/created",
	}
	if callbackURLs != nil {
		manifest["callback_urls"] = callbackURLs
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"manifest": {string(encoded)}}
	req := httptest.NewRequest("POST", "/settings/apps/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "token "+store.AdminToken())
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		return nil, w.Code, w.Body.String()
	}
	app := s.store.GetAppBySlug(store.Slugify(name))
	if app == nil {
		t.Fatalf("manifest submission returned 302 but registered no app named %q", name)
	}
	return app, w.Code, w.Body.String()
}

func TestGitHubAppWebFlowUsesItsRegisteredCallback(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerRoutes()

	app, code, body := registerGitHubAppViaManifest(t, s, authflowName("cb-app"), []string{"https://ghapp.test/cb"})
	if app == nil {
		t.Fatalf("manifest submission status = %d; body=%s", code, body)
	}
	if app.CallbackURL != "https://ghapp.test/cb" {
		t.Fatalf("registered callback = %q, want https://ghapp.test/cb", app.CallbackURL)
	}

	jar := doLogin(t, s, "admin")
	authorize := func(redirectURI string) *httptest.ResponseRecorder {
		return requestWithJar(s, "GET", "/login/oauth/authorize?client_id="+url.QueryEscape(app.ClientID)+
			"&redirect_uri="+url.QueryEscape(redirectURI)+"&scope=repo&state=S", "", "", jar)
	}
	if w := authorize("https://evil.test/steal"); w.Code != http.StatusBadRequest {
		t.Fatalf("GET authorize to an unregistered destination status = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	w := authorize("https://ghapp.test/cb")
	if w.Code != http.StatusOK {
		t.Fatalf("GET authorize to the registered callback status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	form := url.Values{}
	form.Set("authenticity_token", extractCSRF(t, w.Body.String()))
	form.Set("client_id", app.ClientID)
	form.Set("redirect_uri", "https://ghapp.test/cb")
	form.Set("scope", "repo")
	form.Set("state", "S")
	w2 := requestWithJar(s, "POST", "/login/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", jar)
	if w2.Code != http.StatusFound {
		t.Fatalf("POST authorize status = %d, want 302; body=%s", w2.Code, w2.Body.String())
	}
	loc, err := url.Parse(w2.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorize redirect: %v", err)
	}
	if loc.Host != "ghapp.test" || loc.Path != "/cb" {
		t.Fatalf("code delivered to %q, want the registered callback", loc.String())
	}
	authCode := loc.Query().Get("code")
	if authCode == "" {
		t.Fatalf("no authorization code in %q", loc.String())
	}

	exchange := url.Values{}
	exchange.Set("code", authCode)
	exchange.Set("client_id", app.ClientID)
	exchange.Set("client_secret", app.ClientSecret)
	req := httptest.NewRequest("POST", "/login/oauth/access_token", strings.NewReader(exchange.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	w3 := httptest.NewRecorder()
	s.mux.ServeHTTP(w3, req)
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(w3.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode token response %q: %v", w3.Body.String(), err)
	}
	if tok.Error != "" {
		t.Fatalf("token exchange error: %s", tok.Error)
	}
	if !strings.HasPrefix(tok.AccessToken, "ghu_") {
		t.Fatalf("GitHub App web flow token = %q, want a ghu_ user-to-server token", tok.AccessToken)
	}
}

// TestGitHubAppWithoutARegisteredCallbackIsStillRefused is the half of the fix
// that must not regress: an App may now have a callback, and one without a
// callback is still refused rather than tolerated.
func TestGitHubAppWithoutARegisteredCallbackIsStillRefused(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")

	viaStore := s.store.CreateApp(admin.ID, authflowName("nocb-store"), "", nil, nil)
	viaManifest, code, body := registerGitHubAppViaManifest(t, s, authflowName("nocb-manifest"), nil)
	if viaManifest == nil {
		t.Fatalf("manifest submission without callback_urls status = %d; body=%s", code, body)
	}
	if viaManifest.CallbackURL != "" {
		t.Fatalf("a manifest with no callback_urls recorded %q", viaManifest.CallbackURL)
	}

	jar := doLogin(t, s, "admin")
	for _, app := range []*store.App{viaStore, viaManifest} {
		for _, redirectURI := range []string{"https://anywhere.test/cb", "https://ghapp.test/cb", ""} {
			w := requestWithJar(s, "GET", "/login/oauth/authorize?client_id="+url.QueryEscape(app.ClientID)+
				"&redirect_uri="+url.QueryEscape(redirectURI)+"&scope=repo&state=S", "", "", jar)
			if w.Code != http.StatusBadRequest {
				t.Errorf("app %q with no registered callback, redirect_uri=%q: status = %d, want 400",
					app.Slug, redirectURI, w.Code)
			}
		}
	}
}

func TestAppRegistrationRejectsAMalformedCallback(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerRoutes()

	for _, bad := range [][]string{
		{"/relative/cb"},
		{"javascript:alert(1)"},
		{"ftp://ghapp.test/cb"},
		{"https:///nohost"},
		{"https://a.test/cb", "https://b.test/cb"},
	} {
		app, code, body := registerGitHubAppViaManifest(t, s, authflowName("badcb"), bad)
		if app != nil {
			t.Errorf("manifest callback_urls=%v registered an app with callback %q", bad, app.CallbackURL)
			continue
		}
		if code != http.StatusUnprocessableEntity {
			t.Errorf("manifest callback_urls=%v status = %d, want 422; body=%s", bad, code, body)
		}
	}

	// The OAuth App half of the same registration rule.
	for _, bad := range []string{"/relative/cb", "javascript:alert(1)", "ftp://oapp.test/cb"} {
		form := url.Values{"name": {authflowName("badcb-oauth")}, "callback_url": {bad}}
		req := httptest.NewRequest("POST", "/settings/oauth-apps/new", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "token "+store.AdminToken())
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("OAuth app registration callback_url=%q status = %d, want 422; body=%s", bad, w.Code, w.Body.String())
		}
	}
}

func TestGitHubAppCallbackSurvivesAReload(t *testing.T) {
	var appID int
	st := reloadedStore(t, func(_ *store.Persistence, st *store.Store) {
		st.SeedDefaultUser()
		app := st.CreateApp(st.UsersByLogin["admin"].ID, "Reloaded Callback App", "", nil, nil)
		appID = app.ID
		if !st.UpdateApp(app.ID, func(a *store.App) { a.CallbackURL = "https://reloaded.test/cb" }) {
			t.Fatal("UpdateApp did not find the app it just created")
		}
	})
	got := st.GetApp(appID)
	if got == nil {
		t.Fatal("app did not persist")
	}
	if got.CallbackURL != "https://reloaded.test/cb" {
		t.Fatalf("callback after reload = %q, want https://reloaded.test/cb — the web flow would refuse the app", got.CallbackURL)
	}
}

// --- the consent token is not the session cookie ---

func TestOAuthConsentTokenIsNotTheSessionCookie(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	app := createOAuthTestApp(t, s, "https://app.test/callback")
	s.registerGHOAuthRoutes()

	// The session is established the way the identity provider establishes
	// one, which is the factory that used the cookie value as the CSRF token.
	sessionRecorder := httptest.NewRecorder()
	if err := s.createBrowserSession(sessionRecorder, httptest.NewRequest(http.MethodGet, "/", nil), s.store.LookupUserByLogin("admin")); err != nil {
		t.Fatalf("createBrowserSession: %v", err)
	}
	jar := newPermissiveTestJar()
	jarURL, _ := url.Parse("http://bleephub.test")
	jar.SetCookies(jarURL, sessionRecorder.Result().Cookies())
	session := ""
	for _, c := range jar.Cookies(jarURL) {
		if c.Name == "_gh_sess" {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("no _gh_sess cookie after login")
	}

	w := requestWithJar(s, "GET", "/login/oauth/authorize?client_id="+url.QueryEscape(app.ClientID)+
		"&redirect_uri="+url.QueryEscape("https://app.test/callback")+"&scope=repo&state=S", "", "", jar)
	if w.Code != http.StatusOK {
		t.Fatalf("GET authorize status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	csrf := extractCSRF(t, body)
	if csrf == session {
		t.Fatal("authenticity_token equals the session cookie value; the consent page discloses the session")
	}
	if strings.Contains(body, session) {
		t.Fatal("consent page body contains the session cookie value")
	}
}

func TestOAuthConsentTokenIsSingleUse(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	app := createOAuthTestApp(t, s, "https://app.test/callback")
	s.registerGHOAuthRoutes()

	jar := doLogin(t, s, "admin")
	w := requestWithJar(s, "GET", "/login/oauth/authorize?client_id="+url.QueryEscape(app.ClientID)+
		"&redirect_uri="+url.QueryEscape("https://app.test/callback")+"&scope=repo&state=S", "", "", jar)
	csrf := extractCSRF(t, w.Body.String())

	form := url.Values{}
	form.Set("authenticity_token", csrf)
	form.Set("client_id", app.ClientID)
	form.Set("redirect_uri", "https://app.test/callback")
	form.Set("scope", "repo")
	form.Set("state", "S")

	first := requestWithJar(s, "POST", "/login/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", jar)
	if first.Code != http.StatusFound {
		t.Fatalf("first POST authorize status = %d, want 302; body=%s", first.Code, first.Body.String())
	}
	second := requestWithJar(s, "POST", "/login/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", jar)
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("replayed authenticity_token status = %d, want 422; body=%s", second.Code, second.Body.String())
	}
}

// TestBrowserSessionCSRFTokenIsIndependent covers the other session factory:
// the identity/OIDC one, which set the CSRF token to the cookie value itself.
func TestBrowserSessionCSRFTokenIsIndependent(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	user := s.store.LookupUserByLogin("admin")

	w := httptest.NewRecorder()
	if err := s.createBrowserSession(w, httptest.NewRequest(http.MethodGet, "/", nil), user); err != nil {
		t.Fatalf("createBrowserSession: %v", err)
	}
	cookieValue := ""
	for _, c := range w.Result().Cookies() {
		if c.Name == "_gh_sess" {
			cookieValue = c.Value
		}
	}
	if cookieValue == "" {
		t.Fatal("createBrowserSession set no _gh_sess cookie")
	}
	sess, err := s.store.GetLoginSession(cookieValue)
	if err != nil || sess == nil {
		t.Fatalf("GetLoginSession(%q) = %v, %v", cookieValue, sess, err)
	}
	if sess.CSRFToken == cookieValue {
		t.Fatal("session CSRF token equals the session identifier")
	}
	if sess.CSRFToken == "" {
		t.Fatal("session has no CSRF token")
	}
}

func TestTokenLoginExchangesUserCredentialForHttpOnlySession(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	user := s.store.LookupUserByLogin("admin")
	token := s.store.CreateToken(user.ID, "repo")
	request := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	request.Header.Set("Authorization", "Bearer "+token.Value)
	response := httptest.NewRecorder()

	s.handleTokenLogin(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("token login status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("token login cookies = %d, want 1", len(cookies))
	}
	if !cookies[0].HttpOnly || cookies[0].Value == "" {
		t.Fatalf("token login cookie is not an opaque HttpOnly session: %+v", cookies[0])
	}

	invalid := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	invalid.Header.Set("Authorization", "Bearer ghs_not-a-user-token")
	rejected := httptest.NewRecorder()
	s.handleTokenLogin(rejected, invalid)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("installation-shaped token login status = %d, want 401", rejected.Code)
	}
}

// --- repository secrets: resolved scope, administrator only ---

func TestRepositorySecretWriteRefusesAStranger(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, authflowName("secret-owner"), "", false)
	stranger, strangerToken := authflowStranger(t, s, authflowName("stranger"))

	enc, keyID, err := s.store.SealSecretValue("stolen")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"encrypted_value": enc, "key_id": keyID})

	// Through the route, with the stranger's own credential.
	req := httptest.NewRequest("PUT", "/api/v3/repos/"+repo.FullName+"/actions/secrets/PROD", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "token "+strangerToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code == http.StatusCreated || w.Code == http.StatusNoContent {
		t.Errorf("stranger PUT repository secret status = %d, want a denial", w.Code)
	}

	// And directly at the handler, which must refuse on its own rather than
	// relying on whatever decorator happens to be wrapped around it.
	direct := httptest.NewRequest("PUT", "/api/v3/repos/"+repo.FullName+"/actions/secrets/PROD", strings.NewReader(string(payload)))
	direct.Header.Set("Content-Type", "application/json")
	direct.SetPathValue("owner", admin.Login)
	direct.SetPathValue("repo", repo.Name)
	direct.SetPathValue("secret_name", "PROD")
	dw := httptest.NewRecorder()
	s.handlePutSecret(dw, withUser(direct, stranger))
	if dw.Code != http.StatusForbidden {
		t.Errorf("handlePutSecret for a stranger status = %d, want 403; body=%s", dw.Code, dw.Body.String())
	}

	s.store.Mu.RLock()
	written := len(s.store.RepoSecrets[repo.FullName])
	s.store.Mu.RUnlock()
	if written != 0 {
		t.Errorf("stranger wrote %d secrets into %s", written, repo.FullName)
	}
}

// TestRepositorySecretRejectsCaseVariantScope pins the shadow-scope failure
// mode: a path spelling the repository differently must never key a scope map
// nobody reads. Since case-insensitive resolution (GitHub parity), the
// variant path resolves to the canonical repository, so the write must land
// under the canonical key — the one the job injector reads — and succeed.
func TestRepositorySecretRejectsCaseVariantScope(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, authflowName("secret-case"), "", false)
	variantOwner := strings.ToUpper(admin.Login)

	enc, keyID, err := s.store.SealSecretValue("prod-value")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"encrypted_value": enc, "key_id": keyID})

	req := httptest.NewRequest("PUT", "/api/v3/repos/"+variantOwner+"/"+repo.Name+"/actions/secrets/PROD",
		strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("owner", variantOwner)
	req.SetPathValue("repo", repo.Name)
	req.SetPathValue("secret_name", "PROD")
	w := httptest.NewRecorder()
	s.handlePutSecret(w, withUser(req, admin))

	if w.Code != http.StatusCreated {
		t.Errorf("case-variant secret PUT status = %d; want 201 landing on the canonical repository", w.Code)
	}
	s.store.Mu.RLock()
	shadow := len(s.store.RepoSecrets[variantOwner+"/"+repo.Name])
	real := len(s.store.RepoSecrets[repo.FullName])
	s.store.Mu.RUnlock()
	if shadow != 0 {
		t.Errorf("case-variant path wrote %d secrets under the shadow key %q", shadow, variantOwner+"/"+repo.Name)
	}
	if real != 1 {
		t.Errorf("case-variant path wrote %d secrets under the real key %q; want 1", real, repo.FullName)
	}
}

// TestRepositorySecretHandlersKeyOffTheResolvedRepository shows the accepted
// path stores under the repository's own full name, which is the key the job
// injector reads.
func TestRepositorySecretHandlersKeyOffTheResolvedRepository(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, authflowName("secret-key"), "", false)

	enc, keyID, err := s.store.SealSecretValue("prod-value")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"encrypted_value": enc, "key_id": keyID})
	req := httptest.NewRequest("PUT", "/api/v3/repos/"+repo.FullName+"/actions/secrets/PROD", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "token "+store.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("owner PUT repository secret status = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	secrets, _, err := s.actions.CollectJobSecretsAndVars(repo.FullName, "")
	if err != nil {
		t.Fatal(err)
	}
	if secrets["PROD"] != "prod-value" {
		t.Fatalf("injector saw PROD=%q, want prod-value", secrets["PROD"])
	}
}

// --- destructive ref writes ---

// authflowProtectedRepo creates an auto-initialised repository on the shared
// harness, protects main, and returns the repository name plus a pushing
// collaborator's token.
func (s *isolatedServer) authflowProtectedRepo(t *testing.T) (repoName string, pushToken string) {
	t.Helper()
	repoName = s.createRepoWriteRepo(t, true)
	repo := s.store.GetRepo("admin", repoName)
	if repo == nil {
		t.Fatalf("fixture repository admin/%s not found", repoName)
	}
	s.setBranchProtection(repo, "main", &store.BranchProtection{
		EnforceAdmins: &store.BPEnforceAdmins{Enabled: false},
	})
	pusher, token := authflowStranger(t, s.Server, authflowName("pusher"))
	if !s.store.AddRepoCollaborator("admin", repoName, pusher.Login, "push") {
		t.Fatal("could not add the pushing collaborator")
	}
	return repoName, token
}

func TestProtectedBranchRefusesForcePushWithoutAdmin(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoName, pushToken := s.authflowProtectedRepo(t)
	base := "/api/v3/repos/admin/" + repoName

	head := decodeJSONWithStatus(t, s.get(t, base+"/git/refs/heads/main", defaultToken), 200)
	object, _ := head["object"].(map[string]interface{})
	sha, _ := object["sha"].(string)
	if sha == "" {
		t.Fatalf("no head sha for admin/%s: %v", repoName, head)
	}

	body := map[string]interface{}{"sha": sha, "force": true}
	resp := s.patch(t, base+"/git/refs/heads/main", pushToken, body)
	requireStatus(t, resp, http.StatusForbidden)

	_, strangerToken := authflowStranger(t, s.Server, authflowName("noaccess"))
	resp = s.patch(t, base+"/git/refs/heads/main", strangerToken, body)
	if resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		t.Fatalf("a non-collaborator force-pushed refs/heads/main")
	}
	resp.Body.Close()
}

func TestProtectedBranchRefusesDeletionWithoutAdmin(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoName, pushToken := s.authflowProtectedRepo(t)
	base := "/api/v3/repos/admin/" + repoName

	resp := s.delete(t, base+"/git/refs/heads/main", pushToken)
	requireStatus(t, resp, http.StatusForbidden)

	_, strangerToken := authflowStranger(t, s.Server, authflowName("nodelete"))
	resp = s.delete(t, base+"/git/refs/heads/main", strangerToken)
	if resp.StatusCode == http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("a non-collaborator deleted refs/heads/main")
	}
	resp.Body.Close()

	// The branch survived both attempts.
	requireStatus(t, s.get(t, base+"/git/refs/heads/main", defaultToken), 200)
}

// TestProtectedBranchAllowanceIsHonoured keeps the gate from becoming a blanket
// refusal: allow_deletions is exactly the setting that permits the deletion.
func TestProtectedBranchAllowanceIsHonoured(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoName, pushToken := s.authflowProtectedRepo(t)
	repo := s.store.GetRepo("admin", repoName)
	s.setBranchProtection(repo, "main", &store.BranchProtection{
		AllowDeletions: &store.BPEnabled{Enabled: true},
	})
	requireStatus(t, s.delete(t, "/api/v3/repos/admin/"+repoName+"/git/refs/heads/main", pushToken), 204)
}

// --- branch protection is administrator-only ---

func TestBranchProtectionRefusesNonAdmins(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoName, pushToken := s.authflowProtectedRepo(t)
	base := "/api/v3/repos/admin/" + repoName + "/branches/main/protection"

	writes := []struct {
		method string
		path   string
		body   map[string]interface{}
	}{
		{"PUT", base, map[string]interface{}{"enforce_admins": false, "allow_force_pushes": true}},
		{"DELETE", base, nil},
		{"POST", base + "/enforce_admins", map[string]interface{}{}},
		{"DELETE", base + "/restrictions", nil},
	}
	for _, tc := range writes {
		var resp *http.Response
		switch tc.method {
		case "PUT":
			resp = s.put(t, tc.path, pushToken, tc.body)
		case "POST":
			resp = s.post(t, tc.path, pushToken, tc.body)
		default:
			resp = s.delete(t, tc.path, pushToken)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			t.Errorf("%s %s by a pushing non-admin status = %d, want a denial", tc.method, tc.path, resp.StatusCode)
			continue
		}
		resp.Body.Close()
	}

	for _, path := range []string{base, base + "/enforce_admins", base + "/restrictions"} {
		resp := s.get(t, path, pushToken)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			t.Errorf("GET %s by a pushing non-admin status = %d, want a denial", path, resp.StatusCode)
			continue
		}
		resp.Body.Close()
	}

	// The owner is still served, so the denials above are a gate and not an
	// endpoint that refuses everyone.
	requireStatus(t, s.get(t, base, defaultToken), 200)
}

// --- source import must not fetch private address space ---

func TestSourceImportRefusesNonPublicSources(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, authflowName("import-ssrf"), "", false)

	// Loopback is a permitted delivery/fetch target (see the sibling test); every
	// OTHER non-public address is refused, as are non-http(s) schemes. The cloud
	// metadata endpoint, RFC1918, and IPv6 unique-local space must never resolve.
	for _, hostile := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[::ffff:169.254.169.254]/latest/meta-data/",
		"http://10.0.0.5/repo.git",
		"http://[fd00::1]/repo.git",
		"file:///etc/passwd",
		"ssh://git@127.0.0.1/repo.git",
	} {
		payload, _ := json.Marshal(map[string]string{"vcs": "git", "vcs_url": hostile})
		req := httptest.NewRequest("PUT", "/api/v3/repos/"+repo.FullName+"/import", strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "token "+store.AdminToken())
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("PUT import vcs_url=%q status = %d, want 422; body=%s", hostile, w.Code, w.Body.String())
		}
		if imp := s.store.GetRepoImport(repo.ID); imp != nil {
			t.Fatalf("a refused import for %q left a record behind", hostile)
		}
	}
}

// TestLoopbackDeliveryPermittedNonPublicRefused pins the fixed outbound policy:
// loopback is a legitimate delivery target for both server-initiated transports,
// while the scheme rule and the block on every other non-public address still
// hold. There is no switch to relax any of it.
func TestLoopbackDeliveryPermittedNonPublicRefused(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, authflowName("outbound"), "", false)

	importSource := func(raw string) int {
		payload, _ := json.Marshal(map[string]string{"vcs": "git", "vcs_url": raw})
		req := httptest.NewRequest("PUT", "/api/v3/repos/"+repo.FullName+"/import", strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "token "+store.AdminToken())
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w.Code
	}

	// A loopback source is admitted past the address gate (the fetch itself then
	// fails honestly, which is a 201 carrying an error status).
	if got := importSource("http://127.0.0.1:1/repo.git"); got != http.StatusCreated {
		t.Errorf("loopback import status = %d, want 201", got)
	}
	// The scheme rule is absolute — no http(s), no delivery.
	for _, badScheme := range []string{"file:///etc/passwd", "ssh://git@127.0.0.1/repo.git"} {
		if got := importSource(badScheme); got != http.StatusUnprocessableEntity {
			t.Errorf("import vcs_url=%q status = %d, want 422 — the scheme rule is not relaxable", badScheme, got)
		}
	}
	// Webhook configuration admits a loopback https target...
	if err := validateWebhookTargetURL("https://127.0.0.1/hook"); err != nil {
		t.Errorf("loopback webhook target refused: %v", err)
	}
	// ...but never the cloud metadata endpoint or other private space.
	for _, blocked := range []string{"https://169.254.169.254/hook", "https://10.0.0.5/hook"} {
		if err := validateWebhookTargetURL(blocked); err == nil {
			t.Errorf("webhook target %q was admitted; a non-loopback private address must be refused", blocked)
		}
	}
}

// --- the invented per-username codespaces route is gone ---

func TestUserCodespacesByLoginRouteIsNotRegistered(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	victim, _ := authflowStranger(t, s.Server, authflowName("cs-victim"))
	for _, token := range []string{"", defaultToken} {
		resp := s.get(t, "/api/v3/users/"+victim.Login+"/codespaces", token)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /users/%s/codespaces status = %d, want 404 — the route is not a real GitHub endpoint",
				victim.Login, resp.StatusCode)
		}
	}
}

// --- gist sub-resources honour visibility ---

func TestSecretGistSubResourcesRefuseAnonymousCallers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner, ownerToken := authflowStranger(t, s.Server, authflowName("gist-owner"))
	_ = owner
	created := decodeJSONWithStatus(t, s.post(t, "/api/v3/gists", ownerToken, map[string]interface{}{
		"description": "secret",
		"public":      false,
		"files":       map[string]interface{}{"secret.txt": map[string]interface{}{"content": "TOP-SECRET-GIST-BODY"}},
	}), 201)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("no gist id in %v", created)
	}

	commits := decodeJSONWithStatus2xxArray(t, s.get(t, "/api/v3/gists/"+id+"/commits", ownerToken), 200)
	if len(commits) == 0 {
		t.Fatal("the secret gist has no revisions to address")
	}
	sha, _ := commits[0]["version"].(string)
	if sha == "" {
		t.Fatalf("no revision sha in %v", commits[0])
	}

	for _, path := range []string{
		"/api/v3/gists/" + id,
		"/api/v3/gists/" + id + "/" + sha,
		"/api/v3/gists/" + id + "/commits",
		"/api/v3/gists/" + id + "/forks",
		"/api/v3/gists/" + id + "/comments",
	} {
		resp := s.get(t, path, "")
		body := readAllString(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("anonymous GET %s status = %d, want 404", path, resp.StatusCode)
		}
		if strings.Contains(body, "TOP-SECRET-GIST-BODY") {
			t.Errorf("anonymous GET %s returned the secret gist's file contents", path)
		}
	}

	// A stranger with a credential is no better placed than an anonymous one.
	_, strangerToken := authflowStranger(t, s.Server, authflowName("gist-stranger"))
	for _, path := range []string{
		"/api/v3/gists/" + id + "/" + sha,
		"/api/v3/gists/" + id + "/commits",
	} {
		resp := s.get(t, path, strangerToken)
		body := readAllString(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("stranger GET %s status = %d, want 404", path, resp.StatusCode)
		}
		if strings.Contains(body, "TOP-SECRET-GIST-BODY") {
			t.Errorf("stranger GET %s returned the secret gist's file contents", path)
		}
	}

	// The owner still reaches every one of them.
	requireStatus(t, s.get(t, "/api/v3/gists/"+id+"/"+sha, ownerToken), 200)
	requireStatus(t, s.get(t, "/api/v3/gists/"+id+"/forks", ownerToken), 200)
	requireStatus(t, s.get(t, "/api/v3/gists/"+id+"/comments", ownerToken), 200)
}

func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
