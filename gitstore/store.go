package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	gitStorage "github.com/go-git/go-git/v5/storage"
	"golang.org/x/sync/singleflight"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Store is a bucket prefix holding many repositories, each under
// <prefix>/<owner>/<repo>/: a manifest, and the packs and loose objects it and
// git's own key names say are there. It is the one value an
// application builds at startup; every repository handle, and every sibling
// prefix made with Sub, shares its connection, its tunables, its circuit
// breaker and its pack cache.
type Store struct {
	shared *storeShared
	prefix string
}

// storeShared is what a Store and every Sub of it hold in common.
type storeShared struct {
	bucket  objstore.Bucket
	opts    Options
	breaker *s3Breaker
	// now is the clock every freshness bound is measured on. It is a field so a
	// test can move time rather than wait for it.
	now func() time.Time

	mu sync.Mutex
	// baseCtx is the server-lifetime context every store call derives from, so
	// in-flight I/O is cancelled on shutdown instead of detaching.
	baseCtx context.Context
	// repositories holds the one handle per repository, by key prefix. A handle
	// owns the snapshots of what its repository holds, so two handles on one
	// repository would each pay to learn what the other already knew.
	repositories map[string]*repository

	// chunkFetch coalesces concurrent fetches of one pack extent. A replica that
	// starts cold under load has every clone ask for the same extents at once;
	// without this each of them pays for its own copy of identical bytes.
	chunkFetch singleflight.Group
}

// Open returns the store of repositories kept under prefix in bucket. Of opts,
// the connection fields (Region, Credentials, Transport) and MultipartBytes
// belong to whoever built the bucket, and are not consulted here.
func Open(bucket objstore.Bucket, prefix string, opts Options) *Store {
	opts = opts.resolved()
	return &Store{
		prefix: prefix,
		shared: &storeShared{
			bucket:       bucket,
			opts:         opts,
			breaker:      newS3Breaker(opts.BreakerThreshold, opts.BreakerCooldown),
			now:          time.Now,
			baseCtx:      context.Background(),
			repositories: map[string]*repository{},
		},
	}
}

// OpenS3 opens a store on an S3-compatible bucket. An empty endpoint targets
// AWS S3 in opts.Region; anything else is addressed path-style. The pack size
// above which an upload goes in parts, opts.MultipartBytes, is also the size of
// those parts.
func OpenS3(_ context.Context, endpoint, bucket, prefix string, opts Options) (*Store, error) {
	opts = opts.resolved()
	driver, err := objstore.NewS3(bucket, objstore.S3Options{
		Endpoint:    endpoint,
		Region:      opts.Region,
		Credentials: opts.Credentials,
		Transport:   opts.Transport,
		PartBytes:   opts.UploadPieceBytes(),
	})
	if err != nil {
		return nil, err
	}
	return Open(driver, prefix, opts), nil
}

// Bucket returns the bucket the store keeps its repositories in.
func (s *Store) Bucket() objstore.Bucket { return s.shared.bucket }

// Prefix reports the key prefix all of this store's objects live under.
func (s *Store) Prefix() string { return s.prefix }

// Sub returns the store of a sibling prefix on the same bucket, sharing this
// one's connection, tunables, base context and circuit breaker.
func (s *Store) Sub(prefix string) *Store {
	return &Store{shared: s.shared, prefix: prefix}
}

// SetBaseContext installs the server-lifetime context (cancelled on shutdown) so
// in-flight object-store I/O aborts when the process is draining. It applies to
// every Sub and every repository handle.
func (s *Store) SetBaseContext(ctx context.Context) {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	s.shared.baseCtx = ctx
}

func (s *storeShared) baseContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseCtx
}

// repositoryPrefix is the key prefix, ending in "/", under which a repository's
// objects live.
func (s *Store) repositoryPrefix(fullName string) string {
	return path.Join(s.prefix, fullName) + "/"
}

// Repository returns the handle on the repository fullName. There is one handle
// per repository for the life of the store, safe for concurrent use: it owns the
// snapshots of what the repository holds, and sharing it is what lets one
// request's listing answer the next.
func (s *Store) Repository(fullName string) (gitStorage.Storer, error) { //nolint:ireturn
	if err := ValidateRepoStorageFullName(fullName); err != nil {
		return nil, err
	}
	prefix := s.repositoryPrefix(fullName)
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	handle, ok := s.shared.repositories[prefix]
	if !ok {
		handle = newRepository(s.shared, fullName, prefix)
		s.shared.repositories[prefix] = handle
	}
	return handle, nil
}

// ExistingRepository returns the handle on a repository the caller knows to
// exist, and ErrNoManifest if the store holds no manifest for it. It is how an
// application opens what it created earlier: a repository without a manifest is
// then either lost or — in a deployment that predates the manifest — one that
// has not been through Adopt, and in neither case is it an empty repository to
// be initialized over.
func (s *Store) ExistingRepository(fullName string) (gitStorage.Storer, error) { //nolint:ireturn
	handle, err := s.Repository(fullName)
	if err != nil {
		return nil, err
	}
	state, err := handle.(*repository).manifests.recent()
	if err != nil {
		return nil, err
	}
	if !state.exists() {
		return nil, fmt.Errorf("%s: %w", fullName, ErrNoManifest)
	}
	return handle, nil
}

// forget drops the handles of repositories whose objects have just been moved
// or removed underneath them, so the next Repository call learns the prefix
// afresh rather than serve a snapshot of what is no longer there.
func (s *Store) forget(fullNames ...string) {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	for _, fullName := range fullNames {
		delete(s.shared.repositories, s.repositoryPrefix(fullName))
	}
}

// CopyRepository copies every object of oldFull to newFull, leaving the source
// intact. A rename of a live repository runs it outside the application's store
// lock so both names coexist and readers at the old one keep working; the caller
// deletes the old repository after swapping its metadata.
//
// The manifest is read first and written last. Read first, it names only what
// the listing taken after it will show — packs that were live then, which stay
// in the store a grace period even if a compaction retires them meanwhile — and
// a push that lands during the copy is not half carried over. Written last, the
// copy becomes a repository in one step, when everything it names is in place.
// A snapshot's key is relative to the repository, so the manifest is carried as
// it stands.
func (s *Store) CopyRepository(oldFull, newFull string) error {
	for _, fullName := range []string{oldFull, newFull} {
		if err := ValidateRepoStorageFullName(fullName); err != nil {
			return err
		}
	}
	oldPrefix := s.repositoryPrefix(oldFull)
	newPrefix := s.repositoryPrefix(newFull)
	ctx := s.shared.baseContext()

	held, _, err := s.shared.getAll(ctx, oldPrefix+manifestName)
	if errors.Is(err, objstore.ErrNotFound) {
		return fmt.Errorf("copy %s: %w", oldFull, ErrNoManifest)
	}
	if err != nil {
		return err
	}
	var keys []string
	if err := s.shared.list(ctx, oldPrefix, func(entry objstore.Entry) {
		if relative := strings.TrimPrefix(entry.Key, oldPrefix); relative != manifestName {
			keys = append(keys, entry.Key)
		}
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := s.shared.copyObject(ctx, key, newPrefix+strings.TrimPrefix(key, oldPrefix)); err != nil {
			return err
		}
	}
	if _, err := s.shared.put(ctx, storeWriteTimeout, newPrefix+manifestName, bytes.NewReader(held), int64(len(held)), objstore.Always); err != nil {
		return err
	}
	s.forget(newFull)
	return nil
}

// RenameRepository moves a repository (copy, then delete). It is the single-shot
// form for a move fast enough to make under the application's store lock; a
// live repository is moved with CopyRepository and DeleteRepository around the
// metadata swap.
func (s *Store) RenameRepository(oldFull, newFull string) error {
	if err := s.CopyRepository(oldFull, newFull); err != nil {
		return err
	}
	return s.DeleteRepository(oldFull)
}

// DeleteRepository removes every object of a repository, the manifest first:
// with it gone the repository is gone, in one step, and what is left is keys
// nothing names.
func (s *Store) DeleteRepository(fullName string) error {
	if err := ValidateRepoStorageFullName(fullName); err != nil {
		return err
	}
	prefix := s.repositoryPrefix(fullName)
	ctx := s.shared.baseContext()
	defer s.forget(fullName)
	if err := s.shared.deleteObject(ctx, prefix+manifestName); err != nil {
		return err
	}
	// A writer that had not yet heard of the deletion may add a key while the
	// listing is being emptied, so list again until a listing finds nothing.
	for {
		var keys []string
		if err := s.shared.list(ctx, prefix, func(entry objstore.Entry) {
			keys = append(keys, entry.Key)
		}); err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		if err := s.shared.deleteMany(ctx, keys); err != nil {
			return err
		}
	}
}

// The timeouts of one store call, by what the call moves. They are ceilings on
// a wedged request, not paces: a healthy store answers in milliseconds.
const (
	storeReadTimeout   = 30 * time.Second
	storeHeadTimeout   = 15 * time.Second
	storeWriteTimeout  = 60 * time.Second
	storeListTimeout   = 60 * time.Second
	storeDeleteTimeout = 60 * time.Second
	storeExtentTimeout = 120 * time.Second
)

// call makes one object-store call: refused at once while the circuit breaker
// is open, bounded by timeout, and cancelled with the base context as well as
// with parent. Every request this package makes goes through it, which is what
// makes a dead store fail in microseconds everywhere rather than wherever
// someone remembered to check.
func (s *storeShared) call(parent context.Context, timeout time.Duration, do func(ctx context.Context) error) error {
	if err := s.breaker.check(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	stop := context.AfterFunc(s.baseContext(), cancel)
	defer stop()
	err := do(ctx)
	s.breaker.record(err)
	return err
}

// getAll reads a whole object. It is for the small ones — references, the
// config, a loose object — and reads the body inside the call so that the
// timeout and the breaker cover the transfer and not only the headers.
func (s *storeShared) getAll(parent context.Context, key string) ([]byte, objstore.Info, error) {
	var data []byte
	var info objstore.Info
	err := s.call(parent, storeReadTimeout, func(ctx context.Context) error {
		body, described, err := s.bucket.Get(ctx, key)
		if err != nil {
			return err
		}
		defer func() { _ = body.Close() }()
		info = described
		data, err = io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("read %s: %w", key, err)
		}
		return nil
	})
	return data, info, err
}

// getAllIfChanged is getAll for a reader that holds the object at a version: it
// answers objstore.ErrNotModified, having moved no body, while the object is
// still at it.
func (s *storeShared) getAllIfChanged(parent context.Context, key string, held objstore.Version) ([]byte, objstore.Info, error) {
	var data []byte
	var info objstore.Info
	err := s.call(parent, storeReadTimeout, func(ctx context.Context) error {
		body, described, err := s.bucket.GetIfChanged(ctx, key, held)
		if err != nil {
			return err
		}
		defer func() { _ = body.Close() }()
		info = described
		data, err = io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("read %s: %w", key, err)
		}
		return nil
	})
	return data, info, err
}

// getRange reads one extent of an object.
func (s *storeShared) getRange(parent context.Context, key string, offset, length int64) ([]byte, error) {
	var data []byte
	err := s.call(parent, storeExtentTimeout, func(ctx context.Context) error {
		body, _, err := s.bucket.GetRange(ctx, key, offset, length)
		if err != nil {
			return err
		}
		defer func() { _ = body.Close() }()
		data, err = io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("read %s: %w", key, err)
		}
		return nil
	})
	return data, err
}

func (s *storeShared) head(parent context.Context, key string) (objstore.Info, error) {
	var info objstore.Info
	err := s.call(parent, storeHeadTimeout, func(ctx context.Context) error {
		var err error
		info, err = s.bucket.Head(ctx, key)
		return err
	})
	return info, err
}

// put writes an object. timeout is the caller's because it is the one argument
// that depends on what is being written: a reference, or a pack of gigabytes.
func (s *storeShared) put(parent context.Context, timeout time.Duration, key string, body io.Reader, size int64, condition objstore.Condition) (objstore.Version, error) {
	var version objstore.Version
	err := s.call(parent, timeout, func(ctx context.Context) error {
		var err error
		version, err = s.bucket.Put(ctx, key, body, size, condition, nil)
		return err
	})
	return version, err
}

func (s *storeShared) deleteObject(parent context.Context, key string) error {
	return s.call(parent, storeDeleteTimeout, func(ctx context.Context) error {
		return s.bucket.Delete(ctx, key)
	})
}

func (s *storeShared) deleteMany(parent context.Context, keys []string) error {
	return s.call(parent, storeDeleteTimeout, func(ctx context.Context) error {
		return s.bucket.DeleteMany(ctx, keys)
	})
}

// list visits every object under prefix. visit cannot fail, so an error from
// here is always the store's and the breaker may count it.
func (s *storeShared) list(parent context.Context, prefix string, visit func(objstore.Entry)) error {
	return s.call(parent, storeListTimeout, func(ctx context.Context) error {
		return s.bucket.List(ctx, prefix, func(entry objstore.Entry) error {
			visit(entry)
			return nil
		})
	})
}

func (s *storeShared) copyObject(parent context.Context, sourceKey, destinationKey string) error {
	return s.call(parent, storeWriteTimeout, func(ctx context.Context) error {
		return s.bucket.Copy(ctx, sourceKey, destinationKey)
	})
}
