package store

import (
	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"

	"github.com/e6qu/bleephub/gitstore"
)

// copyGitStorage copies all objects and references from src to dst, yielding a
// fork independent of the original. The references are set together: in an
// object store that is one write however many there are.
func copyGitStorage(src, dst gitStorage.Storer) error {
	if err := CopyGitObjects(src, dst); err != nil {
		return err
	}
	refIter, err := src.IterReferences()
	if err != nil {
		return err
	}
	defer refIter.Close()
	var refs []*plumbing.Reference
	if err := refIter.ForEach(func(ref *plumbing.Reference) error {
		refs = append(refs, ref)
		return nil
	}); err != nil {
		return err
	}
	return gitstore.SetReferences(dst, refs)
}
