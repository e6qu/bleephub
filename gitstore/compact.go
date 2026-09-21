package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Compaction turns the loose tier into the pack tier.
//
// ORDERING / CRASH SAFETY. A pack is invisible to a reader until its .pack key
// exists beside its .idx, and a PUT or a multipart completion publishes an
// object all-or-nothing. So .pack is written last:
//
//	1. build the pack and its index on local disk
//	2. upload pack-<sha>.idx
//	3. upload pack-<sha>.bfilter
//	4. upload pack-<sha>.pack        <- the commit point
//	5. publish a snapshot that holds the new pack, and no longer the loose
//	   objects it packed
//	6. delete the loose keys that step 1 actually packed
//
// Crash before step 4: loose objects stay authoritative; the orphan .idx and
// .bfilter name a pack no reader adopts. Crash between 4 and 6: pack and some
// loose objects both exist; a reader takes the packed copy and the next
// compaction removes the duplicates. No object is ever in neither place, because
// a loose delete strictly follows publication of the pack holding it.
//
// CONCURRENCY. Step 6 deletes only the keys this compaction listed and packed,
// so an object written after the listing survives as loose. Two replicas may
// list the same keys; each writes a content-named pack (no collision) and
// deletes only what it packed, so every deleted key is in some published pack.
// If one replica reads a loose object the other already deleted mid-build, the
// read finds nothing and buildPackTolerantly drops it and rebuilds — safe
// because the only way that key is gone is that the other replica already
// packed it. The durable lock avoids the duplicated work but correctness does
// not depend on it (its lease can expire under a long compaction). Against a
// concurrent read on this replica, step 5 precedes step 6, and a reader looks in
// the packs before the loose tier, so it finds the object loose before the swap
// and packed after. A reader on another replica that still lists the object as
// loose finds the key gone, takes that as proof its snapshot is out of date,
// lists again, and finds the pack.

const (
	// compactionMinLooseObjects is the loose count below which packing isn't worth its round trips.
	compactionMinLooseObjects = 64
	// compactionPackWindow is the delta window the pack encoder searches.
	compactionPackWindow = 10
	// compactionMergeThreshold is the live pack count above which a compaction
	// also rewrites existing packs. go-git loads every pack's index before
	// answering any packed lookup, so one-pack-per-push would give back what
	// packing bought.
	compactionMergeThreshold = 8
	// compactionGeometricFactor shapes which packs a merge rewrites: a pack is
	// left alone while it is at least this many times the size of everything
	// smaller than it put together. A push lands as a pack of its own, so
	// merging every pack each time the count crossed the threshold would rewrite
	// the whole repository every few pushes; keeping the sizes geometric bounds
	// the pack count by the logarithm of the repository's size while each byte
	// is rewritten only a logarithmic number of times. It is the rule behind
	// `git repack --geometric`.
	compactionGeometricFactor = 2
	// supersededPackRetention is how long a merged-away pack is kept before its
	// bytes are removed, for requests that began before the merge and still read it.
	supersededPackRetention = time.Hour
	// defaultMultipartThreshold is the size above which a pack is uploaded in
	// parts. Configurable because non-Amazon endpoints cap the single-request
	// upload well below Amazon's 5 GiB.
	defaultMultipartThreshold = 64 << 20
)

// CompactionResult reports what one compaction did.
type CompactionResult struct {
	// Packed is the number of loose objects written into the new pack.
	Packed int
	// Merged is the number of previously packed objects rewritten into it.
	Merged int
	// PackName is the published pack-<sha> base name; empty when nothing to do.
	PackName string
	// PackBytes is the size of the published packfile.
	PackBytes int64
	// RetiredPacks lists packs whose bytes were removed after their retention window.
	RetiredPacks []string
	// FilterBytes is the size of the membership filter published beside the pack.
	FilterBytes int
}

// Compactor is a git storage handle that can pack its own loose objects.
// Local-filesystem storage does not implement it: git's own maintenance owns
// that layout and a loose object there is one file open, not a round trip.
type Compactor interface {
	Compact(ctx context.Context) (CompactionResult, error)
}

// CompactRepository packs a repository's loose objects when its storage
// supports it, reporting an empty result otherwise.
func CompactRepository(ctx context.Context, stor gitStorage.Storer) (CompactionResult, error) {
	compactor, ok := stor.(Compactor)
	if !ok {
		return CompactionResult{}, nil
	}
	return compactor.Compact(ctx)
}

// defaultCompactionTrigger is deliberately larger than an ordinary push, so the
// once-per-object read a compaction costs is amortized over a batch worth
// packing.
const defaultCompactionTrigger = 4096

// noteObjectWritten counts a loose write and asks for a compaction once enough
// accumulate. Only one compaction per repository runs at a time in this process;
// one already running absorbs concurrent writes, since it deletes only the keys
// it listed.
func (r *repository) noteObjectWritten() {
	trigger := r.shared.opts.CompactionTrigger
	if trigger <= 0 || r.looseWrites.Add(1) < trigger {
		return
	}
	r.requestCompaction()
}

// notePackWritten decides, as a push lands, whether the repository is due a
// compaction, and asks for one only then. A compaction opens with a listing of
// the whole object tree, so one run after every push to find nothing to do was
// a request per push; what it would find is already known here.
//
// Two things make one due. Packs: a run of small pushes leaves a pack each, and
// every lookup asks every pack. The count is the snapshot's — the repository's
// live packs, not a tally of this process's pushes — so packs an earlier process
// left are counted after a restart. And loose objects: the API's object writes
// land loose, below the write trigger for a long time, and a push is the natural
// moment to fold in a tier worth packing.
func (r *repository) notePackWritten(livePacks int) {
	if r.shared.opts.CompactionTrigger <= 0 {
		return
	}
	if livePacks <= compactionMergeThreshold && r.looseWrites.Load() < compactionMinLooseObjects {
		return
	}
	r.requestCompaction()
}

func (r *repository) requestCompaction() {
	r.looseWrites.Store(0)
	RequestCompaction(r.name, r)
}

// RequestCompaction asks the installed handler to compact a repository, and
// reports whether one is installed. The storage calls it when a write leaves a
// repository due a compaction; an application may call it for reasons of its
// own, such as an operator asking.
func RequestCompaction(repo string, stor gitStorage.Storer) bool {
	request := compactionRequestHook()
	if request == nil {
		return false
	}
	request(repo, stor)
	return true
}

// compactionRequestFunc is invoked when a repository's loose tier fills. The
// package owns no goroutine lifecycle, so the flush is delegated to an installed
// handler: the server's scheduler runs one supervised, cancel-at-shutdown
// compaction per repository. Left unset (tests, embeddings), the loose tier just
// accumulates — slower, not broken. Needed because the REST git-database
// endpoints write objects directly, outside the post-receive path.
var (
	compactionRequestMu   sync.RWMutex
	compactionRequestFunc func(repo string, stor gitStorage.Storer)
)

// SetCompactionRequestHandler installs the loose-tier-full handler; nil removes it.
func SetCompactionRequestHandler(request func(repo string, stor gitStorage.Storer)) {
	compactionRequestMu.Lock()
	defer compactionRequestMu.Unlock()
	compactionRequestFunc = request
}

func compactionRequestHook() func(repo string, stor gitStorage.Storer) {
	compactionRequestMu.RLock()
	defer compactionRequestMu.RUnlock()
	return compactionRequestFunc
}

// Compact runs one compaction of this repository. See the ordering and
// concurrency argument at the top of this file.
func (r *repository) Compact(ctx context.Context) (CompactionResult, error) {
	var result CompactionResult
	err := withLockName(lockNameFor("git-compact", r.name, "compaction"), func() error {
		var err error
		result, err = r.compactLocked(ctx)
		return err
	})
	return result, err
}

func (r *repository) compactLocked(ctx context.Context) (CompactionResult, error) {
	var result CompactionResult

	// One listing of objects/ answers everything a compaction asks before it
	// decides: what is loose, which superseded packs have aged out, and which
	// packs are live. It is the same listing a reader's snapshot comes from, and
	// leaves one behind. A compaction usually finds nothing to do, so each
	// further request here would be spent learning what the first had said.
	listed, err := r.tiers.refresh()
	if err != nil {
		return result, err
	}
	retired, err := r.retireSupersededPacks(ctx, listed.listing)
	if err != nil {
		return result, err
	}
	result.RetiredPacks = retired

	live := make([]livePack, 0, len(listed.snapshot.packs))
	for _, pack := range listed.snapshot.packs {
		live = append(live, livePack{name: pack.name, size: pack.pack.size})
	}
	var existing []string
	if len(live) > compactionMergeThreshold {
		existing = packsToMerge(live)
	}
	merge := len(existing) > 0

	loose := listed.listing.loose
	if len(loose) < compactionMinLooseObjects && !merge {
		return result, nil
	}

	var mergedHashes []plumbing.Hash
	if merge {
		mergedHashes, err = hashesInPacks(listed.snapshot, existing)
		if err != nil {
			return result, err
		}
	}

	hashes := make([]plumbing.Hash, 0, len(loose)+len(mergedHashes))
	seen := make(map[plumbing.Hash]bool, len(loose)+len(mergedHashes))
	packedLoose := make([]looseObject, 0, len(loose))
	for _, obj := range loose {
		if seen[obj.hash] {
			continue
		}
		seen[obj.hash] = true
		hashes = append(hashes, obj.hash)
		packedLoose = append(packedLoose, obj)
	}
	for _, hash := range mergedHashes {
		if seen[hash] {
			continue
		}
		seen[hash] = true
		hashes = append(hashes, hash)
		result.Merged++
	}
	if len(hashes) == 0 {
		return result, nil
	}

	built, survivors, err := r.buildPackTolerantly(hashes)
	if err != nil {
		return result, err
	}
	defer built.cleanup()
	packedLoose = retainPacked(packedLoose, survivors)
	result.Packed = len(packedLoose)

	published, err := r.publishPack(ctx, built)
	if err != nil {
		return result, err
	}
	result.PackName = built.name
	result.PackBytes = built.packSize
	result.FilterBytes = built.filterBits / 8

	// Make the pack visible, and stop listing as loose what it packed, before
	// deleting any loose key (step 5 before 6): a reader never looks only where
	// the object no longer is. The snapshot forgets the keys while every listing
	// there has been still shows them, which is what makes forgetting them sound.
	r.tiers.apply(tierChange{pack: published})
	packedHashes := make([]plumbing.Hash, 0, len(packedLoose))
	for _, object := range packedLoose {
		packedHashes = append(packedHashes, object.hash)
	}
	r.tiers.retire(nil, packedHashes)

	if err := r.deleteLooseObjects(ctx, packedLoose); err != nil {
		return result, err
	}
	if merge {
		superseded := slices.DeleteFunc(existing, func(name string) bool { return name == built.name })
		if err := r.markSuperseded(ctx, superseded, built.name); err != nil {
			return result, err
		}
		r.tiers.retire(superseded, nil)
	}
	return result, nil
}

// buildPackTolerantly encodes the pack; if an object vanished between listing
// and read (only possible when another replica already packed it), it drops the
// object and retries. Retrying beats probing every object up front, which would
// cost the per-object round trip compaction exists to remove.
func (r *repository) buildPackTolerantly(hashes []plumbing.Hash) (*builtPack, map[plumbing.Hash]bool, error) {
	built, err := r.buildPack(hashes)
	if err == nil {
		return built, hashSet(hashes), nil
	}
	if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, nil, err
	}

	survivors := make([]plumbing.Hash, 0, len(hashes))
	for _, hash := range hashes {
		switch probeErr := r.HasEncodedObject(hash); {
		case probeErr == nil:
			survivors = append(survivors, hash)
		case !errors.Is(probeErr, plumbing.ErrObjectNotFound):
			// Not knowing whether an object survives is not the same as its
			// being gone, and must not drop it from the pack.
			return nil, nil, probeErr
		}
	}
	if len(survivors) == 0 {
		return nil, nil, err
	}
	built, err = r.buildPack(survivors)
	if err != nil {
		return nil, nil, err
	}
	return built, hashSet(survivors), nil
}

func hashSet(hashes []plumbing.Hash) map[plumbing.Hash]bool {
	set := make(map[plumbing.Hash]bool, len(hashes))
	for _, hash := range hashes {
		set[hash] = true
	}
	return set
}

// retainPacked narrows the loose keys due for deletion to those that made it
// into the published pack.
func retainPacked(objects []looseObject, packed map[plumbing.Hash]bool) []looseObject {
	kept := objects[:0]
	for _, object := range objects {
		if packed[object.hash] {
			kept = append(kept, object)
		}
	}
	return kept
}

// livePack is a reader-visible pack no merge has yet rewritten. A superseded
// pack is not one: its objects already live in the pack that replaced it, and
// counting it toward the merge threshold, or merging it again, would rewrite the
// same objects on every compaction until its retention window closed.
type livePack struct {
	name string
	size int64
}

// packsToMerge picks the packs a merge rewrites: the smallest ones, up to the
// point from which every remaining pack is at least compactionGeometricFactor
// times everything below it. It returns nil when the sizes are already
// geometric, or when the rule selects a single pack, which would be rewritten
// into itself.
func packsToMerge(packs []livePack) []string {
	ordered := append([]livePack(nil), packs...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].size != ordered[j].size {
			return ordered[i].size < ordered[j].size
		}
		return ordered[i].name < ordered[j].name
	})
	below := make([]int64, len(ordered))
	var sum int64
	for i, pack := range ordered {
		below[i] = sum
		sum += pack.size
	}
	split := len(ordered)
	for split > 0 && ordered[split-1].size >= compactionGeometricFactor*below[split-1] {
		split--
	}
	if split < 2 {
		return nil
	}
	names := make([]string, 0, split)
	for _, pack := range ordered[:split] {
		names = append(names, pack.name)
	}
	sort.Strings(names)
	return names
}

// hashesInPacks reads the object ids from the named packs' indexes, the set a
// merging compaction rewrites.
func hashesInPacks(snapshot *tierSnapshot, packs []string) ([]plumbing.Hash, error) {
	var hashes []plumbing.Hash
	for _, name := range packs {
		index, err := snapshot.pack(name).loadIndex()
		if err != nil {
			return nil, err
		}
		iter, err := index.EntriesByOffset()
		if err != nil {
			return nil, fmt.Errorf("read index of %s: %w", name, err)
		}
		for {
			entry, err := iter.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("read index of %s: %w", name, err)
			}
			hashes = append(hashes, entry.Hash)
		}
	}
	return hashes, nil
}

// builtPack is a finished pack on local disk, waiting to be published.
type builtPack struct {
	name string
	// stagingDir is the staging directory and packFile the staged file's bare
	// name within it (never a path). Kept apart so every use resolves the name
	// under an os.Root on the directory, stopping the name or a planted symlink
	// from escaping it.
	stagingDir string
	packFile   string
	packSize   int64
	objects    int
	// parsed is the index as the parse of the staged bytes produced it, and
	// index the same encoded for upload. The publisher keeps the first so that it
	// never reads back the second.
	parsed     *idxfile.MemoryIndex
	index      []byte
	filter     []byte
	filterBits int
}

// openRoot scopes access to the pack's staging directory.
func (b *builtPack) openRoot() (*os.Root, error) {
	root, err := os.OpenRoot(b.stagingDir)
	if err != nil {
		return nil, fmt.Errorf("open compaction staging directory: %w", err)
	}
	return root, nil
}

// open opens the staged pack for reading; the file outlives the root it was
// opened through.
func (b *builtPack) open() (*os.File, error) {
	root, err := b.openRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.Open(b.packFile)
}

func (b *builtPack) cleanup() {
	if b == nil || b.packFile == "" {
		return
	}
	root, err := b.openRoot()
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	_ = root.Remove(b.packFile)
}

// buildPack encodes the objects into a packfile on local disk and derives the
// index and membership filter from the bytes it wrote. It stages locally rather
// than streaming because the pack's name is the hash of its contents, unknown
// until the last byte.
func (r *repository) buildPack(hashes []plumbing.Hash) (*builtPack, error) {
	return r.buildPackFrom(r, hashes)
}

// buildPackFrom is buildPack reading the objects from source, which lets a
// pack be built from objects that are not in the repository yet.
func (r *repository) buildPackFrom(source storer.EncodedObjectStorer, hashes []plumbing.Hash) (*builtPack, error) {
	temp, built, err := r.stagePack("compact-*.pack")
	if err != nil {
		return nil, err
	}
	if _, err := packfile.NewEncoder(temp, source, false).Encode(hashes, compactionPackWindow); err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, fmt.Errorf("encode pack for %s: %w", r.name, err)
	}
	if err := built.describe(temp); err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, err
	}
	if err := temp.Close(); err != nil {
		built.cleanup()
		return nil, fmt.Errorf("close staged pack: %w", err)
	}
	return built, nil
}

// stagePack creates the local file a pack is assembled in.
func (r *repository) stagePack(pattern string) (*os.File, *builtPack, error) {
	dir, err := r.compactionScratchDir()
	if err != nil {
		return nil, nil, err
	}
	temp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("stage pack: %w", err)
	}
	return temp, &builtPack{stagingDir: dir, packFile: filepath.Base(temp.Name())}, nil
}

// describe derives everything publication needs — name, size, index and
// membership filter — by parsing the staged bytes, never from whoever wrote
// them, so it can only describe the pack that exists byte for byte. It fails on
// a pack that is not self-contained: a thin pack's deltas name bases outside
// it, and a stored pack must be readable on its own.
func (b *builtPack) describe(staged *os.File) error {
	size, err := staged.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("size staged pack: %w", err)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind staged pack: %w", err)
	}
	writer := new(idxfile.Writer)
	parser, err := packfile.NewParser(packfile.NewScanner(staged), writer)
	if err != nil {
		return fmt.Errorf("parse staged pack: %w", err)
	}
	checksum, err := parser.Parse()
	if err != nil {
		return fmt.Errorf("parse staged pack: %w", err)
	}
	index, err := writer.Index()
	if err != nil {
		return fmt.Errorf("index staged pack: %w", err)
	}

	var encoded bytes.Buffer
	if _, err := idxfile.NewEncoder(&encoded).Encode(index); err != nil {
		return fmt.Errorf("encode index: %w", err)
	}

	var keys []oidKey
	iter, err := index.EntriesByOffset()
	if err != nil {
		return fmt.Errorf("read staged index: %w", err)
	}
	for {
		entry, err := iter.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("read staged index: %w", err)
		}
		keys = append(keys, oidKeyFrom(entry.Hash[:]))
	}
	filter, err := newBinaryFuseFilter(keys)
	if err != nil {
		return err
	}

	b.name = "pack-" + checksum.String()
	b.packSize = size
	b.objects = len(keys)
	b.parsed = index
	b.index = encoded.Bytes()
	b.filter = filter.encode()
	b.filterBits = filter.bits()
	return nil
}

// compactionScratchDir stages a pack while it is built. It shares the pack
// cache's directory: both hold pack bytes against the same local disk budget.
func (r *repository) compactionScratchDir() (string, error) {
	dir := filepath.Join(r.shared.opts.CacheDir, "staging")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("compaction staging directory: %w", err)
	}
	return dir, nil
}

// packPublishTimeout bounds the upload of one pack. It is generous: a ceiling on
// a wedged upload, not a pace.
const packPublishTimeout = 30 * time.Minute

// publishPack uploads the index, the filter, and finally the pack — see the
// crash-safety argument at the top of this file — and returns the pack as a
// snapshot holds it, with its index and filter already in hand. The local cache
// is seeded from the staged bytes, so that neither this handle nor the next one
// on this replica downloads what has just been uploaded.
func (r *repository) publishPack(ctx context.Context, built *builtPack) (*storedPack, error) {
	filter, err := decodeBinaryFuseFilter(built.filter)
	if err != nil {
		return nil, fmt.Errorf("filter of %s: %w", built.name, err)
	}
	filterExtents := r.tiers.extents(built.name+".bfilter", int64(len(built.filter)))
	published := &storedPack{
		name:          built.name,
		pack:          r.tiers.extents(built.name+".pack", built.packSize),
		index:         r.tiers.extents(built.name+".idx", int64(len(built.index))),
		filtered:      true,
		filterExtents: filterExtents,
	}
	published.parsed.Store(built.parsed)
	published.filter.Store(filter)

	if err := r.putObject(ctx, published.index.key, built.index); err != nil {
		return nil, err
	}
	if err := r.putObject(ctx, filterExtents.key, built.filter); err != nil {
		return nil, err
	}
	// One request when the pack is small, else in parts, so that a pack of
	// gigabytes is never held in memory. Both are atomic: the object appears
	// only once the request, or the completion of the parts, succeeds.
	staged, err := built.open()
	if err != nil {
		return nil, fmt.Errorf("open staged pack: %w", err)
	}
	_, err = r.shared.put(ctx, packPublishTimeout, published.pack.key, staged, built.packSize, objstore.Always)
	_ = staged.Close()
	if err != nil {
		return nil, err
	}

	r.seedPackCache(published.pack, built)
	r.seedCache(published.index, built.index)
	r.seedCache(filterExtents, built.filter)
	return published, nil
}

func (r *repository) putObject(ctx context.Context, key string, body []byte) error {
	_, err := r.shared.put(ctx, storeWriteTimeout, key, bytes.NewReader(body), int64(len(body)), objstore.Always)
	return err
}

func (r *repository) seedPackCache(extents packExtents, built *builtPack) {
	file, err := built.open()
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	cache := r.shared.packCache()
	chunkSize := r.shared.opts.ChunkBytes
	for chunk := int64(0); ; chunk++ {
		// Each chunk needs its own buffer: an admitted chunk is shared with later
		// readers, so reusing the buffer would rewrite bytes they are reading.
		buffer := make([]byte, min(chunkSize, max(extents.size-chunk*chunkSize, 0)))
		read, err := io.ReadFull(file, buffer)
		if read > 0 {
			cache.store(r.shared.bucket.Name(), extents.key, chunkSize, chunk, buffer[:read])
		}
		if err != nil || read == 0 {
			return
		}
	}
}

// seedCache does for a published pack's index and filter what seedPackCache does
// for the pack.
func (r *repository) seedCache(extents packExtents, body []byte) {
	cache := r.shared.packCache()
	chunkSize := r.shared.opts.ChunkBytes
	for chunk, start := int64(0), int64(0); start < int64(len(body)); chunk, start = chunk+1, start+chunkSize {
		end := min(start+chunkSize, int64(len(body)))
		// A copy, for the reason seedPackCache gives: an admitted chunk is shared.
		cache.store(r.shared.bucket.Name(), extents.key, chunkSize, chunk, bytes.Clone(body[start:end]))
	}
}

// deleteLooseObjects removes the keys that went into the published pack.
func (r *repository) deleteLooseObjects(ctx context.Context, objects []looseObject) error {
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.key)
	}
	if err := r.shared.deleteMany(ctx, keys); err != nil {
		return fmt.Errorf("delete %d packed loose objects: %w", len(keys), err)
	}
	return nil
}

// markSuperseded records that packs were rewritten into a newer one. It writes
// a marker rather than deleting, since a request begun before the merge may
// still read the old pack; retireSupersededPacks removes the bytes once the
// marker ages.
func (r *repository) markSuperseded(ctx context.Context, packs []string, replacement string) error {
	for _, pack := range packs {
		if err := r.putObject(ctx, r.tiers.extents(pack+".superseded", 0).key, []byte(replacement)); err != nil {
			return err
		}
	}
	return nil
}

// retireSupersededPacks removes packs whose supersession marker is older than
// the retention window, aging against the object store's modification times
// rather than this replica's clock. No snapshot changes for it: a superseded
// pack was never in one taken since its marker was written.
func (r *repository) retireSupersededPacks(ctx context.Context, listing *tierListing) ([]string, error) {
	var newest time.Time
	for _, entry := range listing.packDirectory {
		if entry.modified.After(newest) {
			newest = entry.modified
		}
	}

	var retired []string
	var doomed []string
	for name, entry := range listing.packDirectory {
		pack, ok := strings.CutSuffix(name, ".superseded")
		if !ok {
			continue
		}
		if newest.Sub(entry.modified) < supersededPackRetention {
			continue
		}
		for _, extension := range []string{".pack", ".idx", ".bfilter", ".superseded"} {
			if _, present := listing.packDirectory[pack+extension]; present {
				doomed = append(doomed, r.tiers.extents(pack+extension, 0).key)
			}
		}
		retired = append(retired, pack)
	}
	if len(doomed) == 0 {
		return nil, nil
	}
	// Delete the .pack key first: once gone, no reader looks for the index or filter.
	sort.SliceStable(doomed, func(i, j int) bool {
		return strings.HasSuffix(doomed[i], ".pack") && !strings.HasSuffix(doomed[j], ".pack")
	})
	for _, key := range doomed {
		if err := r.shared.deleteObject(ctx, key); err != nil {
			return retired, fmt.Errorf("retire %s: %w", key, err)
		}
	}
	sort.Strings(retired)
	return retired, nil
}
