package objstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/s3fake"
)

const presignTestExpiry = time.Minute

func newBucket(t *testing.T) (objstore.Bucket, *s3fake.Server) {
	t.Helper()
	server := s3fake.New()
	t.Cleanup(server.Close)
	return objstore.NewS3WithClient(server.Client().Client, "bucket", 0), server
}

// TestAConformingStorePassesAndIsLeftClean runs the startup probe against a
// store that honours its conditions, and checks the probe tidied up: it runs in
// the bucket that holds the repositories.
func TestAConformingStorePassesAndIsLeftClean(t *testing.T) {
	bucket, server := newBucket(t)
	if err := objstore.Conform(context.Background(), bucket, "probe/"); err != nil {
		t.Fatalf("a conforming store was refused: %v", err)
	}
	if keys := server.KeysWithPrefix(""); len(keys) != 0 {
		t.Fatalf("the probe left %v behind", keys)
	}
}

// unconditional is a store that accepts a conditional write and ignores the
// condition, as Google Cloud Storage's S3-compatible endpoint does.
type unconditional struct{ objstore.Bucket }

func (u unconditional) Put(ctx context.Context, key string, body io.Reader, size int64, _ objstore.Condition, metadata objstore.Metadata) (objstore.Version, error) {
	return u.Bucket.Put(ctx, key, body, size, objstore.Always, metadata)
}

// TestAStoreThatIgnoresConditionsIsRefused is why the probe exists. Such a store
// answers every request with success, so nothing but asking it to refuse one
// reveals that two replicas moving one branch would both be told they had.
func TestAStoreThatIgnoresConditionsIsRefused(t *testing.T) {
	bucket, _ := newBucket(t)
	err := objstore.Conform(context.Background(), unconditional{bucket}, "probe/")
	if err == nil || !strings.Contains(err.Error(), "create-if-absent") {
		t.Fatalf("a store that ignores conditions answered %v, want a refusal naming create-if-absent", err)
	}
}

// TestConditionalWritesArbitrateBetweenWriters pins the two conditions the
// engine's compare-and-swap is made of, and that a version read anywhere — from
// a write, a read or a listing — is the same token.
func TestConditionalWritesArbitrateBetweenWriters(t *testing.T) {
	bucket, _ := newBucket(t)
	ctx := context.Background()
	put := func(body string, condition objstore.Condition) (objstore.Version, error) {
		return bucket.Put(ctx, "repo/manifest", strings.NewReader(body), int64(len(body)), condition, nil)
	}

	first, err := put("one", objstore.IfAbsent())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := put("two", objstore.IfAbsent()); !errors.Is(err, objstore.ErrConditionNotMet) {
		t.Fatalf("second create: %v, want ErrConditionNotMet", err)
	}

	info, err := bucket.Head(ctx, "repo/manifest")
	if err != nil || info.Version != first || info.Size != 3 {
		t.Fatalf("head: %+v, %v; want the version the write returned", info, err)
	}
	var listed objstore.Version
	if err := bucket.List(ctx, "repo/", func(entry objstore.Entry) error {
		listed = entry.Version
		return nil
	}); err != nil || listed != first {
		t.Fatalf("listing reported version %q (err %v), want %q", listed, err, first)
	}

	second, err := put("two", objstore.IfVersion(first))
	if err != nil || second == first {
		t.Fatalf("swap at the current version: %q, %v", second, err)
	}
	if _, err := put("three", objstore.IfVersion(first)); !errors.Is(err, objstore.ErrConditionNotMet) {
		t.Fatalf("swap at a stale version: %v, want ErrConditionNotMet", err)
	}
}

// TestReadsListingsAndDeletes covers the rest of the interface against the fake:
// absence is ErrNotFound, a range carries the whole size, a directory listing
// folds what is below it, and a bulk delete removes everything it names.
func TestReadsListingsAndDeletes(t *testing.T) {
	bucket, server := newBucket(t)
	ctx := context.Background()
	for _, key := range []string{"r/HEAD", "r/refs/heads/main", "r/refs/heads/topic", "r/refs/tags/v1"} {
		if _, err := bucket.Put(ctx, key, strings.NewReader("0123456789"), 10, objstore.Always, nil); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	if _, _, err := bucket.Get(ctx, "r/absent"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("get of an absent key: %v", err)
	}
	if _, err := bucket.Head(ctx, "r/absent"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("head of an absent key: %v", err)
	}
	body, info, err := bucket.GetRange(ctx, "r/HEAD", 2, 4)
	if err != nil {
		t.Fatalf("ranged read: %v", err)
	}
	part, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(part, []byte("2345")) || info.Size != 10 {
		t.Fatalf("ranged read returned %q of %d", part, info.Size)
	}
	if _, _, err := bucket.GetRange(ctx, "r/HEAD", 50, 4); !errors.Is(err, objstore.ErrRangeNotSatisfiable) {
		t.Fatalf("a range past the end: %v", err)
	}

	var directory []string
	if err := bucket.ListDirectory(ctx, "r/", func(entry objstore.Entry) error {
		name := entry.Key
		if entry.Prefix {
			name += " (prefix)"
		}
		directory = append(directory, name)
		return nil
	}); err != nil {
		t.Fatalf("directory listing: %v", err)
	}
	if got := strings.Join(directory, ", "); got != "r/HEAD, r/refs/ (prefix)" {
		t.Fatalf("directory listing: %s", got)
	}
	var recursive int
	if err := bucket.List(ctx, "r/refs/", func(objstore.Entry) error { recursive++; return nil }); err != nil || recursive != 3 {
		t.Fatalf("recursive listing found %d, err %v", recursive, err)
	}

	if err := bucket.Copy(ctx, "r/HEAD", "fork/HEAD"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := bucket.DeleteMany(ctx, []string{"r/refs/heads/main", "r/refs/heads/topic", "r/refs/tags/v1"}); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if err := bucket.Delete(ctx, "r/never-existed"); err != nil {
		t.Fatalf("deleting what is not there: %v", err)
	}
	if got := strings.Join(server.KeysWithPrefix(""), ", "); got != "fork/HEAD, r/HEAD" {
		t.Fatalf("what is left: %s", got)
	}
	if signed, err := bucket.PresignGet(ctx, "r/HEAD", presignTestExpiry); err != nil || !strings.Contains(signed, "X-Amz-Signature") {
		t.Fatalf("presign: %q, %v", signed, err)
	}
}

// TestAPartSizeTheProtocolForbidsIsRefusedAtOnce pins that a misconfigured part
// size is an error when the bucket is opened, not when the first object large
// enough to be uploaded in parts arrives — which, in a git server, is some
// push, some day.
func TestAPartSizeTheProtocolForbidsIsRefusedAtOnce(t *testing.T) {
	if _, err := objstore.NewS3("bucket", objstore.S3Options{Endpoint: "http://127.0.0.1:9", PartBytes: 1 << 20}); err == nil {
		t.Fatal("a one-mebibyte part size was accepted")
	}
	for _, partBytes := range []uint64{0, 5 << 20, 64 << 20} {
		if _, err := objstore.NewS3("bucket", objstore.S3Options{Endpoint: "http://127.0.0.1:9", PartBytes: partBytes}); err != nil {
			t.Fatalf("a part size of %d was refused: %v", partBytes, err)
		}
	}
	if _, err := objstore.NewS3("bucket", objstore.S3Options{Endpoint: "http://bad host/"}); err == nil {
		t.Fatal("an endpoint that is not a URL was accepted")
	}
}

// forgetful is a store that accepts metadata and does not keep it.
type forgetful struct{ objstore.Bucket }

func (f forgetful) Put(ctx context.Context, key string, body io.Reader, size int64, condition objstore.Condition, _ objstore.Metadata) (objstore.Version, error) {
	return f.Bucket.Put(ctx, key, body, size, condition, nil)
}

// TestMetadataTravelsWithAnObjectAndOnlyInNamesEveryStoreKeeps pins the facts a
// writer may keep beside an object. They come back from a read and from a Head,
// under the name they were written with whatever case the wire gave it; a name
// one of the stores would refuse or respell is refused here, before it is
// written somewhere it cannot be read back from; and a store that drops them is
// refused at startup, since a digest kept this way is how corruption is caught.
func TestMetadataTravelsWithAnObjectAndOnlyInNamesEveryStoreKeeps(t *testing.T) {
	bucket, _ := newBucket(t)
	ctx := context.Background()
	written := objstore.Metadata{"bleephubsha256": "3q2+7w=="}
	if _, err := bucket.Put(ctx, "objects/asset", strings.NewReader("bytes"), 5, objstore.Always, written); err != nil {
		t.Fatalf("put: %v", err)
	}
	info, err := bucket.Head(ctx, "objects/asset")
	if err != nil || info.Metadata["bleephubsha256"] != "3q2+7w==" {
		t.Fatalf("head returned metadata %v, %v", info.Metadata, err)
	}
	body, info, err := bucket.Get(ctx, "objects/asset")
	if err != nil || info.Metadata["bleephubsha256"] != "3q2+7w==" {
		t.Fatalf("get returned metadata %v, %v", info.Metadata, err)
	}
	content, _ := io.ReadAll(body)
	_ = body.Close()
	if string(content) != "bytes" {
		t.Fatalf("an object written with metadata is not exactly its content: %q", content)
	}

	for _, name := range []string{"bleephub-sha256", "Sha256", "9lives", "", "sha_256"} {
		if _, err := bucket.Put(ctx, "objects/refused", strings.NewReader("x"), 1, objstore.Always, objstore.Metadata{name: "v"}); err == nil {
			t.Errorf("metadata name %q was accepted; not every store keeps it as written", name)
		}
	}
	if _, err := bucket.Head(ctx, "objects/refused"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("a refused write reached the store: %v", err)
	}

	err = objstore.Conform(ctx, forgetful{bucket}, "probe/")
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("a store that drops metadata answered %v, want a refusal naming metadata", err)
	}
}
