package store

import (
	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// ResolveBranchSha resolves a branch name to its commit sha in git
// storage; empty when unknown.
func ResolveBranchSha(stor gitStorage.Storer, branch string) string {
	if stor == nil || branch == "" {
		return ""
	}
	ref, err := stor.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}
