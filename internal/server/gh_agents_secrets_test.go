package bleephub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// GitHub Copilot coding agent repository secrets

func TestAgentsRepoSecrets_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "agents-sec-repo", false)
	base := "/api/v3/repos/" + repo.FullName + "/agents/secrets"

	// Public key matches the shared Actions sealed-box keypair.
	resp := s.get(t, base+"/public-key", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("public-key status %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	kp, err := s.store.ActionsKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if body["key_id"] != kp.KeyID || body["key"] != kp.PublicKey {
		t.Fatalf("public-key = %v, want key_id=%s", body, kp.KeyID)
	}

	mustStatus(t, s.putSealedSecret(t, base+"/AGENT_TOKEN", "plain-1"), 201, "create secret")

	resp = s.get(t, base, defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("list status %d, want 200", resp.StatusCode)
	}
	list := decodeJSON(t, resp)
	if list["total_count"] != float64(1) {
		t.Fatalf("total_count = %v, want 1", list["total_count"])
	}
	secrets := list["secrets"].([]interface{})
	if secrets[0].(map[string]interface{})["name"] != "AGENT_TOKEN" {
		t.Fatalf("secrets[0] = %v, want AGENT_TOKEN", secrets[0])
	}

	resp = s.get(t, base+"/AGENT_TOKEN", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("get status %d, want 200", resp.StatusCode)
	}
	got := decodeJSON(t, resp)
	if got["name"] != "AGENT_TOKEN" {
		t.Fatalf("get name = %v, want AGENT_TOKEN", got["name"])
	}

	mustStatus(t, s.putSealedSecret(t, base+"/AGENT_TOKEN", "plain-2"), 204, "update secret")

	mustStatus(t, s.delete(t, base+"/AGENT_TOKEN", defaultToken), 204, "delete secret")
	mustStatus(t, s.get(t, base+"/AGENT_TOKEN", defaultToken), 404, "get deleted secret")
	mustStatus(t, s.delete(t, base+"/AGENT_TOKEN", defaultToken), 404, "delete deleted secret")
}

func TestAgentsRepoSecrets_ListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "agents-sec-pg", "", false)
	base := "/api/v3/repos/" + repo.FullName + "/agents/secrets"

	putPaginationSealedSecret(t, s, base+"/AAA_FIRST", "v", 201)
	putPaginationSealedSecret(t, s, base+"/BBB_SECOND", "v", 201)

	resp := tokenRequest(s, http.MethodGet, base+"?per_page=1", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1 = %d: %s", resp.Code, resp.Body.String())
	}
	var list map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list["total_count"] != float64(2) {
		t.Fatalf("total_count = %v, want 2", list["total_count"])
	}
	page1 := list["secrets"].([]interface{})
	if len(page1) != 1 || page1[0].(map[string]interface{})["name"] != "AAA_FIRST" {
		t.Fatalf("page 1 = %v, want [AAA_FIRST]", page1)
	}
	if link := resp.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, base+"?per_page=1&page=2", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2 = %d: %s", resp.Code, resp.Body.String())
	}
	list = nil
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	page2 := list["secrets"].([]interface{})
	if len(page2) != 1 || page2[0].(map[string]interface{})["name"] != "BBB_SECOND" {
		t.Fatalf("page 2 = %v, want [BBB_SECOND]", page2)
	}
}

func TestAgentsRepoSecrets_Validation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "agents-sec-valid", false)
	base := "/api/v3/repos/" + repo.FullName + "/agents/secrets"

	mustStatus(t, s.putSealedSecret(t, base+"/1BAD", "v"), 422, "invalid secret name")

	resp := s.put(t, base+"/GOOD_NAME", defaultToken, map[string]interface{}{
		"encrypted_value": "c2VjcmV0",
		"key_id":          "not-the-key",
	})
	mustStatus(t, resp, 422, "wrong key_id")

	mustStatus(t, s.get(t, "/api/v3/repos/admin/no-such-repo/agents/secrets", defaultToken), 404, "unknown repo list")
}

// GitHub Copilot coding agent organization secrets

func TestAgentsOrgSecrets_VisibilityAndSelectedRepos(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "agents-sec-org")
	repo1 := s.seedOrgRepo(t, org, "sel-one", true)
	repo2 := s.seedOrgRepo(t, org, "sel-two", true)
	base := "/api/v3/orgs/" + org.Login + "/agents/secrets"

	mustStatus(t, s.get(t, base+"/public-key", defaultToken), 200, "org public-key")

	enc, keyID := s.sealForServer(t, "org-plain")
	resp := s.put(t, base+"/ORG_AGENT_SECRET", defaultToken, map[string]interface{}{
		"encrypted_value":         enc,
		"key_id":                  keyID,
		"visibility":              "selected",
		"selected_repository_ids": []int{repo1.ID},
	})
	mustStatus(t, resp, 201, "create org secret")

	// The GET carries the /agents/ (not /actions/) selected_repositories_url.
	resp = s.get(t, base+"/ORG_AGENT_SECRET", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("get org secret status %d, want 200", resp.StatusCode)
	}
	got := decodeJSON(t, resp)
	if got["visibility"] != "selected" {
		t.Fatalf("visibility = %v, want selected", got["visibility"])
	}
	wantURL := fmt.Sprintf("/api/v3/orgs/%s/agents/secrets/ORG_AGENT_SECRET/repositories", org.Login)
	if url, _ := got["selected_repositories_url"].(string); url == "" || url[len(url)-len(wantURL):] != wantURL {
		t.Fatalf("selected_repositories_url = %v, want suffix %s", got["selected_repositories_url"], wantURL)
	}

	resp = s.get(t, base+"/ORG_AGENT_SECRET/repositories", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("list selected repos status %d, want 200", resp.StatusCode)
	}
	repos := decodeJSON(t, resp)
	if repos["total_count"] != float64(1) {
		t.Fatalf("selected total_count = %v, want 1", repos["total_count"])
	}

	resp = s.put(t, base+"/ORG_AGENT_SECRET/repositories", defaultToken, map[string]interface{}{
		"selected_repository_ids": []int{repo1.ID, repo2.ID},
	})
	mustStatus(t, resp, 204, "set selected repos")

	mustStatus(t, s.delete(t, fmt.Sprintf("%s/ORG_AGENT_SECRET/repositories/%d", base, repo2.ID), defaultToken), 204, "remove selected repo")
	mustStatus(t, s.put(t, fmt.Sprintf("%s/ORG_AGENT_SECRET/repositories/%d", base, repo2.ID), defaultToken, nil), 204, "add selected repo")

	resp = s.get(t, "/api/v3/repos/"+repo1.FullName+"/agents/organization-secrets", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("repo org-secrets status %d, want 200", resp.StatusCode)
	}
	visible := decodeJSON(t, resp)
	if visible["total_count"] != float64(1) {
		t.Fatalf("repo-visible org secrets = %v, want 1", visible["total_count"])
	}

	// A repo outside the selection sees no org secrets.
	repo3 := s.seedOrgRepo(t, org, "sel-three", true)
	resp = s.get(t, "/api/v3/repos/"+repo3.FullName+"/agents/organization-secrets", defaultToken)
	notVisible := decodeJSON(t, resp)
	if notVisible["total_count"] != float64(0) {
		t.Fatalf("unselected repo sees %v org secrets, want 0", notVisible["total_count"])
	}

	// Per-repo add is a 409 when visibility is not "selected".
	enc, keyID = s.sealForServer(t, "all-plain")
	mustStatus(t, s.put(t, base+"/ORG_ALL_SECRET", defaultToken, map[string]interface{}{
		"encrypted_value": enc, "key_id": keyID, "visibility": "all",
	}), 201, "create all-visibility secret")
	mustStatus(t, s.put(t, fmt.Sprintf("%s/ORG_ALL_SECRET/repositories/%d", base, repo1.ID), defaultToken, nil), 409, "add repo to all-visibility secret")

	enc, keyID = s.sealForServer(t, "no-vis")
	mustStatus(t, s.put(t, base+"/ORG_NO_VIS", defaultToken, map[string]interface{}{
		"encrypted_value": enc, "key_id": keyID,
	}), 422, "create without visibility")

	mustStatus(t, s.delete(t, base+"/ORG_AGENT_SECRET", defaultToken), 204, "delete org secret")
	mustStatus(t, s.get(t, base+"/ORG_AGENT_SECRET", defaultToken), 404, "get deleted org secret")
}

// GitHub Copilot coding agent repository variables

func TestAgentsRepoVariables_CRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "agents-var-repo", false)
	base := "/api/v3/repos/" + repo.FullName + "/agents/variables"

	mustStatus(t, s.post(t, base, defaultToken, map[string]interface{}{
		"name": "agent_mode", "value": "fast",
	}), 201, "create variable")

	mustStatus(t, s.post(t, base, defaultToken, map[string]interface{}{
		"name": "AGENT_MODE", "value": "again",
	}), 409, "duplicate create")

	mustStatus(t, s.post(t, base, defaultToken, map[string]interface{}{
		"name": "GITHUB_RESERVED", "value": "x",
	}), 422, "reserved name")

	resp := s.get(t, base, defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("list status %d, want 200", resp.StatusCode)
	}
	list := decodeJSON(t, resp)
	if list["total_count"] != float64(1) {
		t.Fatalf("total_count = %v, want 1", list["total_count"])
	}

	// Names are upper-cased.
	resp = s.get(t, base+"/AGENT_MODE", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("get status %d, want 200", resp.StatusCode)
	}
	got := decodeJSON(t, resp)
	if got["value"] != "fast" {
		t.Fatalf("value = %v, want fast", got["value"])
	}

	mustStatus(t, s.patch(t, base+"/AGENT_MODE", defaultToken, map[string]interface{}{
		"name": "AGENT_SPEED", "value": "slow",
	}), 204, "patch variable")
	resp = s.get(t, base+"/AGENT_SPEED", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("get renamed status %d, want 200", resp.StatusCode)
	}
	got = decodeJSON(t, resp)
	if got["value"] != "slow" {
		t.Fatalf("patched value = %v, want slow", got["value"])
	}
	mustStatus(t, s.get(t, base+"/AGENT_MODE", defaultToken), 404, "old name gone")

	mustStatus(t, s.delete(t, base+"/AGENT_SPEED", defaultToken), 204, "delete variable")
	mustStatus(t, s.get(t, base+"/AGENT_SPEED", defaultToken), 404, "get deleted variable")
}

// GitHub Copilot coding agent organization variables

func TestAgentsOrgVariables_CRUDAndSelectedRepos(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "agents-var-org")
	repo1 := s.seedOrgRepo(t, org, "var-one", true)
	repo2 := s.seedOrgRepo(t, org, "var-two", true)
	base := "/api/v3/orgs/" + org.Login + "/agents/variables"

	mustStatus(t, s.post(t, base, defaultToken, map[string]interface{}{
		"name": "ORG_AGENT_VAR", "value": "v1",
		"visibility":              "selected",
		"selected_repository_ids": []int{repo1.ID},
	}), 201, "create org variable")

	mustStatus(t, s.post(t, base, defaultToken, map[string]interface{}{
		"name": "NO_VIS", "value": "v",
	}), 422, "create without visibility")

	resp := s.get(t, base+"/ORG_AGENT_VAR", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("get org variable status %d, want 200", resp.StatusCode)
	}
	got := decodeJSON(t, resp)
	if got["visibility"] != "selected" || got["value"] != "v1" {
		t.Fatalf("org variable = %v, want selected/v1", got)
	}

	resp = s.get(t, base+"/ORG_AGENT_VAR/repositories", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("list selected repos status %d, want 200", resp.StatusCode)
	}
	repos := decodeJSON(t, resp)
	if repos["total_count"] != float64(1) {
		t.Fatalf("selected total_count = %v, want 1", repos["total_count"])
	}
	mustStatus(t, s.put(t, base+"/ORG_AGENT_VAR/repositories", defaultToken, map[string]interface{}{
		"selected_repository_ids": []int{repo1.ID, repo2.ID},
	}), 204, "set selected repos")
	mustStatus(t, s.delete(t, fmt.Sprintf("%s/ORG_AGENT_VAR/repositories/%d", base, repo1.ID), defaultToken), 204, "remove selected repo")
	mustStatus(t, s.put(t, fmt.Sprintf("%s/ORG_AGENT_VAR/repositories/%d", base, repo1.ID), defaultToken, nil), 204, "add selected repo")

	// After visibility flips to all, the selection endpoints must conflict.
	mustStatus(t, s.patch(t, base+"/ORG_AGENT_VAR", defaultToken, map[string]interface{}{
		"visibility": "all",
	}), 204, "patch visibility")
	mustStatus(t, s.get(t, base+"/ORG_AGENT_VAR/repositories", defaultToken), 409, "list repos on all-visibility variable")
	mustStatus(t, s.put(t, base+"/ORG_AGENT_VAR/repositories", defaultToken, map[string]interface{}{
		"selected_repository_ids": []int{repo1.ID},
	}), 409, "set repos on all-visibility variable")

	// Visibility all means the variable is visible from every repo.
	resp = s.get(t, "/api/v3/repos/"+repo2.FullName+"/agents/organization-variables", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("repo org-variables status %d, want 200", resp.StatusCode)
	}
	visible := decodeJSON(t, resp)
	if visible["total_count"] != float64(1) {
		t.Fatalf("repo-visible org variables = %v, want 1", visible["total_count"])
	}

	resp = s.get(t, base, defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("list org variables status %d, want 200", resp.StatusCode)
	}
	list := decodeJSON(t, resp)
	if list["total_count"] != float64(1) {
		t.Fatalf("org variables total_count = %v, want 1", list["total_count"])
	}

	mustStatus(t, s.delete(t, base+"/ORG_AGENT_VAR", defaultToken), 204, "delete org variable")
	mustStatus(t, s.get(t, base+"/ORG_AGENT_VAR", defaultToken), 404, "get deleted org variable")
}

// TestAgentsSecrets_IsolatedFromActions: the /agents/ tables are distinct from
// the Actions ones, neither surface showing the other's secrets.
func TestAgentsSecrets_IsolatedFromActions(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "agents-isolation", false)

	mustStatus(t, s.putSealedSecret(t, "/api/v3/repos/"+repo.FullName+"/actions/secrets/ACTIONS_ONLY", "a"), 201, "create actions secret")
	mustStatus(t, s.putSealedSecret(t, "/api/v3/repos/"+repo.FullName+"/agents/secrets/AGENTS_ONLY", "b"), 201, "create agents secret")

	resp := s.get(t, "/api/v3/repos/"+repo.FullName+"/agents/secrets", defaultToken)
	list := decodeJSON(t, resp)
	secrets := list["secrets"].([]interface{})
	if len(secrets) != 1 || secrets[0].(map[string]interface{})["name"] != "AGENTS_ONLY" {
		t.Fatalf("agents secrets = %v, want only AGENTS_ONLY", secrets)
	}

	mustStatus(t, s.get(t, "/api/v3/repos/"+repo.FullName+"/actions/secrets/AGENTS_ONLY", defaultToken), 404, "agents secret invisible to actions surface")
	mustStatus(t, s.get(t, "/api/v3/repos/"+repo.FullName+"/agents/secrets/ACTIONS_ONLY", defaultToken), 404, "actions secret invisible to agents surface")
}

// TestAgentsCodeScanPersistenceReload: every new bucket — Copilot agent
// secrets/variables/tasks, code scanning autofixes, CodeQL databases, and
// CodeQL variant analyses — survives a persistence reload with counters intact.
func TestAgentsCodeScanPersistenceReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	// session 1: create state, then close.
	p1, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	objectFS, objectStore := newObjectByteStoreForTest(t)
	st1.ObjectByteStore = objectStore
	st1.SeedDefaultUser()
	user := st1.UsersByLogin["admin"]
	repo := st1.CreateRepo(user, "agents-reload", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}

	srv1 := &Server{store: st1}
	srv1.upsertSecret(st1.AgentsRepoSecrets, "agents_repo_secrets", repo.FullName, "RELOAD_SECRET", "plain-value")
	now := fixedTestTime.UTC()
	tbl := agentsVariableTable{srv1, "agents_repo_variables", repo.FullName}
	if !tbl.create(&store.ActionsVariable{Name: "RELOAD_VAR", Value: "vv", CreatedAt: now, UpdatedAt: now}) {
		t.Fatal("create agents variable failed")
	}

	task := st1.CreateAgentTask(repo, user, "reload prompt", "claude-sonnet-4.6", false, "", "")

	alert := st1.CreateCodeScanningAlert(repo.FullName, "reload-rule", "error", "d", "CodeQL", "c7a7c45a-8a3c-4f6e-9b5c-1d3c01234567", "open",
		[]store.CodeScanningAlertInstance{{Ref: "refs/heads/main", Path: "f.go", StartLine: 1, State: "open"}})
	if _, created := st1.CreateCodeScanningAutofix(alert); !created {
		t.Fatal("autofix not created")
	}

	db, err := st1.UpsertCodeQLDatabase(repo.FullName, "go", "database.zip", "application/zip", "reload-sha", []byte("db-bytes"), user.ID)
	if err != nil {
		t.Fatalf("UpsertCodeQLDatabase: %v", err)
	}
	if got := string(readS3TestFile(t, objectFS, db.StoragePath)); got != "db-bytes" {
		t.Fatalf("CodeQL database object bytes = %q, want db-bytes", got)
	}
	va, err := st1.CreateCodeQLVariantAnalysis(repo.FullName, user.ID, "go", []byte("pack"), []string{repo.FullName})
	if err != nil {
		t.Fatalf("CreateCodeQLVariantAnalysis: %v", err)
	}
	if got := string(readS3TestFile(t, objectFS, va.StoragePath)); got != "pack" {
		t.Fatalf("CodeQL variant-analysis query-pack object bytes = %q, want pack", got)
	}

	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// session 2: reload, assert everything came back.
	p2, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	st2 := store.NewStore()
	st2.ObjectByteStore = objectStore
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("re-load SetPersistence: %v", err)
	}
	defer p2.Close()

	if sec := st2.AgentsRepoSecrets[repo.FullName]["RELOAD_SECRET"]; sec == nil || sec.Value != "plain-value" {
		t.Fatalf("agents repo secret after reload = %v, want plain-value", sec)
	}
	if v := st2.AgentsRepoVariables[repo.FullName]["RELOAD_VAR"]; v == nil || v.Value != "vv" {
		t.Fatalf("agents repo variable after reload = %v, want vv", v)
	}
	gotTask := st2.GetAgentTask(task.ID)
	if gotTask == nil || gotTask.Prompt != "reload prompt" || len(gotTask.Sessions) != 1 {
		t.Fatalf("agent task after reload = %+v, want prompt + 1 session", gotTask)
	}
	if fix := st2.GetCodeScanningAutofix(repo.FullName, alert.Number); fix == nil || fix.Status != "success" {
		t.Fatalf("autofix after reload = %+v, want success", fix)
	}
	gotDB := st2.GetCodeQLDatabase(repo.FullName, "go")
	if gotDB == nil || gotDB.CommitOID != "reload-sha" {
		t.Fatalf("CodeQL database after reload = %+v", gotDB)
	}
	gotBytes, err := st2.ReadCodeQLDatabaseContent(context.Background(), gotDB)
	if err != nil {
		t.Fatalf("ReadCodeQLDatabaseContent: %v", err)
	}
	if !bytes.Equal(gotBytes, []byte("db-bytes")) {
		t.Fatalf("CodeQL database bytes after reload = %q, want db-bytes", gotBytes)
	}
	if st2.NextCodeQLDatabaseID != db.ID+1 {
		t.Fatalf("NextCodeQLDatabaseID = %d, want %d", st2.NextCodeQLDatabaseID, db.ID+1)
	}
	gotVA := st2.GetCodeQLVariantAnalysis(repo.FullName, va.ID)
	if gotVA == nil || gotVA.Status != "succeeded" || len(gotVA.ScannedRepositories) != 1 {
		t.Fatalf("variant analysis after reload = %+v, want succeeded with 1 scanned repo", gotVA)
	}
	if st2.NextCodeQLVariantAnalysisID != va.ID+1 {
		t.Fatalf("NextCodeQLVariantAnalysisID = %d, want %d", st2.NextCodeQLVariantAnalysisID, va.ID+1)
	}
}
