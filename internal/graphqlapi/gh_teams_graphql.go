package graphqlapi

// The Team object graph, completing the four-field shell the review-request
// union declared.

import (
	"sort"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addTeamFields completes the shared Team type. It runs before the organization
// installers, which name the connections built here.
func (s *Resolver) addTeamFields(types *accountSurfaceTypes) {
	teamType := s.graphqlTypes.team
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")

	stringField := func(key string) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, err := graphQLSourceMap(p.Source)
				if err != nil {
					return nil, err
				}
				return src[key], nil
			},
		}
	}

	teamType.AddFieldConfig("databaseId", &graphql.Field{Type: graphql.Int})
	teamType.AddFieldConfig("description", &graphql.Field{Type: graphql.String})
	teamType.AddFieldConfig("combinedSlug", &graphql.Field{Type: graphql.NewNonNull(graphql.String)})
	teamType.AddFieldConfig("createdAt", &graphql.Field{Type: graphql.NewNonNull(dateTime)})
	teamType.AddFieldConfig("updatedAt", &graphql.Field{Type: graphql.NewNonNull(dateTime)})
	teamType.AddFieldConfig("privacy", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("TeamPrivacy", "SECRET", "VISIBLE")),
	})
	teamType.AddFieldConfig("notificationSetting", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("TeamNotificationSetting",
			"NOTIFICATIONS_DISABLED", "NOTIFICATIONS_ENABLED")),
	})
	teamType.AddFieldConfig("avatarUrl", &graphql.Field{
		Type: uri,
		Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 400}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, err := graphQLSourceMap(p.Source)
			if err != nil {
				return nil, err
			}
			return src["avatarUrl"], nil
		},
	})
	for _, field := range []string{
		"url", "resourcePath", "membersUrl", "membersResourcePath",
		"repositoriesUrl", "repositoriesResourcePath", "teamsUrl", "teamsResourcePath",
		"editTeamUrl", "editTeamResourcePath", "newTeamUrl", "newTeamResourcePath",
	} {
		teamType.AddFieldConfig(field, stringField(field))
	}

	teamType.AddFieldConfig("parentTeam", &graphql.Field{
		Type: teamType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team, org, err := s.teamFromSource(p.Source)
			if err != nil || team == nil || team.ParentID == 0 {
				return nil, err
			}
			return optionalRendered(s.store.GetTeamByID(team.ParentID),
				func(parent *store.Team) map[string]interface{} { return s.teamSource(parent, org) }), nil
		},
	})

	teamConnection := s.gqlTeamConnectionType()
	teamType.AddFieldConfig("childTeams", &graphql.Field{
		Type: graphql.NewNonNull(teamConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"immediateOnly": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
			"orderBy":       &graphql.ArgumentConfig{Type: s.gqlTeamOrderInput(types)},
			"userLogins":    &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team, org, err := s.teamFromSource(p.Source)
			if err != nil || team == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			children := s.store.ListChildTeams(org.Login, team.ID)
			if immediate, ok := p.Args["immediateOnly"].(bool); ok && !immediate {
				children = append(children, s.descendantTeams(org, children)...)
			}
			return paginateGQLItems(s.teamConnectionItems(children, org), p.Args), nil
		},
	})
	teamType.AddFieldConfig("ancestors", &graphql.Field{
		Type: graphql.NewNonNull(teamConnection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team, org, err := s.teamFromSource(p.Source)
			if err != nil || team == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			var ancestors []*store.Team
			// Guard against a malformed parent cycle.
			seen := map[int]bool{team.ID: true}
			for current := team; current.ParentID != 0; {
				parent := s.store.GetTeamByID(current.ParentID)
				if parent == nil || seen[parent.ID] {
					break
				}
				seen[parent.ID] = true
				ancestors = append(ancestors, parent)
				current = parent
			}
			return paginateGQLItems(s.teamConnectionItems(ancestors, org), p.Args), nil
		},
	})

	// members
	teamMemberRole := s.sharedEnum("TeamMemberRole", "MAINTAINER", "MEMBER")
	memberConnection := s.accountConnectionType(types, "TeamMember", types.user, true, graphql.Fields{
		"role":                     &graphql.Field{Type: graphql.NewNonNull(teamMemberRole)},
		"memberAccessUrl":          &graphql.Field{Type: graphql.NewNonNull(uri)},
		"memberAccessResourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
	})
	teamType.AddFieldConfig("members", &graphql.Field{
		Type: graphql.NewNonNull(memberConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"membership": &graphql.ArgumentConfig{
				Type:         s.sharedEnum("TeamMembershipType", "ALL", "CHILD_TEAM", "IMMEDIATE"),
				DefaultValue: "ALL",
			},
			"role":  &graphql.ArgumentConfig{Type: teamMemberRole},
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team, org, err := s.teamFromSource(p.Source)
			if err != nil || team == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			// A secret team's roster is visible only to org owners and its own
			// members, not to every org member.
			if !s.viewerCanSeeTeam(p.Context, org, team) {
				return paginateGQLItems(nil, p.Args), nil
			}
			roleFilter, _ := p.Args["role"].(string)
			query, _ := p.Args["query"].(string)
			members := sortedUsersByLogin(s.store.ListTeamMembers(org.Login, team.Slug))
			items := make([]gqlConnItem, 0, len(members))
			for i := range members {
				member := members[i]
				role, _ := s.store.GetTeamMembership(org.Login, team.Slug, member.ID)
				roleName := "MEMBER"
				if role == store.TeamRoleMaintainer {
					roleName = "MAINTAINER"
				}
				if roleFilter != "" && roleFilter != roleName {
					continue
				}
				if query != "" && !accountMatchesQuery(member, query) {
					continue
				}
				accessPath := "/orgs/" + org.Login + "/teams/" + team.Slug + "/members/" + member.Login
				items = append(items, gqlConnItem{
					identity: member.NodeID,
					render: func() map[string]interface{} {
						node := userToGraphQL(member)
						node["_role"] = roleName
						node["_memberAccessResourcePath"] = accessPath
						node["_memberAccessUrl"] = externalURL(accessPath)
						return node
					},
				})
			}
			connection := paginateGQLItems(items, p.Args)
			withEdgeMember(connection, "role", "_role")
			withEdgeMember(connection, "memberAccessResourcePath", "_memberAccessResourcePath")
			withEdgeMember(connection, "memberAccessUrl", "_memberAccessUrl")
			return connection, nil
		},
	})

	// repositories
	teamRepositoryConnection := s.accountConnectionType(types, "TeamRepository", types.repository, true, graphql.Fields{
		"permission": &graphql.Field{
			Type: graphql.NewNonNull(s.sharedEnum("RepositoryPermission",
				"ADMIN", "MAINTAIN", "READ", "TRIAGE", "WRITE")),
		},
	})
	teamType.AddFieldConfig("repositories", &graphql.Field{
		Type: graphql.NewNonNull(teamRepositoryConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{
				Type: s.gqlOrderInput(types, "TeamRepositoryOrder", "TeamRepositoryOrderField",
					"CREATED_AT", "NAME", "PERMISSION", "PUSHED_AT", "STARGAZERS", "UPDATED_AT"),
			},
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team, org, err := s.teamFromSource(p.Source)
			if err != nil || team == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			query, _ := p.Args["query"].(string)
			repos := s.visibleRepos(p.Context, s.store.ListTeamRepos(org.Login, team.Slug))
			items := make([]gqlConnItem, 0, len(repos))
			sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
			for i := range repos {
				repo := repos[i]
				if query != "" && !strings.Contains(strings.ToLower(repo.Name), strings.ToLower(query)) {
					continue
				}
				permission := team.Permission
				if override, ok := s.store.GetTeamRepoPermission(org.Login, team.Slug, repo.FullName); ok {
					permission = override
				}
				enum := repositoryPermissionEnum(string(permission))
				items = append(items, gqlConnItem{
					identity: repo.NodeID,
					render: func() map[string]interface{} {
						node := repoToGraphQL(s.store, s.store.SnapRepo(repo))
						node["_permission"] = enum
						return node
					},
				})
			}
			return withEdgeMember(paginateGQLItems(items, p.Args), "permission", "_permission"), nil
		},
	})

	teamType.AddFieldConfig("viewerCanAdminister", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team, org, err := s.teamFromSource(p.Source)
			if err != nil || team == nil {
				return false, err
			}
			if s.viewerCanAdminAccount(p.Context, org.Login) {
				return true, nil
			}
			viewer := s.ghUserFromContext(p.Context)
			if viewer == nil {
				return false, nil
			}
			role, ok := s.store.GetTeamMembership(org.Login, team.Slug, viewer.ID)
			return ok && role == store.TeamRoleMaintainer, nil
		},
	})

	// The remaining Team fields (invitations, memberStatuses, projectsV2,
	// review-request delegation, subscriptions).
	s.addTeamExtraFields(types)
}

// gqlTeamConnectionType reuses the Projects v2 family's memoized TeamConnection
// rather than declaring a second type of the same name.
func (s *Resolver) gqlTeamConnectionType() *graphql.Object {
	return s.gqlConnectionType("Team", s.graphqlTypes.team)
}

func (s *Resolver) gqlTeamOrderInput(types *accountSurfaceTypes) *graphql.InputObject {
	return s.gqlOrderInput(types, "TeamOrder", "TeamOrderField", "NAME")
}

// teamFromSource re-reads the team a Team source names and its organization.
func (s *Resolver) teamFromSource(source interface{}) (*store.Team, *store.Org, error) {
	src, err := graphQLSourceMap(source)
	if err != nil {
		return nil, nil, err
	}
	databaseID, _ := src["databaseId"].(int)
	team := s.store.GetTeamByID(databaseID)
	if team == nil {
		return nil, nil, nil
	}
	org := s.store.GetOrgByID(team.OrgID)
	if org == nil {
		return nil, nil, nil
	}
	return team, org, nil
}

// descendantTeams collects every team below the given ones (the whole subtree).
func (s *Resolver) descendantTeams(org *store.Org, teams []*store.Team) []*store.Team {
	var out []*store.Team
	queue := append([]*store.Team(nil), teams...)
	seen := map[int]bool{}
	for _, team := range queue {
		seen[team.ID] = true
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range s.store.ListChildTeams(org.Login, current.ID) {
			if seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

// teamConnectionItems renders teams as connection items, name-ordered for stable
// page boundaries.
func (s *Resolver) teamConnectionItems(teams []*store.Team, org *store.Org) []gqlConnItem {
	sort.Slice(teams, func(i, j int) bool { return teams[i].Slug < teams[j].Slug })
	items := make([]gqlConnItem, 0, len(teams))
	for i := range teams {
		team := teams[i]
		items = append(items, gqlConnItem{
			identity: team.NodeID,
			render:   func() map[string]interface{} { return s.teamSource(team, org) },
		})
	}
	return items
}

// teamSource renders a team as its GraphQL source map, with the hypermedia paths
// GitHub's Team carries.
func (s *Resolver) teamSource(team *store.Team, org *store.Org) map[string]interface{} {
	resourcePath := "/orgs/" + org.Login + "/teams/" + team.Slug
	teamsPath := "/orgs/" + org.Login + "/teams"
	return map[string]interface{}{
		"nodeID":                   team.NodeID,
		"id":                       team.NodeID,
		"databaseId":               team.ID,
		"name":                     team.Name,
		"slug":                     team.Slug,
		"combinedSlug":             org.Login + "/" + team.Slug,
		"description":              nilStr(team.Description),
		"privacy":                  teamPrivacyEnum(team.Privacy),
		"notificationSetting":      teamNotificationEnum(team.NotificationSetting),
		"createdAt":                team.CreatedAt.UTC().Format(rfc3339),
		"updatedAt":                team.UpdatedAt.UTC().Format(rfc3339),
		"avatarUrl":                nilStr(org.AvatarURL),
		"organization":             orgToGraphQL(org),
		"resourcePath":             resourcePath,
		"url":                      externalURL(resourcePath),
		"membersResourcePath":      resourcePath + "/members",
		"membersUrl":               externalURL(resourcePath + "/members"),
		"repositoriesResourcePath": resourcePath + "/repositories",
		"repositoriesUrl":          externalURL(resourcePath + "/repositories"),
		"editTeamResourcePath":     resourcePath + "/edit",
		"editTeamUrl":              externalURL(resourcePath + "/edit"),
		"teamsResourcePath":        teamsPath,
		"teamsUrl":                 externalURL(teamsPath),
		"newTeamResourcePath":      teamsPath + "/new",
		"newTeamUrl":               externalURL(teamsPath + "/new"),
	}
}

// teamPrivacyEnum maps stored privacy onto GitHub's TeamPrivacy, whose members
// are named for visibility rather than the REST closed/secret wire values.
func teamPrivacyEnum(privacy store.TeamPrivacy) string {
	if privacy == store.TeamPrivacySecret {
		return "SECRET"
	}
	return "VISIBLE"
}

func teamNotificationEnum(setting store.TeamNotificationSetting) string {
	if setting == store.TeamNotificationsDisabled {
		return "NOTIFICATIONS_DISABLED"
	}
	return "NOTIFICATIONS_ENABLED"
}
