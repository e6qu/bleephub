package bleephub

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EnterpriseCostCenter is a durable enhanced-billing cost allocation. A
// resource belongs to at most one active cost center; adding it elsewhere
// atomically reassigns it, matching GitHub's response contract.
type EnterpriseCostCenter struct {
	ID                  string                         `json:"id"`
	Name                string                         `json:"name"`
	State               string                         `json:"state"`
	Resources           []EnterpriseCostCenterResource `json:"resources"`
	AICreditPoolEnabled bool                           `json:"ai_credit_pool_enabled"`
	CreatedAt           time.Time                      `json:"created_at"`
	UpdatedAt           time.Time                      `json:"updated_at"`
}

type EnterpriseCostCenterResource struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// EnterpriseBillingReport records one asynchronous usage export request.
// Pending reports become completed when read, which gives clients a
// deterministic lifecycle without a wall-clock-dependent background task.
type EnterpriseBillingReport struct {
	ID           string    `json:"id"`
	ReportType   string    `json:"report_type"`
	StartDate    string    `json:"start_date"`
	EndDate      string    `json:"end_date"`
	Status       string    `json:"status"`
	DownloadURLs []string  `json:"download_urls,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Actor        string    `json:"actor"`
}

func (s *Server) registerGHEnterpriseBillingRoutes() {
	owner := s.requireEnterpriseOwner
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/budgets", owner(s.handleListEnterpriseBudgets))
	s.route("POST /api/v3/enterprises/{enterprise}/settings/billing/budgets", owner(s.handleCreateEnterpriseBudget))
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/budgets/{budget_id}", owner(s.handleGetEnterpriseBudget))
	s.route("PATCH /api/v3/enterprises/{enterprise}/settings/billing/budgets/{budget_id}", owner(s.handleUpdateEnterpriseBudget))
	s.route("DELETE /api/v3/enterprises/{enterprise}/settings/billing/budgets/{budget_id}", owner(s.handleDeleteEnterpriseBudget))
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/budgets/{budget_id}/user-states", owner(s.handleEnterpriseBudgetUserStates))

	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/cost-centers", owner(s.handleListEnterpriseCostCenters))
	s.route("POST /api/v3/enterprises/{enterprise}/settings/billing/cost-centers", owner(s.handleCreateEnterpriseCostCenter))
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/cost-centers/{cost_center_id}", owner(s.handleGetEnterpriseCostCenter))
	s.route("PATCH /api/v3/enterprises/{enterprise}/settings/billing/cost-centers/{cost_center_id}", owner(s.handleUpdateEnterpriseCostCenter))
	s.route("DELETE /api/v3/enterprises/{enterprise}/settings/billing/cost-centers/{cost_center_id}", owner(s.handleDeleteEnterpriseCostCenter))
	s.route("POST /api/v3/enterprises/{enterprise}/settings/billing/cost-centers/{cost_center_id}/resource", owner(s.handleAddEnterpriseCostCenterResources))
	s.route("DELETE /api/v3/enterprises/{enterprise}/settings/billing/cost-centers/{cost_center_id}/resource", owner(s.handleRemoveEnterpriseCostCenterResources))

	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/advanced-security", s.requireEnterpriseOwnerOrPermission("enterprise_administration", "write", s.handleEnterpriseAdvancedSecurityBilling))
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/usage", owner(s.handleEnterpriseBillingUsage))
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/usage/summary", owner(s.handleEnterpriseBillingUsageSummary))
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/premium_request/usage", owner(s.handleEnterprisePremiumRequestUsage))
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/ai_credit/usage", owner(s.handleEnterpriseAICreditUsage))

	reports := func(next http.HandlerFunc) http.HandlerFunc {
		return s.requireEnterpriseOwnerOrPermission("enterprise_administration", "write", next)
	}
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/reports", reports(s.handleListEnterpriseBillingReports))
	s.route("POST /api/v3/enterprises/{enterprise}/settings/billing/reports", reports(s.handleCreateEnterpriseBillingReport))
	s.route("GET /api/v3/enterprises/{enterprise}/settings/billing/reports/{report_id}", reports(s.handleGetEnterpriseBillingReport))
	// GitHub returns a short-lived download URL rather than another API
	// endpoint. The UUID report id is the bearer capability in this local
	// implementation, so browser and SDK downloads must not require the API
	// token to be forwarded to a different URL namespace.
	s.route("GET /enterprises/{enterprise}/billing/reports/{report_id}/download", s.handleDownloadEnterpriseBillingReport)
}

var enterpriseBudgetScopes = map[string]bool{
	"enterprise": true, "organization": true, "repository": true, "cost_center": true,
	"multi_user_customer": true, "multi_user_cost_center": true, "user": true,
}

var enterpriseBudgetTypes = map[string]bool{
	"BundlePricing": true, "ProductPricing": true, "SkuPricing": true,
}

func cloneBudget(b *OrgBudget) *OrgBudget {
	if b == nil {
		return nil
	}
	copy := *b
	copy.BudgetAlerting.AlertRecipients = append([]string(nil), b.BudgetAlerting.AlertRecipients...)
	return &copy
}

func (s *Server) enterpriseBudgets() []*OrgBudget {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	out := make([]*OrgBudget, 0, len(s.store.EnterpriseSettings.EnterpriseBudgets))
	for _, budget := range s.store.EnterpriseSettings.EnterpriseBudgets {
		out = append(out, cloneBudget(budget))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func enterpriseBudgetPage(r *http.Request, budgets []*OrgBudget) ([]*OrgBudget, int, bool) {
	page, perPage := 1, 10
	if parsed, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && parsed > 0 {
		page = parsed
	}
	if parsed, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && parsed > 0 && parsed <= 10 {
		perPage = parsed
	}
	total := len(budgets)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return budgets[start:end], total, end < total
}

func (s *Server) handleListEnterpriseBudgets(w http.ResponseWriter, r *http.Request) {
	budgets := s.enterpriseBudgets()
	if scope := r.URL.Query().Get("scope"); scope != "" {
		if !enterpriseBudgetScopes[scope] {
			writeGHError(w, http.StatusBadRequest, "Invalid budget scope")
			return
		}
		filtered := make([]*OrgBudget, 0, len(budgets))
		for _, budget := range budgets {
			if budget.BudgetScope == scope {
				filtered = append(filtered, budget)
			}
		}
		budgets = filtered
	}
	page, total, more := enterpriseBudgetPage(r, budgets)
	rows := make([]map[string]interface{}, len(page))
	for i, budget := range page {
		rows[i] = budgetJSON(budget)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"budgets": rows, "has_next_page": more, "total_count": total,
	})
}

func validateEnterpriseBudgetRequest(s *Server, req *orgBudgetBody, creating bool) (*OrgBudget, bool) {
	budget := &OrgBudget{ID: uuid.NewString(), CreatedAt: s.currentTime()}
	if !creating {
		return budget, true
	}
	if req.BudgetAmount == nil || req.PreventFurtherUsage == nil || req.BudgetScope == nil ||
		req.BudgetType == nil || req.BudgetProductSKU == nil || req.BudgetAlerting == nil ||
		req.BudgetAlerting.WillAlert == nil || req.BudgetAlerting.AlertRecipients == nil {
		return nil, false
	}
	budget.BudgetAmount = *req.BudgetAmount
	budget.PreventFurtherUsage = *req.PreventFurtherUsage
	budget.BudgetScope = *req.BudgetScope
	budget.BudgetType = *req.BudgetType
	budget.BudgetProductSKU = *req.BudgetProductSKU
	if req.BudgetEntityName != nil {
		budget.BudgetEntityName = *req.BudgetEntityName
	}
	if req.User != nil {
		budget.BudgetEntityName = *req.User
	}
	budget.BudgetAlerting = OrgBudgetAlerting{
		WillAlert: *req.BudgetAlerting.WillAlert, AlertRecipients: append([]string(nil), req.BudgetAlerting.AlertRecipients...),
	}
	return budget, true
}

func (s *Server) validateEnterpriseBudget(w http.ResponseWriter, budget *OrgBudget) bool {
	if budget.BudgetAmount < 0 || !enterpriseBudgetScopes[budget.BudgetScope] ||
		!enterpriseBudgetTypes[budget.BudgetType] || budget.BudgetProductSKU == "" {
		writeGHValidationError(w, "Budget", "budget", "invalid")
		return false
	}
	isMulti := budget.BudgetScope == "user" || budget.BudgetScope == "multi_user_customer" ||
		budget.BudgetScope == "multi_user_cost_center"
	if isMulti && (budget.BudgetProductSKU != "ai_credits" && budget.BudgetProductSKU != "premium_requests") {
		writeGHValidationError(w, "Budget", "budget_product_sku", "invalid")
		return false
	}
	if isMulti && !budget.PreventFurtherUsage {
		writeGHValidationError(w, "Budget", "prevent_further_usage", "invalid")
		return false
	}
	switch budget.BudgetScope {
	case "enterprise", "multi_user_customer":
		if budget.BudgetEntityName != "" {
			writeGHValidationError(w, "Budget", "budget_entity_name", "invalid")
			return false
		}
	case "organization":
		if s.store.GetOrg(budget.BudgetEntityName) == nil {
			writeGHValidationError(w, "Budget", "budget_entity_name", "invalid")
			return false
		}
	case "repository":
		if s.store.GetRepoByFullName(budget.BudgetEntityName) == nil {
			writeGHValidationError(w, "Budget", "budget_entity_name", "invalid")
			return false
		}
	case "cost_center", "multi_user_cost_center":
		s.store.mu.RLock()
		center := s.store.EnterpriseSettings.EnterpriseCostCenters[budget.BudgetEntityName]
		s.store.mu.RUnlock()
		if center == nil || center.State != "active" {
			writeGHValidationError(w, "Budget", "budget_entity_name", "invalid")
			return false
		}
	case "user":
		if s.store.LookupUserByLogin(budget.BudgetEntityName) == nil {
			writeGHValidationError(w, "Budget", "budget_entity_name", "invalid")
			return false
		}
	}
	for _, login := range budget.BudgetAlerting.AlertRecipients {
		if s.store.LookupUserByLogin(login) == nil {
			writeGHValidationError(w, "Budget", "budget_alerting.alert_recipients", "invalid")
			return false
		}
	}
	return true
}

func (s *Server) handleCreateEnterpriseBudget(w http.ResponseWriter, r *http.Request) {
	var req orgBudgetBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	budget, ok := validateEnterpriseBudgetRequest(s, &req, true)
	if !ok {
		writeGHValidationError(w, "Budget", "budget", "missing_field")
		return
	}
	if !s.validateEnterpriseBudget(w, budget) {
		return
	}
	s.store.mu.Lock()
	s.store.EnterpriseSettings.EnterpriseBudgets[budget.ID] = budget
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Budget successfully created.", "budget": budgetJSON(budget),
	})
}

func (s *Server) enterpriseBudget(id string) *OrgBudget {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return cloneBudget(s.store.EnterpriseSettings.EnterpriseBudgets[id])
}

func (s *Server) handleGetEnterpriseBudget(w http.ResponseWriter, r *http.Request) {
	budget := s.enterpriseBudget(r.PathValue("budget_id"))
	if budget == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, budgetJSON(budget))
}

func applyEnterpriseBudgetPatch(budget *OrgBudget, req *orgBudgetBody) {
	if req.BudgetAmount != nil {
		budget.BudgetAmount = *req.BudgetAmount
	}
	if req.PreventFurtherUsage != nil {
		budget.PreventFurtherUsage = *req.PreventFurtherUsage
	}
	if req.BudgetScope != nil {
		budget.BudgetScope = *req.BudgetScope
	}
	if req.BudgetEntityName != nil {
		budget.BudgetEntityName = *req.BudgetEntityName
	}
	if req.User != nil {
		budget.BudgetEntityName = *req.User
	}
	if req.BudgetType != nil {
		budget.BudgetType = *req.BudgetType
	}
	if req.BudgetProductSKU != nil {
		budget.BudgetProductSKU = *req.BudgetProductSKU
	}
	if req.BudgetAlerting != nil {
		if req.BudgetAlerting.WillAlert != nil {
			budget.BudgetAlerting.WillAlert = *req.BudgetAlerting.WillAlert
		}
		if req.BudgetAlerting.AlertRecipients != nil {
			budget.BudgetAlerting.AlertRecipients = append([]string(nil), req.BudgetAlerting.AlertRecipients...)
		}
	}
}

func (s *Server) handleUpdateEnterpriseBudget(w http.ResponseWriter, r *http.Request) {
	var req orgBudgetBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	id := r.PathValue("budget_id")
	budget := s.enterpriseBudget(id)
	if budget == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	applyEnterpriseBudgetPatch(budget, &req)
	if !s.validateEnterpriseBudget(w, budget) {
		return
	}
	s.store.mu.Lock()
	s.store.EnterpriseSettings.EnterpriseBudgets[id] = budget
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Budget successfully updated.", "budget": budgetJSON(budget),
	})
}

func (s *Server) handleDeleteEnterpriseBudget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("budget_id")
	s.store.mu.Lock()
	if s.store.EnterpriseSettings.EnterpriseBudgets[id] == nil {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(s.store.EnterpriseSettings.EnterpriseBudgets, id)
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Budget successfully deleted.", "budget_id": id,
	})
}

func (s *Server) handleEnterpriseBudgetUserStates(w http.ResponseWriter, r *http.Request) {
	budget := s.enterpriseBudget(r.PathValue("budget_id"))
	if budget == nil || (budget.BudgetScope != "multi_user_customer" && budget.BudgetScope != "multi_user_cost_center") {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	lower, upper := 0, 100
	if value := r.URL.Query().Get("threshold_lower_bound"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 100 {
			writeGHError(w, http.StatusBadRequest, "Invalid threshold_lower_bound")
			return
		}
		lower = parsed
	}
	if value := r.URL.Query().Get("threshold_upper_bound"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 100 {
			writeGHError(w, http.StatusBadRequest, "Invalid threshold_upper_bound")
			return
		}
		upper = parsed
	}
	if lower > upper {
		writeGHError(w, http.StatusBadRequest, "Invalid threshold range")
		return
	}
	filter := strings.ToLower(r.URL.Query().Get("user"))
	rows := make([]map[string]interface{}, 0)
	for _, user := range s.store.ListUsers() {
		if user.Type == "Bot" || filter != "" && strings.ToLower(user.Login) != filter {
			continue
		}
		// AI usage is not currently metered, so every member has consumed
		// zero and remains at the zero-percent threshold.
		if lower > 0 {
			continue
		}
		rows = append(rows, map[string]interface{}{
			"user": user.Login, "consumed_amount": 0.0, "target_amount": budget.BudgetAmount,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["user"].(string) < rows[j]["user"].(string) })
	if r.URL.Query().Get("sort_order") == "0" {
		for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
			rows[left], rows[right] = rows[right], rows[left]
		}
	}
	total := len(rows)
	rows = paginateAndLink(w, r, rows)
	page, perPage := 1, 30
	if parsed, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && parsed > 0 {
		page = parsed
	}
	if parsed, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && parsed > 0 && parsed <= 100 {
		perPage = parsed
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_states": rows, "has_next_page": page*perPage < total, "total_count": total,
	})
}

func cloneCostCenter(center *EnterpriseCostCenter) *EnterpriseCostCenter {
	if center == nil {
		return nil
	}
	copy := *center
	copy.Resources = append([]EnterpriseCostCenterResource(nil), center.Resources...)
	return &copy
}

func costCenterJSON(center *EnterpriseCostCenter) map[string]interface{} {
	resources := append([]EnterpriseCostCenterResource(nil), center.Resources...)
	if resources == nil {
		resources = []EnterpriseCostCenterResource{}
	}
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].Type != resources[j].Type {
			return resources[i].Type < resources[j].Type
		}
		return resources[i].Name < resources[j].Name
	})
	out := map[string]interface{}{
		"id": center.ID, "name": center.Name, "state": center.State, "resources": resources,
		"ai_credit_pool_enabled": center.AICreditPoolEnabled,
	}
	if center.AICreditPoolEnabled {
		out["ai_credit_pool_state"] = map[string]interface{}{
			"target_amount": nil, "current_amount": nil,
		}
	}
	return out
}

func (s *Server) enterpriseCostCenter(id string) *EnterpriseCostCenter {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return cloneCostCenter(s.store.EnterpriseSettings.EnterpriseCostCenters[id])
}

func (s *Server) handleListEnterpriseCostCenters(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state != "" && state != "active" && state != "deleted" {
		writeGHError(w, http.StatusBadRequest, "Invalid state")
		return
	}
	s.store.mu.RLock()
	centers := make([]*EnterpriseCostCenter, 0, len(s.store.EnterpriseSettings.EnterpriseCostCenters))
	for _, center := range s.store.EnterpriseSettings.EnterpriseCostCenters {
		if state == "" || center.State == state {
			centers = append(centers, cloneCostCenter(center))
		}
	}
	s.store.mu.RUnlock()
	sort.Slice(centers, func(i, j int) bool { return centers[i].Name < centers[j].Name })
	rows := make([]map[string]interface{}, len(centers))
	for i, center := range centers {
		rows[i] = costCenterJSON(center)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"costCenters": rows})
}

func (s *Server) costCenterNameConflictLocked(name, exceptID string) bool {
	for id, center := range s.store.EnterpriseSettings.EnterpriseCostCenters {
		if id != exceptID && center.State == "active" && strings.EqualFold(center.Name, name) {
			return true
		}
	}
	return false
}

func (s *Server) handleCreateEnterpriseCostCenter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                string `json:"name"`
		AICreditPoolEnabled bool   `json:"ai_credit_pool_enabled"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 255 {
		writeGHError(w, http.StatusBadRequest, "Invalid cost center name")
		return
	}
	now := s.currentTime()
	center := &EnterpriseCostCenter{
		ID: uuid.NewString(), Name: req.Name, State: "active", Resources: []EnterpriseCostCenterResource{},
		AICreditPoolEnabled: req.AICreditPoolEnabled, CreatedAt: now, UpdatedAt: now,
	}
	s.store.mu.Lock()
	if s.costCenterNameConflictLocked(req.Name, "") {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusConflict, "A cost center with this name already exists.")
		return
	}
	s.store.EnterpriseSettings.EnterpriseCostCenters[center.ID] = center
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, costCenterJSON(center))
}

func (s *Server) handleGetEnterpriseCostCenter(w http.ResponseWriter, r *http.Request) {
	center := s.enterpriseCostCenter(r.PathValue("cost_center_id"))
	if center == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, costCenterJSON(center))
}

func (s *Server) handleUpdateEnterpriseCostCenter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                *string `json:"name"`
		AICreditPoolEnabled *bool   `json:"ai_credit_pool_enabled"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	id := r.PathValue("cost_center_id")
	s.store.mu.Lock()
	center := s.store.EnterpriseSettings.EnterpriseCostCenters[id]
	if center == nil || center.State == "deleted" {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 255 {
			s.store.mu.Unlock()
			writeGHValidationError(w, "CostCenter", "name", "invalid")
			return
		}
		if s.costCenterNameConflictLocked(name, id) {
			s.store.mu.Unlock()
			writeGHError(w, http.StatusConflict, "A cost center with this name already exists.")
			return
		}
		center.Name = name
	}
	if req.AICreditPoolEnabled != nil {
		if *req.AICreditPoolEnabled {
			for _, resource := range center.Resources {
				if resource.Type == "Org" || resource.Type == "Repo" {
					s.store.mu.Unlock()
					writeGHValidationError(w, "CostCenter", "ai_credit_pool_enabled", "invalid")
					return
				}
			}
		}
		center.AICreditPoolEnabled = *req.AICreditPoolEnabled
	}
	center.UpdatedAt = s.currentTime()
	copy := cloneCostCenter(center)
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, costCenterJSON(copy))
}

func (s *Server) handleDeleteEnterpriseCostCenter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cost_center_id")
	s.store.mu.Lock()
	center := s.store.EnterpriseSettings.EnterpriseCostCenters[id]
	if center == nil || center.State == "deleted" {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	center.State = "deleted"
	center.Resources = []EnterpriseCostCenterResource{}
	center.UpdatedAt = s.currentTime()
	name := center.Name
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Cost center successfully deleted.", "id": id, "name": name,
		"costCenterState": "CostCenterArchived",
	})
}

type enterpriseCostCenterResourcesBody struct {
	Users           []string `json:"users"`
	Organizations   []string `json:"organizations"`
	Repositories    []string `json:"repositories"`
	EnterpriseTeams []string `json:"enterprise_teams"`
}

func (s *Server) validateCostCenterResources(w http.ResponseWriter, req enterpriseCostCenterResourcesBody) ([]EnterpriseCostCenterResource, bool) {
	total := len(req.Users) + len(req.Organizations) + len(req.Repositories) + len(req.EnterpriseTeams)
	if total == 0 || total > 50 {
		writeGHError(w, http.StatusBadRequest, "Between 1 and 50 resources are required.")
		return nil, false
	}
	var out []EnterpriseCostCenterResource
	seen := map[string]bool{}
	add := func(kind, name string) {
		key := kind + "\x00" + strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			out = append(out, EnterpriseCostCenterResource{Type: kind, Name: name})
		}
	}
	for _, login := range req.Users {
		user := s.store.LookupUserByLogin(login)
		if user == nil {
			writeGHValidationError(w, "CostCenter", "users", "invalid")
			return nil, false
		}
		add("User", user.Login)
	}
	for _, login := range req.Organizations {
		org := s.store.GetOrg(login)
		if org == nil {
			writeGHValidationError(w, "CostCenter", "organizations", "invalid")
			return nil, false
		}
		add("Org", org.Login)
	}
	for _, fullName := range req.Repositories {
		repo := s.store.GetRepoByFullName(fullName)
		if repo == nil {
			writeGHValidationError(w, "CostCenter", "repositories", "invalid")
			return nil, false
		}
		add("Repo", repo.FullName)
	}
	for _, slug := range req.EnterpriseTeams {
		team := s.store.GetEnterpriseTeam(slug)
		if team == nil {
			writeGHValidationError(w, "CostCenter", "enterprise_teams", "invalid")
			return nil, false
		}
		add("Team", team.Slug)
	}
	return out, true
}

func sameCostCenterResource(a, b EnterpriseCostCenterResource) bool {
	return a.Type == b.Type && strings.EqualFold(a.Name, b.Name)
}

func removeCostCenterResource(resources []EnterpriseCostCenterResource, target EnterpriseCostCenterResource) ([]EnterpriseCostCenterResource, bool) {
	for i, resource := range resources {
		if sameCostCenterResource(resource, target) {
			return append(resources[:i], resources[i+1:]...), true
		}
	}
	return resources, false
}

func (s *Server) handleAddEnterpriseCostCenterResources(w http.ResponseWriter, r *http.Request) {
	var req enterpriseCostCenterResourcesBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	resources, ok := s.validateCostCenterResources(w, req)
	if !ok {
		return
	}
	id := r.PathValue("cost_center_id")
	s.store.mu.Lock()
	center := s.store.EnterpriseSettings.EnterpriseCostCenters[id]
	if center == nil || center.State != "active" {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if center.AICreditPoolEnabled {
		for _, resource := range resources {
			if resource.Type == "Org" || resource.Type == "Repo" {
				s.store.mu.Unlock()
				writeGHError(w, http.StatusBadRequest, "AI credit pool cost centers only accept users and teams.")
				return
			}
		}
	}
	reassigned := make([]map[string]interface{}, 0)
	for _, resource := range resources {
		for otherID, other := range s.store.EnterpriseSettings.EnterpriseCostCenters {
			if otherID == id || other.State != "active" {
				continue
			}
			var removed bool
			other.Resources, removed = removeCostCenterResource(other.Resources, resource)
			if removed {
				reassigned = append(reassigned, map[string]interface{}{
					"resource_type": strings.ToLower(resource.Type), "name": resource.Name,
					"previous_cost_center": other.Name,
				})
				other.UpdatedAt = s.currentTime()
			}
		}
		found := false
		for _, existing := range center.Resources {
			found = found || sameCostCenterResource(existing, resource)
		}
		if !found {
			center.Resources = append(center.Resources, resource)
		}
	}
	center.UpdatedAt = s.currentTime()
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Resources successfully added to the cost center.", "reassigned_resources": reassigned,
	})
}

func (s *Server) handleRemoveEnterpriseCostCenterResources(w http.ResponseWriter, r *http.Request) {
	var req enterpriseCostCenterResourcesBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	resources, ok := s.validateCostCenterResources(w, req)
	if !ok {
		return
	}
	id := r.PathValue("cost_center_id")
	s.store.mu.Lock()
	center := s.store.EnterpriseSettings.EnterpriseCostCenters[id]
	if center == nil || center.State != "active" {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, resource := range resources {
		center.Resources, _ = removeCostCenterResource(center.Resources, resource)
	}
	center.UpdatedAt = s.currentTime()
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Resources successfully removed from the cost center.",
	})
}

func (s *Server) enterpriseActionsUsageLines(year, month, day int) []actionsUsageLine {
	var lines []actionsUsageLine
	for _, org := range s.store.ListOrgsAll(0) {
		lines = append(lines, s.store.orgActionsUsageLines(org.Login, year, month, day)...)
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].date != lines[j].date {
			return lines[i].date < lines[j].date
		}
		if lines[i].orgName != lines[j].orgName {
			return lines[i].orgName < lines[j].orgName
		}
		return lines[i].repoName < lines[j].repoName
	})
	return lines
}

func (s *Server) enterpriseBillingPeriod(w http.ResponseWriter, r *http.Request, defaultMonth bool) (billingTimeFilter, bool) {
	filter, err := parseBillingTimeFilter(r, defaultMonth, s.currentTime())
	if err != nil || filter.year < 1000 || filter.year > 9999 || filter.month < 0 || filter.month > 12 || filter.day < 0 || filter.day > 31 {
		if err != nil {
			writeGHError(w, http.StatusBadRequest, err.Error())
		} else {
			writeGHError(w, http.StatusBadRequest, "Invalid billing period")
		}
		return billingTimeFilter{}, false
	}
	return filter, true
}

func (s *Server) costCenterForRepo(fullName string) string {
	owner, _, _ := strings.Cut(fullName, "/")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	for _, center := range s.store.EnterpriseSettings.EnterpriseCostCenters {
		if center.State != "active" {
			continue
		}
		for _, resource := range center.Resources {
			if resource.Type == "Repo" && strings.EqualFold(resource.Name, fullName) ||
				resource.Type == "Org" && strings.EqualFold(resource.Name, owner) {
				return center.ID
			}
		}
	}
	return ""
}

func (s *Server) enterpriseUsageItems(filter billingTimeFilter, costCenterFilter string) []map[string]interface{} {
	lines := s.enterpriseActionsUsageLines(filter.year, filter.month, filter.day)
	items := make([]map[string]interface{}, 0, len(lines))
	for _, line := range lines {
		fullName, orgName := line.orgName+"/"+line.repoName, line.orgName
		centerID := s.costCenterForRepo(fullName)
		if costCenterFilter == "none" && centerID != "" || costCenterFilter != "" && costCenterFilter != "none" && centerID != costCenterFilter {
			continue
		}
		gross := float64(line.minutes) * actionsPricePerMinute
		item := map[string]interface{}{
			"date": line.date, "product": "Actions", "sku": "Actions Linux",
			"quantity": line.minutes, "unitType": "minutes", "pricePerUnit": actionsPricePerMinute,
			"grossAmount": gross, "discountAmount": 0.0, "netAmount": gross,
			"organizationName": orgName, "repositoryName": fullName,
		}
		if centerID != "" {
			item["costCenterId"] = centerID
		}
		items = append(items, item)
	}
	return items
}

func (s *Server) handleEnterpriseBillingUsage(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.enterpriseBillingPeriod(w, r, false)
	if !ok {
		return
	}
	costCenter := r.URL.Query().Get("cost_center_id")
	if costCenter == "" {
		costCenter = "none"
	}
	if costCenter != "none" && s.enterpriseCostCenter(costCenter) == nil {
		writeGHError(w, http.StatusBadRequest, "Invalid cost_center_id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"usageItems": s.enterpriseUsageItems(filter, costCenter),
	})
}

func (s *Server) handleEnterpriseBillingUsageSummary(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.enterpriseBillingPeriod(w, r, true)
	if !ok {
		return
	}
	q := r.URL.Query()
	centerID := q.Get("cost_center_id")
	if centerID != "" && centerID != "none" && s.enterpriseCostCenter(centerID) == nil {
		writeGHError(w, http.StatusBadRequest, "Invalid cost_center_id")
		return
	}
	items := s.enterpriseUsageItems(filter, centerID)
	var quantity, amount float64
	for _, item := range items {
		org := fmt.Sprint(item["organizationName"])
		repo := fmt.Sprint(item["repositoryName"])
		if v := q.Get("organization"); v != "" && !strings.EqualFold(v, org) {
			continue
		}
		if v := q.Get("repository"); v != "" && !strings.EqualFold(v, repo) {
			continue
		}
		if v := q.Get("product"); v != "" && !strings.EqualFold(v, "Actions") {
			continue
		}
		if v := q.Get("sku"); v != "" && !strings.EqualFold(v, "Actions Linux") {
			continue
		}
		quantity += float64(item["quantity"].(int))
		amount += item["grossAmount"].(float64)
	}
	usage := []map[string]interface{}{}
	if quantity > 0 {
		usage = append(usage, map[string]interface{}{
			"product": "Actions", "sku": "Actions Linux", "unitType": "minutes",
			"pricePerUnit": actionsPricePerMinute, "grossQuantity": quantity, "grossAmount": amount,
			"discountQuantity": 0.0, "discountAmount": 0.0, "netQuantity": quantity, "netAmount": amount,
		})
	}
	out := map[string]interface{}{
		"timePeriod": userBillingTimePeriodJSON(filter), "enterprise": s.enterpriseSlug(), "usageItems": usage,
	}
	for _, key := range []string{"organization", "repository", "product", "sku", "cost_center_id"} {
		if value := q.Get(key); value != "" {
			out[key] = value
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) writeEnterpriseEmptyAIUsage(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.enterpriseBillingPeriod(w, r, true)
	if !ok {
		return
	}
	out := map[string]interface{}{
		"timePeriod": userBillingTimePeriodJSON(filter), "enterprise": s.enterpriseSlug(),
		"usageItems": []map[string]interface{}{},
	}
	for _, key := range []string{"organization", "user", "product", "model"} {
		if value := r.URL.Query().Get(key); value != "" {
			out[key] = value
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleEnterprisePremiumRequestUsage(w http.ResponseWriter, r *http.Request) {
	s.writeEnterpriseEmptyAIUsage(w, r)
}

func (s *Server) handleEnterpriseAICreditUsage(w http.ResponseWriter, r *http.Request) {
	s.writeEnterpriseEmptyAIUsage(w, r)
}

func (s *Server) handleEnterpriseAdvancedSecurityBilling(w http.ResponseWriter, r *http.Request) {
	if product := r.URL.Query().Get("advanced_security_product"); product != "" &&
		product != "code_security" && product != "secret_protection" {
		writeGHError(w, http.StatusBadRequest, "Invalid advanced_security_product")
		return
	}
	// Committer metering is only enabled for repositories attached to an
	// Advanced Security configuration. Until such activity is recorded, the
	// faithful report is a typed empty repository page.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_advanced_security_committers":     0,
		"total_count":                            0,
		"maximum_advanced_security_committers":   0,
		"purchased_advanced_security_committers": 0,
		"repositories":                           []map[string]interface{}{},
	})
}

func cloneBillingReport(report *EnterpriseBillingReport) *EnterpriseBillingReport {
	if report == nil {
		return nil
	}
	copy := *report
	copy.DownloadURLs = append([]string(nil), report.DownloadURLs...)
	return &copy
}

func billingReportJSON(report *EnterpriseBillingReport) map[string]interface{} {
	out := map[string]interface{}{
		"id": report.ID, "report_type": report.ReportType, "start_date": report.StartDate,
		"end_date": report.EndDate, "status": report.Status,
		"created_at": report.CreatedAt.Format(time.RFC3339), "actor": report.Actor,
	}
	if report.Status == "completed" {
		out["download_urls"] = append([]string(nil), report.DownloadURLs...)
	}
	return out
}

func (s *Server) handleListEnterpriseBillingReports(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.RLock()
	reports := make([]*EnterpriseBillingReport, 0, len(s.store.EnterpriseSettings.EnterpriseBillingReports))
	for _, report := range s.store.EnterpriseSettings.EnterpriseBillingReports {
		reports = append(reports, cloneBillingReport(report))
	}
	s.store.mu.RUnlock()
	sort.Slice(reports, func(i, j int) bool {
		if !reports[i].CreatedAt.Equal(reports[j].CreatedAt) {
			return reports[i].CreatedAt.After(reports[j].CreatedAt)
		}
		return reports[i].ID < reports[j].ID
	})
	rows := make([]map[string]interface{}, len(reports))
	for i, report := range reports {
		rows[i] = billingReportJSON(report)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"usage_report_exports": rows})
}

func (s *Server) handleCreateEnterpriseBillingReport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReportType string `json:"report_type"`
		StartDate  string `json:"start_date"`
		EndDate    string `json:"end_date"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	allowed := map[string]bool{"detailed": true, "summarized": true, "premium_request": true, "ai_credit": true}
	start, startErr := time.Parse("2006-01-02", req.StartDate)
	if req.EndDate == "" {
		req.EndDate = s.currentTime().Format("2006-01-02")
	}
	end, endErr := time.Parse("2006-01-02", req.EndDate)
	maxDays := 31
	if req.ReportType == "summarized" {
		maxDays = 366
	}
	if !allowed[req.ReportType] || startErr != nil || endErr != nil || end.Before(start) ||
		int(end.Sub(start).Hours()/24)+1 > maxDays {
		writeGHError(w, http.StatusBadRequest, "Invalid usage report export request")
		return
	}
	actor := ""
	if user := ghUserFromContext(r.Context()); user != nil {
		actor = user.Login
	} else if app := ghAppFromContext(r.Context()); app != nil {
		actor = app.Slug + "[bot]"
	}
	now := s.currentTime()
	report := &EnterpriseBillingReport{
		ID: uuid.NewString(), ReportType: req.ReportType, StartDate: req.StartDate, EndDate: req.EndDate,
		Status: "processing", CreatedAt: now, Actor: actor,
	}
	s.store.mu.Lock()
	s.store.EnterpriseSettings.EnterpriseBillingReports[report.ID] = report
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusAccepted, billingReportJSON(report))
}

func (s *Server) handleGetEnterpriseBillingReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("report_id")
	s.store.mu.Lock()
	report := s.store.EnterpriseSettings.EnterpriseBillingReports[id]
	if report == nil {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if report.Status == "processing" {
		report.Status = "completed"
		report.DownloadURLs = []string{
			s.baseURL(r) + "/enterprises/" + s.enterpriseSlug() + "/billing/reports/" + report.ID + "/download",
		}
		s.store.persistEnterpriseSettings()
	}
	copy := cloneBillingReport(report)
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, billingReportJSON(copy))
}

func (s *Server) handleDownloadEnterpriseBillingReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("report_id")
	s.store.mu.RLock()
	report := cloneBillingReport(s.store.EnterpriseSettings.EnterpriseBillingReports[id])
	s.store.mu.RUnlock()
	if report == nil || report.Status != "completed" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	start, _ := time.Parse("2006-01-02", report.StartDate)
	end, _ := time.Parse("2006-01-02", report.EndDate)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, report.ReportType+"-"+report.ID+".csv"))
	w.WriteHeader(http.StatusOK)
	writer := csv.NewWriter(w)
	if report.ReportType == "premium_request" || report.ReportType == "ai_credit" {
		_ = writer.Write([]string{"date", "product", "sku", "model", "username", "quantity", "gross_amount", "net_amount"})
		writer.Flush()
		return
	}
	headers := []string{"date", "product", "sku", "quantity", "unit_type", "price_per_unit", "gross_amount", "discount_amount", "net_amount", "organization", "repository", "cost_center_id"}
	if report.ReportType == "detailed" {
		headers = append(headers, "username", "workflow_path")
	}
	_ = writer.Write(headers)
	value := func(item map[string]interface{}, key string) string {
		if item[key] == nil {
			return ""
		}
		return fmt.Sprint(item[key])
	}
	for cursor := start; !cursor.After(end); cursor = cursor.AddDate(0, 0, 1) {
		filter := billingTimeFilter{year: cursor.Year(), month: int(cursor.Month()), day: cursor.Day()}
		for _, item := range s.enterpriseUsageItems(filter, "") {
			row := []string{
				value(item, "date"), value(item, "product"), value(item, "sku"),
				value(item, "quantity"), value(item, "unitType"), value(item, "pricePerUnit"),
				value(item, "grossAmount"), value(item, "discountAmount"), value(item, "netAmount"),
				value(item, "organizationName"), value(item, "repositoryName"), value(item, "costCenterId"),
			}
			if report.ReportType == "detailed" {
				row = append(row, "", "")
			}
			_ = writer.Write(row)
		}
	}
	writer.Flush()
}
