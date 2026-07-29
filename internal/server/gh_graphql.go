package bleephub

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// ghNotFoundError marks a resolver lookup miss that must surface as a
// GitHub-shaped errors[] entry carrying `"type": "NOT_FOUND"`. That member
// sits OUTSIDE the GraphQL spec's standard error keys — it's GitHub-specific,
// and gh CLI / go-gh key on it (e.g. the PR finder distinguishes "no such PR"
// from transport errors by Type == "NOT_FOUND"). Returning bare null data
// without the typed error makes clients decode a zero-valued object instead
// of reporting "not found".
type ghNotFoundError struct {
	message string
}

func (e *ghNotFoundError) Error() string { return e.message }

// ghErrorIsNotFound unwraps graphql-go's error layering (FormattedError →
// *gqlerrors.Error → resolver error) looking for a ghNotFoundError.
func ghErrorIsNotFound(err error) bool {
	for err != nil {
		if _, ok := err.(*ghNotFoundError); ok {
			return true
		}
		switch e := err.(type) {
		case *gqlerrors.Error:
			err = e.OriginalError
		case gqlerrors.Error:
			err = e.OriginalError
		case gqlerrors.FormattedError:
			err = e.OriginalError()
		default:
			return false
		}
	}
	return false
}

// initGraphQLSchema builds the GraphQL schema with all types and resolvers.
func (s *Server) initGraphQLSchema() {
	nodeTypes := map[string]*graphql.Object{}
	nodeInterface := graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Node",
		Fields: graphql.Fields{
			"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name != "" {
				return nodeTypes[name]
			}
			nodeID, _ := source["nodeID"].(string)
			switch {
			case strings.HasPrefix(nodeID, "U_"):
				return nodeTypes["User"]
			case strings.HasPrefix(nodeID, "O_"):
				return nodeTypes["Organization"]
			case strings.HasPrefix(nodeID, "R_"):
				return nodeTypes["Repository"]
			case strings.HasPrefix(nodeID, "I_"):
				return nodeTypes["Issue"]
			case strings.HasPrefix(nodeID, "PR_"):
				return nodeTypes["PullRequest"]
			default:
				return nil
			}
		},
	})
	userType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "User",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					u, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("user source: unexpected type %T", p.Source)
					}
					return u["nodeID"], nil
				},
			},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"login":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":       &graphql.Field{Type: graphql.String},
			"email":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"avatarUrl":  &graphql.Field{Type: graphql.String},
			"bio":        &graphql.Field{Type: graphql.String},
			"url":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"createdAt":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"updatedAt":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	nodeTypes["User"] = userType

	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"viewer": &graphql.Field{
				Type: userType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					user := ghUserFromContext(p.Context)
					if user == nil {
						return nil, nil
					}
					return userToGraphQL(user), nil
				},
			},
			// user(login:) — `gh org list` resolves the target user's
			// organizations through this root field rather than viewer.
			"user": &graphql.Field{
				Type: userType,
				Args: graphql.FieldConfigArgument{
					"login": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					login, _ := p.Args["login"].(string)
					u := s.store.LookupUserByLogin(login)
					if u == nil {
						return nil, nil
					}
					return userToGraphQL(u), nil
				},
			},
		},
	})
	s.addMetaFieldsToSchema(queryType)

	// Add repository types, queries, and mutations
	repoType, mutationType := s.addRepoFieldsToSchema(userType, queryType, nodeInterface)
	nodeTypes["Repository"] = repoType

	// Add organization types and queries
	orgType := s.addOrgFieldsToSchema(userType, queryType, nodeInterface)
	nodeTypes["Organization"] = orgType

	// Add issue types, queries, and mutations
	issueType := s.addIssueFieldsToSchema(userType, repoType, mutationType, queryType, nodeInterface)
	nodeTypes["Issue"] = issueType

	// Add pull request types, queries, and mutations
	pullRequestType := s.addPullRequestFieldsToSchema(userType, issueType, repoType, mutationType, queryType, nodeInterface)
	nodeTypes["PullRequest"] = pullRequestType
	s.addNodeFieldsToSchema(queryType, nodeInterface)

	// Add discussion types, queries, and mutations
	s.addDiscussionFieldsToSchema(userType, repoType, mutationType)

	// Add moderation mutations (minimize/unminimize comment, lock/unlock).
	s.addModerationMutationsToSchema(mutationType)

	// Add Projects v2 mutations (createProjectV2, addProjectV2ItemById).
	s.addProjectV2MutationsToSchema(mutationType)

	// Every mutation is now registered. Authorization coverage is asserted over
	// the assembled type rather than trusted to each family above, so a
	// mutation that reaches the store without a policy row stops the process
	// here instead of shipping open to any signed-in account.
	assertMutationsAuthorized(mutationType)

	schema, err := graphql.NewSchema(graphql.SchemaConfig{
		Query:    queryType,
		Mutation: mutationType,
	})
	if err != nil {
		panic(fmt.Sprintf("failed to create graphql schema: %v", err))
	}
	s.graphqlSchema = schema
}

func (s *Server) addNodeFieldsToSchema(queryType *graphql.Object, nodeInterface *graphql.Interface) {
	queryType.AddFieldConfig("node", &graphql.Field{
		Type: nodeInterface,
		Args: graphql.FieldConfigArgument{
			"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id, _ := p.Args["id"].(string)
			return s.graphQLNodeByID(p.Context, id), nil
		},
	})
	queryType.AddFieldConfig("nodes", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(nodeInterface)),
		Args: graphql.FieldConfigArgument{
			"ids": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.ID)))},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ids, _ := p.Args["ids"].([]interface{})
			nodes := make([]interface{}, 0, len(ids))
			for _, raw := range ids {
				id, _ := raw.(string)
				nodes = append(nodes, s.graphQLNodeByID(p.Context, id))
			}
			return nodes, nil
		},
	})
}

func (s *Server) graphQLNodeByID(ctx context.Context, nodeID string) interface{} {
	if user := findUserByNodeID(s.store, nodeID); user != nil {
		return userToGraphQL(user)
	}
	s.store.mu.RLock()
	var organization *Org
	for _, candidate := range s.store.Orgs {
		if candidate.NodeID == nodeID {
			copy := *candidate
			organization = &copy
			break
		}
	}
	s.store.mu.RUnlock()
	if organization != nil {
		return orgToGraphQL(organization)
	}
	if repo := findRepoByNodeID(s.store, nodeID); repo != nil {
		if repo.Private && !s.viewerCanReadRepo(ctx, repo) {
			return nil
		}
		return repoToGraphQL(s.store, s.store.snapRepo(repo))
	}
	if issue := findIssueByNodeID(s.store, nodeID); issue != nil {
		repo := s.store.GetRepoByID(issue.RepoID)
		if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
			return nil
		}
		return issueToGQL(issue, s.store)
	}
	if pullRequest := findPullRequestByNodeID(s.store, nodeID); pullRequest != nil {
		repo := s.store.GetRepoByID(pullRequest.RepoID)
		if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
			return nil
		}
		return pullRequestToGQL(pullRequest, s.store)
	}
	return nil
}

func (s *Server) registerGHGraphQLRoutes() {
	s.route("POST /api/graphql", s.handleGraphQL)
}

// handleGraphQL executes a GraphQL query.
//
// The endpoint requires authentication. GitHub's /graphql rejects every
// anonymous request with 401 and has no public read surface at all, which
// matters here for more than parity: resolvers reach the store directly, so an
// anonymous caller admitted at this door is a caller inside every connection
// the schema exposes.
func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if ghUserFromContext(r.Context()) == nil &&
		ghInstallationTokenFromContext(r.Context()) == nil &&
		ghUserToServerTokenFromContext(r.Context()) == nil {
		writeGHError(w, http.StatusUnauthorized, "This endpoint requires you to be authenticated.")
		return
	}

	var req struct {
		Query         string                 `json:"query"`
		Variables     map[string]interface{} `json:"variables"`
		OperationName string                 `json:"operationName"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// graphql-go panics on some syntactically-parsed-but-invalid variable
	// definitions (e.g. `query($A:){A}` — an empty Named type). Pre-validate
	// the parsed AST so malformed queries return a GraphQL errors[] envelope
	// instead of crashing the handler.
	if err := graphqlValidateNoPanic(s.graphqlSchema, req.Query); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"data":   nil,
			"errors": []map[string]interface{}{{"message": err.Error()}},
		})
		return
	}

	result := graphql.Do(graphql.Params{
		Schema:         s.graphqlSchema,
		RequestString:  req.Query,
		VariableValues: req.Variables,
		OperationName:  req.OperationName,
		Context:        r.Context(),
	})

	// Debug: log the query + any errors so the harness can diagnose which
	// gh CLI queries hit unimplemented fields.
	if len(result.Errors) > 0 {
		s.logger.Debug().
			Str("operation", req.OperationName).
			Str("query", req.Query).
			Interface("errors", result.Errors).
			Msg("graphql errors")
	}

	// Re-shape errors[] into GitHub's wire form: real GitHub adds a
	// non-spec top-level "type" member (NOT_FOUND, FORBIDDEN, ...) that
	// graphql-go's FormattedError cannot carry, so the envelope is built
	// by hand instead of serializing graphql.Result directly.
	out := map[string]interface{}{"data": result.Data}
	if len(result.Errors) > 0 {
		errItems := make([]map[string]interface{}, 0, len(result.Errors))
		for _, fe := range result.Errors {
			item := map[string]interface{}{"message": fe.Message}
			if len(fe.Locations) > 0 {
				item["locations"] = fe.Locations
			}
			if len(fe.Path) > 0 {
				item["path"] = fe.Path
			}
			if len(fe.Extensions) > 0 {
				item["extensions"] = fe.Extensions
			}
			if ghErrorIsNotFound(fe) {
				item["type"] = "NOT_FOUND"
			}
			errItems = append(errItems, item)
		}
		out["errors"] = errItems
	}
	writeJSON(w, http.StatusOK, out)
}

// graphqlValidateNoPanic parses and validates a GraphQL query against schema
// without letting graphql-go's panics escape. It returns an error only for
// malformed documents that would otherwise crash the library; syntactically
// invalid but safe documents are left to graphql.Do to report normally.
func graphqlValidateNoPanic(schema graphql.Schema, query string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("graphql validation panic: %v", r)
		}
	}()
	src := source.NewSource(&source.Source{Body: []byte(query), Name: "GraphQL request"})
	AST, parseErr := parser.Parse(parser.ParseParams{Source: src})
	if parseErr != nil {
		return fmt.Errorf("graphql parse error: %w", parseErr)
	}
	validationResult := graphql.ValidateDocument(&schema, AST, nil)
	if !validationResult.IsValid {
		// Validation errors are normal GraphQL failures; let graphql.Do return
		// them in its standard errors[] envelope.
		return nil
	}
	return nil
}

// userToGraphQL converts a User to a map with camelCase keys for GraphQL resolvers.
func userToGraphQL(u *User) map[string]interface{} {
	return map[string]interface{}{
		"nodeID":     u.NodeID,
		"databaseId": u.ID,
		"login":      u.Login,
		"name":       u.Name,
		"email":      u.Email,
		"avatarUrl":  u.AvatarURL,
		"bio":        u.Bio,
		"url":        "/" + u.Login,
		"createdAt":  u.CreatedAt.Format(time.RFC3339),
		"updatedAt":  u.UpdatedAt.Format(time.RFC3339),
	}
}
