//go:build !noui

package bleephub

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// deliveryTestFS builds a dist-shaped tree with bodies large enough to cross
// the uiGzipMinSize pre-compression threshold.
func deliveryTestFS() (fstest.MapFS, []byte, []byte) {
	shell := []byte("<!doctype html><title>spa</title>" + strings.Repeat("<div>shell shell shell</div>", 40))
	entryJS := []byte(strings.Repeat("console.log('entry chunk payload');\n", 100))
	return fstest.MapFS{
		"index.html":               {Data: shell},
		"assets/app-B3xY12Zk.js":   {Data: entryJS},
		"assets/logo-Ab12Cd34.png": {Data: bytes.Repeat([]byte{0x89, 0x50, 0x4e, 0x47}, 300)},
		"favicon.svg":              {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
	}, shell, entryJS
}

func spaGet(t *testing.T, h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestUIHashedAssetImmutableAndGzip pins asset delivery: everything under
// /ui/assets/ is content-hashed by the build and carries the immutable cache
// header (matching the emoji asset route), text-shaped assets are served from
// the pre-compressed cache when the client accepts gzip (with an exact
// Content-Length), and clients without gzip get the identity bytes.
func TestUIHashedAssetImmutableAndGzip(t *testing.T) {
	t.Parallel()
	fsys, _, entryJS := deliveryTestFS()
	h := spaHandler(fsys, "/ui/")

	// Identity: no Accept-Encoding.
	rec := spaGet(t, h, "/ui/assets/app-B3xY12Zk.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("identity asset GET = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != uiImmutableCacheControl {
		t.Fatalf("asset Cache-Control = %q, want %q", got, uiImmutableCacheControl)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("identity asset Content-Encoding = %q, want none", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("asset Vary = %q, want to contain Accept-Encoding", rec.Header().Get("Vary"))
	}
	if !bytes.Equal(rec.Body.Bytes(), entryJS) {
		t.Fatal("identity asset body differs from source")
	}

	// Negotiated gzip: smaller on the wire, exact Content-Length, same bytes
	// after decode, immutable cache header intact.
	rec = spaGet(t, h, "/ui/assets/app-B3xY12Zk.js", map[string]string{"Accept-Encoding": "gzip"})
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip asset Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != uiImmutableCacheControl {
		t.Fatalf("gzip asset Cache-Control = %q, want %q", got, uiImmutableCacheControl)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("gzip asset Content-Length = %q, body is %d bytes", got, rec.Body.Len())
	}
	if rec.Body.Len() >= len(entryJS) {
		t.Fatalf("gzip asset (%d bytes) not smaller than identity (%d bytes)", rec.Body.Len(), len(entryJS))
	}
	if !bytes.Equal(gunzip(t, rec.Body.Bytes()), entryJS) {
		t.Fatal("gunzipped asset differs from source")
	}

	// A binary asset is still immutable but never compressed.
	rec = spaGet(t, h, "/ui/assets/logo-Ab12Cd34.png", map[string]string{"Accept-Encoding": "gzip"})
	if got := rec.Header().Get("Cache-Control"); got != uiImmutableCacheControl {
		t.Fatalf("png Cache-Control = %q, want %q", got, uiImmutableCacheControl)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("png Content-Encoding = %q, want identity", got)
	}

	// A non-hashed top-level file gets no immutable header.
	rec = spaGet(t, h, "/ui/favicon.svg", nil)
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("favicon Cache-Control = %q, want none", got)
	}
}

// TestUIShellETagNoCacheAnd304 pins shell delivery: index.html (served for
// every SPA route) is revalidate-always (Cache-Control: no-cache) with a
// strong startup-computed ETag, a matching If-None-Match yields a bodyless
// 304, and gzip is negotiated for the document itself.
func TestUIShellETagNoCacheAnd304(t *testing.T) {
	t.Parallel()
	fsys, shell, _ := deliveryTestFS()
	h := spaHandler(fsys, "/ui/")

	// The SPA fallback route serves the shell with the caching contract.
	rec := spaGet(t, h, "/ui/repos/octo/hello/pulls/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("SPA route = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("shell Cache-Control = %q, want no-cache", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("shell carried no ETag")
	}
	if !bytes.Equal(rec.Body.Bytes(), shell) {
		t.Fatal("shell body differs from index.html")
	}

	// The bare prefix and the explicit filename serve the same validator.
	for _, path := range []string{"/ui/", "/ui/index.html"} {
		if got := spaGet(t, h, path, nil).Header().Get("ETag"); got != etag {
			t.Fatalf("%s ETag = %q, want %q", path, got, etag)
		}
	}

	// A matching If-None-Match revalidates with a bodyless 304.
	rec = spaGet(t, h, "/ui/", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional shell GET = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 carried %d body bytes, want none", rec.Body.Len())
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}

	// A stale validator gets the full document.
	rec = spaGet(t, h, "/ui/", map[string]string{"If-None-Match": `"stale"`})
	if rec.Code != http.StatusOK {
		t.Fatalf("stale conditional GET = %d, want 200", rec.Code)
	}

	// The shell itself negotiates gzip.
	rec = spaGet(t, h, "/ui/repos/octo/hello", map[string]string{"Accept-Encoding": "gzip"})
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip shell Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("gzip shell Content-Length = %q, body is %d bytes", got, rec.Body.Len())
	}
	if !bytes.Equal(gunzip(t, rec.Body.Bytes()), shell) {
		t.Fatal("gunzipped shell differs from index.html")
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("gzip shell ETag = %q, want identity-bytes ETag %q", got, etag)
	}
}
