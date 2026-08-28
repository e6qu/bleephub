package bleephub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
)

// App-level webhook config + deliveries — GET/PATCH /app/hook/config + the
// per-app /app/hook/deliveries listing surface, matching GitHub's
// installation-vs-app distinction.

func TestAppHookConfig_GetPatch(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHAppHookRoutes()
	app := s.store.CreateApp(1, "Hook Cfg App", "", nil, nil)

	jwt, err := signAppJWT(app.PEMPrivateKey, app.ID, fixedTestTime)
	if err != nil {
		t.Fatal(err)
	}
	doReq := func(method, path string, body []byte) *httptest.ResponseRecorder {
		var bodyR *bytes.Reader
		if body != nil {
			bodyR = bytes.NewReader(body)
		}
		var req *http.Request
		if bodyR != nil {
			req = httptest.NewRequest(method, path, bodyR)
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}

	// The secret is rendered redacted.
	w := doReq("GET", "/api/v3/app/hook/config", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d body = %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["secret"] != "********" {
		t.Errorf("expected redacted secret, got %v", got["secret"])
	}

	body, _ := json.Marshal(map[string]string{"url": "https://example.test/webhook", "secret": "new-secret"})
	w = doReq("PATCH", "/api/v3/app/hook/config", body)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body = %s", w.Code, w.Body.String())
	}
	if updated := s.store.GetApp(app.ID); updated.WebhookURL != "https://example.test/webhook" || updated.WebhookSecret != "new-secret" {
		t.Errorf("PATCH did not persist; url=%q secret=%q", updated.WebhookURL, updated.WebhookSecret)
	}
}

func TestAppHookDeliveries_ListGetRedeliver(t *testing.T) {
	// The sink handler runs on another goroutine while the body polls, so the
	// capture must be synchronized.
	var gotMu sync.Mutex
	var got []byte
	gotLen := func() int { gotMu.Lock(); defer gotMu.Unlock(); return len(got) }
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotMu.Lock()
		got = buf
		gotMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHAppsRoutes()
	s.registerGHAppHookRoutes()
	app := s.store.CreateApp(1, "Deliveries App", "", nil, nil)
	s.store.UpdateAppHookConfig(app.ID, func(a *store.App) {
		a.WebhookURL = sink.URL
		a.WebhookActive = true
	})

	// A prior delivery, as if it fired earlier.
	original := &store.WebhookDelivery{
		HookID:      -app.ID,
		AppID:       app.ID,
		Event:       "installation",
		Action:      "created",
		GUID:        "test-guid",
		StatusCode:  200,
		Duration:    0.01,
		Request:     &store.DeliveryRequest{Headers: map[string]string{"X-GitHub-Event": "installation"}, Payload: json.RawMessage(`{"action":"created"}`)},
		Response:    &store.DeliveryResponse{StatusCode: 200, Body: "ok"},
		DeliveredAt: fixedTestTime,
	}
	s.store.AddAppDelivery(app.ID, original)

	jwt, err := signAppJWT(app.PEMPrivateKey, app.ID, fixedTestTime)
	if err != nil {
		t.Fatal(err)
	}
	doReq := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}

	w := doReq("GET", "/api/v3/app/hook/deliveries")
	if w.Code != http.StatusOK {
		t.Fatalf("LIST status = %d body = %s", w.Code, w.Body.String())
	}
	var list []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(list))
	}
	// Summary carries throttled_at but NOT url.
	if _, ok := list[0]["throttled_at"]; !ok {
		t.Error("delivery summary missing throttled_at")
	}
	if _, ok := list[0]["url"]; ok {
		t.Error("delivery summary must NOT contain url")
	}

	// Single delivery carries the full request/response payload and url.
	w = doReq("GET", fmt.Sprintf("/api/v3/app/hook/deliveries/%d", original.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d body = %s", w.Code, w.Body.String())
	}
	var single map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &single)
	if single["request"] == nil || single["response"] == nil {
		t.Error("expected full request/response in single-delivery view")
	}
	if _, ok := single["url"]; !ok {
		t.Error("full delivery object must include url")
	}

	// Redeliver: 202 with an empty body (GitHub returns none).
	w = doReq("POST", fmt.Sprintf("/api/v3/app/hook/deliveries/%d/attempts", original.ID))
	if w.Code != http.StatusAccepted {
		t.Fatalf("REDELIVER status = %d body = %s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Errorf("REDELIVER body = %q, want empty", w.Body.String())
	}
	testutil.TestEventually(2*time.Second, 20*time.Millisecond, func() bool { return gotLen() > 0 })

	deliveries := s.store.ListAppDeliveries(app.ID)
	if len(deliveries) != 2 {
		t.Errorf("expected 2 deliveries after redeliver, got %d", len(deliveries))
	}
	foundRedelivery := false
	for _, d := range deliveries {
		if d.Redelivery {
			foundRedelivery = true
		}
	}
	if !foundRedelivery {
		t.Error("expected one delivery marked Redelivery=true")
	}
}
