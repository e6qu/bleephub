package graphqlapi

// GitHub Sponsors — the GraphQL surface: the Sponsorable interface User
// and Organization both implement, the listing/tier/goal/featured-item
// object graph, the sponsorship and activity feeds, the newsletters, and
// the nine mutations GitHub publishes.
//
// Sponsors is a GraphQL-only product on real GitHub (there is no REST
// API), so this file is the client-facing contract. Every money field is
// integer United States cents read straight off the store's invoice
// ledger, and every privacy decision goes through one predicate:
// sponsorshipVisible.

import (
	"fmt"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// sponsorsTypeRegistry memoizes the Sponsors type graph. It hangs off the
// resolver's registry so the interface, the unions and the connections are
// each minted once per assembled schema.
type sponsorsTypeRegistry struct {
	sponsorable      *graphql.Interface
	sponsorUnion     *graphql.Union
	sponsorableItem  *graphql.Union
	featureableItem  *graphql.Union
	listing          *graphql.Object
	tier             *graphql.Object
	tierAdminInfo    *graphql.Object
	goal             *graphql.Object
	featuredItem     *graphql.Object
	sponsorship      *graphql.Object
	activity         *graphql.Object
	newsletter       *graphql.Object
	lifetimeValue    *graphql.Object
	connections      map[string]*graphql.Object
	orderInputs      map[string]*graphql.InputObject
	sponsorableTypes map[string]*graphql.Object
	featureableTypes map[string]*graphql.Object
	pendingUserType  *graphql.Object
	pendingOrgType   *graphql.Object
}

func (s *Resolver) sponsorsTypes() *sponsorsTypeRegistry {
	if s.graphqlTypes.sponsors == nil {
		s.graphqlTypes.sponsors = &sponsorsTypeRegistry{
			connections:      map[string]*graphql.Object{},
			orderInputs:      map[string]*graphql.InputObject{},
			sponsorableTypes: map[string]*graphql.Object{},
			featureableTypes: map[string]*graphql.Object{},
		}
	}
	return s.graphqlTypes.sponsors
}

// ---------------------------------------------------------------------------
// shared small types

func (s *Resolver) sponsorsOrderInput(name, fieldEnumName string, fieldValues ...string) *graphql.InputObject {
	types := s.sponsorsTypes()
	if input := types.orderInputs[name]; input != nil {
		return input
	}
	input := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: name,
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC"))},
			"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.graphQLEnum(fieldEnumName, fieldValues...))},
		},
	})
	types.orderInputs[name] = input
	return input
}

// sponsorsConnectionType mints a Relay connection (and its edge) over the
// node type, memoized by connection name.
func (s *Resolver) sponsorsConnectionType(name string, nodeType graphql.Output, extra graphql.Fields) *graphql.Object {
	types := s.sponsorsTypes()
	if conn := types.connections[name]; conn != nil {
		return conn
	}
	edge := graphql.NewObject(graphql.ObjectConfig{
		Name: strings.TrimSuffix(name, "Connection") + "Edge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: nodeType},
		},
	})
	fields := graphql.Fields{
		"edges":      &graphql.Field{Type: graphql.NewList(edge)},
		"nodes":      &graphql.Field{Type: graphql.NewList(nodeType)},
		"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	}
	for name, field := range extra {
		fields[name] = field
	}
	conn := graphql.NewObject(graphql.ObjectConfig{Name: name, Fields: fields})
	types.connections[name] = conn
	return conn
}

// ---------------------------------------------------------------------------
// account rendering

// sponsorsAccountGQL renders a login as the User or Organization map the
// Sponsor / Sponsorable abstract types dispatch on.
func (s *Resolver) sponsorsAccountGQL(login string) map[string]interface{} {
	if login == "" {
		return nil
	}
	if user := s.store.LookupUserByLogin(login); user != nil {
		out := userToGraphQL(user)
		out["__typename"] = "User"
		return out
	}
	if org := s.store.GetOrg(login); org != nil {
		out := orgToGraphQL(org)
		out["__typename"] = "Organization"
		return out
	}
	return nil
}

// sponsorsAccountLogin reads the login off a Sponsorable source map. Every
// Sponsorable field resolves through it, so a User source and an
// Organization source are handled by one code path.
func sponsorsAccountLogin(source interface{}) string {
	m, _ := source.(map[string]interface{})
	if m == nil {
		return ""
	}
	login, _ := m["login"].(string)
	return login
}

// resolveSponsorsAccount turns a global id into the account it names.
func (s *Resolver) resolveSponsorsAccount(nodeID string) (login string, ok bool) {
	id, kind, found := resolveProjectOwner(s.store, nodeID)
	if !found {
		return "", false
	}
	if kind == "User" {
		if user := s.store.GetUserByID(id); user != nil {
			return user.Login, true
		}
		return "", false
	}
	if org := s.store.GetOrgByID(id); org != nil {
		return org.Login, true
	}
	return "", false
}

// sponsorsAccountFromInput reads the (id, login) pair every Sponsors
// mutation input offers for an account, preferring the global id.
func (s *Resolver) sponsorsAccountFromInput(input map[string]interface{}, idKey, loginKey string) string {
	if nodeID, _ := input[idKey].(string); nodeID != "" {
		if login, ok := s.resolveSponsorsAccount(nodeID); ok {
			return login
		}
		return ""
	}
	login, _ := input[loginKey].(string)
	if login == "" {
		return ""
	}
	if resolved, ok := s.sponsorableExists(login); ok {
		return resolved
	}
	return ""
}

func (s *Resolver) sponsorableExists(login string) (string, bool) {
	if user := s.store.LookupUserByLogin(login); user != nil {
		return user.Login, true
	}
	if org := s.store.GetOrg(login); org != nil {
		return org.Login, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// privacy

// sponsorshipVisible is the one privacy predicate the whole Sponsors
// surface uses. A private sponsorship exists, for reading purposes, only
// for its sponsor, for the maintainer receiving it, and for a site
// administrator; to anybody else it must not even be countable.
func (s *Resolver) sponsorshipVisible(p graphql.ResolveParams, sponsorship *store.Sponsorship) bool {
	if sponsorship == nil {
		return false
	}
	if sponsorship.PrivacyLevel != store.SponsorshipPrivacyPrivate {
		return true
	}
	return s.viewerCanAdminAccount(p.Context, sponsorship.SponsorLogin) ||
		s.viewerCanAdminAccount(p.Context, sponsorship.SponsorableLogin)
}

func (s *Resolver) visibleSponsorships(p graphql.ResolveParams, in []*store.Sponsorship) []*store.Sponsorship {
	out := make([]*store.Sponsorship, 0, len(in))
	for _, sponsorship := range in {
		if s.sponsorshipVisible(p, sponsorship) {
			out = append(out, sponsorship)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// object rendering

func sponsorsTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (s *Resolver) sponsorsGoalGQL(listing *store.SponsorsListing) map[string]interface{} {
	if listing == nil || listing.ActiveGoal == nil {
		return nil
	}
	kind, target, percent, ok := s.store.Sponsors.SponsorsGoalProgress(listing.ID)
	if !ok {
		return nil
	}
	title := fmt.Sprintf("Earn $%d per month", target/100)
	if kind == store.SponsorsGoalTotalSponsors {
		title = fmt.Sprintf("Reach %d sponsors", target)
	}
	return map[string]interface{}{
		"description":     nullableString(listing.ActiveGoal.Description),
		"kind":            kind,
		"percentComplete": percent,
		"targetValue":     target,
		"title":           title,
	}
}

func (s *Resolver) sponsorsListingGQL(listing *store.SponsorsListing) map[string]interface{} {
	if listing == nil {
		return nil
	}
	path := "/sponsors/" + listing.Slug
	return map[string]interface{}{
		"__typename":            "SponsorsListing",
		"_listingID":            listing.ID,
		"_dbID":                 listing.ID,
		"nodeID":                listing.NodeID,
		"id":                    listing.NodeID,
		"slug":                  listing.Slug,
		"name":                  listing.Name,
		"shortDescription":      listing.ShortDescription,
		"fullDescription":       listing.FullDescription,
		"fullDescriptionHTML":   "<p>" + listing.FullDescription + "</p>",
		"isPublic":              listing.IsPublic,
		"createdAt":             sponsorsTime(listing.CreatedAt),
		"resourcePath":          path,
		"url":                   externalURL(path),
		"dashboardResourcePath": path + "/dashboard",
		"dashboardUrl":          externalURL(path + "/dashboard"),
		"_sponsorableLogin":     listing.SponsorableLogin,
	}
}

func (s *Resolver) sponsorsTierGQL(tier *store.SponsorsTier) map[string]interface{} {
	if tier == nil {
		return nil
	}
	return map[string]interface{}{
		"__typename":            "SponsorsTier",
		"_tierID":               tier.ID,
		"_dbID":                 tier.ID,
		"_listingID":            tier.ListingID,
		"nodeID":                tier.NodeID,
		"id":                    tier.NodeID,
		"name":                  tier.Name,
		"description":           tier.Description,
		"descriptionHTML":       "<p>" + tier.Description + "</p>",
		"monthlyPriceInCents":   tier.MonthlyPriceInCents,
		"monthlyPriceInDollars": tier.MonthlyPriceInCents / 100,
		"isCustomAmount":        tier.IsCustomAmount,
		"isOneTime":             tier.IsOneTime,
		"createdAt":             sponsorsTime(tier.CreatedAt),
		"updatedAt":             sponsorsTime(tier.UpdatedAt),
	}
}

func (s *Resolver) sponsorshipGQL(sponsorship *store.Sponsorship) map[string]interface{} {
	if sponsorship == nil {
		return nil
	}
	out := map[string]interface{}{
		"__typename":              "Sponsorship",
		"_sponsorshipID":          sponsorship.ID,
		"_dbID":                   sponsorship.ID,
		"nodeID":                  sponsorship.NodeID,
		"id":                      sponsorship.NodeID,
		"createdAt":               sponsorsTime(sponsorship.CreatedAt),
		"isActive":                sponsorship.IsActive,
		"isOneTimePayment":        sponsorship.IsOneTimePayment,
		"isSponsorOptedIntoEmail": sponsorship.IsSponsorOptedIntoEmail,
		"privacyLevel":            sponsorship.PrivacyLevel,
		"paymentSource":           sponsorship.PaymentSource,
		"tierSelectedAt":          sponsorsTime(sponsorship.TierSelectedAt),
		"_sponsorLogin":           sponsorship.SponsorLogin,
		"_sponsorableLogin":       sponsorship.SponsorableLogin,
		"_tierID":                 sponsorship.TierID,
		"_amountInCents":          sponsorship.AmountInCents,
	}
	return out
}

func (s *Resolver) sponsorsActivityGQL(activity *store.SponsorsActivity) map[string]interface{} {
	if activity == nil {
		return nil
	}
	return map[string]interface{}{
		"__typename":          "SponsorsActivity",
		"_activityID":         activity.ID,
		"_dbID":               activity.ID,
		"nodeID":              activity.NodeID,
		"id":                  activity.NodeID,
		"action":              activity.Action,
		"timestamp":           sponsorsTime(activity.Timestamp),
		"viaBulkSponsorship":  activity.ViaBulkSponsorship,
		"currentPrivacyLevel": nullableString(activity.CurrentPrivacyLevel),
		"paymentSource":       nullableString(activity.PaymentSource),
		"_sponsorLogin":       activity.SponsorLogin,
		"_sponsorableLogin":   activity.SponsorableLogin,
		"_tierID":             activity.SponsorsTierID,
		"_previousTierID":     activity.PreviousSponsorsTierID,
	}
}

func (s *Resolver) sponsorshipNewsletterGQL(n *store.SponsorshipNewsletter) map[string]interface{} {
	if n == nil {
		return nil
	}
	listing := s.store.Sponsors.GetSponsorsListing(n.ListingID)
	sponsorable := ""
	if listing != nil {
		sponsorable = listing.SponsorableLogin
	}
	return map[string]interface{}{
		"__typename":        "SponsorshipNewsletter",
		"_newsletterID":     n.ID,
		"_dbID":             n.ID,
		"nodeID":            n.NodeID,
		"id":                n.NodeID,
		"subject":           n.Subject,
		"body":              n.Body,
		"isPublished":       n.IsPublished,
		"createdAt":         sponsorsTime(n.CreatedAt),
		"updatedAt":         sponsorsTime(n.UpdatedAt),
		"_authorID":         n.AuthorID,
		"_sponsorableLogin": sponsorable,
	}
}

func (s *Resolver) sponsorsFeaturedItemGQL(item *store.SponsorsListingFeaturedItem) map[string]interface{} {
	if item == nil {
		return nil
	}
	return map[string]interface{}{
		"__typename":         "SponsorsListingFeaturedItem",
		"_featuredItemID":    item.ID,
		"_dbID":              item.ID,
		"_listingID":         item.ListingID,
		"nodeID":             item.NodeID,
		"id":                 item.NodeID,
		"description":        nullableString(item.Description),
		"position":           item.Position,
		"createdAt":          sponsorsTime(item.CreatedAt),
		"updatedAt":          sponsorsTime(item.UpdatedAt),
		"_featureableType":   item.FeatureableType,
		"_featureableDBID":   item.FeatureableID,
		"_featureableIsRepo": item.FeatureableType == store.SponsorsFeatureableRepository,
	}
}

// sponsorsSourceInt reads a private integer key off a rendered source map.
func sponsorsSourceInt(source interface{}, key string) int {
	m, _ := source.(map[string]interface{})
	if m == nil {
		return 0
	}
	value, _ := m[key].(int)
	return value
}

func sponsorsSourceString(source interface{}, key string) string {
	m, _ := source.(map[string]interface{})
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return value
}

// ---------------------------------------------------------------------------
// type construction

func (s *Resolver) sponsorsGoalType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.goal != nil {
		return types.goal
	}
	types.goal = graphql.NewObject(graphql.ObjectConfig{
		Name: "SponsorsGoal",
		Fields: graphql.Fields{
			"description":     &graphql.Field{Type: graphql.String},
			"kind":            &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum("SponsorsGoalKind", store.SponsorsGoalMonthlyAmount, store.SponsorsGoalTotalSponsors))},
			"percentComplete": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"targetValue":     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"title":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	return types.goal
}

// sponsorableInterfaceType is the Sponsorable interface. Its fields are
// declared through a thunk because they name SponsorsListing, Sponsorship
// and the connections, each of which names Sponsorable back.
func (s *Resolver) sponsorableInterfaceType() *graphql.Interface {
	types := s.sponsorsTypes()
	if types.sponsorable != nil {
		return types.sponsorable
	}
	types.sponsorable = graphql.NewInterface(graphql.InterfaceConfig{
		Name:   "Sponsorable",
		Fields: graphql.FieldsThunk(func() graphql.Fields { return s.sponsorableFields() }),
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			name, _ := source["__typename"].(string)
			if concrete := s.sponsorsTypes().sponsorableTypes[name]; concrete != nil {
				return concrete
			}
			return s.sponsorsTypes().sponsorableTypes["User"]
		},
	})
	return types.sponsorable
}

func (s *Resolver) sponsorUnionType() *graphql.Union {
	types := s.sponsorsTypes()
	if types.sponsorUnion != nil {
		return types.sponsorUnion
	}
	types.sponsorUnion = graphql.NewUnion(graphql.UnionConfig{
		Name:  "Sponsor",
		Types: []*graphql.Object{types.pendingOrgType, types.pendingUserType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name == "Organization" {
				return s.sponsorsTypes().pendingOrgType
			}
			return s.sponsorsTypes().pendingUserType
		},
	})
	return types.sponsorUnion
}

func (s *Resolver) sponsorableItemUnionType() *graphql.Union {
	types := s.sponsorsTypes()
	if types.sponsorableItem != nil {
		return types.sponsorableItem
	}
	types.sponsorableItem = graphql.NewUnion(graphql.UnionConfig{
		Name:  "SponsorableItem",
		Types: []*graphql.Object{types.pendingOrgType, types.pendingUserType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name == "Organization" {
				return s.sponsorsTypes().pendingOrgType
			}
			return s.sponsorsTypes().pendingUserType
		},
	})
	return types.sponsorableItem
}

func (s *Resolver) sponsorsFeatureableItemUnionType() *graphql.Union {
	types := s.sponsorsTypes()
	if types.featureableItem != nil {
		return types.featureableItem
	}
	types.featureableItem = graphql.NewUnion(graphql.UnionConfig{
		Name:  "SponsorsListingFeatureableItem",
		Types: []*graphql.Object{s.graphqlTypes.repository, types.pendingUserType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name == "Repository" {
				return s.graphqlTypes.repository
			}
			return s.sponsorsTypes().pendingUserType
		},
	})
	return types.featureableItem
}

func (s *Resolver) sponsorsTierType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.tier != nil {
		return types.tier
	}
	types.tier = graphql.NewObject(graphql.ObjectConfig{
		Name:       "SponsorsTier",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":                    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"name":                  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"description":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"descriptionHTML":       &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("HTML"))},
				"monthlyPriceInCents":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
				"monthlyPriceInDollars": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
				"isCustomAmount":        &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isOneTime":             &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"createdAt":             &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
				"updatedAt":             &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
				"sponsorsListing": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorsListingType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return s.sponsorsListingGQL(s.store.Sponsors.GetSponsorsListing(sponsorsSourceInt(p.Source, "_listingID"))), nil
					},
				},
				// A tier one price point below this one, at the same
				// frequency: what a sponsor is offered when they downgrade.
				"closestLesserValueTier": &graphql.Field{
					Type: s.sponsorsTierType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						tier := s.store.Sponsors.GetSponsorsTier(sponsorsSourceInt(p.Source, "_tierID"))
						if tier == nil {
							return nil, nil
						}
						var best *store.SponsorsTier
						for _, candidate := range s.store.Sponsors.ListSponsorsTiers(tier.ListingID, false) {
							if candidate.ID == tier.ID || candidate.IsOneTime != tier.IsOneTime {
								continue
							}
							if candidate.MonthlyPriceInCents > tier.MonthlyPriceInCents {
								continue
							}
							if best == nil || candidate.MonthlyPriceInCents > best.MonthlyPriceInCents {
								best = candidate
							}
						}
						return optionalObject(s.sponsorsTierGQL(best)), nil
					},
				},
				"adminInfo": &graphql.Field{
					Type: s.sponsorsTierAdminInfoType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						tier := s.store.Sponsors.GetSponsorsTier(sponsorsSourceInt(p.Source, "_tierID"))
						if tier == nil {
							return nil, nil
						}
						listing := s.store.Sponsors.GetSponsorsListing(tier.ListingID)
						// Draft/published/retired state and the tier's
						// sponsorships are the maintainer's business alone.
						if listing == nil || !s.viewerCanAdminAccount(p.Context, listing.SponsorableLogin) {
							return nil, nil
						}
						return map[string]interface{}{
							"isDraft": tier.IsDraft, "isPublished": tier.IsPublished, "isRetired": tier.IsRetired,
							"_tierID": tier.ID,
						}, nil
					},
				},
			}
		}),
	})
	return types.tier
}

func (s *Resolver) sponsorsTierAdminInfoType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.tierAdminInfo != nil {
		return types.tierAdminInfo
	}
	types.tierAdminInfo = graphql.NewObject(graphql.ObjectConfig{
		Name: "SponsorsTierAdminInfo",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"isDraft":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isPublished": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isRetired":   &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"sponsorships": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorshipConnectionType()),
					Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
						"includePrivate": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
						"orderBy":        &graphql.ArgumentConfig{Type: s.sponsorsOrderInput("SponsorshipOrder", "SponsorshipOrderField", "CREATED_AT")},
					}),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						tierID := sponsorsSourceInt(p.Source, "_tierID")
						includePrivate, _ := p.Args["includePrivate"].(bool)
						rows := s.store.Sponsors.ListSponsorshipsForTier(tierID, true)
						return s.sponsorshipConnection(p, rows, includePrivate), nil
					},
				},
			}
		}),
	})
	return types.tierAdminInfo
}

func (s *Resolver) sponsorsFeaturedItemType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.featuredItem != nil {
		return types.featuredItem
	}
	types.featuredItem = graphql.NewObject(graphql.ObjectConfig{
		Name:       "SponsorsListingFeaturedItem",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"description": &graphql.Field{Type: graphql.String},
				"position":    &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
				"createdAt":   &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
				"updatedAt":   &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
				"sponsorsListing": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorsListingType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return s.sponsorsListingGQL(s.store.Sponsors.GetSponsorsListing(sponsorsSourceInt(p.Source, "_listingID"))), nil
					},
				},
				"featureable": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorsFeatureableItemUnionType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						id := sponsorsSourceInt(p.Source, "_featureableDBID")
						if sponsorsSourceString(p.Source, "_featureableType") == store.SponsorsFeatureableRepository {
							repo := s.store.GetRepoByID(id)
							if repo == nil {
								return nil, nil
							}
							rendered := repoToGraphQL(s.store, repo)
							rendered["__typename"] = "Repository"
							return rendered, nil
						}
						user := s.store.GetUserByID(id)
						if user == nil {
							return nil, nil
						}
						rendered := userToGraphQL(user)
						rendered["__typename"] = "User"
						return rendered, nil
					},
				},
			}
		}),
	})
	return types.featuredItem
}

func (s *Resolver) sponsorsListingType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.listing != nil {
		return types.listing
	}
	types.listing = graphql.NewObject(graphql.ObjectConfig{
		Name:       "SponsorsListing",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			uri := s.graphQLStringScalar("URI")
			return graphql.Fields{
				"id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"slug": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				// bleephub runs no Stripe integration, so a listing has no active
				// Stripe Connect account; the field is present (its type exists)
				// and answers a truthful null rather than being missing.
				"activeStripeConnectAccount": &graphql.Field{
					Type:    s.gqlStripeConnectAccountType(),
					Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
				},
				"shortDescription":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"fullDescription":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"fullDescriptionHTML":   &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("HTML"))},
				"isPublic":              &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"createdAt":             &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
				"resourcePath":          &graphql.Field{Type: graphql.NewNonNull(uri)},
				"url":                   &graphql.Field{Type: graphql.NewNonNull(uri)},
				"dashboardResourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
				"dashboardUrl":          &graphql.Field{Type: graphql.NewNonNull(uri)},
				"activeGoal": &graphql.Field{
					Type: s.sponsorsGoalType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return s.sponsorsGoalGQL(s.store.Sponsors.GetSponsorsListing(sponsorsSourceInt(p.Source, "_listingID"))), nil
					},
				},
				"sponsorable": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorableInterfaceType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return optionalObject(s.sponsorsAccountGQL(sponsorsSourceString(p.Source, "_sponsorableLogin"))), nil
					},
				},
				// The fiscal host is the organization receiving payouts on
				// the maintainer's behalf, when they have one.
				"fiscalHost": &graphql.Field{
					Type: s.graphqlTypes.organization,
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						listing := s.store.Sponsors.GetSponsorsListing(sponsorsSourceInt(p.Source, "_listingID"))
						if listing == nil || listing.FiscalHostLogin == "" {
							return nil, nil
						}
						org := s.store.GetOrg(listing.FiscalHostLogin)
						if org == nil {
							return nil, nil
						}
						return orgToGraphQL(org), nil
					},
				},
				// Payout and contact configuration is the maintainer's:
				// GitHub returns null for it to everybody else, and so must
				// this — the field is not missing, it is not theirs.
				"contactEmailAddress":      s.sponsorsMaintainerOnlyField(graphql.String, func(l *store.SponsorsListing) interface{} { return nullableString(l.ContactEmail) }),
				"billingCountryOrRegion":   s.sponsorsMaintainerOnlyField(graphql.String, func(l *store.SponsorsListing) interface{} { return nullableString(l.BillingCountryOrRegion) }),
				"residenceCountryOrRegion": s.sponsorsMaintainerOnlyField(graphql.String, func(l *store.SponsorsListing) interface{} { return nullableString(l.ResidenceCountryOrRegion) }),
				"nextPayoutDate":           s.sponsorsMaintainerOnlyField(s.graphQLStringScalar("Date"), func(l *store.SponsorsListing) interface{} { return nullableString(l.NextPayoutDate) }),
				"featuredItems": &graphql.Field{
					Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(s.sponsorsFeaturedItemType()))),
					Args: graphql.FieldConfigArgument{
						"featureableTypes": &graphql.ArgumentConfig{
							Type: graphql.NewList(graphql.NewNonNull(s.graphQLEnum("SponsorsListingFeaturedItemFeatureableType",
								store.SponsorsFeatureableRepository, store.SponsorsFeatureableUser))),
							DefaultValue: []interface{}{store.SponsorsFeatureableRepository, store.SponsorsFeatureableUser},
						},
					},
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						wanted := []string{}
						if raw, ok := p.Args["featureableTypes"].([]interface{}); ok {
							for _, entry := range raw {
								if name, ok := entry.(string); ok {
									wanted = append(wanted, name)
								}
							}
						}
						out := []map[string]interface{}{}
						for _, item := range s.store.Sponsors.ListSponsorsListingFeaturedItems(sponsorsSourceInt(p.Source, "_listingID"), wanted) {
							out = append(out, s.sponsorsFeaturedItemGQL(item))
						}
						return out, nil
					},
				},
				"tiers": &graphql.Field{
					Type: s.sponsorsTierConnectionType(),
					Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
						"includeUnpublished": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
						"orderBy": &graphql.ArgumentConfig{
							Type: s.sponsorsOrderInput("SponsorsTierOrder", "SponsorsTierOrderField", "CREATED_AT", "MONTHLY_PRICE_IN_CENTS"),
						},
					}),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						listing := s.store.Sponsors.GetSponsorsListing(sponsorsSourceInt(p.Source, "_listingID"))
						if listing == nil {
							return nil, nil
						}
						includeUnpublished, _ := p.Args["includeUnpublished"].(bool)
						// Unpublished tiers are drafts; only the maintainer
						// may see they exist.
						if includeUnpublished && !s.viewerCanAdminAccount(p.Context, listing.SponsorableLogin) {
							includeUnpublished = false
						}
						nodes := []map[string]interface{}{}
						for _, tier := range s.store.Sponsors.ListSponsorsTiers(listing.ID, includeUnpublished) {
							nodes = append(nodes, s.sponsorsTierGQL(tier))
						}
						return paginateGQLMaps(nodes, p.Args), nil
					},
				},
			}
		}),
	})
	return types.listing
}

// gqlStripeConnectAccountType is GitHub's StripeConnectAccount object
// (memoized). bleephub never populates one — SponsorsListing.activeStripeConnectAccount
// is always null — but the type has to exist with GitHub's signature for that
// field to name it. Its members are declared through a thunk because
// sponsorsListing points back at SponsorsListing, which points here.
func (s *Resolver) gqlStripeConnectAccountType() *graphql.Object {
	return s.mutationObjectLazy("StripeConnectAccount", func() graphql.Fields {
		return graphql.Fields{
			"accountId":              &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"billingCountryOrRegion": &graphql.Field{Type: graphql.String},
			"countryOrRegion":        &graphql.Field{Type: graphql.String},
			"isActive":               &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"sponsorsListing":        &graphql.Field{Type: graphql.NewNonNull(s.sponsorsListingType())},
			"stripeDashboardUrl":     &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("URI"))},
		}
	})
}

// sponsorsMaintainerOnlyField builds a listing field that answers only for
// a viewer who can administer the sponsorable, exactly as GitHub does.
func (s *Resolver) sponsorsMaintainerOnlyField(fieldType graphql.Output, read func(*store.SponsorsListing) interface{}) *graphql.Field {
	return &graphql.Field{
		Type: fieldType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			listing := s.store.Sponsors.GetSponsorsListing(sponsorsSourceInt(p.Source, "_listingID"))
			if listing == nil || !s.viewerCanAdminAccount(p.Context, listing.SponsorableLogin) {
				return nil, nil
			}
			return read(listing), nil
		},
	}
}

func (s *Resolver) sponsorshipType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.sponsorship != nil {
		return types.sponsorship
	}
	types.sponsorship = graphql.NewObject(graphql.ObjectConfig{
		Name:       "Sponsorship",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":                      &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"createdAt":               &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
				"isActive":                &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isOneTimePayment":        &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"isSponsorOptedIntoEmail": &graphql.Field{Type: graphql.Boolean},
				"privacyLevel":            &graphql.Field{Type: graphql.NewNonNull(s.sponsorshipPrivacyEnum())},
				"paymentSource":           &graphql.Field{Type: s.graphQLEnum("SponsorshipPaymentSource", store.SponsorshipPaymentSourceGitHub, store.SponsorshipPaymentSourcePatreon)},
				"tierSelectedAt":          &graphql.Field{Type: s.graphQLStringScalar("DateTime")},
				"tier": &graphql.Field{
					Type: s.sponsorsTierType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return s.sponsorsTierGQL(s.store.Sponsors.GetSponsorsTier(sponsorsSourceInt(p.Source, "_tierID"))), nil
					},
				},
				"sponsorEntity": &graphql.Field{
					Type: s.sponsorUnionType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return optionalObject(s.sponsorsAccountGQL(sponsorsSourceString(p.Source, "_sponsorLogin"))), nil
					},
				},
				"sponsorable": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorableInterfaceType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return optionalObject(s.sponsorsAccountGQL(sponsorsSourceString(p.Source, "_sponsorableLogin"))), nil
					},
				},
				// GitHub keeps `sponsor` and `maintainer` on the type as
				// deprecated User-typed views of the two sides; a client
				// written before sponsorEntity existed still resolves.
				"sponsor": &graphql.Field{
					Type:              s.graphqlTypes.user,
					DeprecationReason: "`Sponsorship.sponsor` will be removed. Use `Sponsorship.sponsorEntity` instead. Removal on 2020-10-01 UTC.",
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						user := s.store.LookupUserByLogin(sponsorsSourceString(p.Source, "_sponsorLogin"))
						if user == nil {
							return nil, nil
						}
						return userToGraphQL(user), nil
					},
				},
				"maintainer": &graphql.Field{
					Type:              graphql.NewNonNull(s.graphqlTypes.user),
					DeprecationReason: "`Sponsorship.maintainer` will be removed. Use `Sponsorship.sponsorable` instead. Removal on 2020-04-01 UTC.",
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						user := s.store.LookupUserByLogin(sponsorsSourceString(p.Source, "_sponsorableLogin"))
						if user == nil {
							return nil, fmt.Errorf("sponsorship maintainer is an organization; use Sponsorship.sponsorable")
						}
						return userToGraphQL(user), nil
					},
				},
			}
		}),
	})
	return types.sponsorship
}

func (s *Resolver) sponsorshipPrivacyEnum() *graphql.Enum {
	return s.graphQLEnum("SponsorshipPrivacy", store.SponsorshipPrivacyPrivate, store.SponsorshipPrivacyPublic)
}

func (s *Resolver) sponsorsActivityType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.activity != nil {
		return types.activity
	}
	types.activity = graphql.NewObject(graphql.ObjectConfig{
		Name:       "SponsorsActivity",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"action": &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum("SponsorsActivityAction",
					store.SponsorsActivityCancelledSponsorship, store.SponsorsActivityNewSponsorship,
					store.SponsorsActivityPendingChange, store.SponsorsActivityRefund,
					store.SponsorsActivitySponsorMatchDisabled, store.SponsorsActivityTierChange))},
				"currentPrivacyLevel": &graphql.Field{Type: s.sponsorshipPrivacyEnum()},
				"paymentSource":       &graphql.Field{Type: s.graphQLEnum("SponsorshipPaymentSource", store.SponsorshipPaymentSourceGitHub, store.SponsorshipPaymentSourcePatreon)},
				"timestamp":           &graphql.Field{Type: s.graphQLStringScalar("DateTime")},
				"viaBulkSponsorship":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"sponsor": &graphql.Field{
					Type: s.sponsorUnionType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return optionalObject(s.sponsorsAccountGQL(sponsorsSourceString(p.Source, "_sponsorLogin"))), nil
					},
				},
				"sponsorable": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorableInterfaceType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return optionalObject(s.sponsorsAccountGQL(sponsorsSourceString(p.Source, "_sponsorableLogin"))), nil
					},
				},
				"sponsorsTier": &graphql.Field{
					Type: s.sponsorsTierType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return s.sponsorsTierGQL(s.store.Sponsors.GetSponsorsTier(sponsorsSourceInt(p.Source, "_tierID"))), nil
					},
				},
				"previousSponsorsTier": &graphql.Field{
					Type: s.sponsorsTierType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return s.sponsorsTierGQL(s.store.Sponsors.GetSponsorsTier(sponsorsSourceInt(p.Source, "_previousTierID"))), nil
					},
				},
			}
		}),
	})
	return types.activity
}

func (s *Resolver) sponsorshipNewsletterType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.newsletter != nil {
		return types.newsletter
	}
	types.newsletter = graphql.NewObject(graphql.ObjectConfig{
		Name:       "SponsorshipNewsletter",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"subject":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"body":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"isPublished": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"createdAt":   &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
				"updatedAt":   &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
				"author": &graphql.Field{
					Type: s.graphqlTypes.user,
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						author := s.store.GetUserByID(sponsorsSourceInt(p.Source, "_authorID"))
						if author == nil {
							return nil, nil
						}
						return userToGraphQL(author), nil
					},
				},
				"sponsorable": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorableInterfaceType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return optionalObject(s.sponsorsAccountGQL(sponsorsSourceString(p.Source, "_sponsorableLogin"))), nil
					},
				},
			}
		}),
	})
	return types.newsletter
}

func (s *Resolver) sponsorLifetimeValueType() *graphql.Object {
	types := s.sponsorsTypes()
	if types.lifetimeValue != nil {
		return types.lifetimeValue
	}
	types.lifetimeValue = graphql.NewObject(graphql.ObjectConfig{
		Name: "SponsorAndLifetimeValue",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"amountInCents":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
				"formattedAmount": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"sponsor": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorableInterfaceType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return optionalObject(s.sponsorsAccountGQL(sponsorsSourceString(p.Source, "_sponsorLogin"))), nil
					},
				},
				"sponsorable": &graphql.Field{
					Type: graphql.NewNonNull(s.sponsorableInterfaceType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return optionalObject(s.sponsorsAccountGQL(sponsorsSourceString(p.Source, "_sponsorableLogin"))), nil
					},
				},
			}
		}),
	})
	return types.lifetimeValue
}

// ---------------------------------------------------------------------------
// connections

func sponsorsConnectionArgs(extra graphql.FieldConfigArgument) graphql.FieldConfigArgument {
	args := relayConnectionArgs()
	for name, arg := range extra {
		args[name] = arg
	}
	return args
}

func (s *Resolver) sponsorshipConnectionType() *graphql.Object {
	return s.sponsorsConnectionType("SponsorshipConnection", s.sponsorshipType(), graphql.Fields{
		"totalRecurringMonthlyPriceInCents":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"totalRecurringMonthlyPriceInDollars": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	})
}

func (s *Resolver) sponsorsTierConnectionType() *graphql.Object {
	return s.sponsorsConnectionType("SponsorsTierConnection", s.sponsorsTierType(), nil)
}

func (s *Resolver) sponsorsActivityConnectionType() *graphql.Object {
	return s.sponsorsConnectionType("SponsorsActivityConnection", s.sponsorsActivityType(), nil)
}

func (s *Resolver) sponsorshipNewsletterConnectionType() *graphql.Object {
	return s.sponsorsConnectionType("SponsorshipNewsletterConnection", s.sponsorshipNewsletterType(), nil)
}

func (s *Resolver) sponsorConnectionType() *graphql.Object {
	return s.sponsorsConnectionType("SponsorConnection", s.sponsorUnionType(), nil)
}

func (s *Resolver) sponsorableItemConnectionType() *graphql.Object {
	return s.sponsorsConnectionType("SponsorableItemConnection", s.sponsorableItemUnionType(), nil)
}

func (s *Resolver) sponsorLifetimeValueConnectionType() *graphql.Object {
	return s.sponsorsConnectionType("SponsorAndLifetimeValueConnection", s.sponsorLifetimeValueType(), nil)
}

// sponsorshipConnection renders a sponsorship connection, dropping the
// private sponsorships the viewer may not see and totalling the recurring
// monthly price of the ones that survive — so the total can never leak the
// value of a sponsorship the viewer cannot read.
func (s *Resolver) sponsorshipConnection(p graphql.ResolveParams, rows []*store.Sponsorship, includePrivate bool) map[string]interface{} {
	visible := s.visibleSponsorships(p, rows)
	kept := make([]*store.Sponsorship, 0, len(visible))
	for _, sponsorship := range visible {
		if !includePrivate && sponsorship.PrivacyLevel == store.SponsorshipPrivacyPrivate {
			continue
		}
		kept = append(kept, sponsorship)
	}
	nodes := make([]map[string]interface{}, 0, len(kept))
	totalCents := 0
	for _, sponsorship := range kept {
		nodes = append(nodes, s.sponsorshipGQL(sponsorship))
		if sponsorship.IsActive && !sponsorship.IsOneTimePayment {
			totalCents += sponsorship.AmountInCents
		}
	}
	conn := paginateGQLMaps(nodes, p.Args)
	conn["totalRecurringMonthlyPriceInCents"] = totalCents
	conn["totalRecurringMonthlyPriceInDollars"] = totalCents / 100
	return conn
}

// ---------------------------------------------------------------------------
// Sponsorable interface fields — shared by User and Organization

// sponsorableFields is the whole Sponsorable field set. It is built once
// and installed on the interface, on User and on Organization, so the three
// can never drift.
func (s *Resolver) sponsorableFields() graphql.Fields {
	dateTime := s.graphQLStringScalar("DateTime")
	sponsorOrder := s.sponsorsOrderInput("SponsorOrder", "SponsorOrderField", "LOGIN", "RELEVANCE")
	sponsorshipOrder := s.sponsorsOrderInput("SponsorshipOrder", "SponsorshipOrderField", "CREATED_AT")

	return graphql.Fields{
		"hasSponsorsListing": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.store.Sponsors.GetSponsorsListingForAccount(sponsorsAccountLogin(p.Source)) != nil, nil
			},
		},
		"sponsorsListing": &graphql.Field{
			Type: s.sponsorsListingType(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				login := sponsorsAccountLogin(p.Source)
				listing := s.store.Sponsors.GetSponsorsListingForAccount(login)
				if listing == nil {
					return nil, nil
				}
				// An unpublished profile is visible to its maintainer only.
				if !listing.IsPublic && !s.viewerCanAdminAccount(p.Context, login) {
					return nil, nil
				}
				return s.sponsorsListingGQL(listing), nil
			},
		},
		"estimatedNextSponsorsPayoutInCents": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Int),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				login := sponsorsAccountLogin(p.Source)
				if !s.viewerCanAdminAccount(p.Context, login) {
					return 0, nil
				}
				return s.store.Sponsors.EstimatedNextSponsorsPayoutInCents(login), nil
			},
		},
		"monthlyEstimatedSponsorsIncomeInCents": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Int),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.store.Sponsors.MonthlyEstimatedSponsorsIncomeInCents(sponsorsAccountLogin(p.Source)), nil
			},
		},
		"isSponsoredBy": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Args: graphql.FieldConfigArgument{
				"accountLogin": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				accountLogin, _ := p.Args["accountLogin"].(string)
				sponsorship := s.store.Sponsors.GetSponsorshipBetween(accountLogin, sponsorsAccountLogin(p.Source), true)
				return s.sponsorshipVisible(p, sponsorship), nil
			},
		},
		"isSponsoringViewer": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				viewer := s.ghUserFromContext(p.Context)
				if viewer == nil {
					return false, nil
				}
				return s.store.Sponsors.GetSponsorshipBetween(sponsorsAccountLogin(p.Source), viewer.Login, true) != nil, nil
			},
		},
		"viewerIsSponsoring": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				viewer := s.ghUserFromContext(p.Context)
				if viewer == nil {
					return false, nil
				}
				return s.store.Sponsors.GetSponsorshipBetween(viewer.Login, sponsorsAccountLogin(p.Source), true) != nil, nil
			},
		},
		"viewerCanSponsor": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				viewer := s.ghUserFromContext(p.Context)
				login := sponsorsAccountLogin(p.Source)
				if viewer == nil || strings.EqualFold(viewer.Login, login) {
					return false, nil
				}
				listing := s.store.Sponsors.GetSponsorsListingForAccount(login)
				if listing == nil || !listing.IsPublic {
					return false, nil
				}
				return len(s.store.Sponsors.ListSponsorsTiers(listing.ID, false)) > 0, nil
			},
		},
		"sponsorshipForViewerAsSponsor": &graphql.Field{
			Type: s.sponsorshipType(),
			Args: graphql.FieldConfigArgument{
				"activeOnly": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				viewer := s.ghUserFromContext(p.Context)
				if viewer == nil {
					return nil, nil
				}
				activeOnly, _ := p.Args["activeOnly"].(bool)
				return s.sponsorshipGQL(s.store.Sponsors.GetSponsorshipBetween(viewer.Login, sponsorsAccountLogin(p.Source), activeOnly)), nil
			},
		},
		"sponsorshipForViewerAsSponsorable": &graphql.Field{
			Type: s.sponsorshipType(),
			Args: graphql.FieldConfigArgument{
				"activeOnly": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				viewer := s.ghUserFromContext(p.Context)
				if viewer == nil {
					return nil, nil
				}
				activeOnly, _ := p.Args["activeOnly"].(bool)
				return s.sponsorshipGQL(s.store.Sponsors.GetSponsorshipBetween(sponsorsAccountLogin(p.Source), viewer.Login, activeOnly)), nil
			},
		},
		"sponsors": &graphql.Field{
			Type: graphql.NewNonNull(s.sponsorConnectionType()),
			Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
				"orderBy": &graphql.ArgumentConfig{Type: sponsorOrder},
				"tierId":  &graphql.ArgumentConfig{Type: graphql.ID},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				login := sponsorsAccountLogin(p.Source)
				tierFilter := 0
				if nodeID, _ := p.Args["tierId"].(string); nodeID != "" {
					if tier := s.store.Sponsors.FindSponsorsTierByNodeID(nodeID); tier != nil {
						tierFilter = tier.ID
					} else {
						tierFilter = -1
					}
				}
				nodes := []map[string]interface{}{}
				for _, sponsorship := range s.store.Sponsors.ListSponsorshipsAsMaintainer(login, true) {
					if tierFilter != 0 && sponsorship.TierID != tierFilter {
						continue
					}
					if !s.sponsorshipVisible(p, sponsorship) {
						continue
					}
					if account := s.sponsorsAccountGQL(sponsorship.SponsorLogin); account != nil {
						nodes = append(nodes, account)
					}
				}
				return paginateGQLMaps(nodes, p.Args), nil
			},
		},
		"sponsoring": &graphql.Field{
			Type: graphql.NewNonNull(s.sponsorConnectionType()),
			Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
				"orderBy": &graphql.ArgumentConfig{Type: sponsorOrder},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				nodes := []map[string]interface{}{}
				for _, sponsorship := range s.store.Sponsors.ListSponsorshipsAsSponsor(sponsorsAccountLogin(p.Source), true) {
					if !s.sponsorshipVisible(p, sponsorship) {
						continue
					}
					if account := s.sponsorsAccountGQL(sponsorship.SponsorableLogin); account != nil {
						nodes = append(nodes, account)
					}
				}
				return paginateGQLMaps(nodes, p.Args), nil
			},
		},
		"sponsorshipsAsMaintainer": &graphql.Field{
			Type: graphql.NewNonNull(s.sponsorshipConnectionType()),
			Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
				"activeOnly":     &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
				"includePrivate": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
				"orderBy":        &graphql.ArgumentConfig{Type: sponsorshipOrder},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				activeOnly, _ := p.Args["activeOnly"].(bool)
				includePrivate, _ := p.Args["includePrivate"].(bool)
				rows := s.store.Sponsors.ListSponsorshipsAsMaintainer(sponsorsAccountLogin(p.Source), activeOnly)
				return s.sponsorshipConnection(p, rows, includePrivate), nil
			},
		},
		"sponsorshipsAsSponsor": &graphql.Field{
			Type: graphql.NewNonNull(s.sponsorshipConnectionType()),
			Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
				"activeOnly":       &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
				"maintainerLogins": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
				"orderBy":          &graphql.ArgumentConfig{Type: sponsorshipOrder},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				activeOnly, _ := p.Args["activeOnly"].(bool)
				wanted := map[string]bool{}
				if raw, ok := p.Args["maintainerLogins"].([]interface{}); ok {
					for _, entry := range raw {
						if login, ok := entry.(string); ok {
							wanted[strings.ToLower(login)] = true
						}
					}
				}
				rows := []*store.Sponsorship{}
				for _, sponsorship := range s.store.Sponsors.ListSponsorshipsAsSponsor(sponsorsAccountLogin(p.Source), activeOnly) {
					if len(wanted) > 0 && !wanted[strings.ToLower(sponsorship.SponsorableLogin)] {
						continue
					}
					rows = append(rows, sponsorship)
				}
				// A sponsor always sees their own sponsorships, private or
				// not; sponsorshipConnection's privacy filter answers for
				// everyone else.
				return s.sponsorshipConnection(p, rows, true), nil
			},
		},
		"sponsorsActivities": &graphql.Field{
			Type: graphql.NewNonNull(s.sponsorsActivityConnectionType()),
			Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
				"actions": &graphql.ArgumentConfig{
					Type: graphql.NewList(graphql.NewNonNull(s.graphQLEnum("SponsorsActivityAction",
						store.SponsorsActivityCancelledSponsorship, store.SponsorsActivityNewSponsorship,
						store.SponsorsActivityPendingChange, store.SponsorsActivityRefund,
						store.SponsorsActivitySponsorMatchDisabled, store.SponsorsActivityTierChange))),
					DefaultValue: []interface{}{},
				},
				"includeAsSponsor": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
				"includePrivate":   &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
				"orderBy":          &graphql.ArgumentConfig{Type: s.sponsorsOrderInput("SponsorsActivityOrder", "SponsorsActivityOrderField", "TIMESTAMP")},
				"period":           &graphql.ArgumentConfig{Type: s.graphQLEnum("SponsorsActivityPeriod", "ALL", "DAY", "MONTH", "WEEK"), DefaultValue: "MONTH"},
				"since":            &graphql.ArgumentConfig{Type: dateTime},
				"until":            &graphql.ArgumentConfig{Type: dateTime},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.resolveSponsorsActivities(p)
			},
		},
		"sponsorshipNewsletters": &graphql.Field{
			Type: graphql.NewNonNull(s.sponsorshipNewsletterConnectionType()),
			Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
				"orderBy": &graphql.ArgumentConfig{Type: s.sponsorsOrderInput("SponsorshipNewsletterOrder", "SponsorshipNewsletterOrderField", "CREATED_AT")},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				login := sponsorsAccountLogin(p.Source)
				listing := s.store.Sponsors.GetSponsorsListingForAccount(login)
				if listing == nil {
					return paginateGQLMaps(nil, p.Args), nil
				}
				// Drafts belong to the maintainer; published updates are for
				// the sponsors who paid for them.
				maintainer := s.viewerCanAdminAccount(p.Context, login)
				viewer := s.ghUserFromContext(p.Context)
				sponsoring := viewer != nil && s.store.Sponsors.GetSponsorshipBetween(viewer.Login, login, true) != nil
				if !maintainer && !sponsoring {
					return paginateGQLMaps(nil, p.Args), nil
				}
				nodes := []map[string]interface{}{}
				for _, newsletter := range s.store.Sponsors.ListSponsorshipNewsletters(listing.ID, maintainer) {
					nodes = append(nodes, s.sponsorshipNewsletterGQL(newsletter))
				}
				return paginateGQLMaps(nodes, p.Args), nil
			},
		},
		"lifetimeReceivedSponsorshipValues": &graphql.Field{
			Type: graphql.NewNonNull(s.sponsorLifetimeValueConnectionType()),
			Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
				"orderBy": &graphql.ArgumentConfig{
					Type: s.sponsorsOrderInput("SponsorAndLifetimeValueOrder", "SponsorAndLifetimeValueOrderField",
						"LIFETIME_VALUE", "SPONSOR_LOGIN", "SPONSOR_RELEVANCE"),
				},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				login := sponsorsAccountLogin(p.Source)
				// What each sponsor has paid is the maintainer's ledger.
				if !s.viewerCanAdminAccount(p.Context, login) {
					return paginateGQLMaps(nil, p.Args), nil
				}
				nodes := []map[string]interface{}{}
				for _, value := range s.store.Sponsors.LifetimeReceivedSponsorshipValues(login) {
					nodes = append(nodes, map[string]interface{}{
						"amountInCents":     value.AmountInCents,
						"formattedAmount":   fmt.Sprintf("$%d.%02d", value.AmountInCents/100, value.AmountInCents%100),
						"_sponsorLogin":     value.SponsorLogin,
						"_sponsorableLogin": value.SponsorableLogin,
					})
				}
				return paginateGQLMaps(nodes, p.Args), nil
			},
		},
		"totalSponsorshipAmountAsSponsorInCents": &graphql.Field{
			Type: graphql.Int,
			Args: graphql.FieldConfigArgument{
				"since":             &graphql.ArgumentConfig{Type: dateTime},
				"sponsorableLogins": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String)), DefaultValue: []interface{}{}},
				"until":             &graphql.ArgumentConfig{Type: dateTime},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				login := sponsorsAccountLogin(p.Source)
				// GitHub returns this only to the account itself or to a
				// manager of the organization: it is their spending.
				if !s.viewerCanAdminAccount(p.Context, login) {
					return nil, nil
				}
				logins := []string{}
				if raw, ok := p.Args["sponsorableLogins"].([]interface{}); ok {
					for _, entry := range raw {
						if name, ok := entry.(string); ok {
							logins = append(logins, name)
						}
					}
				}
				return s.store.Sponsors.TotalSponsorshipAmountAsSponsorInCents(login, sponsorsTimeArg(p.Args, "since"), sponsorsTimeArg(p.Args, "until"), logins), nil
			},
		},
	}
}

// sponsorsTimeArg parses an RFC 3339 DateTime argument.
func sponsorsTimeArg(args map[string]interface{}, key string) *time.Time {
	raw, _ := args[key].(string)
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

// resolveSponsorsActivities applies the action, period and window filters
// to a sponsorable's activity feed.
func (s *Resolver) resolveSponsorsActivities(p graphql.ResolveParams) (interface{}, error) {
	login := sponsorsAccountLogin(p.Source)
	includeAsSponsor, _ := p.Args["includeAsSponsor"].(bool)
	includePrivate, ok := p.Args["includePrivate"].(bool)
	if !ok {
		includePrivate = true
	}
	wanted := map[string]bool{}
	if raw, ok := p.Args["actions"].([]interface{}); ok {
		for _, entry := range raw {
			if action, ok := entry.(string); ok {
				wanted[action] = true
			}
		}
	}
	since, until := sponsorsTimeArg(p.Args, "since"), sponsorsTimeArg(p.Args, "until")
	if since == nil && until == nil {
		if period, _ := p.Args["period"].(string); period != "" && period != "ALL" {
			cutoff := s.store.CurrentTime()
			switch period {
			case "DAY":
				cutoff = cutoff.AddDate(0, 0, -1)
			case "WEEK":
				cutoff = cutoff.AddDate(0, 0, -7)
			default:
				cutoff = cutoff.AddDate(0, -1, 0)
			}
			since = &cutoff
		}
	}
	maintainer := s.viewerCanAdminAccount(p.Context, login)
	nodes := []map[string]interface{}{}
	for _, activity := range s.store.Sponsors.ListSponsorsActivities(login, includeAsSponsor) {
		if len(wanted) > 0 && !wanted[activity.Action] {
			continue
		}
		if since != nil && activity.Timestamp.Before(*since) {
			continue
		}
		if until != nil && !activity.Timestamp.Before(*until) {
			continue
		}
		private := activity.CurrentPrivacyLevel == store.SponsorshipPrivacyPrivate
		if private {
			// A private sponsorship's activity is readable by the two
			// parties to it and nobody else, whatever includePrivate says.
			if !includePrivate {
				continue
			}
			if !maintainer && !s.viewerCanAdminAccount(p.Context, activity.SponsorLogin) {
				continue
			}
		}
		nodes = append(nodes, s.sponsorsActivityGQL(activity))
	}
	return paginateGQLMaps(nodes, p.Args), nil
}

// ---------------------------------------------------------------------------
// schema assembly

// addSponsorsFieldsToSchema installs the Sponsors surface: the Sponsorable
// fields on User and Organization, the Query.sponsorables root field, the
// node types, and every mutation.
func (s *Resolver) addSponsorsFieldsToSchema(userType, orgType, queryType, mutationType *graphql.Object, nodeTypes map[string]*graphql.Object) {
	types := s.sponsorsTypes()
	types.pendingUserType = userType
	types.pendingOrgType = orgType
	types.sponsorableTypes["User"] = userType
	types.sponsorableTypes["Organization"] = orgType

	for name, field := range s.sponsorableFields() {
		userType.AddFieldConfig(name, field)
		orgType.AddFieldConfig(name, field)
	}

	nodeTypes["SponsorsListing"] = s.sponsorsListingType()
	nodeTypes["SponsorsTier"] = s.sponsorsTierType()
	nodeTypes["Sponsorship"] = s.sponsorshipType()
	nodeTypes["SponsorsActivity"] = s.sponsorsActivityType()
	nodeTypes["SponsorshipNewsletter"] = s.sponsorshipNewsletterType()
	nodeTypes["SponsorsListingFeaturedItem"] = s.sponsorsFeaturedItemType()

	queryType.AddFieldConfig("sponsorables", &graphql.Field{
		Type: graphql.NewNonNull(s.sponsorableItemConnectionType()),
		Args: sponsorsConnectionArgs(graphql.FieldConfigArgument{
			"dependencyEcosystem": &graphql.ArgumentConfig{Type: s.graphQLEnum("SecurityAdvisoryEcosystem",
				"ACTIONS", "COMPOSER", "ERLANG", "GO", "MAVEN", "NPM", "NUGET", "PIP", "PUB", "RUBYGEMS", "RUST", "SWIFT")},
			"ecosystem": &graphql.ArgumentConfig{Type: s.graphQLEnum("DependencyGraphEcosystem",
				"ACTIONS", "COMPOSER", "GO", "MAVEN", "NPM", "NUGET", "PIP", "PUB", "RUBYGEMS", "RUST", "SWIFT")},
			"onlyDependencies":        &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
			"orderBy":                 &graphql.ArgumentConfig{Type: s.sponsorsOrderInput("SponsorableOrder", "SponsorableOrderField", "LOGIN")},
			"orgLoginForDependencies": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			nodes := []map[string]interface{}{}
			for _, listing := range s.store.Sponsors.ListSponsorsListings() {
				if !listing.IsPublic && !s.viewerCanAdminAccount(p.Context, listing.SponsorableLogin) {
					continue
				}
				if account := s.sponsorsAccountGQL(listing.SponsorableLogin); account != nil {
					nodes = append(nodes, account)
				}
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	s.addSponsorsMutations(mutationType)

	// The two SponsorsCountryOrRegionCode members of CreateSponsorsListingInput.
	// The input is minted anonymously inside the create mutation, so the two
	// fields are added to the assembled type here rather than at the mutation's
	// declaration site.
	s.addCreateSponsorsListingInputResidueFields(mutationType)
}

// addCreateSponsorsListingInputResidueFields declares the billing and residence
// country-or-region members on CreateSponsorsListingInput. They are input
// declarations only: the create resolver does not read them, matching a
// bleephub that records no bank-account geography.
func (s *Resolver) addCreateSponsorsListingInputResidueFields(mutationType *graphql.Object) {
	field, ok := mutationType.Fields()["createSponsorsListing"]
	if !ok {
		return
	}
	var input *graphql.InputObject
	for _, arg := range field.Args {
		if arg.PrivateName == "input" {
			input, _ = graphql.GetNamed(arg.Type).(*graphql.InputObject)
		}
	}
	if input == nil {
		return
	}
	code := s.gqlSponsorsCountryOrRegionCodeEnum()
	input.AddFieldConfig("billingCountryOrRegionCode", &graphql.InputObjectFieldConfig{Type: code})
	input.AddFieldConfig("residenceCountryOrRegionCode", &graphql.InputObjectFieldConfig{Type: code})
}

// gqlSponsorsCountryOrRegionCodeEnum is GitHub's SponsorsCountryOrRegionCode —
// the ISO 3166-1 alpha-2 codes a sponsorable's bank account or residence may
// name (memoized through the shared enum registry).
func (s *Resolver) gqlSponsorsCountryOrRegionCodeEnum() *graphql.Enum {
	return s.graphQLEnum("SponsorsCountryOrRegionCode",
		"AD", "AE", "AF", "AG", "AI", "AL", "AM", "AO", "AQ", "AR", "AS", "AT",
		"AU", "AW", "AX", "AZ", "BA", "BB", "BD", "BE", "BF", "BG", "BH", "BI",
		"BJ", "BL", "BM", "BN", "BO", "BQ", "BR", "BS", "BT", "BV", "BW", "BY",
		"BZ", "CA", "CC", "CD", "CF", "CG", "CH", "CI", "CK", "CL", "CM", "CN",
		"CO", "CR", "CV", "CW", "CX", "CY", "CZ", "DE", "DJ", "DK", "DM", "DO",
		"DZ", "EC", "EE", "EG", "EH", "ER", "ES", "ET", "FI", "FJ", "FK", "FM",
		"FO", "FR", "GA", "GB", "GD", "GE", "GF", "GG", "GH", "GI", "GL", "GM",
		"GN", "GP", "GQ", "GR", "GS", "GT", "GU", "GW", "GY", "HK", "HM", "HN",
		"HR", "HT", "HU", "ID", "IE", "IL", "IM", "IN", "IO", "IQ", "IR", "IS",
		"IT", "JE", "JM", "JO", "JP", "KE", "KG", "KH", "KI", "KM", "KN", "KR",
		"KW", "KY", "KZ", "LA", "LB", "LC", "LI", "LK", "LR", "LS", "LT", "LU",
		"LV", "LY", "MA", "MC", "MD", "ME", "MF", "MG", "MH", "MK", "ML", "MM",
		"MN", "MO", "MP", "MQ", "MR", "MS", "MT", "MU", "MV", "MW", "MX", "MY",
		"MZ", "NA", "NC", "NE", "NF", "NG", "NI", "NL", "NO", "NP", "NR", "NU",
		"NZ", "OM", "PA", "PE", "PF", "PG", "PH", "PK", "PL", "PM", "PN", "PR",
		"PS", "PT", "PW", "PY", "QA", "RE", "RO", "RS", "RU", "RW", "SA", "SB",
		"SC", "SD", "SE", "SG", "SH", "SI", "SJ", "SK", "SL", "SM", "SN", "SO",
		"SR", "SS", "ST", "SV", "SX", "SY", "SZ", "TC", "TD", "TF", "TG", "TH",
		"TJ", "TK", "TL", "TM", "TN", "TO", "TR", "TT", "TV", "TW", "TZ", "UA",
		"UG", "UM", "US", "UY", "UZ", "VA", "VC", "VE", "VG", "VI", "VN", "VU",
		"WF", "WS", "YE", "YT", "ZA", "ZM", "ZW",
	)
}

// ---------------------------------------------------------------------------
// node lookup

// sponsorsNodeByID resolves a Sponsors global id for Query.node.
func (s *Resolver) sponsorsNodeByID(nodeID string) map[string]interface{} {
	if listing := s.store.Sponsors.FindSponsorsListingByNodeID(nodeID); listing != nil {
		return s.sponsorsListingGQL(listing)
	}
	if tier := s.store.Sponsors.FindSponsorsTierByNodeID(nodeID); tier != nil {
		return s.sponsorsTierGQL(tier)
	}
	if sponsorship := s.store.Sponsors.FindSponsorshipByNodeID(nodeID); sponsorship != nil {
		return s.sponsorshipGQL(sponsorship)
	}
	if activity := s.store.Sponsors.FindSponsorsActivityByNodeID(nodeID); activity != nil {
		return s.sponsorsActivityGQL(activity)
	}
	if newsletter := s.store.Sponsors.FindSponsorshipNewsletterByNodeID(nodeID); newsletter != nil {
		return s.sponsorshipNewsletterGQL(newsletter)
	}
	if item := s.store.Sponsors.FindSponsorsFeaturedItemByNodeID(nodeID); item != nil {
		return s.sponsorsFeaturedItemGQL(item)
	}
	return nil
}
