package bleephub

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
)

func initPhase8Routes(s *Server) {
	s.registerAuthRoutes()
	s.registerGHRestRoutes()
	s.registerGHRepoRoutes()
	s.registerGHSecurityAdvisoriesRoutes()
	s.registerGHSecretScanningRoutes()
	s.registerGHDependabotRoutes()
	s.registerGHCodeScanningRoutes()
	s.registerGHCustomPropertyRoutes()
	s.registerGHDeploymentsRoutes()
	s.registerGHBranchProtectionRoutes()
}

func mustDecodeJSONList(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode JSON list: %v", err)
	}
	return list
}

// TestSecretScanning_OrgAlertFilters verifies that the org-level secret
// scanning alert endpoint accepts and applies the documented query filters
// (state, secret_type, resolution) rather than silently dropping them.
func TestSecretScanning_OrgAlertFilters(t *testing.T) {
	s := newTestServer()
	initPhase8Routes(s)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "ss-filter-org", "SS Filter Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	repo1 := s.store.CreateOrgRepo(org, admin, "ss-filter-repo1", "", false)
	repo2 := s.store.CreateOrgRepo(org, admin, "ss-filter-repo2", "", false)
	if repo1 == nil || repo2 == nil {
		t.Fatal("create org repo failed")
	}

	s.store.mu.Lock()
	_ = s.store.createSecretScanningAlertLocked(repo1.FullName, "github_personal_access_token", nil)
	a2 := s.store.createSecretScanningAlertLocked(repo2.FullName, "aws_access_key_id", nil)
	s.store.mu.Unlock()

	token := adminTokenFor(s)
	base := "/api/v3/orgs/ss-filter-org/secret-scanning/alerts"

	// No filter: both alerts.
	w := pagedJSONRequest(t, s, "GET", base, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list all = %d", w.Code)
	}
	list := mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 2 {
		t.Fatalf("list all = %d alerts, want 2", len(list))
	}

	// state=open filter (both are open by default): 2 alerts.
	w = pagedJSONRequest(t, s, "GET", base+"?state=open", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 2 {
		t.Fatalf("state=open = %d alerts, want 2", len(list))
	}

	// secret_type=aws_access_key_id filter: one alert.
	w = pagedJSONRequest(t, s, "GET", base+"?secret_type=aws_access_key_id", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 1 {
		t.Fatalf("secret_type=aws = %d alerts, want 1", len(list))
	}
	if list[0]["secret_type"] != "aws_access_key_id" {
		t.Errorf("secret_type=aws returned secret_type=%v", list[0]["secret_type"])
	}

	// Resolve one alert and filter by resolution.
	_ = s.store.UpdateSecretScanningAlert(a2, "resolved", "used_in_tests", "")

	w = pagedJSONRequest(t, s, "GET", base+"?resolution=used_in_tests", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 1 {
		t.Fatalf("resolution=used_in_tests = %d alerts, want 1", len(list))
	}

	// Unknown state: accepted, 200, zero matches.
	w = pagedJSONRequest(t, s, "GET", base+"?state=does-not-exist", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("unknown state = %d, want 200", w.Code)
	}
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 0 {
		t.Fatalf("unknown state = %d alerts, want 0", len(list))
	}
}

func adminTokenFor(s *Server) string { return AdminToken() }

// TestDependabot_OrgAlertFilters verifies the org-level dependabot alert
// endpoint accepts and applies state, ecosystem, and package filters.
func TestDependabot_OrgAlertFilters(t *testing.T) {
	s := newTestServer()
	initPhase8Routes(s)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "db-filter-org", "DB Filter Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	repo1 := s.store.CreateOrgRepo(org, admin, "db-filter-repo1", "", false)
	if repo1 == nil {
		t.Fatal("create repo failed")
	}

	s.store.mu.Lock()
	a1 := s.store.createDependabotAlertLocked(repo1.FullName, "pkg-a", "npm", "package-lock.json", "GHSA-a", "", "high", "open", "vuln a", "desc", "<4.17.21", "4.17.21")
	a2 := s.store.createDependabotAlertLocked(repo1.FullName, "pkg-b", "npm", "package-lock.json", "GHSA-b", "", "high", "dismissed", "vuln b", "desc", "<4.17.21", "4.17.21")
	s.store.mu.Unlock()

	token := adminTokenFor(s)
	base := "/api/v3/orgs/db-filter-org/dependabot/alerts"

	// No filter: both alerts.
	w := pagedJSONRequest(t, s, "GET", base, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list all = %d", w.Code)
	}
	list := mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 2 {
		t.Fatalf("list all = %d alerts, want 2", len(list))
	}

	// state filter.
	w = pagedJSONRequest(t, s, "GET", base+"?state=open", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 1 {
		t.Fatalf("state=open = %d alerts, want 1", len(list))
	}

	// package filter.
	w = pagedJSONRequest(t, s, "GET", base+"?package=pkg-b", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 1 {
		t.Fatalf("package=pkg-b = %d alerts, want 1", len(list))
	}

	// ecosystem filter.
	w = pagedJSONRequest(t, s, "GET", base+"?ecosystem=npm", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 2 {
		t.Fatalf("ecosystem=npm = %d alerts, want 2", len(list))
	}

	_ = a1
	_ = a2
}

// TestCodeScanning_ToolGUIDAccepted verifies that a SARIF upload with
// tool.driver.guid is accepted (silently) and the guid is echoed back in
// the alert and analysis JSON instead of being hardcoded to nil.
func TestCodeScanning_ToolGUIDAccepted(t *testing.T) {
	s := newTestServer()
	initPhase8Routes(s)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "cs-guid-org", "CS GUID Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	repo := s.store.CreateOrgRepo(org, admin, "cs-guid-repo", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}

	toolName := "CodeQL"
	toolGUID := "c7a7c45a-8a3c-4f6e-9b5c-1d3c01234567"
	sarif := map[string]any{
		"version": "2.1.0",
		"runs": []map[string]any{
			{
				"tool": map[string]any{
					"driver": map[string]any{
						"name": toolName,
						"guid": toolGUID,
						"rules": []map[string]any{
							{
								"id":                   "cs-guid-rule",
								"fullDescription":      map[string]any{"text": "test desc"},
								"defaultConfiguration": map[string]any{"level": "error"},
							},
						},
					},
				},
				"results": []map[string]any{
					{
						"ruleId":  "cs-guid-rule",
						"message": map[string]any{"text": "problem"},
					},
				},
			},
		},
	}
	seedSARIFUploadOnServer(t, s, repo.FullName, sarif)

	token := adminTokenFor(s)
	alerts := mustDecodeJSONList(t, pagedJSONRequest(t, s, "GET",
		"/api/v3/repos/"+repo.FullName+"/code-scanning/alerts",
		token, nil).Body.Bytes())
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}
	tool := alerts[0]["tool"].(map[string]any)
	if tool["guid"] != toolGUID {
		t.Errorf("alert tool.guid = %v, want %s", tool["guid"], toolGUID)
	}

	analyses := mustDecodeJSONList(t, pagedJSONRequest(t, s, "GET",
		"/api/v3/repos/"+repo.FullName+"/code-scanning/analyses",
		token, nil).Body.Bytes())
	if len(analyses) != 1 {
		t.Fatalf("expected 1 analysis, got %d", len(analyses))
	}
	tool = analyses[0]["tool"].(map[string]any)
	if tool["guid"] != toolGUID {
		t.Errorf("analysis tool.guid = %v, want %s", tool["guid"], toolGUID)
	}
}

func seedSARIFUploadOnServer(t *testing.T, s *Server, repoFullName string, sarif map[string]any) {
	t.Helper()
	token := AdminToken()
	commitSHA := putRepoFileOnServer(t, s, repoFullName, "src/rule.js", "const x = 1;\n", "seed")
	sarifBytes, _ := json.Marshal(sarif)
	w := pagedJSONRequest(t, s, "POST", "/api/v3/repos/"+repoFullName+"/code-scanning/sarifs", token, map[string]any{
		"commit_sha": commitSHA,
		"ref":        "refs/heads/main",
		"sarif":      base64.StdEncoding.EncodeToString(sarifBytes),
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("upload SARIF: %d body=%s", w.Code, w.Body.String())
	}
}

func putRepoFileOnServer(t *testing.T, s *Server, repoFullName, path, content, message string) string {
	t.Helper()
	token := AdminToken()
	w := pagedJSONRequest(t, s, "PUT", "/api/v3/repos/"+repoFullName+"/contents/"+path, token, map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("put contents %s: %d body=%s", path, w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	commit := resp["commit"].(map[string]any)
	return commit["sha"].(string)
}

// TestCodeScanning_OrgAlertFilters verifies the org-level code-scanning alert
// endpoint accepts and applies severity, tool_name, and state query filters.
func TestCodeScanning_OrgAlertFilters(t *testing.T) {
	s := newTestServer()
	initPhase8Routes(s)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "cs-org-filters", "CS Filters Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	repo := s.store.CreateOrgRepo(org, admin, "cs-org-repo", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}

	toolGUID := "c7a7c45a-8a3c-4f6e-9b5c-1d3c01234567"
	s.store.CreateCodeScanningAlert(repo.FullName, "rule-a", "error", "desc a", "CodeQL", toolGUID, "open", []CodeScanningAlertInstance{{Path: "a.go", StartLine: 1}})
	s.store.CreateCodeScanningAlert(repo.FullName, "rule-b", "warning", "desc b", "Semgrep", toolGUID, "open", []CodeScanningAlertInstance{{Path: "b.go", StartLine: 1}})

	token := adminTokenFor(s)
	base := "/api/v3/orgs/cs-org-filters/code-scanning/alerts"

	// No filter: both alerts.
	w := pagedJSONRequest(t, s, "GET", base, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list all = %d", w.Code)
	}
	list := mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 2 {
		t.Fatalf("list all = %d, want 2", len(list))
	}

	// Severity filter.
	w = pagedJSONRequest(t, s, "GET", base+"?severity=error", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 1 {
		t.Fatalf("severity=error = %d, want 1", len(list))
	}

	// Tool name filter.
	w = pagedJSONRequest(t, s, "GET", base+"?tool_name=CodeQL", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 1 {
		t.Fatalf("tool_name=CodeQL = %d, want 1", len(list))
	}

	// Unknown state filter: accepted, 200, zero matches.
	w = pagedJSONRequest(t, s, "GET", base+"?state=nonexistent", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("unknown state = %d, want 200", w.Code)
	}
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 0 {
		t.Fatalf("unknown state = %d, want 0", len(list))
	}
}

// TestCustomProperties_RequireExplicitValues verifies that when a property
// definition has require_explicit_values=true, the effective values do not
// fall back to the default value for repos that have not set an explicit value.
func TestCustomProperties_RequireExplicitValues(t *testing.T) {
	s := newTestServer()
	initPhase8Routes(s)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "cp-require-org", "CP Require Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	repo := s.store.CreateOrgRepo(org, admin, "cp-require-repo", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}
	repo2 := s.store.CreateOrgRepo(org, admin, "cp-require-repo2", "", false)
	if repo2 == nil {
		t.Fatal("create repo2 failed")
	}

	token := adminTokenFor(s)

	createProp := map[string]any{
		"property_name":           "team",
		"value_type":              "string",
		"required":                false,
		"default_value":           "default-team",
		"require_explicit_values": true,
	}
	w := pagedJSONRequest(t, s, "PUT", "/api/v3/orgs/cp-require-org/properties/schema/team", token, createProp)
	if w.Code != http.StatusCreated && w.Code != 200 {
		t.Fatalf("create property: %d body=%s", w.Code, w.Body.String())
	}

	w = pagedJSONRequest(t, s, "PATCH", "/api/v3/repos/"+repo.FullName+"/properties/values", token, map[string]any{
		"properties": []map[string]any{
			{"property_name": "team", "value": "explicit-team"},
		},
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("set repo values: %d body=%s", w.Code, w.Body.String())
	}

	w = pagedJSONRequest(t, s, "GET", "/api/v3/repos/"+repo.FullName+"/properties/values", token, nil)
	list := mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 1 {
		t.Fatalf("effective values = %d, want 1", len(list))
	}
	if list[0]["value"] != "explicit-team" {
		t.Errorf("effective value = %v, want explicit-team", list[0]["value"])
	}

	w = pagedJSONRequest(t, s, "GET", "/api/v3/repos/"+repo2.FullName+"/properties/values", token, nil)
	list = mustDecodeJSONList(t, w.Body.Bytes())
	if len(list) != 0 {
		t.Errorf("repo2 with require_explicit_values should have no defaults, got %d items", len(list))
	}
}

// TestCustomProperties_ValuesEditableBy_Accepted verifies that values_editable_by
// is accepted at schema-creation time and echoed back.
func TestCustomProperties_ValuesEditableBy_Accepted(t *testing.T) {
	s := newTestServer()
	initPhase8Routes(s)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "cp-editable-org", "CP Editable Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	token := adminTokenFor(s)

	for _, editableBy := range []string{"org_actors", "org_and_repo_actors"} {
		createProp := map[string]any{
			"property_name":      "env",
			"value_type":         "string",
			"values_editable_by": editableBy,
		}
		w := pagedJSONRequest(t, s, "PUT", "/api/v3/orgs/cp-editable-org/properties/schema/env", token, createProp)
		if w.Code != http.StatusCreated && w.Code != 200 {
			t.Fatalf("create property with values_editable_by=%s: %d", editableBy, w.Code)
		}

		w = pagedJSONRequest(t, s, "GET", "/api/v3/orgs/cp-editable-org/properties/schema/env", token, nil)
		var prop map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &prop); err != nil {
			t.Fatalf("decode schema: %v", err)
		}
		if prop["values_editable_by"] != editableBy {
			t.Errorf("values_editable_by=%s not echoed back, got %v", editableBy, prop["values_editable_by"])
		}
	}
}

// TestDeployments_AutoInactive verifies that creating a deployment status with
// auto_inactive=true marks prior non-transient deployments in the same
// environment as "inactive".
func TestDeployments_AutoInactive(t *testing.T) {
	s := newTestServer()
	initPhase8Routes(s)
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "dep-autoinactive", "dep-test", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}
	token := adminTokenFor(s)

	w := pagedJSONRequest(t, s, "POST", "/api/v3/repos/"+repo.FullName+"/deployments", token, map[string]any{
		"ref":         "main",
		"environment": "production",
		"payload":     map[string]any{},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create deployment 1: %d", w.Code)
	}
	var dep1 map[string]any
	json.Unmarshal(w.Body.Bytes(), &dep1)
	dep1ID := int(dep1["id"].(float64))

	w = pagedJSONRequest(t, s, "POST", "/api/v3/repos/"+repo.FullName+"/deployments", token, map[string]any{
		"ref":         "main",
		"environment": "production",
		"payload":     map[string]any{},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create deployment 2: %d", w.Code)
	}
	var dep2 map[string]any
	json.Unmarshal(w.Body.Bytes(), &dep2)
	dep2ID := int(dep2["id"].(float64))

	w = pagedJSONRequest(t, s, "POST", "/api/v3/repos/"+repo.FullName+"/deployments/"+itoa(dep2ID)+"/statuses", token, map[string]any{
		"state":         "success",
		"auto_inactive": true,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create status with auto_inactive: %d body=%s", w.Code, w.Body.String())
	}

	statuses := mustDecodeJSONList(t, pagedJSONRequest(t, s, "GET",
		"/api/v3/repos/"+repo.FullName+"/deployments/"+itoa(dep1ID)+"/statuses",
		token, nil).Body.Bytes())
	if len(statuses) == 0 {
		t.Fatal("expected dep1 to have at least one status after auto-inactivation")
	}
	foundInactive := false
	for _, st := range statuses {
		if st["state"] == "inactive" {
			foundInactive = true
		}
	}
	if !foundInactive {
		t.Errorf("dep1 was not auto-inactivated; statuses=%v", statuses)
	}
}
