package bleephub

import (
	"strconv"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/server/testutil"
)

func TestResolveDispatchInputs(t *testing.T) {
	on, err := actions.ParseWorkflowOn([]byte(`
on:
  workflow_dispatch:
    inputs:
      env:
        type: choice
        required: true
        options: [staging, prod]
      dry-run:
        type: boolean
        default: true
      count:
        type: number
        default: 3
      note:
        type: string
jobs: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	td := on["workflow_dispatch"]

	// Happy path with defaults applied
	inputs, typed, errMsg := resolveDispatchInputs(td, map[string]string{"env": "staging"})
	if errMsg != "" {
		t.Fatalf("resolveDispatchInputs: %v", errMsg)
	}
	if inputs["dry-run"] != "true" || inputs["count"] != "3" {
		t.Errorf("defaults not applied: %v", inputs)
	}
	if typed["dry-run"] != true {
		t.Errorf("boolean input not typed: %T %v", typed["dry-run"], typed["dry-run"])
	}
	if typed["count"] != float64(3) {
		t.Errorf("number input not typed: %T %v", typed["count"], typed["count"])
	}
	if typed["env"] != "staging" {
		t.Errorf("choice input = %v", typed["env"])
	}

	// Required missing
	if _, _, msg := resolveDispatchInputs(td, nil); msg == "" {
		t.Error("missing required input should error")
	}
	// Unknown input
	if _, _, msg := resolveDispatchInputs(td, map[string]string{"env": "staging", "bogus": "x"}); msg == "" {
		t.Error("unknown input should error")
	}
	// Bad choice
	if _, _, msg := resolveDispatchInputs(td, map[string]string{"env": "qa"}); msg == "" {
		t.Error("out-of-options choice should error")
	}
	// Bad boolean
	if _, _, msg := resolveDispatchInputs(td, map[string]string{"env": "prod", "dry-run": "yes"}); msg == "" {
		t.Error("non-true/false boolean should error")
	}
}

func TestTriggerFiltersEndToEnd(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "trigowner/trig-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/main-only.yml", `name: main-only
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)

	countRuns := func(name string) int {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		n := 0
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey && w.Name == name {
				n++
			}
		}
		return n
	}

	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/dev", nil)
	if got := countRuns("main-only"); got != 0 {
		t.Fatalf("push to dev created %d runs, want 0", got)
	}
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)
	if got := countRuns("main-only"); got != 1 {
		t.Fatalf("push to main created %d runs, want 1", got)
	}

	// The triggering payload becomes github.event on the run.
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main",
		map[string]interface{}{"head_commit": map[string]interface{}{"message": "x"}})
	s.store.Mu.RLock()
	var withPayload *Workflow
	for _, w := range s.store.Workflows {
		if w.RepoFullName == repoKey && w.EventPayload != nil {
			withPayload = w
		}
	}
	s.store.Mu.RUnlock()
	if withPayload == nil {
		t.Fatal("no run carried the event payload")
	}
	if hc, _ := withPayload.EventPayload["head_commit"].(map[string]interface{}); hc == nil || hc["message"] != "x" {
		t.Fatalf("EventPayload = %v", withPayload.EventPayload)
	}
}

func TestWebhookEventProductionAlwaysFeedsActions(t *testing.T) {
	s := newTestServer()
	repoKey := "eventbridge/event-repo"
	commitWorkflowYAMLToStorage(t, s, repoKey, ".github/workflows/issues.yml", `name: issue-events
on:
  issues:
    types: [opened]
jobs:
  observe:
    runs-on: ubuntu-latest
    steps:
      - run: echo issue
`)
	repo := s.store.GetRepoByFullName(repoKey)
	if repo == nil {
		t.Fatal("workflow fixture did not create repository")
	}
	payload := map[string]interface{}{
		"action":     "opened",
		"repository": repoPayload(repo),
		"issue":      map[string]interface{}{"number": 1},
	}

	s.emitWebhookEvent(repoKey, "issues", "opened", payload)

	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	var matched *Workflow
	for _, workflow := range s.store.Workflows {
		if workflow.RepoFullName == repoKey && workflow.Name == "issue-events" {
			matched = workflow
			break
		}
	}
	if matched == nil {
		t.Fatal("issues webhook event did not produce an Actions run")
	}
	if matched.EventName != "issues" || matched.Ref != "refs/heads/main" {
		t.Fatalf("run event/ref = %q/%q, want issues/refs/heads/main", matched.EventName, matched.Ref)
	}
}

func TestPushCommitDirectiveSkipsWorkflow(t *testing.T) {
	s := newTestServer()
	repoKey := "skip-owner/skip-repo"
	commitWorkflowYAMLToStorage(t, s, repoKey, ".github/workflows/ci.yml", `name: ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo build
`)
	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", map[string]interface{}{
		"head_commit": map[string]interface{}{"message": "docs: update [skip ci]"},
	})
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, workflow := range s.store.Workflows {
		if workflow.RepoFullName == repoKey {
			t.Fatalf("skip directive created workflow %#v", workflow)
		}
	}
}

func TestIssueCommentMutationProducesWebhookAndActionsEvent(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	repoKey := "eventbridge/comment-repo"
	commitWorkflowYAMLToStorage(t, s, repoKey, ".github/workflows/comments.yml", `name: comments
on:
  issue_comment:
    types: [created]
jobs:
  observe:
    runs-on: ubuntu-latest
    steps:
      - run: echo comment
`)

	created := doMiscReq(s, "POST", "/api/v3/repos/"+repoKey+"/issues", `{"title":"event source"}`)
	if created.Code != 201 {
		t.Fatalf("create issue status = %d body=%s", created.Code, created.Body.String())
	}
	commented := doMiscReq(s, "POST", "/api/v3/repos/"+repoKey+"/issues/1/comments", `{"body":"hello"}`)
	if commented.Code != 201 {
		t.Fatalf("create comment status = %d body=%s", commented.Code, commented.Body.String())
	}

	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, workflow := range s.store.Workflows {
		if workflow.RepoFullName == repoKey && workflow.Name == "comments" {
			if workflow.EventName != "issue_comment" ||
				workflow.EventPayload["action"] != "created" {
				t.Fatalf("comment run event/payload = %q/%v", workflow.EventName, workflow.EventPayload)
			}
			return
		}
	}
	t.Fatal("issue comment mutation did not produce its Actions run")
}

func TestPullRequestReviewMutationProducesWebhookAndActionsEvent(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	repoKey := "eventbridge/review-repo"
	commitWorkflowYAMLToStorage(t, s, repoKey, ".github/workflows/reviews.yml", `name: reviews
on:
  pull_request_review:
    types: [submitted]
jobs:
  observe:
    runs-on: ubuntu-latest
    steps:
      - run: echo review
`)
	repo := s.store.GetRepoByFullName(repoKey)
	seedPullRequestBranches(t, s, repo, "feature")
	admin := s.store.UsersByLogin["admin"]
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "Review source", "", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("create pull request failed")
	}

	reviewed := doMiscReq(s, "POST", "/api/v3/repos/"+repoKey+"/pulls/1/reviews",
		`{"body":"ship it","event":"APPROVE"}`)
	if reviewed.Code != 200 {
		t.Fatalf("create review status = %d body=%s", reviewed.Code, reviewed.Body.String())
	}
	review := assertJSON(t, reviewed)
	if review["state"] != "APPROVED" {
		t.Fatalf("created review state = %v, want APPROVED", review["state"])
	}

	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, workflow := range s.store.Workflows {
		if workflow.RepoFullName == repoKey && workflow.Name == "reviews" {
			if workflow.EventName != "pull_request_review" ||
				workflow.EventPayload["action"] != "submitted" {
				t.Fatalf("review run event/payload = %q/%v", workflow.EventName, workflow.EventPayload)
			}
			return
		}
	}
	t.Fatalf("pull request review mutation did not produce its Actions run: %#v", s.store.Workflows)
}

func TestWorkflowTriggerRejectsUnresolvedRef(t *testing.T) {
	s := newTestServer()
	repoKey := "missingref/repo"
	commitWorkflowYAMLToStorage(t, s, repoKey, ".github/workflows/ci.yml", `name: ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)

	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/missing", nil)

	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, wf := range s.store.Workflows {
		if wf.RepoFullName == repoKey {
			t.Fatalf("unresolved ref created workflow run with sha %q", wf.Sha)
		}
	}
}

func TestPullRequestSynchronizeOnPush(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := "syncowner"
	repoName := "sync-repo"
	repoKey := owner + "/" + repoName
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/pr-ci.yml", `name: pr-ci
on: [pull_request]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		t.Fatal("repo missing")
	}
	seedStorePullRequestBranches(t, s.store, repo, "feature-x")
	user := s.store.UsersByLogin[owner]
	pr := s.store.CreatePullRequest(repo.ID, user.ID, "t", "b", "feature-x", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("PR not created")
	}

	s.firePullRequestSynchronize(repo, repoKey, "feature-x")

	var found *Workflow
	ok := testutil.TestEventually(2*time.Second, 20*time.Millisecond, func() bool {
		s.store.Mu.RLock()
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey && w.EventName == "pull_request" {
				found = w
			}
		}
		s.store.Mu.RUnlock()
		return found != nil
	})
	if !ok {
		t.Fatal("no pull_request run created by synchronize")
	}
	if found.Ref != "refs/pull/"+strconv.Itoa(pr.Number)+"/merge" {
		t.Fatalf("synchronize run ref = %q", found.Ref)
	}
}
