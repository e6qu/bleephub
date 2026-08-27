package bleephub

import (
	"context"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// Pull request auto-merge. An armed request merges through the same internal
// path the REST merge handler uses (completePullRequestMerge behind
// canMergePullRequest), so it can never bypass branch protection. Each attempt
// is re-triggered wherever a blocking condition can clear (check run, commit
// status, review, protection change).

// maybeAutoMergePR re-evaluates one pull request's armed auto-merge request.
func (s *Server) maybeAutoMergePR(prID int) {
	pr := s.store.GetPullRequest(prID)
	if pr == nil {
		return
	}
	repo := s.store.GetRepoByID(pr.RepoID)
	if repo == nil {
		return
	}
	s.attemptAutoMerge(repo, pr)
}

// maybeAutoMergeHeadSHA re-evaluates every open PR whose head is the given commit.
func (s *Server) maybeAutoMergeHeadSHA(repo *store.Repo, sha string) {
	if repo == nil || sha == "" {
		return
	}
	for _, pr := range s.store.ListPullRequests(repo.ID, "OPEN") {
		if pr.AutoMerge == nil {
			continue
		}
		if head := s.prHeadSha(repo, pr); head != "" && strings.EqualFold(head, sha) {
			s.attemptAutoMerge(repo, pr)
		}
	}
}

// maybeAutoMergeBranch re-evaluates every armed open PR targeting baseBranch.
func (s *Server) maybeAutoMergeBranch(repo *store.Repo, baseBranch string) {
	if repo == nil {
		return
	}
	for _, pr := range s.store.ListPullRequests(repo.ID, "OPEN") {
		if pr.AutoMerge != nil && pr.BaseRefName == baseBranch {
			s.attemptAutoMerge(repo, pr)
		}
	}
}

// maybeAutoMergeRepo re-evaluates every armed open PR in the repository, for
// pattern-rule protection changes that can affect any base branch.
func (s *Server) maybeAutoMergeRepo(repo *store.Repo) {
	if repo == nil {
		return
	}
	for _, pr := range s.store.ListPullRequests(repo.ID, "OPEN") {
		if pr.AutoMerge != nil {
			s.attemptAutoMerge(repo, pr)
		}
	}
}

// attemptAutoMerge merges an armed PR through the REST merge gates, acting as
// the enabler. On any refusal the request stays armed for the next trigger.
func (s *Server) attemptAutoMerge(repo *store.Repo, pr *store.PullRequest) {
	if pr == nil || pr.State != "OPEN" || pr.AutoMerge == nil {
		return
	}
	enabler := s.store.GetUserByID(pr.AutoMerge.EnabledByID)
	if enabler == nil {
		return
	}
	ctx := contextWithUser(context.Background(), enabler)

	// The enabler must still hold write to the base branch.
	if !s.viewerCanPushRepo(ctx, repo) {
		return
	}
	headSha := s.prHeadSha(repo, pr)
	if headSha == "" {
		return
	}
	// Evaluate required status checks unconditionally so an admin enabler cannot ride the enforce_admins bypass past a red check.
	if st := s.evaluateChecksForMerge(repo, pr.BaseRefName, headSha); len(st.MissingRequired) > 0 {
		return
	}
	if ok, _ := s.canMergePullRequest(ctx, repo, pr); !ok {
		return
	}

	method := strings.ToLower(pr.AutoMerge.MergeMethod)
	if method == "" {
		method = "merge"
	}
	merger := *enabler
	if pr.AutoMerge.AuthorEmail != "" {
		merger.Email = pr.AutoMerge.AuthorEmail
	}
	if _, errMsg := s.completePullRequestMerge(repo, pr, &merger, method, pr.AutoMerge.CommitHeadline, pr.AutoMerge.CommitBody, headSha); errMsg != "" {
		return
	}

	merged := s.store.GetPullRequest(pr.ID)
	payload := buildPullRequestPayload(s.store, repo, merged, enabler, "closed", s.publicOrigin())
	s.emitWebhookEvent(repo.FullName, "pull_request", "closed", payload)
}
