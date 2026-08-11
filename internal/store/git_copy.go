package store

import (
	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// copyGitStorage copies all encoded objects and references from src to dst.
// It is used to implement repository forks while keeping the copy independent
// of the original.
func copyGitStorage(src, dst gitStorage.Storer) error {
	if err := CopyGitObjects(src, dst); err != nil {
		return err
	}

	refIter, err := src.IterReferences()
	if err != nil {
		return err
	}
	defer refIter.Close()
	return refIter.ForEach(func(ref *plumbing.Reference) error {
		return dst.SetReference(ref)
	})
}
