package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

func TestHealthExposesImmutableBuildIdentity(t *testing.T) {
	build := BuildInfo{
		Version:     "0123456789ab",
		Commit:      "0123456789abcdef0123456789abcdef01234567",
		PublishedAt: "2026-07-20T01:02:03Z",
	}
	server := NewServer("127.0.0.1:0", zerolog.Nop(), WithBuildInfo(build))
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()
	server.handleHealth(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusOK)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	for key, want := range map[string]string{
		"version":      build.Version,
		"commit":       build.Commit,
		"published_at": build.PublishedAt,
	} {
		if got := payload[key]; got != want {
			t.Errorf("health %s = %#v, want %q", key, got, want)
		}
	}
}
