package bleephub

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The registered route table is the authority on what surface exists, so it is
// also the right thing to drive an authorization check from: a test that walks
// s.routePatterns cannot go stale when a route is added, the way a hand-written
// list of endpoints does.
//
// The property asserted here is deliberately narrow and absolute, so that it
// needs no per-endpoint judgement: a user who is not the owner, not a
// collaborator, not an org member and not a site admin must never receive a 2xx
// from a state-changing request against somebody else's private repository. Any
// 2xx is either a missing authorization check or a route that mutates state it
// was never asked to.

// authzMatrixSubstitute fills a registered pattern's wildcards with values that
// resolve: the target repository for {owner}/{repo}, and an innocuous literal
// everywhere else. Unresolvable ids are fine — the handler is expected to
// refuse on authorization before it ever looks the id up, and a 404 from a
// missing id is an accepted outcome.
func authzMatrixSubstitute(pattern, owner, repo, org string) (method, path string, ok bool) {
	method, rest, found := strings.Cut(pattern, " ")
	if !found {
		return "", "", false
	}
	segments := strings.Split(rest, "/")
	for i, seg := range segments {
		if !strings.HasPrefix(seg, "{") {
			continue
		}
		name := strings.Trim(seg, "{}")
		name = strings.TrimSuffix(name, "...")
		switch name {
		case "org":
			// A real organization, not the owner's user login: a path naming an
			// organization that does not exist is refused before any handler
			// runs, so substituting a non-org here would make every org-scoped
			// route a guaranteed 404 and prove nothing.
			segments[i] = org
		case "owner", "username", "enterprise":
			segments[i] = owner
		case "repo":
			segments[i] = repo
		case "$":
			segments[i] = ""
		default:
			// Numeric-looking parameters get a number so the handler's own
			// parse succeeds and it reaches its authorization check rather
			// than bouncing off a 400.
			segments[i] = "1"
		}
	}
	return method, strings.Join(segments, "/"), true
}

func TestPrivateRepoMutationsRejectAnUnrelatedUser(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const ownerLogin = "authzmatrix-owner"
	const repoName = "authzmatrix-private"

	store := srv.store

	now := fixedTestTime.UTC()
	store.Mu.Lock()
	owner := &User{ID: store.NextUser, Login: ownerLogin, Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[owner.ID] = owner
	store.UsersByLogin[owner.Login] = owner
	store.NextUser++
	stranger := &User{ID: store.NextUser, Login: "authzmatrix-stranger", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[stranger.ID] = stranger
	store.UsersByLogin[stranger.Login] = stranger
	store.NextUser++
	store.Mu.Unlock()

	if store.CreateRepo(owner, repoName, "private fixture", true) == nil {
		t.Fatalf("could not create the private fixture repository")
	}
	strangerToken := store.CreateToken(stranger.ID, "repo, workflow, read:org, admin:org, gist")
	if strangerToken == nil {
		t.Fatalf("could not mint a token for the unrelated user")
	}

	const orgLogin = "authzmatrix-org"
	if store.GetOrg(orgLogin) == nil && store.CreateOrg(owner, orgLogin, "Authz Matrix Org", "") == nil {
		t.Fatalf("could not create the fixture organization")
	}

	handler := srv.requestHandler()

	var admitted []string
	for _, pattern := range srv.routePatterns {
		if !strings.Contains(pattern, "/api/v3/repos/{owner}/{repo}") {
			continue
		}
		method, path, ok := authzMatrixSubstitute(pattern, ownerLogin, repoName, orgLogin)
		if !ok {
			continue
		}
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}

		req := httptest.NewRequest(method, path, bytes.NewReader([]byte("{}")))
		req.Header.Set("Authorization", "token "+strangerToken.Value)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code >= 200 && w.Code < 300 {
			admitted = append(admitted, pattern+" -> "+http.StatusText(w.Code))
		}
	}

	if len(admitted) > 0 {
		t.Errorf("%d state-changing routes admitted a user with no access to a private repository:", len(admitted))
		for _, entry := range admitted {
			t.Errorf("  %s", entry)
		}
	}

	// Positive control, and a floor on it.
	//
	// Most iterations above cannot distinguish "denied" from "that sub-resource
	// does not exist": the substitution fills {issue_number}, {secret_name} and
	// friends with a literal 1, so the handler 404s for anybody, authorized or
	// not. Those iterations are guaranteed passes and they are the majority.
	//
	// Sweeping the same routes as the owner of a repository they fully control
	// measures how many actually reach a handler that would have said yes. If
	// that number collapses — because a refactor made more routes 404 on a
	// missing id, or because the gate started refusing the owner — this test
	// quietly stops proving anything, so it fails instead.
	ownerRepo := "authzmatrix-owned"
	if store.CreateRepo(owner, ownerRepo, "public fixture", false) == nil {
		t.Fatalf("could not create the owner's fixture repository")
	}
	ownerToken := store.CreateToken(owner.ID, "repo, workflow, read:org, admin:org, gist")

	discriminating := 0
	for _, pattern := range srv.routePatterns {
		if !strings.Contains(pattern, "/api/v3/repos/{owner}/{repo}") {
			continue
		}
		method, path, ok := authzMatrixSubstitute(pattern, ownerLogin, ownerRepo, orgLogin)
		if !ok {
			continue
		}
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}
		// The sweep is destructive: `DELETE /repos/{owner}/{repo}` succeeds
		// like any other owner-authorized mutation, and every later iteration
		// would then be measuring a repository that no longer exists.
		if store.GetRepo(ownerLogin, ownerRepo) == nil && store.CreateRepo(owner, ownerRepo, "public fixture", false) == nil {
			t.Fatalf("could not restore the owner's fixture repository")
		}
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte("{}")))
		req.Header.Set("Authorization", "token "+ownerToken.Value)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code >= 200 && w.Code < 300 {
			discriminating++
		}
	}

	// Measured at 22 when this floor was written. Set below that so ordinary
	// churn does not trip it, high enough that a collapse does.
	const minDiscriminating = 15
	if discriminating < minDiscriminating {
		t.Errorf("only %d of the swept routes answer their own owner 2xx (want at least %d): "+
			"the rest cannot tell a denial from a missing sub-resource, so this test is "+
			"no longer proving what it claims", discriminating, minDiscriminating)
	}
	t.Logf("mutation sweep: %d routes discriminate (owner answered 2xx)", discriminating)
}

// TestPrivateRepoReadsRejectAnUnrelatedUser is the read-side companion. A
// private repository must not disclose its contents, refs, labels, protection
// settings or CI results to a user with no access, and it must answer the same
// way it answers for a repository that does not exist so the response cannot be
// used to prove the repository is there.
func TestPrivateRepoReadsRejectAnUnrelatedUser(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const ownerLogin = "authzread-owner"
	const repoName = "authzread-private"

	store := srv.store

	now := fixedTestTime.UTC()
	store.Mu.Lock()
	owner := &User{ID: store.NextUser, Login: ownerLogin, Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[owner.ID] = owner
	store.UsersByLogin[owner.Login] = owner
	store.NextUser++
	store.Mu.Unlock()

	if store.CreateRepo(owner, repoName, "private fixture", true) == nil {
		t.Fatalf("could not create the private fixture repository")
	}
	_, strangerToken := authflowStranger(t, srv.Server, authflowName("authzread-stranger"))

	handler := srv.requestHandler()

	// Anonymous, which is the sharpest case: no credential at all.
	base := "/api/v3/repos/" + ownerLogin + "/" + repoName
	paths := []string{
		base + "/contents/README.md",
		base + "/commits",
		base + "/readme",
		base + "/branches",
		base + "/tags",
		base + "/labels",
		base + "/milestones",
		base + "/git/matching-refs/",
		base + "/issues",
		base + "/pulls",
		base + "/releases",
		base + "/deployments",
		base + "/environments",
		base + "/hooks",
		base + "/collaborators",
		base + "/branches/main/protection",
		base + "/actions/workflows",
		base + "/actions/runs",
		base + "/actions/secrets",
		base + "/actions/variables",
		base + "/private-vulnerability-reporting",
		base + "/stargazers",
		base + "/import",
		base + "/check-suites/1",
		base + "/code-scanning/alerts",
		base + "/secret-scanning/alerts",
		base + "/dependabot/alerts",
	}

	var disclosed []string
	for _, credential := range []struct {
		name  string
		token string
	}{
		{name: "anonymous"},
		{name: "unrelated classic PAT", token: strangerToken},
	} {
		for _, path := range paths {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if credential.token != "" {
				req.Header.Set("Authorization", "token "+credential.token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code >= 200 && w.Code < 300 {
				disclosed = append(disclosed, credential.name+" GET "+path)
			}
		}
	}

	if len(disclosed) > 0 {
		t.Errorf("%d private-repository reads answered a caller with no access:", len(disclosed))
		for _, entry := range disclosed {
			t.Errorf("  %s", entry)
		}
	}

	// Positive control. Without it a 404 above could just mean the fixture is
	// empty — an endpoint that always 404s would look perfectly gated. The
	// owner must be served, so the anonymous 404 is a denial and not an
	// artifact of the repository having no content.
	ownerToken := store.CreateToken(owner.ID, "repo")
	for _, path := range []string{
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/labels",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/branches",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/private-vulnerability-reporting",
		"/api/v3/repos/" + ownerLogin + "/" + repoName + "/stargazers",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "token "+ownerToken.Value)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code < 200 || w.Code >= 300 {
			t.Errorf("owner GET %s = %d, want 2xx — the gate is refusing the owner too", path, w.Code)
		}
	}
}

// TestGraphQLDeleteRepositoryRejectsANonAdmin covers the GraphQL side of the
// same property the REST matrix above asserts.
//
// GraphQL mutations checked that a caller was signed in and then acted, so the
// schema was an authorization bypass around REST: the REST delete-repository
// handler calls canAdminRepo, and the mutation reaching the same store did not.
func TestGraphQLDeleteRepositoryRejectsANonAdmin(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	store := srv.store

	now := fixedTestTime.UTC()
	store.Mu.Lock()
	owner := &User{ID: store.NextUser, Login: "gqldel-owner", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[owner.ID] = owner
	store.UsersByLogin[owner.Login] = owner
	store.NextUser++
	stranger := &User{ID: store.NextUser, Login: "gqldel-stranger", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[stranger.ID] = stranger
	store.UsersByLogin[stranger.Login] = stranger
	store.NextUser++
	store.Mu.Unlock()

	repo := store.CreateRepo(owner, "gqldel-target", "", false)
	if repo == nil {
		t.Fatalf("could not create the fixture repository")
	}
	strangerToken := store.CreateToken(stranger.ID, "repo")

	resp := srv.post(t, "/api/graphql", strangerToken.Value, map[string]interface{}{
		"query": `mutation($id:ID!){deleteRepository(input:{repositoryId:$id}){clientMutationId}}`,
		"variables": map[string]interface{}{
			"id": repo.NodeID,
		},
	})
	defer resp.Body.Close()

	body := decodeJSON(t, resp)
	errs, _ := body["errors"].([]interface{})
	if len(errs) == 0 {
		t.Fatalf("deleteRepository by a non-admin returned no error: %v", body)
	}
	if store.GetRepoByFullName(repo.FullName) == nil {
		t.Fatalf("repository was deleted by a user with no admin rights")
	}
}

// TestInstallationTokenCannotReachAnotherAccountsRepo closes the hole an
// adversarial review of this branch demonstrated.
//
// requirePerm originally ran its resource check only for the classic-PAT and
// session branch. An installation token was checked against its own permission
// map — which is self-declared and says nothing about which repository the
// request names — and then admitted. An app installed on the attacker's own
// account therefore reached 30 mutating routes on a stranger's private
// repository, including PUT .../collaborators/{username}, which is persistent
// access, and the runner registration-token endpoint.
//
// The mirror image mattered too: the read gate asked canReadRepoAsUser about the
// synthetic bot user standing in for the installation, and that bot is a
// collaborator on nothing, so apps lost reads they were entitled to.
func TestInstallationTokenCannotReachAnotherAccountsRepo(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	store := srv.store

	now := fixedTestTime.UTC()
	store.Mu.Lock()
	attacker := &User{ID: store.NextUser, Login: "instscope-attacker", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[attacker.ID] = attacker
	store.UsersByLogin[attacker.Login] = attacker
	store.NextUser++
	victim := &User{ID: store.NextUser, Login: "instscope-victim", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[victim.ID] = victim
	store.UsersByLogin[victim.Login] = victim
	store.NextUser++
	store.Mu.Unlock()

	victimRepo := store.CreateRepo(victim, "instscope-private", "", true)
	attackerRepo := store.CreateRepo(attacker, "instscope-own", "", true)
	if victimRepo == nil || attackerRepo == nil {
		t.Fatalf("could not create the fixture repositories")
	}

	// An app installed on the attacker's own account, with broad permissions.
	perms := map[string]string{
		"administration": "write", "contents": "write", "issues": "write",
		"metadata": "read", "secrets": "write",
	}
	app := store.CreateApp(attacker.ID, "instscope-app", "", perms, nil)
	inst := store.CreateInstallation(app.ID, "User", attacker.ID, attacker.Login, perms, nil)
	tok := store.CreateInstallationToken(inst.ID, app.ID, perms, nil)
	if tok == nil {
		t.Fatalf("could not mint an installation token")
	}

	handler := srv.requestHandler()

	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte("{}")))
		req.Header.Set("Authorization", "token "+tok.Token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}

	// Against the victim's repository: never a success.
	victimBase := "/api/v3/repos/" + victim.Login + "/" + victimRepo.Name
	for _, probe := range []struct{ method, path string }{
		{http.MethodPut, victimBase + "/collaborators/instscope-attacker"},
		{http.MethodPost, victimBase + "/actions/runners/registration-token"},
		{http.MethodPut, victimBase + "/branches/main/protection"},
		{http.MethodPost, victimBase + "/labels"},
		{http.MethodGet, victimBase + "/labels"},
	} {
		if code := do(probe.method, probe.path); code >= 200 && code < 300 {
			t.Errorf("%s %s = %d: an installation on another account reached this repository",
				probe.method, probe.path, code)
		}
	}

	// Against its own account's repository the installation must still work,
	// or the fix has simply broken GitHub Apps instead of scoping them.
	ownBase := "/api/v3/repos/" + attacker.Login + "/" + attackerRepo.Name
	if code := do(http.MethodGet, ownBase+"/labels"); code < 200 || code >= 300 {
		t.Errorf("GET %s/labels = %d, want 2xx — the installation is entitled to its own account's repository", ownBase, code)
	}
}

// TestOrgMutationsRejectAnUnrelatedUser sweeps the organization surface.
//
// The repository sweep above missed an entire class: several org routes also
// carry {repository_id}, and the resource check used to resolve the repository
// and return its verdict *instead of* the organization's. Because the id comes
// from the caller, naming a repository you own let you administer a stranger's
// organization — a 204 where the same call against the victim's own repository
// id gave 403, which is what made the misdirected check visible.
func TestOrgMutationsRejectAnUnrelatedUser(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	store := srv.store

	now := fixedTestTime.UTC()
	store.Mu.Lock()
	stranger := &User{ID: store.NextUser, Login: "orgsweep-stranger", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[stranger.ID] = stranger
	store.UsersByLogin[stranger.Login] = stranger
	store.NextUser++
	victim := &User{ID: store.NextUser, Login: "orgsweep-victim", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[victim.ID] = victim
	store.UsersByLogin[victim.Login] = victim
	store.NextUser++
	store.Mu.Unlock()

	org := store.CreateOrg(victim, "orgsweep-org", "Orgsweep", "")
	if org == nil {
		t.Fatalf("could not create the fixture organization")
	}
	// A repository the stranger genuinely administers. This is the lever: the
	// check must not accept it as authorization over somebody else's org.
	ownRepo := store.CreateRepo(stranger, "orgsweep-own", "", false)
	if ownRepo == nil {
		t.Fatalf("could not create the stranger's repository")
	}
	strangerToken := store.CreateToken(stranger.ID, "repo, workflow, read:org, admin:org, gist")

	handler := srv.requestHandler()
	ownRepoID := strconv.Itoa(ownRepo.ID)

	var admitted []string
	for _, pattern := range srv.routePatterns {
		if !strings.Contains(pattern, "/api/v3/orgs/{org}") {
			continue
		}
		method, rest, found := strings.Cut(pattern, " ")
		if !found {
			continue
		}
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}
		segments := strings.Split(rest, "/")
		for i, seg := range segments {
			if !strings.HasPrefix(seg, "{") {
				continue
			}
			name := strings.TrimSuffix(strings.Trim(seg, "{}"), "...")
			switch name {
			case "org":
				segments[i] = org.Login
			case "repository_id":
				segments[i] = ownRepoID
			case "username":
				segments[i] = stranger.Login
			case "$":
				segments[i] = ""
			default:
				segments[i] = "1"
			}
		}
		path := strings.Join(segments, "/")

		req := httptest.NewRequest(method, path, bytes.NewReader([]byte("{}")))
		req.Header.Set("Authorization", "token "+strangerToken.Value)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code >= 200 && w.Code < 300 {
			admitted = append(admitted, method+" "+path+" -> "+http.StatusText(w.Code))
		}
	}

	if len(admitted) > 0 {
		t.Errorf("%d organization routes admitted a user with no role in the organization:", len(admitted))
		for _, entry := range admitted {
			t.Errorf("  %s", entry)
		}
	}
}

// TestPublicRepositoryBypassIsKeyedOnTheScope pins the fix for the bypass an
// adversarial pass demonstrated twice.
//
// A public repository does not make every scope on it public: the Actions
// secrets and variables listings are registered under scopeSecrets at read
// level and return secret names and variable *values*. A bypass keyed only on
// "the repository is public" handed those to any credential holding
// secrets:read, for every public repository on the instance. The exploit used a
// fine-grained PAT belonging to a different owner, with no repository ids and
// no permissions at all.
//
// Nothing tested this, which is why it was marked fixed once while still live.
func TestPublicRepositoryBypassIsKeyedOnTheScope(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	store := srv.store

	now := fixedTestTime.UTC()
	store.Mu.Lock()
	owner := &User{ID: store.NextUser, Login: "scopebypass-owner", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[owner.ID] = owner
	store.UsersByLogin[owner.Login] = owner
	store.NextUser++
	outsider := &User{ID: store.NextUser, Login: "scopebypass-outsider", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[outsider.ID] = outsider
	store.UsersByLogin[outsider.Login] = outsider
	store.NextUser++
	store.Mu.Unlock()

	repo := store.CreateRepo(owner, "scopebypass-public", "", false)
	if repo == nil {
		t.Fatalf("could not create the public fixture repository")
	}
	// A fine-grained PAT for a different owner, scoped to nothing at all.
	tok := store.CreateToken(outsider.ID, "")
	store.Mu.Lock()
	tok.FineGrained = true
	tok.ResourceOwner = outsider.Login
	tok.RepositorySelection = "subset"
	store.Mu.Unlock()

	handler := srv.requestHandler()
	base := "/api/v3/repos/" + owner.Login + "/" + repo.Name

	// The first three are the discriminating probes: scopeActions is neither
	// allowed by publicReadAllowed nor promoted to admin by
	// resourceCapabilityFor, so the PAT carve-out is the only gate standing
	// between this caller and a 200. The secrets and variables listings the
	// original exploit used are asserted too, but they cannot fail this test
	// on their own — scopeSecrets is admin-promoted, so the resource gate
	// refuses the outsider before the carve-out is ever consulted.
	for _, path := range []string{
		base + "/actions/permissions",
		base + "/actions/permissions/workflow",
		base + "/actions/permissions/access",
		base + "/actions/variables",
		base + "/actions/secrets",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "token "+tok.Value)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code >= 200 && w.Code < 300 {
			t.Errorf("GET %s = %d for a PAT scoped to another owner with no permissions; body=%s",
				path, w.Code, w.Body.String())
		}
	}

	// Control: a scope that genuinely is public on a public repository must
	// still be served, or the bypass has simply been deleted rather than keyed.
	req := httptest.NewRequest(http.MethodGet, base+"/labels", nil)
	req.Header.Set("Authorization", "token "+tok.Value)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code < 200 || w.Code >= 300 {
		t.Errorf("GET %s/labels = %d, want 2xx — issues are public on a public repository", base, w.Code)
	}
}
