package graphqlapi

// Account-surface completion for GitHub's three most-queried object types:
// Repository, User and Organization.
//
// The families here are the members of those three types that a real client
// selects and that bleephub already holds data for: repository settings and
// the viewer's standing on a repository, the community-health files a
// repository carries in git, its collaborator / milestone / label / deploy-key
// / environment graph, a user's profile, follow graph, contribution surfaces
// and account keys, and an organization's membership, teams, governance and
// profile.
//
// Every field resolves from the same store state the REST surface serves. A
// field is absent from this package only when the feature behind it does not
// exist in bleephub at all — never emitted as an empty connection that would
// tell a client "this instance has none" when the truth is "not implemented".

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// accountSurfaceTypes holds the object types this family mints, so a type
// named by two installers is built once. graphql-go rejects a schema carrying
// two distinct types with one name, and the registry in gh_issues_graphql.go
// is shared with every other family; keeping these here keeps the ownership
// local to the files that create them.
type accountSurfaceTypes struct {
	user         *graphql.Object
	organization *graphql.Object
	repository   *graphql.Object

	// Repository sub-objects.
	interactionAbility *graphql.Object
	codeOfConduct      *graphql.Object

	// Types other families mint that the account surface also names. Each is
	// recorded where it is built, because graphql-go rejects a schema
	// carrying two distinct types with one name and these families run first.
	gistComment                *graphql.Object
	ipAllowListEntryConnection *graphql.Object
	ruleset                    *graphql.Object
	rulesetConnection          *graphql.Object
	pinnableItem               *graphql.Union

	// Shared connections.
	userConnection *graphql.Object
	userEdge       *graphql.Object

	// Order inputs minted by gqlOrderInput, keyed by GitHub's input name.
	orderInputs map[string]*graphql.InputObject
	// Connection/edge pairs minted by accountConnectionType, keyed by node
	// type name.
	connections map[string]*graphql.Object
}

// accountSurfaceRegistry returns this resolver's account-surface type
// registry, creating it on first use. Query.codeOfConduct is assembled before
// the account surface is installed and shares the CodeOfConduct type through
// it, so the registry has to exist independently of the installer's ordering.
func (s *Resolver) accountSurfaceRegistry() *accountSurfaceTypes {
	if s.graphqlTypes.accountSurface == nil {
		s.graphqlTypes.accountSurface = &accountSurfaceTypes{}
	}
	return s.graphqlTypes.accountSurface
}

// addAccountSurfaceFieldsToSchema installs the completed Repository, User and
// Organization field sets. It runs last in schema assembly: its fields name
// the milestone, label, team, ruleset, gist, issue and pull-request types
// every earlier family builds.
func (s *Resolver) addAccountSurfaceFieldsToSchema(userType, orgType, repoType *graphql.Object) {
	types := s.accountSurfaceRegistry()
	types.user = userType
	types.organization = orgType
	types.repository = repoType
	types.userConnection = s.gqlUserConnectionType(userType)
	// The shared UserConnection carries no `edges` yet; GitHub declares one
	// over the same UserEdge the follow connections use.
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
}

// --- source helpers --------------------------------------------------------

// graphQLSourceMap narrows a resolver source to the map every source in this
// package is.
func graphQLSourceMap(source interface{}) (map[string]interface{}, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve source: unexpected type %T", source)
	}
	return src, nil
}

// repoFromSource re-reads the repository a Repository source names. The
// source map carries the identity (nameWithOwner); the live row is read back
// so a field resolves against current state rather than whatever the source
// was rendered from.
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

// --- field builders --------------------------------------------------------

// repoBoolField is a non-null Boolean on Repository read from the live row.
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

// repoEnumField is a non-null enum on Repository whose value comes from a
// stored setting, mapped onto GitHub's enum by the reader.
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

// gqlOrderInput builds one of GitHub's `<Name>Order` inputs: the two-member
// {direction, field} shape every ordering argument in the schema uses. The
// input is memoized by name so two fields naming the same order input share
// one type, which graphql-go requires.
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

// accountConnectionType builds GitHub's `<Name>Connection` / `<Name>Edge`
// pair over a node type: edges, nodes, pageInfo and totalCount, with the
// edge's own cursor plus any extra edge members GitHub declares.
//
// It differs from gqlConnectionType (which the Projects v2 family owns) in
// carrying `edges` and in letting the caller declare the edge's node non-null
// — both of which GitHub's account-surface connections need.
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

// gqlUserEdgeType is GitHub's one UserEdge object. Several account
// connections over users (UserConnection, FollowerConnection,
// FollowingConnection) all declare their edges as [UserEdge], so the type is
// minted once here.
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

// gqlUserEdgeConnection builds a connection over users whose edges are the
// shared UserEdge — the shape GitHub gives FollowerConnection,
// FollowingConnection and UserConnection.
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

// connectionArgs is the four Relay window arguments every connection here
// accepts, plus any extra arguments the caller names.
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
// descending order. A missing or malformed argument leaves the caller's
// default in place.
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

// orderField reads an order-input argument's field selector.
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

// --- rendering helpers -----------------------------------------------------

// renderAccountMarkdown renders a profile or settings string through the one
// markdown pipeline the rest of bleephub renders bodies with
// (store.MarkdownModeRenderer, shared with the discussion and REST markdown
// surfaces), so a *HTML field is never a second renderer's opinion.
func renderAccountMarkdown(text string) string {
	return discussionBodyToHTML(text)
}

// truncateRunes shortens text to at most limit runes, appending GitHub's
// ellipsis when it had to cut.
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

// nullableRFC3339 renders a timestamp as a DateTime, or null when zero.
func nullableRFC3339(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// sortedUsersByLogin orders accounts so a connection's page boundaries are
// stable across requests (store maps iterate nondeterministically).
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

// userConnectionItems renders accounts as lazily-rendered connection items.
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

// interactionAbilitySource renders GitHub's RepositoryInteractionAbility from
// a stored limit. An expired restriction is no longer in effect, exactly as
// the REST interaction-limits endpoint reports it.
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

// gqlInteractionAbilityType is GitHub's RepositoryInteractionAbility, shared
// by Repository.interactionAbility, User.interactionAbility and
// Organization.interactionAbility.
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
