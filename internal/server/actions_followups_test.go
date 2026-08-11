package bleephub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestCancellationSignalsRunningJob verifies cancel sends
// JobCancellation to the runner executing a job, leaves always()-gated
// jobs runnable, and the run concludes cancelled.
func TestCancellationSignalsRunningJob(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "cancelowner/cancel-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", `name: cancel-ci
on: [push]
jobs:
  slow:
    runs-on: self-hosted
    steps:
      - run: sleep 300
  cleanup:
    needs: [slow]
    if: always()
    runs-on: self-hosted
    steps:
      - run: echo cleanup
`)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	var wf *Workflow
	waitUntil(t, "run", func() bool {
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

	// A runner session pulls the slow job and starts running it.
	sess := &Session{
		SessionID: "cancel-sess",
		Agent:     &Agent{ID: 7001, Labels: []Label{{Name: "self-hosted"}}},
		MsgCh:     make(chan *TaskAgentMessage, 10),
	}
	s.store.Mu.Lock()
	s.store.Sessions["cancel-sess"] = sess
	s.store.Mu.Unlock()
	// The pending queue is shared across tests — keep only this run's
	// job so the pull below deterministically takes it.
	s.store.Mu.Lock()
	slowJobID := wf.Jobs["slow"].JobID
	kept := s.store.PendingMessages[:0]
	for _, m := range s.store.PendingMessages {
		if m.JobID == slowJobID {
			kept = append(kept, m)
		}
	}
	s.store.PendingMessages = kept
	s.store.Mu.Unlock()

	msg := s.pullPendingMessage(sess, runnerScope{Repo: repoKey})
	if msg == nil || msg.JobID != slowJobID {
		t.Fatalf("runner did not pull the slow job: %v", msg)
	}
	s.store.Mu.Lock()
	slowID := wf.Jobs["slow"].JobID
	s.store.Jobs[slowID].Status = "running"
	wf.Jobs["slow"].Status = JobStatusRunning
	s.store.Mu.Unlock()

	// Cancel via the REST API.
	resp := s.post(t, fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d/cancel", repoKey, wf.RunID), defaultToken, map[string]interface{}{})
	resp.Body.Close()

	// The runner receives a JobCancellation for the running job.
	var cancelMsg *TaskAgentMessage
	select {
	case cancelMsg = <-sess.MsgCh:
	default:
		t.Fatal("no JobCancellation pushed to the runner's open poll")
	}
	if cancelMsg.MessageType != "JobCancellation" {
		t.Fatalf("message type = %q, want JobCancellation", cancelMsg.MessageType)
	}
	var body struct {
		JobID   string `json:"jobId"`
		Timeout string `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(cancelMsg.Body), &body); err != nil || body.JobID != slowID {
		t.Fatalf("cancellation body = %s (err %v), want jobId %s", cancelMsg.Body, err, slowID)
	}

	s.store.Mu.RLock()
	slowStatus := wf.Jobs["slow"].Status
	cleanupStatus := wf.Jobs["cleanup"].Status
	s.store.Mu.RUnlock()
	if slowStatus != JobStatusRunning {
		t.Errorf("running job force-completed server-side: %q (the runner reports the cancel)", slowStatus)
	}
	if cleanupStatus == JobStatusCompleted && wf.Jobs["cleanup"].Result == ResultCancelled {
		t.Error("always() job was cancelled instead of left runnable")
	}

	// The runner aborts and reports; the always() job then dispatches.
	s.onJobCompleted(context.Background(), slowID, "Canceled")
	waitUntil(t, "cleanup dispatched", func() bool {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		return wf.Jobs["cleanup"].Status == JobStatusQueued
	})

	// Cleanup completes; run concludes cancelled (not failure).
	s.onJobCompleted(context.Background(), wf.Jobs["cleanup"].JobID, "Succeeded")
	waitUntil(t, "run cancelled", func() bool {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		return wf.Status == WorkflowStatusCompleted && wf.Result == ResultCancelled
	})
}

// TestCancelPurgesUndeliveredJobs: cancelling drops queued-but-undelivered
// job messages so a runner can't pull a cancelled job later.
func TestCancelPurgesUndeliveredJobs(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "cancelq/cq-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", `name: cq-ci
on: [push]
jobs:
  a:
    runs-on: self-hosted
    steps:
      - run: echo a
`)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)
	var wf *Workflow
	waitUntil(t, "run", func() bool {
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

	s.cancelWorkflow(wf)

	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, msg := range s.store.PendingMessages {
		if msg.JobID == wf.Jobs["a"].JobID {
			t.Fatal("cancelled job's message still pending")
		}
	}
	if wf.Jobs["a"].Result != ResultCancelled {
		t.Errorf("job result = %q, want cancelled", wf.Jobs["a"].Result)
	}
	if wf.Result != ResultCancelled {
		t.Errorf("run result = %q, want cancelled", wf.Result)
	}
}

// TestStartupFailureRunShell verifies a matched workflow that
// can't start yields a run with conclusion startup_failure, visible on
// the runs API, with no jobs.
func TestStartupFailureRunShell(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "startfail/sf-repo"
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/broken.yml", `name: broken-call
on: [push]
jobs:
  call:
    uses: ./.github/workflows/does-not-exist.yml
`)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	var wf *Workflow
	waitUntil(t, "startup_failure run", func() bool {
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
	if wf.Result != ResultStartupFailure || wf.Status != WorkflowStatusCompleted {
		t.Fatalf("run = %q/%q, want completed/startup_failure", wf.Status, wf.Result)
	}
	if len(wf.Jobs) != 0 {
		t.Errorf("startup_failure run has %d jobs, want 0", len(wf.Jobs))
	}

	// Visible through the runs API with the real conclusion.
	resp, err := http.Get(fmt.Sprintf("%s/api/v3/repos/%s/actions/runs/%d", s.baseURL, repoKey, wf.RunID))
	if err != nil {
		t.Fatal(err)
	}
	var run map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	if run["conclusion"] != "startup_failure" {
		t.Errorf("API conclusion = %v, want startup_failure", run["conclusion"])
	}
	if run["name"] != "broken-call" {
		t.Errorf("API name = %v, want broken-call", run["name"])
	}
	waitUntil(t, "startup failure check suite", func() bool {
		suites := s.store.ListCheckSuitesForCommit(repoKey, wf.Sha, githubActionsAppID)
		return len(suites) == 1 && suites[0].Status == "completed" &&
			suites[0].Conclusion == "startup_failure"
	})
}

// TestRunnerGroupsCRUD exercises runner-group create/read/update/delete.
func TestRunnerGroupsCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/admin/organizations", defaultToken,
		map[string]interface{}{"login": "rg-org", "admin": "admin"})
	resp.Body.Close()

	s.store.Mu.Lock()
	agentID := s.store.NextAgent
	s.store.NextAgent++
	s.store.Agents[agentID] = &Agent{
		ID: agentID, Name: "rg-agent", Status: "online", Scope: runnerScope{Org: "rg-org"},
	}
	s.store.Mu.Unlock()

	do := func(method, path string, body interface{}) (int, map[string]interface{}) {
		var payload []byte
		if body != nil {
			payload, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, s.baseURL+path, bytesReader(payload))
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	// Create
	code, created := do("POST", "/api/v3/orgs/rg-org/actions/runner-groups",
		map[string]interface{}{"name": "gpu-pool", "visibility": "selected"})
	if code != http.StatusCreated {
		t.Fatalf("create group = %d", code)
	}
	gid := int(created["id"].(float64))
	if created["default"] != false || created["visibility"] != "selected" {
		t.Errorf("created group shape: %v", created)
	}

	// List includes Default + new
	code, list := do("GET", "/api/v3/orgs/rg-org/actions/runner-groups", nil)
	if code != http.StatusOK || int(list["total_count"].(float64)) < 2 {
		t.Fatalf("list = %d, %v", code, list["total_count"])
	}

	// Membership
	code, _ = do("PUT", fmt.Sprintf("/api/v3/orgs/rg-org/actions/runner-groups/%d/runners/%d", gid, agentID), nil)
	if code != http.StatusNoContent {
		t.Fatalf("add runner = %d", code)
	}
	code, members := do("GET", fmt.Sprintf("/api/v3/orgs/rg-org/actions/runner-groups/%d/runners", gid), nil)
	if code != http.StatusOK || int(members["total_count"].(float64)) != 1 {
		t.Fatalf("group runners = %d, %v", code, members["total_count"])
	}

	// Runner JSON reflects the group
	code, runner := do("GET", fmt.Sprintf("/api/v3/orgs/rg-org/actions/runners/%d", agentID), nil)
	if code != http.StatusOK || int(runner["runner_group_id"].(float64)) != gid {
		t.Errorf("runner_group_id = %v, want %d", runner["runner_group_id"], gid)
	}

	// Rename
	code, patched := do("PATCH", fmt.Sprintf("/api/v3/orgs/rg-org/actions/runner-groups/%d", gid),
		map[string]interface{}{"name": "gpu-pool-2"})
	if code != http.StatusOK || patched["name"] != "gpu-pool-2" {
		t.Errorf("patch = %d, %v", code, patched["name"])
	}

	// Default group is undeletable
	code, _ = do("DELETE", "/api/v3/orgs/rg-org/actions/runner-groups/1", nil)
	if code != http.StatusBadRequest {
		t.Errorf("delete default = %d, want 400", code)
	}

	// Delete: members fall back to Default
	code, _ = do("DELETE", fmt.Sprintf("/api/v3/orgs/rg-org/actions/runner-groups/%d", gid), nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete group = %d", code)
	}
	code, runner = do("GET", fmt.Sprintf("/api/v3/orgs/rg-org/actions/runners/%d", agentID), nil)
	if code != http.StatusOK || int(runner["runner_group_id"].(float64)) != 1 {
		t.Errorf("post-delete runner_group_id = %v, want 1", runner["runner_group_id"])
	}

	// Unknown org 404s
	code, _ = do("GET", "/api/v3/orgs/no-such/actions/runner-groups", nil)
	if code != http.StatusNotFound {
		t.Errorf("unknown org = %d, want 404", code)
	}
}

// TestRunnerGroupReposPagination covers per_page/page slicing of a runner
// group's repository list with a stable total_count.
func TestRunnerGroupReposPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "rg-page-org", "rg-page-org", "")
	r1 := s.store.CreateOrgRepo(org, admin, "rg-page-repo-1", "", false)
	r2 := s.store.CreateOrgRepo(org, admin, "rg-page-repo-2", "", false)

	created := pagedJSONRequest(t, s, http.MethodPost, "/api/v3/orgs/"+org.Login+"/actions/runner-groups", defaultToken,
		map[string]interface{}{"name": "paged-pool", "visibility": "selected"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group = %d: %s", created.Code, created.Body.String())
	}
	var group map[string]interface{}
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	gid := int(group["id"].(float64))

	setResp := pagedJSONRequest(t, s, http.MethodPut, fmt.Sprintf("/api/v3/orgs/%s/actions/runner-groups/%d/repositories", org.Login, gid), defaultToken,
		map[string]interface{}{"selected_repository_ids": []int{r1.ID, r2.ID}})
	if setResp.Code != http.StatusNoContent {
		t.Fatalf("set group repos = %d: %s", setResp.Code, setResp.Body.String())
	}

	resp := tokenRequest(s, http.MethodGet, fmt.Sprintf("/api/v3/orgs/%s/actions/runner-groups/%d/repositories?per_page=1", org.Login, gid), defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1 = %d: %s", resp.Code, resp.Body.String())
	}
	link := resp.Header().Get("Link")
	var page1 map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &page1); err != nil {
		t.Fatal(err)
	}
	repos1, _ := page1["repositories"].([]interface{})
	if int(page1["total_count"].(float64)) != 2 {
		t.Fatalf("page 1 total_count = %v, want 2", page1["total_count"])
	}
	if len(repos1) != 1 {
		t.Fatalf("page 1 repos = %d, want 1", len(repos1))
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, fmt.Sprintf("/api/v3/orgs/%s/actions/runner-groups/%d/repositories?per_page=1&page=2", org.Login, gid), defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2 = %d: %s", resp.Code, resp.Body.String())
	}
	var page2 map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &page2); err != nil {
		t.Fatal(err)
	}
	repos2, _ := page2["repositories"].([]interface{})
	if int(page2["total_count"].(float64)) != 2 {
		t.Fatalf("page 2 total_count = %v, want 2", page2["total_count"])
	}
	if len(repos2) != 1 {
		t.Fatalf("page 2 repos = %d, want 1", len(repos2))
	}
	id1 := int(repos1[0].(map[string]interface{})["id"].(float64))
	id2 := int(repos2[0].(map[string]interface{})["id"].(float64))
	if id1 == id2 {
		t.Fatalf("page 1 and page 2 returned the same repository: %d", id1)
	}
}

// bytesReader tolerates nil bodies for request construction.
func bytesReader(b []byte) *bytes.Reader {
	if b == nil {
		return bytes.NewReader(nil)
	}
	return bytes.NewReader(b)
}

// TestLocalActionTarball verifies actions hosted on bleephub
// itself serve GitHub-layout tarballs from their own git storage.
func TestLocalActionTarball(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	commitFilesToStorage(t, s.Server, "actowner/hello-action", map[string]string{
		"action.yml": `name: hello
runs:
  using: composite
  steps:
    - run: echo "from composite"
      shell: bash
`,
		"README.md": "composite test action",
	})

	// The runner fetches an action tarball with the job's runtime token.
	jobToken, _ := testJobToken(t, s.Server, "actowner/hello-action")
	resp := runnerDo(t, "GET", s.baseURL+"/_apis/v1/actions/tarball/actowner/hello-action/main", jobToken, "")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadGateway {
		// Default branch may be master in the test storage.
		resp2 := runnerDo(t, "GET", s.baseURL+"/_apis/v1/actions/tarball/actowner/hello-action/master", jobToken, "")
		defer resp2.Body.Close()
		resp = resp2
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tarball status = %d", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	var topPrefix string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(hdr.Name, "/", 2)
		if topPrefix == "" {
			topPrefix = parts[0]
		} else if parts[0] != topPrefix {
			t.Errorf("multiple top-level dirs: %q vs %q", parts[0], topPrefix)
		}
		if len(parts) == 2 {
			found[parts[1]] = true
		}
		if parts[len(parts)-1] == "action.yml" {
			content, _ := io.ReadAll(tr)
			if !strings.Contains(string(content), "using: composite") {
				t.Error("action.yml content mangled")
			}
		}
	}
	if !found["action.yml"] || !found["README.md"] {
		t.Errorf("tarball entries = %v, want action.yml + README.md under one prefix", found)
	}
	if !strings.HasPrefix(topPrefix, "actowner-hello-action-") {
		t.Errorf("top-level dir = %q, want <owner>-<repo>-<sha> layout", topPrefix)
	}
}

func TestNormalizeResultRunnerSpellings(t *testing.T) {
	t.Parallel()
	// The official runner reports the US spelling "Canceled".
	for _, in := range []string{"Canceled", "canceled", "Cancelled", "cancelled"} {
		if got := normalizeResult(in); got != "cancelled" {
			t.Errorf("normalizeResult(%q) = %q, want cancelled", in, got)
		}
	}
}
