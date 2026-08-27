package store

import "time"

// PullRequestMergeAsync records an async merge outcome for the
// GET .../merge-async/{uuid} poll. GitHub enqueues onto the merge queue;
// bleephub merges synchronously and stores the terminal result by UUID.
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

// MergeAsyncStatus is the terminal state of an async merge record.
type MergeAsyncStatus string
