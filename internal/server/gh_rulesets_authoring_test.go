package bleephub

import (
	"net/http"
	"strconv"
	"testing"
)

// The web ruleset editor authors conditions, per-rule parameters, and a bypass
// list for both repo and org rulesets. This pins the on-the-wire round-trip the
// editor depends on — in particular that org CREATE persists bypass_actors,
// which it previously dropped.
func TestRulesetAuthoring_RoundTripsConditionsParamsAndBypass(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createOrg(t, "rules-org")

	body := map[string]interface{}{
		"name":        "protect-main",
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]interface{}{
			"ref_name": map[string]interface{}{
				"include": []string{"~DEFAULT_BRANCH", "refs/heads/release/*"},
				"exclude": []string{"refs/heads/tmp/*"},
			},
		},
		"rules": []map[string]interface{}{
			{"type": "deletion"},
			{"type": "pull_request", "parameters": map[string]interface{}{
				"required_approving_review_count":   2,
				"dismiss_stale_reviews_on_push":     true,
				"require_code_owner_review":         true,
				"require_last_push_approval":        false,
				"required_review_thread_resolution": true,
			}},
			{"type": "required_status_checks", "parameters": map[string]interface{}{
				"required_status_checks":               []map[string]interface{}{{"context": "build"}},
				"strict_required_status_checks_policy": false,
			}},
			{"type": "branch_name_pattern", "parameters": map[string]interface{}{
				"operator": "starts_with", "pattern": "feature/", "negate": false,
			}},
		},
		"bypass_actors": []map[string]interface{}{
			{"actor_id": 5, "actor_type": "User", "bypass_mode": "always"},
		},
	}

	resp := s.post(t, "/api/v3/orgs/rules-org/rulesets", defaultToken, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create org ruleset = %d, want 201", resp.StatusCode)
	}
	created := decodeJSON(t, resp)
	id := int(created["id"].(float64))

	// Re-fetch so we assert what the store persisted, not just the create echo.
	got := decodeJSON(t, s.get(t, "/api/v3/orgs/rules-org/rulesets/"+strconv.Itoa(id), defaultToken))

	// bypass_actors must survive org create (the regression this guards).
	actors, ok := got["bypass_actors"].([]interface{})
	if !ok || len(actors) != 1 {
		t.Fatalf("bypass_actors = %v, want one entry", got["bypass_actors"])
	}
	if a := actors[0].(map[string]interface{}); a["actor_id"].(float64) != 5 || a["bypass_mode"] != "always" {
		t.Errorf("bypass actor = %v", a)
	}

	// conditions.ref_name round-trips both include and exclude.
	cond := got["conditions"].(map[string]interface{})["ref_name"].(map[string]interface{})
	if inc := cond["include"].([]interface{}); len(inc) != 2 || inc[0] != "~DEFAULT_BRANCH" {
		t.Errorf("include = %v", cond["include"])
	}
	if exc := cond["exclude"].([]interface{}); len(exc) != 1 || exc[0] != "refs/heads/tmp/*" {
		t.Errorf("exclude = %v", cond["exclude"])
	}

	// rule parameters round-trip (pull_request count, status-check context).
	rules := got["rules"].([]interface{})
	if len(rules) != 4 {
		t.Fatalf("rules = %d, want 4", len(rules))
	}
	for _, raw := range rules {
		rule := raw.(map[string]interface{})
		switch rule["type"] {
		case "pull_request":
			p := rule["parameters"].(map[string]interface{})
			if p["required_approving_review_count"].(float64) != 2 {
				t.Errorf("pull_request count = %v", p["required_approving_review_count"])
			}
		case "required_status_checks":
			p := rule["parameters"].(map[string]interface{})
			checks := p["required_status_checks"].([]interface{})
			if len(checks) != 1 || checks[0].(map[string]interface{})["context"] != "build" {
				t.Errorf("status checks = %v", checks)
			}
		}
	}
}
