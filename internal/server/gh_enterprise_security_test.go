package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestEnterpriseSecretScanningPatternsAndReviewQueues(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseSecurityRoutes()
	s.replaceClockNow(func() time.Time { return fixedTestTime })
	base := "/api/v3/enterprises/bleephub/secret-scanning"
	rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/custom-patterns", map[string]interface{}{
		"patterns": []map[string]interface{}{{"name": "Enterprise token", "pattern": `ent_[0-9a-f]{16}`}},
	})
	created := decodeRecorderObject(t, rec)
	patterns, _ := created["created_patterns"].([]interface{})
	if rec.Code != http.StatusCreated || len(patterns) != 1 {
		t.Fatalf("create enterprise custom pattern = %d %#v", rec.Code, created)
	}
	pattern := patterns[0].(map[string]interface{})
	if pattern["created_at"] != fixedTestTime.Format(time.RFC3339) {
		t.Fatalf("enterprise custom pattern timestamp = %#v", pattern)
	}
	id := int(pattern["id"].(float64))
	version := pattern["custom_pattern_version"].(string)

	rec = enterpriseActionsRequest(t, s, http.MethodPatch,
		base+"/custom-patterns/"+strconv.Itoa(id), map[string]interface{}{
			"pattern": `ent_[0-9a-f]{24}`, "custom_pattern_version": version,
		})
	updated := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || updated["pattern"] != `ent_[0-9a-f]{24}` ||
		updated["custom_pattern_version"] == version {
		t.Fatalf("update enterprise custom pattern = %d %#v", rec.Code, updated)
	}

	config := decodeRecorderObject(t, enterpriseActionsRequest(t, s, http.MethodGet,
		base+"/pattern-configurations", nil))
	configVersion, _ := config["pattern_config_version"].(string)
	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base+"/pattern-configurations", map[string]interface{}{
		"pattern_config_version": configVersion,
		"provider_pattern_settings": []map[string]interface{}{{
			"token_type": "ghp", "push_protection_setting": "enabled",
		}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update enterprise pattern configuration = %d %q", rec.Code, rec.Body.String())
	}

	version = updated["custom_pattern_version"].(string)
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/custom-patterns", map[string]interface{}{
		"patterns": []map[string]interface{}{{
			"pattern_id": id, "custom_pattern_version": version,
		}},
		"post_delete_action": "resolve_alerts",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete enterprise custom pattern = %d %q", rec.Code, rec.Body.String())
	}

	for _, path := range []string{
		"/api/v3/enterprises/bleephub/bypass-requests/push-rules",
		"/api/v3/enterprises/bleephub/bypass-requests/secret-scanning",
		"/api/v3/enterprises/bleephub/dismissal-requests/secret-scanning",
		"/api/v3/enterprises/bleephub/code-scanning/alerts",
		"/api/v3/enterprises/bleephub/secret-scanning/alerts",
	} {
		rec = enterpriseActionsRequest(t, s, http.MethodGet, path, nil)
		var rows []interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %q: %v", path, rec.Code, rec.Body.String(), err)
		}
	}
}
