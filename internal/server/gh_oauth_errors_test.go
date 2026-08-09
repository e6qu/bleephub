package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// mintOAuthWebFlowCode runs the consent web flow as admin and returns a valid
// one-time authorization code bound to app.ClientID and redirect_uri "http://cb/".
func mintOAuthWebFlowCode(t *testing.T, s *Server, app *OAuthApp) string {
	t.Helper()
	jar := doLogin(t, s, "admin")
	authorizeURL := "/login/oauth/authorize?client_id=" + url.QueryEscape(app.ClientID) + "&redirect_uri=http://cb/&scope=repo&state=S"
	w := requestWithJar(s, "GET", authorizeURL, "", "", jar)
	if w.Code != http.StatusOK {
		t.Fatalf("GET authorize = %d", w.Code)
	}
	form := url.Values{}
	form.Set("authenticity_token", extractCSRF(t, w.Body.String()))
	form.Set("client_id", app.ClientID)
	form.Set("redirect_uri", "http://cb/")
	form.Set("scope", "repo")
	form.Set("state", "S")
	w2 := requestWithJar(s, "POST", "/login/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", jar)
	if w2.Code != http.StatusFound {
		t.Fatalf("POST authorize = %d (%s)", w2.Code, w2.Body.String())
	}
	loc, _ := url.Parse(w2.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %s", w2.Header().Get("Location"))
	}
	return code
}

// exchangeForError POSTs a token-endpoint form and returns the `error` field of
// the response body (empty when the exchange succeeded).
func exchangeForError(t *testing.T, s *Server, form url.Values) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var body struct {
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode token response %q: %v", w.Body.String(), err)
	}
	return body.Error
}

// TestOAuthTokenErrorBranches pins TEST-029's remaining OAuth error coverage:
// every documented failure of the token endpoint returns its GitHub error code.
func TestOAuthTokenErrorBranches(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	app := createOAuthTestApp(t, s, "http://cb/")
	s.registerGHOAuthRoutes()

	t.Run("no grant is unsupported_grant_type", func(t *testing.T) {
		if got := exchangeForError(t, s, url.Values{"client_id": {app.ClientID}}); got != "unsupported_grant_type" {
			t.Fatalf("error = %q, want unsupported_grant_type", got)
		}
	})

	t.Run("web flow unknown code is bad_verification_code", func(t *testing.T) {
		form := url.Values{"code": {"nope"}, "client_id": {app.ClientID}, "client_secret": {app.ClientSecret}}
		if got := exchangeForError(t, s, form); got != "bad_verification_code" {
			t.Fatalf("error = %q, want bad_verification_code", got)
		}
	})

	t.Run("web flow wrong client_id is incorrect_client_credentials", func(t *testing.T) {
		code := mintOAuthWebFlowCode(t, s, app)
		form := url.Values{"code": {code}, "client_id": {"Iv1.wrongwrongwrong"}, "client_secret": {app.ClientSecret}}
		if got := exchangeForError(t, s, form); got != "incorrect_client_credentials" {
			t.Fatalf("error = %q, want incorrect_client_credentials", got)
		}
	})

	t.Run("web flow redirect_uri mismatch is redirect_uri_mismatch", func(t *testing.T) {
		code := mintOAuthWebFlowCode(t, s, app)
		form := url.Values{"code": {code}, "client_id": {app.ClientID}, "client_secret": {app.ClientSecret}, "redirect_uri": {"http://evil/"}}
		if got := exchangeForError(t, s, form); got != "redirect_uri_mismatch" {
			t.Fatalf("error = %q, want redirect_uri_mismatch", got)
		}
	})

	t.Run("web flow wrong secret is incorrect_client_credentials", func(t *testing.T) {
		code := mintOAuthWebFlowCode(t, s, app)
		form := url.Values{"code": {code}, "client_id": {app.ClientID}, "client_secret": {"wrong-secret"}}
		if got := exchangeForError(t, s, form); got != "incorrect_client_credentials" {
			t.Fatalf("error = %q, want incorrect_client_credentials", got)
		}
	})

	t.Run("device flow unknown code is bad_verification_code", func(t *testing.T) {
		form := url.Values{
			"client_id":   {app.ClientID},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {"nope"},
		}
		if got := exchangeForError(t, s, form); got != "bad_verification_code" {
			t.Fatalf("error = %q, want bad_verification_code", got)
		}
	})

	t.Run("device flow before approval is authorization_pending", func(t *testing.T) {
		deviceCode, _ := issueDeviceCode(t, s, app.ClientID, "repo")
		if got := exchangeForError(t, s, url.Values{
			"client_id":   {app.ClientID},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {deviceCode},
		}); got != "authorization_pending" {
			t.Fatalf("error = %q, want authorization_pending", got)
		}
	})

	t.Run("device flow wrong client_id is incorrect_client_credentials", func(t *testing.T) {
		deviceCode, _ := issueDeviceCode(t, s, app.ClientID, "repo")
		if got := exchangeForError(t, s, url.Values{
			"client_id":   {"Iv1.someoneelse00"},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {deviceCode},
		}); got != "incorrect_client_credentials" {
			t.Fatalf("error = %q, want incorrect_client_credentials", got)
		}
	})
}
