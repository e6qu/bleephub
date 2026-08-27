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
	case MergeAsyncFailed:
		details = map[string]interface{}{
			"message": rec.Message,
		}
	default: // pending, enqueued
		details = map[string]interface{}{
			"message":           rec.Message,
			"uuid":              rec.UUID,
			"merge_method":      rec.MergeMethod,
			"merge_action":      rec.MergeAction,
			"expected_head_sha": rec.ExpectedHeadSHA,
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

	var req struct {
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		SHA           string `json:"sha"`
		MergeMethod   string `json:"merge_method"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	switch req.MergeMethod {
	case "", "merge", "squash", "rebase":
	default:
		store.WriteGHValidationError(w, "PullRequest", "merge_method", "invalid")
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

	// Merging against a stale head SHA is a 409.
	if req.SHA != "" {
		if head := s.prHeadSha(repo, pr); head != "" && head != req.SHA {
			writeGHError(w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
			return
		}
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
		CreatedAt:       s.currentTime(),
	}
	s.store.RecordPullRequestMergeAsync(rec)

	// Acknowledge 202 "enqueued" with the poll UUID; the merge is already
	// durable, so a poll reports "merged".
	writeJSON(w, http.StatusAccepted, renderMergeAsyncResult(&store.PullRequestMergeAsync{
		UUID:            rec.UUID,
		Status:          MergeAsyncEnqueued,
		MergeMethod:     mergeMethod,
		MergeAction:     "direct_merge",
		Message:         "Merge enqueued",
		ExpectedHeadSHA: expectedHead,
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
	writeJSON(w, http.StatusOK, renderMergeAsyncResult(rec))
}
