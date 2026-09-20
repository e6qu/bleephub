package gitstore

import (
	"bytes"
	"errors"
	"fmt"
	"path"
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

// What a repository holds is known to a handle as a snapshot: the live packs,
// each with its membership filter and its parsed index once something has
// needed them; and the membership of the loose tier. A snapshot is immutable. Whatever
// changes what the repository holds — a listing, a pack this replica published,
// an object it wrote — builds a new snapshot and swaps it in whole, so a reader
// takes one pointer and works from a consistent picture without a lock, and
// there is nothing half-built for two readers to race on.
//
// A snapshot comes from ONE recursive listing of objects/, which names both
// tiers. It answers "absent" on its own authority only while it is younger than
// the freshness bound (Options.IndexFreshness); past that, a miss re-lists and
// looks again before it is believed, because another replica may have published
// a pack since. Listing after write is strongly consistent on every store this
// package runs on, so within the bound the snapshot is the truth, and the bound
// batches the questions a fetch negotiation asks about objects the repository
// does not have into a few listings a second. What this replica writes itself
// is added to the snapshot as it is written, so a writer is never stale about
// itself whatever the bound.
//
// Every membership answer is negative-only: a filter's "no" is proof, its "yes"
// only sends the caller to look properly. See the filter invariant in filter.go.

const defaultObjectIndexFreshness = 250 * time.Millisecond

// storedPack is one live pack. It is shared, by pointer, between every snapshot
// that lists it, so what has been read of it for one snapshot serves the next.
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
	name  string
	pack  packExtents
	index packExtents
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

// tierSnapshot is what a repository held when a listing looked, plus what this
// replica has added since. It is never modified once published.
type tierSnapshot struct {
	// at is when the listing behind the snapshot began: the bound is on how old
	// what it reports may be, and a long listing is old by the time it ends.
	at time.Time
	// packs are the live packs, largest first, since that is where an object
	// most likely is.
	packs []*storedPack
	// loose is the membership of each objects/XX/ directory. Nil means the
	// directory is empty, which for a repository that pushes packs is all of them.
	loose [256]*cuckooFilter
}

// looseMayHold reports whether the loose tier could hold the object.
func (s *tierSnapshot) looseMayHold(hash plumbing.Hash) bool {
	return s.loose[hash[0]].contains(oidKeyFrom(hash[:]))
}

// pack returns the live pack of that name, or nil.
func (s *tierSnapshot) pack(name string) *storedPack {
	for _, pack := range s.packs {
		if pack.name == name {
			return pack
		}
	}
	return nil
}

// tierChange is something this replica did to the repository: it published a
// pack, or wrote a loose object.
type tierChange struct {
	pack  *storedPack
	loose plumbing.Hash
}

// with returns the snapshot as it is after change. The receiver is untouched:
// only what the change reaches is copied.
func (s *tierSnapshot) with(change tierChange) *tierSnapshot {
	next := *s
	if change.pack != nil {
		next.packs = make([]*storedPack, 0, len(s.packs)+1)
		for _, pack := range s.packs {
			// A listing may have found the pack before its publisher got here;
			// the publisher's copy has the index already parsed.
			if pack.name != change.pack.name {
				next.packs = append(next.packs, pack)
			}
		}
		next.packs = append(next.packs, change.pack)
		sortPacks(next.packs)
		return &next
	}
	fanout := change.loose[0]
	filter := s.loose[fanout].copyForWrite()
	filter.insert(oidKeyFrom(change.loose[:]))
	next.loose[fanout] = filter
	return &next
}

// without returns the snapshot with packs and loose objects removed that this
// replica is retiring. Removing a loose object clears a fingerprint from a
// filter, which is sound only for an object the filter was told of: clearing
// one it never held could take a colliding object's fingerprint with it, and
// turn a filter's "no" into a lie. So a compaction calls this BEFORE it deletes
// the keys, while every listing there has ever been still shows them.
func (s *tierSnapshot) without(packs []string, loose []plumbing.Hash) *tierSnapshot {
	next := *s
	if len(packs) > 0 {
		gone := make(map[string]bool, len(packs))
		for _, name := range packs {
			gone[name] = true
		}
		next.packs = make([]*storedPack, 0, len(s.packs))
		for _, pack := range s.packs {
			if !gone[pack.name] {
				next.packs = append(next.packs, pack)
			}
		}
	}
	copied := map[byte]bool{}
	for _, hash := range loose {
		fanout := hash[0]
		if next.loose[fanout] == nil {
			continue
		}
		if !copied[fanout] {
			next.loose[fanout] = next.loose[fanout].copyForWrite()
			copied[fanout] = true
		}
		next.loose[fanout].remove(oidKeyFrom(hash[:]))
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

// packDirectoryEntry is one key of objects/pack/ as a listing reported it.
type packDirectoryEntry struct {
	modified time.Time
	size     int64
}

// tierListing is one recursive listing of objects/, sorted into the two tiers.
// A reader wants only the snapshot made from it; a compaction also wants the
// raw entries — which loose keys to pack, which superseded packs have aged out.
type tierListing struct {
	at    time.Time
	loose []looseObject
	// packDirectory is every key of objects/pack/, by its name within it.
	packDirectory map[string]packDirectoryEntry
}

// livePacks names the packs a reader may adopt: those whose .pack and .idx are
// both there, which is what makes a pack visible all at once however its parts
// were uploaded, and which carry no supersession marker. A superseded pack
// stays readable by key for its retention window, for whoever was already
// reading it, but holds nothing its replacement does not.
func (l *tierListing) livePacks() []string {
	var names []string
	for entry := range l.packDirectory {
		name, isPack := strings.CutSuffix(entry, ".pack")
		if !isPack || !strings.HasPrefix(name, "pack-") {
			continue
		}
		if _, indexed := l.packDirectory[name+".idx"]; !indexed {
			continue
		}
		if _, superseded := l.packDirectory[name+".superseded"]; superseded {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// objectTiers owns a repository's snapshots: it takes the listings, publishes
// what they and this replica's own writes add up to, and hands the current one
// to readers.
type objectTiers struct {
	shared *storeShared
	// prefix is the repository's key prefix, ending in "/".
	prefix string

	current atomic.Pointer[tierSnapshot]

	// mu serializes publication, so that two changes are never both built on the
	// same predecessor and one of them lost.
	mu sync.Mutex
	// listings counts the listings in flight, and journal holds what this
	// replica added while one was. A listing that began before such a write
	// finished may not show it, so the write is replayed onto the snapshot the
	// listing becomes.
	listings int
	journal  []tierChange

	// flight makes concurrent refreshes one listing. It is what keeps the first
	// clones to arrive at a replica that has just started — all wanting a
	// snapshot that does not exist yet — from each taking their own.
	flight singleflight.Group
}

// refreshed is what one listing produced.
type refreshed struct {
	listing  *tierListing
	snapshot *tierSnapshot
}

// snapshot returns the current snapshot, taking the first if there is none.
func (t *objectTiers) snapshot() (*tierSnapshot, error) {
	if current := t.current.Load(); current != nil {
		return current, nil
	}
	result, err := t.refresh()
	if err != nil {
		return nil, err
	}
	return result.snapshot, nil
}

// fresh reports whether a snapshot may still answer "absent" by itself.
func (t *objectTiers) fresh(snapshot *tierSnapshot) bool {
	freshness := t.shared.opts.IndexFreshness
	return freshness > 0 && t.shared.now().Sub(snapshot.at) <= freshness
}

// refresh lists the repository and publishes the snapshot that results.
func (t *objectTiers) refresh() (refreshed, error) {
	result, err, _ := t.flight.Do("objects", func() (any, error) {
		t.mu.Lock()
		t.listings++
		t.mu.Unlock()

		listing, err := t.list()
		var built *tierSnapshot
		if err == nil {
			built = t.build(listing)
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
		for _, change := range journal {
			built = built.with(change)
		}
		t.current.Store(built)
		return refreshed{listing: listing, snapshot: built}, nil
	})
	if err != nil {
		return refreshed{}, err
	}
	return result.(refreshed), nil
}

// list walks objects/ once and sorts what it finds into the two tiers.
func (t *objectTiers) list() (*tierListing, error) {
	prefix := t.prefix + "objects/"
	packPrefix := prefix + "pack/"
	listing := &tierListing{at: t.shared.now(), packDirectory: map[string]packDirectoryEntry{}}
	err := t.shared.list(t.shared.baseContext(), prefix, func(entry objstore.Entry) {
		if name, inPackDirectory := strings.CutPrefix(entry.Key, packPrefix); inPackDirectory {
			listing.packDirectory[name] = packDirectoryEntry{modified: entry.ModTime, size: entry.Size}
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

// build turns a listing into a snapshot. A pack the previous snapshot already
// held is carried over with whatever has been read of it. Nothing is read here:
// a pack's index and filter are read when something first needs them.
func (t *objectTiers) build(listing *tierListing) *tierSnapshot {
	snapshot := &tierSnapshot{at: listing.at}
	previous := t.current.Load()
	for _, name := range listing.livePacks() {
		if previous != nil {
			if known := previous.pack(name); known != nil {
				snapshot.packs = append(snapshot.packs, known)
				continue
			}
		}
		filterEntry, filtered := listing.packDirectory[name+".bfilter"]
		snapshot.packs = append(snapshot.packs, &storedPack{
			name:          name,
			pack:          t.extents(name+".pack", listing.packDirectory[name+".pack"].size),
			index:         t.extents(name+".idx", listing.packDirectory[name+".idx"].size),
			filtered:      filtered,
			filterExtents: t.extents(name+".bfilter", filterEntry.size),
		})
	}
	sortPacks(snapshot.packs)

	counts := [256]int{}
	for _, object := range listing.loose {
		counts[object.hash[0]]++
	}
	for _, object := range listing.loose {
		fanout := object.hash[0]
		if snapshot.loose[fanout] == nil {
			snapshot.loose[fanout] = newCuckooFilter(max(counts[fanout], cuckooMinimumCapacity))
		}
		snapshot.loose[fanout].insert(oidKeyFrom(object.hash[:]))
	}
	return snapshot
}

// extents addresses one artefact of the pack directory.
func (t *objectTiers) extents(name string, size int64) packExtents {
	return packExtents{shared: t.shared, key: t.prefix + path.Join("objects", "pack", name), size: size}
}

// apply publishes a change this replica made and, if a listing is in flight,
// keeps it to be replayed onto what that listing becomes. With no snapshot yet
// there is nothing to update: the first listing comes after the write, and a
// listing after a write shows it.
func (t *objectTiers) apply(change tierChange) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current := t.current.Load(); current != nil {
		t.current.Store(current.with(change))
	}
	if t.listings > 0 {
		t.journal = append(t.journal, change)
	}
}

// retire publishes the removal of packs and loose objects this replica is
// retiring; see without for when that is sound. It is not journalled: a listing
// in flight either still shows what was retired, which costs a read that finds
// it gone and lists again, or no longer does, and replaying the removal onto it
// would clear fingerprints it never inserted.
func (t *objectTiers) retire(packs []string, loose []plumbing.Hash) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current := t.current.Load(); current != nil {
		t.current.Store(current.without(packs, loose))
	}
}

// errStaleSnapshot reports that the store no longer holds a pack the snapshot
// named: a merge elsewhere superseded it, and its retention window has since
// run out. It is proof that the snapshot is out of date whatever its age, and
// the answer to it is to list again.
var errStaleSnapshot = errors.New("the repository changed underneath the snapshot")
