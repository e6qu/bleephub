package gitbackend

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
)

func TestS3Region(t *testing.T) {
	t.Setenv("BLEEPHUB_S3_REGION", "eu-west-1")
	t.Setenv("AWS_REGION", "us-east-1")
	if got := s3Region(); got != "eu-west-1" {
		t.Fatalf("explicit Bleephub S3 region = %q, want eu-west-1", got)
	}
	t.Setenv("BLEEPHUB_S3_REGION", "")
	if got := s3Region(); got != "us-east-1" {
		t.Fatalf("AWS S3 region = %q, want us-east-1", got)
	}
	t.Setenv("AWS_REGION", "")
	if got := s3Region(); got != "us-east-1" {
		t.Fatalf("default S3 region = %q, want us-east-1", got)
	}
}

// clearTunables pins every storage tunable to unset, so a developer's shell
// cannot leak into the assertions.
func clearTunables(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BLEEPHUB_S3_REGION", "AWS_REGION",
		"BLEEPHUB_GITSTORE_CHUNK_BYTES", "BLEEPHUB_GITSTORE_CACHE_DIR", "BLEEPHUB_GITSTORE_CACHE_BYTES",
		"BLEEPHUB_GITSTORE_MEMORY_CACHE_BYTES", "BLEEPHUB_GITSTORE_INDEX_FRESHNESS",
		"BLEEPHUB_GITSTORE_COMPACT_AFTER", "BLEEPHUB_GITSTORE_MULTIPART_BYTES",
		"BLEEPHUB_S3_BREAKER_THRESHOLD", "BLEEPHUB_S3_BREAKER_COOLDOWN_MS",
	} {
		t.Setenv(name, "")
	}
}

// TestOptionsFromEnvLeavesUnsetTunablesToTheLibrary pins that an empty
// environment hands the library zero values — its "use the default" — for every
// tunable, rather than restating defaults that could drift from the library's.
func TestOptionsFromEnvLeavesUnsetTunablesToTheLibrary(t *testing.T) {
	clearTunables(t)
	opts, err := OptionsFromEnv()
	if err != nil {
		t.Fatalf("an empty environment is an error: %v", err)
	}
	if opts.ChunkBytes != 0 || opts.CacheBytes != 0 || opts.MemoryCacheBytes != 0 ||
		opts.IndexFreshness != 0 || opts.CompactionTrigger != 0 || opts.MultipartBytes != 0 ||
		opts.BreakerThreshold != 0 || opts.BreakerCooldown != 0 {
		t.Fatalf("an empty environment overrode a library default: %+v", opts)
	}
	if opts.Region != "us-east-1" {
		t.Fatalf("region = %q, want us-east-1", opts.Region)
	}
	if filepath.Base(opts.CacheDir) != "bleephub-gitstore-cache" {
		t.Fatalf("cache dir = %q, want bleephub's own directory under the temp dir", opts.CacheDir)
	}
}

func TestOptionsFromEnvParsesEveryTunable(t *testing.T) {
	clearTunables(t)
	t.Setenv("BLEEPHUB_GITSTORE_CHUNK_BYTES", "1048576")
	t.Setenv("BLEEPHUB_GITSTORE_CACHE_DIR", "/var/cache/packs")
	t.Setenv("BLEEPHUB_GITSTORE_CACHE_BYTES", "2048")
	t.Setenv("BLEEPHUB_GITSTORE_MEMORY_CACHE_BYTES", "1024")
	t.Setenv("BLEEPHUB_GITSTORE_INDEX_FRESHNESS", "2s")
	t.Setenv("BLEEPHUB_GITSTORE_COMPACT_AFTER", "500")
	t.Setenv("BLEEPHUB_GITSTORE_MULTIPART_BYTES", "8388608")
	t.Setenv("BLEEPHUB_S3_BREAKER_THRESHOLD", "9")
	t.Setenv("BLEEPHUB_S3_BREAKER_COOLDOWN_MS", "1500")

	opts, err := OptionsFromEnv()
	if err != nil {
		t.Fatalf("valid settings are an error: %v", err)
	}
	if opts.ChunkBytes != 1048576 || opts.CacheDir != "/var/cache/packs" || opts.CacheBytes != 2048 ||
		opts.MemoryCacheBytes != 1024 || opts.IndexFreshness != 2*time.Second || opts.CompactionTrigger != 500 ||
		opts.MultipartBytes != 8388608 || opts.BreakerThreshold != 9 || opts.BreakerCooldown != 1500*time.Millisecond {
		t.Fatalf("parsed options = %+v", opts)
	}
}

// TestOptionsFromEnvTranslatesZeroToOff pins the seam between the two
// conventions: the environment spells "off" as 0, the library as negative, and
// the library reads 0 as "use the default" — so a 0 passed through untranslated
// would silently re-enable what the operator turned off.
func TestOptionsFromEnvTranslatesZeroToOff(t *testing.T) {
	clearTunables(t)
	t.Setenv("BLEEPHUB_GITSTORE_MEMORY_CACHE_BYTES", "0")
	t.Setenv("BLEEPHUB_GITSTORE_INDEX_FRESHNESS", "0")
	t.Setenv("BLEEPHUB_GITSTORE_COMPACT_AFTER", "0")
	t.Setenv("BLEEPHUB_S3_BREAKER_THRESHOLD", "0")

	opts, err := OptionsFromEnv()
	if err != nil {
		t.Fatalf("turning tunables off is an error: %v", err)
	}
	if opts.MemoryCacheBytes >= 0 || opts.IndexFreshness >= 0 || opts.CompactionTrigger >= 0 || opts.BreakerThreshold >= 0 {
		t.Fatalf("a tunable set to 0 was not translated to the library's off: %+v", opts)
	}
}

// TestOptionsFromEnvRefusesASettingItCannotRead pins that a setting which was
// given and cannot be read stops the server, naming every such setting at once.
// It used to be ignored: an operator who wrote a cache size of "8G" ran on the
// default, which nobody had chosen, and found out from a full disk or an empty
// cache. A size of 0 is refused too, for the tunables that have no "off": it
// does not mean what 0 means for the ones that do.
func TestOptionsFromEnvRefusesASettingItCannotRead(t *testing.T) {
	clearTunables(t)
	t.Setenv("BLEEPHUB_GITSTORE_CHUNK_BYTES", "lots")
	t.Setenv("BLEEPHUB_GITSTORE_CACHE_BYTES", "-5")
	t.Setenv("BLEEPHUB_GITSTORE_MULTIPART_BYTES", "0")
	t.Setenv("BLEEPHUB_GITSTORE_INDEX_FRESHNESS", "750")
	t.Setenv("BLEEPHUB_S3_BREAKER_THRESHOLD", "3.5")
	_, err := OptionsFromEnv()
	if err == nil {
		t.Fatal("settings that cannot be read were accepted")
	}
	for _, name := range []string{
		"BLEEPHUB_GITSTORE_CHUNK_BYTES", "BLEEPHUB_GITSTORE_CACHE_BYTES", "BLEEPHUB_GITSTORE_MULTIPART_BYTES",
		"BLEEPHUB_GITSTORE_INDEX_FRESHNESS", "BLEEPHUB_S3_BREAKER_THRESHOLD",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
}

// pointGitStorageAt configures the process-wide git object store from the
// environment, as a deployment does, and forgets whatever an earlier test opened.
func pointGitStorageAt(t *testing.T, endpoint string) {
	t.Helper()
	clearTunables(t)
	t.Setenv("BLEEPHUB_S3_ENDPOINT", endpoint)
	t.Setenv("BLEEPHUB_S3_BUCKET", "bleephub-test")
	t.Setenv("BLEEPHUB_S3_PREFIX", "git")
	t.Setenv("BLEEPHUB_GITSTORE_CACHE_DIR", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "bleephub-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "bleephub-test-secret")
	reset := func() {
		StoreCache.Mu.Lock()
		StoreCache.Store = nil
		StoreCache.Inited = false
		StoreCache.Mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// TestGetStoreOpensAConformingObjectStore pins the ordinary start: a store that
// honours its conditions is opened once, memoized, and handed to every caller.
func TestGetStoreOpensAConformingObjectStore(t *testing.T) {
	pointGitStorageAt(t, testutil.FakeS3Endpoint(t))
	opened, err := GetStore(context.Background())
	if err != nil || opened == nil {
		t.Fatalf("a conforming object store was refused: %v", err)
	}
	if opened.Prefix() != "git" {
		t.Fatalf("store prefix = %q, want the configured git prefix", opened.Prefix())
	}
	again, err := GetStore(context.Background())
	if err != nil || again != opened {
		t.Fatalf("second GetStore = %p, %v; want the memoized %p", again, err, opened)
	}
}

// TestGetStoreRefusesAnObjectStoreThatIgnoresConditions is why the store is
// probed when it is opened: a store that answers a conditional write with
// success and overwrites would let two replicas both move one branch, and
// nothing short of asking it to refuse a write reveals that.
func TestGetStoreRefusesAnObjectStoreThatIgnoresConditions(t *testing.T) {
	pointGitStorageAt(t, testutil.S3EndpointIgnoringConditions(t))
	opened, err := GetStore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not conform") {
		t.Fatalf("a store that ignores conditions answered %v, want a conformance refusal", err)
	}
	if opened != nil {
		t.Fatal("a store that failed the probe was handed out")
	}
	if _, err := OpenOrInitGitStorage(context.Background(), "owner/repo"); err == nil {
		t.Fatal("a repository was opened on a store that failed the probe")
	}
}
