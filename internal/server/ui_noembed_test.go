//go:build noui

package bleephub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNoUIRootReturns404NotRedirect covers CORE-012: under -tags noui nothing
// serves /ui/, so the bare root must return an honest 404 rather than a 307
// redirect into an unserved /ui/ (a route the server would otherwise advertise
// but cannot fulfill).
func TestNoUIRootReturns404NotRedirect(t *testing.T) {
	s := newTestServer()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code == http.StatusTemporaryRedirect {
		t.Fatalf("CORE-012: noui root redirected (status %d, location %q) instead of returning 404",
			rec.Code, rec.Header().Get("Location"))
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("noui root status = %d, want 404", rec.Code)
	}
}
