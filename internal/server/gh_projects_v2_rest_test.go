package bleephub

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *isolatedServer) seedProjectV2Org(t *testing.T, orgLogin, title string) (*store.Org, *store.ProjectV2) {
	t.Helper()
	admin := s.store.UsersByLogin["admin"]
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		org = s.store.CreateOrg(admin, orgLogin, orgLogin, "")
		if org == nil {
			t.Fatalf("create org %s failed", orgLogin)
		}
	}
	p := s.store.ProjectsV2.CreateProject(org.ID, "Organization", title, admin.ID)
	return org, p
}

func TestOrgProjectsV2_ListGetAndVisibility(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, p := s.seedProjectV2Org(t, "pv2-vis-org", "Roadmap Q3")

	// Unauthenticated → 401 (the projects surface requires a token).
	resp := s.get(t, "/api/v3/orgs/"+org.Login+"/projectsV2", "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated list = %d, want 401", resp.StatusCode)
	}

	// Admin (org member) sees the private project.
	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/projectsV2", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("list = %d, want 200", resp.StatusCode)
	}
	projects := decodeJSONArray(t, resp)
	found := false
	for _, pj := range projects {
		if int(pj["id"].(float64)) == p.ID {
			found = true
			if pj["title"] != "Roadmap Q3" {
				t.Errorf("title = %v", pj["title"])
			}
			if pj["state"] != "open" {
				t.Errorf("state = %v, want open", pj["state"])
			}
			owner, _ := pj["owner"].(map[string]interface{})
			if owner == nil || owner["login"] != org.Login {
				t.Errorf("owner = %v, want %s", pj["owner"], org.Login)
			}
			creator, _ := pj["creator"].(map[string]interface{})
			if creator == nil || creator["login"] != "admin" {
				t.Errorf("creator = %v, want admin", pj["creator"])
			}
		}
	}
	if !found {
		t.Fatal("project missing from org list")
	}

	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/projectsV2/"+strconv.Itoa(p.Number), defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("get = %d, want 200", resp.StatusCode)
	}
	got := decodeJSON(t, resp)
	if int(got["number"].(float64)) != p.Number {
		t.Fatalf("number = %v, want %d", got["number"], p.Number)
	}

	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/projectsV2/99999", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown number = %d, want 404", resp.StatusCode)
	}
	resp = s.get(t, "/api/v3/orgs/no-such-org/projectsV2", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown org = %d, want 404", resp.StatusCode)
	}

	// A non-member's PAT cannot see the private project (404 / excluded).
	outsider := s.createTestUser(t, "pv2-outsider")
	outsiderToken := s.store.CreateToken(outsider.ID, "repo").Value
	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/projectsV2/"+strconv.Itoa(p.Number), outsiderToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("outsider get private project = %d, want 404", resp.StatusCode)
	}

	// Once public, the outsider can read it.
	public := true
	s.store.ProjectsV2.UpdateProject(p.ID, nil, nil, &public)
	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/projectsV2/"+strconv.Itoa(p.Number), outsiderToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("outsider get public project = %d, want 200", resp.StatusCode)
	}
	got = decodeJSON(t, resp)
	if got["public"] != true {
		t.Fatalf("public = %v, want true", got["public"])
	}
}

func TestOrgProjectsV2_ListQueryFilter(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, _ := s.seedProjectV2Org(t, "pv2-q-org", "Alpha launch")
	admin := s.store.UsersByLogin["admin"]
	closedProj := s.store.ProjectsV2.CreateProject(org.ID, "Organization", "Beta cleanup", admin.ID)
	closed := true
	s.store.ProjectsV2.UpdateProject(closedProj.ID, nil, &closed, nil)

	resp := s.get(t, "/api/v3/orgs/"+org.Login+"/projectsV2?q=is%3Aclosed", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("q=is:closed = %d, want 200", resp.StatusCode)
	}
	projects := decodeJSONArray(t, resp)
	if len(projects) != 1 || projects[0]["title"] != "Beta cleanup" {
		t.Fatalf("q=is:closed matched %v", projects)
	}
	if projects[0]["state"] != "closed" {
		t.Fatalf("state = %v, want closed", projects[0]["state"])
	}
	if projects[0]["closed_at"] == nil {
		t.Fatal("closed_at should be set on a closed project")
	}

	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/projectsV2?q=alpha", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("q=alpha = %d, want 200", resp.StatusCode)
	}
	projects = decodeJSONArray(t, resp)
	if len(projects) != 1 || projects[0]["title"] != "Alpha launch" {
		t.Fatalf("q=alpha matched %v", projects)
	}
}

func TestOrgProjectV2Fields_CreateListGet(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, p := s.seedProjectV2Org(t, "pv2-fields-org", "Fields")
	seededFields := len(projectV2SeededFieldNames(t, s, p.ID))
	base := "/api/v3/orgs/" + org.Login + "/projectsV2/" + strconv.Itoa(p.Number)

	resp := s.post(t, base+"/fields", defaultToken, map[string]interface{}{
		"name": "Notes", "data_type": "text",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create text field = %d, want 201", resp.StatusCode)
	}
	textField := decodeJSON(t, resp)
	if textField["data_type"] != "text" {
		t.Fatalf("data_type = %v, want text", textField["data_type"])
	}

	resp = s.post(t, base+"/fields", defaultToken, map[string]interface{}{
		"name": "Priority", "data_type": "single_select",
		"single_select_options": []map[string]interface{}{
			{"name": "High", "color": "RED", "description": "Do first"},
			{"name": "Low"},
		},
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create single select field = %d, want 201", resp.StatusCode)
	}
	ssField := decodeJSON(t, resp)
	options, _ := ssField["options"].([]interface{})
	if len(options) != 2 {
		t.Fatalf("options = %v, want 2 entries", ssField["options"])
	}
	first, _ := options[0].(map[string]interface{})
	name, _ := first["name"].(map[string]interface{})
	if name["raw"] != "High" || first["color"] != "RED" {
		t.Fatalf("first option = %v", first)
	}
	second, _ := options[1].(map[string]interface{})
	if second["color"] != "GRAY" {
		t.Fatalf("default option color = %v, want GRAY", second["color"])
	}

	resp = s.post(t, base+"/fields", defaultToken, map[string]interface{}{
		"name": "Sprint", "data_type": "iteration",
		"iteration_configuration": map[string]interface{}{
			"start_date": "2026-07-06", "duration": 7,
			"iterations": []map[string]interface{}{
				{"title": "Sprint 1", "start_date": "2026-07-06", "duration": 7},
				{"title": "Sprint 2", "start_date": "2026-07-13"},
			},
		},
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create iteration field = %d, want 201", resp.StatusCode)
	}
	iterField := decodeJSON(t, resp)
	cfg, _ := iterField["configuration"].(map[string]interface{})
	if cfg == nil {
		t.Fatal("iteration field missing configuration")
	}
	if cfg["start_day"] != float64(1) { // 2026-07-06 is a Monday
		t.Errorf("start_day = %v, want 1", cfg["start_day"])
	}
	iterations, _ := cfg["iterations"].([]interface{})
	if len(iterations) != 2 {
		t.Fatalf("iterations = %v, want 2", cfg["iterations"])
	}

	resp = s.get(t, base+"/fields", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("list fields = %d, want 200", resp.StatusCode)
	}
	fields := decodeJSONArray(t, resp)
	// The seeded defaults every new project carries, plus the three this test
	// created.
	wantFields := seededFields + 3
	if len(fields) != wantFields {
		t.Fatalf("fields = %d, want %d", len(fields), wantFields)
	}

	fieldID := int(ssField["id"].(float64))
	resp = s.get(t, base+"/fields/"+strconv.Itoa(fieldID), defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("get field = %d, want 200", resp.StatusCode)
	}
	gotField := decodeJSON(t, resp)
	if gotField["name"] != "Priority" {
		t.Fatalf("field name = %v", gotField["name"])
	}
	resp = s.get(t, base+"/fields/999999", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown field = %d, want 404", resp.StatusCode)
	}

	for i, body := range []map[string]interface{}{
		{"name": "Priority", "data_type": "text"},       // duplicate name
		{"name": "Empty", "data_type": "single_select"}, // no options
		{"name": "Weird", "data_type": "money"},         // bad data type
		{"data_type": "text"},                           // missing name
		{"name": "Ext", "data_type": "iteration"},       // missing iteration configuration
		{"issue_field_id": 12345},                       // no issue fields exist
		{"name": "BadIter", "data_type": "iteration", // malformed start_date
			"iteration_configuration": map[string]interface{}{"start_date": "07/06/2026"}},
	} {
		resp = s.post(t, base+"/fields", defaultToken, body)
		resp.Body.Close()
		if resp.StatusCode != 422 {
			t.Fatalf("invalid field body #%d = %d, want 422", i, resp.StatusCode)
		}
	}
}

func TestOrgProjectV2Items_AddGetPatchDelete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, p := s.seedProjectV2Org(t, "pv2-items-org", "Items")
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateOrgRepo(org, admin, "pv2-items-repo", "", false)
	if repo == nil {
		t.Fatal("create org repo failed")
	}
	issue := s.store.CreateIssue(repo.ID, admin.ID, "Fix the flux capacitor", "", nil, nil, 0)
	base := "/api/v3/orgs/" + org.Login + "/projectsV2/" + strconv.Itoa(p.Number)

	resp := s.post(t, base+"/items", defaultToken, map[string]interface{}{
		"type": "Issue", "id": issue.ID,
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("add item = %d, want 201", resp.StatusCode)
	}
	item := decodeJSON(t, resp)
	if item["content_type"] != "Issue" {
		t.Fatalf("content_type = %v, want Issue", item["content_type"])
	}
	itemID := int(item["id"].(float64))

	// Adding the same issue again is idempotent (same item ID).
	resp = s.post(t, base+"/items", defaultToken, map[string]interface{}{
		"type": "Issue", "owner": org.Login, "repo": repo.Name, "number": issue.Number,
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("re-add item = %d, want 201", resp.StatusCode)
	}
	again := decodeJSON(t, resp)
	if int(again["id"].(float64)) != itemID {
		t.Fatalf("re-add produced a different item: %v vs %d", again["id"], itemID)
	}

	resp = s.post(t, base+"/drafts", defaultToken, map[string]interface{}{
		"title": "Draft: write docs", "body": "eventually",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create draft = %d, want 201", resp.StatusCode)
	}
	draft := decodeJSON(t, resp)
	if draft["content_type"] != "DraftIssue" {
		t.Fatalf("draft content_type = %v", draft["content_type"])
	}
	draftID := int(draft["id"].(float64))

	resp = s.get(t, base+"/items", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("list items = %d, want 200", resp.StatusCode)
	}
	items := decodeJSONArray(t, resp)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}

	resp = s.get(t, base+"/items/"+strconv.Itoa(itemID), defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("get item = %d, want 200", resp.StatusCode)
	}
	gotItem := decodeJSON(t, resp)
	content, _ := gotItem["content"].(map[string]interface{})
	if content == nil || content["title"] != "Fix the flux capacitor" {
		t.Fatalf("item content = %v", gotItem["content"])
	}

	textField := s.store.ProjectsV2.CreateField(p.ID, "Notes", store.ProjectV2FieldText, nil, nil)
	// Status is one of the fields every project is seeded with, so this reuses
	// it rather than creating a second field of the same name.
	ssField := s.store.ProjectsV2.FieldByNameOnProject(p.ID, "Status")
	if ssField == nil {
		t.Fatal("seeded Status field missing")
	}
	numField := s.store.ProjectsV2.CreateField(p.ID, "Points", store.ProjectV2FieldNumber, nil, nil)
	resp = s.patch(t, base+"/items/"+strconv.Itoa(itemID), defaultToken, map[string]interface{}{
		"fields": []map[string]interface{}{
			{"id": textField.ID, "value": "needs review"},
			{"id": ssField.ID, "value": ssField.Options[2].ID},
			{"id": numField.ID, "value": 5},
		},
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("patch item = %d, want 200", resp.StatusCode)
	}
	patched := decodeJSON(t, resp)
	values := map[string]interface{}{}
	for _, raw := range patched["fields"].([]interface{}) {
		entry := raw.(map[string]interface{})
		values[entry["name"].(string)] = entry["value"]
	}
	if values["Notes"] != "needs review" {
		t.Fatalf("Notes value = %v", values["Notes"])
	}
	status, _ := values["Status"].(map[string]interface{})
	if status == nil || status["name"] != "Done" {
		t.Fatalf("Status value = %v", values["Status"])
	}
	if values["Points"] != float64(5) {
		t.Fatalf("Points value = %v", values["Points"])
	}

	// Clearing a value via null; explicit fields selection returns null.
	resp = s.patch(t, base+"/items/"+strconv.Itoa(itemID), defaultToken, map[string]interface{}{
		"fields": []map[string]interface{}{{"id": textField.ID, "value": nil}},
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("clear value = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	resp = s.get(t, base+"/items/"+strconv.Itoa(itemID)+"?fields="+strconv.Itoa(textField.ID), defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("get with fields selection = %d, want 200", resp.StatusCode)
	}
	selected := decodeJSON(t, resp)
	selFields := selected["fields"].([]interface{})
	if len(selFields) != 1 || selFields[0].(map[string]interface{})["value"] != nil {
		t.Fatalf("selected fields = %v, want one null value", selected["fields"])
	}

	for i, body := range []map[string]interface{}{
		{"fields": []map[string]interface{}{{"id": 999999, "value": "x"}}},
		{"fields": []map[string]interface{}{{"id": numField.ID, "value": "not a number"}}},
		{"fields": []map[string]interface{}{}},
	} {
		resp = s.patch(t, base+"/items/"+strconv.Itoa(itemID), defaultToken, body)
		resp.Body.Close()
		if resp.StatusCode != 422 {
			t.Fatalf("invalid patch #%d = %d, want 422", i, resp.StatusCode)
		}
	}

	// q filter: is:draft matches only the draft.
	resp = s.get(t, base+"/items?q=is%3Adraft", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("q=is:draft = %d, want 200", resp.StatusCode)
	}
	drafts := decodeJSONArray(t, resp)
	if len(drafts) != 1 || int(drafts[0]["id"].(float64)) != draftID {
		t.Fatalf("q=is:draft matched %v", drafts)
	}

	// Field-value filter: Status:Done matches the patched item.
	resp = s.get(t, base+"/items?q=Status%3ADone", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("q=Status:Done = %d, want 200", resp.StatusCode)
	}
	done := decodeJSONArray(t, resp)
	if len(done) != 1 || int(done[0]["id"].(float64)) != itemID {
		t.Fatalf("q=Status:Done matched %v", done)
	}

	resp = s.delete(t, base+"/items/"+strconv.Itoa(draftID), defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete item = %d, want 204", resp.StatusCode)
	}
	resp = s.get(t, base+"/items/"+strconv.Itoa(draftID), defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("deleted item get = %d, want 404", resp.StatusCode)
	}

	for i, body := range []map[string]interface{}{
		{"type": "Gist", "id": issue.ID},
		{"type": "Issue"},
		{"type": "Issue", "id": 999999},
		{"type": "Issue", "owner": org.Login, "repo": "nope", "number": 1},
	} {
		resp = s.post(t, base+"/items", defaultToken, body)
		resp.Body.Close()
		if resp.StatusCode != 422 {
			t.Fatalf("invalid add-item body #%d = %d, want 422", i, resp.StatusCode)
		}
	}
}

func TestOrgProjectV2Views_CreateAndListItems(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, p := s.seedProjectV2Org(t, "pv2-views-org", "Views")
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateOrgRepo(org, admin, "pv2-views-repo", "", false)
	if repo == nil {
		t.Fatal("create org repo failed")
	}
	issue := s.store.CreateIssue(repo.ID, admin.ID, "An issue", "", nil, nil, 0)
	s.store.ProjectsV2.AddItem(p.ID, "Issue", issue.ID, admin.ID)
	s.store.ProjectsV2.AddDraftItem(p.ID, "A draft", "", admin.ID)
	seededFields := len(projectV2SeededFieldNames(t, s, p.ID))
	field := s.store.ProjectsV2.CreateField(p.ID, "Stage", store.ProjectV2FieldText, nil, nil)
	base := "/api/v3/orgs/" + org.Login + "/projectsV2/" + strconv.Itoa(p.Number)

	resp := s.post(t, base+"/views", defaultToken, map[string]interface{}{
		"name": "Issues board", "layout": "board", "filter": "is:issue",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create view = %d, want 201", resp.StatusCode)
	}
	view := decodeJSON(t, resp)
	if view["layout"] != "board" || view["filter"] != "is:issue" {
		t.Fatalf("view = %v", view)
	}
	// visible_fields defaults to every field on the project: the seeded
	// defaults plus the one this test added.
	visible, _ := view["visible_fields"].([]interface{})
	if len(visible) != seededFields+1 {
		t.Fatalf("default visible_fields = %v", view["visible_fields"])
	}
	sawField := false
	for _, raw := range visible {
		if int(raw.(float64)) == field.ID {
			sawField = true
		}
	}
	if !sawField {
		t.Fatalf("default visible_fields %v omits the created field %d", view["visible_fields"], field.ID)
	}
	viewNumber := int(view["number"].(float64))

	// The view's filter hides the draft.
	resp = s.get(t, base+"/views/"+strconv.Itoa(viewNumber)+"/items", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("view items = %d, want 200", resp.StatusCode)
	}
	items := decodeJSONArray(t, resp)
	if len(items) != 1 || items[0]["content_type"] != "Issue" {
		t.Fatalf("view items = %v, want the one issue", items)
	}

	resp = s.get(t, base+"/views/999/items", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown view = %d, want 404", resp.StatusCode)
	}

	for i, body := range []map[string]interface{}{
		{"name": "x", "layout": "kanban"},
		{"layout": "board"},
		{"name": "y", "layout": "table", "visible_fields": []int{999999}},
	} {
		resp = s.post(t, base+"/views", defaultToken, body)
		resp.Body.Close()
		if resp.StatusCode != 422 {
			t.Fatalf("invalid view body #%d = %d, want 422", i, resp.StatusCode)
		}
	}
}

func TestUserProjectsV2_FlowIncludingUserIDRoutes(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	p := s.store.ProjectsV2.CreateProject(admin.ID, "User", "Personal backlog", admin.ID)

	resp := s.get(t, "/api/v3/users/admin/projectsV2", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("user list = %d, want 200", resp.StatusCode)
	}
	projects := decodeJSONArray(t, resp)
	found := false
	for _, pj := range projects {
		if int(pj["id"].(float64)) == p.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("user project missing from list")
	}
	base := "/api/v3/users/admin/projectsV2/" + strconv.Itoa(p.Number)
	resp = s.get(t, base, defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("user get = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.post(t, base+"/fields", defaultToken, map[string]interface{}{
		"name": "Effort", "data_type": "number",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("user field create = %d, want 201", resp.StatusCode)
	}
	field := decodeJSON(t, resp)
	fieldID := int(field["id"].(float64))
	resp = s.get(t, base+"/fields/"+strconv.Itoa(fieldID), defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("user field get = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	resp = s.get(t, base+"/fields", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("user fields list = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Draft creation goes through POST /user/{user_id}/… (by user ID).
	resp = s.post(t, fmt.Sprintf("/api/v3/user/%d/projectsV2/%d/drafts", admin.ID, p.Number), defaultToken,
		map[string]interface{}{"title": "My draft"})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("user draft create = %d, want 201", resp.StatusCode)
	}
	draft := decodeJSON(t, resp)
	draftID := int(draft["id"].(float64))

	resp = s.patch(t, base+"/items/"+strconv.Itoa(draftID), defaultToken, map[string]interface{}{
		"fields": []map[string]interface{}{{"id": fieldID, "value": 3}},
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("user item patch = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	resp = s.get(t, base+"/items", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("user items list = %d, want 200", resp.StatusCode)
	}
	items := decodeJSONArray(t, resp)
	if len(items) != 1 {
		t.Fatalf("user items = %d, want 1", len(items))
	}

	// View creation goes through POST /users/{user_id}/… (by user ID).
	resp = s.post(t, fmt.Sprintf("/api/v3/users/%d/projectsV2/%d/views", admin.ID, p.Number), defaultToken,
		map[string]interface{}{"name": "Table", "layout": "table"})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("user view create = %d, want 201", resp.StatusCode)
	}
	view := decodeJSON(t, resp)
	viewNumber := int(view["number"].(float64))
	resp = s.get(t, base+"/views/"+strconv.Itoa(viewNumber)+"/items", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("user view items = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Another user cannot write to this project (403), and the
	// authenticated-user draft route rejects an addressee mismatch.
	other := s.createTestUser(t, "pv2-other-user")
	otherToken := s.store.CreateToken(other.ID, "repo").Value
	resp = s.post(t, fmt.Sprintf("/api/v3/user/%d/projectsV2/%d/drafts", admin.ID, p.Number), otherToken,
		map[string]interface{}{"title": "Sneaky"})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("mismatched user draft = %d, want 403", resp.StatusCode)
	}

	// Private user project hidden from others.
	resp = s.get(t, base, otherToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("other user get private project = %d, want 404", resp.StatusCode)
	}

	resp = s.delete(t, base+"/items/"+strconv.Itoa(draftID), defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("user item delete = %d, want 204", resp.StatusCode)
	}
}

func TestOrgProjectV2Items_CursorPagination(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org, p := s.seedProjectV2Org(t, "pv2-page-org", "Paging")
	admin := s.store.UsersByLogin["admin"]
	for i := 0; i < 5; i++ {
		s.store.ProjectsV2.AddDraftItem(p.ID, fmt.Sprintf("Draft %d", i), "", admin.ID)
	}
	base := "/api/v3/orgs/" + org.Login + "/projectsV2/" + strconv.Itoa(p.Number)

	resp := s.get(t, base+"/items?per_page=2", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("page 1 = %d, want 200", resp.StatusCode)
	}
	link := resp.Header.Get("Link")
	page1 := decodeJSONArray(t, resp)
	if len(page1) != 2 {
		t.Fatalf("page 1 size = %d, want 2", len(page1))
	}
	if link == "" || !containsRel(link, "next") {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	after := extractCursor(t, link, "after")
	resp = s.get(t, base+"/items?per_page=2&after="+after, defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("page 2 = %d, want 200", resp.StatusCode)
	}
	page2 := decodeJSONArray(t, resp)
	if len(page2) != 2 {
		t.Fatalf("page 2 size = %d, want 2", len(page2))
	}
	if page1[0]["id"] == page2[0]["id"] {
		t.Fatal("page 2 repeats page 1")
	}
}

func containsRel(link, rel string) bool {
	return strings.Contains(link, `rel="`+rel+`"`)
}

func extractCursor(t *testing.T, link, param string) string {
	t.Helper()
	start := strings.Index(link, "<")
	end := strings.Index(link, ">")
	if start < 0 || end < start {
		t.Fatalf("malformed Link header %q", link)
	}
	u, err := url.Parse(link[start+1 : end])
	if err != nil {
		t.Fatalf("parse Link URL: %v", err)
	}
	cursor := u.Query().Get(param)
	if cursor == "" {
		t.Fatalf("Link %q has no %s cursor", link, param)
	}
	return cursor
}
