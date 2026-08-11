package bleephub

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/graphql-go/graphql"
)

// addRepoFieldsToSchema adds repository types, queries, and mutations to the schema.
// Called from initGraphQLSchema after userType and queryType are created.
func (s *Server) addRepoFieldsToSchema(
	userType, queryType *graphql.Object,
	nodeInterface *graphql.Interface,
) (*graphql.Object, *graphql.Object, *graphql.Field, *graphql.Field) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	gitSSHRemote := s.graphQLStringScalar("GitSSHRemote")
	repositoryVisibilityEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "RepositoryVisibility",
		Values: graphql.EnumValueConfigMap{
			"PUBLIC":   &graphql.EnumValueConfig{Value: "PUBLIC"},
			"PRIVATE":  &graphql.EnumValueConfig{Value: "PRIVATE"},
			"INTERNAL": &graphql.EnumValueConfig{Value: "INTERNAL"},
		},
	})
	refType := s.gqlRefType()

	repoType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Repository",
		Interfaces: []*graphql.Interface{nodeInterface},
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
			"defaultBranchRef": &graphql.Field{
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
					return map[string]interface{}{
						"name":   branch,
						"prefix": "refs/heads/",
					}, nil
				},
			},
		},
	})

	// --- Repository fields gh CLI selects (clone/create/view --json) ---
	// gh's `GitHubRepo` query (repo clone, pr create) selects hasWikiEnabled
	// and parent{...repo}; `gh repo view --json` exposes the wider static set
	// below. Fields backed by repository settings or implemented repository
	// features resolve from the same store state as the REST repository shape.

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
			return repoToGraphQL(s.store, s.store.snapRepo(parent)), nil
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
			return repoToGraphQL(s.store, s.store.snapRepo(templateRepo)), nil
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
		// Real GitHub: watchers: UserConnection! — the same shared connection
		// type the assignee surfaces use, backed by the subscription store.
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
	repoType.AddFieldConfig("licenseInfo", &graphql.Field{
		// Real GitHub: licenseInfo: License — the same full License type
		// Query.license serves, resolved from the vendored license catalog.
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
			// A stored license key outside the vendored catalog still resolves
			// (License's non-null contract needs body/id/etc.) from the repo's
			// recorded metadata.
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
	languageType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Language",
		Fields: graphql.Fields{
			"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	repoType.AddFieldConfig("primaryLanguage", &graphql.Field{
		// Backed by Repo.Language (settable via the REST repo surface);
		// null when unset, exactly like a language-less repo on GitHub.
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
						"size": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
						"node": &graphql.Field{Type: graphql.NewNonNull(languageType)},
					},
				}))},
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
			counts := s.store.computeRepoLanguages(repo)
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
					"size": p.size,
					"node": map[string]interface{}{"name": p.lang},
				})
			}
			return map[string]interface{}{"edges": edges, "totalCount": len(pairs)}, nil
		},
	})
	repoType.AddFieldConfig("repositoryTopics", &graphql.Field{
		// Backed by Repo.Topics (REST PUT /repos/{o}/{r}/topics).
		Type: graphql.NewNonNull(graphql.NewObject(graphql.ObjectConfig{
			Name: "RepositoryTopicConnection",
			Fields: graphql.Fields{
				"nodes": &graphql.Field{Type: graphql.NewList(graphql.NewObject(graphql.ObjectConfig{
					Name: "RepositoryTopic",
					Fields: graphql.Fields{
						"topic": &graphql.Field{Type: graphql.NewNonNull(graphql.NewObject(graphql.ObjectConfig{
							Name: "Topic",
							Fields: graphql.Fields{
								"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
							},
						}))},
					},
				}))},
				"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
				"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			},
		})),
		Args: graphql.FieldConfigArgument{
			"first": &graphql.ArgumentConfig{Type: graphql.Int},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			topics, _ := r["topics"].([]string)
			nodes := make([]interface{}, 0, len(topics))
			for _, tp := range topics {
				nodes = append(nodes, map[string]interface{}{
					"topic": map[string]interface{}{"name": tp},
				})
			}
			return map[string]interface{}{
				"nodes": nodes, "totalCount": len(nodes),
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
		// Real value: true until the repo's git storage has a resolvable
		// HEAD commit (matches GitHub's "repository is empty" semantics).
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

	// Enums that real GitHub exposes — gh CLI sends these by name (CREATED_AT, DESC,
	// PUBLIC, OWNER, ...) not as strings. The schema must declare them so gh's
	// `gh repo list`, `gh issue list`, etc. type-check.
	repositoryPrivacyEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "RepositoryPrivacy",
		Values: graphql.EnumValueConfigMap{
			"PUBLIC":  &graphql.EnumValueConfig{Value: "PUBLIC"},
			"PRIVATE": &graphql.EnumValueConfig{Value: "PRIVATE"},
		},
	})
	repositoryAffiliationEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "RepositoryAffiliation",
		Values: graphql.EnumValueConfigMap{
			"OWNER":               &graphql.EnumValueConfig{Value: "OWNER"},
			"COLLABORATOR":        &graphql.EnumValueConfig{Value: "COLLABORATOR"},
			"ORGANIZATION_MEMBER": &graphql.EnumValueConfig{Value: "ORGANIZATION_MEMBER"},
		},
	})
	repositoryOrderFieldEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "RepositoryOrderField",
		Values: graphql.EnumValueConfigMap{
			"CREATED_AT": &graphql.EnumValueConfig{Value: "CREATED_AT"},
			"UPDATED_AT": &graphql.EnumValueConfig{Value: "UPDATED_AT"},
			"PUSHED_AT":  &graphql.EnumValueConfig{Value: "PUSHED_AT"},
			"STARGAZERS": &graphql.EnumValueConfig{Value: "STARGAZERS"},
			"NAME":       &graphql.EnumValueConfig{Value: "NAME"},
		},
	})
	orderDirectionEnum := s.graphQLEnum("OrderDirection", "ASC", "DESC")
	repositoryOrderInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RepositoryOrder",
		Fields: graphql.InputObjectConfigFieldMap{
			"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(repositoryOrderFieldEnum)},
			"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(orderDirectionEnum)},
		},
	})

	// --- Releases (gh release list / view / download / delete) ---
	// `gh release list` queries releases(first:$perPage, orderBy:{field:
	// CREATED_AT, direction:$direction}, after:$endCursor) with $direction
	// typed OrderDirection — the enum above must keep that exact name.
	// `gh release view/download/delete` additionally resolve draft releases
	// via release(tagName:){databaseId,isDraft}. Both are backed by the real
	// release store. The immutable field is derived from the repository and
	// organization immutable-release settings that the REST surface persists.
	releaseType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Release",
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

	releaseConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReleaseConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(releaseType)},
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

			return paginateGQL(releases, first, after, func(rel *Release) map[string]interface{} {
				return releaseToGQL(rel, latestID, repoFullName, immutable)
			}, func(rel *Release) string { return rel.NodeID }), nil
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
				// Real GitHub resolves a missing release(tagName:) to plain
				// null — gh's draft-release lookup keys on the null, not on
				// a NOT_FOUND error.
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

	// RepositoryOwner declares these fields directly. Keeping one field
	// definition for the interface and both implementors prevents the subtle
	// argument/signature drift that breaks gh and Octokit introspection.
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
			var repos []*Repo
			if user != nil {
				repos = s.store.ListReposForAuthUser(user, RepoListOptions{
					Affiliation: strings.Join(ownerAffiliations, ","),
					NoPaginate:  true,
				})
			} else {
				repos = s.store.ListReposForOrg(org.Login, RepoListOptions{NoPaginate: true})
			}

			// Drop what the viewer cannot see, before any other filter.
			repos = s.visibleRepos(p.Context, repos)
			if affiliations := graphQLRepositoryAffiliations(p.Args["affiliations"]); len(affiliations) != 0 {
				viewer := ghUserFromContext(p.Context)
				if viewer == nil {
					repos = nil
				} else {
					visibleByAffiliation := s.store.ListReposForAuthUser(viewer, RepoListOptions{
						Affiliation: strings.Join(affiliations, ","),
						NoPaginate:  true,
					})
					allowed := make(map[int]bool, len(visibleByAffiliation))
					for _, repo := range visibleByAffiliation {
						allowed[repo.ID] = true
					}
					repos = filterRepos(repos, func(repo *Repo) bool { return allowed[repo.ID] })
				}
			}

			// Filter by privacy
			if _, hasPrivacy := p.Args["privacy"]; hasPrivacy {
				if _, hasVisibility := p.Args["visibility"]; hasVisibility {
					return nil, fmt.Errorf("privacy and visibility cannot be combined")
				}
			}
			if privacy, ok := p.Args["privacy"].(string); ok {
				var filtered []*Repo
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

			// Filter by isFork
			if isFork, ok := p.Args["isFork"].(bool); ok {
				var filtered []*Repo
				for _, r := range repos {
					if r.Fork == isFork {
						filtered = append(filtered, r)
					}
				}
				repos = filtered
			}
			if archived, ok := p.Args["isArchived"].(bool); ok {
				repos = filterRepos(repos, func(repo *Repo) bool { return repo.Archived == archived })
			}
			if hasIssues, ok := p.Args["hasIssuesEnabled"].(bool); ok {
				repos = filterRepos(repos, func(repo *Repo) bool { return repo.HasIssues == hasIssues })
			}
			if locked, ok := p.Args["isLocked"].(bool); ok && locked {
				// Bleephub does not currently create locked repositories.
				repos = nil
			}
			if visibility, ok := p.Args["visibility"].(string); ok {
				repos = filterRepos(repos, func(repo *Repo) bool {
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
			return repoToGraphQL(s.store, s.store.snapRepo(repo)), nil
		},
	}
	userType.AddFieldConfig("repository", ownerRepositoryField)
	s.graphqlTypes.repositoryOwner.AddFieldConfig("repository", ownerRepositoryField)

	// Add repository query to queryType
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
			// missing one: real GitHub returns null data + a NOT_FOUND error
			// rather than leaking the repo's existence or contents. This
			// mirrors the REST handler's read gate.
			if repo == nil || (repo.Private && !s.viewerCanReadRepo(p.Context, repo)) {
				// Real GitHub pairs the null with a typed NOT_FOUND error —
				// gh CLI keys on errors[].type to report "repository not
				// found" instead of decoding an empty object.
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name),
				}
			}
			return repoToGraphQL(s.store, s.store.snapRepo(repo)), nil
		},
	})

	// `repositoryOwner(login)` is the interface real GitHub exposes for "user or
	// organization that owns repos". gh CLI's `gh repo list <login>` queries it.
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
			s.store.mu.RLock()
			org := s.store.OrgsByLogin[login]
			s.store.mu.RUnlock()
			if org != nil {
				return orgToGraphQL(org), nil
			}
			return nil, &ghNotFoundError{
				message: fmt.Sprintf("Could not resolve to a RepositoryOwner with the login of '%s'.", login),
			}
		},
	})

	// Build mutation type
	createRepoInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateRepositoryInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"name":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"ownerId":          &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"visibility":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(repositoryVisibilityEnum), DefaultValue: "PUBLIC"},
			"description":      &graphql.InputObjectFieldConfig{Type: graphql.String},
			"hasIssuesEnabled": &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"hasWikiEnabled":   &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
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
			user := ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			name, _ := input["name"].(string)
			description, _ := input["description"].(string)
			visibility, _ := input["visibility"].(string)

			private := strings.ToUpper(visibility) == "PRIVATE"
			kind, ownerLogin, err := s.createRepositoryOwner(p, input)
			if err != nil {
				return nil, err
			}
			var repo *Repo
			if kind == organizationAccount {
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
			if !s.store.UpdateRepo(ownerLogin, name, func(r *Repo) {
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
				"repository": repoToGraphQL(s.store, s.store.snapRepo(repo)),
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

			found := findRepoByNodeID(s.store, repoID)
			if found == nil {
				return nil, fmt.Errorf("could not resolve to a Repository with the global id of '%s'", repoID)
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

func filterRepos(repos []*Repo, keep func(*Repo) bool) []*Repo {
	filtered := make([]*Repo, 0, len(repos))
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
// name graphqlMutationAuthz does not cover. Authorization therefore lives in
// one table rather than in twenty-odd hand-written checks, and a mutation added
// without deciding who may call it fails at schema build instead of shipping
// open to any signed-in account.

// mutationRule decides whether the credential behind a request may perform one
// mutation on whatever its input names. There is an implementation per resource
// class rather than one universal shape: a repository and a project have
// different owners and different questions to ask, and bending the repository
// lookups over a project would answer the wrong one.
type mutationRule interface {
	// check reports a malformed policy row. It runs once, while the schema is
	// being assembled, so a row missing its lookup is a build failure rather
	// than a mutation that quietly authorizes nothing.
	check() error
	authorize(s *Server, p graphql.ResolveParams, input map[string]interface{}) error
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
	repo     *Repo
	authorID int
	// missing answers both "no such node" and "you may not reach it". They have
	// to be the same answer, or a mutation becomes an existence oracle for
	// private repositories.
	missing error
}

// repoCreationRule is the policy for createRepository, the one mutation that
// names no existing repository: the entitlement is over the account the
// repository would belong to.
//
// An authenticated viewer used to be the whole test, on the reasoning that the
// resolver decided the owner for itself. It did — but it decided it from the
// bearer, so a user-to-server token of an app installed nowhere created
// repositories on its bearer's account where the same app's installation token
// was refused.
type repoCreationRule struct{}

func (repoCreationRule) check() error { return nil }

func (repoCreationRule) authorize(s *Server, p graphql.ResolveParams, input map[string]interface{}) error {
	kind, login, err := s.createRepositoryOwner(p, input)
	if err != nil {
		return err
	}
	if !s.credentialGrantsAccount(p.Context, kind, login, scopeAdministration, permWrite) {
		return fmt.Errorf("resource not accessible by integration")
	}
	if kind == organizationAccount && !s.viewerIsOrgMember(p.Context, login) {
		return fmt.Errorf("repository creation for another owner is not authorized")
	}
	return nil
}

// createRepositoryOwner resolves the account a createRepository input names. The
// policy row and the resolver both call it, so the owner the entitlement is
// checked against is by construction the owner the repository is created under.
func (s *Server) createRepositoryOwner(p graphql.ResolveParams, input map[string]interface{}) (accountKind, string, error) {
	user := ghUserFromContext(p.Context)
	ownerID, _ := input["ownerId"].(string)
	if ownerID == "" || ownerID == user.NodeID {
		return anyAccount, user.Login, nil
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	for _, candidate := range s.store.Orgs {
		if candidate.NodeID == ownerID {
			return organizationAccount, candidate.Login, nil
		}
	}
	return anyAccount, "", fmt.Errorf("repository creation for another owner is not authorized")
}

// repoRule is the policy for a mutation whose subject belongs to a repository.
type repoRule struct {
	// scope is the fine-grained permission an app must have been granted to
	// perform this mutation at all, and it is per-row because GitHub grants
	// issue triage at issues:write and pull-request triage at
	// pull_requests:write. One scope for the whole table would refuse apps
	// GitHub allows.
	scope permScope
	level mutationLevel
	// authorMayAct admits the author of the targeted content whatever their
	// repository access: editing your own issue or hiding your own comment
	// never required push.
	authorMayAct bool
	target       func(s *Server, input map[string]interface{}) mutationTarget
}

func (r repoRule) check() error {
	if r.target == nil {
		return fmt.Errorf("no repository target lookup")
	}
	if r.scope == "" {
		return fmt.Errorf("no permission scope")
	}
	return nil
}

func (r repoRule) authorize(s *Server, p graphql.ResolveParams, input map[string]interface{}) error {
	target := r.target(s, input)
	if target.repo == nil || !s.viewerCanReadRepo(p.Context, target.repo) {
		return target.missing
	}
	// The credential half is asked first and is never relaxed by authorship.
	// Every mutation here is a write on its scope, whatever standing on the
	// repository it then needs, so an app that was not granted that scope may
	// not perform it — the author exemption speaks to who the bearer is, and an
	// app's grant is not a fact about the bearer. Ordering these the other way
	// round let a bearer who had merely filed the issue retitle it through an
	// app installed nowhere.
	if !s.credentialGrantsRepo(p.Context, target.repo, r.scope, permWrite) {
		return fmt.Errorf("resource not accessible by integration")
	}
	user := ghUserFromContext(p.Context)
	if r.authorMayAct && target.authorID != 0 && target.authorID == user.ID {
		return nil
	}
	switch r.level {
	case mutationPushRepo:
		if !s.principalHoldsRepoCapability(p.Context, target.repo, permWrite) {
			return fmt.Errorf("must have push access to Repository")
		}
	case mutationAdminRepo:
		if !s.principalHoldsRepoCapability(p.Context, target.repo, permAdmin) {
			return fmt.Errorf("must have admin rights to Repository")
		}
	}
	return nil
}

// graphqlMutationAuthz is the whole authorization policy of the mutation
// surface.
//
// Each row names two independent things. The scope is the permission an app
// must hold to perform the mutation at all, and it follows GitHub's grouping —
// issues, pull requests, discussions and repository administration are separate
// grants, and an app given one of them has not been given the others. The level
// is the standing the bearer needs on the repository, which follows the same
// reasoning resourceCapabilityFor applies to the REST routes: opening an issue,
// commenting, proposing a pull request and reviewing one are how outside
// contributors participate and need only read, while editing, closing, merging,
// moderating and deleting need push, and destroying a repository or somebody
// else's discussion needs admin.
var graphqlMutationAuthz = map[string]mutationRule{
	"createRepository": repoCreationRule{},
	"deleteRepository": repoRule{scope: scopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},

	// Comments on a pull request are stored and served as issue comments, and
	// GitHub gates the issue-comment endpoints on Issues however the subject was
	// opened, so addComment and the comment moderation mutations are scopeIssues
	// even where the subject resolves to a pull request.
	"createIssue": repoRule{scope: scopeIssues, level: mutationReadRepo, target: mutationTargetRepo("repositoryId")},
	"addComment":  repoRule{scope: scopeIssues, level: mutationReadRepo, target: mutationTargetIssueOrPullRequest("subjectId")},
	"closeIssue":  repoRule{scope: scopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssue("issueId")},
	"reopenIssue": repoRule{scope: scopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssue("issueId")},
	"updateIssue": repoRule{scope: scopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssue("id")},

	"createDiscussion":                repoRule{scope: scopeDiscussions, level: mutationReadRepo, target: mutationTargetRepo("repositoryId")},
	"addDiscussionComment":            repoRule{scope: scopeDiscussions, level: mutationReadRepo, target: mutationTargetDiscussion("discussionId")},
	"updateDiscussion":                repoRule{scope: scopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussion("discussionId")},
	"deleteDiscussion":                repoRule{scope: scopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussion("id")},
	"updateDiscussionComment":         repoRule{scope: scopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussionComment("commentId")},
	"deleteDiscussionComment":         repoRule{scope: scopeDiscussions, level: mutationAdminRepo, authorMayAct: true, target: mutationTargetDiscussionComment("id")},
	"markDiscussionCommentAsAnswer":   repoRule{scope: scopeDiscussions, level: mutationPushRepo, authorMayAct: true, target: mutationTargetAnsweredDiscussion("id")},
	"unmarkDiscussionCommentAsAnswer": repoRule{scope: scopeDiscussions, level: mutationPushRepo, authorMayAct: true, target: mutationTargetAnsweredDiscussion("id")},

	"minimizeComment":   repoRule{scope: scopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssueComment("subjectId")},
	"unminimizeComment": repoRule{scope: scopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssueComment("subjectId")},
	"lockLockable":      repoRule{scope: scopeIssues, level: mutationPushRepo, target: mutationTargetIssueOrPullRequest("lockableId")},
	"unlockLockable":    repoRule{scope: scopeIssues, level: mutationPushRepo, target: mutationTargetIssueOrPullRequest("lockableId")},

	"createPullRequest":             repoRule{scope: scopePullRequests, level: mutationReadRepo, target: mutationTargetRepo("repositoryId")},
	"addPullRequestReview":          repoRule{scope: scopePullRequests, level: mutationReadRepo, target: mutationTargetPullRequest("pullRequestId")},
	"closePullRequest":              repoRule{scope: scopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"reopenPullRequest":             repoRule{scope: scopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"updatePullRequest":             repoRule{scope: scopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"markPullRequestReadyForReview": repoRule{scope: scopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"convertPullRequestToDraft":     repoRule{scope: scopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
	"mergePullRequest":              repoRule{scope: scopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
	"resolveReviewThread":           repoRule{scope: scopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReviewThread("threadId")},
	"unresolveReviewThread":         repoRule{scope: scopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReviewThread("threadId")},

	// Projects v2. A project belongs to a user or an organization, not to a
	// repository, so write is the owner-scoped predicate the REST surface uses:
	// the owning user themself, or an active member of the owning org.
	"createProjectV2":               projectRule{target: projectTargetOwner("ownerId")},
	"addProjectV2ItemById":          projectRule{target: projectTargetProject("projectId")},
	"createProjectV2Field":          projectRule{target: projectTargetProject("projectId")},
	"updateProjectV2ItemFieldValue": projectRule{target: projectTargetProject("projectId")},
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
func (s *Server) registerMutation(mutationType *graphql.Object, name string, field *graphql.Field) {
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
		if ghUserFromContext(p.Context) == nil {
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

// assertMutationsAuthorized fails schema construction unless every field on the
// mutation type went through registerMutation and carries a policy row.
//
// The registrar prevents the omission and this catches it, and they fail at
// different moments: a family that adds a mutation without a row cannot start,
// and a family that bypasses the registrar entirely — a new file, a merge that
// resurrects an AddFieldConfig call — cannot either. Coverage stops depending
// on each author remembering the convention.
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

// gqlMissingNode is the answer a mutation gives for a node it cannot reach,
// shaped like the NOT_FOUND errors[] entry real GitHub returns so gh CLI
// reports "not found" instead of decoding an empty payload.
func gqlMissingNode(typeName, nodeID string) error {
	return &ghNotFoundError{
		message: fmt.Sprintf("Could not resolve to a %s with the global id of '%s'.", typeName, nodeID),
	}
}

func mutationTargetRepo(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		return mutationTarget{
			repo:    findRepoByNodeID(s.store, nodeID),
			missing: gqlMissingNode("Repository", nodeID),
		}
	}
}

func mutationTargetIssue(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Issue", nodeID)}
		if issue := findIssueByNodeID(s.store, nodeID); issue != nil {
			target.repo = s.store.GetRepoByID(issue.RepoID)
			target.authorID = issue.AuthorID
		}
		return target
	}
}

func mutationTargetPullRequest(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("PullRequest", nodeID)}
		if pr := findPullRequestByNodeID(s.store, nodeID); pr != nil {
			target.repo = s.store.GetRepoByID(pr.RepoID)
			target.authorID = pr.AuthorID
		}
		return target
	}
}

// mutationTargetIssueOrPullRequest covers the mutations whose subject is either
// — GitHub stores pull requests as issues, so addComment and the lock
// mutations take both node kinds.
func mutationTargetIssueOrPullRequest(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("node", nodeID)}
		if issue := findIssueByNodeID(s.store, nodeID); issue != nil {
			target.repo = s.store.GetRepoByID(issue.RepoID)
			target.authorID = issue.AuthorID
			return target
		}
		if pr := findPullRequestByNodeID(s.store, nodeID); pr != nil {
			target.repo = s.store.GetRepoByID(pr.RepoID)
			target.authorID = pr.AuthorID
		}
		return target
	}
}

func mutationTargetIssueComment(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
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

func mutationTargetDiscussion(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Discussion", nodeID)}
		if d := findDiscussionByNodeID(s.store, nodeID); d != nil {
			target.repo = s.store.GetRepoByID(d.RepoID)
			target.authorID = d.AuthorID
		}
		return target
	}
}

func mutationTargetDiscussionComment(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("DiscussionComment", nodeID)}
		c := findDiscussionCommentByNodeID(s.store, nodeID)
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
func mutationTargetAnsweredDiscussion(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("DiscussionComment", nodeID)}
		c := findDiscussionCommentByNodeID(s.store, nodeID)
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

func mutationTargetReviewThread(key string) func(*Server, map[string]interface{}) mutationTarget {
	return func(s *Server, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("PullRequestReviewThread", nodeID)}
		threadID, ok := parsePRReviewThreadNodeID(nodeID)
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

// repoToGraphQL converts a Repo to a map for GraphQL resolvers. It reads the
// repo's mutable fields (description, topics, timestamps) directly, so the
// caller must pass either a private snapshot (st.snapRepo) or a repo it holds
// the store lock over — never the live shared pointer off-lock, which would
// race a concurrent UpdateRepo. Under-lock callers (the *Locked GraphQL paths)
// pass the live pointer; off-lock resolvers pass a snapshot.
func repoToGraphQL(st *Store, repo *Repo) map[string]interface{} {
	return repoToGraphQLWithOrg(repo, st.GetOrgByID)
}

// repoToGraphQLLocked is repoToGraphQL for callers that already hold st.mu.
func repoToGraphQLLocked(st *Store, repo *Repo) map[string]interface{} {
	return repoToGraphQLWithOrg(repo, func(id int) *Org { return st.Orgs[id] })
}

func repoToGraphQLWithOrg(repo *Repo, getOrg func(int) *Org) map[string]interface{} {
	var ownerMap map[string]interface{}
	if repo.OwnerType == "Organization" {
		if org := getOrg(repo.OwnerID); org != nil {
			ownerMap = orgToGraphQL(org)
		}
	} else if repo.Owner != nil {
		ownerMap = userToGraphQL(repo.Owner)
	}
	webURL := externalURL("/" + repo.FullName)

	return map[string]interface{}{
		"nodeID":              repo.NodeID,
		"databaseId":          repo.ID,
		"name":                repo.Name,
		"nameWithOwner":       repo.FullName,
		"description":         repo.Description,
		"url":                 webURL,
		"sshUrl":              sshGitURL(repo.FullName),
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
		"hasDiscussions":      repoHasDiscussions(repo),
		"parentID":            repo.ParentID,
		"templateRepoID":      repo.TemplateRepoID,
		"allowSquashMerge":    repo.AllowSquashMerge,
		"allowMergeCommit":    repo.AllowMergeCommit,
		"allowRebaseMerge":    repo.AllowRebaseMerge,
		"deleteBranchOnMerge": repo.DeleteBranchOnMerge,
		"isTemplate":          repo.IsTemplate,
		"owner":               ownerMap,
		"createdAt":           repo.CreatedAt.Format(time.RFC3339),
		"updatedAt":           repo.UpdatedAt.Format(time.RFC3339),
		"pushedAt":            nullableTimestamp(repo.PushedAt),
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
		// graphql-go validates public requests before resolver execution. Keep
		// this store-facing helper total as well: a direct resolver test or a
		// future decoder must not turn an invalid optional value into a
		// process panic.
		return false, false
	}
}

// repoOwnerGraphQLLocked returns the concrete User or Organization source for
// the RepositoryOwner interface. Callers already hold st.mu.
func repoOwnerGraphQLLocked(repo *Repo, st *Store) map[string]interface{} {
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

// repoOwnerREST returns a simple-user-shaped map for the owner of repo,
// using snake_case keys. For org-owned repos it resolves the organization
// from the repo's full name rather than the creating user.
func repoOwnerREST(repo *Repo, st *Store, baseURL string) map[string]interface{} {
	if repo == nil {
		return nil
	}
	ownerLogin, _, _ := strings.Cut(repo.FullName, "/")
	st.mu.RLock()
	org := st.OrgsByLogin[ownerLogin]
	st.mu.RUnlock()
	if org != nil {
		api := baseURL + "/api/v3/orgs/" + org.Login
		return map[string]interface{}{
			"login":               org.Login,
			"id":                  org.ID,
			"node_id":             org.NodeID,
			"avatar_url":          org.AvatarURL,
			"gravatar_id":         "",
			"url":                 api,
			"html_url":            baseURL + "/" + org.Login,
			"followers_url":       api + "/followers",
			"following_url":       api + "/following{/other_user}",
			"gists_url":           api + "/gists{/gist_id}",
			"starred_url":         api + "/starred{/owner}{/repo}",
			"subscriptions_url":   api + "/subscriptions",
			"organizations_url":   api + "/orgs",
			"repos_url":           api + "/repos",
			"events_url":          api + "/events{/privacy}",
			"received_events_url": api + "/received_events",
			"type":                org.Type,
			"site_admin":          false,
			"name":                org.Name,
			"email":               org.Email,
			"user_view_type":      "public",
		}
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if repo.Owner != nil {
		return userToJSON(repo.Owner)
	}
	return nil
}

// releaseToGQL renders a stored Release as the GraphQL source map for the
// Release type. latestID is the id of the repo's latest published release
// (0 when none) so isLatest reflects the same derivation REST uses.
func releaseToGQL(rel *Release, latestID int, repoFullName string, immutable bool) map[string]interface{} {
	var publishedAt interface{}
	if rel.PublishedAt != nil {
		publishedAt = rel.PublishedAt.Format(time.RFC3339)
	}
	var name interface{}
	if rel.Name != "" {
		name = rel.Name
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
	}
}

func (s *Server) repoImmutableReleasesEnabled(repoID int) bool {
	repo := s.store.GetRepoByID(repoID)
	if repo == nil {
		return false
	}
	enabled, _ := s.store.RepoImmutableReleasesState(repo)
	return enabled
}

// repoHasNoCommits reports whether the repo's git storage lacks a resolvable
// HEAD commit — GitHub's "empty repository" condition.
func (s *Server) repoHasNoCommits(owner, name string) bool {
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

// encodeConnectionCursor encodes a GraphQL connection cursor that carries both
// the node's position and its stable identity ("cursor:<idx>:<id>"), so a page
// boundary can be re-resolved by identity and does not shift when items are
// inserted before it. With no identity it degrades to the plain index cursor.
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

// gqlRefType returns the shared Ref object type (memoized). Used by
// Repository.defaultBranchRef and PullRequest.baseRef — matching GitHub, where
// both fields are the one Ref type. branchProtectionRule resolves from the
// "branchProtectionRule" key of the source map when the producer embeds one
// (the PR baseRef path does); sources without the key resolve null, the value
// real GitHub returns for an unprotected ref.
func (s *Server) gqlRefType() *graphql.Object {
	if s.graphqlTypes.ref != nil {
		return s.graphqlTypes.ref
	}
	branchProtectionRuleType := graphql.NewObject(graphql.ObjectConfig{
		Name: "BranchProtectionRule",
		Fields: graphql.Fields{
			"requiresStrictStatusChecks":   &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"requiredApprovingReviewCount": &graphql.Field{Type: graphql.Int},
		},
	})
	s.graphqlTypes.ref = graphql.NewObject(graphql.ObjectConfig{
		Name: "Ref",
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
	return s.graphqlTypes.ref
}
