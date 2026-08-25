package graphqlapi

// The Organization account surface: profile members, the viewer's standing,
// the membership and team graph, and the governance records (IP allow list,
// rulesets, repository custom properties) the REST organization routes write.

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addOrganizationProfileFields installs the profile and viewer members of
// Organization.
func (s *Resolver) addOrganizationProfileFields(types *accountSurfaceTypes) {
	orgType := types.organization
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")

	orgString := func(read func(*store.Org) string) *graphql.Field {
		return &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				org, err := s.orgFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				return nilStr(read(org)), nil
			},
		}
	}

	orgType.AddFieldConfig("location", orgString(func(o *store.Org) string { return o.Location }))
	orgType.AddFieldConfig("twitterUsername", orgString(func(o *store.Org) string { return o.TwitterUsername }))
	orgType.AddFieldConfig("websiteUrl", &graphql.Field{
		Type: uri,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return nilStr(org.Blog), nil
		},
	})
	orgType.AddFieldConfig("descriptionHTML", &graphql.Field{
		// GitHub types this String (not HTML!) on Organization; the value is
		// still the description run through the one markdown pipeline.
		Type: graphql.String,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return nilStr(renderAccountMarkdown(org.Description)), nil
		},
	})
	orgType.AddFieldConfig("organizationBillingEmail", &graphql.Field{
		Type: graphql.String,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// The billing address is an owner-only member, exactly as the
			// REST organization shape gates `billing_email`.
			if !s.viewerCanAdminAccount(p.Context, org.Login) {
				return nil, nil
			}
			return nilStr(org.BillingEmail), nil
		},
	})

	orgType.AddFieldConfig("membersCanForkPrivateRepositories", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return org.MembersCanForkPrivateRepositories != nil && *org.MembersCanForkPrivateRepositories, nil
		},
	})
	orgType.AddFieldConfig("webCommitSignoffRequired", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return org.WebCommitSignoffRequired, nil
		},
	})
	orgType.AddFieldConfig("requiresTwoFactorAuthentication", &graphql.Field{
		Type: graphql.Boolean,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// The two-factor requirement on an organization is the governing
			// enterprise's policy, which is exactly what the REST
			// organization shape reports as two_factor_requirement_enabled.
			policy, _ := s.store.EnterprisePolicyForOrg(org.ID)
			return policy.TwoFactorRequired == store.EnterprisePolicyEnabled, nil
		},
	})
	orgType.AddFieldConfig("isVerified", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			// GitHub's verified badge means the organization has verified a
			// domain it owns. Domain verification on this instance is an
			// enterprise-level list, never an organization-level one, so no
			// organization here carries a verified domain of its own.
			return false, nil
		},
	})
	orgType.AddFieldConfig("archivedAt", &graphql.Field{
		Type: dateTime,
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			// bleephub never archives an organization account, so none has an
			// archived-at instant. (Repository archival, which it does model,
			// is Repository.archivedAt.)
			return nil, nil
		},
	})

	teamsPath := func(org *store.Org) string { return "/orgs/" + org.Login + "/teams" }
	orgPath := func(build func(*store.Org) string, absolute bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				org, err := s.orgFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				path := build(org)
				if absolute {
					return externalURL(path), nil
				}
				return path, nil
			},
		}
	}
	orgType.AddFieldConfig("teamsResourcePath", orgPath(teamsPath, false))
	orgType.AddFieldConfig("teamsUrl", orgPath(teamsPath, true))
	orgType.AddFieldConfig("newTeamResourcePath", orgPath(func(o *store.Org) string { return teamsPath(o) + "/new" }, false))
	orgType.AddFieldConfig("newTeamUrl", orgPath(func(o *store.Org) string { return teamsPath(o) + "/new" }, true))

	orgType.AddFieldConfig("interactionAbility", &graphql.Field{
		Type: s.gqlInteractionAbilityType(types),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			limit := s.store.GetOrgInteractionLimit(org.Login)
			if limit == nil {
				return nil, nil
			}
			expiry := limit.ExpiresAt
			return optionalObject(s.interactionAbilitySource(limit.Limit, &expiry, "ORGANIZATION")), nil
		},
	})

	// --- viewer standing ---------------------------------------------------
	orgBool := func(decide func(p graphql.ResolveParams, org *store.Org) bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				org, err := s.orgFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				return decide(p, org), nil
			},
		}
	}
	orgType.AddFieldConfig("viewerCanAdminister", orgBool(func(p graphql.ResolveParams, org *store.Org) bool {
		return s.viewerCanAdminAccount(p.Context, org.Login)
	}))
	orgType.AddFieldConfig("viewerIsAMember", orgBool(func(p graphql.ResolveParams, org *store.Org) bool {
		return s.viewerIsOrgMember(p.Context, org.Login)
	}))
	orgType.AddFieldConfig("viewerCanCreateTeams", orgBool(func(p graphql.ResolveParams, org *store.Org) bool {
		if s.viewerCanAdminAccount(p.Context, org.Login) {
			return true
		}
		if !s.viewerIsOrgMember(p.Context, org.Login) {
			return false
		}
		// nil is GitHub's default of true for the member-privilege toggles.
		return org.MembersCanCreateTeams == nil || *org.MembersCanCreateTeams
	}))
	orgType.AddFieldConfig("viewerCanCreateRepositories", orgBool(func(p graphql.ResolveParams, org *store.Org) bool {
		if s.viewerCanAdminAccount(p.Context, org.Login) {
			return true
		}
		if !s.viewerIsOrgMember(p.Context, org.Login) {
			return false
		}
		return org.MembersCanCreateRepositories == nil || *org.MembersCanCreateRepositories
	}))
	orgType.AddFieldConfig("viewerCanCreateProjects", orgBool(func(p graphql.ResolveParams, org *store.Org) bool {
		return s.viewerIsOrgMember(p.Context, org.Login) || s.viewerCanAdminAccount(p.Context, org.Login)
	}))
	orgType.AddFieldConfig("viewerIsFollowing", orgBool(func(p graphql.ResolveParams, org *store.Org) bool {
		viewer := s.ghUserFromContext(p.Context)
		return viewer != nil && s.store.LoginFollows(viewer.Login, org.Login)
	}))

	// The account-completion members (packages, memberStatuses, issueTypes,
	// domains, enterpriseOwners, recentProjects and the null-answering
	// announcementBanner / samlIdentityProvider) live in
	// gh_users_org_fields_graphql.go.
	s.addOrganizationCompletionFields(types)
}

// addOrganizationPeopleFields installs Organization's membership and team
// connections.
func (s *Resolver) addOrganizationPeopleFields(types *accountSurfaceTypes) {
	orgType := types.organization

	memberConnection := s.accountConnectionType(types, "OrganizationMember", types.user, false, graphql.Fields{
		"role": &graphql.Field{
			Type: s.sharedEnum("OrganizationMemberRole", "ADMIN", "MEMBER"),
		},
		"hasTwoFactorEnabled": &graphql.Field{Type: graphql.Boolean},
	})
	orgType.AddFieldConfig("membersWithRole", &graphql.Field{
		Type: graphql.NewNonNull(memberConnection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// A non-member sees only the members who publicized their
			// membership; a member sees the whole roster. Answering the full
			// list to an outsider would leak private membership.
			members := s.organizationVisibleMembers(p.Context, org)
			viewerIsAdmin := s.viewerCanAdminAccount(p.Context, org.Login)
			items := make([]gqlConnItem, 0, len(members))
			for i := range members {
				member := members[i]
				role := "MEMBER"
				if membership := s.store.GetMembership(org.Login, member.ID); membership != nil &&
					membership.Role == store.OrgRoleAdmin {
					role = "ADMIN"
				}
				// Whether an account has two-factor authentication on is
				// owner-only information; GitHub types the edge member
				// nullable for exactly that reason.
				var twoFactor interface{}
				if viewerIsAdmin {
					twoFactor = member.TwoFactor != nil
				}
				items = append(items, gqlConnItem{
					identity: member.NodeID,
					render: func() map[string]interface{} {
						node := userToGraphQL(member)
						node["_role"] = role
						node["_hasTwoFactorEnabled"] = twoFactor
						return node
					},
				})
			}
			connection := paginateGQLItems(items, p.Args)
			withEdgeMember(connection, "role", "_role")
			withEdgeMember(connection, "hasTwoFactorEnabled", "_hasTwoFactorEnabled")
			return connection, nil
		},
	})

	orgType.AddFieldConfig("pendingMembers", &graphql.Field{
		Type: graphql.NewNonNull(types.userConnection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// Who has been invited but not yet joined is administrative
			// information.
			if !s.viewerCanAdminAccount(p.Context, org.Login) {
				return paginateGQLItems(nil, p.Args), nil
			}
			var pending []*store.User
			for _, invitation := range s.store.ListPendingOrgInvitations(org.Login) {
				if invitation.UserID == 0 {
					continue
				}
				if user := s.store.GetUserByID(invitation.UserID); user != nil {
					pending = append(pending, user)
				}
			}
			return paginateGQLItems(userConnectionItems(sortedUsersByLogin(pending)), p.Args), nil
		},
	})

	orgType.AddFieldConfig("team", &graphql.Field{
		Type: s.graphqlTypes.team,
		Args: graphql.FieldConfigArgument{
			"slug": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			slug, _ := p.Args["slug"].(string)
			team := s.store.GetTeam(org.Login, slug)
			if team == nil || !s.viewerCanSeeTeam(p.Context, org, team) {
				return nil, nil
			}
			return s.teamSource(team, org), nil
		},
	})

	orgType.AddFieldConfig("teams", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlTeamConnectionType()),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy":       &graphql.ArgumentConfig{Type: s.gqlTeamOrderInput(types)},
			"privacy":       &graphql.ArgumentConfig{Type: s.sharedEnum("TeamPrivacy", "SECRET", "VISIBLE")},
			"query":         &graphql.ArgumentConfig{Type: graphql.String},
			"role":          &graphql.ArgumentConfig{Type: s.sharedEnum("TeamRole", "ADMIN", "MEMBER")},
			"rootTeamsOnly": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
			"userLogins":    &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"notificationSetting": &graphql.ArgumentConfig{Type: s.sharedEnum("TeamNotificationSetting",
				"NOTIFICATIONS_DISABLED", "NOTIFICATIONS_ENABLED")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			teams := s.organizationVisibleTeams(p.Context, org, p.Args)
			return paginateGQLItems(s.teamConnectionItems(teams, org), p.Args), nil
		},
	})
}

// addOrganizationGovernanceFields installs the governance records an
// organization owns: its IP allow list, rulesets and repository custom
// properties.
func (s *Resolver) addOrganizationGovernanceFields(types *accountSurfaceTypes) {
	orgType := types.organization

	orgType.AddFieldConfig("ipAllowListEnabledSetting", &graphql.Field{
		Type: graphql.NewNonNull(s.ipAllowListEnabledEnum()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			policy, _ := s.store.EnterprisePolicyForOrg(org.ID)
			if policy.IPAllowListEnabled == store.EnterprisePolicyEnabled {
				return "ENABLED", nil
			}
			return "DISABLED", nil
		},
	})
	orgType.AddFieldConfig("ipAllowListForInstalledAppsEnabledSetting", &graphql.Field{
		Type: graphql.NewNonNull(s.ipAllowListForInstalledAppsEnum()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			policy, _ := s.store.EnterprisePolicyForOrg(org.ID)
			if policy.IPAllowListForInstalledAppsEnabled == store.EnterprisePolicyEnabled {
				return "ENABLED", nil
			}
			return "DISABLED", nil
		},
	})
	orgType.AddFieldConfig("notificationDeliveryRestrictionEnabledSetting", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("NotificationRestrictionSettingValue", "DISABLED", "ENABLED")),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// The enterprise's restriction governs every organization under
			// it — the same value the notification delivery gate enforces per
			// address — and an organization may switch its own on
			// independently, so either being on restricts delivery.
			if enabled, _ := s.store.NotificationDeliveryRestriction(); enabled {
				return "ENABLED", nil
			}
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if org.NotificationDeliveryRestrictionEnabled {
				return "ENABLED", nil
			}
			return "DISABLED", nil
		},
	})

	if types.ipAllowListEntryConnection != nil {
		orgType.AddFieldConfig("ipAllowListEntries", &graphql.Field{
			Type: graphql.NewNonNull(types.ipAllowListEntryConnection),
			Args: connectionArgs(graphql.FieldConfigArgument{
				"orderBy": &graphql.ArgumentConfig{
					Type: s.gqlOrderInput(types, "IpAllowListEntryOrder", "IpAllowListEntryOrderField",
						"ALLOW_LIST_VALUE", "CREATED_AT"),
				},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				org, err := s.orgFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				// The allow list is the organization's network policy; only an
				// owner may read it.
				if !s.viewerCanAdminAccount(p.Context, org.Login) {
					return paginateGQLItems(nil, p.Args), nil
				}
				entries := s.store.ListIPAllowListEntries("Organization", org.ID)
				byValue := orderField(p.Args, "orderBy", "ALLOW_LIST_VALUE") == "ALLOW_LIST_VALUE"
				descending := orderDirectionDescending(p.Args, "orderBy", false)
				sort.Slice(entries, func(i, j int) bool {
					less := entries[i].ID < entries[j].ID
					if byValue {
						less = entries[i].AllowListValue < entries[j].AllowListValue
					}
					if descending {
						return !less
					}
					return less
				})
				items := make([]gqlConnItem, 0, len(entries))
				for i := range entries {
					entry := entries[i]
					items = append(items, gqlConnItem{
						identity: entry.NodeID,
						render: func() map[string]interface{} {
							node := ipAllowListEntryToGraphQL(entry)
							node["owner"] = orgOwnerSource(org)
							return node
						},
					})
				}
				return paginateGQLItems(items, p.Args), nil
			},
		})
	}

	if types.rulesetConnection != nil {
		targetEnum := s.sharedEnum("RepositoryRulesetTarget", "BRANCH", "PUSH", "REPOSITORY", "TAG")
		orgType.AddFieldConfig("rulesets", &graphql.Field{
			Type: types.rulesetConnection,
			Args: connectionArgs(graphql.FieldConfigArgument{
				"includeParents": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
				"targets":        &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(targetEnum))},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				org, err := s.orgFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				// A ruleset states what the organization enforces on its
				// repositories; membership is what GitHub requires to read it.
				if !s.viewerIsOrgMember(p.Context, org.Login) && !s.viewerCanAdminAccount(p.Context, org.Login) {
					return nil, nil
				}
				wanted := map[string]bool{}
				for _, target := range stringListArg(p.Args["targets"]) {
					wanted[target] = true
				}
				rulesets := s.store.ListOrgRulesets(org.ID)
				sort.Slice(rulesets, func(i, j int) bool { return rulesets[i].ID < rulesets[j].ID })
				items := make([]gqlConnItem, 0, len(rulesets))
				for i := range rulesets {
					ruleset := rulesets[i]
					node := s.orgRulesetSource(ruleset, org)
					if len(wanted) > 0 {
						target, _ := node["target"].(string)
						if !wanted[target] {
							continue
						}
					}
					items = append(items, gqlConnItem{
						identity: ruleset.NodeID,
						render:   func() map[string]interface{} { return node },
					})
				}
				return paginateGQLItems(items, p.Args), nil
			},
		})
		orgType.AddFieldConfig("ruleset", &graphql.Field{
			Type: types.ruleset,
			Args: graphql.FieldConfigArgument{
				"databaseId":     &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
				"includeParents": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				org, err := s.orgFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				if !s.viewerIsOrgMember(p.Context, org.Login) && !s.viewerCanAdminAccount(p.Context, org.Login) {
					return nil, nil
				}
				databaseID, _ := intArg(p.Args, "databaseId")
				for _, ruleset := range s.store.ListOrgRulesets(org.ID) {
					if ruleset.ID == databaseID {
						return s.orgRulesetSource(ruleset, org), nil
					}
				}
				return nil, nil
			},
		})
	}

	// --- repository custom properties -------------------------------------
	customProperty := s.gqlRepositoryCustomPropertyType()
	orgType.AddFieldConfig("repositoryCustomProperties", &graphql.Field{
		Type: s.accountConnectionType(types, "RepositoryCustomProperty", customProperty, false, nil),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerIsOrgMember(p.Context, org.Login) && !s.viewerCanAdminAccount(p.Context, org.Login) {
				return nil, nil
			}
			properties := s.store.ListCustomProperties(org.Login)
			sort.Slice(properties, func(i, j int) bool { return properties[i].PropertyName < properties[j].PropertyName })
			items := make([]gqlConnItem, 0, len(properties))
			for i := range properties {
				property := properties[i]
				items = append(items, gqlConnItem{
					identity: customPropertyIdentity(org, property),
					render:   func() map[string]interface{} { return s.repositoryCustomPropertySource(org, property) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
	orgType.AddFieldConfig("repositoryCustomProperty", &graphql.Field{
		Type: customProperty,
		Args: graphql.FieldConfigArgument{
			"propertyName": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerIsOrgMember(p.Context, org.Login) && !s.viewerCanAdminAccount(p.Context, org.Login) {
				return nil, nil
			}
			name, _ := p.Args["propertyName"].(string)
			property := s.store.GetCustomProperty(org.Login, name)
			if property == nil {
				return nil, nil
			}
			return s.repositoryCustomPropertySource(org, property), nil
		},
	})
}

// gqlRepositoryCustomPropertyType returns the shared RepositoryCustomProperty
// object (memoized): the org read surface lists it and the custom-property
// mutation payloads return it. Its `source` union member is added by the
// mutation family, once the Enterprise type it names exists.
func (s *Resolver) gqlRepositoryCustomPropertyType() *graphql.Object {
	return s.mutationObject("RepositoryCustomProperty", graphql.Fields{
		"allowedValues":         &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
		"defaultValue":          &graphql.Field{Type: s.graphQLStringScalar("CustomPropertyValue")},
		"description":           &graphql.Field{Type: graphql.String},
		"id":                    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"propertyName":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"regex":                 &graphql.Field{Type: graphql.String},
		"requireExplicitValues": &graphql.Field{Type: graphql.Boolean},
		"required":              &graphql.Field{Type: graphql.Boolean},
		"valueType": &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum("CustomPropertyValueType",
			"MULTI_SELECT", "SINGLE_SELECT", "STRING", "TRUE_FALSE", "URL"))},
		"valuesEditableBy": &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum(
			"RepositoryCustomPropertyValuesEditableBy", "ORG_ACTORS", "ORG_AND_REPO_ACTORS"))},
	})
}

// repositoryCustomPropertySource renders a definition reached through an
// organization, naming the enterprise as its source when the organization
// merely inherits it.
func (s *Resolver) repositoryCustomPropertySource(org *store.Org, property *store.CustomProperty) map[string]interface{} {
	source := customPropertySource(org, property)
	if !s.store.OrgOwnsCustomProperty(org.Login, property.PropertyName) {
		if e := s.store.GetEnterprise(s.store.PrimaryEnterpriseSlug()); e != nil {
			source["source"] = enterpriseToGraphQL(e)
			return source
		}
	}
	source["source"] = orgOwnerSource(org)
	return source
}

// enterpriseCustomPropertySource renders a definition created directly at
// enterprise scope.
func (s *Resolver) enterpriseCustomPropertySource(e *store.Enterprise, property *store.CustomProperty) map[string]interface{} {
	source := customPropertySource(nil, property)
	source["id"] = "RCP_" + e.Slug + "/" + property.PropertyName
	source["source"] = enterpriseToGraphQL(e)
	return source
}

// orgOwnerSource renders the organization as the IpAllowListOwner union
// member it is.
func orgOwnerSource(org *store.Org) map[string]interface{} {
	source := orgToGraphQL(org)
	source["__typename"] = "Organization"
	return source
}

// orgRulesetSource renders an organization ruleset with the organization as
// its `source`, which is how GitHub labels a ruleset configured on the
// organization rather than on one repository.
func (s *Resolver) orgRulesetSource(ruleset *store.Ruleset, org *store.Org) map[string]interface{} {
	rules := make([]map[string]interface{}, 0, len(ruleset.Rules))
	for i, rule := range ruleset.Rules {
		rules = append(rules, map[string]interface{}{
			"id":   "RR_" + strconv.Itoa(ruleset.ID) + "_" + strconv.Itoa(i),
			"type": strings.ToUpper(rule.Type),
		})
	}
	return map[string]interface{}{
		"nodeID":      ruleset.NodeID,
		"id":          ruleset.NodeID,
		"databaseId":  ruleset.ID,
		"name":        ruleset.Name,
		"target":      strings.ToUpper(ruleset.Target),
		"enforcement": strings.ToUpper(ruleset.Enforcement),
		"createdAt":   ruleset.CreatedAt.UTC().Format(rfc3339),
		"updatedAt":   ruleset.UpdatedAt.UTC().Format(rfc3339),
		"source":      orgOwnerSource(org),
		"rules":       paginateGQLMaps(rules, map[string]interface{}{}),
	}
}

// customPropertySource renders one organization custom-property definition.
func customPropertySource(org *store.Org, property *store.CustomProperty) map[string]interface{} {
	allowed := interface{}(nil)
	if property.AllowedValues != nil {
		allowed = append([]string(nil), property.AllowedValues...)
	}
	identity := ""
	if org != nil {
		identity = customPropertyIdentity(org, property)
	}
	return map[string]interface{}{
		"id":                    identity,
		"propertyName":          property.PropertyName,
		"description":           nilStrPtr(property.Description),
		"valueType":             strings.ToUpper(property.ValueType),
		"required":              property.Required,
		"defaultValue":          customPropertyDefaultValue(property),
		"allowedValues":         allowed,
		"regex":                 nilStrPtr(property.Regex),
		"requireExplicitValues": property.RequireExplicitValues,
		"valuesEditableBy":      customPropertyEditableBy(property.ValuesEditableBy),
	}
}

// customPropertyIdentity is the node id for a property definition, which the
// store keys by (organization, name) rather than a numeric row id.
func customPropertyIdentity(org *store.Org, property *store.CustomProperty) string {
	return "RCP_" + org.Login + "/" + property.PropertyName
}

// customPropertyDefaultValue renders the definition's default, which is a
// single value for the scalar types and a list for MULTI_SELECT.
func customPropertyDefaultValue(property *store.CustomProperty) interface{} {
	if property.DefaultValue == nil {
		return nil
	}
	return property.DefaultValue
}

// customPropertyEditableBy maps the stored editability onto GitHub's enum.
func customPropertyEditableBy(value string) string {
	if strings.EqualFold(value, "org_actors") {
		return "ORG_ACTORS"
	}
	return "ORG_AND_REPO_ACTORS"
}

// organizationVisibleMembers is the roster the request may see: the whole
// membership for a member or owner, only the publicized memberships for
// anyone else.
func (s *Resolver) organizationVisibleMembers(ctx context.Context, org *store.Org) []*store.User {
	if s.viewerIsOrgMember(ctx, org.Login) || s.viewerCanAdminAccount(ctx, org.Login) {
		return sortedUsersByLogin(s.store.ListOrgMembers(org.Login))
	}
	return sortedUsersByLogin(s.store.ListPublicOrgMembers(org.Login))
}

// viewerCanSeeTeam reports whether the request may see a team at all. A secret
// team is invisible outside the organization and outside its own membership,
// which is what makes it secret.
func (s *Resolver) viewerCanSeeTeam(ctx context.Context, org *store.Org, team *store.Team) bool {
	if team.Privacy != store.TeamPrivacySecret {
		return s.viewerIsOrgMember(ctx, org.Login) || s.viewerCanAdminAccount(ctx, org.Login)
	}
	if s.viewerCanAdminAccount(ctx, org.Login) {
		return true
	}
	viewer := s.ghUserFromContext(ctx)
	if viewer == nil {
		return false
	}
	_, member := s.store.GetTeamMembership(org.Login, team.Slug, viewer.ID)
	return member
}

// organizationVisibleTeams applies GitHub's team filters to the teams the
// request may see.
func (s *Resolver) organizationVisibleTeams(ctx context.Context, org *store.Org, args map[string]interface{}) []*store.Team {
	privacy, _ := args["privacy"].(string)
	query, _ := args["query"].(string)
	role, _ := args["role"].(string)
	rootOnly, _ := args["rootTeamsOnly"].(bool)
	userLogins := stringListArg(args["userLogins"])
	notification, _ := args["notificationSetting"].(string)
	viewer := s.ghUserFromContext(ctx)

	var out []*store.Team
	for _, team := range s.store.ListTeams(org.Login) {
		if !s.viewerCanSeeTeam(ctx, org, team) {
			continue
		}
		if privacy != "" && teamPrivacyEnum(team.Privacy) != privacy {
			continue
		}
		if notification != "" && teamNotificationEnum(team.NotificationSetting) != notification {
			continue
		}
		if rootOnly && team.ParentID != 0 {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(team.Name), strings.ToLower(query)) &&
			!strings.Contains(strings.ToLower(team.Slug), strings.ToLower(query)) {
			continue
		}
		if role != "" {
			if viewer == nil {
				continue
			}
			memberRole, ok := s.store.GetTeamMembership(org.Login, team.Slug, viewer.ID)
			if !ok {
				continue
			}
			// GitHub's TeamRole is ADMIN for a maintainer, MEMBER otherwise.
			want := "MEMBER"
			if memberRole == store.TeamRoleMaintainer {
				want = "ADMIN"
			}
			if want != role {
				continue
			}
		}
		if len(userLogins) > 0 && !teamHasAnyMember(s.store, org.Login, team, userLogins) {
			continue
		}
		out = append(out, team)
	}
	return out
}

// teamHasAnyMember reports whether any of the named accounts belongs to team.
func teamHasAnyMember(st *store.Store, orgLogin string, team *store.Team, logins []string) bool {
	for _, login := range logins {
		user := st.LookupUserByLogin(login)
		if user == nil {
			continue
		}
		if _, ok := st.GetTeamMembership(orgLogin, team.Slug, user.ID); ok {
			return true
		}
	}
	return false
}
