package gitstore

import (
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/helper/polyfill"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	gitFilesystem "github.com/go-git/go-git/v5/storage/filesystem"
)

// gitPackWindow mirrors the delta window internal/server's upload-pack uses, so
// a benchmark here encodes exactly the work a clone does.
const gitPackWindow = 10

// looseStorage is the storage shape bleephub had before this package grew a
// pack tier: go-git's dotgit layout directly over S3, where every git object is
// one S3 object. It is retained as the benchmark baseline, so the numbers in
// the report are produced by running both shapes in the same process against
// the same fake rather than by comparing against a remembered figure.
func looseStorage(fake *fakeS3, repo string) (gitStorage.Storer, error) {
	chrooted, err := fake.fs("bucket", "prefix").Chroot(repo)
	if err != nil {
		return nil, err
	}
	return WrapAtomicRefStorage(repo, gitFilesystem.NewStorage(polyfill.New(chrooted), cache.NewObjectLRUDefault())), nil
}

// packedStorage is the storage shape this package now builds: the same dotgit
// layout over the same object store, with the pack tier, the ranged read path
// and the membership index wired in — that is, exactly what newGitStorage
// produces for an S3-backed repository.
func packedStorage(fake *fakeS3, repo string) (*atomicRefStorer, error) {
	chrooted, err := fake.fs("bucket", "prefix").Chroot(repo)
	if err != nil {
		return nil, err
	}
	s3Chroot, ok := chrooted.(*S3FS)
	if !ok {
		return nil, fmt.Errorf("unexpected chroot type %T", chrooted)
	}
	storage := gitFilesystem.NewStorage(polyfill.New(chrooted), cache.NewObjectLRUDefault())
	return wrapObjectStoreStorage(repo, storage, s3Chroot), nil
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

// benchResult is one measured row of the report.
type benchResult struct {
	label    string
	objects  int
	counts   s3Counts
	duration time.Duration
}

func (r benchResult) String() string {
	perObject := float64(r.counts.total()) / float64(r.objects)
	return fmt.Sprintf("%-34s objects=%-7d %s  wall=%-12s requests/object=%.4f",
		r.label, r.objects, r.counts, r.duration.Round(time.Millisecond), perObject)
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
