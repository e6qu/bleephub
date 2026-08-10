package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// eventRecorder captures webhook deliveries (event + action) for
// assertion; bleephub delivers asynchronously.
type eventRecorder struct {
	mu     sync.Mutex
	events []string // "event/action"
}

func (er *eventRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		er.mu.Lock()
		er.events = append(er.events, r.Header.Get("X-GitHub-Event")+"/"+body.Action)
		er.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (er *eventRecorder) has(want string) bool {
	er.mu.Lock()
	defer er.mu.Unlock()
	for _, e := range er.events {
		if e == want {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !testEventually(5*time.Second, 20*time.Millisecond, cond) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestActionsEventSnapshotsAreImmutable(t *testing.T) {
	t.Parallel()
	wf := &Workflow{
		ID:           "snapshot-run",
		Name:         "snapshot",
		DisplayTitle: "before",
		Status:       WorkflowStatusRunning,
		EventPayload: map[string]interface{}{
			"action": "queued",
			"issue":  map[string]interface{}{"number": float64(1)},
		},
		TypedInputs: map[string]interface{}{"deploy": true},
		Inputs:      map[string]string{"target": "staging"},
	}
	job := &WorkflowJob{
		JobID:        "snapshot-job",
		Status:       JobStatusQueued,
		Outputs:      map[string]string{"url": "before"},
		MatrixValues: map[string]interface{}{"os": "linux"},
	}

	wfSnapshot := cloneWorkflowEventSnapshot(wf)
	jobSnapshot := cloneWorkflowJobEventSnapshot(job)

	wf.DisplayTitle = "after"
	wf.Status = WorkflowStatusCompleted
	wf.EventPayload["action"] = "completed"
	wf.EventPayload["issue"].(map[string]interface{})["number"] = float64(2)
	wf.TypedInputs["deploy"] = false
	wf.Inputs["target"] = "production"
	job.Status = JobStatusCompleted
	job.Outputs["url"] = "after"
	job.MatrixValues["os"] = "windows"

	if wfSnapshot.DisplayTitle != "before" || wfSnapshot.Status != WorkflowStatusRunning {
		t.Fatalf("workflow scalar snapshot mutated: %+v", wfSnapshot)
	}
	if got := wfSnapshot.EventPayload["action"]; got != "queued" {
		t.Fatalf("event action snapshot = %v, want queued", got)
	}
	if got := wfSnapshot.EventPayload["issue"].(map[string]interface{})["number"]; got != float64(1) {
		t.Fatalf("nested event snapshot = %v, want 1", got)
	}
	if wfSnapshot.TypedInputs["deploy"] != true || wfSnapshot.Inputs["target"] != "staging" {
		t.Fatalf("input snapshots mutated: typed=%v string=%v", wfSnapshot.TypedInputs, wfSnapshot.Inputs)
	}
	if jobSnapshot.Status != JobStatusQueued ||
		jobSnapshot.Outputs["url"] != "before" ||
		jobSnapshot.MatrixValues["os"] != "linux" {
		t.Fatalf("job snapshot mutated: %+v", jobSnapshot)
	}
}

func TestActionsChecksLifecycle(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "checksowner/checks-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", `name: ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)

	rec := &eventRecorder{}
	receiver := httptest.NewTLSServer(rec.handler())
	defer receiver.Close()
	s.store.CreateHook(repoKey, receiver.URL, "", "json", "0", []string{"*"}, true)

	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	var wf *Workflow
	waitUntil(t, "workflow", func() bool {
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey {
				wf = w
				return true
			}
		}
		return false
	})

	// The drain creates one check run per job, queued, under a suite.
	var checkRun *CheckRun
	waitUntil(t, "check run", func() bool {
		runs := s.store.ListCheckRunsForCommit(repoKey, wf.Sha, "", "", 0)
		if len(runs) != 1 {
			return false
		}
		checkRun = runs[0]
		return true
	})
	if checkRun.Name != "build" {
		t.Errorf("check run name = %q, want build", checkRun.Name)
	}
	if checkRun.AppID != githubActionsAppID {
		t.Errorf("check run app = %d, want %d", checkRun.AppID, githubActionsAppID)
	}
	suites := s.store.ListCheckSuitesForCommit(repoKey, wf.Sha, githubActionsAppID)
	if len(suites) != 1 {
		t.Fatalf("suites = %d, want 1", len(suites))
	}

	waitUntil(t, "workflow_run requested", func() bool { return rec.has("workflow_run/requested") })
	waitUntil(t, "workflow_job queued", func() bool { return rec.has("workflow_job/queued") })
	waitUntil(t, "check_run created", func() bool { return rec.has("check_run/created") })
	waitUntil(t, "check_suite requested", func() bool { return rec.has("check_suite/requested") })

	// Runner pickup: renew the request → in_progress. The renew route belongs
	// to the runner the broker dispatched to, so stand one up and assign it.
	runnerToken, runnerAgent := testAgentSession(t, s.Server, runnerScope{Repo: repoKey})
	s.store.mu.Lock()
	job := s.store.Jobs[wf.Jobs["build"].JobID]
	if job != nil {
		job.AgentID = runnerAgent.ID
	}
	s.store.mu.Unlock()
	if job == nil {
		t.Fatal("engine job missing")
	}
	req, _ := http.NewRequest("PATCH",
		fmt.Sprintf("%s/_apis/v1/AgentRequest/1/%d", s.baseURL, job.RequestID), nil)
	req.Header.Set("Authorization", "Bearer "+runnerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("runner renew status = %d, want 200", resp.StatusCode)
	}
	waitUntil(t, "check run in_progress", func() bool {
		return s.store.GetCheckRun(checkRun.ID).Status == "in_progress"
	})
	waitUntil(t, "workflow_run in_progress", func() bool { return rec.has("workflow_run/in_progress") })
	waitUntil(t, "workflow_job in_progress", func() bool { return rec.has("workflow_job/in_progress") })

	// Completion: check run success, suite completed, completed events.
	s.onJobCompleted(context.Background(), wf.Jobs["build"].JobID, "Succeeded")
	waitUntil(t, "check run success", func() bool {
		cr := s.store.GetCheckRun(checkRun.ID)
		return cr.Status == "completed" && cr.Conclusion == "success"
	})
	waitUntil(t, "suite completed", func() bool {
		s := s.store.GetCheckSuite(suites[0].ID)
		return s.Status == "completed" && s.Conclusion == "success"
	})
	waitUntil(t, "workflow_run completed", func() bool { return rec.has("workflow_run/completed") })
	waitUntil(t, "workflow_job completed", func() bool { return rec.has("workflow_job/completed") })
	waitUntil(t, "check_run completed", func() bool { return rec.has("check_run/completed") })
	waitUntil(t, "check_suite completed", func() bool { return rec.has("check_suite/completed") })
}

func TestActionsSkippedJobCheckRun(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "checkskip/skip-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/ci.yml", `name: skip-ci
on: [push]
jobs:
  build:
    if: github.ref == 'refs/heads/never'
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	var wf *Workflow
	waitUntil(t, "workflow", func() bool {
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey {
				wf = w
				return true
			}
		}
		return false
	})
	waitUntil(t, "skipped check run", func() bool {
		runs := s.store.ListCheckRunsForCommit(repoKey, wf.Sha, "", "", 0)
		return len(runs) == 1 && runs[0].Status == "completed" && runs[0].Conclusion == "skipped"
	})
}

func TestMergeGatingByRequiredChecks(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := "gateowner"
	repoName := "gate-repo"
	repoKey := owner + "/" + repoName
	// Head branch content so the PR head sha resolves.
	commitFilesToStorage(t, s.Server, repoKey, map[string]string{"README.md": "hi"})
	repo := s.store.GetRepo(owner, repoName)
	user := s.store.UsersByLogin[owner]

	// The default branch the commit landed on serves as the PR head.
	stor := s.store.GetGitStorage(owner, repoName)
	headBranch := "main"
	if resolveBranchSha(stor, "main") == "" {
		headBranch = "master"
	}
	seedStorePullRequestBranches(t, s.store, repo, headBranch, "base")
	headSha := resolveBranchSha(stor, headBranch)
	if headSha == "" {
		t.Fatal("head branch sha did not resolve")
	}

	pr := s.store.CreatePullRequest(repo.ID, user.ID, "gate", "", headBranch, "base", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("PR not created")
	}
	s.store.UpdatePullRequest(pr.ID, func(p *PullRequest) { p.Mergeable = "MERGEABLE" })

	// Protect the base branch with a required status check.
	s.store.mu.Lock()
	s.store.Misc.branchProtection[bpKey(repo.ID, "base")] = &BranchProtection{
		RequiredStatusChecks: &BPStatusChecks{
			Strict:   false,
			Contexts: []string{"ci-job"},
		},
	}
	s.store.mu.Unlock()

	// No check runs yet → blocked + merge rejected.
	out := pullRequestToJSON(s.store.GetPullRequest(pr.ID), s.store, "http://x", repoKey)
	s.applyChecksToMergeability(out, repo, s.store.GetPullRequest(pr.ID))
	if out["mergeable_state"] != "blocked" {
		t.Errorf("mergeable_state = %v, want blocked", out["mergeable_state"])
	}

	resp := s.put(t, fmt.Sprintf("/api/v3/repos/%s/pulls/%d/merge", repoKey, pr.Number), defaultToken, map[string]interface{}{})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("merge with missing required check: status %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()

	// A pending check run with the required name still blocks.
	cr := s.store.CreateCheckRun(repoKey, headSha, "ci-job", githubActionsAppID, 0)
	resp = s.put(t, fmt.Sprintf("/api/v3/repos/%s/pulls/%d/merge", repoKey, pr.Number), defaultToken, map[string]interface{}{})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("merge with pending required check: status %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()

	// Green required check → merge allowed.
	now := fixedTestTime
	s.store.UpdateCheckRun(cr.ID, func(c *CheckRun) {
		c.Status = "completed"
		c.Conclusion = "success"
		c.CompletedAt = &now
	})
	out = pullRequestToJSON(s.store.GetPullRequest(pr.ID), s.store, "http://x", repoKey)
	s.applyChecksToMergeability(out, repo, s.store.GetPullRequest(pr.ID))
	if out["mergeable_state"] != "clean" {
		t.Errorf("mergeable_state after green check = %v, want clean", out["mergeable_state"])
	}
	resp = s.put(t, fmt.Sprintf("/api/v3/repos/%s/pulls/%d/merge", repoKey, pr.Number), defaultToken, map[string]interface{}{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("merge with green required check: status %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUnstableMergeableStateOnFailingNonRequired(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := "unstableowner"
	repoName := "unstable-repo"
	repoKey := owner + "/" + repoName
	commitFilesToStorage(t, s.Server, repoKey, map[string]string{"README.md": "hi"})
	repo := s.store.GetRepo(owner, repoName)
	user := s.store.UsersByLogin[owner]
	stor := s.store.GetGitStorage(owner, repoName)
	headBranch := "main"
	if resolveBranchSha(stor, "main") == "" {
		headBranch = "master"
	}
	seedStorePullRequestBranches(t, s.store, repo, headBranch, "base")
	headSha := resolveBranchSha(stor, headBranch)
	if headSha == "" {
		t.Fatal("head branch sha did not resolve")
	}
	pr := s.store.CreatePullRequest(repo.ID, user.ID, "u", "", headBranch, "base", false, nil, nil, 0)
	s.store.UpdatePullRequest(pr.ID, func(p *PullRequest) { p.Mergeable = "MERGEABLE" })

	cr := s.store.CreateCheckRun(repoKey, headSha, "lint", githubActionsAppID, 0)
	now := fixedTestTime
	s.store.UpdateCheckRun(cr.ID, func(c *CheckRun) {
		c.Status = "completed"
		c.Conclusion = "failure"
		c.CompletedAt = &now
	})

	out := pullRequestToJSON(s.store.GetPullRequest(pr.ID), s.store, "http://x", repoKey)
	s.applyChecksToMergeability(out, repo, s.store.GetPullRequest(pr.ID))
	if out["mergeable_state"] != "unstable" {
		t.Errorf("mergeable_state = %v, want unstable (failing non-required check)", out["mergeable_state"])
	}
}
