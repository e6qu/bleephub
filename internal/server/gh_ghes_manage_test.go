package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func manageRequest(t *testing.T, s *Server, method, path string, body interface{}, password string) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req.SetBasicAuth("api_key", password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestGHESManageSettingsMaintenanceAndConfigJourney(t *testing.T) {
	t.Setenv("BLEEPHUB_MANAGEMENT_PASSWORD", "management-secret")
	s := newTestServer()
	s.registerGHESManageRoutes()

	rec := manageRequest(t, s, http.MethodPut, "/manage/v1/config/settings",
		map[string]interface{}{"public_pages": true, "github_hostname": "github.example.test"}, "management-secret")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set settings = %d %q", rec.Code, rec.Body.String())
	}
	rec = manageRequest(t, s, http.MethodGet, "/manage/v1/config/settings", nil, "management-secret")
	settings := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || settings["public_pages"] != true ||
		settings["github_hostname"] != "github.example.test" {
		t.Fatalf("get settings = %d %#v", rec.Code, settings)
	}

	rec = manageRequest(t, s, http.MethodPost, "/manage/v1/maintenance",
		map[string]interface{}{"enabled": true, "maintenance_mode_message": "Upgrading"}, "management-secret")
	var maintenance []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &maintenance); rec.Code != http.StatusOK || err != nil ||
		len(maintenance) != 1 || maintenance[0]["status"] != "on" {
		t.Fatalf("set maintenance = %d %q: %v", rec.Code, rec.Body.String(), err)
	}

	rec = manageRequest(t, s, http.MethodPost, "/manage/v1/config/apply", nil, "management-secret")
	apply := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusAccepted || apply["status"] != "running" {
		t.Fatalf("start config apply = %d %#v", rec.Code, apply)
	}
	rec = manageRequest(t, s, http.MethodGet, "/manage/v1/config/apply", nil, "management-secret")
	if apply = decodeRecorderObject(t, rec); apply["status"] != "success" {
		t.Fatalf("config apply status = %d %#v", rec.Code, apply)
	}
	rec = manageRequest(t, s, http.MethodGet, "/manage/v1/config/apply/events", nil, "management-secret")
	if events := decodeRecorderObject(t, rec); len(events["nodes"].([]interface{})) != 1 {
		t.Fatalf("config events = %d %#v", rec.Code, events)
	}
}

func TestGHESManageSSHLicenseAndNodeReads(t *testing.T) {
	t.Setenv("BLEEPHUB_MANAGEMENT_PASSWORD", "management-secret")
	s := newTestServer()
	s.registerGHESManageRoutes()
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBleephub"
	rec := manageRequest(t, s, http.MethodPost, "/manage/v1/access/ssh",
		map[string]interface{}{"key": key}, "management-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("add SSH key = %d %q", rec.Code, rec.Body.String())
	}
	rec = manageRequest(t, s, http.MethodGet, "/manage/v1/access/ssh", nil, "management-secret")
	var keys []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil || len(keys) != 1 ||
		keys[0]["fingerprint"] == "" {
		t.Fatalf("list SSH keys = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	rec = manageRequest(t, s, http.MethodDelete, "/manage/v1/access/ssh",
		map[string]interface{}{"key": key}, "management-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete SSH key = %d", rec.Code)
	}

	rec = manageRequest(t, s, http.MethodPut, "/manage/v1/config/license",
		map[string]interface{}{"seats": 25, "perpetual": true}, "management-secret")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put license = %d %q", rec.Code, rec.Body.String())
	}
	rec = manageRequest(t, s, http.MethodGet, "/manage/v1/config/license", nil, "management-secret")
	if license := decodeRecorderObject(t, rec); license["seats"] != float64(25) {
		t.Fatalf("get license = %d %#v", rec.Code, license)
	}
	for _, path := range []string{
		"/manage/v1/checks/system-requirements", "/manage/v1/cluster/status",
		"/manage/v1/config/license/check", "/manage/v1/config/nodes",
		"/manage/v1/replication/status", "/manage/v1/version",
	} {
		rec = manageRequest(t, s, http.MethodGet, path, nil, "management-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %q", path, rec.Code, rec.Body.String())
		}
	}
	rec = manageRequest(t, s, http.MethodGet, "/manage/v1/version", nil, "wrong-secret")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("bad management password = %d headers=%v", rec.Code, rec.Header())
	}
}
