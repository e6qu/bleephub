package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

// TestBuildIdentityIsAuthenticatedNotOnHealth covers CORE-016: the anonymous
// /health liveness probe must not disclose build identity or the tenant slug,
// and that identity is instead served from the site-admin-gated internal status
// endpoint.
func TestBuildIdentityIsAuthenticatedNotOnHealth(t *testing.T) {
	build := BuildInfo{
		Version:     "0123456789ab",
		Commit:      "0123456789abcdef0123456789abcdef01234567",
		PublishedAt: "2026-07-20T01:02:03Z",
	}
	server := NewServer("127.0.0.1:0", zerolog.Nop(), WithBuildInfo(build))

	// /health is reachable anonymously — it must leak nothing beyond liveness.
	healthResp := httptest.NewRecorder()
	server.handleHealth(healthResp, httptest.NewRequest(http.MethodGet, "/health", nil))
	if healthResp.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", healthResp.Code, http.StatusOK)
	}
	var health map[string]any
	if err := json.Unmarshal(healthResp.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	for _, leaked := range []string{"version", "commit", "published_at", "enterprise_slug"} {
		if _, present := health[leaked]; present {
			t.Errorf("CORE-016: /health discloses %q to anonymous callers", leaked)
		}
	}

	// The authenticated internal status endpoint carries the build identity.
	statusResp := httptest.NewRecorder()
	server.handleInternalStatus(statusResp, httptest.NewRequest(http.MethodGet, "/internal/status", nil))
	var status map[string]any
	if err := json.Unmarshal(statusResp.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode internal status response: %v", err)
	}
	for key, want := range map[string]string{
		"version":      build.Version,
		"commit":       build.Commit,
		"published_at": build.PublishedAt,
	} {
		if got := status[key]; got != want {
			t.Errorf("internal status %s = %#v, want %q", key, got, want)
		}
	}
}
