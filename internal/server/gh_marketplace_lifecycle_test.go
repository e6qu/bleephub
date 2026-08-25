package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// marketplaceLifecycleSink collects the marketplace_purchase deliveries a
// publisher's webhook receives.
type marketplaceLifecycleSink struct {
	deliveries chan map[string]interface{}
}

func newMarketplaceLifecycleSink(t *testing.T) (*marketplaceLifecycleSink, string) {
	t.Helper()
	sink := &marketplaceLifecycleSink{deliveries: make(chan map[string]interface{}, 16)}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("X-GitHub-Event") == "marketplace_purchase" {
			sink.deliveries <- body
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return sink, server.URL
}

func (sink *marketplaceLifecycleSink) next(t *testing.T, wantAction string) map[string]interface{} {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case body := <-sink.deliveries:
			if body["action"] == wantAction {
				return body
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the marketplace_purchase %q delivery", wantAction)
			return nil
		}
	}
}

func TestMarketplacePendingChangeAndRevocationWebhooks(t *testing.T) {
	s := newIsolatedServer(t)
	sink, receiver := newMarketplaceLifecycleSink(t)
	listing := s.publishMarketplaceGitHubApp(t, "Marketplace Pending Change App", receiver)
	buyer, buyerToken := s.userSurfaceUser(t, "marketplace-pending-buyer")
	_ = buyer

	// Start on the paid plan so a downgrade is a real, deferred change.
	requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodPost,
		"/ui-data/marketplace/listings/"+listing.slug+"/purchase", "token "+buyerToken,
		map[string]interface{}{"plan_id": listing.paidPlanID, "billing_cycle": "monthly"}), http.StatusCreated)
	sink.next(t, "purchased")

	// Downgrading to the free plan is scheduled, not applied.
	requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodPatch,
		"/ui-data/marketplace/listings/"+listing.slug+"/subscription", "token "+buyerToken,
		map[string]interface{}{"plan_id": listing.freePlanID, "billing_cycle": "monthly"}), http.StatusOK)
	pending := sink.next(t, "pending_change")
	if pending["effective_date"] == nil {
		t.Fatalf("pending_change must carry an effective_date: %v", pending)
	}
	upcoming := pending["marketplace_purchase"].(map[string]interface{})["plan"].(map[string]interface{})
	previous := pending["previous_marketplace_purchase"].(map[string]interface{})["plan"].(map[string]interface{})
	if int(upcoming["id"].(float64)) != listing.freePlanID {
		t.Fatalf("pending_change marketplace_purchase should describe the upcoming plan: %v", upcoming)
	}
	if int(previous["id"].(float64)) != listing.paidPlanID {
		t.Fatalf("pending_change previous_marketplace_purchase should describe the current plan: %v", previous)
	}

	// Revoking the scheduled change announces pending_change_cancelled and
	// leaves the subscription on the plan it was already paying for.
	revoked := requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodDelete,
		"/ui-data/marketplace/listings/"+listing.slug+"/subscription/pending", "token "+buyerToken, nil), http.StatusOK)
	if revoked["pending_change"] != nil {
		t.Fatalf("revoking must clear the pending change: %v", revoked)
	}
	cancelled := sink.next(t, "pending_change_cancelled")
	stillOn := cancelled["marketplace_purchase"].(map[string]interface{})["plan"].(map[string]interface{})
	calledOff := cancelled["previous_marketplace_purchase"].(map[string]interface{})["plan"].(map[string]interface{})
	if int(stillOn["id"].(float64)) != listing.paidPlanID || int(calledOff["id"].(float64)) != listing.freePlanID {
		t.Fatalf("pending_change_cancelled payload = %v", cancelled)
	}

	// With nothing scheduled there is nothing to revoke.
	requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodDelete,
		"/ui-data/marketplace/listings/"+listing.slug+"/subscription/pending", "token "+buyerToken, nil), http.StatusNotFound)

	// A paid cancellation is likewise scheduled and announced as pending.
	requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodDelete,
		"/ui-data/marketplace/listings/"+listing.slug+"/subscription", "token "+buyerToken, nil), http.StatusAccepted)
	sink.next(t, "pending_change")
}

func TestMarketplaceCategoryTaxonomyAndProfileAuthorization(t *testing.T) {
	s := newIsolatedServer(t)
	_, receiver := newMarketplaceLifecycleSink(t)
	listing := s.publishMarketplaceGitHubApp(t, "Marketplace Category App", receiver)
	_, outsiderToken := s.userSurfaceUser(t, "marketplace-category-outsider")

	rows := decodeJSONArray(t, s.marketplaceRequest(t, http.MethodGet, "/ui-data/marketplace/categories", "token "+outsiderToken, nil))
	if len(rows) < 10 {
		t.Fatalf("the Marketplace taxonomy should be seeded, got %d categories", len(rows))
	}
	bySlug := map[string]map[string]interface{}{}
	for _, row := range rows {
		bySlug[row["slug"].(string)] = row
	}
	if bySlug["security"] == nil || bySlug["continuous-integration"] == nil {
		t.Fatalf("expected GitHub's own category slugs, got %v", rows)
	}

	// Only the publisher may edit the listing's profile.
	requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodPut,
		"/ui-data/marketplace/listings/"+listing.slug+"/profile", "token "+outsiderToken,
		map[string]interface{}{"primary_category_slug": "security"}), http.StatusForbidden)

	saved := requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodPut,
		"/ui-data/marketplace/listings/"+listing.slug+"/profile", "token "+defaultToken,
		map[string]interface{}{
			"primary_category_slug":   "continuous-integration",
			"secondary_category_slug": "security",
			"support_url":             "https://example.test/support",
			"privacy_policy_url":      "https://example.test/privacy",
			"terms_of_service_url":    "https://example.test/terms",
			"screenshot_urls":         []string{"https://example.test/one.png"},
			"state":                   store.MarketplaceListingVerified,
		}), http.StatusOK)
	if saved["primary_category_slug"] != "continuous-integration" || saved["state"] != store.MarketplaceListingVerified {
		t.Fatalf("saved profile = %v", saved)
	}

	// An unknown category is refused rather than silently stored.
	requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodPut,
		"/ui-data/marketplace/listings/"+listing.slug+"/profile", "token "+defaultToken,
		map[string]interface{}{"primary_category_slug": "not-a-category"}), http.StatusUnprocessableEntity)

	// The category now counts the listing that points at it.
	rows = decodeJSONArray(t, s.marketplaceRequest(t, http.MethodGet, "/ui-data/marketplace/categories", "token "+outsiderToken, nil))
	for _, row := range rows {
		if row["slug"] == "continuous-integration" && row["primary_listing_count"].(float64) != 1 {
			t.Fatalf("continuous-integration primary_listing_count = %v, want 1", row["primary_listing_count"])
		}
		if row["slug"] == "security" && row["secondary_listing_count"].(float64) != 1 {
			t.Fatalf("security secondary_listing_count = %v, want 1", row["secondary_listing_count"])
		}
	}
}

func TestMarketplaceGraphQLSurface(t *testing.T) {
	s := newIsolatedServer(t)
	_, receiver := newMarketplaceLifecycleSink(t)
	listing := s.publishMarketplaceGitHubApp(t, "Marketplace GraphQL App", receiver)
	requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodPut,
		"/ui-data/marketplace/listings/"+listing.slug+"/profile", "token "+defaultToken,
		map[string]interface{}{
			"primary_category_slug": "continuous-integration",
			"logo_url":              "https://example.test/logo.png",
			"support_url":           "https://example.test/support",
			"terms_of_service_url":  "https://example.test/terms",
			"state":                 store.MarketplaceListingVerified,
			"has_verified_owner":    true,
		}), http.StatusOK)

	data := s.gqlData(t, `query($slug: String!) {
	  marketplaceListing(slug: $slug) {
	    id slug name shortDescription fullDescription isPublic isPaid isVerified isDraft
	    hasPublishedFreeTrialPlans hasTermsOfService hasVerifiedOwner
	    logoUrl(size: 200) logoBackgroundColor privacyPolicyUrl supportUrl screenshotUrls
	    primaryCategory { id name slug primaryListingCount }
	    secondaryCategory { slug }
	    viewerIsListingAdmin viewerCanEditPlans viewerHasPurchased installedForViewer
	  }
	  marketplaceListings(first: 10) { totalCount nodes { slug } }
	  marketplaceCategories(excludeEmpty: true) { slug primaryListingCount }
	  marketplaceCategory(slug: "ci", useTopicAliases: true) { slug name }
	}`, map[string]interface{}{"slug": listing.slug})

	node := data["marketplaceListing"].(map[string]interface{})
	if node["slug"] != listing.slug || node["isPublic"] != true || node["isVerified"] != true || node["isDraft"] != false {
		t.Fatalf("marketplaceListing = %v", node)
	}
	if node["isPaid"] != true || node["hasPublishedFreeTrialPlans"] != true || node["hasTermsOfService"] != true {
		t.Fatalf("plan-derived flags = %v", node)
	}
	if node["logoUrl"] != "https://example.test/logo.png?size=200" {
		t.Fatalf("logoUrl = %v", node["logoUrl"])
	}
	if node["secondaryCategory"] != nil {
		t.Fatalf("secondaryCategory should be null when unset: %v", node["secondaryCategory"])
	}
	category := node["primaryCategory"].(map[string]interface{})
	if category["slug"] != "continuous-integration" || category["primaryListingCount"].(float64) != 1 {
		t.Fatalf("primaryCategory = %v", category)
	}
	// The admin token publishes the app, so it administers the listing and
	// has bought nothing.
	if node["viewerIsListingAdmin"] != true || node["viewerCanEditPlans"] != true || node["viewerHasPurchased"] != false {
		t.Fatalf("viewer fields for the publisher = %v", node)
	}
	listings := data["marketplaceListings"].(map[string]interface{})
	if listings["totalCount"].(float64) < 1 {
		t.Fatalf("marketplaceListings = %v", listings)
	}
	if !strings.Contains(mustJSONString(t, data["marketplaceCategories"]), "continuous-integration") {
		t.Fatalf("marketplaceCategories(excludeEmpty:) dropped the only populated category: %v", data["marketplaceCategories"])
	}
	alias := data["marketplaceCategory"].(map[string]interface{})
	if alias["slug"] != "continuous-integration" {
		t.Fatalf("marketplaceCategory by topic alias = %v", alias)
	}

	// The node id round-trips through Query.node.
	nodeData := s.gqlData(t, `query($id: ID!) { node(id: $id) { __typename ... on MarketplaceListing { slug } } }`,
		map[string]interface{}{"id": node["id"]})
	resolved := nodeData["node"].(map[string]interface{})
	if resolved["__typename"] != "MarketplaceListing" || resolved["slug"] != listing.slug {
		t.Fatalf("node(marketplace listing id) = %v", resolved)
	}
}

func TestUnpublishedMarketplaceListingIsHiddenFromNonPublishers(t *testing.T) {
	s := newIsolatedServer(t)
	_, receiver := newMarketplaceLifecycleSink(t)
	listing := s.publishMarketplaceGitHubApp(t, "Marketplace Hidden App", receiver)
	// Delist it: only its publisher may still see it.
	settingsPath := "/settings/apps/" + listing.slug + "/marketplace"
	current := requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodGet, settingsPath, "token "+defaultToken, nil), http.StatusOK)
	body := map[string]interface{}{
		"name": current["name"], "description": current["description"], "full_description": current["full_description"],
		"webhook_url": receiver, "webhook_content_type": "json", "webhook_active": true, "published": false,
	}
	requireMarketplaceStatus(t, s.marketplaceRequest(t, http.MethodPut, settingsPath, "token "+defaultToken, body), http.StatusOK)

	outsider, outsiderToken := s.userSurfaceUser(t, "marketplace-hidden-outsider")
	_ = outsider
	envelope := marketplaceGraphQLAs(t, s, outsiderToken, `query($slug: String!) {
	  marketplaceListing(slug: $slug) { slug }
	  marketplaceListings(first: 10, allStates: true) { totalCount nodes { slug } }
	}`, map[string]interface{}{"slug": listing.slug})
	if envelope["marketplaceListing"] != nil {
		t.Fatalf("a delisted listing must read as absent to a non-publisher: %v", envelope["marketplaceListing"])
	}
	if strings.Contains(mustJSONString(t, envelope["marketplaceListings"]), listing.slug) {
		t.Fatalf("allStates must not expose another publisher's delisted listing: %v", envelope["marketplaceListings"])
	}

	// Its publisher still sees it, and allStates includes it.
	mine := s.gqlData(t, `query($slug: String!) {
	  marketplaceListing(slug: $slug) { slug isPublic }
	  marketplaceListings(first: 10, allStates: true) { nodes { slug } }
	}`, map[string]interface{}{"slug": listing.slug})
	own := mine["marketplaceListing"].(map[string]interface{})
	if own["slug"] != listing.slug || own["isPublic"] != false {
		t.Fatalf("publisher view of the delisted listing = %v", own)
	}
	if !strings.Contains(mustJSONString(t, mine["marketplaceListings"]), listing.slug) {
		t.Fatalf("allStates should include the publisher's own delisted listing: %v", mine["marketplaceListings"])
	}
}

func marketplaceGraphQLAs(t *testing.T, s *isolatedServer, token, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	return sponsorsGraphQLAs(t, s, token, query, variables)
}

func mustJSONString(t *testing.T, value interface{}) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
