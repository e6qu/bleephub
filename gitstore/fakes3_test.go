package gitstore

import (
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/s3fake"
)

// fakeS3 pairs the counting object store with the options the stores opened
// over it run under, so a test tunes one value and every handle it opens agrees.
type fakeS3 struct {
	*s3fake.Server
	opts Options
	// clock, when set, is the clock every store opened from here measures its
	// freshness bounds on, so a test moves time instead of waiting for it.
	clock *testClock
}

// newFakeS3 starts a fake whose stores cache packs in a directory of the test's
// own, so no test reads extents another one cached.
func newFakeS3(tb testing.TB) *fakeS3 {
	tb.Helper()
	server := s3fake.New()
	tb.Cleanup(server.Close)
	return &fakeS3{Server: server, opts: Options{CacheDir: tb.TempDir()}}
}

// store opens a store over the fake. Each call is a replica of its own: nothing
// remembered, nothing listed. Only the pack cache directory is shared, as it is
// between a process and the one that replaces it on the same disk.
func (f *fakeS3) store(prefix string) *Store {
	// The part size is the driver's business; the option that names it is
	// honoured here as OpenS3 honours it.
	var partBytes uint64
	if f.opts.MultipartBytes > 0 {
		partBytes = uint64(f.opts.MultipartBytes)
	}
	store := Open(objstore.NewS3WithClient(f.Client().Client, "bucket", partBytes), prefix, f.opts)
	if f.clock != nil {
		store.shared.now = f.clock.Now
	}
	return store
}

// testClock is a clock that moves only when a test says so. It starts at a
// fixed instant: nothing here depends on what the time is, only on how much of
// it has passed.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
