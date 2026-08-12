package bleephub

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

func (s *isolatedServer) ensureTestOrgRepo(t *testing.T, repo string) {
	t.Helper()
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		t.Fatalf("expected owner/repo, got %q", repo)
	}
	if s.store.GetRepoByFullName(repo) != nil {
		return
	}
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.GetOrg(parts[0])
	if org == nil {
		org = s.store.CreateOrg(admin, parts[0], parts[0], "")
	}
	if org == nil {
		t.Fatalf("create org %s", parts[0])
	}
	if created := s.store.CreateOrgRepo(org, admin, parts[1], "", false); created == nil {
		t.Fatalf("create repo %s", repo)
	}
}

// seedGatedRun installs a run held in action_required (the fork-PR
// approval gate) with one dispatchable pending job, wired so approval
// can dispatch through the real engine.
func (s *isolatedServer) seedGatedRun(t *testing.T, repo string) *store.Workflow {
	t.Helper()
	s.ensureTestOrgRepo(t, repo)
	s.store.Mu.Lock()
	runID := s.store.NextRunID
	s.store.NextRunID++
	wf := &store.Workflow{
		ID:           uuid.New().String(),
		Name:         "gated",
		RunID:        runID,
		RunNumber:    runID,
		Status:       store.WorkflowStatusActionRequired,
		CreatedAt:    fixedTestTime,
		EventName:    "pull_request",
		Ref:          "refs/heads/feature",
		Sha:          "abcdef0123456789abcdef0123456789abcdef01",
		RepoFullName: repo,
		Env:          map[string]string{"__serverURL": s.baseURL, "__defaultImage": "alpine:latest"},
		Jobs:         map[string]*store.WorkflowJob{},
	}
	wf.Jobs["build"] = &store.WorkflowJob{
		Key:         "build",
		JobID:       uuid.New().String(),
		DisplayName: "Build",
		Status:      store.JobStatusPending,
		Outputs:     map[string]string{},
		Def:         &store.JobDef{RunsOn: "ubuntu-latest", Steps: []store.StepDef{{Run: "echo hi"}}},
	}
	s.store.Workflows[wf.ID] = wf
	s.store.Mu.Unlock()
	return wf
}

func TestApproveWorkflowRun_ReleasesGatedRun(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := "approve-org/approve-repo"
	wf := s.seedGatedRun(t, repo)
	base := fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d", repo, wf.RunID)

	// The gated run reports action_required.
	data := decodeJSONWithStatus(t, s.get(t, base, defaultToken), 200)
	if data["status"] != "action_required" {
		t.Fatalf("gated run status = %v, want action_required", data["status"])
	}

	// Approval releases it: jobs dispatch and the run leaves the gate.
	resp := s.post(t, base+"/approve", defaultToken, nil)
	body := decodeJSONWithStatus(t, resp, 201)
	if len(body) != 0 {
		t.Fatalf("approve body = %v, want empty object", body)
	}
	data = decodeJSONWithStatus(t, s.get(t, base, defaultToken), 200)
	if data["status"] != "queued" {
		t.Fatalf("approved run status = %v, want queued (job dispatched)", data["status"])
	}

	// A run that isn't waiting for approval refuses a second approval.
	resp = s.post(t, base+"/approve", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("re-approve = %d, want 403", resp.StatusCode)
	}

	// Unknown run 404s.
	resp = s.post(t, "/api/v3/repos/"+repo+"/actions/runs/999999/approve", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("approve unknown run = %d, want 404", resp.StatusCode)
	}

	// Force-cancel terminates the queued job immediately.
	resp = s.post(t, base+"/force-cancel", defaultToken, nil)
	body = decodeJSONWithStatus(t, resp, 202)
	if len(body) != 0 {
		t.Fatalf("force-cancel body = %v, want empty object", body)
	}
	data = decodeJSONWithStatus(t, s.get(t, base, defaultToken), 200)
	if data["status"] != "completed" || data["conclusion"] != "cancelled" {
		t.Fatalf("force-cancelled run = status %v / conclusion %v, want completed/cancelled",
			data["status"], data["conclusion"])
	}

	// Force-cancelling a completed run conflicts.
	resp = s.post(t, base+"/force-cancel", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("force-cancel completed run = %d, want 409", resp.StatusCode)
	}
}

// TestForkPRApprovalGate_EngineHoldsForkRuns drives the submit-time
// gate: a pull_request run whose head repository differs from the base
// holds in action_required when the repo policy requires approval.
func TestForkPRApprovalGate_EngineHoldsForkRuns(t *testing.T) {
	s := newTestServer()
	repo := "gate-org/gate-repo"
	perms := s.store.GetRepoActionsPermissions(repo)
	perms.ForkPRContributorApproval = "all_external_contributors"
	s.store.SetRepoActionsPermissions(repo, perms)

	forkPayload := map[string]interface{}{
		"pull_request": map[string]interface{}{
			"head": map[string]interface{}{
				"repo": map[string]interface{}{"full_name": "outsider/gate-repo", "fork": true},
			},
		},
	}
	def := &store.WorkflowDef{
		Name: "fork-gated",
		Jobs: map[string]*store.JobDef{
			"build": {RunsOn: "ubuntu-latest", Steps: []store.StepDef{{Run: "echo hi"}}},
		},
	}
	wf, err := s.actions.SubmitWorkflow(t.Context(), "http://127.0.0.1:0", def, "alpine:latest", &actions.WorkflowEventMeta{
		EventName: "pull_request",
		Repo:      repo,
		Payload:   forkPayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != store.WorkflowStatusActionRequired {
		t.Fatalf("fork-PR run status = %q, want action_required", wf.Status)
	}
	if wf.Jobs["build"].Status != store.JobStatusPending {
		t.Fatalf("gated job status = %q, want pending (not dispatched)", wf.Jobs["build"].Status)
	}

	// A same-repository pull request is never gated.
	samePayload := map[string]interface{}{
		"pull_request": map[string]interface{}{
			"head": map[string]interface{}{
				"repo": map[string]interface{}{"full_name": repo, "fork": false},
			},
		},
	}
	def2 := &store.WorkflowDef{
		Name: "same-repo",
		Jobs: map[string]*store.JobDef{
			"build": {RunsOn: "ubuntu-latest", Steps: []store.StepDef{{Run: "echo hi"}}},
		},
	}
	wf2, err := s.actions.SubmitWorkflow(t.Context(), "http://127.0.0.1:0", def2, "alpine:latest", &actions.WorkflowEventMeta{
		EventName: "pull_request",
		Repo:      repo,
		Payload:   samePayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wf2.Status == store.WorkflowStatusActionRequired {
		t.Fatalf("same-repo PR run gated: %q", wf2.Status)
	}
	s.actions.CancelWorkflow(wf2)
}

func TestRerunWorkflowJob_NewAttemptCarriesOtherJobs(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := "jobrr-org/jobrr-repo"
	s.ensureTestOrgRepo(t, repo)
	yaml := "name: jobrr\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo a\n  b:\n    runs-on: ubuntu-latest\n    needs: a\n    steps:\n      - run: echo b\n"
	s.store.RegisterWorkflowFile(repo, ".github/workflows/jobrr.yml", "jobrr", yaml, "submitted")

	// Seed the completed first attempt with both jobs succeeded.
	s.store.Mu.Lock()
	runID := s.store.NextRunID
	s.store.NextRunID++
	wf := &store.Workflow{
		ID:           uuid.New().String(),
		Name:         "jobrr",
		RunID:        runID,
		RunNumber:    runID,
		Status:       store.WorkflowStatusCompleted,
		Result:       store.ResultSuccess,
		CreatedAt:    fixedTestTime,
		EventName:    "push",
		Ref:          "refs/heads/main",
		Sha:          "abcdef0123456789abcdef0123456789abcdef01",
		RepoFullName: repo,
		Jobs:         map[string]*store.WorkflowJob{},
	}
	for _, key := range []string{"a", "b"} {
		wf.Jobs[key] = &store.WorkflowJob{
			Key: key, JobID: uuid.New().String(), DisplayName: key,
			Status: store.JobStatusCompleted, Result: store.ResultSuccess,
			StartedAt: fixedTestTime.Add(-time.Minute), CompletedAt: fixedTestTime,
			Outputs: map[string]string{},
		}
	}
	wf.Jobs["b"].Needs = []string{"a"}
	s.store.Workflows[wf.ID] = wf
	s.store.Mu.Unlock()

	targetID := stableJobID(wf.Jobs["b"].JobID)
	base := fmt.Sprintf("/api/v3/repos/%s/actions", repo)

	// Re-run job b: a new attempt where a carries over and b re-runs.
	body := decodeJSONWithStatus(t, s.post(t,
		fmt.Sprintf("%s/jobs/%d/rerun", base, targetID), defaultToken, nil), 201)
	if len(body) != 0 {
		t.Fatalf("job rerun body = %v, want empty object", body)
	}

	run := decodeJSONWithStatus(t, s.get(t, fmt.Sprintf("%s/runs/%d", base, runID), defaultToken), 200)
	if run["run_attempt"].(float64) != 2 {
		t.Fatalf("run_attempt = %v, want 2", run["run_attempt"])
	}
	jobs := decodeJSONWithStatus(t, s.get(t, fmt.Sprintf("%s/runs/%d/jobs", base, runID), defaultToken), 200)
	byName := map[string]map[string]interface{}{}
	for _, j := range jobs["jobs"].([]interface{}) {
		job, _ := j.(map[string]interface{})
		byName[job["name"].(string)] = job
	}
	if byName["a"] == nil || byName["a"]["status"] != "completed" || byName["a"]["conclusion"] != "success" {
		t.Fatalf("carried job a = %v, want completed/success", byName["a"])
	}
	if byName["b"] == nil || byName["b"]["status"] == "completed" {
		t.Fatalf("target job b = %v, want re-dispatched (not completed)", byName["b"])
	}

	// The first attempt stays retrievable.
	attempt1 := decodeJSONWithStatus(t, s.get(t,
		fmt.Sprintf("%s/runs/%d/attempts/1", base, runID), defaultToken), 200)
	if attempt1["run_attempt"].(float64) != 1 {
		t.Fatalf("attempt 1 run_attempt = %v", attempt1["run_attempt"])
	}

	// Re-running a job of an in-progress run refuses. The new attempt's
	// job carries a fresh id; compute it from the store (the JSON float64
	// round-trip cannot represent the 63-bit id exactly).
	newWf := s.findWorkflowByRunID(runID)
	s.assertWorkflowJobsUseHostMode(t, newWf, "b")
	s.store.Mu.RLock()
	newTargetID := stableJobID(newWf.Jobs["b"].JobID)
	s.store.Mu.RUnlock()
	resp := s.post(t, fmt.Sprintf("%s/jobs/%d/rerun", base, newTargetID), defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("rerun of in-progress run = %d, want 403", resp.StatusCode)
	}

	// Unknown job 404s.
	resp = s.post(t, base+"/jobs/123456789/rerun", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("rerun unknown job = %d, want 404", resp.StatusCode)
	}

	// Clean up the dispatched attempt.
	resp = s.post(t, fmt.Sprintf("%s/runs/%d/force-cancel", base, runID), defaultToken, nil)
	resp.Body.Close()
}

func TestReviewCustomDeploymentProtectionRule(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := s.createTestRepo(t).fullName()
	repoData := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/"+repoKey, defaultToken), 200)
	repoID := int(repoData["id"].(float64))
	env := s.store.Deployments.UpsertEnvironment(repoID, "production")

	// Seed a run waiting on the environment's protection rule.
	s.store.Mu.Lock()
	runID := s.store.NextRunID
	s.store.NextRunID++
	wf := &store.Workflow{
		ID:           uuid.New().String(),
		Name:         "deploy",
		RunID:        runID,
		RunNumber:    runID,
		Status:       store.WorkflowStatusWaiting,
		CreatedAt:    fixedTestTime,
		EventName:    "push",
		Ref:          "refs/heads/main",
		Sha:          "abcdef0123456789abcdef0123456789abcdef01",
		RepoFullName: repoKey,
		Env:          map[string]string{"__serverURL": s.baseURL, "__defaultImage": "alpine:latest"},
		Jobs:         map[string]*store.WorkflowJob{},
		PendingDeployments: []*store.PendingDeployment{
			{EnvID: env.ID, EnvName: "production", WaitTimerStartedAt: fixedTestTime.UTC()},
		},
	}
	wf.Jobs["deploy"] = &store.WorkflowJob{
		Key: "deploy", JobID: uuid.New().String(), DisplayName: "Deploy",
		Status:  store.JobStatusWaiting,
		Outputs: map[string]string{},
		Def: &store.JobDef{
			RunsOn: "ubuntu-latest", Environment: "production",
			Steps: []store.StepDef{{Run: "echo deploy"}},
		},
	}
	s.store.Workflows[wf.ID] = wf
	s.store.Mu.Unlock()

	base := fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d", repoKey, runID)

	// Validation: missing environment_name; unknown environment.
	resp := s.post(t, base+"/deployment_protection_rule", defaultToken, map[string]string{"state": "approved"})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("missing environment_name = %d, want 422", resp.StatusCode)
	}
	resp = s.post(t, base+"/deployment_protection_rule", defaultToken,
		map[string]string{"environment_name": "staging", "state": "approved"})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("unknown environment = %d, want 422", resp.StatusCode)
	}

	// Comment-only review records the comment and keeps the run waiting.
	resp = s.post(t, base+"/deployment_protection_rule", defaultToken,
		map[string]string{"environment_name": "production", "comment": "checking the gate"})
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("comment-only review = %d, want 204", resp.StatusCode)
	}
	data := decodeJSONWithStatus(t, s.get(t, base, defaultToken), 200)
	if data["status"] != "waiting" {
		t.Fatalf("run after comment-only review = %v, want still waiting", data["status"])
	}

	// Approval releases the waiting job.
	resp = s.post(t, base+"/deployment_protection_rule", defaultToken,
		map[string]string{"environment_name": "production", "state": "approved", "comment": "gate passed"})
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("approving review = %d, want 204", resp.StatusCode)
	}
	data = decodeJSONWithStatus(t, s.get(t, base, defaultToken), 200)
	if data["status"] != "queued" {
		t.Fatalf("run after approval = %v, want queued (job dispatched)", data["status"])
	}

	// Clean up the dispatched job.
	resp = s.post(t, base+"/force-cancel", defaultToken, nil)
	resp.Body.Close()
}

func TestRunAttemptLogs_ServesAttemptArchive(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	wf, job := seedRun(t, s.Server, "logs-org/logs-repo", "completed", "success")
	planID, timelineID := linkJobToPlan(t, s.Server, job)
	logID := createLogFile(t, s.Server, planID)
	uploadLogBlock(t, s.Server, planID, logID, []byte("attempt uploaded log\n"))
	patchTimelineRecords(t, s.Server, planID, timelineID, true, []map[string]any{
		{"id": uuid.New().String(), "type": "Task", "name": "attempt step", "order": 1,
			"state": "completed", "result": "succeeded", "log": map[string]any{"id": logID}},
	})

	resp := s.get(t, fmt.Sprintf("/api/v3/repos/logs-org/logs-repo/actions/runs/%d/attempts/1/logs", wf.RunID), defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("attempt logs = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content type = %q, want application/zip", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	var contents []string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		contents = append(contents, string(b))
	}
	joined := strings.Join(contents, "")
	if len(contents) == 0 || !strings.Contains(joined, "attempt uploaded log") {
		t.Fatalf("attempt log zip missing uploaded log bytes: %v", contents)
	}
	if strings.Contains(joined, "line one") {
		t.Fatalf("attempt log zip leaked console capture: %v", contents)
	}

	// Unknown attempt 404s.
	resp2 := s.get(t, fmt.Sprintf("/api/v3/repos/logs-org/logs-repo/actions/runs/%d/attempts/9/logs", wf.RunID), defaultToken)
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("unknown attempt logs = %d, want 404", resp2.StatusCode)
	}
}

func TestWorkflowFileTiming_ComputedFromRunHistory(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := "timing-org/timing-repo"
	yaml := "name: ci\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	s.store.RegisterWorkflowFile(repo, ".github/workflows/ci.yml", "ci", yaml, "submitted")

	wf, job := seedRun(t, s.Server, repo, "completed", "success")
	s.store.Mu.Lock()
	job.StartedAt = fixedTestTime.Add(-3 * time.Second)
	job.CompletedAt = fixedTestTime
	s.store.Mu.Unlock()
	_ = wf

	data := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/"+repo+"/actions/workflows/ci.yml/timing", defaultToken), 200)
	billable, _ := data["billable"].(map[string]interface{})
	ubuntu, _ := billable["UBUNTU"].(map[string]interface{})
	if ubuntu == nil {
		t.Fatalf("timing = %v, want billable.UBUNTU", data)
	}
	if ms := ubuntu["total_ms"].(float64); ms < 2500 {
		t.Fatalf("total_ms = %v, want the seeded ~3s job duration", ms)
	}

	// Unknown workflow file 404s.
	resp := s.get(t, "/api/v3/repos/"+repo+"/actions/workflows/nope.yml/timing", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown workflow timing = %d, want 404", resp.StatusCode)
	}
}
