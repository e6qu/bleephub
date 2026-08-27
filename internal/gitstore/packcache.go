package gitstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// defaultPackChunkSize is the granularity the pack read path fetches and caches
// at. Too small and a sequential pack walk pays a round trip every few objects;
// too large and one blob lookup drags megabytes across the wire. Tunable per
// endpoint; the size in force is folded into every cache entry's name, so a
// reconfigured replica never mistakes a 4 MiB extent for a 1 MiB one.
const (
	defaultPackChunkSize = 4 << 20
	packChunkBytesEnv    = "BLEEPHUB_GITSTORE_CHUNK_BYTES"
)

func packChunkSize() int64 {
	if raw := strings.TrimSpace(os.Getenv(packChunkBytesEnv)); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultPackChunkSize
}

// packDiskCache is a byte-budgeted LRU of pack and index extents on local disk,
// shared by every repository in the process and read back after a restart. It
// caches content-addressed keys only, so a cached chunk can never be stale and
// survives a restart with no validation step or cross-replica coherence
// protocol.
type packDiskCache struct {
	root  string
	limit int64

	memoryLimit int64

	mu      sync.Mutex
	entries map[string]*packCacheEntry
	// hot holds recently read chunks in memory; without it, each object decoded
	// re-reads its whole extent from disk. Chunks are immutable and shared
	// read-only.
	hot        map[string][]byte
	hotOrder   map[string]int64
	hotBytes   int64
	hotEvicted int64
	// clock counts admissions and stamps recency. A monotonic counter cannot be
	// perturbed by the host's clock moving.
	clock int64
	bytes int64
	// inited records that on-disk contents have been enumerated, done once on
	// first use so a process that never touches a pack never scans the directory.
	inited bool
}

type packCacheEntry struct {
	path string
	size int64
	used int64
}

// packCaches memoizes one cache per configured directory, so a reconfigured
// process gets the cache it was reconfigured to use.
var packCaches sync.Map

const (
	packCacheDirEnv    = "BLEEPHUB_GITSTORE_CACHE_DIR"
	packCacheBytesEnv  = "BLEEPHUB_GITSTORE_CACHE_BYTES"
	packMemoryBytesEnv = "BLEEPHUB_GITSTORE_MEMORY_CACHE_BYTES"

	defaultPackCacheBytes  = 8 << 30
	defaultPackMemoryBytes = 256 << 20
)

// sharedPackDiskCache returns the cache for this process's configured
// directory, creating it on first reference.
func sharedPackDiskCache() *packDiskCache {
	dir := packCacheDir()
	if cached, ok := packCaches.Load(dir); ok {
		return cached.(*packDiskCache)
	}
	limit := int64(defaultPackCacheBytes)
	if raw := strings.TrimSpace(os.Getenv(packCacheBytesEnv)); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && parsed > 0 {
			limit = parsed
		}
	}
	cache := newPackDiskCache(dir, limit)
	if raw := strings.TrimSpace(os.Getenv(packMemoryBytesEnv)); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && parsed >= 0 {
			cache.memoryLimit = parsed
		}
	}
	cached, _ := packCaches.LoadOrStore(dir, cache)
	return cached.(*packDiskCache)
}

func packCacheDir() string {
	if dir := strings.TrimSpace(os.Getenv(packCacheDirEnv)); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "bleephub-gitstore-cache")
}

func newPackDiskCache(root string, limit int64) *packDiskCache {
	return &packDiskCache{
		root:        root,
		limit:       limit,
		memoryLimit: defaultPackMemoryBytes,
		entries:     map[string]*packCacheEntry{},
		hot:         map[string][]byte{},
		hotOrder:    map[string]int64{},
	}
}

// hotLoad returns a chunk held in memory. The slice is shared read-only with
// every other reader and must not be written to.
func (c *packDiskCache) hotLoad(name string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.hot[name]
	if !ok {
		return nil
	}
	c.clock++
	c.hotOrder[name] = c.clock
	return data
}

func (c *packDiskCache) hotStore(name string, data []byte) {
	if c.memoryLimit <= 0 || int64(len(data)) > c.memoryLimit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.hot[name]; ok {
		c.hotBytes -= int64(len(existing))
	}
	c.clock++
	c.hot[name] = data
	c.hotOrder[name] = c.clock
	c.hotBytes += int64(len(data))
	for c.hotBytes > c.memoryLimit {
		oldest := ""
		var oldestUsed int64
		for candidate, used := range c.hotOrder {
			if oldest == "" || used < oldestUsed {
				oldest, oldestUsed = candidate, used
			}
		}
		if oldest == "" {
			return
		}
		c.hotBytes -= int64(len(c.hot[oldest]))
		delete(c.hot, oldest)
		delete(c.hotOrder, oldest)
		c.hotEvicted++
	}
}

// packCacheSizeChunk is the pseudo-chunk index caching an object's total
// length, kept beside the bytes so a reader with chunk zero resident need not
// round-trip to the object store for the length.
const packCacheSizeChunk = -1

// cacheKey names one chunk of one bucket-absolute object key. The object key is
// hashed so a key with a path separator, or longer than a file name, still maps
// to exactly one cache file.
func cacheKey(bucket, key string, chunkSize, chunk int64) string {
	digest := sha256.Sum256([]byte(bucket + "\x00" + key + "\x00" + strconv.FormatInt(chunkSize, 10)))
	return hex.EncodeToString(digest[:]) + "." + strconv.FormatInt(chunk, 10)
}

func (c *packDiskCache) pathFor(name string) string {
	return filepath.Join(c.root, name[:2], name)
}

func (c *packDiskCache) loadSize(bucket, key string, chunkSize int64) (int64, bool) {
	raw := c.load(bucket, key, chunkSize, packCacheSizeChunk)
	if raw == nil {
		return 0, false
	}
	size, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || size < 0 {
		return 0, false
	}
	return size, true
}

func (c *packDiskCache) storeSize(bucket, key string, chunkSize, size int64) {
	c.store(bucket, key, chunkSize, packCacheSizeChunk, []byte(strconv.FormatInt(size, 10)))
}

// load returns a cached chunk, or nil when not resident.
func (c *packDiskCache) load(bucket, key string, chunkSize, chunk int64) []byte {
	if c == nil {
		return nil
	}
	name := cacheKey(bucket, key, chunkSize, chunk)
	if data := c.hotLoad(name); data != nil {
		return data
	}
	c.mu.Lock()
	if err := c.initLocked(); err != nil {
		c.mu.Unlock()
		return nil
	}
	entry, ok := c.entries[name]
	if ok {
		c.clock++
		entry.used = c.clock
	}
	c.mu.Unlock()
	if !ok {
		return nil
	}
	data, err := os.ReadFile(entry.path)
	if err != nil {
		c.forget(name)
		return nil
	}
	c.hotStore(name, data)
	return data
}

// store admits a chunk. A write failure is not surfaced: the bytes are in hand,
// and an unwritten cache only costs the next reader a round trip.
func (c *packDiskCache) store(bucket, key string, chunkSize, chunk int64, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}
	name := cacheKey(bucket, key, chunkSize, chunk)
	path := c.pathFor(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}
	// Write to a temp name and rename in: a half-written file would decode as a
	// truncated packfile extent, and rename is atomic within a directory.
	temp, err := os.CreateTemp(filepath.Dir(path), "tmp-")
	if err != nil {
		return
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(temp.Name())
		return
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		_ = os.Remove(temp.Name())
		return
	}

	c.hotStore(name, data)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initLocked(); err != nil {
		return
	}
	c.admitLocked(name, path, int64(len(data)))
	c.evictLocked()
}

func (c *packDiskCache) admitLocked(name, path string, size int64) {
	if existing, ok := c.entries[name]; ok {
		c.bytes -= existing.size
	}
	c.clock++
	c.entries[name] = &packCacheEntry{path: path, size: size, used: c.clock}
	c.bytes += size
}

func (c *packDiskCache) evictLocked() {
	if c.bytes <= c.limit {
		return
	}
	names := make([]string, 0, len(c.entries))
	for name := range c.entries {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return c.entries[names[i]].used < c.entries[names[j]].used
	})
	for _, name := range names {
		if c.bytes <= c.limit {
			return
		}
		entry := c.entries[name]
		_ = os.Remove(entry.path)
		c.bytes -= entry.size
		delete(c.entries, name)
	}
}

func (c *packDiskCache) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[name]
	if !ok {
		return
	}
	c.bytes -= entry.size
	delete(c.entries, name)
}

// initLocked enumerates what a previous run left behind: the chunk files are
// the cache, and the in-memory map is a recency index rebuilt from them.
func (c *packDiskCache) initLocked() error {
	if c.inited {
		return nil
	}
	if err := os.MkdirAll(c.root, 0o750); err != nil {
		return fmt.Errorf("pack cache %s: %w", c.root, err)
	}
	// Walk inside an os.Root so names resolve under the cache directory; a
	// symlink that appears mid-walk cannot name anything outside the cache.
	root, err := os.OpenRoot(c.root)
	if err == nil {
		defer func() { _ = root.Close() }()
		err = fs.WalkDir(root.FS(), ".", func(walked string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if strings.HasPrefix(name, "tmp-") {
				// An unfinished write, never a cache entry since admission
				// renames rather than links.
				_ = root.Remove(walked)
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			c.clock++
			c.entries[name] = &packCacheEntry{
				path: filepath.Join(c.root, filepath.FromSlash(walked)),
				size: info.Size(),
				used: c.clock,
			}
			c.bytes += info.Size()
			return nil
		})
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pack cache %s: %w", c.root, err)
	}
	c.inited = true
	c.evictLocked()
	return nil
}
