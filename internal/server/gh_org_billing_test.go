package bleephub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOrgBillingBudgets_Pagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "billing-budgets-page-org", "billing-budgets-page-org", "")

	for i := 0; i < 2; i++ {
		resp := pagedJSONRequest(t, s, http.MethodPost, "/api/v3/organizations/"+org.Login+"/settings/billing/budgets", defaultToken, map[string]interface{}{
			"budget_amount":      100 + i,
			"budget_scope":       "organization",
			"budget_type":        "ProductPricing",
			"budget_product_sku": "actions",
		})
		if resp.Code != http.StatusOK {
			t.Fatalf("create budget %d: %d", i, resp.Code)
		}
	}

	resp := tokenRequest(s, http.MethodGet, "/api/v3/organizations/"+org.Login+"/settings/billing/budgets?per_page=1", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1: %d", resp.Code)
	}
	link := resp.Header().Get("Link")
	var page1 map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &page1); err != nil {
		t.Fatal(err)
	}
	budgets1, _ := page1["budgets"].([]interface{})
	if len(budgets1) != 1 {
		t.Fatalf("page 1 budgets = %d, want 1", len(budgets1))
	}
	if page1["total_count"] != float64(2) || page1["has_next_page"] != true {
		t.Fatalf("page 1 pagination fields wrong: %v", page1)
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, "/api/v3/organizations/"+org.Login+"/settings/billing/budgets?per_page=1&page=2", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2: %d", resp.Code)
	}
	var page2 map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &page2); err != nil {
		t.Fatal(err)
	}
	budgets2, _ := page2["budgets"].([]interface{})
	if len(budgets2) != 1 {
		t.Fatalf("page 2 budgets = %d, want 1", len(budgets2))
	}
	if page2["total_count"] != float64(2) || page2["has_next_page"] != false {
		t.Fatalf("page 2 pagination fields wrong: %v", page2)
	}
	id1 := budgets1[0].(map[string]interface{})["id"]
	id2 := budgets2[0].(map[string]interface{})["id"]
	if id1 == id2 {
		t.Fatalf("page 1 and page 2 returned the same budget: %v", id1)
	}
}

func TestOrgBillingBudgets_CRUDRoundTrip(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	admin := srv.store.UsersByLogin["admin"]
	org := srv.store.CreateOrg(admin, "billing-budgets-org", "Billing Budgets Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}

	// Create.
	resp := srv.post(t, "/api/v3/organizations/billing-budgets-org/settings/billing/budgets", defaultToken, map[string]interface{}{
		"budget_amount":         500,
		"prevent_further_usage": true,
		"budget_scope":          "organization",
		"budget_entity_name":    "",
		"budget_type":           "ProductPricing",
		"budget_product_sku":    "actions",
		"budget_alerting":       map[string]interface{}{"will_alert": true, "alert_recipients": []string{"admin"}},
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create budget: %d", resp.StatusCode)
	}
	created := decodeJSON(t, resp)
	if created["message"] != "Budget created successfully" {
		t.Fatalf("create message = %v", created["message"])
	}
	budget, _ := created["budget"].(map[string]interface{})
	if budget == nil {
		t.Fatal("create response missing budget")
	}
	budgetID, _ := budget["id"].(string)
	if budgetID == "" {
		t.Fatal("created budget has no id")
	}
	if budget["budget_amount"] != float64(500) || budget["budget_product_sku"] != "actions" {
		t.Fatalf("created budget fields wrong: %v", budget)
	}

	// List.
	resp = srv.get(t, "/api/v3/organizations/billing-budgets-org/settings/billing/budgets", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list budgets: %d", resp.StatusCode)
	}
	list := decodeJSON(t, resp)
	budgets, _ := list["budgets"].([]interface{})
	if len(budgets) != 1 {
		t.Fatalf("expected 1 budget, got %d", len(budgets))
	}
	if list["total_count"] != float64(1) || list["has_next_page"] != false {
		t.Fatalf("list pagination fields wrong: %v", list)
	}

	// Get.
	resp = srv.get(t, "/api/v3/organizations/billing-budgets-org/settings/billing/budgets/"+budgetID, defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("get budget: %d", resp.StatusCode)
	}
	got := decodeJSON(t, resp)
	if got["id"] != budgetID || got["budget_scope"] != "organization" {
		t.Fatalf("get budget mismatch: %v", got)
	}
	alerting, _ := got["budget_alerting"].(map[string]interface{})
	if alerting == nil || alerting["will_alert"] != true {
		t.Fatalf("budget_alerting not round-tripped: %v", got)
	}

	// Update.
	resp = srv.patch(t, "/api/v3/organizations/billing-budgets-org/settings/billing/budgets/"+budgetID, defaultToken, map[string]interface{}{
		"budget_amount":         10,
		"prevent_further_usage": false,
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update budget: %d", resp.StatusCode)
	}
	updated := decodeJSON(t, resp)
	if updated["message"] != "Budget updated successfully" {
		t.Fatalf("update message = %v", updated["message"])
	}
	updatedBudget, _ := updated["budget"].(map[string]interface{})
	if updatedBudget["budget_amount"] != float64(10) || updatedBudget["prevent_further_usage"] != false {
		t.Fatalf("update not applied: %v", updatedBudget)
	}

	// Delete.
	resp = srv.do(t, "DELETE", "/api/v3/organizations/billing-budgets-org/settings/billing/budgets/"+budgetID, defaultToken, nil)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("delete budget: %d", resp.StatusCode)
	}
	deleted := decodeJSON(t, resp)
	if deleted["message"] != "Budget deleted successfully" || deleted["id"] != budgetID {
		t.Fatalf("delete response wrong: %v", deleted)
	}

	// Gone after delete.
	resp = srv.get(t, "/api/v3/organizations/billing-budgets-org/settings/billing/budgets/"+budgetID, defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get deleted budget: %d, want 404", resp.StatusCode)
	}
}

func TestOrgBillingBudgets_Validation(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	admin := srv.store.UsersByLogin["admin"]
	if srv.store.CreateOrg(admin, "billing-budgets-val", "Billing Budgets Val", "") == nil {
		t.Fatal("create org failed")
	}

	// Invalid scope.
	resp := srv.post(t, "/api/v3/organizations/billing-budgets-val/settings/billing/budgets", defaultToken, map[string]interface{}{
		"budget_scope":       "galaxy",
		"budget_product_sku": "actions",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid scope: %d, want 422", resp.StatusCode)
	}

	// Missing SKU.
	resp = srv.post(t, "/api/v3/organizations/billing-budgets-val/settings/billing/budgets", defaultToken, map[string]interface{}{
		"budget_scope": "organization",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing sku: %d, want 422", resp.StatusCode)
	}

	// user scope must set prevent_further_usage.
	resp = srv.post(t, "/api/v3/organizations/billing-budgets-val/settings/billing/budgets", defaultToken, map[string]interface{}{
		"budget_scope":          "user",
		"budget_entity_name":    "admin",
		"budget_product_sku":    "premium_requests",
		"prevent_further_usage": false,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("user scope without prevent_further_usage: %d, want 422", resp.StatusCode)
	}

	// PATCH unknown budget.
	resp = srv.patch(t, "/api/v3/organizations/billing-budgets-val/settings/billing/budgets/550e8400-e29b-41d4-a716-446655440000", defaultToken, map[string]interface{}{
		"budget_amount": 1,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("patch unknown budget: %d, want 404", resp.StatusCode)
	}

	// Non-admin caller is forbidden.
	outsider := srv.createTestUser(t, "billing-outsider")
	srv.store.Tokens["ghp_billing_outsider"] = &Token{Value: "ghp_billing_outsider", UserID: outsider.ID}
	resp = srv.get(t, "/api/v3/organizations/billing-budgets-val/settings/billing/budgets", "ghp_billing_outsider")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin list budgets: %d, want 403", resp.StatusCode)
	}

	// Unknown org.
	resp = srv.get(t, "/api/v3/organizations/billing-no-such-org/settings/billing/budgets", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown org: %d, want 404", resp.StatusCode)
	}
}

func TestOrgBillingUsage_ComputedFromActionsRunHistory(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	admin := srv.store.UsersByLogin["admin"]
	org := srv.store.CreateOrg(admin, "billing-usage-org", "Billing Usage Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	if srv.store.CreateOrgRepo(org, admin, "billing-usage-repo", "", false) == nil {
		t.Fatal("create org repo failed")
	}

	// With no run history the report is honestly empty.
	resp := srv.get(t, "/api/v3/organizations/billing-usage-org/settings/billing/usage?year=2026&month=3", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("usage (empty): %d", resp.StatusCode)
	}
	empty := decodeJSON(t, resp)
	if items, _ := empty["usageItems"].([]interface{}); len(items) != 0 {
		t.Fatalf("expected zero usage items, got %v", items)
	}

	// Record a real workflow run: one job of 150s → billed as 3 minutes.
	started := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	wf := &Workflow{
		ID:           "billing-usage-run",
		RepoFullName: "billing-usage-org/billing-usage-repo",
		Status:       WorkflowStatusCompleted,
		Jobs: map[string]*WorkflowJob{
			"job": {JobID: "billing-usage-job", StartedAt: started, CompletedAt: started.Add(150 * time.Second)},
		},
	}
	srv.store.mu.Lock()
	srv.store.Workflows[wf.ID] = wf
	srv.store.mu.Unlock()
	defer func() {
		srv.store.mu.Lock()
		delete(srv.store.Workflows, wf.ID)
		srv.store.mu.Unlock()
	}()

	resp = srv.get(t, "/api/v3/organizations/billing-usage-org/settings/billing/usage?year=2026&month=3", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("usage: %d", resp.StatusCode)
	}
	report := decodeJSON(t, resp)
	items, _ := report["usageItems"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 usage item, got %v", items)
	}
	item, _ := items[0].(map[string]interface{})
	if item["date"] != "2026-03-10" || item["product"] != "actions" || item["sku"] != "actions_linux" {
		t.Fatalf("usage item identity wrong: %v", item)
	}
	if item["quantity"] != float64(3) || item["unitType"] != "Minutes" {
		t.Fatalf("usage item quantity wrong: %v", item)
	}
	if item["organizationName"] != "billing-usage-org" || item["repositoryName"] != "billing-usage-repo" {
		t.Fatalf("usage item attribution wrong: %v", item)
	}

	// Requesting a different month excludes the run.
	resp = srv.get(t, "/api/v3/organizations/billing-usage-org/settings/billing/usage?year=2026&month=4", defaultToken)
	other := decodeJSON(t, resp)
	if items, _ := other["usageItems"].([]interface{}); len(items) != 0 {
		t.Fatalf("expected zero usage items in other month, got %v", items)
	}

	// Summary aggregates the same real usage.
	resp = srv.get(t, "/api/v3/organizations/billing-usage-org/settings/billing/usage/summary?year=2026&month=3", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("usage summary: %d", resp.StatusCode)
	}
	summary := decodeJSON(t, resp)
	if summary["organization"] != "billing-usage-org" {
		t.Fatalf("summary organization = %v", summary["organization"])
	}
	period, _ := summary["timePeriod"].(map[string]interface{})
	if period == nil || period["year"] != float64(2026) || period["month"] != float64(3) {
		t.Fatalf("summary timePeriod wrong: %v", summary)
	}
	sumItems, _ := summary["usageItems"].([]interface{})
	if len(sumItems) != 1 {
		t.Fatalf("expected 1 summary item, got %v", sumItems)
	}
	sumItem, _ := sumItems[0].(map[string]interface{})
	if sumItem["grossQuantity"] != float64(3) || sumItem["netQuantity"] != float64(3) {
		t.Fatalf("summary quantities wrong: %v", sumItem)
	}

	// Invalid month is rejected.
	resp = srv.get(t, "/api/v3/organizations/billing-usage-org/settings/billing/usage?month=13", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid month: %d, want 400", resp.StatusCode)
	}
}

func TestOrgBillingPremiumRequestAndAICreditUsage_HonestlyEmpty(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	admin := srv.store.UsersByLogin["admin"]
	if srv.store.CreateOrg(admin, "billing-ai-org", "Billing AI Org", "") == nil {
		t.Fatal("create org failed")
	}

	for _, path := range []string{
		"/api/v3/organizations/billing-ai-org/settings/billing/premium_request/usage",
		"/api/v3/organizations/billing-ai-org/settings/billing/ai_credit/usage",
	} {
		resp := srv.get(t, path+"?year=2026&month=3&user=admin", defaultToken)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		report := decodeJSON(t, resp)
		if report["organization"] != "billing-ai-org" || report["user"] != "admin" {
			t.Fatalf("%s attribution wrong: %v", path, report)
		}
		if items, _ := report["usageItems"].([]interface{}); len(items) != 0 {
			t.Fatalf("%s expected empty usageItems, got %v", path, items)
		}
		period, _ := report["timePeriod"].(map[string]interface{})
		if period == nil || period["year"] != float64(2026) {
			t.Fatalf("%s timePeriod wrong: %v", path, report)
		}
	}
}
