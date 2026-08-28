package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

// Org billing under the enhanced billing platform paths
// (/organizations/{org}/settings/billing/...): budgets are persisted CRUD
// entities; usage reports are computed from stored Actions run history and are
// empty when no billable usage exists.

var budgetScopes = map[string]bool{
	"organization":        true,
	"repository":          true,
	"multi_user_customer": true,
	"user":                true,
}

var budgetTypes = map[string]bool{
	"BundlePricing":  true,
	"ProductPricing": true,
	"SkuPricing":     true,
}

func (s *Server) registerGHOrgBillingRoutes() {
	s.route("GET /api/v3/organizations/{org}/settings/billing/budgets", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleListOrgBudgets))
	s.route("POST /api/v3/organizations/{org}/settings/billing/budgets", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermWrite, s.handleCreateOrgBudget))
	s.route("GET /api/v3/organizations/{org}/settings/billing/budgets/{budget_id}", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleGetOrgBudget))
	s.route("PATCH /api/v3/organizations/{org}/settings/billing/budgets/{budget_id}", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermWrite, s.handleUpdateOrgBudget))
	s.route("DELETE /api/v3/organizations/{org}/settings/billing/budgets/{budget_id}", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermWrite, s.handleDeleteOrgBudget))

	s.route("GET /api/v3/organizations/{org}/settings/billing/usage", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleOrgBillingUsage))
	s.route("GET /api/v3/organizations/{org}/settings/billing/usage/summary", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleOrgBillingUsageSummary))
	s.route("GET /api/v3/organizations/{org}/settings/billing/premium_request/usage", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleOrgBillingPremiumRequestUsage))
	s.route("GET /api/v3/organizations/{org}/settings/billing/ai_credit/usage", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleOrgBillingAICreditUsage))
}

type orgBudgetBody struct {
	BudgetAmount        *int            `json:"budget_amount"`
	PreventFurtherUsage *bool           `json:"prevent_further_usage"`
	BudgetScope         *string         `json:"budget_scope"`
	BudgetEntityName    *string         `json:"budget_entity_name"`
	BudgetType          *string         `json:"budget_type"`
	BudgetProductSKU    *string         `json:"budget_product_sku"`
	User                *string         `json:"user"`
	ExpiresAt           json.RawMessage `json:"expires_at"`
	BudgetAlerting      *struct {
		WillAlert       *bool    `json:"will_alert"`
		AlertRecipients []string `json:"alert_recipients"`
	} `json:"budget_alerting"`
}

func budgetJSON(b *store.OrgBudget) map[string]interface{} {
	recipients := b.BudgetAlerting.AlertRecipients
	if recipients == nil {
		recipients = []string{}
	}
	out := map[string]interface{}{
		"id":                    b.ID,
		"budget_scope":          b.BudgetScope,
		"budget_entity_name":    b.BudgetEntityName,
		"budget_amount":         b.BudgetAmount,
		"prevent_further_usage": b.PreventFurtherUsage,
		"budget_product_sku":    b.BudgetProductSKU,
		"budget_type":           b.BudgetType,
		"budget_alerting": map[string]interface{}{
			"will_alert":       b.BudgetAlerting.WillAlert,
			"alert_recipients": recipients,
		},
	}
	if b.BudgetScope == "user" {
		out["user"] = b.BudgetEntityName
	}
	if b.ExpiresAt != nil {
		out["expires_at"] = b.ExpiresAt.UTC().Format(time.DateOnly)
	}
	return out
}

func (s *Server) parseOrgBudgetExpiration(w http.ResponseWriter, raw json.RawMessage, scope string, allowClear bool) (*time.Time, bool) {
	if scope != "user" {
		store.WriteGHValidationError(w, "Budget", "expires_at", "invalid")
		return nil, false
	}
	encoded := strings.TrimSpace(string(raw))
	if allowClear && (encoded == "null" || encoded == "0") {
		return nil, true
	}
	var date string
	if err := json.Unmarshal(raw, &date); err != nil {
		store.WriteGHValidationError(w, "Budget", "expires_at", "invalid")
		return nil, false
	}
	expiresAt, err := time.Parse(time.DateOnly, date)
	if err != nil || !expiresAt.After(s.currentTime().UTC()) {
		store.WriteGHValidationError(w, "Budget", "expires_at", "invalid")
		return nil, false
	}
	return &expiresAt, true
}

func (s *Server) validateOrgBudget(w http.ResponseWriter, orgLogin string, budget *store.OrgBudget) bool {
	if budget == nil || budget.BudgetAmount < 0 || !budgetScopes[budget.BudgetScope] ||
		!budgetTypes[budget.BudgetType] || budget.BudgetProductSKU == "" {
		store.WriteGHValidationError(w, "Budget", "budget", "invalid")
		return false
	}
	isUserBudget := budget.BudgetScope == "user"
	isMultiUserBudget := budget.BudgetScope == "multi_user_customer"
	if (isUserBudget || isMultiUserBudget) &&
		(budget.BudgetProductSKU != "ai_credits" && budget.BudgetProductSKU != "premium_requests") {
		store.WriteGHValidationError(w, "Budget", "budget_product_sku", "invalid")
		return false
	}
	if (isUserBudget || isMultiUserBudget) && !budget.PreventFurtherUsage {
		store.WriteGHValidationError(w, "Budget", "prevent_further_usage", "invalid")
		return false
	}
	if budget.BudgetType == "BundlePricing" && budget.BudgetProductSKU != "ai_credits" {
		store.WriteGHValidationError(w, "Budget", "budget_product_sku", "invalid")
		return false
	}
	if budget.ExpiresAt != nil && !isUserBudget {
		store.WriteGHValidationError(w, "Budget", "expires_at", "invalid")
		return false
	}
	switch budget.BudgetScope {
	case "repository":
		name := budget.BudgetEntityName
		if !strings.Contains(name, "/") {
			name = orgLogin + "/" + name
		}
		repo := s.store.GetRepoByFullName(name)
		org := s.store.GetOrg(orgLogin)
		if repo == nil || org == nil || repo.OwnerType != "Organization" || repo.OwnerID != org.ID {
			store.WriteGHValidationError(w, "Budget", "budget_entity_name", "invalid")
			return false
		}
	case "user":
		user := s.store.LookupUserByLogin(budget.BudgetEntityName)
		if user == nil {
			store.WriteGHValidationError(w, "Budget", "user", "invalid")
			return false
		}
		membership := s.store.GetMembership(orgLogin, user.ID)
		if membership == nil || membership.State != store.MembershipStateActive {
			store.WriteGHValidationError(w, "Budget", "user", "invalid")
			return false
		}
		if budget.BudgetAlerting.WillAlert || len(budget.BudgetAlerting.AlertRecipients) != 0 {
			store.WriteGHValidationError(w, "Budget", "budget_alerting", "invalid")
			return false
		}
	}
	return true
}

func (s *Server) handleListOrgBudgets(w http.ResponseWriter, r *http.Request) {
	budgets := s.store.ListOrgBudgets(r.PathValue("org"))

	if scope := r.URL.Query().Get("scope"); scope != "" {
		filtered := budgets[:0]
		for _, b := range budgets {
			if b.BudgetScope == scope {
				filtered = append(filtered, b)
			}
		}
		budgets = filtered
	}

	total := len(budgets)
	pp := parsePagination(r)
	lastPage := 1
	if total > 0 {
		lastPage = (total + pp.PerPage - 1) / pp.PerPage
	}

	out := make([]map[string]interface{}, 0, total)
	for _, b := range budgets {
		out = append(out, budgetJSON(b))
	}
	paged := paginateAndLink(w, r, out)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"budgets":       paged,
		"total_count":   total,
		"has_next_page": pp.Page < lastPage,
	})
}

func (s *Server) handleCreateOrgBudget(w http.ResponseWriter, r *http.Request) {
	orgLogin := r.PathValue("org")
	var req orgBudgetBody
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	b := &store.OrgBudget{
		ID:          uuid.New().String(),
		BudgetScope: "organization",
		BudgetType:  "ProductPricing",
		CreatedAt:   s.currentTime(),
	}
	if req.BudgetScope != nil {
		if !budgetScopes[*req.BudgetScope] {
			store.WriteGHValidationError(w, "Budget", "budget_scope", "invalid")
			return
		}
		b.BudgetScope = *req.BudgetScope
	}
	if req.BudgetType != nil {
		if !budgetTypes[*req.BudgetType] {
			store.WriteGHValidationError(w, "Budget", "budget_type", "invalid")
			return
		}
		b.BudgetType = *req.BudgetType
	}
	if req.BudgetProductSKU == nil || *req.BudgetProductSKU == "" {
		store.WriteGHValidationError(w, "Budget", "budget_product_sku", "missing_field")
		return
	}
	b.BudgetProductSKU = *req.BudgetProductSKU
	if req.BudgetAmount != nil {
		if *req.BudgetAmount < 0 {
			store.WriteGHValidationError(w, "Budget", "budget_amount", "invalid")
			return
		}
		b.BudgetAmount = *req.BudgetAmount
	}
	if req.PreventFurtherUsage != nil {
		b.PreventFurtherUsage = *req.PreventFurtherUsage
	}
	if req.BudgetEntityName != nil {
		b.BudgetEntityName = *req.BudgetEntityName
	}
	if req.User != nil && b.BudgetScope == "user" {
		b.BudgetEntityName = *req.User
	}
	if req.BudgetAlerting != nil {
		if req.BudgetAlerting.WillAlert != nil {
			b.BudgetAlerting.WillAlert = *req.BudgetAlerting.WillAlert
		}
		b.BudgetAlerting.AlertRecipients = req.BudgetAlerting.AlertRecipients
	}
	if req.ExpiresAt != nil {
		expiresAt, ok := s.parseOrgBudgetExpiration(w, req.ExpiresAt, b.BudgetScope, false)
		if !ok {
			return
		}
		b.ExpiresAt = expiresAt
	}
	if !s.validateOrgBudget(w, orgLogin, b) {
		return
	}

	s.store.CreateOrgBudget(orgLogin, b)
	createdBudget := budgetJSON(b)
	delete(createdBudget, "user")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Budget created successfully",
		"budget":  createdBudget,
	})
}

func (s *Server) handleGetOrgBudget(w http.ResponseWriter, r *http.Request) {
	b := s.store.GetOrgBudget(r.PathValue("org"), r.PathValue("budget_id"))
	if b == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, budgetJSON(b))
}

func (s *Server) handleUpdateOrgBudget(w http.ResponseWriter, r *http.Request) {
	orgLogin := r.PathValue("org")
	id := r.PathValue("budget_id")
	var req orgBudgetBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	b := s.store.GetOrgBudget(orgLogin, id)
	if b == nil {
		writeGHError(w, http.StatusNotFound, fmt.Sprintf("Budget with ID %s not found.", id))
		return
	}
	apply := func(b *store.OrgBudget) {
		if req.BudgetAmount != nil {
			b.BudgetAmount = *req.BudgetAmount
		}
		if req.PreventFurtherUsage != nil {
			b.PreventFurtherUsage = *req.PreventFurtherUsage
		}
		if req.BudgetScope != nil {
			b.BudgetScope = *req.BudgetScope
		}
		if req.BudgetEntityName != nil {
			b.BudgetEntityName = *req.BudgetEntityName
		}
		if req.User != nil && b.BudgetScope == "user" {
			b.BudgetEntityName = *req.User
		}
		if req.BudgetType != nil {
			b.BudgetType = *req.BudgetType
		}
		if req.BudgetProductSKU != nil {
			b.BudgetProductSKU = *req.BudgetProductSKU
		}
		if req.BudgetAlerting != nil {
			if req.BudgetAlerting.WillAlert != nil {
				b.BudgetAlerting.WillAlert = *req.BudgetAlerting.WillAlert
			}
			if req.BudgetAlerting.AlertRecipients != nil {
				b.BudgetAlerting.AlertRecipients = req.BudgetAlerting.AlertRecipients
			}
		}
	}
	apply(b)
	if req.ExpiresAt != nil {
		expiresAt, ok := s.parseOrgBudgetExpiration(w, req.ExpiresAt, b.BudgetScope, true)
		if !ok {
			return
		}
		b.ExpiresAt = expiresAt
	}
	// GitHub ignores alerting fields when a user budget is updated. Clearing
	// existing alerting also makes an organization-to-user scope change obey
	// the same invariant.
	if b.BudgetScope == "user" {
		b.BudgetAlerting = store.OrgBudgetAlerting{}
	}
	if !s.validateOrgBudget(w, orgLogin, b) {
		return
	}
	b = s.store.UpdateOrgBudget(orgLogin, id, func(current *store.OrgBudget) { *current = *b })
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Budget updated successfully",
		"budget":  budgetJSON(b),
	})
}

func (s *Server) handleDeleteOrgBudget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("budget_id")
	if !s.store.DeleteOrgBudget(r.PathValue("org"), id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Budget deleted successfully",
		"id":      id,
	})
}

// actionsPricePerMinute mirrors GitHub's Linux runner list price.
const actionsPricePerMinute = 0.008

// billingPeriod parses the year/month/day query params. defaultMonth defaults
// the month to now, as the summary/premium/AI reports do.
func billingPeriod(w http.ResponseWriter, r *http.Request, defaultMonth bool, now time.Time) (year, month, day int, ok bool) {
	now = now.UTC()
	year = now.Year()
	if defaultMonth {
		month = int(now.Month())
	}
	q := r.URL.Query()
	if v := q.Get("year"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 || n > 9999 {
			writeGHError(w, http.StatusBadRequest, "Invalid year")
			return 0, 0, 0, false
		}
		year = n
	}
	if v := q.Get("month"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 12 {
			writeGHError(w, http.StatusBadRequest, "Invalid month")
			return 0, 0, 0, false
		}
		month = n
	}
	if v := q.Get("day"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 31 {
			writeGHError(w, http.StatusBadRequest, "Invalid day")
			return 0, 0, 0, false
		}
		day = n
	}
	return year, month, day, true
}

func (s *Server) handleOrgBillingUsage(w http.ResponseWriter, r *http.Request) {
	orgLogin := r.PathValue("org")
	year, month, day, ok := billingPeriod(w, r, false, s.currentTime())
	if !ok {
		return
	}
	lines := s.store.OrgActionsUsageLines(orgLogin, year, month, day)
	items := make([]map[string]interface{}, 0, len(lines))
	for _, line := range lines {
		gross := float64(line.Minutes) * actionsPricePerMinute
		items = append(items, map[string]interface{}{
			"date":             line.Date,
			"product":          "actions",
			"sku":              "actions_linux",
			"quantity":         line.Minutes,
			"unitType":         "Minutes",
			"pricePerUnit":     actionsPricePerMinute,
			"grossAmount":      gross,
			"discountAmount":   0.0,
			"netAmount":        gross,
			"organizationName": orgLogin,
			"repositoryName":   line.RepoName,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"usageItems": items})
}

func billingTimePeriodJSON(year, month, day int) map[string]interface{} {
	out := map[string]interface{}{"year": year}
	if month != 0 {
		out["month"] = month
	}
	if day != 0 {
		out["day"] = day
	}
	return out
}

func (s *Server) handleOrgBillingUsageSummary(w http.ResponseWriter, r *http.Request) {
	orgLogin := r.PathValue("org")
	year, month, day, ok := billingPeriod(w, r, true, s.currentTime())
	if !ok {
		return
	}
	q := r.URL.Query()
	repoFilter := q.Get("repository")
	productFilter := q.Get("product")
	skuFilter := q.Get("sku")

	lines := s.store.OrgActionsUsageLines(orgLogin, year, month, day)
	var grossQuantity, grossAmount float64
	for _, line := range lines {
		if repoFilter != "" && line.RepoName != repoFilter {
			continue
		}
		grossQuantity += float64(line.Minutes)
		grossAmount += float64(line.Minutes) * actionsPricePerMinute
	}

	items := []map[string]interface{}{}
	if grossQuantity > 0 &&
		(productFilter == "" || productFilter == "actions") &&
		(skuFilter == "" || skuFilter == "actions_linux") {
		items = append(items, map[string]interface{}{
			"product":          "actions",
			"sku":              "actions_linux",
			"unitType":         "Minutes",
			"pricePerUnit":     actionsPricePerMinute,
			"grossQuantity":    grossQuantity,
			"grossAmount":      grossAmount,
			"discountQuantity": 0.0,
			"discountAmount":   0.0,
			"netQuantity":      grossQuantity,
			"netAmount":        grossAmount,
		})
	}

	out := map[string]interface{}{
		"timePeriod":   billingTimePeriodJSON(year, month, day),
		"organization": orgLogin,
		"usageItems":   items,
	}
	if repoFilter != "" {
		out["repository"] = repoFilter
	}
	if productFilter != "" {
		out["product"] = productFilter
	}
	if skuFilter != "" {
		out["sku"] = skuFilter
	}
	writeJSON(w, http.StatusOK, out)
}

// handleOrgBillingPremiumRequestUsage reports Copilot premium request usage,
// always empty: bleephub runs no metered premium-request product.
func (s *Server) handleOrgBillingPremiumRequestUsage(w http.ResponseWriter, r *http.Request) {
	s.writeOrgBillingMeteredAIUsage(w, r)
}

// handleOrgBillingAICreditUsage reports AI credit usage, always empty: bleephub
// runs no metered AI-credit product.
func (s *Server) handleOrgBillingAICreditUsage(w http.ResponseWriter, r *http.Request) {
	s.writeOrgBillingMeteredAIUsage(w, r)
}

func (s *Server) writeOrgBillingMeteredAIUsage(w http.ResponseWriter, r *http.Request) {
	orgLogin := r.PathValue("org")
	year, month, day, ok := billingPeriod(w, r, true, s.currentTime())
	if !ok {
		return
	}
	q := r.URL.Query()
	out := map[string]interface{}{
		"timePeriod":   billingTimePeriodJSON(year, month, day),
		"organization": orgLogin,
		"usageItems":   []map[string]interface{}{},
	}
	if v := q.Get("user"); v != "" {
		out["user"] = v
	}
	if v := q.Get("product"); v != "" {
		out["product"] = v
	}
	if v := q.Get("model"); v != "" {
		out["model"] = v
	}
	writeJSON(w, http.StatusOK, out)
}
