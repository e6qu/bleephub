package gitstore

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Options tune one Store, every Sub of it and every repository opened from it.
// The zero value selects every default, so a caller sets only what it means to
// change. The package never consults the process environment: an embedding
// application that wants environment-driven configuration parses it and passes
// the result here.
//
// Several tunables have a meaningful "off" as well as a default. For those, zero
// selects the default and a negative value selects off.
type Options struct {
	// Region is the object store's region. Empty selects us-east-1, which is
	// also what S3-compatible simulators expect.
	Region string
	// Credentials signs requests. Nil selects the AWS environment chain
	// (AWS_ACCESS_KEY_ID and friends).
	Credentials *credentials.Credentials
	// Transport carries every object-store request. Nil selects the client's
	// default. A measuring harness wraps it to count requests and inject latency.
	Transport http.RoundTripper

	// ChunkBytes is the extent the pack read path fetches and caches at. Too
	// small and a sequential pack walk pays a round trip every few objects; too
	// large and one blob lookup drags megabytes across the wire. Zero selects
	// 4 MiB.
	ChunkBytes int64
	// CacheDir holds the local pack-extent cache and compaction staging. Empty
	// selects a directory under os.TempDir. Stores naming the same directory
	// share one cache, sized by whichever referenced it first.
	CacheDir string
	// CacheBytes bounds the on-disk pack cache. Zero selects 8 GiB.
	CacheBytes int64
	// MemoryCacheBytes bounds the in-memory tier of the pack cache. Zero
	// selects 256 MiB; negative disables the tier.
	MemoryCacheBytes int64

	// IndexFreshness is how far a read may lag another replica's write: how long
	// the manifest a handle holds may answer reads of references, and "absent"
	// for an object, before the store is asked again, by a conditional read
	// that is usually answered "not modified". A write never relies on it: a commit is a
	// conditional write, and the store refuses one made on an old manifest. Zero
	// selects 250ms; negative revalidates on every read of a reference and every
	// miss.
	IndexFreshness time.Duration

	// CompactAfterPacks is the number of live packs above which a write to a
	// repository requests a compaction through the installed handler. Every push
	// and every flush of written objects adds a pack, and every lookup that
	// misses asks every pack. Zero selects 8; negative never requests a
	// compaction. What a compaction merges is decided by the packs' sizes alone.
	CompactAfterPacks int
	// MultipartBytes is the pack size above which a pack is published through a
	// multipart upload, and the size of its parts. Zero selects 64 MiB. It
	// configures the driver OpenS3 builds; a bucket handed to Open was
	// configured by whoever built it, who asks UploadPieceBytes what to build it
	// with.
	MultipartBytes int64

	// BreakerThreshold is the run of consecutive hard failures that opens the
	// circuit breaker. Zero selects 5; negative disables the breaker.
	BreakerThreshold int
	// BreakerCooldown is how long an open breaker fast-fails before letting one
	// probe through. Zero selects 5s.
	BreakerCooldown time.Duration
}

const (
	defaultRegion           = "us-east-1"
	defaultBreakerThreshold = 5
	defaultBreakerCooldown  = 5 * time.Second
)

// resolved returns o with every zero field replaced by its default and every
// "off" sentinel normalised to the value the consuming code treats as off.
// UploadPieceBytes is MultipartBytes with its default applied: the size a driver
// is to send an upload too large for one request in pieces of — parts on S3,
// blocks on Azure, chunks on Cloud Storage. It is for whoever builds the bucket
// they hand to Open, so that the default is this library's and stated once.
func (o Options) UploadPieceBytes() uint64 {
	return uint64(o.resolved().MultipartBytes) // #nosec G115 -- resolved() leaves it positive
}

func (o Options) resolved() Options {
	if o.Region == "" {
		o.Region = defaultRegion
	}
	if o.ChunkBytes <= 0 {
		o.ChunkBytes = defaultPackChunkSize
	}
	if o.CacheDir == "" {
		o.CacheDir = filepath.Join(os.TempDir(), "gitstore-cache")
	}
	if o.CacheBytes <= 0 {
		o.CacheBytes = defaultPackCacheBytes
	}
	switch {
	case o.MemoryCacheBytes == 0:
		o.MemoryCacheBytes = defaultPackMemoryBytes
	case o.MemoryCacheBytes < 0:
		o.MemoryCacheBytes = 0
	}
	switch {
	case o.IndexFreshness == 0:
		o.IndexFreshness = defaultObjectIndexFreshness
	case o.IndexFreshness < 0:
		o.IndexFreshness = 0
	}
	switch {
	case o.CompactAfterPacks == 0:
		o.CompactAfterPacks = defaultCompactAfterPacks
	case o.CompactAfterPacks < 0:
		o.CompactAfterPacks = 0
	}
	if o.MultipartBytes <= 0 {
		o.MultipartBytes = defaultMultipartThreshold
	}
	switch {
	case o.BreakerThreshold == 0:
		o.BreakerThreshold = defaultBreakerThreshold
	case o.BreakerThreshold < 0:
		o.BreakerThreshold = 0
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = defaultBreakerCooldown
	}
	return o
}
