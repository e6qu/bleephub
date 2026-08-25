package graphqlapi

// GitHub Sponsors — the mutation surface: the nine mutations GitHub
// publishes, their authorization policy, and the payloads they return.
//
// Every mutation goes through registerMutation, so each has a row in
// sponsorsMutationAuthz (folded into graphqlMutationAuthz) and cannot
// reach the store without standing over the account whose money or
// profile it touches.

import (
	"fmt"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// sponsorsRule is the Sponsors authorization policy shape: a lookup that
// names the account the mutation acts for, and the requirement that the
// caller may administer it.
//
// Two accounts appear in these inputs and they mean different things. The
// sponsor is who pays, so every mutation that spends money or changes a
// payment authorizes over the sponsor. The sponsorable is whose profile it
// is, so every listing and tier mutation authorizes over the sponsorable.
// Reversing either would let any signed-in account either spend somebody
// else's money or rewrite somebody else's Sponsors profile.
type sponsorsRule struct {
	target func(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) (string, error)
}

func (r sponsorsRule) check() error {
	if r.target == nil {
		return fmt.Errorf("no sponsors account lookup")
	}
	return nil
}

func (r sponsorsRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	login, err := r.target(s, p, input)
	if err != nil {
		return err
	}
	if login == "" {
		return &ghNotFoundError{message: "Could not resolve to an account with the given login."}
	}
	if !s.viewerCanAdminAccount(p.Context, login) {
		return fmt.Errorf("must be able to manage GitHub Sponsors for %s", login)
	}
	return nil
}

// sponsorsTargetSponsor names the paying account: the one the input gives,
// or the viewer when it gives none.
func sponsorsTargetSponsor(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) (string, error) {
	if login := s.sponsorsAccountFromInput(input, "sponsorId", "sponsorLogin"); login != "" {
		return login, nil
	}
	if _, given := input["sponsorId"]; given {
		return "", &ghNotFoundError{message: "Could not resolve to a Sponsor with the given id."}
	}
	if login, _ := input["sponsorLogin"].(string); login != "" {
		return "", &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a Sponsor with the login of '%s'.", login)}
	}
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil {
		return "", fmt.Errorf("must be authenticated to sponsor")
	}
	return viewer.Login, nil
}

// sponsorsTargetSponsorRequired is sponsorsTargetSponsor for the bulk
// mutation, whose sponsorLogin is non-null.
func sponsorsTargetSponsorRequired(s *Resolver, _ graphql.ResolveParams, input map[string]interface{}) (string, error) {
	login, _ := input["sponsorLogin"].(string)
	resolved, ok := s.sponsorableExists(login)
	if !ok {
		return "", &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a Sponsor with the login of '%s'.", login)}
	}
	return resolved, nil
}

// sponsorsTargetSponsorable names the account whose Sponsors profile the
// mutation edits: the one the input gives, or the viewer when it gives
// none.
func sponsorsTargetSponsorable(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) (string, error) {
	if login := s.sponsorsAccountFromInput(input, "sponsorableId", "sponsorableLogin"); login != "" {
		return login, nil
	}
	if _, given := input["sponsorableId"]; given {
		return "", &ghNotFoundError{message: "Could not resolve to a Sponsorable with the given id."}
	}
	if login, _ := input["sponsorableLogin"].(string); login != "" {
		return "", &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a Sponsorable with the login of '%s'.", login)}
	}
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil {
		return "", fmt.Errorf("must be authenticated to manage a Sponsors profile")
	}
	return viewer.Login, nil
}

// sponsorsTargetTierOwner names the sponsorable whose listing owns the
// tier the mutation names.
func sponsorsTargetTierOwner(s *Resolver, _ graphql.ResolveParams, input map[string]interface{}) (string, error) {
	nodeID, _ := input["tierId"].(string)
	tier := s.store.Sponsors.FindSponsorsTierByNodeID(nodeID)
	if tier == nil {
		return "", &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a SponsorsTier with the global id of '%s'.", nodeID)}
	}
	listing := s.store.Sponsors.GetSponsorsListing(tier.ListingID)
	if listing == nil {
		return "", &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a SponsorsTier with the global id of '%s'.", nodeID)}
	}
	return listing.SponsorableLogin, nil
}

// sponsorsMutationAuthz is the Sponsors half of the mutation policy table.
// It is merged into graphqlMutationAuthz at package init so the whole
// policy still lives in one map for the coverage sweep.
var sponsorsMutationAuthz = map[string]mutationRule{
	"createSponsorship":            sponsorsRule{target: sponsorsTargetSponsor},
	"createSponsorships":           sponsorsRule{target: sponsorsTargetSponsorRequired},
	"cancelSponsorship":            sponsorsRule{target: sponsorsTargetSponsor},
	"updateSponsorshipPreferences": sponsorsRule{target: sponsorsTargetSponsor},
	"createSponsorsListing":        sponsorsRule{target: sponsorsTargetSponsorable},
	"createSponsorsTier":           sponsorsRule{target: sponsorsTargetSponsorable},
	"updatePatreonSponsorability":  sponsorsRule{target: sponsorsTargetSponsorable},
	"publishSponsorsTier":          sponsorsRule{target: sponsorsTargetTierOwner},
	"retireSponsorsTier":           sponsorsRule{target: sponsorsTargetTierOwner},
}

func init() {
	for name, rule := range sponsorsMutationAuthz {
		if _, clash := graphqlMutationAuthz[name]; clash {
			panic("duplicate graphql mutation policy row for " + name)
		}
		graphqlMutationAuthz[name] = rule
	}
}

// ---------------------------------------------------------------------------
// helpers

func sponsorsClientMutationID(input map[string]interface{}) interface{} {
	if value, ok := input["clientMutationId"].(string); ok {
		return value
	}
	return nil
}

// sponsorsAccountInput is the (id, login) pair for one side of a
// sponsorship, plus clientMutationId, that most Sponsors inputs carry.
func sponsorsPartyInputFields() graphql.InputObjectConfigFieldMap {
	return graphql.InputObjectConfigFieldMap{
		"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		"sponsorId":        &graphql.InputObjectFieldConfig{Type: graphql.ID},
		"sponsorLogin":     &graphql.InputObjectFieldConfig{Type: graphql.String},
		"sponsorableId":    &graphql.InputObjectFieldConfig{Type: graphql.ID},
		"sponsorableLogin": &graphql.InputObjectFieldConfig{Type: graphql.String},
	}
}

// sponsorsResolveParties reads both sides of a sponsorship out of an
// input, defaulting the sponsor to the viewer.
func (s *Resolver) sponsorsResolveParties(p graphql.ResolveParams, input map[string]interface{}) (sponsor, sponsorable SponsorsAccount, err error) {
	sponsorLogin, err := sponsorsTargetSponsor(s, p, input)
	if err != nil {
		return sponsor, sponsorable, err
	}
	sponsorableLogin, err := sponsorsTargetSponsorable(s, p, input)
	if err != nil {
		return sponsor, sponsorable, err
	}
	sponsor, ok := s.sponsorsAccount(sponsorLogin)
	if !ok {
		return sponsor, sponsorable, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a Sponsor with the login of '%s'.", sponsorLogin)}
	}
	sponsorable, ok = s.sponsorsAccount(sponsorableLogin)
	if !ok {
		return sponsor, sponsorable, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a Sponsorable with the login of '%s'.", sponsorableLogin)}
	}
	return sponsor, sponsorable, nil
}

// SponsorsAccount is a resolved party to a sponsorship.
type SponsorsAccount struct {
	ID    int
	Type  string
	Login string
}

func (s *Resolver) sponsorsAccount(login string) (SponsorsAccount, bool) {
	if login == "" {
		return SponsorsAccount{}, false
	}
	if user := s.store.LookupUserByLogin(login); user != nil {
		return SponsorsAccount{ID: user.ID, Type: "User", Login: user.Login}, true
	}
	if org := s.store.GetOrg(login); org != nil {
		return SponsorsAccount{ID: org.ID, Type: "Organization", Login: org.Login}, true
	}
	return SponsorsAccount{}, false
}

// sponsorsTierForSponsorable resolves the tier a sponsorship names, or the
// cheapest published tier of the right frequency when the input names an
// amount instead — the path GitHub takes for a custom amount.
func (s *Resolver) sponsorsTierForSponsorable(input map[string]interface{}, sponsorableLogin string, recurring bool) (*store.SponsorsTier, int, error) {
	listing := s.store.Sponsors.GetSponsorsListingForAccount(sponsorableLogin)
	if listing == nil {
		return nil, 0, fmt.Errorf("%s does not have a GitHub Sponsors profile", sponsorableLogin)
	}
	amount, _ := intArg(input, "amount")
	if nodeID, _ := input["tierId"].(string); nodeID != "" {
		tier := s.store.Sponsors.FindSponsorsTierByNodeID(nodeID)
		if tier == nil || tier.ListingID != listing.ID {
			return nil, 0, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a SponsorsTier with the global id of '%s'.", nodeID)}
		}
		return tier, amount, nil
	}
	if amount <= 0 {
		return nil, 0, fmt.Errorf("a tierId or a positive amount in cents is required")
	}
	// No tier named: take the custom-amount tier when the maintainer
	// published one, else the dearest published tier the amount covers.
	var custom, best *store.SponsorsTier
	for _, tier := range s.store.Sponsors.ListSponsorsTiers(listing.ID, false) {
		if tier.IsOneTime == recurring {
			continue
		}
		if tier.IsCustomAmount && custom == nil {
			custom = tier
		}
		if tier.MonthlyPriceInCents <= amount && (best == nil || tier.MonthlyPriceInCents > best.MonthlyPriceInCents) {
			best = tier
		}
	}
	if custom != nil {
		return custom, amount, nil
	}
	if best == nil {
		return nil, 0, fmt.Errorf("no published tier matches that amount")
	}
	return best, amount, nil
}

// ---------------------------------------------------------------------------
// mutations

func (s *Resolver) addSponsorsMutations(mutationType *graphql.Object) {
	sponsorshipType := s.sponsorshipType()
	tierType := s.sponsorsTierType()
	listingType := s.sponsorsListingType()
	privacyEnum := s.sponsorshipPrivacyEnum()

	// createSponsorship — open a recurring or one-time sponsorship.
	createInput := sponsorsPartyInputFields()
	createInput["amount"] = &graphql.InputObjectFieldConfig{Type: graphql.Int}
	createInput["isRecurring"] = &graphql.InputObjectFieldConfig{Type: graphql.Boolean}
	createInput["privacyLevel"] = &graphql.InputObjectFieldConfig{Type: privacyEnum, DefaultValue: store.SponsorshipPrivacyPublic}
	createInput["receiveEmails"] = &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true}
	createInput["tierId"] = &graphql.InputObjectFieldConfig{Type: graphql.ID}
	s.registerMutation(mutationType, "createSponsorship", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "CreateSponsorshipPayload",
			Fields: graphql.Fields{
				"clientMutationId": &graphql.Field{Type: graphql.String},
				"sponsorship":      &graphql.Field{Type: sponsorshipType},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CreateSponsorshipInput", Fields: createInput,
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			sponsor, sponsorable, err := s.sponsorsResolveParties(p, input)
			if err != nil {
				return nil, err
			}
			recurring := true
			if value, ok := input["isRecurring"].(bool); ok {
				recurring = value
			}
			tier, amount, err := s.sponsorsTierForSponsorable(input, sponsorable.Login, recurring)
			if err != nil {
				return nil, err
			}
			privacy, _ := input["privacyLevel"].(string)
			receiveEmails, ok := input["receiveEmails"].(bool)
			if !ok {
				receiveEmails = true
			}
			transition, err := s.store.Sponsors.CreateSponsorship(store.SponsorshipInput{
				SponsorID: sponsor.ID, SponsorType: sponsor.Type, SponsorLogin: sponsor.Login,
				SponsorableID: sponsorable.ID, SponsorableType: sponsorable.Type, SponsorableLogin: sponsorable.Login,
				TierID: tier.ID, AmountInCents: amount, PrivacyLevel: privacy,
				ReceiveEmails: receiveEmails, IsRecurring: recurring,
			})
			if err != nil {
				return nil, err
			}
			s.emitSponsorshipEvent("created", transition, s.ghUserFromContext(p.Context))
			return map[string]interface{}{
				"clientMutationId": sponsorsClientMutationID(input),
				"sponsorship":      s.sponsorshipGQL(transition.Sponsorship),
			}, nil
		},
	})

	// createSponsorships — sponsor many maintainers in one call.
	bulkInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "BulkSponsorship",
		Fields: graphql.InputObjectConfigFieldMap{
			"amount":           &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)},
			"sponsorableId":    &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"sponsorableLogin": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	s.registerMutation(mutationType, "createSponsorships", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "CreateSponsorshipsPayload",
			Fields: graphql.Fields{
				"clientMutationId": &graphql.Field{Type: graphql.String},
				"sponsorables":     &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(s.sponsorableInterfaceType()))},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CreateSponsorshipsInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
				"privacyLevel":     &graphql.InputObjectFieldConfig{Type: privacyEnum, DefaultValue: store.SponsorshipPrivacyPublic},
				"receiveEmails":    &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
				"recurring":        &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
				"sponsorLogin":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
				"sponsorships":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(bulkInput)))},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			sponsorLogin, err := sponsorsTargetSponsorRequired(s, p, input)
			if err != nil {
				return nil, err
			}
			sponsor, _ := s.sponsorsAccount(sponsorLogin)
			recurring, _ := input["recurring"].(bool)
			privacy, _ := input["privacyLevel"].(string)
			receiveEmails, _ := input["receiveEmails"].(bool)
			rows, _ := input["sponsorships"].([]interface{})
			sender := s.ghUserFromContext(p.Context)
			sponsorables := []map[string]interface{}{}
			for _, raw := range rows {
				entry, _ := raw.(map[string]interface{})
				if entry == nil {
					continue
				}
				login := s.sponsorsAccountFromInput(entry, "sponsorableId", "sponsorableLogin")
				sponsorable, ok := s.sponsorsAccount(login)
				if !ok {
					return nil, &ghNotFoundError{message: "Could not resolve to a Sponsorable in the bulk sponsorship list."}
				}
				tier, amount, err := s.sponsorsTierForSponsorable(entry, sponsorable.Login, recurring)
				if err != nil {
					return nil, err
				}
				transition, err := s.store.Sponsors.CreateSponsorship(store.SponsorshipInput{
					SponsorID: sponsor.ID, SponsorType: sponsor.Type, SponsorLogin: sponsor.Login,
					SponsorableID: sponsorable.ID, SponsorableType: sponsorable.Type, SponsorableLogin: sponsorable.Login,
					TierID: tier.ID, AmountInCents: amount, PrivacyLevel: privacy,
					ReceiveEmails: receiveEmails, IsRecurring: recurring, ViaBulkSponsorship: true,
				})
				if err != nil {
					return nil, err
				}
				s.emitSponsorshipEvent("created", transition, sender)
				if account := s.sponsorsAccountGQL(sponsorable.Login); account != nil {
					sponsorables = append(sponsorables, account)
				}
			}
			return map[string]interface{}{
				"clientMutationId": sponsorsClientMutationID(input),
				"sponsorables":     sponsorables,
			}, nil
		},
	})

	// cancelSponsorship — stop funding a maintainer.
	s.registerMutation(mutationType, "cancelSponsorship", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "CancelSponsorshipPayload",
			Fields: graphql.Fields{
				"clientMutationId": &graphql.Field{Type: graphql.String},
				"sponsorsTier":     &graphql.Field{Type: tierType},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CancelSponsorshipInput", Fields: sponsorsPartyInputFields(),
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			sponsor, sponsorable, err := s.sponsorsResolveParties(p, input)
			if err != nil {
				return nil, err
			}
			sponsorship := s.store.Sponsors.GetSponsorshipBetween(sponsor.Login, sponsorable.Login, true)
			if sponsorship == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to an active sponsorship from '%s' to '%s'.", sponsor.Login, sponsorable.Login)}
			}
			transition, err := s.store.Sponsors.CancelSponsorship(sponsorship.ID)
			if err != nil {
				return nil, err
			}
			action := "cancelled"
			if transition.Sponsorship.PendingCancellation {
				action = "pending_cancellation"
			}
			s.emitSponsorshipEvent(action, transition, s.ghUserFromContext(p.Context))
			return map[string]interface{}{
				"clientMutationId": sponsorsClientMutationID(input),
				"sponsorsTier":     s.sponsorsTierGQL(transition.Tier),
			}, nil
		},
	})

	// updateSponsorshipPreferences — privacy and email settings.
	preferencesInput := sponsorsPartyInputFields()
	preferencesInput["privacyLevel"] = &graphql.InputObjectFieldConfig{Type: privacyEnum, DefaultValue: store.SponsorshipPrivacyPublic}
	preferencesInput["receiveEmails"] = &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true}
	s.registerMutation(mutationType, "updateSponsorshipPreferences", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "UpdateSponsorshipPreferencesPayload",
			Fields: graphql.Fields{
				"clientMutationId": &graphql.Field{Type: graphql.String},
				"sponsorship":      &graphql.Field{Type: sponsorshipType},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdateSponsorshipPreferencesInput", Fields: preferencesInput,
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			sponsor, sponsorable, err := s.sponsorsResolveParties(p, input)
			if err != nil {
				return nil, err
			}
			sponsorship := s.store.Sponsors.GetSponsorshipBetween(sponsor.Login, sponsorable.Login, false)
			if sponsorship == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a sponsorship from '%s' to '%s'.", sponsor.Login, sponsorable.Login)}
			}
			privacy, _ := input["privacyLevel"].(string)
			receiveEmails, ok := input["receiveEmails"].(bool)
			if !ok {
				receiveEmails = true
			}
			transition, err := s.store.Sponsors.UpdateSponsorshipPreferences(sponsorship.ID, privacy, receiveEmails)
			if err != nil {
				return nil, err
			}
			s.emitSponsorshipEvent("edited", transition, s.ghUserFromContext(p.Context))
			return map[string]interface{}{
				"clientMutationId": sponsorsClientMutationID(input),
				"sponsorship":      s.sponsorshipGQL(transition.Sponsorship),
			}, nil
		},
	})

	// createSponsorsListing — open a Sponsors profile.
	s.registerMutation(mutationType, "createSponsorsListing", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "CreateSponsorsListingPayload",
			Fields: graphql.Fields{
				"clientMutationId": &graphql.Field{Type: graphql.String},
				"sponsorsListing":  &graphql.Field{Type: listingType},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CreateSponsorsListingInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"clientMutationId":                &graphql.InputObjectFieldConfig{Type: graphql.String},
				"contactEmail":                    &graphql.InputObjectFieldConfig{Type: graphql.String},
				"fiscalHostLogin":                 &graphql.InputObjectFieldConfig{Type: graphql.String},
				"fiscallyHostedProjectProfileUrl": &graphql.InputObjectFieldConfig{Type: graphql.String},
				"fullDescription":                 &graphql.InputObjectFieldConfig{Type: graphql.String},
				"sponsorableLogin":                &graphql.InputObjectFieldConfig{Type: graphql.String},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			login, err := sponsorsTargetSponsorable(s, p, input)
			if err != nil {
				return nil, err
			}
			account, ok := s.sponsorsAccount(login)
			if !ok {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a Sponsorable with the login of '%s'.", login)}
			}
			contactEmail, _ := input["contactEmail"].(string)
			fiscalHost, _ := input["fiscalHostLogin"].(string)
			hostedURL, _ := input["fiscallyHostedProjectProfileUrl"].(string)
			fullDescription, _ := input["fullDescription"].(string)
			listing, err := s.store.Sponsors.CreateSponsorsListing(store.SponsorsListingInput{
				SponsorableID: account.ID, SponsorableType: account.Type, SponsorableLogin: account.Login,
				Name: account.Login, ShortDescription: fullDescription, FullDescription: fullDescription,
				ContactEmail: contactEmail, FiscalHostLogin: fiscalHost, FiscallyHostedProjectProfileURL: hostedURL,
			})
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId": sponsorsClientMutationID(input),
				"sponsorsListing":  s.sponsorsListingGQL(listing),
			}, nil
		},
	})

	// createSponsorsTier — add a price point to a Sponsors profile.
	s.registerMutation(mutationType, "createSponsorsTier", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "CreateSponsorsTierPayload",
			Fields: graphql.Fields{
				"clientMutationId": &graphql.Field{Type: graphql.String},
				"sponsorsTier":     &graphql.Field{Type: tierType},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CreateSponsorsTierInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"amount":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)},
				"clientMutationId":     &graphql.InputObjectFieldConfig{Type: graphql.String},
				"description":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
				"isRecurring":          &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true},
				"publish":              &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
				"repositoryId":         &graphql.InputObjectFieldConfig{Type: graphql.ID},
				"repositoryName":       &graphql.InputObjectFieldConfig{Type: graphql.String},
				"repositoryOwnerLogin": &graphql.InputObjectFieldConfig{Type: graphql.String},
				"sponsorableId":        &graphql.InputObjectFieldConfig{Type: graphql.ID},
				"sponsorableLogin":     &graphql.InputObjectFieldConfig{Type: graphql.String},
				"welcomeMessage":       &graphql.InputObjectFieldConfig{Type: graphql.String},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			login, err := sponsorsTargetSponsorable(s, p, input)
			if err != nil {
				return nil, err
			}
			listing := s.store.Sponsors.GetSponsorsListingForAccount(login)
			if listing == nil {
				return nil, fmt.Errorf("%s does not have a GitHub Sponsors profile", login)
			}
			amount, _ := intArg(input, "amount")
			description, _ := input["description"].(string)
			recurring := true
			if value, ok := input["isRecurring"].(bool); ok {
				recurring = value
			}
			publish, _ := input["publish"].(bool)
			welcome, _ := input["welcomeMessage"].(string)
			repositoryID := 0
			if nodeID, _ := input["repositoryId"].(string); nodeID != "" {
				if repo := store.FindRepoByNodeID(s.store, nodeID); repo != nil {
					repositoryID = repo.ID
				}
			} else if name, _ := input["repositoryName"].(string); name != "" {
				owner, _ := input["repositoryOwnerLogin"].(string)
				if owner == "" {
					owner = login
				}
				if repo := s.store.GetRepoByFullName(owner + "/" + name); repo != nil {
					repositoryID = repo.ID
				}
			}
			tier, err := s.store.Sponsors.CreateSponsorsTier(store.SponsorsTierInput{
				ListingID: listing.ID, Description: description, AmountInCents: amount,
				IsOneTime: !recurring, Publish: publish, WelcomeMessage: welcome, RepositoryID: repositoryID,
			})
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"clientMutationId": sponsorsClientMutationID(input),
				"sponsorsTier":     s.sponsorsTierGQL(tier),
			}, nil
		},
	})

	// publishSponsorsTier / retireSponsorsTier — the tier state machine.
	for _, mutation := range []struct {
		name    string
		payload string
		input   string
		apply   func(int) (*store.SponsorsTier, error)
	}{
		{"publishSponsorsTier", "PublishSponsorsTierPayload", "PublishSponsorsTierInput", s.store.Sponsors.PublishSponsorsTier},
		{"retireSponsorsTier", "RetireSponsorsTierPayload", "RetireSponsorsTierInput", s.store.Sponsors.RetireSponsorsTier},
	} {
		apply := mutation.apply
		s.registerMutation(mutationType, mutation.name, &graphql.Field{
			Type: graphql.NewObject(graphql.ObjectConfig{
				Name: mutation.payload,
				Fields: graphql.Fields{
					"clientMutationId": &graphql.Field{Type: graphql.String},
					"sponsorsTier":     &graphql.Field{Type: tierType},
				},
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
				Name: mutation.input,
				Fields: graphql.InputObjectConfigFieldMap{
					"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
					"tierId":           &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
			}))}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				input, _ := p.Args["input"].(map[string]interface{})
				nodeID, _ := input["tierId"].(string)
				tier := s.store.Sponsors.FindSponsorsTierByNodeID(nodeID)
				if tier == nil {
					return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a SponsorsTier with the global id of '%s'.", nodeID)}
				}
				updated, err := apply(tier.ID)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{
					"clientMutationId": sponsorsClientMutationID(input),
					"sponsorsTier":     s.sponsorsTierGQL(updated),
				}, nil
			},
		})
	}

	// updatePatreonSponsorability — whether Patreon sponsorships count.
	s.registerMutation(mutationType, "updatePatreonSponsorability", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "UpdatePatreonSponsorabilityPayload",
			Fields: graphql.Fields{
				"clientMutationId": &graphql.Field{Type: graphql.String},
				"sponsorsListing":  &graphql.Field{Type: listingType},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdatePatreonSponsorabilityInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"clientMutationId":          &graphql.InputObjectFieldConfig{Type: graphql.String},
				"enablePatreonSponsorships": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Boolean)},
				"sponsorableLogin":          &graphql.InputObjectFieldConfig{Type: graphql.String},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			login, err := sponsorsTargetSponsorable(s, p, input)
			if err != nil {
				return nil, err
			}
			listing := s.store.Sponsors.GetSponsorsListingForAccount(login)
			if listing == nil {
				return nil, fmt.Errorf("%s does not have a GitHub Sponsors profile", login)
			}
			enabled, _ := input["enablePatreonSponsorships"].(bool)
			updated := s.store.Sponsors.UpdateSponsorsListing(listing.ID, store.SponsorsListingUpdate{PatreonEnabled: &enabled})
			return map[string]interface{}{
				"clientMutationId": sponsorsClientMutationID(input),
				"sponsorsListing":  s.sponsorsListingGQL(updated),
			}, nil
		},
	})
}
