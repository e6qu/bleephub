package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PullRequest represents a GitHub pull request.
type PullRequest struct {
	ID                      int
	NodeID                  string
	Number                  int // per-repo, SHARED with issues via NextIssueNumber
	RepoID                  int
	Title                   string
	Body                    string
	State                   string // "OPEN", "CLOSED", "MERGED"
	IsDraft                 bool
	HeadRefName             string // source branch name
	HeadRepoID              int    // source repository; zero on legacy rows means RepoID
	BaseRefName             string // target branch name
	BaseSHA                 string // base branch commit at PR creation ("" when the repo had no git objects)
	MergeCommitSHA          string // merge result commit ("" until merged, or when merged without git refs)
	PotentialMergeCommitSHA string // test-merge of head into base for an open PR ("" if unmergeable/no git); reported to pull_request workflow runs (ACT-027)
	MaintainerCanModify     bool
	AuthorID                int
	AssigneeIDs             []int
	LabelIDs                []int
	RequestedReviewerIDs    []int
	RequestedTeamIDs        []int
	MilestoneID             int    // 0 = none
	Mergeable               string // "MERGEABLE", "CONFLICTING", "UNKNOWN"
	Additions               int
	Deletions               int
	ChangedFiles            int
	MergedByID              int // 0 = not merged
	Locked                  bool
	ActiveLockReason        LockReason // empty = locked without a stated reason
	CreatedAt               time.Time
	UpdatedAt               time.Time
	ClosedAt                *time.Time
	MergedAt                *time.Time
	// AutoMerge is the armed auto-merge request, nil when off. Cleared whenever
	// the PR leaves the OPEN state.
	AutoMerge *PullRequestAutoMerge
	// Archived hides the PR from default views while keeping its state.
	Archived bool
	// ViewedFiles is the per-reviewer set of file paths marked viewed in the
	// diff, keyed by reviewer account id.
	ViewedFiles map[int][]string
	// MergeQueuePosition is the 1-based place in the base branch's merge queue;
	// zero means not queued.
	MergeQueuePosition   int
	MergeQueueEnqueuedAt *time.Time
	// RevertedByID is the PR opened to revert this one, zero until one is.
	RevertedByID int
	// RevertsID is the PR this one reverts, zero when it reverts nothing.
	RevertsID int
}

// PullRequestAutoMerge captures who armed auto-merge on a pull request and
// the merge parameters to use once its blocking conditions clear.
type PullRequestAutoMerge struct {
	EnabledByID    int
	MergeMethod    string // "MERGE", "SQUASH", "REBASE"
	CommitHeadline string
	CommitBody     string
	AuthorEmail    string
	EnabledAt      time.Time
}

// PullRequestReview represents a review on a pull request.
type PullRequestReview struct {
	ID               int
	NodeID           string
	PRID             int // PullRequest.ID
	AuthorID         int
	State            string // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "PENDING", "DISMISSED"
	Body             string
	SubmittedAt      *time.Time
	DismissedAt      *time.Time
	DismissalMessage string
	// PreviousState is the state held before dismissal ("" if never dismissed).
	// Dismissal overwrites State, and ReviewDismissedEvent.previousReviewState
	// needs the overturned standing.
	PreviousState string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type PullRequestOptions struct {
	HeadRepoID          int
	MaintainerCanModify bool
}

var ErrOpenPullRequestExists = errors.New("an open pull request already exists for the head and base")

// CreatePullRequest creates a pull request. Numbering shares the repo's
// NextIssueNumber counter with issues.
func (st *Store) CreatePullRequest(repoID, authorID int, title, body, headRefName, baseRefName string, isDraft bool, labelIDs, assigneeIDs []int, milestoneID int, opts ...PullRequestOptions) *PullRequest {
	pr, _ := st.CreatePullRequestChecked(repoID, authorID, title, body, headRefName, baseRefName, isDraft, labelIDs, assigneeIDs, milestoneID, opts...)
	return pr
}

// CreatePullRequestChecked atomically enforces GitHub's invariant that a
// repository cannot have two open pull requests with the same source
// repository/ref and base ref.
func (st *Store) CreatePullRequestChecked(repoID, authorID int, title, body, headRefName, baseRefName string, isDraft bool, labelIDs, assigneeIDs []int, milestoneID int, opts ...PullRequestOptions) (*PullRequest, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return nil, nil
	}
	headRepoID := repoID
	maintainerCanModify := false
	if len(opts) > 0 {
		if opts[0].HeadRepoID != 0 {
			headRepoID = opts[0].HeadRepoID
		}
		maintainerCanModify = opts[0].MaintainerCanModify
	}
	headRepo := st.Repos[headRepoID]
	if headRepo == nil {
		return nil, nil
	}

	if baseRefName == "" {
		baseRefName = repo.DefaultBranch
	}
	for _, existing := range st.PullsByRepo[repoID] {
		existingHeadRepoID := existing.HeadRepoID
		if existingHeadRepoID == 0 {
			existingHeadRepoID = existing.RepoID
		}
		if existing.State == "OPEN" &&
			existingHeadRepoID == headRepoID &&
			existing.HeadRefName == headRefName &&
			existing.BaseRefName == baseRefName {
			return nil, ErrOpenPullRequestExists
		}
	}

	if labelIDs == nil {
		labelIDs = []int{}
	}
	if assigneeIDs == nil {
		assigneeIDs = []int{}
	}
	headStor := st.GitStorages[headRepo.FullName]
	baseStor := st.GitStorages[repo.FullName]
	headSHA := ResolveBranchSha(headStor, headRefName)
	baseSHA := ResolveBranchSha(baseStor, baseRefName)
	if headSHA == "" || baseSHA == "" {
		return nil, nil
	}

	now := st.CurrentTime()
	pr := &PullRequest{
		ID:                  st.NextPR,
		NodeID:              fmt.Sprintf("PR_kgDO%08d", st.NextPR),
		Number:              repo.NextIssueNumber, // shared counter
		RepoID:              repoID,
		Title:               title,
		Body:                body,
		State:               "OPEN",
		IsDraft:             isDraft,
		HeadRefName:         headRefName,
		HeadRepoID:          headRepoID,
		BaseRefName:         baseRefName,
		MaintainerCanModify: maintainerCanModify,
		// The commit range stays anchored to the base at PR creation even
		// after the base branch advances (including past the PR's merge commit).
		BaseSHA:     baseSHA,
		AuthorID:    authorID,
		AssigneeIDs: assigneeIDs,
		LabelIDs:    labelIDs,
		MilestoneID: milestoneID,
		Mergeable:   "MERGEABLE",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	repo.NextIssueNumber++
	st.NextPR++
	st.PullRequests[pr.ID] = pr
	st.IndexPullLocked(pr)
	if st.Persist != nil {
		st.Persist.MustPut("pull_requests", strconv.Itoa(pr.ID), pr)
	}
	return pr, nil
}

// IndexPullLocked records the PR in the per-repo secondary index so lookups
// resolve in O(PRs-in-repo) instead of a full store scan. Caller holds st.Mu.
func (st *Store) IndexPullLocked(pr *PullRequest) {
	m := st.PullsByRepo[pr.RepoID]
	if m == nil {
		m = make(map[int]*PullRequest)
		st.PullsByRepo[pr.RepoID] = m
	}
	m[pr.Number] = pr
}

// unindexPullLocked removes the PR from the per-repo secondary index.
// Caller holds st.Mu.
func (st *Store) unindexPullLocked(pr *PullRequest) {
	if m := st.PullsByRepo[pr.RepoID]; m != nil {
		delete(m, pr.Number)
		if len(m) == 0 {
			delete(st.PullsByRepo, pr.RepoID)
		}
	}
}

// clonePullRequest returns a detached snapshot safe to hand outside the store
// lock (STORE-021). PR writes go through the keyed UpdatePullRequest(id, fn).
func clonePullRequest(pr *PullRequest) *PullRequest {
	if pr == nil {
		return nil
	}
	clone := *pr
	if pr.AssigneeIDs != nil {
		clone.AssigneeIDs = append([]int(nil), pr.AssigneeIDs...)
	}
	if pr.LabelIDs != nil {
		clone.LabelIDs = append([]int(nil), pr.LabelIDs...)
	}
	if pr.RequestedReviewerIDs != nil {
		clone.RequestedReviewerIDs = append([]int(nil), pr.RequestedReviewerIDs...)
	}
	if pr.RequestedTeamIDs != nil {
		clone.RequestedTeamIDs = append([]int(nil), pr.RequestedTeamIDs...)
	}
	if pr.ClosedAt != nil {
		closed := *pr.ClosedAt
		clone.ClosedAt = &closed
	}
	if pr.MergedAt != nil {
		merged := *pr.MergedAt
		clone.MergedAt = &merged
	}
	if pr.AutoMerge != nil {
		autoMerge := *pr.AutoMerge
		clone.AutoMerge = &autoMerge
	}
	if pr.MergeQueueEnqueuedAt != nil {
		enqueued := *pr.MergeQueueEnqueuedAt
		clone.MergeQueueEnqueuedAt = &enqueued
	}
	if pr.ViewedFiles != nil {
		clone.ViewedFiles = make(map[int][]string, len(pr.ViewedFiles))
		for reviewerID, paths := range pr.ViewedFiles {
			clone.ViewedFiles[reviewerID] = append([]string(nil), paths...)
		}
	}
	return &clone
}

func (st *Store) GetPullRequest(id int) *PullRequest {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return clonePullRequest(st.PullRequests[id])
}

// GetPullRequestByNumber returns a pull request by repo ID and number.
func (st *Store) GetPullRequestByNumber(repoID, number int) *PullRequest {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return clonePullRequest(st.PullsByRepo[repoID][number])
}

// ListPullRequests returns pull requests for a repository, optionally filtered by state.
// State filter: "OPEN", "CLOSED" (includes MERGED), "MERGED", "" or "all" returns all.
func (st *Store) ListPullRequests(repoID int, state string) []*PullRequest {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var prs []*PullRequest
	for _, pr := range st.PullsByRepo[repoID] {
		if state != "" && state != "all" {
			if state == "CLOSED" {
				// GitHub: "closed" includes merged
				if pr.State != "CLOSED" && pr.State != "MERGED" {
					continue
				}
			} else if pr.State != state {
				continue
			}
		}
		prs = append(prs, pr)
	}
	return snapshotPullRequests(prs)
}

// UpdatePullRequest applies a mutation function to a pull request.
func (st *Store) UpdatePullRequest(id int, fn func(*PullRequest)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr, ok := st.PullRequests[id]
	if !ok {
		return false
	}
	fn(pr)
	// Every state transition funnels through here, so retiring an armed
	// auto-merge request when the PR leaves OPEN needs no per-call-site bookkeeping.
	if pr.State != "OPEN" {
		pr.AutoMerge = nil
	}
	pr.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("pull_requests", strconv.Itoa(pr.ID), pr)
	}
	return true
}

// recordPullRequestLabelEventBatchLocked stages a labeled/unlabeled PR-timeline
// event into batch so it commits with the pull-request row in one transaction
// (STORE-001/002). Callers hold st.Mu.
func (st *Store) recordPullRequestLabelEventBatchLocked(batch *PersistBatch, repoID, prID, actorID, labelID int, event string) {
	e := st.buildIssueEventLocked(repoID, prID, actorID, event, "pull_request")
	e.LabelID = labelID
	batch.Put("issue_events", strconv.Itoa(e.ID), e)
}

// AddPullRequestLabels adds labels to a pull request, recording a labeled event
// per new attachment. Returns true when the PR exists; duplicate IDs are ignored.
func (st *Store) AddPullRequestLabels(repoID, prNumber int, labelIDs []int, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullsByRepo[repoID][prNumber]
	if pr == nil {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	added := false
	for _, lid := range labelIDs {
		found := false
		for _, existing := range pr.LabelIDs {
			if existing == lid {
				found = true
				break
			}
		}
		if !found {
			pr.LabelIDs = append(pr.LabelIDs, lid)
			st.recordPullRequestLabelEventBatchLocked(batch, repoID, pr.ID, actorID, lid, "labeled")
			added = true
		}
	}
	if added {
		// One transaction so a crash cannot split the events from the label set
		// (STORE-001/002).
		pr.UpdatedAt = st.CurrentTime()
		batch.Put("pull_requests", strconv.Itoa(pr.ID), pr)
		if err := batch.Commit(); err != nil {
			panic(&PersistenceFailure{Op: "batch", Bucket: "pull_requests", Err: err})
		}
	}
	return true
}

// SetPullRequestPotentialMergeSHA records a pull request's test-merge commit
// (ACT-027). A no-op when unchanged, to avoid churning persistence.
func (st *Store) SetPullRequestPotentialMergeSHA(prID int, sha string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullRequests[prID]
	if pr == nil || pr.PotentialMergeCommitSHA == sha {
		return
	}
	pr.PotentialMergeCommitSHA = sha
	if st.Persist != nil {
		st.Persist.MustPut("pull_requests", strconv.Itoa(pr.ID), pr)
	}
}

// SetPullRequestDiffStats records a pull request's merge-base diff totals. Being
// derived state, it does not bump UpdatedAt and is a no-op when unchanged.
func (st *Store) SetPullRequestDiffStats(prID, changedFiles, additions, deletions int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullRequests[prID]
	if pr == nil || (pr.ChangedFiles == changedFiles && pr.Additions == additions && pr.Deletions == deletions) {
		return
	}
	pr.ChangedFiles = changedFiles
	pr.Additions = additions
	pr.Deletions = deletions
	if st.Persist != nil {
		st.Persist.MustPut("pull_requests", strconv.Itoa(pr.ID), pr)
	}
}

func (st *Store) SetPullRequestLabels(repoID, prNumber int, labelIDs []int, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullsByRepo[repoID][prNumber]
	if pr == nil {
		return false
	}
	old := make(map[int]bool, len(pr.LabelIDs))
	for _, lid := range pr.LabelIDs {
		old[lid] = true
	}
	newSet := make(map[int]bool, len(labelIDs))
	for _, lid := range labelIDs {
		newSet[lid] = true
	}
	// One transaction so a crash cannot split the event history from the PR's
	// label set (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	for _, lid := range pr.LabelIDs {
		if !newSet[lid] {
			st.recordPullRequestLabelEventBatchLocked(batch, repoID, pr.ID, actorID, lid, "unlabeled")
		}
	}
	for _, lid := range labelIDs {
		if !old[lid] {
			st.recordPullRequestLabelEventBatchLocked(batch, repoID, pr.ID, actorID, lid, "labeled")
		}
	}
	// Clone rather than adopt the caller's slice.
	pr.LabelIDs = append([]int(nil), labelIDs...)
	pr.UpdatedAt = st.CurrentTime()
	batch.Put("pull_requests", strconv.Itoa(pr.ID), pr)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "pull_requests", Err: err})
	}
	return true
}

// ClearPullRequestLabels removes every label from a pull request, recording an
// unlabeled event for each previously-attached label. Returns true when the PR
// exists.
func (st *Store) ClearPullRequestLabels(repoID, prNumber, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullsByRepo[repoID][prNumber]
	if pr == nil {
		return false
	}
	if len(pr.LabelIDs) == 0 {
		return true
	}
	// One transaction (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	for _, lid := range pr.LabelIDs {
		st.recordPullRequestLabelEventBatchLocked(batch, repoID, pr.ID, actorID, lid, "unlabeled")
	}
	pr.LabelIDs = nil
	pr.UpdatedAt = st.CurrentTime()
	batch.Put("pull_requests", strconv.Itoa(pr.ID), pr)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "pull_requests", Err: err})
	}
	return true
}

// RemovePullRequestLabel removes a single label from a pull request by id,
// recording an unlabeled event. Returns true when the PR exists (whether or not
// the label was attached), false when it does not.
func (st *Store) RemovePullRequestLabel(repoID, prNumber, labelID, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.PullsByRepo[repoID][prNumber]
	if pr == nil {
		return false
	}
	for idx, lid := range pr.LabelIDs {
		if lid == labelID {
			pr.LabelIDs = append(pr.LabelIDs[:idx], pr.LabelIDs[idx+1:]...)
			pr.UpdatedAt = st.CurrentTime()
			// One transaction so the label drop and its event cannot be split
			// (STORE-001/002).
			batch := NewPersistBatch(st.Persist)
			batch.Put("pull_requests", strconv.Itoa(pr.ID), pr)
			st.recordPullRequestLabelEventBatchLocked(batch, repoID, pr.ID, actorID, labelID, "unlabeled")
			if err := batch.Commit(); err != nil {
				panic(&PersistenceFailure{Op: "batch", Bucket: "pull_requests", Err: err})
			}
			break
		}
	}
	return true
}

// CreatePRReview creates a new review on a pull request (legacy prID-based API).
func (st *Store) CreatePRReview(prID, authorID int, state, body string) *PullRequestReview {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.createPRReviewLocked(prID, authorID, state, body)
}

// createPRReviewLocked creates a review while holding st.Mu.
func (st *Store) createPRReviewLocked(prID, authorID int, state, body string) *PullRequestReview {
	if _, ok := st.PullRequests[prID]; !ok {
		return nil
	}

	now := st.CurrentTime()
	var submittedAt *time.Time
	if state != "PENDING" {
		submittedAt = &now
	}
	review := &PullRequestReview{
		ID:          st.NextPRReview,
		NodeID:      fmt.Sprintf("PRR_kgDO%08d", st.NextPRReview),
		PRID:        prID,
		AuthorID:    authorID,
		State:       state,
		Body:        body,
		SubmittedAt: submittedAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	st.NextPRReview++
	st.PRReviews[review.ID] = review
	st.PRReviewsByPR[review.PRID] = append(st.PRReviewsByPR[review.PRID], review)
	if st.Persist != nil {
		st.Persist.MustPut("pr_reviews", strconv.Itoa(review.ID), review)
	}
	return review
}

// CreatePullRequestReview creates a review addressed by repo key and PR number.
func (st *Store) CreatePullRequestReview(repoKey string, pullNumber int, userID int, body string, state string) *PullRequestReview {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repo := st.ReposByName[repoKey]
	if repo == nil {
		return nil
	}
	pr := st.PullsByRepo[repo.ID][pullNumber]
	if pr == nil {
		return nil
	}
	return st.createPRReviewLocked(pr.ID, userID, state, body)
}

// cloneReview returns a detached snapshot safe to hand outside the store lock
// (STORE-021). Review writes go through the keyed Update/Submit/Dismiss methods.
func cloneReview(r *PullRequestReview) *PullRequestReview {
	if r == nil {
		return nil
	}
	clone := *r
	if r.SubmittedAt != nil {
		submitted := *r.SubmittedAt
		clone.SubmittedAt = &submitted
	}
	if r.DismissedAt != nil {
		dismissed := *r.DismissedAt
		clone.DismissedAt = &dismissed
	}
	return &clone
}

// GetPullRequestReview returns a review by global ID.
func (st *Store) GetPullRequestReview(id int) *PullRequestReview {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneReview(st.PRReviews[id])
}

// ListPullRequestReviews returns all reviews for a repo/PR number.
func (st *Store) ListPullRequestReviews(repoKey string, pullNumber int) []*PullRequestReview {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repo := st.ReposByName[repoKey]
	if repo == nil {
		return nil
	}
	pr := st.PullsByRepo[repo.ID][pullNumber]
	if pr == nil {
		return nil
	}
	reviews := make([]*PullRequestReview, len(st.PRReviewsByPR[pr.ID]))
	copy(reviews, st.PRReviewsByPR[pr.ID])
	return snapshotReviews(reviews)
}

// UpdatePullRequestReview updates a review's body.
func (st *Store) UpdatePullRequestReview(id int, body string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	r, ok := st.PRReviews[id]
	if !ok {
		return false
	}
	r.Body = body
	r.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("pr_reviews", strconv.Itoa(r.ID), r)
	}
	return true
}

// DeletePullRequestReview deletes a pending review.
func (st *Store) DeletePullRequestReview(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	r, ok := st.PRReviews[id]
	if !ok {
		return false
	}
	if r.State != "PENDING" {
		return false
	}
	delete(st.PRReviews, id)
	st.unindexPRReviewLocked(r)
	if st.Persist != nil {
		st.Persist.MustDelete("pr_reviews", strconv.Itoa(id))
	}
	return true
}

// unindexPRReviewLocked removes a review from the per-PR review index. Caller
// holds st.Mu.
func (st *Store) unindexPRReviewLocked(r *PullRequestReview) {
	list := st.PRReviewsByPR[r.PRID]
	for i, x := range list {
		if x.ID == r.ID {
			st.PRReviewsByPR[r.PRID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(st.PRReviewsByPR[r.PRID]) == 0 {
		delete(st.PRReviewsByPR, r.PRID)
	}
}

// SubmitPullRequestReview transitions a pending review to an event state.
func (st *Store) SubmitPullRequestReview(id int, event string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	r, ok := st.PRReviews[id]
	if !ok {
		return false
	}
	if r.State != "PENDING" {
		return false
	}
	now := st.CurrentTime()
	switch strings.ToUpper(event) {
	case "APPROVE":
		r.State = "APPROVED"
	case "REQUEST_CHANGES":
		r.State = "CHANGES_REQUESTED"
	case "COMMENT":
		r.State = "COMMENTED"
	default:
		return false
	}
	r.SubmittedAt = &now
	r.UpdatedAt = now
	if st.Persist != nil {
		st.Persist.MustPut("pr_reviews", strconv.Itoa(r.ID), r)
	}
	return true
}

// PendingReviewForAuthor returns the author's still-pending review on a PR (at
// most one), or nil.
func (st *Store) PendingReviewForAuthor(prID, authorID int) *PullRequestReview {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, r := range st.PRReviewsByPR[prID] {
		if r.AuthorID == authorID && r.State == "PENDING" {
			return cloneReview(r)
		}
	}
	return nil
}

// DismissPullRequestReview marks a review as dismissed.
func (st *Store) DismissPullRequestReview(id int, message string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	r, ok := st.PRReviews[id]
	if !ok {
		return false
	}
	now := st.CurrentTime()
	if r.State != "DISMISSED" {
		r.PreviousState = r.State
	}
	r.State = "DISMISSED"
	r.DismissalMessage = message
	r.DismissedAt = &now
	r.UpdatedAt = now
	if st.Persist != nil {
		st.Persist.MustPut("pr_reviews", strconv.Itoa(r.ID), r)
	}
	return true
}

func (st *Store) FindPRByRepoNumberLocked(repoKey string, pullNumber int) *PullRequest {
	repo := st.ReposByName[repoKey]
	if repo == nil {
		return nil
	}
	// Resolve through the same PullsByRepo index every read and merge path uses,
	// not a scan of st.PullRequests: one source of truth, and O(1).
	return st.PullsByRepo[repo.ID][pullNumber]
}

// RequestReviewers adds reviewer IDs to a PR and records a review_requested
// event per newly added reviewer, attributed to actorID.
func (st *Store) RequestReviewers(repoKey string, pullNumber int, reviewerIDs []int, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.FindPRByRepoNumberLocked(repoKey, pullNumber)
	if pr == nil {
		return false
	}
	// One transaction so a crash cannot split the request from the reviewer add
	// (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	existing := map[int]struct{}{}
	for _, id := range pr.RequestedReviewerIDs {
		existing[id] = struct{}{}
	}
	for _, id := range reviewerIDs {
		if _, ok := existing[id]; !ok {
			pr.RequestedReviewerIDs = append(pr.RequestedReviewerIDs, id)
			existing[id] = struct{}{}
			st.recordPullRequestEventBatchLocked(batch, pr.RepoID, pr.ID, actorID, "review_requested", "", id)
		}
	}
	pr.UpdatedAt = st.CurrentTime()
	batch.Put("pull_requests", strconv.Itoa(pr.ID), pr)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "pull_requests", Err: err})
	}
	return true
}

// RemoveRequestedReviewers removes reviewer IDs from a PR and records a
// review_request_removed event per reviewer removed, attributed to actorID.
func (st *Store) RemoveRequestedReviewers(repoKey string, pullNumber int, reviewerIDs []int, actorID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.FindPRByRepoNumberLocked(repoKey, pullNumber)
	if pr == nil {
		return false
	}
	// One transaction (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	remove := map[int]struct{}{}
	for _, id := range reviewerIDs {
		remove[id] = struct{}{}
	}
	var kept []int
	for _, id := range pr.RequestedReviewerIDs {
		if _, ok := remove[id]; !ok {
			kept = append(kept, id)
			continue
		}
		st.recordPullRequestEventBatchLocked(batch, pr.RepoID, pr.ID, actorID, "review_request_removed", "", id)
	}
	pr.RequestedReviewerIDs = kept
	pr.UpdatedAt = st.CurrentTime()
	batch.Put("pull_requests", strconv.Itoa(pr.ID), pr)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "pull_requests", Err: err})
	}
	return true
}

// RequestTeamReviewers adds team review requests to a pull request. Team IDs,
// not slugs, so a later team rename preserves the request.
func (st *Store) RequestTeamReviewers(repoKey string, pullNumber int, teamIDs []int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.FindPRByRepoNumberLocked(repoKey, pullNumber)
	if pr == nil {
		return false
	}
	existing := make(map[int]struct{}, len(pr.RequestedTeamIDs))
	for _, id := range pr.RequestedTeamIDs {
		existing[id] = struct{}{}
	}
	for _, id := range teamIDs {
		if _, ok := existing[id]; ok {
			continue
		}
		pr.RequestedTeamIDs = append(pr.RequestedTeamIDs, id)
		existing[id] = struct{}{}
	}
	pr.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("pull_requests", strconv.Itoa(pr.ID), pr)
	}
	return true
}

func (st *Store) RemoveRequestedTeamReviewers(repoKey string, pullNumber int, teamIDs []int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pr := st.FindPRByRepoNumberLocked(repoKey, pullNumber)
	if pr == nil {
		return false
	}
	remove := make(map[int]struct{}, len(teamIDs))
	for _, id := range teamIDs {
		remove[id] = struct{}{}
	}
	kept := pr.RequestedTeamIDs[:0]
	for _, id := range pr.RequestedTeamIDs {
		if _, ok := remove[id]; !ok {
			kept = append(kept, id)
		}
	}
	pr.RequestedTeamIDs = kept
	pr.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("pull_requests", strconv.Itoa(pr.ID), pr)
	}
	return true
}

// ListPRReviews returns all reviews for a pull request.
func (st *Store) ListPRReviews(prID int) []*PullRequestReview {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	reviews := make([]*PullRequestReview, len(st.PRReviewsByPR[prID]))
	copy(reviews, st.PRReviewsByPR[prID])
	return snapshotReviews(reviews)
}
