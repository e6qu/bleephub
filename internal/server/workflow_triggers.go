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
	}
}
