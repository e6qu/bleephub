package bleephub

import (
	"net/http"
	"testing"
)

// GitHub lists installation access tokens among the credentials that may create
// a repository for the authenticated user, with the Administration (write)
// permission. An installation's authenticated user is the account it is
// installed on, so the repository belongs to that account; an organization
// installation has no such user and is refused.
func TestInstallationTokenCreatesRepositoryForItsUserAccount(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEntitlementFixture(t, "user-repos", false)

	contentsOnly := f.installationToken(t, map[string]string{"metadata": "read", "contents": "write"})
	resp := s.post(t, "/api/v3/user/repos", contentsOnly, map[string]interface{}{"name": "made-without-admin"})
	requireHTTPStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
	if s.store.GetRepo(f.owner.Login, "made-without-admin") != nil {
		t.Fatal("an installation without administration:write created a repository")
	}

	admin := f.installationToken(t, map[string]string{"metadata": "read", "administration": "write"})
	resp = s.post(t, "/api/v3/user/repos", admin, map[string]interface{}{"name": "made-by-app"})
	requireHTTPStatus(t, resp, http.StatusCreated)
	created := decodeJSON(t, resp)
	owner, _ := created["owner"].(map[string]interface{})
	if owner["login"] != f.owner.Login {
		t.Fatalf("repository owner = %v, want the installation's account %s", owner["login"], f.owner.Login)
	}
	if s.store.GetRepo(f.owner.Login, "made-by-app") == nil {
		t.Fatal("the created repository is not stored under the installation's account")
	}

	adminUser := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(adminUser, "app-repos-org", "App repos org", "")
	granted := map[string]string{"metadata": "read", "administration": "write"}
	orgInst := s.store.CreateInstallation(f.app.ID, "Organization", org.ID, org.Login, granted, nil)
	if orgInst == nil {
		t.Fatal("could not install the app on the organization")
	}
	orgToken := s.store.CreateInstallationToken(orgInst.ID, f.app.ID, granted, nil)
	resp = s.post(t, "/api/v3/user/repos", orgToken.Token, map[string]interface{}{"name": "made-for-org"})
	requireHTTPStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}
