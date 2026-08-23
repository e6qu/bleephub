package gitstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5/helper/polyfill"
	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
	gitFilesystem "github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/uuid"
)

// S3FSCacheState memoizes the process-wide S3 filesystem built from the
// environment. Its fields are exported state, not functions, so tests in
// dependent packages can reset the memo between environment changes without a
// test-only exported function tripping the deadcode gate.
type S3FSCacheState struct {
	Mu     sync.Mutex
	FS     *S3FS
	Err    error
	Inited bool
}

// S3FSCache is the process-wide memo consulted by GetS3FS.
var S3FSCache S3FSCacheState

var (
	ErrReferenceAlreadyExists = errors.New("reference already exists")
	ErrUnsafeReferenceName    = errors.New("unsafe reference name")
	refMutationLocks          = newS3KeyLocks()
)

// checkSafeRefName rejects a reference name that cannot be safely turned into a
// storage path. Every backend composes the ref file path from the name, and
// the S3 backend joins it with `path.Join`, which cleans `..` — so a crafted
// name like `refs/heads/../../other-repo/refs/heads/main` would escape the
// per-repository chroot. go-git's IsSafe applies git's own check-ref-format
// rules (no empty/`.`/`..` segments, no backslash), which accepts every
// legitimate ref bleephub stores (`refs/heads/*`, `refs/pull/N/merge`,
// `refs/tags/v1.0`, `HEAD`).
func checkSafeRefName(name plumbing.ReferenceName) error {
	if !name.IsSafe() {
		return fmt.Errorf("%w: %q", ErrUnsafeReferenceName, name)
	}
	return nil
}

// atomicRefStorer is the single git-storage handle bleephub keeps per
// repository (the value in Store.GitStorages), so every goroutine that touches
// one repository's git data goes through one instance of this type. Two of
// those goroutines routinely run at once: a `git clone`/`git fetch` reads refs
// and objects on its net/http connection goroutine while a `git push`, a REST
// blob/tree/commit/tag write, or repository creation writes them on another
// request goroutine. Neither side holds Store.Mu — GetGitStorage takes and
// releases it just to look the handle up — so without a lock here the backing
// go-git maps are read and written concurrently.
//
// mu is therefore the repository's storage lock and every method below takes
// it: RLock for reads so the common case (many concurrent clones) still runs
// in parallel, Lock for anything that mutates. It is a field rather than a
// process-wide map because the racing accesses are accesses to *this*
// instance's Go memory; a second handle opened over the same on-disk or S3
// repository has its own maps and caches, and cross-handle (and cross-replica)
// exclusion is what refMutationLocks and the durable GitObjectLocker provide.
//
// The delegate is a named field, not an embedded interface: promotion would
// silently leave any method this type forgets to override unguarded, whereas a
// named field makes the interface assertion below fail to compile instead.
type atomicRefStorer struct {
	storer  gitStorage.Storer
	repo    string
	mu      sync.RWMutex
	modules map[string]gitStorage.Storer
	// fs is the object store this repository's git data lives in, or nil when
	// the repository is backed by a local directory or by memory. It is the
	// handle the pack tier needs: compaction publishes through it, and the
	// membership index that answers "no" without a round trip is keyed by its
	// prefix. Storage that is not object-backed has neither.
	fs *S3FS
	// looseWrites counts objects written into the loose tier since the last
	// compaction was requested. Admitting one compaction at a time is the
	// scheduler's job, not this counter's: the handler that owns the goroutine
	// is the only place that knows whether one is already running.
	looseWrites atomic.Int64
	triggerOnce sync.Once
	trigger     int64
}

var _ gitStorage.Storer = (*atomicRefStorer)(nil)
var _ Compactor = (*atomicRefStorer)(nil)

func WrapAtomicRefStorage(repo string, stor gitStorage.Storer) gitStorage.Storer {
	return &atomicRefStorer{storer: stor, repo: repo}
}

// wrapObjectStoreStorage is the object-store form of WrapAtomicRefStorage. It
// carries the filesystem alongside the storer so the pack tier can reach it.
func wrapObjectStoreStorage(repo string, stor gitStorage.Storer, fs *S3FS) *atomicRefStorer {
	return &atomicRefStorer{storer: stor, repo: repo, fs: fs}
}

func (s *atomicRefStorer) lockName(ref plumbing.ReferenceName) string {
	digest := sha256.Sum256([]byte(s.repo + "\x00" + ref.String()))
	return "git-ref:" + hex.EncodeToString(digest[:])
}

func (s *atomicRefStorer) withRefLock(ref plumbing.ReferenceName, mutate func() error) error {
	return s.withLockName(s.lockName(ref), mutate)
}

func (s *atomicRefStorer) withLockName(name string, mutate func() error) error {
	if err := refMutationLocks.acquire(name, gitObjectLockWait); err != nil {
		return err
	}
	defer refMutationLocks.release(name)
	locker := currentGitObjectLocker()
	if locker == nil {
		return mutate()
	}
	owner := uuid.New().String()
	deadline := time.Now().Add(gitObjectLockWait)
	for {
		acquired, err := locker.AcquireLock(name, owner, gitObjectLockTTL)
		if err != nil {
			return err
		}
		if acquired {
			mutationErr := mutate()
			releaseErr := locker.ReleaseLock(name, owner)
			if mutationErr != nil {
				return mutationErr
			}
			if releaseErr != nil {
				return fmt.Errorf("release lock %s: %w", name, releaseErr)
			}
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("lock %s: another replica still holds it after %s", name, gitObjectLockWait)
		}
		time.Sleep(gitObjectLockPoll)
	}
}

func (s *atomicRefStorer) InitializeRepositoryReferences(branch *plumbing.Reference, requireEmpty bool) error {
	digest := sha256.Sum256([]byte(s.repo + "\x00repository-initialization"))
	name := "git-ref:" + hex.EncodeToString(digest[:])
	return s.withLockName(name, func() error {
		// The read-then-write sequence runs under one exclusive hold so no
		// reader observes the branch without HEAD, and so the rollback below
		// cannot race a concurrent clone.
		s.mu.Lock()
		defer s.mu.Unlock()
		refs, err := s.storer.IterReferences()
		if err != nil {
			return err
		}
		defer refs.Close()
		alreadyInitialized := false
		branchExists := false
		if err := refs.ForEach(func(ref *plumbing.Reference) error {
			if ref.Name().IsBranch() {
				alreadyInitialized = true
				if ref.Name() == branch.Name() {
					branchExists = true
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if branchExists || (requireEmpty && alreadyInitialized) {
			return ErrReferenceAlreadyExists
		}
		if err := s.storer.SetReference(branch); err != nil {
			return err
		}
		if !alreadyInitialized {
			head := plumbing.NewSymbolicReference(plumbing.HEAD, branch.Name())
			if err := s.storer.SetReference(head); err != nil {
				rollbackErr := s.storer.RemoveReference(branch.Name())
				return errors.Join(err, rollbackErr)
			}
		}
		return nil
	})
}

func (s *atomicRefStorer) SetReference(ref *plumbing.Reference) error {
	if err := checkSafeRefName(ref.Name()); err != nil {
		return err
	}
	return s.withRefLock(ref.Name(), func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.storer.SetReference(ref)
	})
}

func (s *atomicRefStorer) CheckAndSetReference(next, old *plumbing.Reference) error {
	if err := checkSafeRefName(next.Name()); err != nil {
		return err
	}
	return s.withRefLock(next.Name(), func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.storer.CheckAndSetReference(next, old)
	})
}

func (s *atomicRefStorer) CreateReference(ref *plumbing.Reference) error {
	if err := checkSafeRefName(ref.Name()); err != nil {
		return err
	}
	return s.withRefLock(ref.Name(), func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.storer.Reference(ref.Name()); err == nil {
			return ErrReferenceAlreadyExists
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		return s.storer.SetReference(ref)
	})
}

func (s *atomicRefStorer) RemoveReference(ref plumbing.ReferenceName) error {
	if err := checkSafeRefName(ref); err != nil {
		return err
	}
	return s.withRefLock(ref, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.storer.RemoveReference(ref)
	})
}

func (s *atomicRefStorer) RemoveReferenceCAS(old *plumbing.Reference) error {
	if err := checkSafeRefName(old.Name()); err != nil {
		return err
	}
	return s.withRefLock(old.Name(), func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		current, err := s.storer.Reference(old.Name())
		if err != nil {
			return err
		}
		if current.Type() != old.Type() || current.String() != old.String() {
			return gitStorage.ErrReferenceHasChanged
		}
		return s.storer.RemoveReference(old.Name())
	})
}

func (s *atomicRefStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.Reference(name)
}

func (s *atomicRefStorer) CountLooseRefs() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.CountLooseRefs()
}

// PackRefs rewrites the loose refs into packed-refs, so it is a mutation even
// though callers reach it from a maintenance path rather than a write path.
func (s *atomicRefStorer) PackRefs() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storer.PackRefs()
}

func (s *atomicRefStorer) IterReferences() (storer.ReferenceIter, error) { //nolint:ireturn
	s.mu.RLock()
	iter, err := s.storer.IterReferences()
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return &lockedReferenceIter{mu: &s.mu, iter: iter}, nil
}

// lockedReferenceIter holds the repository's read lock across each call into
// the underlying iterator but never across the caller's callback. Holding it
// for the whole walk would deadlock two ways: callers legitimately write to the
// same storer from inside ForEach (copyGitStorage's dst==src degenerate case,
// ref-pruning loops), and callers legitimately read from it (gh_import's
// object.DecodeCommit(stor, obj)) — and a recursive RLock deadlocks as soon as
// a writer is queued between the two acquisitions, which is exactly the
// clone-during-push situation this lock exists for. Per-call locking still
// removes the race, because both backends materialize the reference set inside
// IterReferences itself, so the guarded construction above already yields a
// snapshot; the per-call holds cover the lazy backends' remaining state and
// establish the happens-before edge with the writer.
type lockedReferenceIter struct {
	mu   *sync.RWMutex
	iter storer.ReferenceIter
}

func (i *lockedReferenceIter) Next() (*plumbing.Reference, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.iter.Next()
}

func (i *lockedReferenceIter) Close() {
	i.mu.RLock()
	defer i.mu.RUnlock()
	i.iter.Close()
}

// ForEach mirrors go-git's forEachReferenceIter contract: io.EOF and
// storer.ErrStop both end the walk without an error, and the iterator is closed
// on the way out.
func (i *lockedReferenceIter) ForEach(cb func(*plumbing.Reference) error) error {
	defer i.Close()
	for {
		ref, err := i.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := cb(ref); err != nil {
			if errors.Is(err, storer.ErrStop) {
				return nil
			}
			return err
		}
	}
}

func (s *atomicRefStorer) NewEncodedObject() plumbing.EncodedObject { //nolint:ireturn
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.NewEncodedObject()
}

func (s *atomicRefStorer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	s.mu.Lock()
	hash, err := s.storer.SetEncodedObject(obj)
	s.mu.Unlock()
	if err != nil {
		return hash, err
	}
	s.noteObjectWritten()
	return hash, nil
}

func (s *atomicRefStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.EncodedObject(t, h)
}

// HasEncodedObject answers the question a fetch negotiation asks once per
// object it is considering. On object-backed storage that answer used to cost
// an S3 GET that returned a 404, because go-git looks for a loose object first
// and every object of a packed repository is absent from the loose tier.
//
// The membership index answers it without a round trip when it can prove the
// object is absent from every pack and from the loose directory that could hold
// it. That is a negative-only use: a filter that says "maybe" delegates to the
// real lookup below, which is exactly what would have happened anyway. See the
// filter invariant in filter.go.
func (s *atomicRefStorer) HasEncodedObject(h plumbing.Hash) error {
	if s.fs != nil && !s.fs.repoIndexFor().maybePresent(s.fs, oidKeyFrom(h[:])) {
		return plumbing.ErrObjectNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.HasEncodedObject(h)
}

func (s *atomicRefStorer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.EncodedObjectSize(h)
}

func (s *atomicRefStorer) AddAlternate(remote string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storer.AddAlternate(remote)
}

func (s *atomicRefStorer) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) { //nolint:ireturn
	s.mu.RLock()
	iter, err := s.storer.IterEncodedObjects(t)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return &lockedObjectIter{mu: &s.mu, iter: iter}, nil
}

// lockedObjectIter guards the object walk on the same terms as
// lockedReferenceIter, and for the same reason: the object iterator is where
// the packfile encoder meets a concurrent writer. Snapshotting every object
// under one hold is not an option here — the filesystem backend's iterator is
// lazy precisely so a clone of a large repository does not materialize the
// whole object database in memory.
type lockedObjectIter struct {
	mu   *sync.RWMutex
	iter storer.EncodedObjectIter
}

func (i *lockedObjectIter) Next() (plumbing.EncodedObject, error) { //nolint:ireturn
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.iter.Next()
}

func (i *lockedObjectIter) Close() {
	i.mu.RLock()
	defer i.mu.RUnlock()
	i.iter.Close()
}

// ForEach mirrors go-git's storer.ForEachIterator contract.
func (i *lockedObjectIter) ForEach(cb func(plumbing.EncodedObject) error) error {
	defer i.Close()
	for {
		obj, err := i.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := cb(obj); err != nil {
			if errors.Is(err, storer.ErrStop) {
				return nil
			}
			return err
		}
	}
}

func (s *atomicRefStorer) SetShallow(commits []plumbing.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storer.SetShallow(commits)
}

func (s *atomicRefStorer) Shallow() ([]plumbing.Hash, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.Shallow()
}

func (s *atomicRefStorer) SetIndex(idx *index.Index) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storer.SetIndex(idx)
}

func (s *atomicRefStorer) Index() (*index.Index, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.Index()
}

func (s *atomicRefStorer) SetConfig(cfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storer.SetConfig(cfg)
}

func (s *atomicRefStorer) Config() (*config.Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.Config()
}

// Module returns the submodule's storage wrapped in its own guard, memoized so
// that repeated lookups of one submodule name share a single lock. The
// underlying backends cache the submodule storage too, so handing out a fresh
// unguarded wrapper each call would leave two goroutines racing on the same
// maps one level down.
func (s *atomicRefStorer) Module(name string) (gitStorage.Storer, error) { //nolint:ireturn
	s.mu.Lock()
	defer s.mu.Unlock()
	if wrapped, ok := s.modules[name]; ok {
		return wrapped, nil
	}
	sub, err := s.storer.Module(name)
	if err != nil {
		return nil, err
	}
	if s.modules == nil {
		s.modules = map[string]gitStorage.Storer{}
	}
	wrapped := WrapAtomicRefStorage(s.repo+"/"+name, sub)
	s.modules[name] = wrapped
	return wrapped, nil
}

func CreateReferenceIfAbsent(stor gitStorage.Storer, ref *plumbing.Reference) error {
	if atomic, ok := stor.(interface {
		CreateReference(*plumbing.Reference) error
	}); ok {
		return atomic.CreateReference(ref)
	}
	if _, err := stor.Reference(ref.Name()); err == nil {
		return ErrReferenceAlreadyExists
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return err
	}
	return stor.SetReference(ref)
}

func RemoveReferenceCAS(stor gitStorage.Storer, old *plumbing.Reference) error {
	if atomic, ok := stor.(interface {
		RemoveReferenceCAS(*plumbing.Reference) error
	}); ok {
		return atomic.RemoveReferenceCAS(old)
	}
	current, err := stor.Reference(old.Name())
	if err != nil {
		return err
	}
	if current.Type() != old.Type() || current.String() != old.String() {
		return gitStorage.ErrReferenceHasChanged
	}
	return stor.RemoveReference(old.Name())
}

func InitializeRepositoryReferences(stor gitStorage.Storer, branch *plumbing.Reference, requireEmpty bool) error {
	if atomic, ok := stor.(interface {
		InitializeRepositoryReferences(*plumbing.Reference, bool) error
	}); ok {
		return atomic.InitializeRepositoryReferences(branch, requireEmpty)
	}
	refs, err := stor.IterReferences()
	if err != nil {
		return err
	}
	defer refs.Close()
	alreadyInitialized := false
	branchExists := false
	if err := refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() {
			alreadyInitialized = true
			if ref.Name() == branch.Name() {
				branchExists = true
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if branchExists || (requireEmpty && alreadyInitialized) {
		return ErrReferenceAlreadyExists
	}
	if err := stor.SetReference(branch); err != nil {
		return err
	}
	if !alreadyInitialized {
		head := plumbing.NewSymbolicReference(plumbing.HEAD, branch.Name())
		if err := stor.SetReference(head); err != nil {
			rollbackErr := stor.RemoveReference(branch.Name())
			return errors.Join(err, rollbackErr)
		}
	}
	return nil
}

func GetS3FS(ctx context.Context) (*S3FS, error) {
	S3FSCache.Mu.Lock()
	defer S3FSCache.Mu.Unlock()
	if S3FSCache.Inited {
		return S3FSCache.FS, S3FSCache.Err
	}

	endpoint := os.Getenv("BLEEPHUB_S3_ENDPOINT")
	bucket := os.Getenv("BLEEPHUB_S3_BUCKET")
	if bucket == "" {
		S3FSCache.Inited = true
		return nil, nil
	}

	prefix := os.Getenv("BLEEPHUB_S3_PREFIX")
	fs, err := NewS3FS(ctx, endpoint, bucket, prefix)
	if err != nil {
		// Configuration discovery can fail transiently (for example while an
		// ECS task waits for credentials). Do not poison Git storage for the
		// lifetime of the process; the next repository operation retries.
		return nil, err
	}
	S3FSCache.FS = fs
	S3FSCache.Err = nil
	S3FSCache.Inited = true
	return fs, nil
}

func GitDataDir() string {
	return os.Getenv("BLEEPHUB_GIT_DIR")
}

func IsS3GitStorage() bool {
	return os.Getenv("BLEEPHUB_S3_BUCKET") != ""
}

// ValidateRepoStorageFullName keeps every filesystem and object-store backend
// at the same trust boundary. Repository keys are always exactly owner/name;
// accepting an absolute path, a dot component, or a platform separator here
// would let a valid API request escape the configured repository namespace.
func ValidateRepoStorageFullName(fullName string) error {
	if fullName == "" ||
		strings.Contains(fullName, `\`) ||
		strings.Count(fullName, "/") != 1 ||
		path.Clean(fullName) != fullName ||
		!filepath.IsLocal(filepath.FromSlash(fullName)) {
		return fmt.Errorf("invalid repository storage name %q", fullName)
	}
	parts := strings.Split(fullName, "/")
	if parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return fmt.Errorf("invalid repository storage name %q", fullName)
	}
	return nil
}

func RepoGitDirPath(gitDir, fullName string) (string, error) {
	if err := ValidateRepoStorageFullName(fullName); err != nil {
		return "", err
	}
	root, err := filepath.Abs(gitDir)
	if err != nil {
		return "", fmt.Errorf("resolve git directory %q: %w", gitDir, err)
	}
	// The containment guard sits directly on the value that feeds
	// filepath.Join — not on a derived variable after the join — so the
	// barrier is visible to dataflow analysis (CodeQL models a
	// filepath.IsLocal guard as a tainted-path sanitizer). The rejection set
	// is identical to the previous post-join Rel+IsLocal check: a local
	// relative path joined onto an absolute root cannot escape it.
	relative := filepath.FromSlash(fullName)
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("repository storage name %q escapes git directory", fullName)
	}
	return filepath.Join(root, relative), nil
}

func newGitStorage(ctx context.Context, fullName string) (gitStorage.Storer, error) {
	if err := ValidateRepoStorageFullName(fullName); err != nil {
		return nil, err
	}
	s3fs, err := GetS3FS(ctx)
	if err != nil {
		return nil, err
	}
	if s3fs != nil {
		chrooted, err := s3fs.Chroot(fullName)
		if err != nil {
			return nil, fmt.Errorf("s3 chroot %s: %w", fullName, err)
		}
		s3Chroot, ok := chrooted.(*S3FS)
		if !ok {
			return nil, fmt.Errorf("s3 chroot %s: unexpected filesystem type %T", fullName, chrooted)
		}
		storage := gitFilesystem.NewStorage(polyfill.New(chrooted), cache.NewObjectLRUDefault())
		return wrapObjectStoreStorage(fullName, storage, s3Chroot), nil
	}

	gitDir := GitDataDir()
	if gitDir == "" {
		return WrapAtomicRefStorage(fullName, memory.NewStorage()), nil
	}
	repoDir, err := RepoGitDirPath(gitDir, fullName)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(repoDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", repoDir, err)
	}
	fs := osfs.New(repoDir)
	return WrapAtomicRefStorage(fullName, gitFilesystem.NewStorage(fs, cache.NewObjectLRUDefault())), nil
}

func OpenOrInitGitStorage(ctx context.Context, fullName string) (gitStorage.Storer, error) {
	stor, err := newGitStorage(ctx, fullName)
	if err != nil {
		return nil, err
	}
	_, err = git.Init(stor, nil)
	if err != nil && !errors.Is(err, git.ErrRepositoryAlreadyExists) {
		return nil, fmt.Errorf("git init %s: %w", fullName, err)
	}
	return stor, nil
}
