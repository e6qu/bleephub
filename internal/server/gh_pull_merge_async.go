package bleephub

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// PullRequestMergeAsync records the outcome of an asynchronous pull-request
// merge so a subsequent poll (GET .../merge-async/{uuid}) can report status.
// GitHub's real merge-async endpoint enqueues the merge onto the merge queue;
// bleephub performs the merge synchronously and stores the terminal result
// keyed by a generated UUID, which the poll endpoint then returns.
type PullRequestMergeAsync struct {
	UUID            string    `json:"uuid"`
	RepoID          int       `json:"repo_id"`
	PRNumber        int       `json:"pr_number"`
	Status          string    `json:"status"` // pending | enqueued | merged | failed
	MergeMethod     string    `json:"merge_method"`
	MergeAction     string    `json:"merge_action"`
	Message         string    `json:"message"`
	SHA             string    `json:"sha"`
	ExpectedHeadSHA string    `json:"expected_head_sha"`
	CreatedAt       time.Time `json:"created_at"`
}

// RecordPullRequestMergeAsync stores an async merge result keyed by its UUID.
func (st *Store) RecordPullRequestMergeAsync(rec *PullRequestMergeAsync) {
	st.mu.Lock()
	defer st.mu.Unlock()
	copy := *rec
	st.PullRequestMergeAsync[rec.UUID] = &copy
}

// GetPullRequestMergeAsync returns the async merge result for a UUID, or nil.
func (st *Store) GetPullRequestMergeAsync(uuid string) *PullRequestMergeAsync {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if rec := st.PullRequestMergeAsync[uuid]; rec != nil {
		copy := *rec
		return &copy
	}
	return nil
}

// renderMergeAsyncResult shapes a record into the pull-request-merge-async-result
// payload: a status plus a details object whose fields depend on the outcome
// (the OpenAPI schema models details as a oneOf of three shapes).
func renderMergeAsyncResult(rec *PullRequestMergeAsync) map[string]interface{} {
	var details map[string]interface{}
	switch rec.Status {
	case "merged":
		details = map[string]interface{}{
			"message": rec.Message,
			"sha":     rec.SHA,
		}
	case "failed":
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

// handleMergePullRequestAsync implements
// PUT /repos/{owner}/{repo}/pulls/{number}/merge-async — it validates and
// performs the merge, stores the terminal result under a fresh UUID, and
// returns an "enqueued" acknowledgement the caller can poll.
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
		writeGHValidationError(w, "PullRequest", "merge_method", "invalid")
		return
	}
	mergeMethod := req.MergeMethod
	if mergeMethod == "" {
		mergeMethod = "default"
	}

	// Already merged: report the merged terminal state (200).
	if pr.State == "MERGED" {
		writeJSON(w, http.StatusOK, renderMergeAsyncResult(&PullRequestMergeAsync{
			Status:  "merged",
			Message: "Pull Request already merged",
			SHA:     pr.MergeCommitSHA,
		}))
		return
	}
	if pr.State == "CLOSED" {
		writeGHError(w, http.StatusUnprocessableEntity, "Pull Request is closed")
		return
	}

	// Expected-head guard: merging against a stale head SHA is a 409.
	if req.SHA != "" {
		if head := s.prHeadSha(repo, pr); head != "" && head != req.SHA {
			writeGHError(w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
			return
		}
	}

	// Branch protection: required status checks must be green on the head commit.
	if headSha := s.prHeadSha(repo, pr); headSha != "" {
		if st := s.evaluateChecksForMerge(repo, pr.BaseRefName, headSha); len(st.MissingRequired) > 0 {
			writeJSON(w, http.StatusConflict, renderMergeAsyncResult(&PullRequestMergeAsync{
				Status:  "failed",
				Message: fmt.Sprintf("Required status check %q is expected.", st.MissingRequired[0]),
			}))
			return
		}
	}

	if ok, msg := s.canMergePullRequest(r.Context(), repo, pr); !ok {
		if msg == "" {
			msg = "Pull Request is not mergeable"
		}
		writeJSON(w, http.StatusConflict, renderMergeAsyncResult(&PullRequestMergeAsync{
			Status:  "failed",
			Message: msg,
		}))
		return
	}

	expectedHead := s.prHeadSha(repo, pr)
	mergeSha, errMsg := s.completePullRequestMerge(repo, pr, user, req.MergeMethod, req.CommitTitle, req.CommitMessage)
	if errMsg != "" {
		writeJSON(w, http.StatusConflict, renderMergeAsyncResult(&PullRequestMergeAsync{
			Status:  "failed",
			Message: errMsg,
		}))
		return
	}

	merged := s.store.GetPullRequest(pr.ID)
	repoKey := owner + "/" + repoName
	mergedPayload := buildPullRequestPayload(s.store, repo, merged, user, "closed")
	s.emitWebhookEvent(repoKey, "pull_request", "closed", mergedPayload)

	rec := &PullRequestMergeAsync{
		UUID:            uuid.New().String(),
		RepoID:          repo.ID,
		PRNumber:        num,
		Status:          "merged",
		MergeMethod:     mergeMethod,
		MergeAction:     "direct_merge",
		Message:         "Pull Request successfully merged",
		SHA:             mergeSha,
		ExpectedHeadSHA: expectedHead,
		CreatedAt:       s.currentTime(),
	}
	s.store.RecordPullRequestMergeAsync(rec)

	// Acknowledge as enqueued with the poll UUID (202); the merge itself is
	// already durable and a poll will report "merged".
	writeJSON(w, http.StatusAccepted, renderMergeAsyncResult(&PullRequestMergeAsync{
		UUID:            rec.UUID,
		Status:          "enqueued",
		MergeMethod:     mergeMethod,
		MergeAction:     "direct_merge",
		Message:         "Merge enqueued",
		ExpectedHeadSHA: expectedHead,
	}))
}

// handleGetMergePullRequestAsyncResult implements
// GET /repos/{owner}/{repo}/pulls/{number}/merge-async/{uuid} — it returns the
// stored terminal result for a previously enqueued async merge.
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
