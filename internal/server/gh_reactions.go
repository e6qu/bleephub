package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Reactions API.
// Real GitHub exposes reactions on issues, issue comments, PR review comments,
// commits/commit comments, releases, and discussions. Eight content values:
//   +1, -1, laugh, confused, heart, hooray, rocket, eyes.
//
// Endpoints (all live under /repos/{owner}/{repo}/...):
//   - issues/{number}/reactions               GET, POST, DELETE /{id}
//   - issues/comments/{comment_id}/reactions  GET, POST, DELETE /{id}
//   - pulls/comments/{comment_id}/reactions   GET, POST, DELETE /{id}
//   - comments/{comment_id}/reactions         GET, POST, DELETE /{id}  (commit comments)
//   - releases/{release_id}/reactions         GET, POST, DELETE /{id}
//
// Plus the user-level: DELETE /users/{username}/reactions/{id} (rarely used; skip).

// --- HTTP surface ---

func (s *Server) registerGHReactionsRoutes() {
	// Issue reactions. GET /issues/{number}/reactions,
	// GET /issues/comments/{comment_id}/reactions, and the corresponding
	// DELETE /.../reactions/{reaction_id} paths are dispatched from
	// registerGHIssueRoutes because Go's mux cannot disambiguate literal
	// segments (comments) from wildcard segments (number) at the same depth.
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{number}/reactions",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateReaction("issue", "number")))

	// Issue comment reactions. The DELETE path has four segments after
	// /issues, so it does not collide with the issue three-segment dispatch.
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateReaction("issue_comment", "comment_id")))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions/{reaction_id}",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleDeleteReaction("issue_comment", "comment_id")))

	// PR review-comment reaction deletion. Four segments after /pulls, so it
	// does not collide with the pulls three-segment dispatch below.
	s.route("DELETE /api/v3/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions/{reaction_id}",
		s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleDeleteReaction("pull_request_review_comment", "comment_id")))

	// PR review-comment reactions. The 3-segment GET/POST routes
	// (/pulls/comments/{comment_id}/reactions) conflict with the PR review
	// routes (/pulls/{number}/reviews/{review_id}) under Go 1.22's mux, so
	// they are dispatched via handlePullsThreeSegDispatch.
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}/{p3}", s.handlePullsThreeSegDispatch("GET"))
	s.route("POST /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}/{p3}", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handlePullsThreeSegDispatch("POST")))
	s.route("PUT /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}/{p3}", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handlePullsThreeSegDispatch("PUT")))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}/{p3}", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handlePullsThreeSegDispatch("DELETE")))

	// Commit comment reactions
	s.route("POST /api/v3/repos/{owner}/{repo}/comments/{comment_id}/reactions",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleCreateReaction("commit_comment", "comment_id")))
	s.route("GET /api/v3/repos/{owner}/{repo}/comments/{comment_id}/reactions",
		s.handleListReactions("commit_comment", "comment_id"))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/comments/{comment_id}/reactions/{reaction_id}",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleDeleteReaction("commit_comment", "comment_id")))

	// Release reactions — register via the disambiguation dispatcher in
	// gh_releases.go because `/releases/tags/{tag}` and
	// `/releases/{release_id}/reactions` are ambiguous to Go 1.22's mux.
	// The dispatcher routes by segment-2 ("tags" vs numeric release_id).
}

// resolveReactionParent converts the reaction path parameter into the
// store-level (parentType, parentID) pair. Issue reactions arrive keyed by
// issue *number*, which is only unique within one repository — they resolve
// through the repository to the issue's global ID so reactions never leak
// between repositories that happen to share issue numbers. Pull requests
// share the issue number space and are reactable on real GitHub via the same
// /issues/{number}/reactions surface, so a number that resolves to a PR is
// keyed under the "pull_request" parent type (issue and PR IDs come from
// independent counters and would otherwise collide). Writes the error
// response and returns false when the repository or parent does not exist.
func (s *Server) resolveReactionParent(w http.ResponseWriter, r *http.Request, parentType, pathParam string) (string, int, bool) {
	parentID, err := strconv.Atoi(r.PathValue(pathParam))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, fmt.Sprintf("invalid %s", pathParam))
		return "", 0, false
	}
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return "", 0, false
	}

	// Every reaction parent is scoped to the repository named in the path. The
	// store looks comments/reviews/releases up by global id alone, so without
	// re-resolving each parent back to its repository a caller who can write to
	// *any* repository could react on — and, through the list endpoint, read
	// the reactions and reacting users of — a comment or release that lives in
	// someone else's private repository. Issue reactions additionally arrive
	// keyed by the per-repo issue *number* (unique only within a repo) and are
	// re-keyed to the global issue/PR id here.
	switch parentType {
	case "issue":
		if issue := s.store.GetIssueByNumber(repo.ID, parentID); issue != nil {
			return "issue", issue.ID, true
		}
		if pr := s.store.GetPullRequestByNumber(repo.ID, parentID); pr != nil {
			return "pull_request", pr.ID, true
		}
	case "issue_comment":
		if c := s.store.GetComment(parentID); c != nil && s.store.CommentRepoID(c) == repo.ID {
			return parentType, parentID, true
		}
	case "commit_comment":
		if c := s.store.CommitComments.Get(parentID); c != nil && c.RepoID == repo.ID {
			return parentType, parentID, true
		}
	case "pull_request_review_comment":
		if c := s.store.PRReviewComments.Get(parentID); c != nil {
			if pr := s.store.GetPullRequest(c.PullRequestID); pr != nil && pr.RepoID == repo.ID {
				return parentType, parentID, true
			}
		}
	case "release":
		if rel := s.store.Releases.Get(parentID); rel != nil && rel.RepoID == repo.ID {
			return parentType, parentID, true
		}
	}

	writeGHError(w, http.StatusNotFound, "Not Found")
	return "", 0, false
}

func (s *Server) handleCreateReaction(parentType, pathParam string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := ghUserFromContext(r.Context())
		if user == nil {
			writeGHError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		effType, parentID, ok := s.resolveReactionParent(w, r, parentType, pathParam)
		if !ok {
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == "" {
			store.WriteGHValidationError(w, "Reaction", "content", "missing_field")
			return
		}
		reaction, alreadyExisted, err := s.store.Reactions.AddReaction(effType, parentID, user.ID, body.Content)
		if err != nil {
			store.WriteGHValidationError(w, "Reaction", "content", "invalid")
			return
		}
		status := http.StatusCreated
		if alreadyExisted {
			status = http.StatusOK
		}
		writeJSON(w, status, reactionToJSON(reaction, user, s.publicOrigin()))
	}
}

func (s *Server) handleListReactions(parentType, pathParam string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.enforceRepoReadable(w, r) {
			return
		}
		effType, parentID, ok := s.resolveReactionParent(w, r, parentType, pathParam)
		if !ok {
			return
		}
		contentFilter := r.URL.Query().Get("content")
		reactions := s.store.Reactions.ListReactions(effType, parentID, contentFilter)
		page := paginateAndLink(w, r, reactions)
		out := make([]map[string]interface{}, 0, len(page))
		for _, rx := range page {
			user := s.store.GetUserByID(rx.UserID)
			out = append(out, reactionToJSON(rx, user, s.publicOrigin()))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) handleDeleteReaction(parentType, pathParam string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := ghUserFromContext(r.Context())
		if user == nil {
			writeGHError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		effType, parentID, ok := s.resolveReactionParent(w, r, parentType, pathParam)
		if !ok {
			return
		}
		reactionID, err := strconv.Atoi(r.PathValue("reaction_id"))
		if err != nil {
			writeGHError(w, http.StatusBadRequest, "invalid reaction id")
			return
		}
		if !s.store.Reactions.DeleteReactionByUser(effType, parentID, reactionID, user.ID) {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func reactionToJSON(r *store.Reaction, user *store.User, baseURL string) map[string]interface{} {
	var userJSON map[string]interface{}
	if user != nil {
		userJSON = store.UserToJSON(user, baseURL)
	}
	return map[string]interface{}{
		"id":         r.ID,
		"node_id":    fmt.Sprintf("REA_kgDO%08d", r.ID),
		"content":    r.Content,
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339),
		"user":       userJSON,
	}
}
