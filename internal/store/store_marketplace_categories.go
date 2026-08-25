package store

// GitHub Marketplace: the category taxonomy and the listing profile —
// the marketing metadata GitHub's GraphQL MarketplaceListing exposes
// (categories, logo, screenshots, policy and support links, and the
// verification state machine) that the REST plan/purchase surface in
// store_marketplace.go does not carry.
//
// It lives beside the existing Marketplace store rather than inside it:
// a purchase is billing state, a profile is publication state, and the
// two have different writers.
//
// STORE-021: every getter and List* here returns a detached snapshot.

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MarketplaceCategoryNodeIDPrefix / MarketplaceListingNodeIDPrefix are the
// global-id prefixes for the two Marketplace node types.
const (
	MarketplaceCategoryNodeIDPrefix = "MC_kgDO"
	MarketplaceListingNodeIDPrefix  = "ML_kgDO"
)

// Marketplace listing verification states, matching the is* flags on
// GitHub's MarketplaceListing type. Exactly one is true at a time.
const (
	MarketplaceListingDraft                             = "draft"
	MarketplaceListingUnverified                        = "unverified"
	MarketplaceListingUnverifiedPending                 = "unverified_pending"
	MarketplaceListingVerificationPendingFromDraft      = "verification_pending_from_draft"
	MarketplaceListingVerificationPendingFromUnverified = "verification_pending_from_unverified"
	MarketplaceListingVerified                          = "verified"
	MarketplaceListingRejected                          = "rejected"
	MarketplaceListingArchived                          = "archived"
)

// MarketplaceListingNodeID derives a listing's global id from its slug.
// The slug is the listing's identity in the REST surface and never
// changes, so the id is stable without a second counter — and a listing
// that has never been given a profile still has one.
func MarketplaceListingNodeID(slug string) string {
	return MarketplaceListingNodeIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(strings.ToLower(slug)))
}

// ParseMarketplaceListingNodeID recovers the slug a listing global id names.
func ParseMarketplaceListingNodeID(nodeID string) (string, bool) {
	if !strings.HasPrefix(nodeID, MarketplaceListingNodeIDPrefix) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(nodeID, MarketplaceListingNodeIDPrefix))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

// DefaultMarketplaceListingProfile is the profile a listing has before its
// publisher fills one in: filed under the catch-all category, in the draft
// state, with no marketing links. It is synthesized rather than written,
// so a GraphQL read never performs a durable write (STORE-034).
func DefaultMarketplaceListingProfile(slug string) *MarketplaceListingProfile {
	slug = strings.ToLower(slug)
	return &MarketplaceListingProfile{
		Slug:                slug,
		NodeID:              MarketplaceListingNodeID(slug),
		PrimaryCategorySlug: "utilities",
		LogoBackgroundColor: "#1a1a1a",
		State:               MarketplaceListingDraft,
		ScreenshotURLs:      []string{},
	}
}

// MarketplaceCategory is one entry in the Marketplace taxonomy.
type MarketplaceCategory struct {
	ID          int    `json:"id"`
	NodeID      string `json:"node_id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	HowItWorks  string `json:"how_it_works,omitempty"`
	// Topic aliases are the alternative slugs GitHub resolves a category
	// by when a client passes useTopicAliases.
	TopicAliases []string `json:"topic_aliases,omitempty"`
	// A subcategory names its parent; the taxonomy is one level deep.
	ParentSlug string `json:"parent_slug,omitempty"`
}

// MarketplaceListingProfile is the publication half of a Marketplace
// listing, keyed by the listing slug the REST surface already uses.
type MarketplaceListingProfile struct {
	Slug                  string    `json:"slug"`
	NodeID                string    `json:"node_id"`
	PrimaryCategorySlug   string    `json:"primary_category_slug"`
	SecondaryCategorySlug string    `json:"secondary_category_slug,omitempty"`
	ExtendedDescription   string    `json:"extended_description,omitempty"`
	HowItWorks            string    `json:"how_it_works,omitempty"`
	NormalizedShortDesc   string    `json:"normalized_short_description,omitempty"`
	LogoURL               string    `json:"logo_url,omitempty"`
	LogoBackgroundColor   string    `json:"logo_background_color,omitempty"`
	ScreenshotURLs        []string  `json:"screenshot_urls,omitempty"`
	CompanyURL            string    `json:"company_url,omitempty"`
	DocumentationURL      string    `json:"documentation_url,omitempty"`
	PricingURL            string    `json:"pricing_url,omitempty"`
	PrivacyPolicyURL      string    `json:"privacy_policy_url,omitempty"`
	StatusURL             string    `json:"status_url,omitempty"`
	SupportEmail          string    `json:"support_email,omitempty"`
	SupportURL            string    `json:"support_url,omitempty"`
	TermsOfServiceURL     string    `json:"terms_of_service_url,omitempty"`
	State                 string    `json:"state"`
	HasVerifiedOwner      bool      `json:"has_verified_owner"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// MarketplaceProfileStore holds the taxonomy and the listing profiles.
type MarketplaceProfileStore struct {
	Mu      sync.RWMutex `json:"-"`
	Persist *Persistence `json:"-"`
	clock   func() time.Time

	categories map[string]*MarketplaceCategory       // slug → category
	profiles   map[string]*MarketplaceListingProfile // listing slug → profile

	nextCategoryID int
}

// NewMarketplaceProfileStore builds an empty profile store.
func NewMarketplaceProfileStore(now func() time.Time) *MarketplaceProfileStore {
	return &MarketplaceProfileStore{
		clock:          now,
		categories:     map[string]*MarketplaceCategory{},
		profiles:       map[string]*MarketplaceListingProfile{},
		nextCategoryID: 1,
	}
}

func (ms *MarketplaceProfileStore) now() time.Time {
	if ms.clock != nil {
		return ms.clock().UTC()
	}
	return time.Now().UTC()
}

const (
	marketplaceCategoriesBucket = "marketplace_categories"
	marketplaceProfilesBucket   = "marketplace_listing_profiles"
)

// defaultMarketplaceCategories is GitHub's own Marketplace taxonomy. It is
// seeded once so `Query.marketplaceCategories` answers with the categories
// a client expects rather than an empty list, and so a listing always has
// a primary category to point at (the field is non-null on GitHub).
var defaultMarketplaceCategories = []MarketplaceCategory{
	{Slug: "api-management", Name: "API management", Description: "Manage, secure and monitor your APIs.", TopicAliases: []string{"api"}},
	{Slug: "chat", Name: "Chat", Description: "Bring your work into the conversation.", TopicAliases: []string{"chatops"}},
	{Slug: "code-quality", Name: "Code quality", Description: "Automate your code review and keep quality high."},
	{Slug: "code-review", Name: "Code review", Description: "Ensure your code meets quality standards before it merges."},
	{Slug: "continuous-integration", Name: "Continuous integration", Description: "Automatically build and test your code.", TopicAliases: []string{"ci"}},
	{Slug: "dependency-management", Name: "Dependency management", Description: "Keep your dependencies current and secure."},
	{Slug: "deployment", Name: "Deployment", Description: "Ship your code anywhere.", TopicAliases: []string{"cd"}},
	{Slug: "ides", Name: "IDEs", Description: "Integrate your editor with your repositories."},
	{Slug: "learning", Name: "Learning", Description: "Level up your skills without leaving your workflow."},
	{Slug: "localization", Name: "Localization", Description: "Translate your project for a global audience."},
	{Slug: "mobile", Name: "Mobile", Description: "Build, test and ship mobile applications."},
	{Slug: "monitoring", Name: "Monitoring", Description: "Know what your software is doing in production."},
	{Slug: "project-management", Name: "Project management", Description: "Organize your work and keep it moving."},
	{Slug: "publishing", Name: "Publishing", Description: "Publish documentation, packages and sites."},
	{Slug: "recently-added", Name: "Recently added", Description: "The newest apps in the Marketplace."},
	{Slug: "security", Name: "Security", Description: "Find and fix vulnerabilities before they ship."},
	{Slug: "support", Name: "Support", Description: "Help your users where they already are."},
	{Slug: "testing", Name: "Testing", Description: "Test everything, all the time."},
	{Slug: "utilities", Name: "Utilities", Description: "Small tools that make a big difference."},
}

// loadMarketplaceProfiles repopulates the taxonomy and profiles, seeding
// GitHub's category set the first time an instance starts.
func (st *Store) loadMarketplaceProfiles() error {
	ms := st.MarketplaceProfiles
	ms.Persist = st.Persist
	if err := st.loadBucket(marketplaceCategoriesBucket, func(raw []byte) error {
		var c MarketplaceCategory
		if err := LoadJSON(raw, &c); err != nil {
			return err
		}
		ms.categories[c.Slug] = &c
		if c.ID >= ms.nextCategoryID {
			ms.nextCategoryID = c.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket(marketplaceProfilesBucket, func(raw []byte) error {
		var p MarketplaceListingProfile
		if err := LoadJSON(raw, &p); err != nil {
			return err
		}
		ms.profiles[p.Slug] = &p
		return nil
	}); err != nil {
		return err
	}
	return ms.SeedDefaultCategories()
}

// SeedDefaultCategories installs GitHub's taxonomy, leaving any category
// that already exists alone.
func (ms *MarketplaceProfileStore) SeedDefaultCategories() error {
	ms.Mu.Lock()
	defer ms.Mu.Unlock()
	batch := NewPersistBatch(ms.Persist)
	for _, template := range defaultMarketplaceCategories {
		if ms.categories[template.Slug] != nil {
			continue
		}
		category := template
		category.ID = ms.nextCategoryID
		category.NodeID = fmt.Sprintf("%s%08d", MarketplaceCategoryNodeIDPrefix, category.ID)
		ms.nextCategoryID++
		ms.categories[category.Slug] = &category
		batch.Put(marketplaceCategoriesBucket, category.Slug, &category)
	}
	if err := batch.Commit(); err != nil {
		return fmt.Errorf("seed marketplace categories: %w", err)
	}
	return nil
}

func cloneMarketplaceCategory(c *MarketplaceCategory) *MarketplaceCategory {
	if c == nil {
		return nil
	}
	clone := *c
	clone.TopicAliases = append([]string(nil), c.TopicAliases...)
	return &clone
}

func cloneMarketplaceProfile(p *MarketplaceListingProfile) *MarketplaceListingProfile {
	if p == nil {
		return nil
	}
	clone := *p
	clone.ScreenshotURLs = append([]string(nil), p.ScreenshotURLs...)
	return &clone
}

// GetMarketplaceCategory resolves a category by slug, optionally through
// its topic aliases (GitHub's useTopicAliases argument).
func (ms *MarketplaceProfileStore) GetMarketplaceCategory(slug string, useTopicAliases bool) *MarketplaceCategory {
	ms.Mu.RLock()
	defer ms.Mu.RUnlock()
	slug = strings.ToLower(slug)
	if category := ms.categories[slug]; category != nil {
		return cloneMarketplaceCategory(category)
	}
	if !useTopicAliases {
		return nil
	}
	for _, category := range ms.categories {
		for _, alias := range category.TopicAliases {
			if strings.EqualFold(alias, slug) {
				return cloneMarketplaceCategory(category)
			}
		}
	}
	return nil
}

// FindMarketplaceCategoryByNodeID returns the live category row.
func (ms *MarketplaceProfileStore) FindMarketplaceCategoryByNodeID(nodeID string) *MarketplaceCategory {
	if !strings.HasPrefix(nodeID, MarketplaceCategoryNodeIDPrefix) {
		return nil
	}
	ms.Mu.RLock()
	defer ms.Mu.RUnlock()
	for _, category := range ms.categories {
		if category.NodeID == nodeID {
			return category
		}
	}
	return nil
}

// ListMarketplaceCategories returns the taxonomy ordered by name. The
// filters mirror Query.marketplaceCategories: includeCategories restricts
// to named slugs, excludeSubcategories drops anything with a parent, and
// excludeEmpty drops categories no listing points at (the caller supplies
// the per-slug listing counts, since listings live in the other store).
func (ms *MarketplaceProfileStore) ListMarketplaceCategories(includeCategories []string, excludeSubcategories, excludeEmpty bool, counts map[string]int) []*MarketplaceCategory {
	ms.Mu.RLock()
	defer ms.Mu.RUnlock()
	wanted := map[string]bool{}
	for _, slug := range includeCategories {
		wanted[strings.ToLower(slug)] = true
	}
	out := make([]*MarketplaceCategory, 0, len(ms.categories))
	for _, category := range ms.categories {
		if len(wanted) > 0 && !wanted[category.Slug] {
			continue
		}
		if excludeSubcategories && category.ParentSlug != "" {
			continue
		}
		if excludeEmpty && counts[category.Slug] == 0 {
			continue
		}
		out = append(out, cloneMarketplaceCategory(category))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetMarketplaceListingProfile returns a listing's publication metadata.
func (ms *MarketplaceProfileStore) GetMarketplaceListingProfile(slug string) *MarketplaceListingProfile {
	ms.Mu.RLock()
	defer ms.Mu.RUnlock()
	return cloneMarketplaceProfile(ms.profiles[strings.ToLower(slug)])
}

// FindMarketplaceListingProfileByNodeID returns the live profile row.
func (ms *MarketplaceProfileStore) FindMarketplaceListingProfileByNodeID(nodeID string) *MarketplaceListingProfile {
	slug, ok := ParseMarketplaceListingNodeID(nodeID)
	if !ok {
		return nil
	}
	ms.Mu.RLock()
	defer ms.Mu.RUnlock()
	return ms.profiles[slug]
}

// ListMarketplaceListingProfiles returns every profile, ordered by slug.
func (ms *MarketplaceProfileStore) ListMarketplaceListingProfiles() []*MarketplaceListingProfile {
	ms.Mu.RLock()
	defer ms.Mu.RUnlock()
	out := make([]*MarketplaceListingProfile, 0, len(ms.profiles))
	for _, profile := range ms.profiles {
		out = append(out, cloneMarketplaceProfile(profile))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// MarketplaceProfileUpdate is a sparse patch over a listing profile.
type MarketplaceProfileUpdate struct {
	PrimaryCategorySlug   *string
	SecondaryCategorySlug *string
	ExtendedDescription   *string
	HowItWorks            *string
	NormalizedShortDesc   *string
	LogoURL               *string
	LogoBackgroundColor   *string
	ScreenshotURLs        []string
	CompanyURL            *string
	DocumentationURL      *string
	PricingURL            *string
	PrivacyPolicyURL      *string
	StatusURL             *string
	SupportEmail          *string
	SupportURL            *string
	TermsOfServiceURL     *string
	State                 *string
	HasVerifiedOwner      *bool
}

// SaveMarketplaceListingProfile creates or patches a listing's profile.
// A listing with no profile yet gets one in the draft state, filed under
// the "utilities" catch-all so its primary category is never absent.
func (ms *MarketplaceProfileStore) SaveMarketplaceListingProfile(slug string, patch MarketplaceProfileUpdate) (*MarketplaceListingProfile, error) {
	ms.Mu.Lock()
	defer ms.Mu.Unlock()
	slug = strings.ToLower(slug)
	if slug == "" {
		return nil, fmt.Errorf("marketplace listing slug is required")
	}
	now := ms.now()
	existing := ms.profiles[slug]
	var profile MarketplaceListingProfile
	if existing == nil {
		profile = *DefaultMarketplaceListingProfile(slug)
		profile.CreatedAt = now
	} else {
		profile = *cloneMarketplaceProfile(existing)
	}
	if patch.PrimaryCategorySlug != nil {
		if ms.categories[strings.ToLower(*patch.PrimaryCategorySlug)] == nil {
			return nil, fmt.Errorf("marketplace category %q does not exist", *patch.PrimaryCategorySlug)
		}
		profile.PrimaryCategorySlug = strings.ToLower(*patch.PrimaryCategorySlug)
	}
	if patch.SecondaryCategorySlug != nil {
		value := strings.ToLower(*patch.SecondaryCategorySlug)
		if value != "" && ms.categories[value] == nil {
			return nil, fmt.Errorf("marketplace category %q does not exist", value)
		}
		profile.SecondaryCategorySlug = value
	}
	applyMarketplaceProfileStrings(&profile, patch)
	if patch.ScreenshotURLs != nil {
		profile.ScreenshotURLs = append([]string(nil), patch.ScreenshotURLs...)
	}
	if patch.State != nil {
		if !validMarketplaceListingState(*patch.State) {
			return nil, fmt.Errorf("marketplace listing state %q is not one of GitHub's", *patch.State)
		}
		profile.State = *patch.State
	}
	if patch.HasVerifiedOwner != nil {
		profile.HasVerifiedOwner = *patch.HasVerifiedOwner
	}
	profile.UpdatedAt = now

	batch := NewPersistBatch(ms.Persist)
	batch.Put(marketplaceProfilesBucket, slug, &profile)
	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("persist marketplace listing profile: %w", err)
	}
	ms.profiles[slug] = &profile
	return cloneMarketplaceProfile(&profile), nil
}

func applyMarketplaceProfileStrings(profile *MarketplaceListingProfile, patch MarketplaceProfileUpdate) {
	for _, field := range []struct {
		value  *string
		target *string
	}{
		{patch.ExtendedDescription, &profile.ExtendedDescription},
		{patch.HowItWorks, &profile.HowItWorks},
		{patch.NormalizedShortDesc, &profile.NormalizedShortDesc},
		{patch.LogoURL, &profile.LogoURL},
		{patch.LogoBackgroundColor, &profile.LogoBackgroundColor},
		{patch.CompanyURL, &profile.CompanyURL},
		{patch.DocumentationURL, &profile.DocumentationURL},
		{patch.PricingURL, &profile.PricingURL},
		{patch.PrivacyPolicyURL, &profile.PrivacyPolicyURL},
		{patch.StatusURL, &profile.StatusURL},
		{patch.SupportEmail, &profile.SupportEmail},
		{patch.SupportURL, &profile.SupportURL},
		{patch.TermsOfServiceURL, &profile.TermsOfServiceURL},
	} {
		if field.value != nil {
			*field.target = *field.value
		}
	}
}

func validMarketplaceListingState(state string) bool {
	switch state {
	case MarketplaceListingDraft, MarketplaceListingUnverified, MarketplaceListingUnverifiedPending,
		MarketplaceListingVerificationPendingFromDraft, MarketplaceListingVerificationPendingFromUnverified,
		MarketplaceListingVerified, MarketplaceListingRejected, MarketplaceListingArchived:
		return true
	}
	return false
}

// DeleteMarketplaceListingProfile drops a listing's profile when the
// listing itself is deleted.
func (ms *MarketplaceProfileStore) DeleteMarketplaceListingProfile(slug string) bool {
	ms.Mu.Lock()
	defer ms.Mu.Unlock()
	slug = strings.ToLower(slug)
	if ms.profiles[slug] == nil {
		return false
	}
	batch := NewPersistBatch(ms.Persist)
	batch.Delete(marketplaceProfilesBucket, slug)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: marketplaceProfilesBucket, Err: err})
	}
	delete(ms.profiles, slug)
	return true
}
