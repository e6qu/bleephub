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

// ErrorIsNotFound unwraps graphql-go's error layering (FormattedError →
// *gqlerrors.Error → resolver error) looking for a ghNotFoundError.
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

// ghForbiddenError marks a permission denial that must surface as a
// GitHub-shaped errors[] entry carrying `"type": "FORBIDDEN"` — the sibling of
// ghNotFoundError. GitHub returns FORBIDDEN when the viewer can read the
// resource but lacks write/admin standing (or the app lacks the scope), which
// is distinct from the NOT_FOUND masking used when the resource can't be read
// at all. Clients discriminate on this `type` channel.
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

// initGraphQLSchema builds the GraphQL schema with all types and resolvers.
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
			// A git object's global id carries its type prefix before the
			// shared infix, so Commit/Tree/Blob/Tag dispatch off the same
			// codec the store encodes them with.
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
		// ProjectV2Owner has to be declared here rather than added when the
		// Projects v2 surface is assembled: graphql-go reads an object's
		// interface list once and memoizes it, so an interface a type does not
		// claim at construction can never gain it as a possible type.
		Interfaces: []*graphql.Interface{
			nodeInterface, actorInterface, repositoryOwnerInterface,
			s.projectV2OwnerInterfaceType(),
			// ProjectOwner (classic projects), for the same memoization reason.
			s.projectOwnerInterfaceType(),
			// Sponsorable, for the same memoization reason.
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

	// Create the shared reaction types (Reaction/ReactionConnection/Reactable)
	// before any concrete subject type so they can declare they implement
	// Reactable at config time. The interface's ResolveType reads the registry
	// lazily, so the concrete types it names may be built afterwards.
	s.initReactionGraphQLTypes(userType)

	// Add repository types, queries, and mutations
	repoType, mutationType, ownerRepositoriesField, ownerRepositoryField := s.addRepoFieldsToSchema(userType, queryType, nodeInterface)
	nodeTypes["Repository"] = repoType
	// The git object graph is assembled while the repository fields are, so
	// its types are registered for Node dispatch here.
	nodeTypes["Commit"] = s.graphqlTypes.commit
	nodeTypes["Tree"] = s.graphqlTypes.tree
	nodeTypes["Blob"] = s.graphqlTypes.blob
	nodeTypes["Tag"] = s.graphqlTypes.tag
	nodeTypes["Ref"] = s.graphqlTypes.ref

	// Add organization types and queries
	orgType := s.addOrgFieldsToSchema(userType, queryType, nodeInterface)
	orgType.AddFieldConfig("repositories", ownerRepositoriesField)
	orgType.AddFieldConfig("repository", ownerRepositoryField)
	nodeTypes["Organization"] = orgType
	repositoryOwnerTypes["Organization"] = orgType

	// Repository rulesets. They go after the organization family because a
	// ruleset's `source` is either the repository or the organization it is
	// inherited from, and the union needs both concrete types.
	s.addRulesetFieldsToSchema(repoType, orgType)

	// Add issue types, queries, and mutations
	issueType, milestoneType := s.addIssueFieldsToSchema(userType, repoType, mutationType, queryType, nodeInterface)
	nodeTypes["Issue"] = issueType

	// Linked branches hang off the issue type and need the shared Ref type the
	// repository family already built, so they go here rather than with the
	// git objects.
	s.addLinkedBranchFieldsToSchema(issueType, mutationType, nodeInterface, nodeTypes)

	// Add pull request types, queries, and mutations
	pullRequestType := s.addPullRequestFieldsToSchema(userType, issueType, milestoneType, repoType, mutationType, queryType, nodeInterface)
	nodeTypes["PullRequest"] = pullRequestType
	s.addNodeFieldsToSchema(queryType, nodeInterface)

	// Add discussion types, queries, and mutations
	s.addDiscussionFieldsToSchema(userType, repoType, mutationType)

	// Add moderation mutations (minimize/unminimize comment, lock/unlock).
	s.addModerationMutationsToSchema(mutationType)

	// Add the Labelable mutations (add/remove/clear labels). They go after the
	// issue and pull-request families because their payloads render whichever
	// of those two the subject turns out to be.
	s.addLabelMutationsToSchema(mutationType)
	s.addGitWriteMutationsToSchema(mutationType)
	s.addAdminMutationsToSchema(mutationType)
	s.addEMUMutationsToSchema(mutationType)

	// Add Projects v2. The read surface goes first: the mutations' payload
	// types are the same ProjectV2/ProjectV2Item objects, so they have to be
	// fully assembled before a payload references one.
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
	// Query.resource turns a pasted web URL into the node behind it, which is
	// how `gh project item-add --url` reaches an issue or pull request.
	s.addResourceFieldToSchema(queryType, nodeTypes)
	s.addProjectV2MutationsToSchema(mutationType)

	// The issue / pull-request timelines. They go here because the two unions
	// name types from the issue, pull-request, discussion and Projects v2
	// families, all of which are now fully assembled.
	s.addTimelineFieldsToSchema(nodeInterface, nodeTypes)

	// Complete GitHub's field surface on the conversation and metadata types
	// (Issue, Discussion, DiscussionComment, Milestone, Label). It runs last so
	// every type these fields name — ProjectV2, the timeline family,
	// IssueComment, Repository — is already assembled.
	s.enrichConversationTypes(userType, repoType)

	// Security advisories, Dependabot vulnerability alerts and the dependency
	// graph. This runs after the pull-request family because DependabotUpdate
	// names PullRequest, and after the repository family because four of its
	// types name Repository.
	s.addAdvisoryFieldsToSchema(userType, repoType, mutationType, queryType, nodeInterface, nodeTypes)

	// The enterprise account family. It goes after the organization types
	// because an enterprise's organizations, its policy-override connections
	// and its IP allow list owner union all name Organization.
	enterpriseType := s.addEnterpriseFieldsToSchema(userType, orgType, queryType, nodeInterface, actorInterface)
	nodeTypes["Enterprise"] = enterpriseType
	nodeTypes["EnterpriseUserAccount"] = s.graphqlTypes.enterpriseUserAccount
	nodeTypes["EnterpriseAdministratorInvitation"] = s.graphqlTypes.enterpriseAdminInvitation
	nodeTypes["EnterpriseMemberInvitation"] = s.graphqlTypes.enterpriseMemberInvite
	nodeTypes["EnterpriseIdentityProvider"] = s.graphqlTypes.enterpriseIdentityProvide
	nodeTypes["IpAllowListEntry"] = s.graphqlTypes.ipAllowListEntry
	actorTypes["EnterpriseUserAccount"] = s.graphqlTypes.enterpriseUserAccount
	s.addEnterpriseMutationsToSchema(mutationType)

	// Gists hang off the shared User type, which is what `viewer` resolves
	// to — the field `gh gist list` reads.
	s.addGistFieldsToSchema(userType)
	s.addReactionMutationsToSchema(mutationType)

	// GitHub Sponsors. It goes last among the read families because the
	// Sponsorable fields it installs on User and Organization name the
	// Repository type (featured items) and the mutation type, both of
	// which have to be fully assembled first.
	s.addSponsorsFieldsToSchema(userType, orgType, queryType, mutationType, nodeTypes)

	// GitHub Marketplace's read surface: the listing/category types and the
	// four root fields. It follows Sponsors because it reuses the connection
	// builder that family defines.
	s.addMarketplaceFieldsToSchema(queryType, nodeTypes)

	// Copilot's GraphQL surface is one field on User: the service endpoints
	// a Copilot client resolves.
	s.addCopilotFieldsToSchema(userType)

	// The GitHub Enterprise Importer. It goes after the organization and
	// enterprise families because Organization.repositoryMigrations hangs off
	// the one Organization type and startOrganizationMigration names an
	// Enterprise.
	s.addMigrationFieldsToSchema(orgType, mutationType, nodeInterface, nodeTypes)

	// Repository, User and Organization are the three types a client selects
	// most, and their remaining members name almost every type above: the
	// milestone and label objects, teams, rulesets, gists, issues, pull
	// requests and their connections. The account surface is therefore
	// installed last, once all of them are assembled.
	s.addAccountSurfaceFieldsToSchema(userType, orgType, repoType)

	// Projects classic (v1): the Project/ProjectColumn/ProjectCard family, the
	// ProjectOwner members on User/Organization/Repository, and the sixteen
	// classic-project mutations. It goes after the account surface because a
	// card's content names the fully-assembled Issue and PullRequest types.
	s.addProjectsClassicToSchema(userType, orgType, repoType, mutationType, nodeTypes)

	// Branch protection: the complete BranchProtectionRule object (whose
	// shell the Ref type already referenced), Repository.branchProtectionRules
	// and the three protection-rule mutations. It goes after the account
	// surface so the rule's repository/creator members name the finished
	// types.
	s.addBranchProtectionFieldsToSchema(repoType, mutationType)

	// Ruleset, custom-property and verifiable-domain writes. They go after
	// the enterprise family because a ruleset's source, a custom property's
	// schema and a domain's owner may each be enterprise-scoped.
	s.addRulesetMutationsToSchema(mutationType)
	s.addCustomPropertyMutationsToSchema(mutationType)
	s.addVerifiableDomainMutationsToSchema(mutationType)

	// The remainder of GitHub's mutation surface
	// (gh_mutations_*_graphql.go). It goes last because every payload it
	// registers returns an object one of the families above defines.
	s.addGitHubMutationSurface(mutationType)

	// The checks and deployments/environments mutation families. They follow
	// the account surface (beside the git-write and admin registrations
	// above they would be too early): their payloads name the CheckRun/
	// CheckSuite rollup types and the Deployment/Environment objects the
	// account surface assembles.
	s.addChecksMutationsToSchema(mutationType)
	s.addDeploymentsMutationsToSchema(mutationType)

	// The residual account/git/actions members GitHub's Commit, Release and
	// status-rollup types declare. This runs last so every type it hangs a
	// field off (Commit, Release, StatusContext, StatusCheckRollup, User,
	// Organization, Repository, Ref) is already assembled.
	s.addAccountActionsFields()

	// The shared Comment-trait, back-reference and viewer-permission fields on
	// CommitComment, IssueComment and PullRequestReviewComment. They are added
	// last because they name the repository, issue, pull-request, review and
	// commit types, all of which are now fully assembled. (GistComment is
	// enriched in place by gqlGistCommentType, which runs after those families.)
	s.enrichCommentTypes()

	// Every mutation is now registered. Authorization coverage is asserted over
	// the assembled type rather than trusted to each family above, so a
	// mutation that reaches the store without a policy row stops the process
	// here instead of shipping open to any signed-in account.
	assertMutationsAuthorized(mutationType)

	schema, err := graphql.NewSchema(graphql.SchemaConfig{
		Query:    queryType,
		Mutation: mutationType,
		// CommitComment is a Reactable subject not reachable from any root
		// field, so register it explicitly to include it (and its Reactable
		// possibleType membership) in the schema. Blob, Tree and Tag are
		// reachable only through the GitObject interface, whose implementors
		// graphql-go cannot discover on its own; without them a client's
		// `... on Blob` fragment fails validation as an unknown type.
		Types: []graphql.Type{
			s.graphqlTypes.commitComment,
			s.graphqlTypes.blob,
			s.graphqlTypes.tree,
			s.graphqlTypes.tag,
			s.graphqlTypes.commit,
		},
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
	// An enterprise, its invitations and its IP allow list entries are Node
	// implementors, so `node(id:)` resolves them. Each carries its own
	// visibility rule, which enterpriseNodeByID applies.
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

// githubCompatibleValidationErrors removes the one validation result where
// GitHub's production GraphQL service is deliberately more permissive than
// graphql-go: fields with the same response name and different leaf enum types
// are accepted when they live below mutually exclusive concrete-object
// fragments.
//
// The official gh client depends on this behavior. Its issue lookup selects
// `state: IssueState!` below `... on Issue` and `state: PullRequestState!`
// below `... on PullRequest` without aliases. GitHub accepts and executes that
// query because only one concrete branch can exist; graphql-go reports an
// OverlappingFieldsCanBeMerged error before execution.
//
// This is intentionally a narrow post-validation exception, not removal of the
// overlap rule. Different fields, arguments, nullability, or selections whose
// concrete branches can coincide keep their original validation errors.
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
// condition that makes it reachable. A location may have multiple contexts
// when a named fragment is spread more than once; the validation exception is
// safe only when every left/right pairing is disjoint.
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
						// Zero is a legal page size, not a malformed one: it asks for
						// the connection's metadata (totalCount, pageInfo) without any
						// nodes. GitHub accepts it and gh CLI relies on it — `gh project
						// create` selects the shared project fragment with
						// `items(first: 0)` and `fields(first: 0)` because it wants
						// neither, so rejecting zero refused the command outright.
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

// userToGraphQL converts a User to a map with camelCase keys for GraphQL resolvers.
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
