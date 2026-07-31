package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func enterpriseBearerRequest(
	t *testing.T,
	s *Server,
	method string,
	path string,
	body map[string]interface{},
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(rec, req)
	return rec
}

func TestEnterpriseLicensingJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseLicensingRoutes()
	base := "/api/v3/enterprises/bleephub"

	rec := enterpriseActionsRequest(t, s, http.MethodGet, base+"/consumed-licenses", nil)
	licenses := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || licenses["total_seats_consumed"] != float64(1) {
		t.Fatalf("consumed licenses = %d %#v", rec.Code, licenses)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/license-sync-status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("license sync status = %d %q", rec.Code, rec.Body.String())
	}

	subscriptionID := "00000000-0000-0000-0000-000000000001"
	rec = enterpriseActionsRequest(t, s, http.MethodPut,
		base+"/visual-studio-subscriptions/"+subscriptionID,
		map[string]interface{}{"user_identifier": "admin"})
	matched := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || matched["subscription_id"] != subscriptionID ||
		matched["username"] != "admin" || matched["manual_match"] != true {
		t.Fatalf("match subscription = %d %#v", rec.Code, matched)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/visual-studio-subscriptions", nil)
	list := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || list["total_count"] != float64(1) {
		t.Fatalf("list subscriptions = %d %#v", rec.Code, list)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete,
		base+"/visual-studio-subscriptions/"+subscriptionID, nil)
	unmatched := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || unmatched["username"] != nil || unmatched["manual_match"] != false {
		t.Fatalf("unmatch subscription = %d %#v", rec.Code, unmatched)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/enterprise-installation/bleephub/server-statistics", nil)
	var stats []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil || len(stats) != 1 ||
		stats[0]["collection_date"] != fixedTestTime.Format("2006-01-02T15:04:05Z07:00") {
		t.Fatalf("server statistics = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
}

func TestEnterpriseInnerSourceSyncAndEnterpriseInstallation(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseLicensingRoutes()
	admin := s.store.LookupUserByLogin("admin")
	app, err := s.store.CreateAppE(admin.ID, "InnerSource App", "", map[string]string{
		"enterprise_innersource_vulnerabilities": "write",
	}, nil)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	installation := s.store.CreateInstallation(app.ID, "Enterprise", 1, "bleephub", app.Permissions, nil)
	token := s.store.CreateInstallationToken(installation.ID, app.ID, installation.Permissions, nil)
	base := "/api/v3/enterprises/bleephub"

	rec := enterpriseBearerRequest(t, s, http.MethodPost,
		base+"/innersource-vulnerabilities/sync", map[string]interface{}{
			"vulnerabilities": []map[string]interface{}{
				{"id": "MVS-2026-001", "summary": "one"},
				{"id": "MVS-2026-002", "withdrawn": "2042-07-15T12:00:00Z"},
			},
		}, token.Token)
	queued := decodeRecorderObject(t, rec)
	jobID, _ := queued["id"].(string)
	if rec.Code != http.StatusAccepted || jobID == "" ||
		rec.Header().Get("Location") != queued["url"] {
		t.Fatalf("queue sync = %d %#v headers=%v", rec.Code, queued, rec.Header())
	}
	statusPath := base + "/innersource-vulnerabilities/sync/status/" + jobID
	rec = enterpriseBearerRequest(t, s, http.MethodGet, statusPath, nil, token.Token)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("pending sync = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseBearerRequest(t, s, http.MethodGet, statusPath, nil, token.Token)
	completed := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || completed["processed"] != float64(2) ||
		completed["created"] != float64(1) || completed["withdrawn"] != float64(1) {
		t.Fatalf("completed sync = %d %#v", rec.Code, completed)
	}

	jwt, err := signAppJWT(app.PEMPrivateKey, app.ID, fixedTestTime)
	if err != nil {
		t.Fatalf("sign app JWT: %v", err)
	}
	rec = enterpriseBearerRequest(t, s, http.MethodGet, base+"/installation", nil, jwt)
	found := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || found["id"] != float64(installation.ID) ||
		found["target_type"] != "Enterprise" {
		t.Fatalf("enterprise installation = %d %#v", rec.Code, found)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost,
		base+"/innersource-vulnerabilities/sync", map[string]interface{}{
			"vulnerabilities": []map[string]interface{}{{"id": "MVS-unauthorized"}},
		})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "integration") {
		t.Fatalf("PAT inner source sync = %d %q", rec.Code, rec.Body.String())
	}
}
