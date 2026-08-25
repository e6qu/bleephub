package graphqlapi

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Projects v2 — the read surface: the ProjectV2 object's full field set, the
// item/status-update/workflow types hanging off it, and the entry points that
// reach a project in the first place (Organization.projectV2, User.projectsV2,
// Repository.projectsV2). The mutation surface lives in
// gh_projects_v2_graphql.go and gh_projects_v2_mutations_graphql.go.
//
// Without the entry points a project was reachable only from an issue that
// happened to be on one, so `gh project list` and `gh project view` — which
// start from the owner — had nothing to query.

// projectV2ToGQLFull renders a project as its GraphQL source map. Everything
// the ProjectV2 object resolves is either here or reachable from the "id" key
// the connection resolvers re-read the store with, so a project rendered for
// one field selection answers every other field the same way.
func projectV2ToGQLFull(st *store.Store, p *store.ProjectV2) map[string]interface{} {
	if p == nil {
		return nil
	}
	ownerLogin, ownerMap := projectV2OwnerSource(st, p)
	resourcePath := projectV2ResourcePath(p, ownerLogin)
	out := map[string]interface{}{
		"id":               p.ID,
		"nodeID":           p.NodeID,
		"databaseId":       p.ID,
		"fullDatabaseId":   p.ID,
		"number":           p.Number,
		"title":            p.Title,
		"closed":           p.Closed,
		"public":           p.Public,
		"template":         p.Template,
		"shortDescription": nullableString(p.ShortDescription),
		"readme":           nullableString(p.Readme),
		"resourcePath":     resourcePath,
		"url":              externalURL(resourcePath),
		"createdAt":        p.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":        p.UpdatedAt.UTC().Format(time.RFC3339),
		"owner":            ownerMap,
	}
	if p.ClosedAt != nil {
		out["closedAt"] = p.ClosedAt.UTC().Format(time.RFC3339)
	} else {
		out["closedAt"] = nil
	}
	if creator := st.GetUserByID(p.CreatorID); creator != nil {
		out["creator"] = userToGraphQL(creator)
	} else {
		out["creator"] = nil
	}
	return out
}

// projectV2OwnerSource resolves a project's owner into its login and the
// source map the ProjectV2Owner interface dispatches on.
func projectV2OwnerSource(st *store.Store, p *store.ProjectV2) (string, map[string]interface{}) {
	if p.OwnerType == "Organization" {
		if org := st.GetOrgByID(p.OwnerID); org != nil {
			source := orgToGraphQL(org)
			source["__typename"] = "Organization"
			return org.Login, source
		}
		return "", nil
	}
	if u := st.GetUserByID(p.OwnerID); u != nil {
		source := userToGraphQL(u)
		source["__typename"] = "User"
		return u.Login, source
	}
	return "", nil
}

// projectV2ResourcePath is github.com's project path: organization projects
// live under /orgs/{login}, user projects under /users/{login}.
func projectV2ResourcePath(p *store.ProjectV2, ownerLogin string) string {
	if ownerLogin == "" {
		return fmt.Sprintf("/projects/%d", p.Number)
	}
	scope := "users"
	if p.OwnerType == "Organization" {
		scope = "orgs"
	}
	return fmt.Sprintf("/%s/%s/projects/%d", scope, ownerLogin, p.Number)
}

// nullableString maps the store's empty string onto GraphQL null, which is
// what a nullable String field means when the instance has no value. Returning
// "" instead would claim the project has an empty description rather than none.
func nullableString(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}

// ---------------------------------------------------------------------------
// Type enrichment

// enrichProjectV2Types adds the fields the ProjectV2 object graph carries
// beyond the handful the issue family built it with. It runs once, after every
// type it references exists.
func (s *Resolver) enrichProjectV2Types(repoType *graphql.Object, nodeTypes map[string]*graphql.Object) {
	projectType := s.projectV2GraphQLTypes()
	// Building the item connection also builds the item type, the field-value
	// union and the ProjectV2.items field.
	s.projectV2ItemConnectionType()

	dateTime := s.graphQLStringScalar("DateTime")
	date := s.graphQLStringScalar("Date")
	uri := s.graphQLStringScalar("URI")
	bigInt := s.graphQLStringScalar("BigInt")

	s.addProjectV2ScalarFields(projectType, dateTime, uri, bigInt)
	s.addProjectV2OwnerField(projectType)
	s.addProjectV2FieldLookup(projectType)
	s.addProjectV2ViewLookup(projectType)
	s.addProjectV2StatusUpdates(projectType, dateTime, date, bigInt)
	s.addProjectV2Workflows(projectType, dateTime, bigInt)
	s.addProjectV2LinkConnections(projectType, repoType)
	s.addProjectV2ViewerPermissions(projectType)
	s.enrichProjectV2ItemType(dateTime, bigInt, nodeTypes)
	s.enrichProjectV2ViewType()
	// The residual members of the ProjectV2 field-configuration family, the
	// option/iteration objects, ProjectV2View and DraftIssue. It runs last so
	// every type it hangs a field off — the four field configurations and their
	// common interface, the ProjectV2 object, the ProjectV2Item connection and
	// the IssueFields union — is already assembled.
	s.addProjectV2FieldFamilyResidualFields()
}

func (s *Resolver) addProjectV2ScalarFields(projectType *graphql.Object, dateTime, uri, bigInt *graphql.Scalar) {
	for name, field := range map[string]*graphql.Field{
		"shortDescription": {Type: graphql.String},
		"readme":           {Type: graphql.String},
		"template":         {Type: graphql.NewNonNull(graphql.Boolean)},
		"closedAt":         {Type: dateTime},
		"createdAt":        {Type: graphql.NewNonNull(dateTime)},
		"updatedAt":        {Type: graphql.NewNonNull(dateTime)},
		"databaseId":       {Type: graphql.Int},
		"fullDatabaseId":   {Type: bigInt},
		"resourcePath":     {Type: graphql.NewNonNull(uri)},
		"creator":          {Type: s.graphqlTypes.actor},
	} {
		if _, exists := projectType.Fields()[name]; !exists {
			projectType.AddFieldConfig(name, field)
		}
	}
}

// projectV2OwnerInterfaceType is GitHub's ProjectV2Owner interface: the thing
// a project belongs to.
//
// It is built before the Organization and User objects are, because
// graphql-go derives an interface's possible types from the Interfaces list
// each object declares at construction and memoizes that on first read. An
// interface minted later can never gain implementations, and a client's
// `... on Organization` spread against it fails validation — which is exactly
// what `gh project list` sends.
//
// Its fields are declared through a thunk so it may name the ProjectV2 object
// and its connection, neither of which exists this early.
func (s *Resolver) projectV2OwnerInterfaceType() *graphql.Interface {
	if s.graphqlTypes.projectV2OwnerInterface != nil {
		return s.graphqlTypes.projectV2OwnerInterface
	}
	s.graphqlTypes.projectV2OwnerInterface = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "ProjectV2Owner",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			projectType := s.projectV2GraphQLTypes()
			return graphql.Fields{
				"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"projectV2": &graphql.Field{
					Type: projectType,
					Args: graphql.FieldConfigArgument{
						"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
					},
				},
				"projectsV2": &graphql.Field{
					Type: graphql.NewNonNull(s.gqlConnectionType("ProjectV2", projectType)),
					Args: s.projectV2ConnectionArgs(),
				},
			}
		}),
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name == "Organization" {
				return s.graphqlTypes.organization
			}
			return s.graphqlTypes.user
		},
	})
	return s.graphqlTypes.projectV2OwnerInterface
}

// addProjectV2OwnerField wires ProjectV2.owner. The owner is an interface with
// two implementations, so the source map carries __typename for dispatch.
func (s *Resolver) addProjectV2OwnerField(projectType *graphql.Object) {
	projectType.AddFieldConfig("owner", &graphql.Field{
		Type: graphql.NewNonNull(s.projectV2OwnerInterfaceType()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			return src["owner"], nil
		},
	})
}

// projectV2ConnectionArgs is the argument set a projectsV2 connection takes:
// the Relay window plus GitHub's ordering and search members.
func (s *Resolver) projectV2ConnectionArgs() graphql.FieldConfigArgument {
	args := relayConnectionArgs()
	args["orderBy"] = &graphql.ArgumentConfig{Type: s.projectV2OrderInput()}
	args["query"] = &graphql.ArgumentConfig{Type: graphql.String}
	return args
}

// addProjectV2FieldLookup wires ProjectV2.field(name:), the single-field
// lookup `gh project item-edit` uses to turn a field name into its id.
func (s *Resolver) addProjectV2FieldLookup(projectType *graphql.Object) {
	projectType.AddFieldConfig("field", &graphql.Field{
		Type: s.projectV2FieldConfigurationUnion(),
		Args: graphql.FieldConfigArgument{
			"name": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			name, _ := p.Args["name"].(string)
			field := s.store.ProjectsV2.FieldByNameOnProject(projectID, name)
			if field == nil {
				return nil, nil
			}
			return projectV2FieldToGQL(field), nil
		},
	})
}

// addProjectV2ViewLookup wires ProjectV2.view(number:).
func (s *Resolver) addProjectV2ViewLookup(projectType *graphql.Object) {
	projectType.AddFieldConfig("view", &graphql.Field{
		Type: s.projectV2ViewObjectType(),
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			number, _ := p.Args["number"].(int)
			view := s.store.ProjectsV2.GetViewByNumber(projectID, number)
			if view == nil {
				return nil, nil
			}
			return projectV2ViewToGQL(view), nil
		},
	})
}

// projectV2ViewObjectType returns the ProjectV2View object, building the view
// connection (which owns the memo) on first use.
func (s *Resolver) projectV2ViewObjectType() *graphql.Object {
	s.projectV2ViewConnectionType()
	return s.graphqlTypes.projectV2ViewTypeMemo
}

// enrichProjectV2ViewType adds the view's configuration surface: the grouping,
// sorting and visible-field selections a board or roadmap view carries.
func (s *Resolver) enrichProjectV2ViewType() {
	viewType := s.projectV2ViewObjectType()
	fieldConnection := s.projectV2FieldConnectionType()

	visibleFieldsOf := func(key string) func(graphql.ResolveParams) (interface{}, error) {
		return func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			ids, _ := src[key].([]int)
			nodes := make([]map[string]interface{}, 0, len(ids))
			for _, id := range ids {
				if f := s.store.ProjectsV2.GetField(id); f != nil {
					nodes = append(nodes, projectV2FieldToGQL(f))
				}
			}
			return paginateGQLMaps(nodes, p.Args), nil
		}
	}

	// GitHub splits these two ways. The bare names predate custom field types
	// and are typed over the plain ProjectV2Field object; the newer `…Fields`
	// names widen to the ProjectV2FieldConfiguration union. Both read the same
	// stored id list.
	plainFieldConnection := s.projectV2PlainFieldConnectionType()
	for name, key := range map[string]string{
		"visibleFields":   "visibleFieldIds",
		"groupBy":         "groupByFieldIds",
		"verticalGroupBy": "verticalGroupByFieldIds",
	} {
		viewType.AddFieldConfig(name, &graphql.Field{
			Type:    plainFieldConnection,
			Args:    orderedConnectionArgs(s.projectV2FieldOrderInput()),
			Resolve: visibleFieldsOf(key),
		})
	}
	for name, key := range map[string]string{
		"groupByFields":         "groupByFieldIds",
		"verticalGroupByFields": "verticalGroupByFieldIds",
	} {
		viewType.AddFieldConfig(name, &graphql.Field{
			Type:    fieldConnection,
			Args:    orderedConnectionArgs(s.projectV2FieldOrderInput()),
			Resolve: visibleFieldsOf(key),
		})
	}

	viewType.AddFieldConfig("databaseId", &graphql.Field{Type: graphql.Int})
	viewType.AddFieldConfig("configuration", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewObject(graphql.ObjectConfig{
			Name: "ProjectV2ViewConfiguration",
			Fields: graphql.Fields{
				"visibleFields": &graphql.Field{
					Type:    graphql.NewNonNull(fieldConnection),
					Args:    relayConnectionArgs(),
					Resolve: visibleFieldsOf("visibleFieldIds"),
				},
			},
		})),
		// The configuration object is a view of the same source map, so it
		// resolves to the view itself rather than a second copy.
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return p.Source, nil },
	})

	sortByType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2SortBy",
		Fields: graphql.Fields{
			"direction": &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC"))},
			"field":     &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.projectV2FieldTypeMemo)},
		},
	})
	sortByConnection := s.gqlConnectionType("ProjectV2SortBy", sortByType)
	resolveSortBy := func(p graphql.ResolveParams) (interface{}, error) {
		src, ok := p.Source.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
		}
		entries, _ := src["sortBy"].([]map[string]interface{})
		nodes := make([]map[string]interface{}, 0, len(entries))
		for _, entry := range entries {
			fieldID, _ := entry["fieldID"].(int)
			field := s.store.ProjectsV2.GetField(fieldID)
			if field == nil {
				continue
			}
			nodes = append(nodes, map[string]interface{}{
				"direction": entry["direction"],
				"field":     projectV2FieldToGQL(field),
			})
		}
		return paginateGQLMaps(nodes, p.Args), nil
	}
	viewType.AddFieldConfig("sortBy", &graphql.Field{
		Type: sortByConnection, Args: relayConnectionArgs(), Resolve: resolveSortBy,
	})
	// sortByFields is the same list under GitHub's newer name, whose element
	// type widens `field` to the configuration union.
	sortByFieldType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2SortByField",
		Fields: graphql.Fields{
			"direction": &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC"))},
			"field":     &graphql.Field{Type: graphql.NewNonNull(s.projectV2FieldConfigurationUnion())},
		},
	})
	viewType.AddFieldConfig("sortByFields", &graphql.Field{
		Type:    s.gqlConnectionType("ProjectV2SortByField", sortByFieldType),
		Args:    relayConnectionArgs(),
		Resolve: resolveSortBy,
	})
	viewType.AddFieldConfig("project", &graphql.Field{
		Type: graphql.NewNonNull(s.projectV2GraphQLTypes()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			projectID, _ := src["projectID"].(int)
			return optionalObject(projectV2ToGQLFull(s.store, s.store.ProjectsV2.GetProject(projectID))), nil
		},
	})
}

// projectV2PlainFieldConnectionType is the connection over the plain
// ProjectV2Field object, as distinct from the union-typed
// ProjectV2FieldConfigurationConnection. GitHub keeps both, and the view's
// grouping fields are typed over this one.
func (s *Resolver) projectV2PlainFieldConnectionType() *graphql.Object {
	// Building the configuration connection is what mints the field objects.
	s.projectV2FieldConnectionType()
	return s.gqlConnectionType("ProjectV2Field", s.graphqlTypes.projectV2FieldTypeMemo)
}

// gqlConnectionType builds the Relay connection and edge objects for a node
// type, under GitHub's <Name>Connection / <Name>Edge naming.
func (s *Resolver) gqlConnectionType(name string, nodeType graphql.Output) *graphql.Object {
	if s.graphqlTypes.projectV2Connections == nil {
		s.graphqlTypes.projectV2Connections = map[string]*graphql.Object{}
	}
	if memo := s.graphqlTypes.projectV2Connections[name]; memo != nil {
		return memo
	}
	edgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: name + "Edge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: nodeType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	connection := graphql.NewObject(graphql.ObjectConfig{
		Name: name + "Connection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(nodeType)},
			"edges":      &graphql.Field{Type: graphql.NewList(edgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	s.graphqlTypes.projectV2Connections[name] = connection
	return connection
}

// ---------------------------------------------------------------------------
// Status updates

func (s *Resolver) addProjectV2StatusUpdates(projectType *graphql.Object, dateTime, date, bigInt *graphql.Scalar) {
	statusEnum := s.graphQLEnum("ProjectV2StatusUpdateStatus", store.ProjectV2StatusUpdateStatuses...)
	statusType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2StatusUpdate",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.ID),
				Resolve: sourceKeyResolver("nodeID"),
			},
			"body":           &graphql.Field{Type: graphql.String},
			"bodyHTML":       &graphql.Field{Type: s.graphQLStringScalar("HTML")},
			"status":         &graphql.Field{Type: statusEnum},
			"startDate":      &graphql.Field{Type: date},
			"targetDate":     &graphql.Field{Type: date},
			"createdAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"databaseId":     &graphql.Field{Type: graphql.Int},
			"fullDatabaseId": &graphql.Field{Type: bigInt},
			"creator":        &graphql.Field{Type: s.graphqlTypes.actor},
			"project": &graphql.Field{
				Type: graphql.NewNonNull(projectType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					projectID, _ := src["projectID"].(int)
					return optionalObject(projectV2ToGQLFull(s.store, s.store.ProjectsV2.GetProject(projectID))), nil
				},
			},
		},
	})
	s.graphqlTypes.projectV2StatusUpdateType = statusType

	projectType.AddFieldConfig("statusUpdates", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlConnectionType("ProjectV2StatusUpdate", statusType)),
		Args: orderedConnectionArgs(s.projectV2StatusOrderInput()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			updates := s.store.ProjectsV2.StatusUpdatesForProject(projectID)
			nodes := make([]map[string]interface{}, 0, len(updates))
			for _, u := range updates {
				nodes = append(nodes, s.projectV2StatusUpdateToGQL(u))
			}
			projectV2SortNodes(nodes, p.Args, map[string]string{"CREATED_AT": "createdAt"})
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

func (s *Resolver) projectV2StatusUpdateToGQL(u *store.ProjectV2StatusUpdate) map[string]interface{} {
	if u == nil {
		return nil
	}
	out := map[string]interface{}{
		"nodeID":         u.NodeID,
		"projectID":      u.ProjectID,
		"databaseId":     u.ID,
		"fullDatabaseId": u.ID,
		"body":           nullableString(u.Body),
		"bodyHTML":       nullableString(u.Body),
		"status":         nullableString(u.Status),
		"startDate":      nullableString(u.StartDate),
		"targetDate":     nullableString(u.TargetDate),
		"createdAt":      u.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":      u.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if creator := s.store.GetUserByID(u.CreatorID); creator != nil {
		out["creator"] = userToGraphQL(creator)
	} else {
		out["creator"] = nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Workflows

func (s *Resolver) addProjectV2Workflows(projectType *graphql.Object, dateTime, bigInt *graphql.Scalar) {
	workflowType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2Workflow",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.ID),
				Resolve: sourceKeyResolver("nodeID"),
			},
			"name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"number":         &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"enabled":        &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"createdAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"databaseId":     &graphql.Field{Type: graphql.Int},
			"fullDatabaseId": &graphql.Field{Type: bigInt},
			"project": &graphql.Field{
				Type: graphql.NewNonNull(projectType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					projectID, _ := src["projectID"].(int)
					return optionalObject(projectV2ToGQLFull(s.store, s.store.ProjectsV2.GetProject(projectID))), nil
				},
			},
		},
	})
	s.graphqlTypes.projectV2WorkflowType = workflowType

	projectType.AddFieldConfig("workflows", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlConnectionType("ProjectV2Workflow", workflowType)),
		Args: orderedConnectionArgs(s.projectV2WorkflowOrderInput()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			workflows := s.store.ProjectsV2.WorkflowsForProject(projectID)
			nodes := make([]map[string]interface{}, 0, len(workflows))
			for _, w := range workflows {
				nodes = append(nodes, projectV2WorkflowToGQL(w))
			}
			projectV2SortNodes(nodes, p.Args, map[string]string{
				"NAME": "name", "NUMBER": "number",
				"CREATED_AT": "createdAt", "UPDATED_AT": "updatedAt",
			})
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	projectType.AddFieldConfig("workflow", &graphql.Field{
		Type: workflowType,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			number, _ := p.Args["number"].(int)
			w := s.store.ProjectsV2.GetWorkflowByNumber(projectID, number)
			if w == nil {
				return nil, nil
			}
			return projectV2WorkflowToGQL(w), nil
		},
	})
}

func projectV2WorkflowToGQL(w *store.ProjectV2Workflow) map[string]interface{} {
	if w == nil {
		return nil
	}
	return map[string]interface{}{
		"nodeID":         w.NodeID,
		"projectID":      w.ProjectID,
		"databaseId":     w.ID,
		"fullDatabaseId": w.ID,
		"name":           w.Name,
		"number":         w.Number,
		"enabled":        w.Enabled,
		"createdAt":      w.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":      w.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// Linked repositories and teams

func (s *Resolver) addProjectV2LinkConnections(projectType *graphql.Object, repoType *graphql.Object) {
	projectType.AddFieldConfig("repositories", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repositoryConnection),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			project := s.store.ProjectsV2.GetProject(projectID)
			if project == nil {
				return paginateGQLMaps(nil, p.Args), nil
			}
			nodes := make([]map[string]interface{}, 0, len(project.LinkedRepoIDs))
			for _, repoID := range project.LinkedRepoIDs {
				repo := s.store.GetRepoByID(repoID)
				// A repository the caller cannot read is not listed: the link
				// list would otherwise disclose private repository names to
				// anyone who can see the project.
				if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
					continue
				}
				nodes = append(nodes, repoToGraphQL(s.store, repo))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	projectType.AddFieldConfig("teams", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlConnectionType("Team", s.graphqlTypes.team)),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			project := s.store.ProjectsV2.GetProject(projectID)
			if project == nil {
				return paginateGQLMaps(nil, p.Args), nil
			}
			nodes := make([]map[string]interface{}, 0, len(project.LinkedTeamIDs))
			for _, teamID := range project.LinkedTeamIDs {
				team := s.store.GetTeamByID(teamID)
				if team == nil {
					continue
				}
				org := s.store.GetOrgByID(team.OrgID)
				if org == nil {
					continue
				}
				nodes = append(nodes, map[string]interface{}{
					"id":           team.NodeID,
					"name":         team.Name,
					"slug":         team.Slug,
					"organization": orgToGraphQL(org),
				})
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// ---------------------------------------------------------------------------
// Viewer permissions

// addProjectV2ViewerPermissions wires the three viewerCan* booleans. They are
// answered by the same predicates that gate the mutations, so a client that
// hides a control on `viewerCanUpdate: false` is hiding exactly the operation
// the server would refuse.
func (s *Resolver) addProjectV2ViewerPermissions(projectType *graphql.Object) {
	canWrite := func(p graphql.ResolveParams) (interface{}, error) {
		projectID, err := projectV2SourceID(p.Source)
		if err != nil {
			return nil, err
		}
		project := s.store.ProjectsV2.GetProject(projectID)
		if project == nil {
			return false, nil
		}
		owner := s.projectV2OwnerByID(project.OwnerID, project.OwnerType)
		if owner == nil {
			return false, nil
		}
		user := s.ghUserFromContext(p.Context)
		return s.canWriteProjectV2(p.Context, user, owner), nil
	}
	for _, name := range []string{"viewerCanUpdate", "viewerCanClose", "viewerCanReopen"} {
		projectType.AddFieldConfig(name, &graphql.Field{
			Type:    graphql.NewNonNull(graphql.Boolean),
			Resolve: canWrite,
		})
	}
}

// sourceKeyResolver reads one key out of a source map. Node ids are stored
// under "nodeID" so the database id can keep the "id" key that the connection
// resolvers re-read the store with.
func sourceKeyResolver(key string) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		src, ok := p.Source.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
		}
		return src[key], nil
	}
}

// ---------------------------------------------------------------------------
// ProjectV2Item

// enrichProjectV2ItemType adds everything on a project item beyond the id,
// project and fieldValueByName the issue family built: the content union that
// `gh project item-list` reads titles out of, the fieldValues connection, and
// the item's own metadata.
func (s *Resolver) enrichProjectV2ItemType(dateTime, bigInt *graphql.Scalar, nodeTypes map[string]*graphql.Object) {
	itemType := s.projectV2ItemType()

	draftIssueType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DraftIssue",
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("nodeID")},
			"title":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"body":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"bodyText":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"bodyHTML":  &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("HTML"))},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"creator":   &graphql.Field{Type: s.graphqlTypes.actor},
		},
	})
	s.graphqlTypes.projectV2DraftIssueType = draftIssueType

	contentTypes := []*graphql.Object{draftIssueType}
	if issueType := nodeTypes["Issue"]; issueType != nil {
		contentTypes = append(contentTypes, issueType)
	}
	if prType := nodeTypes["PullRequest"]; prType != nil {
		contentTypes = append(contentTypes, prType)
	}
	contentUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "ProjectV2ItemContent",
		Types: contentTypes,
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			switch source["__typename"] {
			case "Issue":
				return nodeTypes["Issue"]
			case "PullRequest":
				return nodeTypes["PullRequest"]
			default:
				return draftIssueType
			}
		},
	})

	itemType.AddFieldConfig("content", &graphql.Field{
		Type:    contentUnion,
		Resolve: sourceKeyResolver("content"),
	})
	itemType.AddFieldConfig("type", &graphql.Field{
		Type:    graphql.NewNonNull(s.graphQLEnum("ProjectV2ItemType", "DRAFT_ISSUE", "ISSUE", "PULL_REQUEST", "REDACTED")),
		Resolve: sourceKeyResolver("type"),
	})
	itemType.AddFieldConfig("isArchived", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: sourceKeyResolver("isArchived"),
	})
	itemType.AddFieldConfig("createdAt", &graphql.Field{Type: graphql.NewNonNull(dateTime)})
	itemType.AddFieldConfig("updatedAt", &graphql.Field{Type: graphql.NewNonNull(dateTime)})
	itemType.AddFieldConfig("databaseId", &graphql.Field{Type: graphql.Int})
	itemType.AddFieldConfig("fullDatabaseId", &graphql.Field{Type: bigInt})
	itemType.AddFieldConfig("creator", &graphql.Field{Type: s.graphqlTypes.actor})

	itemType.AddFieldConfig("fieldValues", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlConnectionType("ProjectV2ItemFieldValue", s.graphqlTypes.projectV2ItemFieldValueUnionMemo)),
		Args: orderedConnectionArgs(s.projectV2ItemFieldValueOrderInput()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			values, _ := src["fieldValues"].([]map[string]interface{})
			return paginateGQLMaps(values, p.Args), nil
		},
	})
}

// ---------------------------------------------------------------------------
// Owner entry points

// addProjectV2OwnerFields wires the fields that reach a project from its
// owner. Organization and User carry the pair GitHub's ProjectV2Owner
// interface declares; Repository carries the same pair over its owner's
// projects, which is what github.com's repository Projects tab lists.
func (s *Resolver) addProjectV2OwnerFields(orgType, userType, repoType *graphql.Object) {
	projectType := s.projectV2GraphQLTypes()
	connection := s.gqlConnectionType("ProjectV2", projectType)

	// listFor answers a projectsV2 connection for whichever account the source
	// map names, filtered to what this viewer may see.
	listFor := func(ownerType string) graphql.FieldResolveFn {
		return func(p graphql.ResolveParams) (interface{}, error) {
			ownerID, err := projectV2OwnerSourceID(p.Source)
			if err != nil {
				return nil, err
			}
			return s.projectV2Connection(p, ownerID, ownerType), nil
		}
	}
	lookupFor := func(ownerType string) graphql.FieldResolveFn {
		return func(p graphql.ResolveParams) (interface{}, error) {
			ownerID, err := projectV2OwnerSourceID(p.Source)
			if err != nil {
				return nil, err
			}
			number, _ := p.Args["number"].(int)
			return optionalObject(s.projectV2ByNumber(p, ownerID, ownerType, number)), nil
		}
	}

	for _, owner := range []struct {
		object    *graphql.Object
		ownerType string
	}{{orgType, "Organization"}, {userType, "User"}} {
		owner.object.AddFieldConfig("projectsV2", &graphql.Field{
			Type:    graphql.NewNonNull(connection),
			Args:    s.projectV2ConnectionArgs(),
			Resolve: listFor(owner.ownerType),
		})
		owner.object.AddFieldConfig("projectV2", &graphql.Field{
			Type: projectType,
			Args: graphql.FieldConfigArgument{
				"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
			},
			Resolve: lookupFor(owner.ownerType),
		})
	}

	// A repository's Projects tab lists its owner's projects, so the
	// repository fields resolve through the repository's owner rather than
	// over a project-to-repository association a repository does not have.
	repoOwner := func(source interface{}) (int, string, error) {
		src, ok := source.(map[string]interface{})
		if !ok {
			return 0, "", fmt.Errorf("resolve source: unexpected type %T", source)
		}
		repoID, _ := src["databaseId"].(int)
		repo := s.store.GetRepoByID(repoID)
		if repo == nil {
			return 0, "", fmt.Errorf("repository source missing databaseId")
		}
		if org := s.store.GetOrgByID(repo.OwnerID); org != nil {
			return org.ID, "Organization", nil
		}
		return repo.OwnerID, "User", nil
	}
	repoType.AddFieldConfig("projectsV2", &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: s.projectV2ConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ownerID, ownerType, err := repoOwner(p.Source)
			if err != nil {
				return nil, err
			}
			return s.projectV2Connection(p, ownerID, ownerType), nil
		},
	})
	repoType.AddFieldConfig("projectV2", &graphql.Field{
		Type: projectType,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ownerID, ownerType, err := repoOwner(p.Source)
			if err != nil {
				return nil, err
			}
			number, _ := p.Args["number"].(int)
			return optionalObject(s.projectV2ByNumber(p, ownerID, ownerType, number)), nil
		},
	})
}

// projectV2OrderInput is GitHub's ProjectV2Order input. The ordering is
// applied by projectV2Connection; declaring it lets gh's queries type-check,
// which they do not without it.
func (s *Resolver) projectV2OrderInput() *graphql.InputObject {
	if s.graphqlTypes.projectV2OrderInput != nil {
		return s.graphqlTypes.projectV2OrderInput
	}
	s.graphqlTypes.projectV2OrderInput = graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ProjectV2Order",
		Fields: graphql.InputObjectConfigFieldMap{
			"field": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.graphQLEnum("ProjectV2OrderField", "TITLE", "NUMBER", "UPDATED_AT", "CREATED_AT")),
			},
			"direction": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC")),
			},
		},
	})
	return s.graphqlTypes.projectV2OrderInput
}

// projectV2OwnerSourceID reads the database id of the account a source map
// describes. Organization and User source maps both carry it under
// "databaseId".
func projectV2OwnerSourceID(source interface{}) (int, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("resolve source: unexpected type %T", source)
	}
	id, ok := src["databaseId"].(int)
	if !ok || id == 0 {
		return 0, fmt.Errorf("owner source missing databaseId")
	}
	return id, nil
}

// projectV2Connection lists an owner's projects, dropping the ones this
// viewer may not see and applying the requested ordering and search.
func (s *Resolver) projectV2Connection(p graphql.ResolveParams, ownerID int, ownerType string) map[string]interface{} {
	owner := s.projectV2OwnerByID(ownerID, ownerType)
	if owner == nil {
		return paginateGQLMaps(nil, p.Args)
	}
	user := s.ghUserFromContext(p.Context)
	projects := s.store.ProjectsV2.ListProjectsForOwner(ownerID, ownerType)
	visible := make([]*store.ProjectV2, 0, len(projects))
	for _, project := range projects {
		if s.canReadProjectV2(p.Context, user, owner, project) {
			visible = append(visible, project)
		}
	}
	if query, _ := p.Args["query"].(string); strings.TrimSpace(query) != "" {
		needle := strings.ToLower(strings.TrimSpace(query))
		filtered := visible[:0]
		for _, project := range visible {
			if strings.Contains(strings.ToLower(project.Title), needle) ||
				strings.Contains(strings.ToLower(project.ShortDescription), needle) {
				filtered = append(filtered, project)
			}
		}
		visible = filtered
	}
	sortProjectV2(visible, p.Args)
	nodes := make([]map[string]interface{}, 0, len(visible))
	for _, project := range visible {
		nodes = append(nodes, projectV2ToGQLFull(s.store, project))
	}
	return paginateGQLMaps(nodes, p.Args)
}

// sortProjectV2 applies a ProjectV2Order argument. The default is ascending
// project number, which is the order ListProjectsForOwner already returns.
func sortProjectV2(projects []*store.ProjectV2, args map[string]interface{}) {
	orderBy, _ := args["orderBy"].(map[string]interface{})
	if orderBy == nil {
		return
	}
	field, _ := orderBy["field"].(string)
	descending, _ := orderBy["direction"].(string)
	less := func(a, b *store.ProjectV2) bool { return a.Number < b.Number }
	switch field {
	case "TITLE":
		less = func(a, b *store.ProjectV2) bool { return a.Title < b.Title }
	case "CREATED_AT":
		less = func(a, b *store.ProjectV2) bool { return a.CreatedAt.Before(b.CreatedAt) }
	case "UPDATED_AT":
		less = func(a, b *store.ProjectV2) bool { return a.UpdatedAt.Before(b.UpdatedAt) }
	}
	sort.SliceStable(projects, func(i, j int) bool {
		if descending == "DESC" {
			return less(projects[j], projects[i])
		}
		return less(projects[i], projects[j])
	})
}

// projectV2ByNumber resolves one project by its per-owner number, answering
// nil for both "no such project" and "you may not see it".
func (s *Resolver) projectV2ByNumber(p graphql.ResolveParams, ownerID int, ownerType string, number int) map[string]interface{} {
	owner := s.projectV2OwnerByID(ownerID, ownerType)
	if owner == nil {
		return nil
	}
	project := s.store.ProjectsV2.GetProjectByOwnerNumber(ownerID, ownerType, number)
	if project == nil {
		return nil
	}
	if !s.canReadProjectV2(p.Context, s.ghUserFromContext(p.Context), owner, project) {
		return nil
	}
	return projectV2ToGQLFull(s.store, project)
}
