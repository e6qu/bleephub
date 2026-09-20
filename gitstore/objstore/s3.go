package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Options configure the S3 driver. The zero value targets AWS S3 in us-east-1
// with credentials from the environment.
type S3Options struct {
	// Endpoint is an S3-compatible endpoint URL, addressed path-style. Empty
	// targets AWS S3 itself, addressed by virtual host.
	Endpoint string
	// Region is the region to sign for. Empty selects us-east-1, which is also
	// what S3-compatible servers expect.
	Region string
	// Credentials signs requests. Nil selects the AWS environment chain.
	Credentials *credentials.Credentials
	// Transport carries every request. Nil selects the client's default.
	Transport http.RoundTripper
	// PartBytes is the part size of an upload too large for one request. Zero
	// selects 16 MiB.
	PartBytes uint64
}

const (
	defaultS3Region    = "us-east-1"
	defaultS3PartBytes = 16 << 20
	// s3DeleteBatch is the most keys one multi-object delete may name.
	s3DeleteBatch = 1000
)

// s3Bucket drives every store that speaks the S3 protocol and honours its
// conditional writes: S3, R2, MinIO, SeaweedFS, Ceph, Tigris. It must not be
// pointed at Google Cloud Storage's S3-compatible endpoint, which accepts a
// conditional PUT and ignores the condition.
type s3Bucket struct {
	client    *minio.Client
	bucket    string
	partBytes uint64
}

// NewS3 opens a bucket on an S3-compatible store.
func NewS3(bucket string, opts S3Options) (Bucket, error) {
	region := opts.Region
	if region == "" {
		region = defaultS3Region
	}
	// minio-go takes host[:port] and a separate Secure flag rather than a URL.
	host := fmt.Sprintf("s3.%s.amazonaws.com", region)
	secure := true
	lookup := minio.BucketLookupAuto
	if opts.Endpoint != "" {
		host = opts.Endpoint
		if strings.Contains(opts.Endpoint, "://") {
			parsed, err := url.Parse(opts.Endpoint)
			if err != nil {
				return nil, fmt.Errorf("s3 endpoint %q: %w", opts.Endpoint, err)
			}
			secure = parsed.Scheme == "https"
			host = parsed.Host
		}
		lookup = minio.BucketLookupPath
	}
	creds := opts.Credentials
	if creds == nil {
		creds = credentials.NewEnvAWS()
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Region:       region,
		Transport:    opts.Transport,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return NewS3WithClient(client, bucket, opts.PartBytes), nil
}

// NewS3WithClient opens a bucket through a client the caller configured.
func NewS3WithClient(client *minio.Client, bucket string, partBytes uint64) Bucket {
	if partBytes == 0 {
		partBytes = defaultS3PartBytes
	}
	return &s3Bucket{client: client, bucket: bucket, partBytes: partBytes}
}

func (b *s3Bucket) Name() string { return b.bucket }

func (b *s3Bucket) Capabilities() Capabilities {
	return Capabilities{ConditionalWrites: true, Presign: true}
}

func (b *s3Bucket) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	return b.get(ctx, key, minio.GetObjectOptions{})
}

func (b *s3Bucket) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, Info, error) {
	if offset < 0 || length <= 0 {
		return nil, Info{}, fmt.Errorf("s3 get %s: invalid range %d+%d", key, offset, length)
	}
	var opts minio.GetObjectOptions
	if err := opts.SetRange(offset, offset+length-1); err != nil {
		return nil, Info{}, fmt.Errorf("s3 get %s: %w", key, err)
	}
	return b.get(ctx, key, opts)
}

// get goes through the Core API: its one call is one request, and the response
// headers — the size of the whole object, its version — come back with the body
// rather than from a second request.
func (b *s3Bucket) get(ctx context.Context, key string, opts minio.GetObjectOptions) (io.ReadCloser, Info, error) {
	core := minio.Core{Client: b.client}
	body, info, header, err := core.GetObject(ctx, b.bucket, key, opts)
	if err != nil {
		return nil, Info{}, b.translate("get", key, err)
	}
	described := b.info(key, info)
	if total, ok := totalFromContentRange(header.Get("Content-Range")); ok {
		described.Size = total
	}
	return body, described, nil
}

func (b *s3Bucket) Head(ctx context.Context, key string) (Info, error) {
	info, err := b.client.StatObject(ctx, b.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return Info{}, b.translate("head", key, err)
	}
	return b.info(key, info), nil
}

func (b *s3Bucket) Put(ctx context.Context, key string, body io.Reader, size int64, condition Condition) (Version, error) {
	opts := minio.PutObjectOptions{PartSize: b.partBytes}
	switch {
	case condition.Absent():
		opts.SetMatchETagExcept("*")
	case condition.Version() != "":
		opts.SetMatchETag(string(condition.Version()))
	}
	uploaded, err := b.client.PutObject(ctx, b.bucket, key, body, size, opts)
	if err != nil {
		return "", b.translate("put", key, err)
	}
	return versionOf(uploaded.ETag), nil
}

func (b *s3Bucket) Delete(ctx context.Context, key string) error {
	if err := b.client.RemoveObject(ctx, b.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return b.translate("delete", key, err)
	}
	return nil
}

func (b *s3Bucket) DeleteMany(ctx context.Context, keys []string) error {
	for start := 0; start < len(keys); start += s3DeleteBatch {
		batch := keys[start:min(start+s3DeleteBatch, len(keys))]
		objects := make(chan minio.ObjectInfo, len(batch))
		for _, key := range batch {
			objects <- minio.ObjectInfo{Key: key}
		}
		close(objects)
		var failures []error
		for result := range b.client.RemoveObjects(ctx, b.bucket, objects, minio.RemoveObjectsOptions{}) {
			if result.Err != nil {
				failures = append(failures, fmt.Errorf("s3 delete %s: %w", result.ObjectName, result.Err))
			}
		}
		if len(failures) > 0 {
			return errors.Join(failures...)
		}
	}
	return nil
}

func (b *s3Bucket) List(ctx context.Context, prefix, delimiter string, visit func(Entry) error) error {
	// minio-go knows one delimiter. It is the only one a git layout needs.
	if delimiter != "" && delimiter != "/" {
		return fmt.Errorf("s3 list %s: %w: delimiter %q", prefix, ErrUnsupported, delimiter)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listing := b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: delimiter == ""})
	for object := range listing {
		if object.Err != nil {
			return b.translate("list", prefix, object.Err)
		}
		entry := Entry{Info: b.info(object.Key, object)}
		if delimiter != "" && strings.HasSuffix(object.Key, delimiter) {
			entry = Entry{Info: Info{Key: object.Key}, Prefix: true}
		}
		if err := visit(entry); err != nil {
			return err
		}
	}
	return nil
}

func (b *s3Bucket) Copy(ctx context.Context, sourceKey, destinationKey string) error {
	_, err := b.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: b.bucket, Object: destinationKey},
		minio.CopySrcOptions{Bucket: b.bucket, Object: sourceKey})
	if err != nil {
		return b.translate("copy", sourceKey, err)
	}
	return nil
}

func (b *s3Bucket) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	signed, err := b.client.PresignedGetObject(ctx, b.bucket, key, expiry, url.Values{})
	if err != nil {
		return "", b.translate("presign", key, err)
	}
	return signed.String(), nil
}

func (b *s3Bucket) info(key string, info minio.ObjectInfo) Info {
	return Info{Key: key, Size: info.Size, Version: versionOf(info.ETag), ModTime: info.LastModified}
}

// versionOf takes the quotes off an ETag, which some responses carry and some
// do not, so that one object's version compares equal wherever it was learned.
func versionOf(etag string) Version { return Version(strings.Trim(etag, `"`)) }

// translate turns the store's answer into this package's errors, leaving the
// rest wrapped with what was being done.
func (b *s3Bucket) translate(operation, key string, err error) error {
	response := minio.ToErrorResponse(err)
	switch {
	case response.StatusCode == http.StatusNotFound, response.Code == "NoSuchKey":
		return fmt.Errorf("s3 %s %s: %w", operation, key, ErrNotFound)
	case response.StatusCode == http.StatusPreconditionFailed, response.Code == "PreconditionFailed",
		// Two writers racing to create one key: S3 answers the loser 409.
		response.Code == "ConditionalRequestConflict":
		return fmt.Errorf("s3 %s %s: %w", operation, key, ErrConditionNotMet)
	case response.StatusCode == http.StatusRequestedRangeNotSatisfiable, response.Code == "InvalidRange":
		return fmt.Errorf("s3 %s %s: %w", operation, key, ErrRangeNotSatisfiable)
	}
	return fmt.Errorf("s3 %s %s: %w", operation, key, err)
}

// totalFromContentRange reads the object's whole size out of a ranged response's
// "bytes first-last/total".
func totalFromContentRange(header string) (int64, bool) {
	_, total, found := strings.Cut(header, "/")
	if !found || total == "*" {
		return 0, false
	}
	var size int64
	if _, err := fmt.Sscanf(total, "%d", &size); err != nil {
		return 0, false
	}
	return size, true
}
