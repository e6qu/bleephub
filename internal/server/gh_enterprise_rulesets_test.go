package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestEnterpriseRulesetLifecycleHistoryAndRepositoryInheritance(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseRulesetRoutes()
	s.replaceClockNow(func() time.Time { return fixedTestTime })
	base := "/api/v3/enterprises/bleephub/rulesets"
	rec := enterpriseActionsRequest(t, s, http.MethodPost, base, map[string]interface{}{
		"name": "Enterprise branch policy", "target": "branch", "enforcement": "active",
		"conditions": map[string]interface{}{"ref_name": map[string]interface{}{
			"include": []string{"~ALL"}, "exclude": []string{},
		}},
		"rules": []map[string]interface{}{{"type": "deletion"}},
	})
	created := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || created["source_type"] != "Enterprise" ||
		created["source"] != "bleephub" || created["name"] != "Enterprise branch policy" {
		t.Fatalf("create enterprise ruleset = %d %#v", rec.Code, created)
	}
	id := int(created["id"].(float64))

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "enterprise-ruleset-org", "Ruleset Org", "")
	repo := s.store.CreateOrgRepo(org, admin, "inherited-policy", "", false)
	inherited := s.store.ListRulesetsForRepository(repo, true)
	if len(inherited) != 1 || inherited[0].ID != id || inherited[0].Enterprise != "bleephub" {
		t.Fatalf("repository inherited rulesets = %+v", inherited)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPut, base+"/"+strconv.Itoa(id), map[string]interface{}{
		"name": "Updated enterprise branch policy", "enforcement": "evaluate",
	})
	updated := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || updated["name"] != "Updated enterprise branch policy" ||
		updated["enforcement"] != "evaluate" {
		t.Fatalf("update enterprise ruleset = %d %#v", rec.Code, updated)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/"+strconv.Itoa(id)+"/history", nil)
	var history []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil || len(history) != 1 {
		t.Fatalf("enterprise ruleset history = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	versionID := int(history[0]["version_id"].(float64))
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		base+"/"+strconv.Itoa(id)+"/history/"+strconv.Itoa(versionID), nil)
	version := decodeRecorderObject(t, rec)
	state, _ := version["state"].(map[string]interface{})
	if rec.Code != http.StatusOK || state["name"] != "Enterprise branch policy" {
		t.Fatalf("enterprise ruleset version = %d %#v", rec.Code, version)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/"+strconv.Itoa(id), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete enterprise ruleset = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/"+strconv.Itoa(id), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted enterprise ruleset = %d %q, want 404", rec.Code, rec.Body.String())
	}
}
