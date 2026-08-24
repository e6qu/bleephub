package graphqlapi

import (
	"fmt"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// Resolver-layer converter and connection tests, moved from the server
// package with the resolver layer (ARCH-003).

// newSeededTestStore is a store with the default admin user and no HTTP
// server around it — the moved tests only exercise store-pure resolver
// helpers.
func newSeededTestStore() *store.Store {
	st := store.NewStore()
	st.SeedDefaultUser()
	return st
}

// TestIssueFieldValuesConnectionCountsBeyond100 covers GQL-022: the
// issueFieldValues sub-connection pre-paginated with paginateGQL, whose page
// size is clamped to 100, and the field resolver then re-paginated that
// already-truncated slice — so an issue with more than 100 field values
// reported totalCount 100 and hid the remainder. The connection now returns the
// full node set and lets the resolver apply the single, correct page window.
func TestIssueFieldValuesConnectionCountsBeyond100(t *testing.T) {
	st := newSeededTestStore()
	admin := st.UsersByLogin["admin"]
	org := st.CreateOrg(admin, "fieldorg", "Field Org", "")
	repo := st.CreateOrgRepo(org, admin, "fieldrepo", "", false)
	issue := st.CreateIssue(repo.ID, admin.ID, "issue with many fields", "", nil, nil, 0)
	if issue == nil {
		t.Fatal("issue not created")
	}

	const total = 105
	values := make(map[int]interface{}, total)
	for i := 0; i < total; i++ {
		f := st.CreateIssueField(org.Login, fmt.Sprintf("field-%03d", i), nil, "text", "all", nil)
		values[f.ID] = "v"
	}
	st.SetIssueFieldValues(issue.ID, values)

	st.Mu.RLock()
	conn := issueFieldValuesConnectionLocked(st, issue)
	st.Mu.RUnlock()

	// The Issue.issueFieldValues resolver hands the built connection to
	// repaginateConnection with the client's page args; totalCount must be the
	// true count, not the clamped page size.
	repaged, ok := repaginateConnection(conn, map[string]interface{}{}).(map[string]interface{})
	if !ok {
		t.Fatalf("repaginateConnection returned %T, want map", repaged)
	}
	if got := repaged["totalCount"].(int); got != total {
		t.Fatalf("totalCount = %d, want %d (GQL-022: pre-pagination clamped it to 100)", got, total)
	}
}

func TestDiscussionToGQLNilDiscussionReturnsNil(t *testing.T) {
	if got := discussionToGQL(nil, newSeededTestStore()); got != nil {
		t.Fatalf("discussionToGQL(nil) = %#v, want nil", got)
	}
}

// TestExternalURLPrefix is the GQL-042 regression: GraphQL `url` fields go
// through externalURL, which prefixes BLEEPHUB_EXTERNAL_URL when set and stays
// relative otherwise (resourcePath fields never call it).
func TestExternalURLPrefix(t *testing.T) {
	t.Setenv("BLEEPHUB_EXTERNAL_URL", "https://gh.example.com/")
	if got := externalURL("/octo/repo/issues/1"); got != "https://gh.example.com/octo/repo/issues/1" {
		t.Fatalf("externalURL with endpoint = %q", got)
	}

	t.Setenv("BLEEPHUB_EXTERNAL_URL", "")
	if got := externalURL("/octo/repo/issues/1"); got != "/octo/repo/issues/1" {
		t.Fatalf("externalURL without endpoint = %q, want the relative path", got)
	}
}

// TestRequestedReviewerTypeName is the GQL-037 regression: the RequestedReviewer
// union must resolve its member from the __typename discriminator rather than
// unconditionally reporting User.
func TestRequestedReviewerTypeName(t *testing.T) {
	cases := []struct {
		name string
		src  interface{}
		want string
	}{
		{"user", map[string]interface{}{"__typename": "User", "login": "octocat"}, "User"},
		{"team", map[string]interface{}{"__typename": "Team", "slug": "reviewers"}, "Team"},
		{"bot", map[string]interface{}{"__typename": "Bot"}, "Bot"},
		{"untagged defaults to user", map[string]interface{}{"login": "octocat"}, "User"},
		{"non-map defaults to user", "not a map", "User"},
	}
	for _, c := range cases {
		if got := requestedReviewerTypeName(c.src); got != c.want {
			t.Errorf("%s: requestedReviewerTypeName = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestProjectV2SourceNoLongerCarriesStore is the GQL-029 regression: the project
// source map must not embed a live *Store, and the id is still extractable
// without one.
func TestProjectV2SourceNoLongerCarriesStore(t *testing.T) {
	m := projectV2ToGQL(store.NewStore(), &store.ProjectV2{ID: 7, NodeID: "PVT_x", Number: 3, Title: "Roadmap"})
	if _, ok := m["store"]; ok {
		t.Fatal("projectV2ToGQL still embeds a *Store in the source map")
	}
	id, err := projectV2SourceID(m)
	if err != nil || id != 7 {
		t.Fatalf("projectV2SourceID = %d, %v; want 7, nil", id, err)
	}
	if _, err := projectV2SourceID("not a map"); err == nil {
		t.Fatal("projectV2SourceID accepted a non-map source")
	}
}

func TestRelayPaginationCombinesDirectionalBounds(t *testing.T) {
	nodes := make([]map[string]interface{}, 8)
	for index := range nodes {
		nodes[index] = map[string]interface{}{"index": index}
	}

	connection := repaginateConnection(
		map[string]interface{}{"nodes": nodes},
		map[string]interface{}{"first": 2, "after": encodeCursor(1), "before": encodeCursor(6)},
	).(map[string]interface{})
	page := connection["nodes"].([]map[string]interface{})
	if len(page) != 2 || page[0]["index"] != 2 || page[1]["index"] != 3 {
		t.Fatalf("first/after/before window = %v, want indexes 2 and 3", page)
	}
	pageInfo := connection["pageInfo"].(map[string]interface{})
	if pageInfo["hasPreviousPage"] != true || pageInfo["hasNextPage"] != true {
		t.Fatalf("bounded forward pageInfo = %v, want both directions", pageInfo)
	}

	connection = repaginateConnection(
		map[string]interface{}{"nodes": nodes},
		map[string]interface{}{"last": 2, "after": encodeCursor(1), "before": encodeCursor(6)},
	).(map[string]interface{})
	page = connection["nodes"].([]map[string]interface{})
	if len(page) != 2 || page[0]["index"] != 4 || page[1]["index"] != 5 {
		t.Fatalf("last/after/before window = %v, want indexes 4 and 5", page)
	}
}

func TestRepoGraphQLURLUsesConfiguredExternalURL(t *testing.T) {
	t.Setenv("BLEEPHUB_EXTERNAL_URL", "https://bleephub.example.test/")
	st := newSeededTestStore()
	repo := &store.Repo{FullName: "octo/example", Name: "example"}
	if got := repoToGraphQL(st, repo)["url"]; got != "https://bleephub.example.test/octo/example" {
		t.Fatalf("repository GraphQL url = %v", got)
	}
}
