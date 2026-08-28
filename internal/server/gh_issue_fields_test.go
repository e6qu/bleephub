package bleephub

import (
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/server/testutil"
)

func TestOrgIssueFields_CRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.createTestOrg(t)

	resp := s.post(t, "/api/v3/orgs/"+org+"/issue-fields", defaultToken, map[string]interface{}{
		"name":        "Priority",
		"description": "Level of importance for the issue",
		"data_type":   "single_select",
		"options": []map[string]interface{}{
			{"name": "High", "description": "High priority", "color": "red"},
			{"name": "Low", "description": "Low priority", "color": "green"},
		},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("create issue field: %d", resp.StatusCode)
	}
	created := decodeJSON(t, resp)
	if created["name"] != "Priority" || created["data_type"] != "single_select" {
		t.Fatalf("created = %v", created)
	}
	if created["visibility"] != "organization_members_only" {
		t.Fatalf("default visibility = %v", created["visibility"])
	}
	options := created["options"].([]interface{})
	if len(options) != 2 {
		t.Fatalf("options = %v", options)
	}
	firstOptionID := int(options[0].(map[string]interface{})["id"].(float64))
	fieldID := itoa(int(created["id"].(float64)))

	resp = s.get(t, "/api/v3/orgs/"+org+"/issue-fields", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("list issue fields: %d", resp.StatusCode)
	}
	if list := decodeJSONArray(t, resp); len(list) != 1 {
		t.Fatalf("list = %v", list)
	}

	// Reuse the "High" option's ID so it is retained and renamed, not replaced.
	resp = s.patch(t, "/api/v3/orgs/"+org+"/issue-fields/"+fieldID, defaultToken, map[string]interface{}{
		"name": "Severity",
		"options": []map[string]interface{}{
			{"id": firstOptionID, "name": "Critical", "color": "red", "priority": 1},
			{"name": "Minor", "color": "gray", "priority": 2},
		},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("update issue field: %d", resp.StatusCode)
	}
	updated := decodeJSON(t, resp)
	if updated["name"] != "Severity" {
		t.Fatalf("updated = %v", updated)
	}
	newOptions := updated["options"].([]interface{})
	if len(newOptions) != 2 {
		t.Fatalf("options after update = %v", newOptions)
	}
	retained := newOptions[0].(map[string]interface{})
	if int(retained["id"].(float64)) != firstOptionID || retained["name"] != "Critical" {
		t.Fatalf("option with existing id must be retained and renamed, got %v", retained)
	}

	resp = s.delete(t, "/api/v3/orgs/"+org+"/issue-fields/"+fieldID, defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete issue field: %d", resp.StatusCode)
	}
	resp = s.delete(t, "/api/v3/orgs/"+org+"/issue-fields/"+fieldID, defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("delete missing issue field: %d", resp.StatusCode)
	}
}

func TestOrgIssueFields_Validation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.createTestOrg(t)

	resp := s.post(t, "/api/v3/orgs/"+org+"/issue-fields", defaultToken, map[string]interface{}{
		"name":      "Status",
		"data_type": "single_select",
	})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("single_select without options: %d", resp.StatusCode)
	}

	resp = s.post(t, "/api/v3/orgs/"+org+"/issue-fields", defaultToken, map[string]interface{}{
		"name":      "Weird",
		"data_type": "geo_point",
	})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("bad data_type: %d", resp.StatusCode)
	}

	resp = s.post(t, "/api/v3/orgs/"+org+"/issue-fields", defaultToken, map[string]interface{}{
		"name":      "Notes",
		"data_type": "text",
		"options":   []map[string]interface{}{{"name": "x", "color": "red"}},
	})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("options on text field: %d", resp.StatusCode)
	}
}

func TestIssueFieldValues_AddSetListClear(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.createTestOrg(t)
	repoKey, _ := s.createOrgRepoForGovernance(t, org)
	_, number := s.createIssueForTest(t, repoKey, "field value test")

	mkField := func(body map[string]interface{}) int {
		t.Helper()
		resp := s.post(t, "/api/v3/orgs/"+org+"/issue-fields", defaultToken, body)
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("create field %v: %d", body["name"], resp.StatusCode)
		}
		return int(decodeJSON(t, resp)["id"].(float64))
	}
	textID := mkField(map[string]interface{}{"name": "Team notes", "data_type": "text"})
	numberID := mkField(map[string]interface{}{"name": "Story points", "data_type": "number"})
	selectID := mkField(map[string]interface{}{
		"name": "Priority", "data_type": "single_select",
		"options": []map[string]interface{}{
			{"name": "High", "color": "red"},
			{"name": "Low", "color": "green"},
		},
	})
	multiID := mkField(map[string]interface{}{
		"name": "Areas", "data_type": "multi_select",
		"options": []map[string]interface{}{
			{"name": "backend", "color": "blue"},
			{"name": "frontend", "color": "pink"},
		},
	})
	dateID := mkField(map[string]interface{}{"name": "Due", "data_type": "date"})

	valuesPath := repoKey.path() + "/issues/" + itoa(number) + "/issue-field-values"

	resp := s.post(t, valuesPath, defaultToken, map[string]interface{}{
		"issue_field_values": []map[string]interface{}{
			{"field_id": textID, "value": "needs design review"},
			{"field_id": numberID, "value": 5},
			{"field_id": selectID, "value": "High"},
			{"field_id": multiID, "value": []string{"backend", "frontend"}},
			{"field_id": dateID, "value": "2030-12-31"},
		},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("add field values: %d", resp.StatusCode)
	}
	values := decodeJSONArray(t, resp)
	if len(values) != 5 {
		t.Fatalf("values after POST = %v", values)
	}
	byField := map[int]map[string]interface{}{}
	for _, v := range values {
		byField[int(v["issue_field_id"].(float64))] = v
	}
	if byField[textID]["value"] != "needs design review" {
		t.Fatalf("text value = %v", byField[textID])
	}
	if byField[numberID]["value"].(float64) != 5 {
		t.Fatalf("number value = %v", byField[numberID])
	}
	if byField[selectID]["value"] != "High" {
		t.Fatalf("single select value = %v", byField[selectID])
	}
	sso := byField[selectID]["single_select_option"].(map[string]interface{})
	if sso["name"] != "High" || sso["color"] != "red" {
		t.Fatalf("single_select_option = %v", sso)
	}
	mso := byField[multiID]["multi_select_options"].([]interface{})
	if len(mso) != 2 {
		t.Fatalf("multi_select_options = %v", mso)
	}

	resp = s.get(t, valuesPath, defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("list field values: %d", resp.StatusCode)
	}
	if got := decodeJSONArray(t, resp); len(got) != 5 {
		t.Fatalf("GET values = %v", got)
	}

	resp = s.put(t, valuesPath, defaultToken, map[string]interface{}{
		"issue_field_values": []map[string]interface{}{
			{"field_id": selectID, "value": "Low"},
		},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("set field values: %d", resp.StatusCode)
	}
	replaced := decodeJSONArray(t, resp)
	if len(replaced) != 1 || replaced[0]["value"] != "Low" {
		t.Fatalf("values after PUT = %v", replaced)
	}

	resp = s.post(t, valuesPath, defaultToken, map[string]interface{}{
		"issue_field_values": []map[string]interface{}{
			{"field_id": selectID, "value": "Nonexistent"},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("invalid option: %d", resp.StatusCode)
	}

	resp = s.post(t, valuesPath, defaultToken, map[string]interface{}{
		"issue_field_values": []map[string]interface{}{
			{"field_id": 99999999, "value": "x"},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("unknown field: %d", resp.StatusCode)
	}

	// POST with an empty array clears everything.
	resp = s.post(t, valuesPath, defaultToken, map[string]interface{}{
		"issue_field_values": []map[string]interface{}{},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("clear field values: %d", resp.StatusCode)
	}
	if got := decodeJSONArray(t, resp); len(got) != 0 {
		t.Fatalf("values after clear = %v", got)
	}
}

func TestIssueFieldValues_PushAccessRequired(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.createTestOrg(t)
	repoKey, _ := s.createOrgRepoForGovernance(t, org)
	_, number := s.createIssueForTest(t, repoKey, "outsider test")

	outsider := s.createTestUser(t, "gov-outsider-"+strconv.FormatInt(int64(testutil.NextTestID()), 36))
	tok := s.store.CreateToken(outsider.ID, "repo").Value

	resp := s.post(t, repoKey.path()+"/issues/"+itoa(number)+"/issue-field-values", tok, map[string]interface{}{
		"issue_field_values": []map[string]interface{}{},
	})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("outsider POST: %d", resp.StatusCode)
	}
}
