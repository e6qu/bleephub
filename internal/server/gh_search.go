package bleephub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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

// searchAccessibleRepoIDs snapshots the repositories that the request
// credential may search. Search is unusual among repository-backed APIs:
// GitHub App installation tokens may call every search endpoint without an
// Issues, Pull requests, or Contents grant. Repository reach is still
// enforced, though, so private results are limited to the installation and any
// narrower repository selection on the token. Metadata read is implicit on
// every installation and is the common reach check for this surface.
//
// This must run outside a caller-held store lock. viewerCanReadRepo follows the
// installation, user-to-server, and fine-grained PAT selections and takes its
// own store locks.
func (s *Server) searchAccessibleRepoIDs(ctx context.Context) map[int]struct{} {
	s.store.mu.RLock()
	repositories := make([]*Repo, 0, len(s.store.Repos))
	for _, repo := range s.store.Repos {
		snapshot := *repo
		repositories = append(repositories, &snapshot)
	}
	s.store.mu.RUnlock()

	accessible := make(map[int]struct{}, len(repositories))
	for _, repo := range repositories {
		if s.viewerCanReadRepo(ctx, repo) {
			accessible[repo.ID] = struct{}{}
		}
	}
	return accessible
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

// unsupportedQualifierError names a qualifier — or a qualifier value — that
// the search parser does not implement. Silently dropping one would widen the
// result set to the unfiltered universe, so it is a 422 instead.
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

// writeGHSearchQualifierError writes GitHub's 422 for an unparseable search
// query, carrying the offending qualifier in the errors array.
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

// searchQueryOrError is the only way a handler obtains a searchQuery: it
// rejects queries carrying qualifiers this server does not implement rather
// than dropping them and answering with the unfiltered universe.
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
					case "name", "description", "topics", "readme":
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
				default:
					return q, unsupportedQualifierError{key: "type", value: val}
				}
			case "author":
				if !negated {
					q.Author = val
				}
			case "hash":
				if !negated {
					q.Hash = val
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
			case "size", "followers", "forks", "stars", "topics", "good-first-issues", "help-wanted-issues":
				if _, err := parseNumericSearchConstraint(val); err != nil {
					return q, unsupportedQualifierError{key: key, value: val}
				}
			case "created", "pushed":
				if _, err := parseDateSearchConstraint(val); err != nil {
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
	accessibleRepoIDs := s.searchAccessibleRepoIDs(r.Context())

	// Matching rows are gathered under the read lock; rendering happens
	// after release because the JSON builders (issueToJSON, repoToJSON and
	// friends) take the store lock themselves.
	type issueRow struct {
		issue *Issue
		repo  *Repo
		assoc string
	}
	type prRow struct {
		pr    *PullRequest
		repo  *Repo
		assoc string
	}
	var issueRows []issueRow
	var prRows []prRow

	s.store.mu.RLock()

	for _, issue := range s.store.Issues {
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
		if q.Label != "" {
			if !issueHasLabelNames(s.store, issue, []string{q.Label}) {
				continue
			}
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
		text := issue.Title + " " + issue.Body
		if q.InTitle && !q.InBody {
			text = issue.Title
		} else if q.InBody && !q.InTitle {
			text = issue.Body
		}
		if !q.matchesText(text) {
			continue
		}

		issueRows = append(issueRows, issueRow{issue, repo, authorAssociationLocked(s.store, issue.AuthorID, repo)})
	}

	for _, pr := range s.store.PullRequests {
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
		if q.Label != "" {
			if !prHasLabelNames(s.store, pr, []string{q.Label}) {
				continue
			}
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
		text := pr.Title + " " + pr.Body
		if q.InTitle && !q.InBody {
			text = pr.Title
		} else if q.InBody && !q.InTitle {
			text = pr.Body
		}
		if !q.matchesText(text) {
			continue
		}

		prRows = append(prRows, prRow{pr, repo, authorAssociationLocked(s.store, pr.AuthorID, repo)})
	}

	s.store.mu.RUnlock()

	base := s.baseURL(r)

	// Unify matched issue and PR rows so the whole result set can be sorted and
	// paginated *before* rendering — rendering every matched row (issueToJSON +
	// repoToJSON per row) only to return one page is the dominant per-request
	// cost on a large corpus.
	rows := make([]searchIssueRow, 0, len(issueRows)+len(prRows))
	for _, row := range issueRows {
		rows = append(rows, searchIssueRow{issue: row.issue, repo: row.repo, assoc: row.assoc})
	}
	for _, row := range prRows {
		rows = append(rows, searchIssueRow{pr: row.pr, repo: row.repo, assoc: row.assoc})
	}

	render := func(row searchIssueRow) map[string]interface{} {
		if row.issue != nil {
			item := issueToJSON(row.issue, s.store, base, row.repo.FullName)
			// Search returns issue-search-result-item rather than the richer
			// issue response used by single-issue operations.
			delete(item, "closed_by")
			item["score"] = searchRelevanceScore(q.Terms, row.issue.Title, row.issue.Body)
			item["author_association"] = row.assoc
			item["draft"] = false
			item["pull_request"] = nil
			item["repository"] = repoToJSON(row.repo, s.store, base)
			return item
		}
		item := issueToJSONForPR(row.pr, s.store, base, row.repo.FullName)
		delete(item, "closed_by")
		item["score"] = searchRelevanceScore(q.Terms, row.pr.Title, row.pr.Body)
		item["author_association"] = row.assoc
		item["repository"] = repoToJSON(row.repo, s.store, base)
		return item
	}

	total := len(rows)

	// The "comments" sort key needs a rendered comment count per row, so it
	// keeps the render-all path (rare). created/updated/best-match sort only on
	// row timestamps, so those render just the requested page.
	if q.Sort == "comments" {
		results := make([]map[string]interface{}, 0, total)
		for _, row := range rows {
			results = append(results, render(row))
		}
		writeJSON(w, http.StatusOK, searchEnvelope("issues", results, len(results), false, q, sortSearchResults))
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
		"search_type":        "issues",
	}
	writeJSON(w, http.StatusOK, out)
}

// searchIssueRow is a matched issue or PR gathered by the issue-search scan,
// carrying just enough to sort and then render only the requested page.
type searchIssueRow struct {
	issue *Issue
	pr    *PullRequest
	repo  *Repo
	assoc string
}

func (row searchIssueRow) createdAt() time.Time {
	if row.issue != nil {
		return row.issue.CreatedAt
	}
	return row.pr.CreatedAt
}

func (row searchIssueRow) updatedAt() time.Time {
	if row.issue != nil {
		return row.issue.UpdatedAt
	}
	return row.pr.UpdatedAt
}

// orderKey mirrors searchItemOrderKey for an unrendered row: the entity id
// plus its issue path, which is unique because issues and pull requests draw
// numbers from one per-repository counter.
func (row searchIssueRow) orderKey() (int, string) {
	id, number := 0, 0
	if row.issue != nil {
		id, number = row.issue.ID, row.issue.Number
	} else {
		id, number = row.pr.ID, row.pr.Number
	}
	return id, "/api/v3/repos/" + row.repo.FullName + "/issues/" + strconv.Itoa(number)
}

// sortSearchRows establishes the total row order — the deterministic base
// order that map iteration does not provide, then the requested sort stably on
// top — by the same keys as sortSearchResults, so paginate-before-render
// yields the identical page a render-all-then-sort would.
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

// searchPageBounds computes the [start,end) slice bounds for the given page.
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

func issueToJSONForPR(pr *PullRequest, st *Store, baseURL, repoFullName string) map[string]interface{} {
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

// issueToJSONForPullRequest renders a pull request in the issue shape.
// Must not be called with st.mu held: it takes the read lock itself and
// derives milestone issue counts via milestoneToJSON, which locks too.
func issueToJSONForPullRequest(pr *PullRequest, st *Store, baseURL, repoFullName string) map[string]interface{} {
	pr = st.snapPR(pr)
	st.mu.RLock()
	authorJSON := userToJSON(actorUserLocked(st, pr.AuthorID))
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
			assignees = append(assignees, userToJSON(u))
		}
	}
	var assignee interface{}
	if len(assignees) > 0 {
		assignee = assignees[0]
	}
	// Grab the milestone pointer; conversion happens after unlock because
	// milestoneToJSON derives issue counts under its own lock.
	var milestone *Milestone
	if pr.MilestoneID > 0 {
		milestone = st.Milestones[pr.MilestoneID]
	}
	commentCount := st.countCommentsForLocked("pull_request", pr.ID)
	st.mu.RUnlock()

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
		"state_reason":       "",
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
		"closed_by":          pullRequestClosedByJSON(st, pr),
		"author_association": authorAssociation(st, pr.AuthorID, repo),
		"draft":              pr.IsDraft,
	}
}

func pullRequestClosedByJSON(st *Store, pr *PullRequest) interface{} {
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
	st.mu.RLock()
	defer st.mu.RUnlock()
	actor := actorUserLocked(st, actorID)
	if actor == nil {
		return nil
	}
	return userToJSON(actor)
}

// authorAssociation returns the author_association value for the author of
// an issue, pull request or comment within repo. Must not be called with
// st.mu held; it takes the read lock itself.
func authorAssociation(st *Store, authorID int, repo *Repo) string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return authorAssociationLocked(st, authorID, repo)
}

// authorAssociationLocked is authorAssociation for callers that already hold
// st.mu; it never acquires the lock itself.
func authorAssociationLocked(st *Store, authorID int, repo *Repo) string {
	author := st.Users[authorID]
	if author == nil {
		return "NONE"
	}
	if repo.Owner != nil && repo.Owner.ID == author.ID {
		return "OWNER"
	}
	if repo.Owner != nil && repo.Owner.Type == "Organization" {
		if membership := st.Memberships[membershipKey(repo.Owner.Login, author.ID)]; membership != nil &&
			membership.State == MembershipStateActive {
			return "MEMBER"
		}
	}
	if collaborators := st.RepoCollaborators[repo.FullName]; collaborators != nil {
		if collaborators[author.Login] != "" {
			return "COLLABORATOR"
		}
	}
	for _, pr := range st.PullRequests {
		if pr.RepoID == repo.ID && pr.AuthorID == author.ID && pr.State == "MERGED" {
			return "CONTRIBUTOR"
		}
	}
	return "NONE"
}

// issueHasLabelNames reports whether the issue carries every named label.
// Callers hold st.mu (it reads st.Labels directly).
func issueHasLabelNames(st *Store, issue *Issue, names []string) bool {
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

// prHasLabelNames reports whether the pull request carries every named
// label. Callers hold st.mu (it reads st.Labels directly).
func prHasLabelNames(st *Store, pr *PullRequest, names []string) bool {
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

func (s *Server) handleSearchRepositories(w http.ResponseWriter, r *http.Request) {
	q, ok := searchQueryOrError(w, r, "repositories")
	if !ok {
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("q")) == "" {
		writeGHValidationError(w, "Search", "q", "missing_field")
		return
	}
	switch q.Sort {
	case "", "stars", "forks", "help-wanted-issues", "updated":
	default:
		writeGHValidationError(w, "Search", "sort", "invalid")
		return
	}
	if q.Order != "" && q.Order != "asc" && q.Order != "desc" {
		writeGHValidationError(w, "Search", "order", "invalid")
		return
	}

	// Take private snapshots of readable repositories under the store lock,
	// then evaluate storage-backed qualifiers (size, README and funding file)
	// after releasing it. repoToJSON and several qualifier counters lock the
	// store themselves.
	var candidates []*Repo
	accessibleRepoIDs := s.searchAccessibleRepoIDs(r.Context())
	s.store.mu.RLock()
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
	s.store.mu.RUnlock()

	var matched []*Repo
	for _, repo := range candidates {
		if repositoryMatchesSearch(s.store, repo, q) {
			matched = append(matched, repo)
		}
	}

	base := s.baseURL(r)
	var results []map[string]interface{}
	for _, repo := range matched {
		item := repoToJSON(repo, s.store, base)
		item["score"] = searchRelevanceScore(q.Terms, repo.Name, repo.Description)
		item["_help_wanted_issues"] = repoIssueLabelCount(s.store, repo.ID, "help wanted")
		results = append(results, item)
	}

	writeJSON(w, http.StatusOK, searchEnvelope("", results, len(results), false, q, sortRepoSearchResults))
}

func repoIssueLabelCount(st *Store, repoID int, labelName string) int {
	st.mu.RLock()
	defer st.mu.RUnlock()
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
		writeGHValidationError(w, "Search", "q", "missing_field")
		return
	}

	// Readable repos and their git storages are gathered under the read
	// lock; the tree walk and rendering happen after release because
	// repoToJSON takes the store lock itself.
	type codeSearchRepo struct {
		repo *Repo
		stor gitStorage.Storer
	}
	var searchRepos []codeSearchRepo
	accessibleRepoIDs := s.searchAccessibleRepoIDs(r.Context())
	s.store.mu.RLock()
	for _, repo := range s.store.Repos {
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
	s.store.mu.RUnlock()
	// Scan repositories in a fixed order: the 1000-result cap below otherwise
	// truncates a different subset on every request.
	sort.Slice(searchRepos, func(i, j int) bool { return searchRepos[i].repo.ID < searchRepos[j].repo.ID })

	base := s.baseURL(r)
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
							content := strings.ToLower(string(data))
							if pathMatches(content, q.Terms) || pathMatches(strings.ToLower(path), q.Terms) {
								matched = true
							}
						}
					}
				}
			}
			if !matched {
				return nil
			}

			// Count every match so total_count reflects the full result set;
			// only the first searchResultCap rows are rendered, and the rest
			// is reported via incomplete_results.
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
				"repository": repoToJSON(repo, s.store, base),
				"score":      searchRelevanceScore(q.Terms, name, path),
				"language":   detectLanguage(name),
			}
			results = append(results, item)
			return nil
		})
		if err != nil {
			s.logger.Debug().Err(err).Str("repo", repo.FullName).Msg("code search tree walk")
		}
	}

	writeJSON(w, http.StatusOK, searchEnvelope("", results, total, truncated, q, nil))
}

func (s *Server) handleSearchUsers(w http.ResponseWriter, r *http.Request) {
	q, ok := searchQueryOrError(w, r, "users")
	if !ok {
		return
	}

	// Matching users and orgs are gathered under the read lock; rendering
	// happens after release because fullUserJSON derives follower and repo
	// counts under the store and misc locks itself.
	var users []*User
	var orgs []*Org
	s.store.mu.RLock()
	for _, u := range s.store.Users {
		if q.Type == "org" {
			continue
		}
		if q.excludes("type", "user") {
			continue
		}
		text := u.Login + " " + u.Name + " " + u.Bio
		if !q.matchesText(text) {
			continue
		}
		users = append(users, u)
	}
	for _, org := range s.store.Orgs {
		if q.Type == "user" {
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
	s.store.mu.RUnlock()

	var results []map[string]interface{}
	for _, u := range users {
		item := s.fullUserJSON(u)
		// user-search-result-item carries no twitter_username member.
		delete(item, "twitter_username")
		item["score"] = searchRelevanceScore(q.Terms, u.Login, u.Name+" "+u.Bio)
		results = append(results, item)
	}
	for _, org := range orgs {
		item := orgAsSimpleUserJSON(org)
		item["score"] = searchRelevanceScore(q.Terms, org.Login, org.Name+" "+org.Description)
		results = append(results, item)
	}

	writeJSON(w, http.StatusOK, searchEnvelope("", results, len(results), false, q, sortUserSearchResults))
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

// searchSorter applies a requested sort key to already-ordered results. Every
// implementation must sort stably so the base order survives as the tiebreak.
type searchSorter func(items []map[string]interface{}, sortKey, order string) []map[string]interface{}

// searchItemOrderKey is the identity a search result is tiebroken on: its id,
// then the first globally unique string member it carries. Every rendered item
// has one, so the resulting order is total.
func searchItemOrderKey(item map[string]interface{}) (int, string) {
	id, _ := item["id"].(int)
	for _, member := range []string{"url", "node_id", "name"} {
		if s, ok := item[member].(string); ok && s != "" {
			return id, s
		}
	}
	return id, ""
}

// orderSearchItems imposes the total result order — score descending, then
// identity ascending — that map iteration does not provide. Without it,
// slicing a page out of the result set overlaps and omits results between
// requests for the same query.
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

// searchResultCap bounds the number of items a search collects and renders.
// GitHub caps code and commit search at 1000 results; once that many rows
// have been gathered the remaining matches are counted but not rendered, and
// the response carries incomplete_results=true with a total_count reflecting
// the full match set rather than the truncated slice.
const searchResultCap = 1000

// searchRelevanceScore assigns a deterministic relevance score in [0,1] to a
// free-text search match. A hit in primary — the highest-ranked field, such
// as a repository name, issue title or user login — outscores one only in
// secondary (description, body or bio), and exact equality beats substring
// containment. Multi-term queries average the per-term contributions, so a
// document matching every term ranks above one matching only some.
// Qualifier-only queries (no free-text terms) treat every hit as equally
// relevant and return 1.0.
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

// searchEnvelope orders, sorts and pages a search result set. Ordering is not
// optional: it happens here so no handler can slice an unordered set.
// totalCount is the size of the full match set (before truncation); items is
// the rendered slice, which may be shorter when a handler caps collection.
// incomplete reports whether items was truncated below totalCount.
func searchEnvelope(searchType string, items []map[string]interface{}, totalCount int, incomplete bool, q searchQuery, sortBy searchSorter) map[string]interface{} {
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
	// GitHub's search envelope always carries items as an array; an empty
	// result set is [], never null. Slicing a nil source (no matches) yields a
	// nil slice that would marshal to null, so materialize an empty array.
	pageItems := items[start:end]
	if pageItems == nil {
		pageItems = []map[string]interface{}{}
	}
	m := map[string]interface{}{
		"total_count":        totalCount,
		"incomplete_results": incomplete,
		"items":              pageItems,
	}
	if searchType != "" {
		m["search_type"] = searchType
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
	}
	return items
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

// handleSearchCommits implements GET /search/commits: a real search across
// the git commit history of every repository the caller can read, matching
// query terms against commit messages with repo:/user:/org:/author:/hash:
// qualifiers.
func (s *Server) handleSearchCommits(w http.ResponseWriter, r *http.Request) {
	q, ok := searchQueryOrError(w, r, "commits")
	if !ok {
		return
	}

	if len(q.Terms) == 0 && q.Repo == "" && q.Author == "" && q.Hash == "" && q.User == "" && q.Org == "" {
		writeGHValidationError(w, "Search", "q", "missing_field")
		return
	}

	// Readable repos and their git storages are gathered under the read
	// lock; the log walk and rendering happen after release because
	// commitSearchItemJSON and commitAuthorMatches take the store lock
	// themselves.
	type commitSearchRepo struct {
		repo *Repo
		stor gitStorage.Storer
	}
	var searchRepos []commitSearchRepo
	accessibleRepoIDs := s.searchAccessibleRepoIDs(r.Context())
	s.store.mu.RLock()
	for _, repo := range s.store.Repos {
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
	s.store.mu.RUnlock()
	// Scan repositories in a fixed order: the 1000-result cap below otherwise
	// truncates a different subset on every request.
	sort.Slice(searchRepos, func(i, j int) bool { return searchRepos[i].repo.ID < searchRepos[j].repo.ID })

	base := s.baseURL(r)
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
			// Count every match so total_count reflects the full result set;
			// only the first searchResultCap rows are rendered, and the rest
			// is reported via incomplete_results.
			total++
			if len(results) >= searchResultCap {
				truncated = true
				return nil
			}
			results = append(results, s.commitSearchItemJSON(commit, repo, base, q.Terms))
			return nil
		})
		if err != nil {
			s.logger.Debug().Err(err).Str("repo", repo.FullName).Msg("commit search log walk")
		}
	}

	writeJSON(w, http.StatusOK, searchEnvelope("", results, total, truncated, q, sortCommitSearchResults))
}

// commitAuthorMatches matches the author: qualifier against the commit's
// git author name/email and against the login of a store user with the
// commit author's email. Must not be called with st.mu held; it takes the
// read lock itself.
func commitAuthorMatches(st *Store, commit *object.Commit, author string) bool {
	if strings.EqualFold(commit.Author.Name, author) {
		return true
	}
	if strings.EqualFold(commit.Author.Email, author) {
		return true
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, u := range st.Users {
		if strings.EqualFold(u.Login, author) && strings.EqualFold(u.Email, commit.Author.Email) {
			return true
		}
	}
	return false
}

// commitSearchItemJSON renders the spec `commit-search-result-item` shape.
// Must not be called with the store lock held: it resolves the author under
// its own read lock and embeds the repository via repoToJSON, which derives
// counters under the store lock itself. terms drives the relevance score:
// the commit subject (first line) is the primary field and the full message
// the secondary one.
func (s *Server) commitSearchItemJSON(commit *object.Commit, repo *Repo, base string, terms []string) map[string]interface{} {
	sha := commit.Hash.String()
	api := base + "/api/v3/repos/" + repo.FullName

	// The top-level author is the GitHub account behind the commit author
	// email (null when the email matches no account).
	var authorJSON interface{}
	s.store.mu.RLock()
	for _, u := range s.store.Users {
		if u.Email != "" && strings.EqualFold(u.Email, commit.Author.Email) {
			authorJSON = userToJSON(u)
			break
		}
	}
	s.store.mu.RUnlock()

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
		"repository": repoToJSON(repo, s.store, base),
		"score":      searchRelevanceScore(terms, subject, commit.Message),
	}
}

// sortCommitSearchResults orders commit results for the documented sort
// keys (author-date, committer-date); default is best-match order.
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

// handleSearchLabels implements GET /search/labels: a real search over the
// labels of the repository named by the required repository_id parameter.
func (s *Server) handleSearchLabels(w http.ResponseWriter, r *http.Request) {
	repoIDStr := r.URL.Query().Get("repository_id")
	if repoIDStr == "" {
		writeGHValidationError(w, "Search", "repository_id", "missing_field")
		return
	}
	if r.URL.Query().Get("q") == "" {
		writeGHValidationError(w, "Search", "q", "missing_field")
		return
	}
	repoID, err := strconv.Atoi(repoIDStr)
	if err != nil {
		writeGHValidationError(w, "Search", "repository_id", "invalid")
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

	s.store.mu.RLock()
	labels := make([]*IssueLabel, 0)
	for _, l := range s.store.Labels {
		if l.RepoID != repo.ID {
			continue
		}
		if !q.matchesText(l.Name + " " + l.Description) {
			continue
		}
		labels = append(labels, l)
	}
	s.store.mu.RUnlock()
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
	writeJSON(w, http.StatusOK, searchEnvelope("", items, len(items), false, q, nil))
}

// handleSearchTopics implements GET /search/topics: a real search over the
// topics applied to repositories the caller can read.
func (s *Server) handleSearchTopics(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("q") == "" {
		writeGHValidationError(w, "Search", "q", "missing_field")
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
	s.store.mu.RLock()
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
	s.store.mu.RUnlock()

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
	writeJSON(w, http.StatusOK, searchEnvelope("", items, len(items), false, q, nil))
}
