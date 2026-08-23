package bleephub

import (
	"context"
	"sync"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Compaction scheduling.
//
// Compaction turns a repository's loose objects into a packfile, which is what
// makes a clone a copy of stored bytes rather than a re-encoding of decoded
// ones. When it runs is therefore a scheduling decision this server owns: a
// push is the only event that creates loose objects, so a push is when there is
// something to pack, and running it from the write path of the storage layer
// instead makes the timing depend on how many objects happened to be written
// since a process started.
//
// Three properties are what make scheduling it from the push safe.
//
// The push does not wait for it. The client's report is written by the caller
// as soon as applyGitReceivePack returns, and compaction runs on a goroutine the
// server owns.
//
// A burst of pushes does not start overlapping compactions. One compaction of a
// repository runs at a time, and a push that arrives while one is running does
// not queue behind it — it sets a single follow-up, so any number of pushes
// during a run cost exactly one more run. That follow-up is needed rather than
// merely tidy: a compaction packs the loose keys it listed when it started, so
// objects written after that listing are not in the pack it publishes and want
// a run of their own.
//
// It cannot outlive the server. The goroutine is started through goBackground,
// so shutdown waits for it, and it runs under a context the server cancels when
// it stops serving — so the wait is for a compaction that has been told to
// stop, not for one running out its own timeout.

// gitCompactionTimeout bounds one compaction. A large repository's compaction
// reads every loose object it packs, and on object storage each of those is a
// request, so the bound is generous; it is here to stop a wedged run holding a
// repository's slot forever, not to pace ordinary work.
const gitCompactionTimeout = 30 * time.Minute

// gitCompactionScheduler tracks which repositories are being compacted and
// which have been pushed to since their compaction started.
type gitCompactionScheduler struct {
	mu      sync.Mutex
	running map[string]bool
	pending map[string]bool
}

func newGitCompactionScheduler() *gitCompactionScheduler {
	return &gitCompactionScheduler{running: map[string]bool{}, pending: map[string]bool{}}
}

// claim reports whether the caller becomes the compactor for a repository. When
// one is already running the request is recorded as the single follow-up that
// compactor will perform, and the caller starts nothing.
func (c *gitCompactionScheduler) claim(repo string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[repo] {
		c.pending[repo] = true
		return false
	}
	c.running[repo] = true
	return true
}

// release ends one compaction and reports whether a push arrived during it, in
// which case the caller keeps the claim and runs again.
func (c *gitCompactionScheduler) release(repo string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending[repo] {
		delete(c.pending, repo)
		return true
	}
	delete(c.running, repo)
	return false
}

// scheduleGitCompaction packs the objects a push just wrote, on a goroutine the
// server owns.
//
// A repository whose storage cannot pack itself — memory and local-directory
// storage, where git's own maintenance owns the layout — is not scheduled at
// all, so the common test and single-machine configurations pay nothing.
func (s *Server) scheduleGitCompaction(repo string, stor storer.Storer) {
	full, ok := stor.(gitStorage.Storer)
	if !ok {
		return
	}
	if _, packs := stor.(gitstore.Compactor); !packs {
		return
	}
	if !s.gitCompaction.claim(repo) {
		return
	}
	s.goBackground(func() {
		for {
			s.compactGitRepository(repo, full)
			if !s.gitCompaction.release(repo) {
				return
			}
		}
	})
}

// compactGitRepository runs one compaction and reports what it did.
//
// A failure is logged and dropped: the push it followed has already been
// answered and is not in doubt — the objects it wrote are readable whether or
// not they are packed, because a loose object and a packed one are the same
// object at different levels of the store.
func (s *Server) compactGitRepository(repo string, stor gitStorage.Storer) {
	ctx, cancel := context.WithTimeout(s.lifetime, gitCompactionTimeout)
	defer cancel()
	result, err := gitstore.CompactRepository(ctx, stor)
	if err != nil {
		s.logger.Error().Err(err).Str("repo", repo).Msg("git storage compaction failed")
		return
	}
	if result.PackName == "" {
		return
	}
	s.logger.Info().
		Str("repo", repo).
		Str("pack", result.PackName).
		Int("packed", result.Packed).
		Int("merged", result.Merged).
		Int64("bytes", result.PackBytes).
		Msg("git storage compaction published a pack")
}
