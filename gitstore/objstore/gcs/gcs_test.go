package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	storage "github.com/e6qu/bleephub/gcsclient"
	"github.com/e6qu/bleephub/gcsclient/gcsfake"

	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/objstore/objstoretest"
)

const (
	testBucket = "repositories"
	// quantum is the smallest chunk the service allows.
	quantum = 256 << 10
)

// meter records every request the driver sends, through the Transport option
// that exists so a harness can do exactly this.
type meter struct {
	mu       sync.Mutex
	requests []metered
}

type metered struct {
	method, path string
	query        url.Values
}

func (m *meter) RoundTrip(request *http.Request) (*http.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, metered{request.Method, request.URL.Path, request.URL.Query()})
	m.mu.Unlock()
	return http.DefaultTransport.RoundTrip(request)
}

// sent returns the requests so far of the method given whose path begins as
// given.
func (m *meter) sent(method, pathPrefix string) []metered {
	m.mu.Lock()
	defer m.mu.Unlock()
	var matched []metered
	for _, request := range m.requests {
		if request.method == method && strings.HasPrefix(request.path, pathPrefix) {
			matched = append(matched, request)
		}
	}
	return matched
}

// listings returns the objects.list requests so far, whose path is the prefix
// of every request that names an object.
func (m *meter) listings() []metered {
	var matched []metered
	for _, request := range m.sent(http.MethodGet, listPath) {
		if request.path == listPath {
			matched = append(matched, request)
		}
	}
	return matched
}

const (
	listPath   = "/storage/v1/b/" + testBucket + "/o"
	uploadPath = "/upload/storage/v1/b/" + testBucket + "/o"
)

func newBucket(t *testing.T, chunkBytes int64) (objstore.Bucket, *gcsfake.Server, *meter) {
	t.Helper()
	server := gcsfake.New()
	t.Cleanup(server.Close)
	server.CreateBucket(testBucket)
	requests := &meter{}
	opened, err := New(testBucket, Options{
		Endpoint:        server.URL(),
		CredentialsJSON: server.CredentialsJSON(),
		Transport:       requests,
		ChunkBytes:      chunkBytes,
	})
	if err != nil {
		t.Fatalf("open the fake's bucket: %v", err)
	}
	return opened, server, requests
}

// TestTheGCSDriverPassesTheSuite holds the Cloud Storage driver to what every
// driver must do, against the in-process service.
func TestTheGCSDriverPassesTheSuite(t *testing.T) {
	objstoretest.Run(t, func(t *testing.T) objstore.Bucket {
		opened, _, _ := newBucket(t, 0)
		return opened
	})
}

// TestTheGCSDriverPassesTheSuiteAgainstARealEndpoint runs the same suite
// against a Cloud Storage endpoint that was not written by the driver's author:
// the fake-gcs-server emulator in CI. The in-process fake is one reading of
// Google's documentation; this is the check on that reading.
//
// The emulator checks neither credentials nor signatures, but the driver has
// one way to authenticate and takes it regardless, so the test makes a
// service-account key of its own and stands up the token endpoint the key
// names. That is also why this cannot be pointed at Cloud Storage itself, which
// would refuse the token.
func TestTheGCSDriverPassesTheSuiteAgainstARealEndpoint(t *testing.T) {
	settings := map[string]string{}
	for _, name := range []string{"OBJSTORE_GCS_TEST_ENDPOINT", "OBJSTORE_GCS_TEST_BUCKET"} {
		settings[name] = os.Getenv(name)
		if settings[name] == "" {
			t.Skipf("%s is not set: set it, with the other OBJSTORE_GCS_TEST_ variables (ENDPOINT, BUCKET), to run the suite against an emulator of Cloud Storage", name)
		}
	}
	endpoint, bucketName := strings.TrimRight(settings["OBJSTORE_GCS_TEST_ENDPOINT"], "/"), settings["OBJSTORE_GCS_TEST_BUCKET"]
	options := Options{Endpoint: endpoint, CredentialsJSON: throwawayCredentials(t)}

	// The driver knows nothing of buckets, so the bucket is made by hand, of an
	// emulator that asks for no authorization. 409 is a bucket already there.
	description, err := json.Marshal(map[string]string{"name": bucketName})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	created, err := http.Post(endpoint+"/storage/v1/b?project=objstoretest", "application/json", bytes.NewReader(description)) // #nosec G107 -- the endpoint is the one the test was told to use
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucketName, err)
	}
	answer, _ := io.ReadAll(created.Body)
	_ = created.Body.Close()
	if created.StatusCode != http.StatusOK && created.StatusCode != http.StatusConflict {
		t.Fatalf("create bucket %s: HTTP %d %s", bucketName, created.StatusCode, answer)
	}

	objstoretest.Run(t, func(t *testing.T) objstore.Bucket {
		opened, err := New(bucketName, options)
		if err != nil {
			t.Fatalf("open %s: %v", bucketName, err)
		}
		// The store is shared and outlives the run, so what a test wrote under its
		// prefix is cleared before it and after it.
		_, name, _ := strings.Cut(t.Name(), "/")
		prefix := "objstoretest/" + name + "/"
		empty := func() {
			var keys []string
			if err := opened.List(context.Background(), prefix, func(entry objstore.Entry) error {
				keys = append(keys, entry.Key)
				return nil
			}); err != nil {
				t.Fatalf("list %s to clear it: %v", prefix, err)
			}
			if err := opened.DeleteMany(context.Background(), keys); err != nil {
				t.Fatalf("clear %s: %v", prefix, err)
			}
		}
		empty()
		t.Cleanup(empty)
		return opened
	})
}

// throwawayCredentials makes a service-account key file whose token endpoint is
// a server of the test's own that hands a token to whoever asks.
func throwawayCredentials(t *testing.T) []byte {
	t.Helper()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"for-an-emulator-that-checks-none","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(tokens.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encode the key: %v", err)
	}
	file, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"private_key_id": "throwaway",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "objstoretest@objstoretest.iam.gserviceaccount.com",
		"token_uri":      tokens.URL,
	})
	if err != nil {
		t.Fatalf("encode the key file: %v", err)
	}
	return file
}

// TestWhatTheOperatorMustStateIsRequired pins that nothing about which service
// a deployment writes to, or as whom, is defaulted: a driver that guessed an
// endpoint or went looking for credentials would start against a store nobody
// chose.
func TestWhatTheOperatorMustStateIsRequired(t *testing.T) {
	server := gcsfake.New()
	t.Cleanup(server.Close)
	whole := Options{Endpoint: server.URL(), CredentialsJSON: server.CredentialsJSON()}
	if _, err := New(testBucket, whole); err != nil {
		t.Fatalf("PREMISE: the options this test takes fields away from are refused whole: %v", err)
	}
	for name, change := range map[string]func(*Options){
		"no endpoint":               func(o *Options) { o.Endpoint = "" },
		"an endpoint not a URL":     func(o *Options) { o.Endpoint = "storage.googleapis.com" },
		"no credentials":            func(o *Options) { o.CredentialsJSON = nil },
		"credentials not a key":     func(o *Options) { o.CredentialsJSON = []byte(`{"type":"authorized_user"}`) },
		"a chunk not of 256 KiB":    func(o *Options) { o.ChunkBytes = quantum + 1 },
		"a chunk of less than none": func(o *Options) { o.ChunkBytes = -quantum },
	} {
		options := whole
		change(&options)
		if _, err := New(testBucket, options); err == nil {
			t.Errorf("%s: the driver opened anyway", name)
		}
	}
	if _, err := New("", whole); err == nil {
		t.Error("no bucket: the driver opened anyway")
	}
}

// TestAMissingBucketIsNotAnObjectThatIsNotFound protects the difference between
// "no such object" and "no such bucket". Both are 404 "notFound" on the wire.
// Reported as ErrNotFound, a mistyped bucket name would read as an empty store:
// every repository absent, and the first push creating it anew. It pins too
// what telling them apart costs: one listing, once, and none where nothing was
// ever found missing.
func TestAMissingBucketIsNotAnObjectThatIsNotFound(t *testing.T) {
	server := gcsfake.New()
	t.Cleanup(server.Close)
	opened, err := New("never-created", Options{Endpoint: server.URL(), CredentialsJSON: server.CredentialsJSON()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	nothing := func(objstore.Entry) error { return nil }
	_, headErr := opened.Head(ctx, "HEAD")
	_, _, getErr := opened.Get(ctx, "HEAD")
	_, _, rangeErr := opened.GetRange(ctx, "HEAD", 0, 4)
	_, putErr := opened.Put(ctx, "HEAD", strings.NewReader("x"), 1, objstore.Always, nil)
	_, streamErr := opened.Put(ctx, "HEAD", strings.NewReader("x"), -1, objstore.IfAbsent(), nil)
	for operation, err := range map[string]error{
		"head":           headErr,
		"get":            getErr,
		"ranged get":     rangeErr,
		"put":            putErr,
		"streamed put":   streamErr,
		"delete":         opened.Delete(ctx, "HEAD"),
		"bulk delete":    opened.DeleteMany(ctx, []string{"HEAD", "config"}),
		"copy":           opened.Copy(ctx, "HEAD", "fork/HEAD"),
		"list":           opened.List(ctx, "", nothing),
		"list directory": opened.ListDirectory(ctx, "", nothing),
		"startup probe":  objstore.Conform(ctx, opened, "probe/"),
	} {
		var refused *storage.Error
		if err == nil || errors.Is(err, objstore.ErrNotFound) || errors.Is(err, objstore.ErrConditionNotMet) || !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
			t.Errorf("%s in a bucket that does not exist answered %v, want the service's 404 and not ErrNotFound", operation, err)
		}
	}

	present, _, requests := newBucket(t, 0)
	for range 3 {
		if _, err := present.Head(ctx, "absent"); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("PREMISE: head of an absent key in a bucket that exists answered %v", err)
		}
		if _, _, err := present.Get(ctx, "absent"); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("PREMISE: get of an absent key in a bucket that exists answered %v", err)
		}
	}
	if asked := len(requests.listings()); asked != 1 {
		t.Errorf("six reads of absent keys asked after the bucket %d times, want once", asked)
	}
	written, _, quiet := newBucket(t, 0)
	if _, err := written.Put(ctx, "HEAD", strings.NewReader("x"), 1, objstore.Always, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := written.Head(ctx, "HEAD"); err != nil {
		t.Fatalf("head: %v", err)
	}
	if asked := len(quiet.listings()); asked != 0 {
		t.Errorf("a write and a read of what is there asked after the bucket %d times, want never", asked)
	}
}

// TestAnUploadInChunksIsStillConditional pins what Cloud Storage allows and S3
// does not: a resumable upload carries its precondition to the write that
// completes it, so a writer that does not know its size — or has more than a
// chunk — can still create-if-absent and compare-and-swap. It also pins the
// size at which an upload goes in chunks.
func TestAnUploadInChunksIsStillConditional(t *testing.T) {
	opened, _, requests := newBucket(t, quantum)
	ctx := context.Background()
	body := bytes.Repeat([]byte("0123456789abcdef"), 3*quantum/16+1)

	if _, err := opened.Put(ctx, "whole", bytes.NewReader(body[:quantum]), quantum, objstore.Always, nil); err != nil {
		t.Fatalf("put a chunk's worth: %v", err)
	}
	if chunks := len(requests.sent(http.MethodPut, uploadPath)); chunks != 0 {
		t.Fatalf("an object of exactly one chunk took %d chunk requests, want one multipart upload", chunks)
	}

	first, err := opened.Put(ctx, "streamed", io.MultiReader(bytes.NewReader(body)), -1, objstore.IfAbsent(), objstore.Metadata{"kept": "beside"})
	if err != nil {
		t.Fatalf("create from a stream: %v", err)
	}
	if chunks := len(requests.sent(http.MethodPut, uploadPath)); chunks != 4 {
		t.Fatalf("PREMISE: %d bytes in chunks of %d took %d chunk requests, want 4 — the upload did not go in chunks", len(body), quantum, chunks)
	}
	if _, err := opened.Put(ctx, "streamed", bytes.NewReader([]byte("usurper")), -1, objstore.IfAbsent(), nil); !errors.Is(err, objstore.ErrConditionNotMet) {
		t.Fatalf("a streamed create of what exists answered %v, want ErrConditionNotMet", err)
	}
	for _, stale := range []objstore.Version{first + "0", "1", "not-a-generation", "0", "-5"} {
		sent := len(requests.requests)
		if _, err := opened.Put(ctx, "streamed", bytes.NewReader(body), int64(len(body)), objstore.IfVersion(stale), nil); !errors.Is(err, objstore.ErrConditionNotMet) {
			t.Fatalf("a swap in chunks at the version %q answered %v, want ErrConditionNotMet", stale, err)
		}
		if stale == "0" && len(requests.requests) != sent {
			t.Errorf("a swap at version 0 was sent to the service, to which a generation of 0 means absent")
		}
	}
	read, info, err := opened.Get(ctx, "streamed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(read)
	_ = read.Close()
	if !bytes.Equal(got, body) || info.Version != first || info.Metadata["kept"] != "beside" {
		t.Fatalf("after refused writes the object is %d bytes at %q with %v, want the %d first written at %q", len(got), info.Version, info.Metadata, len(body), first)
	}
	if _, err := opened.Put(ctx, "streamed", bytes.NewReader(body), int64(len(body)), objstore.IfVersion(first), nil); err != nil {
		t.Fatalf("a swap in chunks at the current version: %v", err)
	}
}

// TestABodyThatIsNotTheSizeItWasSaidToBeIsNotWritten protects a caller that
// miscounts from a store holding a truncated object under a good name: git
// would find the pack corrupt long after the push that wrote it succeeded.
func TestABodyThatIsNotTheSizeItWasSaidToBeIsNotWritten(t *testing.T) {
	opened, server, _ := newBucket(t, quantum)
	ctx := context.Background()
	for name, size := range map[string]int64{"short-of-one-request": 100, "short-of-several-chunks": 4 * quantum} {
		if _, err := opened.Put(ctx, name, strings.NewReader("only this"), size, objstore.Always, nil); err == nil {
			t.Errorf("%s: nine bytes were accepted as %d", name, size)
		}
	}
	if _, err := opened.Put(ctx, "longer", bytes.NewReader(make([]byte, 3*quantum)), 2*quantum, objstore.Always, nil); err == nil {
		t.Errorf("%d bytes were accepted as %d", 3*quantum, 2*quantum)
	}
	if names := server.ObjectNames(testBucket); len(names) != 0 {
		t.Errorf("refused writes left %v", names)
	}
	if open := server.UploadsInProgress(); open != 0 {
		t.Errorf("refused writes left %d upload sessions for the service to keep", open)
	}
}

// TestABulkDeleteGoesInBatchesOfAHundred pins the cost of deleting a
// repository: one request for every hundred keys, not one for each. It also
// pins that the driver reads the answer to each call of a batch, since the
// batch as a whole reports success whatever its calls did.
func TestABulkDeleteGoesInBatchesOfAHundred(t *testing.T) {
	opened, server, requests := newBucket(t, 0)
	var keys []string
	for i := range 250 {
		key := fmt.Sprintf("repo/objects/%04d", i)
		server.Put(testBucket, key, []byte("x"), nil)
		keys = append(keys, key)
	}
	server.Put(testBucket, "repo-kept/HEAD", []byte("x"), nil)
	if held := len(server.ObjectNames(testBucket)); held != 251 {
		t.Fatalf("PREMISE: the store holds %d objects before the delete, want 251", held)
	}
	if err := opened.DeleteMany(context.Background(), append(keys, "repo/objects/never-there")); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if left := server.ObjectNames(testBucket); len(left) != 1 || left[0] != "repo-kept/HEAD" {
		t.Fatalf("after the delete the store holds %v, want only the key not named", left)
	}
	if batches, singles := len(requests.sent(http.MethodPost, "/batch/")), len(requests.sent(http.MethodDelete, "/")); batches != 3 || singles != 0 {
		t.Fatalf("251 keys took %d batch requests and %d single deletes, want 3 and 0", batches, singles)
	}
}

// TestACopyTheServiceHasNotFinishedIsFollowedToItsEnd protects a fork from
// reading packs that are not there yet. The service copies what it can within
// one call and answers "not done" with a token; a driver that returned on the
// first answer would hand back a repository whose objects are not all there.
func TestACopyTheServiceHasNotFinishedIsFollowedToItsEnd(t *testing.T) {
	opened, server, requests := newBucket(t, 0)
	ctx := context.Background()
	pack := bytes.Repeat([]byte("a pack "), 500)
	server.Put(testBucket, "source", pack, map[string]string{"kept": "beside"})

	server.SetRewriteBytesPerCall(1000)
	if err := opened.Copy(ctx, "source", "fork"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if calls := len(requests.sent(http.MethodPost, listPath+"/source/rewriteTo/")); calls != 4 {
		t.Fatalf("PREMISE: a copy of %d bytes at 1000 a call took %d calls, want 4 — the driver did not have to wait on it", len(pack), calls)
	}
	if copied, ok := server.Contents(testBucket, "fork"); !ok || !bytes.Equal(copied, pack) {
		t.Fatalf("the copy holds %d bytes, want the source's %d", len(copied), len(pack))
	}
	if info, err := opened.Head(ctx, "fork"); err != nil || info.Metadata["kept"] != "beside" {
		t.Errorf("the copy's metadata is %v (%v), want the source's", info.Metadata, err)
	}
}

// TestASignedURLIsRefusedWithoutAnExpiryTheServiceAllows pins that a URL is
// never signed to expire at or before the moment it was made, or later than the
// service will honour: either would be signed without complaint and then
// refused at every use.
func TestASignedURLIsRefusedWithoutAnExpiryTheServiceAllows(t *testing.T) {
	opened, _, _ := newBucket(t, 0)
	for _, expiry := range []time.Duration{0, -time.Minute, 8 * 24 * time.Hour} {
		if signed, err := opened.PresignGet(context.Background(), "pack", expiry); err == nil {
			t.Errorf("an expiry of %s was signed: %s", expiry, signed)
		}
	}
}

// TestADirectoryListingStaysInOrderAcrossPages protects the order ListDirectory
// promises. The service hands each page over as a list of objects and a list of
// prefixes, which have to be merged, page by page; visiting all of a page's
// objects and then its prefixes would pass any listing too small to have both,
// and hand a larger one over out of order.
func TestADirectoryListingStaysInOrderAcrossPages(t *testing.T) {
	opened, server, requests := newBucket(t, 0)
	var want []string
	for i := range 1200 {
		server.Put(testBucket, fmt.Sprintf("repo/e%04d", i), []byte("x"), nil)
		server.Put(testBucket, fmt.Sprintf("repo/e%04d/below", i), []byte("x"), nil)
		want = append(want, fmt.Sprintf("repo/e%04d", i), fmt.Sprintf("repo/e%04d/ (prefix)", i))
	}
	var got []string
	if err := opened.ListDirectory(context.Background(), "repo/", func(entry objstore.Entry) error {
		name := entry.Key
		if entry.Prefix {
			name += " (prefix)"
		}
		got = append(got, name)
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if pages := len(requests.listings()); pages != 3 {
		t.Fatalf("PREMISE: 2400 entries came in %d pages, want 3 — the listing never crossed a page", pages)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the directory listing returned %d entries, want the %d written with each object before the prefix that extends its name", len(got), len(want))
	}
}

// TestARangeAtTheEndIsNotSatisfiableWhateverTheStoreAnswers pins a rule the
// driver holds itself. The interface says a range that starts at the end of an
// object is not satisfiable, and the engine's pack reader reads to a pack's end
// by being told so. The service says it with a 416; an emulator may answer
// success with no bytes, as Azure's was found to the first time the suite ran
// against it. The response carries the object's whole size either way, so the
// driver decides by that.
func TestARangeAtTheEndIsNotSatisfiableWhateverTheStoreAnswers(t *testing.T) {
	opened, server, _ := newBucket(t, 0)
	server.Put(testBucket, "pack", []byte("0123456789"), nil)
	ctx := context.Background()

	if _, _, err := opened.GetRange(ctx, "pack", 10, 4); !errors.Is(err, objstore.ErrRangeNotSatisfiable) {
		t.Fatalf("premise: a store that answers 416 gave %v", err)
	}
	server.AnswerARangeAtTheEndWithNoBytes()
	if _, _, err := opened.GetRange(ctx, "pack", 10, 4); !errors.Is(err, objstore.ErrRangeNotSatisfiable) {
		t.Fatalf("a store that answers success with no bytes gave %v, want ErrRangeNotSatisfiable", err)
	}
	body, info, err := opened.GetRange(ctx, "pack", 9, 4)
	if err != nil {
		t.Fatalf("the last byte: %v", err)
	}
	last, _ := io.ReadAll(body)
	_ = body.Close()
	if string(last) != "9" || info.Size != 10 {
		t.Fatalf("the last byte read as %q of %d", last, info.Size)
	}
}
