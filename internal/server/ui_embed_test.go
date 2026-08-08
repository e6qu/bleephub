//go:build !noui

package bleephub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// TestSPAHandlerServesAssetsAndFallsBackToIndex pins TEST-021: the SPA handler
// (compiled out of the `noui` test build, and previously untested by anything)
// serves a real asset when the path exists and otherwise falls back to
// index.html — the client-side-routing contract every SPA route depends on.
// It runs only in the embedded (`!noui`) build; a dedicated CI step exercises it
// without the noui tag.
func TestSPAHandlerServesAssetsAndFallsBackToIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>spa</title>")},
		"assets/app.js": {Data: []byte("console.log('app')")},
		"favicon.ico":   {Data: []byte("icon-bytes")},
	}
	h := spaHandler(fsys, "/ui/")

	cases := []struct {
		name     string
		path     string
		wantBody string
	}{
		{"real nested asset served verbatim", "/ui/assets/app.js", "console.log('app')"},
		{"real top-level asset served verbatim", "/ui/favicon.ico", "icon-bytes"},
		{"unknown deep route falls back to index (client routing)", "/ui/repos/octo/hello/pulls/1", "<!doctype html><title>spa</title>"},
		{"bare prefix serves index", "/ui/", "<!doctype html><title>spa</title>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200", tc.path, rec.Code)
			}
			body, _ := io.ReadAll(rec.Body)
			if string(body) != tc.wantBody {
				t.Fatalf("%s body = %q, want %q", tc.path, body, tc.wantBody)
			}
		})
	}
}

// TestSPAHandlerWithoutIndexIs404 pins the degenerate case: with no index.html
// to fall back to, an unknown path is a clean 404 rather than a panic or a
// zero-byte 200.
func TestSPAHandlerWithoutIndexIs404(t *testing.T) {
	h := spaHandler(fstest.MapFS{"assets/app.js": {Data: []byte("x")}}, "/ui/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/no/such/route", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing-index fallback = %d, want 404", rec.Code)
	}
}
