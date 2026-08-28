package bleephub

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawGet issues a GET with transport auto-decompression disabled, so the test
// observes the wire representation (Content-Encoding, raw bytes) instead of
// the Go client's transparently-decoded view.
func rawGet(t *testing.T, url, token string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

func gunzip(t *testing.T, compressed []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("response is not valid gzip: %v", err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return plain
}

// TestAPIGzipNegotiationAndConditional304 pins the end-to-end delivery contract
// for a large API JSON GET: identity vs gzip (decoding to identical bytes), an
// identity-bytes ETag either way, and a conditional re-request yielding an
// unbilled 304 (rate-limit unit refunded via X-RateLimit-Remaining).
func TestAPIGzipNegotiationAndConditional304(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "gzip-repo"}).Body.Close()
	path := s.baseURL + "/api/v3/repos/admin/gzip-repo"

	plain, plainBody := rawGet(t, path, defaultToken, nil)
	if plain.StatusCode != http.StatusOK {
		t.Fatalf("identity GET = %d, want 200", plain.StatusCode)
	}
	if ce := plain.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("identity GET Content-Encoding = %q, want none", ce)
	}
	if len(plainBody) < gzipMinResponseSize {
		t.Fatalf("repo JSON is %d bytes; test needs a body above the %d-byte gzip threshold", len(plainBody), gzipMinResponseSize)
	}
	etag := plain.Header.Get("ETag")
	if etag == "" {
		t.Fatal("identity GET carried no ETag")
	}

	zipped, zippedBody := rawGet(t, path, defaultToken, map[string]string{"Accept-Encoding": "gzip"})
	if got := zipped.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip GET Content-Encoding = %q, want gzip", got)
	}
	if !strings.Contains(zipped.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("gzip GET Vary = %q, want to contain Accept-Encoding", zipped.Header.Get("Vary"))
	}
	if len(zippedBody) >= len(plainBody) {
		t.Fatalf("gzip body (%d bytes) is not smaller than identity (%d bytes)", len(zippedBody), len(plainBody))
	}
	if !bytes.Equal(gunzip(t, zippedBody), plainBody) {
		t.Fatal("gunzipped body differs from identity body")
	}
	if got := zipped.Header.Get("ETag"); got != etag {
		t.Fatalf("gzip GET ETag = %q, want the identity ETag %q", got, etag)
	}
	remainingBefore := zipped.Header.Get("X-RateLimit-Remaining")

	cond, condBody := rawGet(t, path, defaultToken, map[string]string{
		"Accept-Encoding": "gzip",
		"If-None-Match":   etag,
	})
	if cond.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", cond.StatusCode)
	}
	if len(condBody) != 0 {
		t.Fatalf("304 carried a %d-byte body, want none", len(condBody))
	}
	if got := cond.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("304 Content-Encoding = %q, want none", got)
	}
	if got := cond.Header.Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}
	if got := cond.Header.Get("X-RateLimit-Remaining"); got != remainingBefore {
		t.Errorf("304 X-RateLimit-Remaining = %q, want %q (the 304 must be refunded)", got, remainingBefore)
	}
}

// TestAPIGzipSkipsSmallJSON pins the size threshold: a JSON response below
// gzipMinResponseSize goes out identity (with its exact Content-Length) even
// when the client accepts gzip, but still declares Vary for caches.
func TestAPIGzipSkipsSmallJSON(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp, body := rawGet(t, s.baseURL+"/api/v3/rate_limit", defaultToken, map[string]string{"Accept-Encoding": "gzip"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /rate_limit = %d, want 200", resp.StatusCode)
	}
	if len(body) >= gzipMinResponseSize {
		t.Fatalf("rate_limit JSON is %d bytes; test needs a body below the %d-byte threshold", len(body), gzipMinResponseSize)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("small JSON Content-Encoding = %q, want identity", ce)
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("small JSON Vary = %q, want to contain Accept-Encoding", resp.Header.Get("Vary"))
	}
}

// TestAPIETagOnlyOnSuccessfulGET pins that mutations and error responses are
// never ETagged: a validator on a 201 or a 404 body would invite conditional
// requests against representations that are not cacheable resources.
func TestAPIETagOnlyOnSuccessfulGET(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	created := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "etag-scope"})
	created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create repo = %d, want 201", created.StatusCode)
	}
	if etag := created.Header.Get("ETag"); etag != "" {
		t.Fatalf("POST 201 carried ETag %q, want none", etag)
	}

	missing, _ := rawGet(t, s.baseURL+"/api/v3/repos/admin/does-not-exist", defaultToken, nil)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("GET missing repo = %d, want 404", missing.StatusCode)
	}
	if etag := missing.Header.Get("ETag"); etag != "" {
		t.Fatalf("404 carried ETag %q, want none", etag)
	}
}

// TestCompressionMiddlewareBoundaries unit-tests the wrapper's refusal edges:
// non-GET, non-API paths, non-JSON media, pre-encoded responses, and — the one
// that guards streaming — a handler that flushes, which must be committed to
// identity so every increment reaches the client when pushed.
func TestCompressionMiddlewareBoundaries(t *testing.T) {
	t.Parallel()
	bigJSON := []byte(`{"data":"` + strings.Repeat("x", 4096) + `"}`)

	serve := func(method, path, acceptEncoding string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		if acceptEncoding != "" {
			req.Header.Set("Accept-Encoding", acceptEncoding)
		}
		compressionMiddleware(handler).ServeHTTP(rec, req)
		return rec
	}
	jsonHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bigJSON)
	}

	t.Run("large API JSON GET is gzipped", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v3/thing", "gzip", jsonHandler)
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		if got := rec.Header().Get("Content-Length"); got != "" {
			t.Fatalf("gzipped response declared identity Content-Length %q", got)
		}
		if !bytes.Equal(gunzip(t, rec.Body.Bytes()), bigJSON) {
			t.Fatal("gunzipped body differs from handler output")
		}
	})

	t.Run("ui-data prefix is covered", func(t *testing.T) {
		rec := serve(http.MethodGet, "/ui-data/thing", "gzip", jsonHandler)
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
	})

	t.Run("non-GET passes through", func(t *testing.T) {
		rec := serve(http.MethodPost, "/api/v3/thing", "gzip", jsonHandler)
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("POST Content-Encoding = %q, want none", got)
		}
		if !bytes.Equal(rec.Body.Bytes(), bigJSON) {
			t.Fatal("POST body altered by middleware")
		}
	})

	t.Run("non-API path passes through", func(t *testing.T) {
		rec := serve(http.MethodGet, "/internal/status", "gzip", jsonHandler)
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("non-API Content-Encoding = %q, want none", got)
		}
		if got := rec.Header().Get("Vary"); got != "" {
			t.Fatalf("non-API Vary = %q, want none", got)
		}
	})

	t.Run("q=0 refusal is honored", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v3/thing", "gzip;q=0", jsonHandler)
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("q=0 Content-Encoding = %q, want none", got)
		}
	})

	t.Run("flusher-dependent handler stays identity and flushes through", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v3/stream", "gzip", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tick":1}`))
			w.(http.Flusher).Flush()
			_, _ = w.Write(bigJSON) // past the threshold AFTER flushing: must stay identity
		})
		if !rec.Flushed {
			t.Fatal("Flush did not reach the underlying writer")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("streaming Content-Encoding = %q, want identity", got)
		}
		if want := append([]byte(`{"tick":1}`), bigJSON...); !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatal("streaming body altered by middleware")
		}
	})

	t.Run("event stream media stays identity", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v3/stream", "gzip", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("data: x\n\n"), 500))
			w.(http.Flusher).Flush()
		})
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("SSE Content-Encoding = %q, want identity", got)
		}
		if !rec.Flushed {
			t.Fatal("SSE Flush did not reach the underlying writer")
		}
	})

	t.Run("octet-stream stays identity", func(t *testing.T) {
		payload := bytes.Repeat([]byte{0x42}, 4096)
		rec := serve(http.MethodGet, "/api/v3/blob", "gzip", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(payload)
		})
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("octet-stream Content-Encoding = %q, want identity", got)
		}
		if !bytes.Equal(rec.Body.Bytes(), payload) {
			t.Fatal("octet-stream body altered by middleware")
		}
	})

	t.Run("pre-encoded response is never double-compressed", func(t *testing.T) {
		payload := bytes.Repeat([]byte("already-gzipped "), 300)
		rec := serve(http.MethodGet, "/api/v3/archive", "gzip", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(payload)
		})
		if !bytes.Equal(rec.Body.Bytes(), payload) {
			t.Fatal("pre-encoded body altered by middleware")
		}
	})

	t.Run("small JSON stays identity", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v3/tiny", "gzip", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("small JSON Content-Encoding = %q, want identity", got)
		}
		if rec.Body.String() != `{"ok":true}` {
			t.Fatalf("small JSON body = %q", rec.Body.String())
		}
	})

	t.Run("304 stays bodyless and unencoded", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v3/thing", "gzip", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"abc"`)
			w.WriteHeader(http.StatusNotModified)
		})
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("304 Content-Encoding = %q, want none", got)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("304 carried %d body bytes", rec.Body.Len())
		}
	})
}
