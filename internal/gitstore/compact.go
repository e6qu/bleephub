package gitstore

import (
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
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
	// compactionMinLooseObjects is the loose count below which packing is not
	// worth its round trips.
	compactionMinLooseObjects = 64
	// compactionPackWindow is the delta window the pack encoder searches.
	compactionPackWindow = 10
	// compactionMergeThreshold is the pack count above which a compaction also
	// rewrites existing packs into the new one. go-git loads every pack's index
	// before answering any packed lookup, so one-pack-per-push would give back
	// what packing bought.
	compactionMergeThreshold = 8
	// supersededPackRetention is how long a merged-away pack is kept before its
	// bytes are removed, in case a request that began before the merge still
	// reads it.
	supersededPackRetention = time.Hour
	// defaultMultipartThreshold is the size above which a pack is uploaded in
	// parts. Configurable because non-Amazon endpoints cap the single-request
	// upload well below Amazon's 5 GiB.
	defaultMultipartThreshold = 64 << 20
	multipartThresholdEnv     = "BLEEPHUB_GITSTORE_MULTIPART_BYTES"
	multipartPartSize         = 32 << 20
)

func multipartThreshold() int64 {
	if raw := strings.TrimSpace(os.Getenv(multipartThresholdEnv)); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultMultipartThreshold
}

// CompactionResult reports what one compaction did.
type CompactionResult struct {
	// Packed is the number of loose objects written into the new pack.
	Packed int
	// Merged is the number of previously packed objects rewritten into it.
	Merged int
	// PackName is the published pack-<sha> base name, empty when there was
	// nothing to do.
	PackName string
	// PackBytes is the size of the published packfile.
	PackBytes int64
	// RetiredPacks lists packs whose bytes were removed after their retention
	// window elapsed.
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

// compactionTriggerEnv sets the loose-object count that triggers a background
// compaction.
const compactionTriggerEnv = "BLEEPHUB_GITSTORE_COMPACT_AFTER"

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
	trigger := s.compactionTrigger()
	if trigger <= 0 || s.looseWrites.Add(1) < trigger {
		return
	}
	s.looseWrites.Store(0)
	if request := compactionRequestHook(); request != nil {
		request(s.repo, s)
	}
}

// compactionRequestFunc is invoked when a repository's loose tier fills. The
// package owns no goroutine lifecycle, so the actual flush is delegated to an
// installed handler: the server's scheduler runs one supervised, cancel-at-
// shutdown compaction per repository. Left unset (tests, embeddings), the loose
// tier just keeps accumulating — slower, not broken. The signal is needed
// because the REST git-database endpoints write objects directly, outside the
// post-receive path.
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

func (s *atomicRefStorer) compactionTrigger() int64 {
	s.triggerOnce.Do(func() {
		s.trigger = defaultCompactionTrigger
		if raw := strings.TrimSpace(os.Getenv(compactionTriggerEnv)); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed >= 0 {
				s.trigger = parsed
			}
		}
	})
	return s.trigger
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

	existing, err := s.listPublishedPacks(ctx)
	if err != nil {
		return result, err
	}
	merge := len(existing) > compactionMergeThreshold

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
	var continuation *string
	for {
		resp, err := s.fs.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.fs.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuation,
		})
		if err != nil {
			return nil, fmt.Errorf("list loose objects of %s: %w", s.repo, err)
		}
		for _, entry := range resp.Contents {
			key := aws.ToString(entry.Key)
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
		if !aws.ToBool(resp.IsTruncated) {
			break
		}
		continuation = resp.NextContinuationToken
	}
	return objects, nil
}

// listPublishedPacks names every reader-visible pack, i.e. those whose .pack
// key exists.
func (s *atomicRefStorer) listPublishedPacks(ctx context.Context) ([]string, error) {
	keys, err := s.listPackDirectory(ctx)
	if err != nil {
		return nil, err
	}
	var packs []string
	for key := range keys {
		base := path.Base(key)
		if strings.HasPrefix(base, "pack-") && strings.HasSuffix(base, ".pack") {
			packs = append(packs, strings.TrimSuffix(base, ".pack"))
		}
	}
	sort.Strings(packs)
	return packs, nil
}

func (s *atomicRefStorer) listPackDirectory(ctx context.Context) (map[string]time.Time, error) {
	prefix := s.fs.key(path.Join("objects", "pack")) + "/"
	entries := map[string]time.Time{}
	var continuation *string
	for {
		resp, err := s.fs.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.fs.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuation,
		})
		if err != nil {
			return nil, fmt.Errorf("list packs of %s: %w", s.repo, err)
		}
		for _, entry := range resp.Contents {
			entries[aws.ToString(entry.Key)] = aws.ToTime(entry.LastModified)
		}
		if !aws.ToBool(resp.IsTruncated) {
			return entries, nil
		}
		continuation = resp.NextContinuationToken
	}
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
	// under an os.Root on the directory, which stops the name or a planted
	// symlink from escaping it.
	stagingDir string
	packFile   string
	packSize   int64
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
	dir, err := compactionScratchDir()
	if err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(dir, "compact-*.pack")
	if err != nil {
		return nil, fmt.Errorf("stage pack: %w", err)
	}
	built := &builtPack{stagingDir: dir, packFile: filepath.Base(temp.Name())}

	encoder := packfile.NewEncoder(temp, s, false)
	checksum, err := encoder.Encode(hashes, compactionPackWindow)
	if err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, fmt.Errorf("encode pack for %s: %w", s.repo, err)
	}
	size, err := temp.Seek(0, io.SeekCurrent)
	if err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, fmt.Errorf("size staged pack: %w", err)
	}
	built.name = "pack-" + checksum.String()
	built.packSize = size

	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, fmt.Errorf("rewind staged pack: %w", err)
	}
	// Derive the index by parsing the written bytes, not the encoder's
	// bookkeeping, so it can only describe the pack that exists byte for byte.
	writer := new(idxfile.Writer)
	parser, err := packfile.NewParser(packfile.NewScanner(temp), writer)
	if err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, fmt.Errorf("parse staged pack: %w", err)
	}
	if _, err := parser.Parse(); err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, fmt.Errorf("parse staged pack: %w", err)
	}
	index, err := writer.Index()
	if err != nil {
		_ = temp.Close()
		built.cleanup()
		return nil, fmt.Errorf("index staged pack: %w", err)
	}
	if err := temp.Close(); err != nil {
		built.cleanup()
		return nil, fmt.Errorf("close staged pack: %w", err)
	}

	var encoded strings.Builder
	if _, err := idxfile.NewEncoder(&encoded).Encode(index); err != nil {
		built.cleanup()
		return nil, fmt.Errorf("encode index: %w", err)
	}
	built.index = []byte(encoded.String())

	keys := make([]oidKey, 0, len(hashes))
	iter, err := index.EntriesByOffset()
	if err != nil {
		built.cleanup()
		return nil, fmt.Errorf("read staged index: %w", err)
	}
	for {
		entry, err := iter.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			built.cleanup()
			return nil, fmt.Errorf("read staged index: %w", err)
		}
		keys = append(keys, oidKeyFrom(entry.Hash[:]))
	}
	filter, err := newBinaryFuseFilter(keys)
	if err != nil {
		built.cleanup()
		return nil, err
	}
	built.filter = filter.encode()
	built.filterBits = filter.bits()
	return built, nil
}

// compactionScratchDir stages a pack while it is built. It shares the pack
// cache's directory: both hold pack bytes against the same local disk budget.
func compactionScratchDir() (string, error) {
	dir := filepath.Join(packCacheDir(), "staging")
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
	_, err := s.fs.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.fs.bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(string(body)),
	})
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

	if size <= multipartThreshold() {
		if _, err := s.fs.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.fs.bucket),
			Key:           aws.String(key),
			Body:          file,
			ContentLength: aws.Int64(size),
		}); err != nil {
			return fmt.Errorf("s3 put %s: %w", key, err)
		}
		return nil
	}

	created, err := s.fs.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.fs.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 multipart create %s: %w", key, err)
	}
	uploadID := aws.ToString(created.UploadId)

	var parts []s3types.CompletedPart
	buffer := make([]byte, min(int64(multipartPartSize), max(size/2+1, 1)))
	for number := int32(1); ; number++ {
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
		uploaded, uploadErr := s.fs.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(s.fs.bucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(number),
			Body:          strings.NewReader(string(buffer[:read])),
			ContentLength: aws.Int64(int64(read)),
		})
		if uploadErr != nil {
			s.abortMultipart(ctx, key, uploadID)
			return fmt.Errorf("s3 upload part %d of %s: %w", number, key, uploadErr)
		}
		parts = append(parts, s3types.CompletedPart{ETag: uploaded.ETag, PartNumber: aws.Int32(number)})
		if read < len(buffer) {
			break
		}
	}

	if _, err := s.fs.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.fs.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		s.abortMultipart(ctx, key, uploadID)
		return fmt.Errorf("s3 multipart complete %s: %w", key, err)
	}
	return nil
}

func (s *atomicRefStorer) abortMultipart(ctx context.Context, key, uploadID string) {
	_, _ = s.fs.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.fs.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
}

func (s *atomicRefStorer) seedPackCache(name string, built *builtPack) {
	cache := sharedPackDiskCache()
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
	const batch = 1000
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.key)
	}
	for start := 0; start < len(keys); start += batch {
		end := min(start+batch, len(keys))
		identifiers := make([]s3types.ObjectIdentifier, 0, end-start)
		for _, key := range keys[start:end] {
			identifiers = append(identifiers, s3types.ObjectIdentifier{Key: aws.String(key)})
		}
		resp, err := s.fs.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.fs.bucket),
			Delete: &s3types.Delete{Objects: identifiers, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete %d packed loose objects: %w", len(identifiers), err)
		}
		if len(resp.Errors) > 0 {
			first := resp.Errors[0]
			return fmt.Errorf("delete %s: %s", aws.ToString(first.Key), aws.ToString(first.Message))
		}
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
	for _, modified := range entries {
		if modified.After(newest) {
			newest = modified
		}
	}

	var retired []string
	var doomed []string
	for key, modified := range entries {
		base := path.Base(key)
		pack, ok := strings.CutSuffix(base, ".superseded")
		if !ok {
			continue
		}
		if newest.Sub(modified) < supersededPackRetention {
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
		if _, err := s.fs.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.fs.bucket),
			Key:    aws.String(key),
		}); err != nil {
			return retired, fmt.Errorf("retire %s: %w", key, err)
		}
		s.fs.forgetObjectSize(key)
	}
	s.adoptPack()
	sort.Strings(retired)
	return retired, nil
}
