package bleephub

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

var (
	s3FSMu     sync.Mutex
	s3FSCache  *s3FS
	s3FSErr    error
	s3FSInited bool

	errReferenceAlreadyExists = errors.New("reference already exists")
	refMutationLocks          = newS3KeyLocks()
)

type atomicRefStorer struct {
	gitStorage.Storer
	repo string
}

func wrapAtomicRefStorage(repo string, stor gitStorage.Storer) gitStorage.Storer {
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
			return errReferenceAlreadyExists
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
	return s.withRefLock(ref.Name(), func() error {
		return s.Storer.SetReference(ref)
	})
}

func (s *atomicRefStorer) CheckAndSetReference(next, old *plumbing.Reference) error {
	return s.withRefLock(next.Name(), func() error {
		return s.Storer.CheckAndSetReference(next, old)
	})
}

func (s *atomicRefStorer) CreateReference(ref *plumbing.Reference) error {
	return s.withRefLock(ref.Name(), func() error {
		if _, err := s.Storer.Reference(ref.Name()); err == nil {
			return errReferenceAlreadyExists
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		return s.Storer.SetReference(ref)
	})
}

func (s *atomicRefStorer) RemoveReference(ref plumbing.ReferenceName) error {
	return s.withRefLock(ref, func() error {
		return s.Storer.RemoveReference(ref)
	})
}

func (s *atomicRefStorer) RemoveReferenceCAS(old *plumbing.Reference) error {
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

func createReferenceIfAbsent(stor gitStorage.Storer, ref *plumbing.Reference) error {
	if atomic, ok := stor.(interface {
		CreateReference(*plumbing.Reference) error
	}); ok {
		return atomic.CreateReference(ref)
	}
	if _, err := stor.Reference(ref.Name()); err == nil {
		return errReferenceAlreadyExists
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return err
	}
	return stor.SetReference(ref)
}

func removeReferenceCAS(stor gitStorage.Storer, old *plumbing.Reference) error {
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

func initializeRepositoryReferences(stor gitStorage.Storer, branch *plumbing.Reference, requireEmpty bool) error {
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
		return errReferenceAlreadyExists
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

func getS3FS(ctx context.Context) (*s3FS, error) {
	s3FSMu.Lock()
	defer s3FSMu.Unlock()
	if s3FSInited {
		return s3FSCache, s3FSErr
	}

	endpoint := os.Getenv("BLEEPHUB_S3_ENDPOINT")
	bucket := os.Getenv("BLEEPHUB_S3_BUCKET")
	if bucket == "" {
		s3FSInited = true
		return nil, nil
	}

	prefix := os.Getenv("BLEEPHUB_S3_PREFIX")
	fs, err := newS3FS(ctx, endpoint, bucket, prefix)
	if err != nil {
		// Configuration discovery can fail transiently (for example while an
		// ECS task waits for credentials). Do not poison Git storage for the
		// lifetime of the process; the next repository operation retries.
		return nil, err
	}
	s3FSCache = fs
	s3FSErr = nil
	s3FSInited = true
	return fs, nil
}

func GitDataDir() string {
	return os.Getenv("BLEEPHUB_GIT_DIR")
}

func IsS3GitStorage() bool {
	return os.Getenv("BLEEPHUB_S3_BUCKET") != ""
}

// validateRepoStorageFullName keeps every filesystem and object-store backend
// at the same trust boundary. Repository keys are always exactly owner/name;
// accepting an absolute path, a dot component, or a platform separator here
// would let a valid API request escape the configured repository namespace.
func validateRepoStorageFullName(fullName string) error {
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

func repoGitDirPath(gitDir, fullName string) (string, error) {
	if err := validateRepoStorageFullName(fullName); err != nil {
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
	if err := validateRepoStorageFullName(fullName); err != nil {
		return nil, err
	}
	s3fs, err := getS3FS(ctx)
	if err != nil {
		return nil, err
	}
	if s3fs != nil {
		chrooted, err := s3fs.Chroot(fullName)
		if err != nil {
			return nil, fmt.Errorf("s3 chroot %s: %w", fullName, err)
		}
		return wrapAtomicRefStorage(fullName, gitFilesystem.NewStorage(polyfill.New(chrooted), cache.NewObjectLRUDefault())), nil
	}

	gitDir := GitDataDir()
	if gitDir == "" {
		return wrapAtomicRefStorage(fullName, memory.NewStorage()), nil
	}
	repoDir, err := repoGitDirPath(gitDir, fullName)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(repoDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", repoDir, err)
	}
	fs := osfs.New(repoDir)
	return wrapAtomicRefStorage(fullName, gitFilesystem.NewStorage(fs, cache.NewObjectLRUDefault())), nil
}

func openOrInitGitStorage(ctx context.Context, fullName string) (gitStorage.Storer, error) {
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
