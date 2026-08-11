package gitstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/helper/polyfill"
	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
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

type atomicRefStorer struct {
	gitStorage.Storer
	repo string
}

func WrapAtomicRefStorage(repo string, stor gitStorage.Storer) gitStorage.Storer {
	return &atomicRefStorer{Storer: stor, repo: repo}
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
		refs, err := s.Storer.IterReferences()
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
		if err := s.Storer.SetReference(branch); err != nil {
			return err
		}
		if !alreadyInitialized {
			head := plumbing.NewSymbolicReference(plumbing.HEAD, branch.Name())
			if err := s.Storer.SetReference(head); err != nil {
				rollbackErr := s.Storer.RemoveReference(branch.Name())
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
		return s.Storer.SetReference(ref)
	})
}

func (s *atomicRefStorer) CheckAndSetReference(next, old *plumbing.Reference) error {
	if err := checkSafeRefName(next.Name()); err != nil {
		return err
	}
	return s.withRefLock(next.Name(), func() error {
		return s.Storer.CheckAndSetReference(next, old)
	})
}

func (s *atomicRefStorer) CreateReference(ref *plumbing.Reference) error {
	if err := checkSafeRefName(ref.Name()); err != nil {
		return err
	}
	return s.withRefLock(ref.Name(), func() error {
		if _, err := s.Storer.Reference(ref.Name()); err == nil {
			return ErrReferenceAlreadyExists
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		return s.Storer.SetReference(ref)
	})
}

func (s *atomicRefStorer) RemoveReference(ref plumbing.ReferenceName) error {
	if err := checkSafeRefName(ref); err != nil {
		return err
	}
	return s.withRefLock(ref, func() error {
		return s.Storer.RemoveReference(ref)
	})
}

func (s *atomicRefStorer) RemoveReferenceCAS(old *plumbing.Reference) error {
	if err := checkSafeRefName(old.Name()); err != nil {
		return err
	}
	return s.withRefLock(old.Name(), func() error {
		current, err := s.Storer.Reference(old.Name())
		if err != nil {
			return err
		}
		if current.Type() != old.Type() || current.String() != old.String() {
			return gitStorage.ErrReferenceHasChanged
		}
		return s.Storer.RemoveReference(old.Name())
	})
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
	repoDir := filepath.Join(root, filepath.FromSlash(fullName))
	relative, err := filepath.Rel(root, repoDir)
	if err != nil || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("repository storage name %q escapes git directory", fullName)
	}
	return repoDir, nil
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
		return WrapAtomicRefStorage(fullName, gitFilesystem.NewStorage(polyfill.New(chrooted), cache.NewObjectLRUDefault())), nil
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
