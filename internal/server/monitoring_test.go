package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

const monitoringTestToken = "bleephub-monitoring-token-00000000000000000000"

func TestMonitoringDigestRejectsWeakOrAmbiguousTokens(t *testing.T) {
	for _, token := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 32) + " ", strings.Repeat("a", 32) + "\n"} {
		if _, err := monitoringDigest(token); err == nil {
			t.Fatalf("monitoringDigest(%q) succeeded, want error", token)
		}
	}
	if _, err := monitoringDigest(monitoringTestToken); err != nil {
		t.Fatalf("valid monitoring token rejected: %v", err)
	}
}

func TestMonitoringObservationRequiresExactBearerAndPublishesApplicationContract(t *testing.T) {
	server := newServerState(":0", zerolog.Nop(), serverConstruction{})
	digest, err := monitoringDigest(monitoringTestToken)
	if err != nil {
		t.Fatal(err)
	}
	server.monitoringTokenDigest = &digest

	for _, authorization := range []string{"", "bearer " + monitoringTestToken, "Bearer wrong-monitoring-token-00000000000000000000"} {
		request := httptest.NewRequest(http.MethodGet, "/monitoring/observation", http.NoBody)
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		server.handleMonitoringObservation(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %q status = %d, want %d", authorization, response.Code, http.StatusUnauthorized)
		}
		if response.Header().Get("WWW-Authenticate") != `Bearer realm="bleephub-monitoring"` {
			t.Fatalf("authorization challenge = %q", response.Header().Get("WWW-Authenticate"))
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unauthorized Cache-Control = %q", response.Header().Get("Cache-Control"))
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/monitoring/observation", http.NoBody)
	request.Header.Set("Authorization", "Bearer "+monitoringTestToken)
	response := httptest.NewRecorder()
	server.handleMonitoringObservation(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers Content-Type=%q Cache-Control=%q", response.Header().Get("Content-Type"), response.Header().Get("Cache-Control"))
	}
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document["schema_version"] != monitoringSchemaVersion {
		t.Fatalf("schema_version = %v", document["schema_version"])
	}
	if _, present := document["cost_estimate"]; present {
		t.Fatal("application observation fabricated a cost estimate")
	}
	resources, ok := document["resources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("resources = %#v", document["resources"])
	}
	resource := resources[0].(map[string]any)
	if resource["health"] != "healthy" || resource["kind"] != "application" {
		t.Fatalf("resource = %#v", resource)
	}
	metrics := resource["metrics"].([]any)
	if len(metrics) != 7 {
		t.Fatalf("metric count = %d, want 7", len(metrics))
	}
}

func TestMonitoringEnvironmentDropsPlaintextAfterValidation(t *testing.T) {
	t.Setenv("BLEEPHUB_MONITORING_TOKEN", monitoringTestToken)
	option, err := MonitoringTokenFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	option(server)
	if server.monitoringTokenDigest == nil {
		t.Fatal("monitoring token digest was not configured")
	}
	if got := string(server.monitoringTokenDigest[:]); strings.Contains(got, monitoringTestToken) {
		t.Fatal("monitoring token plaintext retained")
	}
}
