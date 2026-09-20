// Package objstore is the object store as gitstore uses it: the operations that
// S3, Google Cloud Storage, Azure Blob Storage and the S3-compatible stores all
// offer with the same meaning, and nothing else.
//
// It exists because the S3 wire protocol is not that common interface. Google
// Cloud Storage's S3-compatible endpoint ignores a conditional PUT — it answers
// 200 and overwrites — and Azure has no S3 endpoint at all, so a storage layer
// written against one protocol is correct on some stores and silently wrong on
// others. Each store gets a driver that speaks its native API, and what they
// share is stated here. See docs/git_storage_research.md.
//
// What may be relied on: a PUT is atomic (the object is wholly there or not at
// all), a read after a write sees it, a listing after a write includes it, and a
// conditional write holds. What may not: that a version token is a hash of the
// content, that a multipart upload can be completed conditionally, that a DELETE
// or a COPY can be conditional, or that a listing is ordered beyond byte order of
// the keys.
package objstore

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotFound reports that no object has the key.
	ErrNotFound = errors.New("object not found")
	// ErrConditionNotMet reports that a conditional write lost: the object
	// existed when it was to be absent, or was not at the version named.
	ErrConditionNotMet = errors.New("object store condition not met")
	// ErrRangeNotSatisfiable reports a ranged read that starts past the end.
	ErrRangeNotSatisfiable = errors.New("range starts beyond the end of the object")
	// ErrUnsupported reports an operation the store does not offer. Callers
	// consult Capabilities rather than probe for it.
	ErrUnsupported = errors.New("object store does not support this operation")
)

// Version identifies one state of one object. It is opaque: equal versions of
// one key mean equal contents, and nothing else may be read into it — it is an
// ETag on some stores and a generation number on others, and even where it looks
// like a hash of the content it is not one that can be trusted as such.
type Version string

// Info describes an object.
type Info struct {
	Key     string
	Size    int64
	Version Version
	ModTime time.Time
}

// Entry is one result of a listing: an object, or — in a delimited listing — a
// common prefix standing for everything below it.
type Entry struct {
	Info
	// Prefix marks a common prefix; Key then ends in the delimiter and the
	// other fields are zero.
	Prefix bool
}

// Condition restricts a write to the state the writer expects.
type Condition struct {
	absent  bool
	version Version
}

// Always places no condition on a write.
var Always = Condition{}

// IfAbsent lets a write through only if no object has the key.
func IfAbsent() Condition { return Condition{absent: true} }

// IfVersion lets a write through only if the object is at the version given.
func IfVersion(version Version) Condition { return Condition{version: version} }

// Absent reports whether the condition is IfAbsent.
func (c Condition) Absent() bool { return c.absent }

// Version returns the version an IfVersion condition names, or "".
func (c Condition) Version() Version { return c.version }

// Conditional reports whether the condition restricts the write at all.
func (c Condition) Conditional() bool { return c.absent || c.version != "" }

// Capabilities says what a store offers beyond the required operations, so a
// caller chooses a strategy up front instead of discovering a refusal mid-write.
type Capabilities struct {
	// ConditionalWrites reports that Put honours its Condition. A store
	// without it cannot arbitrate between replicas, and a caller must use a
	// lock held elsewhere.
	ConditionalWrites bool
	// Presign reports that PresignGet returns a URL a client can fetch without
	// credentials.
	Presign bool
}

// Bucket is one bucket or container, addressed by whole keys.
type Bucket interface {
	// Get reads a whole object.
	Get(ctx context.Context, key string) (io.ReadCloser, Info, error)
	// GetRange reads length bytes from offset; fewer if the object ends first.
	// The Info it returns carries the object's whole size.
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, Info, error)
	// Head describes an object without reading it.
	Head(ctx context.Context, key string) (Info, error)
	// Put writes an object atomically. A size below zero means unknown, and the
	// body is then uploaded in parts, which no store lets be conditional.
	Put(ctx context.Context, key string, body io.Reader, size int64, condition Condition) (Version, error)
	// Delete removes an object. Removing one that is not there is not an error.
	Delete(ctx context.Context, key string) error
	// DeleteMany removes every key given, in as few requests as the store allows.
	DeleteMany(ctx context.Context, keys []string) error
	// List calls visit for everything under prefix, in byte order of the keys.
	// With a delimiter, keys containing it after the prefix are folded into
	// common prefixes; with none, the listing is recursive.
	List(ctx context.Context, prefix, delimiter string, visit func(Entry) error) error
	// Copy copies an object within the bucket without moving its bytes through
	// the caller.
	Copy(ctx context.Context, sourceKey, destinationKey string) error
	// PresignGet returns a URL that reads the object for the given time.
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
	// Capabilities reports what this store offers.
	Capabilities() Capabilities
	// Name identifies the bucket for logs and cache keys.
	Name() string
}
