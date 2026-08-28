package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Guard the self-gating surfaces — /api/graphql (no {repo} in its route) and
// the git smart-HTTP transports (not /api routes) — that never read a token's
// scopes or selection. A scopeless gho_ once deleted repos, cloned/pushed
// private ones, and planted org webhooks; a fine-grained PAT acted on repos
// outside its selection. Each refusal is paired with the same shape genuinely
// holding the grant, asserted by effect rather than status, since a reach test
// would refuse every OAuth app outright.

// credGrantCaller presents a caller as a token or a browser session cookie.
// Sessions are the shape that legitimately carries no selection at all, so they
// pin the over-blocking half.
type credGrantCaller struct {
	srv    *isolatedServer
	name   string
	token  string
	cookie string
}

func (c credGrantCaller) apply(r *http.Request) {
	if c.token != "" {
		r.Header.Set("Authorization", "token "+c.token)
	}
	if c.cookie != "" {
		r.AddCookie(&http.Cookie{Name: "_gh_sess", Value: c.cookie})
	}
}

func (c credGrantCaller) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.srv.baseURL+path, reader)
	if err != nil {
		t.Fatalf("%s: building %s %s: %v", c.name, method, path, err)
	}
	c.apply(req)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", c.name, method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// gitStatus advertises refs for one service, where both transports make their
// access decision.
func (c credGrantCaller) gitStatus(t *testing.T, repo *store.Repo, service string) int {
	t.Helper()
	status, _ := c.do(t, http.MethodGet, "/"+repo.FullName+".git/info/refs?service="+service, "")
	return status
}

// mutates reports whether the server accepted a GraphQL mutation; the caller
// checks the effect separately, since a mutation that answers without errors
// and changes nothing is the failure mode this file exists for.
func (c credGrantCaller) mutates(t *testing.T, doc string, input map[string]interface{}) bool {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"query":     doc,
		"variables": map[string]interface{}{"input": input},
	})
	if err != nil {
		t.Fatalf("%s: encoding the mutation: %v", c.name, err)
	}
	status, body := c.do(t, http.MethodPost, "/api/graphql", string(payload))
	if status != http.StatusOK {
		return false
	}
	var env struct {
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("%s: decoding the mutation response %q: %v", c.name, body, err)
	}
	return len(env.Errors) == 0
}

const (
	credGrantUpdateIssue = `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`
	credGrantDeleteRepo  = `mutation($input:DeleteRepositoryInput!){deleteRepository(input:$input){clientMutationId}}`
)

// credGrantFixture is one owner with an organization they administer; each case
// takes fresh repositories so a destructive probe never decides the next case.
type credGrantFixture struct {
	srv   *isolatedServer
	owner *store.User
	org   *store.Org
	seq   int
}

func (s *isolatedServer) newCredGrantFixture(t *testing.T, tag string) *credGrantFixture {
	t.Helper()
	st := s.store

	st.Mu.Lock()
	now := fixedTestTime.UTC()
	owner := &store.User{
		ID:        st.NextUser,
		NodeID:    fmt.Sprintf("U_credgrant%08d", st.NextUser),
		Login:     "credgrant-" + tag,
		Type:      "User",
		CreatedAt: now,
		UpdatedAt: now,
	}
	st.Users[owner.ID] = owner
	st.UsersByLogin[owner.Login] = owner
	st.NextUser++
	st.Mu.Unlock()

	// CreateOrg makes the creator an active admin, leaving the credential as the
	// only thing standing on the organization routes.
	org := st.CreateOrg(owner, "credgrant-org-"+tag, "credential grant fixture", "")
	if org == nil {
		t.Fatalf("%s: could not create the organization", tag)
	}
	return &credGrantFixture{srv: s, owner: owner, org: org}
}

// repo creates a private repository: a public one is readable by every
// credential ahead of the grant, so the case would pass with the gate removed.
func (f *credGrantFixture) repo(t *testing.T, name string) *store.Repo {
	t.Helper()
	f.seq++
	repo := f.srv.store.CreateRepo(f.owner, fmt.Sprintf("%s-%d", name, f.seq), "", true)
	if repo == nil {
		t.Fatalf("could not create the repository %s", name)
	}
	return repo
}

func (f *credGrantFixture) issue(t *testing.T, repo *store.Repo) *store.Issue {
	t.Helper()
	issue := f.srv.store.CreateIssue(repo.ID, f.owner.ID, "before", "", nil, nil, 0)
	if issue == nil {
		t.Fatalf("could not seed an issue on %s", repo.FullName)
	}
	if issue.NodeID == "" {
		t.Fatalf("the issue on %s has no node id, so the mutation cannot address it", repo.FullName)
	}
	return issue
}

// oauthToken mints a gho_ carrying exactly scopes; an empty string is a real
// shape: an OAuth app the user authorized for nothing.
func (f *credGrantFixture) oauthToken(t *testing.T, scopes string) credGrantCaller {
	t.Helper()
	f.seq++
	tok, _ := f.srv.store.CreateUserToServerToken(
		f.owner.ID, 0, fmt.Sprintf("Iv1.credgrant%s%d", f.owner.Login, f.seq), scopes, time.Hour, false)
	if tok == nil {
		t.Fatal("could not mint the gho_ token")
	}
	if tok.AppID != 0 {
		t.Fatalf("the fixture token must be a gho_ with no app behind it, got AppID %d", tok.AppID)
	}
	return credGrantCaller{srv: f.srv, name: "gho_ scopes=" + fmt.Sprintf("%q", scopes), token: tok.Token}
}

func (f *credGrantFixture) fineGrainedToken(t *testing.T, perms map[string]string, selected ...*store.Repo) credGrantCaller {
	t.Helper()
	st := f.srv.store
	f.seq++
	tok := st.CreateToken(f.owner.ID, "")
	if tok == nil {
		t.Fatal("could not mint the fine-grained token")
	}
	ids := make([]int, 0, len(selected))
	names := make([]string, 0, len(selected))
	for _, repo := range selected {
		ids = append(ids, repo.ID)
		names = append(names, repo.Name)
	}
	st.Mu.Lock()
	tok.FineGrained = true
	tok.FineGrainedID = f.owner.ID*100 + f.seq
	tok.ResourceOwner = f.owner.Login
	tok.RepositorySelection = "subset"
	tok.RepositoryIDs = ids
	tok.Permissions = store.OrgPATPermissions{Repository: perms}
	st.Mu.Unlock()
	return credGrantCaller{srv: f.srv, name: "fine-grained selecting " + strings.Join(names, ","), token: tok.Value}
}

func (f *credGrantFixture) classicToken(t *testing.T, scopes string) credGrantCaller {
	t.Helper()
	tok := f.srv.store.CreateToken(f.owner.ID, scopes)
	if tok == nil {
		t.Fatal("could not mint the classic token")
	}
	return credGrantCaller{srv: f.srv, name: "classic PAT scopes=" + fmt.Sprintf("%q", scopes), token: tok.Value}
}

func (f *credGrantFixture) session(t *testing.T) credGrantCaller {
	t.Helper()
	f.seq++
	id := fmt.Sprintf("credgrant-sess-%s-%d", f.owner.Login, f.seq)
	if err := f.srv.store.PutLoginSession(id, &store.LoginSession{
		UserID:    f.owner.ID,
		ExpiresAt: fixedTestTime.Add(time.Hour),
	}); err != nil {
		t.Fatalf("could not open the browser session: %v", err)
	}
	return credGrantCaller{srv: f.srv, name: "browser session", cookie: id}
}

// credGrantServedRepo asserts the caller reaches the repository on both
// transports and through the mutation lane, and that the mutation landed in the store.
func (s *isolatedServer) credGrantServedRepo(t *testing.T, c credGrantCaller, repo *store.Repo, issue *store.Issue) {
	t.Helper()
	if got := c.gitStatus(t, repo, "git-upload-pack"); got != http.StatusOK {
		t.Errorf("%s: upload-pack on %s = %d, want 200", c.name, repo.FullName, got)
	}
	if got := c.gitStatus(t, repo, "git-receive-pack"); got != http.StatusOK {
		t.Errorf("%s: receive-pack on %s = %d, want 200", c.name, repo.FullName, got)
	}
	title := "retitled by " + c.name
	if !c.mutates(t, credGrantUpdateIssue, map[string]interface{}{"id": issue.NodeID, "title": title}) {
		t.Errorf("%s: updateIssue on %s was refused", c.name, repo.FullName)
	}
	if got := s.store.GetIssue(issue.ID); got == nil || got.Title != title {
		t.Errorf("%s: updateIssue on %s reported success but the title did not change", c.name, repo.FullName)
	}
}

// credGrantRefusedRepo is its mirror: both transports refuse and the mutation
// leaves the issue untouched.
func (s *isolatedServer) credGrantRefusedRepo(t *testing.T, c credGrantCaller, repo *store.Repo, issue *store.Issue) {
	t.Helper()
	if got := c.gitStatus(t, repo, "git-upload-pack"); got == http.StatusOK {
		t.Errorf("%s: upload-pack on %s = 200; it cloned a private repository", c.name, repo.FullName)
	}
	if got := c.gitStatus(t, repo, "git-receive-pack"); got == http.StatusOK {
		t.Errorf("%s: receive-pack on %s = 200; it may push", c.name, repo.FullName)
	}
	before := "before"
	if c.mutates(t, credGrantUpdateIssue, map[string]interface{}{"id": issue.NodeID, "title": "hijacked"}) {
		t.Errorf("%s: updateIssue on %s succeeded", c.name, repo.FullName)
	}
	if got := s.store.GetIssue(issue.ID); got == nil || got.Title != before {
		t.Errorf("%s: the issue on %s was retitled", c.name, repo.FullName)
	}
}

// TestOAuthUserTokenScopesGateGitAndGraphQL: a scopeless gho_ belongs to the
// repo's own owner, so the principal half admits it and the grant is all that's left.
func TestOAuthUserTokenScopesGateGitAndGraphQL(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCredGrantFixture(t, "gho-transports")

	refused := f.repo(t, "victim")
	s.credGrantRefusedRepo(t, f.oauthToken(t, ""), refused, f.issue(t, refused))

	// Control: the same shape holding the scope GitHub grants a clone, push and
	// issue edit under.
	served := f.repo(t, "entitled")
	s.credGrantServedRepo(t, f.oauthToken(t, "repo"), served, f.issue(t, served))

	// The two shapes carrying no app selection of their own must stay unaffected.
	unaffected := f.repo(t, "classic")
	s.credGrantServedRepo(t, f.classicToken(t, "repo"), unaffected, f.issue(t, unaffected))

	viaSession := f.repo(t, "session")
	s.credGrantServedRepo(t, f.session(t), viaSession, f.issue(t, viaSession))
}

// TestOAuthUserTokenScopesGateRepositoryDeletion covers the admin level
// separately, since a deleted repository cannot then serve the control.
func TestOAuthUserTokenScopesGateRepositoryDeletion(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCredGrantFixture(t, "gho-delete")
	st := s.store

	doomed := f.repo(t, "scopeless-target")
	scopeless := f.oauthToken(t, "")
	if scopeless.mutates(t, credGrantDeleteRepo, map[string]interface{}{"repositoryId": doomed.NodeID}) {
		t.Errorf("%s: deleteRepository succeeded", scopeless.name)
	}
	if st.GetRepo(f.owner.Login, doomed.Name) == nil {
		t.Errorf("%s: the repository was deleted", scopeless.name)
	}

	// delete_repo is the classic scope GitHub requires here, and `repo` carries
	// repository administration; either must still be served.
	for _, scopes := range []string{"repo", "delete_repo"} {
		target := f.repo(t, "entitled-target")
		entitled := f.oauthToken(t, scopes)
		if !entitled.mutates(t, credGrantDeleteRepo, map[string]interface{}{"repositoryId": target.NodeID}) {
			t.Errorf("%s: deleteRepository was refused", entitled.name)
		}
		if st.GetRepo(f.owner.Login, target.Name) != nil {
			t.Errorf("%s: deleteRepository reported success but the repository is still there", entitled.name)
		}
	}

	viaSession := f.repo(t, "session-target")
	session := f.session(t)
	if !session.mutates(t, credGrantDeleteRepo, map[string]interface{}{"repositoryId": viaSession.NodeID}) {
		t.Errorf("%s: deleteRepository was refused", session.name)
	}
	if st.GetRepo(f.owner.Login, viaSession.Name) != nil {
		t.Errorf("%s: the owner's own session could not delete their repository", session.name)
	}
}

// TestOAuthUserTokenScopesGateOrganizationWebhooks: an org webhook is persistent
// exfiltration of every event the org emits, and the handler once gated itself
// on the caller's admin role alone.
func TestOAuthUserTokenScopesGateOrganizationWebhooks(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCredGrantFixture(t, "gho-hooks")
	path := "/api/v3/orgs/" + f.org.Login + "/hooks"
	body := func(url string) string {
		return `{"name":"web","active":true,"events":["push"],"config":{"url":"` + url + `","content_type":"json"}}`
	}
	planted := func(url string) bool {
		for _, hook := range s.store.ListOrgHooks(f.org.Login) {
			if hook.URL == url {
				return true
			}
		}
		return false
	}

	const attacker = "https://127.0.0.1:9/attacker-collector"
	scopeless := f.oauthToken(t, "")
	if status, respBody := scopeless.do(t, http.MethodPost, path, body(attacker)); status >= 200 && status < 300 {
		t.Errorf("%s: POST %s = %d; body=%s", scopeless.name, path, status, respBody)
	}
	if planted(attacker) {
		t.Errorf("%s: the webhook was created", scopeless.name)
	}
	for _, c := range []credGrantCaller{
		f.oauthToken(t, "admin:org"),
		f.classicToken(t, "admin:org"),
	} {
		url := "https://127.0.0.1:9/wrong-scope/" + strings.NewReplacer(" ", "-", `"`, "", "=", "-").Replace(c.name)
		if status, respBody := c.do(t, http.MethodPost, path, body(url)); status != http.StatusForbidden {
			t.Errorf("%s: POST %s = %d, want 403 without admin:org_hook; body=%s", c.name, path, status, respBody)
		}
		if planted(url) {
			t.Errorf("%s: the webhook was created with admin:org but no admin:org_hook", c.name)
		}
	}

	for _, c := range []credGrantCaller{
		f.oauthToken(t, "admin:org_hook"),
		f.classicToken(t, "admin:org_hook"),
		f.session(t),
	} {
		url := "https://127.0.0.1:9/legitimate/" + strings.NewReplacer(" ", "-", `"`, "", "=", "-").Replace(c.name)
		if status, respBody := c.do(t, http.MethodPost, path, body(url)); status != http.StatusCreated {
			t.Errorf("%s: POST %s = %d, want 201; body=%s", c.name, path, status, respBody)
		}
		if !planted(url) {
			t.Errorf("%s: POST %s reported success but no webhook was stored", c.name, path)
		}
	}
}

// TestFineGrainedTokenSelectionGatesGitAndGraphQL: the surfaces that never reach
// requirePerm were not reading the token's repository selection.
func TestFineGrainedTokenSelectionGatesGitAndGraphQL(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCredGrantFixture(t, "finegrained")
	st := s.store

	selected := f.repo(t, "selected")
	unselected := f.repo(t, "unselected")
	full := map[string]string{
		"metadata": "read", "contents": "write", "issues": "write", "administration": "admin",
	}
	token := f.fineGrainedToken(t, full, selected)

	// The token holds every permission and both repos share an owner, so only the
	// selection can separate them.
	s.credGrantRefusedRepo(t, token, unselected, f.issue(t, unselected))
	s.credGrantServedRepo(t, token, selected, f.issue(t, selected))

	doomed := f.repo(t, "unselected-target")
	if token.mutates(t, credGrantDeleteRepo, map[string]interface{}{"repositoryId": doomed.NodeID}) {
		t.Errorf("%s: deleteRepository succeeded on a repository it does not select", token.name)
	}
	if st.GetRepo(f.owner.Login, doomed.Name) == nil {
		t.Errorf("%s: a repository it does not select was deleted", token.name)
	}

	target := f.repo(t, "selected-target")
	selecting := f.fineGrainedToken(t, full, selected, target)
	if !selecting.mutates(t, credGrantDeleteRepo, map[string]interface{}{"repositoryId": target.NodeID}) {
		t.Errorf("%s: deleteRepository was refused on a repository it selects", selecting.name)
	}
	if st.GetRepo(f.owner.Login, target.Name) != nil {
		t.Errorf("%s: deleteRepository reported success but the repository is still there", selecting.name)
	}

	// Over-block guard: credentials carrying no repository selection still reach
	// the repository the fine-grained token was refused.
	unaffected := f.repo(t, "classic")
	s.credGrantServedRepo(t, f.classicToken(t, "repo"), unaffected, f.issue(t, unaffected))

	viaSession := f.repo(t, "session")
	s.credGrantServedRepo(t, f.session(t), viaSession, f.issue(t, viaSession))
}

// TestUserToServerGrantIsNotDecidedByMapOrder: an app installed in two places
// was checked against whichever installation Go's map iteration reached first,
// so the same token answered differently from one request to the next.
func TestUserToServerGrantIsNotDecidedByMapOrder(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCredGrantFixture(t, "maporder")
	st := s.store
	repo := f.repo(t, "target")

	// One installation grants contents, the other does not; only the first covers
	// the repository, so the entitled answer is unambiguous.
	app := st.CreateApp(f.owner.ID, "Credential Grant Map Order", "",
		map[string]string{"metadata": "read", "contents": "write", "members": "read"}, nil)
	if app == nil {
		t.Fatal("could not register the app")
	}
	if st.CreateInstallation(app.ID, "User", f.owner.ID, f.owner.Login,
		map[string]string{"metadata": "read", "contents": "write"}, nil) == nil {
		t.Fatal("could not install the app on the owner")
	}
	if st.CreateInstallation(app.ID, "Organization", f.org.ID, f.org.Login,
		map[string]string{"metadata": "read", "members": "read"}, nil) == nil {
		t.Fatal("could not install the app on the organization")
	}
	uts, _ := st.CreateUserToServerToken(f.owner.ID, app.ID, "", "", time.Hour, false)
	if uts == nil {
		t.Fatal("could not mint the ghu_ token")
	}
	caller := credGrantCaller{srv: s, name: "ghu_ of an app installed twice", token: uts.Token}

	seen := map[int]int{}
	// Enough attempts that a coin-flip gate cannot land the same way through all
	// of them (the pre-fix draw favoured one installation about nine times in ten).
	const attempts = 100
	for i := 0; i < attempts; i++ {
		seen[caller.gitStatus(t, repo, "git-upload-pack")]++
	}
	if len(seen) != 1 {
		t.Errorf("%s: upload-pack answered %v across %d identical requests; the grant is being decided by map order",
			caller.name, seen, attempts)
	}
	if seen[http.StatusOK] != attempts {
		t.Errorf("%s: upload-pack answered %v; the installation covering this repository grants contents:write",
			caller.name, seen)
	}
}
