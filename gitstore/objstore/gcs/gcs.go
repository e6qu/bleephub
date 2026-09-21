// Package gcs drives Google Cloud Storage through its native API. Google offers
// an S3-compatible endpoint as well, and the S3 driver must not be pointed at
// it: that endpoint answers a conditional PUT with 200 and overwrites, so the
// one guarantee a git server with more than one replica is built on would
// silently not hold. An object's generation is its version.
//
// The client is gcsclient, this repository's own, and not Google's: Google's
// brings gRPC, xDS and OpenTelemetry with it, some hundreds of packages, for
// the nine operations used here.
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	storage "github.com/e6qu/bleephub/gcsclient"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Options configure the driver. Endpoint and CredentialsJSON have no default:
// which service a deployment writes to, and as whom, is its operator's to state.
type Options struct {
	// Endpoint is the scheme and host of the service:
	// https://storage.googleapis.com for Cloud Storage itself, or an emulator's
	// URL.
	Endpoint string
	// CredentialsJSON is a service-account key file's bytes. It is the one way to
	// authenticate, because it is the one Google credential that holds a private
	// key: the key is exchanged for access tokens, and it signs a URL locally.
	// Workload identity, the metadata server and Application Default Credentials
	// hold no key, could sign a URL only by asking the IAM API to, and are not
	// looked for.
	CredentialsJSON []byte
	// Transport carries every request. Nil selects http.DefaultTransport.
	Transport http.RoundTripper
	// ChunkBytes is how much of an upload too large for one request goes in each,
	// which is also how much of one is held in memory and the size above which an
	// upload goes in chunks at all. Zero selects 16 MiB; the service requires a
	// multiple of 256 KiB.
	ChunkBytes int64
}

// bucket is one bucket, reached as one service account.
type bucket struct {
	client *storage.Client
	name   string
	// exists records that the bucket has been asked after and was there. See
	// absent.
	exists atomic.Bool
}

// New opens a bucket, which must already exist: a missing bucket is a
// deployment's mistake to be told about, not an object that is not found. It
// sends nothing; the first request finds out whether the key is one the service
// knows.
func New(bucketName string, opts Options) (objstore.Bucket, error) {
	if bucketName == "" {
		return nil, errors.New("gcs: no bucket named")
	}
	client, err := storage.New(storage.Options{
		Endpoint:        opts.Endpoint,
		CredentialsJSON: opts.CredentialsJSON,
		Transport:       opts.Transport,
		ChunkBytes:      opts.ChunkBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("gcs: %w", err)
	}
	return &bucket{client: client, name: bucketName}, nil
}

func (b *bucket) Name() string { return b.name }

func (b *bucket) Get(ctx context.Context, key string) (io.ReadCloser, objstore.Info, error) {
	body, object, err := b.client.Get(ctx, b.name, key)
	if err != nil {
		return nil, objstore.Info{}, b.translate(ctx, key, err)
	}
	return body, describe(object), nil
}

func (b *bucket) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, objstore.Info, error) {
	body, object, err := b.client.GetRange(ctx, b.name, key, offset, length)
	if err != nil {
		return nil, objstore.Info{}, b.translate(ctx, key, err)
	}
	// The interface says a range that starts at or past the end is not
	// satisfiable, and a reader walking a pack to its end depends on being told
	// so. The service says it with 416, translated above. An emulator may answer
	// a range that starts exactly at the end with success and no bytes — Azure's
	// was found to — and the response carries the object's whole size either way,
	// so the rule is held here by that fact rather than by which of them answered.
	if offset >= object.Size {
		_ = body.Close()
		return nil, objstore.Info{}, fmt.Errorf("gcs get %s: range starts at %d of %d bytes: %w", key, offset, object.Size, objstore.ErrRangeNotSatisfiable)
	}
	return body, describe(object), nil
}

func (b *bucket) Head(ctx context.Context, key string) (objstore.Info, error) {
	object, err := b.client.Stat(ctx, b.name, key)
	if err != nil {
		return objstore.Info{}, b.translate(ctx, key, err)
	}
	return describe(object), nil
}

// Put writes an object, which appears whole or not at all whichever way the
// client sends it: in one request where the size is known and fits a chunk, and
// otherwise as a resumable upload, of which only a completed one is an object.
// The condition goes with either. For an upload in chunks it is stated when the
// upload is begun and held to when the last chunk makes the object — which the
// fake and the emulator do and Google's own clients rely on, though Google's
// documentation of resumable uploads does not say so in as many words.
func (b *bucket) Put(ctx context.Context, key string, body io.Reader, size int64, condition objstore.Condition, metadata objstore.Metadata) (objstore.Version, error) {
	if err := metadata.Validate(); err != nil {
		return "", fmt.Errorf("gcs put %s: %w", key, err)
	}
	precondition, err := preconditionOf(condition)
	if err != nil {
		return "", fmt.Errorf("gcs put %s: %w", key, err)
	}
	if size < 0 {
		size = storage.SizeUnknown
	}
	written, err := b.client.Insert(ctx, b.name, key, body, storage.InsertOptions{
		Size:         size,
		Precondition: precondition,
		Metadata:     metadata,
	})
	// A write names only the bucket, so a 404 to one is the bucket that is missing,
	// and is left as the service's own error.
	if errors.Is(err, storage.ErrPreconditionFailed) {
		return "", fmt.Errorf("%w: %w", objstore.ErrConditionNotMet, err)
	}
	if err != nil {
		return "", err
	}
	return versionOf(written.Generation), nil
}

func (b *bucket) Delete(ctx context.Context, key string) error {
	err := b.client.Delete(ctx, b.name, key)
	if err == nil {
		return nil
	}
	// Removing what is not there is not an error; removing from a bucket that is
	// not there is.
	if translated := b.translate(ctx, key, err); !errors.Is(translated, objstore.ErrNotFound) {
		return translated
	}
	return nil
}

func (b *bucket) DeleteMany(ctx context.Context, keys []string) error {
	outcomes, err := b.client.DeleteBatch(ctx, b.name, keys)
	if err != nil {
		return fmt.Errorf("gcs delete: %w", err)
	}
	var failures []error
	for i, outcome := range outcomes {
		if outcome == nil {
			continue
		}
		if translated := b.translate(ctx, keys[i], outcome); !errors.Is(translated, objstore.ErrNotFound) {
			failures = append(failures, translated)
		}
	}
	return errors.Join(failures...)
}

func (b *bucket) List(ctx context.Context, prefix string, visit func(objstore.Entry) error) error {
	return b.list(ctx, storage.ListOptions{Prefix: prefix}, visit)
}

func (b *bucket) ListDirectory(ctx context.Context, prefix string, visit func(objstore.Entry) error) error {
	return b.list(ctx, storage.ListOptions{Prefix: prefix, Delimiter: "/"}, visit)
}

// list hands a listing over as the client gives it, which is in byte order with
// the folded prefixes among the objects: the service sends the two apart, and
// the client merges them. A listing names only the bucket, so a 404 to one is
// the bucket that is missing, and is left as the service's own error.
func (b *bucket) list(ctx context.Context, options storage.ListOptions, visit func(objstore.Entry) error) error {
	return b.client.List(ctx, b.name, options, func(entry storage.Entry) error {
		if entry.Prefix {
			return visit(objstore.Entry{Info: objstore.Info{Key: entry.Name}, Prefix: true})
		}
		return visit(objstore.Entry{Info: describe(entry.Object)})
	})
}

// Copy uses the client's Copy, which is objects.rewrite followed until the
// service says it is done: the one copy with no limit on size, and a fork
// copies packs of many GiB.
func (b *bucket) Copy(ctx context.Context, sourceKey, destinationKey string) error {
	if _, err := b.client.Copy(ctx, b.name, sourceKey, b.name, destinationKey); err != nil {
		return b.translate(ctx, sourceKey, err)
	}
	return nil
}

// PresignGet signs a V4 URL with the service account's key, locally. The
// service signs for no less than a second and no more than seven days.
func (b *bucket) PresignGet(_ context.Context, key string, expiry time.Duration) (string, error) {
	signed, err := b.client.SignedGetURL(b.name, key, expiry)
	if err != nil {
		return "", fmt.Errorf("gcs presign %s: %w", key, err)
	}
	return signed, nil
}

// preconditionOf states a write's condition as the generation the service
// arbitrates on. A version that is not a generation is one this store never
// gave out, so no object is at it and the condition is lost without asking —
// and it must not be asked, because to the service a generation of zero means
// "absent", which is another condition altogether.
func preconditionOf(condition objstore.Condition) (storage.Precondition, error) {
	switch {
	case condition.Absent():
		return storage.DoesNotExist(), nil
	case condition.Version() != "":
		generation, err := strconv.ParseInt(string(condition.Version()), 10, 64)
		if err != nil || generation <= 0 {
			return storage.Precondition{}, fmt.Errorf("version %q is not a generation, and no object is at it: %w", condition.Version(), objstore.ErrConditionNotMet)
		}
		return storage.GenerationMatch(generation), nil
	}
	return storage.Precondition{}, nil
}

// versionOf writes a generation as a version: the number in decimal, which is
// how the service itself writes it wherever it appears.
func versionOf(generation int64) objstore.Version {
	return objstore.Version(strconv.FormatInt(generation, 10))
}

func describe(object storage.Object) objstore.Info {
	info := objstore.Info{Key: object.Name, Size: object.Size, Version: versionOf(object.Generation), ModTime: object.Updated}
	if len(object.Metadata) > 0 {
		info.Metadata = objstore.Metadata(object.Metadata)
	}
	return info
}

// translate turns the answer to a request that names an object into objstore's
// errors, leaving the rest as the client gave it, which says what was being
// done and to what.
func (b *bucket) translate(ctx context.Context, key string, err error) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return b.absent(ctx, key, err)
	case errors.Is(err, storage.ErrRangeNotSatisfiable):
		return fmt.Errorf("%w: %w", objstore.ErrRangeNotSatisfiable, err)
	}
	return err
}

// errBucketExists stops the listing that absent asks for at its first entry:
// that there is a listing at all is what was asked.
var errBucketExists = errors.New("the bucket exists")

// absent decides what a 404 to a request that names an object means. The JSON
// API answers it alike whether it is the object or its bucket that is missing —
// both are "notFound" — and the difference matters: reported as ErrNotFound, a
// mistyped bucket name would read as an empty store, every repository absent
// and the first push creating it anew. So a 404 is an object's absence only in
// a bucket known to exist, and the first 404 is what makes the driver find out:
// it asks for a listing, which names the bucket alone. That is one request,
// once, because the answer is kept — a bucket deleted under a running server is
// not what this guards against.
func (b *bucket) absent(ctx context.Context, key string, notFound error) error {
	if !b.exists.Load() {
		err := b.client.List(ctx, b.name, storage.ListOptions{Prefix: key}, func(storage.Entry) error { return errBucketExists })
		if err != nil && !errors.Is(err, errBucketExists) {
			return fmt.Errorf("gcs: %s was not found, and asking whether bucket %s exists: %w", key, b.name, err)
		}
		b.exists.Store(true)
	}
	return fmt.Errorf("%w: %w", objstore.ErrNotFound, notFound)
}
