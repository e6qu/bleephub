package graphqlapi

// The User account surface: the profile members a client renders a user page
// from, the follow graph, the account's authentication keys and social links,
// and the connections over what the account has written and starred.

import (
	"sort"
	"strconv"
	"strings"

	"github.com/graphql-go/graphql"
	"golang.org/x/crypto/ssh"

	"github.com/e6qu/bleephub/internal/store"
)

// addUserProfileFields installs the profile and viewer members of User.
func (s *Resolver) addUserProfileFields(types *accountSurfaceTypes) {
	userType := types.user
	uri := s.graphQLStringScalar("URI")
	html := s.graphQLStringScalar("HTML")

	userString := func(read func(*store.User) string) *graphql.Field {
		return &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				user, err := s.userFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				return nilStr(read(user)), nil
			},
		}
	}

	userType.AddFieldConfig("company", userString(func(u *store.User) string { return u.Company }))
	userType.AddFieldConfig("location", userString(func(u *store.User) string { return u.Location }))
	userType.AddFieldConfig("twitterUsername", userString(func(u *store.User) string { return u.TwitterUsername }))
	userType.AddFieldConfig("websiteUrl", &graphql.Field{
		Type: uri,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return nilStr(user.Blog), nil
		},
	})
	userType.AddFieldConfig("bioHTML", &graphql.Field{
		Type: graphql.NewNonNull(html),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return renderAccountMarkdown(user.Bio), nil
		},
	})
	userType.AddFieldConfig("companyHTML", &graphql.Field{
		Type: graphql.NewNonNull(html),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return renderAccountMarkdown(user.Company), nil
		},
	})

	userBool := func(read func(p graphql.ResolveParams, user *store.User) bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				user, err := s.userFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				return read(p, user), nil
			},
		}
	}
	userType.AddFieldConfig("isSiteAdmin", userBool(func(_ graphql.ResolveParams, u *store.User) bool {
		return u.SiteAdmin
	}))
	userType.AddFieldConfig("isHireable", userBool(func(_ graphql.ResolveParams, u *store.User) bool {
		return u.Hireable != nil && *u.Hireable
	}))
	userType.AddFieldConfig("isEmployee", userBool(func(_ graphql.ResolveParams, u *store.User) bool {
		// GitHub's isEmployee marks a GitHub, Inc. staff account. The
		// equivalent standing on this instance is site administration, which
		// is the only staff role it models.
		return u.SiteAdmin
	}))
	userType.AddFieldConfig("isViewer", userBool(func(p graphql.ResolveParams, u *store.User) bool {
		viewer := s.ghUserFromContext(p.Context)
		return viewer != nil && viewer.ID == u.ID
	}))
	userType.AddFieldConfig("viewerIsFollowing", userBool(func(p graphql.ResolveParams, u *store.User) bool {
		viewer := s.ghUserFromContext(p.Context)
		return viewer != nil && s.store.LoginFollows(viewer.Login, u.Login)
	}))
	userType.AddFieldConfig("isFollowingViewer", userBool(func(p graphql.ResolveParams, u *store.User) bool {
		viewer := s.ghUserFromContext(p.Context)
		return viewer != nil && s.store.LoginFollows(u.Login, viewer.Login)
	}))
	userType.AddFieldConfig("viewerCanFollow", userBool(func(p graphql.ResolveParams, u *store.User) bool {
		// An account may follow any other account but itself, and only while
		// signed in.
		viewer := s.ghUserFromContext(p.Context)
		return viewer != nil && viewer.ID != u.ID
	}))
	userType.AddFieldConfig("userViewType", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("UserViewType", "PRIVATE", "PUBLIC")),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// PRIVATE is the view an account gets of itself: the one that
			// carries its private repositories, secret gists and email
			// addresses.
			viewer := s.ghUserFromContext(p.Context)
			if viewer != nil && viewer.ID == user.ID {
				return "PRIVATE", nil
			}
			return "PUBLIC", nil
		},
	})

	userType.AddFieldConfig("interactionAbility", &graphql.Field{
		Type: s.gqlInteractionAbilityType(types),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			limit, expiry := s.store.GetUserInteractionLimit(user.ID)
			if limit == "" {
				return nil, nil
			}
			return optionalObject(s.interactionAbilitySource(limit, &expiry, "USER")), nil
		},
	})

	// --- account keys and links -------------------------------------------
	publicKey := graphql.NewObject(graphql.ObjectConfig{
		Name: "PublicKey",
		Fields: graphql.Fields{
			"accessedAt":  &graphql.Field{Type: s.graphQLStringScalar("DateTime")},
			"createdAt":   &graphql.Field{Type: s.graphQLStringScalar("DateTime")},
			"fingerprint": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"isReadOnly":  &graphql.Field{Type: graphql.Boolean},
			"key":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"updatedAt":   &graphql.Field{Type: s.graphQLStringScalar("DateTime")},
		},
	})
	userType.AddFieldConfig("publicKeys", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "PublicKey", publicKey, false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			keys := s.store.ListAccountSSHKeys(user.ID)
			items := make([]gqlConnItem, 0, len(keys))
			for i := range keys {
				row := keys[i]
				items = append(items, gqlConnItem{
					identity: "PK_" + strings.ToLower(row.Title) + "_" + sshKeyIdentity(row.ID),
					render: func() map[string]interface{} {
						return map[string]interface{}{
							"id":          "PK_" + sshKeyIdentity(row.ID),
							"key":         row.Key,
							"fingerprint": sshKeyFingerprint(row.Key),
							"createdAt":   nullableRFC3339(row.CreatedAt),
							"updatedAt":   nullableRFC3339(row.CreatedAt),
							// An authentication key records no last-use
							// instant on this instance, and it is never
							// read-only: it authenticates both fetch and push.
							"accessedAt": nil,
							"isReadOnly": false,
						}
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	socialAccount := graphql.NewObject(graphql.ObjectConfig{
		Name: "SocialAccount",
		Fields: graphql.Fields{
			"displayName": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"provider": &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum("SocialAccountProvider",
				"BLUESKY", "FACEBOOK", "GENERIC", "HOMETOWN", "INSTAGRAM", "LINKEDIN", "MASTODON",
				"NPM", "REDDIT", "THREADS", "TWITCH", "TWITTER", "YOUTUBE"))},
			"url": &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
	})
	userType.AddFieldConfig("socialAccounts", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "SocialAccount", socialAccount, false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			accounts := s.store.ListUserSocialAccounts(user.ID)
			items := make([]gqlConnItem, 0, len(accounts))
			for i := range accounts {
				account := accounts[i]
				url, _ := account["url"].(string)
				items = append(items, gqlConnItem{
					identity: url,
					render: func() map[string]interface{} {
						return map[string]interface{}{
							"url":         url,
							"provider":    socialAccountProvider(url),
							"displayName": socialAccountDisplayName(url),
						}
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	userType.AddFieldConfig("organization", &graphql.Field{
		Type: types.organization,
		Args: graphql.FieldConfigArgument{
			"login": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			login, _ := p.Args["login"].(string)
			org := s.store.GetOrg(login)
			if org == nil {
				return nil, nil
			}
			membership := s.store.GetMembership(org.Login, user.ID)
			if membership == nil || membership.State != store.MembershipStateActive {
				return nil, nil
			}
			// A private membership is visible only to the account itself and
			// to somebody who can already see the organization's roster.
			if !membership.Public && !s.viewerIsOrgMember(p.Context, org.Login) {
				viewer := s.ghUserFromContext(p.Context)
				if viewer == nil || viewer.ID != user.ID {
					return nil, nil
				}
			}
			return orgToGraphQL(org), nil
		},
	})
}

// addUserConnectionFields installs the connections over what an account
// follows, has written and has starred.
func (s *Resolver) addUserConnectionFields(types *accountSurfaceTypes) {
	userType := types.user

	// --- follow graph ------------------------------------------------------
	followerConnection := s.gqlUserEdgeConnection(types, "Follower")
	followingConnection := s.gqlUserEdgeConnection(types, "Following")
	followField := func(connection *graphql.Object, logins func(string) []string) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(connection),
			Args: connectionArgs(nil),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				user, err := s.userFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				accounts := make([]*store.User, 0)
				for _, login := range logins(user.Login) {
					if account := s.store.LookupUserByLogin(login); account != nil {
						accounts = append(accounts, account)
					}
				}
				return paginateGQLItems(userConnectionItems(accounts), p.Args), nil
			},
		}
	}
	userType.AddFieldConfig("followers", followField(followerConnection, s.store.FollowerLoginsOf))
	userType.AddFieldConfig("following", followField(followingConnection, s.store.FollowingLoginsOf))

	// --- authored issues and pull requests --------------------------------
	userType.AddFieldConfig("issues", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.issueConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"states":  &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(s.sharedEnum("IssueState", "OPEN", "CLOSED")))},
			"labels":  &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"orderBy": &graphql.ArgumentConfig{Type: s.graphqlTypes.issueOrder},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			states := stringSet(stringListArg(p.Args["states"]))
			labels := stringSet(stringListArg(p.Args["labels"]))
			var items []gqlConnItem
			for _, pair := range s.authoredIssues(p, user) {
				issue := pair.Issue
				if len(states) > 0 && !states[strings.ToUpper(issue.State)] {
					continue
				}
				if len(labels) > 0 && !s.issueCarriesAnyLabel(issue, labels) {
					continue
				}
				items = append(items, gqlConnItem{
					identity: issue.NodeID,
					render:   func() map[string]interface{} { return issueToGQL(issue, s.store) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	userType.AddFieldConfig("pullRequests", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.pullRequestConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"states": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(
				s.sharedEnum("PullRequestState", "OPEN", "CLOSED", "MERGED")))},
			"labels":      &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"baseRefName": &graphql.ArgumentConfig{Type: graphql.String},
			"headRefName": &graphql.ArgumentConfig{Type: graphql.String},
			"orderBy":     &graphql.ArgumentConfig{Type: s.graphqlTypes.issueOrder},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			states := stringSet(stringListArg(p.Args["states"]))
			baseRef, _ := p.Args["baseRefName"].(string)
			headRef, _ := p.Args["headRefName"].(string)
			var items []gqlConnItem
			for _, pr := range s.authoredPullRequests(p, user) {
				row := pr
				if len(states) > 0 && !states[pullRequestStateName(row)] {
					continue
				}
				if baseRef != "" && row.BaseRefName != baseRef {
					continue
				}
				if headRef != "" && row.HeadRefName != headRef {
					continue
				}
				items = append(items, gqlConnItem{
					identity: row.NodeID,
					render:   func() map[string]interface{} { return pullRequestToGQL(row, s.store) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// --- authored comments -------------------------------------------------
	userType.AddFieldConfig("issueComments", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.issueCommentConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{
				Type: s.gqlOrderInput(types, "IssueCommentOrder", "IssueCommentOrderField", "UPDATED_AT"),
			},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			comments := s.authoredIssueComments(p, user)
			descending := orderDirectionDescending(p.Args, "orderBy", false)
			sort.Slice(comments, func(i, j int) bool {
				if descending {
					return comments[i].ID > comments[j].ID
				}
				return comments[i].ID < comments[j].ID
			})
			items := make([]gqlConnItem, 0, len(comments))
			for i := range comments {
				row := comments[i]
				items = append(items, gqlConnItem{
					identity: row.NodeID,
					render:   func() map[string]interface{} { return commentToGQL(row, s.store) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	userType.AddFieldConfig("commitComments", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "CommitComment", s.graphqlTypes.commitComment, false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			var authored []*store.CommitComment
			for _, repo := range s.readableRepos(p) {
				for _, comment := range s.store.CommitComments.ListForRepo(repo.ID) {
					if comment.AuthorID == user.ID {
						authored = append(authored, comment)
					}
				}
			}
			return paginateGQLItems(s.commitCommentItems(authored), p.Args), nil
		},
	})

	userType.AddFieldConfig("gistComments", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "GistComment", s.gqlGistCommentType(types), false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			var authored []*store.GistComment
			for _, gist := range s.visibleGistsFor(p, user) {
				for _, comment := range s.store.ListGistComments(gist.ID) {
					if comment.UserID == user.ID {
						authored = append(authored, comment)
					}
				}
			}
			sort.Slice(authored, func(i, j int) bool { return authored[i].ID < authored[j].ID })
			items := make([]gqlConnItem, 0, len(authored))
			for i := range authored {
				row := authored[i]
				items = append(items, gqlConnItem{
					identity: row.NodeID,
					render: func() map[string]interface{} {
						return map[string]interface{}{
							"id":        row.NodeID,
							"body":      row.Body,
							"createdAt": row.CreatedAt.UTC().Format(rfc3339),
							"updatedAt": row.UpdatedAt.UTC().Format(rfc3339),
							"author":    optionalRendered(s.store.GetUserByID(row.UserID), userToGraphQL),
						}
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// --- one gist by name --------------------------------------------------
	userType.AddFieldConfig("gist", &graphql.Field{
		Type: s.gqlGistType(),
		Args: graphql.FieldConfigArgument{
			"name": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			name, _ := p.Args["name"].(string)
			gist := s.store.GetGist(name)
			if gist == nil || gist.OwnerID != user.ID {
				return nil, nil
			}
			// A secret gist is readable only by its owner, the same rule the
			// gists connection and the REST /gists surface enforce.
			if !gist.Public {
				viewer := s.ghUserFromContext(p.Context)
				if viewer == nil || viewer.ID != user.ID {
					return nil, nil
				}
			}
			return gistToGQL(gist, userToGraphQL(user)), nil
		},
	})

	// --- repositories the account watches, starred and contributed to ------
	repositoryConnection := s.graphqlTypes.repositoryConnection
	userType.AddFieldConfig("watching", &graphql.Field{
		Type: graphql.NewNonNull(repositoryConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy":    &graphql.ArgumentConfig{Type: s.gqlRepositoryOrderInput()},
			"privacy":    &graphql.ArgumentConfig{Type: s.sharedEnum("RepositoryPrivacy", "PRIVATE", "PUBLIC")},
			"visibility": &graphql.ArgumentConfig{Type: s.sharedEnum("RepositoryVisibility", "PUBLIC", "PRIVATE", "INTERNAL")},
			"isLocked":   &graphql.ArgumentConfig{Type: graphql.Boolean},
			"affiliations": &graphql.ArgumentConfig{Type: graphql.NewList(
				s.sharedEnum("RepositoryAffiliation", "COLLABORATOR", "ORGANIZATION_MEMBER", "OWNER"))},
			"ownerAffiliations": &graphql.ArgumentConfig{Type: graphql.NewList(
				s.sharedEnum("RepositoryAffiliation", "COLLABORATOR", "ORGANIZATION_MEMBER", "OWNER"))},
			"hasIssuesEnabled": &graphql.ArgumentConfig{Type: graphql.Boolean},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			repos := s.visibleRepos(p.Context, s.store.ListRepoSubscriptionsForUser(user.ID))
			repos = filterReposByPrivacy(repos, p.Args)
			if hasIssues, ok := p.Args["hasIssuesEnabled"].(bool); ok {
				kept := repos[:0:0]
				for _, repo := range repos {
					if repo.HasIssues == hasIssues {
						kept = append(kept, repo)
					}
				}
				repos = kept
			}
			return paginateGQLItems(s.repositoryConnectionItems(repos), p.Args), nil
		},
	})

	userType.AddFieldConfig("repositoriesContributedTo", &graphql.Field{
		Type: graphql.NewNonNull(repositoryConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy":                 &graphql.ArgumentConfig{Type: s.gqlRepositoryOrderInput()},
			"privacy":                 &graphql.ArgumentConfig{Type: s.sharedEnum("RepositoryPrivacy", "PRIVATE", "PUBLIC")},
			"includeUserRepositories": &graphql.ArgumentConfig{Type: graphql.Boolean},
			"hasIssues":               &graphql.ArgumentConfig{Type: graphql.Boolean},
			"isLocked":                &graphql.ArgumentConfig{Type: graphql.Boolean},
			"contributionTypes": &graphql.ArgumentConfig{Type: graphql.NewList(s.sharedEnum(
				"RepositoryContributionType", "COMMIT", "ISSUE", "PULL_REQUEST", "PULL_REQUEST_REVIEW", "REPOSITORY"))},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			repos := s.repositoriesContributedTo(p, user)
			repos = filterReposByPrivacy(repos, p.Args)
			return paginateGQLItems(s.repositoryConnectionItems(repos), p.Args), nil
		},
	})

	userType.AddFieldConfig("topRepositories", &graphql.Field{
		Type: graphql.NewNonNull(repositoryConnection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.gqlRepositoryOrderInput())},
			"since":   &graphql.ArgumentConfig{Type: s.graphQLStringScalar("DateTime")},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user, err := s.userFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// GitHub's topRepositories are the repositories the account has
			// most recently contributed to, ordered by the caller's orderBy.
			repos := s.repositoriesContributedTo(p, user)
			field := orderField(p.Args, "orderBy", "PUSHED_AT")
			descending := orderDirectionDescending(p.Args, "orderBy", true)
			sortReposByOrderField(repos, field, descending)
			items := make([]gqlConnItem, 0, len(repos))
			for i := range repos {
				row := repos[i]
				items = append(items, gqlConnItem{
					identity: row.NodeID,
					render:   func() map[string]interface{} { return repoToGraphQL(s.store, s.store.SnapRepo(row)) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// gqlGistCommentType is GitHub's GistComment, the subset bleephub's gist
// comment store records.
func (s *Resolver) gqlGistCommentType(types *accountSurfaceTypes) *graphql.Object {
	if types.gistComment != nil {
		return types.gistComment
	}
	dateTime := s.graphQLStringScalar("DateTime")
	types.gistComment = graphql.NewObject(graphql.ObjectConfig{
		Name: "GistComment",
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"body":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"author":    &graphql.Field{Type: s.graphqlTypes.actor},
		},
	})
	return types.gistComment
}

// stringSet turns an enum/string list argument into a membership set.
func stringSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

// readableRepos is every repository the request may read, which is the scope
// an account-wide connection has to walk (a comment or an issue lives in a
// repository, and one the viewer cannot read must not surface through the
// author's profile).
func (s *Resolver) readableRepos(p graphql.ResolveParams) []*store.Repo {
	return s.visibleRepos(p.Context, s.store.ListEveryRepo())
}

// authoredIssues is the issues the account opened, in the repositories the
// request may read.
func (s *Resolver) authoredIssues(p graphql.ResolveParams, user *store.User) []store.IssueWithRepo {
	readable := map[int]bool{}
	for _, repo := range s.readableRepos(p) {
		readable[repo.ID] = true
	}
	var out []store.IssueWithRepo
	for _, pair := range s.store.ListUserFilteredIssues(user, "created") {
		if pair.Issue == nil || pair.Repo == nil || !readable[pair.Repo.ID] {
			continue
		}
		out = append(out, pair)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issue.ID < out[j].Issue.ID })
	return out
}

// authoredPullRequests is the pull requests the account opened, in the
// repositories the request may read.
func (s *Resolver) authoredPullRequests(p graphql.ResolveParams, user *store.User) []*store.PullRequest {
	var out []*store.PullRequest
	for _, repo := range s.readableRepos(p) {
		for _, pr := range s.store.ListPullRequests(repo.ID, "all") {
			if pr.AuthorID == user.ID {
				out = append(out, pr)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// authoredIssueComments is the issue and pull-request comments the account
// wrote, in the repositories the request may read.
func (s *Resolver) authoredIssueComments(p graphql.ResolveParams, user *store.User) []*store.Comment {
	var out []*store.Comment
	for _, repo := range s.readableRepos(p) {
		for _, comment := range s.store.ListRepoIssueComments(repo.ID) {
			if comment.AuthorID == user.ID {
				out = append(out, comment)
			}
		}
	}
	return out
}

// issueCarriesAnyLabel reports whether the issue bears one of the named labels.
func (s *Resolver) issueCarriesAnyLabel(issue *store.Issue, names map[string]bool) bool {
	for _, labelID := range issue.LabelIDs {
		label := s.store.GetLabel(labelID)
		if label != nil && names[label.Name] {
			return true
		}
	}
	return false
}

// repositoriesContributedTo is every readable repository the account has
// opened an issue or a pull request in, or owns.
func (s *Resolver) repositoriesContributedTo(p graphql.ResolveParams, user *store.User) []*store.Repo {
	contributed := map[int]*store.Repo{}
	for _, repo := range s.readableRepos(p) {
		if repo.OwnerType == "User" && repo.OwnerID == user.ID {
			contributed[repo.ID] = repo
			continue
		}
		for _, issue := range s.store.ListIssues(repo.ID, "all") {
			if issue.AuthorID == user.ID {
				contributed[repo.ID] = repo
				break
			}
		}
		if contributed[repo.ID] != nil {
			continue
		}
		for _, pr := range s.store.ListPullRequests(repo.ID, "all") {
			if pr.AuthorID == user.ID {
				contributed[repo.ID] = repo
				break
			}
		}
	}
	out := make([]*store.Repo, 0, len(contributed))
	for _, repo := range contributed {
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out
}

// sortReposByOrderField applies GitHub's RepositoryOrderField to a repository
// list.
func sortReposByOrderField(repos []*store.Repo, field string, descending bool) {
	less := func(a, b *store.Repo) bool {
		switch field {
		case "CREATED_AT":
			return a.CreatedAt.Before(b.CreatedAt)
		case "UPDATED_AT":
			return a.UpdatedAt.Before(b.UpdatedAt)
		case "STARGAZERS":
			if a.StargazersCount != b.StargazersCount {
				return a.StargazersCount < b.StargazersCount
			}
			return a.FullName < b.FullName
		case "NAME":
			return a.FullName < b.FullName
		default: // PUSHED_AT
			return a.PushedAt.Before(b.PushedAt)
		}
	}
	sort.SliceStable(repos, func(i, j int) bool {
		if descending {
			return less(repos[j], repos[i])
		}
		return less(repos[i], repos[j])
	})
}

// pullRequestStateEnum maps a stored pull request onto GitHub's
// PullRequestState.
func pullRequestStateName(pr *store.PullRequest) string {
	return strings.ToUpper(pr.State)
}

// socialAccountProvider identifies the platform a profile link points at, so
// a client renders the right icon. GENERIC is GitHub's own answer for a link
// it does not recognize.
func socialAccountProvider(url string) string {
	host := strings.ToLower(url)
	for _, candidate := range []struct {
		match    string
		provider string
	}{
		{"bsky.app", "BLUESKY"},
		{"facebook.com", "FACEBOOK"},
		{"hometown", "HOMETOWN"},
		{"instagram.com", "INSTAGRAM"},
		{"linkedin.com", "LINKEDIN"},
		{"mastodon", "MASTODON"},
		{"npmjs.com", "NPM"},
		{"reddit.com", "REDDIT"},
		{"threads.net", "THREADS"},
		{"twitch.tv", "TWITCH"},
		{"twitter.com", "TWITTER"},
		{"x.com", "TWITTER"},
		{"youtube.com", "YOUTUBE"},
	} {
		if strings.Contains(host, candidate.match) {
			return candidate.provider
		}
	}
	return "GENERIC"
}

// socialAccountDisplayName is the handle GitHub shows for a profile link: the
// last path segment, or the host when the link names no path.
func socialAccountDisplayName(url string) string {
	trimmed := strings.TrimSuffix(url, "/")
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "https://"), "http://")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 && index < len(trimmed)-1 {
		return trimmed[index+1:]
	}
	return trimmed
}

// sshKeyFingerprint is the SHA-256 fingerprint GitHub reports for an
// authentication key, computed from the same authorized-key text the SSH
// transport authenticates with. A key whose text never parsed has no
// fingerprint to report, and says so rather than inventing one.
func sshKeyFingerprint(authorizedKey string) string {
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(parsed)
}

// sshKeyIdentity renders a key's row id for its node identity.
func sshKeyIdentity(id int) string {
	return strconv.Itoa(id)
}
