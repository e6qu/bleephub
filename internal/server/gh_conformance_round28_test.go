package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// Actions run-list ?status= accepts conclusion values (round-3 ACT).
func TestActionsRunListStatusFilterMatchesConclusion(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	seedRun(t, s.Server, "admin/act-status", "completed", "success")

	hasRun := func(query string) bool {
		resp := s.get(t, "/api/v3/repos/admin/act-status/actions/runs"+query, defaultToken)
		defer resp.Body.Close()
		var out struct {
			TotalCount int `json:"total_count"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		return out.TotalCount > 0
	}
	if !hasRun("?status=success") {
		t.Error("?status=success (a conclusion value) returned no runs")
	}
	if !hasRun("?status=completed") {
		t.Error("?status=completed returned no runs")
	}
	if hasRun("?status=failure") {
		t.Error("?status=failure matched a successful run")
	}
}

// GET /git/trees emits 6-char octal modes (round-3 git finding 1).
func TestGitTreeModeIsSixCharOctal(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := "tree-mode"
	requireStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name, "auto_init": true}), 201)

	commits := decodeJSONWithStatus2xxArray(t, s.get(t, "/api/v3/repos/admin/"+name+"/commits", defaultToken), 200)
	commit, _ := commits[0]["commit"].(map[string]interface{})
	tree, _ := commit["tree"].(map[string]interface{})
	treeSHA, _ := tree["sha"].(string)

	got := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/admin/"+name+"/git/trees/"+treeSHA, defaultToken), 200)
	entries, _ := got["tree"].([]interface{})
	if len(entries) == 0 {
		t.Fatal("empty tree")
	}
	for _, e := range entries {
		m, _ := e.(map[string]interface{})
		mode, _ := m["mode"].(string)
		if len(mode) != 6 {
			t.Errorf("tree entry mode = %q, want 6-char octal (e.g. 100644)", mode)
		}
	}
}

// GET /git/blobs with Accept: application/vnd.github.raw returns raw bytes
// (round-3 git finding 2).
func TestGitBlobRawMediaType(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := "blob-raw"
	requireStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name, "auto_init": true}), 201)

	readme := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/admin/"+name+"/contents/README.md", defaultToken), 200)
	blobSHA, _ := readme["sha"].(string)

	req, err := http.NewRequest(http.MethodGet, s.baseURL+"/api/v3/repos/admin/"+name+"/git/blobs/"+blobSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+defaultToken)
	req.Header.Set("Accept", "application/vnd.github.raw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("blob raw status = %d", resp.StatusCode)
	}
	// Raw bytes, not the base64 JSON envelope.
	if len(body) > 0 && body[0] == '{' {
		t.Errorf("blob .raw returned a JSON envelope, want raw bytes: %s", body)
	}
}

// A comment body that @-mentions a non-participant gives them a "mention"
// notification (round-3: closing the round-2 comment-mention remainder).
func TestNotificationMentionFromCommentBody(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "comment-mention", "", false)
	dan := s.createTestUser(t, "dan-m")
	danTok := s.store.CreateToken(dan.ID, "repo")

	// An issue dan doesn't participate in, then a comment that @-mentions dan.
	issue := s.store.CreateIssue(repo.ID, admin.ID, "topic", "no mention here", nil, nil, 0)
	s.store.CreateComment(issue.ID, admin.ID, "hey @dan-m please review")

	if !notificationReasonsFor(t, s, danTok.Value)["mention"] {
		t.Error("comment @-mention did not produce a `mention` notification for dan-m")
	}
}
