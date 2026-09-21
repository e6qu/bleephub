package gitstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

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
)

var (
	ErrReferenceAlreadyExists = errors.New("reference already exists")
	ErrUnsafeReferenceName    = errors.New("unsafe reference name")
	refMutationLocks          = newS3KeyLocks()
)

// checkSafeRefName rejects a reference name that cannot be safely turned into a
// storage path or key. A reference is stored under its own name, so a crafted
// one like `refs/heads/../../other-repo/refs/heads/main` would reach outside the
// repository on a disk, and one like `config` would overwrite another of the
// repository's files in a bucket. go-git's IsSafe applies git's own
// check-ref-format rules and accepts every legitimate ref bleephub stores.
func checkSafeRefName(name plumbing.ReferenceName) error {
	if !name.IsSafe() {
		return fmt.Errorf("%w: %q", ErrUnsafeReferenceName, name)
	}
	return nil
}

// atomicRefStorer is the single git-storage handle bleephub keeps per repository
// on the local-directory and memory backends, where the storage underneath is
// go-git's own. (The object-store backend is repository, which is safe for
// concurrent use by construction and needs none of this.) Concurrent goroutines routinely touch one
// repository's git data at once (a clone/fetch reading refs and objects while a
// push or REST write mutates them), and neither side holds Store.Mu, so mu
// guards the backing go-git maps: RLock for reads, Lock for mutations. It is a
// field, not a process-wide map, because the races are on this instance's Go
// memory; exclusion between handles comes from refMutationLocks.
//
// The delegate is a named field, not an embedded interface: promotion would
// silently leave any un-overridden method unguarded, whereas the named field
// makes the interface assertion below fail to compile instead.
type atomicRefStorer struct {
	storer  gitStorage.Storer
	repo    string
	mu      sync.RWMutex
	modules map[string]gitStorage.Storer
	// primed records that the delegate's lazily built state exists. See prime.
	primed sync.Once
}

var _ gitStorage.Storer = (*atomicRefStorer)(nil)

// WrapAtomicRefStorage makes a go-git storage safe to share within a process:
// reference updates become a compare-and-swap across goroutines, and reads and
// writes of the storage's own maps are kept apart.
func WrapAtomicRefStorage(repo string, stor gitStorage.Storer) gitStorage.Storer {
	return &atomicRefStorer{storer: stor, repo: repo}
}

// prime builds the delegate's lazy state under the exclusive lock, once, before
// the first shared read. mu lets readers run together on the understanding that
// reading does not write, and go-git's filesystem storage breaks that on first
// use: it publishes its pack-index map and then fills it, and memoizes what it
// finds in the object directory, from inside read calls. A server shares one
// handle per repository between requests, so after a restart the first clones
// arrive together at an unbuilt handle, and one arriving mid-build saw some
// packs and not others — an object the repository holds reported missing, which
// a git client is told as "not our ref". The probe drives that construction
// through go-git's public surface; its answer is irrelevant.
func (s *atomicRefStorer) prime() {
	s.primed.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = s.storer.HasEncodedObject(plumbing.ZeroHash)
	})
}

func (s *atomicRefStorer) withRefLock(ref plumbing.ReferenceName, mutate func() error) error {
	return withLockName(lockNameFor("git-ref", s.repo, ref.String()), mutate)
}

func (s *atomicRefStorer) InitializeRepositoryReferences(branch *plumbing.Reference, requireEmpty bool) error {
	return withLockName(lockNameFor("git-ref", s.repo, "repository-initialization"), func() error {
		// One exclusive hold so no reader observes the branch without HEAD and
		// the rollback below cannot race a concurrent clone.
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
	s.prime()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.Reference(name)
}

func (s *atomicRefStorer) CountLooseRefs() (int, error) {
	s.prime()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.CountLooseRefs()
}

// PackRefs rewrites loose refs into packed-refs, so it takes the write lock
// despite arriving from a maintenance path.
func (s *atomicRefStorer) PackRefs() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storer.PackRefs()
}

func (s *atomicRefStorer) IterReferences() (storer.ReferenceIter, error) { //nolint:ireturn
	s.prime()
	s.mu.RLock()
	iter, err := s.storer.IterReferences()
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return &lockedReferenceIter{mu: &s.mu, iter: iter}, nil
}

// lockedReferenceIter holds the repository's read lock across each call into the
// underlying iterator but never across the caller's callback. Holding it for the
// whole walk would deadlock: callers legitimately read and write the same storer
// from inside ForEach, and a recursive RLock deadlocks once a writer queues
// between the two acquisitions — the clone-during-push case this lock exists for.
// Per-call locking still removes the race: both backends materialize the
// reference set inside IterReferences, so the construction above already yields a
// snapshot, and the per-call holds establish the happens-before edge with the writer.
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
// storer.ErrStop both end the walk without error, and the iterator is closed on
// the way out.
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
	s.prime()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.NewEncodedObject()
}

func (s *atomicRefStorer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storer.SetEncodedObject(obj)
}

func (s *atomicRefStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	s.prime()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.EncodedObject(t, h)
}

func (s *atomicRefStorer) HasEncodedObject(h plumbing.Hash) error {
	s.prime()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.HasEncodedObject(h)
}

func (s *atomicRefStorer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	s.prime()
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
	s.prime()
	s.mu.RLock()
	iter, err := s.storer.IterEncodedObjects(t)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return &lockedObjectIter{mu: &s.mu, iter: iter}, nil
}

// lockedObjectIter guards the object walk on the same terms as
// lockedReferenceIter: the object iterator is where the packfile encoder meets a
// concurrent writer. Snapshotting under one hold is not an option — the
// filesystem backend's iterator is lazy so a clone of a large repository never
// materializes the whole object database in memory.
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
	s.prime()
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
	s.prime()
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
	s.prime()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storer.Config()
}

// Module returns the submodule's storage wrapped in its own guard, memoized so
// repeated lookups of one name share a lock. The backends cache the submodule
// storage too, so a fresh unguarded wrapper each call would leave two goroutines
// racing on the same maps one level down.
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

// ValidateRepoStorageFullName holds every backend to one trust boundary.
// Repository keys are always exactly owner/name; an absolute path, dot
// component, or platform separator would escape the configured namespace.
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
	// Guard the value that feeds filepath.Join directly, not a post-join
	// derivative, so CodeQL sees the filepath.IsLocal tainted-path sanitizer.
	// A local relative path joined onto an absolute root cannot escape it.
	relative := filepath.FromSlash(fullName)
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("repository storage name %q escapes git directory", fullName)
	}
	return filepath.Join(root, relative), nil
}

// OpenDir opens the repository fullName as a bare dotgit directory under gitDir,
// creating the directory if it does not exist. The handle is also a PackSource.
func OpenDir(gitDir, fullName string) (gitStorage.Storer, error) {
	repoDir, err := RepoGitDirPath(gitDir, fullName)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(repoDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", repoDir, err)
	}
	wrapped := &atomicRefStorer{storer: gitFilesystem.NewStorage(osfs.New(repoDir), cache.NewObjectLRUDefault()), repo: fullName}
	return newDirStorer(repoDir, wrapped), nil
}

// OpenMemory returns a repository held in process memory.
func OpenMemory(fullName string) (gitStorage.Storer, error) {
	if err := ValidateRepoStorageFullName(fullName); err != nil {
		return nil, err
	}
	return WrapAtomicRefStorage(fullName, memory.NewStorage()), nil
}

// Init makes stor a git repository if it is not one already. A repository in an
// object store is made one by its first manifest, in one commit; go-git's own
// initialization, which the other backends take, is a reference and then a
// config, each a write of its own.
func Init(stor gitStorage.Storer) error {
	if inStore, ok := stor.(*repository); ok {
		return inStore.create()
	}
	if _, err := git.Init(stor, nil); err != nil && !errors.Is(err, git.ErrRepositoryAlreadyExists) {
		return fmt.Errorf("git init: %w", err)
	}
	return nil
}
