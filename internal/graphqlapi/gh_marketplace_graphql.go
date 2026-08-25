package graphqlapi

// GitHub Marketplace — the GraphQL read surface: the MarketplaceListing and
// MarketplaceCategory types and the four root fields that reach them.
//
// A listing's identity, plans and purchases live in the store's Marketplace
// billing tables; its taxonomy, marketing links and verification state live
// in the profile store. This file joins the two into the one type GitHub
// publishes, and answers every viewer* field from the same publisher and
// purchase facts the REST surface enforces on.

import (
	"context"
	"fmt"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

type marketplaceTypeRegistry struct {
	listing    *graphql.Object
	category   *graphql.Object
	connection *graphql.Object
}

func (s *Resolver) marketplaceTypes() *marketplaceTypeRegistry {
	if s.graphqlTypes.marketplace == nil {
		s.graphqlTypes.marketplace = &marketplaceTypeRegistry{}
	}
	return s.graphqlTypes.marketplace
}

// ---------------------------------------------------------------------------
// rendering

// marketplaceProfileFor returns the listing's stored profile, or the
// synthesized default when its publisher has not filled one in. A read
// never writes one (STORE-034).
func (s *Resolver) marketplaceProfileFor(slug string) *store.MarketplaceListingProfile {
	if profile := s.store.MarketplaceProfiles.GetMarketplaceListingProfile(slug); profile != nil {
		return profile
	}
	return store.DefaultMarketplaceListingProfile(slug)
}

func (s *Resolver) marketplaceCategoryGQL(category *store.MarketplaceCategory) map[string]interface{} {
	if category == nil {
		return nil
	}
	primary, secondary := s.marketplaceCategoryCounts()
	path := "/marketplace/category/" + category.Slug
	return map[string]interface{}{
		"__typename":            "MarketplaceCategory",
		"_dbID":                 category.ID,
		"nodeID":                category.NodeID,
		"id":                    category.NodeID,
		"slug":                  category.Slug,
		"name":                  category.Name,
		"description":           nullableString(category.Description),
		"howItWorks":            nullableString(category.HowItWorks),
		"primaryListingCount":   primary[category.Slug],
		"secondaryListingCount": secondary[category.Slug],
		"resourcePath":          path,
		"url":                   externalURL(path),
	}
}

// marketplaceCategoryCounts counts published listings per category.
func (s *Resolver) marketplaceCategoryCounts() (primary, secondary map[string]int) {
	primary, secondary = map[string]int{}, map[string]int{}
	for _, listing := range s.store.ListMarketplaceListings(true) {
		profile := s.marketplaceProfileFor(listing.Slug)
		primary[profile.PrimaryCategorySlug]++
		if profile.SecondaryCategorySlug != "" {
			secondary[profile.SecondaryCategorySlug]++
		}
	}
	return primary, secondary
}

// marketplaceListingGQL renders a listing joined with its profile. The
// viewer-scoped fields resolve lazily off the slug so one rendering serves
// every viewer.
func (s *Resolver) marketplaceListingGQL(listing *store.MarketplaceListing) map[string]interface{} {
	if listing == nil {
		return nil
	}
	profile := s.marketplaceProfileFor(listing.Slug)
	plans := s.store.ListMarketplacePlans(listing.Slug, true)
	paid, freeTrials := false, false
	for _, plan := range plans {
		if plan.MonthlyPriceInCents > 0 || plan.YearlyPriceInCents > 0 {
			paid = true
		}
		if plan.HasFreeTrial {
			freeTrials = true
		}
	}
	path := "/marketplace/" + listing.Slug
	configure := path + "/order"
	screenshots := make([]interface{}, 0, len(profile.ScreenshotURLs))
	for _, url := range profile.ScreenshotURLs {
		screenshots = append(screenshots, url)
	}
	shortDescription := listing.Description
	normalized := profile.NormalizedShortDesc
	if normalized == "" {
		normalized = strings.TrimSpace(shortDescription)
	}
	return map[string]interface{}{
		"__typename":                          "MarketplaceListing",
		"_slug":                               listing.Slug,
		"_dbID":                               listing.Slug,
		"nodeID":                              store.MarketplaceListingNodeID(listing.Slug),
		"id":                                  store.MarketplaceListingNodeID(listing.Slug),
		"slug":                                listing.Slug,
		"name":                                listing.Name,
		"shortDescription":                    shortDescription,
		"normalizedShortDescription":          normalized,
		"fullDescription":                     listing.FullDescription,
		"fullDescriptionHTML":                 "<p>" + listing.FullDescription + "</p>",
		"extendedDescription":                 nullableString(profile.ExtendedDescription),
		"extendedDescriptionHTML":             "<p>" + profile.ExtendedDescription + "</p>",
		"howItWorks":                          nullableString(profile.HowItWorks),
		"howItWorksHTML":                      "<p>" + profile.HowItWorks + "</p>",
		"resourcePath":                        path,
		"url":                                 externalURL(path),
		"configurationResourcePath":           configure,
		"configurationUrl":                    externalURL(configure),
		"installationUrl":                     nullableString(listing.InstallationURL),
		"companyUrl":                          nullableString(profile.CompanyURL),
		"documentationUrl":                    nullableString(profile.DocumentationURL),
		"pricingUrl":                          nullableString(profile.PricingURL),
		"privacyPolicyUrl":                    marketplaceURLOrDefault(profile.PrivacyPolicyURL, path+"/privacy"),
		"supportUrl":                          marketplaceURLOrDefault(profile.SupportURL, path+"/support"),
		"statusUrl":                           nullableString(profile.StatusURL),
		"supportEmail":                        nullableString(profile.SupportEmail),
		"termsOfServiceUrl":                   nullableString(profile.TermsOfServiceURL),
		"hasTermsOfService":                   profile.TermsOfServiceURL != "",
		"hasVerifiedOwner":                    profile.HasVerifiedOwner,
		"hasPublishedFreeTrialPlans":          freeTrials,
		"isPaid":                              paid,
		"isPublic":                            listing.Published,
		"logoBackgroundColor":                 profile.LogoBackgroundColor,
		"_logoUrl":                            profile.LogoURL,
		"screenshotUrls":                      screenshots,
		"isDraft":                             profile.State == store.MarketplaceListingDraft,
		"isUnverified":                        profile.State == store.MarketplaceListingUnverified,
		"isUnverifiedPending":                 profile.State == store.MarketplaceListingUnverifiedPending,
		"isVerificationPendingFromDraft":      profile.State == store.MarketplaceListingVerificationPendingFromDraft,
		"isVerificationPendingFromUnverified": profile.State == store.MarketplaceListingVerificationPendingFromUnverified,
		"isVerified":                          profile.State == store.MarketplaceListingVerified,
		"isRejected":                          profile.State == store.MarketplaceListingRejected,
		"isArchived":                          profile.State == store.MarketplaceListingArchived,
		"_primaryCategorySlug":                profile.PrimaryCategorySlug,
		"_secondaryCategorySlug":              profile.SecondaryCategorySlug,
	}
}

func marketplaceURLOrDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return externalURL(fallback)
}

// ---------------------------------------------------------------------------
// viewer-scoped facts

// viewerAdminsMarketplaceListing reports whether the viewer publishes the
// app behind the listing. Every listing-management viewer* field answers
// from it, so the GraphQL surface and the settings surface agree on who a
// listing belongs to.
func (s *Resolver) viewerAdminsMarketplaceListing(ctx context.Context, slug string) bool {
	viewer := s.ghUserFromContext(ctx)
	if viewer == nil {
		return false
	}
	listing := s.store.GetMarketplaceListing(slug)
	if listing == nil {
		return false
	}
	if viewer.SiteAdmin {
		return true
	}
	if listing.GitHubAppID != 0 {
		app := s.store.GetApp(listing.GitHubAppID)
		return app != nil && app.OwnerID == viewer.ID
	}
	if listing.OAuthAppClientID != "" {
		app := s.store.GetOAuthApp(listing.OAuthAppClientID)
		return app != nil && app.OwnerID == viewer.ID
	}
	return false
}

// viewerMarketplaceAccounts is the set of accounts the viewer buys for:
// themselves plus every organization they belong to.
func (s *Resolver) viewerMarketplaceAccounts(ctx context.Context) []store.MarketplaceBuyerAccount {
	viewer := s.ghUserFromContext(ctx)
	if viewer == nil {
		return nil
	}
	accounts := []store.MarketplaceBuyerAccount{{Id: viewer.ID, Login: viewer.Login, AccountType: "User"}}
	for _, org := range s.store.ListOrgsByUser(viewer.ID) {
		accounts = append(accounts, store.MarketplaceBuyerAccount{Id: org.ID, Login: org.Login, AccountType: "Organization"})
	}
	return accounts
}

func (s *Resolver) viewerHasPurchasedMarketplaceListing(ctx context.Context, slug string, forAllOrganizations bool) bool {
	accounts := s.viewerMarketplaceAccounts(ctx)
	if len(accounts) == 0 {
		return false
	}
	organizations, purchasedOrgs, purchasedAny := 0, 0, false
	for _, account := range accounts {
		purchased := s.store.GetMarketplacePurchase(slug, account.AccountType, account.Id) != nil
		if purchased {
			purchasedAny = true
		}
		if account.AccountType == "Organization" {
			organizations++
			if purchased {
				purchasedOrgs++
			}
		}
	}
	if !forAllOrganizations {
		return purchasedAny
	}
	return organizations > 0 && organizations == purchasedOrgs
}

// marketplaceListingInstalledForViewer reports whether the GitHub App
// behind the listing is installed on any account the viewer buys for.
func (s *Resolver) marketplaceListingInstalledForViewer(ctx context.Context, slug string) bool {
	listing := s.store.GetMarketplaceListing(slug)
	if listing == nil || listing.GitHubAppID == 0 {
		return false
	}
	wanted := map[string]bool{}
	for _, account := range s.viewerMarketplaceAccounts(ctx) {
		wanted[strings.ToLower(account.Login)] = true
	}
	for _, installation := range s.store.ListAppInstallations(listing.GitHubAppID) {
		if wanted[strings.ToLower(installation.TargetLogin)] {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// types

func (s *Resolver) marketplaceCategoryType() *graphql.Object {
	types := s.marketplaceTypes()
	if types.category != nil {
		return types.category
	}
	uri := s.graphQLStringScalar("URI")
	types.category = graphql.NewObject(graphql.ObjectConfig{
		Name:       "MarketplaceCategory",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.Fields{
			"id":                    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"name":                  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"slug":                  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description":           &graphql.Field{Type: graphql.String},
			"howItWorks":            &graphql.Field{Type: graphql.String},
			"primaryListingCount":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"secondaryListingCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"resourcePath":          &graphql.Field{Type: graphql.NewNonNull(uri)},
			"url":                   &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
	})
	return types.category
}

// marketplaceViewerField builds a Boolean! field answered by the given
// predicate over the listing's slug.
func (s *Resolver) marketplaceViewerField(answer func(context.Context, string) bool) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return answer(p.Context, sponsorsSourceString(p.Source, "_slug")), nil
		},
	}
}

func (s *Resolver) marketplaceListingType() *graphql.Object {
	types := s.marketplaceTypes()
	if types.listing != nil {
		return types.listing
	}
	uri := s.graphQLStringScalar("URI")
	html := s.graphQLStringScalar("HTML")
	admin := func(ctx context.Context, slug string) bool { return s.viewerAdminsMarketplaceListing(ctx, slug) }
	types.listing = graphql.NewObject(graphql.ObjectConfig{
		Name:       "MarketplaceListing",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":                                  &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"name":                                &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"slug":                                &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"shortDescription":                    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"normalizedShortDescription":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"fullDescription":                     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"fullDescriptionHTML":                 &graphql.Field{Type: graphql.NewNonNull(html)},
				"extendedDescription":                 &graphql.Field{Type: graphql.String},
				"extendedDescriptionHTML":             &graphql.Field{Type: graphql.NewNonNull(html)},
				"howItWorks":                          &graphql.Field{Type: graphql.String},
				"howItWorksHTML":                      &graphql.Field{Type: graphql.NewNonNull(html)},
				"resourcePath":                        &graphql.Field{Type: graphql.NewNonNull(uri)},
				"url":                                 &graphql.Field{Type: graphql.NewNonNull(uri)},
				"configurationResourcePath":           &graphql.Field{Type: graphql.NewNonNull(uri)},
				"configurationUrl":                    &graphql.Field{Type: graphql.NewNonNull(uri)},
				"installationUrl":                     &graphql.Field{Type: uri},
				"companyUrl":                          &graphql.Field{Type: uri},
				"documentationUrl":                    &graphql.Field{Type: uri},
				"pricingUrl":                          &graphql.Field{Type: uri},
				"privacyPolicyUrl":                    &graphql.Field{Type: graphql.NewNonNull(uri)},
				"supportUrl":                          &graphql.Field{Type: graphql.NewNonNull(uri)},
				"statusUrl":                           &graphql.Field{Type: uri},
				"supportEmail":                        &graphql.Field{Type: graphql.String},
				"termsOfServiceUrl":                   &graphql.Field{Type: uri},
				"hasTermsOfService":                   &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"hasVerifiedOwner":                    &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"hasPublishedFreeTrialPlans":          &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isPaid":                              &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isPublic":                            &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isDraft":                             &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isUnverified":                        &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isUnverifiedPending":                 &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isVerificationPendingFromDraft":      &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isVerificationPendingFromUnverified": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isVerified":                          &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isRejected":                          &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isArchived":                          &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"logoBackgroundColor":                 &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"screenshotUrls":                      &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.String))},
				"logoUrl": &graphql.Field{
					Type: uri,
					Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 400}},
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						logo := sponsorsSourceString(p.Source, "_logoUrl")
						if logo == "" {
							return nil, nil
						}
						if size, ok := intArg(p.Args, "size"); ok && size > 0 {
							return fmt.Sprintf("%s?size=%d", logo, size), nil
						}
						return logo, nil
					},
				},
				"app": &graphql.Field{
					Type: s.gqlAppType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						listing := s.store.GetMarketplaceListing(sponsorsSourceString(p.Source, "_slug"))
						if listing == nil || listing.GitHubAppID == 0 {
							return nil, nil
						}
						return optionalRendered(s.store.GetApp(listing.GitHubAppID), appGQLSource), nil
					},
				},
				"primaryCategory": &graphql.Field{
					Type: graphql.NewNonNull(s.marketplaceCategoryType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						slug := sponsorsSourceString(p.Source, "_primaryCategorySlug")
						return optionalObject(s.marketplaceCategoryGQL(s.store.MarketplaceProfiles.GetMarketplaceCategory(slug, false))), nil
					},
				},
				"secondaryCategory": &graphql.Field{
					Type: s.marketplaceCategoryType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						slug := sponsorsSourceString(p.Source, "_secondaryCategorySlug")
						if slug == "" {
							return nil, nil
						}
						return optionalObject(s.marketplaceCategoryGQL(s.store.MarketplaceProfiles.GetMarketplaceCategory(slug, false))), nil
					},
				},
				// Listing administration is the publisher's; GitHub answers
				// every one of these false to anybody else.
				"viewerCanAddPlans":        s.marketplaceViewerField(admin),
				"viewerCanApprove":         s.marketplaceViewerField(admin),
				"viewerCanDelist":          s.marketplaceViewerField(admin),
				"viewerCanEdit":            s.marketplaceViewerField(admin),
				"viewerCanEditCategories":  s.marketplaceViewerField(admin),
				"viewerCanEditPlans":       s.marketplaceViewerField(admin),
				"viewerCanRedraft":         s.marketplaceViewerField(admin),
				"viewerCanReject":          s.marketplaceViewerField(admin),
				"viewerCanRequestApproval": s.marketplaceViewerField(admin),
				"viewerIsListingAdmin":     s.marketplaceViewerField(admin),
				"viewerHasPurchased": s.marketplaceViewerField(func(ctx context.Context, slug string) bool {
					return s.viewerHasPurchasedMarketplaceListing(ctx, slug, false)
				}),
				"viewerHasPurchasedForAllOrganizations": s.marketplaceViewerField(func(ctx context.Context, slug string) bool {
					return s.viewerHasPurchasedMarketplaceListing(ctx, slug, true)
				}),
				"installedForViewer": s.marketplaceViewerField(func(ctx context.Context, slug string) bool {
					return s.marketplaceListingInstalledForViewer(ctx, slug)
				}),
			}
		}),
	})
	return types.listing
}

func (s *Resolver) marketplaceListingConnectionType() *graphql.Object {
	types := s.marketplaceTypes()
	if types.connection != nil {
		return types.connection
	}
	types.connection = s.sponsorsConnectionType("MarketplaceListingConnection", s.marketplaceListingType(), nil)
	return types.connection
}

// ---------------------------------------------------------------------------
// root fields

// addMarketplaceFieldsToSchema installs the four Marketplace root fields
// and registers the two node types.
func (s *Resolver) addMarketplaceFieldsToSchema(queryType *graphql.Object, nodeTypes map[string]*graphql.Object) {
	listingType := s.marketplaceListingType()
	categoryType := s.marketplaceCategoryType()
	nodeTypes["MarketplaceListing"] = listingType
	nodeTypes["MarketplaceCategory"] = categoryType

	queryType.AddFieldConfig("marketplaceListing", &graphql.Field{
		Type: listingType,
		Args: graphql.FieldConfigArgument{
			"slug": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			slug, _ := p.Args["slug"].(string)
			listing := s.store.GetMarketplaceListing(slug)
			// An unpublished listing exists only for its publisher; to
			// anybody else it must read as absent rather than as hidden.
			if listing == nil || (!listing.Published && !s.viewerAdminsMarketplaceListing(p.Context, listing.Slug)) {
				return nil, nil
			}
			return s.marketplaceListingGQL(listing), nil
		},
	})

	queryType.AddFieldConfig("marketplaceListings", &graphql.Field{
		Type: graphql.NewNonNull(s.marketplaceListingConnectionType()),
		Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
			"adminId":             &graphql.ArgumentConfig{Type: graphql.ID},
			"allStates":           &graphql.ArgumentConfig{Type: graphql.Boolean},
			"categorySlug":        &graphql.ArgumentConfig{Type: graphql.String},
			"organizationId":      &graphql.ArgumentConfig{Type: graphql.ID},
			"primaryCategoryOnly": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
			"slugs":               &graphql.ArgumentConfig{Type: graphql.NewList(graphql.String)},
			"useTopicAliases":     &graphql.ArgumentConfig{Type: graphql.Boolean},
			"viewerCanAdmin":      &graphql.ArgumentConfig{Type: graphql.Boolean},
			"withFreeTrialsOnly":  &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveMarketplaceListings(p)
		},
	})

	queryType.AddFieldConfig("marketplaceCategory", &graphql.Field{
		Type: categoryType,
		Args: graphql.FieldConfigArgument{
			"slug":            &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"useTopicAliases": &graphql.ArgumentConfig{Type: graphql.Boolean},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			slug, _ := p.Args["slug"].(string)
			useAliases, _ := p.Args["useTopicAliases"].(bool)
			return optionalObject(s.marketplaceCategoryGQL(s.store.MarketplaceProfiles.GetMarketplaceCategory(slug, useAliases))), nil
		},
	})

	queryType.AddFieldConfig("marketplaceCategories", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(categoryType))),
		Args: graphql.FieldConfigArgument{
			"excludeEmpty":         &graphql.ArgumentConfig{Type: graphql.Boolean},
			"excludeSubcategories": &graphql.ArgumentConfig{Type: graphql.Boolean},
			"includeCategories":    &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			excludeEmpty, _ := p.Args["excludeEmpty"].(bool)
			excludeSubcategories, _ := p.Args["excludeSubcategories"].(bool)
			include := []string{}
			if raw, ok := p.Args["includeCategories"].([]interface{}); ok {
				for _, entry := range raw {
					if slug, ok := entry.(string); ok {
						include = append(include, slug)
					}
				}
			}
			primary, _ := s.marketplaceCategoryCounts()
			out := []map[string]interface{}{}
			for _, category := range s.store.MarketplaceProfiles.ListMarketplaceCategories(include, excludeSubcategories, excludeEmpty, primary) {
				out = append(out, s.marketplaceCategoryGQL(category))
			}
			return out, nil
		},
	})
}

func (s *Resolver) resolveMarketplaceListings(p graphql.ResolveParams) (interface{}, error) {
	allStates, _ := p.Args["allStates"].(bool)
	categorySlug, _ := p.Args["categorySlug"].(string)
	useAliases, _ := p.Args["useTopicAliases"].(bool)
	primaryOnly, _ := p.Args["primaryCategoryOnly"].(bool)
	freeTrialsOnly, _ := p.Args["withFreeTrialsOnly"].(bool)
	viewerCanAdminOnly, _ := p.Args["viewerCanAdmin"].(bool)

	wantedSlugs := map[string]bool{}
	if raw, ok := p.Args["slugs"].([]interface{}); ok {
		for _, entry := range raw {
			if slug, ok := entry.(string); ok && slug != "" {
				wantedSlugs[strings.ToLower(slug)] = true
			}
		}
	}
	category := ""
	if categorySlug != "" {
		resolved := s.store.MarketplaceProfiles.GetMarketplaceCategory(categorySlug, useAliases)
		if resolved == nil {
			return paginateGQLMaps(nil, p.Args), nil
		}
		category = resolved.Slug
	}
	// allStates asks for unpublished listings too, which only a listing's
	// own publisher may see.
	nodes := []map[string]interface{}{}
	for _, listing := range s.store.ListMarketplaceListings(false) {
		admin := s.viewerAdminsMarketplaceListing(p.Context, listing.Slug)
		if !listing.Published && !(allStates && admin) {
			continue
		}
		if viewerCanAdminOnly && !admin {
			continue
		}
		if len(wantedSlugs) > 0 && !wantedSlugs[listing.Slug] {
			continue
		}
		profile := s.marketplaceProfileFor(listing.Slug)
		if category != "" {
			matches := profile.PrimaryCategorySlug == category ||
				(!primaryOnly && profile.SecondaryCategorySlug == category)
			if !matches {
				continue
			}
		}
		if freeTrialsOnly {
			trial := false
			for _, plan := range s.store.ListMarketplacePlans(listing.Slug, true) {
				if plan.HasFreeTrial {
					trial = true
					break
				}
			}
			if !trial {
				continue
			}
		}
		nodes = append(nodes, s.marketplaceListingGQL(listing))
	}
	return paginateGQLMaps(nodes, p.Args), nil
}

// marketplaceNodeByID resolves a Marketplace global id for Query.node.
func (s *Resolver) marketplaceNodeByID(ctx context.Context, nodeID string) map[string]interface{} {
	if slug, ok := store.ParseMarketplaceListingNodeID(nodeID); ok {
		listing := s.store.GetMarketplaceListing(slug)
		if listing == nil || (!listing.Published && !s.viewerAdminsMarketplaceListing(ctx, listing.Slug)) {
			return nil
		}
		return s.marketplaceListingGQL(listing)
	}
	if category := s.store.MarketplaceProfiles.FindMarketplaceCategoryByNodeID(nodeID); category != nil {
		return s.marketplaceCategoryGQL(category)
	}
	return nil
}
