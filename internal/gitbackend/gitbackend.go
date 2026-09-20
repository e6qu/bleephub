// Package gitbackend selects and configures bleephub's git storage from the
// process environment. The gitstore library it configures never reads the
// environment itself; this is the one place BLEEPHUB_* storage settings are
// parsed.
package gitbackend

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	opts, err := OptionsFromEnv()
	if err != nil {
		return nil, err
	}
	return gitstore.OpenS3(ctx, endpoint, bucket, prefix, opts)
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
// options. An unset variable leaves the library's documented default in force.
// A variable that is set and cannot be read is an error, and the server does not
// start: an operator who wrote BLEEPHUB_GITSTORE_CACHE_BYTES=8G meant something,
// and running on the default instead is a decision nobody made. The library
// spells "off" as a negative value, so a variable set to 0 — off, for the
// tunables that have an off — is translated here.
func OptionsFromEnv() (gitstore.Options, error) {
	var problems []error
	count := func(name string) (int64, bool) {
		value, set, err := envCount(name)
		if err != nil {
			problems = append(problems, err)
		}
		return value, set
	}
	sized := func(name string) int64 {
		value, set := count(name)
		if set && value == 0 {
			problems = append(problems, fmt.Errorf("%s=0: want a positive number of bytes, or leave it unset", name))
		}
		return value
	}

	opts := gitstore.Options{
		Region:            s3Region(),
		ChunkBytes:        sized("BLEEPHUB_GITSTORE_CHUNK_BYTES"),
		CacheDir:          packCacheDir(),
		CacheBytes:        sized("BLEEPHUB_GITSTORE_CACHE_BYTES"),
		MemoryCacheBytes:  zeroIsOff(count("BLEEPHUB_GITSTORE_MEMORY_CACHE_BYTES")),
		CompactionTrigger: zeroIsOff(count("BLEEPHUB_GITSTORE_COMPACT_AFTER")),
		MultipartBytes:    sized("BLEEPHUB_GITSTORE_MULTIPART_BYTES"),
	}
	freshness, set, err := envDuration("BLEEPHUB_GITSTORE_INDEX_FRESHNESS")
	if err != nil {
		problems = append(problems, err)
	}
	if set {
		opts.IndexFreshness = freshness
		if freshness == 0 {
			opts.IndexFreshness = -1
		}
	}
	if threshold, set := count("BLEEPHUB_S3_BREAKER_THRESHOLD"); set {
		switch {
		case threshold == 0:
			opts.BreakerThreshold = -1
		case threshold > math.MaxInt32:
			problems = append(problems, fmt.Errorf("BLEEPHUB_S3_BREAKER_THRESHOLD=%d: too large to be a count of failures", threshold))
		default:
			opts.BreakerThreshold = int(threshold)
		}
	}
	if millis, set := count("BLEEPHUB_S3_BREAKER_COOLDOWN_MS"); set {
		opts.BreakerCooldown = time.Duration(millis) * time.Millisecond
	}
	return opts, errors.Join(problems...)
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

// envCount reads a variable holding a whole number that is not negative. set
// reports whether the variable was given at all.
func envCount(name string) (value int64, set bool, err error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	parsed, parseErr := strconv.ParseInt(raw, 10, 64)
	if parseErr != nil || parsed < 0 {
		return 0, true, fmt.Errorf("%s=%q: want a whole number that is not negative", name, raw)
	}
	return parsed, true, nil
}

// zeroIsOff translates an explicitly configured 0 into the library's negative
// "off"; an unset variable stays 0, the library's "use the default".
func zeroIsOff(value int64, set bool) int64 {
	if set && value == 0 {
		return -1
	}
	return value
}

// envDuration reads a variable holding a Go duration, such as 250ms, that is not
// negative.
func envDuration(name string) (value time.Duration, set bool, err error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	parsed, parseErr := time.ParseDuration(raw)
	if parseErr != nil || parsed < 0 {
		return 0, true, fmt.Errorf("%s=%q: want a duration that is not negative, such as 250ms", name, raw)
	}
	return parsed, true, nil
}
