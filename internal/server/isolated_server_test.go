package bleephub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
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

// repoRef identifies a repository by its owner and name. The isolated repo
// helpers return one instead of a bare string so a converted test cannot make
// the two mistakes that produced silent 404s and "admin/admin/<name>" during
// the TEST-008 migration: hand-building a "/api/v3/repos/<owner>/<name>" path,
// or passing a full "owner/name" where only the name (or only the owner) was
// meant. Consumers use path()/fullName()/owner/name; the compiler rejects
// concatenating a repoRef into a URL.
type repoRef struct {
	owner, name string
}

// fullName is "owner/name" (e.g. for GraphQL nameWithOwner or a body field).
func (r repoRef) fullName() string { return r.owner + "/" + r.name }

// path is the REST resource path "/api/v3/repos/owner/name"; append subpaths.
func (r repoRef) path() string { return "/api/v3/repos/" + r.owner + "/" + r.name }

func newIsolatedServer(t *testing.T) *isolatedServer {
	t.Helper()
	s := NewServer("127.0.0.1:0", zerolog.Nop())
	useFixedTestClock(s)
	// Package upload routes persist bytes under PackageDataDir; the shared
	// harness sets one in TestMain, so give each isolated server its own
	// per-test temp dir (auto-cleaned, so parallel-safe).
	s.store.PackageDataDir = t.TempDir()
	// Feed the shared OpenAPI shape observer (PAR-011) so isolated-server
	// tests contribute to the parity coverage instead of eroding it as the
	// TEST-008 migration moves traffic off the shared harness. The observer is
	// mutex-guarded, so concurrent isolated servers feed it safely.
	if apiShapeValidator != nil {
		s.responseObserver = apiShapeValidator.Observe
	}
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
	login := "test-org-actions-" + strconv.FormatInt(int64(testutil.NextTestID()), 36)
	s.createOrg(t, login, "Test Org Actions")
	return login
}

func (s *isolatedServer) createTestRepo(t *testing.T) repoRef {
	t.Helper()
	name := "test-repo-actions-" + strconv.FormatInt(int64(testutil.NextTestID()), 36)
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
	return repoRef{owner: "admin", name: name}
}

func (s *isolatedServer) createTestUser(t *testing.T, login string) *store.User {
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

// createIssueForTest mirrors the package helper: open an issue on repo (which is
// "admin/<name>" from createTestRepo) and return its (id, number).
func (s *isolatedServer) createIssueForTest(t *testing.T, repo repoRef, title string) (int, int) {
	t.Helper()
	resp := s.post(t, repo.path()+"/issues", defaultToken, map[string]interface{}{"title": title})
	data := decodeJSONWithStatus(t, resp, 201)
	return int(data["id"].(float64)), int(data["number"].(float64))
}

// gqlDo/gqlData mirror the package GraphQL helpers against this instance.
func (s *isolatedServer) gqlDo(t *testing.T, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	resp := s.post(t, "/api/graphql", defaultToken, body)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("graphql status = %d", resp.StatusCode)
	}
	return decodeJSON(t, resp)
}

func (s *isolatedServer) gqlData(t *testing.T, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	env := s.gqlDo(t, query, variables)
	if errs, ok := env["errors"]; ok && errs != nil {
		t.Fatalf("graphql errors: %v", errs)
	}
	d, _ := env["data"].(map[string]interface{})
	if d == nil {
		t.Fatalf("no data in response: %v", env)
	}
	return d
}

// sweepRepo/sweepPR mirror the package helpers against this instance: create a
// repo with a seeded feature branch, and open a PR on it.
func (s *isolatedServer) sweepRepo(t *testing.T, name string) repoRef {
	t.Helper()
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name, "auto_init": true})
	data := decodeJSON(t, resp)
	owner, _ := data["owner"].(map[string]interface{})
	login, _ := owner["login"].(string)
	repoName, _ := data["name"].(string)
	if login == "" || repoName == "" {
		t.Fatalf("repo create failed: %v", data)
	}
	repo := s.store.GetRepo(login, repoName)
	if repo == nil {
		t.Fatalf("repo %s/%s not found after create", login, repoName)
	}
	seedPullRequestBranches(t, s.Server, repo, "feature")
	return repoRef{owner: login, name: repoName}
}

func (s *isolatedServer) sweepPR(t *testing.T, repo repoRef, title string) (int, int) {
	t.Helper()
	resp := s.post(t, repo.path()+"/pulls", defaultToken, map[string]interface{}{
		"title": title,
		"head":  "feature",
		"base":  "main",
		"body":  "sweep pr body",
	})
	data := decodeJSON(t, resp)
	num, ok := data["number"].(float64)
	if !ok {
		t.Fatalf("pr create failed: %v", data)
	}
	id, _ := data["id"].(float64)
	return int(num), int(id)
}

// createEnterpriseTestUser mirrors the package helper against this instance's
// store: it seeds a user + PAT directly (no HTTP) so an enterprise test can
// provision a non-admin principal on its own server.
func (s *isolatedServer) createEnterpriseTestUser(t *testing.T, login string) string {
	t.Helper()
	s.store.Mu.Lock()
	u := &store.User{ID: s.store.NextUser, Login: login, NodeID: "U_ent" + strconv.Itoa(s.store.NextUser), Type: "User"}
	s.store.NextUser++
	s.store.Users[u.ID] = u
	s.store.UsersByLogin[u.Login] = u
	tok := &store.Token{Value: "ghp_" + login + "0000000000000000000000000000", UserID: u.ID, Scopes: "repo"}
	s.store.Tokens[tok.Value] = tok
	s.store.Mu.Unlock()
	return tok.Value
}

func (s *isolatedServer) createOrgRepoForGovernance(t *testing.T, org string) (repoRef, int) {
	t.Helper()
	name := "gov-repo-" + strconv.FormatInt(int64(testutil.NextTestID()), 36)
	resp := s.post(t, "/api/v3/orgs/"+org+"/repos", defaultToken, map[string]interface{}{
		"name":    name,
		"private": false,
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create org repo: %d", resp.StatusCode)
	}
	repo := decodeJSON(t, resp)
	return repoRef{owner: org, name: name}, int(repo["id"].(float64))
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
func (s *isolatedServer) newUser(t *testing.T, login string) (*store.User, string) {
	t.Helper()
	st := s.store
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if existing := st.UsersByLogin[login]; existing != nil {
		t.Fatalf("user %q already exists", login)
	}
	u := &store.User{ID: st.NextUser, Login: login, Type: "User"}
	st.NextUser++
	st.Users[u.ID] = u
	st.UsersByLogin[login] = u
	tok := &store.Token{Value: "ghp_" + login + "0000000000000000000000000000000000", UserID: u.ID, Scopes: "repo,read:org"}
	st.Tokens[tok.Value] = tok
	return u, tok.Value
}

// seedRepo mirrors the package seedTestRepo helper on this instance's own
// store: an admin-owned repo, created idempotently so a converted test can call
// it without coupling to any shared fixture.
func (s *isolatedServer) seedRepo(t *testing.T, name string, private bool) *store.Repo {
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

// seedTestOrg/seedOrgRepo mirror the package helpers against this instance's
// store: an org owned by admin and an org-owned repo, both idempotent.
func (s *isolatedServer) seedTestOrg(t *testing.T, login string) *store.Org {
	t.Helper()
	if org := s.store.GetOrg(login); org != nil {
		return org
	}
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, login, login, "")
	if org == nil {
		t.Fatalf("CreateOrg %s failed", login)
	}
	return org
}

func (s *isolatedServer) seedOrgRepo(t *testing.T, org *store.Org, name string, private bool) *store.Repo {
	t.Helper()
	if repo := s.store.GetRepo(org.Login, name); repo != nil {
		return repo
	}
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateOrgRepo(org, admin, name, "", private)
	if repo == nil {
		t.Fatalf("CreateOrgRepo %s/%s failed", org.Login, name)
	}
	return repo
}

// sealForServer/putSealedSecret mirror the package helpers against this
// instance: seal a value with this store's actions key pair, and PUT a secret
// the way real clients do.
func (s *isolatedServer) sealForServer(t *testing.T, plain string) (enc, keyID string) {
	t.Helper()
	enc, keyID, err := s.store.SealSecretValue(plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return enc, keyID
}

func (s *isolatedServer) putSealedSecret(t *testing.T, path, plain string) *http.Response {
	t.Helper()
	enc, keyID := s.sealForServer(t, plain)
	return s.put(t, path, defaultToken, map[string]interface{}{
		"encrypted_value": enc,
		"key_id":          keyID,
	})
}

// headShaForTest/submitSnapshotForTest mirror the package dependency-graph
// helpers against this instance (the package versions stay for gh_dependabot's
// seedDependabotAlert, which is shared across un-migrated files).
func (s *isolatedServer) headShaForRepoPath(t *testing.T, repoFullName string) string {
	t.Helper()
	resp := s.get(t, "/api/v3/repos/"+repoFullName+"/commits", defaultToken)
	commits := decodeJSONWithStatus2xxArray(t, resp, 200)
	if len(commits) == 0 {
		t.Fatal("repo has no commits")
	}
	return commits[0]["sha"].(string)
}

func (s *isolatedServer) headShaForTest(t *testing.T, repo string) string {
	t.Helper()
	return s.headShaForRepoPath(t, "admin/"+repo)
}

func (s *isolatedServer) submitSnapshotForRepoPath(t *testing.T, repoFullName, manifestPath, ref, sha, correlator string, purls ...string) map[string]interface{} {
	t.Helper()
	resolved := map[string]interface{}{}
	for _, purl := range purls {
		resolved[purl] = map[string]interface{}{"package_url": purl, "scope": "runtime"}
	}
	resp := s.post(t, "/api/v3/repos/"+repoFullName+"/dependency-graph/snapshots", defaultToken, map[string]interface{}{
		"version": 0,
		"ref":     ref,
		"sha":     sha,
		"job":     map[string]interface{}{"id": "job-1", "correlator": correlator},
		"detector": map[string]interface{}{
			"name": "bleephub-test-detector", "version": "1.0.0", "url": "https://example.com/detector",
		},
		"scanned": "2035-06-15T12:00:00Z",
		"manifests": map[string]interface{}{
			manifestPath: map[string]interface{}{
				"name":     manifestPath,
				"file":     map[string]interface{}{"source_location": manifestPath},
				"resolved": resolved,
			},
		},
	})
	return decodeJSONWithStatus(t, resp, 201)
}

func (s *isolatedServer) submitSnapshotForTest(t *testing.T, repo, ref, sha, correlator string, purls ...string) map[string]interface{} {
	t.Helper()
	return s.submitSnapshotForRepoPath(t, "admin/"+repo, "go.mod", ref, sha, correlator, purls...)
}

// putRepoFile/seedDependabotAlert mirror the package helpers against this
// instance (package versions stay for the un-migrated dependabot/enterprise
// files that share them; dupl.sh excludes _test.go so the duplication is safe).
func (s *isolatedServer) putRepoFile(t *testing.T, repoFullName, path, content, message string) string {
	t.Helper()
	resp := s.put(t, "/api/v3/repos/"+repoFullName+"/contents/"+path, defaultToken, map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("put contents %s: %d body=%s", path, resp.StatusCode, b)
	}
	out := decodeJSON(t, resp)
	commit := out["commit"].(map[string]interface{})
	return commit["sha"].(string)
}

func (s *isolatedServer) seedDependabotAlert(t *testing.T, owner, repo string, overrides map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{
		"package_name":             "dependabot-" + repo + "-pkg",
		"package_ecosystem":        "npm",
		"manifest_path":            "package-lock.json",
		"severity":                 "high",
		"summary":                  "Prototype pollution in lodash",
		"description":              "A vulnerability allows prototype pollution.",
		"vulnerable_version_range": "< 4.17.21",
		"first_patched_version":    "4.17.21",
	}
	for k, v := range overrides {
		body[k] = v
	}
	packageName := body["package_name"].(string)
	ecosystem := body["package_ecosystem"].(string)
	manifestPath := body["manifest_path"].(string)
	rangeExpr := body["vulnerable_version_range"].(string)
	patchedVersion, _ := body["first_patched_version"].(string)
	repoFullName := owner + "/" + repo

	create := s.post(t, "/api/v3/repos/"+repoFullName+"/security-advisories", defaultToken, map[string]interface{}{
		"summary":     body["summary"],
		"description": body["description"],
		"severity":    body["severity"],
		"cwe_ids":     []string{"CWE-79"},
		"vulnerabilities": []map[string]interface{}{
			{
				"package":                  map[string]interface{}{"ecosystem": ecosystem, "name": packageName},
				"vulnerable_version_range": rangeExpr,
				"first_patched_version":    patchedVersion,
			},
		},
	})
	advisory := decodeJSONWithStatus(t, create, http.StatusCreated)
	ghsaID := advisory["ghsa_id"].(string)
	publish := s.patch(t, "/api/v3/repos/"+repoFullName+"/security-advisories/"+ghsaID, defaultToken, map[string]interface{}{"state": "published"})
	decodeJSONWithStatus(t, publish, http.StatusOK)

	manifestContent := fmt.Sprintf("%s %s\n", ecosystem, packageName)
	sha := s.putRepoFile(t, repoFullName, manifestPath, manifestContent, "seed Dependabot dependency")
	s.submitSnapshotForRepoPath(t, repoFullName, manifestPath, "refs/heads/main", sha, "dependabot/"+packageName, dependabotTestPackageURL(ecosystem, packageName, rangeExpr))

	resp := s.authedGet(t, "/api/v3/repos/"+repoFullName+"/dependabot/alerts?package_name="+packageName)
	alerts := decodeJSONArray(t, resp)
	for _, created := range alerts {
		securityAdvisory, _ := created["security_advisory"].(map[string]any)
		if securityAdvisory != nil && securityAdvisory["ghsa_id"] == ghsaID {
			return created
		}
	}
	t.Fatalf("Dependabot alert for advisory %s was not created: %v", ghsaID, alerts)
	return nil
}

// createRepoWriteRepo mirrors the package helper: create an admin repo through
// the REST API (optionally auto-initialized) and return its bare name.
func (s *isolatedServer) createRepoWriteRepo(t *testing.T, autoInit bool) string {
	t.Helper()
	name := fmt.Sprintf("rw-%d-%d", int64(testutil.NextTestID()), atomic.AddInt64(&repoWriteRepoSeq, 1))
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":      name,
		"auto_init": autoInit,
	})
	requireStatus(t, resp, 201)
	return name
}

// createTestIssueRepo mirrors the package helper on this isolated server:
// creates a user repo, failing loudly here rather than as a later 404.
func (s *isolatedServer) createTestIssueRepo(t *testing.T, name string) {
	t.Helper()
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name,
	}))
}

// seedPackageVersion mirrors the package helper (still used by gh_packages
// files): publishes a package version (with a small file) via the internal
// route and returns (packageID, versionID), on this isolated server.
func (s *isolatedServer) seedPackageVersion(t *testing.T, ownerType, owner, pkgType, pkgName, version string) (int, int) {
	t.Helper()
	if pkgType == "container" {
		t.Fatalf("container packages must be published through the GitHub Container Registry-compatible /v2/ data plane, not /internal/packages")
	}
	body, _ := json.Marshal(map[string]any{
		"version":     version,
		"description": "test version",
		"metadata": map[string]any{
			"package_type": pkgType,
			"container":    map[string]any{"tags": []string{"latest"}},
		},
		"files": []map[string]any{
			{
				"name":           "package.tgz",
				"content_type":   "application/gzip",
				"content_base64": base64.StdEncoding.EncodeToString([]byte("hello package")),
			},
		},
	})
	resp, err := s.authedPost("/internal/packages/"+ownerType+"/"+owner+"/"+pkgType+"/"+pkgName+"/versions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("seed package version: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("seed package version: %d %s", resp.StatusCode, b)
	}
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode seeded version: %v", err)
	}
	resp.Body.Close()
	pkgID := 0
	if u := s.store.GetPackage(owner, pkgType, pkgName); u != nil {
		pkgID = u.ID
	}
	return pkgID, int(v["id"].(float64))
}

// registryRequest mirrors the package helper against this isolated server's
// Container-Registry-compatible /v2/ data plane.
func (s *isolatedServer) registryRequest(t *testing.T, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	if method == http.MethodPut && strings.Contains(path, "/manifests/") {
		req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// uploadRegistryBlob mirrors the package helper: monolithic-then-PUT blob
// upload, returning the sha256 digest, on this isolated server.
func (s *isolatedServer) uploadRegistryBlob(t *testing.T, name string, data []byte) string {
	t.Helper()
	start := s.registryRequest(t, http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil)
	requireStatus(t, start, http.StatusAccepted)
	location := start.Header.Get("Location")
	if location == "" {
		t.Fatalf("start upload missing Location")
	}
	patch := s.registryRequest(t, http.MethodPatch, location, bytes.NewReader(data))
	requireStatus(t, patch, http.StatusAccepted)
	digest := digestSHA256(data)
	sep := "?"
	if strings.Contains(location, "?") {
		sep = "&"
	}
	put := s.registryRequest(t, http.MethodPut, location+sep+"digest="+digest, nil)
	requireStatus(t, put, http.StatusCreated)
	if got := put.Header.Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("blob digest = %q, want %q", got, digest)
	}
	return digest
}

// publishContainerPackageVersion mirrors the package helper: publishes a
// single-layer OCI image and returns (packageID, versionID), on this server.
func (s *isolatedServer) publishContainerPackageVersion(t *testing.T, owner, pkgName, version string) (int, int) {
	t.Helper()
	name := owner + "/" + pkgName
	layerBytes := []byte("package layer " + owner + "/" + pkgName + ":" + version)
	layerDigest := s.uploadRegistryBlob(t, name, layerBytes)
	manifest := mustRegistryJSON(map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"layers": []map[string]interface{}{
			{
				"mediaType": "application/vnd.oci.image.layer.v1.tar",
				"digest":    layerDigest,
				"size":      len(layerBytes),
			},
		},
	})
	resp := s.registryRequest(t, http.MethodPut, "/v2/"+name+"/manifests/"+version, bytes.NewReader(manifest))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("publish container package %s:%s: %d %s", name, version, resp.StatusCode, body)
	}
	created := decodeJSON(t, resp)
	pkgID := 0
	if pkg := s.store.GetPackage(owner, "container", pkgName); pkg != nil {
		pkgID = pkg.ID
	}
	return pkgID, int(created["id"].(float64))
}

// putReadsFile mirrors the package helper (still used by gh_repos_reads_test.go):
// commits a file via the contents API and returns the commit SHA.
func (s *isolatedServer) putReadsFile(t *testing.T, repo, path, content, message, branch string) string {
	t.Helper()
	body := map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if branch != "" {
		body["branch"] = branch
	}
	resp := s.do(t, "PUT", "/api/v3/repos/admin/"+repo+"/contents/"+path, defaultToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("put contents %s/%s: %d", repo, path, resp.StatusCode)
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode put contents response: %v", err)
	}
	return out.Commit.SHA
}

// createGitHubAppViaManifest mirrors the package helper (still used by
// gh_apps_flow_test.go and gh_marketplace_test.go): runs the app-manifest
// conversion flow and returns the created app, on this isolated server.
func (s *isolatedServer) createGitHubAppViaManifest(t *testing.T, name string, perms map[string]string, events []string) map[string]interface{} {
	t.Helper()
	manifest := map[string]interface{}{
		"name":                name,
		"url":                 "https://example.test/app",
		"redirect_url":        "https://example.test/callback",
		"default_permissions": perms,
		"default_events":      events,
	}
	manifestJSON, _ := json.Marshal(manifest)
	form := url.Values{"manifest": {string(manifestJSON)}}
	req, _ := http.NewRequest("POST", s.baseURL+"/settings/apps/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "token "+defaultToken)
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("manifest submission: got %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse manifest redirect: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatal("manifest redirect carries no code")
	}
	convResp := s.post(t, "/api/v3/app-manifests/"+code+"/conversions", "", nil)
	if convResp.StatusCode != http.StatusCreated {
		convResp.Body.Close()
		t.Fatalf("manifest conversion: got %d, want 201", convResp.StatusCode)
	}
	return decodeJSON(t, convResp)
}

// installGitHubAppViaBrowser mirrors the package helper (still used by
// gh_apps_flow_test.go): runs the browser install flow and returns the
// installation, on this isolated server.
func (s *isolatedServer) installGitHubAppViaBrowser(t *testing.T, appSlug, targetLogin, selection string, repoIDs ...int) map[string]interface{} {
	t.Helper()
	form := url.Values{}
	if targetLogin != "" {
		form.Set("target_login", targetLogin)
	}
	if selection != "" {
		form.Set("repository_selection", selection)
	}
	for _, id := range repoIDs {
		form.Add("repository_ids", fmt.Sprint(id))
	}
	req, _ := http.NewRequest("POST", s.baseURL+"/apps/"+appSlug+"/installations/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "token "+defaultToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("GitHub App browser installation: got %d, want 201", resp.StatusCode)
	}
	return decodeJSON(t, resp)
}

// createOrgViaAdminAPI mirrors the package helper on this isolated server:
// provisions an organization owned by admin through the admin API.
func (s *isolatedServer) createOrgViaAdminAPI(t *testing.T, login string, profileName ...string) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{
		"login": login,
		"admin": "admin",
	}
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

// installAppOnOrg provisions an app, an organization owned by admin, an
// organization repository, and an installation carrying the given permissions;
// it returns appID, slug, pem, and instID — all on this isolated server.
func (s *isolatedServer) installAppOnOrg(t *testing.T, orgLogin, repoName string, perms map[string]string) (int, string, string, int) {
	t.Helper()
	s.createOrgViaAdminAPI(t, orgLogin)
	s.post(t, "/api/v3/orgs/"+orgLogin+"/repos", defaultToken, map[string]interface{}{"name": repoName}).Body.Close()

	appData := s.createGitHubAppViaManifest(t, orgLogin+"-app", perms, nil)
	appID := int(appData["id"].(float64))
	pemKey := appData["pem"].(string)
	appSlug := appData["slug"].(string)
	instData := s.installGitHubAppViaBrowser(t, appSlug, orgLogin, "all")
	return appID, appSlug, pemKey, int(instData["id"].(float64))
}

// gqlAuthzPost mirrors the package helper (still used by graphql_authz_test.go):
// a GraphQL POST expecting 200, on this isolated server.
func (s *isolatedServer) gqlAuthzPost(t *testing.T, token, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	resp := s.post(t, "/api/graphql", token, map[string]interface{}{
		"query":     query,
		"variables": variables,
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("graphql status = %d", resp.StatusCode)
	}
	return decodeJSON(t, resp)
}

// seedSecretAlert mirrors the package helper (still used by
// gh_search_notifications_rulesets_live_test.go): commits a file containing a
// detectable secret and returns the resulting alert, on this isolated server.
func (s *isolatedServer) seedSecretAlert(t *testing.T, owner, repo, secretType string) map[string]any {
	t.Helper()
	n := atomic.AddUint64(&secretScanningSeedCounter, 1)
	path := fmt.Sprintf("config/secrets-%d.txt", n)
	content := fmt.Sprintf("detected=%s\n", secretScanningSeedValue(secretType))
	resp := s.put(t, "/api/v3/repos/"+owner+"/"+repo+"/contents/"+path, defaultToken, map[string]any{
		"message": "add detected secret",
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("put detected secret: %d body=%s", resp.StatusCode, b)
	}
	resp.Body.Close()

	list := decodeJSONArray(t, s.get(t, "/api/v3/repos/"+owner+"/"+repo+"/secret-scanning/alerts?secret_type="+secretType, defaultToken))
	if len(list) == 0 {
		t.Fatalf("secret scanning did not create %s alert for %s/%s", secretType, owner, repo)
	}
	return list[0]
}

// createColumn/createCard/moveCard mirror the package helpers (still used by
// webhooks_test.go): classic-projects column/card operations on this server.
func (s *isolatedServer) createColumn(t *testing.T, projectID int, name string) int {
	t.Helper()
	resp := s.post(t, "/api/v3/projects/"+itoa(projectID)+"/columns", defaultToken, map[string]any{"name": name})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create column %s: %d %s", name, resp.StatusCode, b)
	}
	data := decodeJSON(t, resp)
	return int(data["id"].(float64))
}

func (s *isolatedServer) createCard(t *testing.T, columnID int, body map[string]any) map[string]any {
	t.Helper()
	resp := s.post(t, "/api/v3/projects/columns/"+itoa(columnID)+"/cards", defaultToken, body)
	if resp.StatusCode != http.StatusCreated {
		bb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create card: %d %s", resp.StatusCode, bb)
	}
	return decodeJSON(t, resp)
}

func (s *isolatedServer) moveCard(t *testing.T, cardID, columnID int, position string) map[string]any {
	t.Helper()
	body := map[string]any{"position": position}
	if columnID != 0 {
		body["column_id"] = columnID
	}
	resp := s.post(t, "/api/v3/projects/columns/cards/"+itoa(cardID)+"/moves", defaultToken, body)
	if resp.StatusCode != http.StatusCreated {
		bb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("move card: %d %s", resp.StatusCode, bb)
	}
	return decodeJSON(t, resp)
}

// uploadAttestation mirrors the package helper (still used by
// gh_artifact_metadata_test.go): posts a sigstore bundle and returns the
// created attestation id, on this isolated server.
func (s *isolatedServer) uploadAttestation(t *testing.T, ownerRepo, token string, bundle map[string]interface{}) int {
	t.Helper()
	resp := s.post(t, "/api/v3/repos/"+ownerRepo+"/attestations", token,
		map[string]interface{}{"bundle": bundle})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("upload attestation = %d, want 201", resp.StatusCode)
	}
	created := decodeJSON(t, resp)
	return int(created["id"].(float64))
}

// userSurfaceUser mirrors the package helper (still used by
// gh_marketplace_test.go): a user plus a broad-scope token, on this isolated
// server.
func (s *isolatedServer) userSurfaceUser(t *testing.T, login string) (*store.User, string) {
	t.Helper()
	u := s.createTestUser(t, login)
	tok := "ghp_" + login + "0000000000token"
	s.store.Mu.Lock()
	s.store.Tokens[tok] = &store.Token{Value: tok, UserID: u.ID, Scopes: "repo, workflow, read:org, admin:org, gist, user", CreatedAt: fixedTestTime}
	s.store.Mu.Unlock()
	return u, tok
}

// copilotTestOrg mirrors the package helper (still used by gh_copilot_test.go):
// an org owned by admin with optional active members, on this isolated server.
func (s *isolatedServer) copilotTestOrg(t *testing.T, login string, memberLogins ...string) (*store.Org, []*store.User) {
	t.Helper()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, login, login, "")
	if org == nil {
		t.Fatalf("CreateOrg(%q) returned nil", login)
	}
	members := make([]*store.User, 0, len(memberLogins))
	for _, ml := range memberLogins {
		u := seedTestUser(s.Server, ml)
		s.store.SetMembership(org.Login, u.ID, store.OrgRoleMember, store.MembershipStateActive)
		members = append(members, u)
	}
	return org, members
}

// createReadsRepo mirrors the package helper (still used by three other repo
// read-surface files): creates an admin repo with optional extra fields.
func (s *isolatedServer) createReadsRepo(t *testing.T, name string, extra map[string]interface{}) {
	t.Helper()
	body := map[string]interface{}{"name": name}
	for k, v := range extra {
		body[k] = v
	}
	resp := s.post(t, "/api/v3/user/repos", defaultToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create repo %s: %d", name, resp.StatusCode)
	}
}

// noRedirectGet mirrors the package helper (still used by
// gh_repos_commit_reads_test.go): a GET that does not follow redirects, on
// this isolated server.
func (s *isolatedServer) noRedirectGet(t *testing.T, path string) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest("GET", s.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+defaultToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// seedCodeScanningAlert mirrors the package helper (still used by three other
// code-scanning files): uploads a one-result SARIF and returns the single
// alert it produces, all on this isolated server.
func (s *isolatedServer) seedCodeScanningAlert(t *testing.T, owner, repo, ruleID, severity, toolName string) map[string]any {
	t.Helper()
	sarif := map[string]any{
		"version": "2.1.0",
		"runs": []map[string]any{
			{
				"tool": map[string]any{
					"driver": map[string]any{
						"name": toolName,
						"rules": []map[string]any{
							{
								"id": ruleID,
								"fullDescription": map[string]any{
									"text": "test description for " + ruleID,
								},
								"defaultConfiguration": map[string]any{
									"level": severity,
								},
								"properties": map[string]any{
									"problem.severity": severity,
								},
							},
						},
					},
				},
				"results": []map[string]any{
					{
						"ruleId":  ruleID,
						"message": map[string]any{"text": "problem here"},
						"locations": []map[string]any{
							{
								"physicalLocation": map[string]any{
									"artifactLocation": map[string]any{"uri": "src/index.js"},
									"region":           map[string]any{"startLine": 10, "endLine": 10, "startColumn": 5, "endColumn": 15},
								},
							},
						},
					},
				},
			},
		},
	}
	sarifBytes, _ := json.Marshal(sarif)
	commitSHA := s.putRepoFile(t, owner+"/"+repo, "src/"+ruleID+".js", "const finding = true;\n", "add "+ruleID+" source")
	body, _ := json.Marshal(map[string]any{
		"commit_sha": commitSHA,
		"ref":        "refs/heads/main",
		"sarif":      base64.StdEncoding.EncodeToString(sarifBytes),
	})
	resp, err := s.authedPost("/api/v3/repos/"+owner+"/"+repo+"/code-scanning/sarifs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("upload SARIF: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload SARIF: %d body=%s", resp.StatusCode, b)
	}
	alertsResp := s.authedGet(t, "/api/v3/repos/"+owner+"/"+repo+"/code-scanning/alerts?rule="+ruleID)
	if alertsResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(alertsResp.Body)
		alertsResp.Body.Close()
		t.Fatalf("list uploaded alert: %d body=%s", alertsResp.StatusCode, b)
	}
	var alerts []map[string]any
	if err := json.NewDecoder(alertsResp.Body).Decode(&alerts); err != nil {
		t.Fatalf("decode uploaded alerts: %v", err)
	}
	alertsResp.Body.Close()
	if len(alerts) != 1 {
		t.Fatalf("uploaded SARIF produced %d alerts for %s, want 1", len(alerts), ruleID)
	}
	return alerts[0]
}

// cleanupCodespaceContainer mirrors the package helper (still used by two
// other codespaces files): best-effort delete of a named codespace on this
// isolated server's store at test cleanup.
func (s *isolatedServer) cleanupCodespaceContainer(t *testing.T, name string) {
	t.Helper()
	if cs := s.store.GetCodespaceByName(name); cs != nil {
		if _, err := s.store.DeleteCodespace(cs.ID); err == nil {
			return
		}
	}
	ctx, cancel := contextWithTimeout(30 * time.Second)
	defer cancel()
	_ = store.DockerRemoveContainer(ctx, store.CodespaceContainerName(name))
}

// createTestPRRepo mirrors the package helper (still used by four other
// files): an auto-initialised admin repo seeded with the standard PR branch
// set, on this isolated server.
func (s *isolatedServer) createTestPRRepo(t *testing.T, name string) {
	t.Helper()
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name, "auto_init": true,
	})
	resp.Body.Close()
	repo := s.store.GetRepo("admin", name)
	if repo == nil {
		t.Fatalf("repo %s not created", name)
	}
	seedPullRequestBranches(t, s.Server, repo, "feature", "feat", "feat1", "feat2", "fix", "branch", "r", "f", "a", "b", "draft-feat")
}

// cancelRepoRunsCleanup mirrors the package helper (still used by six
// shared-server files): at test cleanup it cancels every in-flight run the
// test created, on this isolated server's own store.
func (s *isolatedServer) cancelRepoRunsCleanup(t *testing.T, repoKey string) {
	t.Helper()
	t.Cleanup(func() {
		s.store.Mu.RLock()
		var runs []*store.Workflow
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey && w.Status != store.WorkflowStatusCompleted {
				runs = append(runs, w)
			}
		}
		s.store.Mu.RUnlock()
		for _, w := range runs {
			s.actions.CancelWorkflow(w)
		}
	})
}

// assertWorkflowJobsUseHostMode mirrors the package helper (still used by
// gh_actions_run_control_test.go): each workflow job has a stored runner job
// whose message runs in host mode (no jobContainer), read from s.store.
func (s *isolatedServer) assertWorkflowJobsUseHostMode(t *testing.T, wf *store.Workflow, keys ...string) {
	t.Helper()
	if len(keys) == 0 {
		for key := range wf.Jobs {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		job := wf.Jobs[key]
		if job == nil {
			t.Fatalf("workflow job %q not found", key)
		}
		s.store.Mu.RLock()
		queued := s.store.Jobs[job.JobID]
		s.store.Mu.RUnlock()
		if queued == nil {
			t.Fatalf("workflow job %q has no stored runner job %q", key, job.JobID)
		}
		if queued.Message == "" {
			t.Fatalf("workflow job %q has no runner message", key)
		}
		var msg map[string]interface{}
		if err := json.Unmarshal([]byte(queued.Message), &msg); err != nil {
			t.Fatalf("workflow job %q message JSON: %v", key, err)
		}
		if msg["jobContainer"] != nil {
			t.Fatalf("workflow job %q jobContainer = %#v, want nil for a workflow without container", key, msg["jobContainer"])
		}
	}
}

// createTestGist mirrors the package helper (still used by
// gh_misc_endpoints_users_test.go): a small public/private gist owned by token.
func (s *isolatedServer) createTestGist(t *testing.T, token string, public bool) map[string]interface{} {
	t.Helper()
	resp := s.post(t, "/api/v3/gists", token, map[string]interface{}{
		"description": "test gist",
		"public":      public,
		"files": map[string]interface{}{
			"hello.go": map[string]interface{}{
				"content": "package main\n\nfunc main() {}",
			},
		},
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	return decodeJSON(t, resp)
}

// createTestCodespaceRepo mirrors the package helper: an admin repo seeded with
// a devcontainer.json pointing at the fast test image.
func (s *isolatedServer) createTestCodespaceRepo(t *testing.T, name string) *store.Repo {
	t.Helper()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, name, "codespace test repo", false)
	if repo == nil {
		t.Fatalf("failed to create repo %s", name)
	}
	stor := s.store.GitStorages[repo.FullName]
	if _, err := initRepoWithFiles(stor, repo.DefaultBranch, "init", map[string]string{
		".devcontainer/devcontainer.json": fmt.Sprintf(`{"image":"%s"}`, codespaceTestImage),
	}, repoSignature(admin.Login, "bleephub@local")); err != nil {
		t.Fatalf("init repo files: %v", err)
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
