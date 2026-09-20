package gitstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	minio "github.com/minio/minio-go/v7"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Compaction turns the loose tier into the pack tier.
//
// ORDERING / CRASH SAFETY. A pack is invisible until its .pack key exists
// (go-git lists objects/pack/ for *.pack), and S3 PutObject / multipart
// completion publishes an object all-or-nothing. So .pack is written last:
//
//	1. build the pack and its index on local disk
//	2. upload pack-<sha>.idx
//	3. upload pack-<sha>.bfilter
//	4. upload pack-<sha>.pack        <- the commit point
//	5. make the new pack visible to this process (Reindex, index invalidate)
//	6. delete the loose keys that step 1 actually packed
//
// Crash before step 4: loose objects stay authoritative; the orphan .idx/.bfilter
// name a pack nothing lists and the next compaction sweeps them. Crash between 4
// and 6: pack and some loose objects both exist; go-git reads either and the
// next compaction removes the duplicates. No object is ever in neither place,
// because a loose delete strictly follows publication of the pack holding it.
//
// CONCURRENCY. Step 6 deletes only the keys this compaction listed and packed,
// so a push after the listing survives as loose. Two replicas may list the same
// keys; each writes a content-named pack (no collision) and deletes only what it
// packed, so every deleted key is in some published pack. If one replica reads a
// loose object the other already deleted mid-build, the read fails not-exist and
// buildPackTolerantly drops it and rebuilds — safe because the only way that key
// is gone is that the other replica already packed it. The durable lock avoids
// the duplicated work but correctness does not depend on it (its lease can
// expire under a long compaction). Against a concurrent read on this replica,
// step 5 precedes step 6 under the storage lock, so a reader finds the object
// loose before the swap and packed after.

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
	multipartPartSize         = 32 << 20
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

// noteObjectWritten counts a loose write and starts a compaction once enough
// accumulate. Only one compaction per repository runs at a time in this process;
// one already running absorbs concurrent writes, since it deletes only the keys
// it listed.
func (s *atomicRefStorer) noteObjectWritten() {
	if s.fs == nil {
		return
	}
	trigger := s.fs.options().CompactionTrigger
	if trigger <= 0 || s.looseWrites.Add(1) < trigger {
		return
	}
	s.requestCompaction()
}

// notePackWritten counts a pushed pack toward the next compaction. Its objects
// count as writes do, and the pack itself counts toward the number a merge is
// due at: a run of small pushes never reaches the object trigger, but each
// leaves a pack whose index every packed lookup must load.
func (s *atomicRefStorer) notePackWritten(objects int64) {
	trigger := s.fs.options().CompactionTrigger
	if trigger <= 0 {
		return
	}
	packs := s.packWrites.Add(1)
	if s.looseWrites.Add(objects) < trigger && packs <= compactionMergeThreshold {
		return
	}
	s.requestCompaction()
}

func (s *atomicRefStorer) requestCompaction() {
	s.looseWrites.Store(0)
	s.packWrites.Store(0)
	if request := compactionRequestHook(); request != nil {
		request(s.repo, s)
	}
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
func (s *atomicRefStorer) Compact(ctx context.Context) (CompactionResult, error) {
	if s.fs == nil {
		return CompactionResult{}, nil
	}
	digest := sha256.Sum256([]byte(s.repo + "\x00compaction"))
	name := "git-compact:" + hex.EncodeToString(digest[:])

	var result CompactionResult
	err := s.withLockName(name, func() error {
		var err error
		result, err = s.compactLocked(ctx)
		return err
	})
	return result, err
}

func (s *atomicRefStorer) compactLocked(ctx context.Context) (CompactionResult, error) {
	var result CompactionResult

	retired, err := s.retireSupersededPacks(ctx)
	if err != nil {
		return result, err
	}
	result.RetiredPacks = retired

	loose, err := s.listLooseObjects(ctx)
	if err != nil {
		return result, err
	}

	live, err := s.listLivePacks(ctx)
	if err != nil {
		return result, err
	}
	var existing []string
	if len(live) > compactionMergeThreshold {
		existing = packsToMerge(live)
	}
	merge := len(existing) > 0

	if len(loose) < compactionMinLooseObjects && !merge {
		return result, nil
	}

	candidates := append([]looseObject(nil), loose...)
	var mergedHashes []plumbing.Hash
	if merge {
		mergedHashes, err = s.hashesInPacks(existing)
		if err != nil {
			return result, err
		}
	}

	hashes := make([]plumbing.Hash, 0, len(candidates)+len(mergedHashes))
	seen := make(map[plumbing.Hash]bool, len(candidates)+len(mergedHashes))
	packedLoose := make([]looseObject, 0, len(candidates))
	for _, obj := range candidates {
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

	built, survivors, err := s.buildPackTolerantly(hashes)
	if err != nil {
		return result, err
	}
	defer built.cleanup()
	packedLoose = retainPacked(packedLoose, survivors)
	result.Packed = len(packedLoose)

	if err := s.publishPack(ctx, built); err != nil {
		return result, err
	}
	result.PackName = built.name
	result.PackBytes = built.packSize
	result.FilterBytes = built.filterBits / 8

	// Make the pack visible before deleting any loose key (step 5 before 6), so a
	// concurrent reader never looks only where the object no longer is.
	s.adoptPack()

	if err := s.deleteLooseObjects(ctx, packedLoose); err != nil {
		return result, err
	}
	if merge {
		if err := s.markSuperseded(ctx, existing, built.name); err != nil {
			return result, err
		}
	}
	return result, nil
}

// buildPackTolerantly encodes the pack; if an object vanished between listing
// and read (only possible when another replica already packed it), it drops the
// object and retries. Retrying beats probing every object up front, which would
// cost the per-object round trip compaction exists to remove.
func (s *atomicRefStorer) buildPackTolerantly(hashes []plumbing.Hash) (*builtPack, map[plumbing.Hash]bool, error) {
	built, err := s.buildPack(hashes)
	if err == nil {
		return built, hashSet(hashes), nil
	}
	if !errors.Is(err, plumbing.ErrObjectNotFound) && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	survivors := make([]plumbing.Hash, 0, len(hashes))
	for _, hash := range hashes {
		if s.HasEncodedObject(hash) == nil {
			survivors = append(survivors, hash)
		}
	}
	if len(survivors) == 0 {
		return nil, nil, err
	}
	built, err = s.buildPack(survivors)
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

// looseObject pairs a loose object's hash with the key holding it.
type looseObject struct {
	hash plumbing.Hash
	key  string
}

// listLooseObjects enumerates every loose object of the repository.
func (s *atomicRefStorer) listLooseObjects(ctx context.Context) ([]looseObject, error) {
	prefix := s.fs.key("objects") + "/"
	packPrefix := prefix + "pack/"

	var objects []looseObject
	for entry := range s.fs.client.Client.ListObjects(ctx, s.fs.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if entry.Err != nil {
			return nil, fmt.Errorf("list loose objects of %s: %w", s.repo, entry.Err)
		}
		key := entry.Key
		if strings.HasPrefix(key, packPrefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		fanout, base, found := strings.Cut(rest, "/")
		if !found || len(fanout) != 2 || strings.Contains(base, "/") {
			continue
		}
		hash := plumbing.NewHash(fanout + base)
		if hash.IsZero() {
			continue
		}
		objects = append(objects, looseObject{hash: hash, key: key})
	}
	return objects, nil
}

// livePack is a reader-visible pack no merge has yet rewritten.
type livePack struct {
	name string
	size int64
}

// listLivePacks names every pack whose .pack key exists and that carries no
// supersession marker. A superseded pack stays readable for its retention
// window, but its objects already live in the pack that replaced it: counting
// it toward the merge threshold, or merging it again, would rewrite the same
// objects on every compaction until the window closed.
func (s *atomicRefStorer) listLivePacks(ctx context.Context) ([]livePack, error) {
	entries, err := s.listPackDirectory(ctx)
	if err != nil {
		return nil, err
	}
	prefix := s.fs.key(path.Join("objects", "pack")) + "/"
	var packs []livePack
	for key, entry := range entries {
		base := path.Base(key)
		if !strings.HasPrefix(base, "pack-") || !strings.HasSuffix(base, ".pack") {
			continue
		}
		name := strings.TrimSuffix(base, ".pack")
		if _, superseded := entries[prefix+name+".superseded"]; superseded {
			continue
		}
		packs = append(packs, livePack{name: name, size: entry.size})
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].name < packs[j].name })
	return packs, nil
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

type packDirectoryEntry struct {
	modified time.Time
	size     int64
}

func (s *atomicRefStorer) listPackDirectory(ctx context.Context) (map[string]packDirectoryEntry, error) {
	prefix := s.fs.key(path.Join("objects", "pack")) + "/"
	entries := map[string]packDirectoryEntry{}
	for entry := range s.fs.client.Client.ListObjects(ctx, s.fs.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if entry.Err != nil {
			return nil, fmt.Errorf("list packs of %s: %w", s.repo, entry.Err)
		}
		entries[entry.Key] = packDirectoryEntry{modified: entry.LastModified, size: entry.Size}
	}
	return entries, nil
}

// hashesInPacks reads the object ids from the existing packs' indexes, the set
// a merging compaction rewrites.
func (s *atomicRefStorer) hashesInPacks(packs []string) ([]plumbing.Hash, error) {
	var hashes []plumbing.Hash
	for _, pack := range packs {
		index, err := s.readPackIndex(pack)
		if err != nil {
			return nil, err
		}
		iter, err := index.EntriesByOffset()
		if err != nil {
			return nil, fmt.Errorf("read index of %s: %w", pack, err)
		}
		for {
			entry, err := iter.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("read index of %s: %w", pack, err)
			}
			hashes = append(hashes, entry.Hash)
		}
	}
	return hashes, nil
}

func (s *atomicRefStorer) readPackIndex(pack string) (*idxfile.MemoryIndex, error) {
	file, err := s.fs.Open(path.Join("objects", "pack", pack+".idx"))
	if err != nil {
		return nil, fmt.Errorf("open index of %s: %w", pack, err)
	}
	defer func() { _ = file.Close() }()
	index := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(file).Decode(index); err != nil {
		return nil, fmt.Errorf("decode index of %s: %w", pack, err)
	}
	return index, nil
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
func (s *atomicRefStorer) buildPack(hashes []plumbing.Hash) (*builtPack, error) {
	return s.buildPackFrom(s, hashes)
}

// buildPackFrom is buildPack reading the objects from source, which lets a
// pack be built from objects that are not in the repository yet.
func (s *atomicRefStorer) buildPackFrom(source storer.EncodedObjectStorer, hashes []plumbing.Hash) (*builtPack, error) {
	temp, built, err := s.stagePack("compact-*.pack")
	if err != nil {
		return nil, err
	}
	if _, err := packfile.NewEncoder(temp, source, false).Encode(hashes, compactionPackWindow); err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, fmt.Errorf("encode pack for %s: %w", s.repo, err)
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
func (s *atomicRefStorer) stagePack(pattern string) (*os.File, *builtPack, error) {
	dir, err := s.compactionScratchDir()
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
	b.index = encoded.Bytes()
	b.filter = filter.encode()
	b.filterBits = filter.bits()
	return nil
}

// compactionScratchDir stages a pack while it is built. It shares the pack
// cache's directory: both hold pack bytes against the same local disk budget.
func (s *atomicRefStorer) compactionScratchDir() (string, error) {
	dir := filepath.Join(s.fs.options().CacheDir, "staging")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("compaction staging directory: %w", err)
	}
	return dir, nil
}

// publishPack uploads the index, the filter, and finally the pack. See the
// crash-safety argument at the top of this file.
func (s *atomicRefStorer) publishPack(ctx context.Context, built *builtPack) error {
	base := path.Join("objects", "pack", built.name)
	if err := s.putObject(ctx, base+".idx", built.index); err != nil {
		return err
	}
	if err := s.putObject(ctx, base+".bfilter", built.filter); err != nil {
		return err
	}
	if err := s.uploadPackFile(ctx, base+".pack", built); err != nil {
		return err
	}
	// Seed the disk cache from the staged file so this replica does not download
	// back the pack it just uploaded.
	s.seedPackCache(base+".pack", built)
	s.fs.rememberObjectSize(s.fs.key(base+".pack"), built.packSize)
	return nil
}

func (s *atomicRefStorer) putObject(ctx context.Context, name string, body []byte) error {
	key := s.fs.key(name)
	_, err := s.fs.client.Client.PutObject(ctx, s.fs.bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// uploadPackFile publishes the packfile: one request when small, else a
// multipart upload so a multi-gigabyte pack need not be held in memory. Both
// are atomic — the object appears only once the (completion) request succeeds.
func (s *atomicRefStorer) uploadPackFile(ctx context.Context, name string, built *builtPack) error {
	key := s.fs.key(name)
	size := built.packSize
	file, err := built.open()
	if err != nil {
		return fmt.Errorf("open staged pack: %w", err)
	}
	defer func() { _ = file.Close() }()

	if size <= s.fs.options().MultipartBytes {
		if _, err := s.fs.client.Client.PutObject(ctx, s.fs.bucket, key, file, size, minio.PutObjectOptions{}); err != nil {
			return fmt.Errorf("s3 put %s: %w", key, err)
		}
		return nil
	}

	uploadID, err := s.fs.client.NewMultipartUpload(ctx, s.fs.bucket, key, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3 multipart create %s: %w", key, err)
	}

	var parts []minio.CompletePart
	buffer := make([]byte, min(int64(multipartPartSize), max(size/2+1, 1)))
	for number := 1; ; number++ {
		read, err := io.ReadFull(file, buffer)
		if read == 0 {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			if err != nil {
				s.abortMultipart(ctx, key, uploadID)
				return fmt.Errorf("read staged pack: %w", err)
			}
			break
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			s.abortMultipart(ctx, key, uploadID)
			return fmt.Errorf("read staged pack: %w", err)
		}
		uploaded, uploadErr := s.fs.client.PutObjectPart(ctx, s.fs.bucket, key, uploadID, number, bytes.NewReader(buffer[:read]), int64(read), minio.PutObjectPartOptions{})
		if uploadErr != nil {
			s.abortMultipart(ctx, key, uploadID)
			return fmt.Errorf("s3 upload part %d of %s: %w", number, key, uploadErr)
		}
		parts = append(parts, minio.CompletePart{ETag: uploaded.ETag, PartNumber: number})
		if read < len(buffer) {
			break
		}
	}

	if _, err := s.fs.client.CompleteMultipartUpload(ctx, s.fs.bucket, key, uploadID, parts, minio.PutObjectOptions{}); err != nil {
		s.abortMultipart(ctx, key, uploadID)
		return fmt.Errorf("s3 multipart complete %s: %w", key, err)
	}
	return nil
}

func (s *atomicRefStorer) abortMultipart(ctx context.Context, key, uploadID string) {
	_ = s.fs.client.AbortMultipartUpload(ctx, s.fs.bucket, key, uploadID)
}

func (s *atomicRefStorer) seedPackCache(name string, built *builtPack) {
	cache := s.fs.packCache()
	if cache == nil {
		return
	}
	size := built.packSize
	file, err := built.open()
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	key := s.fs.key(name)
	chunkSize := s.fs.shared().chunkSize
	cache.storeSize(s.fs.bucket, key, chunkSize, size)
	for chunk := int64(0); ; chunk++ {
		// Each chunk needs its own buffer: an admitted chunk is shared with later
		// readers, so reusing the buffer would rewrite bytes they are reading.
		buffer := make([]byte, chunkSize)
		read, err := io.ReadFull(file, buffer)
		if read > 0 {
			cache.store(s.fs.bucket, key, chunkSize, chunk, buffer[:read])
		}
		if err != nil || read < len(buffer) {
			return
		}
	}
}

// adoptPack makes the just-published pack visible to the storer and drops the
// membership snapshots so the next negative answer reflects the new repository.
func (s *atomicRefStorer) adoptPack() {
	s.mu.Lock()
	if reindexer, ok := s.storer.(interface{ Reindex() }); ok {
		reindexer.Reindex()
		// Reindex only clears go-git's lazy pack index; requireIndex rebuilds it
		// on the next read, mutating the shared index/pack list. Read methods run
		// under RLock, so leaving the rebuild to them lets concurrent readers race
		// and observe a partial pack set — a spurious ErrObjectNotFound for an
		// object the new pack holds. Force the rebuild here under the exclusive
		// lock; the probe only drives requireIndex through go-git's public surface,
		// its result is irrelevant.
		_ = s.storer.HasEncodedObject(plumbing.ZeroHash)
	}
	s.mu.Unlock()
	s.fs.repoIndexFor().invalidate()
}

// deleteLooseObjects removes the keys that went into the published pack, a
// thousand at a time.
func (s *atomicRefStorer) deleteLooseObjects(ctx context.Context, objects []looseObject) error {
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.key)
	}
	if err := s.fs.deleteObjectKeys(ctx, keys); err != nil {
		return fmt.Errorf("delete %d packed loose objects: %w", len(keys), err)
	}
	for _, object := range objects {
		s.fs.noteLooseRemoved(strings.TrimPrefix(object.key, s.fs.prefix+"/"))
	}
	return nil
}

// markSuperseded records that a pack was rewritten into a newer one. It writes
// a marker rather than deleting, since a request begun before the merge may
// still read the old pack; retireSupersededPacks removes the bytes once the
// marker ages.
func (s *atomicRefStorer) markSuperseded(ctx context.Context, packs []string, replacement string) error {
	for _, pack := range packs {
		if pack == replacement {
			continue
		}
		name := path.Join("objects", "pack", pack+".superseded")
		if err := s.putObject(ctx, name, []byte(replacement)); err != nil {
			return err
		}
	}
	return nil
}

// retireSupersededPacks removes packs whose supersession marker is older than
// the retention window, aging against the object store's LastModified rather
// than this replica's clock.
func (s *atomicRefStorer) retireSupersededPacks(ctx context.Context) ([]string, error) {
	entries, err := s.listPackDirectory(ctx)
	if err != nil {
		return nil, err
	}
	prefix := s.fs.key(path.Join("objects", "pack")) + "/"

	var newest time.Time
	for _, entry := range entries {
		if entry.modified.After(newest) {
			newest = entry.modified
		}
	}

	var retired []string
	var doomed []string
	for key, entry := range entries {
		base := path.Base(key)
		pack, ok := strings.CutSuffix(base, ".superseded")
		if !ok {
			continue
		}
		if newest.Sub(entry.modified) < supersededPackRetention {
			continue
		}
		for _, extension := range []string{".pack", ".idx", ".bfilter", ".superseded"} {
			if _, present := entries[prefix+pack+extension]; present {
				doomed = append(doomed, prefix+pack+extension)
			}
		}
		retired = append(retired, pack)
	}
	if len(doomed) == 0 {
		return nil, nil
	}
	// Delete the .pack key first: once gone, no reader looks for the index or filter.
	sort.Slice(doomed, func(i, j int) bool {
		return strings.HasSuffix(doomed[i], ".pack") && !strings.HasSuffix(doomed[j], ".pack")
	})
	for _, key := range doomed {
		if err := s.fs.client.Client.RemoveObject(ctx, s.fs.bucket, key, minio.RemoveObjectOptions{}); err != nil {
			return retired, fmt.Errorf("retire %s: %w", key, err)
		}
		s.fs.forgetObjectSize(key)
	}
	s.adoptPack()
	sort.Strings(retired)
	return retired, nil
}
