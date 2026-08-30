package store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
	minio "github.com/minio/minio-go/v7"
)

const ObjectChecksumMetadataKey = "bleephub-sha256"

// ActionsByteStore stores opaque object bytes. The streaming forms move large
// objects without the whole value ever residing in the process heap (STORE-019).
type ActionsByteStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	// PutStream stores everything read from r; implementations must not buffer
	// the entire stream in memory.
	PutStream(ctx context.Context, key string, r io.Reader) error
	// PutStreamHashed stores exactly size bytes read from r whose SHA-256 is
	// sha256Sum, uploading directly without a re-spool or re-hash. Callers that
	// have already staged the object to a temp file (to compute its size + digest
	// for metadata) use this to avoid holding the whole object on the heap.
	PutStreamHashed(ctx context.Context, key string, r io.Reader, size int64, sha256Sum []byte) error
	// GetStream returns the object as a stream the caller must Close. The stored
	// SHA-256 cannot be checked before the first byte reaches the client, but the
	// stream recomputes it and fails the final Read (a non-EOF error) on mismatch,
	// so corruption surfaces as a truncated/failed response rather than silently
	// served bad bytes. Objects predating checksummed writes stream unverified.
	GetStream(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

type S3ActionsByteStore struct {
	Fs *gitstore.S3FS `json:"-"`
}

func NewActionsByteStoreFromEnv(ctx context.Context) (ActionsByteStore, error) {
	bucket := os.Getenv("BLEEPHUB_OBJECT_S3_BUCKET")
	if bucket == "" {
		return nil, nil
	}
	endpoint := os.Getenv("BLEEPHUB_OBJECT_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("BLEEPHUB_S3_ENDPOINT")
	}
	prefix := os.Getenv("BLEEPHUB_OBJECT_S3_PREFIX")
	if prefix == "" {
		prefix = "objects"
	}
	fs, err := gitstore.NewS3FS(ctx, endpoint, bucket, prefix)
	if err != nil {
		return nil, err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if exists, err := fs.Client().Client.BucketExists(verifyCtx, bucket); err != nil {
		return nil, fmt.Errorf("s3 head bucket %s: %w", bucket, err)
	} else if !exists {
		return nil, fmt.Errorf("s3 head bucket %s: bucket does not exist", bucket)
	}
	return &S3ActionsByteStore{Fs: fs}, nil
}

func (s *S3ActionsByteStore) Put(ctx context.Context, key string, data []byte) error {
	checksum := sha256.Sum256(data)
	return s.putObject(ctx, key, bytes.NewReader(data), int64(len(data)), checksum[:])
}

// PutStream buffers the reader to a temp file (never the heap) while hashing it,
// then uploads with the SHA-256 in metadata (STORE-019).
func (s *S3ActionsByteStore) PutStream(ctx context.Context, key string, r io.Reader) error {
	tmp, err := os.CreateTemp("", "bleephub-object-*")
	if err != nil {
		return fmt.Errorf("s3 put %s: stage upload: %w", s.Key(key), err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), r); err != nil {
		return fmt.Errorf("s3 put %s: buffer upload: %w", s.Key(key), err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("s3 put %s: rewind upload: %w", s.Key(key), err)
	}
	info, err := tmp.Stat()
	if err != nil {
		return fmt.Errorf("s3 put %s: size upload: %w", s.Key(key), err)
	}
	return s.putObject(ctx, key, tmp, info.Size(), hasher.Sum(nil))
}

// PutStreamHashed uploads r directly with a caller-computed size and checksum,
// skipping the temp-file staging PutStream does (the caller already staged it).
func (s *S3ActionsByteStore) PutStreamHashed(ctx context.Context, key string, r io.Reader, size int64, sha256Sum []byte) error {
	return s.putObject(ctx, key, r, size, sha256Sum)
}

func (s *S3ActionsByteStore) putObject(ctx context.Context, key string, body io.Reader, size int64, checksum []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := s.Fs.Client().Client.PutObject(ctx, s.Fs.Bucket(), s.Key(key), body, size, minio.PutObjectOptions{
		UserMetadata: map[string]string{
			ObjectChecksumMetadataKey: base64.RawStdEncoding.EncodeToString(checksum),
		},
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", s.Key(key), err)
	}
	return nil
}

func (s *S3ActionsByteStore) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.Fs.Client().Client.GetObject(ctx, s.Fs.Bucket(), s.Key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", s.Key(key), err)
	}
	// minio defers the request until the first read, so force it here to surface
	// a missing object (and any other failure) as an error the caller can handle
	// rather than a mid-stream body it has already begun serving.
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, fmt.Errorf("s3 get %s: %w", s.Key(key), err)
	}
	var expected []byte
	if encoded := objectUserMetadata(info)[ObjectChecksumMetadataKey]; encoded != "" {
		if want, decErr := base64.RawStdEncoding.DecodeString(encoded); decErr == nil && len(want) == sha256.Size {
			expected = want
		}
	}
	return &verifyingReadCloser{rc: obj, hasher: sha256.New(), expected: expected, key: s.Key(key)}, nil
}

// verifyingReadCloser recomputes the stored SHA-256 as the object streams and
// converts the terminating io.EOF into a checksum error when the bytes don't
// match, so a streamed read never silently serves corruption. A nil expected
// (legacy, un-checksummed object) passes the stream through unverified.
type verifyingReadCloser struct {
	rc       io.ReadCloser
	hasher   hash.Hash
	expected []byte
	key      string
	checked  bool
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 && v.expected != nil {
		v.hasher.Write(p[:n])
	}
	if err == io.EOF && v.expected != nil && !v.checked {
		v.checked = true
		if !hmac.Equal(v.expected, v.hasher.Sum(nil)) {
			return n, fmt.Errorf("s3 get %s: stored SHA-256 checksum does not match object bytes", v.key)
		}
	}
	return n, err
}

func (v *verifyingReadCloser) Close() error { return v.rc.Close() }

func (s *S3ActionsByteStore) Get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	obj, err := s.Fs.Client().Client.GetObject(ctx, s.Fs.Bucket(), s.Key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", s.Key(key), err)
	}
	defer obj.Close()
	info, err := obj.Stat()
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", s.Key(key), err)
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("s3 read %s: %w", s.Key(key), err)
	}
	if err := VerifyObjectChecksum(objectUserMetadata(info), data); err != nil {
		return nil, fmt.Errorf("s3 read %s: %w", s.Key(key), err)
	}
	return data, nil
}

// objectUserMetadata recovers the checksum this package stored as user metadata,
// keyed the way VerifyObjectChecksum expects. StatObject/GetObject return user
// metadata with the "X-Amz-Meta-" prefix stripped (UserMetadata) and also in the
// raw response headers (Metadata); read whichever the endpoint populated.
func objectUserMetadata(info minio.ObjectInfo) map[string]string {
	if info.Metadata != nil {
		if v := info.Metadata.Get("X-Amz-Meta-" + ObjectChecksumMetadataKey); v != "" {
			return map[string]string{ObjectChecksumMetadataKey: v}
		}
	}
	for name, value := range info.UserMetadata {
		if strings.EqualFold(name, ObjectChecksumMetadataKey) {
			return map[string]string{ObjectChecksumMetadataKey: value}
		}
	}
	return nil
}

func VerifyObjectChecksum(metadata map[string]string, data []byte) error {
	encoded := metadata[ObjectChecksumMetadataKey]
	if encoded == "" {
		// Objects predating checksummed writes have no checksum; accept them.
		return nil
	}
	want, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("invalid stored SHA-256 checksum")
	}
	got := sha256.Sum256(data)
	if !hmac.Equal(want, got[:]) {
		return fmt.Errorf("stored SHA-256 checksum does not match object bytes")
	}
	return nil
}

func (s *S3ActionsByteStore) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := s.Fs.Client().Client.RemoveObject(ctx, s.Fs.Bucket(), s.Key(key), minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", s.Key(key), err)
	}
	return nil
}

// StageUpload spools everything read from r to a temp file while computing its
// size and SHA-256, then rewinds it. The caller owns the returned file and must
// Close and os.Remove it; uploading it via ActionsByteStore.PutStreamHashed keeps
// the whole object off the heap and avoids a second hash pass. This is how a
// handler turns a (size-capped) request body into a digest + size for metadata
// without buffering the object in memory.
func StageUpload(r io.Reader) (f *os.File, size int64, sum []byte, err error) {
	tmp, err := os.CreateTemp("", "bleephub-upload-*")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("stage upload: %w", err)
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, nil, fmt.Errorf("stage upload: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, nil, fmt.Errorf("stage upload: %w", err)
	}
	return tmp, n, hasher.Sum(nil), nil
}

func (s *S3ActionsByteStore) Key(key string) string {
	return path.Join(s.Fs.Prefix(), strings.TrimPrefix(key, "/"))
}

func ArtifactDataKey(id int64) string {
	return fmt.Sprintf("actions/artifacts/%d/data", id)
}

func CacheDataKey(id int64) string {
	return fmt.Sprintf("actions/caches/%d/data", id)
}

func LogDataKey(id int) string {
	return fmt.Sprintf("actions/logs/%d/data", id)
}

func ReleaseAssetDataKey(id int) string {
	return fmt.Sprintf("releases/assets/%d/data", id)
}

func PackageFileDataKey(fileID int) string {
	return fmt.Sprintf("packages/files/%d/data", fileID)
}

func PackageRegistryBlobDataKey(digest string) string {
	algo, hexPart, ok := strings.Cut(digest, ":")
	if !ok {
		return path.Join("packages/registry/blobs", digest)
	}
	return path.Join("packages/registry/blobs", algo, hexPart)
}

// LFSObjectDataKey names the bytes of one Git LFS object. The key is
// content-addressed on the oid (a bare SHA-256), so repositories sharing an
// object share one stored copy. The two-level fan-out mirrors git-lfs's on-disk
// layout and keeps any listing prefix small.
func LFSObjectDataKey(oid string) string {
	oid = strings.ToLower(oid)
	if len(oid) < 4 {
		return path.Join("lfs/objects", oid)
	}
	return path.Join("lfs/objects", oid[:2], oid[2:4], oid)
}

func CodeQLDatabaseDataKey(id int, content []byte) string {
	digest := sha256.Sum256(content)
	return fmt.Sprintf("code-scanning/codeql/databases/%d/%x.zip", id, digest)
}

func CodeQLVariantAnalysisQueryPackDataKey(id int) string {
	return fmt.Sprintf("code-scanning/codeql/variant-analyses/%d/query-pack.tar.gz", id)
}

func AttestationBundleDataKey(id int) string {
	return fmt.Sprintf("attestations/%d/bundle.json", id)
}
