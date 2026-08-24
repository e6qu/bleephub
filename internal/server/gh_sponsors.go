package bleephub

// GitHub Sponsors — the HTTP half: the browser surface the /ui Sponsors
// pages read and write, the `sponsorship` webhook family, and the
// billing-cycle reconciliation that turns a scheduled tier change or
// cancellation into a real transition once its effective date arrives.
//
// GitHub publishes no REST API for Sponsors — the whole product is
// GraphQL plus webhooks — so every route here lives under /ui-data, which
// is the simulator's own browser surface. The API contract clients see is
// the GraphQL one in internal/graphqlapi/gh_sponsors_graphql.go.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHSponsorsRoutes() {
	s.route("GET /ui-data/sponsors/listings", s.handleListSponsorsListings)
	s.route("GET /ui-data/sponsors/sponsoring", s.handleListViewerSponsoring)
	s.route("GET /ui-data/sponsors/{login}", s.handleGetSponsorsListing)
	s.route("PUT /ui-data/sponsors/{login}", s.handlePutSponsorsListing)
	s.route("GET /ui-data/sponsors/{login}/dashboard", s.handleSponsorsDashboard)
	s.route("POST /ui-data/sponsors/{login}/tiers", s.handleCreateSponsorsTier)
	s.route("PATCH /ui-data/sponsors/{login}/tiers/{tier_id}", s.handleUpdateSponsorsTierState)
	s.route("PUT /ui-data/sponsors/{login}/goal", s.handleSetSponsorsGoal)
	s.route("DELETE /ui-data/sponsors/{login}/goal", s.handleClearSponsorsGoal)
	s.route("POST /ui-data/sponsors/{login}/featured", s.handleFeatureSponsorsItem)
	s.route("DELETE /ui-data/sponsors/{login}/featured/{item_id}", s.handleUnfeatureSponsorsItem)
	s.route("GET /ui-data/sponsors/{login}/newsletters", s.handleListSponsorshipNewsletters)
	s.route("POST /ui-data/sponsors/{login}/newsletters", s.handleCreateSponsorshipNewsletter)
	s.route("POST /ui-data/sponsors/{login}/newsletters/{newsletter_id}/publish", s.handlePublishSponsorshipNewsletter)
	s.route("POST /ui-data/sponsors/{login}/payouts", s.handleRunSponsorsPayout)
	s.route("POST /ui-data/sponsors/{login}/sponsorship", s.handleCreateSponsorshipBrowser)
	s.route("PATCH /ui-data/sponsors/{login}/sponsorship", s.handleUpdateSponsorshipBrowser)
	s.route("DELETE /ui-data/sponsors/{login}/sponsorship", s.handleCancelSponsorshipBrowser)
}

// ---------------------------------------------------------------------------
// account resolution and authorization

// SponsorableAccount is a user or an organization viewed as a party to a
// sponsorship. Sponsors and sponsorables are the same shape, so one type
// serves both sides.
type SponsorableAccount struct {
	ID    int
	Type  string // User | Organization
	Login string
}

// SponsorableAccountByLogin resolves a login to the account behind it,
// preferring the user table so a user and an organization can never be
// confused by case.
func (s *Server) SponsorableAccountByLogin(login string) (SponsorableAccount, bool) {
	if login == "" {
		return SponsorableAccount{}, false
	}
	if user := s.store.LookupUserByLogin(login); user != nil {
		return SponsorableAccount{ID: user.ID, Type: "User", Login: user.Login}, true
	}
	if org := s.store.GetOrg(login); org != nil {
		return SponsorableAccount{ID: org.ID, Type: "Organization", Login: org.Login}, true
	}
	return SponsorableAccount{}, false
}

// ViewerCanAdminSponsorable reports whether the request may manage the
// account's Sponsors listing: the user themselves, an owner of the
// organization, or a site administrator.
func (s *Server) ViewerCanAdminSponsorable(r *http.Request, viewer *store.User, account SponsorableAccount) bool {
	if viewer == nil {
		return false
	}
	if viewer.SiteAdmin {
		return true
	}
	if account.Type == "User" {
		return account.ID == viewer.ID
	}
	return s.viewerCanAdminOrg(r.Context(), account.Login)
}

// ViewerCanSpendAsSponsor reports whether the request may fund a
// sponsorship out of the named account: their own account, or an
// organization they administer.
func (s *Server) ViewerCanSpendAsSponsor(r *http.Request, viewer *store.User, account SponsorableAccount) bool {
	return s.ViewerCanAdminSponsorable(r, viewer, account)
}

// sponsorsBrowserUser authenticates a /ui-data Sponsors request.
func (s *Server) sponsorsBrowserUser(w http.ResponseWriter, r *http.Request) (*store.User, *http.Request) {
	return s.personalAccessTokenWebUser(w, r)
}

// sponsorsAccountFromPath resolves the {login} path segment, answering 404
// for an account that does not exist.
func (s *Server) sponsorsAccountFromPath(w http.ResponseWriter, r *http.Request) (SponsorableAccount, bool) {
	account, ok := s.SponsorableAccountByLogin(r.PathValue("login"))
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return SponsorableAccount{}, false
	}
	return account, true
}

// ---------------------------------------------------------------------------
// billing-cycle reconciliation

// ReconcileSponsorships applies every sponsorship whose next billing date
// has arrived and emits the webhook each transition produces. It runs
// before any Sponsors read or write so a scheduled downgrade or
// cancellation takes effect on time without a background ticker.
func (s *Server) ReconcileSponsorships() {
	for _, transition := range s.store.Sponsors.AdvanceSponsorshipBillingCycles(s.currentTime()) {
		action := "tier_changed"
		switch {
		case transition.Sponsorship != nil && !transition.Sponsorship.IsActive:
			action = "cancelled"
		case transition.PreviousTier != nil && transition.Tier != nil && transition.PreviousTier.ID == transition.Tier.ID:
			// A plain renewal bills another period without changing
			// anything a subscriber could act on, so it is not an event.
			continue
		}
		s.emitSponsorshipEvent(action, transition, nil)
	}
}

// ---------------------------------------------------------------------------
// webhooks

// sponsorsTierWebhookJSON renders a tier the way GitHub's `sponsorship`
// payload does. GitHub's payload really does spell the custom-amount flag
// `is_custom_ammount`; reproducing the misspelling is what lets an
// unmodified consumer read the field.
func sponsorsTierWebhookJSON(tier *store.SponsorsTier) map[string]interface{} {
	if tier == nil {
		return nil
	}
	return map[string]interface{}{
		"node_id":                  tier.NodeID,
		"created_at":               tier.CreatedAt.UTC().Format(time.RFC3339),
		"description":              tier.Description,
		"monthly_price_in_cents":   tier.MonthlyPriceInCents,
		"monthly_price_in_dollars": tier.MonthlyPriceInCents / 100,
		"name":                     tier.Name,
		"is_one_time":              tier.IsOneTime,
		"is_custom_ammount":        tier.IsCustomAmount,
	}
}

// sponsorsAccountWebhookJSON renders the sponsor or sponsorable side of a
// sponsorship as the account object GitHub attaches.
func (s *Server) sponsorsAccountWebhookJSON(login, accountType string) map[string]interface{} {
	if accountType == "Organization" {
		if org := s.store.GetOrg(login); org != nil {
			return orgWebhookPayload(org, s.publicOrigin())
		}
		return nil
	}
	if user := s.store.LookupUserByLogin(login); user != nil {
		return store.UserToJSON(user, s.publicOrigin())
	}
	return nil
}

// sponsorshipWebhookJSON renders the `sponsorship` member of the payload.
func (s *Server) sponsorshipWebhookJSON(sponsorship *store.Sponsorship, tier *store.SponsorsTier) map[string]interface{} {
	if sponsorship == nil {
		return nil
	}
	return map[string]interface{}{
		"node_id":       sponsorship.NodeID,
		"created_at":    sponsorship.CreatedAt.UTC().Format(time.RFC3339),
		"sponsorable":   s.sponsorsAccountWebhookJSON(sponsorship.SponsorableLogin, sponsorship.SponsorableType),
		"sponsor":       s.sponsorsAccountWebhookJSON(sponsorship.SponsorLogin, sponsorship.SponsorType),
		"privacy_level": strings.ToLower(sponsorship.PrivacyLevel),
		"tier":          sponsorsTierWebhookJSON(tier),
	}
}

// emitSponsorshipEvent delivers a `sponsorship` webhook for a lifecycle
// transition. It fans out to the sponsorable organization's hooks and to
// every GitHub App installed on the sponsorable that subscribed to the
// event — the two places GitHub delivers a sponsorship from.
func (s *Server) emitSponsorshipEvent(action string, transition *store.SponsorsTransition, sender *store.User) {
	if transition == nil || transition.Sponsorship == nil {
		return
	}
	sponsorship := transition.Sponsorship
	payload := map[string]interface{}{
		"action":      action,
		"sponsorship": s.sponsorshipWebhookJSON(sponsorship, transition.Tier),
	}
	if sender != nil {
		payload["sender"] = store.UserToJSON(sender, s.publicOrigin())
	} else {
		payload["sender"] = s.sponsorsAccountWebhookJSON(sponsorship.SponsorLogin, sponsorship.SponsorType)
	}
	switch action {
	case "pending_cancellation", "pending_tier_change":
		if sponsorship.PendingEffectiveDate != nil {
			payload["effective_date"] = sponsorship.PendingEffectiveDate.UTC().Format(time.RFC3339)
		}
	}
	changes := map[string]interface{}{}
	if transition.Previous != nil {
		if transition.Previous.PrivacyLevel != sponsorship.PrivacyLevel {
			changes["privacy_level"] = map[string]interface{}{"from": strings.ToLower(transition.Previous.PrivacyLevel)}
		}
	}
	if transition.PreviousTier != nil && transition.Tier != nil && transition.PreviousTier.ID != transition.Tier.ID {
		changes["tier"] = map[string]interface{}{"from": sponsorsTierWebhookJSON(transition.PreviousTier)}
	}
	if action == "pending_tier_change" && transition.PreviousTier != nil {
		changes["tier"] = map[string]interface{}{"from": sponsorsTierWebhookJSON(transition.PreviousTier)}
	}
	if len(changes) > 0 {
		payload["changes"] = changes
	}

	if sponsorship.SponsorableType == "Organization" {
		payload["organization"] = s.sponsorsAccountWebhookJSON(sponsorship.SponsorableLogin, "Organization")
		s.emitOrgWebhookEvent(sponsorship.SponsorableLogin, "sponsorship", action, payload)
	}
	body := mustMarshal(payload)
	for _, installation := range s.store.ListInstallationsForTarget(sponsorship.SponsorableLogin) {
		app := s.store.GetApp(installation.AppID)
		if app == nil || app.WebhookURL == "" || !app.WebhookActive {
			continue
		}
		if !appSubscribesToEvent(app, "sponsorship") {
			continue
		}
		installationID := installation.ID
		s.enqueueWebhookJob(appWebhookQueueKey(app), func() {
			s.deliverAppWebhook(app, "sponsorship", action, installationID, body)
		})
	}
}

// appSubscribesToEvent reports whether a GitHub App asked for the event.
func appSubscribesToEvent(app *store.App, event string) bool {
	for _, subscribed := range app.WebhookEvents {
		if subscribed == "*" || subscribed == event {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// rendering

func sponsorsTierJSON(tier *store.SponsorsTier) map[string]interface{} {
	if tier == nil {
		return nil
	}
	return map[string]interface{}{
		"id":                       tier.ID,
		"node_id":                  tier.NodeID,
		"name":                     tier.Name,
		"description":              tier.Description,
		"monthly_price_in_cents":   tier.MonthlyPriceInCents,
		"monthly_price_in_dollars": tier.MonthlyPriceInCents / 100,
		"is_one_time":              tier.IsOneTime,
		"is_custom_amount":         tier.IsCustomAmount,
		"is_draft":                 tier.IsDraft,
		"is_published":             tier.IsPublished,
		"is_retired":               tier.IsRetired,
		"welcome_message":          tier.WelcomeMessage,
		"created_at":               tier.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":               tier.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) sponsorsGoalJSON(listing *store.SponsorsListing) map[string]interface{} {
	if listing == nil || listing.ActiveGoal == nil {
		return nil
	}
	kind, target, percent, ok := s.store.Sponsors.SponsorsGoalProgress(listing.ID)
	if !ok {
		return nil
	}
	return map[string]interface{}{
		"kind":             kind,
		"target_value":     target,
		"percent_complete": percent,
		"description":      listing.ActiveGoal.Description,
		"title":            SponsorsGoalTitle(kind, target),
	}
}

// SponsorsGoalTitle is GitHub's one-line summary of a goal.
func SponsorsGoalTitle(kind string, target int) string {
	if kind == store.SponsorsGoalTotalSponsors {
		return "Reach " + strconv.Itoa(target) + " sponsors"
	}
	return "Earn $" + strconv.Itoa(target/100) + " per month"
}

func (s *Server) sponsorsFeaturedItemJSON(item *store.SponsorsListingFeaturedItem) map[string]interface{} {
	out := map[string]interface{}{
		"id":               item.ID,
		"node_id":          item.NodeID,
		"featureable_type": item.FeatureableType,
		"description":      item.Description,
		"position":         item.Position,
	}
	if item.FeatureableType == store.SponsorsFeatureableRepository {
		if repo := s.store.GetRepoByID(item.FeatureableID); repo != nil {
			out["featureable"] = map[string]interface{}{"name": repo.Name, "full_name": repo.FullName, "description": repo.Description}
		}
	} else if user := s.store.GetUserByID(item.FeatureableID); user != nil {
		out["featureable"] = map[string]interface{}{"login": user.Login, "name": user.Name, "avatar_url": user.AvatarURL}
	}
	return out
}

// sponsorsListingJSON renders a listing for the browser. Maintainer-only
// members (contact email, billing country, payout schedule) are attached
// only when the viewer can administer the listing — the same boundary the
// GraphQL surface draws.
func (s *Server) sponsorsListingJSON(listing *store.SponsorsListing, isMaintainer bool) map[string]interface{} {
	if listing == nil {
		return nil
	}
	out := map[string]interface{}{
		"id":                listing.ID,
		"node_id":           listing.NodeID,
		"slug":              listing.Slug,
		"name":              listing.Name,
		"sponsorable_login": listing.SponsorableLogin,
		"sponsorable_type":  listing.SponsorableType,
		"short_description": listing.ShortDescription,
		"full_description":  listing.FullDescription,
		"is_public":         listing.IsPublic,
		"patreon_enabled":   listing.PatreonSponsorshipsEnabled,
		"fiscal_host":       listing.FiscalHostLogin,
		"created_at":        listing.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":        listing.UpdatedAt.UTC().Format(time.RFC3339),
		"goal":              s.sponsorsGoalJSON(listing),
	}
	tiers := []map[string]interface{}{}
	for _, tier := range s.store.Sponsors.ListSponsorsTiers(listing.ID, isMaintainer) {
		tiers = append(tiers, sponsorsTierJSON(tier))
	}
	out["tiers"] = tiers
	featured := []map[string]interface{}{}
	for _, item := range s.store.Sponsors.ListSponsorsListingFeaturedItems(listing.ID, nil) {
		featured = append(featured, s.sponsorsFeaturedItemJSON(item))
	}
	out["featured_items"] = featured
	if isMaintainer {
		out["contact_email"] = listing.ContactEmail
		out["billing_country_or_region"] = listing.BillingCountryOrRegion
		out["residence_country_or_region"] = listing.ResidenceCountryOrRegion
		out["next_payout_date"] = listing.NextPayoutDate
		out["payout_minimum_in_cents"] = listing.PayoutMinimumInCents
		out["monthly_estimated_income_in_cents"] = s.store.Sponsors.MonthlyEstimatedSponsorsIncomeInCents(listing.SponsorableLogin)
		out["estimated_next_payout_in_cents"] = s.store.Sponsors.EstimatedNextSponsorsPayoutInCents(listing.SponsorableLogin)
	}
	return out
}

// sponsorshipVisibleTo applies sponsorship privacy: a private sponsorship
// is visible only to its sponsor, to the maintainer receiving it, and to a
// site administrator. Everyone else must not be able to tell it exists.
func (s *Server) sponsorshipVisibleTo(r *http.Request, viewer *store.User, sponsorship *store.Sponsorship) bool {
	if sponsorship == nil {
		return false
	}
	if sponsorship.PrivacyLevel != store.SponsorshipPrivacyPrivate {
		return true
	}
	if viewer == nil {
		return false
	}
	if viewer.SiteAdmin {
		return true
	}
	sponsor := SponsorableAccount{ID: sponsorship.SponsorID, Type: sponsorship.SponsorType, Login: sponsorship.SponsorLogin}
	sponsorable := SponsorableAccount{ID: sponsorship.SponsorableID, Type: sponsorship.SponsorableType, Login: sponsorship.SponsorableLogin}
	return s.ViewerCanAdminSponsorable(r, viewer, sponsor) || s.ViewerCanAdminSponsorable(r, viewer, sponsorable)
}

func (s *Server) sponsorshipJSON(sponsorship *store.Sponsorship) map[string]interface{} {
	tier := s.store.Sponsors.GetSponsorsTier(sponsorship.TierID)
	out := map[string]interface{}{
		"id":                     sponsorship.ID,
		"node_id":                sponsorship.NodeID,
		"sponsor_login":          sponsorship.SponsorLogin,
		"sponsor_type":           sponsorship.SponsorType,
		"sponsorable_login":      sponsorship.SponsorableLogin,
		"sponsorable_type":       sponsorship.SponsorableType,
		"privacy_level":          sponsorship.PrivacyLevel,
		"payment_source":         sponsorship.PaymentSource,
		"is_one_time_payment":    sponsorship.IsOneTimePayment,
		"is_active":              sponsorship.IsActive,
		"receive_emails":         sponsorship.IsSponsorOptedIntoEmail,
		"amount_in_cents":        sponsorship.AmountInCents,
		"created_at":             sponsorship.CreatedAt.UTC().Format(time.RFC3339),
		"tier":                   sponsorsTierJSON(tier),
		"pending_cancellation":   sponsorship.PendingCancellation,
		"pending_tier_id":        sponsorship.PendingTierID,
		"next_billing_date":      nil,
		"pending_effective_date": nil,
	}
	if sponsorship.NextBillingDate != nil {
		out["next_billing_date"] = sponsorship.NextBillingDate.UTC().Format(time.RFC3339)
	}
	if sponsorship.PendingEffectiveDate != nil {
		out["pending_effective_date"] = sponsorship.PendingEffectiveDate.UTC().Format(time.RFC3339)
	}
	return out
}

// ---------------------------------------------------------------------------
// handlers — listings

func (s *Server) handleListSponsorsListings(w http.ResponseWriter, r *http.Request) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return
	}
	s.ReconcileSponsorships()
	rows := []map[string]interface{}{}
	for _, listing := range s.store.Sponsors.ListSponsorsListings() {
		account := SponsorableAccount{ID: listing.SponsorableID, Type: listing.SponsorableType, Login: listing.SponsorableLogin}
		maintainer := s.ViewerCanAdminSponsorable(r, user, account)
		if !listing.IsPublic && !maintainer {
			continue
		}
		rows = append(rows, s.sponsorsListingJSON(listing, maintainer))
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleGetSponsorsListing(w http.ResponseWriter, r *http.Request) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return
	}
	account, ok := s.sponsorsAccountFromPath(w, r)
	if !ok {
		return
	}
	s.ReconcileSponsorships()
	listing := s.store.Sponsors.GetSponsorsListingForAccount(account.Login)
	maintainer := s.ViewerCanAdminSponsorable(r, user, account)
	if listing == nil || (!listing.IsPublic && !maintainer) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"listing": nil, "viewer_can_admin": maintainer, "viewer_sponsorship": nil,
			"sponsors": []map[string]interface{}{},
		})
		return
	}
	// The sponsor list is what the profile page shows; a private
	// sponsorship is dropped from it for anybody but its two parties.
	sponsors := []map[string]interface{}{}
	for _, sponsorship := range s.store.Sponsors.ListSponsorshipsAsMaintainer(account.Login, true) {
		if !s.sponsorshipVisibleTo(r, user, sponsorship) {
			continue
		}
		sponsors = append(sponsors, s.sponsorshipJSON(sponsorship))
	}
	body := map[string]interface{}{
		"listing": s.sponsorsListingJSON(listing, maintainer), "viewer_can_admin": maintainer,
		"viewer_sponsorship": nil, "sponsors": sponsors,
	}
	if sponsorship := s.store.Sponsors.GetSponsorshipBetween(user.Login, account.Login, true); sponsorship != nil {
		body["viewer_sponsorship"] = s.sponsorshipJSON(sponsorship)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePutSponsorsListing(w http.ResponseWriter, r *http.Request) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return
	}
	account, ok := s.sponsorsAccountFromPath(w, r)
	if !ok {
		return
	}
	if !s.ViewerCanAdminSponsorable(r, user, account) {
		writeGHError(w, http.StatusForbidden, "Must be able to manage this account's GitHub Sponsors profile.")
		return
	}
	var req struct {
		Name                     string `json:"name"`
		ShortDescription         string `json:"short_description"`
		FullDescription          string `json:"full_description"`
		ContactEmail             string `json:"contact_email"`
		BillingCountryOrRegion   string `json:"billing_country_or_region"`
		ResidenceCountryOrRegion string `json:"residence_country_or_region"`
		FiscalHostLogin          string `json:"fiscal_host_login"`
		IsPublic                 *bool  `json:"is_public"`
		PatreonEnabled           *bool  `json:"patreon_enabled"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	listing := s.store.Sponsors.GetSponsorsListingForAccount(account.Login)
	if listing == nil {
		created, err := s.store.Sponsors.CreateSponsorsListing(store.SponsorsListingInput{
			SponsorableID: account.ID, SponsorableType: account.Type, SponsorableLogin: account.Login,
			Name: req.Name, ShortDescription: req.ShortDescription, FullDescription: req.FullDescription,
			ContactEmail: req.ContactEmail, BillingCountryOrRegion: req.BillingCountryOrRegion,
			ResidenceCountryOrRegion: req.ResidenceCountryOrRegion, FiscalHostLogin: req.FiscalHostLogin,
		})
		if err != nil {
			store.WriteGHValidationError(w, "SponsorsListing", "sponsorable", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, s.sponsorsListingJSON(created, true))
		return
	}
	updated := s.store.Sponsors.UpdateSponsorsListing(listing.ID, store.SponsorsListingUpdate{
		Name: &req.Name, ShortDescription: &req.ShortDescription, FullDescription: &req.FullDescription,
		ContactEmail: &req.ContactEmail, BillingCountryOrRegion: &req.BillingCountryOrRegion,
		ResidenceCountryOrRegion: &req.ResidenceCountryOrRegion, FiscalHostLogin: &req.FiscalHostLogin,
		IsPublic: req.IsPublic, PatreonEnabled: req.PatreonEnabled,
	})
	writeJSON(w, http.StatusOK, s.sponsorsListingJSON(updated, true))
}

// sponsorsMaintainerListing resolves the {login} listing and refuses a
// viewer who cannot administer it.
func (s *Server) sponsorsMaintainerListing(w http.ResponseWriter, r *http.Request) (*store.SponsorsListing, *store.User, *http.Request, bool) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return nil, nil, r, false
	}
	account, ok := s.sponsorsAccountFromPath(w, r)
	if !ok {
		return nil, nil, r, false
	}
	if !s.ViewerCanAdminSponsorable(r, user, account) {
		writeGHError(w, http.StatusForbidden, "Must be able to manage this account's GitHub Sponsors profile.")
		return nil, nil, r, false
	}
	listing := s.store.Sponsors.GetSponsorsListingForAccount(account.Login)
	if listing == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, r, false
	}
	return listing, user, r, true
}

func (s *Server) handleCreateSponsorsTier(w http.ResponseWriter, r *http.Request) {
	listing, _, r, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	var req struct {
		Name           string `json:"name"`
		Description    string `json:"description"`
		AmountInCents  int    `json:"amount_in_cents"`
		IsOneTime      bool   `json:"is_one_time"`
		IsCustomAmount bool   `json:"is_custom_amount"`
		Publish        bool   `json:"publish"`
		WelcomeMessage string `json:"welcome_message"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	tier, err := s.store.Sponsors.CreateSponsorsTier(store.SponsorsTierInput{
		ListingID: listing.ID, Name: req.Name, Description: req.Description,
		AmountInCents: req.AmountInCents, IsOneTime: req.IsOneTime, IsCustomAmount: req.IsCustomAmount,
		Publish: req.Publish, WelcomeMessage: req.WelcomeMessage,
	})
	if err != nil {
		store.WriteGHValidationError(w, "SponsorsTier", "amount", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sponsorsTierJSON(tier))
}

func (s *Server) handleUpdateSponsorsTierState(w http.ResponseWriter, r *http.Request) {
	listing, _, r, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	tierID, err := strconv.Atoi(r.PathValue("tier_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	tier := s.store.Sponsors.GetSponsorsTier(tierID)
	if tier == nil || tier.ListingID != listing.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		State string `json:"state"` // published | retired
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	var updated *store.SponsorsTier
	switch req.State {
	case "published":
		updated, err = s.store.Sponsors.PublishSponsorsTier(tierID)
	case "retired":
		updated, err = s.store.Sponsors.RetireSponsorsTier(tierID)
	default:
		store.WriteGHValidationError(w, "SponsorsTier", "state", "must be published or retired")
		return
	}
	if err != nil {
		store.WriteGHValidationError(w, "SponsorsTier", "state", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sponsorsTierJSON(updated))
}

func (s *Server) handleSetSponsorsGoal(w http.ResponseWriter, r *http.Request) {
	listing, _, r, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind        string `json:"kind"`
		TargetValue int    `json:"target_value"`
		Description string `json:"description"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Kind != store.SponsorsGoalMonthlyAmount && req.Kind != store.SponsorsGoalTotalSponsors {
		store.WriteGHValidationError(w, "SponsorsGoal", "kind", "must be MONTHLY_SPONSORSHIP_AMOUNT or TOTAL_SPONSORS_COUNT")
		return
	}
	if req.TargetValue <= 0 {
		store.WriteGHValidationError(w, "SponsorsGoal", "target_value", "must be greater than zero")
		return
	}
	updated := s.store.Sponsors.UpdateSponsorsListing(listing.ID, store.SponsorsListingUpdate{
		Goal: &store.SponsorsGoal{Kind: req.Kind, TargetValue: req.TargetValue, Description: req.Description},
	})
	writeJSON(w, http.StatusOK, s.sponsorsListingJSON(updated, true))
}

func (s *Server) handleClearSponsorsGoal(w http.ResponseWriter, r *http.Request) {
	listing, _, _, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	updated := s.store.Sponsors.UpdateSponsorsListing(listing.ID, store.SponsorsListingUpdate{ClearGoal: true})
	writeJSON(w, http.StatusOK, s.sponsorsListingJSON(updated, true))
}

func (s *Server) handleFeatureSponsorsItem(w http.ResponseWriter, r *http.Request) {
	listing, _, r, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	var req struct {
		FeatureableType string `json:"featureable_type"`
		Login           string `json:"login"`
		RepositoryName  string `json:"repository_name"`
		Description     string `json:"description"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	featureableID := 0
	switch req.FeatureableType {
	case store.SponsorsFeatureableRepository:
		repo := s.store.GetRepoByFullName(listing.SponsorableLogin + "/" + req.RepositoryName)
		if repo == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		featureableID = repo.ID
	case store.SponsorsFeatureableUser:
		user := s.store.LookupUserByLogin(req.Login)
		if user == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		featureableID = user.ID
	default:
		store.WriteGHValidationError(w, "SponsorsListingFeaturedItem", "featureable_type", "must be REPOSITORY or USER")
		return
	}
	item, err := s.store.Sponsors.FeatureSponsorsListingItem(listing.ID, req.FeatureableType, featureableID, req.Description)
	if err != nil {
		store.WriteGHValidationError(w, "SponsorsListingFeaturedItem", "featureable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.sponsorsFeaturedItemJSON(item))
}

func (s *Server) handleUnfeatureSponsorsItem(w http.ResponseWriter, r *http.Request) {
	listing, _, _, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	itemID, err := strconv.Atoi(r.PathValue("item_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	item := s.store.Sponsors.FindSponsorsFeaturedItemByNodeID("")
	_ = item
	found := false
	for _, candidate := range s.store.Sponsors.ListSponsorsListingFeaturedItems(listing.ID, nil) {
		if candidate.ID == itemID {
			found = true
			break
		}
	}
	if !found || !s.store.Sponsors.UnfeatureSponsorsListingItem(itemID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// handlers — newsletters

func (s *Server) handleListSponsorshipNewsletters(w http.ResponseWriter, r *http.Request) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return
	}
	account, ok := s.sponsorsAccountFromPath(w, r)
	if !ok {
		return
	}
	listing := s.store.Sponsors.GetSponsorsListingForAccount(account.Login)
	if listing == nil {
		writeJSON(w, http.StatusOK, []map[string]interface{}{})
		return
	}
	maintainer := s.ViewerCanAdminSponsorable(r, user, account)
	// A published newsletter is for sponsors; a draft is for the
	// maintainer alone.
	if !maintainer && s.store.Sponsors.GetSponsorshipBetween(user.Login, account.Login, true) == nil {
		writeGHError(w, http.StatusForbidden, "Must be sponsoring this account to read its updates.")
		return
	}
	rows := []map[string]interface{}{}
	for _, n := range s.store.Sponsors.ListSponsorshipNewsletters(listing.ID, maintainer) {
		row := map[string]interface{}{
			"id": n.ID, "node_id": n.NodeID, "subject": n.Subject, "body": n.Body,
			"is_published": n.IsPublished, "created_at": n.CreatedAt.UTC().Format(time.RFC3339),
		}
		if author := s.store.GetUserByID(n.AuthorID); author != nil {
			row["author"] = author.Login
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleCreateSponsorshipNewsletter(w http.ResponseWriter, r *http.Request) {
	listing, user, r, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	var req struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Publish bool   `json:"publish"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	n, err := s.store.Sponsors.CreateSponsorshipNewsletter(listing.ID, user.ID, req.Subject, req.Body, req.Publish)
	if err != nil {
		store.WriteGHValidationError(w, "SponsorshipNewsletter", "subject", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id": n.ID, "node_id": n.NodeID, "subject": n.Subject, "body": n.Body,
		"is_published": n.IsPublished, "created_at": n.CreatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handlePublishSponsorshipNewsletter(w http.ResponseWriter, r *http.Request) {
	listing, _, _, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("newsletter_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	owned := false
	for _, n := range s.store.Sponsors.ListSponsorshipNewsletters(listing.ID, true) {
		if n.ID == id {
			owned = true
			break
		}
	}
	if !owned {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	n, err := s.store.Sponsors.PublishSponsorshipNewsletter(id)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": n.ID, "node_id": n.NodeID, "subject": n.Subject, "body": n.Body,
		"is_published": n.IsPublished, "created_at": n.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// handlers — the maintainer dashboard and payouts

func (s *Server) handleSponsorsDashboard(w http.ResponseWriter, r *http.Request) {
	listing, _, _, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	s.ReconcileSponsorships()
	listing = s.store.Sponsors.GetSponsorsListing(listing.ID)

	sponsorships := []map[string]interface{}{}
	for _, sponsorship := range s.store.Sponsors.ListSponsorshipsAsMaintainer(listing.SponsorableLogin, false) {
		sponsorships = append(sponsorships, s.sponsorshipJSON(sponsorship))
	}
	activities := []map[string]interface{}{}
	for _, a := range s.store.Sponsors.ListSponsorsActivities(listing.SponsorableLogin, false) {
		activities = append(activities, map[string]interface{}{
			"id": a.ID, "node_id": a.NodeID, "action": a.Action, "sponsor_login": a.SponsorLogin,
			"timestamp": a.Timestamp.UTC().Format(time.RFC3339), "tier_id": a.SponsorsTierID,
			"previous_tier_id": a.PreviousSponsorsTierID,
		})
	}
	invoices := []map[string]interface{}{}
	for _, i := range s.store.Sponsors.ListSponsorsInvoices(listing.ID) {
		invoices = append(invoices, map[string]interface{}{
			"id": i.ID, "sponsor_login": i.SponsorLogin, "amount_in_cents": i.AmountInCents,
			"period_start": i.PeriodStart.UTC().Format(time.RFC3339), "period_end": i.PeriodEnd.UTC().Format(time.RFC3339),
			"one_time": i.OneTime, "prorated": i.Prorated, "status": i.Status, "payout_id": i.PayoutID,
		})
	}
	payouts := []map[string]interface{}{}
	for _, p := range s.store.Sponsors.ListSponsorsPayouts(listing.ID) {
		payouts = append(payouts, map[string]interface{}{
			"id": p.ID, "amount_in_cents": p.AmountInCents, "status": p.Status,
			"scheduled_date": p.ScheduledDate, "created_at": p.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	lifetime := []map[string]interface{}{}
	for _, v := range s.store.Sponsors.LifetimeReceivedSponsorshipValues(listing.SponsorableLogin) {
		lifetime = append(lifetime, map[string]interface{}{
			"sponsor_login": v.SponsorLogin, "amount_in_cents": v.AmountInCents,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"listing":                           s.sponsorsListingJSON(listing, true),
		"sponsorships":                      sponsorships,
		"activities":                        activities,
		"invoices":                          invoices,
		"payouts":                           payouts,
		"lifetime_values":                   lifetime,
		"monthly_estimated_income_in_cents": s.store.Sponsors.MonthlyEstimatedSponsorsIncomeInCents(listing.SponsorableLogin),
		"estimated_next_payout_in_cents":    s.store.Sponsors.EstimatedNextSponsorsPayoutInCents(listing.SponsorableLogin),
	})
}

func (s *Server) handleRunSponsorsPayout(w http.ResponseWriter, r *http.Request) {
	listing, _, _, ok := s.sponsorsMaintainerListing(w, r)
	if !ok {
		return
	}
	s.ReconcileSponsorships()
	payout := s.store.Sponsors.RunSponsorsPayout(listing.ID, s.currentTime())
	if payout == nil {
		store.WriteGHValidationError(w, "SponsorsPayout", "amount", "no sponsorship income is awaiting payout")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id": payout.ID, "amount_in_cents": payout.AmountInCents, "status": payout.Status,
		"scheduled_date": payout.ScheduledDate, "created_at": payout.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// handlers — sponsoring

func (s *Server) handleListViewerSponsoring(w http.ResponseWriter, r *http.Request) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return
	}
	s.ReconcileSponsorships()
	logins := []string{user.Login}
	for _, org := range s.store.ListOrgsByUser(user.ID) {
		if s.viewerCanAdminOrg(r.Context(), org.Login) {
			logins = append(logins, org.Login)
		}
	}
	rows := []map[string]interface{}{}
	for _, login := range logins {
		for _, sponsorship := range s.store.Sponsors.ListSponsorshipsAsSponsor(login, false) {
			rows = append(rows, s.sponsorshipJSON(sponsorship))
		}
	}
	writeJSON(w, http.StatusOK, rows)
}

// sponsorshipRequest is the body every sponsorship write shares.
type sponsorshipRequest struct {
	SponsorLogin  string `json:"sponsor_login"`
	TierID        int    `json:"tier_id"`
	AmountInCents int    `json:"amount_in_cents"`
	PrivacyLevel  string `json:"privacy_level"`
	ReceiveEmails bool   `json:"receive_emails"`
	IsRecurring   bool   `json:"is_recurring"`
}

// sponsorAccountFor resolves the account funding a sponsorship and refuses
// a viewer who may not spend from it.
func (s *Server) sponsorAccountFor(w http.ResponseWriter, r *http.Request, user *store.User, login string) (SponsorableAccount, bool) {
	if login == "" || strings.EqualFold(login, user.Login) {
		return SponsorableAccount{ID: user.ID, Type: "User", Login: user.Login}, true
	}
	account, ok := s.SponsorableAccountByLogin(login)
	if !ok || !s.ViewerCanSpendAsSponsor(r, user, account) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return SponsorableAccount{}, false
	}
	return account, true
}

func (s *Server) handleCreateSponsorshipBrowser(w http.ResponseWriter, r *http.Request) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return
	}
	account, ok := s.sponsorsAccountFromPath(w, r)
	if !ok {
		return
	}
	var req sponsorshipRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	sponsor, ok := s.sponsorAccountFor(w, r, user, req.SponsorLogin)
	if !ok {
		return
	}
	s.ReconcileSponsorships()
	transition, err := s.store.Sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: sponsor.ID, SponsorType: sponsor.Type, SponsorLogin: sponsor.Login,
		SponsorableID: account.ID, SponsorableType: account.Type, SponsorableLogin: account.Login,
		TierID: req.TierID, AmountInCents: req.AmountInCents, PrivacyLevel: req.PrivacyLevel,
		ReceiveEmails: req.ReceiveEmails, IsRecurring: req.IsRecurring,
	})
	if err != nil {
		store.WriteGHValidationError(w, "Sponsorship", "tier", err.Error())
		return
	}
	s.emitSponsorshipEvent("created", transition, user)
	writeJSON(w, http.StatusCreated, s.sponsorshipJSON(transition.Sponsorship))
}

func (s *Server) handleUpdateSponsorshipBrowser(w http.ResponseWriter, r *http.Request) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return
	}
	account, ok := s.sponsorsAccountFromPath(w, r)
	if !ok {
		return
	}
	var req sponsorshipRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	sponsor, ok := s.sponsorAccountFor(w, r, user, req.SponsorLogin)
	if !ok {
		return
	}
	s.ReconcileSponsorships()
	sponsorship := s.store.Sponsors.GetSponsorshipBetween(sponsor.Login, account.Login, true)
	if sponsorship == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.TierID != 0 && req.TierID != sponsorship.TierID {
		transition, err := s.store.Sponsors.ChangeSponsorshipTier(sponsorship.ID, req.TierID)
		if err != nil {
			store.WriteGHValidationError(w, "Sponsorship", "tier", err.Error())
			return
		}
		action := "tier_changed"
		if transition.Sponsorship.PendingTierID != 0 {
			action = "pending_tier_change"
		}
		s.emitSponsorshipEvent(action, transition, user)
		writeJSON(w, http.StatusOK, s.sponsorshipJSON(transition.Sponsorship))
		return
	}
	transition, err := s.store.Sponsors.UpdateSponsorshipPreferences(sponsorship.ID, req.PrivacyLevel, req.ReceiveEmails)
	if err != nil {
		store.WriteGHValidationError(w, "Sponsorship", "privacy_level", err.Error())
		return
	}
	s.emitSponsorshipEvent("edited", transition, user)
	writeJSON(w, http.StatusOK, s.sponsorshipJSON(transition.Sponsorship))
}

func (s *Server) handleCancelSponsorshipBrowser(w http.ResponseWriter, r *http.Request) {
	user, r := s.sponsorsBrowserUser(w, r)
	if user == nil {
		return
	}
	account, ok := s.sponsorsAccountFromPath(w, r)
	if !ok {
		return
	}
	sponsor, ok := s.sponsorAccountFor(w, r, user, r.URL.Query().Get("sponsor"))
	if !ok {
		return
	}
	s.ReconcileSponsorships()
	sponsorship := s.store.Sponsors.GetSponsorshipBetween(sponsor.Login, account.Login, true)
	if sponsorship == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	transition, err := s.store.Sponsors.CancelSponsorship(sponsorship.ID)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	action := "cancelled"
	if transition.Sponsorship.PendingCancellation {
		action = "pending_cancellation"
	}
	s.emitSponsorshipEvent(action, transition, user)
	writeJSON(w, http.StatusOK, s.sponsorshipJSON(transition.Sponsorship))
}

// viewerCanAdminAccount is the Sponsors authorization predicate the
// GraphQL resolver layer consumes through the Authz seam: the account's
// own user, an owner of the organization, or a site administrator.
func (s *Server) viewerCanAdminAccount(ctx context.Context, login string) bool {
	viewer := ghUserFromContext(ctx)
	if viewer == nil {
		return false
	}
	if viewer.SiteAdmin {
		return true
	}
	if strings.EqualFold(viewer.Login, login) {
		return true
	}
	if s.store.GetOrg(login) == nil {
		return false
	}
	return s.viewerCanAdminOrg(ctx, login)
}
