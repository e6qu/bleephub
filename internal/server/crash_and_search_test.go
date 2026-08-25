package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// ─── helpers ─────────────────────────────────────────────────────────────

// deleteOnRead runs fn the first time the request body is read. Handlers
// resolve their target before decoding the body, so a hook here reproduces a
// concurrent delete landing between resolve and mutate — deterministically,
// without a racing goroutine.
type deleteOnRead struct {
	body io.Reader
	fn   func()
	done bool
}

func (d *deleteOnRead) Read(p []byte) (int, error) {
	if !d.done {
		d.done = true
		d.fn()
	}
	return d.body.Read(p)
}

// serveWithConcurrentDelete drives one authenticated request whose body read
// triggers fn.
func serveWithConcurrentDelete(s *Server, method, path string, body interface{}, fn func()) *httptest.ResponseRecorder {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(method, path, &deleteOnRead{body: newBytesReader(raw), fn: fn})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+defaultToken)
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	return w
}

func newBytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// searchItemIDs returns the ids of a search envelope's items.
func searchItemIDs(t *testing.T, w *httptest.ResponseRecorder) []float64 {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		TotalCount int                      `json:"total_count"`
		Items      []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode search envelope: %v (%s)", err, w.Body.String())
	}
	ids := make([]float64, 0, len(env.Items))
	for _, item := range env.Items {
		id, ok := item["id"].(float64)
		if !ok {
			t.Fatalf("search item has no numeric id: %v", item)
		}
		ids = append(ids, id)
	}
	return ids
}

// ─── deleted users render as ghost, never a nil dereference ──────────────

func TestUserToJSONNilRendersGhostAccount(t *testing.T) {
	got := store.UserToJSON(nil, "https://bleephub.test")
	if got["login"] != "ghost" {
		t.Fatalf("login = %v, want ghost", got["login"])
	}
	if got["id"] != 10137 {
		t.Fatalf("id = %v, want 10137", got["id"])
	}
	if got["node_id"] != "U_bleephub_ghost" {
		t.Fatalf("node_id = %v", got["node_id"])
	}
	if got["url"] != "https://bleephub.test/api/v3/users/ghost" {
		t.Fatalf("url = %v", got["url"])
	}
}

func TestSearchIssuesWithDeletedPullRequestAuthorRendersGhost(t *testing.T) {
	s := fuzzRoutedServer(t)
	seedGitBackedPR(t, s)

	// The PR's author account is deleted while the PR survives.
	var fullName string
	s.store.Mu.Lock()
	for _, pr := range s.store.PullRequests {
		pr.AuthorID = 4242
		if repo := s.store.Repos[pr.RepoID]; repo != nil {
			fullName = repo.FullName
		}
	}
	delete(s.store.Users, 4242)
	s.store.Mu.Unlock()
	if fullName == "" {
		t.Fatal("no pull request seeded")
	}

	w := fuzzServe(s, http.MethodGet, "/api/v3/search/issues?q=repo:"+fullName, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Items) == 0 {
		t.Fatalf("no items for repo:%s: %s", fullName, w.Body.String())
	}
	user, _ := env.Items[0]["user"].(map[string]interface{})
	if user == nil || user["login"] != "ghost" {
		t.Fatalf("deleted author rendered as %v, want the ghost account", env.Items[0]["user"])
	}
}

// ─── a card whose column vanished is gone, not a nil dereference ─────────

func seedProjectCard(t *testing.T, s *Server) (*store.ProjectCard, *store.ProjectColumn) {
	t.Helper()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "cards-repo", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	proj := s.store.CreateProjectClassic(repo, admin.ID, "board", "", "open")
	if proj == nil {
		t.Fatal("CreateProjectClassic returned nil")
	}
	col := s.store.CreateProjectColumn(proj.ID, "todo")
	card := s.store.CreateProjectCard(col.ID, admin.ID, "a note", 0, 0)
	if card == nil {
		t.Fatal("CreateProjectCard returned nil")
	}
	return card, col
}

func TestUpdateProjectCardLosingItsColumnIsNotFound(t *testing.T) {
	s := fuzzRoutedServer(t)
	card, col := seedProjectCard(t, s)

	// The column is deleted after the card has been resolved, which is the
	// only window in which a card outlives its column.
	path := fmt.Sprintf("/api/v3/projects/columns/cards/%d", card.ID)
	w := serveWithConcurrentDelete(s, http.MethodPatch, path,
		map[string]interface{}{"note": "renamed"},
		func() {
			s.store.Mu.Lock()
			delete(s.store.ProjectColumns, col.ID)
			s.store.Mu.Unlock()
		})
	if w.Code != http.StatusNotFound {
		t.Fatalf("card PATCH racing its column's delete = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestListProjectCardsStillListsLiveCards(t *testing.T) {
	s := fuzzRoutedServer(t)
	_, col := seedProjectCard(t, s)

	w := fuzzServe(s, http.MethodGet, fmt.Sprintf("/api/v3/projects/columns/%d/cards", col.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list cards = %d: %s", w.Code, w.Body.String())
	}
	var listed []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v (%s)", err, w.Body.String())
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d cards, want 1", len(listed))
	}
}

func TestProjectCardToJSONReportsMissingColumn(t *testing.T) {
	s := fuzzRoutedServer(t)
	card, col := seedProjectCard(t, s)

	if _, ok := projectCardToJSON(card, s.store, "http://x"); !ok {
		t.Fatal("card with a live column reported as unrenderable")
	}

	s.store.Mu.Lock()
	delete(s.store.ProjectColumns, col.ID)
	s.store.Mu.Unlock()

	item, ok := projectCardToJSON(card, s.store, "http://x")
	if ok || item != nil {
		t.Fatalf("card with a deleted column rendered as %v", item)
	}
}

// ─── mutators that lose their target answer 404, not a nil dereference ───

func TestUpdateCampaignLosingTargetIsNotFound(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "campaign-org", "Campaign Org", "")
	if org == nil {
		t.Fatal("CreateOrg returned nil")
	}
	c := s.store.CreateCampaign(org.Login, "camp", "desc", nil, nil, fixedTestTime.UTC().Add(48*time.Hour), nil, nil)
	if c == nil {
		t.Fatal("CreateCampaign returned nil")
	}

	path := fmt.Sprintf("/api/v3/orgs/%s/campaigns/%d", org.Login, c.Number)
	w := serveWithConcurrentDelete(s, http.MethodPatch, path,
		map[string]interface{}{"name": "renamed"},
		func() { s.store.DeleteCampaign(org.Login, c.Number) })
	if w.Code != http.StatusNotFound {
		t.Fatalf("campaign PATCH racing a delete = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestUpdateNetworkConfigurationLosingTargetIsNotFound(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "netconf-org", "Netconf Org", "")
	if org == nil {
		t.Fatal("CreateOrg returned nil")
	}
	c, err := s.store.CreateNetworkConfiguration(org.Login, &store.NetworkConfigurationRequest{Name: strPtr("primary")})
	if err != nil {
		t.Fatalf("CreateNetworkConfiguration: %v", err)
	}

	path := fmt.Sprintf("/api/v3/orgs/%s/settings/network-configurations/%s", org.Login, c.ID)
	w := serveWithConcurrentDelete(s, http.MethodPatch, path,
		map[string]interface{}{"name": "renamed"},
		func() { s.store.DeleteNetworkConfiguration(org.Login, c.ID) })
	if w.Code != http.StatusNotFound {
		t.Fatalf("network configuration PATCH racing a delete = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestSetCodeSecurityConfigurationDefaultLosingTargetIsNotFound(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "codesec-org", "Codesec Org", "")
	if org == nil {
		t.Fatal("CreateOrg returned nil")
	}
	c := s.store.CreateCodeSecurityConfiguration(org.Login, &store.CodeSecurityConfigurationRequest{Name: strPtr("baseline")})
	if c == nil {
		t.Fatal("CreateCodeSecurityConfiguration returned nil")
	}

	path := fmt.Sprintf("/api/v3/orgs/%s/code-security/configurations/%d/defaults", org.Login, c.ID)
	w := serveWithConcurrentDelete(s, http.MethodPut, path,
		map[string]interface{}{"default_for_new_repos": "all"},
		func() { s.store.DeleteCodeSecurityConfiguration(org.Login, c.ID) })
	if w.Code != http.StatusNotFound {
		t.Fatalf("code security defaults PUT racing a delete = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestSetCodeSecurityConfigurationDefaultOnMissingConfigReturnsNil(t *testing.T) {
	s := fuzzRoutedServer(t)
	if got := s.store.SetCodeSecurityConfigurationAsDefault("nobody", 99999, "all"); got != nil {
		t.Fatalf("SetCodeSecurityConfigurationAsDefault on a missing config = %v, want nil", got)
	}
}

// ─── unknown search qualifiers are rejected, never silently dropped ──────

func TestSearchRejectsUnknownQualifierInsteadOfWideningResults(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	if s.store.CreateRepo(admin, "public-widget", "widget", false) == nil {
		t.Fatal("CreateRepo returned nil")
	}
	if s.store.CreateRepo(admin, "secret-widget", "widget", true) == nil {
		t.Fatal("CreateRepo returned nil")
	}

	cases := []struct{ name, query string }{
		// A typo'd is: value must not degrade into "no privacy filter".
		{"is-value", "is%3Aprivte"},
		{"in-value", "in%3Atitel"},
		{"state-value", "state%3Aopne"},
		{"type-value", "type%3Aorgs"},
		{"unknown-key", "not-a-real-qualifier%3Atrue"},
	}
	// repositories first: a dropped is:private is what discloses private repos.
	endpoints := []string{"repositories", "issues", "users", "code", "commits"}
	for _, tc := range cases {
		for _, ep := range endpoints {
			w := fuzzServe(s, http.MethodGet, "/api/v3/search/"+ep+"?q="+tc.query, nil)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("search/%s %s = %d, want 422: %s", ep, tc.name, w.Code, w.Body.String())
			}
			var body struct {
				Message string `json:"message"`
				Errors  []struct {
					Resource string `json:"resource"`
					Field    string `json:"field"`
					Code     string `json:"code"`
					Message  string `json:"message"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode 422 body: %v (%s)", err, w.Body.String())
			}
			if body.Message != "Validation Failed" || len(body.Errors) != 1 {
				t.Fatalf("search/%s %s 422 body = %s", ep, tc.name, w.Body.String())
			}
			if body.Errors[0].Resource != "Search" || body.Errors[0].Field != "q" || body.Errors[0].Code != "invalid" {
				t.Fatalf("search/%s %s error entry = %+v", ep, tc.name, body.Errors[0])
			}
			if body.Errors[0].Message == "" {
				t.Fatalf("search/%s %s 422 does not name the qualifier: %s", ep, tc.name, w.Body.String())
			}
		}
	}

	// The supported spelling still works and still filters.
	w := fuzzServe(s, http.MethodGet, "/api/v3/search/repositories?q=is%3Aprivate+widget", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("is:private status = %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		TotalCount int `json:"total_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.TotalCount != 1 {
		t.Fatalf("is:private total_count = %d, want 1: %s", env.TotalCount, w.Body.String())
	}
}

// ─── search results are totally ordered before pagination ────────────────

const searchOrderCorpus = 60
const searchOrderPerPage = 10
const searchOrderRepeats = 25

// assertStablePagination requests every page of a search repeatedly. Map
// iteration order is randomized per range statement, so an unordered result
// set reliably yields a different page across repeats and pages that overlap
// or drop entries.
func assertStablePagination(t *testing.T, s *Server, label, query string, wantTotal int) {
	t.Helper()
	rateRequest := httptest.NewRequest(http.MethodGet, query, nil)
	rateRequest.Header.Set("Authorization", "token "+defaultToken)
	resource := apiRateResource("/api/v3/" + label)
	if s.rateLimits == nil {
		s.rateLimits = map[string]*apiRateWindow{}
	}
	s.rateLimits[apiRateIdentity(rateRequest)+"\x1f"+resource] = &apiRateWindow{
		Limit:     apiRateResourceLimits[resource],
		Reset:     testRateLimitReset,
		unbounded: true,
	}
	pages := (wantTotal + searchOrderPerPage - 1) / searchOrderPerPage
	var first [][]float64
	for repeat := 0; repeat < searchOrderRepeats; repeat++ {
		var got [][]float64
		seen := map[float64]bool{}
		for page := 1; page <= pages; page++ {
			url := fmt.Sprintf("%s&per_page=%d&page=%d", query, searchOrderPerPage, page)
			ids := searchItemIDs(t, fuzzServe(s, http.MethodGet, url, nil))
			for _, id := range ids {
				if seen[id] {
					t.Fatalf("%s: id %v appears on more than one page (repeat %d)", label, id, repeat)
				}
				seen[id] = true
			}
			got = append(got, ids)
		}
		if len(seen) != wantTotal {
			t.Fatalf("%s: pagination covered %d of %d results (repeat %d)", label, len(seen), wantTotal, repeat)
		}
		if repeat == 0 {
			first = got
			continue
		}
		for page := range got {
			if fmt.Sprint(got[page]) != fmt.Sprint(first[page]) {
				t.Fatalf("%s: page %d differs between identical requests: %v vs %v",
					label, page+1, first[page], got[page])
			}
		}
	}
}

func TestSearchIssuesPaginationIsTotallyOrdered(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "order-repo", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	for i := 0; i < searchOrderCorpus; i++ {
		if s.store.CreateIssue(repo.ID, admin.ID, fmt.Sprintf("orderable %d", i), "body", nil, nil, 0) == nil {
			t.Fatalf("CreateIssue %d returned nil", i)
		}
	}
	assertStablePagination(t, s, "search/issues",
		"/api/v3/search/issues?q=orderable+repo:"+repo.FullName, searchOrderCorpus)
}

func TestSearchRepositoriesPaginationIsTotallyOrdered(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	for i := 0; i < searchOrderCorpus; i++ {
		if s.store.CreateRepo(admin, fmt.Sprintf("orderable-%d", i), "orderable corpus", false) == nil {
			t.Fatalf("CreateRepo %d returned nil", i)
		}
	}
	assertStablePagination(t, s, "search/repositories",
		"/api/v3/search/repositories?q=orderable", searchOrderCorpus)
}

func TestSearchUsersPaginationIsTotallyOrdered(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	total := 0
	s.store.Mu.Lock()
	for i := 0; i < searchOrderCorpus; i++ {
		u := &store.User{
			ID:           s.store.NextUser,
			NodeID:       fmt.Sprintf("U_kgDO%08d", s.store.NextUser),
			Login:        fmt.Sprintf("orderable-user-%d", i),
			Type:         "User",
			StarredRepos: map[string]bool{},
		}
		s.store.NextUser++
		s.store.Users[u.ID] = u
		s.store.UsersByLogin[u.Login] = u
		total++
	}
	s.store.Mu.Unlock()
	if s.store.CreateOrg(admin, "orderable-org", "", "") == nil {
		t.Fatal("CreateOrg returned nil")
	}
	total++
	assertStablePagination(t, s, "search/users", "/api/v3/search/users?q=orderable", total)
}
