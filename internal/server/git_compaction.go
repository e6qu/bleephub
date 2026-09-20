package bleephub

import (
	"context"
	"sync"
	"time"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Compaction packs a repository's loose objects into a packfile and folds the
// small packs pushes leave into larger ones. The storage layer asks for it — it
// knows, as a write lands, how many packs and loose objects the repository has
// gathered — and this scheduler runs it: the write does not wait (background
// goroutine); a burst does not overlap runs (one at a time, with a single
// follow-up for objects written after a run's listing); and a run cannot
// outlive the server (started via goBackground, cancelled on shutdown).

// gitCompactionTimeout bounds a wedged run from holding a repository's slot
// forever; it is generous, not a pace for ordinary work.
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

// claim reports whether the caller becomes the compactor for a repository.
// When one is already running, it records the single follow-up run instead.
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

// scheduleGitCompaction compacts a repository on a server-owned goroutine. Storage that cannot pack itself (memory,
// local-directory) is skipped.
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

// adoptGitCompactionRequests makes this server the one that runs the
// compactions the storage layer asks for, and returns the function that hands
// the role back. The handler is process-global and a Server is not, so only a
// serving server takes it.
func (s *Server) adoptGitCompactionRequests() (release func()) {
	gitstore.SetCompactionRequestHandler(s.onGitCompactionRequested)
	return func() { gitstore.SetCompactionRequestHandler(nil) }
}

// onGitCompactionRequested is the storage layer saying a write has left a
// repository due a compaction.
func (s *Server) onGitCompactionRequested(repo string, stor gitStorage.Storer) {
	s.scheduleGitCompaction(repo, stor)
}

// compactGitRepository runs one compaction. A failure is logged and dropped:
// the objects the push wrote are readable whether or not they are packed.
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
