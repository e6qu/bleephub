package gitstore

import (
	"crypto/sha256"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// incompressible returns size bytes zlib cannot shrink, the same from run to run.
func incompressible(size int) string {
	body := make([]byte, 0, size+sha256.Size)
	link := sha256.Sum256([]byte("streamed"))
	for len(body) < size {
		body = append(body, link[:]...)
		link = sha256.Sum256(link[:])
	}
	return string(body[:size])
}

// TestALargePackedObjectIsStreamedNotHeld pins what a server owes the machine it
// runs on. A repository may hold a blob of any size and a read is made on behalf
// of whoever asks, so an object handed out as bytes costs its whole size in heap
// for every reader at once. go-git's pack decoder hands objects out that way
// unless it is given a filesystem to reopen the pack through, and the engine
// gives it none; a large object stored whole is therefore read by the engine
// itself, inflated as it is consumed. The content must be exact, readable any
// number of times and by several readers together, and small objects must still
// come from the decoder, whose cache is what makes history walks cheap.
func TestALargePackedObjectIsStreamedNotHeld(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	large := incompressible(streamedObjectBytes + 1<<20)
	largeHash := smallPush(t, stor, large)
	smallHash := smallPush(t, stor, "a small object")

	object, err := stor.EncodedObject(plumbing.BlobObject, largeHash)
	if err != nil {
		t.Fatalf("read the large object: %v", err)
	}
	if _, streamed := object.(*streamedObject); !streamed {
		t.Fatalf("premise: a %d byte object stored whole came back as %T, held in memory", len(large), object)
	}
	if object.Size() != int64(len(large)) || object.Type() != plumbing.BlobObject || object.Hash() != largeHash {
		t.Fatalf("streamed object describes itself as %s %d %s", object.Type(), object.Size(), object.Hash())
	}
	if _, cached := stor.objectCache.Get(largeHash); cached {
		t.Fatal("the large object's bytes were kept in the object cache")
	}

	var readers sync.WaitGroup
	failures := make(chan string, 4)
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			reader, err := object.Reader()
			if err != nil {
				failures <- err.Error()
				return
			}
			body, err := io.ReadAll(reader)
			if closeErr := reader.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				failures <- err.Error()
				return
			}
			if got := plumbing.ComputeHash(plumbing.BlobObject, body); got != largeHash {
				failures <- "content hashed to " + got.String()
			}
		}()
	}
	readers.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("a reader of the streamed object: %s", failure)
	}

	small, err := stor.EncodedObject(plumbing.AnyObject, smallHash)
	if err != nil {
		t.Fatalf("read the small object: %v", err)
	}
	if _, streamed := small.(*streamedObject); streamed {
		t.Fatal("a small object was streamed: it would be read from the store again on every use")
	}
	if got := readObjects(t, stor, []plumbing.Hash{smallHash})[smallHash]; !strings.HasPrefix(got, "a small object") {
		t.Fatalf("small object read back as %q", got)
	}
	if _, err := stor.EncodedObject(plumbing.CommitObject, largeHash); err == nil {
		t.Fatal("a blob was returned to a caller that asked for a commit")
	}
}

// TestAnOutageDuringAStreamIsHeardAsAnOutage pins that the stream reports what
// the store said. The decoder underneath reports a failed read in its own terms
// — a corrupt stream, an unexpected end — and a caller that believed it would
// tell a client the repository is damaged when the store is merely unreachable.
func TestAnOutageDuringAStreamIsHeardAsAnOutage(t *testing.T) {
	fake := newFakeS3(t)
	// The memory tier would serve the extents the push seeded, and nothing
	// would be asked of the store at all.
	fake.opts.MemoryCacheBytes = -1
	fake.opts.CacheDir = t.TempDir()
	stor := testPackedStorage(t, fake)
	hash := smallPush(t, stor, incompressible(streamedObjectBytes+1<<20))

	// A replica that did not make the push: it has the pack to fetch.
	fake.opts.CacheDir = t.TempDir()
	reader := testPackedStorage(t, fake)
	before := fake.Snapshot()
	object, err := reader.EncodedObject(plumbing.BlobObject, hash)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Finding the object reads the pack's index and the extent its header is
	// in. It must not read the object: that is the stream's to do, if asked.
	if spent := fake.Snapshot().Sub(before); spent.GetRanged > 2 || spent.BytesDown > streamedObjectBytes {
		t.Fatalf("finding a large object downloaded it: %s", spent)
	}
	fake.SetFailOn(func(method, key string) bool { return method == "GET" && strings.HasSuffix(key, ".pack") })
	t.Cleanup(func() { fake.SetFailOn(nil) })

	content, err := object.Reader()
	if err == nil {
		_, err = io.ReadAll(content)
		_ = content.Close()
	}
	if err == nil {
		t.Fatal("premise: the store was failing and the stream read to the end")
	}
	if strings.Contains(err.Error(), "zlib") || strings.Contains(err.Error(), "not found") {
		t.Fatalf("an outage was reported as %q", err)
	}
}
