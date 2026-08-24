package bleephub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

func (s *Server) registerGHPullRoutes() {
	s.registerGHPullExtensionRoutes()
	s.route("POST /api/v3/repos/{owner}/{repo}/pulls", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleCreatePullRequest))
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls", s.handleListPullRequests)
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", s.handleGetPullRequest)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/pulls/{number}", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleUpdatePullRequest))
	s.route("PUT /api/v3/repos/{owner}/{repo}/pulls/{number}/merge", s.requirePerm(store.ScopeContents, store.PermWrite, s.handleMergePullRequest))
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{number}/merge", s.handleCheckPullRequestMerged)
	s.route("PUT /api/v3/repos/{owner}/{repo}/pulls/{number}/merge-async", s.requirePerm(store.ScopeContents, store.PermWrite, s.handleMergePullRequestAsync))
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{number}/merge-async/{uuid}", s.requirePerm(store.ScopeContents, store.PermRead, s.handleGetMergePullRequestAsyncResult))

	// PR reviews. The 3-segment GET/PUT/DELETE paths conflict with PR
	// review-comment reaction routes under Go 1.22's mux, so they are
	// dispatched via handlePullsThreeSegDispatch in gh_reactions.go.
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", s.requirePerm(store.ScopePullRequests, store.PermRead, s.handleListPRReviews))
	s.route("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleCreatePRReview))
	s.route("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews/{review_id}/events", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleSubmitPRReview))
	s.route("PUT /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews/{review_id}/dismissals", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleDismissPRReview))

	// Requested reviewers
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{number}/requested_reviewers", s.requirePerm(store.ScopePullRequests, store.PermRead, s.handleListRequestedReviewers))
	s.route("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/requested_reviewers", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleRequestReviewers))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/pulls/{number}/requested_reviewers", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleRemoveRequestedReviewers))

	// PR commits (the commits_url every PR response advertises)
	s.route("GET /api/v3/repos/{owner}/{repo}/pulls/{number}/commits", s.handleListPullRequestCommits)

	// Update branch (merge base into PR head)
	s.route("PUT /api/v3/repos/{owner}/{repo}/pulls/{number}/update-branch", s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleUpdateBranch))
}

func (s *Server) handleCheckPullRequestMerged(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, number)
	if pr == nil || pr.State != "MERGED" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreatePullRequest(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Title               string   `json:"title"`
		Body                string   `json:"body"`
		Head                string   `json:"head"`
		Base                string   `json:"base"`
		Draft               flexBool `json:"draft"`
		MaintainerCanModify flexBool `json:"maintainer_can_modify"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Head == "" {
		store.WriteGHValidationError(w, "PullRequest", "head", "missing_field")
		return
	}

	headRepo, headRef := store.ResolvePullRequestHead(s.store, repo, req.Head)
	if headRepo == nil || headRef == "" {
		store.WriteGHValidationError(w, "PullRequest", "head", "invalid")
		return
	}
	if !s.store.CanCreatePullRequest(repo.ID, user.ID, user.Login) {
		store.WriteGHValidationError(w, "PullRequest", "head", "too_many_open_pull_requests")
		return
	}

	pr, err := s.store.CreatePullRequestChecked(repo.ID, user.ID, req.Title, req.Body, headRef, req.Base, bool(req.Draft), nil, nil, 0, store.PullRequestOptions{
		HeadRepoID:          headRepo.ID,
		MaintainerCanModify: bool(req.MaintainerCanModify),
	})
	if errors.Is(err, store.ErrOpenPullRequestExists) {
		writePullRequestAlreadyExists(w)
		return
	}
	if pr == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull request creation failed")
		return
	}

	repoKey := owner + "/" + name
	// Materialize the test-merge commit before the payload is built so the
	// opened event (and the workflow run it triggers) reports the merge ref
	// (ACT-027).
	s.refreshPullRequestPotentialMerge(repo, pr)
	openedPayload := buildPullRequestPayload(s.store, repo, pr, user, "opened", s.baseURL(r))
	s.emitWebhookEvent(repoKey, "pull_request", "opened", openedPayload)
	// The code owners of the changed files are requested as reviewers once the
	// pull request exists, and deliver their own review_requested events after
	// the opened one, as GitHub orders them.
	s.autoRequestCodeOwners(repo, pr, user)
	if updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number); updated != nil {
		pr = updated
	}

	s.recordAuditEvent("pull_request.create", user.Login, "", map[string]interface{}{"repo": repoKey, "pr_id": pr.ID})
	prJSON := pullRequestToJSON(pr, s.store, s.baseURL(r), repo.FullName)
	writeJSONCreated(w, jsonStringField(prJSON, "url"), prJSON)
}

func writePullRequestAlreadyExists(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message":           "Validation Failed",
		"documentation_url": "https://docs.github.com/rest/pulls/pulls#create-a-pull-request",
		"errors": []map[string]string{{
			"resource": "PullRequest",
			"field":    "head",
			"code":     "custom",
			"message":  "A pull request already exists for this head and base.",
		}},
	})
}

func (s *Server) handleListPullRequests(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}

	var stateFilter string
	switch state {
	case "open":
		stateFilter = "OPEN"
	case "closed":
		stateFilter = "CLOSED"
	case "all":
		stateFilter = "all"
	default:
		store.WriteGHValidationError(w, "PullRequest", "state", "invalid")
		return
	}

	prs := s.store.ListPullRequests(repo.ID, stateFilter)
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "created"
	}
	switch sortBy {
	case "created", "updated", "popularity", "long-running":
	default:
		store.WriteGHValidationError(w, "PullRequest", "sort", "invalid")
		return
	}
	direction := r.URL.Query().Get("direction")
	if direction == "" {
		if sortBy == "created" {
			direction = "desc"
		} else {
			direction = "asc"
		}
	}
	if direction != "asc" && direction != "desc" {
		store.WriteGHValidationError(w, "PullRequest", "direction", "invalid")
		return
	}
	if sortBy == "long-running" {
		monthAgo := time.Now().UTC().AddDate(0, -1, 0)
		filtered := prs[:0]
		for _, pr := range prs {
			if pr.State == "OPEN" && pr.CreatedAt.Before(monthAgo) && pr.UpdatedAt.After(monthAgo) {
				filtered = append(filtered, pr)
			}
		}
		prs = filtered
	}

	// Filter by head
	if head := r.URL.Query().Get("head"); head != "" {
		// head can be "owner:branch" or just "branch"
		headOwner := ""
		branch := head
		if idx := strings.Index(head, ":"); idx >= 0 {
			headOwner = head[:idx]
			branch = head[idx+1:]
		}
		var filtered []*store.PullRequest
		for _, pr := range prs {
			if pr.HeadRefName != branch {
				continue
			}
			if headOwner != "" {
				headRepo := store.PullRequestHeadRepo(s.store, pr)
				if headRepo == nil || !strings.EqualFold(repoOwnerLogin(headRepo), headOwner) {
					continue
				}
			}
			filtered = append(filtered, pr)
		}
		prs = filtered
	}

	// Filter by base
	if base := r.URL.Query().Get("base"); base != "" {
		var filtered []*store.PullRequest
		for _, pr := range prs {
			if pr.BaseRefName == base {
				filtered = append(filtered, pr)
			}
		}
		prs = filtered
	}

	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(prs))
	for _, pr := range prs {
		item := pullRequestSimpleJSON(pr, s.store, base, repo.FullName)
		item["_comments"] = s.store.CountCommentsFor("pull_request", pr.ID)
		result = append(result, item)
	}
	sortPullRequestList(result, sortBy, direction)
	for _, item := range result {
		delete(item, "_comments")
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func sortPullRequestList(prs []map[string]interface{}, sortBy, direction string) {
	field := "created_at"
	switch sortBy {
	case "updated":
		field = "updated_at"
	case "popularity":
		field = "_comments"
	}
	ascending := direction == "asc"
	sort.SliceStable(prs, func(i, j int) bool {
		var less bool
		if field == "_comments" {
			a, _ := prs[i][field].(int)
			b, _ := prs[j][field].(int)
			if a == b {
				return pullRequestListNumberLess(prs[i], prs[j], ascending)
			}
			less = a < b
		} else {
			a, _ := prs[i][field].(string)
			b, _ := prs[j][field].(string)
			if a == b {
				return pullRequestListNumberLess(prs[i], prs[j], ascending)
			}
			less = a < b
		}
		if ascending {
			return less
		}
		return !less
	})
}

func pullRequestListNumberLess(a, b map[string]interface{}, ascending bool) bool {
	an, _ := a["number"].(int)
	bn, _ := b["number"].(int)
	if ascending {
		return an < bn
	}
	return an > bn
}

func (s *Server) handleGetPullRequest(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// The diff and patch media types are GitHub's contract for this endpoint,
	// and `gh pr diff` asks for the versioned spelling of the first one; a
	// server that ignored it answered the pull request object instead, which
	// the client printed verbatim as a "diff".
	accept := r.Header.Get("Accept")
	if acceptsGitHubMediaType(accept, "patch") {
		patch, err := pullRequestFormatPatch(s.store, repo, pr)
		if err != nil {
			writeGHError(w, http.StatusInternalServerError, "diff computation failed")
			return
		}
		setGitHubMediaType(w, r, "patch")
		w.Header().Set("Content-Type", "application/vnd.github.patch; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, patch)
		return
	}
	if acceptsGitHubMediaType(accept, "diff") {
		diff, err := pullRequestUnifiedDiff(s.store, repo, pr)
		if err != nil {
			writeGHError(w, http.StatusInternalServerError, "diff computation failed")
			return
		}
		setGitHubMediaType(w, r, "diff")
		w.Header().Set("Content-Type", "application/vnd.github.diff; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, diff)
		return
	}

	out := pullRequestToJSON(pr, s.store, s.baseURL(r), repo.FullName)
	s.applyChecksToMergeability(out, repo, pr)
	writeJSON(w, http.StatusOK, out)
}

// applyChecksToMergeability folds the head commit's check runs into
// mergeable_state the way real GitHub does: unmet REQUIRED status
// checks (branch protection on the base branch) block the merge;
// failing or still-running non-required checks mark it unstable.
func (s *Server) applyChecksToMergeability(out map[string]interface{}, repo *store.Repo, pr *store.PullRequest) {
	if pr.State != "OPEN" || out["mergeable_state"] != "clean" {
		return
	}
	// A base branch that requires code owner review and has not got it is
	// blocked by branch protection exactly as an unmet required status check
	// is, and reports the same state rather than a field of its own.
	if s.codeOwnerReviewMissing(repo, pr) {
		out["mergeable_state"] = "blocked"
		return
	}
	headSha := s.prHeadSha(repo, pr)
	if headSha == "" {
		return
	}
	st := s.evaluateChecksForMerge(repo, pr.BaseRefName, headSha)
	switch {
	case len(st.MissingRequired) > 0:
		out["mergeable_state"] = "blocked"
	case st.AnyFailing, st.AnyPending:
		out["mergeable_state"] = "unstable"
	}
}

func (s *Server) handleUpdatePullRequest(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Optimistic concurrency (STORE-016): reject a stale If-Match with 412.
	if !checkIfMatch(w, r, pullRequestToJSON(pr, s.store, s.baseURL(r), repo.FullName)) {
		return
	}

	var req map[string]interface{}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// GitHub rejects reopening a merged pull request; the state of a merged PR
	// is terminal. Guard before the mutation so it is not a silent no-op.
	if v, ok := req["state"].(string); ok && v == "open" && pr.State == "MERGED" {
		writeGHValidationErrorMessage(w, "PullRequest", "state", "invalid", "State cannot be changed. A merged pull request cannot be reopened.")
		return
	}

	priorState := pr.State
	s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) {
		if v, ok := req["title"].(string); ok {
			p.Title = v
		}
		if v, ok := req["body"].(string); ok {
			p.Body = v
		}
		if v, ok := req["base"].(string); ok {
			p.BaseRefName = v
		}
		if v, ok := coerceBool(req["maintainer_can_modify"]); ok {
			p.MaintainerCanModify = v
		}
		if v, ok := req["state"].(string); ok {
			switch v {
			case "closed":
				if p.State == "OPEN" {
					p.State = "CLOSED"
					now := time.Now()
					p.ClosedAt = &now
				}
			case "open":
				if p.State == "CLOSED" {
					p.State = "OPEN"
					p.ClosedAt = nil
				}
			}
		}
	})

	updated := s.store.GetPullRequest(pr.ID)

	switch {
	case priorState == "OPEN" && updated.State == "CLOSED":
		s.store.RecordPullRequestEvent(repo.ID, pr.ID, user.ID, "closed", "", 0)
	case priorState == "CLOSED" && updated.State == "OPEN":
		s.store.RecordPullRequestEvent(repo.ID, pr.ID, user.ID, "reopened", "", 0)
	}
	// A retitle and a retarget are two more github timeline events this
	// endpoint produces. Both carry a before/after pair, which the event row's
	// rename members hold.
	if v, ok := req["title"].(string); ok && v != pr.Title {
		s.store.RecordIssueOrPREvent(repo.ID, pr.Number, user.ID, "renamed", map[string]interface{}{
			"rename_from": pr.Title,
			"rename_to":   v,
		})
	}
	if v, ok := req["base"].(string); ok && v != pr.BaseRefName {
		s.store.RecordIssueOrPREvent(repo.ID, pr.Number, user.ID, "base_ref_changed", map[string]interface{}{
			"rename_from": pr.BaseRefName,
			"rename_to":   v,
		})
	}

	// One PATCH is several GitHub events: the edit itself, then the state
	// transition. `pr` is the pre-update snapshot, so it carries the
	// before-values the `changes` member reports.
	change := store.SubjectChange{StateFrom: priorState, StateTo: updated.State}
	if v, ok := req["title"].(string); ok && v != pr.Title {
		change.TitleFrom = &pr.Title
	}
	if v, ok := req["body"].(string); ok && v != pr.Body {
		change.BodyFrom = &pr.Body
	}
	if v, ok := req["base"].(string); ok && v != pr.BaseRefName {
		change.BaseRefFrom = &pr.BaseRefName
	}
	s.pullRequestEmitter(repo, updated, user).emitChanges(change)

	writeJSON(w, http.StatusOK, pullRequestToJSON(updated, s.store, s.baseURL(r), repo.FullName))
}

func (s *Server) handleMergePullRequest(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Merging a pull request is a write to the base branch: GitHub requires push
	// access, so a read-only collaborator (or any authenticated user on a public
	// repo with no branch protection) must not be able to merge (REST-123).
	if !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have write access to the repository.")
		return
	}

	if pr.State == "MERGED" {
		writeGHError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	}
	if pr.State == "CLOSED" {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull Request is closed")
		return
	}

	var req struct {
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		SHA           string `json:"sha"`
		MergeMethod   string `json:"merge_method"`
	}
	if raw, err := io.ReadAll(r.Body); err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}
	switch req.MergeMethod {
	case "", "merge", "squash", "rebase":
	default:
		store.WriteGHValidationError(w, "PullRequest", "merge_method", "invalid")
		return
	}
	// GitHub 405s an explicit merge method the repository has disabled.
	var disallowed string
	switch {
	case req.MergeMethod == "merge" && !repo.AllowMergeCommit:
		disallowed = "Merge commits are not allowed on this repository."
	case req.MergeMethod == "squash" && !repo.AllowSquashMerge:
		disallowed = "Squash merges are not allowed on this repository."
	case req.MergeMethod == "rebase" && !repo.AllowRebaseMerge:
		disallowed = "Rebase merges are not allowed on this repository."
	}
	if disallowed != "" {
		writeGHError(w, http.StatusMethodNotAllowed, disallowed)
		return
	}
	// Expected-head guard: merging against a stale head SHA is a 409.
	if req.SHA != "" {
		if head := s.prHeadSha(repo, pr); head != "" && head != req.SHA {
			writeGHError(w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
			return
		}
	}

	// Branch protection: required status checks must be green on the
	// head commit before the merge API succeeds (405, real GitHub).
	if headSha := s.prHeadSha(repo, pr); headSha != "" {
		if st := s.evaluateChecksForMerge(repo, pr.BaseRefName, headSha); len(st.MissingRequired) > 0 {
			writeGHError(w, http.StatusMethodNotAllowed,
				fmt.Sprintf("Required status check %q is expected.", st.MissingRequired[0]))
			return
		}
	}

	if ok, msg := s.canMergePullRequest(r.Context(), repo, pr); !ok {
		status := http.StatusMethodNotAllowed
		if msg == "" {
			msg = "Pull Request is not mergeable"
		}
		writeGHError(w, status, msg)
		return
	}

	mergeSha, errMsg := s.completePullRequestMerge(repo, pr, user, req.MergeMethod, req.CommitTitle, req.CommitMessage, s.prHeadSha(repo, pr))
	if errMsg != "" {
		writeGHError(w, http.StatusMethodNotAllowed, errMsg)
		return
	}

	merged := s.store.GetPullRequest(pr.ID)
	repoKey := owner + "/" + repoName
	mergedPayload := buildPullRequestPayload(s.store, repo, merged, user, "closed", s.baseURL(r))
	s.emitWebhookEvent(repoKey, "pull_request", "closed", mergedPayload)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sha":     mergeSha,
		"merged":  true,
		"message": "Pull Request successfully merged",
	})
}

// completePullRequestMerge materialises the merge in the repository's git
// storage (merge commit, squash commit, or rebased commits per method), marks
// the PR merged, and records the merged and closed timeline events. It returns
// the resulting merge commit SHA or a non-empty error message when the merge
// cannot be performed (e.g. a merge conflict).
// completePullRequestMerge merges pr's head into its base. expectedHead is the
// head SHA the caller's required-status-check / expected-head guards verified;
// the merge is bound to exactly that commit so a head branch advanced by a
// concurrent push between the check and here cannot land an unchecked commit on
// a protected base (a check-then-merge TOCTOU). Pass "" only where no head guard
// applies.
func (s *Server) completePullRequestMerge(repo *store.Repo, pr *store.PullRequest, user *store.User, method, commitTitle, commitMessage, expectedHead string) (string, string) {
	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	stor := s.store.GetGitStorage(owner, name)
	var mergeSha string
	if stor == nil {
		return "", "Pull Request is not mergeable"
	}
	headStor, headRepoFullName := store.PullRequestGitStorage(s.store, repo, pr)
	if headStor == nil {
		return "", "Pull Request is not mergeable"
	}
	headHash, headErr := store.ResolveGitRef(headStor, pr.HeadRefName)
	// Refuse if the head moved since it was checked (or can no longer be
	// resolved): an unverifiable interlock is not a satisfied one.
	if expectedHead != "" && (headErr != nil || !strings.EqualFold(headHash.String(), expectedHead)) {
		return "", "Head branch was modified. Review and try the merge again."
	}
	if headErr == nil && headRepoFullName != repo.FullName {
		if err := store.CopyGitObjects(headStor, stor); err != nil {
			return "", "Pull Request is not mergeable"
		}
	}
	baseRef := plumbing.NewBranchReferenceName(pr.BaseRefName)
	if _, baseErr := stor.Reference(baseRef); headErr != nil || baseErr != nil {
		return "", "Pull Request is not mergeable"
	}
	email := user.Email
	if email == "" {
		email = user.Login + "@users.noreply.bleephub.local"
	}
	author := repoSignature(user.Login, email)
	var hash plumbing.Hash
	var err error
	switch method {
	case "squash":
		message := commitTitle
		if message == "" {
			message = fmt.Sprintf("%s (#%d)", pr.Title, pr.Number)
		}
		if commitMessage != "" {
			message += "\n\n" + commitMessage
		}
		hash, err = performSquashMerge(stor, baseRef, headHash, message, author, author)
	case "rebase":
		hash, err = performRebaseMerge(stor, baseRef, headHash, author)
	default: // "merge"
		message := commitTitle
		if message == "" {
			headOwner := owner
			if headRepo := store.PullRequestHeadRepo(s.store, pr); headRepo != nil && headRepo.Owner != nil {
				headOwner = headRepo.Owner.Login
			}
			message = fmt.Sprintf("Merge pull request #%d from %s/%s", pr.Number, headOwner, pr.HeadRefName)
		}
		body := commitMessage
		if body == "" {
			body = pr.Title
		}
		hash, err = performMergeCommit(stor, baseRef, headHash, message+"\n\n"+body, author)
	}
	if err != nil {
		return "", "Pull Request is not mergeable"
	}
	mergeSha = hash.String()
	s.store.UpdateRepo(owner, name, func(r *store.Repo) {
		r.PushedAt = time.Now().UTC()
	})

	s.store.UpdatePullRequest(pr.ID, func(p *store.PullRequest) {
		now := time.Now()
		p.State = "MERGED"
		p.MergedAt = &now
		p.ClosedAt = &now
		p.MergedByID = user.ID
		p.MergeCommitSHA = mergeSha
	})
	// GitHub's timeline for a merged PR carries a merged event (with the
	// merge commit) immediately followed by a closed event.
	s.store.RecordPullRequestEvent(repo.ID, pr.ID, user.ID, "merged", mergeSha, 0)
	s.store.RecordPullRequestEvent(repo.ID, pr.ID, user.ID, "closed", "", 0)
	return mergeSha, ""
}

func (s *Server) handleCreatePRReview(w http.ResponseWriter, r *http.Request) {
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

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Body     string `json:"body"`
		Event    string `json:"event"`
		CommitID string `json:"commit_id"`
		Comments []struct {
			Path      string  `json:"path"`
			Body      string  `json:"body"`
			Line      flexInt `json:"line"`
			StartLine flexInt `json:"start_line"`
			Side      string  `json:"side"`
			Position  flexInt `json:"position"`
		} `json:"comments"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	state := "PENDING"
	switch strings.ToUpper(req.Event) {
	case "":
	case "APPROVE":
		state = "APPROVED"
	case "REQUEST_CHANGES":
		state = "CHANGES_REQUESTED"
	case "COMMENT":
		state = "COMMENTED"
	default:
		// This operation documents the validation-error-simple 422 shape (a bare
		// string per error), not the object form.
		writeGHValidationErrorSimple(w, "Invalid value for event")
		return
	}

	// GitHub requires a review body when the event is REQUEST_CHANGES or COMMENT
	// (an APPROVE or a still-pending review may have an empty body).
	if (state == "CHANGES_REQUESTED" || state == "COMMENTED") && strings.TrimSpace(req.Body) == "" {
		writeGHValidationErrorSimple(w, "body is required when the event is REQUEST_CHANGES or COMMENT")
		return
	}

	// Validate the complete comment batch before creating either the review
	// or its first comment. A malformed later entry must not leave a partial
	// draft behind.
	for _, rc := range req.Comments {
		if rc.Path == "" || rc.Body == "" {
			writeGHValidationErrorSimple(w, "Every comment requires a path and a body")
			return
		}
	}

	review := s.store.CreatePullRequestReview(repo.FullName, pr.Number, user.ID, req.Body, state)
	if review == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Review creation failed")
		return
	}

	// The create-review API attaches its draft comments to the review; they
	// surface through GET /pulls/{number}/reviews/{review_id}/comments and
	// the regular review-comment endpoints.
	for _, rc := range req.Comments {
		line := int(rc.Line)
		if line == 0 {
			line = int(rc.Position)
		}
		c := s.store.PRReviewComments.CreateRootComment(pr.ID, user.ID, rc.Path, rc.Body, req.CommitID, rc.Side, line, int(rc.StartLine))
		s.store.PRReviewComments.AttachToReview(c.ID, review.ID)
	}

	if review.State != "PENDING" {
		s.emitWebhookEvent(repo.FullName, "pull_request_review", "submitted",
			buildPullRequestReviewPayload(s.store, repo, pr, review, user, "submitted", s.baseURL(r)))
	}
	// An approval can be the required review an armed auto-merge was
	// waiting for.
	if review.State == "APPROVED" {
		s.maybeAutoMergePR(pr.ID)
	}
	writeJSON(w, http.StatusOK, reviewToJSON(review, s.store, s.baseURL(r), repo.FullName, pr.Number))
}

func (s *Server) handleListPRReviews(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	reviews := s.store.ListPullRequestReviews(repo.FullName, pr.Number)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(reviews))
	for _, review := range reviews {
		result = append(result, reviewToJSON(review, s.store, base, repo.FullName, pr.Number))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// handlePullsThreeSegDispatch routes the ambiguous 3-segment paths under
// /repos/{owner}/{repo}/pulls/{a}/{b}/{c}. The PR review surface
// (/pulls/{number}/reviews/{review_id}) and PR review-comment reactions
// (/pulls/comments/{comment_id}/reactions) both occupy three path segments
// after /pulls and cannot both be registered directly with Go 1.22's mux.
// The dispatcher inspects the literal segments and sets the correct path
// values for the delegated handler. It is registered from gh_reactions.go
// so that reaction-only test servers also provide the dispatch surface.
func (s *Server) handlePullsThreeSegDispatch(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p1 := r.PathValue("p1")
		p2 := r.PathValue("p2")
		p3 := r.PathValue("p3")

		// PR review: /pulls/{number}/reviews/{review_id}
		if p2 == "reviews" {
			r.SetPathValue("number", p1)
			r.SetPathValue("review_id", p3)
			switch method {
			case "GET":
				s.handleGetPRReview(w, r)
			case "PUT":
				s.handleUpdatePRReview(w, r)
			case "DELETE":
				s.handleDeletePRReview(w, r)
			default:
				writeGHError(w, http.StatusNotFound, "Not Found")
			}
			return
		}

		// PR review-comment reactions: /pulls/comments/{comment_id}/reactions
		if p1 == "comments" && p3 == "reactions" {
			r.SetPathValue("comment_id", p2)
			switch method {
			case "GET":
				s.handleListReactions("pull_request_review_comment", "comment_id")(w, r)
			case "POST":
				s.handleCreateReaction("pull_request_review_comment", "comment_id")(w, r)
			default:
				writeGHError(w, http.StatusNotFound, "Not Found")
			}
			return
		}

		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) handleGetPRReview(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	reviewID, err := strconv.Atoi(r.PathValue("review_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	review := s.store.GetPullRequestReview(reviewID)
	if review == nil || review.PRID != pr.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	writeJSON(w, http.StatusOK, reviewToJSON(review, s.store, s.baseURL(r), repo.FullName, pr.Number))
}

func (s *Server) handleUpdatePRReview(w http.ResponseWriter, r *http.Request) {
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

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	reviewID, err := strconv.Atoi(r.PathValue("review_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Resolve and scope the review to this PR *before* mutating it: the store
	// looks reviews up by global id alone, so writing first would let a caller
	// who names any repo/PR they control edit a review that lives on another
	// repository. Only then enforce that the caller authored it — GitHub allows
	// just the review's author to edit its summary text.
	review := s.store.GetPullRequestReview(reviewID)
	if review == nil || review.PRID != pr.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if review.AuthorID != user.ID {
		writeGHError(w, http.StatusForbidden, "Only the author of the review can update it")
		return
	}

	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	priorBody := review.Body
	if !s.store.UpdatePullRequestReview(reviewID, req.Body) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	review = s.store.GetPullRequestReview(reviewID)
	if review == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	payload := buildPullRequestReviewPayload(s.store, repo, pr, review, user, "edited", s.baseURL(r))
	payload["changes"] = map[string]interface{}{"body": map[string]interface{}{"from": priorBody}}
	s.emitWebhookEvent(repo.FullName, "pull_request_review", "edited", payload)

	writeJSON(w, http.StatusOK, reviewToJSON(review, s.store, s.baseURL(r), repo.FullName, pr.Number))
}

func (s *Server) handleDeletePRReview(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	reviewID, err := strconv.Atoi(r.PathValue("review_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	review := s.store.GetPullRequestReview(reviewID)
	if review == nil || review.PRID != pr.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if review.State != "PENDING" {
		writeGHError(w, http.StatusUnprocessableEntity, "Review must be pending to delete")
		return
	}

	if !s.store.DeletePullRequestReview(reviewID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSubmitPRReview(w http.ResponseWriter, r *http.Request) {
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

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	reviewID, err := strconv.Atoi(r.PathValue("review_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Event string `json:"event"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	review := s.store.GetPullRequestReview(reviewID)
	if review == nil || review.PRID != pr.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if review.State != "PENDING" {
		writeGHError(w, http.StatusUnprocessableEntity, "Review must be pending to submit")
		return
	}

	if !s.store.SubmitPullRequestReview(reviewID, req.Event) {
		writeGHError(w, http.StatusUnprocessableEntity, "Invalid review event")
		return
	}

	review = s.store.GetPullRequestReview(reviewID)
	s.emitWebhookEvent(repo.FullName, "pull_request_review", "submitted",
		buildPullRequestReviewPayload(s.store, repo, pr, review, user, "submitted", s.baseURL(r)))
	// An approval can be the required review an armed auto-merge was
	// waiting for.
	if review != nil && review.State == "APPROVED" {
		s.maybeAutoMergePR(pr.ID)
	}
	writeJSON(w, http.StatusOK, reviewToJSON(review, s.store, s.baseURL(r), repo.FullName, pr.Number))
}

func (s *Server) handleDismissPRReview(w http.ResponseWriter, r *http.Request) {
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

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	reviewID, err := strconv.Atoi(r.PathValue("review_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Message string `json:"message"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// GitHub's dismiss endpoint requires a non-empty message; its 422 documents
	// the validation-error-simple shape.
	if strings.TrimSpace(req.Message) == "" {
		writeGHValidationErrorSimple(w, "message is required")
		return
	}

	review := s.store.GetPullRequestReview(reviewID)
	if review == nil || review.PRID != pr.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if !s.store.DismissPullRequestReview(reviewID, req.Message) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// GitHub adds a distinct `review_dismissed` timeline event (the original
	// `reviewed` entry stays); without it a dismissal is invisible in history.
	s.store.RecordPullRequestEvent(repo.ID, pr.ID, user.ID, "review_dismissed", "", 0)

	review = s.store.GetPullRequestReview(reviewID)
	s.emitWebhookEvent(repo.FullName, "pull_request_review", "dismissed",
		buildPullRequestReviewPayload(s.store, repo, pr, review, user, "dismissed", s.baseURL(r)))
	// Dismissing a blocking CHANGES_REQUESTED review can clear the condition
	// an armed auto-merge was waiting for.
	s.maybeAutoMergePR(pr.ID)
	writeJSON(w, http.StatusOK, reviewToJSON(review, s.store, s.baseURL(r), repo.FullName, pr.Number))
}

func (s *Server) handleRequestReviewers(w http.ResponseWriter, r *http.Request) {
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

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Reviewers     []interface{} `json:"reviewers"`
		TeamReviewers []string      `json:"team_reviewers"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	reviewerIDs := reviewerIDsFromRequest(s.store, req.Reviewers)
	teamIDs, teamsOK := requestedTeamIDs(s.store, repo, req.TeamReviewers)
	if !teamsOK {
		store.WriteGHValidationError(w, "PullRequest", "team_reviewers", "invalid")
		return
	}
	if len(reviewerIDs) == 0 && len(req.TeamReviewers) == 0 {
		store.WriteGHValidationError(w, "PullRequest", "reviewers", "missing_field")
		return
	}

	if len(reviewerIDs) > 0 {
		if !s.store.RequestReviewers(repo.FullName, pr.Number, reviewerIDs, user.ID) {
			writeGHError(w, http.StatusUnprocessableEntity, "Unable to request reviewers")
			return
		}
	}
	if len(teamIDs) > 0 && !s.store.RequestTeamReviewers(repo.FullName, pr.Number, teamIDs) {
		writeGHError(w, http.StatusUnprocessableEntity, "Unable to request team reviewers")
		return
	}

	updated := s.store.GetPullRequestByNumber(repo.ID, num)
	s.pullRequestEmitter(repo, updated, user).emitReviewRequestDelta(
		pr.RequestedReviewerIDs, updated.RequestedReviewerIDs,
		pr.RequestedTeamIDs, updated.RequestedTeamIDs)
	updatedJSON := pullRequestSimpleJSON(updated, s.store, s.baseURL(r), repo.FullName)
	writeJSONCreated(w, jsonStringField(updatedJSON, "url"), updatedJSON)
}

func (s *Server) handleRemoveRequestedReviewers(w http.ResponseWriter, r *http.Request) {
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

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Reviewers     []interface{} `json:"reviewers"`
		TeamReviewers []string      `json:"team_reviewers"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	reviewerIDs := reviewerIDsFromRequest(s.store, req.Reviewers)
	teamIDs, teamsOK := requestedTeamIDs(s.store, repo, req.TeamReviewers)
	if !teamsOK {
		store.WriteGHValidationError(w, "PullRequest", "team_reviewers", "invalid")
		return
	}
	if len(reviewerIDs) > 0 {
		if !s.store.RemoveRequestedReviewers(repo.FullName, pr.Number, reviewerIDs, user.ID) {
			writeGHError(w, http.StatusUnprocessableEntity, "Unable to remove reviewers")
			return
		}
	}
	if len(teamIDs) > 0 && !s.store.RemoveRequestedTeamReviewers(repo.FullName, pr.Number, teamIDs) {
		writeGHError(w, http.StatusUnprocessableEntity, "Unable to remove team reviewers")
		return
	}

	updated := s.store.GetPullRequestByNumber(repo.ID, num)
	s.pullRequestEmitter(repo, updated, user).emitReviewRequestDelta(
		pr.RequestedReviewerIDs, updated.RequestedReviewerIDs,
		pr.RequestedTeamIDs, updated.RequestedTeamIDs)
	writeJSON(w, http.StatusOK, pullRequestSimpleJSON(updated, s.store, s.baseURL(r), repo.FullName))
}

// handleListRequestedReviewers serves
// GET /repos/{owner}/{repo}/pulls/{number}/requested_reviewers — the
// pull-request-review-request shape ({users, teams}).
func (s *Server) handleListRequestedReviewers(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	users := make([]map[string]interface{}, 0)
	teams := make([]map[string]interface{}, 0)
	s.store.Mu.RLock()
	for _, id := range pr.RequestedReviewerIDs {
		if u, ok := s.store.Users[id]; ok {
			users = append(users, store.UserToJSON(u, s.baseURL(r)))
		}
	}
	org := s.store.OrgsByLogin[ownerFromRepoFullName(repo.FullName)]
	for _, id := range pr.RequestedTeamIDs {
		if team := s.store.Teams[id]; team != nil && org != nil && team.OrgID == org.ID {
			teams = append(teams, requestedTeamJSONLocked(s.store, team, org, s.baseURL(r)))
		}
	}
	s.store.Mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"users": users,
		"teams": teams,
	})
}

// buildPullRequestTimeline assembles the issue-timeline union for a pull
// request: the PR's real git commits ("committed" items, interleaved by
// commit author date), conversation comments ("commented"), submitted
// reviews ("reviewed"), and recorded issue events (review_requested,
// review_request_removed, merged, closed, reopened), ordered by creation
// time. Pending reviews are not part of the public timeline on real GitHub
// and are excluded here too.
func (s *Server) buildPullRequestTimeline(repo *store.Repo, pr *store.PullRequest, baseURL string) ([]map[string]interface{}, error) {
	comments := s.store.ListCommentsFor("pull_request", pr.ID)
	reviews := s.store.ListPullRequestReviews(repo.FullName, pr.Number)
	events := s.store.ListPullRequestEvents(repo.ID, pr.ID)
	commits, err := pullRequestCommitObjects(s.store, repo, pr)
	if err != nil {
		return nil, err
	}

	type timelineEntry struct {
		at time.Time
		// Committed items sort before same-instant events/comments: the
		// commits exist before anything reacting to them.
		rank int
		id   int
		json map[string]interface{}
	}
	entries := make([]timelineEntry, 0, len(commits)+len(comments)+len(reviews)+len(events))
	for i, c := range commits {
		entries = append(entries, timelineEntry{
			at:   c.Author.When.UTC(),
			rank: 0,
			id:   i,
			json: timelineCommittedEventJSON(c, repo.FullName, baseURL),
		})
	}
	for _, c := range comments {
		entries = append(entries, timelineEntry{
			at:   c.CreatedAt,
			rank: 1,
			id:   c.ID,
			json: store.TimelineCommentToJSON(c, s.store, baseURL, repo.FullName, pr.Number, repo),
		})
	}
	for _, review := range reviews {
		if review.State == "PENDING" {
			continue
		}
		j := reviewToJSON(review, s.store, baseURL, repo.FullName, pr.Number)
		j["event"] = "reviewed"
		at := review.CreatedAt
		if review.SubmittedAt != nil {
			at = *review.SubmittedAt
		}
		entries = append(entries, timelineEntry{at: at, rank: 1, id: review.ID, json: j})
	}
	for _, e := range events {
		entries = append(entries, timelineEntry{
			at:   e.CreatedAt,
			rank: 1,
			id:   e.ID,
			json: store.IssueEventForTimelineToJSON(e, s.store, baseURL, repo.FullName),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].at.Equal(entries[j].at) {
			return entries[i].at.Before(entries[j].at)
		}
		if entries[i].rank != entries[j].rank {
			return entries[i].rank < entries[j].rank
		}
		return entries[i].id < entries[j].id
	})

	out := make([]map[string]interface{}, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.json)
	}
	return out, nil
}

// pullRequestCommitObjects derives a pull request's commits from the
// repository's real git history: the commits reachable from the head branch
// but not from the merge base against the PR's recorded creation-time base
// commit, oldest first — the same range GitHub's pulls/{n}/commits lists.
// The recorded base keeps the range stable after the base branch advances,
// including past the PR's own merge commit. The result is empty (with a nil
// error) when the repository holds no git objects for the PR's branches.
func pullRequestCommitObjects(st *store.Store, repo *store.Repo, pr *store.PullRequest) ([]*object.Commit, error) {
	stor, _ := store.PullRequestGitStorage(st, repo, pr)
	if stor == nil {
		return nil, nil
	}
	return store.PullRequestCommitObjectsFromStorage(stor, pr)
}

// timelineCommittedEventJSON renders a real git commit as the GitHub
// timeline-committed-event shape ("event": "committed").
func timelineCommittedEventJSON(c *object.Commit, repoFullName, baseURL string) map[string]interface{} {
	sha := c.Hash.String()
	parents := make([]map[string]interface{}, 0, len(c.ParentHashes))
	for _, h := range c.ParentHashes {
		parents = append(parents, map[string]interface{}{
			"sha":      h.String(),
			"url":      baseURL + "/api/v3/repos/" + repoFullName + "/git/commits/" + h.String(),
			"html_url": baseURL + "/" + repoFullName + "/commit/" + h.String(),
		})
	}
	return map[string]interface{}{
		"event":    "committed",
		"sha":      sha,
		"node_id":  encodeNodeID("Commit", 0, sha),
		"url":      baseURL + "/api/v3/repos/" + repoFullName + "/git/commits/" + sha,
		"html_url": baseURL + "/" + repoFullName + "/commit/" + sha,
		"author": map[string]interface{}{
			"name":  c.Author.Name,
			"email": c.Author.Email,
			"date":  c.Author.When.UTC().Format(time.RFC3339),
		},
		"committer": map[string]interface{}{
			"name":  c.Committer.Name,
			"email": c.Committer.Email,
			"date":  c.Committer.When.UTC().Format(time.RFC3339),
		},
		"message": c.Message,
		"tree": map[string]interface{}{
			"sha": c.TreeHash.String(),
			"url": baseURL + "/api/v3/repos/" + repoFullName + "/git/trees/" + c.TreeHash.String(),
		},
		"parents": parents,
		"verification": map[string]interface{}{
			"verified":    false,
			"reason":      "unsigned",
			"signature":   nil,
			"payload":     nil,
			"verified_at": nil,
		},
	}
}

// handleListPullRequestCommits serves GET
// /repos/{owner}/{repo}/pulls/{number}/commits — the PR's commits derived
// from the repository's real git history (the commits_url every PR
// response advertises).
func (s *Server) handleListPullRequestCommits(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	commits, err := pullRequestCommitObjects(s.store, repo, pr)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "commit lookup failed")
		return
	}
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(commits))
	for _, c := range commits {
		out = append(out, commitToJSON(c, repo, s.store, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleUpdateBranch(w http.ResponseWriter, r *http.Request) {
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

	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if pr.State != "OPEN" {
		store.WriteGHValidationError(w, "PullRequest", "state", "invalid")
		return
	}
	var body struct {
		ExpectedHeadSHA string `json:"expected_head_sha"`
	}
	if !decodeJSONBodyOptional(w, r, &body) {
		return
	}

	headRepo := store.PullRequestHeadRepo(s.store, pr)
	if headRepo == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull request head repository is unavailable")
		return
	}
	headOwner, headName, _ := store.SplitRepoFullName(headRepo.FullName)
	headStor := s.store.GetGitStorage(headOwner, headName)
	baseOwner, baseName, _ := store.SplitRepoFullName(repo.FullName)
	baseStor := s.store.GetGitStorage(baseOwner, baseName)
	if headStor == nil || baseStor == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull request branch cannot be updated")
		return
	}
	headRef := plumbing.NewBranchReferenceName(pr.HeadRefName)
	headReference, err := headStor.Reference(headRef)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull request head branch does not exist")
		return
	}
	before := headReference.Hash()
	if body.ExpectedHeadSHA != "" && body.ExpectedHeadSHA != before.String() {
		store.WriteGHValidationError(w, "PullRequest", "expected_head_sha", "invalid")
		return
	}
	baseHash, err := store.ResolveGitRef(baseStor, pr.BaseRefName)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull request base branch does not exist")
		return
	}
	if headRepo.FullName != repo.FullName {
		if err := store.CopyGitObjects(baseStor, headStor); err != nil {
			writeGHError(w, http.StatusUnprocessableEntity, "Pull request branch cannot be updated")
			return
		}
	}
	email := user.Email
	if email == "" {
		email = user.Login + "@users.noreply.bleephub.local"
	}
	after, _, err := performMerge(
		headStor, headRef, baseHash, pr.BaseRefName,
		fmt.Sprintf("Merge branch '%s' into %s", pr.BaseRefName, pr.HeadRefName),
		repoSignature(user.Login, email),
	)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull request branch cannot be updated due to conflicts")
		return
	}
	s.store.UpdatePullRequest(pr.ID, func(current *store.PullRequest) {
		current.BaseSHA = baseHash.String()
		current.Mergeable = "UNKNOWN"
	})
	if after != before {
		s.afterCommittedRefUpdate(
			headRepo, user, headRef.String(), before.String(), after.String(), s.baseURL(r),
		)
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"message": "Updating pull request branch.",
		"url":     fmt.Sprintf("%s/api/v3/repos/%s/pulls/%d", s.baseURL(r), repo.FullName, pr.Number),
	})
}

// reviewerIDsFromRequest normalises the GitHub request reviewers field,
// which may be an array of logins (strings) or objects with an id/login key.
func reviewerIDsFromRequest(st *store.Store, reviewers []interface{}) []int {
	var ids []int
	for _, v := range reviewers {
		switch x := v.(type) {
		case string:
			if u := st.LookupUserByLogin(x); u != nil {
				ids = append(ids, u.ID)
			}
		case map[string]interface{}:
			if id, ok := x["id"].(float64); ok {
				ids = append(ids, int(id))
			} else if login, ok := x["login"].(string); ok {
				if u := st.LookupUserByLogin(login); u != nil {
					ids = append(ids, u.ID)
				}
			}
		}
	}
	return ids
}

func requestedTeamIDs(st *store.Store, repo *store.Repo, slugs []string) ([]int, bool) {
	if len(slugs) == 0 {
		return nil, true
	}
	orgLogin := ownerFromRepoFullName(repo.FullName)
	org := st.GetOrg(orgLogin)
	if org == nil {
		return nil, false
	}
	ids := make([]int, 0, len(slugs))
	seen := map[int]struct{}{}
	for _, slug := range slugs {
		team := st.GetTeam(orgLogin, slug)
		if team == nil || team.OrgID != org.ID {
			return nil, false
		}
		if _, ok := seen[team.ID]; !ok {
			ids = append(ids, team.ID)
			seen[team.ID] = struct{}{}
		}
	}
	return ids, true
}

// requestedTeamJSONLocked is the team-simple shape used by pull request
// review requests. Callers hold st.mu, so parent lookup uses the map directly
// instead of the locking teamSimpleJSON helper.
func requestedTeamJSONLocked(st *store.Store, team *store.Team, org *store.Org, baseURL string) map[string]interface{} {
	out := teamRefJSON(team, org, baseURL)
	out["parent"] = nil
	if parent := st.Teams[team.ParentID]; parent != nil && parent.OrgID == org.ID {
		out["parent"] = teamRefJSON(parent, org, baseURL)
	}
	return out
}

// --- JSON converters ---

func pullRequestHeadSHA(pr *store.PullRequest, st *store.Store) string {
	if pr == nil {
		return ""
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return store.PullRequestHeadSHALocked(pr, st)
}

func pullRequestReviewCommitSHA(review *store.PullRequestReview, st *store.Store) string {
	if review == nil {
		return ""
	}
	return pullRequestHeadSHA(st.GetPullRequest(review.PRID), st)
}

// pullRequestSimpleJSON converts a PullRequest to the GitHub
// `pull-request-simple` shape used by list responses — the full shape
// minus the merge/diff-stat members that exist only on `pull-request`.
// Must not be called with st.mu held.
func pullRequestSimpleJSON(pr *store.PullRequest, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	// Read the PR's mutable fields off a private snapshot: an UpdatePullRequest
	// / merge / close writer mutates title, body, state, merge shas, and
	// timestamps under st.mu.Lock, so the live pointer must not be read here.
	// The snapshot's RLock is released before the map-resolution RLock below —
	// they are sequential, never nested.
	pr = st.SnapPR(pr)
	st.Mu.RLock()

	// Resolve author
	var authorJSON map[string]interface{}
	if u := store.ActorUserLocked(st, pr.AuthorID); u != nil {
		authorJSON = store.UserToJSON(u, baseURL)
	}

	// Resolve labels
	labels := make([]map[string]interface{}, 0)
	for _, lid := range pr.LabelIDs {
		if l, ok := st.Labels[lid]; ok {
			labels = append(labels, issueLabelToJSON(l, baseURL, repoFullName))
		}
	}

	// Resolve assignees
	assignees := make([]map[string]interface{}, 0)
	for _, aid := range pr.AssigneeIDs {
		if u, ok := st.Users[aid]; ok {
			assignees = append(assignees, store.UserToJSON(u, baseURL))
		}
	}

	// Resolve requested reviewers
	requestedReviewers := make([]map[string]interface{}, 0)
	for _, rid := range pr.RequestedReviewerIDs {
		if u, ok := st.Users[rid]; ok {
			requestedReviewers = append(requestedReviewers, store.UserToJSON(u, baseURL))
		}
	}
	requestedTeams := make([]map[string]interface{}, 0)
	if org := st.OrgsByLogin[ownerFromRepoFullName(repoFullName)]; org != nil {
		for _, teamID := range pr.RequestedTeamIDs {
			if team := st.Teams[teamID]; team != nil && team.OrgID == org.ID {
				requestedTeams = append(requestedTeams, requestedTeamJSONLocked(st, team, org, baseURL))
			}
		}
	}

	// auto_merge: null when off; when armed, the enabled_by user plus the
	// merge parameters, per the published auto-merge shape.
	var autoMerge interface{}
	if pr.AutoMerge != nil {
		var enabledBy map[string]interface{}
		if u, ok := st.Users[pr.AutoMerge.EnabledByID]; ok {
			enabledBy = store.UserToJSON(u, baseURL)
		}
		autoMerge = map[string]interface{}{
			"enabled_by":     enabledBy,
			"merge_method":   strings.ToLower(pr.AutoMerge.MergeMethod),
			"commit_title":   pr.AutoMerge.CommitHeadline,
			"commit_message": pr.AutoMerge.CommitBody,
		}
	}

	// Milestone and repo conversion happens after unlock: both derive
	// counts under their own locks.
	var milestone *store.Milestone
	if pr.MilestoneID > 0 {
		milestone = st.Milestones[pr.MilestoneID]
	}
	repo := st.ReposByName[repoFullName]
	headRepo := store.PullRequestHeadRepoLocked(st, pr)
	headStor := gitStorage.Storer(nil)
	if headRepo != nil {
		headStor = st.GitStorages[headRepo.FullName]
	}

	st.Mu.RUnlock()

	headSHA := store.ResolveBranchSha(headStor, pr.HeadRefName)
	baseSHA := pr.BaseSHA

	var milestoneJSON interface{}
	if milestone != nil {
		milestoneJSON = milestoneToJSON(milestone, st, baseURL, repoFullName)
	}

	var repoJSON interface{}
	var repoOwnerJSON interface{}
	if repo != nil {
		repoJSON = store.RepoToJSON(repo, st, baseURL)
		repoOwnerJSON = repoOwnerREST(repo, st, baseURL)
	}
	var headRepoJSON interface{}
	var headRepoOwnerJSON interface{}
	headRepoFullName := repoFullName
	if headRepo != nil {
		headRepoFullName = headRepo.FullName
		headRepoJSON = store.RepoToJSON(headRepo, st, baseURL)
		headRepoOwnerJSON = repoOwnerREST(headRepo, st, baseURL)
	}
	headOwnerLogin := ownerFromRepoFullName(headRepoFullName)
	baseOwnerLogin := ownerFromRepoFullName(repoFullName)

	// GitHub's assignee is the first assignee, null when unassigned.
	var assignee interface{}
	if len(assignees) > 0 {
		assignee = assignees[0]
	}

	// author_association relative to the repository: its owner authored
	// it or someone else did. Bleephub does not model commit-derived
	// CONTRIBUTOR status.
	authorAssociation := "NONE"
	if repo != nil && repo.Owner != nil && repo.Owner.ID == pr.AuthorID {
		authorAssociation = "OWNER"
	}

	// REST state: "MERGED" → state:"closed", merged:true
	state := strings.ToLower(pr.State)
	if pr.State == "MERGED" {
		state = "closed"
	}

	var closedAt interface{}
	if pr.ClosedAt != nil {
		closedAt = pr.ClosedAt.Format(time.RFC3339)
	}
	var mergedAt interface{}
	if pr.MergedAt != nil {
		mergedAt = pr.MergedAt.Format(time.RFC3339)
	}
	var mergeCommitSHA interface{}
	if pr.MergeCommitSHA != "" {
		mergeCommitSHA = pr.MergeCommitSHA
	}

	numStr := strconv.Itoa(pr.Number)
	api := baseURL + "/api/v3/repos/" + repoFullName + "/pulls/" + numStr
	issueAPI := baseURL + "/api/v3/repos/" + repoFullName + "/issues/" + numStr
	htmlURL := baseURL + "/" + repoFullName + "/pull/" + numStr
	return map[string]interface{}{
		"id":                  pr.ID,
		"node_id":             pr.NodeID,
		"url":                 api,
		"html_url":            htmlURL,
		"diff_url":            htmlURL + ".diff",
		"patch_url":           htmlURL + ".patch",
		"issue_url":           issueAPI,
		"commits_url":         api + "/commits",
		"review_comments_url": api + "/comments",
		"review_comment_url":  baseURL + "/api/v3/repos/" + repoFullName + "/pulls/comments{/number}",
		"comments_url":        issueAPI + "/comments",
		"statuses_url":        baseURL + "/api/v3/repos/" + repoFullName + "/statuses/" + headSHA,
		"number":              pr.Number,
		"title":               pr.Title,
		"body":                pr.Body,
		"state":               state,
		"locked":              pr.Locked,
		"draft":               pr.IsDraft,
		"user":                authorJSON,
		"head": map[string]interface{}{
			"ref":   pr.HeadRefName,
			"sha":   headSHA,
			"label": headOwnerLogin + ":" + pr.HeadRefName,
			"repo":  headRepoJSON,
			"user":  headRepoOwnerJSON,
		},
		"base": map[string]interface{}{
			"ref":   pr.BaseRefName,
			"sha":   baseSHA,
			"label": baseOwnerLogin + ":" + pr.BaseRefName,
			"repo":  repoJSON,
			"user":  repoOwnerJSON,
		},
		"_links": map[string]interface{}{
			"self":            map[string]interface{}{"href": api},
			"html":            map[string]interface{}{"href": htmlURL},
			"issue":           map[string]interface{}{"href": issueAPI},
			"comments":        map[string]interface{}{"href": issueAPI + "/comments"},
			"review_comments": map[string]interface{}{"href": api + "/comments"},
			"review_comment":  map[string]interface{}{"href": baseURL + "/api/v3/repos/" + repoFullName + "/pulls/comments{/number}"},
			"commits":         map[string]interface{}{"href": api + "/commits"},
			"statuses":        map[string]interface{}{"href": baseURL + "/api/v3/repos/" + repoFullName + "/statuses/" + headSHA},
		},
		"labels":              labels,
		"assignee":            assignee,
		"assignees":           assignees,
		"milestone":           milestoneJSON,
		"requested_reviewers": requestedReviewers,
		"requested_teams":     requestedTeams,
		"author_association":  authorAssociation,
		"auto_merge":          autoMerge,
		"merged_at":           mergedAt,
		"merge_commit_sha":    mergeCommitSHA,
		"created_at":          pr.CreatedAt.Format(time.RFC3339),
		"updated_at":          pr.UpdatedAt.Format(time.RFC3339),
		"closed_at":           closedAt,
	}
}

func ownerFromRepoFullName(fullName string) string {
	owner, _, ok := store.SplitRepoFullName(fullName)
	if !ok {
		return fullName
	}
	return owner
}

func repoOwnerLogin(repo *store.Repo) string {
	if repo == nil {
		return ""
	}
	return ownerFromRepoFullName(repo.FullName)
}

// pullRequestToJSON converts a PullRequest to the full GitHub
// `pull-request` shape served by single-PR operations: the simple shape
// plus merge state, diff stats, and conversation counters. Must not be
// called with st.mu held.
func pullRequestToJSON(pr *store.PullRequest, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	out := pullRequestSimpleJSON(pr, st, baseURL, repoFullName)
	// The `pull-request` shape carries requested_teams as team-simple, which
	// has no `parent` member; only the pull-request-simple shape the base
	// builder renders does. Drop it here so a team code owner requested on a
	// pull request cannot put a key on the wire that the detail schema has not
	// got.
	if teams, ok := out["requested_teams"].([]map[string]interface{}); ok {
		simple := make([]map[string]interface{}, 0, len(teams))
		for _, team := range teams {
			trimmed := make(map[string]interface{}, len(team))
			for key, value := range team {
				if key != "parent" {
					trimmed[key] = value
				}
			}
			simple = append(simple, trimmed)
		}
		out["requested_teams"] = simple
	}

	// Snapshot before reading the mutable merge/diff fields off the pointer.
	pr = st.SnapPR(pr)
	st.Mu.RLock()
	commentCount := st.CountCommentsForLocked("pull_request", pr.ID)
	st.Mu.RUnlock()
	reviewCommentCount := len(st.PRReviewComments.ListForPR(pr.ID))

	merged := pr.State == "MERGED"
	mergeableState := "unknown"
	switch pr.Mergeable {
	case "MERGEABLE":
		mergeableState = "clean"
	case "CONFLICTING":
		mergeableState = "dirty"
	}

	var mergedByJSON interface{}
	if pr.MergedByID > 0 {
		st.Mu.RLock()
		if u, ok := st.Users[pr.MergedByID]; ok {
			mergedByJSON = store.UserToJSON(u, baseURL)
		}
		st.Mu.RUnlock()
	}

	out["merged"] = merged
	var mergeable interface{}
	switch pr.Mergeable {
	case "MERGEABLE":
		mergeable = true
	case "CONFLICTING":
		mergeable = false
	}
	out["mergeable"] = mergeable
	out["mergeable_state"] = mergeableState
	out["maintainer_can_modify"] = pr.MaintainerCanModify
	out["merged_by"] = mergedByJSON
	changed, adds, dels := pr.ChangedFiles, pr.Additions, pr.Deletions
	commitCount := 0
	if repo := st.GetRepoByID(pr.RepoID); repo != nil {
		if commits, err := pullRequestCommitObjects(st, repo, pr); err == nil {
			commitCount = len(commits)
		}
		// The diff totals come from the same merge-base diff the files
		// endpoint serves, recomputed per request while the head can still
		// move so they track new commits. A closed/merged PR whose refs no
		// longer resolve (the recompute comes back empty) keeps the totals
		// recorded while they were still computable.
		if c, a, d, err := pullRequestDiffStats(st, repo, pr); err == nil &&
			(pr.State == "OPEN" || c > 0 || a > 0 || d > 0) {
			changed, adds, dels = c, a, d
			st.SetPullRequestDiffStats(pr.ID, changed, adds, dels)
		}
	}
	out["additions"] = adds
	out["deletions"] = dels
	out["changed_files"] = changed
	out["comments"] = commentCount
	out["review_comments"] = reviewCommentCount
	out["commits"] = commitCount
	return out
}

// pullRequestDiffStats sums the per-file additions/deletions of the same
// merge-base diff GET /pulls/{n}/files serves, giving the detail payload's
// additions/deletions/changed_files counters (which real GitHub carries on
// `pull-request` but not on `pull-request-simple` list items).
func pullRequestDiffStats(st *store.Store, repo *store.Repo, pr *store.PullRequest) (changedFiles, additions, deletions int, err error) {
	files, err := pullRequestChangedFiles(st, repo, pr, "")
	if err != nil {
		return 0, 0, 0, err
	}
	for _, f := range files {
		if a, ok := f["additions"].(int); ok {
			additions += a
		}
		if d, ok := f["deletions"].(int); ok {
			deletions += d
		}
	}
	return len(files), additions, deletions, nil
}

// refreshPullRequestDiffStats recomputes and persists a pull request's diff
// totals after its head moves, so readers that serve the stored fields
// (GraphQL's additions/deletions/changedFiles) stay current without waiting
// for a REST detail fetch.
func (s *Server) refreshPullRequestDiffStats(repo *store.Repo, pr *store.PullRequest) {
	if changed, adds, dels, err := pullRequestDiffStats(s.store, repo, pr); err == nil {
		s.store.SetPullRequestDiffStats(pr.ID, changed, adds, dels)
	}
}

func reviewToJSON(review *store.PullRequestReview, st *store.Store, baseURL, repoFullName string, prNumber int) map[string]interface{} {
	var authorJSON map[string]interface{}
	var authorAssociation string
	st.Mu.RLock()
	if u, ok := st.Users[review.AuthorID]; ok {
		authorJSON = store.UserToJSON(u, baseURL)
	}
	// Match the review author's real relationship to the repo (OWNER / MEMBER /
	// COLLABORATOR / CONTRIBUTOR / NONE), the same enum the sibling review-comment
	// serializer uses, rather than the OWNER-or-CONTRIBUTOR-only approximation.
	authorAssociation = "NONE"
	if repo := st.ReposByName[repoFullName]; repo != nil {
		authorAssociation = store.AuthorAssociationLocked(st, review.AuthorID, repo)
	}
	st.Mu.RUnlock()

	htmlURL := baseURL + "/" + repoFullName + "/pull/" + strconv.Itoa(prNumber) + "#pullrequestreview-" + strconv.Itoa(review.ID)
	pullURL := baseURL + "/api/v3/repos/" + repoFullName + "/pulls/" + strconv.Itoa(prNumber)

	m := map[string]interface{}{
		"id":                 review.ID,
		"node_id":            review.NodeID,
		"user":               authorJSON,
		"body":               review.Body,
		"state":              review.State,
		"commit_id":          pullRequestReviewCommitSHA(review, st),
		"html_url":           htmlURL,
		"pull_request_url":   pullURL,
		"author_association": authorAssociation,
		"_links": map[string]interface{}{
			"html":         map[string]interface{}{"href": htmlURL},
			"pull_request": map[string]interface{}{"href": pullURL},
		},
	}
	// submitted_at is optional and non-nullable: a PENDING review has not been
	// submitted, so GitHub omits the key rather than emitting null.
	if review.SubmittedAt != nil {
		m["submitted_at"] = review.SubmittedAt.Format(time.RFC3339)
	}
	return m
}

// handleListPullRequestFiles serves GET /repos/{owner}/{repo}/pulls/{number}/files,
// the changed-file list with per-file unified-diff patches real GitHub returns.
// It is reached through handlePRCommentTwoSegDispatch (p2 == "files") so it adds
// no new mux pattern. The diff is computed between the merge-base of the PR's
// base and head and the head tip — the same range GitHub reports.
func (s *Server) handleListPullRequestFiles(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	files, err := pullRequestChangedFiles(s.store, repo, pr, s.baseURL(r))
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "diff derivation failed")
		return
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, files))
}

// pullRequestDiffChangeSource is the one derivation of what a pull request
// changed: the storage holding its head branch, the merge base of its base and
// head, and the tree pair the change set is the difference of. Every rendering
// of a PR's changes — the files endpoint, the unified diff, the patch series
// and the review-comment position map — starts here, so they can never
// disagree about which trees were compared.
type pullRequestDiffChangeSource struct {
	stor      gitStorage.Storer
	mergeBase plumbing.Hash
	headHash  plumbing.Hash
	baseTree  *object.Tree // nil for a root-commit head: diffed against the empty tree
	headTree  *object.Tree
}

// pullRequestDiffSource resolves that derivation. It returns (nil, nil) when
// the pull request has nothing resolvable to diff — storage gone, head or base
// ref deleted — which is an empty change set rather than a failure, and a
// non-nil error only when git storage that should hold the objects cannot.
func pullRequestDiffSource(st *store.Store, repo *store.Repo, pr *store.PullRequest) (*pullRequestDiffChangeSource, error) {
	stor, _ := store.PullRequestGitStorage(st, repo, pr)
	if stor == nil {
		return nil, nil
	}
	headHash, err := store.ResolveGitRef(stor, pr.HeadRefName)
	if err != nil {
		return nil, nil
	}
	var baseHash plumbing.Hash
	if pr.BaseSHA != "" {
		baseHash = plumbing.NewHash(pr.BaseSHA)
	} else if baseHash, err = store.ResolveGitRef(stor, pr.BaseRefName); err != nil {
		return nil, nil
	}
	mergeBase, err := store.FindMergeBase(stor, baseHash, headHash)
	if err != nil {
		return nil, err
	}
	headCommit, err := object.GetCommit(stor, headHash)
	if err != nil {
		return nil, err
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return nil, err
	}
	source := &pullRequestDiffChangeSource{stor: stor, mergeBase: mergeBase, headHash: headHash, headTree: headTree}
	if !mergeBase.IsZero() {
		if baseCommit, err := object.GetCommit(stor, mergeBase); err == nil {
			source.baseTree, _ = baseCommit.Tree()
		}
	}
	return source, nil
}

// pullRequestUnifiedDiff renders a pull request as the unified diff GitHub
// serves for the .diff media type: one merge-base→head tree diff, the same
// comparison the files endpoint reports per file, concatenated in tree order.
func pullRequestUnifiedDiff(st *store.Store, repo *store.Repo, pr *store.PullRequest) (string, error) {
	source, err := pullRequestDiffSource(st, repo, pr)
	if err != nil || source == nil {
		return "", err
	}
	changes, err := object.DiffTree(source.baseTree, source.headTree)
	if err != nil {
		return "", err
	}
	var diff strings.Builder
	for _, change := range changes {
		patch, err := change.Patch()
		if err != nil {
			return "", err
		}
		diff.WriteString(patch.String())
	}
	return diff.String(), nil
}

// pullRequestFormatPatch renders a pull request for the .patch media type: a
// git-format-patch series, one mbox-headed patch per commit the PR adds,
// oldest first — not the single tree diff .diff carries.
func pullRequestFormatPatch(st *store.Store, repo *store.Repo, pr *store.PullRequest) (string, error) {
	stor, _ := store.PullRequestGitStorage(st, repo, pr)
	if stor == nil {
		return "", nil
	}
	commits, err := store.PullRequestCommitObjectsFromStorage(stor, pr)
	if err != nil {
		return "", err
	}
	var series strings.Builder
	for i, commit := range commits {
		patch, err := commitFormatPatch(commit)
		if err != nil {
			return "", err
		}
		if i > 0 {
			series.WriteString("\n")
		}
		series.WriteString(patch)
	}
	return series.String(), nil
}

// pullRequestChangedFiles diffs the PR's merge-base against its head tip and
// returns the per-file JSON GitHub's pulls/{n}/files endpoint emits, including
// the unified-diff `patch` for text changes.
func pullRequestChangedFiles(st *store.Store, repo *store.Repo, pr *store.PullRequest, baseURL string) ([]map[string]interface{}, error) {
	source, err := pullRequestDiffSource(st, repo, pr)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return []map[string]interface{}{}, nil
	}
	headHash := source.headHash
	changes, err := object.DiffTree(source.baseTree, source.headTree)
	if err != nil {
		return nil, err
	}
	files := make([]map[string]interface{}, 0, len(changes))
	for _, ch := range changes {
		var status string
		switch {
		case ch.To.TreeEntry.Mode == 0:
			status = "removed"
		case ch.From.TreeEntry.Mode == 0:
			status = "added"
		case ch.From.TreeEntry.Hash == ch.To.TreeEntry.Hash:
			continue // unchanged
		default:
			status = "modified"
		}
		adds, dels, err := changeStats(ch)
		if err != nil {
			return nil, err
		}
		filename := ch.To.Name
		if filename == "" {
			filename = ch.From.Name
		}
		sha := ch.To.TreeEntry.Hash.String()
		if ch.To.TreeEntry.Mode == 0 {
			sha = ch.From.TreeEntry.Hash.String()
		}
		file := map[string]interface{}{
			"sha":          sha,
			"filename":     filename,
			"status":       status,
			"additions":    adds,
			"deletions":    dels,
			"changes":      adds + dels,
			"blob_url":     baseURL + "/" + repo.FullName + "/blob/" + headHash.String() + "/" + filename,
			"raw_url":      baseURL + "/" + repo.FullName + "/raw/" + headHash.String() + "/" + filename,
			"contents_url": baseURL + "/api/v3/repos/" + repo.FullName + "/contents/" + filename + "?ref=" + headHash.String(),
		}
		if patch := changeUnifiedPatch(ch); patch != "" {
			file["patch"] = patch
		}
		if status != "added" && status != "removed" && ch.From.Name != "" && ch.From.Name != filename {
			file["previous_filename"] = ch.From.Name
		}
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		ni, _ := files[i]["filename"].(string)
		nj, _ := files[j]["filename"].(string)
		return ni < nj
	})
	return files, nil
}

// changeUnifiedPatch renders the hunk portion of a change's unified diff, matching
// GitHub's `patch` field (which starts at the first "@@" hunk header, without the
// "diff --git"/index/"---"/"+++" preamble). Returns "" for binary or empty diffs.
func changeUnifiedPatch(ch *object.Change) string {
	patch, err := ch.Patch()
	if err != nil {
		return ""
	}
	full := patch.String()
	if idx := strings.Index(full, "@@"); idx >= 0 {
		return strings.TrimRight(full[idx:], "\n")
	}
	return ""
}
