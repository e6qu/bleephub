package gitstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/packfile"
)

// presignTestExpiry is the window the tests sign for. It is only ever compared
// against what the signature says, never waited out.
const presignTestExpiry = 10 * time.Minute

// addressableRepository opens a repository and asserts it has addresses.
func addressableRepository(t *testing.T, fake *fakeS3) Addressable {
	t.Helper()
	stor, err := fake.store("git").Repository("owner/repo")
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	addressable, ok := stor.(Addressable)
	if !ok {
		t.Fatalf("an object-store repository is a %T, which has no addresses", stor)
	}
	return addressable
}

// followURL fetches a presigned URL as a client holding no credentials would.
func followURL(t *testing.T, signed string) []byte {
	t.Helper()
	response, err := http.Get(signed) // #nosec G107 -- the URL under test
	if err != nil {
		t.Fatalf("follow the presigned URL: %v", err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAPresignedURLAddressesOneObject pins what a presigned URL is: a GET of
// one key, in one bucket, signed for a bounded window, that a caller with no
// credentials of this process's can follow.
func TestAPresignedURLAddressesOneObject(t *testing.T) {
	fake := newFakeS3(t)
	repo := addressableRepository(t, fake)
	content := []byte("not really a bundle, but these are the bytes behind the key")
	if err := repo.PutAux(context.Background(), "objects/bundle/one.bundle", bytes.NewReader(content)); err != nil {
		t.Fatalf("put: %v", err)
	}

	signed, err := repo.AuxURL(context.Background(), "objects/bundle/one.bundle", presignTestExpiry)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse the presigned URL: %v", err)
	}
	if want := "/bucket/git/owner/repo/objects/bundle/one.bundle"; parsed.Path != want {
		t.Fatalf("the presigned URL addresses %q, want %q", parsed.Path, want)
	}
	if got, want := parsed.Query().Get("X-Amz-Expires"), strconv.Itoa(int(presignTestExpiry.Seconds())); got != want {
		t.Fatalf("the presigned URL expires in %qs, want %ss", got, want)
	}
	if parsed.Query().Get("X-Amz-Signature") == "" {
		t.Fatal("the presigned URL carries no signature")
	}
	if got := followURL(t, signed); !bytes.Equal(got, content) {
		t.Fatalf("the presigned URL served %q, want the stored bytes", got)
	}
}

// TestAStoredPackHasAURL pins the address a packfile-uri is made of: the pack a
// push published, fetched straight from the bucket, byte for byte.
func TestAStoredPackHasAURL(t *testing.T) {
	fake := newFakeS3(t)
	stor, err := fake.store("git").Repository("owner/repo")
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	pack, _ := pushPack(t, 50)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}
	packs, err := stor.(PackSource).StoredPacks(context.Background())
	if err != nil || len(packs) != 1 {
		t.Fatalf("stored packs: %v, %v", packs, err)
	}
	signed, err := stor.(Addressable).PackURL(context.Background(), packs[0].Name, presignTestExpiry)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if got := followURL(t, signed); !bytes.Equal(got, pack) {
		t.Fatalf("the pack's URL served %d bytes, want the %d pushed", len(got), len(pack))
	}
	if _, err := stor.(Addressable).PackURL(context.Background(), "pack-"+absentHash(1).String(), presignTestExpiry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a URL for a pack the repository does not hold: %v", err)
	}
}

// TestAPresignedURLRefusesAnExpiryThatIsNotOne guards the one input that could
// turn a bounded credential into an unbounded one.
func TestAPresignedURLRefusesAnExpiryThatIsNotOne(t *testing.T) {
	repo := addressableRepository(t, newFakeS3(t))
	for _, expiry := range []time.Duration{0, -time.Minute} {
		if _, err := repo.AuxURL(context.Background(), "objects/bundle/one.bundle", expiry); err == nil {
			t.Fatalf("presigning for %s produced a URL", expiry)
		}
	}
}

// TestAnAuxiliaryNameCannotReachGitsOwnData pins the boundary between what a
// server keeps beside a repository and the repository. Whoever chooses the name
// of a bundle must not thereby be able to write a branch, a pack or the config,
// or reach another repository.
func TestAnAuxiliaryNameCannotReachGitsOwnData(t *testing.T) {
	fake := newFakeS3(t)
	repo := addressableRepository(t, fake)
	for _, name := range []string{
		"", "/absolute", "../other/repo/HEAD", "objects/bundle/../../refs/heads/main", "bundles/",
		"HEAD", "config", "packed-refs", "refs/heads/main", "modules/sub/HEAD",
		"objects", "objects/pack/pack-1.pack", "objects/ab/cdef", "objects/info/packs",
	} {
		if err := repo.PutAux(context.Background(), name, bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("an auxiliary object was written under %q", name)
		}
	}
	if keys := fake.KeysWithPrefix(""); len(keys) != 0 {
		t.Fatalf("refused names reached the bucket: %v", keys)
	}
}

// TestAuxiliaryObjectsAreListedStatedAndRemoved covers the rest of what a server
// does with the bundles it publishes: find out whether one is there, list them,
// and prune the old ones.
func TestAuxiliaryObjectsAreListedStatedAndRemoved(t *testing.T) {
	fake := newFakeS3(t)
	repo := addressableRepository(t, fake)
	ctx := context.Background()
	for _, name := range []string{"objects/bundle/a.bundle", "objects/bundle/b.bundle", "objects/bundle/nested/c.bundle"} {
		if err := repo.PutAux(ctx, name, bytes.NewReader([]byte(name))); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
	if size, err := repo.StatAux(ctx, "objects/bundle/a.bundle"); err != nil || size != int64(len("objects/bundle/a.bundle")) {
		t.Fatalf("stat: %d, %v", size, err)
	}
	if _, err := repo.StatAux(ctx, "objects/bundle/absent.bundle"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat of an absent object: %v, want os.ErrNotExist", err)
	}
	listed, err := repo.ListAux(ctx, "objects/bundle")
	if err != nil || len(listed) != 2 || listed[0].Name != "a.bundle" || listed[1].Name != "b.bundle" {
		t.Fatalf("list: %v, %v; want the two bundles directly inside", listed, err)
	}
	// What a caller expires its publications by.
	if listed[0].Size == 0 || listed[0].ModTime.IsZero() {
		t.Fatalf("a listed object came without its size or the time it was written: %+v", listed[0])
	}
	if err := repo.RemoveAux(ctx, "objects/bundle/a.bundle"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := repo.RemoveAux(ctx, "objects/bundle/a.bundle"); err != nil {
		t.Fatalf("removing what is already gone: %v", err)
	}
	if _, err := repo.StatAux(ctx, "objects/bundle/a.bundle"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after remove: %v", err)
	}
}

// TestPutAuxPublishesASmallStreamInOneRequest pins that a stream that fits in
// one part costs one request, which is what keeps a small artefact from paying
// for a three-step multipart upload.
func TestPutAuxPublishesASmallStreamInOneRequest(t *testing.T) {
	fake := newFakeS3(t)
	repo := addressableRepository(t, fake)
	content := bytes.Repeat([]byte("small "), 1024)

	before := fake.Snapshot()
	if err := repo.PutAux(context.Background(), "objects/bundle/one.bundle", bytes.NewReader(content)); err != nil {
		t.Fatalf("put stream: %v", err)
	}
	spent := fake.Snapshot().Sub(before)
	if spent.Put != 1 || spent.Multipart != 0 || spent.Total() != 1 {
		t.Fatalf("a one-part stream cost %s, want a single put", spent)
	}
	stored, ok := fake.Get("git/owner/repo/objects/bundle/one.bundle")
	if !ok || !bytes.Equal(stored, content) {
		t.Fatal("the stream did not land under its key")
	}
}

// TestPutAuxPublishesALargeStreamInParts pins the other half: a stream too
// large for one request is uploaded in parts and appears only once the upload
// completes, so a reader never sees half an artefact.
func TestPutAuxPublishesALargeStreamInParts(t *testing.T) {
	fake := newFakeS3(t)
	repo := addressableRepository(t, fake)
	// Two full parts and a short one, so the upload is exercised in every state
	// it has: a part that continues, a part that ends the stream, and the
	// completion that publishes them.
	content := make([]byte, 2*auxStreamPartSize+1024)
	for index := range content {
		content[index] = byte(index)
	}

	before := fake.Snapshot()
	if err := repo.PutAux(context.Background(), "objects/bundle/large.bundle", bytes.NewReader(content)); err != nil {
		t.Fatalf("put stream: %v", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Multipart == 0 {
		t.Fatalf("a %d byte stream cost %s, want a multipart upload", len(content), spent)
	}
	stored, ok := fake.Get("git/owner/repo/objects/bundle/large.bundle")
	if !ok {
		t.Fatal("the stream did not land under its key")
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("the stored object is %d bytes, want the %d written", len(stored), len(content))
	}
}
