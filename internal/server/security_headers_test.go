package bleephub

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersMiddleware(t *testing.T) {
	s := &Server{}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.securityHeadersMiddleware(ok)

	// The /ui/ SPA gets nosniff + framing lock + CSP.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("/ui/ X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("/ui/ X-Frame-Options = %q, want DENY", got)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("/ui/ CSP missing/weak: %q", csp)
	}
	if rec.Header().Get("Referrer-Policy") == "" {
		t.Error("/ui/ Referrer-Policy missing")
	}

	// The JSON API gets nosniff but NOT the SPA CSP / framing lock.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/v3/user", nil))
	if got := rec2.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("/api X-Content-Type-Options = %q, want nosniff", got)
	}
	if csp := rec2.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("SPA CSP leaked onto the API: %q", csp)
	}
	if xfo := rec2.Header().Get("X-Frame-Options"); xfo != "" {
		t.Errorf("SPA X-Frame-Options leaked onto the API: %q", xfo)
	}

	// A handler that sets a stricter CSP wins (middleware must not clobber it).
	strict := s.securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.WriteHeader(http.StatusOK)
	}))
	rec3 := httptest.NewRecorder()
	strict.ServeHTTP(rec3, httptest.NewRequest("GET", "/ui/x", nil))
	if got := rec3.Header().Get("Content-Security-Policy"); got != "default-src 'none'" {
		t.Errorf("handler CSP was clobbered: %q", got)
	}

	// HSTS is set only over TLS.
	recPlain := httptest.NewRecorder()
	h.ServeHTTP(recPlain, httptest.NewRequest("GET", "/ui/", nil))
	if recPlain.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS set over plaintext")
	}
	recTLS := httptest.NewRecorder()
	reqTLS := httptest.NewRequest("GET", "https://example/ui/", nil)
	reqTLS.TLS = &tls.ConnectionState{}
	h.ServeHTTP(recTLS, reqTLS)
	if recTLS.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS missing over TLS")
	}
}
