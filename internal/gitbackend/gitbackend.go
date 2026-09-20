// Package gitbackend selects and configures bleephub's git storage from the
// process environment. The gitstore library it configures never reads the
// environment itself; this is the one place BLEEPHUB_* storage settings are
// parsed.
package gitbackend

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/e6qu/bleephub/gitstore/objstore"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// StoreCacheState memoizes the process-wide git object store built from the
// environment. Fields are exported so dependent-package tests can reset the memo
// without a test-only exported function tripping the deadcode gate.
type StoreCacheState struct {
	Mu     sync.Mutex
	Store  *gitstore.Store
	Inited bool
}

// StoreCache is the process-wide memo consulted by GetStore.
var StoreCache StoreCacheState

// conformancePrefix is where, inside the configured git prefix, the startup
// probe writes its one key. No repository can live there: an owner's name never
// starts with a dot.
const conformancePrefix = ".conformance"

// conformanceTimeout bounds the startup probe: a store that does not answer a
// dozen small requests in this long is not one to start serving on.
const conformanceTimeout = 30 * time.Second

// GetStore returns the process-wide git object store, or nil when git storage is
// not in an object store. The first call opens it and proves, against the live
// bucket, the guarantees the storage engine is built on; a store that fails the
// proof is not memoized and its error is returned, which at startup means the
// server does not start.
func GetStore(ctx context.Context) (*gitstore.Store, error) {
	StoreCache.Mu.Lock()
	defer StoreCache.Mu.Unlock()
	if StoreCache.Inited {
		return StoreCache.Store, nil
	}
	bucket := os.Getenv("BLEEPHUB_S3_BUCKET")
	if bucket == "" {
		StoreCache.Inited = true
		return nil, nil
	}
	opened, err := NewStore(ctx, os.Getenv("BLEEPHUB_S3_ENDPOINT"), bucket, os.Getenv("BLEEPHUB_S3_PREFIX"))
	if err != nil {
		return nil, err
	}
	if err := Conform(ctx, opened); err != nil {
		return nil, err
	}
	StoreCache.Store = opened
	StoreCache.Inited = true
	return opened, nil
}

// NewStore opens an object store tuned from the environment.
func NewStore(ctx context.Context, endpoint, bucket, prefix string) (*gitstore.Store, error) {
	return gitstore.OpenS3(ctx, endpoint, bucket, prefix, OptionsFromEnv())
}

// Conform runs the object-store conformance probe under a prefix of its own
// inside the store's prefix. The probe writes and deletes a single key there.
func Conform(ctx context.Context, opened *gitstore.Store) error {
	ctx, cancel := context.WithTimeout(ctx, conformanceTimeout)
	defer cancel()
	return objstore.Conform(ctx, opened.Bucket(), path.Join(opened.Prefix(), conformancePrefix)+"/")
}

func GitDataDir() string {
	return os.Getenv("BLEEPHUB_GIT_DIR")
}

func IsS3GitStorage() bool {
	return os.Getenv("BLEEPHUB_S3_BUCKET") != ""
}

// OpenOrInitGitStorage opens the repository on whichever backend the
// environment selects — object store, then local directory, then memory — and
// makes it a git repository if it is not one already.
func OpenOrInitGitStorage(ctx context.Context, fullName string) (gitStorage.Storer, error) {
	stor, err := openGitStorage(ctx, fullName)
	if err != nil {
		return nil, err
	}
	if err := gitstore.Init(stor); err != nil {
		return nil, err
	}
	return stor, nil
}

func openGitStorage(ctx context.Context, fullName string) (gitStorage.Storer, error) {
	if err := gitstore.ValidateRepoStorageFullName(fullName); err != nil {
		return nil, err
	}
	objectStore, err := GetStore(ctx)
	if err != nil {
		return nil, err
	}
	if objectStore != nil {
		return objectStore.Repository(fullName)
	}
	if gitDir := GitDataDir(); gitDir != "" {
		return gitstore.OpenDir(gitDir, fullName)
	}
	return gitstore.OpenMemory(fullName)
}

// OptionsFromEnv maps the BLEEPHUB_* storage tunables onto the library's
// options. An unset or unparseable variable leaves the library default in force.
// The library spells "off" as a negative value, so a variable set to 0 — off,
// for the tunables that have an off — is translated here.
func OptionsFromEnv() gitstore.Options {
	opts := gitstore.Options{
		Region:            s3Region(),
		ChunkBytes:        envPositiveInt64("BLEEPHUB_GITSTORE_CHUNK_BYTES"),
		CacheDir:          packCacheDir(),
		CacheBytes:        envPositiveInt64("BLEEPHUB_GITSTORE_CACHE_BYTES"),
		MemoryCacheBytes:  zeroIsOff(envNonNegativeInt64("BLEEPHUB_GITSTORE_MEMORY_CACHE_BYTES")),
		CompactionTrigger: zeroIsOff(envNonNegativeInt64("BLEEPHUB_GITSTORE_COMPACT_AFTER")),
		MultipartBytes:    envPositiveInt64("BLEEPHUB_GITSTORE_MULTIPART_BYTES"),
	}
	if freshness, ok := envDuration("BLEEPHUB_GITSTORE_INDEX_FRESHNESS"); ok {
		opts.IndexFreshness = freshness
		if freshness == 0 {
			opts.IndexFreshness = -1
		}
	}
	if threshold, ok := envInt("BLEEPHUB_S3_BREAKER_THRESHOLD"); ok {
		opts.BreakerThreshold = threshold
		if threshold <= 0 {
			opts.BreakerThreshold = -1
		}
	}
	if millis, ok := envInt("BLEEPHUB_S3_BREAKER_COOLDOWN_MS"); ok {
		opts.BreakerCooldown = time.Duration(millis) * time.Millisecond
	}
	return opts
}

// s3Region selects the AWS region: explicit BLEEPHUB_S3_REGION, then
// ECS-supplied AWS_REGION, then a local-simulator default.
func s3Region() string {
	if region := strings.TrimSpace(os.Getenv("BLEEPHUB_S3_REGION")); region != "" {
		return region
	}
	if region := strings.TrimSpace(os.Getenv("AWS_REGION")); region != "" {
		return region
	}
	return "us-east-1"
}

func packCacheDir() string {
	if dir := strings.TrimSpace(os.Getenv("BLEEPHUB_GITSTORE_CACHE_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "bleephub-gitstore-cache")
}

func envInt(name string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// envPositiveInt64 returns the variable's value, or 0 — the library's "use the
// default" — when it is unset, unparseable or not positive.
func envPositiveInt64(name string) int64 {
	parsed, ok := envNonNegativeInt64(name)
	if !ok {
		return 0
	}
	return parsed
}

func envNonNegativeInt64(name string) (int64, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

// zeroIsOff translates an explicitly configured 0 into the library's negative
// "off"; an unset variable stays 0, the library's "use the default".
func zeroIsOff(value int64, set bool) int64 {
	if set && value == 0 {
		return -1
	}
	return value
}

// envDuration accepts a Go duration or a bare count of milliseconds.
func envDuration(name string) (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	if parsed, err := time.ParseDuration(raw); err == nil && parsed >= 0 {
		return parsed, true
	}
	if millis, err := strconv.Atoi(raw); err == nil && millis >= 0 {
		return time.Duration(millis) * time.Millisecond, true
	}
	return 0, false
}
