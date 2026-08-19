package bleephub

import (
	"encoding/base64"
	"testing"
)

// TestPullRequestDetailDiffStats verifies the PR detail payload's
// changed_files/additions/deletions counters: they must equal the per-file
// sums GET /pulls/{n}/files reports, list items must not carry them at all
// (GitHub's pull-request-simple shape), they must track new commits on the
// head branch, and GraphQL's changedFiles/additions/deletions must serve the
// same refreshed totals.
func TestPullRequestDetailDiffStats(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoName := "pr-diff-stats"
	repoPath := "/api/v3/repos/admin/" + repoName
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": repoName, "auto_init": true,
	}).Body.Close()

	// Seed a file on main so the head branch can modify it: "hello\n" (1 line).
	s.put(t, repoPath+"/contents/greeting.txt", defaultToken, map[string]interface{}{
		"message": "seed greeting",
		"content": base64.StdEncoding.EncodeToString([]byte("hello\n")),
		"branch":  "main",
	}).Body.Close()

	refResp := s.get(t, repoPath+"/git/refs/heads/main", defaultToken)
	refData := decodeJSON(t, refResp)
	mainObj, _ := refData["object"].(map[string]interface{})
	mainSha, _ := mainObj["sha"].(string)
	if mainSha == "" {
		t.Fatalf("main ref sha missing: %v", refData)
	}
	s.post(t, repoPath+"/git/refs", defaultToken, map[string]interface{}{
		"ref": "refs/heads/feat", "sha": mainSha,
	}).Body.Close()

	// On feat: modify greeting.txt (1 addition + 1 deletion) and add
	// added.txt (1 addition), so the PR diff is 2 files, +2/-1.
	gResp := s.get(t, repoPath+"/contents/greeting.txt?ref=feat", defaultToken)
	gData := decodeJSON(t, gResp)
	blobSha, _ := gData["sha"].(string)
	if blobSha == "" {
		t.Fatalf("greeting.txt blob sha missing: %v", gData)
	}
	s.put(t, repoPath+"/contents/greeting.txt", defaultToken, map[string]interface{}{
		"message": "change greeting",
		"content": base64.StdEncoding.EncodeToString([]byte("hello there\n")),
		"branch":  "feat",
		"sha":     blobSha,
	}).Body.Close()
	s.put(t, repoPath+"/contents/added.txt", defaultToken, map[string]interface{}{
		"message": "add file",
		"content": base64.StdEncoding.EncodeToString([]byte("brand new\n")),
		"branch":  "feat",
	}).Body.Close()

	prResp := s.post(t, repoPath+"/pulls", defaultToken, map[string]interface{}{
		"title": "Diff stats", "head": "feat", "base": "main",
	})
	prCreated := decodeJSON(t, prResp)
	if prCreated["number"] != float64(1) {
		t.Fatalf("pr number = %v, want 1", prCreated["number"])
	}

	// The files endpoint is the source of truth: sum its per-file counters.
	filesResp := s.get(t, repoPath+"/pulls/1/files", defaultToken)
	files := decodeJSONArray(t, filesResp)
	wantChanged := len(files)
	wantAdds, wantDels := 0, 0
	for _, f := range files {
		wantAdds += int(f["additions"].(float64))
		wantDels += int(f["deletions"].(float64))
	}
	if wantChanged != 2 || wantAdds != 2 || wantDels != 1 {
		t.Fatalf("files sums = %d files +%d/-%d, want 2 files +2/-1: %v", wantChanged, wantAdds, wantDels, files)
	}

	detail := decodeJSON(t, s.get(t, repoPath+"/pulls/1", defaultToken))
	if detail["changed_files"] != float64(wantChanged) ||
		detail["additions"] != float64(wantAdds) ||
		detail["deletions"] != float64(wantDels) {
		t.Fatalf("detail stats = changed_files:%v additions:%v deletions:%v, want %d/%d/%d",
			detail["changed_files"], detail["additions"], detail["deletions"], wantChanged, wantAdds, wantDels)
	}

	// GitHub's list items are pull-request-simple: no diff counters at all.
	listResp := s.get(t, repoPath+"/pulls", defaultToken)
	list := decodeJSONArray(t, listResp)
	if len(list) != 1 {
		t.Fatalf("pulls list length = %d, want 1", len(list))
	}
	for _, key := range []string{"changed_files", "additions", "deletions"} {
		if _, present := list[0][key]; present {
			t.Errorf("list item unexpectedly carries %q (pull-request-simple must not)", key)
		}
	}

	// A new commit on the head branch must move the counters: later.txt adds
	// two lines, so the PR becomes 3 files, +4/-1.
	s.put(t, repoPath+"/contents/later.txt", defaultToken, map[string]interface{}{
		"message": "landed after open",
		"content": base64.StdEncoding.EncodeToString([]byte("one\ntwo\n")),
		"branch":  "feat",
	}).Body.Close()

	// GraphQL is asserted before the next REST detail GET, so it proves the
	// stored totals were refreshed by the head push itself.
	query := `query PRDiffStats($owner:String!,$repo:String!,$number:Int!){
		repository(owner:$owner,name:$repo){
			pullRequest(number:$number){ changedFiles additions deletions }
		}
	}`
	d := s.gqlData(t, query, map[string]interface{}{"owner": "admin", "repo": repoName, "number": 1})
	gqlRepo, _ := d["repository"].(map[string]interface{})
	gqlPR, _ := gqlRepo["pullRequest"].(map[string]interface{})
	if gqlPR == nil {
		t.Fatalf("pullRequest null: %v", d)
	}
	if gqlPR["changedFiles"] != float64(3) || gqlPR["additions"] != float64(4) || gqlPR["deletions"] != float64(1) {
		t.Errorf("GraphQL stats after push = changedFiles:%v additions:%v deletions:%v, want 3/4/1",
			gqlPR["changedFiles"], gqlPR["additions"], gqlPR["deletions"])
	}

	detail = decodeJSON(t, s.get(t, repoPath+"/pulls/1", defaultToken))
	if detail["changed_files"] != float64(3) ||
		detail["additions"] != float64(4) ||
		detail["deletions"] != float64(1) {
		t.Fatalf("detail stats after push = changed_files:%v additions:%v deletions:%v, want 3/4/1",
			detail["changed_files"], detail["additions"], detail["deletions"])
	}
}
