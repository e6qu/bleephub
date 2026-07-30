package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func enterpriseActionsRequest(
	t *testing.T,
	s *Server,
	method string,
	path string,
	body map[string]interface{},
) *httptest.ResponseRecorder {
	t.Helper()

	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.ghHeadersMiddleware(s.mux).ServeHTTP(rec, req)
	return rec
}

func decodeRecorderObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var value map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return value
}

func runnerNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("list runners: got %d %q, want 200", rec.Code, rec.Body.String())
	}
	body := decodeRecorderObject(t, rec)
	raw, ok := body["runners"].([]interface{})
	if !ok {
		t.Fatalf("runners = %#v, want array", body["runners"])
	}
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		runner, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("runner = %#v, want object", item)
		}
		names = append(names, runner["name"].(string))
	}
	if body["total_count"] != float64(len(names)) {
		t.Fatalf("total_count = %#v, want %d", body["total_count"], len(names))
	}
	return names
}

func TestEnterpriseActionsRunnerInventoriesAreScopeSafe(t *testing.T) {
	s := newTestServer()
	s.registerGHActionsRoutes()
	s.registerGHActionsPermissionsRoutes()
	s.registerGHEnterpriseActionsRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "runner-scope-org", "Runner Scope Org", "")
	repo := s.store.CreateOrgRepo(org, admin, "runner-scope-repo", "", false)
	if org == nil || repo == nil {
		t.Fatal("create runner scope fixtures")
	}

	agents := []*Agent{
		{ID: 7101, Name: "enterprise-runner", Enabled: true, Status: "online", Scope: runnerScope{Enterprise: "bleephub"}},
		{ID: 7102, Name: "org-runner", Enabled: true, Status: "online", Scope: runnerScope{Org: org.Login}},
		{ID: 7103, Name: "repo-runner", Enabled: true, Status: "online", Scope: runnerScope{Repo: repo.FullName}},
	}
	s.store.mu.Lock()
	for _, agent := range agents {
		s.store.Agents[agent.ID] = agent
	}
	s.store.mu.Unlock()

	assertNames := func(path string, want ...string) {
		t.Helper()
		got := runnerNames(t, enterpriseActionsRequest(t, s, http.MethodGet, path, nil))
		if len(got) != len(want) {
			t.Fatalf("%s runners = %v, want %v", path, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s runners = %v, want %v", path, got, want)
			}
		}
	}

	assertNames("/api/v3/enterprises/bleephub/actions/runners", "enterprise-runner")
	assertNames("/api/v3/orgs/runner-scope-org/actions/runners", "org-runner")
	assertNames("/api/v3/repos/runner-scope-org/runner-scope-repo/actions/runners", "org-runner", "repo-runner")

	for _, path := range []string{
		"/api/v3/enterprises/bleephub/actions/runners/7102",
		"/api/v3/orgs/runner-scope-org/actions/runners/7101",
		"/api/v3/repos/runner-scope-org/runner-scope-repo/actions/runners/7101",
		"/api/v3/enterprises/bleephub/actions/runners/7102/labels",
	} {
		rec := enterpriseActionsRequest(t, s, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: got %d %q, want 404", path, rec.Code, rec.Body.String())
		}
	}

	labelPath := "/api/v3/enterprises/bleephub/actions/runners/7101/labels"
	rec := enterpriseActionsRequest(t, s, http.MethodPost, labelPath, map[string]interface{}{
		"labels": []string{"enterprise"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("add enterprise runner label: got %d %q, want 200", rec.Code, rec.Body.String())
	}
	labels := decodeRecorderObject(t, rec)["labels"].([]interface{})
	if len(labels) != 1 || labels[0].(map[string]interface{})["name"] != "enterprise" {
		t.Fatalf("labels = %#v, want enterprise", labels)
	}
}

func TestEnterpriseActionsRunnerRegistrationTokenCarriesEnterpriseScope(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseActionsRoutes()

	rec := enterpriseActionsRequest(
		t,
		s,
		http.MethodPost,
		"/api/v3/enterprises/bleephub/actions/runners/registration-token",
		nil,
	)
	if rec.Code != http.StatusCreated {
		t.Fatalf("registration token: got %d %q, want 201", rec.Code, rec.Body.String())
	}
	body := decodeRecorderObject(t, rec)
	token, ok := body["token"].(string)
	if !ok || token == "" {
		t.Fatalf("token = %#v, want non-empty string", body["token"])
	}
	claims, err := parseRunnerRegistrationToken(token, []string{runnerPurposeRegistration})
	if err != nil {
		t.Fatalf("parse registration token: %v", err)
	}
	if claims.Scope != (runnerScope{Enterprise: "bleephub"}) {
		t.Fatalf("scope = %#v, want enterprise bleephub", claims.Scope)
	}

	rec = enterpriseActionsRequest(
		t,
		s,
		http.MethodPost,
		"/api/v3/enterprises/not-bleephub/actions/runners/registration-token",
		nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown enterprise token: got %d %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestRunnerVisibleAt(t *testing.T) {
	cases := []struct {
		agent  runnerScope
		target runnerScope
		want   bool
	}{
		{runnerScope{Repo: "Acme/Widget"}, runnerScope{Repo: "acme/widget"}, true},
		{runnerScope{Org: "Acme"}, runnerScope{Repo: "acme/widget"}, true},
		{runnerScope{Enterprise: "bleephub"}, runnerScope{Repo: "acme/widget"}, false},
		{runnerScope{Org: "Acme"}, runnerScope{Org: "acme"}, true},
		{runnerScope{Repo: "Acme/Widget"}, runnerScope{Org: "acme"}, false},
		{runnerScope{Enterprise: "Bleephub"}, runnerScope{Enterprise: "bleephub"}, true},
	}
	for i, tc := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if got := runnerVisibleAt(tc.agent, tc.target); got != tc.want {
				t.Fatalf("runnerVisibleAt(%#v, %#v) = %t, want %t", tc.agent, tc.target, got, tc.want)
			}
		})
	}
}

func TestEnterpriseRunnerGroupsAreOwnedAndManageOrganizations(t *testing.T) {
	s := newTestServer()
	s.registerRunnerGroupRoutes()
	s.registerGHEnterpriseActionsRoutes()

	admin := s.store.LookupUserByLogin("admin")
	orgA := s.store.CreateOrg(admin, "enterprise-group-a", "Enterprise Group A", "")
	orgB := s.store.CreateOrg(admin, "enterprise-group-b", "Enterprise Group B", "")
	s.store.mu.Lock()
	s.store.Agents[7201] = &Agent{
		ID: 7201, Name: "enterprise-group-runner", Status: "online",
		Scope: runnerScope{Enterprise: "bleephub"},
	}
	s.store.Agents[7202] = &Agent{
		ID: 7202, Name: "org-group-runner", Status: "online",
		Scope: runnerScope{Org: orgA.Login},
	}
	s.store.mu.Unlock()

	base := "/api/v3/enterprises/bleephub/actions/runner-groups"
	rec := enterpriseActionsRequest(t, s, http.MethodPost, base, map[string]interface{}{
		"name":                      "enterprise-selected",
		"visibility":                "selected",
		"selected_organization_ids": []int{orgA.ID},
		"runners":                   []int{7201},
		"restricted_to_workflows":   true,
		"selected_workflows":        []string{"enterprise-group-a/repo/.github/workflows/ci.yml@refs/heads/main"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create enterprise group: got %d %q, want 201", rec.Code, rec.Body.String())
	}
	created := decodeRecorderObject(t, rec)
	groupID := int(created["id"].(float64))
	selectedOrganizationsURL, _ := created["selected_organizations_url"].(string)
	if !strings.HasSuffix(selectedOrganizationsURL,
		base+"/"+strconv.Itoa(groupID)+"/organizations") {
		t.Fatalf("selected_organizations_url = %#v", created["selected_organizations_url"])
	}
	if created["restricted_to_workflows"] != true {
		t.Fatalf("restricted_to_workflows = %#v, want true", created["restricted_to_workflows"])
	}

	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/"+strconv.Itoa(groupID)+"/organizations", nil)
	orgs := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || orgs["total_count"] != float64(1) {
		t.Fatalf("list selected organizations: got %d %#v", rec.Code, orgs)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPut,
		base+"/"+strconv.Itoa(groupID)+"/organizations/"+strconv.Itoa(orgB.ID), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("add selected organization: got %d %q, want 204", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/"+strconv.Itoa(groupID)+"/runners", nil)
	if names := runnerNames(t, rec); len(names) != 1 || names[0] != "enterprise-group-runner" {
		t.Fatalf("enterprise group runners = %v", names)
	}

	// An organization has its own default group and cannot address the
	// enterprise group even though both live in one persisted map.
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/orgs/enterprise-group-a/actions/runner-groups/"+strconv.Itoa(groupID), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("org read of enterprise group: got %d %q, want 404", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost,
		"/api/v3/orgs/enterprise-group-a/actions/runner-groups", map[string]interface{}{
			"name": "org-only",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org group: got %d %q, want 201", rec.Code, rec.Body.String())
	}
	orgGroupID := int(decodeRecorderObject(t, rec)["id"].(float64))
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/"+strconv.Itoa(orgGroupID), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("enterprise read of org group: got %d %q, want 404", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost, base, map[string]interface{}{
		"name":    "wrong-scope-runner",
		"runners": []int{7202},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("create with org runner: got %d %q, want 404", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base, map[string]interface{}{
		"name": "enterprise-selected",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate enterprise group: got %d %q, want 422", rec.Code, rec.Body.String())
	}
}

func TestEnterpriseHostedRunnersCRUDAndScopeIsolation(t *testing.T) {
	s := newTestServer()
	s.registerGHHostedRunnerRoutes()
	s.registerRunnerGroupRoutes()
	s.registerGHEnterpriseActionsRoutes()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "enterprise-hosted-org", "Enterprise Hosted Org", "")

	groupList := decodeRecorderObject(t, enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/enterprises/bleephub/actions/runner-groups", nil))
	groups := groupList["runner_groups"].([]interface{})
	var groupID int
	for _, raw := range groups {
		group := raw.(map[string]interface{})
		if group["default"] == true {
			groupID = int(group["id"].(float64))
		}
	}
	if groupID == 0 {
		t.Fatalf("enterprise default runner group missing: %#v", groupList)
	}

	base := "/api/v3/enterprises/bleephub/actions/hosted-runners"
	rec := enterpriseActionsRequest(t, s, http.MethodPost, base, map[string]interface{}{
		"name":             "enterprise-hosted",
		"image":            map[string]interface{}{"id": "ubuntu-24.04", "source": "github"},
		"size":             "8-core",
		"runner_group_id":  groupID,
		"maximum_runners":  4,
		"enable_static_ip": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create enterprise hosted runner: got %d %q, want 201", rec.Code, rec.Body.String())
	}
	created := decodeRecorderObject(t, rec)
	runnerID := int(created["id"].(float64))
	if created["runner_group_id"] != float64(groupID) || created["maximum_runners"] != float64(4) {
		t.Fatalf("created enterprise hosted runner = %#v", created)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodGet, base, nil)
	list := decodeRecorderObject(t, rec)
	if list["total_count"] != float64(1) {
		t.Fatalf("enterprise hosted runners = %#v", list)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/orgs/"+org.Login+"/actions/hosted-runners/"+strconv.Itoa(runnerID), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("org read of enterprise hosted runner: got %d %q, want 404", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPatch,
		base+"/"+strconv.Itoa(runnerID), map[string]interface{}{
			"name":            "enterprise-hosted-renamed",
			"maximum_runners": 6,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch enterprise hosted runner: got %d %q", rec.Code, rec.Body.String())
	}
	patched := decodeRecorderObject(t, rec)
	if patched["name"] != "enterprise-hosted-renamed" || patched["maximum_runners"] != float64(6) {
		t.Fatalf("patched enterprise hosted runner = %#v", patched)
	}

	limits := decodeRecorderObject(t,
		enterpriseActionsRequest(t, s, http.MethodGet, base+"/limits", nil))
	publicIPs := limits["public_ips"].(map[string]interface{})
	if publicIPs["current_usage"] != float64(6) {
		t.Fatalf("enterprise static IP usage = %#v, want 6", publicIPs)
	}

	image := s.store.CreateEnterpriseHostedRunnerCustomImage("bleephub", "Enterprise Image", "linux-x64")
	if !s.store.AddHostedRunnerCustomImageVersion(image.ID, "1.0.0", 30) {
		t.Fatal("add enterprise hosted-runner image version")
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/images/custom", nil)
	images := decodeRecorderObject(t, rec)
	if images["total_count"] != float64(1) {
		t.Fatalf("enterprise custom images = %#v", images)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/orgs/"+org.Login+"/actions/hosted-runners/images/custom/"+strconv.Itoa(image.ID), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("org read of enterprise custom image: got %d %q, want 404", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/"+strconv.Itoa(runnerID), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete enterprise hosted runner: got %d %q, want 202", rec.Code, rec.Body.String())
	}
	if deleted := decodeRecorderObject(t, rec); deleted["status"] != "Deleting" {
		t.Fatalf("deleted enterprise hosted runner = %#v", deleted)
	}
}
