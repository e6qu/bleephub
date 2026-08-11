package store

import "time"

// PullRequestMergeAsync records the outcome of an asynchronous pull-request
// merge so a subsequent poll (GET .../merge-async/{uuid}) can report status.
// GitHub's real merge-async endpoint enqueues the merge onto the merge queue;
// bleephub performs the merge synchronously and stores the terminal result
// keyed by a generated UUID, which the poll endpoint then returns.
type PullRequestMergeAsync struct {
	UUID            string           `json:"uuid"`
	RepoID          int              `json:"repo_id"`
	PRNumber        int              `json:"pr_number"`
	Status          MergeAsyncStatus `json:"status"`
	MergeMethod     string           `json:"merge_method"`
	MergeAction     string           `json:"merge_action"`
	Message         string           `json:"message"`
	SHA             string           `json:"sha"`
	ExpectedHeadSHA string           `json:"expected_head_sha"`
	CreatedAt       time.Time        `json:"created_at"`
}

// RecordPullRequestMergeAsync stores an async merge result keyed by its UUID.
func (st *Store) RecordPullRequestMergeAsync(rec *PullRequestMergeAsync) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	copy := *rec
	st.PullRequestMergeAsync[rec.UUID] = &copy
}

// GetPullRequestMergeAsync returns the async merge result for a UUID, or nil.
func (st *Store) GetPullRequestMergeAsync(uuid string) *PullRequestMergeAsync {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if rec := st.PullRequestMergeAsync[uuid]; rec != nil {
		copy := *rec
		return &copy
	}
	return nil
}

// MergeAsyncStatus is the terminal state of an async merge record. Only these
// four values occur; typing the field keeps the set in code rather than a
// comment. A typed string marshals to JSON identically to a plain string.
type MergeAsyncStatus string
