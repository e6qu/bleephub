package store

import (
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

func CopyGitObjects(src, dst gitStorage.Storer) error {
	for _, t := range []plumbing.ObjectType{plumbing.CommitObject, plumbing.TreeObject, plumbing.BlobObject, plumbing.TagObject} {
		iter, err := src.IterEncodedObjects(t)
		if err != nil {
			return err
		}
		if err := iter.ForEach(func(obj plumbing.EncodedObject) error {
			newObj := dst.NewEncodedObject()
			newObj.SetType(obj.Type())
			newObj.SetSize(obj.Size())
			w, err := newObj.Writer()
			if err != nil {
				return err
			}
			r, err := obj.Reader()
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, r); err != nil {
				_ = r.Close()
				return err
			}
			_ = r.Close()
			if err := w.Close(); err != nil {
				return err
			}
			_, err = dst.SetEncodedObject(newObj)
			return err
		}); err != nil {
			iter.Close()
			return err
		}
		iter.Close()
	}
	return nil
}
