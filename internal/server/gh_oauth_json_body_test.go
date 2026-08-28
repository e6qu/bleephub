package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// postOAuthJSON sends a JSON-bodied request to an OAuth endpoint, the way
// octokit's device-flow strategy does, and returns the decoded JSON response.
func postOAuthJSON(t *testing.T, s *Server, path string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	// octokit sends exactly these two headers: a JSON body, and JSON back.
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s response %q: %v", path, rec.Body.String(), err)
	}
	return out
}

// TestOAuth_DeviceFlow_AcceptsJSONRequestBody pins that the device-flow
// endpoints read parameters from a JSON body as well as a form-encoded one,
// across the whole grant. octokit sends JSON; reading only the form encoding
// made the grant unreachable — every parameter came back empty, so the request
// was refused as bad client credentials, a wrong answer rather than a missing
// feature.
func TestOAuth_DeviceFlow_AcceptsJSONRequestBody(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.store.Mu.Lock()
	alice := &store.User{ID: s.store.NextUser, Login: "json-device-alice", Type: "User"}
	s.store.NextUser++
	s.store.Users[alice.ID] = alice
	s.store.UsersByLogin[alice.Login] = alice
	s.store.Mu.Unlock()
	app := createOAuthTestApp(t, s, "http://json-device-callback/")
	s.registerGHOAuthRoutes()

	device := postOAuthJSON(t, s, "/login/device/code", map[string]interface{}{
		"client_id": app.ClientID,
		"scope":     "repo",
	})
	if device["error"] != nil {
		t.Fatalf("device code request errored: %v", device)
	}
	deviceCode, _ := device["device_code"].(string)
	userCode, _ := device["user_code"].(string)
	if deviceCode == "" || userCode == "" {
		t.Fatalf("device code response = %v, want device_code and user_code", device)
	}

	// Polling before approval must be the pending answer, not a credentials error.
	pending := postOAuthJSON(t, s, "/login/oauth/access_token", map[string]interface{}{
		"client_id":   app.ClientID,
		"device_code": deviceCode,
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
	})
	if pending["error"] != "authorization_pending" {
		t.Fatalf("polling before approval = %v, want authorization_pending", pending)
	}

	approveDeviceCode(t, s, alice.Login, userCode)
	granted := postOAuthJSON(t, s, "/login/oauth/access_token", map[string]interface{}{
		"client_id":   app.ClientID,
		"device_code": deviceCode,
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
	})
	if granted["error"] != nil {
		t.Fatalf("polling after approval = %v, want a token", granted)
	}
	token, _ := granted["access_token"].(string)
	if token == "" {
		t.Fatalf("granted response = %v, want access_token", granted)
	}
	// The token the JSON grant produced is a real credential bound to the
	// person who approved it, not merely a well-shaped response.
	if tok := s.store.UserToServerTokens[token]; tok == nil || tok.UserID != alice.ID {
		t.Fatalf("token from JSON grant = %+v, want a user-to-server token for %d", tok, alice.ID)
	}
}

// TestOAuth_TokenEndpoint_JSONBodyAndFormEncodingBothWork pins that adding the
// JSON path left the documented form encoding working, that a malformed JSON
// body is reported as malformed rather than as a credentials failure — the
// misreading that hid the original bug — and that an unrecognised grant is
// negotiated like every other body this endpoint returns.
func TestOAuth_TokenEndpoint_JSONBodyAndFormEncodingBothWork(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	app := createOAuthTestApp(t, s, "http://json-form-callback/")
	s.registerGHOAuthRoutes()

	// The form encoding still works, and both encodings reach the same store.
	deviceCode, _ := issueDeviceCode(t, s, app.ClientID, "repo")
	if deviceCode == "" {
		t.Fatal("form-encoded device code request returned no device_code")
	}

	// A body that claims to be JSON and is not says so, rather than being read
	// as a request with no parameters.
	req := httptest.NewRequest(http.MethodPost, "/login/device/code", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON body = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Problems parsing JSON") {
		t.Errorf("malformed JSON body message = %s, want a JSON parse error", rec.Body.String())
	}

	// A JSON body naming no grant is an unsupported grant type, negotiated to
	// JSON because the client asked for JSON.
	unsupported := postOAuthJSON(t, s, "/login/oauth/access_token", map[string]interface{}{
		"client_id": app.ClientID,
	})
	if unsupported["error"] != "unsupported_grant_type" {
		t.Errorf("JSON body with no grant = %v, want unsupported_grant_type", unsupported)
	}

	// Without an Accept preference the same answer is form-encoded, matching
	// the endpoint's documented default.
	formReq := httptest.NewRequest(http.MethodPost, "/login/oauth/access_token", strings.NewReader(""))
	formReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	formRec := httptest.NewRecorder()
	s.mux.ServeHTTP(formRec, formReq)
	if got := formRec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
		t.Errorf("unsupported grant without Accept: Content-Type = %q, want form-encoded", got)
	}
	if body := formRec.Body.String(); !strings.Contains(body, "unsupported_grant_type") {
		t.Errorf("unsupported grant body = %q, want unsupported_grant_type", body)
	}
	if got := formRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — the token endpoint's answers must not be cached", got)
	}
}
