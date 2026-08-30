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

// Pack reuse: answer a fetch with the packfile bytes storage already wrote,
// turning a clone's CPU cost from "delta-compress the repository" into "checksum
// the bytes on the way out". A pack's entry region is position-independent
// (OFS_DELTA names its base by backward distance, REF_DELTA by object id), so
// several stored entry regions concatenate behind one header, followed by
// whatever objects the answer still owes, closed with a fresh checksum.
//
// Two conditions, both checked against the enumeration plan, not the request:
//   - Nothing leaks: every object of every reused pack is in the plan, so a
//     filtered/shallow/partial clone is never handed objects it did not ask for.
//   - Nothing is missing: objects the reused packs do not supply are written after them.
//
// Self-containment needs no check: go-git resolves a delta base within its own
// packfile, so a pack with an external base would already fail every read.
// When no stored pack qualifies (the ordinary incremental fetch) the encoder
// answers as before, at the cost of a search over a small window.

// gitPackDirectory is the pack directory relative to a repository's git dir.
const gitPackDirectory = "objects/pack"

// gitPackHeaderSize is the twelve-byte packfile header ("PACK", version, count).
const gitPackHeaderSize = 12

// gitPackTrailerSize is the closing checksum, one object id wide.
const gitPackTrailerSize = len(plumbing.ZeroHash)

// gitPackReuseCoverage / gitPackReuseCoverageOf is the fraction of the answer
// the reused packs must carry for reuse to be taken. Objects the reused packs
// do not supply are written whole (no delta search), which is the right trade
// only when they are the tail of an otherwise-packed repository, not a small
// fragment of a large answer; the floor separates the two.
const (
	gitPackReuseCoverage   = 3
	gitPackReuseCoverageOf = 4
)

// gitPackReuseStorer pairs a repository's storer with its pack directory, so the
// fetch path reaches the stored bytes through the value it already carries. The
// storer is embedded because every method a fetch calls belongs to it.
type gitPackReuseStorer struct {
	storer.Storer
	packDir billy.Filesystem
}

// gitStorerWithPackReuse returns the fetch storer with its pack directory
// attached when the backend has one. Memory-backed storage and an unresolvable
// name have no packfiles and fall back to the plain (encoder-path) storer.
func gitStorerWithPackReuse(ctx context.Context, fullName string, stor storer.Storer) storer.Storer { //nolint:ireturn
	packDir, err := gitRepositoryFilesystem(ctx, fullName)
	if err != nil || packDir == nil {
		return stor
	}
	return &gitPackReuseStorer{Storer: stor, packDir: packDir}
}

// gitRepositoryFilesystem opens a repository's git directory on whichever
// backend holds it, mirroring the storage layer's choice.
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

// gitPackDirOf returns the pack directory a storer carries, or nil.
func gitPackDirOf(stor storer.EncodedObjectStorer) billy.Filesystem { //nolint:ireturn
	reuse, ok := stor.(*gitPackReuseStorer)
	if !ok {
		return nil
	}
	return reuse.packDir
}

// gitStoredPack is one packfile: the objects its index lists and its file size.
type gitStoredPack struct {
	name    string
	objects map[plumbing.Hash]bool
	size    int64
}

// gitReusablePacks reads a repository's pack directory and returns the packs
// this answer can be built from, or nothing. Two passes because the index is the
// expensive part: the fanout table gives an index's object count from its first
// kilobyte, ruling out packs larger than the answer without reading a
// repository-sized index.
func gitReusablePacks(fs billy.Filesystem, plan *gitPackPlan) ([]*gitStoredPack, error) {
	entries, err := fs.ReadDir(gitPackDirectory)
	if err != nil {
		// No pack directory (never compacted) is the same as an empty one.
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
		// The .pack key is written last, so an index without its packfile belongs
		// to a compaction still in flight and names bytes no reader can serve.
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
	// Even every candidate whole (overlap only reduces this) would miss the
	// coverage floor, so no index is read.
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

// gitPackIndexFanoutSize is the 256 four-byte cumulative counts opening a pack
// index; the last is the number of objects the index lists.
const gitPackIndexFanoutSize = 256 * 4

// gitPackIndexCount reads a pack index's object count from its fanout table
// alone. A version 2 index puts magic and version ahead of the fanout; a version
// 1 index begins with it.
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
// can be built from: each holds only objects the plan owes, and no two hold the
// same object. Disjointness matters because two published packs can legitimately
// overlap (a merge's pack and its still-readable inputs), and writing an object
// twice would give the client's index two entries for one id. The largest pack
// is taken first, resolving overlaps toward the pack carrying more of the answer.
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
	// Order by size then name for a deterministic selection.
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

// writeGitReusedPackfile writes the answer as the chosen packs' entry regions
// followed by the objects they do not carry. The header count is the plan's, the
// entry regions are copied byte for byte, and the checksum is computed over
// everything written — so the client always reads a pack this call built, never
// a stored pack forwarded whole.
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

// copyGitPackEntries copies a stored packfile's entry region (between header and
// checksum) onto the stream. The header is read, not skipped: its entry count
// must agree with the index the object set came from, or the file is not the
// pack that index describes.
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
