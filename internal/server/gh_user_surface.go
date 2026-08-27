package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// User-account surface: profile, emails, interaction limits, Marketplace
// purchases, billing usage, hovercards, and cross-repository issue listing.

func (s *Server) registerGHUserSurfaceRoutes() {
	s.route("PATCH /api/v3/user", s.handleUpdateAuthenticatedUser)
	s.route("GET /api/v3/user/{account_id}", s.handleGetUserByAccountID)

	// Email addresses.
	s.route("POST /api/v3/user/emails", s.handleAddUserEmails)
	s.route("DELETE /api/v3/user/emails", s.handleDeleteUserEmails)
	s.route("GET /api/v3/user/public_emails", s.handleListPublicUserEmails)
	s.route("PATCH /api/v3/user/email/visibility", s.handleSetUserEmailVisibility)

	// SSH signing keys single read; list/create/delete live in gh_misc_endpoints.go.
	s.route("GET /api/v3/user/ssh_signing_keys/{ssh_signing_key_id}", s.handleGetMySSHSigningKey)

	// Account interaction limits.
	s.route("GET /api/v3/user/interaction-limits", s.handleGetUserInteractionLimits)
	s.route("PUT /api/v3/user/interaction-limits", s.handleSetUserInteractionLimits)
	s.route("DELETE /api/v3/user/interaction-limits", s.handleDeleteUserInteractionLimits)

	// GitHub Marketplace purchases.
	s.route("GET /api/v3/user/marketplace_purchases", s.handleListUserMarketplacePurchases)
	s.route("GET /api/v3/user/marketplace_purchases/stubbed", s.handleListUserMarketplacePurchases)

	// Cross-repository issue listing for the authenticated user.
	s.route("GET /api/v3/user/issues", s.handleListAuthUserIssues)

	// Profile hovercard.
	s.route("GET /api/v3/users/{username}/hovercard", s.handleGetUserHovercard)

	// Enhanced billing platform usage reports.
	s.route("GET /api/v3/users/{username}/settings/billing/usage", s.handleUserBillingUsage)
	s.route("GET /api/v3/users/{username}/settings/billing/usage/summary", s.handleUserBillingUsageSummary)
	s.route("GET /api/v3/users/{username}/settings/billing/ai_credit/usage", s.handleUserBillingAICreditUsage)
	s.route("GET /api/v3/users/{username}/settings/billing/premium_request/usage", s.handleUserBillingPremiumRequestUsage)
}

// ─── Profile (PATCH /user, GET /user/{account_id}) ──────────────────────

func (s *Server) handleUpdateAuthenticatedUser(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	var req struct {
		Name            *string `json:"name"`
		Email           *string `json:"email"`
		Blog            *string `json:"blog"`
		TwitterUsername *string `json:"twitter_username"`
		Company         *string `json:"company"`
		Location        *string `json:"location"`
		Hireable        *bool   `json:"hireable"`
		Bio             *string `json:"bio"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	updated := s.store.UpdateUserProfile(user.ID, func(u *store.User) {
		if req.Name != nil {
			u.Name = *req.Name
		}
		if req.Email != nil && *req.Email != "" {
			s.store.SetPrimaryEmailLocked(u, *req.Email)
		}
		if req.Blog != nil {
			u.Blog = *req.Blog
		}
		if req.TwitterUsername != nil {
			u.TwitterUsername = *req.TwitterUsername
		}
		if req.Company != nil {
			u.Company = *req.Company
		}
		if req.Location != nil {
			u.Location = *req.Location
		}
		if req.Hireable != nil {
			u.Hireable = req.Hireable
		}
		if req.Bio != nil {
			u.Bio = *req.Bio
		}
	})
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.privateUserJSON(updated))
}

func (s *Server) handleGetUserByAccountID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("account_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	user := s.store.GetUserByID(id)
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Your own account yields the private view, as on GET /user.
	if viewer := ghUserFromContext(r.Context()); viewer != nil && viewer.ID == user.ID {
		writeJSON(w, http.StatusOK, s.privateUserJSON(user))
		return
	}
	writeJSON(w, http.StatusOK, s.fullUserJSON(user, s.baseURL(r)))
}

// privateUserJSON renders GitHub's `private-user` schema, the authenticated
// user's own account view, with private counters derived live from store state.
func (s *Server) privateUserJSON(u *store.User) map[string]interface{} {
	out := s.fullUserJSON(u, s.publicOrigin())
	out["user_view_type"] = "private"
	privateRepos := s.store.CountPrivateRepos(u.Login)
	out["owned_private_repos"] = privateRepos
	out["total_private_repos"] = privateRepos
	out["private_gists"] = s.store.CountSecretGists(u.ID)
	out["collaborators"] = s.store.CountRepoCollaboratorsForOwner(u.Login)
	out["disk_usage"] = s.store.DiskUsageKBForOwner(u.Login)
	out["two_factor_authentication"] = s.store.TwoFactorEnabled(u.ID)
	return out
}

// ─── Email addresses ─────────────────────────────────────────────────────

// userEmailJSON renders one address in GitHub's `email` schema; unset
// visibility is null on the wire.
func userEmailJSON(e store.UserEmail) map[string]interface{} {
	return map[string]interface{}{
		"email":      e.Email,
		"primary":    e.Primary,
		"verified":   e.Verified,
		"visibility": nullableString(e.Visibility),
	}
}

// decodeEmailsBody decodes the flexible body GitHub accepts for POST/DELETE
// /user/emails: {"emails": [...]}, a bare array, or a single string.
func decodeEmailsBody(r *http.Request) ([]string, bool) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return nil, false
	}
	var obj struct {
		Emails []string `json:"emails"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Emails != nil {
		return obj.Emails, true
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, true
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil && single != "" {
		return []string{single}, true
	}
	return nil, false
}

func (s *Server) handleAddUserEmails(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	emails, ok := decodeEmailsBody(r)
	if !ok || len(emails) == 0 {
		store.WriteGHValidationError(w, "Email", "emails", "missing_field")
		return
	}
	for _, e := range emails {
		if strings.TrimSpace(e) == "" {
			store.WriteGHValidationError(w, "Email", "emails", "invalid")
			return
		}
	}
	added, ok := s.store.AddUserEmails(user.ID, emails)
	if !ok {
		store.WriteGHValidationError(w, "Email", "emails", "already_exists")
		return
	}
	out := make([]map[string]interface{}, len(added))
	for i, e := range added {
		out[i] = userEmailJSON(e)
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleDeleteUserEmails(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	emails, ok := decodeEmailsBody(r)
	if !ok || len(emails) == 0 {
		store.WriteGHValidationError(w, "Email", "emails", "missing_field")
		return
	}
	switch s.store.DeleteUserEmails(user.ID, emails) {
	case store.DeleteEmailsOK:
		w.WriteHeader(http.StatusNoContent)
	case store.DeleteEmailsPrimary:
		store.WriteGHValidationError(w, "Email", "emails", "invalid")
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) handleListPublicUserEmails(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	out := []map[string]interface{}{}
	for _, e := range s.store.ListUserEmails(user.ID) {
		if e.Visibility == "public" {
			out = append(out, userEmailJSON(e))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleSetUserEmailVisibility(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	var req struct {
		Visibility string `json:"visibility"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Visibility != "public" && req.Visibility != "private" {
		store.WriteGHValidationError(w, "Email", "visibility", "invalid")
		return
	}
	updated := s.store.SetPrimaryEmailVisibility(user.ID, req.Visibility)
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := make([]map[string]interface{}, len(updated))
	for i, e := range updated {
		out[i] = userEmailJSON(e)
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── Email store methods ─────────────────────────────────────────────────

// ─── SSH signing key single read ─────────────────────────────────────────

func (s *Server) handleGetMySSHSigningKey(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	id, err := strconv.Atoi(r.PathValue("ssh_signing_key_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, entry := range s.store.ListUserSSHSigningKeys(user.ID) {
		if store.SshSigningKeyEntryID(entry) == id {
			writeJSON(w, http.StatusOK, entry)
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

// ─── Account interaction limits ──────────────────────────────────────────

func (s *Server) handleGetUserInteractionLimits(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	limit, expiresAt := s.store.GetUserInteractionLimit(user.ID)
	if limit == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"limit":      limit,
		"origin":     "user",
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleSetUserInteractionLimits(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	var req struct {
		Limit  string `json:"limit"`
		Expiry string `json:"expiry"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Limit == "" {
		store.WriteGHValidationError(w, "InteractionLimit", "limit", "missing_field")
		return
	}
	if !store.IsInteractionGroup(req.Limit) {
		store.WriteGHValidationError(w, "InteractionLimit", "limit", "invalid")
		return
	}
	expiresAt, ok := store.InteractionLimitExpiry(req.Expiry, s.currentTime())
	if !ok {
		store.WriteGHValidationError(w, "InteractionLimit", "expiry", "invalid")
		return
	}
	if !s.store.SetUserInteractionLimit(user.ID, req.Limit, &expiresAt) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"limit":      req.Limit,
		"origin":     "user",
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

func (s *Server) handleDeleteUserInteractionLimits(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if !s.store.SetUserInteractionLimit(user.ID, "", nil) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── GitHub Marketplace purchases ────────────────────────────────────────

func (s *Server) handleListUserMarketplacePurchases(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	for _, listing := range s.store.ListMarketplaceListings(false) {
		s.reconcileMarketplacePurchases(listing.Slug)
	}
	out := []map[string]interface{}{}
	for _, purchase := range s.store.ListMarketplacePurchasesForAccount("User", user.ID) {
		plan := s.store.GetMarketplacePlanForListing(purchase.ListingSlug, purchase.PlanID)
		if plan == nil {
			writeGHError(w, http.StatusInternalServerError, "Marketplace plan not found for purchase")
			return
		}
		out = append(out, s.userMarketplacePurchaseJSON(purchase, plan, user, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) userMarketplacePurchaseJSON(p *store.MarketplacePurchase, plan *store.MarketplacePlan, account *store.User, baseURL string) map[string]interface{} {
	var nextBilling, updatedAt, freeTrialEnds interface{}
	if p.NextBillingDate != nil {
		nextBilling = p.NextBillingDate.UTC().Format(time.RFC3339)
	}
	if p.UpdatedAt != nil {
		updatedAt = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if p.FreeTrialEnds != nil {
		freeTrialEnds = p.FreeTrialEnds.UTC().Format(time.RFC3339)
	}
	var unitCount interface{}
	if p.UnitCount != nil {
		unitCount = *p.UnitCount
	}
	return map[string]interface{}{
		"billing_cycle":      p.BillingCycle,
		"next_billing_date":  nextBilling,
		"unit_count":         unitCount,
		"on_free_trial":      p.OnFreeTrial,
		"free_trial_ends_on": freeTrialEnds,
		"updated_at":         updatedAt,
		"account": map[string]interface{}{
			"url":     baseURL + "/api/v3/users/" + account.Login,
			"id":      account.ID,
			"type":    account.Type,
			"node_id": account.NodeID,
			"login":   account.Login,
		},
		"plan": marketplacePlanJSON(plan, baseURL),
	}
}

// marketplacePlanJSON renders the marketplace-listing-plan schema; the plan
// number is its ID.
func marketplacePlanJSON(p *store.MarketplacePlan, baseURL string) map[string]interface{} {
	planURL := baseURL + "/api/v3/marketplace_listing/plans/" + strconv.Itoa(p.ID)
	return map[string]interface{}{
		"url":                    planURL,
		"accounts_url":           planURL + "/accounts",
		"id":                     p.ID,
		"number":                 p.ID,
		"name":                   p.Name,
		"description":            p.Description,
		"monthly_price_in_cents": p.MonthlyPriceInCents,
		"yearly_price_in_cents":  p.YearlyPriceInCents,
		"price_model":            p.PriceModel,
		"has_free_trial":         p.HasFreeTrial,
		"unit_name":              nil,
		"state":                  p.State,
		"bullets":                append([]string{}, p.Bullets...),
	}
}

// ─── Profile hovercard ───────────────────────────────────────────────────

func (s *Server) handleGetUserHovercard(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	user := s.store.LookupUserByLogin(r.PathValue("username"))
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	subjectType := r.URL.Query().Get("subject_type")
	subjectID := r.URL.Query().Get("subject_id")
	if subjectType != "" {
		switch subjectType {
		case "organization", "repository", "issue", "pull_request":
		default:
			store.WriteGHValidationError(w, "Hovercard", "subject_type", "invalid")
			return
		}
		if subjectID == "" {
			store.WriteGHValidationError(w, "Hovercard", "subject_id", "missing_field")
			return
		}
	}

	contexts := []map[string]interface{}{}
	for _, orgLogin := range s.store.ActiveOrgLoginsForUser(user.ID) {
		contexts = append(contexts, map[string]interface{}{
			"message": "Member of " + orgLogin,
			"octicon": "organization",
		})
	}
	if subjectType == "repository" {
		if repoID, err := strconv.Atoi(subjectID); err == nil {
			if repo := s.store.GetRepoByID(repoID); repo != nil && repo.OwnerID == user.ID {
				contexts = append(contexts, map[string]interface{}{
					"message": "Owns this repository",
					"octicon": "repo",
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"contexts": contexts})
}

// ─── GET /user/issues ────────────────────────────────────────────────────

func (s *Server) handleListAuthUserIssues(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	q := r.URL.Query()
	filter := q.Get("filter")
	if filter == "" {
		filter = "assigned"
	}
	state := q.Get("state")
	if state == "" {
		state = "open"
	}

	pairs := s.store.ListUserFilteredIssues(user, filter)

	var filtered []store.IssueWithRepo
	since := parseSinceTime(r)
	var labelNames []string
	if labelsParam := q.Get("labels"); labelsParam != "" {
		labelNames = strings.Split(labelsParam, ",")
	}
	for _, p := range pairs {
		if !s.viewerCanReadRepo(r.Context(), p.Repo) {
			continue
		}
		switch state {
		case "open":
			if p.Issue.State != "OPEN" {
				continue
			}
		case "closed":
			if p.Issue.State != "CLOSED" {
				continue
			}
		}
		if !since.IsZero() && p.Issue.UpdatedAt.Before(since) {
			continue
		}
		if len(labelNames) > 0 && !store.IssueHasAllLabels(s.store, p.Issue, labelNames, p.Repo.ID) {
			continue
		}
		filtered = append(filtered, p)
	}

	sortKey := q.Get("sort")
	ascending := q.Get("direction") == "asc"
	sort.Slice(filtered, func(i, j int) bool {
		a, b := filtered[i].Issue, filtered[j].Issue
		var less bool
		switch sortKey {
		case "updated":
			less = a.UpdatedAt.Before(b.UpdatedAt)
		case "comments":
			less = s.store.CountIssueComments(a.ID) < s.store.CountIssueComments(b.ID)
		default: // "created"
			less = a.CreatedAt.Before(b.CreatedAt)
		}
		if ascending {
			return less
		}
		return !less
	})

	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(filtered))
	for _, p := range filtered {
		item := issueToJSON(p.Issue, s.store, base, p.Repo.FullName)
		item["repository"] = store.RepoToJSON(p.Repo, s.store, base)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

// ─── Enhanced billing platform usage reports ─────────────────────────────

type billingTimeFilter struct {
	year, month, day int
}

// parseBillingTimeFilter reads year/month/day query params. A missing year
// defaults to the current year; with defaultMonth, a missing month to the
// current month.
func parseBillingTimeFilter(r *http.Request, defaultMonth bool, now time.Time) (billingTimeFilter, error) {
	q := r.URL.Query()
	now = now.UTC()
	f := billingTimeFilter{year: now.Year()}
	if defaultMonth {
		f.month = int(now.Month())
	}
	for _, p := range []struct {
		name string
		dst  *int
	}{{"year", &f.year}, {"month", &f.month}, {"day", &f.day}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return f, fmt.Errorf("invalid %s parameter", p.name)
			}
			*p.dst = n
		}
	}
	return f, nil
}

func (f billingTimeFilter) matches(t time.Time) bool {
	if f.year != 0 && t.Year() != f.year {
		return false
	}
	if f.month != 0 && int(t.Month()) != f.month {
		return false
	}
	if f.day != 0 && t.Day() != f.day {
		return false
	}
	return true
}

// resolveBillingUser authorizes the request: only the account owner or a site
// admin may read a user's usage.
func (s *Server) resolveBillingUser(w http.ResponseWriter, r *http.Request) *store.User {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return nil
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil || (target.ID != viewer.ID && !viewer.SiteAdmin) {
		writeGHError(w, http.StatusForbidden, "Forbidden")
		return nil
	}
	return target
}

// filteredBillingItems applies time/repository/product/sku query filters.
func (s *Server) filteredBillingItems(w http.ResponseWriter, r *http.Request, user *store.User, defaultMonth bool) ([]store.BillingUsageItem, *billingTimeFilter) {
	f, err := parseBillingTimeFilter(r, defaultMonth, s.currentTime())
	if err != nil {
		writeGHError(w, http.StatusBadRequest, err.Error())
		return nil, nil
	}
	q := r.URL.Query()
	repoFilter := q.Get("repository")
	productFilter := q.Get("product")
	skuFilter := q.Get("sku")
	items := s.store.ActionsBillingUsageForOwner(user.Login)
	out := items[:0]
	for _, it := range items {
		if !f.matches(it.Date) {
			continue
		}
		if repoFilter != "" && !strings.EqualFold(it.RepoFullName, repoFilter) {
			continue
		}
		if productFilter != "" && !strings.EqualFold(it.Product, productFilter) {
			continue
		}
		if skuFilter != "" && !strings.EqualFold(it.SKU, skuFilter) {
			continue
		}
		out = append(out, it)
	}
	return out, &f
}

func (s *Server) handleUserBillingUsage(w http.ResponseWriter, r *http.Request) {
	user := s.resolveBillingUser(w, r)
	if user == nil {
		return
	}
	items, f := s.filteredBillingItems(w, r, user, false)
	if f == nil {
		return
	}
	usageItems := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		gross := float64(it.Quantity) * it.PricePerUnit
		usageItems = append(usageItems, map[string]interface{}{
			"date":           it.Date.Format("2006-01-02"),
			"product":        it.Product,
			"sku":            it.SKU,
			"quantity":       it.Quantity,
			"unitType":       it.UnitType,
			"pricePerUnit":   it.PricePerUnit,
			"grossAmount":    gross,
			"discountAmount": 0.0,
			"netAmount":      gross,
			"repositoryName": it.RepoFullName,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"usageItems": usageItems})
}

func (s *Server) handleUserBillingUsageSummary(w http.ResponseWriter, r *http.Request) {
	user := s.resolveBillingUser(w, r)
	if user == nil {
		return
	}
	items, f := s.filteredBillingItems(w, r, user, true)
	if f == nil {
		return
	}
	type aggKey struct{ product, sku string }
	agg := map[aggKey]*struct {
		quantity int
		unitType string
		price    float64
	}{}
	var order []aggKey
	for _, it := range items {
		k := aggKey{it.Product, it.SKU}
		a := agg[k]
		if a == nil {
			a = &struct {
				quantity int
				unitType string
				price    float64
			}{unitType: it.UnitType, price: it.PricePerUnit}
			agg[k] = a
			order = append(order, k)
		}
		a.quantity += it.Quantity
	}
	usageItems := make([]map[string]interface{}, 0, len(order))
	for _, k := range order {
		a := agg[k]
		gross := float64(a.quantity) * a.price
		usageItems = append(usageItems, map[string]interface{}{
			"product":          k.product,
			"sku":              k.sku,
			"unitType":         a.unitType,
			"pricePerUnit":     a.price,
			"grossQuantity":    float64(a.quantity),
			"grossAmount":      gross,
			"discountQuantity": 0.0,
			"discountAmount":   0.0,
			"netQuantity":      float64(a.quantity),
			"netAmount":        gross,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"timePeriod": userBillingTimePeriodJSON(*f),
		"user":       user.Login,
		"usageItems": usageItems,
	})
}

// handleUserBillingAICreditUsage and handleUserBillingPremiumRequestUsage report
// zero usage: no AI-credit- or premium-request-consuming products are modeled.
func (s *Server) handleUserBillingAICreditUsage(w http.ResponseWriter, r *http.Request) {
	s.writeEmptyModelUsageReport(w, r)
}

func (s *Server) handleUserBillingPremiumRequestUsage(w http.ResponseWriter, r *http.Request) {
	s.writeEmptyModelUsageReport(w, r)
}

func (s *Server) writeEmptyModelUsageReport(w http.ResponseWriter, r *http.Request) {
	user := s.resolveBillingUser(w, r)
	if user == nil {
		return
	}
	f, err := parseBillingTimeFilter(r, true, s.currentTime())
	if err != nil {
		writeGHError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"timePeriod": userBillingTimePeriodJSON(f),
		"user":       user.Login,
		"usageItems": []map[string]interface{}{},
	})
}

func userBillingTimePeriodJSON(f billingTimeFilter) map[string]interface{} {
	out := map[string]interface{}{"year": f.year}
	if f.month != 0 {
		out["month"] = f.month
	}
	if f.day != 0 {
		out["day"] = f.day
	}
	return out
}
