package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/s3fake"
)

// newFakeS3ByteStore returns the object-store byte store on an in-process
// object store, and that store so a test can reach under the byte store.
func newFakeS3ByteStore(t *testing.T) (*ObjectStoreByteStore, *s3fake.Server) {
	t.Helper()
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	bucket := objstore.NewS3WithClient(fake.Client().Client, "bleephub-test", 0)
	objects := gitstore.Open(bucket, "objects", gitstore.Options{CacheDir: t.TempDir()})
	return &ObjectStoreByteStore{Objects: objects}, fake
}

// TestS3ByteStoreReturnsWhatEachWritePathStored covers the three ways bytes
// enter the store — whole, streamed, and streamed with a digest the caller
// already computed — and both ways they leave it.
func TestS3ByteStoreReturnsWhatEachWritePathStored(t *testing.T) {
	byteStore, _ := newFakeS3ByteStore(t)
	ctx := context.Background()
	content := []byte("durable object bytes")
	digest := sha256.Sum256(content)

	writes := map[string]func(key string) error{
		"whole":    func(key string) error { return byteStore.Put(ctx, key, content) },
		"streamed": func(key string) error { return byteStore.PutStream(ctx, key, bytes.NewReader(content)) },
		"hashed": func(key string) error {
			return byteStore.PutStreamHashed(ctx, key, bytes.NewReader(content), int64(len(content)), digest[:])
		},
	}
	for name, write := range writes {
		key := "actions/artifacts/" + name
		if err := write(key); err != nil {
			t.Fatalf("%s write: %v", name, err)
		}
		got, err := byteStore.Get(ctx, key)
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("%s write read back %q, %v; want %q", name, got, err, content)
		}
		stream, err := byteStore.GetStream(ctx, key)
		if err != nil {
			t.Fatalf("%s write: open stream: %v", name, err)
		}
		streamed, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil || !bytes.Equal(streamed, content) {
			t.Fatalf("%s write streamed back %q, %v; want %q", name, streamed, err, content)
		}
	}
}

// TestS3ByteStoreRejectsBytesTheStoreCorrupted is why every object has the
// digest of its content kept beside it: bytes changed underneath the byte store
// must fail the read, whole or streamed, instead of being served. What is stored
// is exactly the content, so that an object can one day be served by URL.
func TestS3ByteStoreRejectsBytesTheStoreCorrupted(t *testing.T) {
	byteStore, fake := newFakeS3ByteStore(t)
	ctx := context.Background()
	if err := byteStore.Put(ctx, "releases/assets/7/data", []byte("durable object bytes")); err != nil {
		t.Fatalf("put: %v", err)
	}
	stored, ok := fake.Get("objects/releases/assets/7/data")
	if !ok {
		t.Fatal("the object is not under the byte store's prefix")
	}
	if string(stored) != "durable object bytes" {
		t.Fatalf("the stored object is %q, not exactly its content", stored)
	}
	corrupted := append([]byte{}, stored...)
	corrupted[len(corrupted)-1] ^= 0xff
	fake.Put("objects/releases/assets/7/data", corrupted)

	if _, err := byteStore.Get(ctx, "releases/assets/7/data"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("whole read of corrupted bytes answered %v, want a checksum mismatch", err)
	}
	stream, err := byteStore.GetStream(ctx, "releases/assets/7/data")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer stream.Close()
	if _, err := io.ReadAll(stream); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("streamed read of corrupted bytes answered %v, want a checksum mismatch", err)
	}
}

// TestS3ByteStoreRefusesAnObjectWithoutItsDigest pins that there is one way an
// object is stored: one with no digest beside it is not one this store wrote,
// and is an error, not bytes to serve unverified.
func TestS3ByteStoreRefusesAnObjectWithoutItsDigest(t *testing.T) {
	byteStore, fake := newFakeS3ByteStore(t)
	fake.Put("objects/lfs/objects/short", []byte("no digest"))
	if _, err := byteStore.Get(context.Background(), "lfs/objects/short"); err == nil {
		t.Fatal("an object with no digest was served")
	}
}

// TestS3ByteStoreReportsAMissingObjectBeforeStreaming pins that a handler learns
// an object is gone while it can still answer 404, not after the first byte.
func TestS3ByteStoreReportsAMissingObjectBeforeStreaming(t *testing.T) {
	byteStore, _ := newFakeS3ByteStore(t)
	if _, err := byteStore.GetStream(context.Background(), "actions/logs/1/data"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("stream of a missing object answered %v, want objstore.ErrNotFound", err)
	}
}

// TestS3ByteStoreListsContentSizesRelativeToItsPrefix pins what the orphan
// reaper reads: keys as the byte store names them, and the size of the content.
func TestS3ByteStoreListsContentSizesRelativeToItsPrefix(t *testing.T) {
	byteStore, fake := newFakeS3ByteStore(t)
	ctx := context.Background()
	if err := byteStore.Put(ctx, "actions/caches/3/data", []byte("cache")); err != nil {
		t.Fatalf("put: %v", err)
	}
	fake.Put("git/owner/repo/HEAD", []byte("ref: refs/heads/main\n"))

	listed, err := byteStore.listAll(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Key != "actions/caches/3/data" || listed[0].Size != int64(len("cache")) {
		t.Fatalf("listing = %+v, want the one object at its relative key with its content size", listed)
	}
	if err := byteStore.Delete(ctx, "actions/caches/3/data"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if listed, err := byteStore.listAll(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("listing after delete = %+v, %v", listed, err)
	}
}
