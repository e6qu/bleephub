package bleephub

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestMergeQueueFiresMergeGroupAndMerges pins the no-required-checks path: an
// enqueued pull request forms a merge group (dispatching an `on: merge_group`
// workflow), and with nothing required to wait on, merges into the base branch.
func TestMergeQueueFiresMergeGroupAndMerges(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := s.createTestRepo(t)
	repo := s.store.GetRepoByFullName(repoKey.fullName())
	if repo == nil {
		t.Fatal("repo not found")
	}
	s.cancelRepoRunsCleanup(t, repo.FullName)
	seedPullRequestBranches(t, s.Server, repo, "feature")
	commitWorkflowYAMLToStorage(t, s.Server, repo.FullName, ".github/workflows/mg.yml", `name: merge-group-check
on: [merge_group]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)
	admin := s.store.UsersByLogin["admin"]
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "queued pr", "", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("failed to create pull request")
	}

	if s.store.EnqueuePullRequest(pr.ID, false) == nil {
		t.Fatal("enqueue failed")
	}
	s.advanceMergeQueue(repo, "main")

	sawMergeGroup := false
	for _, wf := range s.runsForRepo(t, repo.FullName) {
		if wf.EventName == "merge_group" {
			sawMergeGroup = true
		}
	}
	if !sawMergeGroup {
		t.Fatal("enqueue did not fire an on: merge_group workflow run")
	}

	merged := s.store.GetPullRequest(pr.ID)
	if merged.State != "MERGED" {
		t.Fatalf("PR state = %q, want MERGED", merged.State)
	}
	if merged.MergeQueuePosition != 0 {
		t.Fatalf("merged PR is still queued at position %d", merged.MergeQueuePosition)
	}
}

// TestMergeQueueWaitsForRequiredCheckThenMerges pins the gated path: with a
// required status check configured, the queued PR waits until that check
// reports success on the merge-group head, then merges.
func TestMergeQueueWaitsForRequiredCheckThenMerges(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := s.createTestRepo(t)
	repo := s.store.GetRepoByFullName(repoKey.fullName())
	if repo == nil {
		t.Fatal("repo not found")
	}
	s.cancelRepoRunsCleanup(t, repo.FullName)
	seedPullRequestBranches(t, s.Server, repo, "feature")
	admin := s.store.UsersByLogin["admin"]
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "gated pr", "", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("failed to create pull request")
	}

	// Require the "ci" context on the base branch.
	s.store.Mu.Lock()
	s.store.Misc.BranchProtection[store.BpKey(repo.ID, "main")] = &store.BranchProtection{
		RequiredStatusChecks: &store.BPStatusChecks{Contexts: []string{"ci"}},
	}
	s.store.Mu.Unlock()

	if s.store.EnqueuePullRequest(pr.ID, false) == nil {
		t.Fatal("enqueue failed")
	}
	s.advanceMergeQueue(repo, "main")

	// The required check has not reported, so the entry must still be waiting.
	if waiting := s.store.GetPullRequest(pr.ID); waiting.State != "OPEN" || waiting.MergeQueuePosition == 0 {
		t.Fatalf("entry advanced prematurely: state=%q position=%d", waiting.State, waiting.MergeQueuePosition)
	}

	// Resolve the merge-group head the readonly-queue ref points at.
	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	stor := s.store.GetGitStorage(owner, name)
	baseSha := store.ResolveBranchSha(stor, "main")
	groupRef := mergeQueueGroupRef("main", pr.Number, baseSha)
	refObj, err := stor.Reference(groupRef)
	if err != nil {
		t.Fatalf("merge-group ref %s not created: %v", groupRef, err)
	}
	groupHead := refObj.Hash().String()

	// Report the required check green on the merge-group head, which advances the queue.
	resp := s.post(t, fmt.Sprintf("%s/statuses/%s", repoKey.path(), groupHead), defaultToken, map[string]interface{}{
		"state":   "success",
		"context": "ci",
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("post status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	merged := s.store.GetPullRequest(pr.ID)
	if merged.State != "MERGED" {
		t.Fatalf("PR state = %q, want MERGED after required check passed", merged.State)
	}
	if merged.MergeQueuePosition != 0 {
		t.Fatalf("merged PR is still queued at position %d", merged.MergeQueuePosition)
	}
}
