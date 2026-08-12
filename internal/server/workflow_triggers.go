package bleephub

import "github.com/e6qu/bleephub/internal/store"

// Trigger parsing/matching moved to internal/actions (ARCH-002); what stays
// is the PR/webhook-domain fan-out below.

// firePullRequestSynchronize emits pull_request "synchronize" (webhook +
// workflow triggers) for every open PR whose head branch just received a
// push — real GitHub's behavior for pushes to a PR's source branch.
func (s *Server) firePullRequestSynchronize(repo *store.Repo, repoKey, branch string) {
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
		// The head branch moved, so recompute the test-merge before the payload
		// is built (ACT-027).
		s.refreshPullRequestPotentialMerge(baseRepo, pr)
		payload := buildPullRequestPayload(s.store, baseRepo, pr, nil, "synchronize")
		s.emitWebhookEvent(baseRepo.FullName, "pull_request", "synchronize", payload)
	}
}
