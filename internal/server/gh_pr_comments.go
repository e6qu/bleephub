package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// PR review comments (inline / file-line / range).
// Endpoints:
//   POST   /repos/{o}/{r}/pulls/{number}/comments
//   GET    /repos/{o}/{r}/pulls/{number}/comments
//   GET    /repos/{o}/{r}/pulls/comments/{id}
//   PATCH  /repos/{o}/{r}/pulls/comments/{id}
//   DELETE /repos/{o}/{r}/pulls/comments/{id}
//   POST   /repos/{o}/{r}/pulls/{number}/comments/{id}/replies
//
// gh CLI's `gh pr review --thread` / `gh pr comment` uses GraphQL mutations
// (resolveReviewThread / unresolveReviewThread); the REST surface here is
// what octokit + probot use.
//
// PR review comments distinct from issue comments (`/issues/{n}/comments`):
// review comments attach to a specific file path + line + commit SHA and
// participate in review threads.

func (s *Server) registerGHPRCommentsRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/comments",
		s.handleListRepoPRComments)
	// `/pulls/{number}/comments` (3 segments, literal "comments" at pos 3)
	s.route("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/comments",
		s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleCreatePRComment))
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{number}/comments",
		s.handleListPRComments)
	s.route("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/comments/{comment_id}/replies",
		s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleReplyPRComment))

	// `/pulls/{number}/reviews/{review_id}/comments` (4 segments; no clash
	// with the 3-segment dispatch in gh_reactions.go)
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews/{review_id}/comments",
		s.handleListPRReviewCommentsForReview)

	// `/pulls/comments/{comment_id}` — single-review-comment surface.
	// Go 1.22's mux can't register both `/pulls/comments/{cid}` and
	// `/pulls/{number}/comments` because /pulls/comments/comments would match
	// both. Resolve via a 2-segment dispatcher (literal-anchored at p1=="comments"
	// when comment subpath is intended). The existing /pulls/{number}/<literal>
	// routes are strictly more specific (literal at pos 3) and continue to win
	// for their URLs.
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}",
		s.handlePRCommentTwoSegDispatch("GET"))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}",
		s.handlePRCommentTwoSegDispatch("PATCH"))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}",
		s.handlePRCommentTwoSegDispatch("DELETE"))
}

func (s *Server) handleListRepoPRComments(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	prs := s.store.ListPullRequests(repo.ID, "all")
	type row struct {
		comment *store.PRReviewComment
		pr      *store.PullRequest
	}
	var rows []row
	for _, pr := range prs {
		for _, comment := range s.store.PRReviewComments.ListForPR(pr.ID) {
			rows = append(rows, row{comment: comment, pr: pr})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].comment.ID < rows[j].comment.ID })
	rows, ok := filterSince(w, r, "PullRequestReviewComment", rows, func(item row) time.Time {
		return item.comment.UpdatedAt
	})
	if !ok {
		return
	}
	page := paginateAndLink(w, r, rows)
	out := make([]map[string]interface{}, 0, len(page))
	for _, item := range page {
		out = append(out, prReviewCommentToJSON(item.comment, s.store, s.baseURL(r), repo, item.pr))
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePRCommentTwoSegDispatch routes `/pulls/{p1}/{p2}` when p1=="comments"
// (single-review-comment surface) or p2=="files" (the changed-file diff list).
// For any other shape it 404s — the existing literal routes (e.g.
// /pulls/{number}/merge) win on their own paths.
func (s *Server) handlePRCommentTwoSegDispatch(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p1 := r.PathValue("p1")
		if method == "GET" && r.PathValue("p2") == "files" && p1 != "comments" {
			r.SetPathValue("number", p1)
			s.handleListPullRequestFiles(w, r)
			return
		}
		if p1 != "comments" {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		r.SetPathValue("comment_id", r.PathValue("p2"))
		switch method {
		case "GET":
			s.handleGetPRComment(w, r)
		case "PATCH":
			s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleUpdatePRComment)(w, r)
		case "DELETE":
			s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleDeletePRComment)(w, r)
		}
	}
}

func (s *Server) handleCreatePRComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Body      string  `json:"body"`
		CommitID  string  `json:"commit_id"`
		Path      string  `json:"path"`
		Line      flexInt `json:"line"`
		StartLine flexInt `json:"start_line"`
		Side      string  `json:"side"`
		InReplyTo flexInt `json:"in_reply_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		store.WriteGHValidationError(w, "PullRequestReviewComment", "body", "missing_field")
		return
	}
	var c *store.PRReviewComment
	if int(req.InReplyTo) > 0 {
		c = s.store.PRReviewComments.Reply(pr.ID, int(req.InReplyTo), user.ID, req.Body)
		if c == nil {
			writeGHError(w, http.StatusNotFound, "Reply target not found")
			return
		}
	} else {
		if req.Path == "" {
			store.WriteGHValidationError(w, "PullRequestReviewComment", "path", "missing_field")
			return
		}
		c = s.store.PRReviewComments.CreateRootComment(pr.ID, user.ID, req.Path, req.Body, req.CommitID, req.Side, int(req.Line), int(req.StartLine))
	}
	s.emitWebhookEvent(repo.FullName, "pull_request_review_comment", "created",
		buildPRReviewCommentEventPayload(repo, pr, c, user, "created"))
	prCommentJSON := prReviewCommentToJSON(c, s.store, s.baseURL(r), repo, pr)
	writeJSONCreated(w, jsonStringField(prCommentJSON, "url"), prCommentJSON)
}

func (s *Server) handleListPRComments(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	comments := s.store.PRReviewComments.ListForPR(pr.ID)
	comments, ok := filterSince(w, r, "PullRequestReviewComment", comments, func(comment *store.PRReviewComment) time.Time {
		return comment.UpdatedAt
	})
	if !ok {
		return
	}
	page := paginateAndLink(w, r, comments)
	out := make([]map[string]interface{}, 0, len(page))
	for _, c := range page {
		out = append(out, prReviewCommentToJSON(c, s.store, s.baseURL(r), repo, pr))
	}
	writeJSON(w, http.StatusOK, out)
}

// prReviewCommentInRepo resolves the review comment named by {comment_id} and
// the pull request that owns it, answering 404 unless that pull request lives
// in repo. Review comment ids are global, so every by-id handler has to walk
// back to the repository before it acts.
func (s *Server) prReviewCommentInRepo(w http.ResponseWriter, r *http.Request, repo *store.Repo) (*store.PRReviewComment, *store.PullRequest) {
	id, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	c := s.store.PRReviewComments.Get(id)
	if c == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	pr := s.store.GetPullRequest(c.PullRequestID)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	if !requireRepoOwns(w, repo, pr.RepoID) {
		return nil, nil
	}
	return c, pr
}

func (s *Server) handleGetPRComment(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	c, pr := s.prReviewCommentInRepo(w, r, repo)
	if c == nil {
		return
	}
	writeJSON(w, http.StatusOK, prReviewCommentToJSON(c, s.store, s.baseURL(r), repo, pr))
}

func (s *Server) handleUpdatePRComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c, pr := s.prReviewCommentInRepo(w, r, repo)
	if c == nil {
		return
	}
	if c.AuthorID != user.ID && !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to edit another user's comment")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !s.store.PRReviewComments.Update(c.ID, req.Body) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, prReviewCommentToJSON(s.store.PRReviewComments.Get(c.ID), s.store, s.baseURL(r), repo, pr))
}

func (s *Server) handleDeletePRComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c, _ := s.prReviewCommentInRepo(w, r, repo)
	if c == nil {
		return
	}
	if c.AuthorID != user.ID && !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to delete another user's comment")
		return
	}
	if !s.store.PRReviewComments.Delete(c.ID, s.store.Reactions) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReplyPRComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rootID, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		store.WriteGHValidationError(w, "PullRequestReviewComment", "body", "missing_field")
		return
	}
	c := s.store.PRReviewComments.Reply(pr.ID, rootID, user.ID, req.Body)
	if c == nil {
		writeGHError(w, http.StatusNotFound, "Reply target not found")
		return
	}
	replyJSON := prReviewCommentToJSON(c, s.store, s.baseURL(r), repo, pr)
	writeJSONCreated(w, jsonStringField(replyJSON, "url"), replyJSON)
}

// handleListPRReviewCommentsForReview serves
// GET /repos/{owner}/{repo}/pulls/{number}/reviews/{review_id}/comments —
// the review comments that belong to one submitted pull request review.
func (s *Server) handleListPRReviewCommentsForReview(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	reviewID, err := strconv.Atoi(r.PathValue("review_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	review := s.store.GetPullRequestReview(reviewID)
	if review == nil || review.PRID != pr.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := []map[string]interface{}{}
	for _, c := range s.store.PRReviewComments.ListForPR(pr.ID) {
		if c.ReviewID != reviewID {
			continue
		}
		out = append(out, prReviewCommentToJSON(c, s.store, s.baseURL(r), repo, pr))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func prReviewCommentToJSON(c *store.PRReviewComment, st *store.Store, baseURL string, repo *store.Repo, pr *store.PullRequest) map[string]interface{} {
	if c == nil {
		return nil
	}
	var author map[string]interface{}
	st.Mu.RLock()
	if u := st.Users[c.AuthorID]; u != nil {
		author = store.UserToJSON(u)
	}
	st.Mu.RUnlock()
	reactions := st.Reactions.SummarizeReactions("pull_request_review_comment", c.ID)
	reactions["url"] = fmt.Sprintf("%s/api/v3/repos/%s/pulls/comments/%d/reactions", baseURL, repo.FullName, c.ID)
	out := map[string]interface{}{
		"id":                     c.ID,
		"node_id":                c.NodeID,
		"url":                    fmt.Sprintf("%s/api/v3/repos/%s/pulls/comments/%d", baseURL, repo.FullName, c.ID),
		"pull_request_review_id": c.ReviewID,
		"diff_hunk":              c.DiffHunk,
		"path":                   c.Path,
		"position":               c.Position,
		"original_position":      c.OriginalPosition,
		"line":                   c.Line,
		"original_line":          c.OriginalLine,
		"start_line":             c.StartLine,
		"original_start_line":    c.OriginalStartLine,
		"side":                   c.Side,
		"start_side":             nullIfEmpty(c.StartSide),
		"commit_id":              c.CommitID,
		"original_commit_id":     c.OriginalCommitID,
		"body":                   c.Body,
		"user":                   author,
		"created_at":             c.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":             c.UpdatedAt.UTC().Format(time.RFC3339),
		"reactions":              reactions,
		"author_association":     store.AuthorAssociation(st, c.AuthorID, repo),
	}
	if c.InReplyToID > 0 {
		out["in_reply_to_id"] = c.InReplyToID
	}
	if pr != nil {
		out["pull_request_url"] = fmt.Sprintf("%s/api/v3/repos/%s/pulls/%d", baseURL, repo.FullName, pr.Number)
		out["html_url"] = fmt.Sprintf("%s/%s/pull/%d#discussion_r%d", baseURL, repo.FullName, pr.Number, c.ID)
		out["_links"] = map[string]interface{}{
			"self":         map[string]interface{}{"href": out["url"]},
			"html":         map[string]interface{}{"href": out["html_url"]},
			"pull_request": map[string]interface{}{"href": out["pull_request_url"]},
		}
	}
	return out
}

func buildPRReviewCommentEventPayload(repo *store.Repo, pr *store.PullRequest, c *store.PRReviewComment, sender *store.User, action string) map[string]interface{} {
	return attachInstallationBlock(map[string]interface{}{
		"action":  action,
		"comment": map[string]interface{}{"id": c.ID, "body": c.Body, "path": c.Path},
		"pull_request": map[string]interface{}{
			"number": pr.Number,
			"title":  pr.Title,
			"state":  pr.State,
		},
		"repository": repoPayload(repo),
		"sender":     senderPayload(sender),
	}, nil)
}
