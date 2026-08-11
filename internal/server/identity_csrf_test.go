package bleephub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestBrowserSessionEndpointsRejectCrossOriginPost guards the CSRF surface of
// every browser-session state-changing endpoint (WEB-039). Browsers attach
// Origin to every POST, so a present-but-foreign Origin is a cross-site
// request: login CSRF (a smuggled form signs the victim into an attacker's
// account — no victim cookie needed, so SameSite does not cover it) and
// forced logout (clearing cookies needs no cookie) must both be refused.
func TestBrowserSessionEndpointsRejectCrossOriginPost(t *testing.T) {
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	s.externalURL = "https://bleephub.example.test"

	endpoints := []struct {
		name    string
		handler http.HandlerFunc
		path    string
		body    string
	}{
		{"local login", s.handleLocalLogin, "/auth/local", `{"login":"admin","password":"x"}`},
		{"token login", s.handleTokenLogin, "/auth/token", ""},
		{"logout", s.handleIdentityLogout, "/auth/logout", ""},
		{"signed-out landing", s.handleIdentitySignedOut, "/ui/signed-out", ""},
	}
	// "null" is what a sandboxed iframe produces; it must count as foreign.
	for _, origin := range []string{"https://attacker.example", "null"} {
		for _, ep := range endpoints {
			var body *strings.Reader
			if ep.body != "" {
				body = strings.NewReader(ep.body)
			} else {
				body = strings.NewReader("")
			}
			request := httptest.NewRequest(http.MethodPost, ep.path, body)
			request.Header.Set("Origin", origin)
			if ep.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response := httptest.NewRecorder()
			ep.handler(response, request)
			if response.Code != http.StatusForbidden {
				t.Errorf("%s with Origin %q = %d, want 403", ep.name, origin, response.Code)
			}
		}
	}

	// A same-origin login must pass the CSRF gate and fail only on the
	// credential itself (401, not 403) — proving the check is origin-scoped,
	// not a blanket refusal.
	request := httptest.NewRequest(http.MethodPost, "/auth/local", strings.NewReader(`{"login":"nobody","password":"wrong"}`))
	request.Header.Set("Origin", s.externalURL)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.handleLocalLogin(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Errorf("same-origin login with bad credentials = %d, want 401", response.Code)
	}
}
