package bleephub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

func graphQLRequestOnServer(t *testing.T, server *Server, user *User, query string) map[string]interface{} {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/graphql", bytes.NewReader(body))
	request = request.WithContext(contextWithUser(context.Background(), user))
	response := httptest.NewRecorder()
	server.handleGraphQL(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL status = %d, body = %s", response.Code, response.Body.String())
	}
	var result map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if errors, ok := result["errors"].([]interface{}); ok && len(errors) != 0 {
		t.Fatalf("GraphQL errors: %v", errors)
	}
	return result
}

func TestGraphQLDocumentCostLimitsDepthAndFields(t *testing.T) {
	t.Parallel()
	parse := func(query string) *ast.Document {
		t.Helper()
		document, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(query)})})
		if err != nil {
			t.Fatal(err)
		}
		return document
	}

	deep := "{ viewer" + strings.Repeat(" { viewer", 21) + " { login }" + strings.Repeat(" }", 22)
	deepDocument := parse(deep)
	if err := graphqlCheckDocumentCost(deepDocument, 20, 5000); err == nil {
		t.Fatal("deep query passed the depth limit")
	}

	wide := "{ viewer { " + strings.Repeat("login ", 101) + "} }"
	wideDocument := parse(wide)
	if err := graphqlCheckDocumentCost(wideDocument, 20, 100); err == nil {
		t.Fatal("wide query passed the field-count limit")
	}
}

func TestGraphQLSchemasBuildWithServerLocalTypeRegistries(t *testing.T) {
	t.Parallel()
	servers := []*Server{newTestServer(), newTestServer()}
	var wait sync.WaitGroup
	for _, server := range servers {
		wait.Add(1)
		go func(server *Server) {
			defer wait.Done()
			server.initGraphQLSchema()
		}(server)
	}
	wait.Wait()
	if servers[0].graphqlTypes.pageInfo == nil || servers[1].graphqlTypes.pageInfo == nil {
		t.Fatal("schema build did not initialize PageInfo")
	}
	if servers[0].graphqlTypes.pageInfo == servers[1].graphqlTypes.pageInfo {
		t.Fatal("independent servers share a mutable GraphQL type registry")
	}
	if servers[0].graphqlTypes.projectV2Type == servers[1].graphqlTypes.projectV2Type {
		t.Fatal("independent servers share mutable ProjectV2 types")
	}
}

func TestGraphQLDocumentLimitsRejectInvalidRelayWindows(t *testing.T) {
	t.Parallel()
	parse := func(query string) *ast.Document {
		t.Helper()
		document, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(query)})})
		if err != nil {
			t.Fatal(err)
		}
		return document
	}
	for _, query := range []string{
		"{ viewer { repositories(first: 1, last: 1) { totalCount } } }",
		"{ viewer { repositories(first: 101) { totalCount } } }",
		"{ viewer { repositories(after: \"not-a-cursor\") { totalCount } } }",
	} {
		if err := graphqlCheckDocumentLimits(parse(query), nil, 20, 5000); err == nil {
			t.Fatalf("invalid Relay window passed: %s", query)
		}
	}
	valid := "{ viewer { repositories(first: 1, after: \"" + encodeCursor(0) + "\") { totalCount } } }"
	if err := graphqlCheckDocumentLimits(parse(valid), nil, 20, 5000); err != nil {
		t.Fatalf("valid Relay window rejected: %v", err)
	}
}

func TestGraphQLRepositoryConnectionHonorsAffiliationOrderAndBackwardPagination(t *testing.T) {
	t.Parallel()
	server := graphQLFuzzServer(t)
	admin := server.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("seeded admin is missing")
	}
	now := server.store.currentTime()
	other := &User{
		ID: admin.ID + 1, NodeID: "U_kgDOgraphqlother", Login: "other",
		Name: "Other", Email: "other@example.test", Type: "User",
		StarredRepos: map[string]bool{}, CreatedAt: now, UpdatedAt: now,
	}
	server.store.mu.Lock()
	server.store.Users[other.ID] = other
	server.store.UsersByLogin[other.Login] = other
	if server.store.NextUser <= other.ID {
		server.store.NextUser = other.ID + 1
	}
	server.store.mu.Unlock()

	if server.store.CreateRepo(admin, "zeta", "", false) == nil ||
		server.store.CreateRepo(admin, "alpha", "", false) == nil ||
		server.store.CreateRepo(other, "beta", "", false) == nil {
		t.Fatal("seed GraphQL repositories")
	}
	if !server.store.AddRepoCollaborator("other", "beta", admin.Login, "pull") {
		t.Fatal("seed collaborator grant")
	}
	if repos := server.store.ListReposForAuthUser(admin, RepoListOptions{
		Affiliation: "owner,collaborator",
		NoPaginate:  true,
	}); len(repos) != 3 {
		t.Fatalf("seeded affiliations returned %d repos", len(repos))
	}

	result := graphQLRequestOnServer(t, server, admin, `{
		viewer {
			repositories(
				first: 2
				ownerAffiliations: [OWNER, COLLABORATOR]
				orderBy: {field: NAME, direction: ASC}
			) {
				nodes { nameWithOwner }
				pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
			}
		}
	}`)
	data := result["data"].(map[string]interface{})
	viewer := data["viewer"].(map[string]interface{})
	connection := viewer["repositories"].(map[string]interface{})
	nodes := connection["nodes"].([]interface{})
	if got := []string{
		nodes[0].(map[string]interface{})["nameWithOwner"].(string),
		nodes[1].(map[string]interface{})["nameWithOwner"].(string),
	}; got[0] != "admin/alpha" || got[1] != "other/beta" {
		t.Fatalf("ordered affiliated page = %v", got)
	}
	pageInfo := connection["pageInfo"].(map[string]interface{})
	if pageInfo["hasNextPage"] != true || pageInfo["hasPreviousPage"] != false {
		t.Fatalf("first pageInfo = %v", pageInfo)
	}

	before := encodeCursor(2)
	result = graphQLRequestOnServer(t, server, admin, `{
		viewer {
			repositories(
				last: 1
				before: "`+before+`"
				ownerAffiliations: [OWNER, COLLABORATOR]
				orderBy: {field: NAME, direction: ASC}
			) {
				nodes { nameWithOwner }
				pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
			}
		}
	}`)
	data = result["data"].(map[string]interface{})
	viewer = data["viewer"].(map[string]interface{})
	connection = viewer["repositories"].(map[string]interface{})
	nodes = connection["nodes"].([]interface{})
	if len(nodes) != 1 || nodes[0].(map[string]interface{})["nameWithOwner"] != "other/beta" {
		t.Fatalf("backward page = %v", nodes)
	}
	pageInfo = connection["pageInfo"].(map[string]interface{})
	if pageInfo["hasPreviousPage"] != true || pageInfo["hasNextPage"] != true {
		t.Fatalf("backward pageInfo = %v", pageInfo)
	}

	result = graphQLRequestOnServer(t, server, admin, `{
		viewer {
			repositories(ownerAffiliations: [OWNER], orderBy: {field: NAME, direction: ASC}) {
				nodes { nameWithOwner }
			}
		}
	}`)
	data = result["data"].(map[string]interface{})
	viewer = data["viewer"].(map[string]interface{})
	connection = viewer["repositories"].(map[string]interface{})
	nodes = connection["nodes"].([]interface{})
	if len(nodes) != 2 {
		t.Fatalf("owner-only nodes = %v", nodes)
	}
}
