package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Two credential shapes reached the self-gating surfaces — /api/graphql, which
// carries no {repo} in its route pattern, and the git smart-HTTP transports,
// which are not /api routes at all — carrying a selection of their own that
// nothing read.
//
// A gho_ OAuth-App user token carries classic scopes. The grant arm was guarded
// on the token having an installation behind it, and a gho_ has none, so a
// token holding no scopes at all deleted a repository, cloned and pushed a
// private one, and planted an organization webhook.
//
// A fine-grained token carries a repository selection and a permission map. The
// routed gate reads both; nothing on the self-gating surfaces did, so a token
// selecting one repository acted on every repository its bearer could reach.
//
// The two halves are asked asymmetrically of a gho_ and every case here depends
// on that: its scopes are checked, and which installation it reaches is not,
// because it reaches none. A reach test would refuse every OAuth app outright —
// so each refusal below is paired with the same shape genuinely holding the
// grant, asserted by the effect it had rather than by the status it got.

// credGrantCaller is one way of presenting a caller: a token in the
// Authorization header, or a browser session cookie. Sessions are here because
// over-blocking is the other half of what these cases pin, and a session is the
// shape that legitimately carries no selection at all.
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

// gitStatus advertises refs for one service, which is where both transports
// make their access decision.
func (c credGrantCaller) gitStatus(t *testing.T, repo *Repo, service string) int {
	t.Helper()
	status, _ := c.do(t, http.MethodGet, "/"+repo.FullName+".git/info/refs?service="+service, "")
	return status
}

// mutates runs a GraphQL mutation and reports whether the server accepted it.
// The caller checks the effect separately: a mutation that answers without
// errors and changes nothing is the failure mode this whole file exists for.
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

// credGrantFixture is one owner with an organization they administer. Each case
// takes the repositories it needs from it, so a destructive probe never decides
// the outcome of the next one.
type credGrantFixture struct {
	srv   *isolatedServer
	owner *User
	org   *Org
	seq   int
}

func (s *isolatedServer) newCredGrantFixture(t *testing.T, tag string) *credGrantFixture {
	t.Helper()
	st := s.store

	st.mu.Lock()
	now := fixedTestTime.UTC()
	owner := &User{
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
	st.mu.Unlock()

	// CreateOrg makes the creator an active admin, which is what leaves the
	// credential as the only thing standing on the organization routes.
	org := st.CreateOrg(owner, "credgrant-org-"+tag, "credential grant fixture", "")
	if org == nil {
		t.Fatalf("%s: could not create the organization", tag)
	}
	return &credGrantFixture{srv: s, owner: owner, org: org}
}

// repo creates a private repository owned by the fixture's account. Private,
// because a public one is readable by every credential ahead of the grant and
// the case would pass with the gate removed entirely.
func (f *credGrantFixture) repo(t *testing.T, name string) *Repo {
	t.Helper()
	f.seq++
	repo := f.srv.store.CreateRepo(f.owner, fmt.Sprintf("%s-%d", name, f.seq), "", true)
	if repo == nil {
		t.Fatalf("could not create the repository %s", name)
	}
	return repo
}

func (f *credGrantFixture) issue(t *testing.T, repo *Repo) *Issue {
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

// oauthToken mints a gho_ carrying exactly scopes. An empty string is a real
// credential shape: an OAuth app the user authorized for nothing.
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

// fineGrainedToken mints a fine-grained PAT over the owner's account, selecting
// exactly selected and holding perms over them.
func (f *credGrantFixture) fineGrainedToken(t *testing.T, perms map[string]string, selected ...*Repo) credGrantCaller {
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
	st.mu.Lock()
	tok.FineGrained = true
	tok.FineGrainedID = f.owner.ID*100 + f.seq
	tok.ResourceOwner = f.owner.Login
	tok.RepositorySelection = "subset"
	tok.RepositoryIDs = ids
	tok.Permissions = OrgPATPermissions{Repository: perms}
	st.mu.Unlock()
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
	if err := f.srv.store.PutLoginSession(id, &LoginSession{
		UserID:    f.owner.ID,
		ExpiresAt: fixedTestTime.Add(time.Hour),
	}); err != nil {
		t.Fatalf("could not open the browser session: %v", err)
	}
	return credGrantCaller{srv: f.srv, name: "browser session", cookie: id}
}

// credGrantServedRepo asserts the caller reaches the repository on both
// transports and through the mutation lane, and that the mutation landed.
func (s *isolatedServer) credGrantServedRepo(t *testing.T, c credGrantCaller, repo *Repo, issue *Issue) {
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

// credGrantRefusedRepo is its mirror: both transports refuse, and the mutation
// leaves the issue alone.
func (s *isolatedServer) credGrantRefusedRepo(t *testing.T, c credGrantCaller, repo *Repo, issue *Issue) {
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

// TestOAuthUserTokenScopesGateGitAndGraphQL is the gho_ half. A token holding no
// scopes at all belongs to the repository's own owner, so the principal half
// admits it and the grant is the only thing left.
func TestOAuthUserTokenScopesGateGitAndGraphQL(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCredGrantFixture(t, "gho-transports")

	refused := f.repo(t, "victim")
	s.credGrantRefusedRepo(t, f.oauthToken(t, ""), refused, f.issue(t, refused))

	// The control: the same shape holding the scope GitHub grants a clone, a
	// push and an issue edit under.
	served := f.repo(t, "entitled")
	s.credGrantServedRepo(t, f.oauthToken(t, "repo"), served, f.issue(t, served))

	// And the two shapes that carry no app selection of their own, which must be
	// exactly as unaffected as they were before.
	unaffected := f.repo(t, "classic")
	s.credGrantServedRepo(t, f.classicToken(t, "repo"), unaffected, f.issue(t, unaffected))

	viaSession := f.repo(t, "session")
	s.credGrantServedRepo(t, f.session(t), viaSession, f.issue(t, viaSession))
}

// TestOAuthUserTokenScopesGateRepositoryDeletion covers the admin level
// separately, because deleting the repository is what the case proves and a
// deleted repository cannot then serve the control.
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

	// delete_repo is the classic scope GitHub requires for this, and `repo`
	// carries repository administration at admin; either must still be served.
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

// TestOAuthUserTokenScopesGateOrganizationWebhooks is the account half of the
// same defect: an organization webhook is persistent exfiltration of every
// event the organization emits, and the handler gates itself on the caller's
// admin role alone.
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

// TestFineGrainedTokenSelectionGatesGitAndGraphQL is the second half: the
// selection is the whole point of a fine-grained token, and the surfaces that
// never reach requirePerm were not reading it.
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

	// The token holds every permission it could ask for. Only the selection
	// separates the two repositories, and they have the same owner, so nothing
	// but the selection can be doing the work.
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

	// The over-block guard: the credentials that carry no repository selection
	// still reach the repository the fine-grained token was refused.
	unaffected := f.repo(t, "classic")
	s.credGrantServedRepo(t, f.classicToken(t, "repo"), unaffected, f.issue(t, unaffected))

	viaSession := f.repo(t, "session")
	s.credGrantServedRepo(t, f.session(t), viaSession, f.issue(t, viaSession))
}

// TestUserToServerGrantIsNotDecidedByMapOrder pins the other half of the grant
// arm. An app installed in two places was checked against whichever
// installation Go's map iteration reached first, so the same token and the same
// scope answered differently from one request to the next — an authorization
// decision taken by coin flip, and one that no single-run test can catch.
func TestUserToServerGrantIsNotDecidedByMapOrder(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCredGrantFixture(t, "maporder")
	st := s.store
	repo := f.repo(t, "target")

	// One installation grants contents, the other does not. Only the first
	// covers the repository, so the entitled answer is unambiguous.
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
	// Enough attempts that a coin-flip gate cannot land the same way through
	// all of them: the pre-fix draw favoured one installation about nine times
	// in ten, which over this many requests is a vanishing chance of agreement.
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
