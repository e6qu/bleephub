package bleephub

import (
	"context"
	"testing"
	"time"
)

// TestActionsCacheEvictsLRUOverRepoBudget pins ACT-052's eviction policy: when a
// repository's finalized caches exceed the per-repo budget, the least-recently
// -used entries (oldest LastAccessedAt) are evicted until the repo is back under
// budget — not arbitrary or newest-first entries.
func TestActionsCacheEvictsLRUOverRepoBudget(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	as := srv.artifactStore
	as.maxRepoCacheBytes = 1000

	base := fixedTestTime.UTC()
	add := func(id int64, key string, size int64, accessed time.Time) {
		as.mu.Lock()
		e := &CacheEntry{
			ID: id, Repo: "admin/r", Key: key, Version: "v1", Size: size,
			Finalized: true, LastAccessedAt: accessed,
		}
		as.caches[id] = e
		as.cacheIndex[cacheLookupKey(e.Repo, e.Key, e.Version)] = id
		as.mu.Unlock()
	}
	// Total 1500 > budget 1000. Evicting the oldest (id=1, 600 bytes) alone
	// brings the repo to 900 <= 1000, so exactly and only the LRU entry goes.
	add(1, "oldest", 600, base)
	add(2, "middle", 600, base.Add(time.Minute))
	add(3, "newest", 300, base.Add(2*time.Minute))

	srv.evictRepoCacheOverLimit(context.Background(), "admin/r")

	as.mu.RLock()
	defer as.mu.RUnlock()
	if _, present := as.caches[1]; present {
		t.Fatal("LRU entry (id=1) was not evicted")
	}
	if _, ok := as.caches[2]; !ok {
		t.Fatal("id=2 was wrongly evicted (not LRU)")
	}
	if _, ok := as.caches[3]; !ok {
		t.Fatal("id=3 was wrongly evicted (most recently used)")
	}
	if _, ok := as.cacheIndex[cacheLookupKey("admin/r", "oldest", "v1")]; ok {
		t.Fatal("evicted entry's cacheIndex entry remains")
	}
	// A repo already under budget is left untouched.
	as.mu.RUnlock()
	srv.evictRepoCacheOverLimit(context.Background(), "admin/r")
	as.mu.RLock()
	if len(as.caches) != 2 {
		t.Fatalf("under-budget repo mutated: %d caches remain, want 2", len(as.caches))
	}
}
