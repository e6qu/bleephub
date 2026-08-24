package graphqlapi

// Profile pinned items, shared by User and Organization.
//
// A profile's pins are the repositories and gists its owner chose to show
// first. bleephub stores them as the owner's ordered list of repository full
// names (User.PinnedRepos / Org.PinnedRepos, written by the profile-pin
// surface), so the connection reports exactly those, in the order the owner
// put them in, filtered to the ones the request may see.

import (
	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// pinnedItemsOwner is the half of an account the pinned-item fields need: how
// to read its pins, its pinnable repositories and gists, and whether the
// request may change the pins.
type pinnedItemsOwner struct {
	login              string
	pinned             []string
	repositories       []*store.Repo
	gists              []*store.Gist
	viewerCanChangePin bool
}

// addPinnedItemFields installs the pinned-item family on User and
// Organization: both carry the same six members over the same union.
func (s *Resolver) addPinnedItemFields(types *accountSurfaceTypes) {
	pinnableItem := s.gqlPinnableItemUnion(types)
	pinnableItemType := s.sharedEnum("PinnableItemType",
		"GIST", "ISSUE", "ORGANIZATION", "PROJECT", "PULL_REQUEST", "REPOSITORY", "TEAM", "USER")
	connection := s.accountConnectionType(types, "PinnableItem", pinnableItem, false, nil)

	showcase := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProfileItemShowcase",
		Fields: graphql.Fields{
			"hasPinnedItems": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"items": &graphql.Field{
				Type: graphql.NewNonNull(connection),
				Args: connectionArgs(nil),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, err := graphQLSourceMap(p.Source)
					if err != nil {
						return nil, err
					}
					items, _ := src["_items"].([]gqlConnItem)
					return paginateGQLItems(items, p.Args), nil
				},
			},
		},
	})

	// The two accounts differ only in how their pins and pinnable content are
	// read, so the six fields are installed from one description of each.
	for _, account := range []struct {
		object *graphql.Object
		owner  func(p graphql.ResolveParams) (*pinnedItemsOwner, error)
	}{
		{types.user, s.pinnedItemsOwnerForUser},
		{types.organization, s.pinnedItemsOwnerForOrg},
	} {
		read := account.owner
		account.object.AddFieldConfig("pinnedItems", &graphql.Field{
			Type: graphql.NewNonNull(connection),
			Args: connectionArgs(graphql.FieldConfigArgument{
				"types": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(pinnableItemType))},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				owner, err := read(p)
				if err != nil {
					return nil, err
				}
				return paginateGQLItems(s.pinnedItemConnItems(owner, p.Args), p.Args), nil
			},
		})
		account.object.AddFieldConfig("pinnableItems", &graphql.Field{
			Type: graphql.NewNonNull(connection),
			Args: connectionArgs(graphql.FieldConfigArgument{
				"types": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(pinnableItemType))},
			}),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				owner, err := read(p)
				if err != nil {
					return nil, err
				}
				return paginateGQLItems(s.pinnableItemConnItems(owner, p.Args), p.Args), nil
			},
		})
		account.object.AddFieldConfig("anyPinnableItems", &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Args: graphql.FieldConfigArgument{
				"type": &graphql.ArgumentConfig{Type: pinnableItemType},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				owner, err := read(p)
				if err != nil {
					return nil, err
				}
				kind, _ := p.Args["type"].(string)
				filter := map[string]interface{}{}
				if kind != "" {
					filter["types"] = []interface{}{kind}
				}
				return len(s.pinnableItemConnItems(owner, filter)) > 0, nil
			},
		})
		account.object.AddFieldConfig("pinnedItemsRemaining", &graphql.Field{
			Type: graphql.NewNonNull(graphql.Int),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				owner, err := read(p)
				if err != nil {
					return nil, err
				}
				remaining := store.MaxPinnedRepos - len(s.pinnedItemConnItems(owner, map[string]interface{}{}))
				if remaining < 0 {
					remaining = 0
				}
				return remaining, nil
			},
		})
		account.object.AddFieldConfig("viewerCanChangePinnedItems", &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				owner, err := read(p)
				if err != nil {
					return nil, err
				}
				return owner.viewerCanChangePin, nil
			},
		})
		account.object.AddFieldConfig("itemShowcase", &graphql.Field{
			Type: graphql.NewNonNull(showcase),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				owner, err := read(p)
				if err != nil {
					return nil, err
				}
				items := s.pinnedItemConnItems(owner, map[string]interface{}{})
				return map[string]interface{}{
					"hasPinnedItems": len(items) > 0,
					"_items":         items,
				}, nil
			},
		})
	}
}

// gqlPinnableItemUnion is GitHub's PinnableItem: the two kinds of record a
// profile may pin.
func (s *Resolver) gqlPinnableItemUnion(types *accountSurfaceTypes) *graphql.Union {
	if types.pinnableItem != nil {
		return types.pinnableItem
	}
	gistType := s.gqlGistType()
	types.pinnableItem = graphql.NewUnion(graphql.UnionConfig{
		Name:  "PinnableItem",
		Types: []*graphql.Object{gistType, types.repository},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if source, ok := p.Value.(map[string]interface{}); ok {
				if name, _ := source["__typename"].(string); name == "Gist" {
					return gistType
				}
			}
			return types.repository
		},
	})
	return types.pinnableItem
}

// pinnedItemsOwnerForUser reads an account's pins and pinnable content. Only
// the repositories and gists the request may see are pinnable or reported as
// pinned: a pinned private repository must not become visible through the
// profile.
func (s *Resolver) pinnedItemsOwnerForUser(p graphql.ResolveParams) (*pinnedItemsOwner, error) {
	user, err := s.userFromSource(p.Source)
	if err != nil {
		return nil, err
	}
	viewer := s.ghUserFromContext(p.Context)
	return &pinnedItemsOwner{
		login:              user.Login,
		pinned:             s.store.ListPinnedRepos(user.ID),
		repositories:       s.visibleRepos(p.Context, s.store.ListReposByOwner(user.Login)),
		gists:              s.visibleGistsFor(p, user),
		viewerCanChangePin: viewer != nil && viewer.ID == user.ID,
	}, nil
}

// pinnedItemsOwnerForOrg does the same for an organization profile. An
// organization owns no gists, so only its repositories are pinnable.
func (s *Resolver) pinnedItemsOwnerForOrg(p graphql.ResolveParams) (*pinnedItemsOwner, error) {
	org, err := s.orgFromSource(p.Source)
	if err != nil {
		return nil, err
	}
	pinned, _ := s.store.ListOrgPinnedRepos(org.Login)
	return &pinnedItemsOwner{
		login:              org.Login,
		pinned:             pinned,
		repositories:       s.visibleRepos(p.Context, s.store.ListReposByOwner(org.Login)),
		viewerCanChangePin: s.viewerCanAdminAccount(p.Context, org.Login),
	}, nil
}

// pinnedItemConnItems renders the account's pins in the order the owner chose,
// dropping any whose repository the request may not see.
func (s *Resolver) pinnedItemConnItems(owner *pinnedItemsOwner, args map[string]interface{}) []gqlConnItem {
	if !pinnableTypeWanted(args, "REPOSITORY") {
		return nil
	}
	visible := map[string]*store.Repo{}
	for _, repo := range owner.repositories {
		visible[repo.FullName] = repo
	}
	items := make([]gqlConnItem, 0, len(owner.pinned))
	for _, fullName := range owner.pinned {
		repo := visible[fullName]
		if repo == nil {
			continue
		}
		row := repo
		items = append(items, gqlConnItem{
			identity: row.NodeID,
			render:   func() map[string]interface{} { return pinnableRepositorySource(s.store, row) },
		})
	}
	return items
}

// pinnableItemConnItems renders everything the account could pin: the
// repositories it owns and the gists it owns, as far as the request can see
// them.
func (s *Resolver) pinnableItemConnItems(owner *pinnedItemsOwner, args map[string]interface{}) []gqlConnItem {
	var items []gqlConnItem
	if pinnableTypeWanted(args, "REPOSITORY") {
		for i := range owner.repositories {
			row := owner.repositories[i]
			items = append(items, gqlConnItem{
				identity: row.NodeID,
				render:   func() map[string]interface{} { return pinnableRepositorySource(s.store, row) },
			})
		}
	}
	if pinnableTypeWanted(args, "GIST") {
		// gistToGQL leaves `owner` out of the source when the account is
		// missing, which is what keeps the absent child from becoming a
		// typed-nil map.
		var ownerSource map[string]interface{}
		if account := s.store.LookupUserByLogin(owner.login); account != nil {
			ownerSource = userToGraphQL(account)
		}
		for i := range owner.gists {
			row := owner.gists[i]
			items = append(items, gqlConnItem{
				identity: row.NodeID,
				render: func() map[string]interface{} {
					source := gistToGQL(row, ownerSource)
					source["__typename"] = "Gist"
					return source
				},
			})
		}
	}
	return items
}

// pinnableRepositorySource renders a repository as the PinnableItem union
// member it is.
func pinnableRepositorySource(st *store.Store, repo *store.Repo) map[string]interface{} {
	source := repoToGraphQL(st, st.SnapRepo(repo))
	source["__typename"] = "Repository"
	return source
}

// pinnableTypeWanted reports whether the `types` argument admits a kind. An
// absent argument admits every kind, which is GitHub's default.
func pinnableTypeWanted(args map[string]interface{}, kind string) bool {
	wanted := stringListArg(args["types"])
	if len(wanted) == 0 {
		return true
	}
	for _, value := range wanted {
		if value == kind {
			return true
		}
	}
	return false
}
