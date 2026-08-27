package bleephub

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Commit Comments API.

func (s *Server) registerGHCommitCommentsRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/comments", s.handleListRepoCommitComments)
	s.route("GET /api/v3/repos/{owner}/{repo}/commits/{commit_sha}/comments", s.handleListCommitComments)
	s.route("POST /api/v3/repos/{owner}/{repo}/commits/{commit_sha}/comments",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleCreateCommitComment))
	s.route("GET /api/v3/repos/{owner}/{repo}/comments/{comment_id}", s.handleGetCommitComment)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/comments/{comment_id}", s.handleUpdateCommitComment)
	s.route("DELETE /api/v3/repos/{owner}/{repo}/comments/{comment_id}", s.handleDeleteCommitComment)
}

func (s *Server) handleListRepoCommitComments(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	comments := s.store.CommitComments.ListForRepo(repo.ID)
	comments, ok := filterSince(w, r, "CommitComment", comments, func(comment *store.CommitComment) time.Time {
		return comment.UpdatedAt
	})
	if !ok {
		return
	}
	page := paginateAndLink(w, r, comments)
	out := make([]map[string]interface{}, 0, len(page))
	for _, c := range page {
		out = append(out, commitCommentToJSON(c, s.store, s.baseURL(r), repo))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListCommitComments(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	sha := r.PathValue("commit_sha")
	comments := s.store.CommitComments.ListForCommit(repo.ID, sha)
	comments, ok := filterSince(w, r, "CommitComment", comments, func(comment *store.CommitComment) time.Time {
		return comment.UpdatedAt
	})
	if !ok {
		return
	}
	page := paginateAndLink(w, r, comments)
	out := make([]map[string]interface{}, 0, len(page))
	for _, c := range page {
		out = append(out, commitCommentToJSON(c, s.store, s.baseURL(r), repo))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateCommitComment(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		Body     string `json:"body"`
		Path     string `json:"path"`
		Position *int   `json:"position"`
		Line     *int   `json:"line"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Body == "" {
		store.WriteGHValidationError(w, "CommitComment", "body", "missing_field")
		return
	}
	sha := r.PathValue("commit_sha")
	c := s.store.CommitComments.Create(repo.ID, sha, user.ID, req.Body, req.Path, req.Position, req.Line)
	commitCommentJSON := commitCommentToJSON(c, s.store, s.baseURL(r), repo)
	s.emitWebhookEvent(repo.FullName, "commit_comment", "created", map[string]interface{}{
		"action":     "created",
		"comment":    commitCommentJSON,
		"repository": repoPayload(repo, s.baseURL(r)),
		"sender":     store.UserToJSON(user, s.baseURL(r)),
	})
	writeJSONCreated(w, jsonStringField(commitCommentJSON, "url"), commitCommentJSON)
}

func (s *Server) handleGetCommitComment(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.store.CommitComments.Get(id)
	if c == nil || c.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, commitCommentToJSON(c, s.store, s.baseURL(r), repo))
}

func (s *Server) handleUpdateCommitComment(w http.ResponseWriter, r *http.Request) {
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
	id, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	c := s.store.CommitComments.Get(id)
	if c == nil || c.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if c.AuthorID != user.ID && !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeContents, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusForbidden, "Must have push access")
		return
	}
	if !s.store.CommitComments.Update(id, req.Body) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c = s.store.CommitComments.Get(id)
	writeJSON(w, http.StatusOK, commitCommentToJSON(c, s.store, s.baseURL(r), repo))
}

func (s *Server) handleDeleteCommitComment(w http.ResponseWriter, r *http.Request) {
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
	id, err := strconv.Atoi(r.PathValue("comment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.store.CommitComments.Get(id)
	if c == nil || c.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if c.AuthorID != user.ID && !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeContents, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusForbidden, "Must have push access")
		return
	}
	if !s.store.CommitComments.Delete(id, s.store.Reactions) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func commitCommentToJSON(c *store.CommitComment, st *store.Store, baseURL string, repo *store.Repo) map[string]interface{} {
	if c == nil {
		return nil
	}
	var author map[string]interface{}
	st.Mu.RLock()
	if u := st.Users[c.AuthorID]; u != nil {
		author = store.UserToJSON(u, baseURL)
	}
	st.Mu.RUnlock()
	out := map[string]interface{}{
		"id":                 c.ID,
		"node_id":            c.NodeID,
		"body":               c.Body,
		"commit_id":          c.CommitID,
		"user":               author,
		"created_at":         c.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":         c.UpdatedAt.UTC().Format(time.RFC3339),
		"url":                fmt.Sprintf("%s/api/v3/repos/%s/comments/%d", baseURL, repo.FullName, c.ID),
		"html_url":           fmt.Sprintf("%s/%s/commit/%s#commitcomment-%d", baseURL, repo.FullName, c.CommitID, c.ID),
		"author_association": store.AuthorAssociation(st, c.AuthorID, repo),
	}
	// path/position/line are required-but-nullable: emit them unconditionally,
	// null for a commit-level comment.
	if c.Path != "" {
		out["path"] = c.Path
	} else {
		out["path"] = nil
	}
	out["position"] = c.Position
	out["line"] = c.Line
	return out
}
