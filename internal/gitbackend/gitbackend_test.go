package gitbackend

import (
	"path/filepath"
	"testing"
	"time"
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
	opts := OptionsFromEnv()
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
	t.Setenv("BLEEPHUB_GITSTORE_MULTIPART_BYTES", "4096")
	t.Setenv("BLEEPHUB_S3_BREAKER_THRESHOLD", "9")
	t.Setenv("BLEEPHUB_S3_BREAKER_COOLDOWN_MS", "1500")

	opts := OptionsFromEnv()
	if opts.ChunkBytes != 1048576 || opts.CacheDir != "/var/cache/packs" || opts.CacheBytes != 2048 ||
		opts.MemoryCacheBytes != 1024 || opts.IndexFreshness != 2*time.Second || opts.CompactionTrigger != 500 ||
		opts.MultipartBytes != 4096 || opts.BreakerThreshold != 9 || opts.BreakerCooldown != 1500*time.Millisecond {
		t.Fatalf("parsed options = %+v", opts)
	}

	t.Setenv("BLEEPHUB_GITSTORE_INDEX_FRESHNESS", "750")
	if got := OptionsFromEnv().IndexFreshness; got != 750*time.Millisecond {
		t.Fatalf("a bare number is milliseconds: freshness = %s, want 750ms", got)
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

	opts := OptionsFromEnv()
	if opts.MemoryCacheBytes >= 0 || opts.IndexFreshness >= 0 || opts.CompactionTrigger >= 0 || opts.BreakerThreshold >= 0 {
		t.Fatalf("a tunable set to 0 was not translated to the library's off: %+v", opts)
	}
}

func TestOptionsFromEnvIgnoresUnparseableValues(t *testing.T) {
	clearTunables(t)
	t.Setenv("BLEEPHUB_GITSTORE_CHUNK_BYTES", "lots")
	t.Setenv("BLEEPHUB_GITSTORE_CACHE_BYTES", "-5")
	t.Setenv("BLEEPHUB_GITSTORE_INDEX_FRESHNESS", "soon")
	opts := OptionsFromEnv()
	if opts.ChunkBytes != 0 || opts.CacheBytes != 0 || opts.IndexFreshness != 0 {
		t.Fatalf("an unparseable value overrode a library default: %+v", opts)
	}
}
