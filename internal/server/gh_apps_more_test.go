package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- additional / refined App-management endpoints

func TestGetAppBySlug_AnonReadable(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	app := s.store.CreateApp(1, "Slug App", "desc", nil, nil)

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v3/apps/%s", app.Slug), nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["slug"] != app.Slug {
		t.Errorf("slug = %v, want %s", got["slug"], app.Slug)
	}
	if _, ok := got["pem"]; ok {
		t.Error("public app lookup leaked PEM")
	}
}

func TestGetAppBySlug_NotFound(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	req := httptest.NewRequest("GET", "/api/v3/apps/nope", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestSuspendUnsuspendInstallation(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	s.store.SeedDefaultUser()
	app := s.store.CreateApp(1, "Susp App", "", nil, nil)
	inst := s.store.CreateInstallation(app.ID, "User", 1, "admin", nil, nil)

	jwt, err := signAppJWT(app.PEMPrivateKey, app.ID, fixedTestTime)
	if err != nil {
		t.Fatal(err)
	}

	doRequest := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		w := httptest.NewRecorder()
		// Run through the same middleware the real server uses so JWT lands in ctx.
		s.ghHeadersMiddleware(s.mux).ServeHTTP(w, req)
		return w
	}

	// Suspend
	w := doRequest("PUT", fmt.Sprintf("/api/v3/app/installations/%d/suspended", inst.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("suspend status = %d body = %s", w.Code, w.Body.String())
	}
	got := s.store.GetInstallation(inst.ID)
	if got.SuspendedAt == nil {
		t.Fatal("expected SuspendedAt set after suspend")
	}

	// Duplicate suspend → 409
	w = doRequest("PUT", fmt.Sprintf("/api/v3/app/installations/%d/suspended", inst.ID))
	if w.Code != http.StatusConflict {
		t.Fatalf("dup suspend status = %d", w.Code)
	}

	// Token mint on suspended installation → 403
	w = doRequest("POST", fmt.Sprintf("/api/v3/app/installations/%d/access_tokens", inst.ID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("token-mint on suspended status = %d", w.Code)
	}

	// Unsuspend
	w = doRequest("DELETE", fmt.Sprintf("/api/v3/app/installations/%d/suspended", inst.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("unsuspend status = %d body = %s", w.Code, w.Body.String())
	}

	// Duplicate unsuspend → 409
	w = doRequest("DELETE", fmt.Sprintf("/api/v3/app/installations/%d/suspended", inst.ID))
	if w.Code != http.StatusConflict {
		t.Fatalf("dup unsuspend status = %d", w.Code)
	}
}

func TestGetOrgUserInstallation(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	s.store.SeedDefaultUser()
	app := s.store.CreateApp(1, "Org Inst App", "", nil, nil)
	s.store.CreateInstallation(app.ID, "Organization", 100, "octo-org", nil, nil)
	s.store.CreateInstallation(app.ID, "User", 200, "octocat", nil, nil)

	// Org installation
	req := httptest.NewRequest("GET", "/api/v3/orgs/octo-org/installation", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxApp, app))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("org installation status = %d body = %s", w.Code, w.Body.String())
	}

	// User installation
	req = httptest.NewRequest("GET", "/api/v3/users/octocat/installation", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxApp, app))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("user installation status = %d", w.Code)
	}

	// Wrong target type
	req = httptest.NewRequest("GET", "/api/v3/orgs/octocat/installation", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxApp, app))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("wrong target type status = %d, want 404", w.Code)
	}

	// Anon → 401
	req = httptest.NewRequest("GET", "/api/v3/orgs/octo-org/installation", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anon status = %d, want 401", w.Code)
	}

	otherApp := s.store.CreateApp(1, "Other Inst App", "", nil, nil)
	req = httptest.NewRequest("GET", "/api/v3/orgs/octo-org/installation", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxApp, otherApp))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("other app status = %d, want 404", w.Code)
	}
}

func TestUserInstallationRepos_AddRemove(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	s.store.SeedDefaultUser()
	user := s.store.UsersByLogin["admin"]
	app := s.store.CreateApp(user.ID, "Repo Sel App", "", nil, nil)

	r1 := s.store.CreateRepo(user, "r1", "", false)
	r2 := s.store.CreateRepo(user, "r2", "", false)

	inst := s.store.CreateInstallation(app.ID, "User", user.ID, user.Login, nil, nil)
	s.store.SetInstallationRepositorySelection(inst.ID, "selected", nil)

	put := func(repoID int) int {
		req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v3/user/installations/%d/repositories/%d", inst.ID, repoID), nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxUser, user))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		return w.Code
	}
	del := func(repoID int) int {
		req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v3/user/installations/%d/repositories/%d", inst.ID, repoID), nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxUser, user))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		return w.Code
	}

	if code := put(r1.ID); code != http.StatusNoContent {
		t.Fatalf("put r1 = %d", code)
	}
	if code := put(r2.ID); code != http.StatusNoContent {
		t.Fatalf("put r2 = %d", code)
	}
	got := s.store.GetInstallation(inst.ID)
	if got.RepositorySelection != "selected" {
		t.Errorf("RepositorySelection = %q, want selected", got.RepositorySelection)
	}
	if len(got.SelectedRepoIDs) != 2 {
		t.Errorf("SelectedRepoIDs len = %d, want 2", len(got.SelectedRepoIDs))
	}

	if code := del(r1.ID); code != http.StatusNoContent {
		t.Fatalf("del r1 = %d", code)
	}
	got = s.store.GetInstallation(inst.ID)
	if len(got.SelectedRepoIDs) != 1 || got.SelectedRepoIDs[0] != r2.ID {
		t.Errorf("after delete SelectedRepoIDs = %v, want [%d]", got.SelectedRepoIDs, r2.ID)
	}
}

func TestUserInstallationRepos_MutationBindsOwnerSelectionAndRepository(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	s.store.SeedDefaultUser()
	owner := s.store.UsersByLogin["admin"]

	s.store.mu.Lock()
	outsider := &User{ID: s.store.NextUser, Login: "installation-outsider", Type: "User"}
	s.store.NextUser++
	s.store.Users[outsider.ID] = outsider
	s.store.UsersByLogin[outsider.Login] = outsider
	s.store.mu.Unlock()

	app := s.store.CreateApp(owner.ID, "Bound Repo App", "", nil, nil)
	inst := s.store.CreateInstallation(app.ID, "User", owner.ID, owner.Login, nil, nil)
	ownedRepo := s.store.CreateRepo(owner, "owned", "", false)
	foreignRepo := s.store.CreateRepo(outsider, "foreign", "", false)

	request := func(user *User, repoID int) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v3/user/installations/%d/repositories/%d", inst.ID, repoID), nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxUser, user))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		return w
	}

	if w := request(owner, ownedRepo.ID); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("all-mode mutation status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	if got := s.store.GetInstallation(inst.ID); got.RepositorySelection != "all" || len(got.SelectedRepoIDs) != 0 {
		t.Fatalf("all-mode mutation changed installation: selection=%q repositories=%v", got.RepositorySelection, got.SelectedRepoIDs)
	}

	s.store.SetInstallationRepositorySelection(inst.ID, "selected", nil)
	if w := request(owner, foreignRepo.ID); w.Code != http.StatusNotFound {
		t.Fatalf("foreign-repository mutation status = %d, want 404", w.Code)
	}
	if w := request(outsider, ownedRepo.ID); w.Code != http.StatusNotFound {
		t.Fatalf("foreign-user mutation status = %d, want 404", w.Code)
	}
	if got := s.store.GetInstallation(inst.ID); len(got.SelectedRepoIDs) != 0 {
		t.Fatalf("denied mutations changed selected repositories: %v", got.SelectedRepoIDs)
	}
}

func TestOrganizationInstallationRepoMutationRequiresOwnerAndMatchingAppCredential(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	s.store.SeedDefaultUser()
	owner := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(owner, "installation-org", "Installation org", "")
	repo := s.store.CreateOrgRepo(org, owner, "selected-repo", "", true)

	s.store.mu.Lock()
	member := &User{ID: s.store.NextUser, Login: "installation-member", Type: "User"}
	s.store.NextUser++
	s.store.Users[member.ID] = member
	s.store.UsersByLogin[member.Login] = member
	s.store.mu.Unlock()
	s.store.SetMembership(org.Login, member.ID, OrgRoleMember, MembershipStateActive)

	app := s.store.CreateApp(owner.ID, "Organization Selection App", "", nil, nil)
	otherApp := s.store.CreateApp(owner.ID, "Uninstalled Selection App", "", nil, nil)
	inst := s.store.CreateInstallation(app.ID, "Organization", org.ID, org.Login, nil, nil)
	s.store.SetInstallationRepositorySelection(inst.ID, "selected", nil)

	request := func(user *User, token *UserToServerToken) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v3/user/installations/%d/repositories/%d", inst.ID, repo.ID), nil)
		ctx := context.WithValue(req.Context(), ctxUser, user)
		if token != nil {
			ctx = context.WithValue(ctx, ctxUserToServerToken, token)
		}
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		return w
	}

	if w := request(member, nil); w.Code != http.StatusNotFound {
		t.Fatalf("ordinary organization member status = %d, want 404", w.Code)
	}
	if w := request(owner, &UserToServerToken{AppID: otherApp.ID}); w.Code != http.StatusNotFound {
		t.Fatalf("wrong-app user token status = %d, want 404", w.Code)
	}
	if w := request(owner, &UserToServerToken{AppID: app.ID, InstallationIDs: []int{inst.ID}}); w.Code != http.StatusNoContent {
		t.Fatalf("owner with matching app token status = %d, want 204; body = %s", w.Code, w.Body.String())
	}
}

func TestInstallationRepoStoreRejectsInvalidState(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	owner := s.store.UsersByLogin["admin"]
	foreign := &User{ID: 4242, Login: "store-foreign", Type: "User"}
	s.store.mu.Lock()
	s.store.Users[foreign.ID] = foreign
	s.store.UsersByLogin[foreign.Login] = foreign
	s.store.mu.Unlock()
	ownedRepo := s.store.CreateRepo(owner, "store-owned", "", false)
	foreignRepo := s.store.CreateRepo(foreign, "store-foreign", "", false)
	app := s.store.CreateApp(owner.ID, "Store Selection App", "", nil, nil)
	inst := s.store.CreateInstallation(app.ID, "User", owner.ID, owner.Login, nil, nil)

	if added, ok := s.store.AddInstallationRepo(inst.ID, ownedRepo.ID); added || ok {
		t.Fatalf("all-mode store mutation = (%v, %v), want rejected", added, ok)
	}
	s.store.SetInstallationRepositorySelection(inst.ID, "selected", nil)
	if added, ok := s.store.AddInstallationRepo(inst.ID, foreignRepo.ID); added || ok {
		t.Fatalf("cross-owner store mutation = (%v, %v), want rejected", added, ok)
	}
}

func TestListInstallationRepositories_GhsToken(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	s.store.SeedDefaultUser()
	user := s.store.UsersByLogin["admin"]
	app := s.store.CreateApp(user.ID, "Inst Repos App", "", nil, nil)

	s.store.mu.Lock()
	octo := &User{ID: s.store.NextUser, Login: "octocat", Type: "User"}
	s.store.NextUser++
	s.store.Users[octo.ID] = octo
	s.store.UsersByLogin[octo.Login] = octo
	s.store.mu.Unlock()
	r1 := s.store.CreateRepo(octo, "r1", "", false)
	s.store.CreateRepo(octo, "r2", "", false)

	inst := s.store.CreateInstallation(app.ID, "User", octo.ID, "octocat", nil, nil)
	// Token scoped to just r1.
	tok := s.store.CreateInstallationToken(inst.ID, app.ID, nil, []int{r1.ID})

	req := httptest.NewRequest("GET", "/api/v3/installation/repositories", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	w := httptest.NewRecorder()
	s.ghHeadersMiddleware(s.mux).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		TotalCount   int              `json:"total_count"`
		Repositories []map[string]any `json:"repositories"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalCount != 1 {
		t.Errorf("total_count = %d, want 1 (scoped subset)", resp.TotalCount)
	}
	if len(resp.Repositories) > 0 && resp.Repositories[0]["name"] != "r1" {
		t.Errorf("first repo = %v, want r1", resp.Repositories[0]["name"])
	}
}
