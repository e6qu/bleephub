package bleephub

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestEnterpriseActionsPermissionsRoundTrip(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseActionsRoutes()
	admin := s.store.LookupUserByLogin("admin")
	orgA := s.store.CreateOrg(admin, "enterprise-permissions-a", "Permissions A", "")
	orgB := s.store.CreateOrg(admin, "enterprise-permissions-b", "Permissions B", "")

	base := "/api/v3/enterprises/bleephub/actions/permissions"
	rec := enterpriseActionsRequest(t, s, http.MethodGet, base, nil)
	defaults := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || defaults["enabled_organizations"] != "all" ||
		defaults["allowed_actions"] != "all" || defaults["sha_pinning_required"] != false {
		t.Fatalf("default enterprise permissions: got %d %#v", rec.Code, defaults)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPut, base, map[string]interface{}{
		"enabled_organizations": "selected",
		"allowed_actions":       "selected",
		"sha_pinning_required":  true,
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set enterprise permissions: got %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base, nil)
	policy := decodeRecorderObject(t, rec)
	if policy["enabled_organizations"] != "selected" || policy["allowed_actions"] != "selected" ||
		policy["sha_pinning_required"] != true {
		t.Fatalf("enterprise policy = %#v", policy)
	}
	for field, suffix := range map[string]string{
		"selected_organizations_url": base + "/organizations",
		"selected_actions_url":       base + "/selected-actions",
	} {
		value, _ := policy[field].(string)
		if !strings.HasSuffix(value, suffix) {
			t.Fatalf("%s = %q, want suffix %q", field, value, suffix)
		}
	}

	organizations := base + "/organizations"
	rec = enterpriseActionsRequest(t, s, http.MethodPut, organizations, map[string]interface{}{
		"selected_organization_ids": []int{orgA.ID},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set selected organizations: got %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPut,
		organizations+"/"+strconv.Itoa(orgB.ID), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("add selected organization: got %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, organizations, nil)
	selected := decodeRecorderObject(t, rec)
	if selected["total_count"] != float64(2) {
		t.Fatalf("selected organizations = %#v", selected)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete,
		organizations+"/"+strconv.Itoa(orgA.ID), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove selected organization: got %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPut, organizations+"/999999", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("add unknown organization: got %d %q, want 404", rec.Code, rec.Body.String())
	}

	selectedActions := base + "/selected-actions"
	rec = enterpriseActionsRequest(t, s, http.MethodPut, selectedActions, map[string]interface{}{
		"github_owned_allowed": true,
		"verified_allowed":     false,
		"patterns_allowed":     []string{"actions/*", "acme/reusable@*"},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set selected actions: got %d %q", rec.Code, rec.Body.String())
	}
	allowed := decodeRecorderObject(t,
		enterpriseActionsRequest(t, s, http.MethodGet, selectedActions, nil))
	if allowed["github_owned_allowed"] != true ||
		len(allowed["patterns_allowed"].([]interface{})) != 2 {
		t.Fatalf("selected actions = %#v", allowed)
	}

	workflow := base + "/workflow"
	rec = enterpriseActionsRequest(t, s, http.MethodPut, workflow, map[string]interface{}{
		"default_workflow_permissions":     "write",
		"can_approve_pull_request_reviews": true,
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set workflow permissions: got %d %q", rec.Code, rec.Body.String())
	}
	gotWorkflow := decodeRecorderObject(t,
		enterpriseActionsRequest(t, s, http.MethodGet, workflow, nil))
	if gotWorkflow["default_workflow_permissions"] != "write" ||
		gotWorkflow["can_approve_pull_request_reviews"] != true {
		t.Fatalf("workflow permissions = %#v", gotWorkflow)
	}

	retention := base + "/artifact-and-log-retention"
	rec = enterpriseActionsRequest(t, s, http.MethodPut, retention, map[string]interface{}{"days": 120})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set retention: got %d %q", rec.Code, rec.Body.String())
	}
	gotRetention := decodeRecorderObject(t,
		enterpriseActionsRequest(t, s, http.MethodGet, retention, nil))
	if gotRetention["days"] != float64(120) ||
		gotRetention["maximum_allowed_days"] != float64(enterpriseArtifactAndLogRetentionMaxDays) {
		t.Fatalf("retention = %#v", gotRetention)
	}

	approval := base + "/fork-pr-contributor-approval"
	rec = enterpriseActionsRequest(t, s, http.MethodPut, approval, map[string]interface{}{
		"approval_policy": "all_external_contributors",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set fork approval: got %d %q", rec.Code, rec.Body.String())
	}
	if got := decodeRecorderObject(t,
		enterpriseActionsRequest(t, s, http.MethodGet, approval, nil))["approval_policy"]; got != "all_external_contributors" {
		t.Fatalf("approval_policy = %#v", got)
	}

	forks := base + "/fork-pr-workflows-private-repos"
	rec = enterpriseActionsRequest(t, s, http.MethodPut, forks, map[string]interface{}{
		"run_workflows_from_fork_pull_requests":  true,
		"send_write_tokens_to_workflows":         true,
		"send_secrets_and_variables":             false,
		"require_approval_for_fork_pr_workflows": true,
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set private fork workflow permissions: got %d %q", rec.Code, rec.Body.String())
	}
	gotForks := decodeRecorderObject(t,
		enterpriseActionsRequest(t, s, http.MethodGet, forks, nil))
	if gotForks["run_workflows_from_fork_pull_requests"] != true ||
		gotForks["send_write_tokens_to_workflows"] != true ||
		gotForks["require_approval_for_fork_pr_workflows"] != true {
		t.Fatalf("private fork workflow permissions = %#v", gotForks)
	}

	selfHosted := base + "/self-hosted-runners"
	rec = enterpriseActionsRequest(t, s, http.MethodPut, selfHosted, map[string]interface{}{
		"disable_self_hosted_runners_for_all_orgs": true,
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set self-hosted runner policy: got %d %q", rec.Code, rec.Body.String())
	}
	if got := decodeRecorderObject(t,
		enterpriseActionsRequest(t, s, http.MethodGet, selfHosted, nil))["disable_self_hosted_runners_for_all_orgs"]; got != true {
		t.Fatalf("disable_self_hosted_runners_for_all_orgs = %#v", got)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPut,
		"/api/v3/enterprises/bleephub/actions/oidc/customization/issuer",
		map[string]interface{}{"include_enterprise_slug": true})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set OIDC issuer: got %d %q", rec.Code, rec.Body.String())
	}
	s.store.Mu.RLock()
	includeSlug := s.store.EnterpriseSettings.OIDCIncludeEnterpriseSlug
	s.store.Mu.RUnlock()
	if !includeSlug {
		t.Fatal("OIDC enterprise slug policy was not persisted in memory")
	}
	if issuer := s.actionsOIDCIssuer(httptest.NewRequest(http.MethodGet, "https://git.example/token", nil)); issuer != "https://git.example/bleephub" {
		t.Fatalf("custom OIDC issuer = %q, want enterprise slug suffix", issuer)
	}

	cache := decodeRecorderObject(t, enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/enterprises/bleephub/actions/cache/usage", nil))
	if cache["total_active_caches_count"] != float64(0) ||
		cache["total_active_caches_size_in_bytes"] != float64(0) {
		t.Fatalf("empty enterprise cache usage = %#v", cache)
	}
}

func TestEnterpriseActionsPermissionsValidateEnumsAndRequiredFields(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseActionsRoutes()
	base := "/api/v3/enterprises/bleephub/actions/permissions"

	for _, tc := range []struct {
		path string
		body map[string]interface{}
	}{
		{base, map[string]interface{}{"enabled_organizations": "sometimes"}},
		{base, map[string]interface{}{"enabled_organizations": "all", "allowed_actions": "github"}},
		{base + "/workflow", map[string]interface{}{"default_workflow_permissions": "admin"}},
		{base + "/artifact-and-log-retention", map[string]interface{}{"days": 0}},
		{base + "/artifact-and-log-retention", map[string]interface{}{"days": 366}},
		{base + "/fork-pr-contributor-approval", map[string]interface{}{"approval_policy": "never"}},
		{base + "/fork-pr-workflows-private-repos", map[string]interface{}{}},
		{base + "/self-hosted-runners", map[string]interface{}{}},
		{base + "/organizations", map[string]interface{}{}},
	} {
		rec := enterpriseActionsRequest(t, s, http.MethodPut, tc.path, tc.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s %#v: got %d %q, want 422", tc.path, tc.body, rec.Code, rec.Body.String())
		}
	}
}

func TestActionsEnablementIsMonotonicAcrossPolicyScopes(t *testing.T) {
	s := newTestServer()
	admin := s.store.LookupUserByLogin("admin")
	orgA := s.store.CreateOrg(admin, "actions-policy-a", "Actions Policy A", "")
	orgB := s.store.CreateOrg(admin, "actions-policy-b", "Actions Policy B", "")
	repoA := s.store.CreateOrgRepo(orgA, admin, "repo", "", false)
	repoB := s.store.CreateOrgRepo(orgB, admin, "repo", "", false)

	if !s.actionsEnabledForRepo(repoA.FullName) || !s.actionsEnabledForRepo(repoB.FullName) {
		t.Fatal("default enterprise policy should enable both repositories")
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsEnabledOrganizations = "none"
	s.store.Mu.Unlock()
	if s.actionsEnabledForRepo(repoA.FullName) {
		t.Fatal("repository was re-enabled beneath enterprise policy none")
	}

	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsEnabledOrganizations = "selected"
	s.store.EnterpriseSettings.ActionsSelectedOrganizationIDs = []int{orgA.ID}
	s.store.Mu.Unlock()
	if !s.actionsEnabledForRepo(repoA.FullName) || s.actionsEnabledForRepo(repoB.FullName) {
		t.Fatal("enterprise selected-organizations policy was not enforced")
	}

	orgPolicy := defaultOrgActionsPermissions()
	orgPolicy.EnabledRepositories = "none"
	s.store.SetOrgActionsPermissions(orgA.Login, orgPolicy)
	if s.actionsEnabledForRepo(repoA.FullName) {
		t.Fatal("repository was re-enabled beneath organization policy none")
	}
	selectedOrgPolicy := defaultOrgActionsPermissions()
	selectedOrgPolicy.EnabledRepositories = "selected"
	selectedOrgPolicy.SelectedRepositoryIDs = []int{repoA.ID}
	s.store.SetOrgActionsPermissions(orgA.Login, selectedOrgPolicy)
	if !s.actionsEnabledForRepo(repoA.FullName) {
		t.Fatal("selected repository was not enabled")
	}

	repoPolicy := defaultRepoActionsPermissions()
	repoPolicy.Enabled = false
	s.store.SetRepoActionsPermissions(repoA.FullName, repoPolicy)
	if s.actionsEnabledForRepo(repoA.FullName) {
		t.Fatal("repository policy did not disable Actions")
	}
}

func TestRepositoryRunnerRegistrationHonorsEnterpriseAndOrganizationPolicy(t *testing.T) {
	s := newTestServer()
	s.registerAgentRoutes()
	s.registerGHActionsRoutes()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "runner-policy-org", "Runner Policy Org", "")
	repo := s.store.CreateOrgRepo(org, admin, "repo", "", false)
	repoTokenPath := "/api/v3/repos/" + repo.FullName + "/actions/runners/registration-token"
	orgTokenPath := "/api/v3/orgs/" + org.Login + "/actions/runners/registration-token"

	rec := enterpriseActionsRequest(t, s, http.MethodPost, repoTokenPath, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("default repository runner registration: got %d %q", rec.Code, rec.Body.String())
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsDisableSelfHostedRunners = true
	s.store.Mu.Unlock()
	rec = enterpriseActionsRequest(t, s, http.MethodPost, repoTokenPath, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("enterprise-disabled repository runner: got %d %q, want 403", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, orgTokenPath, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("organization runner should remain allowed: got %d %q", rec.Code, rec.Body.String())
	}

	s.store.Mu.Lock()
	s.store.EnterpriseSettings.ActionsDisableSelfHostedRunners = false
	s.store.Mu.Unlock()
	orgPolicy := defaultOrgActionsPermissions()
	orgPolicy.SelfHostedRunnersEnabledRepositories = "none"
	s.store.SetOrgActionsPermissions(org.Login, orgPolicy)
	rec = enterpriseActionsRequest(t, s, http.MethodPost, repoTokenPath, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("org-disabled repository runner: got %d %q, want 403", rec.Code, rec.Body.String())
	}
}
