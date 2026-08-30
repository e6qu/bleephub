package store

import (
	"sort"
	"strconv"
)

// Merge queue and per-reviewer "viewed" diff marks.
//
// A PR's queue position is a field on the PR, not a table of its own, so a
// queue can never reference a PR that no longer exists.

// MergeQueuePullRequests returns the PRs queued against one base branch, in
// queue order.
func (st *Store) MergeQueuePullRequests(repoID int, baseRef string) []*PullRequest {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.mergeQueuePullRequestsLocked(repoID, baseRef)
}

func (st *Store) mergeQueuePullRequestsLocked(repoID int, baseRef string) []*PullRequest {
	var queued []*PullRequest
	for _, pr := range st.PullsByRepo[repoID] {
		if pr.BaseRefName == baseRef && pr.MergeQueuePosition > 0 {
			queued = append(queued, pr)
		}
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].MergeQueuePosition != queued[j].MergeQueuePosition {
			return queued[i].MergeQueuePosition < queued[j].MergeQueuePosition
		}
		return queued[i].ID < queued[j].ID
	})
	out := make([]*PullRequest, 0, len(queued))
	for _, pr := range queued {
		out = append(out, clonePullRequest(pr))
	}
	return out
}

// EnqueuePullRequest puts an open PR at the back of its base branch's queue, or
// the front when jump is set. Returns nil when the PR is not open or is queued.
func (st *Store) EnqueuePullRequest(prID int, jump bool) *PullRequest {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullRequests[prID]
	if pr == nil || pr.State != "OPEN" || pr.MergeQueuePosition > 0 {
		return nil
	}
	queued := st.rawMergeQueueLocked(pr.RepoID, pr.BaseRefName)
	now := st.CurrentTime()
	batch := NewPersistBatch(st.Persist)
	if jump {
		// Shift everyone back one place to keep positions a dense 1..n.
		for _, other := range queued {
			other.MergeQueuePosition++
			batch.Put("pull_requests", strconv.Itoa(other.ID), other)
		}
		pr.MergeQueuePosition = 1
	} else {
		pr.MergeQueuePosition = len(queued) + 1
	}
	pr.MergeQueueEnqueuedAt = &now
	pr.UpdatedAt = now
	batch.Put("pull_requests", strconv.Itoa(pr.ID), pr)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "pull_requests", Err: err})
	}
	return clonePullRequest(pr)
}

// DequeuePullRequest removes a PR from its queue and closes the gap. Returns
// the row as it was while queued (for rendering), or nil when it was not queued.
func (st *Store) DequeuePullRequest(prID int) *PullRequest {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullRequests[prID]
	if pr == nil || pr.MergeQueuePosition == 0 {
		return nil
	}
	removed := clonePullRequest(pr)
	position := pr.MergeQueuePosition
	now := st.CurrentTime()
	batch := NewPersistBatch(st.Persist)
	pr.MergeQueuePosition = 0
	pr.MergeQueueEnqueuedAt = nil
	pr.UpdatedAt = now
	batch.Put("pull_requests", strconv.Itoa(pr.ID), pr)
	for _, other := range st.rawMergeQueueLocked(pr.RepoID, pr.BaseRefName) {
		if other.ID == pr.ID || other.MergeQueuePosition < position {
			continue
		}
		other.MergeQueuePosition--
		batch.Put("pull_requests", strconv.Itoa(other.ID), other)
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "pull_requests", Err: err})
	}
	return removed
}

// rawMergeQueueLocked returns the live queued rows in order for the caller,
// holding the write lock, to mutate.
func (st *Store) rawMergeQueueLocked(repoID int, baseRef string) []*PullRequest {
	var queued []*PullRequest
	for _, pr := range st.PullsByRepo[repoID] {
		if pr.BaseRefName == baseRef && pr.MergeQueuePosition > 0 {
			queued = append(queued, pr)
		}
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].MergeQueuePosition != queued[j].MergeQueuePosition {
			return queued[i].MergeQueuePosition < queued[j].MergeQueuePosition
		}
		return queued[i].ID < queued[j].ID
	})
	return queued
}

// per-reviewer viewed files

// SetPullRequestFileViewed sets or clears one reviewer's "viewed" mark on one
// file of a PR diff. Returns false when the PR does not exist.
func (st *Store) SetPullRequestFileViewed(prID, reviewerID int, path string, viewed bool) bool {
	if path == "" {
		return false
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullRequests[prID]
	if pr == nil {
		return false
	}
	if pr.ViewedFiles == nil {
		pr.ViewedFiles = map[int][]string{}
	}
	existing := pr.ViewedFiles[reviewerID]
	next := make([]string, 0, len(existing)+1)
	for _, seen := range existing {
		if seen != path {
			next = append(next, seen)
		}
	}
	if viewed {
		next = append(next, path)
		sort.Strings(next)
	}
	if len(next) == 0 {
		delete(pr.ViewedFiles, reviewerID)
	} else {
		pr.ViewedFiles[reviewerID] = next
	}
	pr.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("pull_requests", strconv.Itoa(pr.ID), pr)
	}
	return true
}

// PullRequestViewedFiles returns the paths a reviewer has marked viewed.
func (st *Store) PullRequestViewedFiles(prID, reviewerID int) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	pr := st.PullRequests[prID]
	if pr == nil {
		return nil
	}
	return append([]string(nil), pr.ViewedFiles[reviewerID]...)
}
