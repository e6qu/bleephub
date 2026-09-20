package bleephub

import (
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/go-git/go-git/v5/plumbing"
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

// gitStoredPack is one packfile: the objects its index lists and its file size.
type gitStoredPack struct {
	name    string
	objects map[plumbing.Hash]bool
	size    int64
}

// gitReusablePacks returns the stored packs this answer can be built from, or
// nothing. The storage engine already holds the live pack set and its parsed
// indexes, so nothing is listed or read here that a previous request has not
// paid for. Two passes all the same, because turning an index into an object set
// is the expensive part: the counts rule out packs larger than the answer, and
// the coverage floor rules out the whole repository, before any set is built.
func gitReusablePacks(ctx context.Context, source gitstore.PackSource, plan *gitPackPlan) ([]*gitStoredPack, error) {
	stored, err := source.StoredPacks(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []gitstore.StoredPack
	reachable := 0
	for _, pack := range stored {
		if pack.Objects == 0 || pack.Objects > len(plan.objects) {
			continue
		}
		candidates = append(candidates, pack)
		reachable += pack.Objects
	}
	// Even every candidate whole (overlap only reduces this) would miss the
	// coverage floor, so no object set is built.
	if reachable*gitPackReuseCoverageOf < len(plan.objects)*gitPackReuseCoverage {
		return nil, nil
	}
	packs := make([]*gitStoredPack, 0, len(candidates))
	for _, candidate := range candidates {
		objects, err := gitPackIndexObjects(ctx, source, candidate.Name)
		if err != nil {
			return nil, err
		}
		packs = append(packs, &gitStoredPack{name: candidate.Name, objects: objects, size: candidate.Size})
	}
	return gitSelectReusablePacks(packs, plan), nil
}

// gitPackIndexObjects is the set of objects a stored pack's index lists.
func gitPackIndexObjects(ctx context.Context, source gitstore.PackSource, name string) (map[plumbing.Hash]bool, error) {
	index, err := source.PackIndex(ctx, name)
	if err != nil {
		return nil, err
	}
	iter, err := index.Entries()
	if err != nil {
		return nil, err
	}
	defer func() { _ = iter.Close() }()
	objects := map[plumbing.Hash]bool{}
	for {
		entry, err := iter.Next()
		if errors.Is(err, io.EOF) {
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
func writeGitReusedPackfile(ctx context.Context, band *gitBandWriter, stor storer.EncodedObjectStorer, source gitstore.PackSource, packs []*gitStoredPack, plan *gitPackPlan) error {
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
		if err := copyGitPackEntries(ctx, stream, source, pack); err != nil {
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
func copyGitPackEntries(ctx context.Context, stream io.Writer, source gitstore.PackSource, pack *gitStoredPack) error {
	entries := pack.size - int64(gitPackHeaderSize) - int64(gitPackTrailerSize)
	if entries <= 0 {
		return fmt.Errorf("pack %s is %d bytes, too short to hold entries", pack.name, pack.size)
	}
	reader, err := source.OpenPack(ctx, pack.name)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	header := make([]byte, gitPackHeaderSize)
	// io.ReaderAt may report io.EOF beside a full read, so the count decides.
	if read, err := reader.ReadAt(header, 0); read != len(header) {
		return fmt.Errorf("read header of pack %s: %w", pack.name, err)
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
	copied, err := io.Copy(stream, io.NewSectionReader(reader, int64(gitPackHeaderSize), entries))
	if err != nil {
		return err
	}
	if copied != entries {
		return fmt.Errorf("pack %s ended %d bytes early", pack.name, entries-copied)
	}
	return nil
}
