package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/storage/memory"
)

// packWindow is the delta window bleephub's upload-pack encodes with, so a clone
// measured here does the work a served clone does.
const packWindow = 10

const benchBranch = plumbing.ReferenceName("refs/heads/main")

// WorkloadSpec sizes the synthetic repository. Every driver is handed the same
// bytes: the repository is generated once, from a fixed seed, and each push is
// cut into a packfile once.
type WorkloadSpec struct {
	// Files is the number of files in the tree.
	Files int
	// Commits is the length of the history the initial push carries.
	Commits int
	// Pushes is the number of single-commit pushes that follow it.
	Pushes int
	// Changes is the number of files each commit rewrites.
	Changes int
	// Seed fixes the content, so two runs measure the same repository.
	Seed uint64
}

// Push is one client push: the packfile a client would send and the tip the
// branch moves to.
type Push struct {
	Tip     plumbing.Hash
	Pack    []byte
	Objects int
}

// Workload is a generated repository, cut into the pushes that build it.
type Workload struct {
	Spec WorkloadSpec
	// Source holds every object, for verifying what a driver hands back.
	Source *memory.Storage
	// Initial carries the whole starting history; Incremental are the pushes
	// that follow, one commit each.
	Initial     Push
	Incremental []Push
	// Objects is the number of objects reachable from the final tip.
	Objects int
}

// fixedSignature keeps commit hashes reproducible: a wall-clock stamp would make
// every run measure a different object set.
func fixedSignature(step int) object.Signature {
	return object.Signature{
		Name:  "Gitstore Benchmark",
		Email: "benchmark@gitstore.invalid",
		When:  time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(step) * time.Minute),
	}
}

// treeState is the working tree, held as leaf directories so a commit rebuilds
// only the trees on the path to a file it changed.
type treeState struct {
	stor *memory.Storage
	// leaves maps "dNN/dMM" to its files; bodies holds current file contents so
	// a change is an edit of the previous revision, which is what makes the
	// delta encoder's work representative.
	leaves map[string]map[string]plumbing.Hash
	bodies map[string][]byte
	paths  []string
}

const filesPerLeaf = 32
const leavesPerDir = 16

func filePath(i int) (leaf, name string) {
	leafIndex := i / filesPerLeaf
	return fmt.Sprintf("d%03d/d%03d", leafIndex/leavesPerDir, leafIndex%leavesPerDir), fmt.Sprintf("file%06d.go", i)
}

// stream is the workload's source of variation: SHA-256 in counter mode over the
// seed. It is a generator rather than math/rand because the requirement is
// reproducibility and nothing else — the same seed must build byte-identical
// repositories on every machine and Go release, which a hash of a counter
// guarantees by construction.
type stream struct {
	seed    uint64
	counter uint64
}

// IntN returns a value in [0, n). It draws 31 bits, assembled from bytes so the
// result fits an int on every platform; the modulo bias is immaterial to shaping
// a synthetic repository.
func (s *stream) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	var block [16]byte
	binary.BigEndian.PutUint64(block[:8], s.seed)
	binary.BigEndian.PutUint64(block[8:], s.counter)
	s.counter++
	sum := sha256.Sum256(block[:])
	value := int(sum[0]&0x7f)<<24 | int(sum[1])<<16 | int(sum[2])<<8 | int(sum[3])
	return value % n
}

// sourceLike returns text with the redundancy of source code: compressible, and
// similar from one revision to the next.
func sourceLike(rng *stream, index int) []byte {
	var body bytes.Buffer
	fmt.Fprintf(&body, "package pkg%03d\n\n", index%97)
	lines := 20 + rng.IntN(120)
	for line := range lines {
		fmt.Fprintf(&body, "func f%d_%d(a, b int) int {\n\treturn a*%d + b*%d\n}\n\n", index, line, rng.IntN(1000), rng.IntN(1000))
	}
	return body.Bytes()
}

// edit rewrites a few lines in place, the shape of an ordinary commit.
func edit(rng *stream, body []byte) []byte {
	lines := bytes.Split(body, []byte("\n"))
	for range 1 + rng.IntN(4) {
		at := rng.IntN(len(lines))
		lines[at] = fmt.Appendf(nil, "\t// revised %d", rng.IntN(1_000_000))
	}
	return bytes.Join(lines, []byte("\n"))
}

func (t *treeState) writeBlob(path string, body []byte) error {
	obj := t.stor.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(body)))
	writer, err := obj.Writer()
	if err != nil {
		return err
	}
	if _, err := writer.Write(body); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	hash, err := t.stor.SetEncodedObject(obj)
	if err != nil {
		return err
	}
	leaf, name := splitLeaf(path)
	if t.leaves[leaf] == nil {
		t.leaves[leaf] = map[string]plumbing.Hash{}
	}
	t.leaves[leaf][name] = hash
	t.bodies[path] = body
	return nil
}

func splitLeaf(path string) (leaf, name string) {
	at := bytes.LastIndexByte([]byte(path), '/')
	return path[:at], path[at+1:]
}

func (t *treeState) storeTree(entries []object.TreeEntry) (plumbing.Hash, error) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	obj := t.stor.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return t.stor.SetEncodedObject(obj)
}

// rootTree writes the three levels of trees. Identical trees hash identically
// and the object store deduplicates them, so rebuilding an unchanged leaf costs
// CPU here but adds no object to the workload.
func (t *treeState) rootTree() (plumbing.Hash, error) {
	top := map[string][]object.TreeEntry{}
	for leaf, files := range t.leaves {
		entries := make([]object.TreeEntry, 0, len(files))
		for name, hash := range files {
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: hash})
		}
		hash, err := t.storeTree(entries)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		dir, sub := splitLeaf(leaf)
		top[dir] = append(top[dir], object.TreeEntry{Name: sub, Mode: filemode.Dir, Hash: hash})
	}
	root := make([]object.TreeEntry, 0, len(top))
	for dir, entries := range top {
		hash, err := t.storeTree(entries)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		root = append(root, object.TreeEntry{Name: dir, Mode: filemode.Dir, Hash: hash})
	}
	return t.storeTree(root)
}

func (t *treeState) commit(step int, parent plumbing.Hash) (plumbing.Hash, error) {
	tree, err := t.rootTree()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	signature := fixedSignature(step)
	commit := &object.Commit{
		Author:    signature,
		Committer: signature,
		Message:   fmt.Sprintf("change %d\n", step),
		TreeHash:  tree,
	}
	if !parent.IsZero() {
		commit.ParentHashes = []plumbing.Hash{parent}
	}
	obj := t.stor.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return t.stor.SetEncodedObject(obj)
}

// GenerateWorkload builds the repository and cuts it into pushes.
func GenerateWorkload(spec WorkloadSpec) (*Workload, error) {
	if spec.Files <= 0 || spec.Commits <= 0 || spec.Pushes < 0 || spec.Changes <= 0 {
		return nil, fmt.Errorf("workload needs positive files, commits and changes: %+v", spec)
	}
	rng := &stream{seed: spec.Seed}
	source := memory.NewStorage()
	tree := &treeState{stor: source, leaves: map[string]map[string]plumbing.Hash{}, bodies: map[string][]byte{}}

	for i := range spec.Files {
		leaf, name := filePath(i)
		path := leaf + "/" + name
		tree.paths = append(tree.paths, path)
		if err := tree.writeBlob(path, sourceLike(rng, i)); err != nil {
			return nil, err
		}
	}

	workload := &Workload{Spec: spec, Source: source}
	var tip plumbing.Hash
	var pushedTips []plumbing.Hash
	for step := range spec.Commits + spec.Pushes {
		if step > 0 {
			for range spec.Changes {
				path := tree.paths[rng.IntN(len(tree.paths))]
				if err := tree.writeBlob(path, edit(rng, tree.bodies[path])); err != nil {
					return nil, err
				}
			}
		}
		next, err := tree.commit(step, tip)
		if err != nil {
			return nil, err
		}
		tip = next
		if step < spec.Commits-1 {
			continue
		}
		push, err := cutPush(source, tip, pushedTips)
		if err != nil {
			return nil, err
		}
		if len(pushedTips) == 0 {
			workload.Initial = push
		} else {
			workload.Incremental = append(workload.Incremental, push)
		}
		pushedTips = []plumbing.Hash{tip}
	}

	all, err := revlist.Objects(source, []plumbing.Hash{tip}, nil)
	if err != nil {
		return nil, err
	}
	workload.Objects = len(all)
	return workload, nil
}

// cutPush encodes what a client holding tip would send a server holding have.
func cutPush(source *memory.Storage, tip plumbing.Hash, have []plumbing.Hash) (Push, error) {
	hashes, err := revlist.Objects(source, []plumbing.Hash{tip}, have)
	if err != nil {
		return Push{}, err
	}
	// The walk returns hashes in map order, and the encoder's delta choices
	// follow its input order, so an unsorted walk yields a different packfile
	// on every generation: the same objects, but not the same bytes to ingest.
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, source, false).Encode(hashes, packWindow); err != nil {
		return Push{}, err
	}
	return Push{Tip: tip, Pack: pack.Bytes(), Objects: len(hashes)}, nil
}
