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
// at.
//
// It is the trade between the two costs a ranged read balances. Too small and a
// sequential walk of a large pack pays a round trip every few objects; too
// large and reading one blob out of a monorepo drags megabytes across the wire.
// Four mebibytes holds roughly forty thousand of the small objects a source
// tree is mostly made of, so a clone streams a gigabyte pack in 256 round trips
// while a single blob lookup transfers at most four mebibytes.
//
// It is tunable because the balance depends on the endpoint: a store with a
// long round trip and wide pipe wants larger extents, one nearby wants smaller.
// The size in force is folded into every cache entry's name, so a replica that
// is reconfigured reads its old chunks under names the new configuration never
// asks for rather than mistaking a four mebibyte extent for a one mebibyte one.
const (
	defaultPackChunkSize = 4 << 20
	packChunkBytesEnv    = "BLEEPHUB_GITSTORE_CHUNK_BYTES"
)

// packChunkSize reports the extent size to read and cache at.
func packChunkSize() int64 {
	if raw := strings.TrimSpace(os.Getenv(packChunkBytesEnv)); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultPackChunkSize
}

// packDiskCache is a byte-budgeted LRU of pack and index extents on local disk,
// shared by every repository in the process and read back after a restart.
//
// It caches content-addressed keys only. A packfile is named for the SHA of its
// own contents and an index is named for the pack it indexes, so a cached chunk
// can never be stale: the name changing is the content changing. That property
// is what lets the cache survive a restart with no validation step and no
// coherence protocol between replicas — there is nothing to invalidate.
type packDiskCache struct {
	root  string
	limit int64

	memoryLimit int64

	mu      sync.Mutex
	entries map[string]*packCacheEntry
	// hot holds recently read chunks in memory. Without it every object
	// decoded out of a pack re-reads the whole four mebibyte extent that holds
	// it from disk, which for a clone of a hundred thousand objects is
	// hundreds of gigabytes of local reads to serve nine megabytes of pack.
	// The chunks are immutable and shared read-only, exactly like the files
	// they came from.
	hot        map[string][]byte
	hotOrder   map[string]int64
	hotBytes   int64
	hotEvicted int64
	// clock counts admissions and is the recency stamp. A counter rather than
	// a wall clock: eviction only needs an order, and a monotonic counter
	// cannot be perturbed by the host's clock moving.
	clock int64
	bytes int64
	// inited records that the on-disk contents have been enumerated, which
	// happens once on first use rather than at construction so that a process
	// that never touches a pack never scans the directory.
	inited bool
}

type packCacheEntry struct {
	path string
	size int64
	used int64
}

// packCaches memoizes one cache per configured directory. Keying the memo on
// the directory rather than on a once-only initialization means a process that
// is reconfigured gets the cache it was reconfigured to use, and it keeps the
// resolution a pure function of the environment with no test-only entry point.
var packCaches sync.Map

// packCacheDirEnv names the directory holding the shared pack cache and
// packCacheBytesEnv its byte budget.
const (
	packCacheDirEnv    = "BLEEPHUB_GITSTORE_CACHE_DIR"
	packCacheBytesEnv  = "BLEEPHUB_GITSTORE_CACHE_BYTES"
	packMemoryBytesEnv = "BLEEPHUB_GITSTORE_MEMORY_CACHE_BYTES"

	defaultPackCacheBytes  = 8 << 30
	defaultPackMemoryBytes = 256 << 20
)

// sharedPackDiskCache returns the cache for the directory this process is
// configured to use, creating it on first reference.
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

// packCacheDir resolves the directory holding cached pack extents.
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

// hotLoad returns a chunk held in memory. The slice is shared with every other
// reader of the same chunk and must not be written to; the keys it serves are
// content addressed, so its contents can never need to change.
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

// packCacheSizeChunk is the pseudo-chunk index under which an object's total
// length is cached. The length has to be cached with the bytes: a reader that
// finds chunk zero already resident would otherwise have to ask the object
// store how long the object is, which is the round trip the cache exists to
// avoid.
const packCacheSizeChunk = -1

// cacheKey names one chunk of one bucket-absolute object key. The object key is
// hashed so that a key containing a path separator, or one longer than a file
// name may be, still maps to exactly one cache file.
func cacheKey(bucket, key string, chunkSize, chunk int64) string {
	digest := sha256.Sum256([]byte(bucket + "\x00" + key + "\x00" + strconv.FormatInt(chunkSize, 10)))
	return hex.EncodeToString(digest[:]) + "." + strconv.FormatInt(chunk, 10)
}

func (c *packDiskCache) pathFor(name string) string {
	return filepath.Join(c.root, name[:2], name)
}

// loadSize returns an object's cached total length.
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

// storeSize records an object's total length beside its chunks.
func (c *packDiskCache) storeSize(bucket, key string, chunkSize, size int64) {
	c.store(bucket, key, chunkSize, packCacheSizeChunk, []byte(strconv.FormatInt(size, 10)))
}

// load returns a cached chunk, or nil when the chunk is not resident.
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

// store admits a chunk. A failure to write is not an error the caller needs to
// see: the bytes are already in hand, and a cache that cannot be written just
// costs the next reader a round trip.
func (c *packDiskCache) store(bucket, key string, chunkSize, chunk int64, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}
	name := cacheKey(bucket, key, chunkSize, chunk)
	path := c.pathFor(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}
	// A reader that opened a half-written cache file would decode a truncated
	// packfile extent, so the bytes land under a temporary name and become
	// visible with a rename, which is atomic within a directory.
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

// initLocked enumerates what a previous run of this process left behind. This
// is the whole of the restart-survival mechanism: the chunk files are the
// cache, and the in-memory map is a recency index rebuilt from them.
func (c *packDiskCache) initLocked() error {
	if c.inited {
		return nil
	}
	if err := os.MkdirAll(c.root, 0o750); err != nil {
		return fmt.Errorf("pack cache %s: %w", c.root, err)
	}
	// The walk runs inside an os.Root on the cache directory rather than over
	// absolute paths, so the names it hands back are relative to that root and
	// the removal below resolves under it. A symlink that appeared in the cache
	// directory between the walk reading a name and this deleting it therefore
	// has nothing outside the cache it could name.
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
				// A temporary file is a write that did not finish, from this
				// run or a previous one. It is not a cache entry and never
				// becomes one, because admission renames rather than links.
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
