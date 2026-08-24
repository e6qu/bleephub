package bleephub

import (
	"context"
	"testing"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

// forkPullRequestPayload is a pull_request event whose head lives in a
// different repository from the base — i.e. a fork contribution.
func forkPullRequestPayload(baseRepo, headRepo string) map[string]interface{} {
	return map[string]interface{}{
		"pull_request": map[string]interface{}{
			"number": 7,
			"head":   map[string]interface{}{"ref": "patch-1", "repo": map[string]interface{}{"full_name": headRepo}},
			"base":   map[string]interface{}{"ref": "main", "repo": map[string]interface{}{"full_name": baseRepo}},
		},
	}
}

// TestForkPullRequestJobGetsNoSecrets pins GitHub's rule that "with the
// exception of GITHUB_TOKEN, secrets are not passed to the runner when a
// workflow is triggered from a forked repository". A fork contributor can put
// arbitrary steps in the PR's workflow file, so handing that job the
// repository's secrets hands them to the contributor.
func TestForkPullRequestJobGetsNoSecrets(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	owner := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(owner, "fork-secrets", "", false)

	s.store.Mu.Lock()
	s.store.RepoSecrets[repo.FullName] = map[string]*store.Secret{
		"DEPLOY_KEY": {Name: "DEPLOY_KEY", Value: "very-secret"},
	}
	s.store.Mu.Unlock()

	build := func(eventName string, payload map[string]interface{}) map[string]string {
		t.Helper()
		def := &store.WorkflowDef{Name: "ci", Jobs: map[string]*store.JobDef{
			"build": {Steps: []store.StepDef{{Run: "echo hi"}}},
		}}
		meta := &actions.WorkflowEventMeta{
			EventName: eventName,
			Repo:      repo.FullName,
			Ref:       "refs/pull/7/merge",
			Payload:   payload,
		}
		wf, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", def, "alpine:latest", meta)
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		msg, err := s.actions.BuildJobMessageFromDef("http://localhost", wf, wf.Jobs["build"], "p", "t", 1, "alpine:latest")
		if err != nil {
			t.Fatal(err)
		}
		return secretsContextNames(t, msg)
	}

	fork := build("pull_request", forkPullRequestPayload(repo.FullName, "outsider/fork-secrets"))
	if _, leaked := fork["DEPLOY_KEY"]; leaked {
		t.Errorf("fork pull_request job received DEPLOY_KEY: %v", secretNames(fork))
	}
	if fork["GITHUB_TOKEN"] == "" {
		t.Error("fork pull_request job must still receive GITHUB_TOKEN")
	}

	// The same repository's own branch PR is not a fork and keeps its secrets.
	same := build("pull_request", forkPullRequestPayload(repo.FullName, repo.FullName))
	if same["DEPLOY_KEY"] != "very-secret" {
		t.Errorf("same-repo pull_request job lost DEPLOY_KEY: %v", secretNames(same))
	}

	// pull_request_target deliberately runs in the BASE repository's context
	// and does receive secrets — that is what distinguishes it.
	target := build("pull_request_target", forkPullRequestPayload(repo.FullName, "outsider/fork-secrets"))
	if target["DEPLOY_KEY"] != "very-secret" {
		t.Errorf("pull_request_target job lost DEPLOY_KEY: %v", secretNames(target))
	}
}
