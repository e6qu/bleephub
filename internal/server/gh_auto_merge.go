package bleephub

import (
	"context"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// Pull request auto-merge (GraphQL enablePullRequestAutoMerge /
// disablePullRequestAutoMerge; REST pull-request.auto_merge).
//
// An armed request merges through the exact internal path the REST merge
// handler uses — completePullRequestMerge behind canMergePullRequest — so
// auto-merge can never become a way around branch protection. The attempt
// runs wherever a blocking condition can clear:
//
//   - a check run for the head SHA completes (create/update check-run),
//   - a commit status for the head SHA succeeds,
//   - a review lands or a blocking review is dismissed (REST + GraphQL),
//   - branch protection state changes (PUT/DELETE /protection, every
//     protection sub-resource via setBranchProtection, and the /ui-data
//     pattern rules).
//
// Enabling auto-merge on a PR that is already mergeable is refused
// ("Pull request is in clean status", as on GitHub), so there is no
// enabled-after-green race to poll for.

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

// maybeAutoMergeHeadSHA re-evaluates every open PR whose current head is the
// commit a check run or commit status just reported on.
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

// maybeAutoMergeBranch re-evaluates every open PR targeting a base branch
// whose protection state just changed.
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

// maybeAutoMergeRepo re-evaluates every armed open PR in the repository —
// used when pattern-rule protection changes, which can affect any base branch.
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

// attemptAutoMerge tries to merge an armed pull request through the same
// gates the REST merge handler applies, acting as the user who enabled
// auto-merge. On any refusal the request stays armed for the next trigger;
// on success the standard merged state, timeline events, and pull_request
// closed webhook are recorded.
func (s *Server) attemptAutoMerge(repo *store.Repo, pr *store.PullRequest) {
	if pr == nil || pr.State != "OPEN" || pr.AutoMerge == nil {
		return
	}
	enabler := s.store.GetUserByID(pr.AutoMerge.EnabledByID)
	if enabler == nil {
		return
	}
	ctx := contextWithUser(context.Background(), enabler)

	// Merging is a write to the base branch; the enabler must still hold it.
	if !s.viewerCanPushRepo(ctx, repo) {
		return
	}
	headSha := s.prHeadSha(repo, pr)
	if headSha == "" {
		return
	}
	// Required status checks are evaluated unconditionally, exactly as the
	// REST merge handler does, so an admin enabler cannot ride the
	// enforce_admins bypass past a red check.
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
	payload := buildPullRequestPayload(s.store, repo, merged, enabler, "closed")
	s.emitWebhookEvent(repo.FullName, "pull_request", "closed", payload)
}
