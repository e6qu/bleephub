package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/e6qu/bleephub/gitstore/azfake"
	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/objstore/objstoretest"
)

const testContainer = "repositories"

// meter records every request the driver sends, through the Transport option
// that exists so a harness can do exactly this.
type meter struct {
	mu       sync.Mutex
	requests []metered
}

type metered struct {
	method string
	query  url.Values
}

func (m *meter) RoundTrip(request *http.Request) (*http.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, metered{request.Method, request.URL.Query()})
	m.mu.Unlock()
	return http.DefaultTransport.RoundTrip(request)
}

// sent returns the requests so far of the method and the comp parameter given,
// which between them name a Blob service operation.
func (m *meter) sent(method, comp string) []metered {
	m.mu.Lock()
	defer m.mu.Unlock()
	var matched []metered
	for _, request := range m.requests {
		if request.method == method && request.query.Get("comp") == comp {
			matched = append(matched, request)
		}
	}
	return matched
}

func newBucket(t *testing.T, blockBytes uint64) (*bucket, *azfake.Server, *meter) {
	t.Helper()
	server := azfake.New()
	t.Cleanup(server.Close)
	server.CreateContainer(testContainer)
	requests := &meter{}
	opened, err := New(testContainer, Options{
		Endpoint:    server.URL(),
		AccountName: server.AccountName(),
		AccountKey:  server.AccountKey(),
		Transport:   requests,
		BlockBytes:  blockBytes,
	})
	if err != nil {
		t.Fatalf("open the fake's container: %v", err)
	}
	return opened.(*bucket), server, requests
}

// TestTheAzureDriverPassesTheSuite holds the Azure driver to what every driver
// must do, against the in-process Blob service.
func TestTheAzureDriverPassesTheSuite(t *testing.T) {
	objstoretest.Run(t, func(t *testing.T) objstore.Bucket {
		opened, _, _ := newBucket(t, 0)
		return opened
	})
}

// TestTheAzureDriverPassesTheSuiteAgainstARealEndpoint runs the same suite
// against a Blob service that was not written by the driver's author — the
// Azurite emulator in CI, or a storage account. The in-process fake is one
// reading of Azure's documentation; this is the check on that reading.
func TestTheAzureDriverPassesTheSuiteAgainstARealEndpoint(t *testing.T) {
	settings := map[string]string{}
	for _, name := range []string{"OBJSTORE_AZURE_TEST_ENDPOINT", "OBJSTORE_AZURE_TEST_ACCOUNT", "OBJSTORE_AZURE_TEST_KEY", "OBJSTORE_AZURE_TEST_CONTAINER"} {
		settings[name] = os.Getenv(name)
		if settings[name] == "" {
			t.Skipf("%s is not set: set it, with the other OBJSTORE_AZURE_TEST_ variables (ENDPOINT, ACCOUNT, KEY, CONTAINER), to run the suite against a real Blob endpoint", name)
		}
	}
	options := Options{
		Endpoint:    settings["OBJSTORE_AZURE_TEST_ENDPOINT"],
		AccountName: settings["OBJSTORE_AZURE_TEST_ACCOUNT"],
		AccountKey:  settings["OBJSTORE_AZURE_TEST_KEY"],
	}
	containerName := settings["OBJSTORE_AZURE_TEST_CONTAINER"]

	credential, err := container.NewSharedKeyCredential(options.AccountName, options.AccountKey)
	if err != nil {
		t.Fatalf("the account key: %v", err)
	}
	client, err := container.NewClientWithSharedKeyCredential(strings.TrimRight(options.Endpoint, "/")+"/"+containerName, credential, nil)
	if err != nil {
		t.Fatalf("a client for the container: %v", err)
	}
	if _, err := client.Create(context.Background(), nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		t.Fatalf("create container %s: %v", containerName, err)
	}

	objstoretest.Run(t, func(t *testing.T) objstore.Bucket {
		opened, err := New(containerName, options)
		if err != nil {
			t.Fatalf("open %s: %v", containerName, err)
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

// TestWhatTheOperatorMustStateIsRequired pins that nothing about which account
// a deployment writes to is defaulted: a driver that guessed an endpoint or
// went looking for credentials would start against a store nobody chose.
func TestWhatTheOperatorMustStateIsRequired(t *testing.T) {
	server := azfake.New()
	t.Cleanup(server.Close)
	whole := Options{Endpoint: server.URL(), AccountName: server.AccountName(), AccountKey: server.AccountKey()}
	if _, err := New(testContainer, whole); err != nil {
		t.Fatalf("PREMISE: the options this test takes fields away from are refused whole: %v", err)
	}
	for name, change := range map[string]func(*Options){
		"no endpoint":           func(o *Options) { o.Endpoint = "" },
		"an endpoint not a URL": func(o *Options) { o.Endpoint = "account.blob.core.windows.net" },
		"no account name":       func(o *Options) { o.AccountName = "" },
		"no account key":        func(o *Options) { o.AccountKey = "" },
		"a key not base64":      func(o *Options) { o.AccountKey = "not base64!" },
		"a block above 4000MiB": func(o *Options) { o.BlockBytes = 4001 << 20 },
	} {
		options := whole
		change(&options)
		if _, err := New(testContainer, options); err == nil {
			t.Errorf("%s: the driver opened anyway", name)
		}
	}
	if _, err := New("", whole); err == nil {
		t.Error("no container: the driver opened anyway")
	}
}

// TestAMissingContainerIsNotAnObjectThatIsNotFound protects the difference
// between "no such object" and "no such container". Both are 404 on the wire.
// Reported as ErrNotFound, a mistyped container name would read as an empty
// store: every repository absent, and the first push creating it anew.
func TestAMissingContainerIsNotAnObjectThatIsNotFound(t *testing.T) {
	server := azfake.New()
	t.Cleanup(server.Close)
	opened, err := New("never-created", Options{Endpoint: server.URL(), AccountName: server.AccountName(), AccountKey: server.AccountKey()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	_, headErr := opened.Head(ctx, "HEAD")
	_, _, getErr := opened.Get(ctx, "HEAD")
	listErr := opened.List(ctx, "", func(objstore.Entry) error { return nil })
	for operation, err := range map[string]error{"head": headErr, "get": getErr, "list": listErr} {
		if err == nil || errors.Is(err, objstore.ErrNotFound) || !bloberror.HasCode(err, bloberror.ContainerNotFound) {
			t.Errorf("%s in a container that does not exist answered %v, want the service's ContainerNotFound and not ErrNotFound", operation, err)
		}
	}
}

// TestAnUploadInBlocksIsStillConditional pins what Azure allows and S3 does
// not: the commit of staged blocks carries the condition, so a writer that does
// not know its size — or has more than a block — can still create-if-absent and
// compare-and-swap. It also pins the size at which an upload goes in blocks.
func TestAnUploadInBlocksIsStillConditional(t *testing.T) {
	const blockBytes = 1 << 10
	opened, _, metered := newBucket(t, blockBytes)
	ctx := context.Background()
	body := bytes.Repeat([]byte("0123456789abcdef"), 3*blockBytes/16+1)

	if _, err := opened.Put(ctx, "whole", bytes.NewReader(body[:blockBytes]), blockBytes, objstore.Always, nil); err != nil {
		t.Fatalf("put a block's worth: %v", err)
	}
	if staged := len(metered.sent(http.MethodPut, "block")); staged != 0 {
		t.Fatalf("an object of exactly one block took %d block requests, want one Put Blob", staged)
	}

	first, err := opened.Put(ctx, "streamed", io.MultiReader(bytes.NewReader(body)), -1, objstore.IfAbsent(), objstore.Metadata{"kept": "beside"})
	if err != nil {
		t.Fatalf("create from a stream: %v", err)
	}
	if staged := len(metered.sent(http.MethodPut, "block")); staged != 4 {
		t.Fatalf("PREMISE: %d bytes in blocks of %d took %d Put Block requests, want 4 — the upload did not go in blocks", len(body), blockBytes, staged)
	}
	if _, err := opened.Put(ctx, "streamed", bytes.NewReader([]byte("usurper")), -1, objstore.IfAbsent(), nil); !errors.Is(err, objstore.ErrConditionNotMet) {
		t.Fatalf("a streamed create of what exists answered %v, want ErrConditionNotMet", err)
	}
	if _, err := opened.Put(ctx, "streamed", bytes.NewReader(body), int64(len(body)), objstore.IfVersion(first+"stale"), nil); !errors.Is(err, objstore.ErrConditionNotMet) {
		t.Fatalf("a swap in blocks at a stale version answered %v, want ErrConditionNotMet", err)
	}
	read, info, err := opened.Get(ctx, "streamed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(read)
	_ = read.Close()
	if !bytes.Equal(got, body) || info.Version != first || info.Metadata["kept"] != "beside" {
		t.Fatalf("after two refused writes the object is %d bytes at %q with %v, want the %d first written at %q", len(got), info.Version, info.Metadata, len(body), first)
	}
	if _, err := opened.Put(ctx, "streamed", bytes.NewReader(body), int64(len(body)), objstore.IfVersion(first), nil); err != nil {
		t.Fatalf("a swap in blocks at the current version: %v", err)
	}
}

// TestABodyThatIsNotTheSizeItWasSaidToBeIsNotWritten protects a caller that
// miscounts from a store holding a truncated object under a good name: git
// would find the pack corrupt long after the push that wrote it succeeded.
func TestABodyThatIsNotTheSizeItWasSaidToBeIsNotWritten(t *testing.T) {
	const blockBytes = 1 << 10
	opened, server, _ := newBucket(t, blockBytes)
	ctx := context.Background()
	for name, size := range map[string]int64{"short-of-one-request": 100, "short-of-several-blocks": 4 * blockBytes} {
		if _, err := opened.Put(ctx, name, strings.NewReader("only this"), size, objstore.Always, nil); err == nil {
			t.Errorf("%s: nine bytes were accepted as %d", name, size)
		}
	}
	if _, err := opened.Put(ctx, "longer", bytes.NewReader(make([]byte, 3*blockBytes)), 2*blockBytes, objstore.Always, nil); err == nil {
		t.Errorf("%d bytes were accepted as %d", 3*blockBytes, 2*blockBytes)
	}
	if names := server.BlobNames(testContainer); len(names) != 0 {
		t.Errorf("refused writes left %v", names)
	}
}

// TestTwoStreamsToOneKeyStageDifferentBlocks protects concurrent writers of one
// key from each other. Azure keeps staged blocks under the blob's name, not
// under an upload, so writers that both numbered from zero would overwrite one
// another's blocks and one would commit a mixture of the two bodies.
func TestTwoStreamsToOneKeyStageDifferentBlocks(t *testing.T) {
	opened, _, metered := newBucket(t, 1<<10)
	ctx := context.Background()
	for range 2 {
		if _, err := opened.Put(ctx, "contended", bytes.NewReader(make([]byte, 2<<10)), -1, objstore.Always, nil); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	staged := map[string]bool{}
	for _, request := range metered.sent(http.MethodPut, "block") {
		staged[request.query.Get("blockid")] = true
	}
	if len(staged) != 4 {
		t.Fatalf("two uploads of two blocks staged %d distinct block IDs, want 4: %v", len(staged), staged)
	}
}

// TestABulkDeleteGoesInBatchesOf256 pins the cost of deleting a repository:
// one request for every 256 keys, not one for each. It also pins that the
// driver reads the answer to each part of a batch, since the batch as a whole
// reports success whatever its parts did.
func TestABulkDeleteGoesInBatchesOf256(t *testing.T) {
	opened, server, metered := newBucket(t, 0)
	var keys []string
	for i := range 600 {
		key := fmt.Sprintf("repo/objects/%04d", i)
		server.Put(testContainer, key, []byte("x"))
		keys = append(keys, key)
	}
	server.Put(testContainer, "repo-kept/HEAD", []byte("x"))
	if held := len(server.BlobNames(testContainer)); held != 601 {
		t.Fatalf("PREMISE: the store holds %d blobs before the delete, want 601", held)
	}
	if err := opened.DeleteMany(context.Background(), append(keys, "repo/objects/never-there")); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if left := server.BlobNames(testContainer); len(left) != 1 || left[0] != "repo-kept/HEAD" {
		t.Fatalf("after the delete the store holds %v, want only the key not named", left)
	}
	if batches, singles := len(metered.sent(http.MethodPost, "batch")), len(metered.sent(http.MethodDelete, "")); batches != 3 || singles != 0 {
		t.Fatalf("601 keys took %d batch requests and %d single deletes, want 3 and 0", batches, singles)
	}
}

// TestACopyStillInProgressIsWaitedFor protects a fork from reading packs that
// are not there yet. Azure acknowledges a copy before the bytes have moved; a
// driver that returned on the acknowledgement would hand back a repository whose
// objects appear some time later.
func TestACopyStillInProgressIsWaitedFor(t *testing.T) {
	opened, server, metered := newBucket(t, 0)
	opened.copyPoll = time.Millisecond
	ctx := context.Background()
	server.Put(testContainer, "source", []byte("a pack"))

	server.SetPendingCopyPolls(3)
	if err := opened.Copy(ctx, "source", "fork"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	// Three reads say pending and the fourth says success.
	if polls := len(metered.sent(http.MethodHead, "")); polls != 4 {
		t.Fatalf("PREMISE: a copy reported pending 3 times was asked after %d times, want 4 — the driver did not wait on it", polls)
	}

	// A copy that never finishes is waited on for as long as the caller allows.
	server.SetPendingCopyPolls(1 << 30)
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := opened.Copy(bounded, "source", "never"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a copy that stays pending past the deadline answered %v, want the context's error", err)
	}
}

// TestASignedURLIsRefusedWithoutAnExpiryAhead pins that a URL is never signed
// to expire at or before the moment it was made, which the service would
// accept the signing of and then refuse every use of.
func TestASignedURLIsRefusedWithoutAnExpiryAhead(t *testing.T) {
	opened, _, _ := newBucket(t, 0)
	for _, expiry := range []time.Duration{0, -time.Minute} {
		if signed, err := opened.PresignGet(context.Background(), "pack", expiry); err == nil {
			t.Errorf("an expiry of %s was signed: %s", expiry, signed)
		}
	}
}

// TestADirectoryListingStaysInOrderAcrossPages protects the order ListDirectory
// promises. The client library hands each page over as a list of blobs and a
// list of prefixes, so the driver has to merge them, page by page; visiting all
// of a page's blobs and then its prefixes would pass any listing too small to
// have both, and hand a larger one over out of order.
func TestADirectoryListingStaysInOrderAcrossPages(t *testing.T) {
	opened, server, metered := newBucket(t, 0)
	var want []string
	for i := range 1200 {
		server.Put(testContainer, fmt.Sprintf("repo/e%04d", i), []byte("x"))
		server.Put(testContainer, fmt.Sprintf("repo/e%04d/below", i), []byte("x"))
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
	if pages := len(metered.sent(http.MethodGet, "list")); pages != 3 {
		t.Fatalf("PREMISE: 2400 entries came in %d pages, want 3 — the listing never crossed a page", pages)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the directory listing returned %d entries, want the %d written with each blob before the prefix that extends its name", len(got), len(want))
	}
}

// TestARangeAtTheEndIsNotSatisfiableWhateverTheStoreAnswers pins a rule the
// driver holds itself. The interface says a range that starts at the end of an
// object is not satisfiable, and the engine's pack reader reads to a pack's end
// by being told so. The service says it with a 416; Azurite answers success
// with no bytes, which the suite found the first time it ran against the
// emulator. The response carries the blob's whole size either way, so the
// driver decides by that.
func TestARangeAtTheEndIsNotSatisfiableWhateverTheStoreAnswers(t *testing.T) {
	fake := azfake.New()
	t.Cleanup(fake.Close)
	fake.CreateContainer("c")
	fake.Put("c", "pack", []byte("0123456789"))
	bucket, err := New("c", Options{Endpoint: fake.URL(), AccountName: fake.AccountName(), AccountKey: fake.AccountKey()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()

	if _, _, err := bucket.GetRange(ctx, "pack", 10, 4); !errors.Is(err, objstore.ErrRangeNotSatisfiable) {
		t.Fatalf("premise: a store that answers 416 gave %v", err)
	}
	fake.AnswerARangeAtTheEndWithNoBytes()
	if _, _, err := bucket.GetRange(ctx, "pack", 10, 4); !errors.Is(err, objstore.ErrRangeNotSatisfiable) {
		t.Fatalf("a store that answers success with no bytes gave %v, want ErrRangeNotSatisfiable", err)
	}
	body, info, err := bucket.GetRange(ctx, "pack", 9, 4)
	if err != nil {
		t.Fatalf("the last byte: %v", err)
	}
	last, _ := io.ReadAll(body)
	_ = body.Close()
	if string(last) != "9" || info.Size != 10 {
		t.Fatalf("the last byte read as %q of %d", last, info.Size)
	}
}
