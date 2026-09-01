package bleephub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

func (s *Server) registerGHSearchRoutes() {
	s.route("GET /api/v3/search/issues", s.handleSearchIssues)
	s.route("GET /api/v3/search/repositories", s.handleSearchRepositories)
	s.route("GET /api/v3/search/code", s.handleSearchCode)
	s.route("GET /api/v3/search/users", s.handleSearchUsers)
	s.route("GET /api/v3/search/commits", s.handleSearchCommits)
	s.route("GET /api/v3/search/labels", s.handleSearchLabels)
	s.route("GET /api/v3/search/topics", s.handleSearchTopics)
}

// searchAccessibleRepoIDs snapshots the repositories the credential may search.
// Search grants installation tokens reach without an Issues/PRs/Contents scope,
// but repository reach is still enforced, so private results stay within the
// token's selection. Must run outside a caller-held store lock:
// viewerCanReadRepo takes its own.
func (s *Server) searchAccessibleRepoIDs(ctx context.Context) map[int]struct{} {
	s.store.Mu.RLock()
	repositories := make([]*store.Repo, 0, len(s.store.Repos))
	for _, repo := range s.store.Repos {
		snapshot := *repo
		repositories = append(repositories, &snapshot)
	}
	s.store.Mu.RUnlock()

	accessible := make(map[int]struct{}, len(repositories))
	for _, repo := range repositories {
		if s.viewerCanReadRepo(ctx, repo) {
			accessible[repo.ID] = struct{}{}
		}
	}
	return accessible
}

// searchAccessibleRepoIDForScope resolves reachability for a single repo:-scoped
// search without cloning and access-checking every repository in the instance.
// Returns an empty set when the repo is unknown or unreadable.
func (s *Server) searchAccessibleRepoIDForScope(ctx context.Context, fullName string) map[int]struct{} {
	s.store.Mu.RLock()
	var snapshot *store.Repo
	if repo := s.store.RepoByNameLocked(fullName); repo != nil {
		clone := *repo
		snapshot = &clone
	}
	s.store.Mu.RUnlock()

	accessible := map[int]struct{}{}
	if snapshot != nil && s.viewerCanReadRepo(ctx, snapshot) {
		accessible[snapshot.ID] = struct{}{}
	}
	return accessible
}

// searchCandidateReposLocked returns the repositories a search must consider: the
// single repository named by a repo: qualifier (or none, if unknown) instead of
// every repo in the instance, else all repos. Callers hold the store read lock.
func (s *Server) searchCandidateReposLocked(repoQualifier string) []*store.Repo {
	if repoQualifier != "" {
		if repo := s.store.RepoByNameLocked(repoQualifier); repo != nil {
			return []*store.Repo{repo}
		}
		return nil
	}
	out := make([]*store.Repo, 0, len(s.store.Repos))
	for _, repo := range s.store.Repos {
		out = append(out, repo)
	}
	return out
}

// searchQuery holds the parsed pieces of a GitHub search query.
type searchQuery struct {
	Terms         []string
	ExcludedTerms []string
	Qualifiers    []searchQualifier
	Repo          string
	User          string
	Org           string
	Language      string
	Topics        []string
	Label         string
	State         string
	IsIssue       *bool
	IsPR          *bool
	IsPrivate     *bool
	IsPublic      *bool
	InTitle       bool
	InBody        bool
	InComments    bool
	Sort          string
	Order         string
	PerPage       int
	Page          int
	Path          string
	Extension     string
	Filename      string
	Type          string // user search type: user/org
	Author        string // commit search: author qualifier
	Hash          string // commit search: hash qualifier
}

type searchQualifier struct {
	Key     string
	Value   string
	Negated bool
}

// unsupportedQualifierError names a qualifier or value the parser does not
// implement. Dropping one silently would widen results to the unfiltered
// universe, so it becomes a 422.
type unsupportedQualifierError struct {
	key   string
	value string
}

func (e unsupportedQualifierError) Error() string {
	if e.value == "" {
		return "The search qualifier \"" + e.key + "\" is not supported."
	}
	return "The value \"" + e.value + "\" is not supported for the search qualifier \"" + e.key + "\"."
}

// writeGHSearchQualifierError writes GitHub's 422 with the offending qualifier
// in the errors array.
func writeGHSearchQualifierError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message":           "Validation Failed",
		"documentation_url": "https://docs.github.com/rest/search/search",
		"errors": []map[string]string{
			{
				"message":  err.Error(),
				"resource": "Search",
				"field":    "q",
				"code":     "invalid",
			},
		},
	})
}

// searchQueryOrError parses a searchQuery, rejecting unimplemented qualifiers
// rather than answering with the unfiltered universe.
func searchQueryOrError(w http.ResponseWriter, r *http.Request, searchType string) (searchQuery, bool) {
	q, err := parseSearchQuery(r)
	if err != nil {
		writeGHSearchQualifierError(w, err)
		return q, false
	}
	if err := q.validateFor(searchType); err != nil {
		writeGHSearchQualifierError(w, err)
		return q, false
	}
	return q, true
}

func parseSearchQuery(r *http.Request) (searchQuery, error) {
	q := searchQuery{
		Terms:         []string{},
		ExcludedTerms: []string{},
		Qualifiers:    []searchQualifier{},
		Sort:          r.URL.Query().Get("sort"),
		Order:         r.URL.Query().Get("order"),
		PerPage:       30,
		Page:          1,
	}
	pp := parsePagination(r)
	q.PerPage = pp.PerPage
	q.Page = pp.Page

	for _, parsedToken := range tokenizeSearchQuery(r.URL.Query().Get("q")) {
		token := parsedToken.Value
		if token == "" {
			continue
		}
		negated := strings.HasPrefix(token, "-") && len(token) > 1
		if negated {
			token = strings.TrimPrefix(token, "-")
		}
		if !parsedToken.Quoted && strings.Contains(token, ":") {
			parts := strings.SplitN(token, ":", 2)
			key, val := strings.ToLower(parts[0]), parts[1]
			normalizedValue := strings.ToLower(val)
			if val == "" {
				return q, unsupportedQualifierError{key: key, value: val}
			}
			qualifier := searchQualifier{Key: key, Value: val, Negated: negated}
			if propertyName, ok := strings.CutPrefix(key, "props."); ok {
				if propertyName == "" || !validCustomPropertyName(propertyName) {
					return q, unsupportedQualifierError{key: key, value: val}
				}
				q.Qualifiers = append(q.Qualifiers, qualifier)
				continue
			}
			switch key {
			case "repo":
				if !negated {
					q.Repo = val
				}
			case "user":
				if !negated {
					q.User = val
				}
			case "org":
				if !negated {
					q.Org = val
				}
			case "language":
				if !negated {
					q.Language = val
				}
			case "topic":
				if !negated {
					q.Topics = append(q.Topics, val)
				}
			case "label":
				if !negated {
					q.Label = val
				}
			case "state":
				switch strings.ToLower(val) {
				case "open", "closed":
					if !negated {
						q.State = strings.ToLower(val)
					}
				default:
					return q, unsupportedQualifierError{key: "state", value: val}
				}
			case "is":
				switch strings.ToLower(val) {
				case "issue":
					if !negated {
						v := true
						q.IsIssue = &v
					}
				case "pr", "pull-request":
					if !negated {
						v := true
						q.IsPR = &v
					}
				case "private":
					if !negated {
						v := true
						q.IsPrivate = &v
					}
				case "public":
					if !negated {
						v := true
						q.IsPublic = &v
					}
				case "open", "closed":
					if !negated {
						q.State = strings.ToLower(val)
					}
				case "merged", "unmerged", "draft":
					// Applied per-pull-request in handleSearchIssues.
				case "internal", "sponsorable":
				default:
					return q, unsupportedQualifierError{key: "is", value: val}
				}
			case "in":
				for _, field := range strings.Split(strings.ToLower(val), ",") {
					switch field {
					case "title":
						q.InTitle = true
					case "body":
						q.InBody = true
					case "comments":
						q.InComments = true
					case "name", "description", "topics", "readme":
					// User-search `in:` fields (applied in handleSearchUsers).
					case "login", "email", "fullname":
					default:
						return q, unsupportedQualifierError{key: "in", value: val}
					}
				}
			case "path":
				if !negated {
					q.Path = val
				}
			case "extension", "ext":
				if !negated {
					q.Extension = val
				}
			case "filename", "file":
				if !negated {
					q.Filename = val
				}
			case "type":
				switch strings.ToLower(val) {
				case "user", "org":
					if !negated {
						q.Type = strings.ToLower(val)
					}
				case "issue":
					if !negated {
						v := true
						q.IsIssue = &v
					}
				case "pr", "pull-request":
					if !negated {
						v := true
						q.IsPR = &v
					}
				default:
					return q, unsupportedQualifierError{key: "type", value: val}
				}
			case "assignee", "milestone", "mentions", "involves", "commenter", "head", "base", "reviewed-by", "review-requested":
				// Applied per-item in handleSearchIssues; no parse-time state.
			case "review":
				switch normalizedValue {
				case "none", "required", "approved", "changes_requested":
				default:
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "linked":
				switch normalizedValue {
				case "pr", "issue":
				default:
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "reactions", "interactions":
				if _, err := parseNumericSearchConstraint(val); err != nil {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "no":
				switch strings.ToLower(val) {
				case "label", "assignee", "milestone":
				default:
					return q, unsupportedQualifierError{key: "no", value: val}
				}
			case "author":
				if !negated {
					q.Author = val
				}
			case "hash":
				if !negated {
					q.Hash = val
				}
			case "committer", "author-name", "author-email", "committer-name", "committer-email":
				// Commit-search identity qualifiers, applied per-commit in
				// handleSearchCommits against the git author/committer fields.
			case "author-date", "committer-date":
				if _, err := parseDateSearchConstraint(val); err != nil {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "merge":
				if normalizedValue != "true" && normalizedValue != "false" {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "archived", "template", "mirror", "deployable", "deployed":
				if normalizedValue != "true" && normalizedValue != "false" {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "fork":
				if normalizedValue != "true" && normalizedValue != "false" && normalizedValue != "only" {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "visibility":
				if normalizedValue != "public" && normalizedValue != "private" && normalizedValue != "internal" {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "size", "followers", "forks", "stars", "topics", "good-first-issues", "help-wanted-issues", "repos":
				if _, err := parseNumericSearchConstraint(val); err != nil {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "location":
				// User-search location filter; applied in handleSearchUsers.
			case "created", "pushed", "updated", "closed":
				if _, err := parseDateSearchConstraint(val); err != nil {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "draft":
				if normalizedValue != "true" && normalizedValue != "false" {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "license":
			case "has":
				if normalizedValue != "funding-file" {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			default:
				return q, unsupportedQualifierError{key: key}
			}
			q.Qualifiers = append(q.Qualifiers, qualifier)
			continue
		}
		if negated {
			q.ExcludedTerms = append(q.ExcludedTerms, strings.ToLower(token))
		} else {
			q.Terms = append(q.Terms, strings.ToLower(token))
		}
	}
	return q, nil
}

// searchTextFor builds the term-match target, honouring `in:`. Default is
// title+body; otherwise the union of the named fields.
func (q searchQuery) searchTextFor(title, body, comments string) string {
	if !q.InTitle && !q.InBody && !q.InComments {
		return title + " " + body
	}
	parts := make([]string, 0, 3)
	if q.InTitle {
		parts = append(parts, title)
	}
	if q.InBody {
		parts = append(parts, body)
	}
	if q.InComments {
		parts = append(parts, comments)
	}
	return strings.Join(parts, " ")
}

func (q searchQuery) matchesText(text string) bool {
	text = strings.ToLower(text)
	for _, term := range q.Terms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	for _, term := range q.ExcludedTerms {
		if strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func (s *Server) handleSearchIssues(w http.ResponseWriter, r *http.Request) {
	q, ok := searchQueryOrError(w, r, "issues")
	if !ok {
		return
	}
	// A repo:-scoped search only needs that one repository's reachability, not a
	// clone-and-check of every repo in the instance.
	var accessibleRepoIDs map[int]struct{}
	if q.Repo != "" {
		accessibleRepoIDs = s.searchAccessibleRepoIDForScope(r.Context(), q.Repo)
	} else {
		accessibleRepoIDs = s.searchAccessibleRepoIDs(r.Context())
	}

	// Rows are gathered under the read lock, then rendered after release (the
	// JSON builders re-lock). The fields the scorer and sorter read directly
	// (title, body, timestamps) are snapshotted here so those raw reads do not
	// race a concurrent UpdateIssue.
	type issueRow struct {
		issue   *store.Issue
		repo    *store.Repo
		assoc   string
		title   string
		body    string
		created time.Time
		updated time.Time
	}
	type prRow struct {
		pr      *store.PullRequest
		repo    *store.Repo
		assoc   string
		title   string
		body    string
		created time.Time
		updated time.Time
	}
	var issueRows []issueRow
	var prRows []prRow

	s.store.Mu.RLock()

	// A repo: qualifier (the common case) scopes the scan to one repository's
	// issues/PRs via the per-repo indexes, so search does not walk every issue and
	// PR in the instance. An unknown repo name yields no results.
	issuesToScan := s.store.Issues
	prsToScan := s.store.PullRequests
	var scopedRepoID int
	scopedComments := true
	if q.Repo != "" {
		if repo := s.store.RepoByNameLocked(q.Repo); repo != nil {
			scopedRepoID = repo.ID
			issuesToScan = s.store.IssuesByRepo[repo.ID]
			prsToScan = s.store.PullsByRepo[repo.ID]
		} else {
			issuesToScan = nil
			prsToScan = nil
			scopedComments = false
		}
	}

	// in:comments needs comment bodies concatenated per subject, keyed by
	// "parentType:id"; build it once, only when the query asks. When the search is
	// scoped to one repo, restrict the pass to that repo's parents.
	var commentBodies map[string]string
	if q.InComments && scopedComments {
		commentBodies = map[string]string{}
		addComments := func(parentType string, id int) {
			for _, c := range s.store.CommentsByParent[store.CommentCountKey(parentType, id)] {
				commentBodies[parentType+":"+strconv.Itoa(id)] += " " + c.Body
			}
		}
		if scopedRepoID != 0 {
			for _, issue := range issuesToScan {
				addComments("issue", issue.ID)
			}
			for _, pr := range prsToScan {
				addComments("pull_request", pr.ID)
			}
		} else {
			for _, c := range s.store.Comments {
				key := c.ParentType + ":" + strconv.Itoa(c.IssueID)
				commentBodies[key] += " " + c.Body
			}
		}
	}
	commentBodyFor := func(parentType string, id int) string {
		if commentBodies == nil {
			return ""
		}
		return commentBodies[parentType+":"+strconv.Itoa(id)]
	}

	for _, issue := range issuesToScan {
		repo := s.store.Repos[issue.RepoID]
		if repo == nil {
			continue
		}
		if _, accessible := accessibleRepoIDs[repo.ID]; !accessible {
			continue
		}
		if q.Repo != "" && !strings.EqualFold(repo.FullName, q.Repo) {
			continue
		}
		owner, _, _ := strings.Cut(repo.FullName, "/")
		if q.User != "" && !strings.EqualFold(owner, q.User) {
			continue
		}
		if q.Org != "" && repo.OwnerType != "Organization" {
			continue
		}
		if q.Org != "" {
			parts := strings.SplitN(repo.FullName, "/", 2)
			if len(parts) == 0 || !strings.EqualFold(parts[0], q.Org) {
				continue
			}
		}
		if q.IsPrivate != nil && *q.IsPrivate != repo.Private {
			continue
		}
		if q.IsPublic != nil && (*q.IsPublic == repo.Private || strings.EqualFold(repo.Visibility, "internal")) {
			continue
		}
		if q.includes("is", "internal") && !strings.EqualFold(repo.Visibility, "internal") {
			continue
		}
		if q.excludes("repo", repo.FullName) || q.excludes("user", owner) ||
			repo.OwnerType == "Organization" && q.excludes("org", owner) ||
			repo.Private && q.excludes("is", "private") ||
			!repo.Private && q.excludes("is", "public") ||
			strings.EqualFold(repo.Visibility, "internal") && q.excludes("is", "internal") {
			continue
		}
		if !searchIssueQualifiersMatchLocked(s.store, q, issue.AuthorID, issue.AssigneeIDs, issue.LabelIDs, issue.MilestoneID, issue.Title+" "+issue.Body,
			issueSearchSubject{createdAt: issue.CreatedAt, updatedAt: issue.UpdatedAt, closedAt: issue.ClosedAt, archived: repo.Archived}) {
			continue
		}
		excludedLabel := false
		for _, qualifier := range q.Qualifiers {
			if qualifier.Negated && qualifier.Key == "label" &&
				issueHasLabelNames(s.store, issue, []string{qualifier.Value}) {
				excludedLabel = true
				break
			}
		}
		if excludedLabel || q.excludes("state", strings.ToLower(issue.State)) ||
			q.excludes("is", strings.ToLower(issue.State)) || q.excludes("is", "issue") {
			continue
		}
		if q.State != "" && !strings.EqualFold(issue.State, q.State) {
			continue
		}
		if q.IsIssue != nil && !*q.IsIssue {
			continue
		}
		if q.IsPR != nil && *q.IsPR {
			continue
		}
		// is:merged / is:unmerged / is:draft are PR-only; no issue matches them.
		if q.includes("is", "merged") || q.includes("is", "unmerged") || q.includes("is", "draft") {
			continue
		}
		text := q.searchTextFor(issue.Title, issue.Body, commentBodyFor("issue", issue.ID))
		if !q.matchesText(text) {
			continue
		}

		issueRows = append(issueRows, issueRow{issue, repo, store.AuthorAssociationLocked(s.store, issue.AuthorID, repo), issue.Title, issue.Body, issue.CreatedAt, issue.UpdatedAt})
	}

	for _, pr := range prsToScan {
		repo := s.store.Repos[pr.RepoID]
		if repo == nil {
			continue
		}
		if _, accessible := accessibleRepoIDs[repo.ID]; !accessible {
			continue
		}
		if q.Repo != "" && !strings.EqualFold(repo.FullName, q.Repo) {
			continue
		}
		owner, _, _ := strings.Cut(repo.FullName, "/")
		if q.User != "" && !strings.EqualFold(owner, q.User) {
			continue
		}
		if q.Org != "" {
			parts := strings.SplitN(repo.FullName, "/", 2)
			if len(parts) == 0 || !strings.EqualFold(parts[0], q.Org) {
				continue
			}
		}
		if q.IsPrivate != nil && *q.IsPrivate != repo.Private {
			continue
		}
		if q.IsPublic != nil && (*q.IsPublic == repo.Private || strings.EqualFold(repo.Visibility, "internal")) {
			continue
		}
		if q.includes("is", "internal") && !strings.EqualFold(repo.Visibility, "internal") {
			continue
		}
		if q.excludes("repo", repo.FullName) || q.excludes("user", owner) ||
			repo.OwnerType == "Organization" && q.excludes("org", owner) ||
			repo.Private && q.excludes("is", "private") ||
			!repo.Private && q.excludes("is", "public") ||
			strings.EqualFold(repo.Visibility, "internal") && q.excludes("is", "internal") {
			continue
		}
		if !searchIssueQualifiersMatchLocked(s.store, q, pr.AuthorID, pr.AssigneeIDs, pr.LabelIDs, pr.MilestoneID, pr.Title+" "+pr.Body,
			issueSearchSubject{createdAt: pr.CreatedAt, updatedAt: pr.UpdatedAt, closedAt: pr.ClosedAt, isDraft: pr.IsDraft, archived: repo.Archived}) {
			continue
		}
		excludedLabel := false
		for _, qualifier := range q.Qualifiers {
			if qualifier.Negated && qualifier.Key == "label" &&
				prHasLabelNames(s.store, pr, []string{qualifier.Value}) {
				excludedLabel = true
				break
			}
		}
		prState := "closed"
		if pr.State == "OPEN" {
			prState = "open"
		}
		if excludedLabel || q.excludes("state", prState) || q.excludes("is", prState) ||
			q.excludes("is", "pr") || q.excludes("is", "pull-request") {
			continue
		}
		if q.State != "" {
			if q.State == "open" && pr.State != "OPEN" {
				continue
			}
			if q.State == "closed" && pr.State != "CLOSED" && pr.State != "MERGED" {
				continue
			}
		}
		if q.IsIssue != nil && *q.IsIssue {
			continue
		}
		if q.IsPR != nil && !*q.IsPR {
			continue
		}
		// is:merged / is:unmerged / is:draft (PR-only).
		if q.includes("is", "merged") && pr.State != "MERGED" {
			continue
		}
		if q.excludes("is", "merged") && pr.State == "MERGED" {
			continue
		}
		if q.includes("is", "unmerged") && pr.State == "MERGED" {
			continue
		}
		if q.includes("is", "draft") && !pr.IsDraft {
			continue
		}
		if q.excludes("is", "draft") && pr.IsDraft {
			continue
		}
		text := q.searchTextFor(pr.Title, pr.Body, commentBodyFor("pull_request", pr.ID))
		if !q.matchesText(text) {
			continue
		}

		prRows = append(prRows, prRow{pr, repo, store.AuthorAssociationLocked(s.store, pr.AuthorID, repo), pr.Title, pr.Body, pr.CreatedAt, pr.UpdatedAt})
	}

	s.store.Mu.RUnlock()

	base := s.baseURL(r)

	// Unify issue and PR rows so the set can be sorted and paginated before
	// rendering — rendering every match only to return one page dominates cost.
	rows := make([]searchIssueRow, 0, len(issueRows)+len(prRows))
	for _, row := range issueRows {
		rows = append(rows, searchIssueRow{issue: row.issue, repo: row.repo, assoc: row.assoc, title: row.title, body: row.body, created: row.created, updated: row.updated})
	}
	for _, row := range prRows {
		rows = append(rows, searchIssueRow{pr: row.pr, repo: row.repo, assoc: row.assoc, title: row.title, body: row.body, created: row.created, updated: row.updated})
	}

	// Interaction qualifiers read comment authors and reaction counts through
	// their own locks, so apply them here, after the store lock is released.
	if q.hasInteractionQualifiers() {
		kept := rows[:0]
		for _, row := range rows {
			if s.rowMatchesInteractionQualifiers(row, q) {
				kept = append(kept, row)
			}
		}
		rows = kept
	}

	// linked:pr / linked:issue derive the closing-reference relationship from PR
	// bodies; built once over the result rows' repositories, then filtered.
	if q.hasLinkedQualifier() {
		rows = s.filterLinkedQualifiers(rows, q)
	}

	withTextMatches := acceptsTextMatch(r)
	render := func(row searchIssueRow) map[string]interface{} {
		var item map[string]interface{}
		if row.issue != nil {
			item = issueToJSON(row.issue, s.store, base, row.repo.FullName)
			// Search returns the leaner issue-search-result-item.
			delete(item, "closed_by")
			item["score"] = searchRelevanceScore(q.Terms, row.title, row.body)
			item["author_association"] = row.assoc
			item["draft"] = false
			// pull_request is omitted for plain issues; set only on PR rows.
			item["repository"] = store.RepoToJSON(row.repo, s.store, base)
		} else {
			item = issueToJSONForPR(row.pr, s.store, base, row.repo.FullName)
			delete(item, "closed_by")
			item["score"] = searchRelevanceScore(q.Terms, row.title, row.body)
			item["author_association"] = row.assoc
			item["repository"] = store.RepoToJSON(row.repo, s.store, base)
		}
		if withTextMatches {
			objectURL, _ := item["url"].(string)
			item["text_matches"] = searchTextMatches(objectURL, "Issue", []searchTextMatchProperty{
				{"title", row.title},
				{"body", row.body},
			}, q.Terms)
		}
		return item
	}

	total := len(rows)

	// comments/reactions/interactions sorts need rendered per-row counts, so
	// they keep the render-all path; timestamp sorts render just the page.
	if q.Sort == "comments" || q.Sort == "reactions" || q.Sort == "interactions" {
		results := make([]map[string]interface{}, 0, total)
		for _, row := range rows {
			results = append(results, render(row))
		}
		env := searchEnvelope(results, len(results), false, q, sortSearchResults)
		env["search_type"] = "lexical" // the algorithm, not the subject
		writeSearchEnvelope(w, r, q, env)
		return
	}

	sortSearchRows(rows, q.Sort, q.Order)
	start, end := searchPageBounds(q.Page, q.PerPage, total)
	pageItems := make([]map[string]interface{}, 0, end-start)
	for _, row := range rows[start:end] {
		pageItems = append(pageItems, render(row))
	}
	out := map[string]interface{}{
		"total_count":        total,
		"incomplete_results": false,
		"items":              pageItems,
		// Required on issues search: the algorithm, not the subject.
		"search_type": "lexical",
	}
	writeSearchEnvelope(w, r, q, out)
}

// searchIssueRow is a matched issue or PR carrying just enough to sort and then
// render only the requested page.
type searchIssueRow struct {
	issue *store.Issue
	pr    *store.PullRequest
	repo  *store.Repo
	assoc string
	// Snapshotted under the store lock; the scorer and sorter read these instead
	// of the live entity so they do not race a concurrent writer.
	title   string
	body    string
	created time.Time
	updated time.Time
}

func (row searchIssueRow) createdAt() time.Time { return row.created }

func (row searchIssueRow) updatedAt() time.Time { return row.updated }

// orderKey mirrors searchItemOrderKey for an unrendered row: entity id plus its
// issue path, unique because issues and PRs share one per-repository counter.
func (row searchIssueRow) orderKey() (int, string) {
	id, number := 0, 0
	if row.issue != nil {
		id, number = row.issue.ID, row.issue.Number
	} else {
		id, number = row.pr.ID, row.pr.Number
	}
	return id, "/api/v3/repos/" + row.repo.FullName + "/issues/" + strconv.Itoa(number)
}

// sortSearchRows establishes the total row order (deterministic base order,
// then the requested sort stably on top) by the same keys as sortSearchResults,
// so paginate-before-render yields the identical page.
func sortSearchRows(rows []searchIssueRow, sortKey, order string) {
	sort.Slice(rows, func(i, j int) bool {
		idI, keyI := rows[i].orderKey()
		idJ, keyJ := rows[j].orderKey()
		if idI != idJ {
			return idI < idJ
		}
		return keyI < keyJ
	})
	switch sortKey {
	case "created":
		sort.SliceStable(rows, func(i, j int) bool {
			if order == "asc" {
				return rows[i].createdAt().Before(rows[j].createdAt())
			}
			return rows[i].createdAt().After(rows[j].createdAt())
		})
	case "updated":
		sort.SliceStable(rows, func(i, j int) bool {
			if order == "asc" {
				return rows[i].updatedAt().Before(rows[j].updatedAt())
			}
			return rows[i].updatedAt().After(rows[j].updatedAt())
		})
	}
}

func searchPageBounds(page, perPage, total int) (int, int) {
	start := (page - 1) * perPage
	if start < 0 || start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return start, end
}

func issueToJSONForPR(pr *store.PullRequest, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	out := issueToJSONForPullRequest(pr, st, baseURL, repoFullName)
	out["pull_request"] = map[string]interface{}{
		"url":       baseURL + "/api/v3/repos/" + repoFullName + "/pulls/" + strconv.Itoa(pr.Number),
		"html_url":  baseURL + "/" + repoFullName + "/pull/" + strconv.Itoa(pr.Number),
		"diff_url":  baseURL + "/" + repoFullName + "/pull/" + strconv.Itoa(pr.Number) + ".diff",
		"patch_url": baseURL + "/" + repoFullName + "/pull/" + strconv.Itoa(pr.Number) + ".patch",
		"merged_at": nil,
	}
	return out
}

// issueToJSONForPullRequest renders a pull request in the issue shape. Must not
// be called with st.mu held: it takes the read lock itself.
func issueToJSONForPullRequest(pr *store.PullRequest, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	pr = st.SnapPR(pr)
	st.Mu.RLock()
	authorJSON := store.UserToJSON(store.ActorUserLocked(st, pr.AuthorID), baseURL)
	repo := st.Repos[pr.RepoID]

	labels := make([]map[string]interface{}, 0)
	for _, lid := range pr.LabelIDs {
		if l := st.Labels[lid]; l != nil {
			labels = append(labels, issueLabelToJSON(l, baseURL, repoFullName))
		}
	}
	assignees := make([]map[string]interface{}, 0)
	for _, aid := range pr.AssigneeIDs {
		if u := st.Users[aid]; u != nil {
			assignees = append(assignees, store.UserToJSON(u, baseURL))
		}
	}
	var assignee interface{}
	if len(assignees) > 0 {
		assignee = assignees[0]
	}
	// Convert the milestone after unlock; milestoneToJSON locks itself.
	var milestone *store.Milestone
	if pr.MilestoneID > 0 {
		milestone = st.Milestones[pr.MilestoneID]
	}
	commentCount := st.CountCommentsForLocked("pull_request", pr.ID)
	st.Mu.RUnlock()

	var milestoneJSON interface{}
	if milestone != nil {
		milestoneJSON = milestoneToJSON(milestone, st, baseURL, repoFullName)
	}
	var closedAt interface{}
	if pr.ClosedAt != nil {
		closedAt = pr.ClosedAt.Format(time.RFC3339)
	}
	var activeLockReason interface{}
	if pr.Locked {
		activeLockReason = pr.ActiveLockReason
	}
	numStr := strconv.Itoa(pr.Number)
	api := baseURL + "/api/v3/repos/" + repoFullName + "/issues/" + numStr
	// PR search rows reuse the issues reactions parent ("pull_request") and URL.
	reactions := st.Reactions.SummarizeReactions("pull_request", pr.ID)
	reactions["url"] = api + "/reactions"
	return map[string]interface{}{
		"id":                 pr.ID,
		"node_id":            pr.NodeID,
		"url":                api,
		"html_url":           baseURL + "/" + repoFullName + "/issues/" + numStr,
		"repository_url":     baseURL + "/api/v3/repos/" + repoFullName,
		"comments_url":       api + "/comments",
		"events_url":         api + "/events",
		"timeline_url":       api + "/timeline",
		"labels_url":         api + "/labels{/name}",
		"number":             pr.Number,
		"title":              pr.Title,
		"body":               pr.Body,
		"state":              strings.ToLower(pr.State),
		"state_reason":       nil,
		"user":               authorJSON,
		"labels":             labels,
		"assignee":           assignee,
		"assignees":          assignees,
		"milestone":          milestoneJSON,
		"locked":             pr.Locked,
		"active_lock_reason": activeLockReason,
		"comments":           commentCount,
		"created_at":         pr.CreatedAt.Format(time.RFC3339),
		"updated_at":         pr.UpdatedAt.Format(time.RFC3339),
		"closed_at":          closedAt,
		"closed_by":          pullRequestClosedByJSON(st, pr, baseURL),
		"author_association": store.AuthorAssociation(st, pr.AuthorID, repo),
		"draft":              pr.IsDraft,
		"reactions":          reactions,
	}
}

func pullRequestClosedByJSON(st *store.Store, pr *store.PullRequest, baseURL string) interface{} {
	if pr == nil || (pr.State != "CLOSED" && pr.State != "MERGED") {
		return nil
	}
	actorID := pr.MergedByID
	if actorID == 0 {
		events := st.ListPullRequestEvents(pr.RepoID, pr.ID)
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Event == "closed" {
				actorID = events[i].ActorID
				break
			}
		}
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	actor := store.ActorUserLocked(st, actorID)
	if actor == nil {
		return nil
	}
	return store.UserToJSON(actor, baseURL)
}

// issueHasLabelNames reports whether the issue carries every named label.
// Callers hold st.mu.
func issueHasLabelNames(st *store.Store, issue *store.Issue, names []string) bool {
	for _, name := range names {
		found := false
		for _, lid := range issue.LabelIDs {
			if l := st.Labels[lid]; l != nil && strings.EqualFold(l.Name, name) {
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

// prHasLabelNames reports whether the pull request carries every named label.
// Callers hold st.mu.
func prHasLabelNames(st *store.Store, pr *store.PullRequest, names []string) bool {
	for _, name := range names {
		found := false
		for _, lid := range pr.LabelIDs {
			if l := st.Labels[lid]; l != nil && strings.EqualFold(l.Name, name) {
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

// hasInteractionQualifiers reports whether the query carries a qualifier whose
// evaluation needs comment/reaction data fetched outside the store lock.
func (q searchQuery) hasInteractionQualifiers() bool {
	for _, ql := range q.Qualifiers {
		switch ql.Key {
		case "involves", "commenter", "reactions", "interactions", "head", "base",
			"review", "reviewed-by", "review-requested":
			return true
		}
	}
	return false
}

func (q searchQuery) hasLinkedQualifier() bool {
	for _, ql := range q.Qualifiers {
		if ql.Key == "linked" {
			return true
		}
	}
	return false
}

// prClosingRefPattern matches GitHub's closing keywords followed by a same-repo
// issue reference, e.g. "Closes #12", "fixes: #3", "Resolved #9".
var prClosingRefPattern = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#(\d+)\b`)

// parsePRClosingIssueRefs extracts the issue numbers a PR body closes.
func parsePRClosingIssueRefs(body string) []int {
	var out []int
	for _, m := range prClosingRefPattern.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// filterLinkedQualifiers applies linked:pr / linked:issue. An issue matches
// linked:pr when a PR in its repository closes it; a PR matches linked:issue
// when it closes at least one issue, derived from PR-body closing keywords.
func (s *Server) filterLinkedQualifiers(rows []searchIssueRow, q searchQuery) []searchIssueRow {
	repoIDs := map[int]bool{}
	for _, row := range rows {
		if row.repo != nil {
			repoIDs[row.repo.ID] = true
		}
	}
	closedIssueNums := map[int]map[int]bool{} // repoID -> set of closed issue numbers
	prClosesIssue := map[int]bool{}           // pr.ID -> closes >= 1 issue
	s.store.Mu.RLock()
	for _, pr := range s.store.PullRequests {
		if !repoIDs[pr.RepoID] {
			continue
		}
		refs := parsePRClosingIssueRefs(pr.Body)
		if len(refs) == 0 {
			continue
		}
		prClosesIssue[pr.ID] = true
		set := closedIssueNums[pr.RepoID]
		if set == nil {
			set = map[int]bool{}
			closedIssueNums[pr.RepoID] = set
		}
		for _, n := range refs {
			set[n] = true
		}
	}
	s.store.Mu.RUnlock()

	kept := rows[:0]
	for _, row := range rows {
		if rowMatchesLinkedQualifiers(row, q, closedIssueNums, prClosesIssue) {
			kept = append(kept, row)
		}
	}
	return kept
}

func rowMatchesLinkedQualifiers(row searchIssueRow, q searchQuery, closedIssueNums map[int]map[int]bool, prClosesIssue map[int]bool) bool {
	for _, ql := range q.Qualifiers {
		if ql.Key != "linked" {
			continue
		}
		var ok bool
		switch strings.ToLower(strings.Trim(ql.Value, `"`)) {
		case "pr":
			ok = row.issue != nil && row.repo != nil && closedIssueNums[row.repo.ID][row.issue.Number]
		case "issue":
			ok = row.pr != nil && prClosesIssue[row.pr.ID]
		}
		if ok == ql.Negated {
			return false
		}
	}
	return true
}

// prReviewDecision derives a PR's review decision from its latest-per-author
// submitted reviews: CHANGES_REQUESTED wins over APPROVED, "" when neither.
func (s *Server) prReviewDecision(prID int) string {
	latest := map[int]*store.PullRequestReview{}
	for _, rv := range s.store.ListPRReviews(prID) {
		switch rv.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		default:
			continue
		}
		cur := latest[rv.AuthorID]
		if cur == nil || rv.UpdatedAt.After(cur.UpdatedAt) ||
			(rv.UpdatedAt.Equal(cur.UpdatedAt) && rv.ID > cur.ID) {
			latest[rv.AuthorID] = rv
		}
	}
	hasApproved, hasChangesRequested := false, false
	for _, rv := range latest {
		switch rv.State {
		case "APPROVED":
			hasApproved = true
		case "CHANGES_REQUESTED":
			hasChangesRequested = true
		}
	}
	if hasChangesRequested {
		return "CHANGES_REQUESTED"
	}
	if hasApproved {
		return "APPROVED"
	}
	return ""
}

// rowMatchesInteractionQualifiers applies the interaction qualifiers to one row.
// Must be called with the store read lock released.
func (s *Server) rowMatchesInteractionQualifiers(row searchIssueRow, q searchQuery) bool {
	var parentType string
	var parentID, authorID int
	var assigneeIDs []int
	var text string
	if row.issue != nil {
		parentType, parentID = "issue", row.issue.ID
		authorID, assigneeIDs, text = row.issue.AuthorID, row.issue.AssigneeIDs, row.issue.Title+" "+row.issue.Body
	} else {
		parentType, parentID = "pull_request", row.pr.ID
		authorID, assigneeIDs, text = row.pr.AuthorID, row.pr.AssigneeIDs, row.pr.Title+" "+row.pr.Body
	}
	loginOf := func(id int) string {
		if u := s.store.GetUserByID(id); u != nil {
			return u.Login
		}
		return ""
	}
	var comments []*store.Comment
	loaded := false
	loadComments := func() []*store.Comment {
		if !loaded {
			comments = s.store.ListCommentsFor(parentType, parentID)
			loaded = true
		}
		return comments
	}
	isCommenter := func(login string) bool {
		for _, c := range loadComments() {
			if strings.EqualFold(loginOf(c.AuthorID), login) {
				return true
			}
		}
		return false
	}
	reactionTotal := func() int {
		t, _ := s.store.Reactions.SummarizeReactions(parentType, parentID)["total_count"].(int)
		return t
	}
	var reviews []*store.PullRequestReview
	reviewsLoaded := false
	loadReviews := func() []*store.PullRequestReview {
		if !reviewsLoaded {
			if row.pr != nil {
				reviews = s.store.ListPRReviews(row.pr.ID)
			}
			reviewsLoaded = true
		}
		return reviews
	}
	// Submitted reviews exclude a viewer's own PENDING review.
	submittedReviewerLogins := func() map[string]bool {
		out := map[string]bool{}
		for _, rv := range loadReviews() {
			if rv.State == "PENDING" {
				continue
			}
			if l := loginOf(rv.AuthorID); l != "" {
				out[strings.ToLower(l)] = true
			}
		}
		return out
	}
	for _, ql := range q.Qualifiers {
		val := strings.Trim(ql.Value, `"`)
		var ok bool
		switch ql.Key {
		case "commenter":
			ok = isCommenter(val)
		case "involves":
			// GitHub's union: author, assignee, mentioned, or commenter.
			ok = strings.EqualFold(loginOf(authorID), val)
			for _, aid := range assigneeIDs {
				if ok {
					break
				}
				ok = strings.EqualFold(loginOf(aid), val)
			}
			if !ok {
				ok = strings.Contains(strings.ToLower(text), "@"+strings.ToLower(val))
			}
			if !ok {
				ok = isCommenter(val)
			}
		case "reactions":
			c, err := parseNumericSearchConstraint(val)
			ok = err == nil && c.matches(int64(reactionTotal()))
		case "interactions":
			c, err := parseNumericSearchConstraint(val)
			ok = err == nil && c.matches(int64(reactionTotal()+len(loadComments())))
		case "head":
			// head:/base: match a PR's source/target branch; an issue never matches.
			ok = row.pr != nil && strings.EqualFold(row.pr.HeadRefName, val)
		case "base":
			ok = row.pr != nil && strings.EqualFold(row.pr.BaseRefName, val)
		case "reviewed-by":
			ok = row.pr != nil && submittedReviewerLogins()[strings.ToLower(val)]
		case "review-requested":
			if row.pr != nil {
				for _, id := range row.pr.RequestedReviewerIDs {
					if strings.EqualFold(loginOf(id), val) {
						ok = true
						break
					}
				}
			}
		case "review":
			// PR-only.
			if row.pr != nil {
				switch strings.ToLower(val) {
				case "approved":
					ok = s.prReviewDecision(row.pr.ID) == "APPROVED"
				case "changes_requested":
					ok = s.prReviewDecision(row.pr.ID) == "CHANGES_REQUESTED"
				case "none":
					ok = len(submittedReviewerLogins()) == 0
				case "required":
					// Review requested but not yet approved.
					ok = len(row.pr.RequestedReviewerIDs) > 0 && s.prReviewDecision(row.pr.ID) != "APPROVED"
				}
			}
		default:
			continue
		}
		if ok == ql.Negated {
			return false
		}
	}
	return true
}

// issueSearchSubject carries the per-item fields the temporal and state
// qualifiers filter on, so the issue and PR callers drive one matcher.
type issueSearchSubject struct {
	createdAt time.Time
	updatedAt time.Time
	closedAt  *time.Time
	isDraft   bool
	archived  bool
}

func searchIssueQualifiersMatchLocked(st *store.Store, q searchQuery, authorID int, assigneeIDs, labelIDs []int, milestoneID int, text string, subj issueSearchSubject) bool {
	loginOf := func(id int) string {
		if u := store.ActorUserLocked(st, id); u != nil {
			return u.Login
		}
		return ""
	}
	for _, ql := range q.Qualifiers {
		val := strings.Trim(ql.Value, `"`)
		// Compute whether the subject matches this qualifier's value, then apply
		// negation uniformly. Previously every negated qualifier was skipped, so
		// `-author:x` / `-assignee:x` / `-milestone:x` / `-mentions:x` were
		// silently ignored and returned issues GitHub would have excluded.
		var matched bool
		switch ql.Key {
		case "author":
			matched = strings.EqualFold(loginOf(authorID), val)
		case "assignee":
			if val == "*" {
				matched = len(assigneeIDs) > 0
			} else {
				for _, aid := range assigneeIDs {
					if strings.EqualFold(loginOf(aid), val) {
						matched = true
						break
					}
				}
			}
		case "milestone":
			if val == "*" {
				matched = milestoneID != 0
			} else if ms := st.Milestones[milestoneID]; ms != nil {
				matched = strings.EqualFold(ms.Title, val)
			}
		case "label":
			for _, lid := range labelIDs {
				if l := st.Labels[lid]; l != nil && strings.EqualFold(l.Name, val) {
					matched = true
					break
				}
			}
		case "mentions":
			matched = strings.Contains(strings.ToLower(text), "@"+strings.ToLower(val))
		case "no":
			switch strings.ToLower(val) {
			case "label":
				matched = len(labelIDs) == 0
			case "assignee":
				matched = len(assigneeIDs) == 0
			case "milestone":
				matched = milestoneID == 0
			default:
				continue
			}
		case "created", "updated":
			c, err := parseDateSearchConstraint(val)
			at := subj.createdAt
			if ql.Key == "updated" {
				at = subj.updatedAt
			}
			matched = err == nil && c.matches(at)
		case "closed":
			c, err := parseDateSearchConstraint(val)
			matched = err == nil && subj.closedAt != nil && c.matches(*subj.closedAt)
		case "draft":
			matched = subj.isDraft == strings.EqualFold(val, "true")
		case "archived":
			matched = subj.archived == strings.EqualFold(val, "true")
		default:
			// Qualifiers handled elsewhere (repo/user/org/is/state/in/...); don't
			// evaluate them here or a negated one would wrongly exclude everything.
			continue
		}
		if matched == ql.Negated {
			return false
		}
	}
	return true
}

func (s *Server) handleSearchRepositories(w http.ResponseWriter, r *http.Request) {
	q, ok := searchQueryOrError(w, r, "repositories")
	if !ok {
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("q")) == "" {
		store.WriteGHValidationError(w, "Search", "q", "missing_field")
		return
	}
	switch q.Sort {
	case "", "stars", "forks", "help-wanted-issues", "updated":
	default:
		store.WriteGHValidationError(w, "Search", "sort", "invalid")
		return
	}
	if q.Order != "" && q.Order != "asc" && q.Order != "desc" {
		store.WriteGHValidationError(w, "Search", "order", "invalid")
		return
	}

	// Snapshot readable repositories under the store lock, then evaluate
	// storage-backed qualifiers after release (repoToJSON and several counters
	// lock the store themselves).
	var candidates []*store.Repo
	accessibleRepoIDs := s.searchAccessibleRepoIDs(r.Context())
	s.store.Mu.RLock()
	for _, repo := range s.store.Repos {
		if _, accessible := accessibleRepoIDs[repo.ID]; !accessible {
			continue
		}
		snapshot := *repo
		snapshot.Topics = append([]string(nil), repo.Topics...)
		if repo.Owner != nil {
			owner := *repo.Owner
			snapshot.Owner = &owner
		}
		candidates = append(candidates, &snapshot)
	}
	s.store.Mu.RUnlock()

	var matched []*store.Repo
	for _, repo := range candidates {
		if repositoryMatchesSearch(s.store, repo, q) {
			matched = append(matched, repo)
		}
	}

	base := s.baseURL(r)
	withTextMatches := acceptsTextMatch(r)
	var results []map[string]interface{}
	for _, repo := range matched {
		item := store.RepoToJSON(repo, s.store, base)
		item["score"] = searchRelevanceScore(q.Terms, repo.Name, repo.Description)
		// repoIssueLabelCount scans every issue in the instance, so compute it only
		// when the caller sorts by it — otherwise a broad repo search was
		// O(matched-repos × all-issues) for a value that was then discarded.
		if q.Sort == "help-wanted-issues" {
			item["_help_wanted_issues"] = repoIssueLabelCount(s.store, repo.ID, "help wanted")
		}
		if withTextMatches {
			objectURL, _ := item["url"].(string)
			item["text_matches"] = searchTextMatches(objectURL, "Repository", []searchTextMatchProperty{
				{"name", repo.Name},
				{"description", repo.Description},
			}, q.Terms)
		}
		results = append(results, item)
	}

	writeSearchEnvelope(w, r, q, searchEnvelope(results, len(results), false, q, sortRepoSearchResults))
}

func repoIssueLabelCount(st *store.Store, repoID int, labelName string) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	count := 0
	for _, issue := range st.Issues {
		if issue == nil || issue.RepoID != repoID || !strings.EqualFold(issue.State, "open") {
			continue
		}
		for _, labelID := range issue.LabelIDs {
			if label := st.Labels[labelID]; label != nil && strings.EqualFold(label.Name, labelName) {
				count++
				break
			}
		}
	}
	return count
}

func (s *Server) handleSearchCode(w http.ResponseWriter, r *http.Request) {
	if ghUserFromContext(r.Context()) == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	q, ok := searchQueryOrError(w, r, "code")
	if !ok {
		return
	}

	if len(q.Terms) == 0 && q.Filename == "" && q.Extension == "" && q.Path == "" {
		store.WriteGHValidationError(w, "Search", "q", "missing_field")
		return
	}

	// Gather repos and their git storages under the read lock; the tree walk and
	// rendering happen after release (repoToJSON locks itself).
	type codeSearchRepo struct {
		repo *store.Repo
		stor gitStorage.Storer
	}
	var searchRepos []codeSearchRepo
	var accessibleRepoIDs map[int]struct{}
	if q.Repo != "" {
		accessibleRepoIDs = s.searchAccessibleRepoIDForScope(r.Context(), q.Repo)
	} else {
		accessibleRepoIDs = s.searchAccessibleRepoIDs(r.Context())
	}
	s.store.Mu.RLock()
	for _, repo := range s.searchCandidateReposLocked(q.Repo) {
		if _, accessible := accessibleRepoIDs[repo.ID]; !accessible {
			continue
		}
		if q.Repo != "" && !strings.EqualFold(repo.FullName, q.Repo) {
			continue
		}
		if q.User != "" || q.Org != "" {
			parts := strings.SplitN(repo.FullName, "/", 2)
			if len(parts) == 0 {
				continue
			}
			owner := parts[0]
			if q.User != "" && !strings.EqualFold(owner, q.User) {
				continue
			}
			if q.Org != "" && (repo.OwnerType != "Organization" || !strings.EqualFold(owner, q.Org)) {
				continue
			}
		}
		if q.Language != "" && !strings.EqualFold(repo.Language, q.Language) {
			continue
		}
		owner, _, _ := strings.Cut(repo.FullName, "/")
		if q.excludes("repo", repo.FullName) || q.excludes("user", owner) ||
			repo.OwnerType == "Organization" && q.excludes("org", owner) ||
			q.excludes("language", repo.Language) {
			continue
		}
		stor, ok := s.store.GitStorages[repo.FullName]
		if !ok {
			continue
		}
		searchRepos = append(searchRepos, codeSearchRepo{repo, stor})
	}
	s.store.Mu.RUnlock()
	// Fixed scan order: the 1000-result cap otherwise truncates a different
	// subset each request.
	sort.Slice(searchRepos, func(i, j int) bool { return searchRepos[i].repo.ID < searchRepos[j].repo.ID })

	base := s.baseURL(r)
	withTextMatches := acceptsTextMatch(r)
	var results []map[string]interface{}
	total := 0
	truncated := false

	for _, sr := range searchRepos {
		repo := sr.repo
		gr, err := git.Open(sr.stor, nil)
		if err != nil {
			continue
		}
		head, err := gr.Head()
		if err != nil {
			continue
		}
		commit, err := gr.CommitObject(head.Hash())
		if err != nil {
			continue
		}
		tree, err := gr.TreeObject(commit.TreeHash)
		if err != nil {
			continue
		}

		err = tree.Files().ForEach(func(f *object.File) error {
			path := f.Name
			name := filepath.Base(path)
			ext := strings.TrimPrefix(filepath.Ext(name), ".")

			if q.Filename != "" && !strings.EqualFold(name, q.Filename) {
				return nil
			}
			if q.Extension != "" && !strings.EqualFold(ext, q.Extension) {
				return nil
			}
			if q.Path != "" && !strings.Contains(path, q.Path) {
				return nil
			}
			if q.excludes("filename", name) || q.excludes("file", name) ||
				q.excludes("extension", ext) || q.excludes("ext", ext) {
				return nil
			}
			for _, qualifier := range q.Qualifiers {
				if qualifier.Negated && qualifier.Key == "path" && strings.Contains(path, qualifier.Value) {
					return nil
				}
			}

			matched := false
			content := ""
			if len(q.Terms) == 0 {
				matched = true
			} else {
				blob, err := gr.BlobObject(plumbing.NewHash(f.Hash.String()))
				if err == nil {
					reader, err := blob.Reader()
					if err == nil {
						data, err := io.ReadAll(reader)
						_ = reader.Close()
						if err == nil && len(data) < 384*1024 {
							content = string(data)
							if pathMatches(strings.ToLower(content), q.Terms) || pathMatches(strings.ToLower(path), q.Terms) {
								matched = true
							}
						}
					}
				}
			}
			if !matched {
				return nil
			}

			// Count every match for total_count; only searchResultCap rows are
			// rendered, the rest reported via incomplete_results.
			total++
			if len(results) >= searchResultCap {
				truncated = true
				return nil
			}
			api := base + "/api/v3/repos/" + repo.FullName
			item := map[string]interface{}{
				"name":       name,
				"path":       path,
				"sha":        f.Hash.String(),
				"url":        api + "/contents/" + path,
				"git_url":    api + "/git/blobs/" + f.Hash.String(),
				"html_url":   base + "/" + repo.FullName + "/blob/" + repo.DefaultBranch + "/" + path,
				"repository": store.RepoToJSON(repo, s.store, base),
				"score":      searchRelevanceScore(q.Terms, name, path),
				"language":   detectLanguage(name),
			}
			if withTextMatches {
				item["text_matches"] = searchTextMatches(api+"/contents/"+path, "FileContent", []searchTextMatchProperty{
					{"content", content},
					{"path", path},
				}, q.Terms)
			}
			results = append(results, item)
			return nil
		})
		if err != nil {
			s.logger.Debug().Err(err).Str("repo", repo.FullName).Msg("code search tree walk")
		}
	}

	writeSearchEnvelope(w, r, q, searchEnvelope(results, total, truncated, q, nil))
}

// userSearchText restricts the match target to the fields named by in:
// (login/name/email/fullname); default is login+name+bio.
func userSearchText(q searchQuery, u *store.User) string {
	var fields []string
	for _, ql := range q.Qualifiers {
		if ql.Negated || ql.Key != "in" {
			continue
		}
		fields = append(fields, strings.Split(strings.ToLower(ql.Value), ",")...)
	}
	if len(fields) == 0 {
		return u.Login + " " + u.Name + " " + u.Bio
	}
	var parts []string
	for _, f := range fields {
		switch f {
		case "login":
			parts = append(parts, u.Login)
		case "name", "fullname":
			parts = append(parts, u.Name)
		case "email":
			parts = append(parts, u.Email)
		}
	}
	return strings.Join(parts, " ")
}

// userMatchesInLockQualifiers applies the qualifiers readable directly off the
// User struct (location:, created:). Caller holds st.Mu.RLock.
func userMatchesInLockQualifiers(q searchQuery, u *store.User) bool {
	for _, ql := range q.Qualifiers {
		if ql.Negated {
			continue
		}
		switch ql.Key {
		case "location":
			if !strings.Contains(strings.ToLower(u.Location), strings.ToLower(strings.Trim(ql.Value, `"`))) {
				return false
			}
		case "created":
			if !matchesDateQualifier(ql.Value, u.CreatedAt) {
				return false
			}
		}
	}
	return true
}

// userMatchesCountQualifiers applies repos:/followers: constraints. Call after
// releasing the store lock: the count helpers take their own.
func (s *Server) userMatchesCountQualifiers(q searchQuery, u *store.User) bool {
	for _, ql := range q.Qualifiers {
		if ql.Negated {
			continue
		}
		switch ql.Key {
		case "repos":
			if !matchesNumericQualifier(ql.Value, int64(s.store.CountPublicRepos(u.Login))) {
				return false
			}
		case "followers":
			if !matchesNumericQualifier(ql.Value, int64(s.store.CountFollowers(u.Login))) {
				return false
			}
		case "language":
			matched := false
			for _, repo := range s.store.ListReposForUser(u, store.RepoListOptions{}) {
				if !repo.Private && strings.EqualFold(repo.Language, ql.Value) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

// queryHasUserScopedQualifier reports whether the query carries a user-only
// qualifier, so organizations should be excluded.
func queryHasUserScopedQualifier(q searchQuery) bool {
	for _, ql := range q.Qualifiers {
		switch ql.Key {
		case "repos", "followers", "location", "created", "language":
			return true
		case "in":
			for _, f := range strings.Split(strings.ToLower(ql.Value), ",") {
				if f == "login" || f == "email" || f == "fullname" {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) handleSearchUsers(w http.ResponseWriter, r *http.Request) {
	q, ok := searchQueryOrError(w, r, "users")
	if !ok {
		return
	}

	// Gather users and orgs under the read lock; render after release
	// (fullUserJSON derives its counts under its own locks).
	var users []*store.User
	var orgs []*store.Org
	s.store.Mu.RLock()
	for _, u := range s.store.Users {
		if q.Type == "org" {
			continue
		}
		if q.excludes("type", "user") {
			continue
		}
		if !q.matchesText(userSearchText(q, u)) {
			continue
		}
		if !userMatchesInLockQualifiers(q, u) {
			continue
		}
		users = append(users, u)
	}
	// A user-scoped qualifier excludes organizations, which cannot satisfy it.
	orgsExcluded := queryHasUserScopedQualifier(q)
	for _, org := range s.store.Orgs {
		if q.Type == "user" || orgsExcluded {
			continue
		}
		if q.excludes("type", "org") {
			continue
		}
		text := org.Login + " " + org.Name + " " + org.Description
		if !q.matchesText(text) {
			continue
		}
		orgs = append(orgs, org)
	}
	s.store.Mu.RUnlock()

	// repos:/followers: read counts whose helpers take their own locks — apply
	// after releasing st.Mu.
	users = slices.DeleteFunc(users, func(u *store.User) bool {
		return !s.userMatchesCountQualifiers(q, u)
	})

	var results []map[string]interface{}
	for _, u := range users {
		item := s.fullUserJSON(u, s.baseURL(r))
		// user-search-result-item has no twitter_username.
		delete(item, "twitter_username")
		item["score"] = searchRelevanceScore(q.Terms, u.Login, u.Name+" "+u.Bio)
		results = append(results, item)
	}
	for _, org := range orgs {
		item := store.OrgAsSimpleUserJSON(org, s.baseURL(r))
		item["score"] = searchRelevanceScore(q.Terms, org.Login, org.Name+" "+org.Description)
		results = append(results, item)
	}

	writeSearchEnvelope(w, r, q, searchEnvelope(results, len(results), false, q, sortUserSearchResults))
}

func pathMatches(text string, terms []string) bool {
	for _, term := range terms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func detectLanguage(filename string) interface{} {
	ext := strings.TrimPrefix(filepath.Ext(filename), ".")
	switch strings.ToLower(ext) {
	case "go":
		return "Go"
	case "js", "jsx":
		return "JavaScript"
	case "ts", "tsx":
		return "TypeScript"
	case "py":
		return "Python"
	case "md":
		return "Markdown"
	case "yml", "yaml":
		return "YAML"
	case "json":
		return "JSON"
	case "sh":
		return "Shell"
	case "dockerfile":
		return "Dockerfile"
	case "":
		return nil
	}
	return ext
}

// searchSorter applies a sort key to already-ordered results. It must sort
// stably so the base order survives as the tiebreak.
type searchSorter func(items []map[string]interface{}, sortKey, order string) []map[string]interface{}

// searchItemOrderKey is the identity a result is tiebroken on: its id, then the
// first globally unique string member it carries.
func searchItemOrderKey(item map[string]interface{}) (int, string) {
	id, _ := item["id"].(int)
	for _, member := range []string{"url", "node_id", "name"} {
		if s, ok := item[member].(string); ok && s != "" {
			return id, s
		}
	}
	return id, ""
}

// orderSearchItems imposes the total result order (score descending, then
// identity ascending) that map iteration lacks, so paging is stable across
// requests.
func orderSearchItems(items []map[string]interface{}) {
	sort.SliceStable(items, func(i, j int) bool {
		si, _ := items[i]["score"].(float64)
		sj, _ := items[j]["score"].(float64)
		if si != sj {
			return si > sj
		}
		idI, keyI := searchItemOrderKey(items[i])
		idJ, keyJ := searchItemOrderKey(items[j])
		if idI != idJ {
			return idI < idJ
		}
		return keyI < keyJ
	})
}

// searchResultCap bounds items a search collects and renders. GitHub caps code
// and commit search at 1000; further matches are counted but not rendered, and
// the response carries incomplete_results=true.
const searchResultCap = 1000

// searchRelevanceScore assigns a deterministic score in [0,1]: a hit in primary
// outscores one only in secondary, exact equality beats containment, and
// multi-term queries average per-term contributions. No terms returns 1.0.
func searchRelevanceScore(terms []string, primary, secondary string) float64 {
	if len(terms) == 0 {
		return 1.0
	}
	p := strings.ToLower(primary)
	s := strings.ToLower(secondary)
	var sum float64
	for _, term := range terms {
		switch {
		case term == p:
			sum += 1.0
		case strings.Contains(p, term):
			sum += 0.9
		case term == s:
			sum += 0.6
		case strings.Contains(s, term):
			sum += 0.5
		default:
			sum += 0.3
		}
	}
	return sum / float64(len(terms))
}

// writeSearchEnvelope emits a search response with the paging Link header,
// whose total_count decides how many pages exist.
func writeSearchEnvelope(w http.ResponseWriter, r *http.Request, q searchQuery, envelope map[string]interface{}) {
	total, _ := envelope["total_count"].(int)
	setSearchLinkHeader(w, r, q.Page, q.PerPage, total)
	writeJSON(w, http.StatusOK, envelope)
}

func searchEnvelope(items []map[string]interface{}, totalCount int, incomplete bool, q searchQuery, sortBy searchSorter) map[string]interface{} {
	orderSearchItems(items)
	if sortBy != nil {
		items = sortBy(items, q.Sort, q.Order)
	}
	page, perPage := q.Page, q.PerPage
	available := len(items)
	start := (page - 1) * perPage
	if start < 0 || start > available {
		start = available
	}
	end := start + perPage
	if end > available {
		end = available
	}
	// items must marshal as [], never null: a nil slice would become null.
	pageItems := items[start:end]
	if pageItems == nil {
		pageItems = []map[string]interface{}{}
	}
	m := map[string]interface{}{
		"total_count":        totalCount,
		"incomplete_results": incomplete,
		"items":              pageItems,
	}
	return m
}

func sortSearchResults(items []map[string]interface{}, sortKey, order string) []map[string]interface{} {
	switch sortKey {
	case "created":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["created_at"].(string)
			b, _ := items[j]["created_at"].(string)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "updated":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["updated_at"].(string)
			b, _ := items[j]["updated_at"].(string)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "comments":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["comments"].(int)
			b, _ := items[j]["comments"].(int)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "reactions":
		sort.SliceStable(items, func(i, j int) bool {
			if order == "asc" {
				return itemReactionTotal(items[i]) < itemReactionTotal(items[j])
			}
			return itemReactionTotal(items[i]) > itemReactionTotal(items[j])
		})
	case "interactions":
		// interactions = reactions + comments.
		inter := func(m map[string]interface{}) int {
			c, _ := m["comments"].(int)
			return itemReactionTotal(m) + c
		}
		sort.SliceStable(items, func(i, j int) bool {
			if order == "asc" {
				return inter(items[i]) < inter(items[j])
			}
			return inter(items[i]) > inter(items[j])
		})
	}
	return items
}

// itemReactionTotal reads the reaction-rollup total_count off a rendered item.
func itemReactionTotal(m map[string]interface{}) int {
	r, _ := m["reactions"].(map[string]interface{})
	t, _ := r["total_count"].(int)
	return t
}

func sortRepoSearchResults(items []map[string]interface{}, sortKey, order string) []map[string]interface{} {
	switch sortKey {
	case "stars", "stargazers":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["stargazers_count"].(int)
			b, _ := items[j]["stargazers_count"].(int)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "forks":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["forks_count"].(int)
			b, _ := items[j]["forks_count"].(int)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "help-wanted-issues":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["_help_wanted_issues"].(int)
			b, _ := items[j]["_help_wanted_issues"].(int)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "created":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["created_at"].(string)
			b, _ := items[j]["created_at"].(string)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "updated":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["updated_at"].(string)
			b, _ := items[j]["updated_at"].(string)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	}
	for _, item := range items {
		delete(item, "_help_wanted_issues")
	}
	return items
}

func sortUserSearchResults(items []map[string]interface{}, sortKey, order string) []map[string]interface{} {
	switch sortKey {
	case "followers":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["followers"].(int)
			b, _ := items[j]["followers"].(int)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "created":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["created_at"].(string)
			b, _ := items[j]["created_at"].(string)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	case "updated":
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["updated_at"].(string)
			b, _ := items[j]["updated_at"].(string)
			if order == "asc" {
				return a < b
			}
			return a > b
		})
	}
	return items
}

// handleSearchCommits searches commit messages across every readable
// repository's git history, honoring repo:/user:/org:/author:/hash:.
func (s *Server) handleSearchCommits(w http.ResponseWriter, r *http.Request) {
	q, ok := searchQueryOrError(w, r, "commits")
	if !ok {
		return
	}

	if len(q.Terms) == 0 && q.Repo == "" && q.Author == "" && q.Hash == "" && q.User == "" && q.Org == "" {
		store.WriteGHValidationError(w, "Search", "q", "missing_field")
		return
	}

	// Gather repos and their git storages under the read lock; the log walk and
	// rendering happen after release (the helpers lock the store themselves).
	type commitSearchRepo struct {
		repo *store.Repo
		stor gitStorage.Storer
	}
	var searchRepos []commitSearchRepo
	var accessibleRepoIDs map[int]struct{}
	if q.Repo != "" {
		accessibleRepoIDs = s.searchAccessibleRepoIDForScope(r.Context(), q.Repo)
	} else {
		accessibleRepoIDs = s.searchAccessibleRepoIDs(r.Context())
	}
	s.store.Mu.RLock()
	for _, repo := range s.searchCandidateReposLocked(q.Repo) {
		if _, accessible := accessibleRepoIDs[repo.ID]; !accessible {
			continue
		}
		if q.Repo != "" && !strings.EqualFold(repo.FullName, q.Repo) {
			continue
		}
		if q.User != "" || q.Org != "" {
			owner, _, _ := strings.Cut(repo.FullName, "/")
			if q.User != "" && !strings.EqualFold(owner, q.User) {
				continue
			}
			if q.Org != "" && (repo.OwnerType != "Organization" || !strings.EqualFold(owner, q.Org)) {
				continue
			}
		}
		owner, _, _ := strings.Cut(repo.FullName, "/")
		if q.excludes("repo", repo.FullName) || q.excludes("user", owner) ||
			repo.OwnerType == "Organization" && q.excludes("org", owner) {
			continue
		}
		stor, ok := s.store.GitStorages[repo.FullName]
		if !ok {
			continue
		}
		searchRepos = append(searchRepos, commitSearchRepo{repo, stor})
	}
	s.store.Mu.RUnlock()
	// Fixed scan order: the 1000-result cap otherwise truncates a different
	// subset each request.
	sort.Slice(searchRepos, func(i, j int) bool { return searchRepos[i].repo.ID < searchRepos[j].repo.ID })

	base := s.baseURL(r)
	withTextMatches := acceptsTextMatch(r)
	var results []map[string]interface{}
	total := 0
	truncated := false

	for _, sr := range searchRepos {
		repo := sr.repo
		gr, err := git.Open(sr.stor, nil)
		if err != nil {
			continue
		}
		head, err := gr.Head()
		if err != nil {
			continue
		}
		iter, err := gr.Log(&git.LogOptions{From: head.Hash()})
		if err != nil {
			continue
		}
		err = iter.ForEach(func(commit *object.Commit) error {
			if !q.matchesText(commit.Message) {
				return nil
			}
			sha := commit.Hash.String()
			if q.Hash != "" && !strings.HasPrefix(sha, q.Hash) {
				return nil
			}
			for _, qualifier := range q.Qualifiers {
				if !qualifier.Negated {
					continue
				}
				if qualifier.Key == "hash" && strings.HasPrefix(sha, qualifier.Value) {
					return nil
				}
				if qualifier.Key == "author" && commitAuthorMatches(s.store, commit, qualifier.Value) {
					return nil
				}
			}
			if q.Author != "" && !commitAuthorMatches(s.store, commit, q.Author) {
				return nil
			}
			if !commitMatchesCommitQualifiers(s.store, commit, q) {
				return nil
			}
			// Count every match for total_count; only searchResultCap rows are
			// rendered, the rest reported via incomplete_results.
			total++
			if len(results) >= searchResultCap {
				truncated = true
				return nil
			}
			item := s.commitSearchItemJSON(commit, repo, base, q.Terms)
			if withTextMatches {
				objectURL, _ := item["url"].(string)
				item["text_matches"] = searchTextMatches(objectURL, "Commit", []searchTextMatchProperty{
					{"message", strings.TrimRight(commit.Message, "\n")},
				}, q.Terms)
			}
			results = append(results, item)
			return nil
		})
		if err != nil {
			s.logger.Debug().Err(err).Str("repo", repo.FullName).Msg("commit search log walk")
		}
	}

	writeSearchEnvelope(w, r, q, searchEnvelope(results, total, truncated, q, sortCommitSearchResults))
}

// commitAuthorMatches matches author: against the commit's git author name/email
// and the login of a store user with that email. Must not be called with st.mu
// held.
func commitAuthorMatches(st *store.Store, commit *object.Commit, author string) bool {
	if strings.EqualFold(commit.Author.Name, author) {
		return true
	}
	if strings.EqualFold(commit.Author.Email, author) {
		return true
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, u := range st.Users {
		if strings.EqualFold(u.Login, author) && strings.EqualFold(u.Email, commit.Author.Email) {
			return true
		}
	}
	return false
}

// commitCommitterMatches mirrors commitAuthorMatches for committer:. Must not be
// called with st.mu held.
func commitCommitterMatches(st *store.Store, commit *object.Commit, committer string) bool {
	if strings.EqualFold(commit.Committer.Name, committer) || strings.EqualFold(commit.Committer.Email, committer) {
		return true
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, u := range st.Users {
		if strings.EqualFold(u.Login, committer) && strings.EqualFold(u.Email, commit.Committer.Email) {
			return true
		}
	}
	return false
}

// commitMatchesCommitQualifiers applies the commit-search identity, date and
// merge qualifiers. author: and hash: are handled by the caller. Must not be
// called with st.mu held.
func commitMatchesCommitQualifiers(st *store.Store, commit *object.Commit, q searchQuery) bool {
	for _, ql := range q.Qualifiers {
		val := strings.Trim(ql.Value, `"`)
		var ok bool
		switch ql.Key {
		case "committer":
			ok = commitCommitterMatches(st, commit, val)
		case "author-name":
			ok = strings.Contains(strings.ToLower(commit.Author.Name), strings.ToLower(val))
		case "committer-name":
			ok = strings.Contains(strings.ToLower(commit.Committer.Name), strings.ToLower(val))
		case "author-email":
			ok = strings.EqualFold(commit.Author.Email, val)
		case "committer-email":
			ok = strings.EqualFold(commit.Committer.Email, val)
		case "author-date":
			c, err := parseDateSearchConstraint(val)
			ok = err == nil && c.matches(commit.Author.When)
		case "committer-date":
			c, err := parseDateSearchConstraint(val)
			ok = err == nil && c.matches(commit.Committer.When)
		case "merge":
			ok = (commit.NumParents() > 1) == strings.EqualFold(val, "true")
		default:
			continue
		}
		if ok == ql.Negated {
			return false
		}
	}
	return true
}

// commitSearchItemJSON renders the `commit-search-result-item` shape, scoring
// the commit subject as primary and the full message as secondary. Must not be
// called with the store lock held.
func (s *Server) commitSearchItemJSON(commit *object.Commit, repo *store.Repo, base string, terms []string) map[string]interface{} {
	sha := commit.Hash.String()
	api := base + "/api/v3/repos/" + repo.FullName

	// The account behind the commit author email, or null when none matches.
	var authorJSON interface{}
	s.store.Mu.RLock()
	for _, u := range s.store.Users {
		if u.Email != "" && strings.EqualFold(u.Email, commit.Author.Email) {
			authorJSON = store.UserToJSON(u, base)
			break
		}
	}
	s.store.Mu.RUnlock()

	parents := make([]map[string]interface{}, 0, len(commit.ParentHashes))
	for _, p := range commit.ParentHashes {
		parents = append(parents, map[string]interface{}{
			"sha":      p.String(),
			"url":      api + "/commits/" + p.String(),
			"html_url": base + "/" + repo.FullName + "/commit/" + p.String(),
		})
	}

	subject, _, _ := strings.Cut(commit.Message, "\n")

	return map[string]interface{}{
		"sha":          sha,
		"node_id":      "C_" + sha[:16],
		"url":          api + "/commits/" + sha,
		"html_url":     base + "/" + repo.FullName + "/commit/" + sha,
		"comments_url": api + "/commits/" + sha + "/comments",
		"commit": map[string]interface{}{
			"author": map[string]interface{}{
				"name":  commit.Author.Name,
				"email": commit.Author.Email,
				"date":  commit.Author.When.UTC().Format(time.RFC3339),
			},
			"committer": map[string]interface{}{
				"name":  commit.Committer.Name,
				"email": commit.Committer.Email,
				"date":  commit.Committer.When.UTC().Format(time.RFC3339),
			},
			"comment_count": 0,
			"message":       strings.TrimRight(commit.Message, "\n"),
			"tree": map[string]interface{}{
				"sha": commit.TreeHash.String(),
				"url": api + "/git/trees/" + commit.TreeHash.String(),
			},
			"url": api + "/git/commits/" + sha,
		},
		"author": authorJSON,
		"committer": map[string]interface{}{
			"name":  commit.Committer.Name,
			"email": commit.Committer.Email,
			"date":  commit.Committer.When.UTC().Format(time.RFC3339),
		},
		"parents":    parents,
		"repository": store.RepoToJSON(repo, s.store, base),
		"score":      searchRelevanceScore(terms, subject, commit.Message),
	}
}

// sortCommitSearchResults orders by author-date/committer-date; default is
// best-match order.
func sortCommitSearchResults(items []map[string]interface{}, sortKey, order string) []map[string]interface{} {
	if sortKey != "author-date" && sortKey != "committer-date" {
		return items
	}
	role := "author"
	if sortKey == "committer-date" {
		role = "committer"
	}
	dateOf := func(item map[string]interface{}) string {
		commit, _ := item["commit"].(map[string]interface{})
		gitUser, _ := commit[role].(map[string]interface{})
		d, _ := gitUser["date"].(string)
		return d
	}
	sort.SliceStable(items, func(i, j int) bool {
		if order == "asc" {
			return dateOf(items[i]) < dateOf(items[j])
		}
		return dateOf(items[i]) > dateOf(items[j])
	})
	return items
}

// handleSearchLabels searches the labels of the repository named by the required
// repository_id parameter.
func (s *Server) handleSearchLabels(w http.ResponseWriter, r *http.Request) {
	repoIDStr := r.URL.Query().Get("repository_id")
	if repoIDStr == "" {
		store.WriteGHValidationError(w, "Search", "repository_id", "missing_field")
		return
	}
	if r.URL.Query().Get("q") == "" {
		store.WriteGHValidationError(w, "Search", "q", "missing_field")
		return
	}
	repoID, err := strconv.Atoi(repoIDStr)
	if err != nil {
		store.WriteGHValidationError(w, "Search", "repository_id", "invalid")
		return
	}
	repo := s.store.GetRepoByID(repoID)
	if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	q, ok := searchQueryOrError(w, r, "labels")
	if !ok {
		return
	}

	s.store.Mu.RLock()
	labels := make([]*store.IssueLabel, 0)
	for _, l := range s.store.Labels {
		if l.RepoID != repo.ID {
			continue
		}
		if !q.matchesText(l.Name + " " + l.Description) {
			continue
		}
		labels = append(labels, l)
	}
	s.store.Mu.RUnlock()
	sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })

	base := s.baseURL(r)
	items := make([]map[string]interface{}, 0, len(labels))
	for _, l := range labels {
		items = append(items, map[string]interface{}{
			"id":          l.ID,
			"node_id":     l.NodeID,
			"url":         base + "/api/v3/repos/" + repo.FullName + "/labels/" + l.Name,
			"name":        l.Name,
			"color":       l.Color,
			"default":     l.Default,
			"description": nullOrString(l.Description),
			"score":       searchRelevanceScore(q.Terms, l.Name, l.Description),
		})
	}
	writeSearchEnvelope(w, r, q, searchEnvelope(items, len(items), false, q, nil))
}

// handleSearchTopics searches the topics applied to readable repositories.
func (s *Server) handleSearchTopics(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("q") == "" {
		store.WriteGHValidationError(w, "Search", "q", "missing_field")
		return
	}
	q, ok := searchQueryOrError(w, r, "topics")
	if !ok {
		return
	}

	type topicAgg struct {
		name      string
		count     int
		createdAt time.Time
		updatedAt time.Time
	}
	accessibleRepoIDs := s.searchAccessibleRepoIDs(r.Context())
	s.store.Mu.RLock()
	agg := map[string]*topicAgg{}
	for _, repo := range s.store.Repos {
		if _, accessible := accessibleRepoIDs[repo.ID]; !accessible {
			continue
		}
		for _, topic := range repo.Topics {
			if !q.matchesText(topic) {
				continue
			}
			t := agg[topic]
			if t == nil {
				t = &topicAgg{name: topic, createdAt: repo.CreatedAt, updatedAt: repo.UpdatedAt}
				agg[topic] = t
			}
			t.count++
			if repo.CreatedAt.Before(t.createdAt) {
				t.createdAt = repo.CreatedAt
			}
			if repo.UpdatedAt.After(t.updatedAt) {
				t.updatedAt = repo.UpdatedAt
			}
		}
	}
	s.store.Mu.RUnlock()

	topics := make([]*topicAgg, 0, len(agg))
	for _, t := range agg {
		topics = append(topics, t)
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].name < topics[j].name })

	items := make([]map[string]interface{}, 0, len(topics))
	for _, t := range topics {
		items = append(items, map[string]interface{}{
			"name":              t.name,
			"display_name":      nil,
			"short_description": nil,
			"description":       nil,
			"created_by":        nil,
			"released":          nil,
			"created_at":        t.createdAt.UTC().Format(time.RFC3339),
			"updated_at":        t.updatedAt.UTC().Format(time.RFC3339),
			"featured":          false,
			"curated":           false,
			"score":             searchRelevanceScore(q.Terms, t.name, ""),
			"repository_count":  t.count,
		})
	}
	writeSearchEnvelope(w, r, q, searchEnvelope(items, len(items), false, q, nil))
}
