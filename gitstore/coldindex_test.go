package gitstore

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"
)

// TestConcurrentFirstReadsOfAColdHandleFindEveryObject pins the first moments of
// a replica's life. A server keeps one storage handle per repository and shares
// it between requests, so after a restart the first clones of a repository
// arrive together at a handle that has not yet loaded its pack indexes. go-git
// loads them lazily and publishes the index map before filling it, so without
// protection a reader arriving mid-load sees some packs and not others, and
// reports an object the repository holds as missing — which a git client sees
// as "not our ref".
func TestConcurrentFirstReadsOfAColdHandleFindEveryObject(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactionTrigger = -1
	writer := testPackedStorage(t, fake)

	// One object per pack, several packs: the object a reader wants is, as
	// often as not, in a pack the loader has not reached yet.
	var hashes []plumbing.Hash
	for push := range 6 {
		client := memory.NewStorage()
		hash := storeBlob(t, client, fmt.Sprintf("pack %d", push))
		var pack bytes.Buffer
		if _, err := packfile.NewEncoder(&pack, client, false).Encode([]plumbing.Hash{hash}, gitPackWindow); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := packfile.UpdateObjectStorage(writer, &pack); err != nil {
			t.Fatalf("push %d: %v", push, err)
		}
		hashes = append(hashes, hash)
	}

	for round := range 5 {
		// A fresh filesystem and handle is a replica that has just started.
		cold := newFakeS3(t)
		cold.Server = fake.Server
		handle, err := packedStorage(cold, testRepo)
		if err != nil {
			t.Fatalf("storage: %v", err)
		}
		// Slow the store so the index load is still under way when the other
		// readers arrive.
		fake.SetLatency(5 * time.Millisecond)

		var wg sync.WaitGroup
		errs := make([]error, len(hashes))
		for i, hash := range hashes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = handle.HasEncodedObject(hash)
			}()
		}
		wg.Wait()
		fake.SetLatency(0)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: object in pack %d reported missing by a cold handle under concurrent first reads: %v", round, i, err)
			}
		}
	}
}
