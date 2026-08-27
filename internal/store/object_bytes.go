package store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/e6qu/bleephub/internal/gitstore"
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
	// GetStream returns the object as a stream the caller must Close. It does
	// not verify the stored checksum: a mid-stream body cannot be rejected once
	// its first byte reaches the client.
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
	if _, err := fs.Client().HeadBucket(verifyCtx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return nil, fmt.Errorf("s3 head bucket %s: %w", bucket, err)
	}
	return &S3ActionsByteStore{Fs: fs}, nil
}

func (s *S3ActionsByteStore) Put(ctx context.Context, key string, data []byte) error {
	checksum := sha256.Sum256(data)
	return s.putObject(ctx, key, bytes.NewReader(data), checksum[:])
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
	return s.putObject(ctx, key, tmp, hasher.Sum(nil))
}

func (s *S3ActionsByteStore) putObject(ctx context.Context, key string, body io.ReadSeeker, checksum []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := s.Fs.Client().PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Fs.Bucket()),
		Key:    aws.String(s.Key(key)),
		Body:   body,
		Metadata: map[string]string{
			ObjectChecksumMetadataKey: base64.RawStdEncoding.EncodeToString(checksum),
		},
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", s.Key(key), err)
	}
	return nil
}

func (s *S3ActionsByteStore) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.Fs.Client().GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Fs.Bucket()),
		Key:    aws.String(s.Key(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", s.Key(key), err)
	}
	return resp.Body, nil
}

func (s *S3ActionsByteStore) Get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := s.Fs.Client().GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Fs.Bucket()),
		Key:    aws.String(s.Key(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", s.Key(key), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("s3 read %s: %w", s.Key(key), err)
	}
	if err := VerifyObjectChecksum(resp.Metadata, data); err != nil {
		return nil, fmt.Errorf("s3 read %s: %w", s.Key(key), err)
	}
	return data, nil
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
	_, err := s.Fs.Client().DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.Fs.Bucket()),
		Key:    aws.String(s.Key(key)),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", s.Key(key), err)
	}
	return nil
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
