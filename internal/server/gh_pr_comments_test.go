package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestPRReviewComments_RootAndReply(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHPRCommentsRoutes()

	user := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(user, "rc-repo", "", false)
	seedPullRequestBranches(t, s, repo, "feat")
	pr := s.store.CreatePullRequest(repo.ID, user.ID, "title", "body", "feat", "main", false, nil, nil, 0)

	create := func(path string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}

	rootBody, _ := json.Marshal(map[string]any{
		"body":      "consider refactoring this",
		"path":      "src/foo.go",
		"line":      42,
		"side":      "RIGHT",
		"commit_id": "abc123",
	})
	w := create("/api/v3/repos/admin/rc-repo/pulls/"+itoa(pr.Number)+"/comments", rootBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("create root: %d body=%s", w.Code, w.Body.String())
	}
	var root map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &root)
	rootID := int(root["id"].(float64))
	if root["path"] != "src/foo.go" || root["line"].(float64) != 42 {
		t.Errorf("root shape: %v", root)
	}

	replyBody, _ := json.Marshal(map[string]string{"body": "I agree"})
	w = create("/api/v3/repos/admin/rc-repo/pulls/"+itoa(pr.Number)+"/comments/"+itoa(rootID)+"/replies", replyBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("reply: %d body=%s", w.Code, w.Body.String())
	}
	var reply map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &reply)
	if reply["in_reply_to_id"].(float64) != float64(rootID) {
		t.Errorf("in_reply_to: %v", reply["in_reply_to_id"])
	}
	if reply["path"] != "src/foo.go" {
		t.Errorf("reply path: %v", reply["path"])
	}

	inlineReply, _ := json.Marshal(map[string]any{"body": "another reply", "in_reply_to": rootID})
	w = create("/api/v3/repos/admin/rc-repo/pulls/"+itoa(pr.Number)+"/comments", inlineReply)
	if w.Code != http.StatusCreated {
		t.Fatalf("inline reply: %d body=%s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest("GET", "/api/v3/repos/admin/rc-repo/pulls/"+itoa(pr.Number)+"/comments", nil)
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	var list []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 3 {
		t.Errorf("list len = %d, want 3", len(list))
	}

	req = httptest.NewRequest("GET", "/api/v3/repos/admin/rc-repo/pulls/comments/"+itoa(rootID), nil)
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get by id: %d body=%s", w.Code, w.Body.String())
	}

	patch, _ := json.Marshal(map[string]string{"body": "EDITED"})
	req = httptest.NewRequest("PATCH", "/api/v3/repos/admin/rc-repo/pulls/comments/"+itoa(rootID), bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w = httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d body=%s", w.Code, w.Body.String())
	}
	var patched map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &patched)
	if patched["body"] != "EDITED" {
		t.Errorf("body after patch: %v", patched["body"])
	}

	// Root and both replies share one thread; public clients read and mutate it through GitHub GraphQL.
	threads := s.store.PRReviewComments.ListThreads(pr.ID)
	if len(threads) != 1 {
		t.Fatalf("threads len = %d", len(threads))
	}
	if threads[0].IsResolved {
		t.Errorf("thread resolved = %v", threads[0].IsResolved)
	}
}

func TestPRReviewComments_MissingBody422(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHPRCommentsRoutes()

	user := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(user, "rc2", "", false)
	seedPullRequestBranches(t, s, repo, "f", "m")
	pr := s.store.CreatePullRequest(repo.ID, user.ID, "t", "b", "f", "m", false, nil, nil, 0)

	bad, _ := json.Marshal(map[string]any{"path": "x.go"})
	req := httptest.NewRequest("POST", "/api/v3/repos/admin/rc2/pulls/"+itoa(pr.Number)+"/comments", bytes.NewReader(bad))
	req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing body: %d", w.Code)
	}
}

// TestPRReviewComments_InvertedRange422 covers a multi-line comment whose
// start_line does not precede line: GitHub rejects it with 422 rather than
// storing an inverted range.
func TestPRReviewComments_InvertedRange422(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHPRCommentsRoutes()

	user := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(user, "rc3", "", false)
	seedPullRequestBranches(t, s, repo, "f", "m")
	pr := s.store.CreatePullRequest(repo.ID, user.ID, "t", "b", "f", "m", false, nil, nil, 0)

	post := func(payload map[string]any) int {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/v3/repos/admin/rc3/pulls/"+itoa(pr.Number)+"/comments", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer bleephub-admin-token-00000000000000000000")
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w.Code
	}

	// start_line >= line is rejected.
	if code := post(map[string]any{"body": "x", "path": "a.go", "line": 5, "start_line": 5, "side": "RIGHT"}); code != http.StatusUnprocessableEntity {
		t.Errorf("start_line==line: status %d, want 422", code)
	}
	if code := post(map[string]any{"body": "x", "path": "a.go", "line": 5, "start_line": 9, "side": "RIGHT"}); code != http.StatusUnprocessableEntity {
		t.Errorf("start_line>line: status %d, want 422", code)
	}
	// A valid range (start_line < line) is accepted.
	if code := post(map[string]any{"body": "x", "path": "a.go", "line": 9, "start_line": 5, "side": "RIGHT"}); code != http.StatusCreated {
		t.Errorf("valid range: status %d, want 201", code)
	}
}

func TestPRReviewCommentReadsReturnDetachedSnapshots(t *testing.T) {
	st := store.NewPRReviewCommentStore(nil)
	comment := st.CreateRootComment(1, 2, "file.go", "stored", "deadbeef", "RIGHT", 7, 3)
	if comment == nil {
		t.Fatal("could not create review comment")
	}

	got := st.Get(comment.ID)
	*got.Line = 99
	got.Body = "mutated"
	listed := st.ListForPR(1)
	listed[0].Path = "other.go"
	threads := st.ListThreads(1)
	threads[0].Comments[0].StartLine = nil

	again := st.Get(comment.ID)
	if again.Body != "stored" || again.Path != "file.go" || again.Line == nil || *again.Line != 7 || again.StartLine == nil || *again.StartLine != 3 {
		t.Fatalf("caller mutation reached stored review comment: %+v", again)
	}
}
