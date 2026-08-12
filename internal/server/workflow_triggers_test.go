package bleephub

import (
	"strconv"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
)

func TestParseWorkflowOnShapes(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		events []string
	}{
		{"string", "on: push\njobs: {}", []string{"push"}},
		{"list", "on: [push, pull_request]\njobs: {}", []string{"push", "pull_request"}},
		{"map-null-values", "on:\n  push:\n  workflow_dispatch:\njobs: {}", []string{"push", "workflow_dispatch"}},
		{"map-with-filters", "on:\n  push:\n    branches: [main]\njobs: {}", []string{"push"}},
	}
	for _, tc := range cases {
		on, err := ParseWorkflowOn([]byte(tc.yaml))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		for _, e := range tc.events {
			if _, ok := on[e]; !ok {
				t.Errorf("%s: missing event %q in %v", tc.name, e, on)
			}
		}
		if len(on) != len(tc.events) {
			t.Errorf("%s: got %d events, want %d", tc.name, len(on), len(tc.events))
		}
	}
}

func TestParseWorkflowOnFilters(t *testing.T) {
	yaml := `
on:
  push:
    branches: [main, 'releases/**']
    paths: ['src/**', '!src/docs/**']
  pull_request:
    types: [opened, labeled]
    branches: [main]
  schedule:
    - cron: '0 4 * * 1-5'
      timezone: Europe/Bucharest
    - cron: '30 0 * * 0'
  workflow_dispatch:
    inputs:
      env:
        type: choice
        required: true
        options: [staging, prod]
      dry-run:
        type: boolean
        default: true
jobs: {}
`
	on, err := ParseWorkflowOn([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	push := on["push"]
	if len(push.Branches) != 2 || push.Branches[1] != "releases/**" {
		t.Errorf("push branches = %v", push.Branches)
	}
	if len(push.Paths) != 2 {
		t.Errorf("push paths = %v", push.Paths)
	}
	pr := on["pull_request"]
	if len(pr.Types) != 2 || pr.Types[1] != "labeled" {
		t.Errorf("pr types = %v", pr.Types)
	}
	sched := on["schedule"]
	if len(sched.Schedules) != 2 || sched.Schedules[0].Cron != "0 4 * * 1-5" ||
		sched.Schedules[0].Timezone != "Europe/Bucharest" {
		t.Errorf("schedule entries = %+v", sched.Schedules)
	}
	wd := on["workflow_dispatch"]
	if wd.Inputs["env"] == nil || wd.Inputs["env"].Type != "choice" || !wd.Inputs["env"].Required {
		t.Errorf("dispatch input env = %+v", wd.Inputs["env"])
	}
	if wd.Inputs["dry-run"] == nil || wd.Inputs["dry-run"].Default != true {
		t.Errorf("dispatch input dry-run = %+v", wd.Inputs["dry-run"])
	}
}

func TestParseWorkflowOnInvalidCombos(t *testing.T) {
	for _, yaml := range []string{
		"on:\n  push:\n    branches: [main]\n    branches-ignore: [dev]\njobs: {}",
		"on:\n  push:\n    tags: [v1]\n    tags-ignore: [v2]\njobs: {}",
		"on:\n  push:\n    paths: [a]\n    paths-ignore: [b]\njobs: {}",
		"on:\n  schedule:\n    - cron: '0 0 * * *'\n      timezone: Not/AZone\njobs: {}",
	} {
		if _, err := ParseWorkflowOn([]byte(yaml)); err == nil {
			t.Errorf("expected error for combined include+ignore filters in %q", yaml)
		}
	}
}

func TestFilterPatternMatch(t *testing.T) {
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"main", "main", true},
		{"main", "maintenance", false},
		{"*", "main", true},
		{"*", "releases/v1", false}, // * never crosses /
		{"**", "releases/v1", true},
		{"releases/**", "releases/v1/hotfix", true},
		{"releases/*", "releases/v1", true},
		{"releases/*", "releases/v1/hotfix", false},
		{"v2*", "v2", true},
		{"v2*", "v2.0", true},
		{"v2*", "v1.9", false},
		{"v[12].[0-9]+", "v2.0", true},
		{"v[12].[0-9]+", "v2.10", true},
		{"v[12].[0-9]+", "v3.0", false},
		{"feature/?dev", "feature/dev", true}, // '?' = zero or one of preceding char
		{"feature/?dev", "featuredev", true},
		{"feature/?dev", "feature//dev", false},
		{"**.js", "src/app/index.js", true},
		{"**.js", "index.js", true},
		{"**.js", "index.ts", false},
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "src/docs.md", false},
	}
	for _, tc := range cases {
		if got := filterPatternMatch(tc.pattern, tc.value); got != tc.want {
			t.Errorf("filterPatternMatch(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestFilterPatternsNegation(t *testing.T) {
	patterns := []string{"releases/**", "!releases/**-alpha"}
	if !filterPatternsMatch(patterns, "releases/v1") {
		t.Error("releases/v1 should match")
	}
	if filterPatternsMatch(patterns, "releases/v1-alpha") {
		t.Error("releases/v1-alpha should be excluded by negation")
	}
	// Re-inclusion after negation
	patterns = []string{"releases/**", "!releases/**-alpha", "releases/v2-alpha"}
	if !filterPatternsMatch(patterns, "releases/v2-alpha") {
		t.Error("releases/v2-alpha should be re-included by the later positive pattern")
	}
}

func TestWorkflowTriggersOn(t *testing.T) {
	mustOn := func(y string) map[string]*TriggerDef {
		on, err := ParseWorkflowOn([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return on
	}

	branchFiltered := mustOn("on:\n  push:\n    branches: [main]\njobs: {}")
	if !workflowTriggersOn(branchFiltered, triggerEvent{Type: "push", Ref: "refs/heads/main"}) {
		t.Error("push to main should trigger")
	}
	if workflowTriggersOn(branchFiltered, triggerEvent{Type: "push", Ref: "refs/heads/dev"}) {
		t.Error("push to dev should not trigger")
	}
	if workflowTriggersOn(branchFiltered, triggerEvent{Type: "push", Ref: "refs/tags/v1"}) {
		t.Error("tag push should not trigger a branches-only filter")
	}

	tagFiltered := mustOn("on:\n  push:\n    tags: ['v*']\njobs: {}")
	if !workflowTriggersOn(tagFiltered, triggerEvent{Type: "push", Ref: "refs/tags/v1.0"}) {
		t.Error("tag push should trigger tag filter")
	}
	if workflowTriggersOn(tagFiltered, triggerEvent{Type: "push", Ref: "refs/heads/main"}) {
		t.Error("branch push should not trigger a tags-only filter")
	}

	prDefault := mustOn("on: [pull_request]\njobs: {}")
	if !workflowTriggersOn(prDefault, triggerEvent{Type: "pull_request", Action: "opened", Ref: "refs/heads/main"}) {
		t.Error("PR opened should trigger default types")
	}
	if !workflowTriggersOn(prDefault, triggerEvent{Type: "pull_request", Action: "synchronize", Ref: "refs/heads/main"}) {
		t.Error("PR synchronize should trigger default types")
	}
	if workflowTriggersOn(prDefault, triggerEvent{Type: "pull_request", Action: "closed", Ref: "refs/heads/main"}) {
		t.Error("PR closed must NOT trigger default types")
	}

	prTyped := mustOn("on:\n  pull_request:\n    types: [closed]\njobs: {}")
	if !workflowTriggersOn(prTyped, triggerEvent{Type: "pull_request", Action: "closed", Ref: "refs/heads/main"}) {
		t.Error("PR closed should trigger explicit types: [closed]")
	}
	if workflowTriggersOn(prTyped, triggerEvent{Type: "pull_request", Action: "opened", Ref: "refs/heads/main"}) {
		t.Error("PR opened should not trigger types: [closed]")
	}

	pathFiltered := mustOn("on:\n  push:\n    paths: ['src/**']\njobs: {}")
	if !workflowTriggersOn(pathFiltered, triggerEvent{Type: "push", Ref: "refs/heads/main",
		ChangedFiles: []string{"src/a.go"}, ChangedFilesKnown: true}) {
		t.Error("matching changed path should trigger")
	}
	if workflowTriggersOn(pathFiltered, triggerEvent{Type: "push", Ref: "refs/heads/main",
		ChangedFiles: []string{"docs/readme.md"}, ChangedFilesKnown: true}) {
		t.Error("non-matching changed path should not trigger")
	}
	if !workflowTriggersOn(pathFiltered, triggerEvent{Type: "push", Ref: "refs/heads/main"}) {
		t.Error("unknown diff must pass path filters open (new-branch push)")
	}

	pathsIgnore := mustOn("on:\n  push:\n    paths-ignore: ['docs/**']\njobs: {}")
	if workflowTriggersOn(pathsIgnore, triggerEvent{Type: "push", Ref: "refs/heads/main",
		ChangedFiles: []string{"docs/a.md", "docs/b.md"}, ChangedFilesKnown: true}) {
		t.Error("all-ignored changes should not trigger")
	}
	if !workflowTriggersOn(pathsIgnore, triggerEvent{Type: "push", Ref: "refs/heads/main",
		ChangedFiles: []string{"docs/a.md", "src/x.go"}, ChangedFilesKnown: true}) {
		t.Error("one non-ignored change should trigger")
	}

	repoDispatch := mustOn("on:\n  repository_dispatch:\n    types: [deploy]\njobs: {}")
	if !workflowTriggersOn(repoDispatch, triggerEvent{Type: "repository_dispatch", Action: "deploy"}) {
		t.Error("matching event_type should trigger")
	}
	if workflowTriggersOn(repoDispatch, triggerEvent{Type: "repository_dispatch", Action: "other"}) {
		t.Error("non-matching event_type should not trigger")
	}

	if workflowTriggersOn(branchFiltered, triggerEvent{Type: "release", Action: "published"}) {
		t.Error("event absent from on: must never trigger")
	}
}

func TestResolveDispatchInputs(t *testing.T) {
	on, err := ParseWorkflowOn([]byte(`
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

func TestTemplateToFormatExpr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"echo hi", ""}, // no template → literal token (covered below)
		{"${{ matrix.os }}", "matrix.os"},
		{"echo \"os=${{ matrix.os }} v=${{ matrix.version }}\"",
			"format('echo \"os={0} v={1}\"', matrix.os, matrix.version)"},
		{"it's ${{ env.X }}", "format('it''s {0}', env.X)"},
		{"a {brace} ${{ github.ref }}", "format('a {{brace}} {0}', github.ref)"},
	}
	for _, tc := range cases[1:] {
		if got := templateToFormatExpr(tc.in); got != tc.want {
			t.Errorf("templateToFormatExpr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	tok := templateToken("echo hi")
	if tok["type"] != 0 || tok["lit"] != "echo hi" {
		t.Errorf("plain string should stay literal: %v", tok)
	}
	tok = templateToken("echo ${{ matrix.os }}")
	if tok["type"] != 3 {
		t.Errorf("templated string should be a BasicExpression token: %v", tok)
	}
}
