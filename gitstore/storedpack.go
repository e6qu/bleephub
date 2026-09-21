package gitstore

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Every object of a repository is in a pack, and the packs are named by the
// manifest (state.go), never by a listing. An object this replica has written
// and not yet packed is pending (pending.go), and is read from memory.

// defaultObjectIndexFreshness is how long a manifest the store vouched for may
// answer that an object is absent before it is asked again.
const defaultObjectIndexFreshness = 250 * time.Millisecond

// storedPack is one pack as this process reads it. It is shared, by pointer,
// between every state whose manifest names it, so what has been read of it for
// one serves the next.
//
// A pack is described by two things beside its bytes, both kept in its sidecar
// (sidecar.go), and each is read when something first needs it, not before. Its
// index says exactly where every object is, at some thirty bytes an object; a
// read needs it. Its membership filter says only which objects the pack cannot
// hold, at about one byte an object; a fetch negotiation, most of whose
// questions are about objects the repository does not have, is answered by it
// without the index being read at all. Each is read at most once, under its own
// mutex, and published only when complete: a reader sees it whole or not yet.
type storedPack struct {
	name string
	// objects is how many objects the pack holds, as the manifest records it.
	objects int
	pack    packExtents
	// sidecar holds the index, then the filter, then the footer; indexBytes and
	// filterBytes are the lengths of the first two. A pack with no filter rules
	// nothing out, and a probe goes to its index.
	sidecar     packExtents
	indexBytes  int64
	filterBytes int64

	indexMu  sync.Mutex
	parsed   atomic.Pointer[idxfile.MemoryIndex]
	filterMu sync.Mutex
	filter   atomic.Pointer[binaryFuseFilter]
}

// loadIndex returns the pack's parsed index, reading it on first use. The index
// is read-only from here on and shared by every reader. The sidecar's footer is
// read with it and must describe this pack as the manifest does.
func (p *storedPack) loadIndex() (*idxfile.MemoryIndex, error) {
	if index := p.parsed.Load(); index != nil {
		return index, nil
	}
	p.indexMu.Lock()
	defer p.indexMu.Unlock()
	if index := p.parsed.Load(); index != nil {
		return index, nil
	}
	rawFooter, err := p.sidecar.readRange(p.sidecar.size-int64(sidecarFooterSize), int64(sidecarFooterSize))
	if err != nil {
		return nil, fmt.Errorf("read sidecar of %s: %w", p.name, err)
	}
	footer, err := decodeSidecarFooter(rawFooter)
	if err != nil {
		return nil, fmt.Errorf("sidecar of %s: %w", p.name, err)
	}
	if "pack-"+footer.pack.String() != p.name || footer.indexBytes != p.indexBytes || footer.filterBytes != p.filterBytes {
		return nil, fmt.Errorf("sidecar of %s: %w: it describes pack-%s with an index of %d bytes and a filter of %d, the manifest an index of %d and a filter of %d",
			p.name, errSidecar, footer.pack, footer.indexBytes, footer.filterBytes, p.indexBytes, p.filterBytes)
	}
	raw, err := p.sidecar.readRange(0, p.indexBytes)
	if err != nil {
		return nil, fmt.Errorf("read index of %s: %w", p.name, err)
	}
	index := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(bytes.NewReader(raw)).Decode(index); err != nil {
		return nil, fmt.Errorf("decode index of %s: %w", p.name, err)
	}
	if "pack-"+plumbing.Hash(index.PackfileChecksum).String() != p.name {
		return nil, fmt.Errorf("index of %s: %w: it indexes pack-%s", p.name, errSidecar, plumbing.Hash(index.PackfileChecksum))
	}
	p.parsed.Store(index)
	return index, nil
}

// loadFilter returns the pack's membership filter, reading it on first use.
func (p *storedPack) loadFilter() (*binaryFuseFilter, error) {
	if filter := p.filter.Load(); filter != nil {
		return filter, nil
	}
	p.filterMu.Lock()
	defer p.filterMu.Unlock()
	if filter := p.filter.Load(); filter != nil {
		return filter, nil
	}
	raw, err := p.sidecar.readRange(p.indexBytes, p.filterBytes)
	if err != nil {
		return nil, fmt.Errorf("read filter of %s: %w", p.name, err)
	}
	filter, err := decodeBinaryFuseFilter(raw)
	if err != nil {
		return nil, fmt.Errorf("decode filter of %s: %w", p.name, err)
	}
	p.filter.Store(filter)
	return filter, nil
}

// find locates an object in the pack: its index and, for a read, the object's
// offset; or plumbing.ErrObjectNotFound. An index already in memory answers exactly and at
// once. Otherwise what is read depends on who is asking. A probe — does the
// repository have this? — reads the filter, and the index only if the filter
// cannot rule the pack out; a read of the object goes straight to the index it
// is going to need.
func (p *storedPack) find(hash plumbing.Hash, probing bool) (*idxfile.MemoryIndex, int64, error) {
	index := p.parsed.Load()
	if index == nil {
		if probing && p.filterBytes > 0 {
			filter, err := p.loadFilter()
			if err != nil {
				return nil, 0, err
			}
			if !filter.contains(oidKeyFrom(hash[:])) {
				return nil, 0, plumbing.ErrObjectNotFound
			}
		}
		var err error
		if index, err = p.loadIndex(); err != nil {
			return nil, 0, err
		}
	}
	if probing {
		// Contains, unlike FindOffset, records nothing, and so takes no lock:
		// a negotiation's probes do not queue behind one another.
		held, err := index.Contains(hash)
		if err == nil && !held {
			err = plumbing.ErrObjectNotFound
		}
		return index, 0, err
	}
	offset, err := index.FindOffset(hash)
	return index, offset, err
}

func sortPacks(packs []*storedPack) {
	sort.Slice(packs, func(i, j int) bool {
		if packs[i].pack.size != packs[j].pack.size {
			return packs[i].pack.size > packs[j].pack.size
		}
		return packs[i].name < packs[j].name
	})
}

// listedObject is one key of objects/pack/ or of the reference snapshots as a
// listing reported it.
type listedObject struct {
	modified time.Time
}

// objectsListing is one recursive listing of objects/, which only a compaction
// takes: which packs and reference snapshots lie in the store, so that those no
// manifest names can be found and swept.
type objectsListing struct {
	// packDirectory is every key of objects/pack/, by its name within it.
	packDirectory map[string]listedObject
	// refSnapshots is every reference snapshot, by its key within the repository.
	refSnapshots map[string]listedObject
}

// listObjects walks the repository's objects/ once.
func (r *repository) listObjects() (*objectsListing, error) {
	prefix := r.prefix + "objects/"
	packPrefix := prefix + "pack/"
	snapshotPrefix := r.prefix + refSnapshotDirectory
	listing := &objectsListing{packDirectory: map[string]listedObject{}, refSnapshots: map[string]listedObject{}}
	err := r.shared.list(r.shared.baseContext(), prefix, func(entry objstore.Entry) {
		if name, inPackDirectory := strings.CutPrefix(entry.Key, packPrefix); inPackDirectory {
			listing.packDirectory[name] = listedObject{modified: entry.ModTime}
			return
		}
		if strings.HasPrefix(entry.Key, snapshotPrefix) {
			listing.refSnapshots[strings.TrimPrefix(entry.Key, r.prefix)] = listedObject{modified: entry.ModTime}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("list objects of %s: %w", r.name, err)
	}
	return listing, nil
}

// errStaleSnapshot reports that the store no longer holds a pack the manifest
// held names: a compaction elsewhere retired it, and its grace period has since
// run out. It is proof that the manifest held is out of date whatever its age,
// and the answer to it is to read the manifest again.
var errStaleSnapshot = errors.New("the repository changed underneath the manifest held")
