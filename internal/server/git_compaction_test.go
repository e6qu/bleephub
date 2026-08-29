package bleephub

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/rs/zerolog"
)

// fakeCompactingStorer is a storer that can pack itself, the shape the scheduler
// looks for. It reports when a compaction starts and waits to be let go, so a
// test can hold one open and push again underneath it.
type fakeCompactingStorer struct {
	gitStorage.Storer
	// gitRefLifecycleStorer is the compare-and-set reference lifecycle a push
	// needs, promoted from the wrapped storer so this type can stand in for
	// a repository's real storage and still take a push.
	gitRefLifecycleStorer
	started chan struct{}
	release chan struct{}
	runs    atomic.Int32
	fail    error
	// observed is the context of the most recent compaction, so a test can
	// assert on what cancels it.
	observed chan context.Context
}

func newFakeCompactingStorer(inner gitStorage.Storer) *fakeCompactingStorer {
	fake := &fakeCompactingStorer{
		Storer:   inner,
		started:  make(chan struct{}, 64),
		release:  make(chan struct{}),
		observed: make(chan context.Context, 64),
	}
	if lifecycle, ok := inner.(gitRefLifecycleStorer); ok {
		fake.gitRefLifecycleStorer = lifecycle
	}
	return fake
}

func (f *fakeCompactingStorer) Compact(ctx context.Context) (gitstore.CompactionResult, error) {
	f.runs.Add(1)
	f.observed <- ctx
	f.started <- struct{}{}
	<-f.release
	if f.fail != nil {
		return gitstore.CompactionResult{}, f.fail
	}
	return gitstore.CompactionResult{Packed: 1, PackName: "pack-fake"}, nil
}

// awaitStart blocks until a compaction has begun, failing rather than hanging
// the suite if none does.
func awaitStart(t *testing.T, stor *fakeCompactingStorer) {
	t.Helper()
	select {
	case <-stor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no compaction started")
	}
}

// bytesRecorder collects log output from the goroutine that writes it, so a test
// can read it from its own without racing the writer.
type bytesRecorder struct {
	mu    sync.Mutex
	lines []byte
}

func (r *bytesRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, p...)
	return len(p), nil
}

func (r *bytesRecorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.lines)
}

func (r *bytesRecorder) contains(needle string) bool {
	return strings.Contains(r.text(), needle)
}

// TestABurstOfPushesRunsOneCompactionAndOneFollowUp is the deduplication
// contract: pushes arriving while a repository is being compacted never start a
// second compaction of it, and are not each answered with a run of their own.
func TestABurstOfPushesRunsOneCompactionAndOneFollowUp(t *testing.T) {
	t.Parallel()
	srv := NewServer("127.0.0.1:0", zerolog.New(io.Discard))
	stor := newFakeCompactingStorer(memory.NewStorage())

	srv.scheduleGitCompaction("owner/repo", stor)
	awaitStart(t, stor)
	// Eight more pushes land while the first compaction is still running.
	for i := 0; i < 8; i++ {
		srv.scheduleGitCompaction("owner/repo", stor)
	}
	if got := stor.runs.Load(); got != 1 {
		t.Fatalf("%d compactions were running at once, want 1", got)
	}
	close(stor.release)
	srv.background.Wait()
	if got := stor.runs.Load(); got != 2 {
		t.Fatalf("a burst of nine pushes ran %d compactions, want 2 — one running and one follow-up", got)
	}
}

// TestCompactionCannotOutliveTheServer covers the ownership requirement from
// both sides: shutdown waits for a running compaction, and it cancels it rather
// than waiting out the compaction's own bound.
func TestCompactionCannotOutliveTheServer(t *testing.T) {
	t.Parallel()
	srv := NewServer("127.0.0.1:0", zerolog.New(io.Discard))
	stor := newFakeCompactingStorer(memory.NewStorage())
	// This compaction ends when its context does, the way a real one aborts
	// mid-way through a listing.
	go func() {
		ctx := <-stor.observed
		<-ctx.Done()
		close(stor.release)
	}()

	srv.scheduleGitCompaction("owner/repo", stor)
	awaitStart(t, stor)

	stopped := make(chan struct{})
	go func() {
		srv.background.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("shutdown was not waiting for the compaction it started")
	case <-time.After(50 * time.Millisecond):
	}

	srv.stopServing()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the compaction ignored the server's shutdown and is still running")
	}
}

// TestCompactionFailureIsLoggedAndDropped: a push is answered before its
// compaction runs, so a compaction that fails has nothing left to fail.
func TestCompactionFailureIsLoggedAndDropped(t *testing.T) {
	t.Parallel()
	var logged bytesRecorder
	srv := NewServer("127.0.0.1:0", zerolog.New(&logged))
	stor := newFakeCompactingStorer(memory.NewStorage())
	stor.fail = errors.New("the object store refused a listing")
	close(stor.release)

	srv.scheduleGitCompaction("owner/repo", stor)
	srv.background.Wait()
	if got := stor.runs.Load(); got != 1 {
		t.Fatalf("the failing compaction ran %d times, want 1", got)
	}
	if !logged.contains("the object store refused a listing") {
		t.Fatalf("a failed compaction was not logged:\n%s", logged.text())
	}
}

// TestStorageThatCannotPackItselfIsNeverScheduled: local-directory and memory
// storage leave packing to git's own maintenance, so scheduling a goroutine for
// them would be work that always does nothing.
func TestStorageThatCannotPackItselfIsNeverScheduled(t *testing.T) {
	t.Parallel()
	srv := NewServer("127.0.0.1:0", zerolog.New(io.Discard))
	srv.scheduleGitCompaction("owner/repo", memory.NewStorage())
	srv.background.Wait()
	if srv.gitCompaction.claim("owner/repo") == false {
		t.Fatal("a storer that cannot pack itself was recorded as being compacted")
	}
}

// TestAPushSchedulesCompactionOfTheRepositoryItWroteTo is the deterministic
// half of the scheduling change: the objects a push writes are packed because
// the push happened, not because a counter somewhere reached a threshold.
func TestAPushSchedulesCompactionOfTheRepositoryItWroteTo(t *testing.T) {
	t.Parallel()
	git := requireGitCLI(t)
	srv := newIsolatedServer(t)
	const name = "compact-on-push"
	seedGitShallowRepo(t, srv.Server, name)

	fake := newFakeCompactingStorer(srv.store.GetGitStorage("admin", name))
	close(fake.release)
	srv.store.Mu.Lock()
	srv.store.GitStorages["admin/"+name] = fake
	srv.store.Mu.Unlock()

	cloneURL := strings.Replace(srv.baseURL, "://", "://admin:"+defaultToken+"@", 1) + "/admin/" + name + ".git"
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	git.run(root, "clone", cloneURL, clone)
	if fake.runs.Load() != 0 {
		t.Fatal("a clone scheduled a compaction: only a push writes loose objects")
	}

	git.run(clone, "config", "user.name", "Compaction Pusher")
	git.run(clone, "config", "user.email", "compaction@bleephub.invalid")
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("pushed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git.run(clone, "add", "f.txt")
	git.run(clone, "commit", "-m", "pushed")
	git.run(clone, "push", "origin", "HEAD:main")

	awaitStart(t, fake)
	srv.background.Wait()
}
