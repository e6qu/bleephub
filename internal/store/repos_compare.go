package store

import (
	"github.com/e6qu/bleephub/gitstore"

	gitStorage "github.com/go-git/go-git/v5/storage"
)

// CopyGitObjects copies every object of src into dst, and returns once they are
// durable in dst: a copy is answered on, forked from and merged into by callers
// that may move no reference of dst for a while, if ever. In an object store it
// copies src's packs as they stand (gitstore.CopyObjects).
func CopyGitObjects(src, dst gitStorage.Storer) error {
	return gitstore.CopyObjects(src, dst)
}
