package graphqlapi

// Projects classic (v1) GraphQL read surface: the Project / ProjectColumn /
// ProjectCard family, their connections and enums, and the ProjectOwner
// interface (User, Organization, Repository). Alongside the REST surface's
// repo-scoped boards, this adds account-owned boards, modeled with
// OwnerType/OwnerLogin on the same record.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// ---------------------------------------------------------------------------
// Enums

func (s *Resolver) projectStateEnum() *graphql.Enum {
	return s.graphQLEnum("ProjectState", "CLOSED", "OPEN")
}

func (s *Resolver) projectTemplateEnum() *graphql.Enum {
	return s.graphQLEnum("ProjectTemplate",
		"AUTOMATED_KANBAN_V2", "AUTOMATED_REVIEWS_KANBAN", "BASIC_KANBAN", "BUG_TRIAGE")
}

func (s *Resolver) projectCardArchivedStateEnum() *graphql.Enum {
	return s.graphQLEnum("ProjectCardArchivedState", "ARCHIVED", "NOT_ARCHIVED")
}

func (s *Resolver) projectCardStateEnum() *graphql.Enum {
	return s.graphQLEnum("ProjectCardState", "CONTENT_ONLY", "NOTE_ONLY", "REDACTED")
}

func (s *Resolver) projectColumnPurposeEnum() *graphql.Enum {
	return s.graphQLEnum("ProjectColumnPurpose", "DONE", "IN_PROGRESS", "TODO")
}

// ---------------------------------------------------------------------------
// Access predicates
//
// A repo-scoped board uses its repository's visibility and Projects permission,
// matching the REST handlers so neither surface routes around the other. An
// account-owned board is readable when public or by an account member, and
// writable by the owning user or the org's members.

func (s *Resolver) canReadProjectClassic(ctx context.Context, p *store.ProjectClassic) bool {
	if p == nil {
		return false
	}
	if p.RepoKey != "" {
		repo := s.store.GetRepoByFullName(p.RepoKey)
		return repo != nil && s.viewerCanReadRepo(ctx, repo)
	}
	if p.Public {
		return true
	}
	return s.viewerBelongsToProjectClassicOwner(ctx, p)
}

func (s *Resolver) canWriteProjectClassic(ctx context.Context, p *store.ProjectClassic) bool {
	if p == nil {
		return false
	}
	if p.RepoKey != "" {
		repo := s.store.GetRepoByFullName(p.RepoKey)
		return repo != nil && s.viewerHasRepoPermission(ctx, repo, store.ScopeProjects, store.PermWrite)
	}
	return s.viewerBelongsToProjectClassicOwner(ctx, p)
}

func (s *Resolver) viewerBelongsToProjectClassicOwner(ctx context.Context, p *store.ProjectClassic) bool {
	viewer := s.ghUserFromContext(ctx)
	if viewer == nil {
		return false
	}
	if p.OwnerType == "Organization" {
		return s.viewerIsOrgMember(ctx, p.OwnerLogin) || s.viewerCanAdminAccount(ctx, p.OwnerLogin)
	}
	return viewer.Login == p.OwnerLogin
}

// ---------------------------------------------------------------------------
// Renderers

// projectClassicOwnerSource renders a project's ProjectOwner (Repository, Org or
// User source with __typename set) and the owner's web base path. A nil owner
// means the owning record no longer resolves.
func (s *Resolver) projectClassicOwnerSource(p *store.ProjectClassic) (map[string]interface{}, string) {
	switch {
	case p.RepoKey != "":
		repo := s.store.GetRepoByFullName(p.RepoKey)
		if repo == nil {
			return nil, ""
		}
		src := repoToGraphQL(s.store, s.store.SnapRepo(repo))
		src["__typename"] = "Repository"
		return src, "/" + p.RepoKey
	case p.OwnerType == "Organization":
		org := s.store.GetOrg(p.OwnerLogin)
		if org == nil {
			return nil, ""
		}
		src := orgToGraphQL(org)
		src["__typename"] = "Organization"
		return src, "/orgs/" + p.OwnerLogin
	default:
		user := s.store.LookupUserByLogin(p.OwnerLogin)
		if user == nil {
			return nil, ""
		}
		src := userToGraphQL(user)
		src["__typename"] = "User"
		return src, "/users/" + p.OwnerLogin
	}
}

func (s *Resolver) projectClassicToGQL(p *store.ProjectClassic) map[string]interface{} {
	if p == nil {
		return nil
	}
	owner, basePath := s.projectClassicOwnerSource(p)
	if owner == nil {
		return nil
	}
	resourcePath := basePath + "/projects/" + strconv.Itoa(p.Number)
	var closedAt interface{}
	if p.ClosedAt != nil {
		closedAt = p.ClosedAt.UTC().Format(time.RFC3339)
	}
	state := "OPEN"
	if p.State == "closed" {
		state = "CLOSED"
	}
	return map[string]interface{}{
		"__typename":   "Project",
		"nodeID":       p.NodeID,
		"databaseId":   p.ID,
		"name":         p.Name,
		"body":         p.Body,
		"closed":       p.State == "closed",
		"closedAt":     closedAt,
		"number":       p.Number,
		"state":        state,
		"createdAt":    p.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":    p.UpdatedAt.UTC().Format(time.RFC3339),
		"creator":      optionalRendered(s.store.GetUserByID(p.CreatorID), userToGraphQL),
		"owner":        owner,
		"resourcePath": resourcePath,
		"url":          externalURL(resourcePath),
	}
}

func projectClassicColumnToGQL(c *store.ProjectColumn) map[string]interface{} {
	if c == nil {
		return nil
	}
	resourcePath := "/projects/columns/" + strconv.Itoa(c.ID)
	return map[string]interface{}{
		"__typename":   "ProjectColumn",
		"nodeID":       c.NodeID,
		"databaseId":   c.ID,
		"name":         c.Name,
		"createdAt":    c.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":    c.UpdatedAt.UTC().Format(time.RFC3339),
		"resourcePath": resourcePath,
		"url":          externalURL(resourcePath),
		"_projectID":   c.ProjectID,
	}
}

func (s *Resolver) projectClassicCardToGQL(c *store.ProjectCard) map[string]interface{} {
	if c == nil {
		return nil
	}
	var note interface{}
	if c.Note != "" {
		note = c.Note
	}
	var state interface{}
	switch {
	case c.IssueID != 0:
		if s.store.GetIssue(c.IssueID) != nil {
			state = "CONTENT_ONLY"
		} else {
			state = "REDACTED"
		}
	case c.PullRequestID != 0:
		if s.store.GetPullRequest(c.PullRequestID) != nil {
			state = "CONTENT_ONLY"
		} else {
			state = "REDACTED"
		}
	case c.Note != "":
		state = "NOTE_ONLY"
	}
	resourcePath := "/projects/columns/cards/" + strconv.Itoa(c.ID)
	return map[string]interface{}{
		"__typename":   "ProjectCard",
		"nodeID":       c.NodeID,
		"databaseId":   c.ID,
		"note":         note,
		"state":        state,
		"isArchived":   c.Archived,
		"createdAt":    c.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":    c.UpdatedAt.UTC().Format(time.RFC3339),
		"creator":      optionalRendered(s.store.GetUserByID(c.CreatorID), userToGraphQL),
		"resourcePath": resourcePath,
		"url":          externalURL(resourcePath),
		"_columnID":    c.ColumnID,
		"_issueID":     c.IssueID,
		"_prID":        c.PullRequestID,
	}
}

// ---------------------------------------------------------------------------
// Object types

// projectClassicConnectionPair returns the memoized Edge and Connection objects
// for a classic-project node type. The edge is memoized separately because the
// add/move mutation payloads name it directly (cardEdge, columnEdge).
func (s *Resolver) projectClassicConnectionPair(name string, nodeType *graphql.Object) (*graphql.Object, *graphql.Object) {
	edge := s.mutationObject(name+"Edge", graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   gqlField(nodeType),
	})
	connection := s.mutationObject(name+"Connection", graphql.Fields{
		"edges":      &graphql.Field{Type: graphql.NewList(edge)},
		"nodes":      &graphql.Field{Type: graphql.NewList(nodeType)},
		"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
		"totalCount": gqlNonNull(graphql.Int),
	})
	return connection, edge
}

// projectClassicBool builds a viewerCan* resolver deciding from the live project
// the source names.
func (s *Resolver) projectClassicBool(decide func(ctx context.Context, p *store.ProjectClassic) bool) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		src, ok := p.Source.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
		}
		id, _ := src["databaseId"].(int)
		return decide(p.Context, s.store.GetProjectClassic(id)), nil
	}
}

// projectClassicType is the `Project` object (a classic board). Members go
// through a thunk because columns↔cards↔project form a cycle; the memo is
// recorded before the thunk runs so the cycle resolves to this instance.
func (s *Resolver) projectClassicType() *graphql.Object {
	if memo := s.memoizedMutationObject("Project"); memo != nil {
		return memo
	}
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	html := s.graphQLStringScalar("HTML")
	object := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Project",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			columnConnection, _ := s.projectClassicConnectionPair("ProjectColumn", s.projectClassicColumnType())
			cardConnection, _ := s.projectClassicConnectionPair("ProjectCard", s.projectClassicCardType())
			pendingCardsArgs := relayConnectionArgs()
			pendingCardsArgs["archivedStates"] = &graphql.ArgumentConfig{
				Type:         graphql.NewList(s.projectCardArchivedStateEnum()),
				DefaultValue: []interface{}{"ARCHIVED", "NOT_ARCHIVED"},
			}
			progressType := s.mutationObject("ProjectProgress", graphql.Fields{
				"doneCount":            gqlNonNull(graphql.Int),
				"donePercentage":       gqlNonNull(graphql.Float),
				"inProgressCount":      gqlNonNull(graphql.Int),
				"inProgressPercentage": gqlNonNull(graphql.Float),
				"todoCount":            gqlNonNull(graphql.Int),
				"todoPercentage":       gqlNonNull(graphql.Float),
				"enabled":              gqlNonNull(graphql.Boolean),
			})
			return graphql.Fields{
				"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("nodeID")},
				"databaseId": gqlField(graphql.Int),
				"name":       gqlNonNull(graphql.String),
				"body":       gqlField(graphql.String),
				"bodyHTML":   &graphql.Field{Type: graphql.NewNonNull(html), Resolve: sourceKeyResolver("body")},
				"closed":     gqlNonNull(graphql.Boolean),
				"closedAt":   gqlField(dateTime),
				"createdAt":  gqlNonNull(dateTime),
				"updatedAt":  gqlNonNull(dateTime),
				"creator":    gqlField(s.graphqlTypes.actor),
				"number":     gqlNonNull(graphql.Int),
				"state":      gqlNonNull(s.projectStateEnum()),
				"owner":      gqlNonNull(s.projectOwnerInterfaceType()),
				"viewerCanClose": &graphql.Field{
					Type: graphql.NewNonNull(graphql.Boolean),
					Resolve: s.projectClassicBool(func(ctx context.Context, p *store.ProjectClassic) bool {
						return p != nil && p.State != "closed" && s.canWriteProjectClassic(ctx, p)
					}),
				},
				"viewerCanReopen": &graphql.Field{
					Type: graphql.NewNonNull(graphql.Boolean),
					Resolve: s.projectClassicBool(func(ctx context.Context, p *store.ProjectClassic) bool {
						return p != nil && p.State == "closed" && s.canWriteProjectClassic(ctx, p)
					}),
				},
				"viewerCanUpdate": &graphql.Field{
					Type: graphql.NewNonNull(graphql.Boolean),
					Resolve: s.projectClassicBool(func(ctx context.Context, p *store.ProjectClassic) bool {
						return s.canWriteProjectClassic(ctx, p)
					}),
				},
				"resourcePath": gqlNonNull(uri),
				"url":          gqlNonNull(uri),
				"columns": &graphql.Field{
					Type: graphql.NewNonNull(columnConnection),
					Args: relayConnectionArgs(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						src, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
						}
						id, _ := src["databaseId"].(int)
						cols := s.store.ListProjectColumns(id)
						nodes := make([]map[string]interface{}, 0, len(cols))
						for _, c := range cols {
							nodes = append(nodes, projectClassicColumnToGQL(c))
						}
						return paginateGQLMaps(nodes, p.Args), nil
					},
				},
				// pendingCards are cards not yet in a column. bleephub models
				// only cards inside a column, so this is a truthful empty connection.
				"pendingCards": &graphql.Field{
					Type: graphql.NewNonNull(cardConnection),
					Args: pendingCardsArgs,
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return paginateGQLMaps(nil, p.Args), nil
					},
				},
				// progress reports card-purpose counts. bleephub's columns carry
				// no purpose, so it is disabled: enabled false, every count zero.
				"progress": &graphql.Field{
					Type: graphql.NewNonNull(progressType),
					Resolve: func(graphql.ResolveParams) (interface{}, error) {
						return map[string]interface{}{
							"doneCount":            0,
							"donePercentage":       0.0,
							"inProgressCount":      0,
							"inProgressPercentage": 0.0,
							"todoCount":            0,
							"todoPercentage":       0.0,
							"enabled":              false,
						}, nil
					},
				},
			}
		}),
	})
	s.mutationObjects["Project"] = object
	return object
}

func (s *Resolver) projectClassicColumnType() *graphql.Object {
	if memo := s.memoizedMutationObject("ProjectColumn"); memo != nil {
		return memo
	}
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	object := graphql.NewObject(graphql.ObjectConfig{
		Name:       "ProjectColumn",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			cardConnection, _ := s.projectClassicConnectionPair("ProjectCard", s.projectClassicCardType())
			cardsArgs := relayConnectionArgs()
			cardsArgs["archivedStates"] = &graphql.ArgumentConfig{
				Type:         graphql.NewList(s.projectCardArchivedStateEnum()),
				DefaultValue: []interface{}{"ARCHIVED", "NOT_ARCHIVED"},
			}
			return graphql.Fields{
				"id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("nodeID")},
				"databaseId":   gqlField(graphql.Int),
				"name":         gqlNonNull(graphql.String),
				"createdAt":    gqlNonNull(dateTime),
				"updatedAt":    gqlNonNull(dateTime),
				"purpose":      gqlField(s.projectColumnPurposeEnum()),
				"resourcePath": gqlNonNull(uri),
				"url":          gqlNonNull(uri),
				"project": &graphql.Field{
					Type: graphql.NewNonNull(s.projectClassicType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						src, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
						}
						projectID, _ := src["_projectID"].(int)
						return optionalObject(s.projectClassicToGQL(s.store.GetProjectClassic(projectID))), nil
					},
				},
				"cards": &graphql.Field{
					Type: graphql.NewNonNull(cardConnection),
					Args: cardsArgs,
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						src, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
						}
						id, _ := src["databaseId"].(int)
						includeArchived, includeActive := projectCardArchivedFilter(p.Args)
						cards := s.store.ListProjectCards(id)
						nodes := make([]map[string]interface{}, 0, len(cards))
						for _, c := range cards {
							if c.Archived && !includeArchived {
								continue
							}
							if !c.Archived && !includeActive {
								continue
							}
							nodes = append(nodes, s.projectClassicCardToGQL(c))
						}
						return paginateGQLMaps(nodes, p.Args), nil
					},
				},
			}
		}),
	})
	s.mutationObjects["ProjectColumn"] = object
	return object
}

// projectCardArchivedFilter reads the archivedStates argument, defaulting to
// both archived and active cards.
func projectCardArchivedFilter(args map[string]interface{}) (includeArchived, includeActive bool) {
	states, ok := args["archivedStates"].([]interface{})
	if !ok || len(states) == 0 {
		return true, true
	}
	for _, raw := range states {
		switch raw {
		case "ARCHIVED":
			includeArchived = true
		case "NOT_ARCHIVED":
			includeActive = true
		}
	}
	return includeArchived, includeActive
}

// projectCardItemUnion is ProjectCardItem: the Issue or PullRequest a content
// card points at.
func (s *Resolver) projectCardItemUnion() *graphql.Union {
	return s.mutationUnion("ProjectCardItem",
		func() []*graphql.Object {
			return []*graphql.Object{s.graphqlTypes.issue, s.graphqlTypes.pullRequest}
		},
		func(p graphql.ResolveTypeParams) *graphql.Object {
			src, _ := p.Value.(map[string]interface{})
			if name, _ := src["__typename"].(string); name == "PullRequest" {
				return s.graphqlTypes.pullRequest
			}
			return s.graphqlTypes.issue
		})
}

func (s *Resolver) projectClassicCardType() *graphql.Object {
	if memo := s.memoizedMutationObject("ProjectCard"); memo != nil {
		return memo
	}
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	object := graphql.NewObject(graphql.ObjectConfig{
		Name:       "ProjectCard",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("nodeID")},
				"databaseId":   gqlField(graphql.Int),
				"note":         gqlField(graphql.String),
				"state":        gqlField(s.projectCardStateEnum()),
				"isArchived":   gqlNonNull(graphql.Boolean),
				"createdAt":    gqlNonNull(dateTime),
				"updatedAt":    gqlNonNull(dateTime),
				"creator":      gqlField(s.graphqlTypes.actor),
				"resourcePath": gqlNonNull(uri),
				"url":          gqlNonNull(uri),
				"column": &graphql.Field{
					Type: s.projectClassicColumnType(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						src, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
						}
						columnID, _ := src["_columnID"].(int)
						return optionalObject(projectClassicColumnToGQL(s.store.GetProjectColumn(columnID))), nil
					},
				},
				"project": &graphql.Field{
					Type: graphql.NewNonNull(s.projectClassicType()),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						src, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
						}
						columnID, _ := src["_columnID"].(int)
						column := s.store.GetProjectColumn(columnID)
						if column == nil {
							return nil, nil
						}
						return optionalObject(s.projectClassicToGQL(s.store.GetProjectClassic(column.ProjectID))), nil
					},
				},
				"content": &graphql.Field{
					Type: s.projectCardItemUnion(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						src, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
						}
						// Content the viewer cannot read is absent, so a card in a
						// shared board can't leak a private repo's issue titles.
						if issueID, _ := src["_issueID"].(int); issueID != 0 {
							issue := s.store.GetIssue(issueID)
							if issue == nil || !s.viewerCanReadProjectContent(p.Context, "Issue", issueID) {
								return nil, nil
							}
							rendered := issueToGQL(issue, s.store)
							rendered["__typename"] = "Issue"
							return rendered, nil
						}
						if prID, _ := src["_prID"].(int); prID != 0 {
							pr := s.store.GetPullRequest(prID)
							if pr == nil || !s.viewerCanReadProjectContent(p.Context, "PullRequest", prID) {
								return nil, nil
							}
							rendered := pullRequestToGQL(pr, s.store)
							rendered["__typename"] = "PullRequest"
							return rendered, nil
						}
						return nil, nil
					},
				},
			}
		}),
	})
	s.mutationObjects["ProjectCard"] = object
	return object
}

// ---------------------------------------------------------------------------
// ProjectOwner

// projectOwnerInterfaceType is the ProjectOwner interface — the account or
// repository a classic board belongs to. Memoized and thunked because User,
// Organization and Repository each declare it at construction, before the
// Project family is assembled.
func (s *Resolver) projectOwnerInterfaceType() *graphql.Interface {
	return s.mutationInterface("ProjectOwner", func() graphql.Fields {
		uri := s.graphQLStringScalar("URI")
		projectType := s.projectClassicType()
		connection, _ := s.projectClassicConnectionPair("Project", projectType)
		return graphql.Fields{
			"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"project": &graphql.Field{
				Type: projectType,
				Args: graphql.FieldConfigArgument{
					"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
				},
			},
			"projects": &graphql.Field{
				Type: graphql.NewNonNull(connection),
				Args: s.projectClassicProjectsArgs(),
			},
			"projectsResourcePath":    &graphql.Field{Type: graphql.NewNonNull(uri)},
			"projectsUrl":             &graphql.Field{Type: graphql.NewNonNull(uri)},
			"viewerCanCreateProjects": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		}
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		src, _ := p.Value.(map[string]interface{})
		switch name, _ := src["__typename"].(string); name {
		case "Organization":
			return s.graphqlTypes.organization
		case "Repository":
			return s.graphqlTypes.repository
		}
		return s.graphqlTypes.user
	})
}

// projectClassicProjectsArgs is a `projects` connection's args: the Relay
// window plus the state filter.
func (s *Resolver) projectClassicProjectsArgs() graphql.FieldConfigArgument {
	args := relayConnectionArgs()
	args["states"] = &graphql.ArgumentConfig{
		Type: graphql.NewList(graphql.NewNonNull(s.projectStateEnum())),
	}
	return args
}

// projectClassicOwnerRef is one ProjectOwner implementor's lens: how to list its
// boards, where its projects pages live, and who may create boards.
type projectClassicOwnerRef struct {
	list      func(src map[string]interface{}) []*store.ProjectClassic
	basePath  func(src map[string]interface{}) string
	canCreate func(ctx context.Context, src map[string]interface{}) bool
}

// addProjectClassicOwnerFields installs the ProjectOwner members on User,
// Organization and Repository, adding only fields each type lacks (Organization
// already carries viewerCanCreateProjects from the account surface).
func (s *Resolver) addProjectClassicOwnerFields(userType, orgType, repoType *graphql.Object) {
	uri := s.graphQLStringScalar("URI")
	projectType := s.projectClassicType()
	connection, _ := s.projectClassicConnectionPair("Project", projectType)

	sourceLogin := func(src map[string]interface{}) string {
		login, _ := src["login"].(string)
		return login
	}
	refs := map[*graphql.Object]projectClassicOwnerRef{
		userType: {
			list: func(src map[string]interface{}) []*store.ProjectClassic {
				return s.store.ListProjectClassicsForOwner("User", sourceLogin(src))
			},
			basePath: func(src map[string]interface{}) string { return "/users/" + sourceLogin(src) },
			canCreate: func(ctx context.Context, src map[string]interface{}) bool {
				viewer := s.ghUserFromContext(ctx)
				return viewer != nil && viewer.Login == sourceLogin(src)
			},
		},
		orgType: {
			list: func(src map[string]interface{}) []*store.ProjectClassic {
				return s.store.ListProjectClassicsForOwner("Organization", sourceLogin(src))
			},
			basePath: func(src map[string]interface{}) string { return "/orgs/" + sourceLogin(src) },
			canCreate: func(ctx context.Context, src map[string]interface{}) bool {
				login := sourceLogin(src)
				return s.viewerIsOrgMember(ctx, login) || s.viewerCanAdminAccount(ctx, login)
			},
		},
		repoType: {
			list: func(src map[string]interface{}) []*store.ProjectClassic {
				fullName, _ := src["nameWithOwner"].(string)
				return s.store.ListProjectClassicsForRepo(fullName)
			},
			basePath: func(src map[string]interface{}) string {
				fullName, _ := src["nameWithOwner"].(string)
				return "/" + fullName
			},
			canCreate: func(ctx context.Context, src map[string]interface{}) bool {
				fullName, _ := src["nameWithOwner"].(string)
				repo := s.store.GetRepoByFullName(fullName)
				return repo != nil && s.viewerHasRepoPermission(ctx, repo, store.ScopeProjects, store.PermWrite)
			},
		},
	}

	for objType, ref := range refs {
		ref := ref
		sourceOf := func(p graphql.ResolveParams) (map[string]interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			return src, nil
		}
		addField := func(name string, field *graphql.Field) {
			if _, exists := objType.Fields()[name]; !exists {
				objType.AddFieldConfig(name, field)
			}
		}
		addField("project", &graphql.Field{
			Type: projectType,
			Args: graphql.FieldConfigArgument{
				"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, err := sourceOf(p)
				if err != nil {
					return nil, err
				}
				number, _ := p.Args["number"].(int)
				for _, project := range ref.list(src) {
					if project.Number == number && s.canReadProjectClassic(p.Context, project) {
						return optionalObject(s.projectClassicToGQL(project)), nil
					}
				}
				return nil, nil
			},
		})
		addField("projects", &graphql.Field{
			Type: graphql.NewNonNull(connection),
			Args: s.projectClassicProjectsArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, err := sourceOf(p)
				if err != nil {
					return nil, err
				}
				wantState := map[string]bool{}
				if states, ok := p.Args["states"].([]interface{}); ok {
					for _, raw := range states {
						if state, ok := raw.(string); ok {
							wantState[state] = true
						}
					}
				}
				nodes := make([]map[string]interface{}, 0)
				for _, project := range ref.list(src) {
					if !s.canReadProjectClassic(p.Context, project) {
						continue
					}
					rendered := s.projectClassicToGQL(project)
					if rendered == nil {
						continue
					}
					if len(wantState) > 0 {
						if state, _ := rendered["state"].(string); !wantState[state] {
							continue
						}
					}
					nodes = append(nodes, rendered)
				}
				return paginateGQLMaps(nodes, p.Args), nil
			},
		})
		addField("projectsResourcePath", &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, err := sourceOf(p)
				if err != nil {
					return nil, err
				}
				return ref.basePath(src) + "/projects", nil
			},
		})
		addField("projectsUrl", &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, err := sourceOf(p)
				if err != nil {
					return nil, err
				}
				return externalURL(ref.basePath(src) + "/projects"), nil
			},
		})
		addField("viewerCanCreateProjects", &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, err := sourceOf(p)
				if err != nil {
					return nil, err
				}
				return ref.canCreate(p.Context, src), nil
			},
		})
	}
}

// ---------------------------------------------------------------------------
// Node dispatch

// projectClassicNodeByID resolves a classic project, column or card global id
// for Query.node, applying the read fields' visibility rule.
func (s *Resolver) projectClassicNodeByID(ctx context.Context, nodeID string) interface{} {
	if live := store.FindProjectClassicByNodeID(s.store, nodeID); live != nil {
		project := s.store.GetProjectClassic(live.ID)
		if !s.canReadProjectClassic(ctx, project) {
			return nil
		}
		return optionalObject(s.projectClassicToGQL(project))
	}
	if live := store.FindProjectColumnByNodeID(s.store, nodeID); live != nil {
		column := s.store.GetProjectColumn(live.ID)
		if column == nil || !s.canReadProjectClassic(ctx, s.store.GetProjectClassic(column.ProjectID)) {
			return nil
		}
		return optionalObject(projectClassicColumnToGQL(column))
	}
	if live := store.FindProjectCardByNodeID(s.store, nodeID); live != nil {
		card := s.store.GetProjectCard(live.ID)
		if card == nil {
			return nil
		}
		column := s.store.GetProjectColumn(card.ColumnID)
		if column == nil || !s.canReadProjectClassic(ctx, s.store.GetProjectClassic(column.ProjectID)) {
			return nil
		}
		return optionalObject(s.projectClassicCardToGQL(card))
	}
	return nil
}

// addProjectsClassicToSchema assembles the classic-projects family: the three
// node types, the ProjectOwner members, and the classic-project mutations.
func (s *Resolver) addProjectsClassicToSchema(userType, orgType, repoType, mutationType *graphql.Object, nodeTypes map[string]*graphql.Object) {
	nodeTypes["Project"] = s.projectClassicType()
	nodeTypes["ProjectColumn"] = s.projectClassicColumnType()
	nodeTypes["ProjectCard"] = s.projectClassicCardType()
	s.addProjectClassicOwnerFields(userType, orgType, repoType)
	s.addProjectsClassicMutations(mutationType)
}
