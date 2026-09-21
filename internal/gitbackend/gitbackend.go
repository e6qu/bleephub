// Package gitbackend selects and configures bleephub's git storage, and the
// object store its service bytes share with it, from the process environment.
// The gitstore library it configures never reads the environment itself; this
// is the one place BLEEPHUB_* storage settings are parsed, and the one place
// that chooses between object-store drivers (openBucket).
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
	"github.com/e6qu/bleephub/gitstore/objstore/azure"
	"github.com/e6qu/bleephub/gitstore/objstore/gcs"
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
	settings, err := SettingsFromEnv()
	if err != nil {
		return nil, err
	}
	if settings.GitBucket == "" {
		StoreCache.Inited = true
		return nil, nil
	}
	opened, err := openConforming(ctx, settings, settings.GitBucket, settings.GitPrefix)
	if err != nil {
		return nil, err
	}
	StoreCache.Store = opened
	StoreCache.Inited = true
	return opened, nil
}

// OpenByteStore opens the store the service's bytes are kept in — artifacts,
// logs, packages, LFS objects, release assets — or returns nil when they are not
// in an object store. It is held to the same proof as the git store: a bucket
// that is missing, refuses this process's credentials or does not keep what it
// is given is found out here, not by the first artifact upload.
func OpenByteStore(ctx context.Context) (*gitstore.Store, error) {
	settings, err := SettingsFromEnv()
	if err != nil {
		return nil, err
	}
	if settings.ObjectBucket == "" {
		return nil, nil
	}
	return openConforming(ctx, settings, settings.ObjectBucket, settings.ObjectPrefix)
}

// openConforming opens the store kept under prefix in the named bucket and runs
// the conformance probe under a prefix of its own inside it. The probe writes
// and deletes a single key there.
func openConforming(ctx context.Context, settings Settings, bucketName, prefix string) (*gitstore.Store, error) {
	bucket, err := openBucket(settings, bucketName)
	if err != nil {
		return nil, err
	}
	opened := gitstore.Open(bucket, prefix, settings.Options)
	ctx, cancel := context.WithTimeout(ctx, conformanceTimeout)
	defer cancel()
	if err := objstore.Conform(ctx, opened.Bucket(), path.Join(opened.Prefix(), conformancePrefix)+"/"); err != nil {
		return nil, err
	}
	return opened, nil
}

// openBucket builds the driver the deployment named, for one of its buckets. It
// is the one place that knows there is more than one kind of object store:
// everything above it holds an objstore.Bucket, and everything below it is one
// driver. Only the chosen driver's settings are read, and SettingsFromEnv has
// already refused a deployment that set another's.
//
// BLEEPHUB_GITSTORE_MULTIPART_BYTES is the size an upload too large for one
// request goes in pieces of, whatever the driver calls a piece. Each driver has
// a constraint of its own on that size — S3 no part under 5 MiB, Azure no block
// over 4000 MiB, Cloud Storage a multiple of 256 KiB — and states it in the
// error returned here.
func openBucket(settings Settings, bucketName string) (objstore.Bucket, error) {
	pieceBytes := settings.Options.UploadPieceBytes()
	var bucket objstore.Bucket
	var err error
	switch settings.Driver {
	case driverS3:
		bucket, err = objstore.NewS3(bucketName, objstore.S3Options{
			Endpoint:  settings.Endpoint,
			Region:    setting(envS3Region),
			PartBytes: pieceBytes,
		})
	case driverAzure:
		bucket, err = azure.New(bucketName, azure.Options{
			Endpoint:    settings.Endpoint,
			AccountName: setting(envAzureAccount),
			AccountKey:  setting(envAzureKey),
			BlockBytes:  pieceBytes,
		})
	case driverGCS:
		var credentialsJSON []byte
		credentialsJSON, err = readCredentialsFile(setting(envGCSCredentialsFile))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envGCSCredentialsFile, err)
		}
		// The tunable was read as an int64, so this holds; it is checked because
		// Cloud Storage's driver counts in signed bytes and the library's
		// accessor in unsigned ones.
		if pieceBytes > math.MaxInt64 {
			return nil, fmt.Errorf("BLEEPHUB_GITSTORE_MULTIPART_BYTES=%d: too large to be a chunk size", pieceBytes)
		}
		bucket, err = gcs.New(bucketName, gcs.Options{
			Endpoint:        settings.Endpoint,
			CredentialsJSON: credentialsJSON,
			ChunkBytes:      int64(pieceBytes),
		})
	default:
		return nil, fmt.Errorf("%s=%q: no such driver", envObjectStore, settings.Driver)
	}
	if err != nil {
		return nil, fmt.Errorf("%s=%s, bucket %q, uploading in pieces of %d bytes (BLEEPHUB_GITSTORE_MULTIPART_BYTES): %w",
			envObjectStore, settings.Driver, bucketName, pieceBytes, err)
	}
	return bucket, nil
}

// readCredentialsFile reads the service-account key file the operator named. It
// is opened through a root on its own directory, so the name reaches the file
// system as one path element and nothing in it can walk anywhere else.
func readCredentialsFile(name string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(filepath.Base(name))
}

func GitDataDir() string {
	return os.Getenv("BLEEPHUB_GIT_DIR")
}

// GitStorageIsObjectStore reports whether the deployment keeps its git
// repositories in an object store, whichever one: naming a bucket for them is
// what says so.
func GitStorageIsObjectStore() bool {
	return setting(envGitBucket) != ""
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

// OpenExistingGitStorage opens a repository the application already has a record
// of, which is what a restart does with every one of them. In an object store
// such a repository has a manifest, and one that has none is refused with
// gitstore.ErrNoManifest rather than initialized over: it is either lost, or
// was written by a version of the engine that kept references as objects and is
// waiting for `bleephub adopt`. The other backends hold nothing a restart could
// mistake — a directory is reopened and a memory repository starts empty — and
// are opened as ever.
func OpenExistingGitStorage(ctx context.Context, fullName string) (gitStorage.Storer, error) {
	if err := gitstore.ValidateRepoStorageFullName(fullName); err != nil {
		return nil, err
	}
	objectStore, err := GetStore(ctx)
	if err != nil {
		return nil, err
	}
	if objectStore != nil {
		stor, err := objectStore.ExistingRepository(fullName)
		if errors.Is(err, gitstore.ErrNoManifest) {
			// This is the error an operator meets on the first start after
			// upgrading a store written by an earlier bleephub, and it stops the
			// server. It has to say what to do, to someone who has not read the
			// engine's history.
			return nil, fmt.Errorf("%w: if this store was written by an earlier bleephub, run `bleephub adopt` once against it and start again (README, \"Upgrade note\"; docs/git-storage.md); nothing has been changed", err)
		}
		return stor, err
	}
	return OpenOrInitGitStorage(ctx, fullName)
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
// options. They mean the same whichever driver the deployment chose, and how the
// store is reached is not among them: see openBucket. An unset variable leaves
// the library's documented default in force.
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
	if threshold, set := count("BLEEPHUB_OBJECT_STORE_BREAKER_THRESHOLD"); set {
		switch {
		case threshold == 0:
			opts.BreakerThreshold = -1
		case threshold > math.MaxInt32:
			problems = append(problems, fmt.Errorf("BLEEPHUB_OBJECT_STORE_BREAKER_THRESHOLD=%d: too large to be a count of failures", threshold))
		default:
			opts.BreakerThreshold = int(threshold)
		}
	}
	if millis, set := count("BLEEPHUB_OBJECT_STORE_BREAKER_COOLDOWN_MS"); set {
		opts.BreakerCooldown = time.Duration(millis) * time.Millisecond
	}
	return opts, errors.Join(problems...)
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
