package bleephub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInFlightLimit covers the opt-in HTTP backpressure limiter: a normal
// request is shed with 503 + Retry-After when the slots are full, a
// byte-transfer route bypasses the cap, and a nil limiter (the default) never
// limits.
func TestInFlightLimit(t *testing.T) {
	t.Parallel()
	s := newTestServer()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	s.inFlightSlots = make(chan struct{}, 1)
	h := s.inFlightLimitMiddleware(ok)
	s.inFlightSlots <- struct{}{} // occupy the only slot

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v3/user", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated request = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 response missing Retry-After header")
	}

	// A git byte-transfer route bypasses the cap even when full.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/admin/demo.git/info/refs?service=git-upload-pack", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("exempt git route = %d, want 200", w2.Code)
	}

	// Disabled (the default) never limits.
	s.inFlightSlots = nil
	w3 := httptest.NewRecorder()
	s.inFlightLimitMiddleware(ok).ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/api/v3/user", nil))
	if w3.Code != http.StatusOK {
		t.Fatalf("disabled limiter = %d, want 200", w3.Code)
	}
}
