package bleephub

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// initRepoWithFiles is the test/import helper for creating an independent
// root branch in an existing repository. Production first-commit paths use
// initEmptyRepoWithFiles, whose require-empty precondition closes the
// concurrent repository-initialization race.
func initRepoWithFiles(stor gitStorage.Storer, branch, message string, files map[string]string, sig *object.Signature) (plumbing.Hash, error) {
	return commitRootBranchWithFiles(stor, branch, message, files, sig, false)
}
