package gitstore

import (
	"errors"
	"os"
	"testing"

	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// transientS3Client returns a *minio.Core whose every request fails to reach a
// server (the endpoint refuses connections), standing in for a transient S3
// outage (throttle, 5xx, network blip). The point is that the failure is not an
// HTTP 404, so it must never be mistaken for a definite absence.
func transientS3Client() *minio.Core {
	core, err := minio.NewCore("127.0.0.1:1", &minio.Options{
		Creds:        credentials.NewStaticV4("x", "y", ""),
		Secure:       false,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   1,
	})
	if err != nil {
		panic(err)
	}
	return core
}

// TestS3TransientErrorIsNotMistakenForNotExist covers STORE-037: a transient S3
// failure while reading a ref must surface as an error, never as os.ErrNotExist.
// go-git treats a missing ref as "branch absent" and a push would then overwrite
// a live branch — a silent history loss. Only a definite 404 (NoSuchKey /
// NotFound) may map to os.ErrNotExist; every other failure must propagate.
func TestS3TransientErrorIsNotMistakenForNotExist(t *testing.T) {
	fs := &S3FS{
		client: transientS3Client(),
		bucket: "bucket",
		prefix: "repo",
		active: &s3ActiveFiles{files: map[string]*s3FileState{}},
		locks:  newS3KeyLocks(),
	}

	if _, err := fs.Open("refs/heads/main"); err == nil {
		t.Fatal("Open returned nil error on a transient S3 failure")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open mapped a transient S3 failure to os.ErrNotExist: %v", err)
	}

	if _, err := fs.Stat("refs/heads/main"); err == nil {
		t.Fatal("Stat returned nil error on a transient S3 failure")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat mapped a transient S3 failure to os.ErrNotExist: %v", err)
	}
}
