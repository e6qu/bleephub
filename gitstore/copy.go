package gitstore

import (
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"golang.org/x/sync/errgroup"
)

// Copying every object of one repository into another — a fork, a pull request
// from a fork merged into its parent, a branch updated from its parent — is
// what the store's packs make cheap: they are named by their contents, so a
// repository's objects are copied by copying its packs and sidecars as the
// store holds them, under the other repository's prefix, and naming them in
// one swap of its manifest. Nothing is decoded, re-encoded or downloaded, and
// a pack the destination already holds is not copied again. On the
// local-directory and memory backends, which hold objects rather than packs of
// their own, the objects are copied one by one.

// packSourceCopy is what the manifest records of a pack copied from another
// repository.
const packSourceCopy = "copy"

// copyPackParallelism bounds the server-side copies in flight for one copy of
// a repository.
const copyPackParallelism = 8

// ObjectCopier is git storage that can take in every object of another
// repository of the same kind. Every backend of this package is one.
type ObjectCopier interface {
	CopyObjectsFrom(src gitStorage.Storer) error
}

var (
	_ ObjectCopier = (*repository)(nil)
	_ ObjectCopier = (*atomicRefStorer)(nil)
)

// CopyObjects makes every object of src part of dst, durably, moving no
// reference of either. It refuses storage that is not this package's.
func CopyObjects(src, dst gitStorage.Storer) error {
	copier, ok := dst.(ObjectCopier)
	if !ok {
		return fmt.Errorf("gitstore: a %T cannot take in another repository's objects", dst)
	}
	return copier.CopyObjectsFrom(src)
}

// CopyObjectsFrom copies src's live packs, and what src has written and not yet
// packed, into this repository.
func (r *repository) CopyObjectsFrom(src gitStorage.Storer) error {
	from, ok := src.(*repository)
	if !ok || from.shared != r.shared {
		return fmt.Errorf("gitstore: %s can copy the objects of a repository of its own store, not a %T", r.name, src)
	}
	// What the source holds only in memory goes into a pack of its own first,
	// so that what is copied is every object it has.
	if err := from.FlushObjects(); err != nil {
		return err
	}
	source, err := from.manifests.revalidate()
	if err != nil {
		return err
	}
	held, err := r.manifests.held()
	if err != nil {
		return err
	}
	var copied []manifestPack
	var copies errgroup.Group
	copies.SetLimit(copyPackParallelism)
	ctx := r.shared.baseContext()
	for _, pack := range source.manifest.Packs {
		if held.pack(pack.Name) != nil {
			continue
		}
		for _, suffix := range packKeySuffixes {
			from, to := from.manifests.extents(pack.Name+suffix, 0).key, r.manifests.extents(pack.Name+suffix, 0).key
			copies.Go(func() error { return r.shared.copyObject(ctx, from, to) })
		}
		pack.Source = packSourceCopy
		copied = append(copied, pack)
	}
	if err := copies.Wait(); err != nil {
		return fmt.Errorf("copy the packs of %s into %s: %w", from.name, r.name, err)
	}
	if len(copied) == 0 {
		return nil
	}
	state, err := r.commit(func(d *draft) error {
		for _, pack := range copied {
			if err := d.addPack(pack); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.notePackWritten(len(state.packs))
	return nil
}

// CopyObjectsFrom copies every object of src into this storage, one by one.
func (s *atomicRefStorer) CopyObjectsFrom(src gitStorage.Storer) error {
	for _, kind := range []plumbing.ObjectType{plumbing.CommitObject, plumbing.TreeObject, plumbing.BlobObject, plumbing.TagObject} {
		iter, err := src.IterEncodedObjects(kind)
		if err != nil {
			return err
		}
		err = iter.ForEach(func(object plumbing.EncodedObject) error {
			copied := s.NewEncodedObject()
			copied.SetType(object.Type())
			copied.SetSize(object.Size())
			writer, err := copied.Writer()
			if err != nil {
				return err
			}
			reader, err := object.Reader()
			if err != nil {
				return err
			}
			_, err = io.Copy(writer, reader)
			if closeErr := reader.Close(); err == nil {
				err = closeErr
			}
			if closeErr := writer.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
			_, err = s.SetEncodedObject(copied)
			return err
		})
		iter.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
