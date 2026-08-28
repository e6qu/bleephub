package store

// GitHub Sponsors: the durable model behind the Sponsorable GraphQL surface
// and the `sponsorship` webhook family. A [SponsorsListing] carries tiers, a
// goal, featured items and newsletters; a [Sponsorship] bills monthly into a
// [SponsorsInvoice], rolled up into a [SponsorsPayout].
//
// Money is integer US cents everywhere, and no arithmetic divides before it
// multiplies. Nothing here contacts a payment processor; the whole lifecycle
// is simulated.
//
// STORE-021: every getter and List* returns a detached snapshot; only
// Find*ByNodeID returns the live row.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Node-id prefixes for the Sponsors object graph ("<PREFIX>_kgDO%08d").
const (
	SponsorsListingNodeIDPrefix             = "SL_kgDO"
	SponsorsTierNodeIDPrefix                = "ST_kgDO"
	SponsorshipNodeIDPrefix                 = "SP_kgDO"
	SponsorsActivityNodeIDPrefix            = "SA_kgDO"
	SponsorshipNewsletterNodeIDPrefix       = "SN_kgDO"
	SponsorsListingFeaturedItemNodeIDPrefix = "SLFI_kgDO"
)

// Sponsorship privacy levels, matching GraphQL's SponsorshipPrivacy.
const (
	SponsorshipPrivacyPublic  = "PUBLIC"
	SponsorshipPrivacyPrivate = "PRIVATE"
)

// Sponsorship payment sources, matching GraphQL's SponsorshipPaymentSource.
const (
	SponsorshipPaymentSourceGitHub  = "GITHUB"
	SponsorshipPaymentSourcePatreon = "PATREON"
)

// SponsorsActivity actions, matching GraphQL's SponsorsActivityAction.
const (
	SponsorsActivityNewSponsorship       = "NEW_SPONSORSHIP"
	SponsorsActivityCancelledSponsorship = "CANCELLED_SPONSORSHIP"
	SponsorsActivityTierChange           = "TIER_CHANGE"
	SponsorsActivityPendingChange        = "PENDING_CHANGE"
	SponsorsActivityRefund               = "REFUND"
	SponsorsActivitySponsorMatchDisabled = "SPONSOR_MATCH_DISABLED"
)

// SponsorsGoal kinds, matching GraphQL's SponsorsGoalKind.
const (
	SponsorsGoalMonthlyAmount = "MONTHLY_SPONSORSHIP_AMOUNT"
	SponsorsGoalTotalSponsors = "TOTAL_SPONSORS_COUNT"
)

// Featureable kinds, matching GraphQL's
// SponsorsListingFeaturedItemFeatureableType.
const (
	SponsorsFeatureableRepository = "REPOSITORY"
	SponsorsFeatureableUser       = "USER"
)

// SponsorsGoal is the target a maintainer has set on their listing.
// TargetValue is cents for MONTHLY_SPONSORSHIP_AMOUNT and a sponsor count
// for TOTAL_SPONSORS_COUNT.
type SponsorsGoal struct {
	Kind        string `json:"kind"`
	TargetValue int    `json:"target_value"`
	Description string `json:"description,omitempty"`
}

// SponsorsListing is a sponsorable account's GitHub Sponsors profile.
type SponsorsListing struct {
	ID               int    `json:"id"`
	NodeID           string `json:"node_id"`
	Slug             string `json:"slug"`
	SponsorableID    int    `json:"sponsorable_id"`
	SponsorableType  string `json:"sponsorable_type"` // User | Organization
	SponsorableLogin string `json:"sponsorable_login"`
	Name             string `json:"name"`
	ShortDescription string `json:"short_description"`
	FullDescription  string `json:"full_description"`
	ContactEmail     string `json:"contact_email,omitempty"`
	// Payout settings (no money moves, but the maintainer's real config).
	BillingCountryOrRegion          string        `json:"billing_country_or_region,omitempty"`
	ResidenceCountryOrRegion        string        `json:"residence_country_or_region,omitempty"`
	FiscalHostLogin                 string        `json:"fiscal_host_login,omitempty"`
	FiscallyHostedProjectProfileURL string        `json:"fiscally_hosted_project_profile_url,omitempty"`
	PayoutMinimumInCents            int           `json:"payout_minimum_in_cents"`
	NextPayoutDate                  string        `json:"next_payout_date,omitempty"` // YYYY-MM-DD
	IsPublic                        bool          `json:"is_public"`
	PatreonSponsorshipsEnabled      bool          `json:"patreon_sponsorships_enabled"`
	ActiveGoal                      *SponsorsGoal `json:"active_goal,omitempty"`
	CreatedAt                       time.Time     `json:"created_at"`
	UpdatedAt                       time.Time     `json:"updated_at"`
}

// SponsorsTier is one price point on a listing.
type SponsorsTier struct {
	ID                  int       `json:"id"`
	NodeID              string    `json:"node_id"`
	ListingID           int       `json:"listing_id"`
	Name                string    `json:"name"`
	Description         string    `json:"description"`
	MonthlyPriceInCents int       `json:"monthly_price_in_cents"`
	IsOneTime           bool      `json:"is_one_time"`
	IsCustomAmount      bool      `json:"is_custom_amount"`
	IsDraft             bool      `json:"is_draft"`
	IsPublished         bool      `json:"is_published"`
	IsRetired           bool      `json:"is_retired"`
	WelcomeMessage      string    `json:"welcome_message,omitempty"`
	RepositoryID        int       `json:"repository_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// SponsorsListingFeaturedItem promotes a repository or a user on a listing.
type SponsorsListingFeaturedItem struct {
	ID              int       `json:"id"`
	NodeID          string    `json:"node_id"`
	ListingID       int       `json:"listing_id"`
	FeatureableType string    `json:"featureable_type"` // REPOSITORY | USER
	FeatureableID   int       `json:"featureable_id"`
	Description     string    `json:"description,omitempty"`
	Position        int       `json:"position"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Sponsorship is one sponsor's funding relationship with one sponsorable.
//
// The billing state machine lives in these fields:
//
//	IsActive && NextBillingDate != nil && no pending change → ACTIVE
//	PendingTierID != 0                                      → PENDING_TIER_CHANGE
//	PendingCancellation                                     → PENDING_CANCELLATION
//	!IsActive                                               → CANCELLED
//
// A one-time payment has IsOneTimePayment set, no NextBillingDate, and
// exactly one invoice.
type Sponsorship struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`

	SponsorID    int    `json:"sponsor_id"`
	SponsorType  string `json:"sponsor_type"` // User | Organization
	SponsorLogin string `json:"sponsor_login"`

	SponsorableID    int    `json:"sponsorable_id"`
	SponsorableType  string `json:"sponsorable_type"`
	SponsorableLogin string `json:"sponsorable_login"`

	TierID                  int    `json:"tier_id"`
	PrivacyLevel            string `json:"privacy_level"`
	PaymentSource           string `json:"payment_source"`
	IsOneTimePayment        bool   `json:"is_one_time_payment"`
	IsActive                bool   `json:"is_active"`
	IsSponsorOptedIntoEmail bool   `json:"is_sponsor_opted_into_email"`
	ViaBulkSponsorship      bool   `json:"via_bulk_sponsorship"`

	// AmountInCents is copied off the tier at selection time so a later tier
	// edit cannot reprice an existing sponsor.
	AmountInCents int `json:"amount_in_cents"`

	PendingTierID        int        `json:"pending_tier_id,omitempty"`
	PendingCancellation  bool       `json:"pending_cancellation,omitempty"`
	PendingEffectiveDate *time.Time `json:"pending_effective_date,omitempty"`
	NextBillingDate      *time.Time `json:"next_billing_date,omitempty"`

	TierSelectedAt time.Time  `json:"tier_selected_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CancelledAt    *time.Time `json:"cancelled_at,omitempty"`
}

// SponsorsActivity is one entry in a sponsorable's activity feed.
type SponsorsActivity struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
	Action string `json:"action"`

	SponsorableID    int    `json:"sponsorable_id"`
	SponsorableType  string `json:"sponsorable_type"`
	SponsorableLogin string `json:"sponsorable_login"`

	SponsorID    int    `json:"sponsor_id"`
	SponsorType  string `json:"sponsor_type"`
	SponsorLogin string `json:"sponsor_login"`

	SponsorshipID          int       `json:"sponsorship_id"`
	SponsorsTierID         int       `json:"sponsors_tier_id,omitempty"`
	PreviousSponsorsTierID int       `json:"previous_sponsors_tier_id,omitempty"`
	CurrentPrivacyLevel    string    `json:"current_privacy_level,omitempty"`
	PaymentSource          string    `json:"payment_source,omitempty"`
	ViaBulkSponsorship     bool      `json:"via_bulk_sponsorship"`
	Timestamp              time.Time `json:"timestamp"`
}

// SponsorshipNewsletter is a maintainer's update to their sponsors.
type SponsorshipNewsletter struct {
	ID          int       `json:"id"`
	NodeID      string    `json:"node_id"`
	ListingID   int       `json:"listing_id"`
	AuthorID    int       `json:"author_id"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body"`
	IsPublished bool      `json:"is_published"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SponsorsInvoice records one billed period. It is the ledger every reported
// money figure derives from, so totals cannot drift from what was billed.
type SponsorsInvoice struct {
	ID               int       `json:"id"`
	SponsorshipID    int       `json:"sponsorship_id"`
	ListingID        int       `json:"listing_id"`
	SponsorLogin     string    `json:"sponsor_login"`
	SponsorableLogin string    `json:"sponsorable_login"`
	TierID           int       `json:"tier_id"`
	AmountInCents    int       `json:"amount_in_cents"`
	PeriodStart      time.Time `json:"period_start"`
	PeriodEnd        time.Time `json:"period_end"`
	OneTime          bool      `json:"one_time"`
	Prorated         bool      `json:"prorated"`
	Status           string    `json:"status"` // paid | refunded
	PayoutID         int       `json:"payout_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// SponsorsPayout is one maintainer payout run: the invoices billed in a
// period, rolled up into the amount that would be transferred.
type SponsorsPayout struct {
	ID            int       `json:"id"`
	ListingID     int       `json:"listing_id"`
	AmountInCents int       `json:"amount_in_cents"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	Status        string    `json:"status"` // pending | paid
	ScheduledDate string    `json:"scheduled_date"`
	CreatedAt     time.Time `json:"created_at"`
}

// SponsorsStore holds the whole Sponsors object graph behind one mutex so a
// lifecycle transition is atomic across the records it touches.
type SponsorsStore struct {
	Mu      sync.RWMutex `json:"-"`
	Persist *Persistence `json:"-"`
	clock   func() time.Time

	listings      map[int]*SponsorsListing
	tiers         map[int]*SponsorsTier
	sponsorships  map[int]*Sponsorship
	activities    map[int]*SponsorsActivity
	newsletters   map[int]*SponsorshipNewsletter
	featuredItems map[int]*SponsorsListingFeaturedItem
	invoices      map[int]*SponsorsInvoice
	payouts       map[int]*SponsorsPayout

	nextListingID      int
	nextTierID         int
	nextSponsorshipID  int
	nextActivityID     int
	nextNewsletterID   int
	nextFeaturedItemID int
	nextInvoiceID      int
	nextPayoutID       int
}

// NewSponsorsStore builds an empty Sponsors store sharing the given clock, so
// freezing time freezes billing.
func NewSponsorsStore(now func() time.Time) *SponsorsStore {
	return &SponsorsStore{
		clock:              now,
		listings:           map[int]*SponsorsListing{},
		tiers:              map[int]*SponsorsTier{},
		sponsorships:       map[int]*Sponsorship{},
		activities:         map[int]*SponsorsActivity{},
		newsletters:        map[int]*SponsorshipNewsletter{},
		featuredItems:      map[int]*SponsorsListingFeaturedItem{},
		invoices:           map[int]*SponsorsInvoice{},
		payouts:            map[int]*SponsorsPayout{},
		nextListingID:      1,
		nextTierID:         1,
		nextSponsorshipID:  1,
		nextActivityID:     1,
		nextNewsletterID:   1,
		nextFeaturedItemID: 1,
		nextInvoiceID:      1,
		nextPayoutID:       1,
	}
}

func (ss *SponsorsStore) now() time.Time {
	if ss.clock != nil {
		return ss.clock().UTC()
	}
	return time.Now().UTC()
}

// SetClock rebinds the Sponsors store's clock to the owning store's.
func (ss *SponsorsStore) SetClock(now func() time.Time) { ss.clock = now }

func sponsorsNodeID(prefix string, id int) string { return fmt.Sprintf("%s%08d", prefix, id) }

// parseSponsorsNodeID returns the numeric id encoded in a Sponsors global id.
func parseSponsorsNodeID(prefix, nodeID string) (int, bool) {
	if !strings.HasPrefix(nodeID, prefix) {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(nodeID, prefix))
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// clones — every getter hands back a detached snapshot (STORE-021)

func cloneSponsorsListing(l *SponsorsListing) *SponsorsListing {
	if l == nil {
		return nil
	}
	c := *l
	if l.ActiveGoal != nil {
		g := *l.ActiveGoal
		c.ActiveGoal = &g
	}
	return &c
}

func cloneSponsorship(s *Sponsorship) *Sponsorship {
	if s == nil {
		return nil
	}
	c := *s
	if s.PendingEffectiveDate != nil {
		t := *s.PendingEffectiveDate
		c.PendingEffectiveDate = &t
	}
	if s.NextBillingDate != nil {
		t := *s.NextBillingDate
		c.NextBillingDate = &t
	}
	if s.CancelledAt != nil {
		t := *s.CancelledAt
		c.CancelledAt = &t
	}
	return &c
}

// persistence

const (
	sponsorsListingsBucket  = "sponsors_listings"
	sponsorsTiersBucket     = "sponsors_tiers"
	sponsorshipsBucket      = "sponsorships"
	sponsorsActivityBucket  = "sponsors_activities"
	sponsorsNewsletterBkt   = "sponsorship_newsletters"
	sponsorsFeaturedBucket  = "sponsors_featured_items"
	sponsorsInvoicesBucket  = "sponsors_invoices"
	sponsorsPayoutsBucket   = "sponsors_payouts"
	sponsorsPersistFailedOp = "batch"
)

func (ss *SponsorsStore) batch() *PersistBatch { return NewPersistBatch(ss.Persist) }

func (ss *SponsorsStore) commit(b *PersistBatch, bucket string) {
	if err := b.Commit(); err != nil {
		panic(&PersistenceFailure{Op: sponsorsPersistFailedOp, Bucket: bucket, Err: err})
	}
}

// loadSponsors repopulates the Sponsors store from disk.
func (st *Store) loadSponsors() error {
	ss := st.Sponsors
	ss.Persist = st.Persist
	if err := st.loadBucket(sponsorsListingsBucket, func(raw []byte) error {
		var l SponsorsListing
		if err := LoadJSON(raw, &l); err != nil {
			return err
		}
		ss.listings[l.ID] = &l
		if l.ID >= ss.nextListingID {
			ss.nextListingID = l.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket(sponsorsTiersBucket, func(raw []byte) error {
		var t SponsorsTier
		if err := LoadJSON(raw, &t); err != nil {
			return err
		}
		ss.tiers[t.ID] = &t
		if t.ID >= ss.nextTierID {
			ss.nextTierID = t.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket(sponsorshipsBucket, func(raw []byte) error {
		var s Sponsorship
		if err := LoadJSON(raw, &s); err != nil {
			return err
		}
		ss.sponsorships[s.ID] = &s
		if s.ID >= ss.nextSponsorshipID {
			ss.nextSponsorshipID = s.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket(sponsorsActivityBucket, func(raw []byte) error {
		var a SponsorsActivity
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		ss.activities[a.ID] = &a
		if a.ID >= ss.nextActivityID {
			ss.nextActivityID = a.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket(sponsorsNewsletterBkt, func(raw []byte) error {
		var n SponsorshipNewsletter
		if err := LoadJSON(raw, &n); err != nil {
			return err
		}
		ss.newsletters[n.ID] = &n
		if n.ID >= ss.nextNewsletterID {
			ss.nextNewsletterID = n.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket(sponsorsFeaturedBucket, func(raw []byte) error {
		var f SponsorsListingFeaturedItem
		if err := LoadJSON(raw, &f); err != nil {
			return err
		}
		ss.featuredItems[f.ID] = &f
		if f.ID >= ss.nextFeaturedItemID {
			ss.nextFeaturedItemID = f.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket(sponsorsInvoicesBucket, func(raw []byte) error {
		var i SponsorsInvoice
		if err := LoadJSON(raw, &i); err != nil {
			return err
		}
		ss.invoices[i.ID] = &i
		if i.ID >= ss.nextInvoiceID {
			ss.nextInvoiceID = i.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	return st.loadBucket(sponsorsPayoutsBucket, func(raw []byte) error {
		var p SponsorsPayout
		if err := LoadJSON(raw, &p); err != nil {
			return err
		}
		ss.payouts[p.ID] = &p
		if p.ID >= ss.nextPayoutID {
			ss.nextPayoutID = p.ID + 1
		}
		return nil
	})
}

// listings

// SponsorsListingInput is the maintainer-supplied half of a listing.
type SponsorsListingInput struct {
	SponsorableID                   int
	SponsorableType                 string
	SponsorableLogin                string
	Name                            string
	ShortDescription                string
	FullDescription                 string
	ContactEmail                    string
	BillingCountryOrRegion          string
	ResidenceCountryOrRegion        string
	FiscalHostLogin                 string
	FiscallyHostedProjectProfileURL string
}

// CreateSponsorsListing opens a listing for the sponsorable. An account may
// hold only one.
func (ss *SponsorsStore) CreateSponsorsListing(in SponsorsListingInput) (*SponsorsListing, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	if in.SponsorableLogin == "" || in.SponsorableID <= 0 {
		return nil, fmt.Errorf("sponsorable account is required")
	}
	for _, l := range ss.listings {
		if strings.EqualFold(l.SponsorableLogin, in.SponsorableLogin) {
			return nil, fmt.Errorf("sponsorable already has a GitHub Sponsors listing")
		}
	}
	now := ss.now()
	listing := &SponsorsListing{
		ID:                              ss.nextListingID,
		NodeID:                          sponsorsNodeID(SponsorsListingNodeIDPrefix, ss.nextListingID),
		Slug:                            strings.ToLower(in.SponsorableLogin),
		SponsorableID:                   in.SponsorableID,
		SponsorableType:                 in.SponsorableType,
		SponsorableLogin:                in.SponsorableLogin,
		Name:                            in.Name,
		ShortDescription:                in.ShortDescription,
		FullDescription:                 in.FullDescription,
		ContactEmail:                    in.ContactEmail,
		BillingCountryOrRegion:          in.BillingCountryOrRegion,
		ResidenceCountryOrRegion:        in.ResidenceCountryOrRegion,
		FiscalHostLogin:                 in.FiscalHostLogin,
		FiscallyHostedProjectProfileURL: in.FiscallyHostedProjectProfileURL,
		PayoutMinimumInCents:            10000,
		NextPayoutDate:                  SponsorsNextPayoutDate(now),
		IsPublic:                        true,
		CreatedAt:                       now,
		UpdatedAt:                       now,
	}
	b := ss.batch()
	b.Put(sponsorsListingsBucket, strconv.Itoa(listing.ID), listing)
	ss.commit(b, sponsorsListingsBucket)
	ss.listings[listing.ID] = listing
	ss.nextListingID++
	return cloneSponsorsListing(listing), nil
}

// SponsorsNextPayoutDate is the first day of the next calendar month.
func SponsorsNextPayoutDate(now time.Time) string {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return first.Format("2006-01-02")
}

// GetSponsorsListing returns the listing by database id.
func (ss *SponsorsStore) GetSponsorsListing(id int) *SponsorsListing {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return cloneSponsorsListing(ss.listings[id])
}

// GetSponsorsListingForAccount returns the sponsorable's listing, or nil.
func (ss *SponsorsStore) GetSponsorsListingForAccount(login string) *SponsorsListing {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return cloneSponsorsListing(ss.listingForAccountLocked(login))
}

func (ss *SponsorsStore) listingForAccountLocked(login string) *SponsorsListing {
	for _, l := range ss.listings {
		if strings.EqualFold(l.SponsorableLogin, login) {
			return l
		}
	}
	return nil
}

// FindSponsorsListingByNodeID returns the live listing row (STORE-021 node
// lookup exception).
func (ss *SponsorsStore) FindSponsorsListingByNodeID(nodeID string) *SponsorsListing {
	id, ok := parseSponsorsNodeID(SponsorsListingNodeIDPrefix, nodeID)
	if !ok {
		return nil
	}
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return ss.listings[id]
}

// ListSponsorsListings returns every listing, ordered by sponsorable login.
func (ss *SponsorsStore) ListSponsorsListings() []*SponsorsListing {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	out := make([]*SponsorsListing, 0, len(ss.listings))
	for _, l := range ss.listings {
		out = append(out, cloneSponsorsListing(l))
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].SponsorableLogin) < strings.ToLower(out[j].SponsorableLogin)
	})
	return out
}

// SponsorsListingUpdate is a sparse patch: a nil member is left alone.
type SponsorsListingUpdate struct {
	Name                     *string
	ShortDescription         *string
	FullDescription          *string
	ContactEmail             *string
	BillingCountryOrRegion   *string
	ResidenceCountryOrRegion *string
	FiscalHostLogin          *string
	IsPublic                 *bool
	PatreonEnabled           *bool
	Goal                     *SponsorsGoal
	ClearGoal                bool
}

// UpdateSponsorsListing applies a sparse patch and returns the new state.
func (ss *SponsorsStore) UpdateSponsorsListing(id int, patch SponsorsListingUpdate) *SponsorsListing {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	listing := ss.listings[id]
	if listing == nil {
		return nil
	}
	updated := *listing
	if listing.ActiveGoal != nil {
		g := *listing.ActiveGoal
		updated.ActiveGoal = &g
	}
	if patch.Name != nil {
		updated.Name = *patch.Name
	}
	if patch.ShortDescription != nil {
		updated.ShortDescription = *patch.ShortDescription
	}
	if patch.FullDescription != nil {
		updated.FullDescription = *patch.FullDescription
	}
	if patch.ContactEmail != nil {
		updated.ContactEmail = *patch.ContactEmail
	}
	if patch.BillingCountryOrRegion != nil {
		updated.BillingCountryOrRegion = *patch.BillingCountryOrRegion
	}
	if patch.ResidenceCountryOrRegion != nil {
		updated.ResidenceCountryOrRegion = *patch.ResidenceCountryOrRegion
	}
	if patch.FiscalHostLogin != nil {
		updated.FiscalHostLogin = *patch.FiscalHostLogin
	}
	if patch.IsPublic != nil {
		updated.IsPublic = *patch.IsPublic
	}
	if patch.PatreonEnabled != nil {
		updated.PatreonSponsorshipsEnabled = *patch.PatreonEnabled
	}
	if patch.ClearGoal {
		updated.ActiveGoal = nil
	} else if patch.Goal != nil {
		g := *patch.Goal
		updated.ActiveGoal = &g
	}
	updated.UpdatedAt = ss.now()
	b := ss.batch()
	b.Put(sponsorsListingsBucket, strconv.Itoa(updated.ID), &updated)
	ss.commit(b, sponsorsListingsBucket)
	ss.listings[updated.ID] = &updated
	return cloneSponsorsListing(&updated)
}

// tiers

// SponsorsTierInput is the maintainer-supplied half of a tier.
type SponsorsTierInput struct {
	ListingID      int
	Name           string
	Description    string
	AmountInCents  int
	IsOneTime      bool
	IsCustomAmount bool
	Publish        bool
	WelcomeMessage string
	RepositoryID   int
}

// CreateSponsorsTier adds a tier to a listing. A tier is created as a
// draft unless Publish is set; a draft tier cannot back a sponsorship.
func (ss *SponsorsStore) CreateSponsorsTier(in SponsorsTierInput) (*SponsorsTier, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	if ss.listings[in.ListingID] == nil {
		return nil, fmt.Errorf("sponsors listing not found")
	}
	if in.AmountInCents <= 0 {
		return nil, fmt.Errorf("tier amount must be a positive number of cents")
	}
	for _, t := range ss.tiers {
		if t.ListingID == in.ListingID && !t.IsRetired && t.IsOneTime == in.IsOneTime &&
			t.MonthlyPriceInCents == in.AmountInCents && !t.IsCustomAmount {
			return nil, fmt.Errorf("a tier at that amount already exists")
		}
	}
	now := ss.now()
	name := in.Name
	if name == "" {
		name = fmt.Sprintf("$%d a month", in.AmountInCents/100)
		if in.IsOneTime {
			name = fmt.Sprintf("$%d one time", in.AmountInCents/100)
		}
	}
	tier := &SponsorsTier{
		ID:                  ss.nextTierID,
		NodeID:              sponsorsNodeID(SponsorsTierNodeIDPrefix, ss.nextTierID),
		ListingID:           in.ListingID,
		Name:                name,
		Description:         in.Description,
		MonthlyPriceInCents: in.AmountInCents,
		IsOneTime:           in.IsOneTime,
		IsCustomAmount:      in.IsCustomAmount,
		IsDraft:             !in.Publish,
		IsPublished:         in.Publish,
		WelcomeMessage:      in.WelcomeMessage,
		RepositoryID:        in.RepositoryID,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	b := ss.batch()
	b.Put(sponsorsTiersBucket, strconv.Itoa(tier.ID), tier)
	ss.commit(b, sponsorsTiersBucket)
	ss.tiers[tier.ID] = tier
	ss.nextTierID++
	t := *tier
	return &t, nil
}

// GetSponsorsTier returns a tier by database id.
func (ss *SponsorsStore) GetSponsorsTier(id int) *SponsorsTier {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	if t := ss.tiers[id]; t != nil {
		c := *t
		return &c
	}
	return nil
}

// FindSponsorsTierByNodeID returns the live tier row for a global id.
func (ss *SponsorsStore) FindSponsorsTierByNodeID(nodeID string) *SponsorsTier {
	id, ok := parseSponsorsNodeID(SponsorsTierNodeIDPrefix, nodeID)
	if !ok {
		return nil
	}
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return ss.tiers[id]
}

// ListSponsorsTiers returns a listing's tiers ordered by monthly price,
// including drafts and retired tiers only when asked.
func (ss *SponsorsStore) ListSponsorsTiers(listingID int, includeUnpublished bool) []*SponsorsTier {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	out := make([]*SponsorsTier, 0, len(ss.tiers))
	for _, t := range ss.tiers {
		if t.ListingID != listingID {
			continue
		}
		if !includeUnpublished && !t.IsPublished {
			continue
		}
		c := *t
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MonthlyPriceInCents != out[j].MonthlyPriceInCents {
			return out[i].MonthlyPriceInCents < out[j].MonthlyPriceInCents
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// PublishSponsorsTier moves a draft tier to published.
func (ss *SponsorsStore) PublishSponsorsTier(id int) (*SponsorsTier, error) {
	return ss.setTierState(id, func(t *SponsorsTier) error {
		if t.IsRetired {
			return fmt.Errorf("a retired tier cannot be published")
		}
		t.IsDraft, t.IsPublished, t.IsRetired = false, true, false
		return nil
	})
}

// RetireSponsorsTier retires a tier: existing sponsorships keep billing at
// the amount they locked in, but no new sponsorship may select it.
func (ss *SponsorsStore) RetireSponsorsTier(id int) (*SponsorsTier, error) {
	return ss.setTierState(id, func(t *SponsorsTier) error {
		t.IsDraft, t.IsPublished, t.IsRetired = false, false, true
		return nil
	})
}

func (ss *SponsorsStore) setTierState(id int, apply func(*SponsorsTier) error) (*SponsorsTier, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	tier := ss.tiers[id]
	if tier == nil {
		return nil, fmt.Errorf("sponsors tier not found")
	}
	updated := *tier
	if err := apply(&updated); err != nil {
		return nil, err
	}
	updated.UpdatedAt = ss.now()
	b := ss.batch()
	b.Put(sponsorsTiersBucket, strconv.Itoa(updated.ID), &updated)
	ss.commit(b, sponsorsTiersBucket)
	ss.tiers[updated.ID] = &updated
	c := updated
	return &c, nil
}

// featured items

// FeatureSponsorsListingItem promotes a repository or user on a listing,
// appending it at the end of the featured order.
func (ss *SponsorsStore) FeatureSponsorsListingItem(listingID int, featureableType string, featureableID int, description string) (*SponsorsListingFeaturedItem, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	if ss.listings[listingID] == nil {
		return nil, fmt.Errorf("sponsors listing not found")
	}
	if featureableType != SponsorsFeatureableRepository && featureableType != SponsorsFeatureableUser {
		return nil, fmt.Errorf("featureable type must be REPOSITORY or USER")
	}
	position := 0
	for _, f := range ss.featuredItems {
		if f.ListingID != listingID {
			continue
		}
		if f.FeatureableType == featureableType && f.FeatureableID == featureableID {
			return nil, fmt.Errorf("item is already featured")
		}
		if f.Position > position {
			position = f.Position
		}
	}
	now := ss.now()
	item := &SponsorsListingFeaturedItem{
		ID:              ss.nextFeaturedItemID,
		NodeID:          sponsorsNodeID(SponsorsListingFeaturedItemNodeIDPrefix, ss.nextFeaturedItemID),
		ListingID:       listingID,
		FeatureableType: featureableType,
		FeatureableID:   featureableID,
		Description:     description,
		Position:        position + 1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	b := ss.batch()
	b.Put(sponsorsFeaturedBucket, strconv.Itoa(item.ID), item)
	ss.commit(b, sponsorsFeaturedBucket)
	ss.featuredItems[item.ID] = item
	ss.nextFeaturedItemID++
	c := *item
	return &c, nil
}

// UnfeatureSponsorsListingItem removes a featured item and closes the gap
// its position left, so positions stay 1..n with no holes.
func (ss *SponsorsStore) UnfeatureSponsorsListingItem(id int) bool {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	item := ss.featuredItems[id]
	if item == nil {
		return false
	}
	b := ss.batch()
	b.Delete(sponsorsFeaturedBucket, strconv.Itoa(id))
	renumbered := map[int]*SponsorsListingFeaturedItem{}
	for _, other := range ss.featuredItems {
		if other.ListingID != item.ListingID || other.ID == id || other.Position < item.Position {
			continue
		}
		moved := *other
		moved.Position--
		renumbered[moved.ID] = &moved
		b.Put(sponsorsFeaturedBucket, strconv.Itoa(moved.ID), &moved)
	}
	ss.commit(b, sponsorsFeaturedBucket)
	delete(ss.featuredItems, id)
	for movedID, moved := range renumbered {
		ss.featuredItems[movedID] = moved
	}
	return true
}

// ListSponsorsListingFeaturedItems returns a listing's featured items in
// promotion order, optionally filtered to the given featureable types.
func (ss *SponsorsStore) ListSponsorsListingFeaturedItems(listingID int, types []string) []*SponsorsListingFeaturedItem {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	wanted := map[string]bool{}
	for _, t := range types {
		wanted[t] = true
	}
	out := make([]*SponsorsListingFeaturedItem, 0, len(ss.featuredItems))
	for _, f := range ss.featuredItems {
		if f.ListingID != listingID {
			continue
		}
		if len(wanted) > 0 && !wanted[f.FeatureableType] {
			continue
		}
		c := *f
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

// FindSponsorsFeaturedItemByNodeID returns the live featured-item row.
func (ss *SponsorsStore) FindSponsorsFeaturedItemByNodeID(nodeID string) *SponsorsListingFeaturedItem {
	id, ok := parseSponsorsNodeID(SponsorsListingFeaturedItemNodeIDPrefix, nodeID)
	if !ok {
		return nil
	}
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return ss.featuredItems[id]
}

// newsletters

// CreateSponsorshipNewsletter drafts (or publishes) an update to sponsors.
func (ss *SponsorsStore) CreateSponsorshipNewsletter(listingID, authorID int, subject, body string, publish bool) (*SponsorshipNewsletter, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	if ss.listings[listingID] == nil {
		return nil, fmt.Errorf("sponsors listing not found")
	}
	if strings.TrimSpace(subject) == "" {
		return nil, fmt.Errorf("newsletter subject is required")
	}
	now := ss.now()
	n := &SponsorshipNewsletter{
		ID:          ss.nextNewsletterID,
		NodeID:      sponsorsNodeID(SponsorshipNewsletterNodeIDPrefix, ss.nextNewsletterID),
		ListingID:   listingID,
		AuthorID:    authorID,
		Subject:     subject,
		Body:        body,
		IsPublished: publish,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	b := ss.batch()
	b.Put(sponsorsNewsletterBkt, strconv.Itoa(n.ID), n)
	ss.commit(b, sponsorsNewsletterBkt)
	ss.newsletters[n.ID] = n
	ss.nextNewsletterID++
	c := *n
	return &c, nil
}

// PublishSponsorshipNewsletter sends a drafted newsletter to sponsors.
func (ss *SponsorsStore) PublishSponsorshipNewsletter(id int) (*SponsorshipNewsletter, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	n := ss.newsletters[id]
	if n == nil {
		return nil, fmt.Errorf("sponsorship newsletter not found")
	}
	updated := *n
	updated.IsPublished = true
	updated.UpdatedAt = ss.now()
	b := ss.batch()
	b.Put(sponsorsNewsletterBkt, strconv.Itoa(updated.ID), &updated)
	ss.commit(b, sponsorsNewsletterBkt)
	ss.newsletters[updated.ID] = &updated
	c := updated
	return &c, nil
}

// ListSponsorshipNewsletters returns a listing's newsletters newest first;
// unpublished drafts are included only for the maintainer.
func (ss *SponsorsStore) ListSponsorshipNewsletters(listingID int, includeDrafts bool) []*SponsorshipNewsletter {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	out := make([]*SponsorshipNewsletter, 0, len(ss.newsletters))
	for _, n := range ss.newsletters {
		if n.ListingID != listingID || (!includeDrafts && !n.IsPublished) {
			continue
		}
		c := *n
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// FindSponsorshipNewsletterByNodeID returns the live newsletter row.
func (ss *SponsorsStore) FindSponsorshipNewsletterByNodeID(nodeID string) *SponsorshipNewsletter {
	id, ok := parseSponsorsNodeID(SponsorshipNewsletterNodeIDPrefix, nodeID)
	if !ok {
		return nil
	}
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return ss.newsletters[id]
}

// sponsorships — the billing state machine

// SponsorshipInput opens a sponsorship.
type SponsorshipInput struct {
	SponsorID        int
	SponsorType      string
	SponsorLogin     string
	SponsorableID    int
	SponsorableType  string
	SponsorableLogin string
	TierID           int
	// AmountInCents is honoured only for a custom-amount tier; otherwise
	// the tier's price is authoritative.
	AmountInCents      int
	PrivacyLevel       string
	ReceiveEmails      bool
	IsRecurring        bool
	ViaBulkSponsorship bool
	PaymentSource      string
}

// SponsorsTransition is what a lifecycle call reports back: the sponsorship
// after the change, its activity, any invoice, and the tiers on either side.
// The server renders the `sponsorship` webhook from it.
type SponsorsTransition struct {
	Sponsorship  *Sponsorship
	Previous     *Sponsorship
	Activity     *SponsorsActivity
	Invoice      *SponsorsInvoice
	Tier         *SponsorsTier
	PreviousTier *SponsorsTier
}

// sponsorshipKeyLocked finds the sponsor→sponsorable sponsorship. Callers hold
// the lock.
func (ss *SponsorsStore) sponsorshipKeyLocked(sponsorLogin, sponsorableLogin string, activeOnly bool) *Sponsorship {
	var found *Sponsorship
	for _, s := range ss.sponsorships {
		if !strings.EqualFold(s.SponsorLogin, sponsorLogin) || !strings.EqualFold(s.SponsorableLogin, sponsorableLogin) {
			continue
		}
		if activeOnly && !s.IsActive {
			continue
		}
		if found == nil || s.CreatedAt.After(found.CreatedAt) || (s.CreatedAt.Equal(found.CreatedAt) && s.ID > found.ID) {
			found = s
		}
	}
	return found
}

// nextSponsorsBillingDate advances one calendar month (the recurring cycle).
func nextSponsorsBillingDate(from time.Time) time.Time {
	return from.AddDate(0, 1, 0).UTC()
}

// CreateSponsorship opens a sponsorship and bills its first period. Recurring
// gets a next billing date one month out; a one-time payment gets one invoice.
func (ss *SponsorsStore) CreateSponsorship(in SponsorshipInput) (*SponsorsTransition, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()

	if in.SponsorID <= 0 || in.SponsorableID <= 0 {
		return nil, fmt.Errorf("sponsor and sponsorable accounts are required")
	}
	if strings.EqualFold(in.SponsorLogin, in.SponsorableLogin) {
		return nil, fmt.Errorf("an account cannot sponsor itself")
	}
	listing := ss.listingForAccountLocked(in.SponsorableLogin)
	if listing == nil {
		return nil, fmt.Errorf("sponsorable has no GitHub Sponsors listing")
	}
	tier := ss.tiers[in.TierID]
	if tier == nil || tier.ListingID != listing.ID {
		return nil, fmt.Errorf("sponsors tier not found for this sponsorable")
	}
	if !tier.IsPublished {
		return nil, fmt.Errorf("sponsors tier is not published")
	}
	if tier.IsOneTime == in.IsRecurring {
		return nil, fmt.Errorf("tier frequency does not match the requested sponsorship")
	}
	if existing := ss.sponsorshipKeyLocked(in.SponsorLogin, in.SponsorableLogin, true); existing != nil && existing.IsOneTimePayment == !in.IsRecurring {
		if !existing.IsOneTimePayment {
			return nil, fmt.Errorf("sponsor already has an active sponsorship for this sponsorable")
		}
	}
	amount := tier.MonthlyPriceInCents
	if tier.IsCustomAmount && in.AmountInCents > 0 {
		amount = in.AmountInCents
	}
	privacy := in.PrivacyLevel
	if privacy != SponsorshipPrivacyPrivate {
		privacy = SponsorshipPrivacyPublic
	}
	paymentSource := in.PaymentSource
	if paymentSource != SponsorshipPaymentSourcePatreon {
		paymentSource = SponsorshipPaymentSourceGitHub
	}

	now := ss.now()
	sponsorship := &Sponsorship{
		ID:                      ss.nextSponsorshipID,
		NodeID:                  sponsorsNodeID(SponsorshipNodeIDPrefix, ss.nextSponsorshipID),
		SponsorID:               in.SponsorID,
		SponsorType:             in.SponsorType,
		SponsorLogin:            in.SponsorLogin,
		SponsorableID:           in.SponsorableID,
		SponsorableType:         in.SponsorableType,
		SponsorableLogin:        in.SponsorableLogin,
		TierID:                  tier.ID,
		PrivacyLevel:            privacy,
		PaymentSource:           paymentSource,
		IsOneTimePayment:        !in.IsRecurring,
		IsActive:                true,
		IsSponsorOptedIntoEmail: in.ReceiveEmails,
		ViaBulkSponsorship:      in.ViaBulkSponsorship,
		AmountInCents:           amount,
		TierSelectedAt:          now,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	periodEnd := nextSponsorsBillingDate(now)
	if in.IsRecurring {
		next := periodEnd
		sponsorship.NextBillingDate = &next
	}

	invoice := ss.newInvoiceLocked(sponsorship, listing, amount, now, periodEnd, !in.IsRecurring, false)
	activity := ss.newActivityLocked(SponsorsActivityNewSponsorship, sponsorship, tier, nil, now)

	b := ss.batch()
	b.Put(sponsorshipsBucket, strconv.Itoa(sponsorship.ID), sponsorship)
	b.Put(sponsorsInvoicesBucket, strconv.Itoa(invoice.ID), invoice)
	b.Put(sponsorsActivityBucket, strconv.Itoa(activity.ID), activity)
	ss.commit(b, sponsorshipsBucket)

	ss.sponsorships[sponsorship.ID] = sponsorship
	ss.nextSponsorshipID++
	ss.invoices[invoice.ID] = invoice
	ss.nextInvoiceID++
	ss.activities[activity.ID] = activity
	ss.nextActivityID++

	tierCopy := *tier
	return &SponsorsTransition{
		Sponsorship: cloneSponsorship(sponsorship),
		Activity:    activity,
		Invoice:     invoice,
		Tier:        &tierCopy,
	}, nil
}

// newInvoiceLocked mints an invoice; the caller persists and indexes it.
// Callers hold the lock.
func (ss *SponsorsStore) newInvoiceLocked(s *Sponsorship, listing *SponsorsListing, amount int, start, end time.Time, oneTime, prorated bool) *SponsorsInvoice {
	return &SponsorsInvoice{
		ID:               ss.nextInvoiceID,
		SponsorshipID:    s.ID,
		ListingID:        listing.ID,
		SponsorLogin:     s.SponsorLogin,
		SponsorableLogin: s.SponsorableLogin,
		TierID:           s.TierID,
		AmountInCents:    amount,
		PeriodStart:      start,
		PeriodEnd:        end,
		OneTime:          oneTime,
		Prorated:         prorated,
		Status:           "paid",
		CreatedAt:        start,
	}
}

// newActivityLocked mints an activity row. Callers hold the lock.
func (ss *SponsorsStore) newActivityLocked(action string, s *Sponsorship, tier, previousTier *SponsorsTier, at time.Time) *SponsorsActivity {
	a := &SponsorsActivity{
		ID:                  ss.nextActivityID,
		NodeID:              sponsorsNodeID(SponsorsActivityNodeIDPrefix, ss.nextActivityID),
		Action:              action,
		SponsorableID:       s.SponsorableID,
		SponsorableType:     s.SponsorableType,
		SponsorableLogin:    s.SponsorableLogin,
		SponsorID:           s.SponsorID,
		SponsorType:         s.SponsorType,
		SponsorLogin:        s.SponsorLogin,
		SponsorshipID:       s.ID,
		CurrentPrivacyLevel: s.PrivacyLevel,
		PaymentSource:       s.PaymentSource,
		ViaBulkSponsorship:  s.ViaBulkSponsorship,
		Timestamp:           at,
	}
	if tier != nil {
		a.SponsorsTierID = tier.ID
	}
	if previousTier != nil {
		a.PreviousSponsorsTierID = previousTier.ID
	}
	return a
}

// ChangeSponsorshipTier moves a recurring sponsorship to another tier. An
// upgrade takes effect now and bills the prorated difference (multiply by
// remaining days before dividing, so no rounding accumulates); a downgrade is
// deferred to the next billing date, as GitHub does.
func (ss *SponsorsStore) ChangeSponsorshipTier(sponsorshipID, tierID int) (*SponsorsTransition, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	s := ss.sponsorships[sponsorshipID]
	if s == nil || !s.IsActive {
		return nil, fmt.Errorf("sponsorship not found")
	}
	if s.IsOneTimePayment {
		return nil, fmt.Errorf("a one-time sponsorship has no tier to change")
	}
	newTier := ss.tiers[tierID]
	oldTier := ss.tiers[s.TierID]
	listing := ss.listings[newTier.ListingIDOrZero()]
	if newTier == nil || oldTier == nil || newTier.ListingID != oldTier.ListingID || listing == nil {
		return nil, fmt.Errorf("sponsors tier not found for this sponsorable")
	}
	if !newTier.IsPublished {
		return nil, fmt.Errorf("sponsors tier is not published")
	}
	if newTier.IsOneTime {
		return nil, fmt.Errorf("a recurring sponsorship cannot move to a one-time tier")
	}
	if newTier.ID == s.TierID {
		return nil, fmt.Errorf("sponsorship is already at that tier")
	}

	now := ss.now()
	previous := cloneSponsorship(s)
	updated := *s
	if updated.NextBillingDate != nil {
		t := *updated.NextBillingDate
		updated.NextBillingDate = &t
	}
	b := ss.batch()

	var invoice *SponsorsInvoice
	var activity *SponsorsActivity
	if newTier.MonthlyPriceInCents > s.AmountInCents {
		// Upgrade: effective now, difference billed for the remainder of
		// the period.
		periodEnd := now
		if s.NextBillingDate != nil {
			periodEnd = *s.NextBillingDate
		}
		periodStart := periodEnd.AddDate(0, -1, 0)
		total := int(periodEnd.Sub(periodStart) / (24 * time.Hour))
		remaining := int(periodEnd.Sub(now) / (24 * time.Hour))
		if remaining < 0 {
			remaining = 0
		}
		if total <= 0 {
			total, remaining = 1, 1
		}
		difference := (newTier.MonthlyPriceInCents - s.AmountInCents) * remaining / total
		updated.TierID = newTier.ID
		updated.AmountInCents = newTier.MonthlyPriceInCents
		updated.TierSelectedAt = now
		updated.PendingTierID = 0
		updated.PendingEffectiveDate = nil
		if difference > 0 {
			invoice = ss.newInvoiceLocked(&updated, listing, difference, now, periodEnd, false, true)
			b.Put(sponsorsInvoicesBucket, strconv.Itoa(invoice.ID), invoice)
		}
		activity = ss.newActivityLocked(SponsorsActivityTierChange, &updated, newTier, oldTier, now)
	} else {
		// Downgrade: scheduled for the next billing date.
		effective := nextSponsorsBillingDate(now)
		if s.NextBillingDate != nil {
			effective = *s.NextBillingDate
		}
		updated.PendingTierID = newTier.ID
		updated.PendingCancellation = false
		updated.PendingEffectiveDate = &effective
		activity = ss.newActivityLocked(SponsorsActivityPendingChange, &updated, newTier, oldTier, now)
	}
	updated.UpdatedAt = now
	b.Put(sponsorshipsBucket, strconv.Itoa(updated.ID), &updated)
	b.Put(sponsorsActivityBucket, strconv.Itoa(activity.ID), activity)
	ss.commit(b, sponsorshipsBucket)

	ss.sponsorships[updated.ID] = &updated
	if invoice != nil {
		ss.invoices[invoice.ID] = invoice
		ss.nextInvoiceID++
	}
	ss.activities[activity.ID] = activity
	ss.nextActivityID++

	newTierCopy, oldTierCopy := *newTier, *oldTier
	return &SponsorsTransition{
		Sponsorship:  cloneSponsorship(&updated),
		Previous:     previous,
		Activity:     activity,
		Invoice:      invoice,
		Tier:         &newTierCopy,
		PreviousTier: &oldTierCopy,
	}, nil
}

// ListingIDOrZero is nil-safe so a tier lookup miss reads as "no listing"
// rather than panicking in the change path.
func (t *SponsorsTier) ListingIDOrZero() int {
	if t == nil {
		return 0
	}
	return t.ListingID
}

// UpdateSponsorshipPreferences changes a sponsorship's privacy level and
// email preference.
func (ss *SponsorsStore) UpdateSponsorshipPreferences(sponsorshipID int, privacy string, receiveEmails bool) (*SponsorsTransition, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	s := ss.sponsorships[sponsorshipID]
	if s == nil {
		return nil, fmt.Errorf("sponsorship not found")
	}
	if privacy != SponsorshipPrivacyPrivate {
		privacy = SponsorshipPrivacyPublic
	}
	previous := cloneSponsorship(s)
	updated := *s
	if updated.NextBillingDate != nil {
		t := *updated.NextBillingDate
		updated.NextBillingDate = &t
	}
	updated.PrivacyLevel = privacy
	updated.IsSponsorOptedIntoEmail = receiveEmails
	updated.UpdatedAt = ss.now()
	b := ss.batch()
	b.Put(sponsorshipsBucket, strconv.Itoa(updated.ID), &updated)
	ss.commit(b, sponsorshipsBucket)
	ss.sponsorships[updated.ID] = &updated
	var tier *SponsorsTier
	if t := ss.tiers[updated.TierID]; t != nil {
		c := *t
		tier = &c
	}
	return &SponsorsTransition{Sponsorship: cloneSponsorship(&updated), Previous: previous, Tier: tier}, nil
}

// CancelSponsorship ends a sponsorship. A recurring one stops at the end of the
// paid-for period; a one-time payment cancels immediately.
func (ss *SponsorsStore) CancelSponsorship(sponsorshipID int) (*SponsorsTransition, error) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	s := ss.sponsorships[sponsorshipID]
	if s == nil || !s.IsActive {
		return nil, fmt.Errorf("sponsorship not found")
	}
	now := ss.now()
	previous := cloneSponsorship(s)
	updated := *s
	if updated.NextBillingDate != nil {
		t := *updated.NextBillingDate
		updated.NextBillingDate = &t
	}
	tier := ss.tiers[s.TierID]

	b := ss.batch()
	var activity *SponsorsActivity
	if s.IsOneTimePayment || s.NextBillingDate == nil || !s.NextBillingDate.After(now) {
		updated.IsActive = false
		updated.PendingCancellation = false
		updated.PendingTierID = 0
		updated.PendingEffectiveDate = nil
		updated.NextBillingDate = nil
		updated.CancelledAt = &now
		activity = ss.newActivityLocked(SponsorsActivityCancelledSponsorship, &updated, tier, nil, now)
	} else {
		effective := *s.NextBillingDate
		updated.PendingCancellation = true
		updated.PendingTierID = 0
		updated.PendingEffectiveDate = &effective
		activity = ss.newActivityLocked(SponsorsActivityPendingChange, &updated, tier, nil, now)
	}
	updated.UpdatedAt = now
	b.Put(sponsorshipsBucket, strconv.Itoa(updated.ID), &updated)
	b.Put(sponsorsActivityBucket, strconv.Itoa(activity.ID), activity)
	ss.commit(b, sponsorshipsBucket)
	ss.sponsorships[updated.ID] = &updated
	ss.activities[activity.ID] = activity
	ss.nextActivityID++

	var tierCopy *SponsorsTier
	if tier != nil {
		c := *tier
		tierCopy = &c
	}
	return &SponsorsTransition{
		Sponsorship: cloneSponsorship(&updated),
		Previous:    previous,
		Activity:    activity,
		Tier:        tierCopy,
	}, nil
}

// AdvanceSponsorshipBillingCycles rolls every recurring sponsorship whose next
// billing date has arrived (pending cancellation ends it, pending tier change
// applies, otherwise bills another period) and returns one transition each.
func (ss *SponsorsStore) AdvanceSponsorshipBillingCycles(now time.Time) []*SponsorsTransition {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	now = now.UTC()

	due := make([]*Sponsorship, 0)
	for _, s := range ss.sponsorships {
		if !s.IsActive || s.IsOneTimePayment || s.NextBillingDate == nil || s.NextBillingDate.After(now) {
			continue
		}
		due = append(due, s)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].ID < due[j].ID })

	out := make([]*SponsorsTransition, 0, len(due))
	b := ss.batch()
	staged := make([]*SponsorsTransition, 0, len(due))
	for _, s := range due {
		listing := ss.listings[ss.listingIDForAccountLocked(s.SponsorableLogin)]
		if listing == nil {
			continue
		}
		previous := cloneSponsorship(s)
		updated := *s
		periodStart := *s.NextBillingDate
		oldTier := ss.tiers[s.TierID]

		switch {
		case s.PendingCancellation:
			updated.IsActive = false
			updated.PendingCancellation = false
			updated.PendingEffectiveDate = nil
			updated.NextBillingDate = nil
			cancelled := periodStart
			updated.CancelledAt = &cancelled
			updated.UpdatedAt = now
			activity := ss.newActivityLocked(SponsorsActivityCancelledSponsorship, &updated, oldTier, nil, periodStart)
			b.Put(sponsorshipsBucket, strconv.Itoa(updated.ID), &updated)
			b.Put(sponsorsActivityBucket, strconv.Itoa(activity.ID), activity)
			ss.activities[activity.ID] = activity
			ss.nextActivityID++
			staged = append(staged, &SponsorsTransition{
				Sponsorship: &updated, Previous: previous, Activity: activity, PreviousTier: cloneTier(oldTier),
			})
		default:
			newTier := oldTier
			if s.PendingTierID != 0 {
				if pending := ss.tiers[s.PendingTierID]; pending != nil {
					newTier = pending
					updated.TierID = pending.ID
					updated.AmountInCents = pending.MonthlyPriceInCents
					updated.TierSelectedAt = periodStart
				}
				updated.PendingTierID = 0
				updated.PendingEffectiveDate = nil
			}
			periodEnd := nextSponsorsBillingDate(periodStart)
			for !periodEnd.After(now) {
				periodEnd = nextSponsorsBillingDate(periodEnd)
			}
			next := periodEnd
			updated.NextBillingDate = &next
			updated.UpdatedAt = now
			invoice := ss.newInvoiceLocked(&updated, listing, updated.AmountInCents, periodStart, periodEnd, false, false)
			action := SponsorsActivityNewSponsorship
			if newTier != oldTier {
				action = SponsorsActivityTierChange
			}
			activity := ss.newActivityLocked(action, &updated, newTier, oldTier, periodStart)
			b.Put(sponsorshipsBucket, strconv.Itoa(updated.ID), &updated)
			b.Put(sponsorsInvoicesBucket, strconv.Itoa(invoice.ID), invoice)
			b.Put(sponsorsActivityBucket, strconv.Itoa(activity.ID), activity)
			ss.invoices[invoice.ID] = invoice
			ss.nextInvoiceID++
			ss.activities[activity.ID] = activity
			ss.nextActivityID++
			staged = append(staged, &SponsorsTransition{
				Sponsorship: &updated, Previous: previous, Activity: activity, Invoice: invoice,
				Tier: cloneTier(newTier), PreviousTier: cloneTier(oldTier),
			})
		}
	}
	if len(staged) == 0 {
		return out
	}
	ss.commit(b, sponsorshipsBucket)
	for _, t := range staged {
		ss.sponsorships[t.Sponsorship.ID] = t.Sponsorship
		out = append(out, &SponsorsTransition{
			Sponsorship: cloneSponsorship(t.Sponsorship), Previous: t.Previous, Activity: t.Activity,
			Invoice: t.Invoice, Tier: t.Tier, PreviousTier: t.PreviousTier,
		})
	}
	return out
}

func cloneTier(t *SponsorsTier) *SponsorsTier {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func (ss *SponsorsStore) listingIDForAccountLocked(login string) int {
	if l := ss.listingForAccountLocked(login); l != nil {
		return l.ID
	}
	return 0
}

// GetSponsorship returns a sponsorship by database id.
func (ss *SponsorsStore) GetSponsorship(id int) *Sponsorship {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return cloneSponsorship(ss.sponsorships[id])
}

// FindSponsorshipByNodeID returns the live sponsorship row.
func (ss *SponsorsStore) FindSponsorshipByNodeID(nodeID string) *Sponsorship {
	id, ok := parseSponsorsNodeID(SponsorshipNodeIDPrefix, nodeID)
	if !ok {
		return nil
	}
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return ss.sponsorships[id]
}

// FindSponsorsActivityByNodeID returns the live activity row.
func (ss *SponsorsStore) FindSponsorsActivityByNodeID(nodeID string) *SponsorsActivity {
	id, ok := parseSponsorsNodeID(SponsorsActivityNodeIDPrefix, nodeID)
	if !ok {
		return nil
	}
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return ss.activities[id]
}

// GetSponsorshipBetween returns the sponsorship from sponsorLogin to
// sponsorableLogin, optionally only while it is still active.
func (ss *SponsorsStore) GetSponsorshipBetween(sponsorLogin, sponsorableLogin string, activeOnly bool) *Sponsorship {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return cloneSponsorship(ss.sponsorshipKeyLocked(sponsorLogin, sponsorableLogin, activeOnly))
}

// ListSponsorshipsAsMaintainer returns the sponsorships funding the
// sponsorable, newest first.
func (ss *SponsorsStore) ListSponsorshipsAsMaintainer(sponsorableLogin string, activeOnly bool) []*Sponsorship {
	return ss.listSponsorships(func(s *Sponsorship) bool {
		return strings.EqualFold(s.SponsorableLogin, sponsorableLogin)
	}, activeOnly)
}

// ListSponsorshipsAsSponsor returns the sponsorships the account funds.
func (ss *SponsorsStore) ListSponsorshipsAsSponsor(sponsorLogin string, activeOnly bool) []*Sponsorship {
	return ss.listSponsorships(func(s *Sponsorship) bool {
		return strings.EqualFold(s.SponsorLogin, sponsorLogin)
	}, activeOnly)
}

// ListSponsorshipsForTier returns the sponsorships currently on a tier.
func (ss *SponsorsStore) ListSponsorshipsForTier(tierID int, activeOnly bool) []*Sponsorship {
	return ss.listSponsorships(func(s *Sponsorship) bool { return s.TierID == tierID }, activeOnly)
}

func (ss *SponsorsStore) listSponsorships(match func(*Sponsorship) bool, activeOnly bool) []*Sponsorship {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	out := make([]*Sponsorship, 0)
	for _, s := range ss.sponsorships {
		if !match(s) || (activeOnly && !s.IsActive) {
			continue
		}
		out = append(out, cloneSponsorship(s))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// ListSponsorsActivities returns a sponsorable's activity feed, newest
// first. includeAsSponsor also returns the events where the account was
// the sponsor rather than the recipient.
func (ss *SponsorsStore) ListSponsorsActivities(login string, includeAsSponsor bool) []*SponsorsActivity {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	out := make([]*SponsorsActivity, 0)
	for _, a := range ss.activities {
		matches := strings.EqualFold(a.SponsorableLogin, login) ||
			(includeAsSponsor && strings.EqualFold(a.SponsorLogin, login))
		if !matches {
			continue
		}
		c := *a
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.After(out[j].Timestamp)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// money — every figure derived from the invoice ledger, in integer cents

// ListSponsorsInvoices returns the invoices billed for a listing, newest
// first.
func (ss *SponsorsStore) ListSponsorsInvoices(listingID int) []*SponsorsInvoice {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	out := make([]*SponsorsInvoice, 0)
	for _, i := range ss.invoices {
		if i.ListingID != listingID {
			continue
		}
		c := *i
		out = append(out, &c)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.After(out[b].CreatedAt)
		}
		return out[a].ID > out[b].ID
	})
	return out
}

// ListSponsorsInvoicesForSponsor returns what an account has been billed.
func (ss *SponsorsStore) ListSponsorsInvoicesForSponsor(sponsorLogin string) []*SponsorsInvoice {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	out := make([]*SponsorsInvoice, 0)
	for _, i := range ss.invoices {
		if !strings.EqualFold(i.SponsorLogin, sponsorLogin) {
			continue
		}
		c := *i
		out = append(out, &c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// SponsorLifetimeValue is one row of lifetimeReceivedSponsorshipValues.
type SponsorLifetimeValue struct {
	SponsorLogin     string
	SponsorType      string
	SponsorableLogin string
	AmountInCents    int
}

// LifetimeReceivedSponsorshipValues totals, per sponsor, everything ever billed
// for this sponsorable. Refunded invoices are excluded.
func (ss *SponsorsStore) LifetimeReceivedSponsorshipValues(sponsorableLogin string) []*SponsorLifetimeValue {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	totals := map[string]int{}
	types := map[string]string{}
	for _, i := range ss.invoices {
		if !strings.EqualFold(i.SponsorableLogin, sponsorableLogin) || i.Status != "paid" {
			continue
		}
		totals[i.SponsorLogin] += i.AmountInCents
	}
	for _, s := range ss.sponsorships {
		if strings.EqualFold(s.SponsorableLogin, sponsorableLogin) {
			types[s.SponsorLogin] = s.SponsorType
		}
	}
	out := make([]*SponsorLifetimeValue, 0, len(totals))
	for login, cents := range totals {
		out = append(out, &SponsorLifetimeValue{
			SponsorLogin: login, SponsorType: types[login],
			SponsorableLogin: sponsorableLogin, AmountInCents: cents,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].SponsorLogin) < strings.ToLower(out[j].SponsorLogin)
	})
	return out
}

// MonthlyEstimatedSponsorsIncomeInCents sums every active recurring
// sponsorship's locked-in amount, with pending downgrades applied (they take
// effect before the next payout).
func (ss *SponsorsStore) MonthlyEstimatedSponsorsIncomeInCents(sponsorableLogin string) int {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	total := 0
	for _, s := range ss.sponsorships {
		if !strings.EqualFold(s.SponsorableLogin, sponsorableLogin) || !s.IsActive || s.IsOneTimePayment {
			continue
		}
		if s.PendingCancellation {
			continue
		}
		amount := s.AmountInCents
		if s.PendingTierID != 0 {
			if pending := ss.tiers[s.PendingTierID]; pending != nil {
				amount = pending.MonthlyPriceInCents
			}
		}
		total += amount
	}
	return total
}

// EstimatedNextSponsorsPayoutInCents is the money already billed for this
// sponsorable that has not yet been rolled into a payout.
func (ss *SponsorsStore) EstimatedNextSponsorsPayoutInCents(sponsorableLogin string) int {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	total := 0
	for _, i := range ss.invoices {
		if !strings.EqualFold(i.SponsorableLogin, sponsorableLogin) || i.Status != "paid" || i.PayoutID != 0 {
			continue
		}
		total += i.AmountInCents
	}
	return total
}

// TotalSponsorshipAmountAsSponsorInCents is what an account has spent
// funding sponsorships, optionally filtered by window and recipient.
func (ss *SponsorsStore) TotalSponsorshipAmountAsSponsorInCents(sponsorLogin string, since, until *time.Time, sponsorableLogins []string) int {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	wanted := map[string]bool{}
	for _, l := range sponsorableLogins {
		wanted[strings.ToLower(l)] = true
	}
	total := 0
	for _, i := range ss.invoices {
		if !strings.EqualFold(i.SponsorLogin, sponsorLogin) || i.Status != "paid" {
			continue
		}
		if len(wanted) > 0 && !wanted[strings.ToLower(i.SponsorableLogin)] {
			continue
		}
		if since != nil && i.CreatedAt.Before(*since) {
			continue
		}
		if until != nil && !i.CreatedAt.Before(*until) {
			continue
		}
		total += i.AmountInCents
	}
	return total
}

// RunSponsorsPayout rolls every unpaid invoice for a listing into one payout
// and advances its next payout date, or nil when nothing is owed.
func (ss *SponsorsStore) RunSponsorsPayout(listingID int, now time.Time) *SponsorsPayout {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	listing := ss.listings[listingID]
	if listing == nil {
		return nil
	}
	now = now.UTC()
	amount := 0
	// periodEnd widens to the latest claimed invoice; periodStart is set from
	// the first claimed invoice below.
	periodEnd := now
	claimed := make([]*SponsorsInvoice, 0)
	for _, i := range ss.invoices {
		if i.ListingID != listingID || i.Status != "paid" || i.PayoutID != 0 {
			continue
		}
		claimed = append(claimed, i)
	}
	if len(claimed) == 0 {
		return nil
	}
	sort.Slice(claimed, func(a, b int) bool { return claimed[a].ID < claimed[b].ID })
	periodStart := claimed[0].CreatedAt
	for _, i := range claimed {
		amount += i.AmountInCents
		if i.CreatedAt.Before(periodStart) {
			periodStart = i.CreatedAt
		}
		if i.CreatedAt.After(periodEnd) {
			periodEnd = i.CreatedAt
		}
	}
	payout := &SponsorsPayout{
		ID: ss.nextPayoutID, ListingID: listingID, AmountInCents: amount,
		PeriodStart: periodStart, PeriodEnd: periodEnd, Status: "paid",
		ScheduledDate: listing.NextPayoutDate, CreatedAt: now,
	}
	updatedListing := *listing
	if listing.ActiveGoal != nil {
		g := *listing.ActiveGoal
		updatedListing.ActiveGoal = &g
	}
	updatedListing.NextPayoutDate = SponsorsNextPayoutDate(now)
	updatedListing.UpdatedAt = now

	b := ss.batch()
	b.Put(sponsorsPayoutsBucket, strconv.Itoa(payout.ID), payout)
	b.Put(sponsorsListingsBucket, strconv.Itoa(updatedListing.ID), &updatedListing)
	settled := make([]*SponsorsInvoice, 0, len(claimed))
	for _, i := range claimed {
		moved := *i
		moved.PayoutID = payout.ID
		settled = append(settled, &moved)
		b.Put(sponsorsInvoicesBucket, strconv.Itoa(moved.ID), &moved)
	}
	ss.commit(b, sponsorsPayoutsBucket)

	ss.payouts[payout.ID] = payout
	ss.nextPayoutID++
	ss.listings[updatedListing.ID] = &updatedListing
	for _, i := range settled {
		ss.invoices[i.ID] = i
	}
	c := *payout
	return &c
}

// ListSponsorsPayouts returns a listing's payout history, newest first.
func (ss *SponsorsStore) ListSponsorsPayouts(listingID int) []*SponsorsPayout {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	out := make([]*SponsorsPayout, 0)
	for _, p := range ss.payouts {
		if p.ListingID != listingID {
			continue
		}
		c := *p
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// SponsorsGoalProgress reports a listing's goal progress as an integer percent
// clamped to 0..100.
func (ss *SponsorsStore) SponsorsGoalProgress(listingID int) (kind string, target, percent int, ok bool) {
	ss.Mu.RLock()
	listing := ss.listings[listingID]
	if listing == nil || listing.ActiveGoal == nil {
		ss.Mu.RUnlock()
		return "", 0, 0, false
	}
	goal := *listing.ActiveGoal
	login := listing.SponsorableLogin
	current := 0
	switch goal.Kind {
	case SponsorsGoalTotalSponsors:
		for _, s := range ss.sponsorships {
			if strings.EqualFold(s.SponsorableLogin, login) && s.IsActive {
				current++
			}
		}
	default:
		for _, s := range ss.sponsorships {
			if strings.EqualFold(s.SponsorableLogin, login) && s.IsActive && !s.IsOneTimePayment {
				current += s.AmountInCents
			}
		}
	}
	ss.Mu.RUnlock()

	if goal.TargetValue <= 0 {
		return goal.Kind, goal.TargetValue, 0, true
	}
	percent = current * 100 / goal.TargetValue
	if percent > 100 {
		percent = 100
	}
	return goal.Kind, goal.TargetValue, percent, true
}

// ListInstallationsForTarget returns every App installation on the account, to
// fan an account-scoped event out to the apps that subscribed.
func (st *Store) ListInstallationsForTarget(login string) []*Installation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*Installation, 0)
	for _, inst := range st.Installations {
		if strings.EqualFold(inst.TargetLogin, login) {
			out = append(out, CloneInstallation(inst))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
