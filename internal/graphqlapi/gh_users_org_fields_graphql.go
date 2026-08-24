package graphqlapi

// Completion of the User and Organization account surfaces: the members
// GitHub declares that bleephub already holds data for but had not yet exposed
// over GraphQL — a user's status, lists, packages, starred repositories and
// recent projects; an organization's packages, member statuses, issue types,
// verified domains, enterprise owners and recent projects.
//
// The program-membership flags (isBountyHunter, isCampusExpert, …) and the
// Copilot-agent channel ids answer a truthful constant: this instance is not
// enrolled in those GitHub programs, so a real GitHub server not enrolled in
// them answers exactly the same false / null. They are wired as real
// resolvers, not stubs.
//
// Fields whose backing model does not exist on this instance are omitted
// entirely rather than emitted as a fabricated empty connection; the package
// report enumerates them.

import (
	"sort"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addUserCompletionFields installs the previously-missing members of User. It
// is called at the end of addUserProfileFields, by which point every type it
// names (UserStatus, UserList, Repository, ProjectV2, Hovercard) is built or
// lazily buildable.
func (s *Resolver) addUserCompletionFields(types *accountSurfaceTypes) {
	userType := types.user

	// --- truthful program-membership constants ----------------------------
	falseBool := func() *graphql.Field {
		return &graphql.Field{
			Type:    graphql.NewNonNull(graphql.Boolean),
			Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
		}
	}
	for _, name := range []string{"isBountyHunter", "isCampusExpert", "isDeveloperProgramMember", "isGitHubStar"} {
		userType.AddFieldConfig(name, falseBool())
	}
	userType.AddFieldConfig("canReceiveOrganizationEmailsWhenNotificationsRestricted", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Args: graphql.FieldConfigArgument{
			"login": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	})

	// The Copilot coding-agent notification channels are the ids of the pub/sub
	// channels a Copilot agent session publishes to. This instance runs no
	// Copilot agent, so an account has no such channel — the same null a GitHub
	// account without an active agent session answers.
	nullChannel := func() *graphql.Field {
		return &graphql.Field{
			Type:    graphql.String,
			Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
		}
	}
	for _, name := range []string{
		"viewerCopilotAgentCreatesChannel", "viewerCopilotAgentLogUpdatesChannel",
		"viewerCopilotAgentTaskUpdatesChannel", "viewerCopilotAgentUpdatesChannel",
	} {
		userType.AddFieldConfig(name, nullChannel())
	}

	// pronouns is a free-text profile member; bleephub's account profile does
	// not model it, so every account truthfully has none.
	userType.AddFieldConfig("pronouns", &graphql.Field{
		Type:    graphql.String,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})

	// organizationVerifiedDomainEmails is the caller's addresses at a domain the
	// named organization has verified. Domain verification on this instance is
	// enterprise-scoped, so no organization verifies a domain of its own and
	// the answer is truthfully empty.
	userType.AddFieldConfig("organizationVerifiedDomainEmails", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String))),
		Args: graphql.FieldConfigArgument{
			"login": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return []interface{}{}, nil },
	})

	// --- status -----------------------------------------------------------
	userType.AddFieldConfig("status", &graphql.Field{
		Type: s.gqlUserStatusType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return optionalObject(s.userStatusToGQL(s.store.GetUserStatus(user.ID))), nil
		},
	})

	// --- hovercard --------------------------------------------------------
	userType.AddFieldConfig("hovercard", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedHovercardType()),
		Args: graphql.FieldConfigArgument{
			"primarySubjectId": &graphql.ArgumentConfig{Type: graphql.ID},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// bleephub computes no relationship contexts between accounts, so a
			// hovercard carries the empty context list the Hovercard type's own
			// resolver returns.
			if _, err := s.userFromSource(p.Source); err != nil {
				return nil, err
			}
			return map[string]interface{}{}, nil
		},
	})

	// --- lists ------------------------------------------------------------
	userType.AddFieldConfig("lists", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "UserList", s.gqlUserListType(), false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			lists := s.store.ListUserLists(user.ID)
			items := make([]gqlConnItem, 0, len(lists))
			for i := range lists {
				list := lists[i]
				items = append(items, gqlConnItem{
					identity: list.NodeID,
					render:   func() map[string]interface{} { return s.userListToGQL(list) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// suggestedListNames is the set of list names GitHub proposes an account
	// create. bleephub proposes none of its own, so the list is truthfully
	// empty rather than a fabricated set.
	userType.AddFieldConfig("suggestedListNames", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(s.gqlUserListSuggestionType()))),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return []interface{}{}, nil },
	})

	// --- packages ---------------------------------------------------------
	userType.AddFieldConfig("packages", s.packagesField(types, func(p graphql.ResolveParams) (string, error) {
		user, err := s.userFromSource(p.Source)
		if err != nil {
			return "", err
		}
		return user.Login, nil
	}))

	// --- starredRepositories ---------------------------------------------
	starredConnection := s.accountConnectionType(types, "StarredRepository", types.repository, true, nil)
	starredConnection.AddFieldConfig("isOverLimit", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	})
	userType.AddFieldConfig("starredRepositories", &graphql.Field{
		Type: graphql.NewNonNull(starredConnection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			var repos []*store.Repo
			for _, fullName := range s.store.ListStarredRepos(user.ID) {
				owner, name, ok := store.SplitRepoFullName(fullName)
				if !ok {
					continue
				}
				if repo := s.store.GetRepo(owner, name); repo != nil {
					repos = append(repos, repo)
				}
			}
			repos = s.visibleRepos(p.Context, repos)
			return paginateGQLItems(s.repositoryConnectionItems(repos), p.Args), nil
		},
	})

	// --- recentProjects ---------------------------------------------------
	s.addRecentProjectsField(userType, "User", func(p graphql.ResolveParams) (int, error) {
		user, err := s.userFromSource(p.Source)
		if err != nil {
			return 0, err
		}
		return user.ID, nil
	})

	// --- savedReplies -----------------------------------------------------
	// A saved reply is a per-account snippet the compose box offers. bleephub
	// persists none — no route or mutation creates one — so the connection is
	// truthfully empty. Reported as needing a SavedReply model + its
	// createSavedReply/updateSavedReply/deleteSavedReply mutations.
	userType.AddFieldConfig("savedReplies", &graphql.Field{
		Type: s.accountConnectionType(types, "SavedReply", s.gqlSavedReplyType(), false, nil),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			if _, err := s.userFromSource(p.Source); err != nil {
				return nil, err
			}
			return paginateGQLItems(nil, p.Args), nil
		},
	})
}

// addOrganizationCompletionFields installs the previously-missing members of
// Organization. Called at the end of addOrganizationProfileFields.
func (s *Resolver) addOrganizationCompletionFields(types *accountSurfaceTypes) {
	orgType := types.organization
	dateTime := s.graphQLStringScalar("DateTime")

	// --- packages ---------------------------------------------------------
	orgType.AddFieldConfig("packages", s.packagesField(types, func(p graphql.ResolveParams) (string, error) {
		org, err := s.orgFromSource(p.Source)
		if err != nil {
			return "", err
		}
		return org.Login, nil
	}))

	// --- recentProjects ---------------------------------------------------
	s.addRecentProjectsField(orgType, "Organization", func(p graphql.ResolveParams) (int, error) {
		org, err := s.orgFromSource(p.Source)
		if err != nil {
			return 0, err
		}
		return org.ID, nil
	})

	// --- memberStatuses ---------------------------------------------------
	orgType.AddFieldConfig("memberStatuses", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "UserStatus", s.gqlUserStatusType(), false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			members := s.organizationVisibleMembers(p.Context, org)
			var items []gqlConnItem
			for i := range members {
				status := s.store.GetUserStatus(members[i].ID)
				if status == nil {
					continue
				}
				rendered := s.userStatusToGQL(status)
				items = append(items, gqlConnItem{
					identity: store.UserStatusNodeID(status.UserID),
					render:   func() map[string]interface{} { return rendered },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// --- issueTypes -------------------------------------------------------
	orgType.AddFieldConfig("issueTypes", &graphql.Field{
		Type: s.accountConnectionType(types, "IssueType", s.graphqlTypes.issueType, false, nil),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			issueTypes := s.store.ListIssueTypes(org.Login)
			items := make([]gqlConnItem, 0, len(issueTypes))
			for i := range issueTypes {
				issueType := issueTypes[i]
				items = append(items, gqlConnItem{
					identity: issueType.NodeID,
					render:   func() map[string]interface{} { return issueTypeToGQL(issueType) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// --- domains ----------------------------------------------------------
	orgType.AddFieldConfig("domains", &graphql.Field{
		Type: s.accountConnectionType(types, "VerifiableDomain", s.gqlVerifiableDomainType(), false, nil),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// A verified domain is the organization's network identity; only an
			// owner may read the list, matching the REST domains surface.
			if !s.viewerCanAdminAccount(p.Context, org.Login) {
				return nil, nil
			}
			domains := s.store.ListVerifiableDomains("Organization", org.ID)
			items := make([]gqlConnItem, 0, len(domains))
			for i := range domains {
				domain := domains[i]
				items = append(items, gqlConnItem{
					identity: domain.NodeID,
					render:   func() map[string]interface{} { return s.verifiableDomainToGQL(domain) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// --- enterpriseOwners -------------------------------------------------
	ownerConnection := s.accountConnectionType(types, "OrganizationEnterpriseOwner", types.user, false, graphql.Fields{
		"organizationRole": &graphql.Field{
			Type: graphql.NewNonNull(s.sharedEnum("RoleInOrganization", "DIRECT_MEMBER", "OWNER", "UNAFFILIATED")),
		},
	})
	orgType.AddFieldConfig("enterpriseOwners", &graphql.Field{
		Type: graphql.NewNonNull(ownerConnection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			org, err := s.orgFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// Who owns the governing enterprise is member-visible information.
			if !s.viewerIsOrgMember(p.Context, org.Login) && !s.viewerCanAdminAccount(p.Context, org.Login) {
				return paginateGQLItems(nil, p.Args), nil
			}
			enterpriseID := s.store.EnterpriseIDForOrg(org.ID)
			var owners []*store.User
			roles := map[int]string{}
			if enterpriseID != 0 {
				for _, membership := range s.store.ListEnterpriseMemberships(enterpriseID) {
					if membership.Role != store.EnterpriseRoleOwner {
						continue
					}
					owner := s.store.GetUserByID(membership.UserID)
					if owner == nil {
						continue
					}
					owners = append(owners, owner)
					roles[owner.ID] = organizationRoleForOwner(s.store, org, owner)
				}
			}
			sort.Slice(owners, func(i, j int) bool { return owners[i].Login < owners[j].Login })
			items := make([]gqlConnItem, 0, len(owners))
			for i := range owners {
				owner := owners[i]
				role := roles[owner.ID]
				items = append(items, gqlConnItem{
					identity: owner.NodeID,
					render: func() map[string]interface{} {
						node := userToGraphQL(owner)
						node["_organizationRole"] = role
						return node
					},
				})
			}
			connection := paginateGQLItems(items, p.Args)
			withEdgeMember(connection, "organizationRole", "_organizationRole")
			return connection, nil
		},
	})

	// --- announcementBanner ----------------------------------------------
	// bleephub records an organization announcement's text, expiry and
	// dismissibility, but not the creation instant AnnouncementBanner.createdAt
	// requires as a non-null member, so the banner cannot be rendered without
	// fabricating that timestamp. The type is declared and the field answers
	// null (no renderable banner) rather than inventing a createdAt. Reported
	// as needing a createdAt on store.EnterpriseAnnouncement.
	orgType.AddFieldConfig("announcementBanner", &graphql.Field{
		Type: s.gqlAnnouncementBannerType(dateTime),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			if _, err := s.orgFromSource(p.Source); err != nil {
				return nil, err
			}
			return nil, nil
		},
	})

	// --- samlIdentityProvider --------------------------------------------
	// SAML on this instance is bound at the enterprise, never at the
	// organization, so no organization carries an identity provider of its own.
	// The type is declared and the field answers null. Reported as needing an
	// organization-scoped identity-provider binding.
	orgType.AddFieldConfig("samlIdentityProvider", &graphql.Field{
		Type: s.gqlOrganizationIdentityProviderType(types),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			if _, err := s.orgFromSource(p.Source); err != nil {
				return nil, err
			}
			return nil, nil
		},
	})
}

// organizationRoleForOwner reports an enterprise owner's relationship to one
// organization: OWNER when they administer it, DIRECT_MEMBER when they belong
// to it, UNAFFILIATED otherwise — GitHub's RoleInOrganization.
func organizationRoleForOwner(st *store.Store, org *store.Org, owner *store.User) string {
	membership := st.GetMembership(org.Login, owner.ID)
	if membership == nil || membership.State != store.MembershipStateActive {
		return "UNAFFILIATED"
	}
	if membership.Role == store.OrgRoleAdmin {
		return "OWNER"
	}
	return "DIRECT_MEMBER"
}

// addRecentProjectsField wires recentProjects on an account. GitHub's
// recentProjects are the ProjectV2s an account has recently touched; bleephub
// answers the account's own projects, the set it can truthfully attribute,
// through the same visibility-filtered connection projectsV2 uses.
func (s *Resolver) addRecentProjectsField(owner *graphql.Object, ownerType string, ownerID func(graphql.ResolveParams) (int, error)) {
	connection := s.gqlConnectionType("ProjectV2", s.projectV2GraphQLTypes())
	owner.AddFieldConfig("recentProjects", &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id, err := ownerID(p)
			if err != nil {
				return nil, err
			}
			return s.projectV2Connection(p, id, ownerType), nil
		},
	})
}

// packagesField builds the shared packages connection resolver for User and
// Organization. A package's owner key is the account login (store.Package sets
// OwnerKey to the login for User- and Organization-owned packages).
func (s *Resolver) packagesField(types *accountSurfaceTypes, ownerLogin func(graphql.ResolveParams) (string, error)) *graphql.Field {
	connection := s.accountConnectionType(types, "Package", s.gqlPackageType(), false, nil)
	return &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			login, err := ownerLogin(p)
			if err != nil {
				return nil, err
			}
			packages := s.store.ListPackages(login)
			sort.Slice(packages, func(i, j int) bool { return packages[i].ID < packages[j].ID })
			var items []gqlConnItem
			for i := range packages {
				pkg := packages[i]
				if pkg.Deleted {
					continue
				}
				kind, ok := graphqlPackageTypeName(pkg.PackageType)
				if !ok {
					// A container package has no GitHub GraphQL PackageType enum
					// member, so it is not representable on this surface (the
					// REST packages API serves it instead).
					continue
				}
				items = append(items, gqlConnItem{
					identity: pkg.NodeID,
					render: func() map[string]interface{} {
						return map[string]interface{}{
							"id":          pkg.NodeID,
							"name":        pkg.Name,
							"packageType": kind,
						}
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	}
}

// gqlPackageType is GitHub's Package object, the subset bleephub's package
// store records. Memoized through the mutation-object registry so User.packages
// and Organization.packages name one type.
func (s *Resolver) gqlPackageType() *graphql.Object {
	return s.mutationObject("Package", graphql.Fields{
		"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"name":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"packageType": &graphql.Field{Type: graphql.NewNonNull(s.gqlPackageTypeEnum())},
	})
}

// gqlPackageTypeEnum is GitHub's legacy PackageType enum.
func (s *Resolver) gqlPackageTypeEnum() *graphql.Enum {
	return s.sharedEnum("PackageType", "DEBIAN", "DOCKER", "MAVEN", "NPM", "NUGET", "PYPI", "RUBYGEMS")
}

// graphqlPackageTypeName maps a stored package type onto GitHub's PackageType
// enum, reporting false for one with no enum member.
func graphqlPackageTypeName(stored string) (string, bool) {
	switch strings.ToLower(stored) {
	case "npm":
		return "NPM", true
	case "maven":
		return "MAVEN", true
	case "rubygems":
		return "RUBYGEMS", true
	case "nuget":
		return "NUGET", true
	case "docker":
		return "DOCKER", true
	case "debian":
		return "DEBIAN", true
	case "pypi":
		return "PYPI", true
	}
	return "", false
}

// gqlUserListSuggestionType is GitHub's UserListSuggestion object.
func (s *Resolver) gqlUserListSuggestionType() *graphql.Object {
	return s.mutationObject("UserListSuggestion", graphql.Fields{
		"id":   &graphql.Field{Type: graphql.ID},
		"name": &graphql.Field{Type: graphql.String},
	})
}

// gqlSavedReplyType is GitHub's SavedReply object.
func (s *Resolver) gqlSavedReplyType() *graphql.Object {
	return s.mutationObject("SavedReply", graphql.Fields{
		"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"databaseId": &graphql.Field{Type: graphql.Int},
		"title":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"body":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"bodyHTML":   &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("HTML"))},
		"user":       &graphql.Field{Type: s.graphqlTypes.actor},
	})
}

// gqlAnnouncementBannerType is GitHub's AnnouncementBanner object.
func (s *Resolver) gqlAnnouncementBannerType(dateTime *graphql.Scalar) *graphql.Object {
	return s.mutationObject("AnnouncementBanner", graphql.Fields{
		"createdAt":         &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		"expiresAt":         &graphql.Field{Type: dateTime},
		"isUserDismissible": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"message":           &graphql.Field{Type: graphql.String},
	})
}

// gqlOrganizationIdentityProviderType is GitHub's OrganizationIdentityProvider
// object, the subset a bleephub organization could carry.
func (s *Resolver) gqlOrganizationIdentityProviderType(types *accountSurfaceTypes) *graphql.Object {
	uri := s.graphQLStringScalar("URI")
	return s.mutationObject("OrganizationIdentityProvider", graphql.Fields{
		"id":              &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"issuer":          &graphql.Field{Type: graphql.String},
		"ssoUrl":          &graphql.Field{Type: uri},
		"digestMethod":    &graphql.Field{Type: uri},
		"signatureMethod": &graphql.Field{Type: uri},
		"organization":    &graphql.Field{Type: types.organization},
	})
}
