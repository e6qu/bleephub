package bleephub

// GitHub Marketplace — the rest of the purchase lifecycle and the listing's
// publication metadata.
//
// gh_marketplace.go owns the purchase/plan-change/cancellation surface and
// the `purchased`, `changed` and `cancelled` webhook actions. This file adds
// the two actions that describe a change that has been *scheduled* rather
// than applied — `pending_change` and `pending_change_cancelled` — plus the
// route that revokes a scheduled change, the Marketplace category taxonomy,
// and the listing profile the GraphQL MarketplaceListing type is rendered
// from.

import (
	"net/http"
	"sort"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHMarketplaceLifecycleRoutes() {
	s.route("DELETE /ui-data/marketplace/listings/{listing_slug}/subscription/pending", s.handleRevokeMarketplacePendingChange)
	s.route("GET /ui-data/marketplace/categories", s.handleListMarketplaceCategories)
	s.route("GET /ui-data/marketplace/listing-profiles", s.handleListMarketplaceListingProfiles)
	s.route("GET /ui-data/marketplace/listings/{listing_slug}/profile", s.handleGetMarketplaceListingProfile)
	s.route("PUT /ui-data/marketplace/listings/{listing_slug}/profile", s.handlePutMarketplaceListingProfile)
}

// ---------------------------------------------------------------------------
// pending-change webhooks

// marketplacePendingPurchaseView renders the subscription as it will stand
// once the scheduled change takes effect. GitHub's `pending_change` payload
// describes the *upcoming* state in `marketplace_purchase` and the current
// one in `previous_marketplace_purchase`, so a subscriber can diff them.
func marketplacePendingPurchaseView(purchase *store.MarketplacePurchase, plan *store.MarketplacePlan) *store.MarketplacePurchase {
	upcoming := store.CloneMarketplacePurchase(purchase)
	pending := purchase.PendingChange
	if pending == nil {
		return upcoming
	}
	if pending.PlanID != 0 && plan != nil {
		upcoming.PlanID, upcoming.PlanName = plan.ID, plan.Name
	}
	if pending.BillingCycle != "" {
		upcoming.BillingCycle = pending.BillingCycle
	}
	if pending.UnitCount != nil {
		units := *pending.UnitCount
		upcoming.UnitCount = &units
	}
	effective := pending.EffectiveDate
	upcoming.NextBillingDate = &effective
	upcoming.PendingChange = nil
	return upcoming
}

// emitMarketplacePendingChange fires the `pending_change` action for a
// downgrade or cancellation scheduled for the next billing date.
func (s *Server) emitMarketplacePendingChange(listing *store.MarketplaceListing, purchase *store.MarketplacePurchase, sender *store.User) {
	pending := purchase.PendingChange
	if listing == nil || pending == nil {
		return
	}
	plan := s.store.GetMarketplacePlanForListing(listing.Slug, pending.PlanID)
	upcoming := marketplacePendingPurchaseView(purchase, plan)
	baseURL := s.baseURLFromConfig()
	payload := map[string]interface{}{
		"action":                        "pending_change",
		"effective_date":                pending.EffectiveDate.UTC().Format(time.RFC3339),
		"marketplace_purchase":          s.marketplacePurchaseWebhookJSON(listing, upcoming, baseURL),
		"previous_marketplace_purchase": s.marketplacePurchaseWebhookJSON(listing, purchase, baseURL),
		"sender":                        store.UserToJSON(sender, s.publicOrigin()),
	}
	s.emitMarketplaceWebhook(listing, "marketplace_purchase", "pending_change", payload)
}

// emitMarketplacePendingChangeCancelled fires `pending_change_cancelled`
// for a scheduled change the account revoked before it took effect. The
// purchase passed in is the one that still carries the pending change, so
// the payload can say what was called off.
func (s *Server) emitMarketplacePendingChangeCancelled(listing *store.MarketplaceListing, purchase *store.MarketplacePurchase, sender *store.User) {
	pending := purchase.PendingChange
	if listing == nil || pending == nil {
		return
	}
	plan := s.store.GetMarketplacePlanForListing(listing.Slug, pending.PlanID)
	upcoming := marketplacePendingPurchaseView(purchase, plan)
	baseURL := s.baseURLFromConfig()
	settled := store.CloneMarketplacePurchase(purchase)
	settled.PendingChange = nil
	payload := map[string]interface{}{
		"action":                        "pending_change_cancelled",
		"effective_date":                pending.EffectiveDate.UTC().Format(time.RFC3339),
		"marketplace_purchase":          s.marketplacePurchaseWebhookJSON(listing, settled, baseURL),
		"previous_marketplace_purchase": s.marketplacePurchaseWebhookJSON(listing, upcoming, baseURL),
		"sender":                        store.UserToJSON(sender, s.publicOrigin()),
	}
	s.emitMarketplaceWebhook(listing, "marketplace_purchase", "pending_change_cancelled", payload)
}

// handleRevokeMarketplacePendingChange drops a scheduled plan change or
// cancellation, leaving the subscription on its current plan.
func (s *Server) handleRevokeMarketplacePendingChange(w http.ResponseWriter, r *http.Request) {
	user, _ := s.marketplaceBrowserUser(w, r)
	if user == nil {
		return
	}
	s.marketplaceMu.Lock()
	defer s.marketplaceMu.Unlock()
	listing := s.store.GetMarketplaceListing(r.PathValue("listing_slug"))
	if listing == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.reconcileMarketplacePurchasesLocked(listing.Slug)
	account, ok := s.marketplaceBuyerAccount(r.Context(), w, user, r.URL.Query().Get("account"))
	if !ok {
		return
	}
	purchase := s.store.GetMarketplacePurchase(listing.Slug, account.AccountType, account.Id)
	if purchase == nil || purchase.PendingChange == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	plan := s.store.GetMarketplacePlanForListing(listing.Slug, purchase.PlanID)
	if plan == nil {
		writeGHError(w, http.StatusInternalServerError, "Current Marketplace plan not found")
		return
	}
	scheduled := store.CloneMarketplacePurchase(purchase)
	now := s.currentTime()
	purchase.PendingChange = nil
	purchase.UpdatedAt = &now
	if err := s.store.SaveMarketplacePurchase(purchase); err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.emitMarketplacePendingChangeCancelled(listing, scheduled, user)
	writeJSON(w, http.StatusOK, s.marketplaceBrowserSubscriptionJSON(purchase, plan, listing, account.Login, s.baseURL(r)))
}

// ---------------------------------------------------------------------------
// categories and listing profiles

func marketplaceCategoryJSON(category *store.MarketplaceCategory, primary, secondary int) map[string]interface{} {
	return map[string]interface{}{
		"id":                      category.ID,
		"node_id":                 category.NodeID,
		"slug":                    category.Slug,
		"name":                    category.Name,
		"description":             category.Description,
		"how_it_works":            category.HowItWorks,
		"primary_listing_count":   primary,
		"secondary_listing_count": secondary,
	}
}

// marketplaceCategoryListingCounts counts, per category slug, how many
// published listings name it as their primary and secondary category.
func (s *Server) marketplaceCategoryListingCounts() (primary, secondary map[string]int) {
	primary, secondary = map[string]int{}, map[string]int{}
	published := map[string]bool{}
	for _, listing := range s.store.ListMarketplaceListings(true) {
		published[listing.Slug] = true
	}
	for _, profile := range s.store.MarketplaceProfiles.ListMarketplaceListingProfiles() {
		if !published[profile.Slug] {
			continue
		}
		primary[profile.PrimaryCategorySlug]++
		if profile.SecondaryCategorySlug != "" {
			secondary[profile.SecondaryCategorySlug]++
		}
	}
	return primary, secondary
}

func (s *Server) handleListMarketplaceCategories(w http.ResponseWriter, r *http.Request) {
	user, _ := s.marketplaceBrowserUser(w, r)
	if user == nil {
		return
	}
	primary, secondary := s.marketplaceCategoryListingCounts()
	rows := []map[string]interface{}{}
	for _, category := range s.store.MarketplaceProfiles.ListMarketplaceCategories(nil, false, false, primary) {
		rows = append(rows, marketplaceCategoryJSON(category, primary[category.Slug], secondary[category.Slug]))
	}
	writeJSON(w, http.StatusOK, rows)
}

func marketplaceProfileJSON(profile *store.MarketplaceListingProfile) map[string]interface{} {
	if profile == nil {
		return nil
	}
	screenshots := profile.ScreenshotURLs
	if screenshots == nil {
		screenshots = []string{}
	}
	return map[string]interface{}{
		"slug":                    profile.Slug,
		"node_id":                 profile.NodeID,
		"primary_category_slug":   profile.PrimaryCategorySlug,
		"secondary_category_slug": profile.SecondaryCategorySlug,
		"extended_description":    profile.ExtendedDescription,
		"how_it_works":            profile.HowItWorks,
		"logo_url":                profile.LogoURL,
		"logo_background_color":   profile.LogoBackgroundColor,
		"screenshot_urls":         screenshots,
		"company_url":             profile.CompanyURL,
		"documentation_url":       profile.DocumentationURL,
		"pricing_url":             profile.PricingURL,
		"privacy_policy_url":      profile.PrivacyPolicyURL,
		"status_url":              profile.StatusURL,
		"support_email":           profile.SupportEmail,
		"support_url":             profile.SupportURL,
		"terms_of_service_url":    profile.TermsOfServiceURL,
		"state":                   profile.State,
		"has_verified_owner":      profile.HasVerifiedOwner,
	}
}

// handleListMarketplaceListingProfiles serves every published listing's
// profile in one response so the Marketplace browser can group listings by
// category without a request per listing.
func (s *Server) handleListMarketplaceListingProfiles(w http.ResponseWriter, r *http.Request) {
	user, _ := s.marketplaceBrowserUser(w, r)
	if user == nil {
		return
	}
	published := map[string]bool{}
	for _, listing := range s.store.ListMarketplaceListings(true) {
		published[listing.Slug] = true
	}
	rows := []map[string]interface{}{}
	for slug := range published {
		profile := s.store.MarketplaceProfiles.GetMarketplaceListingProfile(slug)
		if profile == nil {
			profile = store.DefaultMarketplaceListingProfile(slug)
		}
		rows = append(rows, marketplaceProfileJSON(profile))
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["slug"].(string) < rows[j]["slug"].(string)
	})
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleGetMarketplaceListingProfile(w http.ResponseWriter, r *http.Request) {
	user, _ := s.marketplaceBrowserUser(w, r)
	if user == nil {
		return
	}
	listing := s.store.GetMarketplaceListing(r.PathValue("listing_slug"))
	if listing == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"profile": marketplaceProfileJSON(s.store.MarketplaceProfiles.GetMarketplaceListingProfile(listing.Slug)),
	})
}

// handlePutMarketplaceListingProfile is publisher-only: the listing's
// marketing metadata belongs to whoever owns the app behind it.
func (s *Server) handlePutMarketplaceListingProfile(w http.ResponseWriter, r *http.Request) {
	user, r := s.marketplaceBrowserUser(w, r)
	if user == nil {
		return
	}
	listing := s.store.GetMarketplaceListing(r.PathValue("listing_slug"))
	if listing == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerOwnsMarketplaceListing(user, listing) {
		writeGHError(w, http.StatusForbidden, "Must be the publisher of this Marketplace listing.")
		return
	}
	var req struct {
		PrimaryCategorySlug   *string  `json:"primary_category_slug"`
		SecondaryCategorySlug *string  `json:"secondary_category_slug"`
		ExtendedDescription   *string  `json:"extended_description"`
		HowItWorks            *string  `json:"how_it_works"`
		LogoURL               *string  `json:"logo_url"`
		LogoBackgroundColor   *string  `json:"logo_background_color"`
		ScreenshotURLs        []string `json:"screenshot_urls"`
		CompanyURL            *string  `json:"company_url"`
		DocumentationURL      *string  `json:"documentation_url"`
		PricingURL            *string  `json:"pricing_url"`
		PrivacyPolicyURL      *string  `json:"privacy_policy_url"`
		StatusURL             *string  `json:"status_url"`
		SupportEmail          *string  `json:"support_email"`
		SupportURL            *string  `json:"support_url"`
		TermsOfServiceURL     *string  `json:"terms_of_service_url"`
		State                 *string  `json:"state"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	profile, err := s.store.MarketplaceProfiles.SaveMarketplaceListingProfile(listing.Slug, store.MarketplaceProfileUpdate{
		PrimaryCategorySlug:   req.PrimaryCategorySlug,
		SecondaryCategorySlug: req.SecondaryCategorySlug,
		ExtendedDescription:   req.ExtendedDescription,
		HowItWorks:            req.HowItWorks,
		LogoURL:               req.LogoURL,
		LogoBackgroundColor:   req.LogoBackgroundColor,
		ScreenshotURLs:        req.ScreenshotURLs,
		CompanyURL:            req.CompanyURL,
		DocumentationURL:      req.DocumentationURL,
		PricingURL:            req.PricingURL,
		PrivacyPolicyURL:      req.PrivacyPolicyURL,
		StatusURL:             req.StatusURL,
		SupportEmail:          req.SupportEmail,
		SupportURL:            req.SupportURL,
		TermsOfServiceURL:     req.TermsOfServiceURL,
		State:                 req.State,
	})
	if err != nil {
		store.WriteGHValidationError(w, "MarketplaceListing", "primary_category", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, marketplaceProfileJSON(profile))
}

// viewerOwnsMarketplaceListing reports whether the account publishes the
// app the listing is for. It is the same ownership test the settings
// surface applies, expressed over a listing rather than a publisher path.
func (s *Server) viewerOwnsMarketplaceListing(user *store.User, listing *store.MarketplaceListing) bool {
	if user == nil || listing == nil {
		return false
	}
	if user.SiteAdmin {
		return true
	}
	if listing.GitHubAppID != 0 {
		app := s.store.GetApp(listing.GitHubAppID)
		return app != nil && app.OwnerID == user.ID
	}
	if listing.OAuthAppClientID != "" {
		app := s.store.GetOAuthApp(listing.OAuthAppClientID)
		return app != nil && app.OwnerID == user.ID
	}
	return false
}
