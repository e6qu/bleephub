package bleephub

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"

	"github.com/e6qu/bleephub/internal/store"
)

// The merge queue. Enqueuing a pull request records its position (merge_queue.go
// in the store); this processor makes the queue advance the way GitHub's does.
// For the front entry it forms a merge group — a temporary commit merging the
// base branch and the pull request head, published on a
// gh-readonly-queue/<base>/pr-<n>-<base_sha> ref — and fires the `merge_group`
// event so `on: merge_group` workflows run against it. When that group's
// required checks are green the pull request merges into the base branch and the
// queue advances to the next entry.

const mergeQueueAdvanceBudget = 64

// mergeQueueGroupRef is the read-only ref a merge group lives on, matching
// GitHub's gh-readonly-queue/<base>/pr-<n>-<base_sha> convention.
func mergeQueueGroupRef(baseBranch string, prNumber int, baseSha string) plumbing.ReferenceName {
	return plumbing.NewBranchReferenceName(fmt.Sprintf("gh-readonly-queue/%s/pr-%d-%s", baseBranch, prNumber, baseSha))
}

// advanceMergeQueue processes one base branch's queue, looping to the next entry
// as each one merges. The budget bounds a pathological run; each iteration
// either merges the front entry (and continues) or stops.
func (s *Server) advanceMergeQueue(repo *store.Repo, baseBranch string) {
	if repo == nil || baseBranch == "" {
		return
	}
	for i := 0; i < mergeQueueAdvanceBudget; i++ {
		entries := s.store.MergeQueuePullRequests(repo.ID, baseBranch)
		if len(entries) == 0 {
			return
		}
		if !s.processMergeQueueFront(repo, baseBranch, entries[0]) {
			return
		}
	}
}

// advanceMergeQueuesForRepo re-processes every base branch that has queued pull
// requests. A check or status landing on a merge-group head does not tell us
// which branch it belongs to, so this fans out; advanceMergeQueue is idempotent.
func (s *Server) advanceMergeQueuesForRepo(repo *store.Repo) {
	if repo == nil {
		return
	}
	seen := map[string]bool{}
	for _, pr := range s.store.ListPullRequests(repo.ID, "OPEN") {
		if pr.MergeQueuePosition > 0 && !seen[pr.BaseRefName] {
			seen[pr.BaseRefName] = true
			s.advanceMergeQueue(repo, pr.BaseRefName)
		}
	}
}

// processMergeQueueFront handles the front entry: it forms the merge group if
// absent (and fires merge_group), evaluates the group's required checks, and
// merges when they pass. It returns true when the entry was merged or dropped
// (so the caller advances to the next entry), false when the queue must wait.
func (s *Server) processMergeQueueFront(repo *store.Repo, baseBranch string, pr *store.PullRequest) bool {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return false
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return false
	}
	baseRefName := plumbing.NewBranchReferenceName(baseBranch)
	baseSha := store.ResolveBranchSha(stor, baseBranch)
	if baseSha == "" {
		return false
	}
	headSha := s.prHeadSha(repo, pr)
	if headSha == "" {
		return false
	}

	groupRefName := mergeQueueGroupRef(baseBranch, pr.Number, baseSha)
	groupHead := ""
	if ref, err := stor.Reference(groupRefName); err == nil {
		groupHead = ref.Hash().String()
	}

	if groupHead == "" {
		sig := &object.Signature{Name: "bleephub", Email: "bleephub@bleephub.invalid", When: s.currentTime()}
		message := fmt.Sprintf("Merge pull request #%d into %s (merge queue)", pr.Number, baseBranch)
		mergeHash, err := computeMergeCommitHash(stor, baseRefName, plumbing.NewHash(headSha), message, sig)
		if err != nil {
			// A conflict makes the entry unmergeable; GitHub drops it from the queue.
			s.logger.Info().Str("repo", repo.FullName).Int("pr", pr.Number).Err(err).
				Msg("merge queue: dropping unmergeable entry")
			s.dropFromMergeQueue(pr, groupRefName, stor)
			return true
		}
		if err := stor.SetReference(plumbing.NewHashReference(groupRefName, mergeHash)); err != nil {
			s.logger.Warn().Err(err).Msg("merge queue: could not publish group ref")
			return false
		}
		groupHead = mergeHash.String()
		s.emitMergeGroupEvent(repo, baseBranch, baseSha, groupRefName.String(), groupHead)
	}

	state := s.evaluateChecksForMerge(repo, baseBranch, groupHead)
	if state.RequiredFailing {
		// A required check failed on the merge group; GitHub removes the entry.
		s.logger.Info().Str("repo", repo.FullName).Int("pr", pr.Number).
			Msg("merge queue: dropping entry with a failing required check")
		s.dropFromMergeQueue(pr, groupRefName, stor)
		return true
	}
	if len(state.MissingRequired) > 0 {
		return false // required checks still outstanding
	}

	// Checks passed — merge the pull request into the base branch. The queue
	// enforced the checks; the merge itself goes through the shared merge path.
	enabler := s.store.GetUserByID(pr.AuthorID)
	if enabler == nil {
		enabler = s.store.GetUserByID(repo.OwnerID)
	}
	if enabler == nil {
		return false
	}
	if _, errMsg := s.completePullRequestMerge(repo, pr, enabler, "merge", "", "", headSha); errMsg != "" {
		s.logger.Warn().Str("repo", repo.FullName).Int("pr", pr.Number).
			Msg("merge queue: merge failed: " + errMsg)
		return false
	}
	merged := s.store.GetPullRequest(pr.ID)
	payload := buildPullRequestPayload(s.store, repo, merged, enabler, "closed", s.publicOrigin())
	s.emitWebhookEvent(repo.FullName, "pull_request", "closed", payload)
	s.store.DequeuePullRequest(pr.ID)
	_ = stor.RemoveReference(groupRefName)
	return true
}

func (s *Server) dropFromMergeQueue(pr *store.PullRequest, groupRefName plumbing.ReferenceName, stor gitStorage.Storer) {
	s.store.DequeuePullRequest(pr.ID)
	_ = stor.RemoveReference(groupRefName)
}

// emitMergeGroupEvent fires GitHub's `merge_group` event (action
// checks_requested), which the generic dispatch turns into `on: merge_group`
// workflow runs against the group's head commit.
func (s *Server) emitMergeGroupEvent(repo *store.Repo, baseBranch, baseSha, headRef, headSha string) {
	origin := s.publicOrigin()
	payload := map[string]interface{}{
		"action": "checks_requested",
		"merge_group": map[string]interface{}{
			"head_sha": headSha,
			"head_ref": headRef,
			"base_sha": baseSha,
			"base_ref": plumbing.NewBranchReferenceName(baseBranch).String(),
			"head_commit": map[string]interface{}{
				"id":      headSha,
				"tree_id": "",
				"message": fmt.Sprintf("Merge group for %s", baseBranch),
			},
		},
		"repository": store.RepoToJSON(repo, s.store, origin),
	}
	s.emitWebhookEvent(repo.FullName, "merge_group", "checks_requested", payload)
}
