package graphqlapi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/location"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// ghNotFoundError surfaces as a GitHub-shaped errors[] entry carrying
// `"type": "NOT_FOUND"` — a non-standard, GitHub-specific key that gh CLI /
// go-gh discriminate on to tell "no such resource" from transport errors.
type ghNotFoundError struct {
	message string
}

func (e *ghNotFoundError) Error() string { return e.message }

// ErrorIsNotFound unwraps graphql-go's error layering looking for a
// ghNotFoundError.
func ErrorIsNotFound(err error) bool {
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

// ghForbiddenError surfaces as a GitHub-shaped errors[] entry carrying
// `"type": "FORBIDDEN"` — the sibling of ghNotFoundError, returned when the
// viewer can read the resource but lacks write/admin standing (distinct from
// NOT_FOUND masking used when it can't be read at all).
type ghForbiddenError struct {
	message string
}

func (e *ghForbiddenError) Error() string { return e.message }

// ErrorIsForbidden unwraps graphql-go's error layering looking for a
// ghForbiddenError (mirrors ErrorIsNotFound).
func ErrorIsForbidden(err error) bool {
	for err != nil {
		if _, ok := err.(*ghForbiddenError); ok {
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

// initGraphQLSchema builds the schema with all types and resolvers.
func (s *Resolver) initGraphQLSchema() {
	s.graphqlTypes = graphQLTypeRegistry{}
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	nodeTypes := map[string]*graphql.Object{}
	actorTypes := map[string]*graphql.Object{}
	repositoryOwnerTypes := map[string]*graphql.Object{}
	actorInterface := graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Actor",
		Fields: graphql.Fields{
			"login": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"avatarUrl": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int}},
			},
			"resourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name != "" {
				return actorTypes[name]
			}
			return actorTypes["User"]
		},
	})
	s.graphqlTypes.actor = actorInterface
	repositoryOwnerInterface := graphql.NewInterface(graphql.InterfaceConfig{
		Name: "RepositoryOwner",
		Fields: graphql.Fields{
			"avatarUrl": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int}},
			},
			"id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"login":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"resourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name != "" {
				return repositoryOwnerTypes[name]
			}
			nodeID, _ := source["nodeID"].(string)
			if strings.HasPrefix(nodeID, "O_") {
				return repositoryOwnerTypes["Organization"]
			}
			return repositoryOwnerTypes["User"]
		},
	})
	s.graphqlTypes.repositoryOwner = repositoryOwnerInterface
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
			case strings.HasPrefix(nodeID, "REF_"):
				return nodeTypes["Ref"]
			}
			// A git object's global id carries its type prefix, so dispatch
			// off the store's codec.
			if prefix, _, _, ok := store.ParseGitObjectNodeID(nodeID); ok {
				switch prefix {
				case store.GitCommitNodeIDPrefix:
					return nodeTypes["Commit"]
				case store.GitTreeNodeIDPrefix:
					return nodeTypes["Tree"]
				case store.GitBlobNodeIDPrefix:
					return nodeTypes["Blob"]
				case store.GitTagNodeIDPrefix:
					return nodeTypes["Tag"]
				case store.LinkedBranchNodeIDPrefix:
					return nodeTypes["LinkedBranch"]
				}
			}
			return nil
		},
	})
	s.graphqlTypes.node = nodeInterface
	userType := graphql.NewObject(graphql.ObjectConfig{
		Name: "User",
		// graphql-go memoizes an object's interface list at construction, so
		// every interface User implements must be declared here — an interface
		// not claimed now can never be added later.
		Interfaces: []*graphql.Interface{
			nodeInterface, actorInterface, repositoryOwnerInterface,
			s.projectV2OwnerInterfaceType(),
			s.projectOwnerInterfaceType(),
			s.sponsorableInterfaceType(),
		},
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
			"avatarUrl": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int}},
			},
			"bio":          &graphql.Field{Type: graphql.String},
			"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
			"resourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"createdAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		},
	})
	nodeTypes["User"] = userType
	actorTypes["User"] = userType
	repositoryOwnerTypes["User"] = userType
	s.graphqlTypes.user = userType

	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"viewer": &graphql.Field{
				Type: graphql.NewNonNull(userType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					user := s.ghUserFromContext(p.Context)
					if user == nil {
						return nil, fmt.Errorf("authentication required")
					}
					return userToGraphQL(user), nil
				},
			},
			// `gh org list` resolves a target user's organizations through
			// this root field rather than viewer.
			"user": &graphql.Field{
				Type: userType,
				Args: graphql.FieldConfigArgument{
					"login": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					login, _ := p.Args["login"].(string)
					u := s.store.LookupUserByLogin(login)
					if u == nil {
						return nil, &ghNotFoundError{
							message: fmt.Sprintf("Could not resolve to a User with the login of '%s'.", login),
						}
					}
					return userToGraphQL(u), nil
				},
			},
		},
	})
	s.addMetaFieldsToSchema(queryType)

	// Build the shared reaction types before any concrete subject type so
	// those can declare they implement Reactable at config time.
	s.initReactionGraphQLTypes(userType)

	repoType, mutationType, ownerRepositoriesField, ownerRepositoryField := s.addRepoFieldsToSchema(userType, queryType, nodeInterface)
	nodeTypes["Repository"] = repoType
	// The git object graph is assembled with the repository fields; register
	// its types for Node dispatch.
	nodeTypes["Commit"] = s.graphqlTypes.commit
	nodeTypes["Tree"] = s.graphqlTypes.tree
	nodeTypes["Blob"] = s.graphqlTypes.blob
	nodeTypes["Tag"] = s.graphqlTypes.tag
	nodeTypes["Ref"] = s.graphqlTypes.ref

	orgType := s.addOrgFieldsToSchema(userType, queryType, nodeInterface)
	orgType.AddFieldConfig("repositories", ownerRepositoriesField)
	orgType.AddFieldConfig("repository", ownerRepositoryField)
	nodeTypes["Organization"] = orgType
	repositoryOwnerTypes["Organization"] = orgType

	// Rulesets go after the org family: a ruleset's `source` union needs both
	// the repository and organization concrete types.
	s.addRulesetFieldsToSchema(repoType, orgType)

	issueType, milestoneType := s.addIssueFieldsToSchema(userType, repoType, mutationType, queryType, nodeInterface)
	nodeTypes["Issue"] = issueType

	// Linked branches need the shared Ref type the repository family built.
	s.addLinkedBranchFieldsToSchema(issueType, mutationType, nodeInterface, nodeTypes)

	pullRequestType := s.addPullRequestFieldsToSchema(userType, issueType, milestoneType, repoType, mutationType, queryType, nodeInterface)
	nodeTypes["PullRequest"] = pullRequestType
	s.addNodeFieldsToSchema(queryType, nodeInterface)

	s.addDiscussionFieldsToSchema(userType, repoType, mutationType)
	s.addModerationMutationsToSchema(mutationType)

	// Labelable mutations go after the issue and PR families because their
	// payloads render whichever the subject turns out to be.
	s.addLabelMutationsToSchema(mutationType)
	s.addGitWriteMutationsToSchema(mutationType)
	s.addAdminMutationsToSchema(mutationType)
	s.addEMUMutationsToSchema(mutationType)

	// Projects v2 read surface first: the mutation payloads reuse these same
	// ProjectV2/ProjectV2Item objects and must reference finished ones.
	s.enrichProjectV2Types(repoType, nodeTypes)
	s.addProjectV2OwnerFields(orgType, userType, repoType)
	nodeTypes["ProjectV2"] = s.graphqlTypes.projectV2Type
	nodeTypes["ProjectV2Item"] = s.graphqlTypes.projectV2ItemTypeMemo
	nodeTypes["ProjectV2View"] = s.graphqlTypes.projectV2ViewTypeMemo
	nodeTypes["ProjectV2Field"] = s.graphqlTypes.projectV2FieldTypeMemo
	nodeTypes["ProjectV2SingleSelectField"] = s.graphqlTypes.projectV2SingleSelectFieldMemo
	nodeTypes["ProjectV2IterationField"] = s.graphqlTypes.projectV2IterationFieldMemo
	nodeTypes["ProjectV2StatusUpdate"] = s.graphqlTypes.projectV2StatusUpdateType
	nodeTypes["ProjectV2Workflow"] = s.graphqlTypes.projectV2WorkflowType
	// Query.resource turns a pasted web URL into its node — how `gh project
	// item-add --url` reaches an issue or pull request.
	s.addResourceFieldToSchema(queryType, nodeTypes)
	s.addProjectV2MutationsToSchema(mutationType)

	// Timelines go here: the two unions name issue, PR, discussion and
	// Projects v2 types, all now assembled.
	s.addTimelineFieldsToSchema(nodeInterface, nodeTypes)

	// Conversation/metadata fields run last so every type they name (ProjectV2,
	// timeline, IssueComment, Repository) is assembled.
	s.enrichConversationTypes(userType, repoType)

	// Advisories/Dependabot: after the PR family (DependabotUpdate names
	// PullRequest) and repository family (four types name Repository).
	s.addAdvisoryFieldsToSchema(userType, repoType, mutationType, queryType, nodeInterface, nodeTypes)

	// Enterprise family: after the org types, whose Organization its
	// organizations, policy-override connections and IP-allow-list union name.
	enterpriseType := s.addEnterpriseFieldsToSchema(userType, orgType, queryType, nodeInterface, actorInterface)
	nodeTypes["Enterprise"] = enterpriseType
	nodeTypes["EnterpriseUserAccount"] = s.graphqlTypes.enterpriseUserAccount
	nodeTypes["EnterpriseAdministratorInvitation"] = s.graphqlTypes.enterpriseAdminInvitation
	nodeTypes["EnterpriseMemberInvitation"] = s.graphqlTypes.enterpriseMemberInvite
	nodeTypes["EnterpriseIdentityProvider"] = s.graphqlTypes.enterpriseIdentityProvide
	nodeTypes["IpAllowListEntry"] = s.graphqlTypes.ipAllowListEntry
	actorTypes["EnterpriseUserAccount"] = s.graphqlTypes.enterpriseUserAccount
	s.addEnterpriseMutationsToSchema(mutationType)

	// Gists hang off the shared User type that `viewer` resolves to.
	s.addGistFieldsToSchema(userType)
	s.addReactionMutationsToSchema(mutationType)

	// Sponsors last among read families: its Sponsorable fields name Repository
	// (featured items) and the mutation type, which must be assembled first.
	s.addSponsorsFieldsToSchema(userType, orgType, queryType, mutationType, nodeTypes)

	// Marketplace follows Sponsors, reusing that family's connection builder.
	s.addMarketplaceFieldsToSchema(queryType, nodeTypes)

	s.addCopilotFieldsToSchema(userType)

	// Migrations: after the org and enterprise families, whose Organization and
	// Enterprise types Organization.repositoryMigrations and
	// startOrganizationMigration name.
	s.addMigrationFieldsToSchema(orgType, mutationType, nodeInterface, nodeTypes)

	// The account surface's remaining members on Repository/User/Organization
	// name almost every type above, so it is installed once all are assembled.
	s.addAccountSurfaceFieldsToSchema(userType, orgType, repoType)

	// Query.topic needs the queryType handed to this builder.
	s.addQueryTopicField(queryType)

	// Projects classic: after the account surface, since a card's content names
	// the assembled Issue and PullRequest types.
	s.addProjectsClassicToSchema(userType, orgType, repoType, mutationType, nodeTypes)

	// Branch protection: after the account surface so the rule's
	// repository/creator members name finished types.
	s.addBranchProtectionFieldsToSchema(repoType, mutationType)

	// Ruleset/custom-property/domain writes: after the enterprise family, since
	// each subject may be enterprise-scoped.
	s.addRulesetMutationsToSchema(mutationType)
	s.addCustomPropertyMutationsToSchema(mutationType)
	s.addVerifiableDomainMutationsToSchema(mutationType)

	// The mutation-surface remainder goes last: every payload it registers
	// returns an object one of the families above defines.
	s.addGitHubMutationSurface(mutationType)

	// Checks and deployment mutations: their payloads name the CheckRun/
	// CheckSuite and Deployment/Environment types the account surface builds.
	s.addChecksMutationsToSchema(mutationType)
	s.addDeploymentsMutationsToSchema(mutationType)

	// Commit.deployments and Repository.pinnedEnvironments name types the two
	// families above just assembled.
	s.addLateGitResidualFields()

	// Residual Commit/Release/status-rollup members, after every type they
	// hang off is assembled.
	s.addAccountActionsFields()

	// Actions run graph and residual Checks members, after every type they name
	// is assembled.
	s.addActionsFamilyFields()

	// Shared Comment-trait/back-reference/viewer-permission fields on
	// CommitComment, IssueComment and PullRequestReviewComment, added last since
	// they name the now-assembled repository/issue/PR/review/commit types.
	s.enrichCommentTypes()

	// The tail of GitHub fields whose subject bleephub does not model (ruleset
	// conditions, bypass-actor union, permissionSources, pinnedDiscussions).
	// Runs last — its types are now assembled and every field resolves a
	// truthful null/empty.
	s.addResidueTailFields()

	// Assert authorization coverage over the fully-assembled type, so a mutation
	// reaching the store without a policy row halts startup rather than shipping
	// open to any signed-in account.
	assertMutationsAuthorized(mutationType)

	s.addSchemaFidelityShells()

	// Schema-fidelity shells for data this instance does not produce, each
	// registered via registerExtraSchemaType.
	schemaTypes := append([]graphql.Type{
		s.graphqlTypes.commitComment,
		s.graphqlTypes.blob,
		s.graphqlTypes.tree,
		s.graphqlTypes.tag,
		s.graphqlTypes.commit,
		// GitSignature members are reachable only via Commit.signature's
		// interface; register them so `... on GpgSignature` fragments validate.
		s.namedObject("GpgSignature"),
		s.namedObject("SshSignature"),
		s.namedObject("SmimeSignature"),
		s.namedObject("UnknownSignature"),
		// The six agent-triage events are reachable only via the
		// IssueEventWithRationale union; register them so their fragments validate.
		s.namedObject("IssueFieldAddedEvent"),
		s.namedObject("IssueFieldChangedEvent"),
		s.namedObject("IssueFieldRemovedEvent"),
		s.namedObject("IssueTypeAddedEvent"),
		s.namedObject("IssueTypeChangedEvent"),
		s.namedObject("IssueTypeRemovedEvent"),
	}, s.extraSchemaTypes...)

	schema, err := graphql.NewSchema(graphql.SchemaConfig{
		Query:    queryType,
		Mutation: mutationType,
		Types:    schemaTypes,
	})
	if err != nil {
		panic(fmt.Sprintf("failed to create graphql schema: %v", err))
	}
	s.graphqlSchema = schema
}

func (s *Resolver) addNodeFieldsToSchema(queryType *graphql.Object, nodeInterface *graphql.Interface) {
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

func (s *Resolver) graphQLNodeByID(ctx context.Context, nodeID string) interface{} {
	if user := store.FindUserByNodeID(s.store, nodeID); user != nil {
		return userToGraphQL(user)
	}
	// enterpriseNodeByID resolves enterprise/invitation/IP-allow-list nodes and
	// applies each one's visibility rule.
	if node := s.enterpriseNodeByID(ctx, nodeID); node != nil {
		return node
	}
	s.store.Mu.RLock()
	var organization *store.Org
	for _, candidate := range s.store.Orgs {
		if candidate.NodeID == nodeID {
			copy := *candidate
			organization = &copy
			break
		}
	}
	s.store.Mu.RUnlock()
	if organization != nil {
		return orgToGraphQL(organization)
	}
	if repo := store.FindRepoByNodeID(s.store, nodeID); repo != nil {
		if repo.Private && !s.viewerCanReadRepo(ctx, repo) {
			return nil
		}
		return repoToGraphQL(s.store, s.store.SnapRepo(repo))
	}
	if issue := store.FindIssueByNodeID(s.store, nodeID); issue != nil {
		repo := s.store.GetRepoByID(issue.RepoID)
		if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
			return nil
		}
		return issueToGQL(issue, s.store)
	}
	if pullRequest := store.FindPullRequestByNodeID(s.store, nodeID); pullRequest != nil {
		repo := s.store.GetRepoByID(pullRequest.RepoID)
		if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
			return nil
		}
		return pullRequestToGQL(pullRequest, s.store)
	}
	if issue, link, ok := store.FindIssueByLinkedBranchNodeID(s.store, nodeID); ok {
		repo := s.store.GetRepoByID(issue.RepoID)
		if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
			return nil
		}
		return s.linkedBranchSource(issue.ID, link)
	}
	if gitObject := s.gitObjectNodeByID(ctx, nodeID); gitObject != nil {
		return gitObject
	}
	if classicNode := s.projectClassicNodeByID(ctx, nodeID); classicNode != nil {
		return classicNode
	}
	if advisoryNode := s.advisoryNodeByID(ctx, nodeID); advisoryNode != nil {
		return advisoryNode
	}
	if sponsorsNode := s.sponsorsNodeByID(nodeID); sponsorsNode != nil {
		return sponsorsNode
	}
	if marketplaceNode := s.marketplaceNodeByID(ctx, nodeID); marketplaceNode != nil {
		return marketplaceNode
	}
	if migrationNode := s.migrationNodeByID(ctx, nodeID); migrationNode != nil {
		return migrationNode
	}
	return nil
}

func PrepareDocument(schema graphql.Schema, query string) (document *ast.Document, validationErrors []gqlerrors.FormattedError, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("graphql validation panic: %v", r)
		}
	}()
	src := source.NewSource(&source.Source{Body: []byte(query), Name: "GraphQL request"})
	var parseErr error
	document, parseErr = parser.Parse(parser.ParseParams{Source: src})
	if parseErr != nil {
		return nil, nil, fmt.Errorf("graphql parse error: %w", parseErr)
	}
	validationResult := graphql.ValidateDocument(&schema, document, nil)
	if !validationResult.IsValid {
		return document, githubCompatibleValidationErrors(schema, document, validationResult.Errors), nil
	}
	return document, nil, nil
}

// githubCompatibleValidationErrors drops the one overlap error GitHub is
// deliberately permissive about: same-response-name fields with different leaf
// enum types below mutually exclusive concrete-object fragments (gh's issue
// lookup selects state below `... on Issue` and `... on PullRequest` unaliased;
// graphql-go rejects it, GitHub executes it). A narrow post-validation
// exception — branches that can coincide keep their errors.
func githubCompatibleValidationErrors(schema graphql.Schema, document *ast.Document, errors []gqlerrors.FormattedError) []gqlerrors.FormattedError {
	if len(errors) == 0 {
		return nil
	}
	contexts := concreteFieldContexts(schema, document)
	filtered := make([]gqlerrors.FormattedError, 0, len(errors))
	for _, validationError := range errors {
		if !isExclusiveLeafTypeConflict(validationError, contexts) {
			filtered = append(filtered, validationError)
		}
	}
	return filtered
}

type graphqlSourcePosition struct {
	line   int
	column int
}

func isExclusiveLeafTypeConflict(validationError gqlerrors.FormattedError, contexts map[graphqlSourcePosition]map[string]struct{}) bool {
	if len(validationError.Locations) != 2 ||
		!strings.HasPrefix(validationError.Message, `Fields "`) ||
		!strings.Contains(validationError.Message, "conflict because they return conflicting types ") {
		return false
	}
	left := contexts[graphqlSourcePosition{
		line: validationError.Locations[0].Line, column: validationError.Locations[0].Column,
	}]
	right := contexts[graphqlSourcePosition{
		line: validationError.Locations[1].Line, column: validationError.Locations[1].Column,
	}]
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	for leftType := range left {
		for rightType := range right {
			if leftType == rightType {
				return false
			}
		}
	}
	return true
}

// concreteFieldContexts maps each selected field to the concrete object type
// condition that makes it reachable. A location has multiple contexts when a
// fragment is spread more than once; the exception is safe only when every
// left/right pairing is disjoint.
func concreteFieldContexts(schema graphql.Schema, document *ast.Document) map[graphqlSourcePosition]map[string]struct{} {
	fragments := map[string]*ast.FragmentDefinition{}
	for _, definition := range document.Definitions {
		if fragment, ok := definition.(*ast.FragmentDefinition); ok && fragment.Name != nil {
			fragments[fragment.Name.Value] = fragment
		}
	}

	result := map[graphqlSourcePosition]map[string]struct{}{}
	var walk func(*ast.SelectionSet, string, map[string]bool)
	walk = func(selectionSet *ast.SelectionSet, concrete string, fragmentStack map[string]bool) {
		if selectionSet == nil {
			return
		}
		for _, selection := range selectionSet.Selections {
			switch node := selection.(type) {
			case *ast.Field:
				if concrete != "" && node.Loc != nil {
					position := location.GetLocation(node.Loc.Source, node.Loc.Start)
					key := graphqlSourcePosition{line: position.Line, column: position.Column}
					if result[key] == nil {
						result[key] = map[string]struct{}{}
					}
					result[key][concrete] = struct{}{}
				}
				walk(node.SelectionSet, concrete, fragmentStack)
			case *ast.InlineFragment:
				next := concreteObjectCondition(schema, node.TypeCondition, concrete)
				walk(node.SelectionSet, next, fragmentStack)
			case *ast.FragmentSpread:
				if node.Name == nil || fragmentStack[node.Name.Value] {
					continue
				}
				fragment := fragments[node.Name.Value]
				if fragment == nil {
					continue
				}
				nextStack := make(map[string]bool, len(fragmentStack)+1)
				for name, active := range fragmentStack {
					nextStack[name] = active
				}
				nextStack[node.Name.Value] = true
				next := concreteObjectCondition(schema, fragment.TypeCondition, concrete)
				walk(fragment.SelectionSet, next, nextStack)
			}
		}
	}
	for _, definition := range document.Definitions {
		if operation, ok := definition.(*ast.OperationDefinition); ok {
			walk(operation.SelectionSet, "", map[string]bool{})
		}
	}
	return result
}

func concreteObjectCondition(schema graphql.Schema, condition *ast.Named, fallback string) string {
	if condition == nil || condition.Name == nil {
		return fallback
	}
	if _, ok := schema.Type(condition.Name.Value).(*graphql.Object); ok {
		return condition.Name.Value
	}
	return fallback
}

func CheckDocumentLimits(document *ast.Document, variables map[string]interface{}, maxDepth, maxFields int) error {
	fragments := map[string]*ast.FragmentDefinition{}
	for _, definition := range document.Definitions {
		if fragment, ok := definition.(*ast.FragmentDefinition); ok && fragment.Name != nil {
			fragments[fragment.Name.Value] = fragment
		}
	}
	fields := 0
	var walk func(*ast.SelectionSet, int, map[string]bool) error
	walk = func(selectionSet *ast.SelectionSet, depth int, stack map[string]bool) error {
		if selectionSet == nil {
			return nil
		}
		if depth > maxDepth {
			return fmt.Errorf("query exceeds maximum depth of %d", maxDepth)
		}
		for _, selection := range selectionSet.Selections {
			switch node := selection.(type) {
			case *ast.Field:
				arguments := map[string]ast.Value{}
				for _, argument := range node.Arguments {
					if argument != nil && argument.Name != nil {
						arguments[argument.Name.Value] = argument.Value
					}
				}
				if arguments["first"] != nil && arguments["last"] != nil &&
					!graphqlVariableIsNull(arguments["first"], variables) &&
					!graphqlVariableIsNull(arguments["last"], variables) {
					return fmt.Errorf("fields may not specify both first and last")
				}
				for _, name := range []string{"first", "last"} {
					if value := arguments[name]; value != nil {
						if graphqlVariableIsNull(value, variables) {
							continue
						}
						size, ok := graphqlIntegerValue(value, variables)
						// Zero is legal: it asks for connection metadata without
						// nodes, and gh relies on it (`gh project create` selects
						// `items(first: 0)`). Rejecting zero refused the command.
						if !ok || size < 0 || size > 100 {
							return fmt.Errorf("%s must be between 0 and 100", name)
						}
					}
				}
				for _, name := range []string{"after", "before"} {
					if value := arguments[name]; value != nil {
						if graphqlVariableIsNull(value, variables) {
							continue
						}
						cursor, ok := graphqlStringValue(value, variables)
						if !ok {
							return fmt.Errorf("%s must be a cursor string", name)
						}
						if _, err := decodeCursorStrict(cursor); err != nil {
							return fmt.Errorf("%s is not a valid cursor", name)
						}
					}
				}
				fields++
				if fields > maxFields {
					return fmt.Errorf("query exceeds maximum field count of %d", maxFields)
				}
				if err := walk(node.SelectionSet, depth+1, stack); err != nil {
					return err
				}
			case *ast.InlineFragment:
				if err := walk(node.SelectionSet, depth, stack); err != nil {
					return err
				}
			case *ast.FragmentSpread:
				if node.Name == nil || stack[node.Name.Value] {
					continue
				}
				fragment := fragments[node.Name.Value]
				if fragment == nil {
					continue
				}
				next := make(map[string]bool, len(stack)+1)
				for name := range stack {
					next[name] = true
				}
				next[node.Name.Value] = true
				if err := walk(fragment.SelectionSet, depth, next); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, definition := range document.Definitions {
		if operation, ok := definition.(*ast.OperationDefinition); ok {
			if err := walk(operation.SelectionSet, 1, map[string]bool{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func graphqlVariableIsNull(value ast.Value, variables map[string]interface{}) bool {
	variable, ok := value.(*ast.Variable)
	if !ok || variable.Name == nil {
		return false
	}
	raw, present := variables[variable.Name.Value]
	return !present || raw == nil
}

func graphqlIntegerValue(value ast.Value, variables map[string]interface{}) (int, bool) {
	switch value := value.(type) {
	case *ast.IntValue:
		number, err := strconv.Atoi(value.Value)
		return number, err == nil
	case *ast.Variable:
		if value.Name == nil {
			return 0, false
		}
		return intArg(variables, value.Name.Value)
	default:
		return 0, false
	}
}

func graphqlStringValue(value ast.Value, variables map[string]interface{}) (string, bool) {
	switch value := value.(type) {
	case *ast.StringValue:
		return value.Value, true
	case *ast.Variable:
		if value.Name == nil {
			return "", false
		}
		text, ok := variables[value.Name.Value].(string)
		return text, ok
	default:
		return "", false
	}
}

// userToGraphQL converts a User to a camelCase source map.
func userToGraphQL(u *store.User) map[string]interface{} {
	return map[string]interface{}{
		"nodeID":       u.NodeID,
		"databaseId":   u.ID,
		"login":        u.Login,
		"name":         u.Name,
		"email":        u.Email,
		"avatarUrl":    u.AvatarURL,
		"bio":          u.Bio,
		"url":          externalURL("/" + u.Login),
		"resourcePath": "/" + u.Login,
		"createdAt":    u.CreatedAt.Format(time.RFC3339),
		"updatedAt":    u.UpdatedAt.Format(time.RFC3339),
	}
}
