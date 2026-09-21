package gitstore

import (
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// gitPackWindow mirrors the delta window internal/server's upload-pack uses, so
// a benchmark here encodes exactly the work a clone does.
const gitPackWindow = 10

// packedStorage opens the repository as a replica that has never seen it would:
// a store of its own, so nothing is remembered from the handle that wrote it.
// It is exactly what Store.Repository produces for an object-store repository.
func packedStorage(fake *fakeS3, repo string) (*repository, error) {
	stor, err := fake.store("prefix").Repository(repo)
	if err != nil {
		return nil, err
	}
	handle, ok := stor.(*repository)
	if !ok {
		return nil, fmt.Errorf("unexpected storer type %T", stor)
	}
	return handle, nil
}

// seedObjects writes n blobs plus the tree and commit that reference them,
// returning every hash in the order a pack would carry them. Blob bodies are
// deterministic and compressible in the way source files are, so the delta
// encoder does representative work.
func seedObjects(tb testing.TB, stor gitStorage.Storer, n int) []plumbing.Hash {
	tb.Helper()
	hashes := make([]plumbing.Hash, 0, n+2)
	tree := &treeBuilder{}
	for i := range n {
		body := blobBody(i)
		obj := stor.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		obj.SetSize(int64(len(body)))
		writer, err := obj.Writer()
		if err != nil {
			tb.Fatalf("blob writer: %v", err)
		}
		if _, err := writer.Write(body); err != nil {
			tb.Fatalf("blob write: %v", err)
		}
		if err := writer.Close(); err != nil {
			tb.Fatalf("blob close: %v", err)
		}
		hash, err := stor.SetEncodedObject(obj)
		if err != nil {
			tb.Fatalf("set blob %d: %v", i, err)
		}
		hashes = append(hashes, hash)
		tree.add(fmt.Sprintf("file-%06d.txt", i), hash)
	}

	treeHash, err := tree.store(stor)
	if err != nil {
		tb.Fatalf("store tree: %v", err)
	}
	hashes = append(hashes, treeHash)

	commitHash, err := storeCommit(stor, treeHash)
	if err != nil {
		tb.Fatalf("store commit: %v", err)
	}
	hashes = append(hashes, commitHash)

	if err := stor.SetReference(plumbing.NewHashReference("refs/heads/main", commitHash)); err != nil {
		tb.Fatalf("set ref: %v", err)
	}
	return hashes
}

func blobBody(i int) []byte {
	return []byte("package main\n\n// object " + strconv.Itoa(i) + "\nfunc main() {\n\tprintln(\"" + strconv.Itoa(i) + "\")\n}\n")
}

// clonePack encodes every object into a packfile exactly as upload-pack does,
// discarding the bytes. It is the read-path workload: one EncodedObject call
// per object, plus whatever the delta window re-reads.
func clonePack(tb testing.TB, stor gitStorage.Storer, hashes []plumbing.Hash) {
	tb.Helper()
	encoder := packfile.NewEncoder(io.Discard, stor, false)
	if _, err := encoder.Encode(hashes, gitPackWindow); err != nil {
		tb.Fatalf("encode pack: %v", err)
	}
}

// treeBuilder accumulates the entries of the single tree the seeded objects
// hang from.
type treeBuilder struct {
	entries []object.TreeEntry
}

func (b *treeBuilder) add(name string, hash plumbing.Hash) {
	b.entries = append(b.entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: hash})
}

func (b *treeBuilder) store(stor gitStorage.Storer) (plumbing.Hash, error) {
	tree := &object.Tree{Entries: b.entries}
	obj := stor.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return stor.SetEncodedObject(obj)
}

// benchSignature is a fixed identity: a benchmark that stamped the wall clock
// would produce a different commit hash on every run and make the measured
// object set irreproducible.
var benchSignature = object.Signature{
	Name:  "Bleephub Benchmark",
	Email: "benchmark@bleephub.invalid",
	When:  time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC),
}

func storeCommit(stor gitStorage.Storer, tree plumbing.Hash) (plumbing.Hash, error) {
	commit := &object.Commit{
		Author:    benchSignature,
		Committer: benchSignature,
		Message:   "seed\n",
		TreeHash:  tree,
	}
	obj := stor.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return stor.SetEncodedObject(obj)
}
