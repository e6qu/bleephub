// Package objstoretest is what every objstore driver must pass. A driver is
// accepted into gitstore by passing Run against the store it drives — an
// in-process fake wherever the suite runs, and the store's own emulator or the
// real service where one is to hand — and nothing about a driver is taken on
// trust that this does not check.
//
// It is a package and not a _test file so that a driver kept outside this
// module can be held to it too.
package objstoretest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Run holds a driver to the interface. open returns a bucket that is empty under
// the prefix it is given, which Run chooses so that runs against a shared store
// do not meet.
func Run(t *testing.T, open func(t *testing.T) objstore.Bucket) {
	t.Helper()
	tests := map[string]func(*testing.T, objstore.Bucket, string){
		"TheStartupProbePassesAndLeavesNothingBehind":   probePasses,
		"ConditionalWritesArbitrateBetweenWriters":      conditionalWrites,
		"AVersionIsTheSameTokenWhereverItIsLearned":     versionsAgree,
		"AConditionalReadMovesNoBodyUntilTheObjectDoes": conditionalRead,
		"MetadataTravelsWithAnObject":                   metadataTravels,
		"AbsenceIsNotFoundAndARangeCarriesTheWholeSize": readsAndRanges,
		"ListingsAreWholeOrderedAndFoldedByDirectory":   listings,
		"CopyAndDeletesDoWhatTheySay":                   copyAndDelete,
		"AStreamOfUnknownSizeLandsWholeOrNotAtAll":      streamedUpload,
		"ASignedURLReadsTheObjectWithoutCredentials":    signedURL,
	}
	names := make([]string, 0, len(tests))
	for name := range tests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			tests[name](t, open(t), "objstoretest/"+name+"/")
		})
	}
}

func put(t *testing.T, bucket objstore.Bucket, key, body string, condition objstore.Condition, metadata objstore.Metadata) (objstore.Version, error) {
	t.Helper()
	return bucket.Put(context.Background(), key, strings.NewReader(body), int64(len(body)), condition, metadata)
}

func mustPut(t *testing.T, bucket objstore.Bucket, key, body string) objstore.Version {
	t.Helper()
	version, err := put(t, bucket, key, body, objstore.Always, nil)
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return version
}

func read(t *testing.T, bucket objstore.Bucket, key string) (string, objstore.Info) {
	t.Helper()
	body, info, err := bucket.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	content, err := io.ReadAll(body)
	if closeErr := body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return string(content), info
}

func keysUnder(t *testing.T, bucket objstore.Bucket, prefix string) []string {
	t.Helper()
	var keys []string
	if err := bucket.List(context.Background(), prefix, func(entry objstore.Entry) error {
		keys = append(keys, entry.Key)
		return nil
	}); err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	return keys
}

func probePasses(t *testing.T, bucket objstore.Bucket, prefix string) {
	if err := objstore.Conform(context.Background(), bucket, prefix); err != nil {
		t.Fatalf("the driver's own store was refused: %v", err)
	}
	if left := keysUnder(t, bucket, prefix); len(left) != 0 {
		t.Fatalf("the probe left %v behind", left)
	}
}

func conditionalWrites(t *testing.T, bucket objstore.Bucket, prefix string) {
	key := prefix + "manifest"
	first, err := put(t, bucket, key, "one", objstore.IfAbsent(), nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := put(t, bucket, key, "two", objstore.IfAbsent(), nil); !errors.Is(err, objstore.ErrConditionNotMet) {
		t.Fatalf("a second create answered %v, want ErrConditionNotMet", err)
	}
	if content, _ := read(t, bucket, key); content != "one" {
		t.Fatalf("a refused create changed the object to %q", content)
	}
	second, err := put(t, bucket, key, "two", objstore.IfVersion(first), nil)
	if err != nil || second == first || second == "" {
		t.Fatalf("a swap at the current version returned %q, %v", second, err)
	}
	if _, err := put(t, bucket, key, "three", objstore.IfVersion(first), nil); !errors.Is(err, objstore.ErrConditionNotMet) {
		t.Fatalf("a swap at a stale version answered %v, want ErrConditionNotMet", err)
	}
	if _, err := put(t, bucket, prefix+"never-written", "x", objstore.IfVersion(first), nil); !errors.Is(err, objstore.ErrConditionNotMet) {
		t.Fatalf("a swap of an object that does not exist answered %v, want ErrConditionNotMet", err)
	}
	if content, _ := read(t, bucket, key); content != "two" {
		t.Fatalf("a refused swap changed the object to %q", content)
	}
}

func versionsAgree(t *testing.T, bucket objstore.Bucket, prefix string) {
	key := prefix + "object"
	written := mustPut(t, bucket, key, "content")
	_, got := read(t, bucket, key)
	head, err := bucket.Head(context.Background(), key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	var listed objstore.Entry
	if err := bucket.List(context.Background(), prefix, func(entry objstore.Entry) error {
		listed = entry
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	ranged, rangedInfo, err := bucket.GetRange(context.Background(), key, 0, 3)
	if err != nil {
		t.Fatalf("ranged get: %v", err)
	}
	_ = ranged.Close()
	for where, version := range map[string]objstore.Version{"get": got.Version, "head": head.Version, "list": listed.Version, "ranged get": rangedInfo.Version} {
		if version != written {
			t.Errorf("the version from a %s is %q, the write returned %q: a listing could not revalidate what a read fetched", where, version, written)
		}
	}
	if head.Size != 7 || listed.Size != 7 || got.Size != 7 {
		t.Errorf("sizes: head %d, list %d, get %d; want 7", head.Size, listed.Size, got.Size)
	}
	if listed.ModTime.IsZero() || head.ModTime.IsZero() {
		t.Error("an object came without the time it was written, which expiry depends on")
	}
	if rewritten := mustPut(t, bucket, key, "changed"); rewritten == written {
		t.Error("an object's version did not change when its content did")
	}
}

// conditionalRead holds a driver to the read a manifest's readers make most: is
// the copy I hold still the object? A driver that answered "unchanged" for an
// object that had moved would leave a replica serving references another had
// since updated, for ever; one that sent the body every time would make every
// revalidation cost what a read does; and one that reported a deleted object as
// unchanged would keep serving a repository that is gone.
func conditionalRead(t *testing.T, bucket objstore.Bucket, prefix string) {
	ctx := context.Background()
	key := prefix + "manifest"
	if _, _, err := bucket.GetIfChanged(ctx, prefix+"absent", "1"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("a conditional read of an absent key answered %v, want ErrNotFound", err)
	}
	first := mustPut(t, bucket, key, "sequence one")
	for _, from := range []string{"the write", "a read"} {
		held := first
		if from == "a read" {
			_, info := read(t, bucket, key)
			held = info.Version
		}
		body, _, err := bucket.GetIfChanged(ctx, key, held)
		if !errors.Is(err, objstore.ErrNotModified) {
			t.Fatalf("a conditional read at the version %s returned answered %v, want ErrNotModified", from, err)
		}
		if body != nil {
			t.Fatalf("a conditional read at the version %s returned came with a body", from)
		}
	}

	second := mustPut(t, bucket, key, "sequence two")
	body, info, err := bucket.GetIfChanged(ctx, key, first)
	if err != nil {
		t.Fatalf("a conditional read at a stale version: %v", err)
	}
	content, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(content) != "sequence two" {
		t.Fatalf("a conditional read at a stale version read %q, %v; want the new content", content, err)
	}
	if info.Version != second || info.Size != int64(len("sequence two")) {
		t.Errorf("the changed object came described as version %q of %d bytes, want %q of %d: the reader could not hold it for the next revalidation",
			info.Version, info.Size, second, len("sequence two"))
	}
	if _, _, err := bucket.GetIfChanged(ctx, key, info.Version); !errors.Is(err, objstore.ErrNotModified) {
		t.Errorf("a conditional read at the version the last one returned answered %v, want ErrNotModified", err)
	}

	if err := bucket.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := bucket.GetIfChanged(ctx, key, second); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("a conditional read of a deleted object, at the version it last had, answered %v, want ErrNotFound", err)
	}
}

func metadataTravels(t *testing.T, bucket objstore.Bucket, prefix string) {
	key := prefix + "asset"
	written := objstore.Metadata{"bleephubsha256": "3q2+7w/kept=beside", "second": "value"}
	if _, err := put(t, bucket, key, "bytes", objstore.Always, written); err != nil {
		t.Fatalf("put: %v", err)
	}
	content, info := read(t, bucket, key)
	if content != "bytes" {
		t.Fatalf("an object written with metadata is not exactly its content: %q", content)
	}
	head, err := bucket.Head(context.Background(), key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	for where, got := range map[string]objstore.Metadata{"get": info.Metadata, "head": head.Metadata} {
		for name, want := range written {
			if got[name] != want {
				t.Errorf("metadata %s from a %s is %q, want %q", name, where, got[name], want)
			}
		}
	}
	mustPut(t, bucket, key, "rewritten")
	if _, info := read(t, bucket, key); len(info.Metadata) != 0 {
		t.Errorf("a rewrite with no metadata kept the old object's: %v", info.Metadata)
	}
	for _, name := range []string{"with-hyphen", "Upper", "9digit", "under_score", ""} {
		if _, err := put(t, bucket, prefix+"refused", "x", objstore.Always, objstore.Metadata{name: "v"}); err == nil {
			t.Errorf("metadata name %q was accepted; not every store keeps it as written", name)
		}
	}
	if _, err := bucket.Head(context.Background(), prefix+"refused"); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("a refused write reached the store: %v", err)
	}
}

func readsAndRanges(t *testing.T, bucket objstore.Bucket, prefix string) {
	ctx := context.Background()
	if _, _, err := bucket.Get(ctx, prefix+"absent"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("get of an absent key: %v", err)
	}
	if _, err := bucket.Head(ctx, prefix+"absent"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("head of an absent key: %v", err)
	}
	if _, _, err := bucket.GetRange(ctx, prefix+"absent", 0, 4); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("ranged get of an absent key: %v", err)
	}
	key := prefix + "pack"
	mustPut(t, bucket, key, "0123456789")
	for _, extent := range []struct {
		offset, length int64
		want           string
	}{{0, 4, "0123"}, {2, 4, "2345"}, {8, 100, "89"}, {9, 1, "9"}} {
		body, info, err := bucket.GetRange(ctx, key, extent.offset, extent.length)
		if err != nil {
			t.Fatalf("range %d+%d: %v", extent.offset, extent.length, err)
		}
		part, _ := io.ReadAll(body)
		_ = body.Close()
		if string(part) != extent.want || info.Size != 10 {
			t.Errorf("range %d+%d returned %q of %d, want %q of 10", extent.offset, extent.length, part, info.Size, extent.want)
		}
	}
	if _, _, err := bucket.GetRange(ctx, key, 10, 4); !errors.Is(err, objstore.ErrRangeNotSatisfiable) {
		t.Errorf("a range starting at the end: %v, want ErrRangeNotSatisfiable", err)
	}
	empty := prefix + "empty"
	mustPut(t, bucket, empty, "")
	if content, info := read(t, bucket, empty); content != "" || info.Size != 0 {
		t.Errorf("an empty object read back as %q of %d", content, info.Size)
	}
}

func listings(t *testing.T, bucket objstore.Bucket, prefix string) {
	ctx := context.Background()
	for _, name := range []string{"HEAD", "refs/heads/main", "refs/heads/topic/a", "refs/tags/v1", "refs-not-a-directory"} {
		mustPut(t, bucket, prefix+name, name)
	}
	mustPut(t, bucket, strings.TrimSuffix(prefix, "/")+"-sibling/HEAD", "not under the prefix")

	want := []string{"HEAD", "refs-not-a-directory", "refs/heads/main", "refs/heads/topic/a", "refs/tags/v1"}
	var got []string
	for _, key := range keysUnder(t, bucket, prefix) {
		got = append(got, strings.TrimPrefix(key, prefix))
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("recursive listing = %v, want %v in byte order", got, want)
	}

	var directory []string
	if err := bucket.ListDirectory(ctx, prefix, func(entry objstore.Entry) error {
		name := strings.TrimPrefix(entry.Key, prefix)
		if entry.Prefix {
			name += " (prefix)"
			if entry.Size != 0 || entry.Version != "" {
				t.Errorf("a common prefix came with a size or a version: %+v", entry)
			}
		}
		directory = append(directory, name)
		return nil
	}); err != nil {
		t.Fatalf("directory listing: %v", err)
	}
	if got := strings.Join(directory, ", "); got != "HEAD, refs-not-a-directory, refs/ (prefix)" {
		t.Errorf("directory listing = %s", got)
	}

	stop := errors.New("enough")
	seen := 0
	if err := bucket.List(ctx, prefix, func(objstore.Entry) error { seen++; return stop }); !errors.Is(err, stop) || seen != 1 {
		t.Errorf("a visit that returned an error saw %d entries and the listing returned %v", seen, err)
	}

	// Past one page of any store's listing.
	const many = 1100
	body := []byte("x")
	for i := range many {
		key := prefix + "many/" + string(rune('a'+i/676%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i%26))
		if _, err := bucket.Put(ctx, key, bytes.NewReader(body), 1, objstore.Always, nil); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if listed := keysUnder(t, bucket, prefix+"many/"); len(listed) != many || !sort.StringsAreSorted(listed) {
		t.Errorf("a listing of %d keys returned %d, sorted %v", many, len(listed), sort.StringsAreSorted(listed))
	}
}

func copyAndDelete(t *testing.T, bucket objstore.Bucket, prefix string) {
	ctx := context.Background()
	mustPut(t, bucket, prefix+"source", "copied bytes")
	if err := bucket.Copy(ctx, prefix+"source", prefix+"fork/destination"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if content, _ := read(t, bucket, prefix+"fork/destination"); content != "copied bytes" {
		t.Fatalf("the copy holds %q", content)
	}
	if err := bucket.Copy(ctx, prefix+"absent", prefix+"nowhere"); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("copying what is not there: %v, want ErrNotFound", err)
	}

	if err := bucket.Delete(ctx, prefix+"source"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := bucket.Delete(ctx, prefix+"source"); err != nil {
		t.Errorf("deleting what is already gone: %v, want no error", err)
	}

	var doomed []string
	for i := range 1100 {
		key := prefix + "doomed/" + string(rune('a'+i/676%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i%26))
		mustPut(t, bucket, key, "x")
		doomed = append(doomed, key)
	}
	doomed = append(doomed, prefix+"doomed/never-existed")
	if err := bucket.DeleteMany(ctx, doomed); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if err := bucket.DeleteMany(ctx, nil); err != nil {
		t.Errorf("a bulk delete of nothing: %v", err)
	}
	if left := keysUnder(t, bucket, prefix); len(left) != 1 || left[0] != prefix+"fork/destination" {
		t.Errorf("what is left: %v, want only the copy", left)
	}
}

// streamedBytes is more than any driver sends in one request when it does not
// know the size, so the upload is in parts.
const streamedBytes = 20<<20 + 12345

func streamedUpload(t *testing.T, bucket objstore.Bucket, prefix string) {
	ctx := context.Background()
	key := prefix + "bundle"
	body := make([]byte, 0, streamedBytes+sha256.Size)
	link := sha256.Sum256([]byte("streamed"))
	for len(body) < streamedBytes {
		body = append(body, link[:]...)
		link = sha256.Sum256(link[:])
	}
	body = body[:streamedBytes]

	// A reader that hides its length, as a pipe does.
	if _, err := bucket.Put(ctx, key, io.MultiReader(bytes.NewReader(body)), -1, objstore.Always, nil); err != nil {
		t.Fatalf("streamed put: %v", err)
	}
	info, err := bucket.Head(ctx, key)
	if err != nil || info.Size != streamedBytes {
		t.Fatalf("the streamed object is %d bytes (%v), want %d", info.Size, err, streamedBytes)
	}
	tail, _, err := bucket.GetRange(ctx, key, streamedBytes-1000, 1000)
	if err != nil {
		t.Fatalf("read the tail: %v", err)
	}
	got, _ := io.ReadAll(tail)
	_ = tail.Close()
	if !bytes.Equal(got, body[streamedBytes-1000:]) {
		t.Fatal("the streamed object's last bytes are not the ones sent")
	}

	// A stream that fails part way must leave nothing under the key.
	failing := io.MultiReader(bytes.NewReader(body[:6<<20]), failingReader{})
	if _, err := bucket.Put(ctx, prefix+"interrupted", failing, -1, objstore.Always, nil); err == nil {
		t.Fatal("a stream that failed part way was reported as written")
	}
	if _, err := bucket.Head(ctx, prefix+"interrupted"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("an interrupted stream left an object behind: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the source failed") }

func signedURL(t *testing.T, bucket objstore.Bucket, prefix string) {
	key := prefix + "pack with a space"
	mustPut(t, bucket, key, "fetched without credentials")
	signed, err := bucket.PresignGet(context.Background(), key, time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	response, err := http.Get(signed) // #nosec G107 -- the URL was just signed by the driver under test
	if err != nil {
		t.Fatalf("fetch the signed URL: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "fetched without credentials" {
		t.Fatalf("the signed URL answered %d %q", response.StatusCode, body)
	}
}
