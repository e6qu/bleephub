package bleephub

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Browser-only discussion operations. Real GitHub exposes discussions over
// GraphQL and the web UI only — converting an issue to a discussion and
// pinning discussions have no REST operations at all — so these live under
// /ui-data rather than an invented /api/v3 path (`s.route` auto-wraps /ui-data
// with authenticateUIData).
//
//	POST /ui-data/repos/{owner}/{repo}/issues/{number}/convert-to-discussion
//	  body {"category_id": int} — creates a discussion in that category
//	  carrying the issue's title/body/author and its conversation comments
//	  (original authors + timestamps), closes the issue as not_planned with a
//	  "converted_to_discussion" timeline event, and returns the new
//	  discussion. Requires issue-write on the repository, mirroring how
//	  closing an issue is gated (requirePerm(issues, write) on PATCH
//	  /issues/{number}).
//
//	GET /ui-data/repos/{owner}/{repo}/discussions/pinned
//	  the repo's ordered pinned discussions (any viewer who can read the repo).
//	PUT /ui-data/repos/{owner}/{repo}/discussions/pinned
//	  body {"numbers": [int]} — replaces the ordered pin list (≤ 4, GitHub's
//	  cap). Requires discussion-write on the repository.
func (s *Server) registerGHDiscussionsUIDataRoutes() {
	s.route("POST /ui-data/repos/{owner}/{repo}/issues/{number}/convert-to-discussion", s.handleUIConvertIssueToDiscussion)
	s.route("GET /ui-data/repos/{owner}/{repo}/discussions/pinned", s.handleUIListPinnedDiscussions)
	s.route("PUT /ui-data/repos/{owner}/{repo}/discussions/pinned", s.handleUISetPinnedDiscussions)
}

// uiDiscussionJSON renders a discussion for the /ui-data surface: the fields
// the web UI's discussion list/detail views consume, in REST-style snake_case.
func (s *Server) uiDiscussionJSON(d *store.Discussion, repo *store.Repo, baseURL string) map[string]interface{} {
	var author map[string]interface{}
	s.store.Mu.RLock()
	if u := s.store.Users[d.AuthorID]; u != nil {
		author = store.UserToJSON(u, baseURL)
	}
	comments := 0
	for _, c := range s.store.DiscussionComments {
		if c.DiscussionID == d.ID && !c.Deleted {
			comments++
		}
	}
	s.store.Mu.RUnlock()

	var category map[string]interface{}
	if cat := s.store.GetDiscussionCategory(d.CategoryID); cat != nil {
		category = map[string]interface{}{
			"id":            cat.ID,
			"node_id":       cat.NodeID,
			"name":          cat.Name,
			"emoji":         cat.Emoji,
			"description":   cat.Description,
			"is_answerable": cat.IsAnswerable,
		}
	}
	return map[string]interface{}{
		"id":         d.ID,
		"node_id":    d.NodeID,
		"number":     d.Number,
		"title":      d.Title,
		"body":       d.Body,
		"user":       author,
		"category":   category,
		"locked":     d.Locked,
		"comments":   comments,
		"upvotes":    len(d.UpvoterIDs),
		"html_url":   fmt.Sprintf("%s/%s/discussions/%d", baseURL, repo.FullName, d.Number),
		"created_at": d.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": d.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// handleUIConvertIssueToDiscussion implements the web-only "convert issue to
// discussion" action.
func (s *Server) handleUIConvertIssueToDiscussion(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	// Closing an issue is gated at issues:write (the standalone PATCH
	// /issues/{number} route runs behind requirePerm(issues, write)); the
	// conversion closes the issue, so it demands the same standing. A viewer
	// without it gets the same 404 the resource gate answers elsewhere.
	if !s.viewerHasRepoPermission(r.Context(), repo, store.ScopeIssues, store.PermWrite) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// GetIssueByNumber resolves issues only, so a PR number 404s — pull
	// requests cannot be converted to discussions on real GitHub either.
	issue := s.store.GetIssueByNumber(repo.ID, num)
	if issue == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !store.RepoHasDiscussions(repo) {
		store.WriteGHValidationError(w, "Discussion", "repository", "invalid")
		return
	}

	var req struct {
		CategoryID flexInt `json:"category_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	cat := s.store.GetDiscussionCategory(int(req.CategoryID))
	if cat == nil || cat.RepoID != repo.ID {
		store.WriteGHValidationError(w, "Discussion", "category_id", "invalid")
		return
	}

	// The discussion carries the issue's title/body/author; its comments carry
	// the conversation's original authors and timestamps.
	d := s.store.CreateDiscussion(repo.ID, cat.ID, issue.AuthorID, issue.Title, issue.Body)
	for _, c := range s.store.ListCommentsFor("issue", issue.ID) {
		s.store.CreateDiscussionCommentAt(d.ID, c.AuthorID, c.Body, 0, c.CreatedAt)
	}

	// Close the issue as not_planned (as github.com does on conversion) and
	// record the conversion on the timeline. "converted_to_discussion" is
	// GitHub's own timeline event name for this transition; the timeline
	// renderer passes unknown-to-it event names through with actor and
	// timestamp, which is exactly the distinguishable close note needed here.
	wasOpen := issue.State != "CLOSED"
	s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
		if i.State != "CLOSED" {
			i.State = "CLOSED"
			now := time.Now()
			i.ClosedAt = &now
		}
		i.StateReason = "NOT_PLANNED"
	})
	s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "converted_to_discussion", nil)
	if wasOpen {
		if updated := s.store.GetIssue(issue.ID); updated != nil {
			s.emitWebhookEvent(repo.FullName, "issues", "closed", buildIssuesPayload(s.store, repo, updated, user, "closed", s.baseURL(r)))
		}
	}

	writeJSON(w, http.StatusCreated, s.uiDiscussionJSON(d, repo, s.baseURL(r)))
}

// handleUIListPinnedDiscussions returns the repo's ordered pinned discussions.
func (s *Server) handleUIListPinnedDiscussions(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	out := make([]map[string]interface{}, 0, store.MaxPinnedDiscussions)
	for _, id := range s.store.ListPinnedDiscussions(repo.ID) {
		if d := s.store.GetDiscussion(id); d != nil {
			out = append(out, s.uiDiscussionJSON(d, repo, s.baseURL(r)))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUISetPinnedDiscussions replaces the repo's ordered pin list.
func (s *Server) handleUISetPinnedDiscussions(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	if !s.viewerHasRepoPermission(r.Context(), repo, store.ScopeDiscussions, store.PermWrite) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Numbers []int `json:"numbers"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.Numbers) > store.MaxPinnedDiscussions {
		store.WriteGHValidationError(w, "PinnedDiscussions", "numbers", "too_many")
		return
	}
	ids := make([]int, 0, len(req.Numbers))
	seen := map[int]bool{}
	for _, number := range req.Numbers {
		d := s.store.GetDiscussionByNumber(repo.ID, number)
		if d == nil {
			store.WriteGHValidationError(w, "PinnedDiscussions", "numbers", "invalid")
			return
		}
		if seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		ids = append(ids, d.ID)
	}
	s.store.SetPinnedDiscussions(repo.ID, ids)
	s.handleUIListPinnedDiscussions(w, r)
}
