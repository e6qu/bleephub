package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Addressable is a repository whose bytes have addresses of their own: a stored
// pack, or an auxiliary object kept beside the repository's git data (a bundle,
// say), can be handed to a client as a URL it fetches straight from the object
// store. Only the object-store backend is one.
//
// A URL authorizes exactly one GET of one key for its expiry, without lending
// this process's credentials. Ask for one only on behalf of a reader already
// entitled to the bytes; expiry bounds how long that entitlement lasts.
//
// An auxiliary object is named by its path within the repository, such as
// objects/bundle/bundle-<id>.bundle. The names git's own data lives under are
// refused.
type Addressable interface {
	// PackURL signs a URL for a stored pack, named as PackSource names it.
	PackURL(ctx context.Context, name string, expiry time.Duration) (string, error)
	// StatAux reports an auxiliary object's size, or os.ErrNotExist.
	StatAux(ctx context.Context, name string) (size int64, err error)
	// PutAux writes a stream to an auxiliary object without buffering all of it.
	// The object appears whole or not at all.
	PutAux(ctx context.Context, name string, source io.Reader) error
	// ListAux describes the auxiliary objects directly inside dir.
	ListAux(ctx context.Context, dir string) ([]AuxObject, error)
	// RemoveAux removes an auxiliary object. Removing one that is not there is
	// not an error.
	RemoveAux(ctx context.Context, name string) error
	// AuxURL signs a URL for an auxiliary object.
	AuxURL(ctx context.Context, name string, expiry time.Duration) (string, error)
}

// AuxObject describes one auxiliary object. Name is its name within the
// directory listed; ModTime is when the store last saw it written, which is what
// lets a caller expire what it published.
type AuxObject struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// auxStreamPartSize is how much of a stream PutAux reads before it knows whether
// the stream fits in one request.
const auxStreamPartSize = 16 << 20

// auxKey turns an auxiliary object's name into its key. The name must be a
// clean relative path, and must not reach into git's own data: a caller able to
// choose the name of a bundle must not thereby be able to write a reference.
func (r *repository) auxKey(name string) (string, error) {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("invalid auxiliary object name %q", name)
	}
	top, rest, _ := strings.Cut(name, "/")
	own := false
	switch top {
	case "HEAD", "config", "index", "shallow", packedRefsName, "refs", "modules":
		own = true
	case "objects":
		// Beside the fanout directories and the pack directory, objects/ is
		// where a server keeps what it derives from them, such as bundles.
		directory, _, _ := strings.Cut(rest, "/")
		own = directory == "" || directory == "pack" || directory == "info" || len(directory) == 2
	}
	if own {
		return "", fmt.Errorf("auxiliary object name %q is one of git's own", name)
	}
	return r.prefix + name, nil
}

func (r *repository) presign(ctx context.Context, key string, expiry time.Duration) (string, error) {
	if expiry <= 0 {
		return "", fmt.Errorf("presign %s: expiry %s is not in the future", key, expiry)
	}
	var signed string
	err := r.shared.call(ctx, storeHeadTimeout, func(ctx context.Context) error {
		var err error
		signed, err = r.shared.bucket.PresignGet(ctx, key, expiry)
		return err
	})
	return signed, err
}

// PackURL signs a URL for one of the snapshot's packs.
func (r *repository) PackURL(ctx context.Context, name string, expiry time.Duration) (string, error) {
	pack, err := r.storedPack(name)
	if err != nil {
		return "", err
	}
	return r.presign(ctx, pack.pack.key, expiry)
}

// AuxURL signs a URL for an auxiliary object. It does not ask whether the
// object exists: a URL for one that does not answers 404 to whoever follows it.
func (r *repository) AuxURL(ctx context.Context, name string, expiry time.Duration) (string, error) {
	key, err := r.auxKey(name)
	if err != nil {
		return "", err
	}
	return r.presign(ctx, key, expiry)
}

// StatAux reports an auxiliary object's size: one HEAD.
func (r *repository) StatAux(ctx context.Context, name string) (int64, error) {
	key, err := r.auxKey(name)
	if err != nil {
		return 0, err
	}
	info, err := r.shared.head(ctx, key)
	if errors.Is(err, objstore.ErrNotFound) {
		return 0, fmt.Errorf("auxiliary object %q: %w", name, os.ErrNotExist)
	}
	return info.Size, err
}

// PutAux sends a stream that fits in one part as a single request, which is
// what keeps a small artefact from paying for a three-step upload in parts;
// anything longer is uploaded in parts by the driver, which publishes it on
// completion.
func (r *repository) PutAux(ctx context.Context, name string, source io.Reader) error {
	key, err := r.auxKey(name)
	if err != nil {
		return err
	}
	first := make([]byte, auxStreamPartSize)
	read, err := io.ReadFull(source, first)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if read < len(first) {
		_, err := r.shared.put(ctx, storeWriteTimeout, key, bytes.NewReader(first[:read]), int64(read), objstore.Always)
		return err
	}
	_, err = r.shared.put(ctx, packPublishTimeout, key, io.MultiReader(bytes.NewReader(first), source), -1, objstore.Always)
	return err
}

// ListAux describes the objects directly inside dir, and not those further down.
func (r *repository) ListAux(ctx context.Context, dir string) ([]AuxObject, error) {
	key, err := r.auxKey(dir)
	if err != nil {
		return nil, err
	}
	var objects []AuxObject
	err = r.shared.call(ctx, storeListTimeout, func(ctx context.Context) error {
		objects = objects[:0]
		return r.shared.bucket.ListDirectory(ctx, key+"/", func(entry objstore.Entry) error {
			if !entry.Prefix {
				objects = append(objects, AuxObject{Name: path.Base(entry.Key), Size: entry.Size, ModTime: entry.ModTime})
			}
			return nil
		})
	})
	return objects, err
}

// RemoveAux removes an auxiliary object.
func (r *repository) RemoveAux(ctx context.Context, name string) error {
	key, err := r.auxKey(name)
	if err != nil {
		return err
	}
	return r.shared.deleteObject(ctx, key)
}
