package gcsclient_test

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
	"mime"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/gcsclient"
	"github.com/e6qu/bleephub/gcsclient/gcsfake"
)

const (
	held = "held"
	// quantum is the smallest chunk the service allows, which keeps the uploads
	// here that go in chunks small.
	quantum = 256 << 10
)

// meter records every request the client sends, through the Transport option
// that exists so a harness can do exactly this.
type meter struct {
	mu       sync.Mutex
	requests []metered
}

type metered struct {
	method, path, query string
	header              http.Header
}

func (m *meter) RoundTrip(request *http.Request) (*http.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, metered{request.Method, request.URL.Path, request.URL.RawQuery, request.Header.Clone()})
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

func newClient(t *testing.T, chunkBytes int64) (*gcsclient.Client, *gcsfake.Server, *meter) {
	t.Helper()
	server := gcsfake.New()
	t.Cleanup(server.Close)
	server.CreateBucket(held)
	requests := &meter{}
	client, err := gcsclient.New(gcsclient.Options{
		Endpoint:        server.URL(),
		CredentialsJSON: server.CredentialsJSON(),
		Transport:       requests,
		ChunkBytes:      chunkBytes,
	})
	if err != nil {
		t.Fatalf("a client for the fake: %v", err)
	}
	return client, server, requests
}

func insert(t *testing.T, client *gcsclient.Client, name, body string, options gcsclient.InsertOptions) (gcsclient.Object, error) {
	t.Helper()
	options.Size = int64(len(body))
	return client.Insert(context.Background(), held, name, strings.NewReader(body), options)
}

func mustInsert(t *testing.T, client *gcsclient.Client, name, body string) gcsclient.Object {
	t.Helper()
	written, err := insert(t, client, name, body, gcsclient.InsertOptions{})
	if err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
	return written
}

func contents(t *testing.T, client *gcsclient.Client, name string) (string, gcsclient.Object) {
	t.Helper()
	body, described, err := client.Get(context.Background(), held, name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	content, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content), described
}

// pattern is a body that no two offsets of which read alike for long, so that a
// chunk put in the wrong place shows.
func pattern(size int) []byte {
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*7 + i/251)
	}
	return body
}

// TestWhatTheCallerMustStateIsRequired pins that nothing about which service a
// program talks to, or as whom, is defaulted: a client that guessed an endpoint
// or went looking for credentials would run against a store nobody chose.
func TestWhatTheCallerMustStateIsRequired(t *testing.T) {
	server := gcsfake.New()
	t.Cleanup(server.Close)
	whole := gcsclient.Options{Endpoint: server.URL(), CredentialsJSON: server.CredentialsJSON()}
	if _, err := gcsclient.New(whole); err != nil {
		t.Fatalf("PREMISE: the options this test takes things away from are refused whole: %v", err)
	}
	edited := func(edit func(map[string]any)) []byte {
		var file map[string]any
		if err := json.Unmarshal(server.CredentialsJSON(), &file); err != nil {
			t.Fatalf("the fake's key file: %v", err)
		}
		edit(file)
		encoded, err := json.Marshal(file)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return encoded
	}
	for name, change := range map[string]func(*gcsclient.Options){
		"no endpoint":                   func(o *gcsclient.Options) { o.Endpoint = "" },
		"an endpoint with no scheme":    func(o *gcsclient.Options) { o.Endpoint = "storage.googleapis.com" },
		"an endpoint with a path":       func(o *gcsclient.Options) { o.Endpoint = server.URL() + "/storage/v1" },
		"an endpoint with a query":      func(o *gcsclient.Options) { o.Endpoint = server.URL() + "?alt=json" },
		"no credentials":                func(o *gcsclient.Options) { o.CredentialsJSON = nil },
		"credentials that are not JSON": func(o *gcsclient.Options) { o.CredentialsJSON = []byte("not a key file") },
		"a user's credentials, not a service account's": func(o *gcsclient.Options) {
			o.CredentialsJSON = edited(func(file map[string]any) { file["type"] = "authorized_user" })
		},
		"a key file that names no token endpoint": func(o *gcsclient.Options) {
			o.CredentialsJSON = edited(func(file map[string]any) { delete(file, "token_uri") })
		},
		"a key file that names no account": func(o *gcsclient.Options) {
			o.CredentialsJSON = edited(func(file map[string]any) { delete(file, "client_email") })
		},
		"a key file whose key is not PEM": func(o *gcsclient.Options) {
			o.CredentialsJSON = edited(func(file map[string]any) { file["private_key"] = "not PEM" })
		},
		"a key file whose key is not a key": func(o *gcsclient.Options) {
			o.CredentialsJSON = edited(func(file map[string]any) {
				file["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not DER")}))
			})
		},
		"a chunk that is not a multiple of 256 KiB": func(o *gcsclient.Options) { o.ChunkBytes = quantum + 1 },
		"a negative chunk":                          func(o *gcsclient.Options) { o.ChunkBytes = -quantum },
	} {
		options := whole
		change(&options)
		if _, err := gcsclient.New(options); err == nil {
			t.Errorf("%s: a client was made anyway", name)
		}
	}
}

// TestEveryRequestCarriesATokenTheServiceIssued protects the one authentication
// path: the key file is exchanged for a token at the endpoint it names, once,
// and a key the service does not know is an error and not an anonymous request.
func TestEveryRequestCarriesATokenTheServiceIssued(t *testing.T) {
	client, server, requests := newClient(t, 0)
	ctx := context.Background()
	mustInsert(t, client, "one", "1")
	mustInsert(t, client, "two", "2")
	if _, err := client.Stat(ctx, held, "one"); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if exchanges := len(requests.sent(http.MethodPost, "/token")); exchanges != 1 {
		t.Fatalf("three requests took %d token exchanges, want the one whose token is reused until it expires", exchanges)
	}
	for _, request := range requests.sent(http.MethodPost, "/upload/") {
		if !strings.HasPrefix(request.header.Get("Authorization"), "Bearer ") {
			t.Fatalf("an upload went without a bearer token: %v", request.header)
		}
	}

	// The same key file with another private key in it: what a revoked or a
	// mistaken key looks like to the service.
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(stranger)
	if err != nil {
		t.Fatalf("encode the key: %v", err)
	}
	var file map[string]any
	if err := json.Unmarshal(server.CredentialsJSON(), &file); err != nil {
		t.Fatalf("the fake's key file: %v", err)
	}
	file["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	forged, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	unknown, err := gcsclient.New(gcsclient.Options{Endpoint: server.URL(), CredentialsJSON: forged})
	if err != nil {
		t.Fatalf("a client is made without asking the service anything, so this key is not yet known to be wrong: %v", err)
	}
	if _, err := unknown.Stat(ctx, held, "one"); err == nil || !strings.Contains(err.Error(), "Invalid JWT Signature") {
		t.Fatalf("a request signed for by a key the service does not know answered %v, want the token endpoint's refusal", err)
	}
}

// TestAnObjectIsDescribedAlikeWhereverItIsLearnedOf protects revalidation: a
// caller that lists, then reads, then writes on a precondition compares
// generations learned three ways, and they must be one number. It also pins
// that the metadata and the whole size come with a read, not only with a Stat.
func TestAnObjectIsDescribedAlikeWhereverItIsLearnedOf(t *testing.T) {
	client, _, _ := newClient(t, 0)
	ctx := context.Background()
	written, err := insert(t, client, "dir/asset", "0123456789", gcsclient.InsertOptions{
		ContentType: "text/plain",
		Metadata:    map[string]string{"sha256": "3q2+7w/kept=beside", "second": "value"},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if written.Generation <= 0 || written.Size != 10 || written.Updated.IsZero() || written.Bucket != held || written.Name != "dir/asset" {
		t.Fatalf("the write was described as %+v", written)
	}

	content, got := contents(t, client, "dir/asset")
	if content != "0123456789" {
		t.Fatalf("read back %q", content)
	}
	stat, err := client.Stat(ctx, held, "dir/asset")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	body, ranged, err := client.GetRange(ctx, held, "dir/asset", 2, 4)
	if err != nil {
		t.Fatalf("ranged get: %v", err)
	}
	part, _ := io.ReadAll(body)
	_ = body.Close()
	if string(part) != "2345" {
		t.Fatalf("bytes 2-5 read as %q", part)
	}
	var listed []gcsclient.Entry
	if err := client.List(ctx, held, gcsclient.ListOptions{Prefix: "dir/"}, func(entry gcsclient.Entry) error {
		listed = append(listed, entry)
		return nil
	}); err != nil || len(listed) != 1 {
		t.Fatalf("list: %v, %d entries", err, len(listed))
	}
	for where, described := range map[string]gcsclient.Object{"get": got, "stat": stat, "ranged get": ranged, "list": listed[0].Object} {
		if described.Generation != written.Generation || described.Size != 10 || described.Updated.IsZero() || described.Name != "dir/asset" || described.Bucket != held {
			t.Errorf("a %s describes the object as %+v, the write as %+v", where, described, written)
		}
	}
	for where, described := range map[string]gcsclient.Object{"get": got, "stat": stat, "ranged get": ranged} {
		if described.Metadata["sha256"] != "3q2+7w/kept=beside" || described.Metadata["second"] != "value" || len(described.Metadata) != 2 {
			t.Errorf("a %s carries the metadata %v", where, described.Metadata)
		}
	}

	rewritten := mustInsert(t, client, "dir/asset", "0123456789")
	if rewritten.Generation == written.Generation {
		t.Error("writing the same bytes again did not change the generation: a generation is a version, not a hash")
	}
	if _, described := contents(t, client, "dir/asset"); len(described.Metadata) != 0 {
		t.Errorf("a rewrite with no metadata kept the old object's: %v", described.Metadata)
	}
}

// TestARangeIsCutShortAtTheEndAndRefusedPastIt pins the three answers a ranged
// read has: the bytes, fewer bytes where the object ends first, and
// ErrRangeNotSatisfiable where it starts at or past the end. It also pins what
// the client reports of a store that answers the last of those with success and
// no bytes, as an emulator may: the whole size, by which a caller can tell.
func TestARangeIsCutShortAtTheEndAndRefusedPastIt(t *testing.T) {
	client, server, _ := newClient(t, 0)
	ctx := context.Background()
	mustInsert(t, client, "pack", "0123456789")
	for _, extent := range []struct {
		offset, length int64
		want           string
	}{{0, 4, "0123"}, {8, 100, "89"}, {9, 1, "9"}} {
		body, described, err := client.GetRange(ctx, held, "pack", extent.offset, extent.length)
		if err != nil {
			t.Fatalf("range %d+%d: %v", extent.offset, extent.length, err)
		}
		part, _ := io.ReadAll(body)
		_ = body.Close()
		if string(part) != extent.want || described.Size != 10 {
			t.Errorf("range %d+%d read %q of %d, want %q of 10", extent.offset, extent.length, part, described.Size, extent.want)
		}
	}
	for _, offset := range []int64{10, 11, 1 << 40} {
		_, _, err := client.GetRange(ctx, held, "pack", offset, 4)
		var refused *gcsclient.Error
		if !errors.Is(err, gcsclient.ErrRangeNotSatisfiable) || !errors.As(err, &refused) || refused.Reason != "InvalidRange" {
			t.Errorf("a range starting at %d of 10 bytes answered %v, want ErrRangeNotSatisfiable", offset, err)
		}
	}
	for name, extent := range map[string][2]int64{"a negative offset": {-1, 4}, "no length": {0, 0}, "a negative length": {0, -4}} {
		if _, _, err := client.GetRange(ctx, held, "pack", extent[0], extent[1]); err == nil || errors.Is(err, gcsclient.ErrRangeNotSatisfiable) {
			t.Errorf("%s answered %v, want a refusal that is the caller's mistake and not the object's size", name, err)
		}
	}

	server.AnswerARangeAtTheEndWithNoBytes()
	body, described, err := client.GetRange(ctx, held, "pack", 10, 4)
	if err != nil {
		t.Fatalf("a store that answers a range at the end with success gave %v", err)
	}
	part, _ := io.ReadAll(body)
	_ = body.Close()
	if len(part) != 0 || described.Size != 10 {
		t.Fatalf("read %q of %d, want nothing of 10", part, described.Size)
	}
}

// TestAbsenceIsNotFoundAndARefusalSaysWhatTheServiceSaid pins the typed error
// for a 404 on every operation that can meet one, and that the service's own
// account of a refusal — its reason and its message, in either API's format —
// reaches whoever reads the error.
func TestAbsenceIsNotFoundAndARefusalSaysWhatTheServiceSaid(t *testing.T) {
	client, _, _ := newClient(t, 0)
	ctx := context.Background()
	mustInsert(t, client, "present", "x")
	nothing := func(gcsclient.Entry) error { return nil }

	_, _, getErr := client.Get(ctx, held, "absent")
	_, _, rangeErr := client.GetRange(ctx, held, "absent", 0, 4)
	_, statErr := client.Stat(ctx, held, "absent")
	_, copyErr := client.Copy(ctx, held, "absent", held, "nowhere")
	_, _, otherBucketGet := client.Get(ctx, "never-created", "present")
	_, otherBucketStat := client.Stat(ctx, "never-created", "present")
	_, otherBucketInsert := client.Insert(ctx, "never-created", "x", strings.NewReader("x"), gcsclient.InsertOptions{Size: 1})
	_, otherBucketStream := client.Insert(ctx, "never-created", "x", strings.NewReader("x"), gcsclient.InsertOptions{Size: gcsclient.SizeUnknown})
	for operation, want := range map[string]struct {
		err    error
		reason string
	}{
		"get":                        {getErr, "NoSuchKey"},
		"ranged get":                 {rangeErr, "NoSuchKey"},
		"stat":                       {statErr, "notFound"},
		"delete":                     {client.Delete(ctx, held, "absent"), "notFound"},
		"copy":                       {copyErr, "notFound"},
		"get in a missing bucket":    {otherBucketGet, "NoSuchBucket"},
		"stat in a missing bucket":   {otherBucketStat, "notFound"},
		"insert in a missing bucket": {otherBucketInsert, "notFound"},
		"stream to a missing bucket": {otherBucketStream, "notFound"},
		"list a missing bucket":      {client.List(ctx, "never-created", gcsclient.ListOptions{}, nothing), "notFound"},
	} {
		var refused *gcsclient.Error
		if !errors.Is(want.err, gcsclient.ErrNotFound) || !errors.As(want.err, &refused) {
			t.Errorf("%s answered %v, want ErrNotFound", operation, want.err)
			continue
		}
		if refused.Status != http.StatusNotFound || refused.Reason != want.reason || refused.Message == "" || !strings.Contains(want.err.Error(), refused.Message) {
			t.Errorf("%s was refused as %+v, want the service's 404 %s and its message", operation, refused, want.reason)
		}
		if errors.Is(want.err, gcsclient.ErrPreconditionFailed) || errors.Is(want.err, gcsclient.ErrRangeNotSatisfiable) {
			t.Errorf("%s: a 404 matches more than ErrNotFound", operation)
		}
	}
	if _, err := client.Stat(ctx, held, "nowhere"); !errors.Is(err, gcsclient.ErrNotFound) {
		t.Errorf("copying what is not there left %v at the destination", err)
	}
}

// TestPreconditionsArbitrateBetweenWriters pins create-if-absent and
// compare-and-swap, which are what a caller builds a lock or a reference out
// of: exactly one of two contenders may win, and the loser must be told.
func TestPreconditionsArbitrateBetweenWriters(t *testing.T) {
	client, _, requests := newClient(t, 0)
	first, err := insert(t, client, "ref", "one", gcsclient.InsertOptions{Precondition: gcsclient.DoesNotExist()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sent := requests.sent(http.MethodPost, "/upload/"); len(sent) != 1 || !strings.Contains(sent[0].query, "ifGenerationMatch=0") {
		t.Fatalf("PREMISE: a create-if-absent was sent as %v, want one request with ifGenerationMatch=0", sent)
	}
	refusedWrites := map[string]gcsclient.Precondition{
		"a second create":                 gcsclient.DoesNotExist(),
		"a swap at a generation not held": gcsclient.GenerationMatch(first.Generation + 1),
	}
	for name, precondition := range refusedWrites {
		_, err := insert(t, client, "ref", "usurper", gcsclient.InsertOptions{Precondition: precondition})
		var refused *gcsclient.Error
		if !errors.Is(err, gcsclient.ErrPreconditionFailed) || !errors.As(err, &refused) || refused.Reason != "conditionNotMet" {
			t.Errorf("%s answered %v, want ErrPreconditionFailed", name, err)
		}
	}
	if _, err := insert(t, client, "never-written", "x", gcsclient.InsertOptions{Precondition: gcsclient.GenerationMatch(first.Generation)}); !errors.Is(err, gcsclient.ErrPreconditionFailed) {
		t.Errorf("a swap of an object that does not exist answered %v, want ErrPreconditionFailed", err)
	}
	if content, described := contents(t, client, "ref"); content != "one" || described.Generation != first.Generation {
		t.Fatalf("refused writes left %q at generation %d", content, described.Generation)
	}
	second, err := insert(t, client, "ref", "two", gcsclient.InsertOptions{Precondition: gcsclient.GenerationMatch(first.Generation)})
	if err != nil || second.Generation == first.Generation {
		t.Fatalf("a swap at the generation held returned %+v, %v", second, err)
	}
	if _, err := insert(t, client, "ref", "three", gcsclient.InsertOptions{Precondition: gcsclient.GenerationMatch(first.Generation)}); !errors.Is(err, gcsclient.ErrPreconditionFailed) {
		t.Errorf("a swap at a stale generation answered %v, want ErrPreconditionFailed", err)
	}
	if content, _ := contents(t, client, "ref"); content != "two" {
		t.Fatalf("after the swap the object holds %q", content)
	}
}

// TestAnUploadGoesInOneRequestOrInChunksByItsSize pins the cost and the
// correctness of both ways of writing: a body of known size that fits a chunk is
// one request; anything else is a resumable upload whose chunks are each the
// size chosen, in order, and whose bytes arrive exactly — including the bodies
// that end on a chunk's boundary, where an upload that could not tell its last
// chunk from its others would never complete.
func TestAnUploadGoesInOneRequestOrInChunksByItsSize(t *testing.T) {
	ctx := context.Background()
	for name, upload := range map[string]struct {
		bytes      int
		sizeKnown  bool
		wantChunks int
	}{
		"a known size that fits a chunk":               {quantum, true, 0},
		"a known size of nothing":                      {0, true, 0},
		"a known size above a chunk":                   {2*quantum + 12345, true, 3},
		"a known size of whole chunks":                 {2 * quantum, true, 2},
		"an unknown size below a chunk":                {100, false, 1},
		"an unknown size of exactly a chunk":           {quantum, false, 1},
		"an unknown size above a chunk":                {2*quantum + 12345, false, 3},
		"an unknown size of whole chunks":              {3 * quantum, false, 3},
		"an unknown size that turns out to be nothing": {0, false, 1},
	} {
		t.Run(name, func(t *testing.T) {
			client, server, requests := newClient(t, quantum)
			body := pattern(upload.bytes)
			options := gcsclient.InsertOptions{Size: gcsclient.SizeUnknown, Metadata: map[string]string{"kept": "beside"}, ContentType: "application/x-git-pack"}
			if upload.sizeKnown {
				options.Size = int64(len(body))
			}
			// A reader that hides its length, as a pipe does.
			written, err := client.Insert(ctx, held, "pack", io.MultiReader(bytes.NewReader(body)), options)
			if err != nil {
				t.Fatalf("insert: %v", err)
			}
			stored, _ := server.Contents(held, "pack")
			if !bytes.Equal(stored, body) || written.Size != int64(len(body)) {
				t.Fatalf("%d bytes were sent and %d stored, described as %d; or they differ", len(body), len(stored), written.Size)
			}
			if _, described := contents(t, client, "pack"); described.Metadata["kept"] != "beside" || described.Generation != written.Generation {
				t.Errorf("the object read back as %+v, the write was %+v", described, written)
			}
			chunks := requests.sent(http.MethodPut, "/upload/")
			posts := requests.sent(http.MethodPost, "/upload/")
			if len(chunks) != upload.wantChunks || len(posts) != 1 {
				t.Fatalf("%d bytes took %d POSTs and %d chunks, want 1 and %d", len(body), len(posts), len(chunks), upload.wantChunks)
			}
			if wantType := map[bool]string{true: "uploadType=multipart", false: "uploadType=resumable"}[upload.wantChunks == 0]; !strings.Contains(posts[0].query, wantType) {
				t.Fatalf("the upload was begun with %q, want %s", posts[0].query, wantType)
			}
			if server.UploadsInProgress() != 0 {
				t.Errorf("a completed upload left %d sessions open", server.UploadsInProgress())
			}
		})
	}
}

// TestAResumableUploadHoldsItsPrecondition pins what makes a large conditional
// write possible: the precondition stated when a resumable upload is initiated
// is applied when its last chunk makes the object. Google's documentation does
// not say this in so many words; the fake, the fake-gcs-server emulator and
// Google's own client libraries all take it to be so.
func TestAResumableUploadHoldsItsPrecondition(t *testing.T) {
	client, server, requests := newClient(t, quantum)
	ctx := context.Background()
	body := pattern(2*quantum + 1)
	stream := func(precondition gcsclient.Precondition, size int64) (gcsclient.Object, error) {
		return client.Insert(ctx, held, "pack", io.MultiReader(bytes.NewReader(body)), gcsclient.InsertOptions{Size: size, Precondition: precondition})
	}

	first, err := stream(gcsclient.DoesNotExist(), gcsclient.SizeUnknown)
	if err != nil {
		t.Fatalf("create from a stream: %v", err)
	}
	if chunks := len(requests.sent(http.MethodPut, "/upload/")); chunks != 3 {
		t.Fatalf("PREMISE: the create took %d chunks, want 3 — the upload was not resumable", chunks)
	}
	if _, err := stream(gcsclient.DoesNotExist(), gcsclient.SizeUnknown); !errors.Is(err, gcsclient.ErrPreconditionFailed) {
		t.Fatalf("a streamed create of what exists answered %v, want ErrPreconditionFailed", err)
	}
	if _, err := stream(gcsclient.GenerationMatch(first.Generation+1), int64(len(body))); !errors.Is(err, gcsclient.ErrPreconditionFailed) {
		t.Fatalf("a swap in chunks at a generation not held answered %v, want ErrPreconditionFailed", err)
	}
	if stat, err := client.Stat(ctx, held, "pack"); err != nil || stat.Generation != first.Generation {
		t.Fatalf("after two refused uploads the object is %+v (%v), want generation %d", stat, err, first.Generation)
	}
	if second, err := stream(gcsclient.GenerationMatch(first.Generation), int64(len(body))); err != nil || second.Generation == first.Generation {
		t.Fatalf("a swap in chunks at the generation held returned %+v, %v", second, err)
	}
	if server.UploadsInProgress() != 0 {
		t.Errorf("%d sessions are still open", server.UploadsInProgress())
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the source failed") }

// TestAnUploadThatFailsLeavesNothingBehind protects a caller from a truncated
// object under a good name, and the service from sessions nobody will finish: a
// body that fails part way, or is not the size it was said to be, writes no
// object, and the session it opened is cancelled rather than left for the week
// the service would keep it.
func TestAnUploadThatFailsLeavesNothingBehind(t *testing.T) {
	client, server, requests := newClient(t, quantum)
	ctx := context.Background()
	body := pattern(3 * quantum)

	failing := io.MultiReader(bytes.NewReader(body[:quantum+quantum/2]), failingReader{})
	if _, err := client.Insert(ctx, held, "interrupted", failing, gcsclient.InsertOptions{Size: gcsclient.SizeUnknown}); err == nil || !strings.Contains(err.Error(), "the source failed") {
		t.Fatalf("a stream that failed part way answered %v, want the source's error", err)
	}
	if chunks, cancels := len(requests.sent(http.MethodPut, "/upload/")), len(requests.sent(http.MethodDelete, "/upload/")); chunks != 1 || cancels != 1 {
		t.Fatalf("PREMISE: the failed stream sent %d chunks and %d cancellations, want 1 and 1 — it did not fail part way through a session", chunks, cancels)
	}

	for name, upload := range map[string]struct {
		sent int
		said int64
	}{
		"short of one request":     {9, 100},
		"longer than one request":  {100, 9},
		"short of several chunks":  {quantum + 9, 3 * quantum},
		"longer than chunks said":  {3 * quantum, 2*quantum + 9},
		"longer by whole chunks":   {3 * quantum, 2 * quantum},
		"short by a whole chunk":   {2 * quantum, 3 * quantum},
		"nothing where some was":   {0, 2 * quantum},
		"a chunk where none was":   {quantum + 1, 0},
		"one byte past one chunk":  {quantum + 1, quantum},
		"one byte short of a pair": {2*quantum - 1, 2 * quantum},
	} {
		if written, err := client.Insert(ctx, held, name, bytes.NewReader(body[:upload.sent]), gcsclient.InsertOptions{Size: upload.said}); err == nil {
			t.Errorf("%s: %d bytes were accepted as %d: %+v", name, upload.sent, upload.said, written)
		}
	}
	if names := server.ObjectNames(held); len(names) != 0 {
		t.Errorf("refused writes left %v", names)
	}
	if open := server.UploadsInProgress(); open != 0 {
		t.Errorf("refused writes left %d upload sessions open", open)
	}

	// A caller that gives up is a failure like any other, and the cancellation
	// must be sent although the caller's context is what ended.
	cancelled, cancel := context.WithCancel(ctx)
	if _, err := client.Insert(cancelled, held, "abandoned", &cancellingReader{body: body, after: quantum + 1, cancel: cancel}, gcsclient.InsertOptions{Size: gcsclient.SizeUnknown}); !errors.Is(err, context.Canceled) {
		t.Fatalf("an upload whose caller gave up answered %v, want the context's error", err)
	}
	if open := server.UploadsInProgress(); open != 0 {
		t.Errorf("an abandoned upload left %d sessions open", open)
	}
}

// cancellingReader gives its body out, and cancels a context once it has given
// more than a number of bytes: a caller that gives up mid-upload.
type cancellingReader struct {
	body   []byte
	given  int
	after  int
	cancel context.CancelFunc
}

func (r *cancellingReader) Read(into []byte) (int, error) {
	if r.given > r.after {
		r.cancel()
	}
	if r.given == len(r.body) {
		return 0, io.EOF
	}
	n := copy(into, r.body[r.given:min(len(r.body), r.given+64<<10)])
	r.given += n
	return n, nil
}

// TestABatchDeleteGoesAHundredToARequestAndReadsEveryAnswer pins the cost of
// deleting many objects — one request for every hundred — and that the outcome
// of each call is read and matched to its name, since the batch as a whole
// reports success whatever its calls did, and answers them in no promised order.
func TestABatchDeleteGoesAHundredToARequestAndReadsEveryAnswer(t *testing.T) {
	client, server, requests := newClient(t, 0)
	var names []string
	for i := range 250 {
		name := fmt.Sprintf("repo/objects/%04d", i)
		if i%50 != 7 {
			server.Put(held, name, []byte("x"), nil)
		}
		names = append(names, name)
	}
	server.Put(held, "repo-kept/HEAD", []byte("x"), nil)
	if stored := len(server.ObjectNames(held)); stored != 246 {
		t.Fatalf("PREMISE: the store holds %d objects before the delete, want 246", stored)
	}

	outcomes, err := client.DeleteBatch(context.Background(), held, names)
	if err != nil || len(outcomes) != len(names) {
		t.Fatalf("batch delete: %v, %d outcomes for %d names", err, len(outcomes), len(names))
	}
	for i, outcome := range outcomes {
		if i%50 == 7 {
			var refused *gcsclient.Error
			if !errors.Is(outcome, gcsclient.ErrNotFound) || !errors.As(outcome, &refused) || !strings.Contains(refused.Message, names[i]) {
				t.Errorf("%s was never there and its outcome is %v, want the ErrNotFound that names it", names[i], outcome)
			}
		} else if outcome != nil {
			t.Errorf("%s: %v", names[i], outcome)
		}
	}
	if left := server.ObjectNames(held); len(left) != 1 || left[0] != "repo-kept/HEAD" {
		t.Fatalf("after the delete the store holds %v, want only the name not given", left)
	}
	if batches, singles := len(requests.sent(http.MethodPost, "/batch/")), len(requests.sent(http.MethodDelete, "/")); batches != 3 || singles != 0 {
		t.Fatalf("250 names took %d batch requests and %d single deletes, want 3 and 0", batches, singles)
	}

	if outcomes, err := client.DeleteBatch(context.Background(), held, nil); err != nil || len(outcomes) != 0 {
		t.Errorf("a batch of nothing: %v, %v", outcomes, err)
	}
	if sent := len(requests.sent(http.MethodPost, "/batch/")); sent != 3 {
		t.Errorf("a batch of nothing was sent: %d batch requests", sent)
	}
	// A batch that fails as a whole says so for every name in it.
	outcomes, err = client.DeleteBatch(context.Background(), "never-created", []string{"a", "b"})
	if err != nil {
		t.Fatalf("a batch whose every call fails is still a batch that was answered: %v", err)
	}
	for i, outcome := range outcomes {
		if !errors.Is(outcome, gcsclient.ErrNotFound) {
			t.Errorf("call %d in a bucket that does not exist: %v", i, outcome)
		}
	}
}

// TestABatchThatFailsAsAWholeSaysSoForEveryNameItDidNotDelete protects a caller
// that deletes by the thousand from taking a failure part way for a success: the
// names in batches already answered keep their own outcomes, and every name
// after carries the failure. It pins too that an answer which does not account
// for every call is a failure, not a batch of successes.
func TestABatchThatFailsAsAWholeSaysSoForEveryNameItDidNotDelete(t *testing.T) {
	_, server, _ := newClient(t, 0)
	var names []string
	for i := range 250 {
		names = append(names, fmt.Sprintf("objects/%04d", i))
		server.Put(held, names[i], []byte("x"), nil)
	}
	batches := 0
	failing, err := gcsclient.New(gcsclient.Options{Endpoint: server.URL(), CredentialsJSON: server.CredentialsJSON(), Transport: tampering(func(r *http.Response) {
		if r.Request.URL.Path != "/batch/storage/v1" {
			return
		}
		if batches++; batches == 2 {
			r.StatusCode = http.StatusServiceUnavailable
			r.Body = io.NopCloser(strings.NewReader("the backend is unavailable"))
		}
	})})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	outcomes, err := failing.DeleteBatch(context.Background(), held, names)
	var refused *gcsclient.Error
	if !errors.As(err, &refused) || refused.Status != http.StatusServiceUnavailable || refused.Message != "the backend is unavailable" {
		t.Fatalf("a batch answered 503 returned %v, want the service's refusal", err)
	}
	if batches != 2 {
		t.Errorf("after a batch failed %d were sent in all, want the sending to stop at the second", batches)
	}
	for i, outcome := range outcomes {
		if (i < 100) != (outcome == nil) {
			t.Fatalf("name %d of 250, with the second batch of a hundred failing, has the outcome %v", i, outcome)
		}
	}

	for name, tamper := range map[string]func(*http.Response){
		"an answer that is not multipart": func(r *http.Response) { r.Header.Set("Content-Type", "text/html") },
		"an answer with a part missing": func(r *http.Response) {
			body, _ := io.ReadAll(r.Body)
			_, parameters, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			parts := strings.Split(string(body), "--"+parameters["boundary"])
			r.Body = io.NopCloser(strings.NewReader(strings.Join(append(parts[:1], parts[2:]...), "--"+parameters["boundary"])))
			r.ContentLength = -1
			r.Header.Del("Content-Length")
		},
		"an answer to a call never made": func(r *http.Response) {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(body), "<response-delete+0>", "<response-delete+7>", 1)))
		},
		"an answer that is not HTTP": func(r *http.Response) {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(body), "HTTP/1.1 ", "GOPHER ", 1)))
		},
	} {
		server.Put(held, "one", []byte("x"), nil)
		server.Put(held, "two", []byte("x"), nil)
		client, err := gcsclient.New(gcsclient.Options{Endpoint: server.URL(), CredentialsJSON: server.CredentialsJSON(), Transport: tampering(func(r *http.Response) {
			if r.Request.URL.Path == "/batch/storage/v1" {
				tamper(r)
			}
		})})
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		if outcomes, err := client.DeleteBatch(context.Background(), held, []string{"one", "two"}); err == nil {
			t.Errorf("%s was taken for a batch answered: %v", name, outcomes)
		}
	}
}

// TestAListingIsWholeOrderedAndFoldedAcrossPages protects the order a listing
// promises. The service sends each page's objects and its folded prefixes as
// two lists, so the client has to merge them, page by page; visiting a page's
// objects and then its prefixes would pass any listing too small to have both,
// and hand a larger one over out of order.
func TestAListingIsWholeOrderedAndFoldedAcrossPages(t *testing.T) {
	client, server, requests := newClient(t, 0)
	ctx := context.Background()
	var folded, flat []string
	for i := range 1200 {
		server.Put(held, fmt.Sprintf("repo/e%04d", i), []byte("x"), nil)
		server.Put(held, fmt.Sprintf("repo/e%04d/below", i), []byte("xx"), nil)
		folded = append(folded, fmt.Sprintf("repo/e%04d", i), fmt.Sprintf("repo/e%04d/ (prefix)", i))
		flat = append(flat, fmt.Sprintf("repo/e%04d", i), fmt.Sprintf("repo/e%04d/below", i))
	}
	server.Put(held, "repo-sibling/HEAD", []byte("x"), nil)
	visit := func(into *[]string) func(gcsclient.Entry) error {
		return func(entry gcsclient.Entry) error {
			name := entry.Name
			if entry.Prefix {
				name += " (prefix)"
				if entry.Size != 0 || entry.Generation != 0 || !entry.Updated.IsZero() {
					t.Errorf("a prefix came described as an object: %+v", entry)
				}
			} else if entry.Generation == 0 || entry.Updated.IsZero() || entry.Bucket != held {
				t.Errorf("an object came without its generation or the time it was written: %+v", entry)
			}
			*into = append(*into, name)
			return nil
		}
	}

	var got []string
	if err := client.List(ctx, held, gcsclient.ListOptions{Prefix: "repo/", Delimiter: "/"}, visit(&got)); err != nil {
		t.Fatalf("list by directory: %v", err)
	}
	if pages := len(requests.sent(http.MethodGet, "/storage/v1/b/held/o")); pages != 3 {
		t.Fatalf("PREMISE: 2400 entries came in %d pages, want 3 — the listing never crossed a page", pages)
	}
	if strings.Join(got, "\n") != strings.Join(folded, "\n") {
		t.Fatalf("the directory listing returned %d entries, want the %d written with each object before the prefix that extends its name", len(got), len(folded))
	}

	got = nil
	if err := client.List(ctx, held, gcsclient.ListOptions{Prefix: "repo/"}, visit(&got)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Join(got, "\n") != strings.Join(flat, "\n") {
		t.Fatalf("the listing returned %d entries, want the %d under the prefix, in order", len(got), len(flat))
	}

	stop := errors.New("enough")
	seen := 0
	if err := client.List(ctx, held, gcsclient.ListOptions{}, func(gcsclient.Entry) error { seen++; return stop }); !errors.Is(err, stop) || seen != 1 {
		t.Errorf("a visit that returned an error saw %d entries and the listing returned %v", seen, err)
	}
}

// TestACopyIsFollowedUntilTheServiceSaysItIsDone protects a fork from packs
// that are not there yet. The service copies what it can within one call and
// hands back a token to go on from; a client that stopped at the first answer
// would report a copy of many GiB made when it had only been begun.
func TestACopyIsFollowedUntilTheServiceSaysItIsDone(t *testing.T) {
	client, server, requests := newClient(t, 0)
	ctx := context.Background()
	body := pattern(3500)
	source, err := client.Insert(ctx, held, "source pack", bytes.NewReader(body), gcsclient.InsertOptions{Size: 3500, Metadata: map[string]string{"kept": "beside"}, ContentType: "application/x-git-pack"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	server.SetRewriteBytesPerCall(1000)
	copied, err := client.Copy(ctx, held, "source pack", held, "fork/pack")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	calls := requests.sent(http.MethodPost, "/storage/v1/b/held/o/")
	if len(calls) != 4 {
		t.Fatalf("PREMISE: a copy of 3500 bytes at 1000 a call took %d calls, want 4 — the client did not have to follow it", len(calls))
	}
	if strings.Contains(calls[0].query, "rewriteToken=") || !strings.Contains(calls[3].query, "rewriteToken=") {
		t.Fatalf("the calls were %v, want every one after the first to carry the token of the one before", calls)
	}
	stored, _ := server.Contents(held, "fork/pack")
	if !bytes.Equal(stored, body) {
		t.Fatalf("the copy holds %d bytes that are not the source's %d", len(stored), len(body))
	}
	if copied.Size != 3500 || copied.Generation == 0 || copied.Generation == source.Generation || copied.Name != "fork/pack" || copied.Metadata["kept"] != "beside" {
		t.Errorf("the copy was described as %+v", copied)
	}
	if _, described := contents(t, client, "fork/pack"); described.Metadata["kept"] != "beside" || described.Generation != copied.Generation {
		t.Errorf("the copy reads back as %+v", described)
	}
}

// TestANameIsCarriedExactlyWhateverItsCharacters protects names from the URL
// they travel in. An object's name is one segment of a JSON API path, a whole
// path to the XML API, and a line of a batch; a character left bare in any of
// them addresses another object, or none.
func TestANameIsCarriedExactlyWhateverItsCharacters(t *testing.T) {
	client, server, _ := newClient(t, quantum)
	ctx := context.Background()
	for _, name := range []string{
		"dir/with space/and+plus",
		"query?and=equals&ampersand#fragment",
		"percent%2Fencoded%already",
		"ünïcödé/日本語",
		"dots/./and/../more",
		"trailing/slash/",
		"quotes\"'<>[]{}|\\^`",
	} {
		written, err := insert(t, client, name, "named", gcsclient.InsertOptions{})
		if err != nil {
			t.Errorf("insert %q: %v", name, err)
			continue
		}
		if stored, ok := server.Contents(held, name); !ok || string(stored) != "named" {
			t.Errorf("%q was stored as %v", name, server.ObjectNames(held))
		}
		if content, described := contents(t, client, name); content != "named" || described.Generation != written.Generation {
			t.Errorf("get %q: %q at %d", name, content, described.Generation)
		}
		if stat, err := client.Stat(ctx, held, name); err != nil || stat.Name != name {
			t.Errorf("stat %q: %+v, %v", name, stat, err)
		}
		if _, err := client.Insert(ctx, held, name+".streamed", strings.NewReader("streamed"), gcsclient.InsertOptions{Size: gcsclient.SizeUnknown}); err != nil {
			t.Errorf("stream to %q: %v", name, err)
		}
		if _, err := client.Copy(ctx, held, name, held, name+".copy"); err != nil {
			t.Errorf("copy %q: %v", name, err)
		}
		signed, err := client.SignedGetURL(held, name, time.Minute)
		if err != nil {
			t.Errorf("sign %q: %v", name, err)
		} else if status, body := fetch(t, signed); status != http.StatusOK || body != "named" {
			t.Errorf("the signed URL of %q answered %d %q", name, status, body)
		}
		var listed []string
		if err := client.List(ctx, held, gcsclient.ListOptions{Prefix: name}, func(entry gcsclient.Entry) error {
			listed = append(listed, entry.Name)
			return nil
		}); err != nil || len(listed) != 3 || listed[0] != name {
			t.Errorf("list %q: %q, %v", name, listed, err)
		}
		if err := client.Delete(ctx, held, name); err != nil {
			t.Errorf("delete %q: %v", name, err)
		}
		if outcomes, err := client.DeleteBatch(ctx, held, []string{name + ".copy", name + ".streamed"}); err != nil || outcomes[0] != nil || outcomes[1] != nil {
			t.Errorf("batch delete of %q's copies: %v, %v", name, outcomes, err)
		}
	}
	if left := server.ObjectNames(held); len(left) != 0 {
		t.Errorf("what was written and deleted by name left %q", left)
	}
}

func fetch(t *testing.T, signed string) (int, string) {
	t.Helper()
	response, err := http.Get(signed) // #nosec G107 -- the URL was just signed by the client under test
	if err != nil {
		t.Fatalf("fetch %s: %v", signed, err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return response.StatusCode, string(body)
}

// TestASignedURLReadsItsObjectForItsTimeAndNothingElse pins V4 signing against
// a verifier: the URL reads the object with no credentials, stops doing so when
// it expires, and cannot be turned to another object. It pins too that a URL is
// never signed for a life the service would refuse.
func TestASignedURLReadsItsObjectForItsTimeAndNothingElse(t *testing.T) {
	client, server, requests := newClient(t, 0)
	mustInsert(t, client, "packs/pack-1", "fetched without credentials")
	mustInsert(t, client, "packs/pack-2", "another object")
	sent := len(requests.requests)

	signed, err := client.SignedGetURL(held, "packs/pack-1", time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if len(requests.requests) != sent {
		t.Errorf("signing a URL sent %d requests; it is done with the key, locally", len(requests.requests)-sent)
	}
	if !strings.HasPrefix(signed, server.URL()+"/held/packs/pack-1?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential="+strings.ReplaceAll(server.ServiceAccount(), "@", "%40")+"%2F") {
		t.Errorf("the URL is %s, want it path-style under the endpoint and signed as the service account", signed)
	}
	if status, body := fetch(t, signed); status != http.StatusOK || body != "fetched without credentials" {
		t.Fatalf("PREMISE: the signed URL answered %d %q, so its refusals below would prove nothing", status, body)
	}
	if status, body := fetch(t, strings.Replace(signed, "pack-1", "pack-2", 1)); status != http.StatusForbidden || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Errorf("the URL turned to another object answered %d %q, want 403 SignatureDoesNotMatch", status, body)
	}
	if status, body := fetch(t, strings.Replace(signed, "X-Goog-Expires=60", "X-Goog-Expires=6000", 1)); status != http.StatusForbidden || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Errorf("the URL with its life extended answered %d %q, want 403 SignatureDoesNotMatch", status, body)
	}
	if status, _ := fetch(t, server.URL()+"/held/packs/pack-1"); status != http.StatusUnauthorized {
		t.Errorf("the object's bare URL answered %d, want 401: the store is not public", status)
	}

	// A client whose clock stopped some years ago signs URLs that have expired.
	stopped, err := gcsclient.New(gcsclient.Options{
		Endpoint:        server.URL(),
		CredentialsJSON: server.CredentialsJSON(),
		Clock:           func() time.Time { return time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("a client with a clock: %v", err)
	}
	expired, err := stopped.SignedGetURL(held, "packs/pack-1", MaximumExpiry)
	if err != nil || !strings.Contains(expired, "X-Goog-Date=20200301T120000Z&X-Goog-Expires=604800&") {
		t.Fatalf("sign with a stopped clock: %s, %v", expired, err)
	}
	if status, body := fetch(t, expired); status != http.StatusBadRequest || !strings.Contains(body, "ExpiredToken") {
		t.Errorf("a URL that expired in 2020 answered %d %q, want 400 ExpiredToken", status, body)
	}

	for name, expiry := range map[string]time.Duration{"no life": 0, "a negative life": -time.Minute, "less than a second": time.Second - 1, "more than seven days": MaximumExpiry + time.Second} {
		if signed, err := client.SignedGetURL(held, "packs/pack-1", expiry); err == nil {
			t.Errorf("%s was signed: %s", name, signed)
		}
	}
	if signed, err := client.SignedGetURL(held, "", time.Minute); err == nil {
		t.Errorf("a URL for no object was signed: %s", signed)
	}
}

// MaximumExpiry is the client's own constant, named here for brevity.
const MaximumExpiry = gcsclient.MaximumSignedURLExpiry

// TestAnAnswerThatIsNotTheServicesIsAnError protects a caller from a proxy, a
// captive portal or an emulator that answers 200 with something else: an
// object described without its generation, size or time is refused rather than
// reported with zeros a caller would go on to compare and expire by.
func TestAnAnswerThatIsNotTheServicesIsAnError(t *testing.T) {
	_, server, _ := newClient(t, 0)
	server.Put(held, "object", []byte("content"), nil)
	for name, tamper := range map[string]func(*http.Response){
		"a download without its generation": func(r *http.Response) { r.Header.Del("X-Goog-Generation") },
		"a download without its time":       func(r *http.Response) { r.Header.Del("Last-Modified") },
		"a ranged download without its total": func(r *http.Response) {
			if r.StatusCode == http.StatusPartialContent {
				r.Header.Set("Content-Range", "bytes 0-3")
			}
		},
		"a description that is not JSON": func(r *http.Response) {
			if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.Request.URL.Path != "/token" {
				r.Body = io.NopCloser(strings.NewReader("<html>sign in to the wifi</html>"))
			}
		},
		"a description without a generation": func(r *http.Response) {
			if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.Request.URL.Path != "/token" {
				r.Body = io.NopCloser(strings.NewReader(`{"name":"object","size":"7","updated":"2026-01-02T03:04:05.678Z","items":[{"name":"object","size":"7"}]}`))
			}
		},
	} {
		client, err := gcsclient.New(gcsclient.Options{Endpoint: server.URL(), CredentialsJSON: server.CredentialsJSON(), Transport: tampering(tamper)})
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		ctx := context.Background()
		_, _, getErr := client.Get(ctx, held, "object")
		_, _, rangeErr := client.GetRange(ctx, held, "object", 0, 4)
		_, statErr := client.Stat(ctx, held, "object")
		_, insertErr := client.Insert(ctx, held, "object", strings.NewReader("x"), gcsclient.InsertOptions{Size: 1})
		listErr := client.List(ctx, held, gcsclient.ListOptions{}, func(gcsclient.Entry) error { return nil })
		failed := 0
		for _, err := range []error{getErr, rangeErr, statErr, insertErr, listErr} {
			if err != nil {
				failed++
			}
		}
		if failed == 0 {
			t.Errorf("%s: every operation succeeded", name)
		}
	}
}

// tampering is a transport that alters every response on its way back.
type tampering func(*http.Response)

func (tamper tampering) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil {
		tamper(response)
	}
	return response, err
}
