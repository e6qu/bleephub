package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestImportReadsRequireRepoAccess pins that the source-import GET endpoints no
// longer leak a private repo's import record (vcs_url, author PII, large-file
// paths) to a caller with no access. Prior to the fix they used the
// non-visibility-checking lookupRepoFromPath with no requirePerm gate.
func TestImportReadsRequireRepoAccess(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "importable", true) // private
	pct := 100
	s.store.PutRepoImport(&store.RepoImport{
		RepoID:        repo.ID,
		VCS:           "git",
		VCSURL:        "https://user:secret@example.test/old.git",
		Status:        "complete",
		ImportPercent: &pct,
	})
	_, strangerTok := s.newUser(t, "import-stranger")

	for _, path := range []string{
		"/api/v3/repos/admin/importable/import",
		"/api/v3/repos/admin/importable/import/authors",
	} {
		resp := s.get(t, path, strangerTok)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("stranger GET %s = %d, want 404", path, resp.StatusCode)
		}
		// The owner still reads it.
		resp = s.get(t, path, defaultToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("owner GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// TestSecurityScanningAlertsRequireSecurityAccess pins that code-scanning and
// secret-scanning alert reads require repo security access (write standing /
// admin / security manager), not mere repo-read. On a public repo an
// unprivileged stranger — who can read the repo — must not see the alerts or,
// worst of all, the secret's file+commit location.
func TestSecurityScanningAlertsRequireSecurityAccess(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "scanme", false) // public
	s.seedCodeScanningAlert(t, "admin", "scanme", "js/xss", "error", "CodeQL")
	s.seedSecretAlert(t, "admin", "scanme", "github_personal_access_token")
	_, strangerTok := s.newUser(t, "scan-stranger")

	gated := []string{
		"/api/v3/repos/admin/scanme/code-scanning/alerts",
		"/api/v3/repos/admin/scanme/secret-scanning/alerts",
		"/api/v3/repos/admin/scanme/secret-scanning/alerts/1/locations",
	}
	for _, path := range gated {
		resp := s.get(t, path, strangerTok)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("public-repo stranger GET %s = %d, want 404", path, resp.StatusCode)
		}
		resp = s.get(t, path, defaultToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("owner GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// TestRepoArtifactMetadataHiddenFromNonReader pins that Actions artifact
// metadata for a private repo is no longer world-readable: getRepoArtifact now
// enforces repo readability.
func TestRepoArtifactMetadataHiddenFromNonReader(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "artifacts-priv", true) // private

	const artID int64 = 700001
	s.artifactStore.Mu.Lock()
	s.artifactStore.Artifacts[artID] = &store.Artifact{
		ID:           artID,
		Name:         "build-output",
		Size:         5,
		Finalized:    true,
		RepoFullName: repo.FullName,
		GitHubRunID:  42,
	}
	s.artifactStore.Mu.Unlock()

	path := "/api/v3/repos/admin/artifacts-priv/actions/artifacts/" + strconv.FormatInt(artID, 10)
	_, strangerTok := s.newUser(t, "artifact-stranger")

	resp := s.get(t, path, strangerTok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("stranger GET artifact metadata = %d, want 404", resp.StatusCode)
	}
	resp = s.get(t, path, defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("owner GET artifact metadata = %d, want 200", resp.StatusCode)
	}
}

// TestCheckRunStateValidationAndConclusionCoupling pins that check-run
// create/update validate status/conclusion against GitHub's enums (422 on an
// out-of-enum value) and couple them: a conclusion completes the run.
func TestCheckRunStateValidationAndConclusionCoupling(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	s.store.CreateRepo(admin, "checks-r15", "", false)
	app := s.store.CreateApp(admin.ID, "R15 Checks", "", map[string]string{"checks": "write"}, nil)
	inst := s.store.CreateInstallation(app.ID, "User", admin.ID, admin.Login, map[string]string{"checks": "write"}, nil)
	tok := s.store.CreateInstallationToken(inst.ID, app.ID, map[string]string{"checks": "write"}, nil)
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	base := "/api/v3/repos/admin/checks-r15/check-runs"

	// An out-of-enum status is a 422.
	resp := s.do(t, http.MethodPost, base, tok.Token,
		map[string]interface{}{"name": "x", "head_sha": sha, "status": "garbage"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create with invalid status = %d, want 422", resp.StatusCode)
	}

	// A conclusion with no status completes the run (status forced to completed).
	resp = s.do(t, http.MethodPost, base, tok.Token,
		map[string]interface{}{"name": "y", "head_sha": sha, "conclusion": "success"})
	body := decodeCheckRunBody(t, resp)
	if body["status"] != "completed" {
		t.Fatalf("conclusion-only create: status = %v, want completed", body["status"])
	}
	if body["conclusion"] != "success" {
		t.Fatalf("conclusion-only create: conclusion = %v, want success", body["conclusion"])
	}
	if body["completed_at"] == nil {
		t.Fatalf("conclusion-only create: completed_at not set")
	}
	runID := strconv.Itoa(int(body["id"].(float64)))

	// An out-of-enum conclusion on update is a 422.
	resp = s.do(t, http.MethodPatch, base+"/"+runID, tok.Token,
		map[string]interface{}{"conclusion": "not-a-conclusion"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("update with invalid conclusion = %d, want 422", resp.StatusCode)
	}
}

// TestProjectV2ItemsRedactPrivateRepoContent pins that the GraphQL
// ProjectV2.items connection hides an item's content (and content-derived
// built-in field values) from a viewer who can read the project but not the
// private repository the item's issue lives in — matching the REST gate.
func TestProjectV2ItemsRedactPrivateRepoContent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	repo := s.seedRepo(t, "pv2-priv", true) // private
	issue := s.store.CreateIssue(repo.ID, admin.ID, "secret issue title", "body", nil, nil, 0)

	project := s.store.ProjectsV2.CreateProject(admin.ID, "User", "public board", admin.ID)
	pub := true
	s.store.ProjectsV2.UpdateProject(project.ID, nil, nil, &pub) // make it public
	if s.store.ProjectsV2.AddItem(project.ID, "Issue", issue.ID, admin.ID) == nil {
		t.Fatalf("could not add issue item to project")
	}

	_, strangerTok := s.newUser(t, "pv2-stranger")
	query := `query($login:String!,$number:Int!){user(login:$login){projectV2(number:$number){items(first:10){nodes{type content{__typename ... on Issue{title}}}}}}}`

	itemContent := func(token string) (int, interface{}) {
		env := s.gqlAuthzPost(t, token, query,
			map[string]interface{}{"login": admin.Login, "number": project.Number})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("query errored: %v", errs)
		}
		data, _ := env["data"].(map[string]interface{})
		user, _ := data["user"].(map[string]interface{})
		proj, _ := user["projectV2"].(map[string]interface{})
		items, _ := proj["items"].(map[string]interface{})
		nodes, _ := items["nodes"].([]interface{})
		if len(nodes) != 1 {
			t.Fatalf("expected 1 item node, got %d: %v", len(nodes), nodes)
		}
		first, _ := nodes[0].(map[string]interface{})
		return len(nodes), first["content"]
	}

	// The stranger can read the public project but not the private issue: content redacted.
	if _, content := itemContent(strangerTok); content != nil {
		t.Fatalf("private issue content leaked to a project-only viewer: %v", content)
	}
	// The owner still sees the content.
	_, content := itemContent(defaultToken)
	c, ok := content.(map[string]interface{})
	if !ok || c["title"] != "secret issue title" {
		t.Fatalf("owner did not see the issue content: %v", content)
	}
}

func decodeCheckRunBody(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create check run = %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode check run: %v", err)
	}
	return out
}
