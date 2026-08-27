package graphqlapi

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/graphql-go/graphql"
)

// addRepoFieldsToSchema adds repository types, queries, and mutations to the
// schema, after userType and queryType exist.
func (s *Resolver) addRepoFieldsToSchema(
	userType, queryType *graphql.Object,
	nodeInterface *graphql.Interface,
) (*graphql.Object, *graphql.Object, *graphql.Field, *graphql.Field) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	gitSSHRemote := s.graphQLStringScalar("GitSSHRemote")
	// Shared enum table: a later family naming RepositoryVisibility reuses this
	// one type instead of minting a duplicate.
	repositoryVisibilityEnum := s.sharedEnum("RepositoryVisibility", "PUBLIC", "PRIVATE", "INTERNAL")
	repoType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Repository",
		Interfaces: []*graphql.Interface{
			nodeInterface,
			s.uniformResourceLocatableInterface(),
			s.starrableInterface(),
			s.subscribableInterface(),
			// Declared at construction: graphql-go memoizes an object's
			// interface list on first read.
			s.projectOwnerInterfaceType(),
			// RepositoryInfo, the shared repository-shape interface
			// RepositoryInvitation.repository returns.
			s.repositoryInfoInterface(),
		},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["nodeID"], nil
				},
			},
			"databaseId":     &graphql.Field{Type: graphql.Int},
			"name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"nameWithOwner":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description":    &graphql.Field{Type: graphql.String},
			"url":            &graphql.Field{Type: graphql.NewNonNull(uri)},
			"resourcePath":   &graphql.Field{Type: graphql.NewNonNull(uri)},
			"sshUrl":         &graphql.Field{Type: graphql.NewNonNull(gitSSHRemote)},
			"isPrivate":      &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isFork":         &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isArchived":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"visibility":     &graphql.Field{Type: graphql.NewNonNull(repositoryVisibilityEnum)},
			"createdAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"pushedAt":       &graphql.Field{Type: dateTime},
			"stargazerCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"owner": &graphql.Field{
				Type: graphql.NewNonNull(s.graphqlTypes.repositoryOwner),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return r["owner"], nil
				},
			},
		},
	})

	// Ref and GitObject point back at Repository, so register the repository
	// type before installing fields that reference them.
	s.graphqlTypes.repository = repoType
	refType := s.gqlRefType()
	s.addGitObjectFieldsToRepository(repoType)

	repoType.AddFieldConfig("defaultBranchRef", &graphql.Field{
		Type: refType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			branch, _ := r["defaultBranch"].(string)
			if branch == "" {
				return nil, nil
			}
			fullName, _ := r["nameWithOwner"].(string)
			return gitRefSource(fullName, "refs/heads/"+branch, ""), nil
		},
	})

	// --- Repository fields gh CLI selects (clone/create/view --json) ---
	// These resolve from the same store state as the REST repository shape.

	repoType.AddFieldConfig("hasWikiEnabled", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["hasWiki"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing hasWiki")
			}
			return v, nil
		},
	})
	repoType.AddFieldConfig("hasIssuesEnabled", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["hasIssues"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing hasIssues")
			}
			return v, nil
		},
	})
	repoType.AddFieldConfig("parent", &graphql.Field{
		Type: repoType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			parentID, ok := r["parentID"].(int)
			if !ok {
				return nil, fmt.Errorf("repository source missing parentID")
			}
			if parentID == 0 {
				return nil, nil
			}
			parent := s.store.GetRepoByID(parentID)
			if parent == nil {
				return nil, fmt.Errorf("repository parent %d not found", parentID)
			}
			return repoToGraphQL(s.store, s.store.SnapRepo(parent)), nil
		},
	})
	repoType.AddFieldConfig("templateRepository", &graphql.Field{
		Type: repoType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			templateID, ok := r["templateRepoID"].(int)
			if !ok {
				return nil, fmt.Errorf("repository source missing templateRepoID")
			}
			if templateID == 0 {
				return nil, nil
			}
			templateRepo := s.store.GetRepoByID(templateID)
			if templateRepo == nil {
				return nil, nil
			}
			return repoToGraphQL(s.store, s.store.SnapRepo(templateRepo)), nil
		},
	})
	repoType.AddFieldConfig("homepageUrl", &graphql.Field{
		Type: uri,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			homepage, ok := r["homepage"].(string)
			if !ok {
				return nil, fmt.Errorf("repository source missing homepage")
			}
			if homepage == "" {
				return nil, nil
			}
			return homepage, nil
		},
	})
	repoType.AddFieldConfig("hasProjectsEnabled", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["hasProjects"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing hasProjects")
			}
			return v, nil
		},
	})
	repoType.AddFieldConfig("hasDiscussionsEnabled", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["hasDiscussions"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing hasDiscussions")
			}
			return v, nil
		},
	})
	repoType.AddFieldConfig("forkCount", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, ok := r["databaseId"].(int)
			if !ok || repoID == 0 {
				return nil, fmt.Errorf("repository forkCount source missing databaseId")
			}
			return s.store.CountForks(repoID), nil
		},
	})
	repoType.AddFieldConfig("watchers", &graphql.Field{
		// watchers: UserConnection! — the shared connection type, backed by
		// the subscription store.
		Type: graphql.NewNonNull(s.gqlUserConnectionType(userType)),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, ok := r["databaseId"].(int)
			if !ok || repoID == 0 {
				return nil, fmt.Errorf("repository watcher source missing databaseId")
			}
			subscribers := s.store.ListRepoSubscribers(repoID)
			nodes := make([]map[string]interface{}, 0, len(subscribers))
			for _, u := range subscribers {
				nodes = append(nodes, userToGraphQL(u))
			}
			return map[string]interface{}{
				"nodes":      nodes,
				"totalCount": len(nodes),
				"pageInfo": map[string]interface{}{
					"hasNextPage":     false,
					"hasPreviousPage": false,
					"startCursor":     nil,
					"endCursor":       nil,
				},
			}, nil
		},
	})
	repoType.AddFieldConfig("stargazers", &graphql.Field{
		// stargazers: StargazerConnection! — backed by the same star store
		// PUT /user/starred writes.
		Type: graphql.NewNonNull(s.gqlStargazerConnectionType()),
		Args: s.stargazerConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, ok := r["databaseId"].(int)
			if !ok || repoID == 0 {
				return nil, fmt.Errorf("repository stargazer source missing databaseId")
			}
			repo := s.store.GetRepoByID(repoID)
			if repo == nil {
				return gqlConnectionSource(nil), nil
			}
			return repaginateConnection(s.stargazerConnectionSource(repo), p.Args), nil
		},
	})
	repoType.AddFieldConfig("licenseInfo", &graphql.Field{
		// licenseInfo: License — resolved from the vendored license catalog.
		Type: s.gqlLicenseType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			key, _ := r["licenseKey"].(string)
			if key == "" {
				return nil, nil
			}
			if license := graphQLLicenseJSON(key); license != nil {
				return license, nil
			}
			// A license key outside the vendored catalog still resolves from the
			// repo's recorded metadata (License's non-null contract needs body/id/etc.).
			name, ok := r["licenseName"].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("repository source missing licenseName")
			}
			spdxID, _ := r["licenseSPDX"].(string)
			return map[string]interface{}{
				"body": "", "conditions": []interface{}{}, "description": nil,
				"featured": false, "hidden": false, "id": "L_" + key,
				"implementation": nil, "key": key, "limitations": []interface{}{},
				"name": name, "nickname": nil, "permissions": []interface{}{},
				"pseudoLicense": false, "spdxId": nilStr(spdxID), "url": nil,
			}, nil
		},
	})
	languageType := s.gqlLanguageType()
	repoType.AddFieldConfig("primaryLanguage", &graphql.Field{
		// Backed by Repo.Language; null when unset.
		Type: languageType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			lang, _ := r["language"].(string)
			if lang == "" {
				return nil, nil
			}
			return map[string]interface{}{"name": lang}, nil
		},
	})
	repoType.AddFieldConfig("languages", &graphql.Field{
		// gh selects languages(first:100){edges{size,node{name}}}.
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "LanguageConnection",
			Fields: graphql.Fields{
				"edges": &graphql.Field{Type: graphql.NewList(graphql.NewObject(graphql.ObjectConfig{
					Name: "LanguageEdge",
					Fields: graphql.Fields{
						"size":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
						"node":   &graphql.Field{Type: graphql.NewNonNull(languageType)},
						"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
					},
				}))},
				"nodes": &graphql.Field{
					Type: graphql.NewList(languageType),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return languageConnectionNodes(p.Source), nil
					},
				},
				"pageInfo": &graphql.Field{
					Type:    graphql.NewNonNull(s.gqlPageInfoType()),
					Resolve: fullPageInfoResolver,
				},
				"totalSize": &graphql.Field{
					Type: graphql.NewNonNull(graphql.Int),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return languageConnectionTotalSize(p.Source), nil
					},
				},
				"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			},
		}),
		Args: graphql.FieldConfigArgument{
			"first": &graphql.ArgumentConfig{Type: graphql.Int},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			fullName, _ := r["nameWithOwner"].(string)
			owner, repoName, _ := strings.Cut(fullName, "/")
			repo := s.store.GetRepo(owner, repoName)
			if repo == nil {
				return map[string]interface{}{"edges": []interface{}{}, "totalCount": 0}, nil
			}
			counts := s.store.ComputeRepoLanguages(repo)
			first := 100
			if n, ok := intArg(p.Args, "first"); ok && n > 0 && n < first {
				first = n
			}
			edges := make([]interface{}, 0, len(counts))
			// GitHub returns languages sorted by size descending.
			type pair struct {
				lang string
				size int64
			}
			pairs := make([]pair, 0, len(counts))
			for lang, size := range counts {
				pairs = append(pairs, pair{lang, size})
			}
			sort.Slice(pairs, func(i, j int) bool {
				if pairs[i].size != pairs[j].size {
					return pairs[i].size > pairs[j].size
				}
				return pairs[i].lang < pairs[j].lang
			})
			for i, p := range pairs {
				if i >= first {
					break
				}
				edges = append(edges, map[string]interface{}{
					"size":   p.size,
					"node":   map[string]interface{}{"name": p.lang},
					"cursor": encodeCursor(i),
				})
			}
			return map[string]interface{}{"edges": edges, "totalCount": len(pairs)}, nil
		},
	})
	repoType.AddFieldConfig("repositoryTopics", &graphql.Field{
		// Backed by Repo.Topics (REST PUT /repos/{o}/{r}/topics).
		Type: graphql.NewNonNull(s.gqlRepositoryTopicConnectionType()),
		Args: graphql.FieldConfigArgument{
			"first": &graphql.ArgumentConfig{Type: graphql.Int},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			topics, _ := r["topics"].([]string)
			nodes := make([]map[string]interface{}, 0, len(topics))
			edges := make([]map[string]interface{}, 0, len(topics))
			for i, tp := range topics {
				node := map[string]interface{}{"topic": map[string]interface{}{"name": tp}, "name": tp}
				nodes = append(nodes, node)
				edges = append(edges, map[string]interface{}{"node": node, "cursor": encodeCursor(i)})
			}
			return map[string]interface{}{
				"nodes": nodes, "edges": edges, "totalCount": len(nodes),
				"pageInfo": map[string]interface{}{
					"hasNextPage": false, "hasPreviousPage": false,
					"startCursor": nil, "endCursor": nil,
				},
			}, nil
		},
	})
	repoType.AddFieldConfig("deleteBranchOnMerge", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["deleteBranchOnMerge"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing deleteBranchOnMerge")
			}
			return v, nil
		},
	})
	repoType.AddFieldConfig("isTemplate", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["isTemplate"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing isTemplate")
			}
			return v, nil
		},
	})
	repoType.AddFieldConfig("isEmpty", &graphql.Field{
		// True until the repo's git storage has a resolvable HEAD commit.
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			nameWithOwner, _ := r["nameWithOwner"].(string)
			owner, name, ok := strings.Cut(nameWithOwner, "/")
			if !ok {
				return true, nil
			}
			return s.repoHasNoCommits(owner, name), nil
		},
	})
	repoType.AddFieldConfig("archivedAt", &graphql.Field{
		Type: dateTime,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			return r["archivedAt"], nil
		},
	})

	pageInfoType := s.gqlPageInfoType()

	repoEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: repoType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	repoConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(repoType)},
			"edges":      &graphql.Field{Type: graphql.NewList(repoEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(pageInfoType)},
		},
	})
	s.graphqlTypes.repositoryConnection = repoConnectionType

	// Enums gh CLI sends by name (CREATED_AT, DESC, PUBLIC, OWNER, ...), so the
	// schema must declare them. Shared enum table: other repository connections
	// name the same three types.
	repositoryPrivacyEnum := s.sharedEnum("RepositoryPrivacy", "PUBLIC", "PRIVATE")
	repositoryAffiliationEnum := s.sharedEnum("RepositoryAffiliation",
		"OWNER", "COLLABORATOR", "ORGANIZATION_MEMBER")
	orderDirectionEnum := s.sharedEnum("OrderDirection", "ASC", "DESC")
	repositoryOrderInput := s.gqlRepositoryOrderInput()

	// --- Releases (gh release list / view / download / delete) ---
	// gh release list needs $direction typed OrderDirection — that enum must keep
	// its exact name. Backed by the real release store; immutable derives from the
	// repo and org immutable-release settings the REST surface persists.
	releaseType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Release",
		Interfaces: []*graphql.Interface{s.graphqlTypes.reactable},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					rel, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return rel["nodeID"], nil
				},
			},
			"databaseId":   &graphql.Field{Type: graphql.Int},
			"name":         &graphql.Field{Type: graphql.String},
			"tagName":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"isDraft":      &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"immutable":    &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isLatest":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isPrerelease": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"createdAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"publishedAt":  &graphql.Field{Type: dateTime},
			"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
			"description":  &graphql.Field{Type: graphql.String},
		},
	})
	s.graphqlTypes.release = releaseType
	s.addReactableFields(releaseType, "release")

	releaseEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReleaseEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: releaseType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	releaseConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReleaseConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(releaseType)},
			"edges":      &graphql.Field{Type: graphql.NewList(releaseEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	releaseOrderFieldEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "ReleaseOrderField",
		Values: graphql.EnumValueConfigMap{
			"CREATED_AT": &graphql.EnumValueConfig{Value: "CREATED_AT"},
			"NAME":       &graphql.EnumValueConfig{Value: "NAME"},
		},
	})

	repoType.AddFieldConfig("releases", &graphql.Field{
		Type: graphql.NewNonNull(releaseConnectionType),
		Args: graphql.FieldConfigArgument{
			"first": &graphql.ArgumentConfig{Type: graphql.Int},
			"after": &graphql.ArgumentConfig{Type: graphql.String},
			"orderBy": &graphql.ArgumentConfig{Type: graphql.NewInputObject(graphql.InputObjectConfig{
				Name: "ReleaseOrder",
				Fields: graphql.InputObjectConfigFieldMap{
					"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(releaseOrderFieldEnum)},
					"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(orderDirectionEnum)},
				},
			})},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			repoFullName, _ := repo["nameWithOwner"].(string)

			releases := s.store.Releases.List(repoID)

			orderField, direction := "CREATED_AT", "DESC"
			if orderBy, ok := p.Args["orderBy"].(map[string]interface{}); ok {
				if f, ok := orderBy["field"].(string); ok && f != "" {
					orderField = f
				}
				if d, ok := orderBy["direction"].(string); ok && d != "" {
					direction = d
				}
			}
			sort.SliceStable(releases, func(a, b int) bool {
				var less bool
				if orderField == "NAME" {
					less = releases[a].Name < releases[b].Name
				} else {
					less = releases[a].CreatedAt.Before(releases[b].CreatedAt)
				}
				if direction == "DESC" {
					return !less
				}
				return less
			})

			latestID := 0
			if latest := s.store.Releases.Latest(repoID); latest != nil {
				latestID = latest.ID
			}
			immutable := s.repoImmutableReleasesEnabled(repoID)

			first := 30
			if f, ok := intArg(p.Args, "first"); ok && f > 0 {
				first = f
			}
			after, _ := p.Args["after"].(string)

			return paginateGQL(releases, first, after, func(rel *store.Release) map[string]interface{} {
				return releaseToGQL(rel, latestID, repoFullName, immutable)
			}, func(rel *store.Release) string { return rel.NodeID }), nil
		},
	})

	repoType.AddFieldConfig("release", &graphql.Field{
		Type: releaseType,
		Args: graphql.FieldConfigArgument{
			"tagName": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			repoFullName, _ := repo["nameWithOwner"].(string)
			tagName, _ := p.Args["tagName"].(string)

			rel := s.store.Releases.GetByTag(repoID, tagName)
			if rel == nil {
				// A missing release(tagName:) resolves to null, not NOT_FOUND —
				// gh's draft-release lookup keys on the null.
				return nil, nil
			}
			latestID := 0
			if latest := s.store.Releases.Latest(repoID); latest != nil {
				latestID = latest.ID
			}
			return releaseToGQL(rel, latestID, repoFullName, s.repoImmutableReleasesEnabled(repoID)), nil
		},
	})

	repoType.AddFieldConfig("latestRelease", &graphql.Field{
		// gh repo view --json latestRelease selects {publishedAt,tagName,name,url}.
		Type: releaseType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			repoFullName, _ := repo["nameWithOwner"].(string)
			latest := s.store.Releases.Latest(repoID)
			if latest == nil {
				return nil, nil
			}
			return releaseToGQL(latest, latest.ID, repoFullName, s.repoImmutableReleasesEnabled(repoID)), nil
		},
	})

	// One field definition shared by the interface and both implementors, to
	// prevent argument/signature drift that breaks gh and Octokit introspection.
	ownerRepositoriesField := &graphql.Field{
		Type: graphql.NewNonNull(repoConnectionType),
		Args: graphql.FieldConfigArgument{
			"affiliations":      &graphql.ArgumentConfig{Type: graphql.NewList(repositoryAffiliationEnum)},
			"after":             &graphql.ArgumentConfig{Type: graphql.String},
			"before":            &graphql.ArgumentConfig{Type: graphql.String},
			"first":             &graphql.ArgumentConfig{Type: graphql.Int},
			"hasIssuesEnabled":  &graphql.ArgumentConfig{Type: graphql.Boolean},
			"isArchived":        &graphql.ArgumentConfig{Type: graphql.Boolean},
			"isFork":            &graphql.ArgumentConfig{Type: graphql.Boolean},
			"isLocked":          &graphql.ArgumentConfig{Type: graphql.Boolean},
			"last":              &graphql.ArgumentConfig{Type: graphql.Int},
			"orderBy":           &graphql.ArgumentConfig{Type: repositoryOrderInput},
			"ownerAffiliations": &graphql.ArgumentConfig{Type: graphql.NewList(repositoryAffiliationEnum), DefaultValue: []interface{}{"OWNER", "COLLABORATOR"}},
			"privacy":           &graphql.ArgumentConfig{Type: repositoryPrivacyEnum},
			"visibility":        &graphql.ArgumentConfig{Type: repositoryVisibilityEnum},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			owner, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			login, _ := owner["login"].(string)
			user := s.store.LookupUserByLogin(login)
			org := s.store.GetOrg(login)
			if user == nil && org == nil {
				return nil, fmt.Errorf("repository owner not found")
			}
			ownerAffiliations := []string{"owner", "collaborator"}
			if values := graphQLRepositoryAffiliations(p.Args["ownerAffiliations"]); len(values) != 0 {
				ownerAffiliations = values
			}
			var repos []*store.Repo
			if user != nil {
				repos = s.store.ListReposForAuthUser(user, store.RepoListOptions{
					Affiliation: strings.Join(ownerAffiliations, ","),
					NoPaginate:  true,
				})
			} else {
				repos = s.store.ListReposForOrg(org.Login, store.RepoListOptions{NoPaginate: true})
			}

			// Drop what the viewer cannot see, before any other filter.
			repos = s.visibleRepos(p.Context, repos)
			if affiliations := graphQLRepositoryAffiliations(p.Args["affiliations"]); len(affiliations) != 0 {
				viewer := s.ghUserFromContext(p.Context)
				if viewer == nil {
					repos = nil
				} else {
					visibleByAffiliation := s.store.ListReposForAuthUser(viewer, store.RepoListOptions{
						Affiliation: strings.Join(affiliations, ","),
						NoPaginate:  true,
					})
					allowed := make(map[int]bool, len(visibleByAffiliation))
					for _, repo := range visibleByAffiliation {
						allowed[repo.ID] = true
					}
					repos = filterRepos(repos, func(repo *store.Repo) bool { return allowed[repo.ID] })
				}
			}

			if _, hasPrivacy := p.Args["privacy"]; hasPrivacy {
				if _, hasVisibility := p.Args["visibility"]; hasVisibility {
					return nil, fmt.Errorf("privacy and visibility cannot be combined")
				}
			}
			if privacy, ok := p.Args["privacy"].(string); ok {
				var filtered []*store.Repo
				for _, r := range repos {
					switch strings.ToUpper(privacy) {
					case "PUBLIC":
						if !r.Private {
							filtered = append(filtered, r)
						}
					case "PRIVATE":
						if r.Private {
							filtered = append(filtered, r)
						}
					}
				}
				repos = filtered
			}

			if isFork, ok := p.Args["isFork"].(bool); ok {
				var filtered []*store.Repo
				for _, r := range repos {
					if r.Fork == isFork {
						filtered = append(filtered, r)
					}
				}
				repos = filtered
			}
			if archived, ok := p.Args["isArchived"].(bool); ok {
				repos = filterRepos(repos, func(repo *store.Repo) bool { return repo.Archived == archived })
			}
			if hasIssues, ok := p.Args["hasIssuesEnabled"].(bool); ok {
				repos = filterRepos(repos, func(repo *store.Repo) bool { return repo.HasIssues == hasIssues })
			}
			if locked, ok := p.Args["isLocked"].(bool); ok && locked {
				// Bleephub does not currently create locked repositories.
				repos = nil
			}
			if visibility, ok := p.Args["visibility"].(string); ok {
				repos = filterRepos(repos, func(repo *store.Repo) bool {
					return strings.EqualFold(repo.Visibility, visibility)
				})
			}

			sortField := "CREATED_AT"
			sortDescending := true
			if order, ok := p.Args["orderBy"].(map[string]interface{}); ok {
				if field, ok := order["field"].(string); ok && field != "" {
					sortField = field
				}
				if direction, ok := order["direction"].(string); ok {
					sortDescending = direction != "ASC"
				}
			}
			sort.Slice(repos, func(i, j int) bool {
				cmp := 0
				switch sortField {
				case "NAME":
					cmp = strings.Compare(strings.ToLower(repos[i].Name), strings.ToLower(repos[j].Name))
				case "STARGAZERS":
					cmp = repos[i].StargazersCount - repos[j].StargazersCount
				default:
					left, right := repos[i].CreatedAt, repos[j].CreatedAt
					if sortField == "UPDATED_AT" {
						left, right = repos[i].UpdatedAt, repos[j].UpdatedAt
					} else if sortField == "PUSHED_AT" {
						left, right = repos[i].PushedAt, repos[j].PushedAt
					}
					cmp = left.Compare(right)
				}
				if cmp == 0 {
					cmp = repos[i].ID - repos[j].ID
				}
				if sortDescending {
					return cmp > 0
				}
				return cmp < 0
			})
			nodes := make([]map[string]interface{}, 0, len(repos))
			for _, repo := range repos {
				nodes = append(nodes, repoToGraphQL(s.store, repo))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	}
	userType.AddFieldConfig("repositories", ownerRepositoriesField)
	s.graphqlTypes.repositoryOwner.AddFieldConfig("repositories", ownerRepositoriesField)

	ownerRepositoryField := &graphql.Field{
		Type: repoType,
		Args: graphql.FieldConfigArgument{
			"followRenames": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
			"name":          &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			owner, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			login, _ := owner["login"].(string)
			name, _ := p.Args["name"].(string)
			repo := s.store.GetRepo(login, name)
			if repo == nil || (repo.Private && !s.viewerCanReadRepo(p.Context, repo)) {
				return nil, nil
			}
			return repoToGraphQL(s.store, s.store.SnapRepo(repo)), nil
		},
	}
	userType.AddFieldConfig("repository", ownerRepositoryField)
	s.graphqlTypes.repositoryOwner.AddFieldConfig("repository", ownerRepositoryField)

	queryType.AddFieldConfig("repository", &graphql.Field{
		Type: repoType,
		Args: graphql.FieldConfigArgument{
			"owner": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"name":  &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			owner, _ := p.Args["owner"].(string)
			name, _ := p.Args["name"].(string)
			repo := s.store.GetRepo(owner, name)
			// A private repo the viewer can't read must look identical to a
			// missing one, or existence leaks. Mirrors the REST read gate.
			if repo == nil || (repo.Private && !s.viewerCanReadRepo(p.Context, repo)) {
				// The typed NOT_FOUND error gh CLI keys on to report
				// "repository not found".
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name),
				}
			}
			return repoToGraphQL(s.store, s.store.SnapRepo(repo)), nil
		},
	})

	// repositoryOwner(login): the user-or-organization interface
	// `gh repo list <login>` queries.
	queryType.AddFieldConfig("repositoryOwner", &graphql.Field{
		Type: s.graphqlTypes.repositoryOwner,
		Args: graphql.FieldConfigArgument{
			"login": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			login, _ := p.Args["login"].(string)
			if u := s.store.LookupUserByLogin(login); u != nil {
				return userToGraphQL(u), nil
			}
			s.store.Mu.RLock()
			org := s.store.OrgsByLogin[login]
			s.store.Mu.RUnlock()
			if org != nil {
				return orgToGraphQL(org), nil
			}
			return nil, &ghNotFoundError{
				message: fmt.Sprintf("Could not resolve to a RepositoryOwner with the login of '%s'.", login),
			}
		},
	})

	createRepoInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateRepositoryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"name":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"ownerId":          &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"visibility":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(repositoryVisibilityEnum), DefaultValue: "PUBLIC"},
			"description":      &graphql.InputObjectFieldConfig{Type: graphql.String},
			"hasIssuesEnabled": &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"hasWikiEnabled":   &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			// Declaration-only members of GitHub's CreateRepositoryInput; the
			// resolver does not act on them, but the input's shape must match GitHub's.
			"homepageUrl": &graphql.InputObjectFieldConfig{Type: s.graphQLStringScalar("URI")},
			"teamId":      &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"template":    &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
		},
	})

	deleteRepoInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DeleteRepositoryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"repositoryId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	createRepoPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateRepositoryPayload",
		Fields: graphql.Fields{
			"repository": &graphql.Field{Type: repoType},
		},
	})

	deleteRepoPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeleteRepositoryPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	mutationType := graphql.NewObject(graphql.ObjectConfig{
		Name:   "Mutation",
		Fields: graphql.Fields{},
	})

	s.registerMutation(mutationType, "createRepository", &graphql.Field{
		Type: createRepoPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createRepoInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			name, _ := input["name"].(string)
			description, _ := input["description"].(string)
			visibility, _ := input["visibility"].(string)

			private := strings.ToUpper(visibility) == "PRIVATE"
			kind, ownerLogin, err := s.createRepositoryOwner(p, input)
			if err != nil {
				return nil, err
			}
			var repo *store.Repo
			if kind == store.OrganizationAccount {
				owner := s.store.GetOrg(ownerLogin)
				if owner == nil {
					return nil, fmt.Errorf("repository creation for another owner is not authorized")
				}
				repo = s.store.CreateOrgRepo(owner, user, name, description, private)
			} else {
				repo = s.store.CreateRepo(user, name, description, private)
			}
			if repo == nil {
				return nil, fmt.Errorf("repository creation failed")
			}
			if !s.store.UpdateRepo(ownerLogin, name, func(r *store.Repo) {
				if v, ok := graphQLInputBool(input, "hasIssuesEnabled"); ok {
					r.HasIssues = v
				}
				if v, ok := graphQLInputBool(input, "hasWikiEnabled"); ok {
					r.HasWiki = v
				}
			}) {
				return nil, fmt.Errorf("repository %s/%s not found after creation", ownerLogin, name)
			}
			repo = s.store.GetRepo(ownerLogin, name)
			if repo == nil {
				return nil, fmt.Errorf("repository %s/%s not found after update", ownerLogin, name)
			}

			return map[string]interface{}{
				"repository": repoToGraphQL(s.store, s.store.SnapRepo(repo)),
			}, nil
		},
	})

	s.registerMutation(mutationType, "deleteRepository", &graphql.Field{
		Type: deleteRepoPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(deleteRepoInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			repoID, _ := input["repositoryId"].(string)

			found := store.FindRepoByNodeID(s.store, repoID)
			if found == nil {
				return nil, gqlMissingNode("Repository", repoID)
			}

			if _, err := s.store.DeleteRepo(found.Owner.Login, found.Name); err != nil {
				return nil, err
			}

			return map[string]interface{}{
				"clientMutationId": nil,
			}, nil
		},
	})

	return repoType, mutationType, ownerRepositoriesField, ownerRepositoryField
}

func filterRepos(repos []*store.Repo, keep func(*store.Repo) bool) []*store.Repo {
	filtered := make([]*store.Repo, 0, len(repos))
	for _, repo := range repos {
		if keep(repo) {
			filtered = append(filtered, repo)
		}
	}
	return filtered
}

func graphQLRepositoryAffiliations(value interface{}) []string {
	var affiliations []string
	switch values := value.(type) {
	case []interface{}:
		for _, item := range values {
			affiliations = append(affiliations, strings.ToLower(fmt.Sprint(item)))
		}
	case []string:
		for _, item := range values {
			affiliations = append(affiliations, strings.ToLower(item))
		}
	}
	return affiliations
}

// --- Mutation authorization ---
//
// Every mutation reaches the schema through registerMutation, which refuses a
// name graphqlMutationAuthz does not cover. A mutation added without a policy
// row fails at schema build instead of shipping open to any signed-in account.

// mutationRule decides whether the credential behind a request may perform one
// mutation on whatever its input names. One implementation per resource class:
// a repository and a project have different owners and different questions.
type mutationRule interface {
	// check reports a malformed policy row at schema-assembly time, so a row
	// missing its lookup is a build failure rather than a silent authorize-nothing.
	check() error
	authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error
}

// mutationLevel is the entitlement a mutation demands on the repository its
// input names.
type mutationLevel int

const (
	mutationReadRepo mutationLevel = iota
	mutationPushRepo
	mutationAdminRepo
)

// mutationTarget is the repository a mutation acts on.
type mutationTarget struct {
	repo     *store.Repo
	authorID int
	// missing answers both "no such node" and "you may not reach it". They have
	// to be the same answer, or a mutation becomes an existence oracle for
	// private repositories.
	missing error
}

// repoCreationRule is the policy for createRepository, the one mutation that
// names no existing repository: the entitlement is over the account the
// repository would belong to, checked against the credential's grant (not just
// an authenticated viewer).
type repoCreationRule struct{}

func (repoCreationRule) check() error { return nil }

func (repoCreationRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	kind, login, err := s.createRepositoryOwner(p, input)
	if err != nil {
		return err
	}
	if !s.credentialGrantsAccount(p.Context, kind, login, store.ScopeAdministration, store.PermWrite) {
		return fmt.Errorf("resource not accessible by integration")
	}
	if kind == store.OrganizationAccount && !s.viewerIsOrgMember(p.Context, login) {
		return fmt.Errorf("repository creation for another owner is not authorized")
	}
	return nil
}

// createRepositoryOwner resolves the account a createRepository input names.
// The policy row and resolver both call it, so the entitlement is checked
// against the same owner the repository is created under.
func (s *Resolver) createRepositoryOwner(p graphql.ResolveParams, input map[string]interface{}) (store.AccountKind, string, error) {
	user := s.ghUserFromContext(p.Context)
	ownerID, _ := input["ownerId"].(string)
	if ownerID == "" || ownerID == user.NodeID {
		return store.AnyAccount, user.Login, nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, candidate := range s.store.Orgs {
		if candidate.NodeID == ownerID {
			return store.OrganizationAccount, candidate.Login, nil
		}
	}
	return store.AnyAccount, "", fmt.Errorf("repository creation for another owner is not authorized")
}

// repoRule is the policy for a mutation whose subject belongs to a repository.
type repoRule struct {
	// scope is the fine-grained permission an app must hold, per-row because
	// GitHub grants issue triage at issues:write and PR triage at
	// pull_requests:write.
	scope store.PermScope
	// scopeFor, when set, derives the scope from the input. addReaction needs
	// this: an issue reaction needs issues:write, a PR reaction pull_requests:write.
	scopeFor func(s *Resolver, input map[string]interface{}) store.PermScope
	level    mutationLevel
	// authorMayAct admits the author of the targeted content whatever their
	// repository access: editing your own issue or hiding your own comment
	// never required push.
	authorMayAct bool
	target       func(s *Resolver, input map[string]interface{}) mutationTarget
}

func (r repoRule) check() error {
	if r.target == nil {
		return fmt.Errorf("no repository target lookup")
	}
	if r.scope == "" && r.scopeFor == nil {
		return fmt.Errorf("no permission scope")
	}
	return nil
}

func (r repoRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	target := r.target(s, input)
	if target.repo == nil || !s.viewerCanReadRepo(p.Context, target.repo) {
		return target.missing
	}
	// The credential half is asked first and never relaxed by authorship: the
	// author exemption speaks to the bearer, not to an app's grant. Ordering it
	// the other way let an author retitle an issue through an app installed nowhere.
	scope := r.scope
	if r.scopeFor != nil {
		scope = r.scopeFor(s, input)
	}
	if !s.credentialGrantsRepo(p.Context, target.repo, scope, store.PermWrite) {
		return &ghForbiddenError{message: "resource not accessible by integration"}
	}
	user := s.ghUserFromContext(p.Context)
	if r.authorMayAct && target.authorID != 0 && target.authorID == user.ID {
		return nil
	}
	switch r.level {
	case mutationPushRepo:
		if !s.principalHoldsRepoCapability(p.Context, target.repo, store.PermWrite) {
			return &ghForbiddenError{message: "must have push access to Repository"}
		}
	case mutationAdminRepo:
		if !s.principalHoldsRepoCapability(p.Context, target.repo, store.PermAdmin) {
			return &ghForbiddenError{message: "must have admin rights to Repository"}
		}
	}
	return nil
}

// issueTransferRule is the policy for transferIssue, whose input names two
// repositories: it runs two repoRule checks, refusing a bearer with push on
// only one side before the resolver runs.
type issueTransferRule struct{}

func (issueTransferRule) check() error { return nil }

func (issueTransferRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	source := repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")}
	if err := source.authorize(s, p, input); err != nil {
		return err
	}
	destination := repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")}
	return destination.authorize(s, p, input)
}

// graphqlMutationAuthz is the whole authorization policy of the mutation surface.
//
// Each row names a scope (the app grant: issues, pull requests, discussions and
// repository administration are separate) and a level (the bearer's standing on
// the repo). Participation — opening/commenting/reviewing — needs read; editing,
// closing, merging and moderating need push; destroying needs admin.
var graphqlMutationAuthz = map[string]mutationRule{
	"createRepository": repoCreationRule{},
	"deleteRepository": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},

	// PR comments are stored and gated as issue comments, so addComment and the
	// moderation mutations are scopeIssues even for a pull-request subject.
	"createIssue": repoRule{scope: store.ScopeIssues, level: mutationReadRepo, target: mutationTargetRepo("repositoryId")},
	"addComment":  repoRule{scope: store.ScopeIssues, level: mutationReadRepo, target: mutationTargetIssueOrPullRequest("subjectId")},
	"closeIssue":  repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssue("issueId")},
	"reopenIssue": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssue("issueId")},
	"updateIssue": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssue("id")},

	// Pinning is triage; with no triage level, push is the nearest. No author
	// exemption: it is curation of the issues list, not your own content.
	"pinIssue":   repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
	"unpinIssue": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
	// Deleting an issue is GitHub's one admin-gated issue mutation — even the
	// issue's author may not delete it without admin rights.
	"deleteIssue":   repoRule{scope: store.ScopeIssues, level: mutationAdminRepo, target: mutationTargetIssue("issueId")},
	"transferIssue": issueTransferRule{},

	// Linking a branch writes a ref into the repo, so it needs contents;
	// unlinking only removes the association, so it is an issues write.
	"createLinkedBranch": repoRule{scope: store.ScopeContents, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
	"deleteLinkedBranch": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetLinkedBranch("linkedBranchId")},

	"createDiscussion":                repoRule{scope: store.ScopeDiscussions, level: mutationReadRepo, target: mutationTargetRepo("repositoryId")},
	"addDiscussionComment":            repoRule{scope: store.ScopeDiscussions, level: mutationReadRepo, target: mutationTargetDiscussion("discussionId")},
	"addReaction":                     repoRule{scopeFor: reactableScope("subjectId"), level: mutationReadRepo, target: mutationTargetReactable("subjectId")},
	"removeReaction":                  repoRule{scopeFor: reactableScope("subjectId"), level: mutationReadRepo, target: mutationTargetReactable("subjectId")},
	"updateDiscussion":                repoRule{scope: store.ScopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussion("discussionId")},
	"closeDiscussion":                 repoRule{scope: store.ScopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussion("discussionId")},
	"reopenDiscussion":                repoRule{scope: store.ScopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussion("discussionId")},
	"deleteDiscussion":                repoRule{scope: store.ScopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussion("id")},
	"updateDiscussionComment":         repoRule{scope: store.ScopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussionComment("commentId")},
	"deleteDiscussionComment":         repoRule{scope: store.ScopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussionComment("id")},
	"markDiscussionCommentAsAnswer":   repoRule{scope: store.ScopeDiscussions, level: mutationPushRepo, authorMayAct: true, target: mutationTargetAnsweredDiscussion("id")},
	"unmarkDiscussionCommentAsAnswer": repoRule{scope: store.ScopeDiscussions, level: mutationPushRepo, authorMayAct: true, target: mutationTargetAnsweredDiscussion("id")},
	// Upvoting is participation, like reacting: any reader may vote.
	"addUpvote":    repoRule{scope: store.ScopeDiscussions, level: mutationReadRepo, target: mutationTargetVotable("subjectId")},
	"removeUpvote": repoRule{scope: store.ScopeDiscussions, level: mutationReadRepo, target: mutationTargetVotable("subjectId")},

	// Labels. An issue's and a PR's labels share one /issues/{n}/labels surface
	// gated on Issues, so these are scopeIssues whichever kind labelableId names.
	// Labeling is triage curation: push, no author exemption.
	"addLabelsToLabelable":      repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: labelableMutationTarget("labelableId")},
	"removeLabelsFromLabelable": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: labelableMutationTarget("labelableId")},
	"clearLabelsFromLabelable":  repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: labelableMutationTarget("labelableId")},

	"minimizeComment":   repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssueComment("subjectId")},
	"unminimizeComment": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssueComment("subjectId")},
	"lockLockable":      repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetLockable("lockableId")},
	"unlockLockable":    repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetLockable("lockableId")},

	"createPullRequest":             repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: mutationTargetRepo("repositoryId")},
	"addPullRequestReview":          repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: mutationTargetPullRequest("pullRequestId")},
	"submitPullRequestReview":       repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, authorMayAct: true, target: mutationTargetReview("pullRequestReviewId")},
	"dismissPullRequestReview":      repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetReview("pullRequestReviewId")},
	"closePullRequest":              repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"reopenPullRequest":             repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"updatePullRequest":             repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"markPullRequestReadyForReview": repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"convertPullRequestToDraft":     repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"mergePullRequest":              repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
	// Auto-merge arms/disarms a deferred merge, so both sides demand push, like
	// mergePullRequest.
	"enablePullRequestAutoMerge":  repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
	"disablePullRequestAutoMerge": repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
	"resolveReviewThread":         repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReviewThread("threadId")},
	"unresolveReviewThread":       repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReviewThread("threadId")},

	// Projects v2. A project belongs to a user or org, not a repo, so write is
	// owner-scoped: the owning user, or an active member of the owning org.
	"createProjectV2":               projectRule{target: projectTargetOwner("ownerId")},
	"addProjectV2ItemById":          projectRule{target: projectTargetProject("projectId")},
	"deleteProjectV2Item":           projectRule{target: projectTargetProject("projectId")},
	"createProjectV2Field":          projectRule{target: projectTargetProject("projectId")},
	"updateProjectV2ItemFieldValue": projectRule{target: projectTargetProject("projectId")},

	// Project metadata and lifecycle.
	"updateProjectV2":               projectRule{target: projectTargetProject("projectId")},
	"deleteProjectV2":               projectRule{target: projectTargetProject("projectId")},
	"markProjectV2AsTemplate":       projectRule{target: projectTargetProject("projectId")},
	"unmarkProjectV2AsTemplate":     projectRule{target: projectTargetProject("projectId")},
	"linkProjectV2ToRepository":     projectRule{target: projectTargetProject("projectId")},
	"linkProjectV2ToTeam":           projectRule{target: projectTargetProject("projectId")},
	"unlinkProjectV2FromRepository": projectRule{target: projectTargetProject("projectId")},
	"unlinkProjectV2FromTeam":       projectRule{target: projectTargetProject("projectId")},
	"updateProjectV2Collaborators":  projectRule{target: projectTargetProject("projectId")},
	// A copy reads one project and writes to another account, so neither half
	// alone is the entitlement: projectCopyRule asks both.
	"copyProjectV2": projectCopyRule{},

	// Items. The ones GitHub keys on a bare item id still authorize over the
	// project that item belongs to.
	"addProjectV2DraftIssue":                projectRule{target: projectTargetProject("projectId")},
	"archiveProjectV2Item":                  projectRule{target: projectTargetProject("projectId")},
	"unarchiveProjectV2Item":                projectRule{target: projectTargetProject("projectId")},
	"clearProjectV2ItemFieldValue":          projectRule{target: projectTargetProject("projectId")},
	"updateProjectV2ItemPosition":           projectRule{target: projectTargetProject("projectId")},
	"updateProjectV2DraftIssue":             projectRule{target: projectTargetItem("draftIssueId")},
	"convertProjectV2DraftIssueItemToIssue": projectRule{target: projectTargetItem("itemId")},

	// Fields and views, keyed on the subject rather than the project.
	"createProjectV2IssueField": projectRule{target: projectTargetProject("projectId")},
	"updateProjectV2Field":      projectRule{target: projectTargetField("fieldId")},
	"deleteProjectV2Field":      projectRule{target: projectTargetField("fieldId")},
	"createProjectV2View":       projectRule{target: projectTargetProject("projectId")},
	"updateProjectV2View":       projectRule{target: projectTargetView("viewId")},
	"deleteProjectV2View":       projectRule{target: projectTargetView("viewId")},

	// Dismissing a Dependabot alert writes the repo's security events:
	// security_events scope at write, push standing on the repo — the same
	// PATCH /repos/{o}/{r}/dependabot/alerts/{n} demands.
	"dismissRepositoryVulnerabilityAlert": repoRule{
		scope:  store.ScopeSecurityEvents,
		level:  mutationPushRepo,
		target: mutationTargetVulnerabilityAlert("repositoryVulnerabilityAlertId"),
	},

	// Status updates and workflows.
	"createProjectV2StatusUpdate": projectRule{target: projectTargetProject("projectId")},
	"updateProjectV2StatusUpdate": projectRule{target: projectTargetStatusUpdate("statusUpdateId")},
	"deleteProjectV2StatusUpdate": projectRule{target: projectTargetStatusUpdate("statusUpdateId")},
	"deleteProjectV2Workflow":     projectRule{target: projectTargetWorkflow("workflowId")},
}

// projectCopyRule is the policy for copyProjectV2, whose input names two
// accounts: the project copied and the owner it lands under. Standing is
// required on both — read on the source, write on the destination.
type projectCopyRule struct{}

func (projectCopyRule) check() error { return nil }

func (projectCopyRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	source := projectTargetProject("projectId")(s, input)
	if source.owner == nil {
		return source.missing
	}
	user := s.ghUserFromContext(p.Context)
	if source.project != nil && !s.canReadProjectV2(p.Context, user, source.owner, source.project) {
		return source.missing
	}
	destination := projectRule{target: projectTargetOwner("ownerId")}
	return destination.authorize(s, p, input)
}

// guardedMutations records which names reached a mutation type through
// registerMutation. assertMutationsAuthorized consumes and clears the entry, so
// a schema's worth of names is held only while that schema is being assembled.
var guardedMutations = struct {
	mu     sync.Mutex
	byType map[*graphql.Object]map[string]bool
}{byType: map[*graphql.Object]map[string]bool{}}

// registerMutation installs a mutation behind the entitlement its policy row
// declares. It is the only way a mutation should reach the schema;
// assertMutationsAuthorized is the backstop for one that arrives another way.
func (s *Resolver) registerMutation(mutationType *graphql.Object, name string, field *graphql.Field) {
	rule, ok := graphqlMutationAuthz[name]
	if !ok {
		panic(fmt.Sprintf("graphql mutation %q has no row in graphqlMutationAuthz", name))
	}
	if err := rule.check(); err != nil {
		panic(fmt.Sprintf("graphql mutation %q has a malformed policy row: %v", name, err))
	}
	if field.Resolve == nil {
		panic(fmt.Sprintf("graphql mutation %q has no resolver", name))
	}
	if inputArg := field.Args["input"]; inputArg != nil {
		if inputType, ok := unwrapGraphQLType(inputArg.Type).(*graphql.InputObject); ok {
			if _, exists := inputType.Fields()["clientMutationId"]; !exists {
				inputType.AddFieldConfig("clientMutationId", &graphql.InputObjectFieldConfig{Type: graphql.String})
			}
		}
	}
	if payloadType, ok := unwrapGraphQLType(field.Type).(*graphql.Object); ok {
		if _, exists := payloadType.Fields()["clientMutationId"]; !exists {
			payloadType.AddFieldConfig("clientMutationId", &graphql.Field{Type: graphql.String})
		}
	}
	resolve := field.Resolve
	field.Resolve = func(p graphql.ResolveParams) (interface{}, error) {
		// Authentication is common to every rule and is asked here so no rule
		// can forget it; resolvers may then assume a non-nil viewer.
		if s.ghUserFromContext(p.Context) == nil {
			return nil, fmt.Errorf("authentication required")
		}
		input, _ := p.Args["input"].(map[string]interface{})
		if err := rule.authorize(s, p, input); err != nil {
			return nil, err
		}
		result, err := resolve(p)
		if err != nil {
			return nil, err
		}
		if payload, ok := result.(map[string]interface{}); ok {
			payload["clientMutationId"] = input["clientMutationId"]
		}
		return result, nil
	}
	mutationType.AddFieldConfig(name, field)

	guardedMutations.mu.Lock()
	defer guardedMutations.mu.Unlock()
	if guardedMutations.byType[mutationType] == nil {
		guardedMutations.byType[mutationType] = map[string]bool{}
	}
	guardedMutations.byType[mutationType][name] = true
}

func unwrapGraphQLType(value graphql.Type) graphql.Type {
	for {
		nonNull, ok := value.(*graphql.NonNull)
		if !ok {
			return value
		}
		value = nonNull.OfType
	}
}

// assertMutationsAuthorized fails schema construction unless every mutation
// field went through registerMutation and carries a policy row — the backstop
// for a mutation that bypasses the registrar entirely.
func assertMutationsAuthorized(mutationType *graphql.Object) {
	guardedMutations.mu.Lock()
	guarded := guardedMutations.byType[mutationType]
	delete(guardedMutations.byType, mutationType)
	guardedMutations.mu.Unlock()

	var offenders []string
	for name := range mutationType.Fields() {
		if _, ok := graphqlMutationAuthz[name]; !ok {
			offenders = append(offenders, name+" (no row in graphqlMutationAuthz)")
			continue
		}
		if !guarded[name] {
			offenders = append(offenders, name+" (not registered through registerMutation)")
		}
	}
	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	panic("graphql mutations reach the store unauthorized: " + strings.Join(offenders, ", "))
}

// externalURL prefixes a relative resource path with BLEEPHUB_EXTERNAL_URL when
// configured, producing the absolute URL that GitHub's GraphQL `url` fields
// return. GraphQL `resourcePath` fields stay relative and must not use this.
func externalURL(path string) string {
	if base := strings.TrimRight(os.Getenv("BLEEPHUB_EXTERNAL_URL"), "/"); base != "" {
		return base + path
	}
	return path
}

// gqlMissingNode is the NOT_FOUND errors[] entry a mutation returns for a node
// it cannot reach, so gh CLI reports "not found" rather than decoding empty.
func gqlMissingNode(typeName, nodeID string) error {
	return &ghNotFoundError{
		message: fmt.Sprintf("Could not resolve to a %s with the global id of '%s'.", typeName, nodeID),
	}
}

// gqlMissingNodeType is gqlMissingNode for paths that know only the type. It
// still returns a NOT_FOUND-typed error so a client can distinguish "no such
// object" from a transport failure.
func gqlMissingNodeType(typeName string) error {
	article := "a"
	if len(typeName) > 0 {
		switch typeName[0] {
		case 'A', 'E', 'I', 'O', 'U':
			article = "an"
		}
	}
	return &ghNotFoundError{
		message: fmt.Sprintf("Could not resolve to %s %s with the id provided.", article, typeName),
	}
}

func mutationTargetRepo(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		return mutationTarget{
			repo:    store.FindRepoByNodeID(s.store, nodeID),
			missing: gqlMissingNode("Repository", nodeID),
		}
	}
}

func mutationTargetIssue(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Issue", nodeID)}
		if issue := store.FindIssueByNodeID(s.store, nodeID); issue != nil {
			target.repo = s.store.GetRepoByID(issue.RepoID)
			target.authorID = issue.AuthorID
		}
		return target
	}
}

// mutationTargetLinkedBranch resolves a linked branch's global id to the
// repository of the issue that carries the link.
func mutationTargetLinkedBranch(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("LinkedBranch", nodeID)}
		if issue, _, ok := store.FindIssueByLinkedBranchNodeID(s.store, nodeID); ok {
			target.repo = s.store.GetRepoByID(issue.RepoID)
			target.authorID = issue.AuthorID
		}
		return target
	}
}

func mutationTargetPullRequest(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("PullRequest", nodeID)}
		if pr := store.FindPullRequestByNodeID(s.store, nodeID); pr != nil {
			target.repo = s.store.GetRepoByID(pr.RepoID)
			target.authorID = pr.AuthorID
		}
		return target
	}
}

// mutationTargetIssueOrPullRequest covers the mutations whose subject is either
// — GitHub stores pull requests as issues, so addComment and the lock
// mutations take both node kinds.
func mutationTargetIssueOrPullRequest(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("node", nodeID)}
		if issue := store.FindIssueByNodeID(s.store, nodeID); issue != nil {
			target.repo = s.store.GetRepoByID(issue.RepoID)
			target.authorID = issue.AuthorID
			return target
		}
		if pr := store.FindPullRequestByNodeID(s.store, nodeID); pr != nil {
			target.repo = s.store.GetRepoByID(pr.RepoID)
			target.authorID = pr.AuthorID
		}
		return target
	}
}

// mutationTargetLockable covers lockLockable / unlockLockable, whose subject is
// any Lockable: an issue, pull request, or discussion.
func mutationTargetLockable(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	issueOrPR := mutationTargetIssueOrPullRequest(key)
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		if target := issueOrPR(s, input); target.repo != nil {
			return target
		}
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("node", nodeID)}
		if d := store.FindDiscussionByNodeID(s.store, nodeID); d != nil {
			target.repo = s.store.GetRepoByID(d.RepoID)
			target.authorID = d.AuthorID
		}
		return target
	}
}

func mutationTargetIssueComment(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("node", nodeID)}
		c := s.store.LookupCommentByNodeID(nodeID)
		if c == nil {
			return target
		}
		target.authorID = c.AuthorID
		if c.ParentType == "pull_request" {
			if pr := s.store.GetPullRequest(c.IssueID); pr != nil {
				target.repo = s.store.GetRepoByID(pr.RepoID)
			}
			return target
		}
		if issue := s.store.GetIssue(c.IssueID); issue != nil {
			target.repo = s.store.GetRepoByID(issue.RepoID)
		}
		return target
	}
}

// mutationTargetVotable covers addUpvote / removeUpvote, whose subject is any
// Votable: a discussion or a discussion comment.
func mutationTargetVotable(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("node", nodeID)}
		if d := store.FindDiscussionByNodeID(s.store, nodeID); d != nil {
			target.repo = s.store.GetRepoByID(d.RepoID)
			target.authorID = d.AuthorID
			return target
		}
		if c := store.FindDiscussionCommentByNodeID(s.store, nodeID); c != nil {
			target.authorID = c.AuthorID
			if d := s.store.GetDiscussion(c.DiscussionID); d != nil {
				target.repo = s.store.GetRepoByID(d.RepoID)
			}
		}
		return target
	}
}

func mutationTargetDiscussion(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Discussion", nodeID)}
		if d := store.FindDiscussionByNodeID(s.store, nodeID); d != nil {
			target.repo = s.store.GetRepoByID(d.RepoID)
			target.authorID = d.AuthorID
		}
		return target
	}
}

func mutationTargetDiscussionComment(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("DiscussionComment", nodeID)}
		c := store.FindDiscussionCommentByNodeID(s.store, nodeID)
		if c == nil {
			return target
		}
		target.authorID = c.AuthorID
		if d := s.store.GetDiscussion(c.DiscussionID); d != nil {
			target.repo = s.store.GetRepoByID(d.RepoID)
		}
		return target
	}
}

// mutationTargetAnsweredDiscussion addresses a comment but hands back the
// discussion's author: marking an answer is the asker's call, not the
// answerer's.
func mutationTargetAnsweredDiscussion(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("DiscussionComment", nodeID)}
		c := store.FindDiscussionCommentByNodeID(s.store, nodeID)
		if c == nil {
			return target
		}
		if d := s.store.GetDiscussion(c.DiscussionID); d != nil {
			target.repo = s.store.GetRepoByID(d.RepoID)
			target.authorID = d.AuthorID
		}
		return target
	}
}

func mutationTargetReviewThread(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("PullRequestReviewThread", nodeID)}
		threadID, ok := store.ParsePRReviewThreadNodeID(nodeID)
		if !ok {
			return target
		}
		thread := s.store.PRReviewComments.GetThread(threadID)
		if thread == nil || len(thread.Comments) == 0 {
			return target
		}
		pr := s.store.GetPullRequest(thread.Comments[0].PullRequestID)
		if pr == nil {
			return target
		}
		target.repo = s.store.GetRepoByID(pr.RepoID)
		target.authorID = pr.AuthorID
		return target
	}
}

// mutationTargetReview resolves a review node id (PRR_…) to the repo of its
// pull request, for the submit/dismiss lifecycle mutations.
func mutationTargetReview(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("PullRequestReview", nodeID)}
		review := store.FindReviewByNodeID(s.store, nodeID)
		if review == nil {
			return target
		}
		pr := s.store.GetPullRequest(review.PRID)
		if pr == nil {
			return target
		}
		target.repo = s.store.GetRepoByID(pr.RepoID)
		target.authorID = review.AuthorID
		return target
	}
}

// repoToGraphQL converts a Repo to a resolver map. It reads mutable fields
// directly, so the caller must pass a snapshot or a repo it holds the store
// lock over — never the live shared pointer off-lock, which races UpdateRepo.
func repoToGraphQL(st *store.Store, repo *store.Repo) map[string]interface{} {
	return repoToGraphQLWithOrg(repo, st.GetOrgByID)
}

// repoToGraphQLLocked is repoToGraphQL for callers that already hold st.mu.
func repoToGraphQLLocked(st *store.Store, repo *store.Repo) map[string]interface{} {
	return repoToGraphQLWithOrg(repo, func(id int) *store.Org { return st.Orgs[id] })
}

func repoToGraphQLWithOrg(repo *store.Repo, getOrg func(int) *store.Org) map[string]interface{} {
	var ownerMap map[string]interface{}
	if repo.OwnerType == "Organization" {
		if org := getOrg(repo.OwnerID); org != nil {
			ownerMap = orgToGraphQL(org)
		}
	} else if repo.Owner != nil {
		ownerMap = userToGraphQL(repo.Owner)
	}
	resourcePath := "/" + repo.FullName
	webURL := externalURL(resourcePath)

	return map[string]interface{}{
		"nodeID":              repo.NodeID,
		"databaseId":          repo.ID,
		"name":                repo.Name,
		"nameWithOwner":       repo.FullName,
		"description":         repo.Description,
		"url":                 webURL,
		"resourcePath":        resourcePath,
		"sshUrl":              store.SshGitURL(repo.FullName),
		"isPrivate":           repo.Private,
		"isFork":              repo.Fork,
		"isArchived":          repo.Archived,
		"visibility":          strings.ToUpper(repo.Visibility),
		"defaultBranch":       repo.DefaultBranch,
		"stargazerCount":      repo.StargazersCount,
		"language":            repo.Language,
		"licenseKey":          repo.LicenseKey,
		"licenseName":         repo.LicenseName,
		"licenseSPDX":         repo.LicenseSPDX,
		"homepage":            repo.Homepage,
		"topics":              repo.Topics,
		"hasIssues":           repo.HasIssues,
		"hasProjects":         repo.HasProjects,
		"hasWiki":             repo.HasWiki,
		"hasDiscussions":      store.RepoHasDiscussions(repo),
		"parentID":            repo.ParentID,
		"templateRepoID":      repo.TemplateRepoID,
		"allowSquashMerge":    repo.AllowSquashMerge,
		"allowMergeCommit":    repo.AllowMergeCommit,
		"allowRebaseMerge":    repo.AllowRebaseMerge,
		"deleteBranchOnMerge": repo.DeleteBranchOnMerge,
		"isTemplate":          repo.IsTemplate,
		"owner":               optionalObject(ownerMap),
		"createdAt":           repo.CreatedAt.Format(time.RFC3339),
		"updatedAt":           repo.UpdatedAt.Format(time.RFC3339),
		"pushedAt":            store.NullableTimestamp(repo.PushedAt),
		"archivedAt":          nullableTimePtr(repo.ArchivedAt),
	}
}

func graphQLInputBool(input map[string]interface{}, key string) (bool, bool) {
	v, ok := input[key]
	if !ok || v == nil {
		return false, false
	}
	switch b := v.(type) {
	case bool:
		return b, true
	case *bool:
		if b == nil {
			return false, false
		}
		return *b, true
	default:
		// Keep this helper total: a direct resolver test or future decoder must
		// not turn an invalid optional value into a panic.
		return false, false
	}
}

// repoOwnerGraphQLLocked returns the concrete User or Organization source for
// the RepositoryOwner interface. Callers already hold st.mu.
func repoOwnerGraphQLLocked(repo *store.Repo, st *store.Store) map[string]interface{} {
	if repo == nil {
		return nil
	}
	ownerLogin, _, _ := strings.Cut(repo.FullName, "/")
	org := st.OrgsByLogin[ownerLogin]
	if org != nil {
		return orgToGraphQL(org)
	}
	if repo.Owner != nil {
		return userToGraphQL(repo.Owner)
	}
	return nil
}

// releaseToGQL renders a stored Release as the GraphQL source map for the
// Release type. latestID is the id of the repo's latest published release
// (0 when none) so isLatest reflects the same derivation REST uses.
func releaseToGQL(rel *store.Release, latestID int, repoFullName string, immutable bool) map[string]interface{} {
	var publishedAt interface{}
	if rel.PublishedAt != nil {
		publishedAt = rel.PublishedAt.Format(time.RFC3339)
	}
	var name interface{}
	if rel.Name != "" {
		name = rel.Name
	}
	// updatedAt: no release edit times are stored, so use the publish time
	// when published, else creation time.
	updatedAt := rel.CreatedAt.Format(time.RFC3339)
	if rel.PublishedAt != nil {
		updatedAt = rel.PublishedAt.Format(time.RFC3339)
	}
	return map[string]interface{}{
		"nodeID":       rel.NodeID,
		"databaseId":   rel.ID,
		"name":         name,
		"tagName":      rel.TagName,
		"isDraft":      rel.Draft,
		"immutable":    immutable,
		"isLatest":     latestID != 0 && rel.ID == latestID,
		"isPrerelease": rel.Prerelease,
		"createdAt":    rel.CreatedAt.Format(time.RFC3339),
		"publishedAt":  publishedAt,
		"url":          externalURL("/" + repoFullName + "/releases/tag/" + rel.TagName),
		"description":  nilStr(rel.Body),
		// Raw fields the account-surface Release members resolve from
		// (author, descriptionHTML, repository, resourcePath, tag, tagCommit,
		// mentions, releaseAssets, updatedAt) — see addAccountActionsFields.
		"authorID":        rel.AuthorID,
		"repoID":          rel.RepoID,
		"repoFullName":    repoFullName,
		"body":            rel.Body,
		"targetCommitish": rel.TargetCommitish,
		"updatedAt":       updatedAt,
	}
}

func (s *Resolver) repoImmutableReleasesEnabled(repoID int) bool {
	repo := s.store.GetRepoByID(repoID)
	if repo == nil {
		return false
	}
	enabled, _ := s.store.RepoImmutableReleasesState(repo)
	return enabled
}

// repoHasNoCommits reports whether the repo's git storage lacks a resolvable
// HEAD commit — GitHub's "empty repository" condition.
func (s *Resolver) repoHasNoCommits(owner, name string) bool {
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return true
	}
	headRef, err := stor.Reference(plumbing.HEAD)
	if err != nil {
		return true
	}
	if headRef.Type() == plumbing.SymbolicReference {
		targetRef, err := stor.Reference(headRef.Target())
		if err != nil {
			return true
		}
		return targetRef.Hash().IsZero()
	}
	return headRef.Hash().IsZero()
}

func encodeCursor(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", idx)))
}

// encodeConnectionCursor encodes a cursor carrying both position and stable
// identity ("cursor:<idx>:<id>"), so a page boundary does not shift when items
// are inserted before it. With no identity it degrades to the index cursor.
func encodeConnectionCursor(idx int, id string) string {
	if id == "" {
		return encodeCursor(idx)
	}
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d:%s", idx, id)))
}

// connectionCursorID returns the node identity embedded in a connection cursor,
// or "" when the cursor carries only an index (legacy / REST cursors).
func connectionCursorID(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(string(b), "cursor:")
	if !ok {
		return ""
	}
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		return rest[colon+1:]
	}
	return ""
}

func decodeCursor(s string) int {
	n, err := decodeCursorStrict(s)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return n
}

func decodeCursorStrict(s string) (int, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	rest, ok := strings.CutPrefix(string(b), "cursor:")
	if !ok {
		return 0, fmt.Errorf("cursor prefix")
	}
	// A connection cursor is "cursor:<idx>:<id>"; only the leading index is the
	// numeric position, the rest is the node identity.
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		rest = rest[:colon]
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("cursor index")
	}
	return n, nil
}

// gqlRefType returns the shared Ref object type (memoized), the one type
// Repository.defaultBranchRef/ref/refs and PullRequest.baseRef all use.
// branchProtectionRule resolves from the source map's key when present, else
// null (an unprotected ref).
func (s *Resolver) gqlRefType() *graphql.Object {
	if s.graphqlTypes.ref != nil {
		return s.graphqlTypes.ref
	}
	branchProtectionRuleType := s.gqlBranchProtectionRuleType()
	s.graphqlTypes.ref = graphql.NewObject(graphql.ObjectConfig{
		Name:       "Ref",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.Fields{
			"name":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"prefix": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"branchProtectionRule": &graphql.Field{
				Type: branchProtectionRuleType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					ref, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("ref source: unexpected type %T", p.Source)
					}
					rule, ok := ref["branchProtectionRule"].(map[string]interface{})
					if !ok || rule == nil {
						return nil, nil
					}
					return rule, nil
				},
			},
		},
	})
	s.addGitRefFields(s.graphqlTypes.ref)
	return s.graphqlTypes.ref
}

// nullableTimePtr renders an optional timestamp as RFC3339 or JSON null.
func nullableTimePtr(t *time.Time) interface{} {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
