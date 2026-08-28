package graphqlapi

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// seedActionsRun wires a repository with a workflow file, a workflow run, a
// check suite linked to that run, and a completed check run (with an
// annotation) on the head commit. It returns the head sha the rollup keys on.
func seedActionsRun(t *testing.T, h *accountHarness, repo *store.Repo) string {
	t.Helper()
	h.commitRepoFiles(repo, map[string]string{"main.go": "package main\n"})
	sha := defaultBranchCommit(t, h.store, repo)

	file := h.store.RegisterWorkflowFile(repo.FullName, ".github/workflows/ci.yml", "CI", "on: push\n", "api")
	if file == nil {
		t.Fatal("workflow file not registered")
	}

	suite := h.store.CreateCheckSuite(repo.FullName, repo.DefaultBranch, sha, 0)
	if suite == nil {
		t.Fatal("check suite not created")
	}

	wf := &store.Workflow{
		ID:             "run-1",
		Name:           "CI",
		DisplayTitle:   "CI on main",
		RunID:          4242,
		RunNumber:      7,
		Attempt:        1,
		EventName:      "push",
		Status:         store.WorkflowStatusCompleted,
		Result:         store.ResultSuccess,
		CreatedAt:      h.store.CurrentTime(),
		RepoFullName:   repo.FullName,
		Sha:            sha,
		WorkflowFileID: file.ID,
		CheckSuiteID:   suite.ID,
	}
	h.store.Mu.Lock()
	h.store.Workflows[wf.ID] = wf
	// Link the suite back to the run so CheckSuite.workflowRun resolves it.
	live := h.store.CheckSuites[suite.ID]
	live.WorkflowRunID = wf.RunID
	live.WorkflowName = wf.Name
	live.WorkflowFileID = file.ID
	h.store.Mu.Unlock()

	cr := h.store.CreateCheckRun(repo.FullName, sha, "build", 0, suite.ID)
	if cr == nil {
		t.Fatal("check run not created")
	}
	h.store.Mu.Lock()
	liveRun := h.store.CheckRuns[cr.ID]
	liveRun.Status = "completed"
	liveRun.Conclusion = "success"
	liveRun.Output = &store.CheckRunOutput{
		Title:   "Build",
		Summary: "ok",
		Annotations: []*store.CheckAnnotation{{
			Path:            "main.go",
			StartLine:       1,
			EndLine:         1,
			AnnotationLevel: "warning",
			Message:         "watch out",
			Title:           "note",
		}},
	}
	h.store.Mu.Unlock()
	return sha
}

const actionsRollupQuery = `query($owner:String!, $name:String!, $oid:GitObjectID!) {
  repository(owner:$owner, name:$name) {
    object(oid:$oid) {
      ... on Commit {
        statusCheckRollup {
          contexts(first:10) {
            nodes {
              ... on CheckRun {
                name
                url
                resourcePath
                permalink
                repository { nameWithOwner }
                annotations(first:5) { totalCount nodes { path message annotationLevel location { start { line } end { line } } } }
                steps(first:5) { totalCount }
                checkSuite {
                  id
                  status
                  url
                  resourcePath
                  app { name }
                  creator { login }
                  push { id }
                  branch { name }
                  commit { oid }
                  repository { nameWithOwner }
                  checkRuns(first:5) { totalCount nodes { name } }
                  matchingPullRequests(first:5) { totalCount }
                  workflowRun {
                    id
                    databaseId
                    runNumber
                    runAttempt
                    event
                    displayTitle
                    url
                    resourcePath
                    createdAt
                    updatedAt
                    checkSuite { id }
                    file { path url repositoryName viewerCanReadRepository viewerCanPushRepository run { id } }
                    workflow { name state url resourcePath databaseId runs(first:5) { totalCount nodes { runNumber } } }
                    deploymentReviews(first:5) { totalCount }
                    pendingDeploymentRequests(first:5) { totalCount }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

// TestActionsRunGraphIsBackedByRealData exercises the whole Actions/Checks run
// graph residue — CheckRun, CheckSuite, WorkflowRun, Workflow and
// WorkflowRunFile — reached through a commit's status-check rollup, asserting
// every value comes from the real check/workflow stores.
func TestActionsRunGraphIsBackedByRealData(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	repo := h.store.CreateRepo(owner, "actions", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	sha := seedActionsRun(t, h, repo)

	data := h.query(owner, actionsRollupQuery, map[string]interface{}{"owner": "admin", "name": "actions", "oid": sha})

	nodes, ok := at(t, data, "repository", "object", "statusCheckRollup", "contexts", "nodes").([]interface{})
	if !ok || len(nodes) != 1 {
		t.Fatalf("expected one rollup context, got %#v", at(t, data, "repository", "object", "statusCheckRollup", "contexts", "nodes"))
	}
	run := nodes[0].(map[string]interface{})

	if run["name"] != "build" {
		t.Errorf("checkRun.name = %v", run["name"])
	}
	if run["resourcePath"] == nil || run["url"] == nil || run["permalink"] == nil {
		t.Errorf("checkRun url members are null: %#v", run)
	}
	if got := at(t, run, "repository", "nameWithOwner"); got != "admin/actions" {
		t.Errorf("checkRun.repository = %v", got)
	}
	if got := at(t, run, "annotations", "totalCount"); got != float64(1) {
		t.Errorf("annotations.totalCount = %v", got)
	}
	ann := at(t, run, "annotations", "nodes").([]interface{})[0].(map[string]interface{})
	if ann["path"] != "main.go" || ann["annotationLevel"] != "WARNING" {
		t.Errorf("annotation = %#v", ann)
	}
	if got := at(t, ann, "location", "start", "line"); got != float64(1) {
		t.Errorf("annotation.location.start.line = %v", got)
	}
	if got := at(t, run, "steps", "totalCount"); got != float64(0) {
		t.Errorf("steps.totalCount = %v, want 0 (steps live on jobs)", got)
	}

	suite := run["checkSuite"].(map[string]interface{})
	if suite["status"] == nil || suite["url"] == nil || suite["resourcePath"] == nil {
		t.Errorf("checkSuite scalar members null: %#v", suite)
	}
	if suite["app"] != nil {
		t.Errorf("checkSuite.app should be null (appID 0), got %v", suite["app"])
	}
	if suite["creator"] != nil {
		t.Errorf("checkSuite.creator should be null, got %v", suite["creator"])
	}
	if suite["push"] != nil {
		t.Errorf("checkSuite.push should be null, got %v", suite["push"])
	}
	if got := at(t, suite, "branch", "name"); got != repo.DefaultBranch {
		t.Errorf("checkSuite.branch.name = %v, want %v", got, repo.DefaultBranch)
	}
	if got := at(t, suite, "commit", "oid"); got != sha {
		t.Errorf("checkSuite.commit.oid = %v, want %v", got, sha)
	}
	if got := at(t, suite, "repository", "nameWithOwner"); got != "admin/actions" {
		t.Errorf("checkSuite.repository = %v", got)
	}
	if got := at(t, suite, "checkRuns", "totalCount"); got != float64(1) {
		t.Errorf("checkSuite.checkRuns.totalCount = %v", got)
	}
	if got := at(t, suite, "matchingPullRequests", "totalCount"); got != float64(0) {
		t.Errorf("checkSuite.matchingPullRequests.totalCount = %v", got)
	}

	wr := suite["workflowRun"].(map[string]interface{})
	if wr["id"] != "WFR_run-1" {
		t.Errorf("workflowRun.id = %v", wr["id"])
	}
	if got := wr["databaseId"]; got != float64(4242) {
		t.Errorf("workflowRun.databaseId = %v", got)
	}
	if got := wr["runNumber"]; got != float64(7) {
		t.Errorf("workflowRun.runNumber = %v", got)
	}
	if got := wr["runAttempt"]; got != float64(1) {
		t.Errorf("workflowRun.runAttempt = %v", got)
	}
	if wr["event"] != "push" {
		t.Errorf("workflowRun.event = %v", wr["event"])
	}
	if wr["displayTitle"] != "CI on main" {
		t.Errorf("workflowRun.displayTitle = %v", wr["displayTitle"])
	}
	if wr["url"] == nil || wr["createdAt"] == nil || wr["updatedAt"] == nil {
		t.Errorf("workflowRun scalar members null: %#v", wr)
	}
	if got := at(t, wr, "checkSuite", "id"); got != suite["id"] {
		t.Errorf("workflowRun.checkSuite.id = %v, want %v", got, suite["id"])
	}
	file := wr["file"].(map[string]interface{})
	if file["path"] != ".github/workflows/ci.yml" {
		t.Errorf("workflowRun.file.path = %v", file["path"])
	}
	if file["viewerCanReadRepository"] != true {
		t.Errorf("file.viewerCanReadRepository = %v", file["viewerCanReadRepository"])
	}
	if got := at(t, file, "run", "id"); got != "WFR_run-1" {
		t.Errorf("file.run.id = %v", got)
	}
	wfl := wr["workflow"].(map[string]interface{})
	if wfl["name"] != "CI" || wfl["state"] != "ACTIVE" {
		t.Errorf("workflow = %#v", wfl)
	}
	if got := at(t, wfl, "runs", "totalCount"); got != float64(1) {
		t.Errorf("workflow.runs.totalCount = %v", got)
	}
	if got := at(t, wfl, "runs", "nodes").([]interface{})[0].(map[string]interface{})["runNumber"]; got != float64(7) {
		t.Errorf("workflow.runs.nodes[0].runNumber = %v", got)
	}
	if got := at(t, wr, "deploymentReviews", "totalCount"); got != float64(0) {
		t.Errorf("deploymentReviews.totalCount = %v", got)
	}
	if got := at(t, wr, "pendingDeploymentRequests", "totalCount"); got != float64(0) {
		t.Errorf("pendingDeploymentRequests.totalCount = %v", got)
	}
}

// TestActionsRunGraphRefusesAStranger asserts a private repository's Actions
// run graph is not reachable by an outsider: the repository root gate returns
// null, so no check suite, workflow run or commit leaks behind it.
func TestActionsRunGraphRefusesAStranger(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	stranger := h.user("outsider")
	repo := h.store.CreateRepo(owner, "sealed-actions", "", true)
	if repo == nil {
		t.Fatal("repository not created")
	}
	sha := seedActionsRun(t, h, repo)

	vars := map[string]interface{}{"owner": "admin", "name": "sealed-actions", "oid": sha}

	// The stranger is refused the private repository entirely: the root
	// resolver answers a NOT_FOUND error and a null repository, so nothing
	// behind it (check suite, workflow run, commit) is ever reached.
	strangerData, errs := h.queryWithErrors(stranger, actionsRollupQuery, vars)
	if len(errs) == 0 {
		t.Fatal("stranger was not refused the private repository")
	}
	if strangerData["repository"] != nil {
		t.Fatalf("stranger reached a private repository's actions graph: %#v", strangerData["repository"])
	}

	ownerData := h.query(owner, actionsRollupQuery, vars)
	if ownerData["repository"] == nil {
		t.Fatal("owner was refused their own private repository")
	}
	nodes, ok := at(t, ownerData, "repository", "object", "statusCheckRollup", "contexts", "nodes").([]interface{})
	if !ok || len(nodes) != 1 {
		t.Fatalf("owner expected one rollup context, got %#v", nodes)
	}
}
