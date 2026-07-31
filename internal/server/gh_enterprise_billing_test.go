package bleephub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEnterpriseBillingBudgetAndCostCenterJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseBillingRoutes()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "billing-enterprise-org", "Billing Enterprise Org", "")
	repo := s.store.CreateOrgRepo(org, admin, "service", "", false)
	if org == nil || repo == nil {
		t.Fatal("seed enterprise billing resources")
	}
	team := s.store.CreateEnterpriseTeam("Billing Team", "", "", nil, "")
	base := "/api/v3/enterprises/bleephub/settings/billing"

	rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/cost-centers", map[string]interface{}{
		"name": "Engineering",
	})
	center := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || center["name"] != "Engineering" {
		t.Fatalf("create cost center = %d %#v", rec.Code, center)
	}
	centerID := center["id"].(string)

	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/cost-centers/"+centerID+"/resource",
		map[string]interface{}{
			"users": []string{"admin"}, "organizations": []string{org.Login},
			"repositories": []string{repo.FullName}, "enterprise_teams": []string{team.Slug},
		})
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK || got["reassigned_resources"] == nil {
		t.Fatalf("add resources = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/cost-centers/"+centerID, nil)
	center = decodeRecorderObject(t, rec)
	if resources := center["resources"].([]interface{}); rec.Code != http.StatusOK || len(resources) != 4 {
		t.Fatalf("get cost center = %d %#v", rec.Code, center)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/budgets", map[string]interface{}{
		"budget_amount": 250, "prevent_further_usage": false, "budget_scope": "cost_center",
		"budget_entity_name": centerID, "budget_type": "ProductPricing", "budget_product_sku": "actions",
		"budget_alerting": map[string]interface{}{"will_alert": true, "alert_recipients": []string{"admin"}},
	})
	created := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("create budget = %d %#v", rec.Code, created)
	}
	budgetID := created["budget"].(map[string]interface{})["id"].(string)
	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base+"/budgets/"+budgetID,
		map[string]interface{}{"budget_amount": 400})
	updated := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK ||
		updated["budget"].(map[string]interface{})["budget_amount"] != float64(400) {
		t.Fatalf("update budget = %d %#v", rec.Code, updated)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/cost-centers/"+centerID+"/resource",
		map[string]interface{}{"users": []string{"admin"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove resource = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/budgets/"+budgetID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete budget = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/cost-centers/"+centerID, nil)
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK || got["costCenterState"] != "CostCenterArchived" {
		t.Fatalf("archive cost center = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/cost-centers?state=deleted", nil)
	if rows := decodeRecorderObject(t, rec)["costCenters"].([]interface{}); len(rows) != 1 {
		t.Fatalf("list archived cost centers = %q", rec.Body.String())
	}
}

func TestEnterpriseBillingCostCenterReassignmentAndMultiUserStates(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseBillingRoutes()
	base := "/api/v3/enterprises/bleephub/settings/billing"
	createCenter := func(name string) string {
		rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/cost-centers",
			map[string]interface{}{"name": name})
		return decodeRecorderObject(t, rec)["id"].(string)
	}
	first, second := createCenter("First"), createCenter("Second")
	requireResource := func(id string) map[string]interface{} {
		rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/cost-centers/"+id+"/resource",
			map[string]interface{}{"users": []string{"admin"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("add user to %s = %d %q", id, rec.Code, rec.Body.String())
		}
		return decodeRecorderObject(t, rec)
	}
	requireResource(first)
	reassigned := requireResource(second)["reassigned_resources"].([]interface{})
	if len(reassigned) != 1 || reassigned[0].(map[string]interface{})["previous_cost_center"] != "First" {
		t.Fatalf("reassignment response = %#v", reassigned)
	}

	rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/budgets", map[string]interface{}{
		"budget_amount": 1000, "prevent_further_usage": true, "budget_scope": "multi_user_customer",
		"budget_entity_name": "", "budget_type": "BundlePricing", "budget_product_sku": "ai_credits",
		"budget_alerting": map[string]interface{}{"will_alert": false, "alert_recipients": []string{}},
	})
	budgetID := decodeRecorderObject(t, rec)["budget"].(map[string]interface{})["id"].(string)
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		base+"/budgets/"+budgetID+"/user-states?user=admin&threshold_upper_bound=0", nil)
	states := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || states["total_count"] != float64(1) {
		t.Fatalf("user states = %d %#v", rec.Code, states)
	}
}

func TestEnterpriseBillingUsageAndReportExportJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseBillingRoutes()
	s.replaceClockNow(func() time.Time {
		return time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)
	})
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "enterprise-usage-org", "Enterprise Usage", "")
	repo := s.store.CreateOrgRepo(org, admin, "api", "", false)
	if repo == nil {
		t.Fatal("seed usage repo")
	}
	started := time.Date(2026, time.March, 10, 12, 0, 0, 0, time.UTC)
	s.store.mu.Lock()
	s.store.Workflows["enterprise-usage-run"] = &Workflow{
		ID: "enterprise-usage-run", RepoFullName: repo.FullName, Status: WorkflowStatusCompleted,
		Jobs: map[string]*WorkflowJob{
			"test": {JobID: "enterprise-usage-job", StartedAt: started, CompletedAt: started.Add(70 * time.Second)},
		},
	}
	s.store.mu.Unlock()
	base := "/api/v3/enterprises/bleephub/settings/billing"

	// Unallocated usage is the default detailed report.
	rec := enterpriseActionsRequest(t, s, http.MethodGet, base+"/usage?year=2026&month=3", nil)
	items := decodeRecorderObject(t, rec)["usageItems"].([]interface{})
	if rec.Code != http.StatusOK || len(items) != 1 ||
		items[0].(map[string]interface{})["repositoryName"] != repo.FullName {
		t.Fatalf("enterprise usage = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/usage/summary?year=2026&month=3", nil)
	summary := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK ||
		summary["usageItems"].([]interface{})[0].(map[string]interface{})["grossQuantity"] != float64(2) {
		t.Fatalf("enterprise summary = %d %#v", rec.Code, summary)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/reports", map[string]interface{}{
		"report_type": "detailed", "start_date": "2026-03-01", "end_date": "2026-03-20",
	})
	report := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusAccepted || report["status"] != "processing" {
		t.Fatalf("create report = %d %#v", rec.Code, report)
	}
	reportID := report["id"].(string)
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/reports/"+reportID, nil)
	report = decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || report["status"] != "completed" ||
		len(report["download_urls"].([]interface{})) != 1 {
		t.Fatalf("complete report = %d %#v", rec.Code, report)
	}
	download := httptest.NewRequest(http.MethodGet,
		"/enterprises/bleephub/billing/reports/"+reportID+"/download", nil)
	rec = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(rec, download)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" ||
		!strings.Contains(rec.Body.String(), repo.FullName) {
		t.Fatalf("download report = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/reports", nil)
	if exports := decodeRecorderObject(t, rec)["usage_report_exports"].([]interface{}); len(exports) != 1 {
		t.Fatalf("list reports = %d %q", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/advanced-security", nil)
	advanced := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || advanced["repositories"] == nil {
		t.Fatalf("advanced security billing = %d %#v", rec.Code, advanced)
	}
	for _, suffix := range []string{"/premium_request/usage", "/ai_credit/usage"} {
		rec = enterpriseActionsRequest(t, s, http.MethodGet, base+suffix+"?year=2026&month=3", nil)
		if usage := decodeRecorderObject(t, rec); rec.Code != http.StatusOK ||
			len(usage["usageItems"].([]interface{})) != 0 {
			t.Fatalf("%s = %d %#v", suffix, rec.Code, usage)
		}
	}
}

func TestEnterpriseBillingValidationAndAuthorization(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseBillingRoutes()
	base := "/api/v3/enterprises/bleephub/settings/billing"
	rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/budgets",
		map[string]interface{}{"budget_amount": 1})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("partial budget = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/cost-centers",
		map[string]interface{}{"name": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty cost center name = %d", rec.Code)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/reports",
		map[string]interface{}{"report_type": "detailed", "start_date": "2026-01-01", "end_date": "2026-03-01"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overlong detailed export = %d", rec.Code)
	}
	member := seedTestUser(s, "enterprise-billing-member")
	s.store.mu.Lock()
	s.store.Tokens["enterprise-billing-member-token"] = &Token{
		Value: "enterprise-billing-member-token", UserID: member.ID,
	}
	s.store.mu.Unlock()
	rec = enterpriseBearerRequest(t, s, http.MethodGet, base+"/budgets", nil, "enterprise-billing-member-token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member budgets = %d", rec.Code)
	}
}
