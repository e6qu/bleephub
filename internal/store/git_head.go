package store

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// SetGitHeadBranch points a repository's git HEAD at refs/heads/<branch>. The
// sole writer of the HEAD symref: a clone reads it (symref=HEAD capability) to
// pick its checkout branch, so it must agree with the recorded default branch.
// The branch need not exist yet, matching `git init` writing HEAD before the
// first commit.
func SetGitHeadBranch(stor storer.ReferenceStorer, branch string) error {
	if stor == nil || branch == "" {
		return nil
	}
	target := plumbing.NewBranchReferenceName(branch)
	// Skip the write when HEAD already names the branch, avoiding a ref-lock round trip.
	if current, err := stor.Reference(plumbing.HEAD); err == nil &&
		current.Type() == plumbing.SymbolicReference && current.Target() == target {
		return nil
	}
	return stor.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, target))
}
