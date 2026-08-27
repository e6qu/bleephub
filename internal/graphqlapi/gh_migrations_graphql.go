package graphqlapi

// The GitHub Enterprise Importer's GraphQL surface: the Migration interface and
// its two implementors, the migration source, the organization's migration
// connection, and the seven mutations over them.
//
// Every authz row resolves the owning organization from the input and checks
// migrator standing on *that* organization, giving cross-tenant isolation;
// startOrganizationMigration instead demands ownership of the target
// enterprise. Organization.repositoryMigrations refuses a viewer with no
// standing rather than answering an empty connection, so "none" and "may not
// look" stay distinguishable.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// --- authorization rules ---------------------------------------------------

// migrationOrgRule requires the viewer to be an owner of, or a granted
// migrator on, the organization the input names through orgKey.
type migrationOrgRule struct {
	orgKey string
}

func (r migrationOrgRule) check() error {
	if r.orgKey == "" {
		return fmt.Errorf("no organization id input key")
	}
	return nil
}

func (r migrationOrgRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.orgKey].(string)
	org := s.orgByNodeID(nodeID)
	if org == nil {
		return gqlMissingNode("Organization", nodeID)
	}
	return s.requireMigratorStanding(p.Context, org)
}

// migrationSubjectRule is the policy for a mutation naming a migration rather
// than its organization; standing is resolved from the migration's owning
// organization.
type migrationSubjectRule struct {
	idKey string
}

func (r migrationSubjectRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no migration id input key")
	}
	return nil
}

func (r migrationSubjectRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.idKey].(string)
	migration := store.FindRepositoryMigrationByNodeID(s.store, nodeID)
	if migration == nil {
		return gqlMissingNode("RepositoryMigration", nodeID)
	}
	org := s.store.GetOrgByID(migration.OwnerOrgID)
	if org == nil {
		return gqlMissingNode("RepositoryMigration", nodeID)
	}
	return s.requireMigratorStanding(p.Context, org)
}

// migrationEnterpriseRule requires the viewer to own the named enterprise (an
// org migration creates an org inside it). A distinct type from the enterprise
// family's own rule so the two tables stay independently pinned.
type migrationEnterpriseRule struct {
	idKey string
}

func (r migrationEnterpriseRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no enterprise id input key")
	}
	return nil
}

func (r migrationEnterpriseRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.idKey].(string)
	e := store.FindEnterpriseByNodeID(s.store, nodeID)
	if e == nil {
		return gqlMissingNode("Enterprise", nodeID)
	}
	if !s.store.IsEnterpriseOwner(e.ID, s.ghUserFromContext(p.Context)) {
		return &ghForbiddenError{message: "You must be an owner of the enterprise to perform this action."}
	}
	return nil
}

// requireMigratorStanding gates seeing or moving an organization's migrations.
func (s *Resolver) requireMigratorStanding(ctx context.Context, org *store.Org) error {
	if !s.viewerMayMigrateOrg(ctx, org) {
		return &ghForbiddenError{
			message: "You must be an organization owner or have been granted the migrator role to perform this action.",
		}
	}
	return nil
}

// migrationMutationAuthzRows is the migration family's slice of the mutation
// policy table.
func migrationMutationAuthzRows() map[string]mutationRule {
	return map[string]mutationRule{
		"createMigrationSource":      migrationOrgRule{orgKey: "ownerId"},
		"startRepositoryMigration":   migrationOrgRule{orgKey: "ownerId"},
		"abortQueuedMigrations":      migrationOrgRule{orgKey: "ownerId"},
		"grantMigratorRole":          migrationOrgRule{orgKey: "organizationId"},
		"revokeMigratorRole":         migrationOrgRule{orgKey: "organizationId"},
		"abortRepositoryMigration":   migrationSubjectRule{idKey: "migrationId"},
		"startOrganizationMigration": migrationEnterpriseRule{idKey: "targetEnterpriseId"},
	}
}

func init() {
	for name, rule := range migrationMutationAuthzRows() {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic("graphql mutation " + name + " already has an authorization row")
		}
		graphqlMutationAuthz[name] = rule
	}
}

// --- enums -----------------------------------------------------------------

func (s *Resolver) migrationSourceTypeEnum() *graphql.Enum {
	return s.sharedEnum("MigrationSourceType",
		store.MigrationSourceTypeAzureDevOps, store.MigrationSourceTypeBitbucketServer,
		store.MigrationSourceTypeGitHubArchive, store.MigrationSourceTypeGitLab)
}

func (s *Resolver) migrationStateEnum() *graphql.Enum {
	return s.sharedEnum("MigrationState",
		store.GEIMigrationStateFailed, store.GEIMigrationStateFailedValidation,
		store.GEIMigrationStateInProgress, store.GEIMigrationStateNotStarted,
		store.GEIMigrationStatePendingValidation, store.GEIMigrationStateQueued,
		store.GEIMigrationStateSucceeded)
}

func (s *Resolver) organizationMigrationStateEnum() *graphql.Enum {
	return s.sharedEnum("OrganizationMigrationState",
		store.GEIMigrationStateFailed, store.GEIMigrationStateFailedValidation,
		store.GEIMigrationStateInProgress, store.GEIMigrationStateNotStarted,
		store.GEIMigrationStatePendingValidation,
		store.OrgMigrationStatePostRepoMigration, store.OrgMigrationStatePreRepoMigration,
		store.GEIMigrationStateQueued, store.OrgMigrationStateRepoMigration,
		store.GEIMigrationStateSucceeded)
}

func (s *Resolver) actorTypeEnum() *graphql.Enum {
	return s.sharedEnum("ActorType", "TEAM", "USER")
}

// --- projections ------------------------------------------------------------

// migrationSourceToGQL renders a source. Credentials are deliberately omitted:
// no MigrationSource field serves them, so a migrator cannot read back the
// configured token.
func migrationSourceToGQL(src *store.MigrationSource) map[string]interface{} {
	if src == nil {
		return nil
	}
	return map[string]interface{}{
		"__typename": "MigrationSource",
		"nodeID":     src.NodeID,
		"name":       src.Name,
		"type":       src.Type,
		"url":        src.URL,
		"createdAt":  src.CreatedAt.Format(time.RFC3339),
	}
}

func (s *Resolver) repositoryMigrationToGQL(m *store.RepositoryMigration) map[string]interface{} {
	if m == nil {
		return nil
	}
	return map[string]interface{}{
		"__typename":      "RepositoryMigration",
		"nodeID":          m.NodeID,
		"databaseId":      strconv.Itoa(m.ID),
		"continueOnError": m.ContinueOnError,
		"createdAt":       m.CreatedAt.Format(time.RFC3339),
		"failureReason":   nilOrGQLString(m.FailureReason),
		"migrationLogUrl": nilOrGQLString(s.repositoryMigrationLogURL(m)),
		"migrationSource": optionalObject(migrationSourceToGQL(s.store.GetMigrationSource(m.SourceID))),
		"repositoryName":  m.RepositoryName,
		"sourceUrl":       m.SourceURL,
		"state":           m.State,
		"warningsCount":   m.WarningsCount,
	}
}

func organizationMigrationToGQL(m *store.OrganizationMigration) map[string]interface{} {
	if m == nil {
		return nil
	}
	return map[string]interface{}{
		"__typename":                 "OrganizationMigration",
		"nodeID":                     m.NodeID,
		"databaseId":                 strconv.Itoa(m.ID),
		"createdAt":                  m.CreatedAt.Format(time.RFC3339),
		"failureReason":              nilOrGQLString(m.FailureReason),
		"remainingRepositoriesCount": nilOrGQLInt(m.RemainingRepositoriesCount),
		"sourceOrgName":              m.SourceOrgName,
		"sourceOrgUrl":               m.SourceOrgURL,
		"state":                      m.State,
		"targetOrgName":              m.TargetOrgName,
		"totalRepositoriesCount":     nilOrGQLInt(m.TotalRepositoriesCount),
	}
}

// nilOrGQLString renders "" as GraphQL null.
func nilOrGQLString(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}

func nilOrGQLInt(value *int) interface{} {
	if value == nil {
		return nil
	}
	return *value
}

// --- schema ----------------------------------------------------------------

// addMigrationFieldsToSchema installs the migration types, the organization's
// migration connection and the mutations. Runs after the organization and
// enterprise families, which its fields hang off.
func (s *Resolver) addMigrationFieldsToSchema(orgType, mutationType *graphql.Object, nodeInterface *graphql.Interface, nodeTypes map[string]*graphql.Object) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")

	sourceType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "MigrationSource",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":   gqlNodeIDField(),
			"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"type": &graphql.Field{Type: graphql.NewNonNull(s.migrationSourceTypeEnum())},
			"url":  &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
	})
	nodeTypes["MigrationSource"] = sourceType

	// Build the interface and RepositoryMigration from one field map so
	// graphql-go's field-by-field implementor check is provably satisfied.
	migrationFields := func() graphql.Fields {
		return graphql.Fields{
			"continueOnError": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"createdAt":       &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"databaseId":      &graphql.Field{Type: graphql.String},
			"failureReason":   &graphql.Field{Type: graphql.String},
			"id":              gqlNodeIDField(),
			"migrationLogUrl": &graphql.Field{Type: uri},
			"migrationSource": &graphql.Field{Type: graphql.NewNonNull(sourceType)},
			"repositoryName":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"sourceUrl":       &graphql.Field{Type: graphql.NewNonNull(uri)},
			"state":           &graphql.Field{Type: graphql.NewNonNull(s.migrationStateEnum())},
			"warningsCount":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		}
	}
	migrationInterface := graphql.NewInterface(graphql.InterfaceConfig{
		Name:   "Migration",
		Fields: migrationFields(),
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			return nodeTypes["RepositoryMigration"]
		},
	})
	repositoryMigrationType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "RepositoryMigration",
		Interfaces: []*graphql.Interface{migrationInterface, nodeInterface},
		Fields:     migrationFields(),
	})
	nodeTypes["RepositoryMigration"] = repositoryMigrationType

	orgMigrationType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "OrganizationMigration",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"createdAt":                  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"databaseId":                 &graphql.Field{Type: graphql.String},
			"failureReason":              &graphql.Field{Type: graphql.String},
			"id":                         gqlNodeIDField(),
			"remainingRepositoriesCount": &graphql.Field{Type: graphql.Int},
			"sourceOrgName":              &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"sourceOrgUrl":               &graphql.Field{Type: graphql.NewNonNull(uri)},
			"state":                      &graphql.Field{Type: graphql.NewNonNull(s.organizationMigrationStateEnum())},
			"targetOrgName":              &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"totalRepositoriesCount":     &graphql.Field{Type: graphql.Int},
		},
	})
	nodeTypes["OrganizationMigration"] = orgMigrationType

	edgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryMigrationEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: repositoryMigrationType},
		},
	})
	connectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryMigrationConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(edgeType)},
			"nodes":      &graphql.Field{Type: graphql.NewList(repositoryMigrationType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	orderFieldEnum := s.sharedEnum("RepositoryMigrationOrderField", "CREATED_AT")
	orderDirectionEnum := s.sharedEnum("RepositoryMigrationOrderDirection", "ASC", "DESC")
	orderInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RepositoryMigrationOrder",
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(orderDirectionEnum)},
			"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(orderFieldEnum)},
		},
	})

	s.addOrganizationRepositoryMigrations(orgType, connectionType, orderInput)
	s.addMigrationMutations(mutationType, sourceType, repositoryMigrationType, orgMigrationType)
}

// gqlNodeIDField is the `id: ID!` served off the projection's nodeID key.
func gqlNodeIDField() *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.ID),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			return source["nodeID"], nil
		},
	}
}

// addOrganizationRepositoryMigrations installs
// Organization.repositoryMigrations.
func (s *Resolver) addOrganizationRepositoryMigrations(orgType *graphql.Object, connectionType *graphql.Object, orderInput *graphql.InputObject) {
	args := relayConnectionArgs()
	args["orderBy"] = &graphql.ArgumentConfig{
		Type:         orderInput,
		DefaultValue: map[string]interface{}{"field": "CREATED_AT", "direction": "ASC"},
	}
	args["repositoryName"] = &graphql.ArgumentConfig{Type: graphql.String}
	args["state"] = &graphql.ArgumentConfig{Type: s.migrationStateEnum()}

	orgType.AddFieldConfig("repositoryMigrations", &graphql.Field{
		Type: graphql.NewNonNull(connectionType),
		Args: args,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			login, _ := source["login"].(string)
			org := s.store.GetOrg(login)
			if org == nil {
				return nil, gqlMissingNodeType("Organization")
			}
			if err := s.requireMigratorStanding(p.Context, org); err != nil {
				return nil, err
			}
			migrations := s.store.ListRepositoryMigrations(org.ID)
			if name, _ := p.Args["repositoryName"].(string); name != "" {
				migrations = filterRepositoryMigrations(migrations, func(m *store.RepositoryMigration) bool {
					return m.RepositoryName == name
				})
			}
			if state, _ := p.Args["state"].(string); state != "" {
				migrations = filterRepositoryMigrations(migrations, func(m *store.RepositoryMigration) bool {
					return m.State == state
				})
			}
			if repositoryMigrationOrderIsDescending(p.Args) {
				for i, j := 0, len(migrations)-1; i < j; i, j = i+1, j-1 {
					migrations[i], migrations[j] = migrations[j], migrations[i]
				}
			}
			nodes := make([]map[string]interface{}, 0, len(migrations))
			for _, m := range migrations {
				nodes = append(nodes, s.repositoryMigrationToGQL(m))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

func filterRepositoryMigrations(in []*store.RepositoryMigration, keep func(*store.RepositoryMigration) bool) []*store.RepositoryMigration {
	out := in[:0]
	for _, m := range in {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

// repositoryMigrationOrderIsDescending reads orderBy. CREATED_AT is GitHub's
// only orderable field and the store returns oldest first, so ordering reduces
// to whether to reverse.
func repositoryMigrationOrderIsDescending(args map[string]interface{}) bool {
	order, _ := args["orderBy"].(map[string]interface{})
	direction, _ := order["direction"].(string)
	return direction == "DESC"
}

// --- mutations --------------------------------------------------------------

func (s *Resolver) addMigrationMutations(mutationType, sourceType, repositoryMigrationType, orgMigrationType *graphql.Object) {
	s.addCreateMigrationSourceMutation(mutationType, sourceType)
	s.addStartRepositoryMigrationMutation(mutationType, repositoryMigrationType)
	s.addStartOrganizationMigrationMutation(mutationType, orgMigrationType)
	s.addAbortMigrationMutations(mutationType)
	s.addMigratorRoleMutations(mutationType)
}

func (s *Resolver) addCreateMigrationSourceMutation(mutationType, sourceType *graphql.Object) {
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateMigrationSourceInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"accessToken": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"githubPat":   &graphql.InputObjectFieldConfig{Type: graphql.String},
			"name":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"ownerId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"type":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.migrationSourceTypeEnum())},
			"url":         &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateMigrationSourcePayload",
		Fields: graphql.Fields{
			"migrationSource": &graphql.Field{Type: sourceType},
		},
	})
	s.registerMutation(mutationType, "createMigrationSource", &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			org := s.orgByNodeID(stringInput(input, "ownerId"))
			if org == nil {
				return nil, gqlMissingNode("Organization", stringInput(input, "ownerId"))
			}
			sourceType := stringInput(input, "type")
			if !store.ValidMigrationSourceType(sourceType) {
				return nil, fmt.Errorf("%q is not a valid MigrationSourceType", sourceType)
			}
			src := s.store.CreateMigrationSource(org.ID, stringInput(input, "name"), sourceType,
				stringInput(input, "url"), stringInput(input, "accessToken"), stringInput(input, "githubPat"))
			if src == nil {
				return nil, gqlMissingNodeType("MigrationSource")
			}
			return map[string]interface{}{"migrationSource": optionalObject(migrationSourceToGQL(src))}, nil
		},
	})
}

func (s *Resolver) addStartRepositoryMigrationMutation(mutationType, repositoryMigrationType *graphql.Object) {
	uri := s.graphQLStringScalar("URI")
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "StartRepositoryMigrationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"accessToken":          &graphql.InputObjectFieldConfig{Type: graphql.String},
			"continueOnError":      &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"gitArchiveUrl":        &graphql.InputObjectFieldConfig{Type: graphql.String},
			"githubPat":            &graphql.InputObjectFieldConfig{Type: graphql.String},
			"lockSource":           &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"metadataArchiveUrl":   &graphql.InputObjectFieldConfig{Type: graphql.String},
			"ownerId":              &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"repositoryName":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"skipReleases":         &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"sourceId":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"sourceRepositoryUrl":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(uri)},
			"targetRepoVisibility": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "StartRepositoryMigrationPayload",
		Fields: graphql.Fields{
			"repositoryMigration": &graphql.Field{Type: repositoryMigrationType},
		},
	})
	s.registerMutation(mutationType, "startRepositoryMigration", &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			org := s.orgByNodeID(stringInput(input, "ownerId"))
			if org == nil {
				return nil, gqlMissingNode("Organization", stringInput(input, "ownerId"))
			}
			source := store.FindMigrationSourceByNodeID(s.store, stringInput(input, "sourceId"))
			// The source must belong to the target org, else a migrator could
			// name another org's source and migrate with its credentials.
			if source == nil || source.OwnerOrgID != org.ID {
				return nil, gqlMissingNode("MigrationSource", stringInput(input, "sourceId"))
			}
			name := stringInput(input, "repositoryName")
			if name == "" {
				return nil, fmt.Errorf("repositoryName is required")
			}
			visibility := stringInput(input, "targetRepoVisibility")
			switch visibility {
			case "", "public", "private", "internal":
			default:
				return nil, fmt.Errorf("%q is not a valid repository visibility", visibility)
			}
			viewer := s.ghUserFromContext(p.Context)
			migration := s.store.CreateRepositoryMigration(store.NewRepositoryMigration{
				OwnerOrgID:           org.ID,
				SourceID:             source.ID,
				RepositoryName:       name,
				SourceURL:            stringInput(input, "sourceRepositoryUrl"),
				ContinueOnError:      boolInputOrDefault(input, "continueOnError", true),
				LockSource:           boolInputOrDefault(input, "lockSource", false),
				SkipReleases:         boolInputOrDefault(input, "skipReleases", false),
				TargetRepoVisibility: visibility,
				GitArchiveURL:        stringInput(input, "gitArchiveUrl"),
				MetadataArchiveURL:   stringInput(input, "metadataArchiveUrl"),
				StartedByUserID:      viewer.ID,
			})
			if migration == nil {
				return nil, gqlMissingNodeType("RepositoryMigration")
			}
			s.startRepositoryMigration(migration.ID)
			return map[string]interface{}{"repositoryMigration": optionalObject(s.repositoryMigrationToGQL(migration))}, nil
		},
	})
}

func (s *Resolver) addStartOrganizationMigrationMutation(mutationType, orgMigrationType *graphql.Object) {
	uri := s.graphQLStringScalar("URI")
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "StartOrganizationMigrationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"sourceAccessToken":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"sourceOrgUrl":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(uri)},
			"targetEnterpriseId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"targetOrgName":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "StartOrganizationMigrationPayload",
		Fields: graphql.Fields{
			"orgMigration": &graphql.Field{Type: orgMigrationType},
		},
	})
	s.registerMutation(mutationType, "startOrganizationMigration", &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			e := store.FindEnterpriseByNodeID(s.store, stringInput(input, "targetEnterpriseId"))
			if e == nil {
				return nil, gqlMissingNode("Enterprise", stringInput(input, "targetEnterpriseId"))
			}
			targetName := stringInput(input, "targetOrgName")
			if targetName == "" {
				return nil, fmt.Errorf("targetOrgName is required")
			}
			sourceURL := stringInput(input, "sourceOrgUrl")
			viewer := s.ghUserFromContext(p.Context)
			migration := s.store.CreateOrganizationMigration(e.ID, sourceURL,
				migrationSourceOrgName(sourceURL), targetName, stringInput(input, "sourceAccessToken"), viewer.ID)
			if migration == nil {
				return nil, gqlMissingNodeType("OrganizationMigration")
			}
			s.startOrganizationMigration(migration.ID)
			return map[string]interface{}{"orgMigration": optionalObject(organizationMigrationToGQL(migration))}, nil
		},
	})
}

// migrationSourceOrgName derives the org name from a source org URL, which is
// all GitHub's input carries.
func migrationSourceOrgName(rawURL string) string {
	trimmed := strings.TrimSuffix(rawURL, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

func (s *Resolver) addAbortMigrationMutations(mutationType *graphql.Object) {
	abortInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AbortRepositoryMigrationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"migrationId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	s.registerMutation(mutationType, "abortRepositoryMigration", &graphql.Field{
		Type: gqlSuccessPayload("AbortRepositoryMigrationPayload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(abortInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			migration := store.FindRepositoryMigrationByNodeID(s.store, stringInput(input, "migrationId"))
			if migration == nil {
				return nil, gqlMissingNode("RepositoryMigration", stringInput(input, "migrationId"))
			}
			// SetRepositoryMigrationState refuses to leave a terminal state, so
			// a late worker cannot overwrite the abort and aborting a finished
			// migration reports false rather than rewriting history.
			aborted := s.store.SetRepositoryMigrationState(migration.ID, store.GEIMigrationStateFailed,
				"the migration was aborted") != nil
			return map[string]interface{}{"success": aborted}, nil
		},
	})

	abortQueuedInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AbortQueuedMigrationsInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"ownerId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	s.registerMutation(mutationType, "abortQueuedMigrations", &graphql.Field{
		Type: gqlSuccessPayload("AbortQueuedMigrationsPayload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(abortQueuedInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			org := s.orgByNodeID(stringInput(input, "ownerId"))
			if org == nil {
				return nil, gqlMissingNode("Organization", stringInput(input, "ownerId"))
			}
			s.store.AbortQueuedRepositoryMigrations(org.ID, "the queued migrations were aborted")
			return map[string]interface{}{"success": true}, nil
		},
	})
}

func (s *Resolver) addMigratorRoleMutations(mutationType *graphql.Object) {
	s.registerMigratorRoleMutation(mutationType, "grantMigratorRole", true)
	s.registerMigratorRoleMutation(mutationType, "revokeMigratorRole", false)
}

// registerMigratorRoleMutation installs grantMigratorRole or
// revokeMigratorRole, which differ only in direction. One function keeps grant
// and revoke resolving actors identically, so no grant becomes unremovable.
func (s *Resolver) registerMigratorRoleMutation(mutationType *graphql.Object, name string, grant bool) {
	verb := "revoke the migrator role from"
	if grant {
		verb = "grant the migrator role to"
	}
	inputName := strings.ToUpper(name[:1]) + name[1:] + "Input"
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: inputName,
		Fields: graphql.InputObjectConfigFieldMap{
			"actor":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"actorType":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.actorTypeEnum())},
			"organizationId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	s.registerMutation(mutationType, name, &graphql.Field{
		Type: gqlSuccessPayload(strings.ToUpper(name[:1]) + name[1:] + "Payload"),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			org := s.orgByNodeID(stringInput(input, "organizationId"))
			if org == nil {
				return nil, gqlMissingNode("Organization", stringInput(input, "organizationId"))
			}
			actor := stringInput(input, "actor")
			actorType := stringInput(input, "actorType")
			if err := s.checkMigratorActorExists(org, actorType, actor); err != nil {
				return nil, err
			}
			viewer := s.ghUserFromContext(p.Context)
			changed := s.store.SetOrgMigratorRole(org.ID, actorType, actor, viewer.ID, grant)
			s.logger.Info().Str("org", org.Login).Str("actor", actor).Str("actor_type", actorType).
				Bool("granted", grant).Bool("changed", changed).Msgf("%s %s", verb, actor)
			// success reports the requested end state, not whether it changed;
			// re-granting an existing role is not a failure.
			return map[string]interface{}{"success": true}, nil
		},
	})
}

// checkMigratorActorExists refuses a grant naming a nonexistent user or team,
// or a team from another org: a grant against an unheld name would silently
// activate the day someone registers it.
func (s *Resolver) checkMigratorActorExists(org *store.Org, actorType, actor string) error {
	switch strings.ToUpper(actorType) {
	case "USER":
		if s.store.LookupUserByLogin(actor) == nil {
			return &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a User with the login of '%s'.", actor)}
		}
	case "TEAM":
		if s.store.GetTeam(org.Login, actor) == nil {
			return &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a Team with the slug of '%s'.", actor)}
		}
	default:
		return fmt.Errorf("%q is not a valid ActorType", actorType)
	}
	return nil
}

// gqlSuccessPayload is the shared `success: Boolean` payload.
func gqlSuccessPayload(name string) *graphql.Object {
	return graphql.NewObject(graphql.ObjectConfig{
		Name:   name,
		Fields: graphql.Fields{"success": &graphql.Field{Type: graphql.Boolean}},
	})
}

func stringInput(input map[string]interface{}, key string) string {
	value, _ := input[key].(string)
	return value
}

func boolInputOrDefault(input map[string]interface{}, key string, fallback bool) bool {
	if value, ok := input[key].(bool); ok {
		return value
	}
	return fallback
}

// --- node resolution --------------------------------------------------------

// migrationNodeByID resolves the three migration global ids for `node(id:)`,
// each gated on migrator standing so guessing an id reveals nothing.
func (s *Resolver) migrationNodeByID(ctx context.Context, nodeID string) interface{} {
	if src := store.FindMigrationSourceByNodeID(s.store, nodeID); src != nil {
		if org := s.store.GetOrgByID(src.OwnerOrgID); org == nil || s.requireMigratorStanding(ctx, org) != nil {
			return nil
		}
		return migrationSourceToGQL(s.store.GetMigrationSource(src.ID))
	}
	if m := store.FindRepositoryMigrationByNodeID(s.store, nodeID); m != nil {
		if org := s.store.GetOrgByID(m.OwnerOrgID); org == nil || s.requireMigratorStanding(ctx, org) != nil {
			return nil
		}
		return s.repositoryMigrationToGQL(s.store.GetRepositoryMigration(m.ID))
	}
	if m := store.FindOrganizationMigrationByNodeID(s.store, nodeID); m != nil {
		if !s.store.IsEnterpriseOwner(m.EnterpriseID, s.ghUserFromContext(ctx)) {
			return nil
		}
		return organizationMigrationToGQL(s.store.GetOrganizationMigration(m.ID))
	}
	return nil
}
