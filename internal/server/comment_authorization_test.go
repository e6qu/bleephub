package bleephub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func commentMutationRequest(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(rec, req)
	return rec
}

func TestCommentAuthorsMayEditAndDeleteWithoutPush(t *testing.T) {
	s := newTestServer()
	s.registerGHIssueRoutes()
	s.registerGHPRCommentsRoutes()

	owner := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(owner, "author-comment-permissions", "", false)
	author := seedTestUser(s, "comment-author")
	other := seedTestUser(s, "comment-other")
	token := s.store.CreateToken(author.ID, "repo").Value

	issue := s.store.CreateIssue(repo.ID, owner.ID, "issue", "", nil, nil, 0)
	ownIssueComment := s.store.CreateComment(issue.ID, author.ID, "before")
	otherIssueComment := s.store.CreateComment(issue.ID, other.ID, "other")

	issuePath := "/api/v3/repos/" + repo.FullName + "/issues/comments/"
	if got := commentMutationRequest(t, s, http.MethodPatch, issuePath+itoa(ownIssueComment.ID), token, `{"body":"after"}`); got.Code != http.StatusOK {
		t.Fatalf("author PATCH issue comment = %d: %s", got.Code, got.Body.String())
	}
	if got := s.store.GetComment(ownIssueComment.ID); got == nil || got.Body != "after" {
		t.Fatalf("author PATCH did not persist: %+v", got)
	}
	if got := commentMutationRequest(t, s, http.MethodPatch, issuePath+itoa(otherIssueComment.ID), token, `{"body":"stolen"}`); got.Code != http.StatusForbidden {
		t.Fatalf("non-author PATCH issue comment = %d, want 403: %s", got.Code, got.Body.String())
	}
	if got := commentMutationRequest(t, s, http.MethodDelete, issuePath+itoa(ownIssueComment.ID), token, ""); got.Code != http.StatusNoContent {
		t.Fatalf("author DELETE issue comment = %d: %s", got.Code, got.Body.String())
	}

	seedStorePullRequestBranches(t, s.store, repo, "feature")
	pr := s.store.CreatePullRequest(repo.ID, owner.ID, "pull", "", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("could not create pull request fixture")
	}
	ownReviewComment := s.store.PRReviewComments.CreateRootComment(pr.ID, author.ID, "file.go", "before", "deadbeef", "RIGHT", 1, 0)
	otherReviewComment := s.store.PRReviewComments.CreateRootComment(pr.ID, other.ID, "file.go", "other", "deadbeef", "RIGHT", 2, 0)

	pullPath := "/api/v3/repos/" + repo.FullName + "/pulls/comments/"
	if got := commentMutationRequest(t, s, http.MethodPatch, pullPath+itoa(ownReviewComment.ID), token, `{"body":"after"}`); got.Code != http.StatusOK {
		t.Fatalf("author PATCH review comment = %d: %s", got.Code, got.Body.String())
	}
	if got := commentMutationRequest(t, s, http.MethodDelete, pullPath+itoa(otherReviewComment.ID), token, ""); got.Code != http.StatusForbidden {
		t.Fatalf("non-author DELETE review comment = %d, want 403: %s", got.Code, got.Body.String())
	}
	if got := commentMutationRequest(t, s, http.MethodDelete, pullPath+itoa(ownReviewComment.ID), token, ""); got.Code != http.StatusNoContent {
		t.Fatalf("author DELETE review comment = %d: %s", got.Code, got.Body.String())
	}
}
