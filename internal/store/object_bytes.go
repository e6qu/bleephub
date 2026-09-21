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

	"github.com/e6qu/bleephub/gitstore"
	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/internal/gitbackend"
)

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
	// served bad bytes.
	GetStream(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// ObjectStoreByteStore keeps object bytes under one prefix of an object-store
// bucket. A stored object is exactly its content, and the SHA-256 of that content
// is kept beside it as object metadata, so that a read can tell bytes the store
// corrupted from bytes that were written. The digest is not inside the object
// because an object that is its content can one day be handed to a client by
// URL, as release assets and LFS objects are elsewhere; and it is not the
// store's version token because that is not a content hash that can be trusted
// (see gitstore/objstore). An object with no digest beside it is not one this
// store wrote, and reading it is an error.
type ObjectStoreByteStore struct {
	Objects *gitstore.Store `json:"-"`
}

// ObjectChecksumMetadataName names the metadata holding an object's SHA-256, in
// unpadded base64. Its spelling is the one every store keeps as written: see
// objstore.Metadata.
const ObjectChecksumMetadataName = "bleephubsha256"

// NewActionsByteStoreFromEnv returns the byte store the environment configures,
// or nil when the service's bytes are not kept in an object store. Which store,
// and how it is reached, is gitbackend's to read: the byte store and the git
// store are two prefixes of one deployment's object store.
func NewActionsByteStoreFromEnv(ctx context.Context) (ActionsByteStore, error) {
	objects, err := gitbackend.OpenByteStore(ctx)
	if err != nil {
		return nil, err
	}
	if objects == nil {
		return nil, nil
	}
	return &ObjectStoreByteStore{Objects: objects}, nil
}

func (s *ObjectStoreByteStore) Put(ctx context.Context, key string, data []byte) error {
	checksum := sha256.Sum256(data)
	return s.putObject(ctx, key, bytes.NewReader(data), int64(len(data)), checksum[:])
}

// PutStream buffers the reader to a temp file (never the heap) while hashing it,
// then uploads it behind its SHA-256 (STORE-019).
func (s *ObjectStoreByteStore) PutStream(ctx context.Context, key string, r io.Reader) error {
	tmp, err := os.CreateTemp("", "bleephub-object-*")
	if err != nil {
		return fmt.Errorf("object store put %s: stage upload: %w", s.Key(key), err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), r); err != nil {
		return fmt.Errorf("object store put %s: buffer upload: %w", s.Key(key), err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("object store put %s: rewind upload: %w", s.Key(key), err)
	}
	info, err := tmp.Stat()
	if err != nil {
		return fmt.Errorf("object store put %s: size upload: %w", s.Key(key), err)
	}
	return s.putObject(ctx, key, tmp, info.Size(), hasher.Sum(nil))
}

// PutStreamHashed uploads r directly with a caller-computed size and checksum,
// skipping the temp-file staging PutStream does (the caller already staged it).
func (s *ObjectStoreByteStore) PutStreamHashed(ctx context.Context, key string, r io.Reader, size int64, sha256Sum []byte) error {
	return s.putObject(ctx, key, r, size, sha256Sum)
}

func (s *ObjectStoreByteStore) putObject(ctx context.Context, key string, body io.Reader, size int64, checksum []byte) error {
	if len(checksum) != sha256.Size {
		return fmt.Errorf("object store put %s: checksum is %d bytes, want a SHA-256", s.Key(key), len(checksum))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	metadata := objstore.Metadata{ObjectChecksumMetadataName: base64.RawStdEncoding.EncodeToString(checksum)}
	if _, err := s.Objects.Bucket().Put(ctx, s.Key(key), body, size, objstore.Always, metadata); err != nil {
		return fmt.Errorf("object store put %s: %w", s.Key(key), err)
	}
	return nil
}

// GetStream surfaces a missing object (and any other failure to reach it) as an
// error the caller can handle, rather than a mid-stream body it has already
// begun serving: the request is made, and the digest read, before it returns.
func (s *ObjectStoreByteStore) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	body, info, err := s.Objects.Bucket().Get(ctx, s.Key(key))
	if err != nil {
		return nil, fmt.Errorf("object store get %s: %w", s.Key(key), err)
	}
	expected, err := base64.RawStdEncoding.DecodeString(info.Metadata[ObjectChecksumMetadataName])
	if err != nil || len(expected) != sha256.Size {
		_ = body.Close()
		return nil, fmt.Errorf("object store get %s: the object has no SHA-256 beside it, so it is not one this store wrote", s.Key(key))
	}
	return &verifyingReadCloser{rc: body, hasher: sha256.New(), expected: expected, key: s.Key(key)}, nil
}

// verifyingReadCloser recomputes the stored SHA-256 as the object streams and
// converts the terminating io.EOF into a checksum error when the bytes don't
// match, so a streamed read never silently serves corruption.
type verifyingReadCloser struct {
	rc       io.ReadCloser
	hasher   hash.Hash
	expected []byte
	key      string
	checked  bool
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 {
		v.hasher.Write(p[:n])
	}
	if err == io.EOF && !v.checked {
		v.checked = true
		if !hmac.Equal(v.expected, v.hasher.Sum(nil)) {
			return n, fmt.Errorf("object store get %s: stored SHA-256 checksum does not match object bytes", v.key)
		}
	}
	return n, err
}

func (v *verifyingReadCloser) Close() error { return v.rc.Close() }

func (s *ObjectStoreByteStore) Get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream, err := s.GetStream(ctx, key)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("object store read %s: %w", s.Key(key), err)
	}
	return data, nil
}

func (s *ObjectStoreByteStore) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.Objects.Bucket().Delete(ctx, s.Key(key)); err != nil {
		return fmt.Errorf("object store delete %s: %w", s.Key(key), err)
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

func (s *ObjectStoreByteStore) Key(key string) string {
	return path.Join(s.Objects.Prefix(), strings.TrimPrefix(key, "/"))
}

// ObjectListing is one stored object, keyed relative to the store prefix. Size
// is that of the content, without the digest stored ahead of it.
type ObjectListing struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// listAll enumerates every stored object, keyed relative to the store prefix
// (the inverse of Key), for the orphan reaper.
func (s *ObjectStoreByteStore) listAll(ctx context.Context) ([]ObjectListing, error) {
	listPrefix := s.Objects.Prefix()
	if listPrefix != "" && !strings.HasSuffix(listPrefix, "/") {
		listPrefix += "/"
	}
	var out []ObjectListing
	err := s.Objects.Bucket().List(ctx, listPrefix, func(entry objstore.Entry) error {
		out = append(out, ObjectListing{
			Key:          strings.TrimPrefix(entry.Key, listPrefix),
			Size:         entry.Size,
			LastModified: entry.ModTime,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	return out, nil
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

// LFSStagingKey names the temporary bytes of an in-progress LFS upload, keyed
// by a unique per-upload id (NOT the oid). An upload streams here first and is
// promoted to the content-addressed LFSObjectDataKey only after its SHA-256 is
// verified, so a concurrent mismatched upload of the same oid can never
// overwrite or delete a good object's committed bytes.
func LFSStagingKey(uploadID string) string {
	return path.Join("lfs/staging", uploadID)
}

// CodeQLDatabaseDataKeyHashed builds the key from an already-computed SHA-256, so
// a streamed upload never has to hold the whole database in memory to hash it.
func CodeQLDatabaseDataKeyHashed(id int, sha256Sum []byte) string {
	return fmt.Sprintf("code-scanning/codeql/databases/%d/%x.zip", id, sha256Sum)
}

func CodeQLVariantAnalysisQueryPackDataKey(id int) string {
	return fmt.Sprintf("code-scanning/codeql/variant-analyses/%d/query-pack.tar.gz", id)
}

func AttestationBundleDataKey(id int) string {
	return fmt.Sprintf("attestations/%d/bundle.json", id)
}
