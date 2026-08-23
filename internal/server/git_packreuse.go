package bleephub

import (
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/e6qu/bleephub/internal/gitstore"
	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/hash"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Pack reuse: answering a fetch with the packfile bytes the storage layer
// already wrote.
//
// The objects of this server live in packfiles, and a packfile is already the
// wire format of a fetch — the bytes a client receives are the bytes a pack
// holds, entry for entry. Encoding a fresh pack out of decoded objects
// therefore does the same work twice: it inflates and un-deltas every stored
// entry, then runs a delta search over a window to arrive back at something
// very close to what it started from. Copying the stored entry region instead
// turns the CPU cost of a clone from "delta-compress the repository" into
// "checksum the bytes on the way out".
//
// WHAT MAKES A COPY OF A STORED PACK A CORRECT ANSWER
//
// A packfile is a twelve-byte header (signature, version, entry count), a run
// of entries, and a trailing checksum over everything before it. The entries
// carry no absolute positions: an OFS_DELTA names its base by the distance
// backwards from its own start, and a REF_DELTA names it by object id. So a
// pack's entry region is position-independent as a whole — placed at the same
// offset twelve in another pack, with the entries in the same order, every
// delta in it still resolves to the same base. That is what lets several stored
// entry regions be concatenated behind one header, followed by whatever objects
// the answer still owes, and closed with a checksum computed over the result.
//
// Two conditions make such a pack a correct answer to a particular request, and
// both are checked against the plan the enumeration produced rather than
// against anything the request said:
//
//   - Nothing leaks. Every object of every reused pack is in the plan. A pack
//     that carries even one object the plan does not is not reused, which is
//     what keeps a filtered, shallow or partial clone — whose plan is a strict
//     subset of the repository — from being handed a pack containing the
//     objects it was not entitled to or did not ask for.
//   - Nothing is missing. The objects the reused packs do not supply are
//     written after them, so the pack carries the plan exactly.
//
// Self-containment needs no separate check. A stored pack whose delta chains
// reached outside it could not be read by this server at all: go-git resolves a
// delta base within the packfile that holds the delta, so an external base
// would already fail every ordinary object read from that pack.
//
// When no stored pack satisfies the conditions the fetch is answered by the
// encoder as before. That is the ordinary case for an incremental fetch, whose
// plan is a handful of objects that no whole pack is a subset of, and it is the
// correct answer for it: the pack that fetch owes is small, and encoding it
// costs a search over a small window.

// gitPackDirectory is the path, relative to a repository's git directory, that
// holds its packfiles and their indexes.
const gitPackDirectory = "objects/pack"

// gitPackHeaderSize is the twelve bytes that open a packfile: "PACK", the
// format version and the entry count.
const gitPackHeaderSize = 12

// gitPackTrailerSize is the checksum that closes a packfile, which is one
// object id wide.
const gitPackTrailerSize = len(plumbing.ZeroHash)

// gitPackReuseCoverage is the share of the answer the reused packs must carry
// for reuse to be taken, expressed as a fraction with gitPackReuseCoverageOf as
// its denominator.
//
// The objects a reused pack does not supply are written whole, without a delta
// search, because there is no window to search that the stored packs have not
// already been searched over. That is the right trade when they are the tail of
// a repository that is otherwise packed — a push that has not been compacted
// yet — and the wrong one when the packs are a small fragment of a large
// answer, which would trade a modest saving in CPU for a much larger pack on
// the wire. The floor is what separates the two.
const (
	gitPackReuseCoverage   = 3
	gitPackReuseCoverageOf = 4
)

// gitPackReuseStorer pairs a repository's storer with the directory its
// packfiles live in, so the fetch path can reach the stored bytes through the
// same value it already carries.
//
// The storer is embedded because this type adds to the interface rather than
// changing any of it: every method a fetch calls is the storage layer's own.
type gitPackReuseStorer struct {
	storer.Storer
	packDir billy.Filesystem
}

// gitStorerWithPackReuse returns the storer a fetch of this repository should
// use, with its pack directory attached when the storage backend has one.
//
// Memory-backed storage has no packfiles, and a repository whose name does not
// resolve to a location has none either; both are served by the plain storer,
// which is the encoder path.
func gitStorerWithPackReuse(ctx context.Context, fullName string, stor storer.Storer) storer.Storer { //nolint:ireturn
	packDir, err := gitRepositoryFilesystem(ctx, fullName)
	if err != nil || packDir == nil {
		return stor
	}
	return &gitPackReuseStorer{Storer: stor, packDir: packDir}
}

// gitRepositoryFilesystem opens the git directory of a repository on whichever
// backend holds it, mirroring the choice the storage layer itself made.
func gitRepositoryFilesystem(ctx context.Context, fullName string) (billy.Filesystem, error) { //nolint:ireturn
	if err := gitstore.ValidateRepoStorageFullName(fullName); err != nil {
		return nil, err
	}
	objectStore, err := gitstore.GetS3FS(ctx)
	if err != nil {
		return nil, err
	}
	if objectStore != nil {
		return objectStore.Chroot(fullName)
	}
	dataDir := gitstore.GitDataDir()
	if dataDir == "" {
		return nil, nil
	}
	repoDir, err := gitstore.RepoGitDirPath(dataDir, fullName)
	if err != nil {
		return nil, err
	}
	return osfs.New(repoDir), nil
}

// gitPackDirOf returns the pack directory a storer carries, or nil for a storer
// that carries none.
func gitPackDirOf(stor storer.EncodedObjectStorer) billy.Filesystem { //nolint:ireturn
	reuse, ok := stor.(*gitPackReuseStorer)
	if !ok {
		return nil
	}
	return reuse.packDir
}

// gitStoredPack is one packfile of a repository: the objects its index lists
// and the size of the file itself.
type gitStoredPack struct {
	name    string
	objects map[plumbing.Hash]bool
	size    int64
}

// gitReusablePacks reads a repository's pack directory and returns the packs
// this answer can be built from, or nothing when it cannot be.
//
// The reading is in two passes because the expensive part is the index. A pack
// index's fanout table gives the number of objects in it from its first kilobyte,
// which is enough to rule out every pack larger than the whole answer — so an
// incremental fetch, whose plan is a handful of objects, decides against a
// repository-sized pack without reading a repository-sized index.
func gitReusablePacks(fs billy.Filesystem, plan *gitPackPlan) ([]*gitStoredPack, error) {
	entries, err := fs.ReadDir(gitPackDirectory)
	if err != nil {
		// A repository that has never been compacted has no pack directory,
		// which is the same answer as a pack directory with nothing in it.
		return nil, nil
	}
	type candidate struct {
		name  string
		size  int64
		count int
	}
	var candidates []candidate
	reachable := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".idx") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".idx")
		// A pack is published by writing its .pack key last, so an index whose
		// packfile is not there yet belongs to a compaction still in flight: it
		// names bytes no reader can serve.
		info, err := fs.Stat(fs.Join(gitPackDirectory, name+".pack"))
		if err != nil {
			continue
		}
		count, err := gitPackIndexCount(fs, name)
		if err != nil {
			return nil, err
		}
		if count == 0 || count > len(plan.objects) {
			continue
		}
		candidates = append(candidates, candidate{name: name, size: info.Size(), count: count})
		reachable += count
	}
	// Even taking every candidate whole — which overlap between them can only
	// reduce — the coverage floor would not be met, so no index is read.
	if reachable*gitPackReuseCoverageOf < len(plan.objects)*gitPackReuseCoverage {
		return nil, nil
	}
	packs := make([]*gitStoredPack, 0, len(candidates))
	for _, candidate := range candidates {
		objects, err := gitPackIndexObjects(fs, candidate.name)
		if err != nil {
			return nil, err
		}
		packs = append(packs, &gitStoredPack{name: candidate.name, objects: objects, size: candidate.size})
	}
	return gitSelectReusablePacks(packs, plan), nil
}

// gitPackIndexFanoutSize is the 256 four-byte cumulative counts that open a
// pack index. The last of them is the number of objects the index lists.
const gitPackIndexFanoutSize = 256 * 4

// gitPackIndexCount reads the number of objects a pack index lists, from its
// fanout table alone.
//
// A version 2 index puts its magic and version ahead of the fanout; a version 1
// index — which git still writes on request and still reads — begins with it.
func gitPackIndexCount(fs billy.Filesystem, name string) (int, error) {
	file, err := fs.Open(fs.Join(gitPackDirectory, name+".idx"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, 8+gitPackIndexFanoutSize)
	if _, err := io.ReadFull(file, head); err != nil {
		return 0, fmt.Errorf("read %s.idx: %w", name, err)
	}
	fanout := head[:gitPackIndexFanoutSize]
	if string(head[:4]) == gitPackIndexMagic {
		if version := binary.BigEndian.Uint32(head[4:8]); version != 2 {
			return 0, fmt.Errorf("pack index %s is format version %d", name, version)
		}
		fanout = head[8:]
	}
	return int(binary.BigEndian.Uint32(fanout[gitPackIndexFanoutSize-4:])), nil
}

// gitPackIndexMagic opens a version 2 pack index.
const gitPackIndexMagic = "\xfftOc"

// gitPackIndexObjects reads the object ids a packfile index lists.
func gitPackIndexObjects(fs billy.Filesystem, name string) (map[plumbing.Hash]bool, error) {
	file, err := fs.Open(fs.Join(gitPackDirectory, name+".idx"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	index := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(file).Decode(index); err != nil {
		return nil, fmt.Errorf("decode %s.idx: %w", name, err)
	}
	iter, err := index.Entries()
	if err != nil {
		return nil, err
	}
	objects := map[plumbing.Hash]bool{}
	for {
		entry, err := iter.Next()
		if err == io.EOF {
			return objects, nil
		}
		if err != nil {
			return nil, err
		}
		objects[entry.Hash] = true
	}
}

// gitSelectReusablePacks picks the stored packs whose entry regions this answer
// can be built from: each one holds only objects the plan owes, and no two of
// them hold the same object.
//
// Disjointness is required because the pack is one namespace. Two published
// packs can legitimately overlap — a merge publishes a pack holding what its
// inputs held, and the inputs stay readable until their retention window
// elapses — and writing an object twice into one pack would give the client's
// index two entries for one id.
//
// The largest pack is taken first, so an overlap is resolved in favour of the
// pack that carries more of the answer.
func gitSelectReusablePacks(packs []*gitStoredPack, plan *gitPackPlan) []*gitStoredPack {
	var eligible []*gitStoredPack
	for _, pack := range packs {
		if len(pack.objects) == 0 || len(pack.objects) > len(plan.objects) {
			continue
		}
		contained := true
		for object := range pack.objects {
			if !plan.packed[object] {
				contained = false
				break
			}
		}
		if contained {
			eligible = append(eligible, pack)
		}
	}
	// Ordered by size and then by name so the same repository and the same
	// request produce the same pack every time.
	sort.Slice(eligible, func(i, j int) bool {
		if len(eligible[i].objects) != len(eligible[j].objects) {
			return len(eligible[i].objects) > len(eligible[j].objects)
		}
		return eligible[i].name < eligible[j].name
	})
	var chosen []*gitStoredPack
	taken := map[plumbing.Hash]bool{}
	for _, pack := range eligible {
		overlaps := false
		for object := range pack.objects {
			if taken[object] {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}
		for object := range pack.objects {
			taken[object] = true
		}
		chosen = append(chosen, pack)
	}
	if len(chosen) == 0 || len(taken)*gitPackReuseCoverageOf < len(plan.objects)*gitPackReuseCoverage {
		return nil
	}
	return chosen
}

// writeGitReusedPackfile writes the answer as the entry regions of the chosen
// stored packs followed by the objects they do not carry.
//
// The count in the header is the plan's, the entry regions are copied byte for
// byte in the order the packs were chosen, and the checksum is computed over
// everything actually written — so the pack the client reads is a pack this
// call built, not a stored pack forwarded whole, even when one stored pack
// happens to supply all of it.
func writeGitReusedPackfile(band *gitBandWriter, stor storer.EncodedObjectStorer, fs billy.Filesystem, packs []*gitStoredPack, plan *gitPackPlan) error {
	reused := map[plumbing.Hash]bool{}
	for _, pack := range packs {
		for object := range pack.objects {
			reused[object] = true
		}
	}
	remainder := make([]plumbing.Hash, 0, len(plan.objects)-len(reused))
	for _, id := range plan.objects {
		if !reused[id] {
			remainder = append(remainder, id)
		}
	}

	out := band.pack()
	digest := plumbing.Hasher{Hash: hash.New(hash.CryptoType)}
	stream := io.MultiWriter(out, digest)
	if err := writeGitPackHeader(stream, len(plan.objects)); err != nil {
		return err
	}
	copied := 0
	for _, pack := range packs {
		if err := copyGitPackEntries(stream, fs, pack); err != nil {
			return err
		}
		copied += len(pack.objects)
		band.progressf("Reusing objects: %d%% (%d/%d)\r", copied*100/len(plan.objects), copied, len(plan.objects))
	}
	compressor := zlib.NewWriter(stream)
	for _, id := range remainder {
		encoded, err := stor.EncodedObject(plumbing.AnyObject, id)
		if err != nil {
			return err
		}
		if err := writeGitPackObject(stream, compressor, encoded); err != nil {
			return err
		}
	}
	band.progressf("Compressing objects: 100%% (%d/%d), done.\n", len(plan.objects), len(plan.objects))
	trailer := digest.Sum()
	_, err := out.Write(trailer[:])
	return err
}

// copyGitPackEntries copies the entry region of a stored packfile — everything
// between its header and its checksum — onto the stream.
//
// The header is read rather than skipped: its entry count must agree with the
// index the object set was read from, or the file on disk is not the pack that
// index describes and this answer would be built from the wrong bytes.
func copyGitPackEntries(stream io.Writer, fs billy.Filesystem, pack *gitStoredPack) error {
	entries := pack.size - int64(gitPackHeaderSize) - int64(gitPackTrailerSize)
	if entries <= 0 {
		return fmt.Errorf("pack %s is %d bytes, too short to hold entries", pack.name, pack.size)
	}
	file, err := fs.Open(fs.Join(gitPackDirectory, pack.name+".pack"))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, gitPackHeaderSize)
	if _, err := io.ReadFull(file, header); err != nil {
		return err
	}
	if string(header[:4]) != "PACK" {
		return fmt.Errorf("pack %s does not start with a pack signature", pack.name)
	}
	if version := binary.BigEndian.Uint32(header[4:8]); version != gitPackVersion {
		return fmt.Errorf("pack %s is format version %d", pack.name, version)
	}
	if count := binary.BigEndian.Uint32(header[8:12]); int(count) != len(pack.objects) {
		return fmt.Errorf("pack %s holds %d entries but its index lists %d", pack.name, count, len(pack.objects))
	}
	copied, err := io.CopyN(stream, file, entries)
	if err != nil {
		return err
	}
	if copied != entries {
		return fmt.Errorf("pack %s ended %d bytes early", pack.name, entries-copied)
	}
	return nil
}
