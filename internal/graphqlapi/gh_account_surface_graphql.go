package graphqlapi

// Account-surface fields on Repository, User and Organization. Fields resolve
// from the same store state REST serves. A field is absent only when the
// feature does not exist in bleephub — never emitted as an empty connection.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// accountSurfaceTypes memoizes the object types this family mints so a type
// named by two installers is built once (graphql-go rejects duplicate names).
type accountSurfaceTypes struct {
	user         *graphql.Object
	organization *graphql.Object
	repository   *graphql.Object

	interactionAbility *graphql.Object
	codeOfConduct      *graphql.Object

	// Types other families mint that the account surface also names.
	gistComment                *graphql.Object
	ipAllowListEntryConnection *graphql.Object
	ruleset                    *graphql.Object
	rulesetConnection          *graphql.Object
	pinnableItem               *graphql.Union

	userConnection *graphql.Object
	userEdge       *graphql.Object

	orderInputs map[string]*graphql.InputObject
	connections map[string]*graphql.Object
}

// accountSurfaceRegistry returns the type registry, creating it on first use.
// It must exist independently of installer ordering — Query.codeOfConduct is
// assembled before the account surface and shares CodeOfConduct through it.
func (s *Resolver) accountSurfaceRegistry() *accountSurfaceTypes {
	if s.graphqlTypes.accountSurface == nil {
		s.graphqlTypes.accountSurface = &accountSurfaceTypes{}
	}
	return s.graphqlTypes.accountSurface
}

// addAccountSurfaceFieldsToSchema installs the Repository, User and
// Organization field sets. It must run last: its fields name the milestone,
// label, team, ruleset, gist, issue and pull-request types earlier families
// build.
func (s *Resolver) addAccountSurfaceFieldsToSchema(userType, orgType, repoType *graphql.Object) {
	types := s.accountSurfaceRegistry()
	types.user = userType
	types.organization = orgType
	types.repository = repoType
	types.userConnection = s.gqlUserConnectionType(userType)
	types.userConnection.AddFieldConfig("edges", &graphql.Field{
		Type: graphql.NewList(s.gqlUserEdgeType(types)),
	})
	s.addRepositorySettingFields(types)
	s.addRepositoryViewerFields(types)
	s.addRepositoryCommunityFields(types)
	s.addRepositoryPeopleFields(types)
	s.addRepositoryDeploymentFields(types)
	s.addTeamFields(types)
	s.addOrganizationProfileFields(types)
	s.addOrganizationPeopleFields(types)
	s.addOrganizationGovernanceFields(types)
	s.addUserProfileFields(types)
	s.addUserConnectionFields(types)
	s.addPinnedItemFields(types)

	// Runs last: names the PackageConnection, AssigneeConnection,
	// RepositoryRuleConnection, Repository, Ref and Release types built above.
	s.addFinalResidueFields(types)
}

// source helpers

func graphQLSourceMap(source interface{}) (map[string]interface{}, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve source: unexpected type %T", source)
	}
	return src, nil
}

// repoFromSource re-reads the live repository row a Repository source names, so
// a field resolves against current state rather than the rendered source.
func (s *Resolver) repoFromSource(source interface{}) (*store.Repo, error) {
	src, err := graphQLSourceMap(source)
	if err != nil {
		return nil, err
	}
	fullName, _ := src["nameWithOwner"].(string)
	owner, name, ok := store.SplitRepoFullName(fullName)
	if !ok {
		return nil, fmt.Errorf("repository source carries no nameWithOwner")
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		return nil, fmt.Errorf("repository %s not found", fullName)
	}
	return repo, nil
}

// userFromSource re-reads the account a User source names.
func (s *Resolver) userFromSource(source interface{}) (*store.User, error) {
	src, err := graphQLSourceMap(source)
	if err != nil {
		return nil, err
	}
	login, _ := src["login"].(string)
	user := s.store.LookupUserByLogin(login)
	if user == nil {
		return nil, fmt.Errorf("user %q not found", login)
	}
	return user, nil
}

// orgFromSource re-reads the organization an Organization source names.
func (s *Resolver) orgFromSource(source interface{}) (*store.Org, error) {
	src, err := graphQLSourceMap(source)
	if err != nil {
		return nil, err
	}
	login, _ := src["login"].(string)
	org := s.store.GetOrg(login)
	if org == nil {
		return nil, fmt.Errorf("organization %q not found", login)
	}
	return org, nil
}

// field builders

func (s *Resolver) repoBoolField(read func(*store.Repo) bool) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return read(repo), nil
		},
	}
}

func (s *Resolver) repoEnumField(enum *graphql.Enum, read func(*store.Repo) string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(enum),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return read(repo), nil
		},
	}
}

// gqlOrderInput builds a `<Name>Order` {direction, field} input, memoized by
// name so fields naming the same order input share one type.
func (s *Resolver) gqlOrderInput(types *accountSurfaceTypes, name, fieldEnum string, fieldValues ...string) *graphql.InputObject {
	if types.orderInputs == nil {
		types.orderInputs = map[string]*graphql.InputObject{}
	}
	if existing := types.orderInputs[name]; existing != nil {
		return existing
	}
	input := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: name,
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.sharedEnum("OrderDirection", "ASC", "DESC")),
			},
			"field": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.sharedEnum(fieldEnum, fieldValues...)),
			},
		},
	})
	types.orderInputs[name] = input
	return input
}

// accountConnectionType builds a `<Name>Connection`/`<Name>Edge` pair over a
// node type. Unlike gqlConnectionType it carries `edges` and lets the caller
// mark the edge's node non-null, both of which account-surface connections need.
func (s *Resolver) accountConnectionType(
	types *accountSurfaceTypes,
	name string,
	nodeType graphql.Output,
	edgeNodeNonNull bool,
	extraEdgeFields graphql.Fields,
) *graphql.Object {
	if types.connections == nil {
		types.connections = map[string]*graphql.Object{}
	}
	if existing := types.connections[name]; existing != nil {
		return existing
	}
	edgeNodeType := nodeType
	if edgeNodeNonNull {
		edgeNodeType = graphql.NewNonNull(nodeType)
	}
	edgeFields := graphql.Fields{
		"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"node":   &graphql.Field{Type: edgeNodeType},
	}
	for field, config := range extraEdgeFields {
		edgeFields[field] = config
	}
	edgeType := graphql.NewObject(graphql.ObjectConfig{Name: name + "Edge", Fields: edgeFields})
	connection := graphql.NewObject(graphql.ObjectConfig{
		Name: name + "Connection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(edgeType)},
			"nodes":      &graphql.Field{Type: graphql.NewList(nodeType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	types.connections[name] = connection
	return connection
}

// gqlUserEdgeType mints the one shared UserEdge object (UserConnection,
// FollowerConnection and FollowingConnection all declare edges as [UserEdge]).
func (s *Resolver) gqlUserEdgeType(types *accountSurfaceTypes) *graphql.Object {
	if types.userEdge != nil {
		return types.userEdge
	}
	types.userEdge = graphql.NewObject(graphql.ObjectConfig{
		Name: "UserEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: types.user},
		},
	})
	return types.userEdge
}

// gqlUserEdgeConnection builds a user connection whose edges are the shared
// UserEdge.
func (s *Resolver) gqlUserEdgeConnection(types *accountSurfaceTypes, name string) *graphql.Object {
	if types.connections == nil {
		types.connections = map[string]*graphql.Object{}
	}
	if existing := types.connections[name]; existing != nil {
		return existing
	}
	connection := graphql.NewObject(graphql.ObjectConfig{
		Name: name + "Connection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(s.gqlUserEdgeType(types))},
			"nodes":      &graphql.Field{Type: graphql.NewList(types.user)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	types.connections[name] = connection
	return connection
}

// connectionArgs is the four Relay window arguments plus any extra the caller names.
func connectionArgs(extra graphql.FieldConfigArgument) graphql.FieldConfigArgument {
	args := graphql.FieldConfigArgument{
		"first":  &graphql.ArgumentConfig{Type: graphql.Int},
		"after":  &graphql.ArgumentConfig{Type: graphql.String},
		"last":   &graphql.ArgumentConfig{Type: graphql.Int},
		"before": &graphql.ArgumentConfig{Type: graphql.String},
	}
	for name, config := range extra {
		args[name] = config
	}
	return args
}

// orderDirectionDescending reports whether an order-input argument asks for
// descending order; a missing or malformed argument returns fallback.
func orderDirectionDescending(args map[string]interface{}, key string, fallback bool) bool {
	order, ok := args[key].(map[string]interface{})
	if !ok {
		return fallback
	}
	direction, ok := order["direction"].(string)
	if !ok {
		return fallback
	}
	return direction == "DESC"
}

func orderField(args map[string]interface{}, key, fallback string) string {
	order, ok := args[key].(map[string]interface{})
	if !ok {
		return fallback
	}
	field, ok := order["field"].(string)
	if !ok || field == "" {
		return fallback
	}
	return field
}

// rendering helpers

// renderAccountMarkdown renders a profile/settings string through the shared
// markdown pipeline so a *HTML field is never a second renderer's opinion.
func renderAccountMarkdown(text string) string {
	return discussionBodyToHTML(text)
}

// truncateRunes shortens text to at most limit runes, appending an ellipsis.
func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimRight(string(runes[:limit]), " ") + "…"
}

// nullableRFC3339 renders a timestamp as DateTime, or null when zero.
func nullableRFC3339(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// sortedUsersByLogin orders accounts so connection page boundaries are stable
// across requests (store maps iterate nondeterministically).
func sortedUsersByLogin(users []*store.User) []*store.User {
	out := make([]*store.User, 0, len(users))
	for _, u := range users {
		if u != nil {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Login < out[j].Login })
	return out
}

func userConnectionItems(users []*store.User) []gqlConnItem {
	items := make([]gqlConnItem, 0, len(users))
	for _, u := range users {
		user := u
		items = append(items, gqlConnItem{
			identity: user.NodeID,
			render:   func() map[string]interface{} { return userToGraphQL(user) },
		})
	}
	return items
}

// interactionAbilitySource renders RepositoryInteractionAbility from a stored
// limit. An expired restriction reads as no longer in effect, matching REST.
func (s *Resolver) interactionAbilitySource(limit string, expiry *time.Time, origin string) map[string]interface{} {
	if limit == "" || expiry == nil || s.store.CurrentTime().After(*expiry) {
		return nil
	}
	return map[string]interface{}{
		"limit":     strings.ToUpper(limit),
		"origin":    origin,
		"expiresAt": expiry.UTC().Format(time.RFC3339),
	}
}

// gqlInteractionAbilityType is the shared RepositoryInteractionAbility type.
func (s *Resolver) gqlInteractionAbilityType(types *accountSurfaceTypes) *graphql.Object {
	if types.interactionAbility != nil {
		return types.interactionAbility
	}
	types.interactionAbility = graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryInteractionAbility",
		Fields: graphql.Fields{
			"expiresAt": &graphql.Field{Type: s.graphQLStringScalar("DateTime")},
			"limit": &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum(
				"RepositoryInteractionLimit",
				"COLLABORATORS_ONLY", "CONTRIBUTORS_ONLY", "EXISTING_USERS", "NO_LIMIT"))},
			"origin": &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum(
				"RepositoryInteractionLimitOrigin", "ORGANIZATION", "REPOSITORY", "USER"))},
		},
	})
	return types.interactionAbility
}
