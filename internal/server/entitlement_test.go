package bleephub

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// An app credential answers two questions, not one. "Is this installation on
// this repository" is reachability; "was this app granted this kind of act" is
// entitlement. A gate that asks only the first admits an app holding nothing
// but the metadata:read every installation gets for free to everything the
// installation can reach — repository deletion through the GraphQL lane, and a
// clone or a push of a private repository over git.
//
// Every case below is paired. The refusal is the bug; the control beside it is
// an app that *was* granted the permission and must still be served, because
// over-blocking has been the other recurring regression on this surface.

// entitlementFixture is one repository owned by one account, with an app
// installed on that account. Each test mints its own tokens from it so a token
// carries exactly the grant the case is about.
type entitlementFixture struct {
	store *Store
	owner *User
	repo  *Repo
	app   *App
	inst  *Installation
}

// metadataOnly is the grant every GitHub App installation carries whether it
// asked for it or not, and is therefore the floor an entitlement check has to
// refuse at.
var metadataOnly = map[string]string{"metadata": "read"}

func (s *isolatedServer) newEntitlementFixture(t *testing.T, tag string, private bool) *entitlementFixture {
	t.Helper()
	st := s.store

	owner := st.LookupUserByLogin("entitle-owner-" + tag)
	if owner == nil {
		st.Mu.Lock()
		now := fixedTestTime.UTC()
		owner = &User{
			ID:        st.NextUser,
			NodeID:    fmt.Sprintf("U_entitle%08d", st.NextUser),
			Login:     "entitle-owner-" + tag,
			Type:      "User",
			CreatedAt: now,
			UpdatedAt: now,
		}
		st.Users[owner.ID] = owner
		st.UsersByLogin[owner.Login] = owner
		st.NextUser++
		st.Mu.Unlock()
	}

	repo := st.CreateRepo(owner, "entitle-repo-"+tag, "", private)
	if repo == nil {
		t.Fatalf("fixture %s: could not create the repository", tag)
	}
	// The app is registered with every permission the cases draw on. What each
	// case varies is the *token*, so a refusal is never an artefact of the app
	// being unable to ask for the permission in the first place.
	granted := map[string]string{
		"metadata":       "read",
		"administration": "write",
		"issues":         "write",
		"contents":       "write",
	}
	app := st.CreateApp(owner.ID, "Entitlement Probe "+tag, "", granted, nil)
	if app == nil {
		t.Fatalf("fixture %s: could not register the app", tag)
	}
	inst := st.CreateInstallation(app.ID, "User", owner.ID, owner.Login, granted, nil)
	if inst == nil {
		t.Fatalf("fixture %s: could not install the app on the owner", tag)
	}
	return &entitlementFixture{store: s.store, owner: owner, repo: repo, app: app, inst: inst}
}

// installationToken mints a ghs_ carrying exactly perms, over every repository
// the installation covers.
func (f *entitlementFixture) installationToken(t *testing.T, perms map[string]string) string {
	t.Helper()
	tok := f.store.CreateInstallationToken(f.inst.ID, f.app.ID, perms, nil)
	if tok == nil {
		t.Fatal("could not mint an installation token")
	}
	return tok.Token
}

func (s *isolatedServer) entitlementGQLErrors(t *testing.T, token, doc string, input map[string]interface{}) []interface{} {
	t.Helper()
	env := s.gqlAuthzPost(t, token, doc, map[string]interface{}{"input": input})
	return gqlAuthzErrors(env)
}

// TestGraphQLAdminMutationNeedsTheAdministrationGrant is the proof case: a
// metadata-only installation token deleted a repository outright, because the
// admin arm asked only whether the installation reached it.
func TestGraphQLAdminMutationNeedsTheAdministrationGrant(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	doc := `mutation($input:DeleteRepositoryInput!){deleteRepository(input:$input){clientMutationId}}`

	refused := s.newEntitlementFixture(t, "gql-admin-deny", true)
	token := refused.installationToken(t, metadataOnly)
	if errs := s.entitlementGQLErrors(t, token, doc, map[string]interface{}{"repositoryId": refused.repo.NodeID}); len(errs) == 0 {
		t.Error("deleteRepository succeeded for an installation holding only metadata:read")
	}
	if s.store.GetRepo(refused.owner.Login, refused.repo.Name) == nil {
		t.Error("the repository was deleted by an installation holding only metadata:read")
	}

	allowed := s.newEntitlementFixture(t, "gql-admin-allow", true)
	entitled := allowed.installationToken(t, map[string]string{"metadata": "read", "administration": "write"})
	if errs := s.entitlementGQLErrors(t, entitled, doc, map[string]interface{}{"repositoryId": allowed.repo.NodeID}); len(errs) != 0 {
		t.Errorf("deleteRepository refused an installation holding administration:write: %v", errs)
	}
	if s.store.GetRepo(allowed.owner.Login, allowed.repo.Name) != nil {
		t.Error("deleteRepository reported success but the repository is still there")
	}
}

// TestGraphQLPushMutationNeedsTheIssuesGrant is the same defect one level down,
// and it is why the scope belongs on the policy row: an app entitled to issues
// must be served even though it holds no contents grant at all.
func TestGraphQLPushMutationNeedsTheIssuesGrant(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	doc := `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`

	f := s.newEntitlementFixture(t, "gql-push", true)
	issue := s.store.CreateIssue(f.repo.ID, f.owner.ID, "before", "body", nil, nil, 0)
	if issue == nil {
		t.Fatal("could not seed the issue")
	}

	token := f.installationToken(t, metadataOnly)
	if errs := s.entitlementGQLErrors(t, token, doc, map[string]interface{}{"id": issue.NodeID, "title": "after"}); len(errs) == 0 {
		t.Error("updateIssue succeeded for an installation holding only metadata:read")
	}
	if got := s.store.GetIssue(issue.ID); got != nil && got.Title != "before" {
		t.Errorf("issue title = %q; a metadata-only installation edited it", got.Title)
	}

	entitled := f.installationToken(t, map[string]string{"metadata": "read", "issues": "write"})
	if errs := s.entitlementGQLErrors(t, entitled, doc, map[string]interface{}{"id": issue.NodeID, "title": "after"}); len(errs) != 0 {
		t.Errorf("updateIssue refused an installation holding issues:write: %v", errs)
	}
	if got := s.store.GetIssue(issue.ID); got == nil || got.Title != "after" {
		t.Error("updateIssue reported success but the title did not change")
	}
}

// TestAuthorExemptionDoesNotBypassTheCredential pins the ordering. Authorship
// says something about the bearer and nothing about the app speaking for them,
// so it may relax the standing the bearer needs on the repository and must not
// relax the grant. Returning early above the credential check let the author of
// an issue retitle it through an app installed nowhere.
func TestAuthorExemptionDoesNotBypassTheCredential(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	doc := `mutation($input:UpdateIssueInput!){updateIssue(input:$input){issue{title}}}`

	f := s.newEntitlementFixture(t, "author", false)

	st.Mu.Lock()
	now := fixedTestTime.UTC()
	author := &User{
		ID:        st.NextUser,
		NodeID:    fmt.Sprintf("U_entitleauth%08d", st.NextUser),
		Login:     "entitle-author",
		Type:      "User",
		CreatedAt: now,
		UpdatedAt: now,
	}
	st.Users[author.ID] = author
	st.UsersByLogin[author.Login] = author
	st.NextUser++
	st.Mu.Unlock()

	issue := st.CreateIssue(f.repo.ID, author.ID, "authored", "body", nil, nil, 0)
	if issue == nil {
		t.Fatal("could not seed the authored issue")
	}

	// An app with no installation at all: the bearer wrote the issue and holds
	// no push on the repository.
	strangerApp := st.CreateApp(author.ID, "Entitlement Author Probe", "", metadataOnly, nil)
	if strangerApp == nil {
		t.Fatal("could not register the uninstalled app")
	}
	uts, _ := st.CreateUserToServerToken(author.ID, strangerApp.ID, "", "", time.Hour, false)
	if uts == nil {
		t.Fatal("could not mint the user-to-server token")
	}
	if errs := s.entitlementGQLErrors(t, uts.Token, doc, map[string]interface{}{"id": issue.NodeID, "title": "renamed"}); len(errs) == 0 {
		t.Error("the issue's author retitled it through an app installed nowhere")
	}
	if got := st.GetIssue(issue.ID); got != nil && got.Title != "authored" {
		t.Errorf("issue title = %q; the author exemption bypassed the credential", got.Title)
	}

	// The control: the same author acting as themselves, with no app in the
	// way, still edits their own issue without holding push.
	pat := st.CreateToken(author.ID, "repo")
	if pat == nil {
		t.Fatal("could not mint the author's token")
	}
	if errs := s.entitlementGQLErrors(t, pat.Value, doc, map[string]interface{}{"id": issue.NodeID, "title": "renamed"}); len(errs) != 0 {
		t.Errorf("the author was refused an edit of their own issue: %v", errs)
	}
	if got := st.GetIssue(issue.ID); got == nil || got.Title != "renamed" {
		t.Error("the author's own edit did not take effect")
	}
}

func (s *isolatedServer) entitlementGitStatus(t *testing.T, method, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, s.baseURL+url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestGitTransportsNeedTheContentsGrant covers the other proven consequence: a
// metadata-only installation token cloned and pushed a private repository,
// because the git routes sit outside the /api middleware and had nothing but
// reachability to go on.
func TestGitTransportsNeedTheContentsGrant(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEntitlementFixture(t, "git", true)
	base := "/" + f.owner.Login + "/" + f.repo.Name + ".git/info/refs?service="

	token := f.installationToken(t, metadataOnly)
	if got := s.entitlementGitStatus(t, "GET", base+"git-upload-pack", token); got != http.StatusNotFound {
		t.Errorf("upload-pack for a metadata-only installation = %d, want 404 (no existence oracle)", got)
	}
	if got := s.entitlementGitStatus(t, "GET", base+"git-receive-pack", token); got != http.StatusNotFound {
		t.Errorf("receive-pack for a metadata-only installation = %d, want 404 (no existence oracle)", got)
	}

	reader := f.installationToken(t, map[string]string{"metadata": "read", "contents": "read"})
	if got := s.entitlementGitStatus(t, "GET", base+"git-upload-pack", reader); got != http.StatusOK {
		t.Errorf("upload-pack for an installation holding contents:read = %d, want 200", got)
	}
	if got := s.entitlementGitStatus(t, "GET", base+"git-receive-pack", reader); got != http.StatusForbidden {
		t.Errorf("receive-pack for an installation holding only contents:read = %d, want 403", got)
	}

	writer := f.installationToken(t, map[string]string{"metadata": "read", "contents": "write"})
	if got := s.entitlementGitStatus(t, "GET", base+"git-upload-pack", writer); got != http.StatusOK {
		t.Errorf("upload-pack for an installation holding contents:write = %d, want 200", got)
	}
	if got := s.entitlementGitStatus(t, "GET", base+"git-receive-pack", writer); got != http.StatusOK {
		t.Errorf("receive-pack for an installation holding contents:write = %d, want 200", got)
	}
}

// TestOrgMemberListingIntersectsTheCredential covers the self-entitlement
// class: the handler read the bearer's own membership row directly, so a
// user-to-server token of an app installed nowhere saw the private member list
// its own installation token could not.
func TestOrgMemberListingIntersectsTheCredential(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	org := s.seedTestOrg(t, "entitle-members-org")

	st.Mu.Lock()
	now := fixedTestTime.UTC()
	member := &User{
		ID:        st.NextUser,
		NodeID:    fmt.Sprintf("U_entitlemem%08d", st.NextUser),
		Login:     "entitle-private-member",
		Type:      "User",
		CreatedAt: now,
		UpdatedAt: now,
	}
	st.Users[member.ID] = member
	st.UsersByLogin[member.Login] = member
	st.NextUser++
	st.Mu.Unlock()
	st.SetMembership(org.Login, member.ID, OrgRoleMember, MembershipStateActive)

	admin := st.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("default admin user missing")
	}

	listing := func(token string) []map[string]interface{} {
		t.Helper()
		resp := s.get(t, "/api/v3/orgs/"+org.Login+"/members", token)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("GET /orgs/%s/members = %d, want 200", org.Login, resp.StatusCode)
		}
		return decodeJSONArray(t, resp)
	}
	contains := func(entries []map[string]interface{}, login string) bool {
		for _, entry := range entries {
			if entry["login"] == login {
				return true
			}
		}
		return false
	}

	// admin is an active member of the org, so the private list is theirs to
	// see — as themselves.
	if !contains(listing(defaultToken), member.Login) {
		t.Fatal("an active member was refused the org's own member list")
	}

	uninstalled := st.CreateApp(admin.ID, "Entitlement Members Probe", "", map[string]string{"members": "read"}, nil)
	if uninstalled == nil {
		t.Fatal("could not register the uninstalled app")
	}
	uts, _ := st.CreateUserToServerToken(admin.ID, uninstalled.ID, "", "", time.Hour, false)
	if uts == nil {
		t.Fatal("could not mint the user-to-server token")
	}
	if contains(listing(uts.Token), member.Login) {
		t.Error("a user-to-server token of an app installed nowhere read the org's private member list")
	}

	installedApp := st.CreateApp(admin.ID, "Entitlement Members Grant", "", map[string]string{"members": "read"}, nil)
	if installedApp == nil {
		t.Fatal("could not register the installed app")
	}
	if st.CreateInstallation(installedApp.ID, "Organization", org.ID, org.Login, map[string]string{"members": "read"}, nil) == nil {
		t.Fatal("could not install the app on the org")
	}
	installedUTS, _ := st.CreateUserToServerToken(admin.ID, installedApp.ID, "", "", time.Hour, false)
	if installedUTS == nil {
		t.Fatal("could not mint the installed app's user-to-server token")
	}
	if !contains(listing(installedUTS.Token), member.Login) {
		t.Error("a user-to-server token of an app installed on the org was refused its member list")
	}
}
