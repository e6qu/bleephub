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
	"golang.org/x/sync/singleflight"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// A repository's objects are in two tiers. The packs are named by the manifest
// (state.go), never by a listing. The loose tier — one object a key, which is
// how the API's single-object writes land until a compaction packs them — is
// not in the manifest, because writing a loose object is one PUT and no commit;
// it is discovered by listing objects/, and what a listing found is held as a
// looseSnapshot: the membership of each objects/XX/ directory.
//
// The listing is taken only when it has to be: when an object is in none of the
// packs and the snapshot held is older than the freshness bound
// (Options.IndexFreshness), or there is none. A repository whose objects are all
// packed is therefore never listed by a read that finds what it is looking for.
// Listing after write is strongly consistent on every store this package runs
// on, so within the bound the snapshot is the truth. What this replica writes
// itself is added to the snapshot as it is written, so a writer is never stale
// about itself whatever the bound.
//
// A snapshot is immutable: whatever changes it builds a new one and swaps it in
// whole, so a reader takes one pointer and works from a consistent picture
// without a lock, and there is nothing half-built for two readers to race on.
//
// Every membership answer is negative-only: a filter's "no" is proof, its "yes"
// only sends the caller to look properly. See the filter invariant in filter.go.

const defaultObjectIndexFreshness = 250 * time.Millisecond

// storedPack is one pack as this process reads it. It is shared, by pointer,
// between every state whose manifest names it, so what has been read of it for
// one serves the next.
//
// A pack is described by two things beside its bytes, and each is read when
// something first needs it, not before. Its index says exactly where every
// object is, at some thirty bytes an object; a read needs it. Its membership
// filter says only which objects the pack cannot hold, at about one byte an
// object; a fetch negotiation, most of whose questions are about objects the
// repository does not have, is answered by it without the index being read at
// all. Each is read at most once, under its own mutex, and published only when
// complete: a reader sees it whole or not yet.
type storedPack struct {
	name string
	// objects is how many objects the pack holds, as the manifest records it.
	objects int
	pack    packExtents
	index   packExtents
	// filtered says whether a filter lies beside the pack, and filterExtents
	// where. A pack without one rules nothing out, and a probe goes to its index.
	filtered      bool
	filterExtents packExtents

	indexMu  sync.Mutex
	parsed   atomic.Pointer[idxfile.MemoryIndex]
	filterMu sync.Mutex
	filter   atomic.Pointer[binaryFuseFilter]
}

// loadIndex returns the pack's parsed index, reading it on first use. The index
// is read-only from here on and shared by every reader.
func (p *storedPack) loadIndex() (*idxfile.MemoryIndex, error) {
	if index := p.parsed.Load(); index != nil {
		return index, nil
	}
	p.indexMu.Lock()
	defer p.indexMu.Unlock()
	if index := p.parsed.Load(); index != nil {
		return index, nil
	}
	raw, err := p.index.readAll()
	if err != nil {
		return nil, fmt.Errorf("read index of %s: %w", p.name, err)
	}
	index := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(bytes.NewReader(raw)).Decode(index); err != nil {
		return nil, fmt.Errorf("decode index of %s: %w", p.name, err)
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
	raw, err := p.filterExtents.readAll()
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
		if probing && p.filtered {
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

// looseSnapshot is the loose tier as a listing found it, plus what this replica
// has written since. It is never modified once published.
type looseSnapshot struct {
	// at is when the listing behind the snapshot began: the bound is on how old
	// what it reports may be, and a long listing is old by the time it ends.
	at time.Time
	// filters are the membership of each objects/XX/ directory. Nil means the
	// directory is empty, which for a repository that pushes packs is all of them.
	filters [256]*cuckooFilter
}

// mayHold reports whether the loose tier could hold the object.
func (s *looseSnapshot) mayHold(hash plumbing.Hash) bool {
	return s.filters[hash[0]].contains(oidKeyFrom(hash[:]))
}

// with returns the snapshot as it is after this replica wrote hash loose. The
// receiver is untouched: only the one directory's filter is copied.
func (s *looseSnapshot) with(hash plumbing.Hash) *looseSnapshot {
	next := *s
	fanout := hash[0]
	filter := s.filters[fanout].copyForWrite()
	filter.insert(oidKeyFrom(hash[:]))
	next.filters[fanout] = filter
	return &next
}

// without returns the snapshot with loose objects removed that this replica has
// packed. Removing one clears a fingerprint from a filter, which is sound only
// for an object the filter was told of: clearing one it never held could take a
// colliding object's fingerprint with it, and turn a filter's "no" into a lie.
// So a compaction calls this BEFORE it deletes the keys, while every listing
// there has ever been still shows them.
func (s *looseSnapshot) without(loose []plumbing.Hash) *looseSnapshot {
	next := *s
	copied := map[byte]bool{}
	for _, hash := range loose {
		fanout := hash[0]
		if next.filters[fanout] == nil {
			continue
		}
		if !copied[fanout] {
			next.filters[fanout] = next.filters[fanout].copyForWrite()
			copied[fanout] = true
		}
		next.filters[fanout].remove(oidKeyFrom(hash[:]))
	}
	return &next
}

func sortPacks(packs []*storedPack) {
	sort.Slice(packs, func(i, j int) bool {
		if packs[i].pack.size != packs[j].pack.size {
			return packs[i].pack.size > packs[j].pack.size
		}
		return packs[i].name < packs[j].name
	})
}

// looseObject pairs a loose object's hash with the key holding it.
type looseObject struct {
	hash plumbing.Hash
	key  string
}

// listedObject is one key of objects/pack/ or of the reference snapshots as a
// listing reported it.
type listedObject struct {
	modified time.Time
	size     int64
}

// objectsListing is one recursive listing of objects/. A reader wants only the
// loose snapshot made from it; a compaction also wants the rest — which loose
// keys to pack, and which packs and reference snapshots lie in the store that
// the manifest does not name.
type objectsListing struct {
	at    time.Time
	loose []looseObject
	// packDirectory is every key of objects/pack/, by its name within it.
	packDirectory map[string]listedObject
	// refSnapshots is every reference snapshot, by its key within the repository.
	refSnapshots map[string]listedObject
}

// looseTier owns a repository's loose snapshots: it takes the listings,
// publishes what they and this replica's own writes add up to, and hands the
// current one to readers.
type looseTier struct {
	shared *storeShared
	// prefix is the repository's key prefix, ending in "/".
	prefix string

	current atomic.Pointer[looseSnapshot]

	// mu serializes publication, so that two changes are never both built on the
	// same predecessor and one of them lost.
	mu sync.Mutex
	// listings counts the listings in flight, and journal holds what this
	// replica wrote while one was. A listing that began before such a write
	// finished may not show it, so the write is replayed onto the snapshot the
	// listing becomes.
	listings int
	journal  []plumbing.Hash

	// flight makes concurrent refreshes one listing.
	flight singleflight.Group
}

// listedLoose is what one listing produced.
type listedLoose struct {
	listing  *objectsListing
	snapshot *looseSnapshot
}

// fresh reports whether a snapshot may still answer "absent" by itself.
func (t *looseTier) fresh(snapshot *looseSnapshot) bool {
	freshness := t.shared.opts.IndexFreshness
	return snapshot != nil && freshness > 0 && t.shared.now().Sub(snapshot.at) <= freshness
}

// refresh lists objects/ and publishes the loose snapshot that results.
func (t *looseTier) refresh() (listedLoose, error) {
	result, err, _ := t.flight.Do("objects", func() (any, error) {
		t.mu.Lock()
		t.listings++
		t.mu.Unlock()

		listing, err := t.list()
		var built *looseSnapshot
		if err == nil {
			built = buildLooseSnapshot(listing)
		}

		t.mu.Lock()
		defer t.mu.Unlock()
		journal := t.journal
		t.listings--
		if t.listings == 0 {
			t.journal = nil
		}
		if err != nil {
			return nil, err
		}
		for _, hash := range journal {
			built = built.with(hash)
		}
		t.current.Store(built)
		return listedLoose{listing: listing, snapshot: built}, nil
	})
	if err != nil {
		return listedLoose{}, err
	}
	return result.(listedLoose), nil
}

// list walks objects/ once and sorts what it finds.
func (t *looseTier) list() (*objectsListing, error) {
	prefix := t.prefix + "objects/"
	packPrefix := prefix + "pack/"
	snapshotPrefix := t.prefix + refSnapshotDirectory
	listing := &objectsListing{at: t.shared.now(), packDirectory: map[string]listedObject{}, refSnapshots: map[string]listedObject{}}
	err := t.shared.list(t.shared.baseContext(), prefix, func(entry objstore.Entry) {
		if name, inPackDirectory := strings.CutPrefix(entry.Key, packPrefix); inPackDirectory {
			listing.packDirectory[name] = listedObject{modified: entry.ModTime, size: entry.Size}
			return
		}
		if strings.HasPrefix(entry.Key, snapshotPrefix) {
			listing.refSnapshots[strings.TrimPrefix(entry.Key, t.prefix)] = listedObject{modified: entry.ModTime, size: entry.Size}
			return
		}
		fanout, base, found := strings.Cut(strings.TrimPrefix(entry.Key, prefix), "/")
		if !found || len(fanout) != 2 || !plumbing.IsHash(fanout+base) {
			return
		}
		hash := plumbing.NewHash(fanout + base)
		if hash.IsZero() {
			return
		}
		listing.loose = append(listing.loose, looseObject{hash: hash, key: entry.Key})
	})
	if err != nil {
		return nil, fmt.Errorf("list objects of %s: %w", strings.TrimSuffix(t.prefix, "/"), err)
	}
	return listing, nil
}

// buildLooseSnapshot turns a listing into a snapshot.
func buildLooseSnapshot(listing *objectsListing) *looseSnapshot {
	snapshot := &looseSnapshot{at: listing.at}
	counts := [256]int{}
	for _, object := range listing.loose {
		counts[object.hash[0]]++
	}
	for _, object := range listing.loose {
		fanout := object.hash[0]
		if snapshot.filters[fanout] == nil {
			snapshot.filters[fanout] = newCuckooFilter(max(counts[fanout], cuckooMinimumCapacity))
		}
		snapshot.filters[fanout].insert(oidKeyFrom(object.hash[:]))
	}
	return snapshot
}

// wrote publishes a loose object this replica wrote and, if a listing is in
// flight, keeps it to be replayed onto what that listing becomes. With no
// snapshot yet there is nothing to update: the first listing comes after the
// write, and a listing after a write shows it.
func (t *looseTier) wrote(hash plumbing.Hash) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current := t.current.Load(); current != nil {
		t.current.Store(current.with(hash))
	}
	if t.listings > 0 {
		t.journal = append(t.journal, hash)
	}
}

// packed publishes the removal of loose objects this replica has packed; see
// without for when that is sound. It is not journalled: a listing in flight
// either still shows what was packed, which costs a read that finds it gone and
// lists again, or no longer does, and replaying the removal onto it would clear
// fingerprints it never inserted.
func (t *looseTier) packed(loose []plumbing.Hash) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current := t.current.Load(); current != nil {
		t.current.Store(current.without(loose))
	}
}

// errStaleSnapshot reports that the store no longer holds a pack the manifest
// held names: a compaction elsewhere retired it, and its grace period has since
// run out. It is proof that the manifest held is out of date whatever its age,
// and the answer to it is to read the manifest again.
var errStaleSnapshot = errors.New("the repository changed underneath the manifest held")
