package graphqlapi

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// User.gists and the Gist object graph behind it — the surface `gh gist list`
// reads. gh asks for it as
//
//	viewer{gists(first:$n, after:$c, privacy:$visibility,
//	             orderBy:{field:CREATED_AT, direction:DESC}){
//	  nodes{description files{name text} isPublic name updatedAt}
//	  pageInfo{hasNextPage endCursor}}}
//
// so the enum, the ordering input and the connection all have to exist before
// the command can run at all: without them its document fails validation and
// `gh gist list` reports a schema error rather than listing anything.
//
// The gists themselves are the ones the REST /gists surface already serves —
// this is a second reading of one store, not a second store.

// gqlGistPrivacyEnum returns GitHub's GistPrivacy enum (memoized).
func (s *Resolver) gqlGistPrivacyEnum() *graphql.Enum {
	return s.graphQLEnum("GistPrivacy", "ALL", "PUBLIC", "SECRET")
}

// gqlGistOrderInput returns GitHub's GistOrder input object (memoized through
// the enum registry it is built from).
func (s *Resolver) gqlGistOrderInput() *graphql.InputObject {
	// Memoized by name: both User.gists and Gist.forks name this input, and a
	// schema may contain only one type called "GistOrder".
	return s.mutationInput("GistOrder", graphql.InputObjectConfigFieldMap{
		"direction": &graphql.InputObjectFieldConfig{
			Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC")),
		},
		"field": &graphql.InputObjectFieldConfig{
			Type: graphql.NewNonNull(s.graphQLEnum("GistOrderField", "CREATED_AT", "PUSHED_AT", "UPDATED_AT")),
		},
	})
}

// gqlGistFileType returns GitHub's GistFile object (memoized).
func (s *Resolver) gqlGistFileType() *graphql.Object {
	if s.graphqlTypes.gistFile != nil {
		return s.graphqlTypes.gistFile
	}
	s.graphqlTypes.gistFile = graphql.NewObject(graphql.ObjectConfig{
		Name: "GistFile",
		Fields: graphql.Fields{
			"name":        &graphql.Field{Type: graphql.String},
			"encodedName": &graphql.Field{Type: graphql.String},
			"encoding":    &graphql.Field{Type: graphql.String},
			"extension":   &graphql.Field{Type: graphql.String},
			"isImage":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isTruncated": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"size":        &graphql.Field{Type: graphql.Int},
			"text": &graphql.Field{
				Type: graphql.String,
				Args: graphql.FieldConfigArgument{
					"truncate": &graphql.ArgumentConfig{Type: graphql.Int},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					file, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					text, present := file["text"].(string)
					if !present {
						// A binary file has no UTF-8 text, which GitHub
						// answers with null rather than mojibake.
						return nil, nil
					}
					if truncate, ok := intArg(p.Args, "truncate"); ok && truncate >= 0 && truncate < len(text) {
						return text[:truncate], nil
					}
					return text, nil
				},
			},
		},
	})
	return s.graphqlTypes.gistFile
}

// gqlGistType returns GitHub's Gist object (memoized).
func (s *Resolver) gqlGistType() *graphql.Object {
	if s.graphqlTypes.gist != nil {
		return s.graphqlTypes.gist
	}
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	s.graphqlTypes.gist = graphql.NewObject(graphql.ObjectConfig{
		Name: "Gist",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					gist, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("gist source: unexpected type %T", p.Source)
					}
					return gist["nodeID"], nil
				},
			},
			// GitHub's Gist.name is the gist's own hash-shaped identifier, not
			// a file name and not the description.
			"name":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description":  &graphql.Field{Type: graphql.String},
			"isPublic":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isFork":       &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"createdAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"pushedAt":     &graphql.Field{Type: dateTime},
			"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
			"resourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"owner":        &graphql.Field{Type: s.graphqlTypes.repositoryOwner},
			"viewerHasStarred": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					gist, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("gist source: unexpected type %T", p.Source)
					}
					viewer := s.ghUserFromContext(p.Context)
					id, _ := gist["gistID"].(string)
					if viewer == nil || id == "" {
						return false, nil
					}
					return s.store.IsGistStarred(viewer.ID, id), nil
				},
			},
			"files": &graphql.Field{
				Type: graphql.NewList(s.gqlGistFileType()),
				Args: graphql.FieldConfigArgument{
					"limit": &graphql.ArgumentConfig{Type: graphql.Int},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					gist, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("gist source: unexpected type %T", p.Source)
					}
					files, _ := gist["files"].([]interface{})
					limit, ok := intArg(p.Args, "limit")
					if !ok {
						// GitHub's own default for the argument.
						limit = 10
					}
					if limit >= 0 && limit < len(files) {
						files = files[:limit]
					}
					return files, nil
				},
			},
		},
	})
	return s.graphqlTypes.gist
}

// gqlGistConnectionType returns GitHub's GistConnection (memoized), with its
// GistEdge.
func (s *Resolver) gqlGistConnectionType() *graphql.Object {
	if s.graphqlTypes.gistConnection != nil {
		return s.graphqlTypes.gistConnection
	}
	gistType := s.gqlGistType()
	s.graphqlTypes.gistEdge = graphql.NewObject(graphql.ObjectConfig{
		Name: "GistEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: gistType},
		},
	})
	s.graphqlTypes.gistConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "GistConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(s.graphqlTypes.gistEdge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(gistType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	return s.graphqlTypes.gistConnection
}

// addGistFieldsToSchema hangs User.gists off the shared User type, which is
// also what `viewer` resolves to — the field `gh gist list` reads.
func (s *Resolver) addGistFieldsToSchema(userType *graphql.Object) {
	userType.AddFieldConfig("gists", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlGistConnectionType()),
		Args: graphql.FieldConfigArgument{
			"first":   &graphql.ArgumentConfig{Type: graphql.Int},
			"last":    &graphql.ArgumentConfig{Type: graphql.Int},
			"after":   &graphql.ArgumentConfig{Type: graphql.String},
			"before":  &graphql.ArgumentConfig{Type: graphql.String},
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlGistOrderInput()},
			"privacy": &graphql.ArgumentConfig{Type: s.gqlGistPrivacyEnum()},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("user source: unexpected type %T", p.Source)
			}
			ownerID, _ := source["databaseId"].(int)
			owner := s.store.GetUserByID(ownerID)
			if owner == nil {
				return emptyGistConnection(), nil
			}
			gists := s.visibleGistsFor(p, owner)
			gists = filterGistsByPrivacy(gists, p.Args["privacy"])
			sortGists(gists, p.Args["orderBy"])

			ownerSource := userToGraphQL(owner)
			items := make([]gqlConnItem, 0, len(gists))
			for _, gist := range gists {
				gist := gist
				items = append(items, gqlConnItem{
					identity: gist.NodeID,
					render:   func() map[string]interface{} { return gistToGQL(gist, ownerSource) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// visibleGistsFor returns the owner's gists the request may see. A secret gist
// is readable only by the account that owns it, exactly as the REST /gists
// surface treats it — listing somebody else's secret gists through GraphQL
// would be a way around that.
func (s *Resolver) visibleGistsFor(p graphql.ResolveParams, owner *store.User) []*store.Gist {
	gists := s.store.ListGistsForUser(owner.ID, time.Time{})
	viewer := s.ghUserFromContext(p.Context)
	if viewer != nil && viewer.ID == owner.ID {
		return gists
	}
	visible := make([]*store.Gist, 0, len(gists))
	for _, gist := range gists {
		if gist.Public {
			visible = append(visible, gist)
		}
	}
	return visible
}

// filterGistsByPrivacy applies the GistPrivacy argument. Absent means ALL,
// which is GitHub's own default for the field.
func filterGistsByPrivacy(gists []*store.Gist, privacy interface{}) []*store.Gist {
	want, _ := privacy.(string)
	if want != "PUBLIC" && want != "SECRET" {
		return gists
	}
	out := make([]*store.Gist, 0, len(gists))
	for _, gist := range gists {
		if gist.Public == (want == "PUBLIC") {
			out = append(out, gist)
		}
	}
	return out
}

// sortGists applies the GistOrder argument. bleephub records no push distinct
// from an update — a gist's content and its git history are the same write —
// so PUSHED_AT and UPDATED_AT order by the same instant.
func sortGists(gists []*store.Gist, orderBy interface{}) {
	field, direction := "CREATED_AT", "DESC"
	if order, ok := orderBy.(map[string]interface{}); ok {
		if value, ok := order["field"].(string); ok && value != "" {
			field = value
		}
		if value, ok := order["direction"].(string); ok && value != "" {
			direction = value
		}
	}
	key := func(gist *store.Gist) time.Time {
		if field == "CREATED_AT" {
			return gist.CreatedAt
		}
		return gist.UpdatedAt
	}
	sort.SliceStable(gists, func(a, b int) bool {
		left, right := key(gists[a]), key(gists[b])
		if left.Equal(right) {
			// Ties would otherwise order by whatever the store's map walk
			// produced, which moves cursors between identical requests. The
			// node id carries the creation sequence, so equal timestamps still
			// order oldest-to-newest rather than by the gist's random name.
			if direction == "ASC" {
				return gists[a].NodeID < gists[b].NodeID
			}
			return gists[a].NodeID > gists[b].NodeID
		}
		if direction == "ASC" {
			return left.Before(right)
		}
		return left.After(right)
	})
}

func emptyGistConnection() map[string]interface{} {
	return paginateGQLItems(nil, nil)
}

// addGistResidueFields completes the Starrable and comment/fork members of the
// Gist and GistFile objects. It runs late (from the misc installer, after the
// account surface has built GistCommentConnection and the star/stargazer
// connection types), so it reaches those cross-family types read-only rather
// than re-minting them.
func (s *Resolver) addGistResidueFields() {
	gistType := s.gqlGistType()

	// GistCommentConnection is the type User.gistComments already serves; it is
	// built by the account surface, which runs before this installer.
	types := s.accountSurfaceRegistry()
	commentConnection := s.accountConnectionType(types, "GistComment", s.gqlGistCommentType(types), false, nil)
	gistType.AddFieldConfig("comments", &graphql.Field{
		Type: graphql.NewNonNull(commentConnection),
		Args: connectionPagingArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			gistID := gistSourceString(p.Source, "gistID")
			comments := s.store.ListGistComments(gistID)
			items := make([]gqlConnItem, 0, len(comments))
			for i := range comments {
				c := comments[i]
				items = append(items, gqlConnItem{
					identity: c.NodeID,
					render: func() map[string]interface{} {
						return map[string]interface{}{
							"id":        c.NodeID,
							"nodeID":    c.NodeID,
							"_dbID":     c.ID,
							"authorID":  c.UserID,
							"gistID":    c.GistID,
							"body":      c.Body,
							"createdAt": c.CreatedAt.UTC().Format(time.RFC3339),
							"updatedAt": c.UpdatedAt.UTC().Format(time.RFC3339),
							"author":    optionalRendered(s.store.GetUserByID(c.UserID), userToGraphQL),
						}
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	gistType.AddFieldConfig("forks", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlGistConnectionType()),
		Args: graphql.FieldConfigArgument{
			"first":   &graphql.ArgumentConfig{Type: graphql.Int},
			"last":    &graphql.ArgumentConfig{Type: graphql.Int},
			"after":   &graphql.ArgumentConfig{Type: graphql.String},
			"before":  &graphql.ArgumentConfig{Type: graphql.String},
			"orderBy": &graphql.ArgumentConfig{Type: s.gqlGistOrderInput()},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			gistID := gistSourceString(p.Source, "gistID")
			forks := s.store.ListGistForks(gistID)
			sortGists(forks, p.Args["orderBy"])
			items := make([]gqlConnItem, 0, len(forks))
			for i := range forks {
				fork := forks[i]
				items = append(items, gqlConnItem{
					identity: fork.NodeID,
					render: func() map[string]interface{} {
						owner := optionalRendered(s.store.GetUserByID(fork.OwnerID), userToGraphQL)
						ownerMap, _ := owner.(map[string]interface{})
						return gistToGQL(fork, ownerMap)
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	gistType.AddFieldConfig("stargazerCount", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return len(s.store.GistStargazerIDs(gistSourceString(p.Source, "gistID"))), nil
		},
	})

	gistType.AddFieldConfig("stargazers", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlStargazerConnectionType()),
		Args: s.stargazerConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			gistID := gistSourceString(p.Source, "gistID")
			// The gist star store records no per-star timestamp; the gist's own
			// creation instant stands in for starredAt, mirroring how the
			// repository stargazer connection uses the repository's.
			starredAt := gistSourceString(p.Source, "createdAt")
			ids := s.store.GistStargazerIDs(gistID)
			items := make([]gqlConnItem, 0, len(ids))
			for _, id := range ids {
				id := id
				user := s.store.GetUserByID(id)
				if user == nil {
					continue
				}
				items = append(items, gqlConnItem{
					identity: user.NodeID,
					render: func() map[string]interface{} {
						node := userToGraphQL(user)
						node["starredAt"] = starredAt
						return node
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// GistFile.language is inferred from the file name's extension; bleephub
	// carries the same small Linguist-style map the repository language fields
	// use, and returns null for an extension it does not recognise.
	s.gqlGistFileType().AddFieldConfig("language", &graphql.Field{
		Type: s.gqlLanguageType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			file, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			name, _ := file["name"].(string)
			lang, ok := store.LanguageForFilename(name)
			if !ok {
				return nil, nil
			}
			return map[string]interface{}{"name": lang}, nil
		},
	})
}

// gistSourceString reads a string key off a Gist source map.
func gistSourceString(source interface{}, key string) string {
	m, _ := source.(map[string]interface{})
	v, _ := m[key].(string)
	return v
}

// gistToGQL renders one gist as the Gist type's source map.
func gistToGQL(gist *store.Gist, owner map[string]interface{}) map[string]interface{} {
	resourcePath := "/gist/" + gist.ID
	source := map[string]interface{}{
		"nodeID":       gist.NodeID,
		"gistID":       gist.ID,
		"name":         gist.ID,
		"description":  gist.Description,
		"isPublic":     gist.Public,
		"isFork":       gist.ForkOfID != "",
		"createdAt":    gist.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":    gist.UpdatedAt.UTC().Format(time.RFC3339),
		"pushedAt":     gist.UpdatedAt.UTC().Format(time.RFC3339),
		"url":          externalURL(resourcePath),
		"resourcePath": resourcePath,
		"files":        gistFileSources(gist),
	}
	if owner != nil {
		source["owner"] = owner
	}
	return source
}

// gistFileSources renders a gist's files in name order: the store holds them
// in a map, and an unordered connection would move a client's cursors between
// two requests that asked the same question.
func gistFileSources(gist *store.Gist) []interface{} {
	names := make([]string, 0, len(gist.Files))
	for name := range gist.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]interface{}, 0, len(names))
	for _, name := range names {
		file := gist.Files[name]
		if file == nil {
			continue
		}
		source := map[string]interface{}{
			"name":        file.Filename,
			"encodedName": strings.ReplaceAll(file.Filename, " ", "%20"),
			"encoding":    "utf-8",
			"extension":   path.Ext(file.Filename),
			"isImage":     strings.HasPrefix(file.Type, "image/"),
			"isTruncated": false,
			"size":        file.Size,
		}
		// text is absent rather than empty for a file whose bytes were not
		// loaded, so GistFile.text answers null instead of claiming the file
		// is empty.
		if file.Content != "" {
			source["text"] = file.Content
		}
		out = append(out, source)
	}
	return out
}
