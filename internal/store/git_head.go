package store

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// SetGitHeadBranch points a repository's git HEAD at refs/heads/<branch>.
//
// This is the one place the HEAD symref is written. HEAD is how a clone learns
// which branch to check out — the ref advertisement carries it as the
// symref=HEAD:refs/heads/<branch> capability — so a HEAD that disagrees with
// the repository's recorded default branch lands every clone on the wrong
// branch. Every site that creates a repository or moves its default branch
// calls this rather than writing the reference itself.
//
// The branch need not exist yet: `git init` writes HEAD before the first
// commit too, and a repository created empty must already name the branch its
// first push will land on.
//
// The parameter is the narrow reference-storer interface both git transports
// and the repository store hand around, so no caller has to widen its handle.
func SetGitHeadBranch(stor storer.ReferenceStorer, branch string) error {
	if stor == nil || branch == "" {
		return nil
	}
	target := plumbing.NewBranchReferenceName(branch)
	// Skip the write when HEAD already names the branch, so the common case
	// costs a read rather than a ref-lock round trip.
	if current, err := stor.Reference(plumbing.HEAD); err == nil &&
		current.Type() == plumbing.SymbolicReference && current.Target() == target {
		return nil
	}
	return stor.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, target))
}
