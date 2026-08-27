package bleephub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

// Issue + PR moderation: comment edit/delete, issue/PR locking.
// Real GitHub keeps these on the /issues path (PRs are issues internally).

// validLockReasons matches GitHub's REST lock reasons (lowercase; the GraphQL
// enum is uppercase).
var validLockReasons = map[string]bool{
	"off-topic":  true,
	"too heated": true,
	"resolved":   true,
	"spam":       true,
}

// --- Comment edit / delete (Issue + PR conversation comments) ---

func (s *Server) handleUpdateIssueComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	idStr := r.PathValue("comment_id")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	existing := s.store.GetComment(id)
	if existing == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !requireRepoOwns(w, repo, s.store.CommentRepoID(existing)) {
		return
	}
	if existing.AuthorID != user.ID && !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to edit another user's comment")
		return
	}

	// Optimistic concurrency (STORE-016): reject a stale If-Match with 412.
	if !checkIfMatch(w, r, store.CommentToJSON(existing, s.store, s.baseURL(r), repo.FullName, commentParentNumber(s.store, existing))) {
		return
	}

	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Body == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}

	updated := s.store.UpdateCommentBody(id, user.ID, req.Body)
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	parentNumber := commentParentNumber(s.store, updated)
	s.emitWebhookEvent(repo.FullName, "issue_comment", "edited",
		buildIssueCommentPayload(s.store, repo, updated, user, "edited", s.baseURL(r), parentNumber))
	writeJSON(w, http.StatusOK, store.CommentToJSON(updated, s.store, s.baseURL(r), repo.FullName, parentNumber))
}

func (s *Server) handleDeleteIssueComment(w http.ResponseWriter, r *http.Request, idStr string) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.store.GetComment(id)
	if c == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !requireRepoOwns(w, repo, s.store.CommentRepoID(c)) {
		return
	}
	if c.AuthorID != user.ID && !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to delete another user's comment")
		return
	}
	parentNumber := commentParentNumber(s.store, c)
	payload := buildIssueCommentPayload(s.store, repo, c, user, "deleted", s.baseURL(r), parentNumber)
	if !s.store.DeleteComment(id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// github records a comment_deleted event so the timeline shows a comment was
	// removed and by whom; it carries the deleted comment's id and author.
	s.store.RecordIssueOrPREvent(repo.ID, parentNumber, user.ID, "comment_deleted", map[string]interface{}{
		"comment_id":  c.ID,
		"assignee_id": c.AuthorID,
	})
	s.emitWebhookEvent(repo.FullName, "issue_comment", "deleted", payload)
	w.WriteHeader(http.StatusNoContent)
}

// handleIssuesDeleteDispatch routes DELETE /repos/{}/issues/{p1}/{p2} to the
// by-id comment delete, unlock, or sub-issue removal.
func (s *Server) handleIssuesDeleteDispatch(w http.ResponseWriter, r *http.Request) {
	p1 := r.PathValue("p1")
	p2 := r.PathValue("p2")
	if p1 == "comments" {
		s.handleDeleteIssueComment(w, r, p2)
		return
	}
	if p2 == "lock" {
		s.handleUnlockIssue(w, r, p1)
		return
	}
	if p2 == "sub_issues" || p2 == "sub_issue" {
		r.SetPathValue("number", p1)
		s.handleRemoveSubIssue(w, r)
		return
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

// commentParentNumber returns the issue or PR number owning the comment, or 0
// when neither parent is found.
func commentParentNumber(st *store.Store, c *store.Comment) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	switch c.ParentType {
	case "issue":
		if i, ok := st.Issues[c.IssueID]; ok {
			return i.Number
		}
	case "pull_request":
		if pr, ok := st.PullRequests[c.IssueID]; ok {
			return pr.Number
		}
	}
	return 0
}

// --- Issue / PR lock + unlock ---

func (s *Server) handleLockIssue(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo, numStr := r.PathValue("owner"), r.PathValue("number")
	repoObj := s.store.GetRepo(repo, r.PathValue("repo"))
	if repoObj == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		LockReason string `json:"lock_reason"`
	}
	// Body is optional; still reject a present-but-malformed one so a real
	// lock_reason is never silently dropped.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if req.LockReason != "" && !validLockReasons[req.LockReason] {
		writeGHError(w, http.StatusUnprocessableEntity, "Invalid lock reason")
		return
	}

	if !s.store.SetIssueOrPRLock(repoObj.ID, num, true, req.LockReason) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// RecordIssueOrPREvent stamps ParentType by number, so a PR lock lands in
	// the PR timeline.
	s.store.RecordIssueOrPREvent(repoObj.ID, num, user.ID, "locked", map[string]interface{}{"lock_reason": req.LockReason})
	s.emitLockAction(repoObj, num, user, true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnlockIssue(w http.ResponseWriter, r *http.Request, numberStr string) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repoObj := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repoObj == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(numberStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.SetIssueOrPRLock(repoObj.ID, num, false, "") {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.RecordIssueOrPREvent(repoObj.ID, num, user.ID, "unlocked", map[string]interface{}{})
	s.emitLockAction(repoObj, num, user, false)
	w.WriteHeader(http.StatusNoContent)
}
