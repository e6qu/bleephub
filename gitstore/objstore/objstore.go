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
// or a COPY can be conditional, or that a listing comes in any particular order
// (Amazon's directory buckets and SeaweedFS's filer list in their own).
//
// Every driver implements every operation with its whole meaning. There is no
// capability to ask about and no lesser behaviour to settle for: a store that
// cannot hold a conditional write, or sign a URL, is a store gitstore does not
// run on, and Conform says so at startup rather than letting a replica find out
// by losing a reference. Which driver a deployment uses is something its
// operator states; nothing here guesses it from an endpoint.
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	// ErrNotFound reports that no object has the key.
	ErrNotFound = errors.New("object not found")
	// ErrConditionNotMet reports that a conditional write lost: the object
	// existed when it was to be absent, or was not at the version named.
	ErrConditionNotMet = errors.New("object store condition not met")
	// ErrNotModified reports that a conditional read found the object still at
	// the version the reader holds, and so sent no body. It is an answer, not a
	// failure: the reader's copy is the store's.
	ErrNotModified = errors.New("object not modified")
	// ErrRangeNotSatisfiable reports a ranged read that starts past the end.
	ErrRangeNotSatisfiable = errors.New("range starts beyond the end of the object")
)

// Version identifies one state of one object. It is opaque: equal versions of
// one key mean equal contents, and nothing else may be read into it — it is an
// ETag on some stores and a generation number on others, and even where it looks
// like a hash of the content it is not one that can be trusted as such.
type Version string

// Metadata is a few short facts a writer keeps with an object and gets back
// with every read of it — a digest of the content, say, held beside the bytes
// rather than inside them, so that the object stays exactly its content.
//
// Names are lower-case ASCII letters and digits, starting with a letter. That is
// the intersection of what the stores allow: S3 carries a name as an HTTP header
// and folds its case, and Azure requires an identifier, which rules out the
// hyphen. Values are printable ASCII.
type Metadata map[string]string

// Validate reports the first name or value a store would refuse or alter.
func (m Metadata) Validate() error {
	for name, value := range m {
		if !validMetadataName(name) {
			return fmt.Errorf("object metadata name %q: want lower-case letters and digits, starting with a letter", name)
		}
		for _, r := range value {
			if r < ' ' || r > '~' {
				return fmt.Errorf("object metadata %s: value is not printable ASCII", name)
			}
		}
	}
	return nil
}

func validMetadataName(name string) bool {
	for i, r := range name {
		letter := r >= 'a' && r <= 'z'
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return name != ""
}

// Info describes an object.
type Info struct {
	Key     string
	Size    int64
	Version Version
	ModTime time.Time
	// Metadata is what the object was written with. A listing does not carry
	// it; a read and a Head do.
	Metadata Metadata
}

// Entry is one result of a listing: an object, or — from ListDirectory — a
// common prefix standing for everything below it.
type Entry struct {
	Info
	// Prefix marks a common prefix; Key then ends in "/" and the other fields
	// are zero.
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

// Bucket is one bucket or container, addressed by whole keys.
type Bucket interface {
	// Get reads a whole object.
	Get(ctx context.Context, key string) (io.ReadCloser, Info, error)
	// GetIfChanged reads a whole object unless it is still at the version held,
	// in which case it answers ErrNotModified and moves no body: what a reader
	// that keeps a copy of a small object pays to learn its copy is current. An
	// object that is not there is ErrNotFound, whatever version was held. The
	// version held must be one a read or a write of this key returned.
	GetIfChanged(ctx context.Context, key string, held Version) (io.ReadCloser, Info, error)
	// GetRange reads length bytes from offset; fewer if the object ends first.
	// The Info it returns carries the object's whole size.
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, Info, error)
	// Head describes an object without reading it.
	Head(ctx context.Context, key string) (Info, error)
	// Put writes an object atomically, with the metadata given. A size below
	// zero means unknown, and the body is then uploaded in parts. Not every
	// store can complete such an upload conditionally — Azure can, Google Cloud
	// Storage holds a resumable upload to its precondition though its
	// documentation does not promise to, and S3 cannot — so a caller that needs
	// the condition to hold states the size.
	Put(ctx context.Context, key string, body io.Reader, size int64, condition Condition, metadata Metadata) (Version, error)
	// Delete removes an object. Removing one that is not there is not an error.
	Delete(ctx context.Context, key string) error
	// DeleteMany removes every key given, in as few requests as the store allows.
	DeleteMany(ctx context.Context, keys []string) error
	// List calls visit once for every object under prefix, in no particular
	// order.
	List(ctx context.Context, prefix string, visit func(Entry) error) error
	// ListDirectory calls visit for what is immediately under prefix: objects
	// whose key has no "/" after it, and one common prefix for each run of keys
	// that do, in no particular order.
	ListDirectory(ctx context.Context, prefix string, visit func(Entry) error) error
	// Copy copies an object within the bucket without moving its bytes through
	// the caller.
	Copy(ctx context.Context, sourceKey, destinationKey string) error
	// PresignGet returns a URL that reads the object, without credentials, for
	// the given time.
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
	// Name identifies the bucket for logs and cache keys.
	Name() string
}
