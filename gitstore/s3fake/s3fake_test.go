package s3fake

import (
	"bytes"
	"context"
	"testing"

	minio "github.com/minio/minio-go/v7"
)

// TestEveryBucketExists pins the answer to the existence check S3 clients make
// before they use a bucket. The fake keys objects without regard to a bucket,
// so refusing the check would fail a client that the fake could otherwise serve.
func TestEveryBucketExists(t *testing.T) {
	server := New()
	t.Cleanup(server.Close)
	exists, err := server.Client().Client.BucketExists(context.Background(), "any-bucket-at-all")
	if err != nil || !exists {
		t.Fatalf("BucketExists = %v, %v; want true", exists, err)
	}
}

// TestUserMetadataRoundTrips pins that x-amz-meta-* headers are stored with an
// object, returned on GET and HEAD, carried by a copy and dropped on delete.
// Storage designs that keep a fact beside an object's bytes depend on it.
func TestUserMetadataRoundTrips(t *testing.T) {
	server := New()
	t.Cleanup(server.Close)
	client := server.Client()
	ctx := context.Background()

	body := []byte("tree contents")
	if _, err := client.Client.PutObject(ctx, "bucket", "objects/ab/cdef", bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{UserMetadata: map[string]string{"git-type": "tree"}}); err != nil {
		t.Fatalf("put: %v", err)
	}

	stat, err := client.Client.StatObject(ctx, "bucket", "objects/ab/cdef", minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := stat.UserMetadata["Git-Type"]; got != "tree" {
		t.Fatalf("HEAD returned git-type %q, want tree (metadata %v)", got, stat.UserMetadata)
	}

	object, err := client.Client.GetObject(ctx, "bucket", "objects/ab/cdef", minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	read, err := object.Stat()
	_ = object.Close()
	if err != nil {
		t.Fatalf("get stat: %v", err)
	}
	if got := read.UserMetadata["Git-Type"]; got != "tree" {
		t.Fatalf("GET returned git-type %q, want tree", got)
	}

	if _, err := client.Client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: "bucket", Object: "objects/copy"},
		minio.CopySrcOptions{Bucket: "bucket", Object: "objects/ab/cdef"}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	copied, err := client.Client.StatObject(ctx, "bucket", "objects/copy", minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("stat copy: %v", err)
	}
	if got := copied.UserMetadata["Git-Type"]; got != "tree" {
		t.Fatalf("copy carried git-type %q, want tree", got)
	}

	if err := client.Client.RemoveObject(ctx, "bucket", "objects/ab/cdef", minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := client.Client.PutObject(ctx, "bucket", "objects/ab/cdef", bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{}); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	again, err := client.Client.StatObject(ctx, "bucket", "objects/ab/cdef", minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("stat re-put: %v", err)
	}
	if len(again.UserMetadata) != 0 {
		t.Fatalf("a re-created object inherited metadata %v", again.UserMetadata)
	}
}
