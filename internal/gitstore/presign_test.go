package gitstore

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// presignTestExpiry is the window the tests sign for. It is only ever compared
// against what the signature says, never waited out.
const presignTestExpiry = 10 * time.Minute

// TestPresignedGetURLAddressesOneObject pins what a presigned URL is: a GET of
// one key, in one bucket, signed for a bounded window, that a caller with no
// credentials of this process's can follow.
func TestPresignedGetURLAddressesOneObject(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "git/owner/repo")
	content := []byte("PACK not really, but these are the bytes behind the key")
	fake.put("git/owner/repo/objects/pack/pack-abc.pack", content)

	signed, err := fs.PresignedGetURL(context.Background(), "objects/pack/pack-abc.pack", presignTestExpiry)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse the presigned URL: %v", err)
	}
	if want := "/bucket/git/owner/repo/objects/pack/pack-abc.pack"; parsed.Path != want {
		t.Fatalf("the presigned URL addresses %q, want %q", parsed.Path, want)
	}
	if got, want := parsed.Query().Get("X-Amz-Expires"), strconv.Itoa(int(presignTestExpiry.Seconds())); got != want {
		t.Fatalf("the presigned URL expires in %qs, want %ss", got, want)
	}
	if parsed.Query().Get("X-Amz-Signature") == "" {
		t.Fatal("the presigned URL carries no signature")
	}

	response, err := http.Get(signed) // #nosec G107 -- the URL under test
	if err != nil {
		t.Fatalf("follow the presigned URL: %v", err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("the presigned URL served %q, want the stored bytes", got)
	}
}

// TestPresignedGetURLRefusesAnExpiryThatIsNotOne guards the one input that
// could turn a bounded credential into an unbounded one.
func TestPresignedGetURLRefusesAnExpiryThatIsNotOne(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "git")
	for _, expiry := range []time.Duration{0, -time.Minute} {
		if _, err := fs.PresignedGetURL(context.Background(), "objects/pack/pack-abc.pack", expiry); err == nil {
			t.Fatalf("presigning for %s produced a URL", expiry)
		}
	}
}

// TestPutStreamPublishesASmallStreamInOneRequest pins that a stream that fits
// in one part costs one request, which is what keeps a small artefact from
// paying for a three-step multipart upload.
func TestPutStreamPublishesASmallStreamInOneRequest(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "git")
	content := bytes.Repeat([]byte("small "), 1024)

	before := fake.snapshot()
	if err := fs.PutStream(context.Background(), "objects/bundle/one.bundle", bytes.NewReader(content)); err != nil {
		t.Fatalf("put stream: %v", err)
	}
	spent := fake.snapshot().sub(before)
	if spent.put != 1 || spent.multipart != 0 {
		t.Fatalf("a one-part stream cost %s, want a single put", spent)
	}
	stored, ok := fake.get("git/objects/bundle/one.bundle")
	if !ok || !bytes.Equal(stored, content) {
		t.Fatal("the stream did not land under its key")
	}
}

// TestPutStreamPublishesALargeStreamInParts pins the other half: a stream too
// large for one request is uploaded in parts and appears only once the upload
// completes, so a reader never sees half an artefact.
func TestPutStreamPublishesALargeStreamInParts(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "git")
	// Two full parts and a short one, so the loop is exercised in every state
	// it has: a part that continues, a part that ends the stream, and the
	// completion that publishes them.
	content := make([]byte, 2*s3StreamPartSize+1024)
	for index := range content {
		content[index] = byte(index)
	}

	before := fake.snapshot()
	if err := fs.PutStream(context.Background(), "objects/bundle/large.bundle", bytes.NewReader(content)); err != nil {
		t.Fatalf("put stream: %v", err)
	}
	if spent := fake.snapshot().sub(before); spent.multipart == 0 {
		t.Fatalf("a %d byte stream cost %s, want a multipart upload", len(content), spent)
	}
	stored, ok := fake.get("git/objects/bundle/large.bundle")
	if !ok {
		t.Fatal("the stream did not land under its key")
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("the stored object is %d bytes, want the %d written", len(stored), len(content))
	}
}
