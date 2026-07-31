package bleephub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessChecksPersistence(t *testing.T) {
	server := newTestServer()
	request := httptest.NewRequest(http.MethodGet, "/ready", nil)

	healthy := httptest.NewRecorder()
	server.handleReady(healthy, request)
	if healthy.Code != http.StatusOK {
		t.Fatalf("in-memory readiness = %d: %s", healthy.Code, healthy.Body.String())
	}

	server.store.mu.Lock()
	server.store.persistenceRecoveryRequired = true
	server.store.mu.Unlock()
	unavailable := httptest.NewRecorder()
	server.handleReady(unavailable, request)
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("poisoned-store readiness = %d: %s", unavailable.Code, unavailable.Body.String())
	}
}
