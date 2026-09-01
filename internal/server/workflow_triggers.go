package bleephub

import "github.com/e6qu/bleephub/internal/store"

// firePullRequestSynchronize emits pull_request "synchronize" for every open PR
// whose head branch just received a push, matching GitHub's behavior for pushes
// to a PR's source branch. sender is the pusher (the event's sender on GitHub),
// and before/after are the head branch's old/new tip SHAs, which GitHub includes
// as top-level members of a synchronize payload.
func (s *Server) firePullRequestSynchronize(repo *store.Repo, repoKey, branch string, sender *store.User, before, after string) {
	s.store.Mu.RLock()
	var prs []*store.PullRequest
	for _, pr := range s.store.PullRequests {
		if store.PullRequestHeadRepoID(pr) == repo.ID && pr.State == "OPEN" && pr.HeadRefName == branch {
			prs = append(prs, pr)
		}
	}
	s.store.Mu.RUnlock()

	for _, pr := range prs {
		baseRepo := s.store.GetRepoByID(pr.RepoID)
		if baseRepo == nil {
			continue
		}
		// The head moved: recompute the test-merge and refresh diff totals before
		// building the payload.
		s.refreshPullRequestPotentialMerge(baseRepo, pr)
		s.refreshPullRequestDiffStats(baseRepo, pr)
		payload := buildPullRequestPayload(s.store, baseRepo, pr, sender, "synchronize", s.publicOrigin())
		payload["before"] = before
		payload["after"] = after
		s.emitWebhookEvent(baseRepo.FullName, "pull_request", "synchronize", payload)
		// The push may add files whose code owners GitHub requests as on open.
		s.autoRequestCodeOwners(baseRepo, pr, nil)
		// A new commit makes prior approvals stale when the base branch enables
		// dismiss_stale_reviews.
		s.dismissStaleReviewsOnPush(baseRepo, pr, sender)
	}
}

// dismissStaleReviewsOnPush dismisses a PR's current APPROVED / CHANGES_REQUESTED
// reviews when a new commit is pushed to its head and the base branch enables
// dismiss_stale_reviews. Without it a stale approval of an earlier commit lets
// never-reviewed code merge onto a protected branch — the exact bypass the
// setting exists to prevent. The dismissal drops the review from
// countApprovingReviews so the merge gate re-blocks until re-approved.
func (s *Server) dismissStaleReviewsOnPush(repo *store.Repo, pr *store.PullRequest, sender *store.User) {
	bp := s.effectiveBranchProtectionFor(repo.ID, pr.BaseRefName)
	if bp == nil || bp.RequiredPullRequestReviews == nil || !bp.RequiredPullRequestReviews.DismissStaleReviews {
		return
	}
	actorID := 0
	if sender != nil {
		actorID = sender.ID
	}
	for _, review := range s.store.ListPullRequestReviews(repo.FullName, pr.Number) {
		if review.State != "APPROVED" && review.State != "CHANGES_REQUESTED" {
			continue
		}
		if !s.store.DismissPullRequestReview(review.ID, "Stale review dismissed because new commits were pushed.") {
			continue
		}
		s.store.RecordPullRequestEvent(repo.ID, pr.ID, actorID, "review_dismissed", "", 0)
		dismissed := s.store.GetPullRequestReview(review.ID)
		s.emitWebhookEvent(repo.FullName, "pull_request_review", "dismissed",
			buildPullRequestReviewPayload(s.store, repo, pr, dismissed, sender, "dismissed", s.publicOrigin()))
	}
}
