package bleephub

import (
	"net/http"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Cross-repository issue listings — GET /issues, GET /user/issues, and
// GET /orgs/{org}/issues — return pull requests alongside issues. On GitHub
// every pull request is an issue, so a caller asking for "issues assigned to
// me" must also see their assigned pull requests, each carrying the
// `pull_request` member. These helpers give the three handlers one row model,
// one ordering, and one renderer.

type crossRepoIssueRow struct {
	number       int
	commentCount int
	issue        *store.Issue       // set for issue rows
	pr           *store.PullRequest // set for pull-request rows
	repo         *store.Repo
	// Sort keys captured under the store lock at construction. Reading
	// CreatedAt/UpdatedAt off the live issue/pr during the post-unlock sort would
	// race the in-place UpdatedAt writes in UpdateIssue/UpdatePullRequest.
	createdAtVal time.Time
	updatedAtVal time.Time
}

// crossRepoRowLess orders rows by the requested sort key, tie-broken by the
// shared per-repo number, honoring the direction. It reads the sort fields off
// whichever of issue/pr the row holds.
func crossRepoRowLess(sortKey string, ascending bool) func(a, b crossRepoIssueRow) bool {
	return func(a, b crossRepoIssueRow) bool {
		var less bool
		switch sortKey {
		case "updated":
			less = a.updatedAt().Before(b.updatedAt())
			if a.updatedAt().Equal(b.updatedAt()) {
				less = a.number < b.number
			}
		case "comments":
			less = a.commentCount < b.commentCount
			if a.commentCount == b.commentCount {
				less = a.number < b.number
			}
		default: // "created"
			less = a.createdAt().Before(b.createdAt())
			if a.createdAt().Equal(b.createdAt()) {
				less = a.number < b.number
			}
		}
		if ascending {
			return less
		}
		return !less
	}
}

func (r crossRepoIssueRow) createdAt() time.Time { return r.createdAtVal }

func (r crossRepoIssueRow) updatedAt() time.Time { return r.updatedAtVal }

// renderCrossRepoIssueRows paginates, then serializes each row — issues via
// issueToJSON, pull requests via issueToJSONForPR (which sets the pull_request
// member) — attaching the cross-repository `repository` object GitHub includes
// on these listings.
func (s *Server) renderCrossRepoIssueRows(w http.ResponseWriter, r *http.Request, base string, rows []crossRepoIssueRow) {
	page := paginateAndLink(w, r, rows)
	out := make([]map[string]interface{}, 0, len(page))
	for _, row := range page {
		var item map[string]interface{}
		if row.issue != nil {
			item = issueToJSON(row.issue, s.store, base, row.repo.FullName)
		} else {
			item = issueToJSONForPR(row.pr, s.store, base, row.repo.FullName)
		}
		item["repository"] = store.RepoToJSON(row.repo, s.store, base)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// issueMatchesStateFilter maps the lowercase `state` query value against an
// issue's uppercase stored state. An unrecognized value (including "all") keeps
// the row.
func issueMatchesStateFilter(issueState, state string) bool {
	switch state {
	case "open":
		return issueState == "OPEN"
	case "closed":
		return issueState == "CLOSED"
	default:
		return true
	}
}

// prMatchesStateFilter mirrors issueMatchesStateFilter for pull requests, where
// GitHub folds a merged PR into the "closed" state.
func prMatchesStateFilter(pr *store.PullRequest, state string) bool {
	switch state {
	case "open":
		return pr.State == "OPEN"
	case "closed":
		return pr.State == "CLOSED" || pr.State == "MERGED"
	default:
		return true
	}
}

// pullMatchesUserFilter is the pull-request twin of issueMatchesUserFilter,
// implementing the `filter` values for the cross-repository listings. Caller
// holds the store read lock. mentions is the precomputed mentioned-parent set.
func pullMatchesUserFilter(st *store.Store, pr *store.PullRequest, repo *store.Repo, user *store.User, filter string, mentions map[string]bool) bool {
	assigned := false
	for _, id := range pr.AssigneeIDs {
		if id == user.ID {
			assigned = true
			break
		}
	}
	created := pr.AuthorID == user.ID
	switch filter {
	case "assigned":
		return assigned
	case "created":
		return created
	case "mentioned":
		return pullMentionsUser(pr, user, mentions)
	case "subscribed":
		if _, watching := st.RepoSubscriptions[store.RepoSubscriptionKey(user.ID, repo.ID)]; watching {
			return true
		}
		return assigned || created || pullMentionsUser(pr, user, mentions)
	case "repos":
		return repo.OwnerID == user.ID || store.RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, user.Login, "pull")
	case "all":
		return assigned || created || pullMentionsUser(pr, user, mentions)
	}
	return false
}

// labelIDsHaveNames reports whether labelIDs cover every name. It reads the
// label table directly without locking, so a caller already holding
// s.store.Mu.RLock uses it in place of store.LabelIDsCoverNames, which takes
// the lock itself (a non-reentrant RWMutex would deadlock).
func labelIDsHaveNames(st *store.Store, labelIDs []int, names []string) bool {
	for _, name := range names {
		want := strings.TrimSpace(name)
		found := false
		for _, lid := range labelIDs {
			if l := st.Labels[lid]; l != nil && l.Name == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// pullMentionsUser reports whether the PR body mentions @user or a conversation
// comment does (via the precomputed mentions set).
func pullMentionsUser(pr *store.PullRequest, user *store.User, mentions map[string]bool) bool {
	if strings.Contains(pr.Body, "@"+user.Login) {
		return true
	}
	return mentions[commentMentionKey("pull_request", pr.ID)]
}
