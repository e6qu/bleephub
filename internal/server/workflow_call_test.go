package bleephub

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

// commitFilesToStorage commits a set of files in ONE commit at HEAD of
// the repo's git storage, creating owner + repo when missing.
func commitFilesToStorage(t *testing.T, s *Server, repoFullName string, files map[string]string) {
	t.Helper()
	parts := strings.Split(repoFullName, "/")
	if len(parts) != 2 {
		t.Fatalf("expected owner/repo, got %q", repoFullName)
	}
	if s.store.UsersByLogin[parts[0]] == nil {
		s.store.Mu.Lock()
		user := &store.User{ID: s.store.NextUser, Login: parts[0], Type: "User", CreatedAt: fixedTestTime, UpdatedAt: fixedTestTime}
		s.store.NextUser++
		s.store.Users[user.ID] = user
		s.store.UsersByLogin[user.Login] = user
		s.store.Mu.Unlock()
	}
	if s.store.GetRepo(parts[0], parts[1]) == nil {
		s.store.CreateRepo(s.store.UsersByLogin[parts[0]], parts[1], "", false)
	}
	storer := s.store.GetGitStorage(parts[0], parts[1])
	if storer == nil {
		t.Fatalf("no git storage for %s", repoFullName)
	}
	fs := memfs.New()
	repo, err := git.Init(storer, fs)
	if err != nil {
		repo, err = git.Open(storer, fs)
		if err != nil {
			t.Fatalf("init/open repo: %v", err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for path, body := range files {
		if idx := strings.LastIndex(path, "/"); idx > 0 {
			if err := fs.MkdirAll(path[:idx], 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", path[:idx], err)
			}
		}
		f, err := fs.Create(path)
		if err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		_, _ = f.Write([]byte(body))
		_ = f.Close()
		if _, err := wt.Add(path); err != nil {
			t.Fatalf("git add %s: %v", path, err)
		}
	}
	commitHash, err := wt.Commit("test files", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: fixedTestTime},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	mainRef := plumbing.NewBranchReferenceName("main")
	if err := storer.SetReference(plumbing.NewHashReference(mainRef, commitHash)); err != nil {
		t.Fatalf("set main ref: %v", err)
	}
	if err := storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, mainRef)); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
}

const calledWorkflowYAML = `name: called
on:
  workflow_call:
    inputs:
      env:
        type: string
        required: true
      replicas:
        type: number
        default: 2
    secrets:
      deploy-key:
        required: false
    outputs:
      url:
        value: ${{ jobs.publish.outputs.url }}
jobs:
  publish:
    runs-on: ubuntu-latest
    outputs:
      url: ${{ steps.out.outputs.url }}
    steps:
      - run: echo publish
`

const callerWorkflowYAML = `name: caller
on: [push]
jobs:
  setup:
    runs-on: ubuntu-latest
    steps:
      - run: echo setup
  deploy:
    needs: [setup]
    uses: ./.github/workflows/called.yml
    with:
      env: prod-${{ needs.setup.outputs.version }}
  notify:
    needs: [deploy]
    runs-on: ubuntu-latest
    steps:
      - run: echo done
`

func TestWorkflowCallEndToEnd(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "callowner/call-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitFilesToStorage(t, s.Server, repoKey, map[string]string{
		".github/workflows/caller.yml": callerWorkflowYAML,
		".github/workflows/called.yml": calledWorkflowYAML,
	})

	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	s.store.Mu.RLock()
	var wf *store.Workflow
	for _, w := range s.store.Workflows {
		if w.RepoFullName == repoKey && w.Name == "caller" {
			wf = w
		}
	}
	s.store.Mu.RUnlock()
	if wf == nil {
		t.Fatal("caller workflow not triggered")
	}

	// Expanded graph: setup, notify, deploy/__call (gate), deploy/publish, deploy (collector).
	for _, key := range []string{"setup", "notify", "deploy/__call", "deploy/publish", "deploy"} {
		if wf.Jobs[key] == nil {
			t.Fatalf("missing job %q; have %v", key, jobKeys(wf))
		}
	}
	if !wf.Jobs["deploy/__call"].Hidden || !wf.Jobs["deploy"].Hidden {
		t.Error("gate and collector must be hidden")
	}
	if wf.Jobs["deploy/publish"].Hidden {
		t.Error("called job must not be hidden")
	}
	if got := wf.Jobs["deploy/publish"].DisplayName; got != "deploy / publish" {
		t.Errorf("called job display name = %q, want 'deploy / publish'", got)
	}

	// Complete setup with an output; the gate must resolve `with:`.
	s.store.Mu.Lock()
	wf.Jobs["setup"].Outputs["version"] = "1.2.3"
	setupID := wf.Jobs["setup"].JobID
	s.store.Mu.Unlock()
	s.actions.OnJobCompleted(context.Background(), setupID, "Succeeded")

	s.store.Mu.RLock()
	gate := wf.Jobs["deploy/__call"]
	publish := wf.Jobs["deploy/publish"]
	gateStatus := gate.Status
	publishStatus := publish.Status
	binding := publish.Def.Call
	s.store.Mu.RUnlock()

	if gateStatus != store.JobStatusCompleted {
		t.Fatalf("gate status = %q, want completed", gateStatus)
	}
	if publishStatus != store.JobStatusQueued {
		t.Fatalf("publish status = %q, want queued", publishStatus)
	}
	resolved := binding.ResolvedInputs()
	if resolved["env"] != "prod-1.2.3" {
		t.Errorf("resolved input env = %v, want prod-1.2.3", resolved["env"])
	}
	if resolved["replicas"] != float64(2) {
		t.Errorf("resolved input replicas = %v (%T), want default 2", resolved["replicas"], resolved["replicas"])
	}

	// The called job's runner message carries the call inputs and the
	// caller-view needs context (no gate, unprefixed keys).
	msg, err := s.actions.BuildJobMessageFromDef("http://localhost", wf, publish, "p", "t", 1, "alpine:latest")
	if err != nil {
		t.Fatal(err)
	}
	ctxData := msg["contextData"].(map[string]interface{})
	if ctxData["inputs"] == nil {
		t.Error("called job message missing inputs context")
	}

	// Complete publish with the url output; the collector must map it.
	s.store.Mu.Lock()
	publish.Outputs["url"] = "https://prod.example"
	publishID := publish.JobID
	s.store.Mu.Unlock()
	s.actions.OnJobCompleted(context.Background(), publishID, "Succeeded")

	s.store.Mu.RLock()
	collector := wf.Jobs["deploy"]
	notify := wf.Jobs["notify"]
	collectorStatus := collector.Status
	collectorURL := collector.Outputs["url"]
	notifyStatus := notify.Status
	s.store.Mu.RUnlock()

	if collectorStatus != store.JobStatusCompleted {
		t.Fatalf("collector status = %q, want completed", collectorStatus)
	}
	if collectorURL != "https://prod.example" {
		t.Errorf("collector output url = %q, want mapped from called job", collectorURL)
	}
	if notifyStatus != store.JobStatusQueued {
		t.Fatalf("notify status = %q, want queued (needs.deploy satisfied)", notifyStatus)
	}

	// notify's needs context exposes the caller key with the mapped outputs.
	nmsg, err := s.actions.BuildJobMessageFromDef("http://localhost", wf, notify, "p2", "t2", 2, "alpine:latest")
	if err != nil {
		t.Fatal(err)
	}
	nctx := nmsg["contextData"].(map[string]interface{})
	needsJSON := nctx["needs"]
	if needsJSON == nil {
		t.Fatal("notify message missing needs context")
	}
}

func jobKeys(wf *store.Workflow) []string {
	keys := make([]string, 0, len(wf.Jobs))
	for k := range wf.Jobs {
		keys = append(keys, k)
	}
	return keys
}

func TestWorkflowCallSkipCascade(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "callskip/skip-repo"
	caller := `name: skip-caller
on: [push]
jobs:
  deploy:
    if: github.ref == 'refs/heads/release'
    uses: ./.github/workflows/called.yml
    with:
      env: prod
  notify:
    needs: [deploy]
    runs-on: ubuntu-latest
    steps:
      - run: echo done
`
	commitFilesToStorage(t, s.Server, repoKey, map[string]string{
		".github/workflows/caller.yml": caller,
		".github/workflows/called.yml": calledWorkflowYAML,
	})

	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	s.store.Mu.RLock()
	var wf *store.Workflow
	for _, w := range s.store.Workflows {
		if w.RepoFullName == repoKey {
			wf = w
		}
	}
	s.store.Mu.RUnlock()
	if wf == nil {
		t.Fatal("workflow not triggered")
	}

	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, key := range []string{"deploy/__call", "deploy/publish", "deploy", "notify"} {
		j := wf.Jobs[key]
		if j == nil {
			t.Fatalf("missing job %q", key)
		}
		if j.Status != store.JobStatusSkipped {
			t.Errorf("job %q status = %q, want skipped (gate if: false must cascade)", key, j.Status)
		}
	}
	if wf.Status != store.WorkflowStatusCompleted {
		t.Errorf("workflow status = %q, want completed", wf.Status)
	}
}

func TestWorkflowCallValidation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "callval/val-repo"
	commitFilesToStorage(t, s.Server, repoKey, map[string]string{
		".github/workflows/called.yml":     calledWorkflowYAML,
		".github/workflows/not-called.yml": "name: x\non: [push]\njobs:\n  a:\n    steps:\n      - run: echo a\n",
	})

	cases := []struct {
		name string
		job  *store.JobDef
		want string
	}{
		{"missing required input", &store.JobDef{Uses: "./.github/workflows/called.yml"}, "requires input"},
		{"unknown input", &store.JobDef{Uses: "./.github/workflows/called.yml",
			With: map[string]string{"env": "x", "bogus": "y"}}, "does not define input"},
		{"not workflow_call", &store.JobDef{Uses: "./.github/workflows/not-called.yml",
			With: map[string]string{}}, "does not declare on: workflow_call"},
		{"missing file", &store.JobDef{Uses: "./.github/workflows/nope.yml"}, "not found"},
	}
	for _, tc := range cases {
		def := &store.WorkflowDef{Name: "v", Jobs: map[string]*store.JobDef{"call": tc.job}}
		meta := &actions.WorkflowEventMeta{EventName: "push", Repo: repoKey}
		_, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", def, "alpine:latest", meta)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
}

func TestWorkflowCallNestingDepthLimit(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "calldeep/deep-repo"
	// l1 → l2 → l3 → l4 → l5: five levels exceeds GitHub's four.
	files := map[string]string{}
	for i := 1; i <= 5; i++ {
		var body string
		if i == 5 {
			body = "name: l5\non:\n  workflow_call:\njobs:\n  leaf:\n    steps:\n      - run: echo leaf\n"
		} else {
			body = "name: l" + string(rune('0'+i)) + "\non:\n  workflow_call:\njobs:\n  next:\n    uses: ./.github/workflows/l" + string(rune('0'+i+1)) + ".yml\n"
		}
		files[".github/workflows/l"+string(rune('0'+i))+".yml"] = body
	}
	commitFilesToStorage(t, s.Server, repoKey, files)

	def := &store.WorkflowDef{Name: "deep", Jobs: map[string]*store.JobDef{
		"start": {Uses: "./.github/workflows/l2.yml"},
	}}
	meta := &actions.WorkflowEventMeta{EventName: "push", Repo: repoKey}
	_, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", def, "alpine:latest", meta)
	if err == nil || !strings.Contains(err.Error(), "nested deeper") {
		t.Errorf("err = %v, want nesting-depth error", err)
	}
}

func TestRemapCallSecrets(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	binding := &store.WorkflowCallBinding{
		CalledPath: "x.yml",
		SecretsMap: map[string]string{
			"deploy-key": "${{ secrets.PROD_KEY }}",
		},
	}
	wf := &store.Workflow{}
	got, err := actions.RemapCallSecrets(s.actions, wf, binding, map[string]string{
		"PROD_KEY": "sekrit",
		"OTHER":    "hidden-from-called",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["deploy-key"] != "sekrit" {
		t.Errorf("deploy-key = %q, want mapped from PROD_KEY", got["deploy-key"])
	}
	if _, leaked := got["OTHER"]; leaked {
		t.Error("unmapped caller secret must not pass through")
	}
	if len(got) != 1 {
		t.Errorf("got %d secrets, want 1", len(got))
	}
}

func TestWorkflowCallInputTypingIsStrict(t *testing.T) {
	t.Parallel()
	boolean := &store.WorkflowInputDef{Type: "boolean"}
	if _, err := actions.TypedCallInput(boolean, "yes"); err == nil {
		t.Fatal("boolean input accepted a truthy non-boolean value")
	}
	got, err := actions.TypedCallInput(boolean, "false")
	if err != nil || got != false {
		t.Fatalf("false boolean = %#v, %v", got, err)
	}

	number := &store.WorkflowInputDef{Type: "number"}
	if _, err := actions.TypedCallInput(number, "12abc"); err == nil {
		t.Fatal("number input accepted a numeric prefix")
	}
	got, err = actions.TypedCallInput(number, "12.5")
	if err != nil || got != float64(12.5) {
		t.Fatalf("number = %#v, %v", got, err)
	}

	choice := &store.WorkflowInputDef{Type: "choice", Options: []interface{}{"blue", "green"}}
	if _, err := actions.TypedCallInput(choice, "red"); err == nil {
		t.Fatal("choice input accepted an undeclared option")
	}
}

func TestReusableWorkflowTopLevelEnvironmentReachesCalledJobs(t *testing.T) {
	t.Parallel()
	out := &store.WorkflowDef{Jobs: map[string]*store.JobDef{}}
	called := &store.WorkflowDef{
		Env: map[string]string{"FROM_CALLED": "workflow", "OVERRIDDEN": "workflow"},
		Jobs: map[string]*store.JobDef{
			"build": {Env: map[string]string{"OVERRIDDEN": "job"}},
		},
	}
	binding := &store.WorkflowCallBinding{CallerKey: "call"}
	for key, job := range called.Jobs {
		child := *job
		child.Env = actions.MergedCallEnvironment(called.Env, child.Env)
		child.Call = binding
		out.Jobs["call/"+key] = &child
	}
	env := out.Jobs["call/build"].Env
	if env["FROM_CALLED"] != "workflow" || env["OVERRIDDEN"] != "job" {
		t.Fatalf("called job environment = %#v", env)
	}
}

func TestReusableWorkflowTemplateFailuresFailClosed(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	binding := &store.WorkflowCallBinding{
		CalledPath: "called.yml",
		With:       map[string]string{"flag": "${{ inputs.missing["},
		InputDefs:  map[string]*store.WorkflowInputDef{"flag": {Type: "boolean"}},
	}
	wf := &store.Workflow{Jobs: map[string]*store.WorkflowJob{}}
	gate := &store.WorkflowJob{Def: &store.JobDef{Call: binding}}
	if s.actions.ResolveCallInputsLocked(wf, gate) {
		t.Fatal("broken input template succeeded")
	}

	binding.SecretsMap = map[string]string{"TOKEN": "${{ secrets.MISSING["}
	if _, err := actions.RemapCallSecrets(s.actions, wf, binding, map[string]string{}); err == nil {
		t.Fatal("broken secret template succeeded")
	}
}
