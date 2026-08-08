package bleephub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/rs/zerolog"
)

// TEST-008 migration scaffolding.
//
// The package-wide `testServer` is a single live server shared by every test,
// so tests are coupled through its mutable store and cannot run in parallel.
// newIsolatedServer builds a fully-routed server backed by its own httptest
// listener and its own store, seeded exactly like the shared one (NewServer
// registers every route and seeds the admin user; the clock is pinned to
// fixedTestTime). A test that switches to it can call t.Parallel() because it
// shares no request-visible state with any other test.
//
// The migration is incremental: this coexists with `testServer`, and files are
// converted a group at a time.
type isolatedServer struct {
	*Server
	baseURL string
}

func newIsolatedServer(t *testing.T) *isolatedServer {
	t.Helper()
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	useFixedTestClock(s)
	ts := httptest.NewServer(s.requestHandler())
	t.Cleanup(ts.Close)
	return &isolatedServer{Server: s, baseURL: ts.URL}
}

func (s *isolatedServer) do(t *testing.T, method, path, token string, body interface{}) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.baseURL+path, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// get/post/patch mirror the package ghGet/ghPost/ghPatch helpers but target
// this instance's listener instead of the shared base URL. put/delete/etc. are
// added the same way (via do) as converted files need them.
func (s *isolatedServer) get(t *testing.T, path, token string) *http.Response {
	return s.do(t, http.MethodGet, path, token, nil)
}

func (s *isolatedServer) post(t *testing.T, path, token string, body interface{}) *http.Response {
	return s.do(t, http.MethodPost, path, token, body)
}

func (s *isolatedServer) patch(t *testing.T, path, token string, body interface{}) *http.Response {
	return s.do(t, http.MethodPatch, path, token, body)
}

func (s *isolatedServer) put(t *testing.T, path, token string, body interface{}) *http.Response {
	return s.do(t, http.MethodPut, path, token, body)
}

func (s *isolatedServer) delete(t *testing.T, path, token string) *http.Response {
	return s.do(t, http.MethodDelete, path, token, nil)
}

// authedGet/authedPost mirror the package authedGet/authedPost helpers, which
// present the admin credential as a Bearer token against this instance's
// listener. authedPost keeps the drop-in http.Post-style signature (no *testing.T,
// returns the error) so converted call sites change only their receiver.
func (s *isolatedServer) authedGet(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (s *isolatedServer) authedPost(path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, s.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	return http.DefaultClient.Do(req)
}

// createOrg mirrors the package createOrgViaAdminAPI helper against this
// instance's admin organizations endpoint, so a converted public-feature test
// provisions its org through the same operator API on its own server.
func (s *isolatedServer) createOrg(t *testing.T, login string, profileName ...string) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{"login": login, "admin": "admin"}
	if len(profileName) > 0 && profileName[0] != "" {
		body["profile_name"] = profileName[0]
	}
	resp := s.post(t, "/api/v3/admin/organizations", defaultToken, body)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("POST /api/v3/admin/organizations for %s = %d, want 201", login, resp.StatusCode)
	}
	return decodeJSON(t, resp)
}

// createTestOrg/createTestRepo/createTestUser/activateOrgMember/newUser are
// per-instance mirrors of the identically named package helpers, so files that
// provision orgs/repos/users through them can convert without touching the
// shared server. The package versions remain for files not yet migrated.
func (s *isolatedServer) createTestOrg(t *testing.T) string {
	t.Helper()
	login := "test-org-actions-" + strconv.FormatInt(int64(nextTestID()), 36)
	s.createOrg(t, login, "Test Org Actions")
	return login
}

func (s *isolatedServer) createTestRepo(t *testing.T) string {
	t.Helper()
	name := "test-repo-actions-" + strconv.FormatInt(int64(nextTestID()), 36)
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":    name,
		"private": false,
	})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	return "admin/" + name
}

func (s *isolatedServer) createTestUser(t *testing.T, login string) *User {
	t.Helper()
	resp, err := s.authedPost("/internal/users", "application/json", bytes.NewReader(mustJSON(map[string]interface{}{
		"login": login,
		"email": login + "@example.com",
	})))
	if err != nil {
		t.Fatalf("create user %s: %v", login, err)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create user %s: %d %s", login, resp.StatusCode, b)
	}
	resp.Body.Close()
	return s.store.UsersByLogin[login]
}

func (s *isolatedServer) createOrgRepoForGovernance(t *testing.T, org string) (string, int) {
	t.Helper()
	name := "gov-repo-" + strconv.FormatInt(int64(nextTestID()), 36)
	resp := s.post(t, "/api/v3/orgs/"+org+"/repos", defaultToken, map[string]interface{}{
		"name":    name,
		"private": false,
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create org repo: %d", resp.StatusCode)
	}
	repo := decodeJSON(t, resp)
	return name, int(repo["id"].(float64))
}

func (s *isolatedServer) activateOrgMember(t *testing.T, orgLogin, login, memberToken string) {
	t.Helper()
	expectStatus(t, s.put(t, "/api/v3/orgs/"+orgLogin+"/memberships/"+login, defaultToken,
		map[string]interface{}{"role": "member"}), http.StatusOK, "PUT membership "+login)
	expectStatus(t, s.patch(t, "/api/v3/user/memberships/orgs/"+orgLogin, memberToken,
		map[string]interface{}{"state": "active"}), http.StatusOK, "accept membership "+login)
}

// newUser mirrors the package newSharedServerUser helper against this instance's
// store: a fresh user with a deterministic token, provisioned directly.
func (s *isolatedServer) newUser(t *testing.T, login string) (*User, string) {
	t.Helper()
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	if existing := st.UsersByLogin[login]; existing != nil {
		t.Fatalf("user %q already exists", login)
	}
	u := &User{ID: st.NextUser, Login: login, Type: "User"}
	st.NextUser++
	st.Users[u.ID] = u
	st.UsersByLogin[login] = u
	tok := &Token{Value: "ghp_" + login + "0000000000000000000000000000000000", UserID: u.ID, Scopes: "repo,read:org"}
	st.Tokens[tok.Value] = tok
	return u, tok.Value
}

// seedRepo mirrors the package seedTestRepo helper on this instance's own
// store: an admin-owned repo, created idempotently so a converted test can call
// it without coupling to any shared fixture.
func (s *isolatedServer) seedRepo(t *testing.T, name string, private bool) *Repo {
	t.Helper()
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("default admin user missing")
	}
	if repo := s.store.GetRepo("admin", name); repo != nil {
		return repo
	}
	repo := s.store.CreateRepo(admin, name, "", private)
	if repo == nil {
		t.Fatalf("CreateRepo %s failed", name)
	}
	return repo
}

// TestIsolatedServersAreIndependentAndParallelSafe pins the invariant the whole
// migration relies on: two isolated servers share no store state, so mutations
// in one are invisible to the other and both can run under t.Parallel().
func TestIsolatedServersAreIndependentAndParallelSafe(t *testing.T) {
	t.Parallel()
	a := newIsolatedServer(t)
	b := newIsolatedServer(t)

	resp := a.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "iso-only-on-a"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo on server A: got %d, want 201", resp.StatusCode)
	}

	// The repo exists on A...
	onA := a.get(t, "/api/v3/repos/admin/iso-only-on-a", defaultToken)
	onA.Body.Close()
	if onA.StatusCode != http.StatusOK {
		t.Fatalf("repo on its own server: got %d, want 200", onA.StatusCode)
	}
	// ...and not on the independent server B.
	onB := b.get(t, "/api/v3/repos/admin/iso-only-on-a", defaultToken)
	onB.Body.Close()
	if onB.StatusCode != http.StatusNotFound {
		t.Fatalf("repo leaked into an independent server: got %d, want 404", onB.StatusCode)
	}
}
