package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
)

func TestAgentSatisfiesLabels(t *testing.T) {
	t.Parallel()
	agent := &store.Agent{Labels: []store.Label{{Name: "self-hosted"}, {Name: "Linux"}, {Name: "gpu"}}}
	cases := []struct {
		required []string
		want     bool
	}{
		{nil, true},
		{[]string{"self-hosted"}, true},
		{[]string{"self-hosted", "gpu"}, true},
		{[]string{"SELF-HOSTED", "linux"}, true}, // case-insensitive
		{[]string{"self-hosted", "windows"}, false},
		{[]string{"ubuntu-latest"}, true}, // hosted alias: any agent
		{[]string{"ubuntu-22.04"}, true},  // hosted alias family
		{[]string{"macos-14"}, true},      // hosted alias family
		{[]string{"ubuntu-latest", "gpu"}, true},
		{[]string{"ubuntu-latest", "tpu"}, false}, // custom label still strict
	}
	for _, tc := range cases {
		if got := actions.AgentSatisfiesLabels(agent, tc.required); got != tc.want {
			t.Errorf("actions.AgentSatisfiesLabels(%v) = %v, want %v", tc.required, got, tc.want)
		}
	}
	if actions.AgentSatisfiesLabels(nil, []string{"self-hosted"}) {
		t.Error("nil agent must not satisfy strict labels")
	}
	if !actions.AgentSatisfiesLabels(nil, []string{"ubuntu-latest"}) {
		t.Error("nil agent satisfies hosted aliases")
	}
}

// ACT-051: runner.os/arch/name are placeholders at queue time and must be
// late-bound to the agent that actually leases the message.
func TestRunnerContextDerivedFromAgent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		agent            *store.Agent
		wantOS, wantArch string
		wantName         string
	}{
		{"nil is generic default", nil, "Linux", "X64", "test-runner"},
		{"os+arch from labels", &store.Agent{Name: "win-box", Labels: []store.Label{{Name: "self-hosted"}, {Name: "Windows"}, {Name: "ARM64"}}}, "Windows", "ARM64", "win-box"},
		{"macos label", &store.Agent{Name: "mac", Labels: []store.Label{{Name: "macOS"}, {Name: "X64"}}}, "macOS", "X64", "mac"},
		{"fallback to os description", &store.Agent{Name: "d", OSDescription: "Linux 6.1 aarch64"}, "Linux", "ARM64", "d"},
	} {
		got := actions.RunnerContextData(tc.agent)
		entries, _ := got["d"].([]map[string]interface{})
		vals := map[string]string{}
		for _, e := range entries {
			vals[e["k"].(string)] = e["v"].(string)
		}
		if vals["os"] != tc.wantOS || vals["arch"] != tc.wantArch || vals["name"] != tc.wantName {
			t.Errorf("%s: os=%q arch=%q name=%q, want os=%q arch=%q name=%q", tc.name, vals["os"], vals["arch"], vals["name"], tc.wantOS, tc.wantArch, tc.wantName)
		}
	}
}

func TestLeasedJobRebindsRunnerContext(t *testing.T) {
	s := newTestServer()
	sess := &store.Session{
		SessionID: "lease-sess",
		Agent:     &store.Agent{ID: 42, Name: "prod-runner-7", Labels: []store.Label{{Name: "self-hosted"}, {Name: "Windows"}, {Name: "ARM64"}}},
		MsgCh:     make(chan *store.TaskAgentMessage, 1),
	}
	s.store.Mu.Lock()
	s.store.Sessions[sess.SessionID] = sess
	s.store.Mu.Unlock()

	// A queued job message carries the runner-agnostic placeholder.
	body, _ := json.Marshal(map[string]interface{}{
		"contextData": map[string]interface{}{
			"runner": actions.RunnerContextData(nil),
		},
	})
	s.actions.QueueJobMessage(&store.TaskAgentMessage{MessageID: 5, JobID: "j5", MessageType: "PipelineAgentJobRequest", Body: string(body)})

	msg := s.actions.PullPendingMessage(sess, store.RunnerScope{Org: "octo"})
	if msg == nil {
		t.Fatal("free runner did not pull the queued job")
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(msg.Body), &decoded); err != nil {
		t.Fatalf("delivered body not JSON: %v", err)
	}
	runner := decoded["contextData"].(map[string]interface{})["runner"].(map[string]interface{})
	vals := map[string]string{}
	for _, e := range runner["d"].([]interface{}) {
		m := e.(map[string]interface{})
		vals[m["k"].(string)] = m["v"].(string)
	}
	if vals["os"] != "Windows" || vals["arch"] != "ARM64" || vals["name"] != "prod-runner-7" {
		t.Fatalf("delivered runner context not rebound to leasing agent: os=%q arch=%q name=%q", vals["os"], vals["arch"], vals["name"])
	}
}

func TestBusyRunnerNeverReceivesJobs(t *testing.T) {
	s := newTestServer()
	sess := &store.Session{
		SessionID: "busy-sess",
		Agent:     &store.Agent{ID: 901, Labels: []store.Label{{Name: "self-hosted"}}},
		MsgCh:     make(chan *store.TaskAgentMessage, 10),
	}
	s.store.Mu.Lock()
	s.store.Sessions["busy-sess"] = sess
	// An assigned, unfinished job marks the agent busy, mirroring
	// recordJobAgentLocked: the job's AgentID and the agent's assignment bookkeeping.
	s.store.Jobs["job-1"] = &store.Job{ID: "job-1", AgentID: 901, Status: "running"}
	s.store.Jobs["job-2"] = &store.Job{ID: "job-2", Status: "queued"}
	sess.Agent.AssignedJobID = "job-1"
	sess.Agent.EverAssigned = true
	s.store.Mu.Unlock()

	s.actions.QueueJobMessage(&store.TaskAgentMessage{MessageID: 7, JobID: "job-2", Labels: []string{"self-hosted"}})

	// While busy, polls must not pull the queued job.
	if got := s.actions.PullPendingMessage(sess, store.RunnerScope{Org: "octo"}); got != nil {
		t.Fatal("busy runner's poll pulled a job message")
	}

	s.store.Mu.Lock()
	s.store.Jobs["job-1"].Status = "completed"
	s.store.Mu.Unlock()
	got := s.actions.PullPendingMessage(sess, store.RunnerScope{Org: "octo"})
	if got == nil || got.MessageID != 7 {
		t.Fatalf("free runner's poll did not pull the pending job: %v", got)
	}
	s.store.Mu.RLock()
	agentID := s.store.Jobs["job-2"].AgentID
	pending := len(s.store.PendingMessages)
	s.store.Mu.RUnlock()
	if agentID != 901 {
		t.Errorf("pulled job not associated with the agent: AgentID=%d", agentID)
	}
	if pending != 0 {
		t.Errorf("pending queue not drained: %d left", pending)
	}
}

func TestLabelRoutingQueuesUntilMatch(t *testing.T) {
	s := newTestServer()

	mkSession := func(id string, labels ...string) *store.Session {
		ls := make([]store.Label, 0, len(labels))
		for _, l := range labels {
			ls = append(ls, store.Label{Name: l})
		}
		sess := &store.Session{
			SessionID: id,
			Agent:     &store.Agent{ID: len(id), Labels: ls},
			MsgCh:     make(chan *store.TaskAgentMessage, 10),
		}
		s.store.Mu.Lock()
		s.store.Sessions[id] = sess
		s.store.Mu.Unlock()
		return sess
	}
	plain := mkSession("a-plain", "self-hosted", "linux")

	s.actions.QueueJobMessage(&store.TaskAgentMessage{MessageID: 1, Labels: []string{"self-hosted", "gpu"}})

	// A poll from a non-matching runner must not pull the job.
	if got := s.actions.PullPendingMessage(plain, store.RunnerScope{Org: "octo"}); got != nil {
		t.Fatal("job pulled by a runner without the required labels")
	}

	gpu := mkSession("b-gpu", "self-hosted", "linux", "gpu")
	got := s.actions.PullPendingMessage(gpu, store.RunnerScope{Org: "octo"})
	if got == nil || got.MessageID != 1 {
		t.Fatalf("matching runner's poll did not pull the job: %v", got)
	}
	if again := s.actions.PullPendingMessage(gpu, store.RunnerScope{Org: "octo"}); again != nil {
		t.Fatal("message pulled twice")
	}
}

func (s *isolatedServer) seedRerunRepo(t *testing.T, repoKey, yaml string) *store.Workflow {
	t.Helper()
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", yaml)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)
	var wf *store.Workflow
	waitUntil(t, "triggered run", func() bool {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey {
				wf = w
				return true
			}
		}
		return false
	})
	return wf
}

const twoJobYAML = `name: ci
on: [push]
jobs:
  good:
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
  bad:
    runs-on: ubuntu-latest
    steps:
      - run: exit 1
`

func TestRerunKeepsRunIDAndBumpsAttempt(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "rerunowner/rerun-repo"
	wf := s.seedRerunRepo(t, repoKey, twoJobYAML)
	origRunID := wf.RunID
	s.assertWorkflowJobsUseHostMode(t, wf)

	// Finish both jobs (one failure) so the run completes.
	s.actions.OnJobCompleted(context.Background(), wf.Jobs["good"].JobID, "Succeeded")
	s.actions.OnJobCompleted(context.Background(), wf.Jobs["bad"].JobID, "Failed")

	resp := s.post(t, fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d/rerun", repoKey, origRunID), defaultToken, map[string]interface{}{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("rerun status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Same run id, attempt 2, both jobs fresh.
	resp2, err := http.Get(fmt.Sprintf("%s/api/v3/repos/%s/actions/runs/%d", s.baseURL, repoKey, origRunID))
	if err != nil {
		t.Fatal(err)
	}
	var run map[string]interface{}
	_ = json.NewDecoder(resp2.Body).Decode(&run)
	resp2.Body.Close()
	if int(run["run_attempt"].(float64)) != 2 {
		t.Errorf("run_attempt = %v, want 2", run["run_attempt"])
	}
	if int(run["id"].(float64)) != origRunID {
		t.Errorf("rerun id = %v, want %d (same run id)", run["id"], origRunID)
	}
	s.store.Mu.RLock()
	var attempt2 *store.Workflow
	for _, w := range s.store.Workflows {
		if w.RepoFullName == repoKey && w.RunID == origRunID {
			attempt2 = w
			break
		}
	}
	s.store.Mu.RUnlock()
	if attempt2 == nil {
		t.Fatal("rerun attempt 2 not found")
	}
	s.assertWorkflowJobsUseHostMode(t, attempt2)

	resp3, err := http.Get(fmt.Sprintf("%s/api/v3/repos/%s/actions/runs/%d/attempts/1", s.baseURL, repoKey, origRunID))
	if err != nil {
		t.Fatal(err)
	}
	var att map[string]interface{}
	_ = json.NewDecoder(resp3.Body).Decode(&att)
	resp3.Body.Close()
	if int(att["run_attempt"].(float64)) != 1 {
		t.Errorf("attempt 1 run_attempt = %v", att["run_attempt"])
	}
	if att["conclusion"] != "failure" {
		t.Errorf("attempt 1 conclusion = %v, want failure", att["conclusion"])
	}

	// Attempt 1 jobs endpoint serves the archived jobs.
	resp4, err := http.Get(fmt.Sprintf("%s/api/v3/repos/%s/actions/runs/%d/attempts/1/jobs", s.baseURL, repoKey, origRunID))
	if err != nil {
		t.Fatal(err)
	}
	var jobs struct {
		TotalCount int `json:"total_count"`
	}
	_ = json.NewDecoder(resp4.Body).Decode(&jobs)
	resp4.Body.Close()
	if jobs.TotalCount != 2 {
		t.Errorf("attempt 1 jobs = %d, want 2", jobs.TotalCount)
	}
}

func TestRerunFailedJobsCarriesSuccesses(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "rerunfail/rf-repo"
	wf := s.seedRerunRepo(t, repoKey, twoJobYAML)
	runID := wf.RunID
	s.assertWorkflowJobsUseHostMode(t, wf)

	s.store.Mu.Lock()
	wf.Jobs["good"].Outputs["artifact"] = "kept"
	s.store.Mu.Unlock()
	s.actions.OnJobCompleted(context.Background(), wf.Jobs["good"].JobID, "Succeeded")
	s.actions.OnJobCompleted(context.Background(), wf.Jobs["bad"].JobID, "Failed")

	resp := s.post(t, fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d/rerun-failed-jobs", repoKey, runID), defaultToken, map[string]interface{}{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("rerun-failed-jobs status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	var attempt2 *store.Workflow
	waitUntil(t, "attempt 2", func() bool {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey && w.RunID == runID {
				attempt2 = w
				return w.AttemptNumber() == 2
			}
		}
		return false
	})

	s.store.Mu.RLock()
	good := attempt2.Jobs["good"]
	bad := attempt2.Jobs["bad"]
	goodStatus, goodResult, goodOut := good.Status, good.Result, good.Outputs["artifact"]
	badStatus := bad.Status
	s.store.Mu.RUnlock()

	if goodStatus != store.JobStatusCompleted || goodResult != store.ResultSuccess {
		t.Errorf("good carried over: status=%q result=%q", goodStatus, goodResult)
	}
	if goodOut != "kept" {
		t.Errorf("good outputs not carried: %q", goodOut)
	}
	if badStatus != store.JobStatusQueued {
		t.Errorf("bad job should re-dispatch (queued), got %q", badStatus)
	}
	s.assertWorkflowJobsUseHostMode(t, attempt2, "bad")
}

func TestWorkflowEnableDisable(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "disowner/dis-repo"
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", `name: dis-ci
on: [push, workflow_dispatch]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)
	s.store.DiscoverWorkflowFilesFromGit(repoKey)

	disable := func(verb string) int {
		req, _ := http.NewRequest("PUT",
			fmt.Sprintf("%s/api/v3/repos/%s/actions/workflows/ci.yml/%s", s.baseURL, repoKey, verb), nil)
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := disable("disable"); code != http.StatusNoContent {
		t.Fatalf("disable status = %d", code)
	}
	wfFile := s.resolveWorkflowFile(repoKey, "ci.yml")
	if wfFile.State != "disabled_manually" {
		t.Errorf("state = %q, want disabled_manually", wfFile.State)
	}

	// Dispatch while disabled → 403.
	resp := s.post(t, fmt.Sprintf("/api/v3/repos/%s/actions/workflows/ci.yml/dispatches", repoKey), defaultToken,
		map[string]interface{}{"ref": "refs/heads/main"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("dispatch while disabled = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Push trigger while disabled → no run.
	before := s.countRepoRuns(repoKey)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)
	changed := testutil.TestEventually(200*time.Millisecond, 10*time.Millisecond, func() bool {
		return s.countRepoRuns(repoKey) != before
	})
	if got := s.countRepoRuns(repoKey); changed {
		t.Errorf("disabled workflow triggered: runs %d → %d", before, got)
	}

	if code := disable("enable"); code != http.StatusNoContent {
		t.Fatalf("enable status = %d", code)
	}
	if wfFile := s.resolveWorkflowFile(repoKey, "ci.yml"); wfFile.State != "active" {
		t.Errorf("state after enable = %q", wfFile.State)
	}
}

func (s *isolatedServer) countRepoRuns(repoKey string) int {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	n := 0
	for _, w := range s.store.Workflows {
		if w.RepoFullName == repoKey {
			n++
		}
	}
	return n
}

func TestOrgRunnerEndpoints(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/admin/organizations", defaultToken,
		map[string]interface{}{"login": "runner-org", "admin": "admin"})
	resp.Body.Close()

	s.store.Mu.Lock()
	agentID := s.store.NextAgent
	s.store.NextAgent++
	s.store.Agents[agentID] = &store.Agent{ID: agentID, Name: "org-agent", Status: "online",
		Labels: []store.Label{{Name: "self-hosted"}}, Scope: store.RunnerScope{Org: "runner-org"}}
	s.store.Mu.Unlock()

	get := func(path string) (int, map[string]interface{}) {
		req, _ := http.NewRequest("GET", s.baseURL+path, nil)
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		return r.StatusCode, body
	}

	code, body := get("/api/v3/orgs/runner-org/actions/runners")
	if code != http.StatusOK {
		t.Fatalf("org runners list = %d", code)
	}
	if int(body["total_count"].(float64)) < 1 {
		t.Errorf("org runners total_count = %v", body["total_count"])
	}

	code, runner := get(fmt.Sprintf("/api/v3/orgs/runner-org/actions/runners/%d", agentID))
	if code != http.StatusOK || runner["name"] != "org-agent" {
		t.Errorf("org runner get = %d, name %v", code, runner["name"])
	}
	if _, ok := runner["busy"].(bool); !ok {
		t.Errorf("runner busy missing/false-typed: %v", runner["busy"])
	}

	code, _ = get("/api/v3/orgs/no-such-org/actions/runners")
	if code != http.StatusNotFound {
		t.Errorf("unknown org runners list = %d, want 404", code)
	}

	respTok := s.post(t, "/api/v3/orgs/runner-org/actions/runners/registration-token", defaultToken, map[string]interface{}{})
	if respTok.StatusCode != http.StatusCreated {
		t.Errorf("org registration-token = %d, want 201", respTok.StatusCode)
	}
	respTok.Body.Close()
}
