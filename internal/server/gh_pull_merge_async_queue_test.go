package bleephub

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// merge_action merge_queue adds the pull request to its base branch's merge
// queue. The request is acknowledged "pending" with a UUID; its result is
// "enqueued" while the entry waits for its required check and "merged", with
// the merge commit, once the queue merges it. A repeat while queued answers
// 200 "enqueued", and fields only a direct merge honours are refused.
func TestMergeAsyncMergeQueueAction(t *testing.T) {
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
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "queued pr", "", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("failed to create pull request")
	}
	s.store.Mu.Lock()
	s.store.Misc.BranchProtection[store.BpKey(repo.ID, "main")] = &store.BranchProtection{
		RequiredStatusChecks: &store.BPStatusChecks{Contexts: []string{"ci"}},
	}
	s.store.Mu.Unlock()
	asyncBase := fmt.Sprintf("%s/pulls/%d/merge-async", repoKey.path(), pr.Number)

	for _, refused := range []map[string]interface{}{
		{"merge_action": "merge_queue", "merge_method": "squash"},
		{"merge_action": "train"},
	} {
		resp := s.put(t, asyncBase, defaultToken, refused)
		requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
		resp.Body.Close()
	}

	resp := s.put(t, asyncBase, defaultToken, map[string]interface{}{"merge_action": "merge_queue", "bypass_rules": true})
	requireHTTPStatus(t, resp, http.StatusAccepted)
	ack := decodeJSON(t, resp)
	details, _ := ack["details"].(map[string]interface{})
	if ack["status"] != "pending" || details["merge_action"] != "merge_queue" || details["bypass_rules"] != true {
		t.Fatalf("merge_queue acknowledgement = %#v", ack)
	}
	mergeUUID, _ := details["uuid"].(string)
	if mergeUUID == "" {
		t.Fatalf("merge_queue acknowledgement has no uuid: %#v", ack)
	}
	if waiting := s.store.GetPullRequest(pr.ID); waiting.State != "OPEN" || waiting.MergeQueuePosition == 0 {
		t.Fatalf("entry not waiting in the queue: state=%q position=%d", waiting.State, waiting.MergeQueuePosition)
	}

	resp = s.get(t, asyncBase+"/"+mergeUUID, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	result := decodeJSON(t, resp)
	resultDetails, _ := result["details"].(map[string]interface{})
	if result["status"] != "enqueued" || len(resultDetails) != 1 || resultDetails["message"] == "" {
		t.Fatalf("queued result = %#v", result)
	}

	resp = s.put(t, asyncBase, defaultToken, map[string]interface{}{"merge_action": "merge_queue"})
	requireHTTPStatus(t, resp, http.StatusOK)
	if again := decodeJSON(t, resp); again["status"] != "enqueued" {
		t.Fatalf("repeat merge_queue request = %#v", again)
	}

	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	stor := s.store.GetGitStorage(owner, name)
	groupRef := mergeQueueGroupRef("main", pr.Number, store.ResolveBranchSha(stor, "main"))
	refObj, err := stor.Reference(groupRef)
	if err != nil {
		t.Fatalf("merge-group ref %s not created: %v", groupRef, err)
	}
	resp = s.post(t, fmt.Sprintf("%s/statuses/%s", repoKey.path(), refObj.Hash()), defaultToken, map[string]interface{}{
		"state": "success", "context": "ci",
	})
	requireHTTPStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = s.get(t, asyncBase+"/"+mergeUUID, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	merged := decodeJSON(t, resp)
	mergedDetails, _ := merged["details"].(map[string]interface{})
	if merged["status"] != "merged" || mergedDetails["sha"] != s.store.GetPullRequest(pr.ID).MergeCommitSHA || mergedDetails["sha"] == "" {
		t.Fatalf("result after the queue merged = %#v", merged)
	}
}
