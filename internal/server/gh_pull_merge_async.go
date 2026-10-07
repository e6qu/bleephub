package bleephub

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

const (
	MergeAsyncPending  store.MergeAsyncStatus = "pending"
	MergeAsyncEnqueued store.MergeAsyncStatus = "enqueued"
	MergeAsyncMerged   store.MergeAsyncStatus = "merged"
	MergeAsyncFailed   store.MergeAsyncStatus = "failed"
)

// renderMergeAsyncResult shapes a record into the merge-async-result payload.
// details is a oneOf of three shapes keyed on the outcome.
func renderMergeAsyncResult(rec *store.PullRequestMergeAsync) map[string]interface{} {
	var details map[string]interface{}
	switch rec.Status {
	case MergeAsyncMerged:
		details = map[string]interface{}{
			"message": rec.Message,
			"sha":     rec.SHA,
		}
	case MergeAsyncFailed, MergeAsyncEnqueued:
		details = map[string]interface{}{
			"message": rec.Message,
		}
	default: // pending
		details = map[string]interface{}{
			"message":           rec.Message,
			"uuid":              rec.UUID,
			"merge_method":      rec.MergeMethod,
			"merge_action":      rec.MergeAction,
			"expected_head_sha": rec.ExpectedHeadSHA,
			"bypass_rules":      rec.BypassRules,
		}
	}
	return map[string]interface{}{
		"status":  rec.Status,
		"details": details,
	}
}

// handleMergePullRequestAsync performs the merge, stores the terminal result
// under a fresh UUID, and returns an "enqueued" acknowledgement to poll.
func (s *Server) handleMergePullRequestAsync(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.store.GetRepo(owner, repoName)
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
	if s.rejectIfArchived(w, repo) {
		return
	}

	var req struct {
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		SHA           string `json:"sha"`
		MergeMethod   string `json:"merge_method"`
		MergeAction   string `json:"merge_action"`
		BypassRules   bool   `json:"bypass_rules"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	// No branch configures a merge queue here, so `default` merges directly;
	// `merge_queue` adds the pull request to its base branch's queue.
	mergeAction := req.MergeAction
	switch mergeAction {
	case "", "default", "direct_merge":
		mergeAction = "direct_merge"
	case "merge_queue":
		if req.MergeMethod != "" || req.CommitTitle != "" || req.CommitMessage != "" {
			store.WriteGHValidationError(w, "PullRequest", "merge_action", "invalid")
			return
		}
	default:
		store.WriteGHValidationError(w, "PullRequest", "merge_action", "invalid")
		return
	}
	switch req.MergeMethod {
	case "", "merge", "squash", "rebase":
	default:
		store.WriteGHValidationError(w, "PullRequest", "merge_method", "invalid")
		return
	}
	// 405 an explicit merge method the repository has disabled — the async path
	// must not accept a method the synchronous merge endpoint (and github.com)
	// refuse.
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
	mergeMethod := req.MergeMethod
	if mergeMethod == "" {
		mergeMethod = "default"
	}

	if pr.State == "MERGED" {
		writeJSON(w, http.StatusOK, renderMergeAsyncResult(&store.PullRequestMergeAsync{
			Status:  MergeAsyncMerged,
			Message: "Pull Request already merged",
			SHA:     pr.MergeCommitSHA,
		}))
		return
	}
	if pr.State == "CLOSED" {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull Request is closed")
		return
	}
	if pr.IsDraft {
		writeGHError(w, http.StatusMethodNotAllowed, "Draft pull requests cannot be merged.")
		return
	}

	// Merging against a stale head SHA is a 409.
	if req.SHA != "" {
		if head := s.prHeadSha(repo, pr); head != "" && head != req.SHA {
			writeGHError(w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
			return
		}
	}

	if mergeAction == "merge_queue" {
		s.enqueuePullRequestAsync(w, r, repo, pr, req.BypassRules)
		return
	}

	// Branch protection: required status checks must be green on the head commit.
	if headSha := s.prHeadSha(repo, pr); headSha != "" {
		if st := s.evaluateChecksForMerge(repo, pr.BaseRefName, headSha); len(st.MissingRequired) > 0 {
			writeJSON(w, http.StatusConflict, renderMergeAsyncResult(&store.PullRequestMergeAsync{
				Status:  MergeAsyncFailed,
				Message: fmt.Sprintf("Required status check %q is expected.", st.MissingRequired[0]),
			}))
			return
		}
	}

	if ok, msg := s.canMergePullRequest(r.Context(), repo, pr); !ok {
		if msg == "" {
			msg = "Pull Request is not mergeable"
		}
		writeJSON(w, http.StatusConflict, renderMergeAsyncResult(&store.PullRequestMergeAsync{
			Status:  MergeAsyncFailed,
			Message: msg,
		}))
		return
	}

	expectedHead := s.prHeadSha(repo, pr)
	mergeSha, errMsg := s.completePullRequestMerge(repo, pr, user, req.MergeMethod, req.CommitTitle, req.CommitMessage, expectedHead)
	if errMsg != "" {
		writeJSON(w, http.StatusConflict, renderMergeAsyncResult(&store.PullRequestMergeAsync{
			Status:  MergeAsyncFailed,
			Message: errMsg,
		}))
		return
	}

	merged := s.store.GetPullRequest(pr.ID)
	repoKey := owner + "/" + repoName
	mergedPayload := buildPullRequestPayload(s.store, repo, merged, user, "closed", s.baseURL(r))
	s.emitWebhookEvent(repoKey, "pull_request", "closed", mergedPayload)

	rec := &store.PullRequestMergeAsync{
		UUID:            uuid.New().String(),
		RepoID:          repo.ID,
		PRNumber:        num,
		Status:          MergeAsyncMerged,
		MergeMethod:     mergeMethod,
		MergeAction:     "direct_merge",
		Message:         "Pull Request successfully merged",
		SHA:             mergeSha,
		ExpectedHeadSHA: expectedHead,
		BypassRules:     req.BypassRules,
		CreatedAt:       s.currentTime(),
	}
	s.store.RecordPullRequestMergeAsync(rec)

	// Acknowledge 202 "pending" with the poll UUID; the merge is already
	// durable, so a poll reports "merged".
	writeJSON(w, http.StatusAccepted, renderMergeAsyncResult(&store.PullRequestMergeAsync{
		UUID:            rec.UUID,
		Status:          MergeAsyncPending,
		MergeMethod:     mergeMethod,
		MergeAction:     "direct_merge",
		Message:         "Merge request accepted",
		ExpectedHeadSHA: expectedHead,
		BypassRules:     req.BypassRules,
	}))
}

// enqueuePullRequestAsync adds the pull request to its base branch's merge
// queue. One already queued answers 200 "enqueued" at once; a new entry answers
// 202 "pending" with the UUID whose result reports "enqueued".
func (s *Server) enqueuePullRequestAsync(w http.ResponseWriter, r *http.Request, repo *store.Repo, pr *store.PullRequest, bypassRules bool) {
	if pr.MergeQueuePosition > 0 {
		writeJSON(w, http.StatusOK, renderMergeAsyncResult(&store.PullRequestMergeAsync{
			Status:  MergeAsyncEnqueued,
			Message: "Pull Request is already in the merge queue",
		}))
		return
	}
	// The queue resolves being up to date and re-runs checks; the review
	// requirements must already hold, as for the GraphQL enqueuePullRequest.
	if ok, msg := s.mergeQueueEligible(r.Context(), repo, pr); !ok {
		writeJSON(w, http.StatusConflict, renderMergeAsyncResult(&store.PullRequestMergeAsync{
			Status:  MergeAsyncFailed,
			Message: msg,
		}))
		return
	}
	queued := s.store.EnqueuePullRequest(pr.ID, false)
	if queued == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull Request is not mergeable")
		return
	}
	expectedHead := s.prHeadSha(repo, pr)
	rec := &store.PullRequestMergeAsync{
		UUID:            uuid.New().String(),
		RepoID:          repo.ID,
		PRNumber:        pr.Number,
		Status:          MergeAsyncEnqueued,
		MergeMethod:     "default",
		MergeAction:     "merge_queue",
		Message:         "Pull Request added to the merge queue",
		ExpectedHeadSHA: expectedHead,
		BypassRules:     bypassRules,
		CreatedAt:       s.currentTime(),
	}
	s.store.RecordPullRequestMergeAsync(rec)
	s.advanceMergeQueue(repo, queued.BaseRefName)
	writeJSON(w, http.StatusAccepted, renderMergeAsyncResult(&store.PullRequestMergeAsync{
		UUID:            rec.UUID,
		Status:          MergeAsyncPending,
		MergeMethod:     "default",
		MergeAction:     "merge_queue",
		Message:         "Merge request accepted",
		ExpectedHeadSHA: expectedHead,
		BypassRules:     bypassRules,
	}))
}

// handleGetMergePullRequestAsyncResult returns the stored terminal result for a
// previously enqueued async merge.
func (s *Server) handleGetMergePullRequestAsyncResult(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.store.GetRepo(owner, repoName)
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

	rec := s.store.GetPullRequestMergeAsync(r.PathValue("uuid"))
	if rec == nil || rec.RepoID != repo.ID || rec.PRNumber != num {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, renderMergeAsyncResult(mergeQueueOutcome(rec, pr)))
}

// mergeQueueOutcome reports a queued request as the queue has since decided:
// merged once the queue merged the pull request, failed once it left the queue
// unmerged, and enqueued while it waits.
func mergeQueueOutcome(rec *store.PullRequestMergeAsync, pr *store.PullRequest) *store.PullRequestMergeAsync {
	if rec.MergeAction != "merge_queue" || rec.Status != MergeAsyncEnqueued {
		return rec
	}
	switch {
	case pr.State == "MERGED":
		return &store.PullRequestMergeAsync{Status: MergeAsyncMerged, Message: "Pull Request successfully merged", SHA: pr.MergeCommitSHA}
	case pr.MergeQueuePosition == 0:
		return &store.PullRequestMergeAsync{Status: MergeAsyncFailed, Message: "Pull Request was removed from the merge queue"}
	}
	return rec
}
