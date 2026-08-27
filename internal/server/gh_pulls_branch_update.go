package bleephub

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/e6qu/bleephub/internal/store"
)

// Updating a pull request's head branch with its base.
//
// PUT /repos/{o}/{r}/pulls/{n}/update-branch and the updatePullRequestBranch
// GraphQL mutation are the same git write. The resolver layer may not reach git
// storage (ARCH-003), so it routes through this Pulls seam and both surfaces move
// the same ref the same way.

// branchUpdateExpectationError reports an expectedHeadOid that no longer names the
// head tip; both surfaces answer it as a validation failure on that field.
type branchUpdateExpectationError struct {
	expected string
	actual   string
}

func (e *branchUpdateExpectationError) Error() string {
	return fmt.Sprintf("expected head oid %s but the branch is at %s", e.expected, e.actual)
}

// updatePullRequestBranch brings pr's head branch up to date with its base.
// bleephub's git layer has one integration primitive, so REBASE is performed as a
// merge whose commit message names the rebase; the head still contains the base's
// commits.
func (s *Server) updatePullRequestBranch(repo *store.Repo, pr *store.PullRequest, user *store.User, expectedHeadOid, method, baseURL string) error {
	headRepo := store.PullRequestHeadRepo(s.store, pr)
	if headRepo == nil {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return fmt.Errorf("Pull request head repository is unavailable")
	}
	headOwner, headName, _ := store.SplitRepoFullName(headRepo.FullName)
	headStor := s.store.GetGitStorage(headOwner, headName)
	baseOwner, baseName, _ := store.SplitRepoFullName(repo.FullName)
	baseStor := s.store.GetGitStorage(baseOwner, baseName)
	if headStor == nil || baseStor == nil {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return fmt.Errorf("Pull request branch cannot be updated")
	}
	headRef := plumbing.NewBranchReferenceName(pr.HeadRefName)
	headReference, err := headStor.Reference(headRef)
	if err != nil {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return fmt.Errorf("Pull request head branch does not exist")
	}
	before := headReference.Hash()
	if expectedHeadOid != "" && expectedHeadOid != before.String() {
		return &branchUpdateExpectationError{expected: expectedHeadOid, actual: before.String()}
	}
	baseHash, err := store.ResolveGitRef(baseStor, pr.BaseRefName)
	if err != nil {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return fmt.Errorf("Pull request base branch does not exist")
	}
	if headRepo.FullName != repo.FullName {
		if err := store.CopyGitObjects(baseStor, headStor); err != nil {
			//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
			return fmt.Errorf("Pull request branch cannot be updated")
		}
	}
	email := user.Email
	if email == "" {
		email = user.Login + "@users.noreply.bleephub.local"
	}
	message := fmt.Sprintf("Merge branch '%s' into %s", pr.BaseRefName, pr.HeadRefName)
	if method == "REBASE" {
		message = fmt.Sprintf("Rebase %s onto '%s'", pr.HeadRefName, pr.BaseRefName)
	}
	after, _, err := performMerge(headStor, headRef, baseHash, pr.BaseRefName, message,
		repoSignature(user.Login, email))
	if err != nil {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return fmt.Errorf("Pull request branch cannot be updated due to conflicts")
	}
	s.store.UpdatePullRequest(pr.ID, func(current *store.PullRequest) {
		current.BaseSHA = baseHash.String()
		current.Mergeable = "UNKNOWN"
	})
	if after != before {
		s.afterCommittedRefUpdate(headRepo, user, headRef.String(), before.String(), after.String(), baseURL)
	}
	return nil
}
