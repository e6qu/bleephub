package bleephub

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// GET /issues — issues involving the authenticated user across every repo
// the user can see.

func (s *Server) registerGHUserIssuesRoutes() {
	s.route("GET /api/v3/issues", s.handleListGlobalUserIssues)
}

func (s *Server) handleListGlobalUserIssues(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	q := r.URL.Query()

	filter := q.Get("filter")
	if filter == "" {
		filter = "assigned"
	}
	switch filter {
	case "assigned", "created", "mentioned", "subscribed", "repos", "all":
	default:
		store.WriteGHValidationError(w, "Issue", "filter", "invalid")
		return
	}
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	switch state {
	case "open", "closed", "all":
	default:
		store.WriteGHValidationError(w, "Issue", "state", "invalid")
		return
	}
	var labelFilter []string
	if v := q.Get("labels"); v != "" {
		labelFilter = strings.Split(v, ",")
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			store.WriteGHValidationError(w, "Issue", "since", "invalid")
			return
		}
		since = t
	}

	// Every pull request is an issue on GitHub, so this cross-repository listing
	// interleaves issues and their pull requests.
	s.store.Mu.RLock()
	// The mention-derived filters would otherwise re-scan every comment per row
	// (O(issues×comments)). Precompute the mentioned-parent set in one pass, as
	// the notifications path does, so the per-row test is O(1).
	var mentions map[string]bool
	switch filter {
	case "mentioned", "subscribed", "all":
		mentions = buildCommentMentionIndex(s.store, user.Login)
	}
	rows := make([]crossRepoIssueRow, 0)
	for _, issue := range s.store.Issues {
		repo := s.store.Repos[issue.RepoID]
		if repo == nil {
			continue
		}
		if !issueMatchesUserFilter(s.store, issue, repo, user, filter, mentions) {
			continue
		}
		if state != "all" && !strings.EqualFold(issue.State, state) {
			continue
		}
		if len(labelFilter) > 0 && !issueHasLabelNames(s.store, issue, labelFilter) {
			continue
		}
		if !since.IsZero() && issue.UpdatedAt.Before(since) {
			continue
		}
		rows = append(rows, crossRepoIssueRow{
			number: issue.Number, commentCount: s.store.CountCommentsForLocked("issue", issue.ID),
			issue: issue, repo: repo,
			createdAtVal: issue.CreatedAt, updatedAtVal: issue.UpdatedAt,
		})
	}
	for _, pr := range s.store.PullRequests {
		repo := s.store.Repos[pr.RepoID]
		if repo == nil {
			continue
		}
		if !pullMatchesUserFilter(s.store, pr, repo, user, filter, mentions) {
			continue
		}
		if !prMatchesStateFilter(pr, state) {
			continue
		}
		if len(labelFilter) > 0 && !labelIDsHaveNames(s.store, pr.LabelIDs, labelFilter) {
			continue
		}
		if !since.IsZero() && pr.UpdatedAt.Before(since) {
			continue
		}
		rows = append(rows, crossRepoIssueRow{
			number: pr.Number, commentCount: s.store.CountCommentsForLocked("pull_request", pr.ID),
			pr: pr, repo: repo,
			createdAtVal: pr.CreatedAt, updatedAtVal: pr.UpdatedAt,
		})
	}
	s.store.Mu.RUnlock()

	rows = slices.DeleteFunc(rows, func(candidate crossRepoIssueRow) bool {
		return !s.viewerCanReadRepo(r.Context(), candidate.repo)
	})

	less := crossRepoRowLess(q.Get("sort"), q.Get("direction") == "asc")
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })

	s.renderCrossRepoIssueRows(w, r, s.baseURL(r), rows)
}

// issueMatchesUserFilter implements the `filter` values. Caller holds the
// store read lock. mentions is the precomputed mentioned-parent set (nil for
// filters that don't consult mentions).
func issueMatchesUserFilter(st *store.Store, issue *store.Issue, repo *store.Repo, user *store.User, filter string, mentions map[string]bool) bool {
	assigned := false
	for _, id := range issue.AssigneeIDs {
		if id == user.ID {
			assigned = true
			break
		}
	}
	created := issue.AuthorID == user.ID
	switch filter {
	case "assigned":
		return assigned
	case "created":
		return created
	case "mentioned":
		return issueMentionsUser(issue, user, mentions)
	case "subscribed":
		// Issues in repos the user watches, plus issues the user participates in.
		if _, watching := st.RepoSubscriptions[store.RepoSubscriptionKey(user.ID, repo.ID)]; watching {
			return true
		}
		return assigned || created || issueMentionsUser(issue, user, mentions)
	case "repos":
		return repo.OwnerID == user.ID || store.RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, user.Login, "pull")
	case "all":
		return assigned || created || issueMentionsUser(issue, user, mentions)
	}
	return false
}

// buildCommentMentionIndex records, in one pass over st.Comments, which
// (parentType, parentID) conversations @-mention login. Callers hold the store
// read lock and test membership in O(1) instead of rescanning every comment per
// issue/PR. Mirrors the precompute in store.NotificationRowsFor.
func buildCommentMentionIndex(st *store.Store, login string) map[string]bool {
	mention := "@" + login
	idx := make(map[string]bool)
	for _, c := range st.Comments {
		if strings.Contains(c.Body, mention) {
			idx[commentMentionKey(c.ParentType, c.IssueID)] = true
		}
	}
	return idx
}

// commentMentionKey keys the mention index by a comment's parent.
func commentMentionKey(parentType string, parentID int) string {
	return parentType + "\x00" + strconv.Itoa(parentID)
}

// issueMentionsUser reports whether the issue body mentions @user or a comment
// on it does (via the precomputed mentions set).
func issueMentionsUser(issue *store.Issue, user *store.User, mentions map[string]bool) bool {
	if strings.Contains(issue.Body, "@"+user.Login) {
		return true
	}
	return mentions[commentMentionKey("issue", issue.ID)]
}
