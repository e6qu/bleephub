package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Reactions API. GitHub exposes reactions on issues, issue comments, PR review
// comments, commit comments, releases, and discussions, with eight content
// values (+1, -1, laugh, confused, heart, hooray, rocket, eyes).

func (s *Server) registerGHReactionsRoutes() {
	// Issue-reaction GET and DELETE routes are dispatched from
	// registerGHIssueRoutes because Go's mux cannot disambiguate the literal
	// "comments" segment from the {number} wildcard at the same depth.
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{number}/reactions",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateReaction("issue", "number")))

	// Issue-comment reaction DELETE has four segments after /issues, so it does
	// not collide with the three-segment issue dispatch.
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateReaction("issue_comment", "comment_id")))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions/{reaction_id}",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleDeleteReaction("issue_comment", "comment_id")))

	// PR review-comment reaction DELETE: four segments after /pulls, no collision
	// with the three-segment pulls dispatch below.
	s.route("DELETE /api/v3/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions/{reaction_id}",
		s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleDeleteReaction("pull_request_review_comment", "comment_id")))

	// The 3-segment /pulls/comments/{id}/reactions routes conflict with the PR
	// review routes under Go's mux, so they go through handlePullsThreeSegDispatch.
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}/{p3}", s.handlePullsThreeSegDispatch("GET"))
	s.route("POST /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}/{p3}", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handlePullsThreeSegDispatch("POST")))
	s.route("PUT /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}/{p3}", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handlePullsThreeSegDispatch("PUT")))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/pulls/{p1}/{p2}/{p3}", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handlePullsThreeSegDispatch("DELETE")))

	s.route("POST /api/v3/repos/{owner}/{repo}/comments/{comment_id}/reactions",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleCreateReaction("commit_comment", "comment_id")))
	s.route("GET /api/v3/repos/{owner}/{repo}/comments/{comment_id}/reactions",
		s.handleListReactions("commit_comment", "comment_id"))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/comments/{comment_id}/reactions/{reaction_id}",
		s.requirePerm(store.ScopeContents, store.PermWrite, s.handleDeleteReaction("commit_comment", "comment_id")))

	// Release reactions register via the dispatcher in gh_releases.go, which
	// disambiguates /releases/tags/{tag} from /releases/{release_id}/reactions
	// by segment-2.
}

// resolveReactionParent converts the reaction path parameter into the store
// (parentType, parentID) pair. Issue numbers are unique only within a repo, so
// they resolve through the repository; a number that resolves to a PR is keyed
// as "pull_request" (issue and PR ids come from independent counters). Writes
// the error and returns false when the repository or parent does not exist.
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

	// Every reaction parent is scoped to the repository named in the path.
	// Comments/reviews/releases are stored by global id alone, so a parent not
	// re-resolved to its repository here would let a caller who can write any
	// repository react on — and read the reactions of — resources in someone
	// else's private repository.
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
		// Every reaction parent here is repo-scoped; enforce the owner/org block
		// and interaction limit the same as any other content creation. A future
		// non-repo reaction (a team discussion) has no {owner}/{repo} path and is
		// left untouched.
		if repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo")); repo != nil {
			if s.rejectIfInteractionLimited(w, user, repo) {
				return
			}
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
