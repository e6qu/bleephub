package store

import (
	"sort"
	"strconv"
)

// The merge queue, and the per-reviewer "viewed" marks on a diff.
//
// A merge queue belongs to a base branch of a repository: pull requests
// targeting that branch join it in order and are merged in that order. The
// queue is not a table of its own — a pull request's place in it is a property
// of the pull request, so enqueuing and dequeuing are writes to the rows that
// are already persisted, and a queue can never reference a pull request that
// no longer exists.

// MergeQueuePullRequests returns the pull requests queued against one base
// branch of a repository, in queue order.
func (st *Store) MergeQueuePullRequests(repoID int, baseRef string) []*PullRequest {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.mergeQueuePullRequestsLocked(repoID, baseRef)
}

func (st *Store) mergeQueuePullRequestsLocked(repoID int, baseRef string) []*PullRequest {
	var queued []*PullRequest
	for _, pr := range st.PullRequests {
		if pr.RepoID == repoID && pr.BaseRefName == baseRef && pr.MergeQueuePosition > 0 {
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

// EnqueuePullRequest puts an open pull request at the back of its base
// branch's merge queue, or at the front when jump is set. It answers the
// stored row, or nil when the pull request is not open or is already queued.
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
		// Jumping the queue shifts everyone already in it back one place, so
		// the positions stay a dense 1..n ordering.
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

// DequeuePullRequest takes a pull request out of its merge queue and closes
// the gap it leaves. It answers the row as it was while queued, so a caller
// can render the entry it removed, or nil when it was not queued.
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

// rawMergeQueueLocked returns the live queued rows in order; callers hold the
// write lock and mutate them.
func (st *Store) rawMergeQueueLocked(repoID int, baseRef string) []*PullRequest {
	var queued []*PullRequest
	for _, pr := range st.PullRequests {
		if pr.RepoID == repoID && pr.BaseRefName == baseRef && pr.MergeQueuePosition > 0 {
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

// --- per-reviewer viewed files -------------------------------------------

// SetPullRequestFileViewed records (or clears) one reviewer's "viewed" mark on
// one file of a pull request's diff. It answers false when the pull request
// does not exist.
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

// PullRequestViewedFiles returns the paths one reviewer has marked viewed.
func (st *Store) PullRequestViewedFiles(prID, reviewerID int) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	pr := st.PullRequests[prID]
	if pr == nil {
		return nil
	}
	return append([]string(nil), pr.ViewedFiles[reviewerID]...)
}
