package bleephub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var fixedRulesetTestTime = time.Date(2050, time.January, 2, 3, 4, 5, 0, time.UTC)

func TestRulesets_FullLifecycle(t *testing.T) {
	s := newTestServer()
	s.registerGHRulesetRoutes()

	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "rules-repo", "", false)

	do := func(method, path string, body []byte) *httptest.ResponseRecorder {
		var req *http.Request
		if body != nil {
			req = httptest.NewRequest(method, path, bytes.NewReader(body))
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}

	create, _ := json.Marshal(map[string]any{
		"name":        "protect-main",
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{
				"include": []string{"~DEFAULT_BRANCH"},
			},
		},
		"rules": []map[string]any{
			{"type": "creation"},
			{"type": "deletion"},
			{"type": "required_linear_history"},
		},
	})
	w := do("POST", "/api/v3/repos/admin/rules-repo/rulesets", create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create ruleset: %d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	rsID := int(created["id"].(float64))
	if created["name"] != "protect-main" {
		t.Errorf("name = %v", created["name"])
	}
	if created["source_type"] != "Repository" {
		t.Errorf("source_type = %v", created["source_type"])
	}
	if rules, ok := created["rules"].([]any); !ok || len(rules) != 3 {
		t.Errorf("expected 3 rules, got %v", created["rules"])
	}

	// List
	w = do("GET", "/api/v3/repos/admin/rules-repo/rulesets", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d body=%s", w.Code, w.Body.String())
	}
	var list []map[string]any
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 ruleset, got %d", len(list))
	}
	if _, ok := list[0]["rules"]; ok {
		t.Errorf("list should not include rules by default")
	}

	// Get
	w = do("GET", "/api/v3/repos/admin/rules-repo/rulesets/"+itoa(rsID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["name"] != "protect-main" {
		t.Errorf("get name = %v", got["name"])
	}

	// Update
	update, _ := json.Marshal(map[string]any{
		"enforcement": "evaluate",
		"rules": []map[string]any{
			{"type": "creation"},
		},
	})
	w = do("PUT", "/api/v3/repos/admin/rules-repo/rulesets/"+itoa(rsID), update)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d body=%s", w.Code, w.Body.String())
	}
	var updated map[string]any
	json.Unmarshal(w.Body.Bytes(), &updated)
	if updated["enforcement"] != "evaluate" {
		t.Errorf("enforcement = %v", updated["enforcement"])
	}
	if rules, ok := updated["rules"].([]any); !ok || len(rules) != 1 {
		t.Errorf("expected 1 rule after update, got %v", updated["rules"])
	}

	// History exists after update.
	w = do("GET", "/api/v3/repos/admin/rules-repo/rulesets/"+itoa(rsID)+"/history", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("history: %d body=%s", w.Code, w.Body.String())
	}
	var hist []map[string]any
	json.Unmarshal(w.Body.Bytes(), &hist)
	if len(hist) != 1 {
		t.Errorf("expected 1 history version, got %d", len(hist))
	}

	// List branch rules
	w = do("GET", "/api/v3/repos/admin/rules-repo/rules/branches/main", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("branch rules: %d body=%s", w.Code, w.Body.String())
	}
	var brules []map[string]any
	json.Unmarshal(w.Body.Bytes(), &brules)
	if len(brules) != 1 || brules[0]["type"] != "creation" {
		t.Errorf("expected active creation rule on main, got %+v", brules)
	}

	// Delete
	w = do("DELETE", "/api/v3/repos/admin/rules-repo/rulesets/"+itoa(rsID), nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("delete: %d", w.Code)
	}
	w = do("GET", "/api/v3/repos/admin/rules-repo/rulesets/"+itoa(rsID), nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", w.Code)
	}
}

func TestListBranchRulesPagination(t *testing.T) {
	s := newTestServer()
	s.registerGHRulesetRoutes()

	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "rules-pg-repo", "", false)

	create, _ := json.Marshal(map[string]any{
		"name":        "protect-main-pg",
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{
				"include": []string{"~DEFAULT_BRANCH"},
			},
		},
		"rules": []map[string]any{
			{"type": "creation"},
			{"type": "deletion"},
		},
	})
	do := func(method, path string, body []byte) *httptest.ResponseRecorder {
		var req *http.Request
		if body != nil {
			req = httptest.NewRequest(method, path, bytes.NewReader(body))
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}
	w := do("POST", "/api/v3/repos/admin/rules-pg-repo/rulesets", create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create ruleset: %d body=%s", w.Code, w.Body.String())
	}

	w = do("GET", "/api/v3/repos/admin/rules-pg-repo/rules/branches/main?per_page=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("branch rules page 1: %d body=%s", w.Code, w.Body.String())
	}
	var page1 []map[string]any
	json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1) != 1 {
		t.Fatalf("expected 1 rule on page 1, got %d", len(page1))
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("expected rel=next in Link, got %s", link)
	}

	w = do("GET", "/api/v3/repos/admin/rules-pg-repo/rules/branches/main?per_page=1&page=2", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("branch rules page 2: %d body=%s", w.Code, w.Body.String())
	}
	var page2 []map[string]any
	json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2) != 1 {
		t.Fatalf("expected 1 rule on page 2, got %d", len(page2))
	}
	if page2[0]["type"] == page1[0]["type"] {
		t.Fatalf("expected distinct rules across pages, got %v twice", page1[0]["type"])
	}
}

func TestRulesets_ListIncludesParentsTargetsAndPagination(t *testing.T) {
	s := newTestServer()
	s.registerGHRulesetRoutes()
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "ruleset-list-org", "", "")
	repo := s.store.CreateOrgRepo(org, admin, "ruleset-list-repo", "", false)
	parent := s.store.CreateOrgRuleset(org.ID, "parent-tags", "tag", "active", store.RulesetConditions{}, []store.Rule{{Type: "deletion"}}, nil)
	local := s.store.CreateRuleset(repo, &store.Ruleset{
		Name: "local-branches", Target: "branch", Enforcement: "active", Rules: []store.Rule{{Type: "deletion"}},
	})

	request := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v3/repos/ruleset-list-org/ruleset-list-repo/rulesets?"+query, nil)
		req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}
	decode := func(w *httptest.ResponseRecorder) []map[string]interface{} {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("list = %d body=%s", w.Code, w.Body.String())
		}
		var list []map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return list
	}

	if list := decode(request("")); len(list) != 2 ||
		int(list[0]["id"].(float64)) != parent.ID || int(list[1]["id"].(float64)) != local.ID {
		t.Fatalf("default parent-inclusive list = %#v", list)
	}
	if list := decode(request("includes_parents=false")); len(list) != 1 || list[0]["source_type"] != "Repository" {
		t.Fatalf("parent-free list = %#v", list)
	}
	if list := decode(request("targets=tag")); len(list) != 1 || list[0]["source_type"] != "Organization" {
		t.Fatalf("tag-target list = %#v", list)
	}
	page := request("per_page=1&page=2")
	if list := decode(page); len(list) != 1 || int(list[0]["id"].(float64)) != local.ID || page.Header().Get("Link") == "" {
		t.Fatalf("paginated list = %#v Link=%q", list, page.Header().Get("Link"))
	}
	for _, query := range []string{"includes_parents=yes", "targets=branch,unknown", "page=0"} {
		if got := request(query); got.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d body=%s, want 422", query, got.Code, got.Body.String())
		}
	}
}

func TestRulesets_CreateMissingName(t *testing.T) {
	s := newTestServer()
	s.registerGHRulesetRoutes()

	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "rules-repo2", "", false)

	body, _ := json.Marshal(map[string]any{
		"target": "branch",
	})
	req := httptest.NewRequest("POST", "/api/v3/repos/admin/rules-repo2/rulesets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRulesets_OrgFullLifecycle(t *testing.T) {
	s := newTestServer()
	s.registerGHRulesetRoutes()

	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "rules-org", "Rules Org", "")
	if org == nil {
		t.Fatal("failed to create org")
	}

	do := func(method, path string, body []byte) *httptest.ResponseRecorder {
		var req *http.Request
		if body != nil {
			req = httptest.NewRequest(method, path, bytes.NewReader(body))
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}

	create, _ := json.Marshal(map[string]any{
		"name":        "protect-main",
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{
				"include": []string{"~ALL"},
			},
		},
		"rules": []map[string]any{
			{"type": "creation"},
			{"type": "deletion"},
		},
	})
	w := do("POST", "/api/v3/orgs/rules-org/rulesets", create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create org ruleset: %d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	rsID := int(created["id"].(float64))
	if created["name"] != "protect-main" {
		t.Errorf("name = %v", created["name"])
	}
	if created["source_type"] != "Organization" {
		t.Errorf("source_type = %v", created["source_type"])
	}
	if created["source"] != "rules-org" {
		t.Errorf("source = %v", created["source"])
	}
	if rules, ok := created["rules"].([]any); !ok || len(rules) != 2 {
		t.Errorf("expected 2 rules, got %v", created["rules"])
	}

	// List
	w = do("GET", "/api/v3/orgs/rules-org/rulesets", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d body=%s", w.Code, w.Body.String())
	}
	var list []map[string]any
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 ruleset, got %d", len(list))
	}
	if _, ok := list[0]["rules"]; ok {
		t.Errorf("list should not include rules by default")
	}

	// Get
	w = do("GET", "/api/v3/orgs/rules-org/rulesets/"+itoa(rsID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["name"] != "protect-main" {
		t.Errorf("get name = %v", got["name"])
	}

	// Update
	update, _ := json.Marshal(map[string]any{
		"enforcement": "evaluate",
		"rules": []map[string]any{
			{"type": "creation"},
		},
	})
	w = do("PUT", "/api/v3/orgs/rules-org/rulesets/"+itoa(rsID), update)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d body=%s", w.Code, w.Body.String())
	}
	var updated map[string]any
	json.Unmarshal(w.Body.Bytes(), &updated)
	if updated["enforcement"] != "evaluate" {
		t.Errorf("enforcement = %v", updated["enforcement"])
	}
	if rules, ok := updated["rules"].([]any); !ok || len(rules) != 1 {
		t.Errorf("expected 1 rule after update, got %v", updated["rules"])
	}

	// History exists after update.
	w = do("GET", "/api/v3/orgs/rules-org/rulesets/"+itoa(rsID)+"/history", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("history: %d body=%s", w.Code, w.Body.String())
	}
	var hist []map[string]any
	json.Unmarshal(w.Body.Bytes(), &hist)
	if len(hist) != 1 {
		t.Errorf("expected 1 history version, got %d", len(hist))
	}

	// List rule suites returns empty list.
	w = do("GET", "/api/v3/orgs/rules-org/rulesets/rule-suites", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("rule suites: %d body=%s", w.Code, w.Body.String())
	}
	var suites []map[string]any
	json.Unmarshal(w.Body.Bytes(), &suites)
	if len(suites) != 0 {
		t.Errorf("expected empty rule suites, got %+v", suites)
	}

	// Delete
	w = do("DELETE", "/api/v3/orgs/rules-org/rulesets/"+itoa(rsID), nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("delete: %d", w.Code)
	}
	w = do("GET", "/api/v3/orgs/rules-org/rulesets/"+itoa(rsID), nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", w.Code)
	}
}

func TestRulesets_OrgNonAdminCannotCreate(t *testing.T) {
	s := newTestServer()
	s.registerGHRulesetRoutes()

	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "rules-org2", "Rules Org 2", "")
	_ = org

	// Create a non-admin member.
	member := &store.User{ID: 999, Login: "member-user", Email: "member@example.com"}
	s.store.Users[member.ID] = member
	s.store.UsersByLogin[member.Login] = member
	s.store.SetMembership("rules-org2", member.ID, store.OrgRoleMember, store.MembershipStateActive)
	tok := s.store.CreateToken(member.ID, "repo,read:org")

	body, _ := json.Marshal(map[string]any{
		"name":        "protect-main",
		"target":      "branch",
		"enforcement": "active",
	})
	req := httptest.NewRequest("POST", "/api/v3/orgs/rules-org2/rulesets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRulesets_BranchRuleExclusion(t *testing.T) {
	s := newTestServer()
	s.registerGHRulesetRoutes()

	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "rules-repo3", "", false)
	_ = repo

	create, _ := json.Marshal(map[string]any{
		"name":        "skip-release",
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{
				"include": []string{"~ALL"},
				"exclude": []string{"release/*"},
			},
		},
		"rules": []map[string]any{
			{"type": "deletion"},
		},
	})
	req := httptest.NewRequest("POST", "/api/v3/repos/admin/rules-repo3/rulesets", bytes.NewReader(create))
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v3/repos/admin/rules-repo3/rules/branches/release/v1", nil)
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	s.requestHandler().ServeHTTP(w, req)
	var rules []map[string]any
	json.Unmarshal(w.Body.Bytes(), &rules)
	if len(rules) != 0 {
		t.Errorf("expected no rules on excluded branch, got %+v", rules)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v3/repos/admin/rules-repo3/rules/branches/main", nil)
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	s.requestHandler().ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &rules)
	if len(rules) != 1 {
		t.Errorf("expected 1 rule on main, got %+v", rules)
	}
}

func TestRulesets_ActiveRulesBlockRefWritesAndRecordOfficialSuiteShape(t *testing.T) {
	s := newTestServer()
	s.replaceClockNow(func() time.Time { return fixedRulesetTestTime })
	s.registerGHRulesetRoutes()

	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "ruleset-evaluation", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	base, err := initRepoWithFiles(stor, "main", "root", map[string]string{"README.md": "root"}, &object.Signature{
		Name: "admin", Email: "admin@example.com", When: fixedRulesetTestTime,
	})
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	s.store.CreateRuleset(repo, &store.Ruleset{
		Name:        "no-feature-creation",
		Target:      "branch",
		Enforcement: "active",
		Conditions: store.RulesetConditions{RefName: store.RefNameCondition{
			Include: []string{"feature"},
		}},
		Rules: []store.Rule{{Type: "creation"}},
	})

	body, _ := json.Marshal(map[string]interface{}{
		"ref": "refs/heads/feature",
		"sha": base.String(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v3/repos/admin/ruleset-evaluation/git/refs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w := httptest.NewRecorder()
	s.registerGHGitDataRoutes()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("active creation rule admitted a matching branch creation: %d body=%s", w.Code, w.Body.String())
	}
	if _, err := stor.Reference(plumbing.NewBranchReferenceName("feature")); err == nil {
		t.Fatal("ref was created despite the active creation rule")
	}

	suites := s.store.ListRepoRulesetSuites(repo.ID)
	if len(suites) != 1 {
		t.Fatalf("recorded suites = %d, want 1", len(suites))
	}
	suite := suites[0]
	if suite.Result != "fail" || suite.EvaluationResult != nil {
		t.Fatalf("suite result = %q evaluation_result=%v, want fail/null", suite.Result, suite.EvaluationResult)
	}
	if len(suite.RuleEvaluations) != 1 || suite.RuleEvaluations[0].RuleType != "creation" ||
		suite.RuleEvaluations[0].Result != "fail" {
		t.Fatalf("rule evaluations = %#v", suite.RuleEvaluations)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v3/repos/admin/ruleset-evaluation/rulesets/rule-suites", nil)
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list suites = %d body=%s", w.Code, w.Body.String())
	}
	var list []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode suites: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("suite list = %#v", list)
	}
	for _, field := range []string{"id", "actor_id", "actor_name", "before_sha", "after_sha", "ref", "repository_id", "repository_name", "pushed_at", "result", "evaluation_result"} {
		if _, ok := list[0][field]; !ok {
			t.Errorf("suite list item omitted %q: %#v", field, list[0])
		}
	}
	for _, obsolete := range []string{"node_id", "ruleset_id", "status", "created_at", "updated_at", "rule_evaluations"} {
		if _, ok := list[0][obsolete]; ok {
			t.Errorf("suite list item exposed non-list field %q: %#v", obsolete, list[0])
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v3/repos/admin/ruleset-evaluation/rulesets/rule-suites/"+itoa(suite.ID), nil)
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get suite = %d body=%s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode suite: %v", err)
	}
	if evaluations, ok := detail["rule_evaluations"].([]interface{}); !ok || len(evaluations) != 1 {
		t.Fatalf("detailed rule_evaluations = %#v", detail["rule_evaluations"])
	}
}

func TestRulesets_InstallationTokenRequiresAdministrationGrant(t *testing.T) {
	s := newTestServer()
	s.replaceClockNow(func() time.Time { return fixedRulesetTestTime })
	s.registerGHRulesetRoutes()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "ruleset-installation-auth", "", true)
	app := s.store.CreateApp(admin.ID, "ruleset-app", "", map[string]string{"administration": "write"}, nil)
	installation := s.store.CreateInstallation(app.ID, "User", admin.ID, admin.Login, app.Permissions, nil)
	metadataOnly := s.store.CreateInstallationToken(installation.ID, app.ID, map[string]string{"metadata": "read"}, []int{repo.ID})
	adminWrite := s.store.CreateInstallationToken(installation.ID, app.ID, map[string]string{"administration": "write"}, []int{repo.ID})

	request := func(method, token string, payload []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v3/repos/admin/ruleset-installation-auth/rulesets", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}
	if got := request(http.MethodGet, metadataOnly.Token, nil); got.Code != http.StatusForbidden {
		t.Fatalf("metadata-only installation list = %d body=%s, want 403", got.Code, got.Body.String())
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"name": "installed-rule", "target": "branch", "enforcement": "active",
		"rules": []map[string]interface{}{{"type": "deletion"}},
	})
	if got := request(http.MethodPost, metadataOnly.Token, payload); got.Code != http.StatusForbidden {
		t.Fatalf("metadata-only installation create = %d body=%s, want 403", got.Code, got.Body.String())
	}
	if got := request(http.MethodPost, adminWrite.Token, payload); got.Code != http.StatusCreated {
		t.Fatalf("administration-write installation create = %d body=%s, want 201", got.Code, got.Body.String())
	}
}

func TestRulesets_ContentsAPIUsesTheSameRefWriteGate(t *testing.T) {
	s := newTestServer()
	s.replaceClockNow(func() time.Time { return fixedRulesetTestTime })
	s.registerGHRulesetRoutes()
	s.registerGHRepoObjectRoutes()

	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "ruleset-contents-gate", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	base, err := initRepoWithFiles(stor, "main", "root", map[string]string{"README.md": "root"}, &object.Signature{
		Name: "admin", Email: "admin@example.com", When: fixedRulesetTestTime,
	})
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	blobSHA, ok := contentBlobSHA(stor, "main", "README.md")
	if !ok {
		t.Fatal("seeded README was not readable")
	}
	s.store.CreateRuleset(repo, &store.Ruleset{
		Name:        "no-direct-updates",
		Target:      "branch",
		Enforcement: "active",
		Conditions: store.RulesetConditions{RefName: store.RefNameCondition{
			Include: []string{"main"},
		}},
		Rules: []store.Rule{{Type: "update"}},
	})

	body, _ := json.Marshal(map[string]interface{}{
		"message": "attempt direct edit",
		"content": base64.StdEncoding.EncodeToString([]byte("changed")),
		"sha":     blobSHA,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v3/repos/admin/ruleset-contents-gate/contents/README.md", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("contents update bypassed active ruleset: %d body=%s", w.Code, w.Body.String())
	}
	ref, err := stor.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatalf("read main after rejection: %v", err)
	}
	if ref.Hash() != base {
		t.Fatalf("rejected contents update moved main from %s to %s", base, ref.Hash())
	}
	suites := s.store.ListRepoRulesetSuites(repo.ID)
	if len(suites) != 1 || suites[0].Result != "fail" || suites[0].AfterSHA == base.String() {
		t.Fatalf("recorded contents evaluation = %#v", suites)
	}
}

func TestRulesets_EvaluateModeRecordsFailureWithoutBlocking(t *testing.T) {
	s := newTestServer()
	s.replaceClockNow(func() time.Time { return fixedRulesetTestTime })
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "ruleset-evaluate-mode", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	base, err := initRepoWithFiles(stor, "main", "root", map[string]string{"README.md": "root"}, &object.Signature{
		Name: "admin", Email: "admin@example.com", When: fixedRulesetTestTime,
	})
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	s.store.CreateRuleset(repo, &store.Ruleset{
		Name:        "observe-feature-creation",
		Target:      "branch",
		Enforcement: "evaluate",
		Conditions: store.RulesetConditions{RefName: store.RefNameCondition{
			Include: []string{"feature"},
		}},
		Rules: []store.Rule{{Type: "creation"}},
	})

	ctx := contextWithUser(context.Background(), admin)
	if refusal := s.evaluateRulesetsForRefWrite(ctx, repo, stor, plumbing.NewBranchReferenceName("feature"), refCreation, base); refusal != "" {
		t.Fatalf("evaluate-mode rule blocked the write: %s", refusal)
	}
	suites := s.store.ListRepoRulesetSuites(repo.ID)
	if len(suites) != 1 || suites[0].Result != "pass" || suites[0].EvaluationResult == nil || *suites[0].EvaluationResult != "fail" {
		t.Fatalf("evaluate-mode suite = %#v", suites)
	}
}

func TestRulesets_BypassAndMixedModeSuiteSemantics(t *testing.T) {
	s := newTestServer()
	s.replaceClockNow(func() time.Time { return fixedRulesetTestTime })
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "ruleset-bypass", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	base, err := initRepoWithFiles(stor, "main", "root", map[string]string{"README.md": "root"}, &object.Signature{
		Name: "admin", Email: "admin@example.com", When: fixedRulesetTestTime,
	})
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	s.store.CreateRuleset(repo, &store.Ruleset{
		Name: "bypass-create", Target: "branch", Enforcement: "active",
		BypassActors: []store.RulesetBypassActor{{ActorID: admin.ID, ActorType: "User", BypassMode: "always"}},
		Rules:        []store.Rule{{Type: "creation"}},
	})
	s.store.CreateRuleset(repo, &store.Ruleset{
		Name: "observe-create", Target: "branch", Enforcement: "evaluate",
		Rules: []store.Rule{{Type: "creation"}},
	})

	ctx := contextWithUser(context.Background(), admin)
	if refusal := s.evaluateRulesetsForRefWrite(ctx, repo, stor, plumbing.NewBranchReferenceName("feature"), refCreation, base); refusal != "" {
		t.Fatalf("bypass actor was refused: %s", refusal)
	}
	suites := s.store.ListRepoRulesetSuites(repo.ID)
	if len(suites) != 1 || suites[0].Result != "bypass" || suites[0].EvaluationResult == nil || *suites[0].EvaluationResult != "fail" {
		t.Fatalf("mixed-mode bypass suite = %#v", suites)
	}
	if !rulesetSuiteHasEnforcement(&suites[0], "active") || !rulesetSuiteHasEnforcement(&suites[0], "evaluate") {
		t.Fatalf("mixed suite was not discoverable through both enforcement filters: %#v", suites[0])
	}
}

func TestRulesets_SuiteFiltersAndPaginationAreValidated(t *testing.T) {
	s := newTestServer()
	s.replaceClockNow(func() time.Time { return fixedRulesetTestTime.Add(2 * time.Minute) })
	s.registerGHRulesetRoutes()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "ruleset-suite-filtering", "", false)
	fail := "fail"
	s.store.RecordRulesetSuite(repo, admin, "refs/heads/main", plumbing.ZeroHash.String(), strings.Repeat("1", 40), "pass", nil, nil, fixedRulesetTestTime)
	s.store.RecordRulesetSuite(repo, admin, "refs/heads/release", strings.Repeat("1", 40), strings.Repeat("2", 40), "pass", &fail, nil, fixedRulesetTestTime.Add(time.Minute))

	request := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v3/repos/admin/ruleset-suite-filtering/rulesets/rule-suites?"+query, nil)
		req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}
	w := request("ref=release&evaluate_status=evaluate&rule_suite_result=pass&per_page=1")
	if w.Code != http.StatusOK {
		t.Fatalf("filtered list = %d body=%s", w.Code, w.Body.String())
	}
	var suites []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &suites); err != nil || len(suites) != 1 || suites[0]["ref"] != "refs/heads/release" {
		t.Fatalf("filtered suites = %#v error=%v", suites, err)
	}
	for _, query := range []string{"page=0", "per_page=nope", "time_period=year", "evaluate_status=disabled", "rule_suite_result=unknown", "ref=release%2F*"} {
		if got := request(query); got.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d body=%s, want 422", query, got.Code, got.Body.String())
		}
	}
}

func TestRulesets_HistoryAndSuitesSurvivePersistenceReload(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dataDir)

	p1, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("attach persistence: %v", err)
	}
	st1.SeedDefaultUser()
	admin := st1.UsersByLogin["admin"]
	org := st1.CreateOrg(admin, "ruleset-persist-org", "", "")
	repo := st1.CreateOrgRepo(org, admin, "ruleset-persist-repo", "", false)
	ruleset := st1.CreateRuleset(repo, &store.Ruleset{
		Name: "persisted", Target: "branch", Enforcement: "active",
		Rules: []store.Rule{{Type: "deletion"}},
	})
	st1.UpdateRuleset(repo, ruleset, &store.Ruleset{Enforcement: "evaluate"}, admin.ID)
	st1.RecordRulesetSuite(
		repo, admin, "refs/heads/main", strings.Repeat("1", 40), strings.Repeat("2", 40),
		"pass", nil, []store.RulesetEvaluation{{
			RuleSource:  store.RulesetEvaluationSource{Type: "ruleset", ID: intPointer(ruleset.ID), Name: stringPointer(ruleset.Name)},
			Enforcement: "active", Result: "pass", RuleType: "deletion",
		}}, fixedRulesetTestTime,
	)
	if err := p1.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	p2, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("reopen persistence: %v", err)
	}
	defer p2.Close()
	st2 := store.NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload persistence: %v", err)
	}
	reloaded := st2.GetRuleset(ruleset.ID)
	if reloaded == nil {
		t.Fatal("ruleset did not survive reload")
	}
	if history := st2.GetRulesetHistory(reloaded); len(history) != 1 || history[0].Ruleset.Enforcement != "active" {
		t.Fatalf("ruleset history after reload = %#v", history)
	}
	suites := st2.ListRepoRulesetSuites(repo.ID)
	if len(suites) != 1 || suites[0].Ref != "refs/heads/main" || len(suites[0].RuleEvaluations) != 1 {
		t.Fatalf("ruleset suites after reload = %#v", suites)
	}
}
