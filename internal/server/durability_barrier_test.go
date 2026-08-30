package bleephub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func groupCommitServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	t.Setenv("BLEEPHUB_GROUP_COMMIT", "true")
	p, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	if !p.GroupCommitActive() {
		t.Fatal("expected group commit active")
	}
	s := newTestServer()
	if err := s.store.SetPersistence(p); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return s
}

// TestDurabilityBarrierGatesAndPreservesResponse checks that a mutating request
// whose handler enqueues a write is held until durable, and that the buffered
// response reaches the client byte-for-byte with a correct Content-Length.
func TestDurabilityBarrierGatesAndPreservesResponse(t *testing.T) {
	s := groupCommitServer(t)
	body := `{"ok":true,"n":42}`
	h := s.durabilityBarrierMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.store.Persist.MustPut("kv-barrier", "k", map[string]int{"n": 42})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v3/x", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if rec.Body.String() != body {
		t.Fatalf("body = %q, want %q", rec.Body.String(), body)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}
	// The write the handler enqueued must be durable by the time the response was
	// flushed (the barrier waited before returning).
	if err := s.store.Persist.WaitDurable(context.Background(), 0); err != nil {
		t.Fatalf("wait durable: %v", err)
	}
	if raw, err := s.store.Persist.Get("kv-barrier", "k"); err != nil || raw == nil {
		t.Fatalf("write not durable after barrier: raw=%v err=%v", raw, err)
	}
}

// TestDurabilityBarrierIgnoresReads confirms GETs are neither buffered nor gated
// (they enqueue nothing), so the response passes straight through.
func TestDurabilityBarrierIgnoresReads(t *testing.T) {
	s := groupCommitServer(t)
	streamed := false
	h := s.durabilityBarrierMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(*durabilityWriter); ok {
			streamed = true // a GET must not be wrapped
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("read"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v3/x", nil))
	if streamed {
		t.Fatal("GET was wrapped by the durability writer; reads should pass through")
	}
	if rec.Body.String() != "read" || rec.Code != http.StatusOK {
		t.Fatalf("unexpected read response: %d %q", rec.Code, rec.Body.String())
	}
}

// TestDurabilityBarrierNoContent checks a 204 mutating response flushes with no
// body and no Content-Length.
func TestDurabilityBarrierNoContent(t *testing.T) {
	s := groupCommitServer(t)
	h := s.durabilityBarrierMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.store.Persist.MustPut("kv-barrier", "d", 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v3/x", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 had body %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("204 set Content-Length %q", got)
	}
}
