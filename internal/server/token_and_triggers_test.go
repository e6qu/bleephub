package bleephub

import (
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

// --- Presented credentials that do not verify ---

// TestPresentedCredentialThatDoesNotVerifyIsRejected pins the difference
// between "no credential" and "a credential that is not good". Every
// verification failure used to be discarded and the request continued as
// anonymous, so a revoked, expired or forged token read whatever the public
// surface answers instead of being told it was rejected.
func TestPresentedCredentialThatDoesNotVerifyIsRejected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":      "credprobe",
		"auto_init": true,
	}).Body.Close()
	const path = "/api/v3/repos/admin/credprobe"

	// Positive controls: no credential at all stays anonymous and reads the
	// public repository, and a good credential still works.
	requireStatus(t, s.get(t, path, ""), http.StatusOK)
	requireStatus(t, s.get(t, path, defaultToken), http.StatusOK)

	for name, credential := range map[string]string{
		"unknown classic pat":  "ghp_0000000000000000000000000000000000",
		"unknown oauth token":  "gho_0000000000000000000000000000000000",
		"unknown app user":     "ghu_0000000000000000000000000000000000",
		"unknown installation": "ghs_0000000000000000000000000000000000",
		"refresh token":        "ghr_0000000000000000000000000000000000",
		"unverifiable jwt":     "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiI5OTkifQ.bm90LWEtc2lnbmF0dXJl",
		"free-form garbage":    "not-a-token-at-all",
	} {
		t.Run(name, func(t *testing.T) {
			requireStatus(t, s.get(t, path, credential), http.StatusUnauthorized)
		})
	}
}

// TestSessionSurvivesWithoutAnAuthorizationHeader is the other half of the
// control: rejecting presented-but-invalid credentials must not reject the
// requests that present none.
func TestSessionSurvivesWithoutAnAuthorizationHeader(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	requireStatus(t, s.get(t, "/api/v3/zen", ""), http.StatusOK)
	requireStatus(t, s.get(t, "/api/v3/meta", ""), http.StatusOK)
}

// --- Classic OAuth scopes ---

// TestClassicScopesGateOrganizationDeletion is the finding itself: the scope
// string was stored, persisted and echoed in X-OAuth-Scopes while being
// enforced nowhere, so a token the user deliberately narrowed to read:org
// deleted their organizations.
func TestClassicScopesGateOrganizationDeletion(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := s.createTestUser(t, "scopedel-owner")
	readOnly := s.store.CreateToken(owner.ID, "read:org").Value
	admin := s.store.CreateToken(owner.ID, "admin:org").Value

	if org := s.store.CreateOrg(owner, "scopedel-org", "", ""); org == nil {
		t.Fatal("CreateOrg returned nil")
	}

	requireStatus(t, s.delete(t, "/api/v3/orgs/scopedel-org", readOnly), http.StatusForbidden)
	if s.store.GetOrg("scopedel-org") == nil {
		t.Fatal("a read:org token deleted the organization")
	}

	// Positive control: the scope that does grant it still does.
	requireStatus(t, s.delete(t, "/api/v3/orgs/scopedel-org", admin), http.StatusNoContent)
	if s.store.GetOrg("scopedel-org") != nil {
		t.Fatal("an admin:org token failed to delete the organization")
	}
}

// TestClassicScopesAdmitWhatTheyGrant is the over-blocking control. Each of
// these is a write a real classic scope selection permits, and each one is
// what a too-tight gate breaks first — CI drives this surface with the real
// gh CLI.
func TestClassicScopesAdmitWhatTheyGrant(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := s.createTestUser(t, "scopeok-owner")
	repoTok := s.store.CreateToken(owner.ID, "repo").Value
	adminOrgTok := s.store.CreateToken(owner.ID, "admin:org").Value

	repo := s.store.CreateRepo(owner, "scopeok-repo", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	base := "/api/v3/repos/scopeok-owner/scopeok-repo"

	// `repo` covers repository contents, issues and repository administration.
	requireStatus(t, s.put(t, base+"/contents/hello.txt", repoTok, map[string]interface{}{
		"message": "add",
		"content": base64.StdEncoding.EncodeToString([]byte("hi\n")),
	}), http.StatusCreated)
	requireStatus(t, s.post(t, base+"/issues", repoTok, map[string]interface{}{"title": "t"}), http.StatusCreated)
	requireStatus(t, s.post(t, base+"/hooks", repoTok, map[string]interface{}{
		"config": map[string]interface{}{"url": "https://example.test/hook"},
	}), http.StatusCreated)
	requireStatus(t, s.patch(t, base, repoTok, map[string]interface{}{"description": "d"}), http.StatusOK)

	// `admin:org` covers organization administration.
	if org := s.store.CreateOrg(owner, "scopeok-org", "", ""); org == nil {
		t.Fatal("CreateOrg returned nil")
	}
	requireStatus(t, s.patch(t, "/api/v3/orgs/scopeok-org", adminOrgTok, map[string]interface{}{
		"description": "d",
	}), http.StatusOK)
	requireStatus(t, s.post(t, "/api/v3/orgs/scopeok-org/teams", adminOrgTok, map[string]interface{}{
		"name": "scopeok-team",
	}), http.StatusCreated)
}

// TestClassicScopeSeparatorsBothParse: X-OAuth-Scopes and stored PATs are
// comma-separated, the OAuth `scope` request parameter is space-separated, and
// this codebase mints tokens both ways. A parser that knows only one of them
// reads the other as a single nonsense scope and grants nothing.
func TestClassicScopeSeparatorsBothParse(t *testing.T) {
	t.Parallel()
	for _, scopes := range []string{"repo,read:org", "repo read:org", "repo, read:org", " repo\tread:org "} {
		if !classicScopeCovers(scopes, store.ScopeContents, store.PermWrite) {
			t.Errorf("scopes %q did not grant contents:write", scopes)
		}
		if !classicScopeCovers(scopes, store.ScopeOrgAdministration, store.PermRead) {
			t.Errorf("scopes %q did not grant organization_administration:read", scopes)
		}
		if classicScopeCovers(scopes, store.ScopeOrgAdministration, store.PermWrite) {
			t.Errorf("scopes %q granted organization_administration:write", scopes)
		}
	}
}

// TestUnscopedClassicTokenReadsPublicMetadataOnly documents what an empty
// scope string means. It is a real GitHub credential shape — a classic PAT
// with nothing selected reads public information — and not an internal
// "everything" escape hatch.
func TestUnscopedClassicTokenReadsPublicMetadataOnly(t *testing.T) {
	t.Parallel()
	if !classicScopeCovers("", store.ScopeMetadata, store.PermRead) {
		t.Error("an unscoped token cannot read public metadata")
	}
	for _, scope := range []store.PermScope{store.ScopeContents, store.ScopeIssues, store.ScopeOrgAdministration, store.ScopeAdministration} {
		if classicScopeCovers("", scope, store.PermWrite) {
			t.Errorf("an unscoped token was granted %s:write", scope)
		}
	}
}

// TestClassicScopeGrantsCoverEveryPermission is the loudness guard on the
// mapping table: a permission constant introduced without a classic mapping
// must fail here rather than turn into a silent deny (or, in the shape this
// replaced, a silent admit) for every classic credential.
func TestClassicScopeGrantsCoverEveryPermission(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{}
	fset := token.NewFileSet()
	// ARCH-001 moved the permScope constants to internal/store/apps_perms.go
	// (exported as PermScope); the loudness guard follows them.
	file, err := parser.ParseFile(fset, filepath.Join("..", "store", "apps_perms.go"), nil, 0)
	if err != nil {
		t.Fatalf("parsing internal/store/apps_perms.go: %v", err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		ident, ok := spec.Type.(*ast.Ident)
		if !ok || ident.Name != "PermScope" {
			return true
		}
		for _, name := range spec.Names {
			declared[name.Name] = true
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatal("found no permScope constants; the AST walk is looking in the wrong place")
	}

	listed := map[string]bool{}
	for _, scope := range allPermScopes {
		listed[string(scope)] = true
		if _, ok := classicScopeGrants[scope]; !ok {
			t.Errorf("permission %s has no classic OAuth scope mapping", scope)
		}
	}
	// The AST gives constant identifiers; map them to their values through the
	// enumerated list, which is what the rest of the package uses.
	if len(declared) != len(allPermScopes) {
		t.Errorf("gh_apps_perms.go declares %d permScope constants but allPermScopes lists %d; add the new one to allPermScopes and to classicScopeGrants",
			len(declared), len(allPermScopes))
	}
}

// --- Workflow trigger refs ---

const forkTriggerBaseYAML = `name: base-ci
on: [pull_request]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo base
`

const forkTriggerForkYAML = `name: fork-ci
on: [pull_request]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo fork
`

// TestForkPullRequestTriggersWithTheBaseWorkflowDefinition covers both halves:
// a fork pull request produced no run at all (the fork's branch does not
// resolve in the base repository), and the definition that does run must be
// the base repository's — a fork supplying its own could rewrite the workflow
// and read the base repository's secrets.
func TestForkPullRequestTriggersWithTheBaseWorkflowDefinition(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	const baseKey = "forkbase-owner/fork-app"
	const forkKey = "forkhead-owner/fork-app"
	s.cancelRepoRunsCleanup(t, baseKey)

	commitWorkflowYAMLToStorage(t, s.Server, baseKey, ".github/workflows/ci.yml", forkTriggerBaseYAML)
	forkHeadSha := commitWorkflowYAMLToStorage(t, s.Server, forkKey, ".github/workflows/ci.yml", forkTriggerForkYAML)

	payload := map[string]interface{}{
		"pull_request": map[string]interface{}{
			"number": float64(1),
			"head": map[string]interface{}{
				"ref":  "contrib-branch",
				"sha":  forkHeadSha,
				"repo": map[string]interface{}{"full_name": forkKey},
			},
			"base": map[string]interface{}{
				"ref":  "main",
				"repo": map[string]interface{}{"full_name": baseKey},
			},
		},
	}
	s.triggerWorkflowsForEvent(baseKey, "pull_request", "opened", "refs/heads/contrib-branch", payload)

	runs := s.runsForRepo(t, baseKey)
	if len(runs) != 1 {
		t.Fatalf("fork pull request produced %d runs, want 1", len(runs))
	}
	if runs[0].Name != "base-ci" {
		t.Fatalf("run used the %q definition; the base repository's is base-ci", runs[0].Name)
	}
	if runs[0].Sha != forkHeadSha {
		t.Fatalf("run sha = %q, want the pull request head %q", runs[0].Sha, forkHeadSha)
	}
	if runs[0].Ref != "refs/pull/1/merge" {
		t.Fatalf("run ref = %q, want refs/pull/1/merge", runs[0].Ref)
	}

	// And the approval machinery below the trigger, which was unreachable
	// while no fork pull request ever produced a run, now engages.
	perms := s.store.GetRepoActionsPermissions(baseKey)
	perms.ForkPRContributorApproval = "all_external_contributors"
	s.store.SetRepoActionsPermissions(baseKey, perms)
	s.triggerWorkflowsForEvent(baseKey, "pull_request", "synchronize", "refs/heads/contrib-branch", payload)

	runs = s.runsForRepo(t, baseKey)
	if len(runs) != 2 {
		t.Fatalf("second fork pull request event produced %d runs in total, want 2", len(runs))
	}
	held := 0
	for _, run := range runs {
		if run.Status == store.WorkflowStatusActionRequired {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("%d runs held for fork-PR approval, want 1", held)
	}
}

// TestPushRunsTheDefinitionOnThePushedRef: definitions were read from HEAD, so
// a push to any other branch ran whatever the default branch happened to say.
func TestPushRunsTheDefinitionOnThePushedRef(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	const repoKey = "refdef-owner/refdef-app"
	s.cancelRepoRunsCleanup(t, repoKey)

	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", `name: main-ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo main
`)
	stor := s.store.GetGitStorage("refdef-owner", "refdef-app")
	if stor == nil {
		t.Fatal("no git storage for the fixture repository")
	}
	mainSha := actions.ResolveRefSha(stor, "refs/heads/main")
	if mainSha == actions.ZeroCommitSha {
		t.Fatal("fixture main branch does not resolve")
	}
	if err := stor.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("feature"), plumbing.NewHash(mainSha))); err != nil {
		t.Fatalf("branch feature: %v", err)
	}
	if _, err := createFileCommit(stor, "feature", ".github/workflows/ci.yml", `name: feature-ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo feature
`, "redefine on feature", repoSignature("refdef-owner", "refdef@bleephub.local")); err != nil {
		t.Fatalf("commit on feature: %v", err)
	}
	// Committing through a worktree leaves HEAD on the commit it just made.
	// The fixture is a bare repository whose HEAD is the default branch, and
	// the whole point of the test is that HEAD is not the triggering ref.
	if err := stor.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("restore HEAD: %v", err)
	}

	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/feature", nil)
	runs := s.runsForRepo(t, repoKey)
	if len(runs) != 1 || runs[0].Name != "feature-ci" {
		t.Fatalf("push to feature produced %v, want one run named feature-ci", runNames(runs))
	}

	// Positive control: the default branch still runs its own definition.
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)
	runs = s.runsForRepo(t, repoKey)
	if len(runs) != 2 {
		t.Fatalf("push to main produced %v, want a second run", runNames(runs))
	}
	names := runNames(runs)
	if !strings.Contains(strings.Join(names, ","), "main-ci") {
		t.Fatalf("push to main produced %v, want one named main-ci", names)
	}
}

func (s *isolatedServer) runsForRepo(t *testing.T, repoKey string) []*store.Workflow {
	t.Helper()
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	var out []*store.Workflow
	for _, wf := range s.store.Workflows {
		if wf.RepoFullName == repoKey {
			out = append(out, wf)
		}
	}
	return out
}

func runNames(runs []*store.Workflow) []string {
	names := make([]string, 0, len(runs))
	for _, run := range runs {
		names = append(names, run.Name)
	}
	return names
}

// --- Job leases ---

// TestExpiredJobLeaseIsRedelivered: the lease was written at dispatch, renewed
// on every runner poll, and read nowhere, so a runner that vanished mid-job
// left the job assigned to it forever.
func TestExpiredJobLeaseIsRedelivered(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	const repoKey = "leaseowner/lease-app"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", `name: lease-ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	var wf *store.Workflow
	waitUntil(t, "dispatched run", func() bool {
		runs := s.runsForRepo(t, repoKey)
		if len(runs) == 0 {
			return false
		}
		wf = runs[0]
		return len(wf.Jobs) > 0
	})

	var jobID string
	for _, wfJob := range wf.Jobs {
		jobID = wfJob.JobID
	}
	waitUntil(t, "queued job message", func() bool { return s.pendingMessageFor(jobID) != nil })

	// A runner takes the message and then disappears: the message leaves the
	// queue, the job is assigned, and nothing ever renews the lease.
	s.store.Mu.Lock()
	remaining := s.store.PendingMessages[:0]
	for _, msg := range s.store.PendingMessages {
		if msg.JobID != jobID {
			remaining = append(remaining, msg)
		}
	}
	s.store.PendingMessages = remaining
	job := s.store.Jobs[jobID]
	if job == nil {
		s.store.Mu.Unlock()
		t.Fatalf("no engine job for %s", jobID)
	}
	job.AgentID = 4242
	job.Status = "running"
	job.LockedUntil = fixedTestTime.Add(-time.Minute)
	s.store.Mu.Unlock()

	s.actions.CheckJobTimeouts(wf)

	if s.pendingMessageFor(jobID) == nil {
		t.Fatal("an expired lease left the job with the runner that vanished; it was never requeued")
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	if job.AgentID != 0 {
		t.Errorf("reclaimed job is still assigned to agent %d", job.AgentID)
	}
	if job.Status != "queued" {
		t.Errorf("reclaimed job status = %q, want queued", job.Status)
	}
	if !job.LockedUntil.After(fixedTestTime) {
		t.Error("reclaimed job kept its expired lease and will be reclaimed again on every tick")
	}
}

// TestLiveJobLeaseIsLeftAlone is the control: reclaiming must not steal a job
// from a runner that is still renewing.
func TestLiveJobLeaseIsLeftAlone(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	const repoKey = "leaselive/lease-app"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", `name: lease-live
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	var wf *store.Workflow
	waitUntil(t, "dispatched run", func() bool {
		runs := s.runsForRepo(t, repoKey)
		if len(runs) == 0 {
			return false
		}
		wf = runs[0]
		return len(wf.Jobs) > 0
	})
	var jobID string
	for _, wfJob := range wf.Jobs {
		jobID = wfJob.JobID
	}
	waitUntil(t, "queued job message", func() bool { return s.pendingMessageFor(jobID) != nil })

	s.store.Mu.Lock()
	remaining := s.store.PendingMessages[:0]
	for _, msg := range s.store.PendingMessages {
		if msg.JobID != jobID {
			remaining = append(remaining, msg)
		}
	}
	s.store.PendingMessages = remaining
	job := s.store.Jobs[jobID]
	job.AgentID = 99
	job.Status = "running"
	job.LockedUntil = fixedTestTime.Add(time.Hour)
	s.store.Mu.Unlock()

	s.actions.CheckJobTimeouts(wf)

	if s.pendingMessageFor(jobID) != nil {
		t.Fatal("a live lease was reclaimed; the job would run twice")
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	if job.AgentID != 99 {
		t.Errorf("live job was unassigned from its runner (agent %d)", job.AgentID)
	}
}

func (s *isolatedServer) pendingMessageFor(jobID string) *store.TaskAgentMessage {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, msg := range s.store.PendingMessages {
		if msg.JobID == jobID {
			return msg
		}
	}
	return nil
}

// --- Contents preconditions ---

// TestPutContentsEnforcesTheBlobSHA: the sha was decoded and never read, so
// every write was an unconditional overwrite and two editors of the same file
// silently lost one edit.
func TestPutContentsEnforcesTheBlobSHA(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":      "contents-sha",
		"auto_init": true,
	}).Body.Close()
	const path = "/api/v3/repos/admin/contents-sha/contents/note.txt"

	// Creating carries no sha.
	created := decodeJSONWithStatus(t, s.put(t, path, defaultToken, map[string]interface{}{
		"message": "create",
		"content": base64.StdEncoding.EncodeToString([]byte("one\n")),
	}), http.StatusCreated)
	content, _ := created["content"].(map[string]interface{})
	currentSHA, _ := content["sha"].(string)
	if currentSHA == "" {
		t.Fatalf("create response carried no content sha: %v", created)
	}

	// Replacing with a sha that is not the current blob is a lost update.
	requireStatus(t, s.put(t, path, defaultToken, map[string]interface{}{
		"message": "stale overwrite",
		"content": base64.StdEncoding.EncodeToString([]byte("stale\n")),
		"sha":     "0000000000000000000000000000000000000000",
	}), http.StatusConflict)

	// Replacing without any sha is equally blind.
	requireStatus(t, s.put(t, path, defaultToken, map[string]interface{}{
		"message": "blind overwrite",
		"content": base64.StdEncoding.EncodeToString([]byte("blind\n")),
	}), http.StatusUnprocessableEntity)

	// The refusals must not have changed the file.
	got := decodeJSONWithStatus(t, s.get(t, path, defaultToken), http.StatusOK)
	if got["sha"] != currentSHA {
		t.Fatalf("a refused write changed the file: sha %v, want %v", got["sha"], currentSHA)
	}

	// Positive control: the current sha replaces it, and updating answers 200.
	updated := decodeJSONWithStatus(t, s.put(t, path, defaultToken, map[string]interface{}{
		"message": "update",
		"content": base64.StdEncoding.EncodeToString([]byte("two\n")),
		"sha":     currentSHA,
	}), http.StatusOK)
	newContent, _ := updated["content"].(map[string]interface{})
	if newContent["sha"] == currentSHA {
		t.Fatal("the accepted write did not change the blob")
	}

	// A sha for a path that holds no file names something that is not there.
	requireStatus(t, s.put(t, "/api/v3/repos/admin/contents-sha/contents/absent.txt", defaultToken,
		map[string]interface{}{
			"message": "create with a sha",
			"content": base64.StdEncoding.EncodeToString([]byte("x\n")),
			"sha":     currentSHA,
		}), http.StatusUnprocessableEntity)
}

// --- Pull requests are issues ---

// TestPullRequestsAppearAsIssues: on GitHub every pull request is also an
// issue, and the `pull_request` member is what distinguishes them. Both the
// listing and the by-number read omitted pull requests entirely.
func TestPullRequestsAppearAsIssues(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := s.createTestUser(t, "prissue-owner")
	repo := s.store.CreateRepo(owner, "prissue-repo", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	seedStorePullRequestBranches(t, s.store, repo, "feature")
	issue := s.store.CreateIssue(repo.ID, owner.ID, "a plain issue", "", nil, nil, 0)
	if issue == nil {
		t.Fatal("CreateIssue returned nil")
	}
	pr := s.store.CreatePullRequest(repo.ID, owner.ID, "a pull request", "", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("CreatePullRequest returned nil")
	}

	listed := decodeJSONArray(t, s.get(t, "/api/v3/repos/prissue-owner/prissue-repo/issues", defaultToken))
	byNumber := map[float64]map[string]interface{}{}
	for _, row := range listed {
		number, _ := row["number"].(float64)
		byNumber[number] = row
	}

	prRow := byNumber[float64(pr.Number)]
	if prRow == nil {
		t.Fatalf("pull request #%d is missing from the issues list (%d rows)", pr.Number, len(listed))
	}
	links, _ := prRow["pull_request"].(map[string]interface{})
	if links == nil {
		t.Fatalf("pull request row carries no pull_request member: %v", prRow)
	}
	for _, key := range []string{"url", "html_url", "diff_url", "patch_url"} {
		if s, _ := links[key].(string); s == "" {
			t.Errorf("pull_request.%s is empty", key)
		}
	}

	issueRow := byNumber[float64(issue.Number)]
	if issueRow == nil {
		t.Fatalf("plain issue #%d disappeared from the issues list", issue.Number)
	}
	if _, has := issueRow["pull_request"]; has {
		t.Error("a plain issue carries a pull_request member; nothing could tell the two apart")
	}

	// The by-number read answers for a pull request too.
	fetched := decodeJSONWithStatus(t,
		s.get(t, "/api/v3/repos/prissue-owner/prissue-repo/issues/"+strconv.Itoa(pr.Number), defaultToken),
		http.StatusOK)
	if _, has := fetched["pull_request"]; !has {
		t.Errorf("GET issues/%d carries no pull_request member: %v", pr.Number, fetched)
	}
	if fetched["title"] != "a pull request" {
		t.Errorf("GET issues/%d returned %v", pr.Number, fetched["title"])
	}
}
