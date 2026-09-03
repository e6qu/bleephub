package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestCheckRunPatchAdvancesMergeQueue pins that reporting a required check the
// canonical check-run way — POST in_progress, then PATCH to completed/success —
// advances the merge queue, exactly as the statuses API does. The update handler
// previously called only the auto-merge hook (which matches a PR head, never the
// merge-group commit), so a queue gated on a check reported this way stalled.
func TestCheckRunPatchAdvancesMergeQueue(t *testing.T) {
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

	s.store.Mu.Lock()
	s.store.Misc.BranchProtection[store.BpKey(repo.ID, "main")] = &store.BranchProtection{
		RequiredStatusChecks: &store.BPStatusChecks{Contexts: []string{"ci"}},
	}
	s.store.Mu.Unlock()

	if s.store.EnqueuePullRequest(pr.ID, false) == nil {
		t.Fatal("enqueue failed")
	}
	s.advanceMergeQueue(repo, "main")
	if waiting := s.store.GetPullRequest(pr.ID); waiting.State != "OPEN" || waiting.MergeQueuePosition == 0 {
		t.Fatalf("entry advanced prematurely: state=%q position=%d", waiting.State, waiting.MergeQueuePosition)
	}

	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	stor := s.store.GetGitStorage(owner, name)
	baseSha := store.ResolveBranchSha(stor, "main")
	groupRef := mergeQueueGroupRef("main", pr.Number, baseSha)
	refObj, err := stor.Reference(groupRef)
	if err != nil {
		t.Fatalf("merge-group ref %s not created: %v", groupRef, err)
	}
	groupHead := refObj.Hash().String()

	// Report the required check via check-runs: create in_progress, then complete.
	resp := s.post(t, repoKey.path()+"/check-runs", defaultToken, map[string]interface{}{
		"name": "ci", "head_sha": groupHead, "status": "in_progress",
	})
	created := decodeJSON(t, resp)
	runID := int(created["id"].(float64))

	resp = s.patch(t, fmt.Sprintf("%s/check-runs/%d", repoKey.path(), runID), defaultToken, map[string]interface{}{
		"status": "completed", "conclusion": "success",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch check run = %d, want 200", resp.StatusCode)
	}

	merged := s.store.GetPullRequest(pr.ID)
	if merged.State != "MERGED" {
		t.Fatalf("PR state = %q, want MERGED after the required check completed via check-runs", merged.State)
	}
	if merged.MergeQueuePosition != 0 {
		t.Fatalf("merged PR still queued at position %d", merged.MergeQueuePosition)
	}
}

// TestCheckRunUpdateAccumulatesAnnotations pins that annotations reported across
// successive check-run updates accumulate, matching GitHub (≤50 per request,
// appended). Each PATCH previously replaced the whole output, discarding every
// annotation reported before it and resetting annotations_count.
func TestCheckRunUpdateAccumulatesAnnotations(t *testing.T) {
	t.Parallel()
	s, doReq, headSHA := setupChecksPaginationServer(t)

	create, _ := json.Marshal(map[string]any{
		"name":     "lint",
		"head_sha": headSHA,
		"output": map[string]any{
			"title":   "lint",
			"summary": "start",
			"annotations": []map[string]any{
				{"path": "a.go", "start_line": 1, "end_line": 1, "annotation_level": "warning", "message": "first"},
			},
		},
	})
	w := doReq("POST", "/api/v3/repos/admin/checks-pg/check-runs", create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	runID := int(created["id"].(float64))

	patch, _ := json.Marshal(map[string]any{
		"output": map[string]any{
			"title":   "lint",
			"summary": "more",
			"annotations": []map[string]any{
				{"path": "b.go", "start_line": 2, "end_line": 2, "annotation_level": "failure", "message": "second"},
			},
		},
	})
	w = doReq("PATCH", fmt.Sprintf("/api/v3/repos/admin/checks-pg/check-runs/%d", runID), patch)
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d body=%s", w.Code, w.Body.String())
	}

	// annotations_count reflects the accumulated total.
	var updated map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if out, _ := updated["output"].(map[string]any); out == nil || out["annotations_count"] != float64(2) {
		t.Fatalf("annotations_count after two reports = %v, want 2", updated["output"])
	}

	w = doReq("GET", fmt.Sprintf("/api/v3/repos/admin/checks-pg/check-runs/%d/annotations", runID), nil)
	var anns []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &anns)
	if len(anns) != 2 {
		t.Fatalf("annotations after two reports = %d, want 2 (accumulated)", len(anns))
	}
	msgs := map[string]bool{}
	for _, a := range anns {
		msgs[a["message"].(string)] = true
	}
	if !msgs["first"] || !msgs["second"] {
		t.Fatalf("expected both 'first' and 'second' annotations, got %v", anns)
	}
	_ = s
}
