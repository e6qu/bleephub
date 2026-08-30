package bleephub

import (
	"net/http"
	"slices"
	"sort"
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
	rows := make([]crossRepoIssueRow, 0)
	for _, issue := range s.store.Issues {
		repo := s.store.Repos[issue.RepoID]
		if repo == nil {
			continue
		}
		if !issueMatchesUserFilter(s.store, issue, repo, user, filter) {
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
		})
	}
	for _, pr := range s.store.PullRequests {
		repo := s.store.Repos[pr.RepoID]
		if repo == nil {
			continue
		}
		if !pullMatchesUserFilter(s.store, pr, repo, user, filter) {
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
// store read lock.
func issueMatchesUserFilter(st *store.Store, issue *store.Issue, repo *store.Repo, user *store.User, filter string) bool {
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
		return issueMentionsUser(st, issue, user)
	case "subscribed":
		// Issues in repos the user watches, plus issues the user participates in.
		if _, watching := st.RepoSubscriptions[store.RepoSubscriptionKey(user.ID, repo.ID)]; watching {
			return true
		}
		return assigned || created || issueMentionsUser(st, issue, user)
	case "repos":
		return repo.OwnerID == user.ID || store.RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, user.Login, "pull")
	case "all":
		return assigned || created || issueMentionsUser(st, issue, user)
	}
	return false
}

// issueMentionsUser reports whether the issue body or a comment mentions @user.
// Caller holds the store read lock.
func issueMentionsUser(st *store.Store, issue *store.Issue, user *store.User) bool {
	mention := "@" + user.Login
	if strings.Contains(issue.Body, mention) {
		return true
	}
	for _, c := range st.Comments {
		if c.ParentType == "issue" && c.IssueID == issue.ID && strings.Contains(c.Body, mention) {
			return true
		}
	}
	return false
}
