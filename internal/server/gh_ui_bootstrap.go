package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Browser-only page-bootstrap aggregations. The SPA's repo home, issue detail,
// PR detail, and insights pages each fan out into 16-23 REST calls; these
// endpoints collapse each page into one request. Real GitHub has no such REST
// operations (github.com hydrates its pages server-side), so they live under
// /ui-data — inventing them under /api/v3 is a defect the route-definition
// gates reject. (`s.route` auto-wraps /ui-data patterns with
// authenticateUIData.)
//
// Every sub-payload is produced by executing the SAME registered handler the
// standalone endpoint runs (an in-process sub-request through the identical
// authorization chain, including the requirePerm wrapper where the standalone
// route has one), so each sub-object is byte-identical to the standalone
// endpoint's body and the SPA can hydrate its TanStack Query caches from it.
// List sub-payloads are the standalone endpoint's FIRST page with default
// pagination (per_page=30, no Link headers are forwarded). A nullable
// sub-resource whose standalone endpoint would answer non-2xx (missing README,
// no releases, empty repo) is null; a list whose standalone endpoint answers
// 204 (contributors on an empty repo) is []. Repo resolution and 404 semantics
// are those of GET /api/v3/repos/{owner}/{repo}: unknown repos and private
// repos the viewer cannot read are an identical 404, before any sub-request
// runs.
//
// Response key schema (keys are stable; the value shapes are owned by the
// standalone endpoints named in each comment):
//
//	GET /ui-data/bootstrap/repos/{owner}/{repo}
//	  repo                 GET /api/v3/repos/{o}/{r}
//	  readme               GET .../readme                        (null when 404)
//	  root_entries         GET .../contents/ (default ref root)  (null when unavailable)
//	  branches             { first_page: GET .../branches, total_count: int }
//	  tags                 { first_page: GET .../tags,     total_count: int }
//	  languages            GET .../languages
//	  contributors         GET .../contributors                  ([] when 204)
//	  latest_release       GET .../releases/latest               (null when 404)
//	  latest_commit        GET .../commits/{default_branch}      (null on empty repo)
//	  pulls_open_count     int — open pull requests
//	  issues_open_count    int — open issues EXCLUDING pull requests
//	  discussions_enabled  bool
//
//	GET /ui-data/repos/{owner}/{repo}/tree-meta?ref={ref}&path={dir}
//	  ref, path            echoed (ref defaults to the default branch)
//	  latest_commit        commits-list-shaped commit JSON: the newest commit
//	                       touching {dir} (the tip itself for the root)
//	  entries              [{ name, path, type(file|dir|symlink|submodule),
//	                         size, latest: { sha, message_headline,
//	                         author_login, author_date } | null }]
//
//	GET /ui-data/bootstrap/repos/{owner}/{repo}/issues/{number}
//	  issue                GET .../issues/{number}
//	  comments             GET .../issues/{number}/comments
//	  timeline             GET .../issues/{number}/timeline
//	  labels               GET .../labels (repo labels)
//	  milestones           GET .../milestones?state=open
//	  assignees_available  GET .../assignees
//
//	GET /ui-data/bootstrap/repos/{owner}/{repo}/pulls/{number}
//	  pull                 GET .../pulls/{number} (includes diff stats)
//	  timeline             GET .../issues/{number}/timeline
//	  comments             GET .../issues/{number}/comments (conversation)
//	  reviews              GET .../pulls/{number}/reviews
//	  review_comments      GET .../pulls/{number}/comments
//	  requested_reviewers  GET .../pulls/{number}/requested_reviewers
//	  check_runs           GET .../commits/{head_sha}/check-runs
//	  combined_status      GET .../commits/{head_sha}/status
//	  files_summary        { changed_files, additions, deletions } (from the pull)
//
//	GET /ui-data/bootstrap/repos/{owner}/{repo}/insights?period={24h|3d|1w|1m}
//	  period               echoed (defaults to 1w; anything else is a 422)
//	  merged_prs_count     PRs with merged_at inside the window
//	  opened_prs_count     PRs with created_at inside the window
//	  closed_issues_count  issues with closed_at inside the window
//	  new_issues_count     issues with created_at inside the window
//	  active_contributors  distinct default-branch commit authors in the window
//	  top_contributors     first 10 of [{ login, commits }] (commits desc; login
//	                       is the resolved account, else the raw author name)
//	  commit_activity      GET .../stats/commit_activity (52 Sunday weeks)
//	  languages            GET .../languages
func (s *Server) registerGHUIBootstrapRoutes() {
	s.route("GET /ui-data/bootstrap/repos/{owner}/{repo}", s.handleUIBootstrapRepo)
	s.route("GET /ui-data/repos/{owner}/{repo}/tree-meta", s.handleUITreeMeta)
	s.route("GET /ui-data/bootstrap/repos/{owner}/{repo}/issues/{number}", s.handleUIBootstrapIssue)
	s.route("GET /ui-data/bootstrap/repos/{owner}/{repo}/pulls/{number}", s.handleUIBootstrapPull)
	s.route("GET /ui-data/bootstrap/repos/{owner}/{repo}/insights", s.handleUIBootstrapInsights)
}

// uiConditionalWriter gives these GET endpoints the same strong-ETag +
// If-None-Match 304 semantics the REST middleware provides: that layer only
// activates for /api/ paths, so /ui-data responses would otherwise ship
// unvalidated. writeJSON discovers conditionalJSON by walking the writer
// chain, so wrapping the outer writer in each handler is sufficient — the
// gzip layer sits outside and is unaffected.
type uiConditionalWriter struct {
	http.ResponseWriter
	ifNoneMatch string
}

func (w *uiConditionalWriter) conditionalJSON(etag string, status int) bool {
	if status != http.StatusOK {
		return false
	}
	w.Header().Set("ETag", etag)
	return etagMatches(w.ifNoneMatch, etag)
}

// uiConditional wraps a response writer for the aggregate's final writeJSON.
func uiConditional(w http.ResponseWriter, r *http.Request) http.ResponseWriter {
	return &uiConditionalWriter{ResponseWriter: w, ifNoneMatch: r.Header.Get("If-None-Match")}
}

// bufferedResponse captures one in-process sub-request's response. It
// deliberately implements neither Unwrap nor conditionalJSON, so the inner
// writeJSON emits plain identity bytes: only the OUTER response is ETag'd
// (uiConditionalWriter) and gzipped (compressionMiddleware covers /ui-data/).
type bufferedResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header {
	if b.header == nil {
		b.header = make(http.Header)
	}
	return b.header
}

func (b *bufferedResponse) WriteHeader(status int) { b.status = status }

func (b *bufferedResponse) Write(p []byte) (int, error) { return b.buf.Write(p) }

// body returns the captured body without writeJSON's trailing newline, so it
// embeds into the aggregate as a json.RawMessage byte-identical to the
// standalone endpoint's JSON document.
func (b *bufferedResponse) body() []byte {
	return bytes.TrimSuffix(b.buf.Bytes(), []byte("\n"))
}

// uiSubGET executes an already-registered GET handler in-process against a
// synthetic request that mirrors the standalone endpoint: the real /api/v3
// URL path (so requirePerm's path-derived checks evaluate exactly as they
// would on the standalone route), the caller's authenticated context, and any
// extra path values the inner handler reads. The outer /ui-data pattern's
// {owner}/{repo} (and {number}, where present) survive the clone; extras are
// supplied via pathValues.
func uiSubGET(r *http.Request, handler http.HandlerFunc, path string, query url.Values, pathValues map[string]string) *bufferedResponse {
	req := r.Clone(r.Context())
	req.Method = http.MethodGet
	req.Body = http.NoBody
	rawQuery := ""
	if query != nil {
		rawQuery = query.Encode()
	}
	req.URL = &url.URL{Path: path, RawQuery: rawQuery}
	req.RequestURI = ""
	// The inner handlers vary their representation on Accept
	// (vnd.github.sha/diff/patch); the aggregate always embeds JSON.
	req.Header = r.Header.Clone()
	req.Header.Set("Accept", "application/json")
	for name, value := range pathValues {
		req.SetPathValue(name, value)
	}
	resp := &bufferedResponse{status: http.StatusOK}
	handler(resp, req)
	return resp
}

// uiSubListTotal reruns a paginated list handler with per_page=1 and reads the
// total off the rel="last" Link target (lastPage == total when per_page is 1).
// The count therefore reflects exactly what the standalone endpoint would
// enumerate — no re-derivation of its filtering. A response without a Link
// header holds the entire list (0 or 1 items).
func uiSubListTotal(r *http.Request, handler http.HandlerFunc, path string, query url.Values, pathValues map[string]string) int {
	q := url.Values{}
	for name, values := range query {
		q[name] = values
	}
	q.Set("per_page", "1")
	q.Set("page", "1")
	resp := uiSubGET(r, handler, path, q, pathValues)
	if resp.status != http.StatusOK {
		return 0
	}
	for _, part := range strings.Split(resp.Header().Get("Link"), ",") {
		if !strings.Contains(part, `rel="last"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end <= start {
			continue
		}
		target, err := url.Parse(part[start+1 : end])
		if err != nil {
			continue
		}
		if page, err2 := strconv.Atoi(target.Query().Get("page")); err2 == nil && page > 0 {
			return page
		}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(resp.body(), &items); err != nil {
		return 0
	}
	return len(items)
}

// uiSubJSONOrNull embeds a 2xx sub-response verbatim; anything else becomes
// JSON null ("resource unavailable" — a missing README, no releases yet, an
// empty repository).
func uiSubJSONOrNull(resp *bufferedResponse) json.RawMessage {
	if resp.status < 200 || resp.status > 299 || resp.buf.Len() == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(resp.body())
}

// uiSubJSONOrEmptyList is uiSubJSONOrNull for list sub-resources whose
// standalone endpoint can answer 204 with no body (contributors on an empty
// repository): the aggregate embeds [] so list consumers always see a list.
func uiSubJSONOrEmptyList(resp *bufferedResponse) json.RawMessage {
	out := uiSubJSONOrNull(resp)
	if string(out) == "null" {
		return json.RawMessage("[]")
	}
	return out
}

// uiRelaySubResponse forwards a failed core sub-response (status and body)
// verbatim, so the aggregate refuses exactly where and how the standalone
// endpoint refuses.
func uiRelaySubResponse(w http.ResponseWriter, resp *bufferedResponse) {
	if ct := resp.Header().Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.buf.Bytes())
}

func (s *Server) handleUIBootstrapRepo(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	api := "/api/v3/repos/" + repo.FullName

	repoResp := uiSubGET(r, s.handleGetRepo, api, nil, nil)
	if repoResp.status != http.StatusOK {
		uiRelaySubResponse(w, repoResp)
		return
	}

	branches := uiSubGET(r, s.handleListBranches, api+"/branches", nil, nil)
	tags := uiSubGET(r, s.handleListTags, api+"/tags", nil, nil)
	readme := uiSubGET(r, s.handleGetReadme, api+"/readme", nil, nil)
	rootEntries := uiSubGET(r, s.handleGetContents, api+"/contents/", nil, map[string]string{"path": ""})
	languages := uiSubGET(r, s.handleGetRepoLanguages, api+"/languages", nil, nil)
	contributors := uiSubGET(r, s.handleListRepoContributors, api+"/contributors", nil, nil)
	latestRelease := uiSubGET(r, s.handleGetLatestRelease, api+"/releases/latest", nil, nil)
	latestCommit := uiSubGET(r, s.handleGetSingleCommit, api+"/commits/"+repo.DefaultBranch, nil,
		map[string]string{"ref": repo.DefaultBranch})

	writeJSON(uiConditional(w, r), http.StatusOK, map[string]interface{}{
		"repo":   json.RawMessage(repoResp.body()),
		"readme": uiSubJSONOrNull(readme),
		// The contents listing of "" is the default-ref root tree.
		"root_entries": uiSubJSONOrNull(rootEntries),
		"branches": map[string]interface{}{
			"first_page":  uiSubJSONOrEmptyList(branches),
			"total_count": uiSubListTotal(r, s.handleListBranches, api+"/branches", nil, nil),
		},
		"tags": map[string]interface{}{
			"first_page":  uiSubJSONOrEmptyList(tags),
			"total_count": uiSubListTotal(r, s.handleListTags, api+"/tags", nil, nil),
		},
		"languages":      uiSubJSONOrNull(languages),
		"contributors":   uiSubJSONOrEmptyList(contributors),
		"latest_release": uiSubJSONOrNull(latestRelease),
		"latest_commit":  uiSubJSONOrNull(latestCommit),
		// GitHub's open_issues_count mixes PRs in; the repo home tab counters
		// need the real split, counted on the store side.
		"pulls_open_count":    len(s.store.ListPullRequests(repo.ID, "OPEN")),
		"issues_open_count":   len(s.store.ListIssues(repo.ID, "OPEN")),
		"discussions_enabled": store.RepoHasDiscussions(repo),
	})
}

func (s *Server) handleUIBootstrapIssue(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	api := "/api/v3/repos/" + repo.FullName
	number := r.PathValue("number")

	issueResp := uiSubGET(r, s.handleGetIssue, api+"/issues/"+number, nil, nil)
	if issueResp.status != http.StatusOK {
		uiRelaySubResponse(w, issueResp)
		return
	}

	comments := uiSubGET(r, s.handleListIssueComments, api+"/issues/"+number+"/comments", nil, nil)
	timeline := uiSubGET(r, s.handleListIssueTimeline, api+"/issues/"+number+"/timeline", nil, nil)
	labels := uiSubGET(r, s.handleListLabels, api+"/labels", nil, nil)
	milestones := uiSubGET(r, s.handleListMilestones, api+"/milestones",
		url.Values{"state": {"open"}}, nil)
	assignees := uiSubGET(r, s.handleListRepoAssignees, api+"/assignees", nil, nil)

	writeJSON(uiConditional(w, r), http.StatusOK, map[string]interface{}{
		"issue":               json.RawMessage(issueResp.body()),
		"comments":            uiSubJSONOrEmptyList(comments),
		"timeline":            uiSubJSONOrEmptyList(timeline),
		"labels":              uiSubJSONOrEmptyList(labels),
		"milestones":          uiSubJSONOrEmptyList(milestones),
		"assignees_available": uiSubJSONOrEmptyList(assignees),
	})
}

func (s *Server) handleUIBootstrapPull(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	api := "/api/v3/repos/" + repo.FullName
	number := r.PathValue("number")

	pullResp := uiSubGET(r, s.handleGetPullRequest, api+"/pulls/"+number, nil, nil)
	if pullResp.status != http.StatusOK {
		uiRelaySubResponse(w, pullResp)
		return
	}
	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		ChangedFiles int `json:"changed_files"`
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
	}
	if err := json.Unmarshal(pullResp.body(), &pull); err != nil {
		writeGHError(w, http.StatusInternalServerError, "pull request payload unreadable")
		return
	}

	timeline := uiSubGET(r, s.handleListIssueTimeline, api+"/issues/"+number+"/timeline", nil, nil)
	comments := uiSubGET(r, s.handleListIssueComments, api+"/issues/"+number+"/comments", nil, nil)
	// The reviews / requested-reviewers / check-runs standalone routes are
	// registered behind requirePerm; run the identical chain so a scoped
	// credential is refused exactly as it would be on the standalone endpoint.
	reviews := uiSubGET(r, s.requirePerm(store.ScopePullRequests, store.PermRead, s.handleListPRReviews),
		api+"/pulls/"+number+"/reviews", nil, nil)
	reviewComments := uiSubGET(r, s.handleListPRComments, api+"/pulls/"+number+"/comments", nil, nil)
	requestedReviewers := uiSubGET(r, s.requirePerm(store.ScopePullRequests, store.PermRead, s.handleListRequestedReviewers),
		api+"/pulls/"+number+"/requested_reviewers", nil, nil)
	checkRuns := uiSubGET(r, s.requirePerm(store.ScopeChecks, store.PermRead, s.handleListCheckRunsForCommit),
		api+"/commits/"+pull.Head.SHA+"/check-runs", nil, map[string]string{"sha": pull.Head.SHA})
	combinedStatus := uiSubGET(r, s.handleGetCombinedStatus,
		api+"/commits/"+pull.Head.SHA+"/status", nil, map[string]string{"ref": pull.Head.SHA})

	writeJSON(uiConditional(w, r), http.StatusOK, map[string]interface{}{
		"pull":                json.RawMessage(pullResp.body()),
		"timeline":            uiSubJSONOrEmptyList(timeline),
		"comments":            uiSubJSONOrEmptyList(comments),
		"reviews":             uiSubJSONOrEmptyList(reviews),
		"review_comments":     uiSubJSONOrEmptyList(reviewComments),
		"requested_reviewers": uiSubJSONOrNull(requestedReviewers),
		"check_runs":          uiSubJSONOrNull(checkRuns),
		"combined_status":     uiSubJSONOrNull(combinedStatus),
		"files_summary": map[string]interface{}{
			"changed_files": pull.ChangedFiles,
			"additions":     pull.Additions,
			"deletions":     pull.Deletions,
		},
	})
}

// uiInsightsPeriods maps the pulse selector values to their window lengths.
var uiInsightsPeriods = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"3d":  3 * 24 * time.Hour,
	"1w":  7 * 24 * time.Hour,
	"1m":  30 * 24 * time.Hour,
}

func (s *Server) handleUIBootstrapInsights(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	api := "/api/v3/repos/" + repo.FullName

	period := r.URL.Query().Get("period")
	if period == "" {
		period = "1w"
	}
	window, ok := uiInsightsPeriods[period]
	if !ok {
		store.WriteGHValidationError(w, "Insights", "period", "invalid")
		return
	}
	since := s.currentTime().Add(-window)
	inWindow := func(t *time.Time) bool { return t != nil && !t.Before(since) }

	mergedPRs, openedPRs := 0, 0
	for _, pr := range s.store.ListPullRequests(repo.ID, "all") {
		if inWindow(pr.MergedAt) {
			mergedPRs++
		}
		created := pr.CreatedAt
		if inWindow(&created) {
			openedPRs++
		}
	}
	closedIssues, newIssues := 0, 0
	for _, issue := range s.store.ListIssues(repo.ID, "all") {
		if inWindow(issue.ClosedAt) {
			closedIssues++
		}
		created := issue.CreatedAt
		if inWindow(&created) {
			newIssues++
		}
	}

	// Contributor activity inside the window, attributed like the standalone
	// contributors endpoint: signatures resolve to an account when possible,
	// otherwise they count under the raw author name.
	commits, _ := s.defaultBranchCommits(repo)
	commitsByLogin := map[string]int{}
	for _, c := range commits {
		when := c.Committer.When.UTC()
		if when.Before(since) {
			continue
		}
		login := c.Author.Name
		if u := s.store.ResolveUserBySignature(c.Author.Name, c.Author.Email); u != nil {
			login = u.Login
		}
		commitsByLogin[login]++
	}
	top := make([]map[string]interface{}, 0, len(commitsByLogin))
	for login, count := range commitsByLogin {
		top = append(top, map[string]interface{}{"login": login, "commits": count})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i]["commits"].(int) != top[j]["commits"].(int) {
			return top[i]["commits"].(int) > top[j]["commits"].(int)
		}
		return top[i]["login"].(string) < top[j]["login"].(string)
	})
	if len(top) > 10 {
		top = top[:10]
	}

	commitActivity := uiSubGET(r, s.handleStatsCommitActivity, api+"/stats/commit_activity", nil, nil)
	languages := uiSubGET(r, s.handleGetRepoLanguages, api+"/languages", nil, nil)

	writeJSON(uiConditional(w, r), http.StatusOK, map[string]interface{}{
		"period":              period,
		"merged_prs_count":    mergedPRs,
		"opened_prs_count":    openedPRs,
		"closed_issues_count": closedIssues,
		"new_issues_count":    newIssues,
		"active_contributors": len(commitsByLogin),
		"top_contributors":    top,
		"commit_activity":     uiSubJSONOrEmptyList(commitActivity),
		"languages":           uiSubJSONOrNull(languages),
	})
}

// treeMetaLogWalkCap bounds the tree-meta commit walk: entries whose newest
// touching commit is older than this many commits from the tip stay
// unattributed (latest: null) rather than making the request unbounded.
const treeMetaLogWalkCap = 400

type uiTreeMetaEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	// Latest is the newest commit touching this entry (prefix match for
	// directories, so a rename or edit anywhere under it counts), or null if
	// none was found within treeMetaLogWalkCap commits of the tip.
	Latest *uiTreeMetaLatest `json:"latest"`
}

type uiTreeMetaLatest struct {
	SHA             string `json:"sha"`
	MessageHeadline string `json:"message_headline"`
	AuthorLogin     string `json:"author_login"`
	AuthorDate      string `json:"author_date"`
}

func (s *Server) handleUITreeMeta(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	stor := s.gitStorageForRepo(repo)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = repo.DefaultBranch
	}
	dirPath := strings.Trim(r.URL.Query().Get("path"), "/")

	hash, err := store.ResolveGitRef(stor, ref)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "No commit found for ref: "+ref)
		return
	}
	tip, err := object.GetCommit(stor, hash)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "No commit found for ref: "+ref)
		return
	}
	tree, err := tip.Tree()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
		return
	}
	if dirPath != "" {
		tree, err = tree.Tree(dirPath)
		if err != nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}

	entries := make([]*uiTreeMetaEntry, 0, len(tree.Entries))
	for _, entry := range tree.Entries {
		fullPath := entry.Name
		if dirPath != "" {
			fullPath = dirPath + "/" + entry.Name
		}
		entryType := "file"
		var size int64
		switch {
		case entry.Mode == filemode.Dir:
			entryType = "dir"
		case entry.Mode == filemode.Submodule:
			entryType = "submodule"
		case entry.Mode == filemode.Symlink:
			entryType = "symlink"
		}
		if entry.Mode == filemode.Symlink || entry.Mode.IsFile() {
			if blob, blobErr := object.GetBlob(stor, entry.Hash); blobErr == nil {
				size = blob.Size
			}
		}
		entries = append(entries, &uiTreeMetaEntry{
			Name: entry.Name,
			Path: fullPath,
			Type: entryType,
			Size: size,
		})
	}

	dirLatest := s.attributeTreeMetaEntries(tip, dirPath, entries)

	writeJSON(uiConditional(w, r), http.StatusOK, map[string]interface{}{
		"ref":           ref,
		"path":          dirPath,
		"latest_commit": commitToJSON(dirLatest, repo, s.store, s.baseURL(r)),
		"entries":       entries,
	})
}

// attributeTreeMetaEntries walks the history newest-to-oldest ONCE, assigning
// each still-unattributed entry the first (newest) commit whose first-parent
// diff touches it, and returning the newest commit that touches dirPath (the
// directory's own path-scoped tip; the walk tip itself for the root). Touch
// matching is the same equality-or-"prefix/" rule as commitTouchesPath, over
// both diff sides so renames into and out of an entry count. The walk stops
// once everything is attributed or after treeMetaLogWalkCap commits; whatever
// is still unattributed keeps latest == nil. This single pass replaces the
// UI's per-entry `commits?path=` fan-out.
func (s *Server) attributeTreeMetaEntries(tip *object.Commit, dirPath string, entries []*uiTreeMetaEntry) *object.Commit {
	touches := func(changed []string, target string) bool {
		for _, candidate := range changed {
			if candidate == target || strings.HasPrefix(candidate, target+"/") {
				return true
			}
		}
		return false
	}

	var dirLatest *object.Commit
	if dirPath == "" {
		dirLatest = tip
	}
	unattributed := len(entries)

	iter := object.NewCommitPreorderIter(tip, nil, nil)
	defer iter.Close()
	walked := 0
	_ = iter.ForEach(func(commit *object.Commit) error {
		if walked >= treeMetaLogWalkCap || (unattributed == 0 && dirLatest != nil) {
			return storer.ErrStop
		}
		walked++

		changed, err := commitChangedPaths(commit)
		if err != nil {
			// Best effort: an unreadable object ends the walk; remaining
			// entries stay unattributed rather than failing the page.
			return storer.ErrStop
		}
		if dirLatest == nil && touches(changed, dirPath) {
			dirLatest = commit
		}
		if unattributed == 0 {
			return nil
		}

		var latest *uiTreeMetaLatest
		for _, entry := range entries {
			if entry.Latest != nil || !touches(changed, entry.Path) {
				continue
			}
			if latest == nil {
				login := commit.Author.Name
				if u := s.store.ResolveUserBySignature(commit.Author.Name, commit.Author.Email); u != nil {
					login = u.Login
				}
				latest = &uiTreeMetaLatest{
					SHA:             commit.Hash.String(),
					MessageHeadline: strings.SplitN(strings.TrimSpace(commit.Message), "\n", 2)[0],
					AuthorLogin:     login,
					AuthorDate:      commit.Author.When.UTC().Format(time.RFC3339),
				}
			}
			entry.Latest = latest
			unattributed--
		}
		return nil
	})
	return dirLatest
}

// commitChangedPaths lists the paths a commit touches against its first
// parent, both diff sides (mirroring commitTouchesPath); a root commit touches
// everything in its tree.
func commitChangedPaths(commit *object.Commit) ([]string, error) {
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	if commit.NumParents() == 0 {
		var paths []string
		walker := object.NewTreeWalker(tree, true, nil)
		defer walker.Close()
		for {
			name, _, walkErr := walker.Next()
			if walkErr != nil {
				break
			}
			paths = append(paths, name)
		}
		return paths, nil
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := object.DiffTree(parentTree, tree)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, 2*len(changes))
	for _, change := range changes {
		if change.From.Name != "" {
			paths = append(paths, change.From.Name)
		}
		if change.To.Name != "" && change.To.Name != change.From.Name {
			paths = append(paths, change.To.Name)
		}
	}
	return paths, nil
}
