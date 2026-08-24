package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// sponsorsFixture is a maintainer with a published Sponsors profile and two
// recurring tiers, plus a sponsor account with a token.
type sponsorsFixture struct {
	*isolatedServer
	maintainer      *store.User
	maintainerToken string
	sponsor         *store.User
	sponsorToken    string
	listing         *store.SponsorsListing
	fiveDollarTier  *store.SponsorsTier
	tenDollarTier   *store.SponsorsTier
	oneTimeTier     *store.SponsorsTier
	clock           *sponsorsTestClock
}

// sponsorsTestClock is a settable clock: billing is a function of time, so a
// test has to be able to move time without sleeping. TEST rules forbid
// time.Now() in a test, so every instant is derived from fixedTestTime.
type sponsorsTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *sponsorsTestClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *sponsorsTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newSponsorsFixture(t *testing.T) *sponsorsFixture {
	t.Helper()
	s := newIsolatedServer(t)
	clock := &sponsorsTestClock{now: fixedTestTime}
	s.replaceClockNow(clock.read)

	maintainer := s.createTestUser(t, "sponsors-maintainer")
	sponsor := s.createTestUser(t, "sponsors-sponsor")
	fixture := &sponsorsFixture{
		isolatedServer:  s,
		maintainer:      maintainer,
		maintainerToken: s.store.CreateToken(maintainer.ID, "repo,user").Value,
		sponsor:         sponsor,
		sponsorToken:    s.store.CreateToken(sponsor.ID, "repo,user").Value,
		clock:           clock,
	}

	listing, err := s.store.Sponsors.CreateSponsorsListing(store.SponsorsListingInput{
		SponsorableID: maintainer.ID, SponsorableType: "User", SponsorableLogin: maintainer.Login,
		Name: "Sponsors Maintainer", ShortDescription: "Keeps the lights on",
		FullDescription: "Full profile text", ContactEmail: "maintainer@example.test",
		BillingCountryOrRegion: "Netherlands",
	})
	if err != nil {
		t.Fatalf("create sponsors listing: %v", err)
	}
	fixture.listing = listing
	fixture.fiveDollarTier = mustCreateSponsorsTier(t, s, listing.ID, 500, false)
	fixture.tenDollarTier = mustCreateSponsorsTier(t, s, listing.ID, 1000, false)
	fixture.oneTimeTier = mustCreateSponsorsTier(t, s, listing.ID, 2500, true)
	return fixture
}

func mustCreateSponsorsTier(t *testing.T, s *isolatedServer, listingID, cents int, oneTime bool) *store.SponsorsTier {
	t.Helper()
	tier, err := s.store.Sponsors.CreateSponsorsTier(store.SponsorsTierInput{
		ListingID: listingID, Description: "tier", AmountInCents: cents, IsOneTime: oneTime, Publish: true,
	})
	if err != nil {
		t.Fatalf("create sponsors tier at %d cents: %v", cents, err)
	}
	return tier
}

// ---------------------------------------------------------------------------
// store: the billing state machine

func TestSponsorshipBillingLifecycle(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	sponsors := f.store.Sponsors

	transition, err := sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: f.sponsor.ID, SponsorType: "User", SponsorLogin: f.sponsor.Login,
		SponsorableID: f.maintainer.ID, SponsorableType: "User", SponsorableLogin: f.maintainer.Login,
		TierID: f.fiveDollarTier.ID, IsRecurring: true, PrivacyLevel: store.SponsorshipPrivacyPublic,
	})
	if err != nil {
		t.Fatalf("create sponsorship: %v", err)
	}
	sponsorship := transition.Sponsorship
	if !sponsorship.IsActive || sponsorship.AmountInCents != 500 || sponsorship.NextBillingDate == nil {
		t.Fatalf("new recurring sponsorship = %+v", sponsorship)
	}
	if transition.Invoice == nil || transition.Invoice.AmountInCents != 500 {
		t.Fatalf("first period invoice = %+v", transition.Invoice)
	}
	if transition.Activity.Action != store.SponsorsActivityNewSponsorship {
		t.Fatalf("activity action = %q", transition.Activity.Action)
	}
	if got := sponsors.MonthlyEstimatedSponsorsIncomeInCents(f.maintainer.Login); got != 500 {
		t.Fatalf("monthly estimated income = %d cents, want 500", got)
	}

	// Upgrade: effective immediately, and the difference is billed prorated
	// over the days left in the period. The sponsorship has just started, so
	// essentially the whole difference is due.
	upgrade, err := sponsors.ChangeSponsorshipTier(sponsorship.ID, f.tenDollarTier.ID)
	if err != nil {
		t.Fatalf("upgrade tier: %v", err)
	}
	if upgrade.Sponsorship.TierID != f.tenDollarTier.ID || upgrade.Sponsorship.PendingTierID != 0 {
		t.Fatalf("upgrade should take effect immediately: %+v", upgrade.Sponsorship)
	}
	if upgrade.Invoice == nil || upgrade.Invoice.AmountInCents != 500 || !upgrade.Invoice.Prorated {
		t.Fatalf("upgrade should bill the prorated difference, got %+v", upgrade.Invoice)
	}
	if upgrade.Activity.Action != store.SponsorsActivityTierChange {
		t.Fatalf("upgrade activity = %q", upgrade.Activity.Action)
	}

	// Downgrade: deferred to the next billing date.
	downgrade, err := sponsors.ChangeSponsorshipTier(sponsorship.ID, f.fiveDollarTier.ID)
	if err != nil {
		t.Fatalf("downgrade tier: %v", err)
	}
	if downgrade.Sponsorship.TierID != f.tenDollarTier.ID || downgrade.Sponsorship.PendingTierID != f.fiveDollarTier.ID {
		t.Fatalf("downgrade should be scheduled, not immediate: %+v", downgrade.Sponsorship)
	}
	if downgrade.Invoice != nil {
		t.Fatalf("a scheduled downgrade must not bill anything: %+v", downgrade.Invoice)
	}
	if downgrade.Activity.Action != store.SponsorsActivityPendingChange {
		t.Fatalf("downgrade activity = %q", downgrade.Activity.Action)
	}
	// The estimate already reflects the pending downgrade: it is what the
	// maintainer will actually receive next month.
	if got := sponsors.MonthlyEstimatedSponsorsIncomeInCents(f.maintainer.Login); got != 500 {
		t.Fatalf("monthly estimate with a pending downgrade = %d cents, want 500", got)
	}

	// Roll the billing cycle: the pending tier applies and a new period is
	// billed at the new amount.
	f.clock.advance(32 * 24 * time.Hour)
	rolled := sponsors.AdvanceSponsorshipBillingCycles(f.clock.read())
	if len(rolled) != 1 {
		t.Fatalf("advancing the cycle produced %d transitions, want 1", len(rolled))
	}
	after := rolled[0].Sponsorship
	if after.TierID != f.fiveDollarTier.ID || after.PendingTierID != 0 || after.AmountInCents != 500 {
		t.Fatalf("pending downgrade did not apply: %+v", after)
	}
	if rolled[0].Invoice == nil || rolled[0].Invoice.AmountInCents != 500 {
		t.Fatalf("renewal invoice = %+v", rolled[0].Invoice)
	}
	if after.NextBillingDate == nil || !after.NextBillingDate.After(f.clock.read()) {
		t.Fatalf("next billing date should be in the future: %+v", after.NextBillingDate)
	}

	// Cancel: deferred to the end of the paid-for period, then applied.
	cancelled, err := sponsors.CancelSponsorship(sponsorship.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !cancelled.Sponsorship.PendingCancellation || !cancelled.Sponsorship.IsActive {
		t.Fatalf("cancellation should be pending, not immediate: %+v", cancelled.Sponsorship)
	}
	f.clock.advance(32 * 24 * time.Hour)
	finished := sponsors.AdvanceSponsorshipBillingCycles(f.clock.read())
	if len(finished) != 1 || finished[0].Sponsorship.IsActive {
		t.Fatalf("pending cancellation did not end the sponsorship: %+v", finished)
	}
	if finished[0].Invoice != nil {
		t.Fatalf("a cancelled sponsorship must not be billed again: %+v", finished[0].Invoice)
	}
	if got := sponsors.MonthlyEstimatedSponsorsIncomeInCents(f.maintainer.Login); got != 0 {
		t.Fatalf("monthly estimate after cancellation = %d cents, want 0", got)
	}

	// The ledger: 500 (first period) + 500 (prorated upgrade) + 500 (renewal).
	lifetime := sponsors.LifetimeReceivedSponsorshipValues(f.maintainer.Login)
	if len(lifetime) != 1 || lifetime[0].AmountInCents != 1500 {
		t.Fatalf("lifetime received values = %+v, want one row of 1500 cents", lifetime)
	}
	if got := sponsors.EstimatedNextSponsorsPayoutInCents(f.maintainer.Login); got != 1500 {
		t.Fatalf("estimated next payout = %d cents, want 1500", got)
	}
	payout := sponsors.RunSponsorsPayout(f.listing.ID, f.clock.read())
	if payout == nil || payout.AmountInCents != 1500 {
		t.Fatalf("payout = %+v, want 1500 cents", payout)
	}
	if got := sponsors.EstimatedNextSponsorsPayoutInCents(f.maintainer.Login); got != 0 {
		t.Fatalf("estimated next payout after a run = %d cents, want 0", got)
	}
	if sponsors.RunSponsorsPayout(f.listing.ID, f.clock.read()) != nil {
		t.Fatal("a second payout run with nothing owed must produce no payout")
	}
}

func TestOneTimeSponsorshipBillsOnceAndCancelsImmediately(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	sponsors := f.store.Sponsors

	transition, err := sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: f.sponsor.ID, SponsorType: "User", SponsorLogin: f.sponsor.Login,
		SponsorableID: f.maintainer.ID, SponsorableType: "User", SponsorableLogin: f.maintainer.Login,
		TierID: f.oneTimeTier.ID, IsRecurring: false,
	})
	if err != nil {
		t.Fatalf("create one-time sponsorship: %v", err)
	}
	if !transition.Sponsorship.IsOneTimePayment || transition.Sponsorship.NextBillingDate != nil {
		t.Fatalf("one-time sponsorship must not schedule a next billing date: %+v", transition.Sponsorship)
	}
	if transition.Invoice.AmountInCents != 2500 || !transition.Invoice.OneTime {
		t.Fatalf("one-time invoice = %+v", transition.Invoice)
	}
	// A one-time payment is never recurring income.
	if got := sponsors.MonthlyEstimatedSponsorsIncomeInCents(f.maintainer.Login); got != 0 {
		t.Fatalf("monthly estimate from a one-time payment = %d, want 0", got)
	}
	f.clock.advance(40 * 24 * time.Hour)
	if rolled := sponsors.AdvanceSponsorshipBillingCycles(f.clock.read()); len(rolled) != 0 {
		t.Fatalf("a one-time sponsorship must never bill again, got %d transitions", len(rolled))
	}
	cancelled, err := sponsors.CancelSponsorship(transition.Sponsorship.ID)
	if err != nil {
		t.Fatalf("cancel one-time: %v", err)
	}
	if cancelled.Sponsorship.IsActive || cancelled.Sponsorship.PendingCancellation {
		t.Fatalf("a one-time sponsorship cancels immediately: %+v", cancelled.Sponsorship)
	}
}

func TestSponsorsTierStateMachineRefusesUnpublishedTiers(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	sponsors := f.store.Sponsors

	draft, err := sponsors.CreateSponsorsTier(store.SponsorsTierInput{
		ListingID: f.listing.ID, Description: "draft", AmountInCents: 300,
	})
	if err != nil {
		t.Fatalf("create draft tier: %v", err)
	}
	if !draft.IsDraft || draft.IsPublished {
		t.Fatalf("a tier is created as a draft: %+v", draft)
	}
	if _, err := sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: f.sponsor.ID, SponsorType: "User", SponsorLogin: f.sponsor.Login,
		SponsorableID: f.maintainer.ID, SponsorableType: "User", SponsorableLogin: f.maintainer.Login,
		TierID: draft.ID, IsRecurring: true,
	}); err == nil {
		t.Fatal("a draft tier must not be sponsorable")
	}
	published, err := sponsors.PublishSponsorsTier(draft.ID)
	if err != nil || !published.IsPublished || published.IsDraft {
		t.Fatalf("publish tier = %+v, %v", published, err)
	}
	retired, err := sponsors.RetireSponsorsTier(draft.ID)
	if err != nil || !retired.IsRetired || retired.IsPublished {
		t.Fatalf("retire tier = %+v, %v", retired, err)
	}
	if _, err := sponsors.PublishSponsorsTier(draft.ID); err == nil {
		t.Fatal("a retired tier must not be republished")
	}
}

func TestSponsorsGoalProgressIsIntegerAndClamped(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	sponsors := f.store.Sponsors
	sponsors.UpdateSponsorsListing(f.listing.ID, store.SponsorsListingUpdate{
		Goal: &store.SponsorsGoal{Kind: store.SponsorsGoalMonthlyAmount, TargetValue: 1000},
	})
	if _, _, percent, ok := sponsors.SponsorsGoalProgress(f.listing.ID); !ok || percent != 0 {
		t.Fatalf("empty goal progress = %d%%, ok=%v", percent, ok)
	}
	if _, err := sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: f.sponsor.ID, SponsorType: "User", SponsorLogin: f.sponsor.Login,
		SponsorableID: f.maintainer.ID, SponsorableType: "User", SponsorableLogin: f.maintainer.Login,
		TierID: f.fiveDollarTier.ID, IsRecurring: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, percent, _ := sponsors.SponsorsGoalProgress(f.listing.ID); percent != 50 {
		t.Fatalf("goal progress = %d%%, want 50", percent)
	}
	sponsors.UpdateSponsorsListing(f.listing.ID, store.SponsorsListingUpdate{
		Goal: &store.SponsorsGoal{Kind: store.SponsorsGoalMonthlyAmount, TargetValue: 100},
	})
	if _, _, percent, _ := sponsors.SponsorsGoalProgress(f.listing.ID); percent != 100 {
		t.Fatalf("goal progress past target = %d%%, want it clamped to 100", percent)
	}
}

func TestSponsorsStoreGettersReturnDetachedSnapshots(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	// STORE-021: mutating a getter's result must not reach the stored row.
	listing := f.store.Sponsors.GetSponsorsListing(f.listing.ID)
	listing.Name = "mutated"
	listing.ContactEmail = "attacker@example.test"
	if again := f.store.Sponsors.GetSponsorsListing(f.listing.ID); again.Name == "mutated" || again.ContactEmail == "attacker@example.test" {
		t.Fatal("GetSponsorsListing handed back the live row")
	}
	tier := f.store.Sponsors.GetSponsorsTier(f.fiveDollarTier.ID)
	tier.MonthlyPriceInCents = 1
	if again := f.store.Sponsors.GetSponsorsTier(f.fiveDollarTier.ID); again.MonthlyPriceInCents != 500 {
		t.Fatal("GetSponsorsTier handed back the live row")
	}
	// Find*ByNodeID is the documented exception: it returns the live row.
	if live := f.store.Sponsors.FindSponsorsTierByNodeID(f.fiveDollarTier.NodeID); live == nil || live.ID != f.fiveDollarTier.ID {
		t.Fatalf("FindSponsorsTierByNodeID = %+v", live)
	}
}

// ---------------------------------------------------------------------------
// the /ui-data browser surface

func TestSponsorsBrowserSurfaceLifecycle(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)

	// The sponsor opens a recurring sponsorship.
	resp := f.post(t, "/ui-data/sponsors/"+f.maintainer.Login+"/sponsorship", f.sponsorToken, map[string]interface{}{
		"tier_id": f.fiveDollarTier.ID, "is_recurring": true, "privacy_level": "PUBLIC",
	})
	created := decodeJSONWithStatus(t, resp, http.StatusCreated)
	if created["amount_in_cents"].(float64) != 500 || created["is_active"] != true {
		t.Fatalf("created sponsorship = %v", created)
	}

	// The listing page shows the viewer's own sponsorship back to them.
	resp = f.get(t, "/ui-data/sponsors/"+f.maintainer.Login, f.sponsorToken)
	page := decodeJSONWithStatus(t, resp, http.StatusOK)
	if page["viewer_sponsorship"] == nil {
		t.Fatalf("listing page did not report the viewer's sponsorship: %v", page)
	}
	if page["viewer_can_admin"] != false {
		t.Fatal("a sponsor must not be reported as able to administer the listing")
	}
	listing := page["listing"].(map[string]interface{})
	if _, leaked := listing["contact_email"]; leaked {
		t.Fatal("the maintainer's contact email leaked to a sponsor")
	}

	// The maintainer's own view carries the payout configuration.
	resp = f.get(t, "/ui-data/sponsors/"+f.maintainer.Login, f.maintainerToken)
	page = decodeJSONWithStatus(t, resp, http.StatusOK)
	listing = page["listing"].(map[string]interface{})
	if listing["contact_email"] != "maintainer@example.test" {
		t.Fatalf("maintainer view missing contact email: %v", listing)
	}
	if listing["estimated_next_payout_in_cents"].(float64) != 500 {
		t.Fatalf("estimated next payout = %v, want 500", listing["estimated_next_payout_in_cents"])
	}

	// Upgrading the tier through the browser surface.
	resp = f.patch(t, "/ui-data/sponsors/"+f.maintainer.Login+"/sponsorship", f.sponsorToken, map[string]interface{}{
		"tier_id": f.tenDollarTier.ID,
	})
	upgraded := decodeJSONWithStatus(t, resp, http.StatusOK)
	if upgraded["amount_in_cents"].(float64) != 1000 {
		t.Fatalf("upgraded sponsorship = %v", upgraded)
	}

	// The dashboard is the maintainer's, and refuses everybody else.
	resp = f.get(t, "/ui-data/sponsors/"+f.maintainer.Login+"/dashboard", f.sponsorToken)
	assertSponsorsStatus(t, resp, http.StatusForbidden)
	resp = f.get(t, "/ui-data/sponsors/"+f.maintainer.Login+"/dashboard", f.maintainerToken)
	dashboard := decodeJSONWithStatus(t, resp, http.StatusOK)
	if len(dashboard["invoices"].([]interface{})) != 2 {
		t.Fatalf("dashboard invoices = %v, want the first period plus the prorated upgrade", dashboard["invoices"])
	}
	if dashboard["monthly_estimated_income_in_cents"].(float64) != 1000 {
		t.Fatalf("dashboard monthly estimate = %v", dashboard["monthly_estimated_income_in_cents"])
	}

	// Cancelling defers to the end of the period the sponsor paid for.
	resp = f.delete(t, "/ui-data/sponsors/"+f.maintainer.Login+"/sponsorship", f.sponsorToken)
	cancelled := decodeJSONWithStatus(t, resp, http.StatusOK)
	if cancelled["pending_cancellation"] != true || cancelled["is_active"] != true {
		t.Fatalf("cancel = %v, want a pending cancellation", cancelled)
	}
}

func TestSponsorsListingManagementIsMaintainerOnly(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	other := f.createTestUser(t, "sponsors-outsider")
	otherToken := f.store.CreateToken(other.ID, "repo,user").Value

	for _, probe := range []struct {
		method, path string
		body         interface{}
	}{
		{http.MethodPut, "/ui-data/sponsors/" + f.maintainer.Login, map[string]interface{}{"name": "hijacked"}},
		{http.MethodPost, "/ui-data/sponsors/" + f.maintainer.Login + "/tiers", map[string]interface{}{"amount_in_cents": 100}},
		{http.MethodPut, "/ui-data/sponsors/" + f.maintainer.Login + "/goal", map[string]interface{}{"kind": store.SponsorsGoalMonthlyAmount, "target_value": 100}},
		{http.MethodPost, "/ui-data/sponsors/" + f.maintainer.Login + "/newsletters", map[string]interface{}{"subject": "hi"}},
		{http.MethodPost, "/ui-data/sponsors/" + f.maintainer.Login + "/payouts", nil},
	} {
		resp := f.do(t, probe.method, probe.path, otherToken, probe.body)
		assertSponsorsStatus(t, resp, http.StatusForbidden)
	}
	if listing := f.store.Sponsors.GetSponsorsListingForAccount(f.maintainer.Login); listing.Name == "hijacked" {
		t.Fatal("an outsider rewrote the maintainer's Sponsors profile")
	}

	// The maintainer themselves may do all of it.
	resp := f.post(t, "/ui-data/sponsors/"+f.maintainer.Login+"/tiers", f.maintainerToken, map[string]interface{}{
		"amount_in_cents": 100, "description": "coffee", "publish": true,
	})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
}

func TestSponsorshipNewslettersRequireSponsorshipOrOwnership(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	outsider := f.createTestUser(t, "sponsors-newsletter-outsider")
	outsiderToken := f.store.CreateToken(outsider.ID, "repo,user").Value

	resp := f.post(t, "/ui-data/sponsors/"+f.maintainer.Login+"/newsletters", f.maintainerToken, map[string]interface{}{
		"subject": "Draft update", "body": "not published yet",
	})
	draft := decodeJSONWithStatus(t, resp, http.StatusCreated)
	resp = f.post(t, "/ui-data/sponsors/"+f.maintainer.Login+"/newsletters", f.maintainerToken, map[string]interface{}{
		"subject": "Published update", "body": "for sponsors", "publish": true,
	})
	decodeJSONWithStatus(t, resp, http.StatusCreated)

	// Someone who is not sponsoring cannot read the updates at all.
	resp = f.get(t, "/ui-data/sponsors/"+f.maintainer.Login+"/newsletters", outsiderToken)
	assertSponsorsStatus(t, resp, http.StatusForbidden)

	// A sponsor sees the published update but never the draft.
	resp = f.post(t, "/ui-data/sponsors/"+f.maintainer.Login+"/sponsorship", f.sponsorToken, map[string]interface{}{
		"tier_id": f.fiveDollarTier.ID, "is_recurring": true,
	})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	resp = f.get(t, "/ui-data/sponsors/"+f.maintainer.Login+"/newsletters", f.sponsorToken)
	rows := decodeJSONArray(t, resp)
	if len(rows) != 1 || rows[0]["subject"] != "Published update" {
		t.Fatalf("sponsor newsletter list = %v, want only the published update", rows)
	}

	// The maintainer sees both, and publishing the draft reveals it.
	resp = f.get(t, "/ui-data/sponsors/"+f.maintainer.Login+"/newsletters", f.maintainerToken)
	if rows := decodeJSONArray(t, resp); len(rows) != 2 {
		t.Fatalf("maintainer newsletter list = %v, want both", rows)
	}
	draftID := int(draft["id"].(float64))
	resp = f.post(t, "/ui-data/sponsors/"+f.maintainer.Login+"/newsletters/"+itoa(draftID)+"/publish", f.maintainerToken, nil)
	decodeJSONWithStatus(t, resp, http.StatusOK)
	resp = f.get(t, "/ui-data/sponsors/"+f.maintainer.Login+"/newsletters", f.sponsorToken)
	if rows := decodeJSONArray(t, resp); len(rows) != 2 {
		t.Fatalf("sponsor newsletter list after publishing = %v, want both", rows)
	}
}

// ---------------------------------------------------------------------------
// webhooks

func TestSponsorshipWebhookDeliversTheActionSequence(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)

	sink := &sponsorshipWebhookSink{}
	receiver := newSponsorsWebhookReceiver(t, sink)
	// The sponsorable is an organization here: sponsorship is an
	// account-level event, and an org's hooks are where GitHub delivers it.
	orgLogin := "sponsors-org-" + itoa(int(f.maintainer.ID))
	f.createOrg(t, orgLogin, "Sponsors Org")
	resp := f.post(t, "/api/v3/orgs/"+orgLogin+"/hooks", defaultToken, map[string]interface{}{
		"name": "web", "active": true, "events": []string{"sponsorship"},
		"config": map[string]interface{}{"url": receiver, "content_type": "json"},
	})
	decodeJSONWithStatus(t, resp, http.StatusCreated)

	orgListing, err := f.store.Sponsors.CreateSponsorsListing(store.SponsorsListingInput{
		SponsorableID: f.store.GetOrg(orgLogin).ID, SponsorableType: "Organization", SponsorableLogin: orgLogin,
		Name: orgLogin,
	})
	if err != nil {
		t.Fatal(err)
	}
	tier := mustCreateSponsorsTier(t, f.isolatedServer, orgListing.ID, 700, false)
	cheaper := mustCreateSponsorsTier(t, f.isolatedServer, orgListing.ID, 200, false)

	resp = f.post(t, "/ui-data/sponsors/"+orgLogin+"/sponsorship", f.sponsorToken, map[string]interface{}{
		"tier_id": tier.ID, "is_recurring": true,
	})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	resp = f.patch(t, "/ui-data/sponsors/"+orgLogin+"/sponsorship", f.sponsorToken, map[string]interface{}{
		"tier_id": cheaper.ID,
	})
	decodeJSONWithStatus(t, resp, http.StatusOK)
	resp = f.delete(t, "/ui-data/sponsors/"+orgLogin+"/sponsorship", f.sponsorToken)
	decodeJSONWithStatus(t, resp, http.StatusOK)

	actions := sink.waitFor(t, 3)
	want := []string{"created", "pending_tier_change", "pending_cancellation"}
	for i, action := range want {
		if actions[i].action != action {
			t.Fatalf("delivery %d action = %q, want %q (all: %v)", i, actions[i].action, action, actions)
		}
		if actions[i].event != "sponsorship" {
			t.Fatalf("delivery %d event = %q", i, actions[i].event)
		}
	}
	first := actions[0].payload["sponsorship"].(map[string]interface{})
	if first["privacy_level"] != "public" {
		t.Fatalf("payload privacy_level = %v, want the lowercased GitHub spelling", first["privacy_level"])
	}
	payloadTier := first["tier"].(map[string]interface{})
	if payloadTier["monthly_price_in_cents"].(float64) != 700 || payloadTier["monthly_price_in_dollars"].(float64) != 7 {
		t.Fatalf("payload tier price = %v", payloadTier)
	}
	if _, ok := payloadTier["is_custom_ammount"]; !ok {
		t.Fatalf("payload tier is missing GitHub's is_custom_ammount member: %v", payloadTier)
	}
	if actions[1].payload["changes"] == nil {
		t.Fatalf("a tier change must carry a changes member: %v", actions[1].payload)
	}
	if actions[2].payload["effective_date"] == nil {
		t.Fatalf("a pending cancellation must carry an effective_date: %v", actions[2].payload)
	}
}

type sponsorshipDelivery struct {
	event   string
	action  string
	payload map[string]interface{}
}

type sponsorshipWebhookSink struct {
	mu         sync.Mutex
	deliveries []sponsorshipDelivery
}

func (sink *sponsorshipWebhookSink) waitFor(t *testing.T, want int) []sponsorshipDelivery {
	t.Helper()
	for attempt := 0; attempt < 200; attempt++ {
		sink.mu.Lock()
		got := make([]sponsorshipDelivery, 0, len(sink.deliveries))
		for _, delivery := range sink.deliveries {
			// The hook's own `ping` lands first and is not a sponsorship.
			if delivery.event == "sponsorship" {
				got = append(got, delivery)
			}
		}
		sink.mu.Unlock()
		if len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	t.Fatalf("expected %d sponsorship deliveries, got %d: %v", want, len(sink.deliveries), sink.deliveries)
	return nil
}

func newSponsorsWebhookReceiver(t *testing.T, sink *sponsorshipWebhookSink) string {
	t.Helper()
	url, cleanup := startWebhookReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		action, _ := parsed["action"].(string)
		sink.mu.Lock()
		sink.deliveries = append(sink.deliveries, sponsorshipDelivery{
			event: r.Header.Get("X-GitHub-Event"), action: action, payload: parsed,
		})
		sink.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	t.Cleanup(cleanup)
	return url
}

// ---------------------------------------------------------------------------
// GraphQL

func TestSponsorableGraphQLSurface(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	if _, err := f.store.Sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: f.sponsor.ID, SponsorType: "User", SponsorLogin: f.sponsor.Login,
		SponsorableID: f.maintainer.ID, SponsorableType: "User", SponsorableLogin: f.maintainer.Login,
		TierID: f.fiveDollarTier.ID, IsRecurring: true, PrivacyLevel: store.SponsorshipPrivacyPublic,
	}); err != nil {
		t.Fatal(err)
	}
	f.store.Sponsors.UpdateSponsorsListing(f.listing.ID, store.SponsorsListingUpdate{
		Goal: &store.SponsorsGoal{Kind: store.SponsorsGoalMonthlyAmount, TargetValue: 2000},
	})

	query := `query($login: String!) {
	  user(login: $login) {
	    hasSponsorsListing
	    monthlyEstimatedSponsorsIncomeInCents
	    estimatedNextSponsorsPayoutInCents
	    isSponsoredBy(accountLogin: "sponsors-sponsor")
	    sponsorsListing {
	      id name slug isPublic contactEmailAddress nextPayoutDate
	      activeGoal { kind targetValue percentComplete title }
	      tiers(first: 10) { totalCount nodes { id name monthlyPriceInCents monthlyPriceInDollars isOneTime } }
	      sponsorable { __typename ... on User { login } }
	    }
	    sponsors(first: 10) { totalCount nodes { __typename ... on User { login } } }
	    sponsorshipsAsMaintainer(first: 10) {
	      totalCount totalRecurringMonthlyPriceInCents totalRecurringMonthlyPriceInDollars
	      nodes { id isActive privacyLevel tier { monthlyPriceInCents } sponsorEntity { ... on User { login } } }
	    }
	  }
	}`
	data := f.gqlData(t, query, map[string]interface{}{"login": f.maintainer.Login})
	user := data["user"].(map[string]interface{})
	if user["hasSponsorsListing"] != true {
		t.Fatalf("hasSponsorsListing = %v", user["hasSponsorsListing"])
	}
	if user["monthlyEstimatedSponsorsIncomeInCents"].(float64) != 500 {
		t.Fatalf("monthlyEstimatedSponsorsIncomeInCents = %v", user["monthlyEstimatedSponsorsIncomeInCents"])
	}
	if user["isSponsoredBy"] != true {
		t.Fatal("isSponsoredBy should see the public sponsorship")
	}
	listing := user["sponsorsListing"].(map[string]interface{})
	if listing["contactEmailAddress"] != "maintainer@example.test" {
		t.Fatalf("the site admin viewer should read the maintainer-only fields: %v", listing)
	}
	goal := listing["activeGoal"].(map[string]interface{})
	if goal["percentComplete"].(float64) != 25 || goal["title"] != "Earn $20 per month" {
		t.Fatalf("activeGoal = %v", goal)
	}
	tiers := listing["tiers"].(map[string]interface{})
	if tiers["totalCount"].(float64) != 3 {
		t.Fatalf("published tiers = %v, want 3", tiers["totalCount"])
	}
	sponsorships := user["sponsorshipsAsMaintainer"].(map[string]interface{})
	if sponsorships["totalRecurringMonthlyPriceInCents"].(float64) != 500 ||
		sponsorships["totalRecurringMonthlyPriceInDollars"].(float64) != 5 {
		t.Fatalf("sponsorship connection totals = %v", sponsorships)
	}
	sponsorsConn := user["sponsors"].(map[string]interface{})
	if sponsorsConn["totalCount"].(float64) != 1 {
		t.Fatalf("sponsors connection = %v", sponsorsConn)
	}
}

func TestPrivateSponsorshipIsHiddenFromThirdParties(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	if _, err := f.store.Sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: f.sponsor.ID, SponsorType: "User", SponsorLogin: f.sponsor.Login,
		SponsorableID: f.maintainer.ID, SponsorableType: "User", SponsorableLogin: f.maintainer.Login,
		TierID: f.fiveDollarTier.ID, IsRecurring: true, PrivacyLevel: store.SponsorshipPrivacyPrivate,
	}); err != nil {
		t.Fatal(err)
	}
	outsider := f.createTestUser(t, "sponsors-private-outsider")
	outsiderToken := f.store.CreateToken(outsider.ID, "repo,user").Value

	query := `query($login: String!) {
	  user(login: $login) {
	    isSponsoredBy(accountLogin: "sponsors-sponsor")
	    sponsors(first: 10) { totalCount }
	    sponsorshipsAsMaintainer(first: 10, includePrivate: true) { totalCount }
	    lifetimeReceivedSponsorshipValues(first: 10) { totalCount }
	    estimatedNextSponsorsPayoutInCents
	  }
	}`
	vars := map[string]interface{}{"login": f.maintainer.Login}

	outside := sponsorsGraphQLAs(t, f.isolatedServer, outsiderToken, query, vars)["user"].(map[string]interface{})
	if outside["isSponsoredBy"] != false {
		t.Fatal("a private sponsorship must not be visible through isSponsoredBy")
	}
	if outside["sponsors"].(map[string]interface{})["totalCount"].(float64) != 0 {
		t.Fatalf("a private sponsor leaked into the sponsors connection: %v", outside["sponsors"])
	}
	if outside["sponsorshipsAsMaintainer"].(map[string]interface{})["totalCount"].(float64) != 0 {
		t.Fatal("includePrivate must not override the privacy boundary for a third party")
	}
	if outside["lifetimeReceivedSponsorshipValues"].(map[string]interface{})["totalCount"].(float64) != 0 {
		t.Fatal("the maintainer's per-sponsor ledger leaked to a third party")
	}
	if outside["estimatedNextSponsorsPayoutInCents"].(float64) != 0 {
		t.Fatal("the maintainer's payout figure leaked to a third party")
	}

	inside := sponsorsGraphQLAs(t, f.isolatedServer, f.maintainerToken, query, vars)["user"].(map[string]interface{})
	if inside["isSponsoredBy"] != true {
		t.Fatal("the maintainer must see their own private sponsor")
	}
	if inside["sponsorshipsAsMaintainer"].(map[string]interface{})["totalCount"].(float64) != 1 {
		t.Fatalf("maintainer view of private sponsorships = %v", inside["sponsorshipsAsMaintainer"])
	}
	if inside["estimatedNextSponsorsPayoutInCents"].(float64) != 500 {
		t.Fatalf("maintainer payout figure = %v", inside["estimatedNextSponsorsPayoutInCents"])
	}
}

func TestSponsorsMutationsRefuseAndEntitle(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	outsider := f.createTestUser(t, "sponsors-mutation-outsider")
	outsiderToken := f.store.CreateToken(outsider.ID, "repo,user").Value

	// Refusal: an outsider cannot spend the sponsor's money.
	refused := sponsorsGraphQLErrors(t, f.isolatedServer, outsiderToken, `mutation($input: CreateSponsorshipInput!) {
	  createSponsorship(input: $input) { sponsorship { id } }
	}`, map[string]interface{}{"input": map[string]interface{}{
		"sponsorLogin": f.sponsor.Login, "sponsorableLogin": f.maintainer.Login, "tierId": f.fiveDollarTier.NodeID,
	}})
	if !strings.Contains(refused, "manage GitHub Sponsors for "+f.sponsor.Login) {
		t.Fatalf("createSponsorship refusal = %q", refused)
	}

	// Refusal: an outsider cannot publish somebody else's tier.
	draft, err := f.store.Sponsors.CreateSponsorsTier(store.SponsorsTierInput{
		ListingID: f.listing.ID, Description: "draft", AmountInCents: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	refused = sponsorsGraphQLErrors(t, f.isolatedServer, outsiderToken, `mutation($input: PublishSponsorsTierInput!) {
	  publishSponsorsTier(input: $input) { sponsorsTier { id } }
	}`, map[string]interface{}{"input": map[string]interface{}{"tierId": draft.NodeID}})
	if !strings.Contains(refused, "manage GitHub Sponsors for "+f.maintainer.Login) {
		t.Fatalf("publishSponsorsTier refusal = %q", refused)
	}

	// Entitled: the sponsor opens their own sponsorship.
	data := sponsorsGraphQLAs(t, f.isolatedServer, f.sponsorToken, `mutation($input: CreateSponsorshipInput!) {
	  createSponsorship(input: $input) {
	    clientMutationId
	    sponsorship { id isActive privacyLevel tier { monthlyPriceInCents } sponsorable { ... on User { login } } }
	  }
	}`, map[string]interface{}{"input": map[string]interface{}{
		"sponsorableLogin": f.maintainer.Login, "tierId": f.fiveDollarTier.NodeID,
		"privacyLevel": "PRIVATE", "clientMutationId": "abc123",
	}})
	payload := data["createSponsorship"].(map[string]interface{})
	if payload["clientMutationId"] != "abc123" {
		t.Fatalf("clientMutationId = %v", payload["clientMutationId"])
	}
	sponsorship := payload["sponsorship"].(map[string]interface{})
	if sponsorship["privacyLevel"] != "PRIVATE" || sponsorship["isActive"] != true {
		t.Fatalf("created sponsorship = %v", sponsorship)
	}

	// Entitled: the maintainer publishes their own tier.
	data = sponsorsGraphQLAs(t, f.isolatedServer, f.maintainerToken, `mutation($input: PublishSponsorsTierInput!) {
	  publishSponsorsTier(input: $input) { sponsorsTier { id adminInfo { isPublished isDraft } } }
	}`, map[string]interface{}{"input": map[string]interface{}{"tierId": draft.NodeID}})
	tier := data["publishSponsorsTier"].(map[string]interface{})["sponsorsTier"].(map[string]interface{})
	admin := tier["adminInfo"].(map[string]interface{})
	if admin["isPublished"] != true || admin["isDraft"] != false {
		t.Fatalf("published tier adminInfo = %v", admin)
	}

	// Entitled: the sponsor updates their own preferences, then cancels.
	data = sponsorsGraphQLAs(t, f.isolatedServer, f.sponsorToken, `mutation($input: UpdateSponsorshipPreferencesInput!) {
	  updateSponsorshipPreferences(input: $input) { sponsorship { privacyLevel isSponsorOptedIntoEmail } }
	}`, map[string]interface{}{"input": map[string]interface{}{
		"sponsorableLogin": f.maintainer.Login, "privacyLevel": "PUBLIC", "receiveEmails": false,
	}})
	updated := data["updateSponsorshipPreferences"].(map[string]interface{})["sponsorship"].(map[string]interface{})
	if updated["privacyLevel"] != "PUBLIC" || updated["isSponsorOptedIntoEmail"] != false {
		t.Fatalf("updated preferences = %v", updated)
	}
	data = sponsorsGraphQLAs(t, f.isolatedServer, f.sponsorToken, `mutation($input: CancelSponsorshipInput!) {
	  cancelSponsorship(input: $input) { sponsorsTier { monthlyPriceInCents } }
	}`, map[string]interface{}{"input": map[string]interface{}{"sponsorableLogin": f.maintainer.Login}})
	if data["cancelSponsorship"].(map[string]interface{})["sponsorsTier"].(map[string]interface{})["monthlyPriceInCents"].(float64) != 500 {
		t.Fatalf("cancelSponsorship payload = %v", data["cancelSponsorship"])
	}
}

func TestSponsorsNodeIDsResolveThroughQueryNode(t *testing.T) {
	t.Parallel()
	f := newSponsorsFixture(t)
	transition, err := f.store.Sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: f.sponsor.ID, SponsorType: "User", SponsorLogin: f.sponsor.Login,
		SponsorableID: f.maintainer.ID, SponsorableType: "User", SponsorableLogin: f.maintainer.Login,
		TierID: f.fiveDollarTier.ID, IsRecurring: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct{ nodeID, typename string }{
		{f.listing.NodeID, "SponsorsListing"},
		{f.fiveDollarTier.NodeID, "SponsorsTier"},
		{transition.Sponsorship.NodeID, "Sponsorship"},
		{transition.Activity.NodeID, "SponsorsActivity"},
	} {
		data := f.gqlData(t, `query($id: ID!) { node(id: $id) { __typename id } }`, map[string]interface{}{"id": probe.nodeID})
		node, _ := data["node"].(map[string]interface{})
		if node == nil || node["__typename"] != probe.typename || node["id"] != probe.nodeID {
			t.Fatalf("node(%q) = %v, want a %s", probe.nodeID, node, probe.typename)
		}
	}
}

// ---------------------------------------------------------------------------
// test helpers

func sponsorsGraphQLAs(t *testing.T, s *isolatedServer, token, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	envelope := sponsorsGraphQLEnvelope(t, s, token, query, variables)
	if errs, ok := envelope["errors"]; ok && errs != nil {
		t.Fatalf("graphql errors: %v", errs)
	}
	data, _ := envelope["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("no data in response: %v", envelope)
	}
	return data
}

func sponsorsGraphQLErrors(t *testing.T, s *isolatedServer, token, query string, variables map[string]interface{}) string {
	t.Helper()
	envelope := sponsorsGraphQLEnvelope(t, s, token, query, variables)
	errs, _ := envelope["errors"].([]interface{})
	if len(errs) == 0 {
		t.Fatalf("expected a refusal, got %v", envelope)
	}
	messages := make([]string, 0, len(errs))
	for _, raw := range errs {
		entry, _ := raw.(map[string]interface{})
		message, _ := entry["message"].(string)
		messages = append(messages, message)
	}
	return strings.Join(messages, "; ")
}

func sponsorsGraphQLEnvelope(t *testing.T, s *isolatedServer, token, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	resp := s.post(t, "/api/graphql", token, body)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("graphql status = %d", resp.StatusCode)
	}
	return decodeJSON(t, resp)
}

func assertSponsorsStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, body)
	}
}

// --- the Sponsors mutation authorization table -----------------------------
//
// Every Sponsors mutation names an account rather than a repository or a
// project, so the repository and project refusal tables say nothing about
// them. This is their table: for each mutation, the input an outsider is
// refused and the principal who is entitled to it.

type gqlSponsorsMutationCase struct {
	name     string
	doc      string
	input    func(f *sponsorsMutationFixture) map[string]interface{}
	entitled func(f *sponsorsMutationFixture) string
}

type sponsorsMutationFixture struct {
	*isolatedServer
	maintainer      *store.User
	maintainerToken string
	sponsor         *store.User
	sponsorToken    string
	outsiderToken   string
	fresh           *store.User
	freshToken      string
	listing         *store.SponsorsListing
	publishedTier   *store.SponsorsTier
	draftTier       *store.SponsorsTier
}

// newSponsorsMutationFixture builds an independent maintainer/sponsor pair
// with a live sponsorship, plus an outsider with no standing over either
// and a fresh account with no Sponsors profile at all.
func (s *isolatedServer) newSponsorsMutationFixture(t *testing.T, suffix string) *sponsorsMutationFixture {
	t.Helper()
	maintainer := s.createTestUser(t, "sponsors-mut-maintainer-"+suffix)
	sponsor := s.createTestUser(t, "sponsors-mut-sponsor-"+suffix)
	outsider := s.createTestUser(t, "sponsors-mut-outsider-"+suffix)
	fresh := s.createTestUser(t, "sponsors-mut-fresh-"+suffix)
	f := &sponsorsMutationFixture{
		isolatedServer:  s,
		maintainer:      maintainer,
		maintainerToken: s.store.CreateToken(maintainer.ID, "repo,user").Value,
		sponsor:         sponsor,
		sponsorToken:    s.store.CreateToken(sponsor.ID, "repo,user").Value,
		outsiderToken:   s.store.CreateToken(outsider.ID, "repo,user").Value,
		fresh:           fresh,
		freshToken:      s.store.CreateToken(fresh.ID, "repo,user").Value,
	}
	listing, err := s.store.Sponsors.CreateSponsorsListing(store.SponsorsListingInput{
		SponsorableID: maintainer.ID, SponsorableType: "User", SponsorableLogin: maintainer.Login,
		Name: maintainer.Login,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.listing = listing
	f.publishedTier = mustCreateSponsorsTier(t, s, listing.ID, 500, false)
	draft, err := s.store.Sponsors.CreateSponsorsTier(store.SponsorsTierInput{
		ListingID: listing.ID, Description: "draft", AmountInCents: 1500,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.draftTier = draft
	if _, err := s.store.Sponsors.CreateSponsorship(store.SponsorshipInput{
		SponsorID: sponsor.ID, SponsorType: "User", SponsorLogin: sponsor.Login,
		SponsorableID: maintainer.ID, SponsorableType: "User", SponsorableLogin: maintainer.Login,
		TierID: f.publishedTier.ID, IsRecurring: true,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func gqlSponsorsMutationCases() []gqlSponsorsMutationCase {
	return []gqlSponsorsMutationCase{
		{
			name: "createSponsorship",
			doc:  `mutation($input: CreateSponsorshipInput!) { createSponsorship(input: $input) { sponsorship { id } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{
					"sponsorLogin": f.fresh.Login, "sponsorableLogin": f.maintainer.Login,
					"tierId": f.publishedTier.NodeID,
				}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.freshToken },
		},
		{
			name: "createSponsorships",
			doc:  `mutation($input: CreateSponsorshipsInput!) { createSponsorships(input: $input) { sponsorables { ... on User { login } } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{
					"sponsorLogin": f.fresh.Login,
					"recurring":    true,
					"sponsorships": []interface{}{map[string]interface{}{
						"amount": 500, "sponsorableLogin": f.maintainer.Login,
					}},
				}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.freshToken },
		},
		{
			name: "cancelSponsorship",
			doc:  `mutation($input: CancelSponsorshipInput!) { cancelSponsorship(input: $input) { sponsorsTier { id } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{"sponsorLogin": f.sponsor.Login, "sponsorableLogin": f.maintainer.Login}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.sponsorToken },
		},
		{
			name: "updateSponsorshipPreferences",
			doc:  `mutation($input: UpdateSponsorshipPreferencesInput!) { updateSponsorshipPreferences(input: $input) { sponsorship { privacyLevel } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{
					"sponsorLogin": f.sponsor.Login, "sponsorableLogin": f.maintainer.Login,
					"privacyLevel": store.SponsorshipPrivacyPrivate,
				}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.sponsorToken },
		},
		{
			name: "createSponsorsListing",
			doc:  `mutation($input: CreateSponsorsListingInput!) { createSponsorsListing(input: $input) { sponsorsListing { slug } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{"sponsorableLogin": f.fresh.Login, "fullDescription": "hello"}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.freshToken },
		},
		{
			name: "createSponsorsTier",
			doc:  `mutation($input: CreateSponsorsTierInput!) { createSponsorsTier(input: $input) { sponsorsTier { monthlyPriceInCents } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{
					"sponsorableLogin": f.maintainer.Login, "amount": 2500, "description": "new tier",
				}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.maintainerToken },
		},
		{
			name: "publishSponsorsTier",
			doc:  `mutation($input: PublishSponsorsTierInput!) { publishSponsorsTier(input: $input) { sponsorsTier { id } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{"tierId": f.draftTier.NodeID}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.maintainerToken },
		},
		{
			name: "retireSponsorsTier",
			doc:  `mutation($input: RetireSponsorsTierInput!) { retireSponsorsTier(input: $input) { sponsorsTier { id } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{"tierId": f.publishedTier.NodeID}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.maintainerToken },
		},
		{
			name: "updatePatreonSponsorability",
			doc:  `mutation($input: UpdatePatreonSponsorabilityInput!) { updatePatreonSponsorability(input: $input) { sponsorsListing { slug } } }`,
			input: func(f *sponsorsMutationFixture) map[string]interface{} {
				return map[string]interface{}{
					"sponsorableLogin": f.maintainer.Login, "enablePatreonSponsorships": true,
				}
			},
			entitled: func(f *sponsorsMutationFixture) string { return f.maintainerToken },
		},
	}
}

// TestGraphQLSponsorsMutationsRefuseAnOutsider is the refusal half: an
// account with no standing over either party to the mutation — neither the
// paying sponsor nor the sponsorable whose profile it is — is refused every
// Sponsors mutation, and nothing it named changed.
func TestGraphQLSponsorsMutationsRefuseAnOutsider(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for i, tc := range gqlSponsorsMutationCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := s.newSponsorsMutationFixture(t, "r"+itoa(i))
			before := s.store.Sponsors.GetSponsorsListing(f.listing.ID)
			beforeTiers := s.store.Sponsors.ListSponsorsTiers(f.listing.ID, true)
			beforeSponsorships := s.store.Sponsors.ListSponsorshipsAsMaintainer(f.maintainer.Login, false)

			message := sponsorsGraphQLErrors(t, s, f.outsiderToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
			if !strings.Contains(message, "manage GitHub Sponsors for") {
				t.Fatalf("%s refusal = %q, want the Sponsors standing refusal", tc.name, message)
			}
			if after := s.store.Sponsors.GetSponsorsListing(f.listing.ID); after.PatreonSponsorshipsEnabled != before.PatreonSponsorshipsEnabled {
				t.Fatalf("%s changed the listing despite being refused", tc.name)
			}
			if len(s.store.Sponsors.ListSponsorsTiers(f.listing.ID, true)) != len(beforeTiers) {
				t.Fatalf("%s changed the tier set despite being refused", tc.name)
			}
			if len(s.store.Sponsors.ListSponsorshipsAsMaintainer(f.maintainer.Login, false)) != len(beforeSponsorships) {
				t.Fatalf("%s changed the sponsorship set despite being refused", tc.name)
			}
		})
	}
}

// TestGraphQLSponsorsMutationsAdmitTheEntitledPrincipal is the other half:
// the account the policy row names — the sponsor for the money mutations,
// the sponsorable for the profile ones — performs each mutation.
func TestGraphQLSponsorsMutationsAdmitTheEntitledPrincipal(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for i, tc := range gqlSponsorsMutationCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := s.newSponsorsMutationFixture(t, "e"+itoa(i))
			data := sponsorsGraphQLAs(t, s, tc.entitled(f), tc.doc, map[string]interface{}{"input": tc.input(f)})
			payload, _ := data[tc.name].(map[string]interface{})
			if payload == nil {
				t.Fatalf("%s returned no payload: %v", tc.name, data)
			}
		})
	}
}
