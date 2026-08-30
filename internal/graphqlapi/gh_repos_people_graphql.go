package graphqlapi

// Repository connections over the accounts and records a repository owns: its collaborators, the people who may be
// @-mentioned in it, its forks, milestones and labels by identifier, its deploy keys and its commit comments.

import (
	"sort"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// repoCollaborator is one account's standing on a repository, tracking how it was acquired for the affiliation filter.
type repoCollaborator struct {
	user       *store.User
	permission string
	direct     bool
	outside    bool
}

// addRepositoryPeopleFields installs the account- and record-valued connections on Repository.
func (s *Resolver) addRepositoryPeopleFields(types *accountSurfaceTypes) {
	repoType := types.repository
	dateTime := s.graphQLStringScalar("DateTime")
	repositoryPermission := s.sharedEnum("RepositoryPermission", "ADMIN", "MAINTAIN", "READ", "TRIAGE", "WRITE")

	// collaborators
	collaboratorConnection := s.accountConnectionType(types, "RepositoryCollaborator", types.user, true, graphql.Fields{
		"permission": &graphql.Field{Type: graphql.NewNonNull(repositoryPermission)},
	})
	repoType.AddFieldConfig("collaborators", &graphql.Field{
		Type: collaboratorConnection,
		Args: connectionArgs(graphql.FieldConfigArgument{
			"affiliation": &graphql.ArgumentConfig{
				Type: s.sharedEnum("CollaboratorAffiliation", "ALL", "DIRECT", "OUTSIDE"),
			},
			"login": &graphql.ArgumentConfig{Type: graphql.String},
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// Listing collaborators requires push standing; the field is nullable so "not visible" is distinct from "none".
			if !s.viewerCanPushRepo(p.Context, repo) {
				return nil, nil
			}
			collaborators := s.repositoryCollaborators(repo, p.Args)
			items := make([]gqlConnItem, 0, len(collaborators))
			for i := range collaborators {
				entry := collaborators[i]
				items = append(items, gqlConnItem{
					identity: entry.user.NodeID,
					render: func() map[string]interface{} {
						node := userToGraphQL(entry.user)
						node["_permission"] = entry.permission
						return node
					},
				})
			}
			return withEdgeMember(paginateGQLItems(items, p.Args), "permission", "_permission"), nil
		},
	})

	// mentionable accounts
	repoType.AddFieldConfig("mentionableUsers", &graphql.Field{
		Type: graphql.NewNonNull(types.userConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLItems(nil, p.Args), nil
			}
			users := s.repositoryMentionableUsers(repo, p.Args)
			return paginateGQLItems(userConnectionItems(users), p.Args), nil
		},
	})

	// forks
	repoType.AddFieldConfig("forks", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repositoryConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy":    &graphql.ArgumentConfig{Type: s.gqlRepositoryOrderInput()},
			"privacy":    &graphql.ArgumentConfig{Type: s.sharedEnum("RepositoryPrivacy", "PRIVATE", "PUBLIC")},
			"visibility": &graphql.ArgumentConfig{Type: s.sharedEnum("RepositoryVisibility", "PUBLIC", "PRIVATE", "INTERNAL")},
			"isLocked":   &graphql.ArgumentConfig{Type: graphql.Boolean},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLItems(nil, p.Args), nil
			}
			// The viewer sees only the forks they may read.
			forks := s.visibleRepos(p.Context, s.store.ListForks(repo.ID, store.RepoListOptions{NoPaginate: true}))
			forks = filterReposByPrivacy(forks, p.Args)
			return paginateGQLItems(s.repositoryConnectionItems(forks), p.Args), nil
		},
	})

	// milestone / label by identifier
	repoType.AddFieldConfig("milestone", &graphql.Field{
		Type: s.graphqlTypes.milestone,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			number, ok := intArg(p.Args, "number")
			if !ok {
				return nil, nil
			}
			return optionalRendered(s.store.GetMilestoneByNumber(repo.ID, number), milestoneToGQL), nil
		},
	})
	repoType.AddFieldConfig("label", &graphql.Field{
		Type: s.gqlLabelType(),
		Args: graphql.FieldConfigArgument{
			"name": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			name, _ := p.Args["name"].(string)
			return optionalRendered(s.store.GetLabelByName(repo.ID, name), labelToGQL), nil
		},
	})

	// deploy keys
	deployKey := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeployKey",
		Fields: graphql.Fields{
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"enabled":   &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"key":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"readOnly":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"title":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"verified":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
	})
	repoType.AddFieldConfig("deployKeys", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "DeployKey", deployKey, false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// A deploy key is a credential; only an administrator sees the list, as with the REST keys routes.
			if !s.viewerCanAdminRepo(p.Context, repo) {
				return paginateGQLItems(nil, p.Args), nil
			}
			keys := s.store.ListRepoDeployKeys(repo.ID)
			items := make([]gqlConnItem, 0, len(keys))
			for i := range keys {
				row := keys[i]
				items = append(items, gqlConnItem{
					identity: row.NodeID,
					render: func() map[string]interface{} {
						return map[string]interface{}{
							"createdAt": row.CreatedAt.UTC().Format(rfc3339),
							"enabled":   true, // bleephub never disables a registered deploy key
							"id":        row.NodeID,
							"key":       row.Key,
							"readOnly":  row.ReadOnly,
							"title":     row.Title,
							"verified":  row.Verified,
						}
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// commit comments
	repoType.AddFieldConfig("commitComments", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "CommitComment", s.graphqlTypes.commitComment, false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLItems(nil, p.Args), nil
			}
			return paginateGQLItems(s.commitCommentItems(s.store.CommitComments.ListForRepo(repo.ID)), p.Args), nil
		},
	})

	// Residual repos/git-object members on Repository, Commit, Ref, TreeEntry and RepositoryConnection.
	// Wired last, so every family it reaches is already assembled.
	s.addRepoGitResidualFields(types)
}

// rfc3339 is the DateTime layout every timestamp in this family renders with.
const rfc3339 = "2006-01-02T15:04:05Z07:00"

// withEdgeMember copies a private node member onto each edge, carrying an edge-only GitHub field
// (a collaborator's permission, a stargazer's starred-at) without a second pagination pass.
func withEdgeMember(connection map[string]interface{}, edgeField, nodeKey string) map[string]interface{} {
	edges, _ := connection["edges"].([]map[string]interface{})
	for _, edge := range edges {
		node, ok := edge["node"].(map[string]interface{})
		if !ok {
			continue
		}
		edge[edgeField] = node[nodeKey]
	}
	return connection
}

// commitCommentItems renders commit comments oldest first.
func (s *Resolver) commitCommentItems(comments []*store.CommitComment) []gqlConnItem {
	sort.Slice(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	items := make([]gqlConnItem, 0, len(comments))
	for i := range comments {
		row := comments[i]
		items = append(items, gqlConnItem{
			identity: row.NodeID,
			render:   func() map[string]interface{} { return commitCommentToGQL(row, s.store) },
		})
	}
	return items
}

func (s *Resolver) repositoryConnectionItems(repos []*store.Repo) []gqlConnItem {
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
	items := make([]gqlConnItem, 0, len(repos))
	for i := range repos {
		row := repos[i]
		items = append(items, gqlConnItem{
			identity: row.NodeID,
			render:   func() map[string]interface{} { return repoToGraphQL(s.store, s.store.SnapRepo(row)) },
		})
	}
	return items
}

// filterReposByPrivacy applies the privacy/visibility arguments GitHub's repository connections accept.
func filterReposByPrivacy(repos []*store.Repo, args map[string]interface{}) []*store.Repo {
	privacy, _ := args["privacy"].(string)
	visibility, _ := args["visibility"].(string)
	if privacy == "" && visibility == "" {
		return repos
	}
	out := make([]*store.Repo, 0, len(repos))
	for _, repo := range repos {
		if privacy == "PRIVATE" && !repo.Private {
			continue
		}
		if privacy == "PUBLIC" && repo.Private {
			continue
		}
		if visibility != "" && !strings.EqualFold(repo.Visibility, visibility) {
			continue
		}
		out = append(out, repo)
	}
	return out
}

// gqlRepositoryOrderInput reads GitHub's RepositoryOrder input out of the shared table the repository family already declared it in.
func (s *Resolver) gqlRepositoryOrderInput() *graphql.InputObject {
	return s.gqlOrderInput(s.accountSurfaceRegistry(), "RepositoryOrder", "RepositoryOrderField",
		"CREATED_AT", "NAME", "PUSHED_AT", "STARGAZERS", "UPDATED_AT")
}

// repositoryCollaborators lists the accounts with standing on a repository: its owner, directly-added accounts, and
// (for an org repo) members of teams granted access. affiliation, login and query narrow the list.
func (s *Resolver) repositoryCollaborators(repo *store.Repo, args map[string]interface{}) []repoCollaborator {
	byLogin := map[string]repoCollaborator{}
	orgLogin := ""
	if repo.OwnerType == "Organization" {
		orgLogin, _, _ = store.SplitRepoFullName(repo.FullName)
	}
	isOrgMember := func(login string) bool {
		if orgLogin == "" {
			return false
		}
		user := s.store.LookupUserByLogin(login)
		if user == nil {
			return false
		}
		membership := s.store.GetMembership(orgLogin, user.ID)
		return membership != nil && membership.State == store.MembershipStateActive
	}
	record := func(user *store.User, permission string, direct bool) {
		if user == nil {
			return
		}
		existing, seen := byLogin[user.Login]
		if seen && repositoryPermissionRank(existing.permission) >= repositoryPermissionRank(permission) {
			// Keep the stronger standing, direct if either grant was.
			existing.direct = existing.direct || direct
			byLogin[user.Login] = existing
			return
		}
		byLogin[user.Login] = repoCollaborator{
			user:       user,
			permission: permission,
			direct:     direct || (seen && existing.direct),
			outside:    !isOrgMember(user.Login),
		}
	}

	// The owning account administers the repo; for an org repo that is every org owner, matching the authorization layer.
	if orgLogin == "" {
		if repo.Owner != nil {
			record(repo.Owner, "ADMIN", true)
		}
	} else {
		for _, member := range s.store.ListOrgMembers(orgLogin) {
			membership := s.store.GetMembership(orgLogin, member.ID)
			if membership != nil && membership.Role == store.OrgRoleAdmin {
				record(member, "ADMIN", true)
			}
		}
	}
	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	for login, permission := range s.store.ListRepoCollaborators(owner, name) {
		record(s.store.LookupUserByLogin(login), repositoryPermissionEnum(permission), true)
	}
	if orgLogin != "" {
		for _, team := range s.store.ListTeamsForRepo(repo.FullName) {
			permission := team.Permission
			if override, ok := s.store.GetTeamRepoPermission(orgLogin, team.Slug, repo.FullName); ok {
				permission = override
			}
			for _, member := range s.store.ListTeamMembers(orgLogin, team.Slug) {
				record(member, repositoryPermissionEnum(string(permission)), false)
			}
		}
	}

	affiliation, _ := args["affiliation"].(string)
	loginFilter, _ := args["login"].(string)
	query, _ := args["query"].(string)
	out := make([]repoCollaborator, 0, len(byLogin))
	for _, entry := range byLogin {
		switch affiliation {
		case "DIRECT":
			if !entry.direct {
				continue
			}
		case "OUTSIDE":
			if !entry.outside {
				continue
			}
		}
		if loginFilter != "" && !strings.EqualFold(entry.user.Login, loginFilter) {
			continue
		}
		if query != "" && !accountMatchesQuery(entry.user, query) {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].user.Login < out[j].user.Login })
	return out
}

// repositoryPermissionEnum maps a stored repository permission onto GitHub's RepositoryPermission enum.
func repositoryPermissionEnum(permission string) string {
	switch strings.ToLower(permission) {
	case "admin":
		return "ADMIN"
	case "maintain":
		return "MAINTAIN"
	case "push", "write":
		return "WRITE"
	case "triage":
		return "TRIAGE"
	default:
		return "READ"
	}
}

// repositoryPermissionRank orders the enum so the strongest of two grants wins.
func repositoryPermissionRank(permission string) int {
	switch permission {
	case "ADMIN":
		return 5
	case "MAINTAIN":
		return 4
	case "WRITE":
		return 3
	case "TRIAGE":
		return 2
	default:
		return 1
	}
}

// accountMatchesQuery is the substring match GitHub's `query` argument applies to login and name.
func accountMatchesQuery(user *store.User, query string) bool {
	needle := strings.ToLower(query)
	return strings.Contains(strings.ToLower(user.Login), needle) ||
		strings.Contains(strings.ToLower(user.Name), needle)
}

// repositoryMentionableUsers lists the accounts a comment may @-mention: everyone with standing on the repo plus everyone who has taken part in one of its issues or pull requests.
func (s *Resolver) repositoryMentionableUsers(repo *store.Repo, args map[string]interface{}) []*store.User {
	byID := map[int]*store.User{}
	for _, entry := range s.repositoryCollaborators(repo, map[string]interface{}{}) {
		byID[entry.user.ID] = entry.user
	}
	if repo.OwnerType == "Organization" {
		orgLogin, _, _ := store.SplitRepoFullName(repo.FullName)
		for _, member := range s.store.ListOrgMembers(orgLogin) {
			byID[member.ID] = member
		}
	}
	for _, issue := range s.store.ListIssues(repo.ID, "all") {
		if author := s.store.GetUserByID(issue.AuthorID); author != nil {
			byID[author.ID] = author
		}
	}
	query, _ := args["query"].(string)
	out := make([]*store.User, 0, len(byID))
	for _, user := range byID {
		if query != "" && !accountMatchesQuery(user, query) {
			continue
		}
		out = append(out, user)
	}
	return sortedUsersByLogin(out)
}
