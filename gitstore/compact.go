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
	"golang.org/x/sync/errgroup"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Compaction merges small packs, and removes what the repository no longer
// needs.
//
// ORDERING / CRASH SAFETY. Nothing a compaction uploads is part of the
// repository until the manifest names it, and the manifest changes by one
// conditional write (commit.go):
//
//	1. list objects/, then revalidate the manifest
//	2. delete the packs the manifest retired more than a grace period ago, and
//	   the reference snapshots no manifest names; list the uploads no manifest
//	   names as retired orphans; drop what was deleted from the manifest
//	3. build the new pack and its sidecar on local disk
//	4. upload pack-<sha>.sidecar and pack-<sha>.pack
//	5. commit: the manifest gains the new pack and retires the packs it merged
//
// Crash before step 5: the old packs stay authoritative, and the upload is an
// orphan for a later compaction to sweep. A retired pack's bytes outlive the
// commit that retired it by the grace period, so a reader that began before the
// commit still finds them.
//
// CONCURRENCY. No lock is held, in process or out. Two replicas may compact one
// repository at once. Both mutations retire the same packs, and the one that
// commits second finds them no longer live: it refuses, and that replica
// deletes the pack it uploaded — unless the two packs are the same pack, named
// alike because they hold the same objects, in which case it is live and stays.
//
// WHAT IS NEVER DELETED. A pack the manifest names as live. A pack it names as
// retired, until a grace period after the retirement. An upload it does not
// name, until it has lain there a grace period — it may be a push that is about
// to commit — and then only by way of the retired list: pack names are digests
// of their contents, so the same pack may be uploaded again, and a delete that
// raced such an upload's commit would remove a live pack. Listing the orphan as
// retired first makes the manifest say what is going to be deleted a grace
// period before it is; a commit that wants the name back in that time takes it
// back (draft.addPack), and after that is refused.

const (
	// compactionPackWindow is the delta window the pack encoder searches.
	compactionPackWindow = 10
	// defaultCompactAfterPacks is the live pack count above which a write asks
	// for a compaction (Options.CompactAfterPacks). Every lookup that misses
	// asks every pack, so a pack per write would give back what packing bought.
	defaultCompactAfterPacks = 8
	// compactionGeometricFactor shapes which packs a merge rewrites: a pack is
	// left alone while it is at least this many times the size of everything
	// smaller than it put together. A push lands as a pack of its own, so
	// merging every pack each time the count crossed the threshold would rewrite
	// the whole repository every few pushes; keeping the sizes geometric bounds
	// the pack count by the logarithm of the repository's size while each byte
	// is rewritten only a logarithmic number of times. It is the rule behind
	// `git repack --geometric`.
	compactionGeometricFactor = 2
	// retiredPackGrace is how long a merged-away pack is kept before its bytes are
	// removed, for requests that began before the merge and still read it: longer
	// than any request runs. It is also how long an upload no manifest names must
	// have lain in the store before it is taken for an orphan.
	retiredPackGrace = time.Hour
	// defaultMultipartThreshold is the size above which a pack is uploaded in
	// parts. Configurable because non-Amazon endpoints cap the single-request
	// upload well below Amazon's 5 GiB.
	defaultMultipartThreshold = 64 << 20
)

// CompactionResult reports what one compaction did.
type CompactionResult struct {
	// Merged is the number of previously packed objects rewritten into the new
	// pack.
	Merged int
	// PackName is the published pack-<sha> base name; empty when nothing to do.
	PackName string
	// PackBytes is the size of the published packfile.
	PackBytes int64
	// RetiredPacks lists packs whose bytes were removed, their grace period over.
	RetiredPacks []string
	// Orphans lists uploads no manifest named that were found old enough to be
	// listed as retired, which starts their grace period.
	Orphans []string
	// SweptSnapshots counts the reference snapshots removed that no manifest named.
	SweptSnapshots int
	// FilterBytes is the size of the membership filter published beside the pack.
	FilterBytes int
}

// Compactor is a git storage handle that merges its own packs. Local-filesystem
// storage does not implement it: git's own maintenance owns that layout.
type Compactor interface {
	Compact(ctx context.Context) (CompactionResult, error)
}

// CompactRepository compacts a repository when its storage supports it,
// reporting an empty result otherwise.
func CompactRepository(ctx context.Context, stor gitStorage.Storer) (CompactionResult, error) {
	compactor, ok := stor.(Compactor)
	if !ok {
		return CompactionResult{}, nil
	}
	return compactor.Compact(ctx)
}

// notePackWritten decides, as a pack lands, whether the repository is due a
// compaction, and asks for one only then. A compaction opens with a listing of
// the whole object tree, so one run after every write to find nothing to do was
// a request per write; what it would find is already known here. The count is
// the manifest's — the repository's live packs, not a tally of this process's
// writes — so packs an earlier process left are counted after a restart.
func (r *repository) notePackWritten(livePacks int) {
	threshold := r.shared.opts.CompactAfterPacks
	if threshold <= 0 || livePacks <= threshold {
		return
	}
	r.requestCompaction()
}

func (r *repository) requestCompaction() {
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

// compactionRequestFunc is invoked when a repository's packs are due a merge.
// The package owns no goroutine lifecycle, so the compaction is delegated to an
// installed handler: the server's scheduler runs one supervised,
// cancel-at-shutdown compaction per repository. Left unset (tests, embeddings),
// packs just accumulate — slower, not broken.
var (
	compactionRequestMu   sync.RWMutex
	compactionRequestFunc func(repo string, stor gitStorage.Storer)
)

// SetCompactionRequestHandler installs the compaction handler; nil removes it.
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
	r.compacting.Lock()
	defer r.compacting.Unlock()
	var result CompactionResult

	// One listing of objects/ answers everything a compaction asks of the store
	// before it decides: what lies in the pack directory and among the reference
	// snapshots. The manifest is revalidated AFTER it: what the manifest read
	// then does not name, and the listing shows as old, was not named by any
	// manifest while the listing was taken.
	listing, err := r.listObjects()
	if err != nil {
		return result, err
	}
	state, err := r.manifests.revalidate()
	if err != nil {
		return result, err
	}
	if state, err = r.sweep(ctx, state, listing, &result); err != nil {
		return result, err
	}

	// What is merged is what the geometric rule picks; how many packs make a
	// compaction due is the business of whoever asked for this one.
	live := make([]livePack, 0, len(state.packs))
	for _, pack := range state.packs {
		live = append(live, livePack{name: pack.name, size: pack.pack.size})
	}
	existing := packsToMerge(live)
	if len(existing) == 0 {
		return result, nil
	}
	hashes, err := hashesInPacks(state, existing)
	if err != nil {
		return result, err
	}
	seen := make(map[plumbing.Hash]bool, len(hashes))
	unique := hashes[:0]
	for _, hash := range hashes {
		if !seen[hash] {
			seen[hash] = true
			unique = append(unique, hash)
		}
	}
	if len(unique) == 0 {
		return result, nil
	}
	result.Merged = len(unique)

	built, err := r.buildPack(unique)
	if err != nil {
		return result, err
	}
	defer built.cleanup()

	uploaded, err := r.uploadPack(ctx, built)
	if err != nil {
		return result, err
	}
	retiring := slices.DeleteFunc(existing, func(name string) bool { return name == built.name })
	if _, err := r.commits.commit(func(d *draft) error {
		if err := d.retire(retiring, false); err != nil {
			return err
		}
		return d.addPack(uploaded.entry(packSourceCompaction))
	}); err != nil {
		r.manifests.withdraw(built.name)
		if errors.Is(err, errCompactionLostRace) {
			return result, r.discardUpload(ctx, uploaded)
		}
		return result, err
	}
	result.PackName = built.name
	result.PackBytes = built.packSize
	result.FilterBytes = built.filterBits / 8
	return result, nil
}

// discardUpload removes the pack of a compaction that lost its race, unless the
// manifest names a pack of that name: the winner merged the same packs into the
// same objects, so the same pack, and its keys are the winner's.
func (r *repository) discardUpload(ctx context.Context, uploaded *uploadedPack) error {
	state, err := r.manifests.revalidate()
	if err != nil {
		return err
	}
	name := uploaded.stored.name
	if state.pack(name) != nil || slices.ContainsFunc(state.manifest.Retired, func(pack retiredPack) bool { return pack.Name == name }) {
		return nil
	}
	return r.deletePackKeys(ctx, name)
}

// packKeySuffixes are the objects a pack is made of. The pack goes first when
// they are deleted: once it has gone, nothing looks for its sidecar.
var packKeySuffixes = []string{".pack", sidecarSuffix}

func (r *repository) deletePackKeys(ctx context.Context, name string) error {
	for _, suffix := range packKeySuffixes {
		if err := r.shared.deleteObject(ctx, r.manifests.extents(name+suffix, 0).key); err != nil {
			return fmt.Errorf("delete %s%s: %w", name, suffix, err)
		}
	}
	return nil
}

// sweep removes what the repository no longer needs, and returns the state
// after whatever it committed. state was read after listing was taken.
//
// A retired pack goes once it has been retired a grace period: its keys are
// deleted first and its entry dropped from the manifest after, so that a crash
// between the two leaves an entry whose deletion the next sweep repeats. An
// upload the manifest does not name, every key of which has lain in the store a
// grace period, is entered as a retired orphan and deleted by the sweep that
// finds that entry a grace period old. A reference snapshot the manifest does
// not name, as old, is deleted at once: its key was used by one commit and will
// be by no other.
func (r *repository) sweep(ctx context.Context, state *repoState, listing *objectsListing, result *CompactionResult) (*repoState, error) {
	now := r.shared.now()
	named := map[string]bool{}
	for _, pack := range state.manifest.Packs {
		named[pack.Name] = true
	}
	var expired []retiredPack
	for _, pack := range state.manifest.Retired {
		named[pack.Name] = true
		if now.Sub(pack.Retired) > retiredPackGrace {
			expired = append(expired, pack)
		}
	}

	// An upload is as young as the youngest of its keys.
	youngest := map[string]time.Time{}
	for key, entry := range listing.packDirectory {
		for _, suffix := range packKeySuffixes {
			name, isPackKey := strings.CutSuffix(key, suffix)
			if !isPackKey || !validPackName(name) || named[name] {
				continue
			}
			if entry.modified.After(youngest[name]) {
				youngest[name] = entry.modified
			}
		}
	}
	var orphans []string
	for name, modified := range youngest {
		if now.Sub(modified) > retiredPackGrace {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)

	var snapshots []string
	for key, entry := range listing.refSnapshots {
		if key != state.manifest.Refs.Snapshot && now.Sub(entry.modified) > retiredPackGrace {
			snapshots = append(snapshots, r.prefix+key)
		}
	}
	if len(snapshots) > 0 {
		if err := r.shared.deleteMany(ctx, snapshots); err != nil {
			return nil, fmt.Errorf("sweep reference snapshots: %w", err)
		}
		result.SweptSnapshots = len(snapshots)
	}

	for _, pack := range expired {
		if err := r.deletePackKeys(ctx, pack.Name); err != nil {
			return nil, err
		}
		result.RetiredPacks = append(result.RetiredPacks, pack.Name)
	}
	sort.Strings(result.RetiredPacks)
	if len(expired) == 0 && len(orphans) == 0 {
		return state, nil
	}
	committed, err := r.commits.commit(func(d *draft) error {
		for _, pack := range expired {
			// Dropped only as the entry whose keys were deleted: one a commit
			// brought back and another sweep retired again is a newer entry.
			if at := d.retiredIndex(pack.Name); at >= 0 && d.manifest.Retired[at].Retired.Equal(pack.Retired) {
				d.manifest.Retired = append(d.manifest.Retired[:at], d.manifest.Retired[at+1:]...)
			}
		}
		for _, name := range orphans {
			if !d.livePack(name) && d.retiredIndex(name) < 0 {
				d.manifest.Retired = append(d.manifest.Retired, retiredPack{Name: name, Retired: d.now.UTC(), Orphan: true})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result.Orphans = orphans
	return committed, nil
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
func hashesInPacks(state *repoState, packs []string) ([]plumbing.Hash, error) {
	var hashes []plumbing.Hash
	for _, name := range packs {
		index, err := state.pack(name).loadIndex()
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
	// sidecar the index and the filter encoded for upload (sidecar.go), the index
	// indexBytes long and the filter filterBytes. The publisher keeps parsed and
	// filter so that it never reads back what it uploads.
	parsed      *idxfile.MemoryIndex
	filter      *binaryFuseFilter
	sidecar     []byte
	indexBytes  int64
	filterBytes int64
	filterBits  int
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
	if err := built.describe(temp, layoutOf(temp), nil); err != nil {
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
// membership filter — by indexing the staged bytes, never from whoever wrote
// them, so it can only describe the pack that exists byte for byte. layout is
// the first pass over them. A delta's base the pack lacks is read from bases and
// appended, and with bases nil the pack must be readable on its own: a stored
// pack always is.
func (b *builtPack) describe(staged *os.File, layout packLayout, bases storer.EncodedObjectStorer) error {
	index, checksum, err := indexPack(staged, layout, bases)
	if err != nil {
		return fmt.Errorf("index staged pack: %w", err)
	}
	size, err := staged.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("size staged pack: %w", err)
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

	filterBytes := filter.encode()
	b.name = "pack-" + checksum.String()
	b.packSize = size
	b.objects = len(keys)
	b.parsed = index
	b.filter = filter
	b.sidecar = encodeSidecar(encoded.Bytes(), filterBytes, checksum)
	b.indexBytes = int64(encoded.Len())
	b.filterBytes = int64(len(filterBytes))
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

// uploadedPack is a pack that is in the store and in no manifest yet.
type uploadedPack struct {
	stored *storedPack
}

// entry is the pack as a manifest records it.
func (u *uploadedPack) entry(source string) manifestPack {
	return manifestPack{
		Name:         u.stored.name,
		Bytes:        u.stored.pack.size,
		SidecarBytes: u.stored.sidecar.size,
		IndexBytes:   u.stored.indexBytes,
		FilterBytes:  u.stored.filterBytes,
		Objects:      u.stored.objects,
		Source:       source,
	}
}

// uploadPack puts a pack and its sidecar in the store, and returns the pack as a
// state will hold it once a manifest names it, with its index and filter already
// in hand. The two go in either order and make nothing visible: the commit that
// follows does that. The local cache is seeded from the staged bytes, so that
// neither this handle nor the next one on this replica downloads what has just
// been uploaded.
func (r *repository) uploadPack(ctx context.Context, built *builtPack) (*uploadedPack, error) {
	stored := &storedPack{
		name:        built.name,
		objects:     built.objects,
		pack:        r.manifests.extents(built.name+".pack", built.packSize),
		sidecar:     r.manifests.extents(built.name+sidecarSuffix, int64(len(built.sidecar))),
		indexBytes:  built.indexBytes,
		filterBytes: built.filterBytes,
	}
	stored.parsed.Store(built.parsed)
	stored.filter.Store(built.filter)

	// The two uploads wait on nothing of each other's — it is the commit that
	// follows that makes them a pack — so they go together, and a write pays the
	// latency of the longer and not of their sum. The pack is one request when
	// it is small, else in parts, so that a pack of gigabytes is never held in
	// memory.
	var uploads errgroup.Group
	uploads.Go(func() error { return r.putObject(ctx, stored.sidecar.key, built.sidecar) })
	uploads.Go(func() error {
		staged, err := built.open()
		if err != nil {
			return fmt.Errorf("open staged pack: %w", err)
		}
		defer func() { _ = staged.Close() }()
		_, err = r.shared.put(ctx, packPublishTimeout, stored.pack.key, staged, built.packSize, objstore.Always)
		return err
	})
	if err := uploads.Wait(); err != nil {
		return nil, err
	}

	r.seedPackCache(stored.pack, built)
	r.seedCache(stored.sidecar, built.sidecar)
	r.manifests.offer(stored)
	return &uploadedPack{stored: stored}, nil
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

// seedCache does for a published pack's sidecar what seedPackCache does for the
// pack.
func (r *repository) seedCache(extents packExtents, body []byte) {
	cache := r.shared.packCache()
	chunkSize := r.shared.opts.ChunkBytes
	for chunk, start := int64(0), int64(0); start < int64(len(body)); chunk, start = chunk+1, start+chunkSize {
		end := min(start+chunkSize, int64(len(body)))
		// A copy, for the reason seedPackCache gives: an admitted chunk is shared.
		cache.store(r.shared.bucket.Name(), extents.key, chunkSize, chunk, bytes.Clone(body[start:end]))
	}
}
