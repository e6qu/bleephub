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
// The storage this package presents to go-git is a log-structured merge tree
// whose levels git already defines. A loose object is a memtable entry: one
// object, one key, written once and never rewritten. A packfile is a sorted
// string table and its .idx is the table's index: many objects in one key, with
// an exact object id to byte offset map beside it. Everything the design gains
// — one round trip per clone instead of one per object, ranged reads that cost
// the size of the object rather than the size of the store, a cache with
// nothing to invalidate — follows from moving objects down that one level.
//
// ORDERING, AND WHY A CRASH CANNOT CORRUPT A REPOSITORY
//
// go-git discovers a pack by listing objects/pack/ for names ending in .pack
// (dotgit.objectPacks), and this package's own index does the same. A pack is
// therefore invisible until its .pack key exists, and an S3 PutObject — like a
// CompleteMultipartUpload — either publishes the whole object or none of it.
// That single fact is what makes the sequence below crash-safe, and it is why
// the .pack key is written last:
//
//	1. build the pack and its index on local disk
//	2. upload pack-<sha>.idx
//	3. upload pack-<sha>.bfilter
//	4. upload pack-<sha>.pack        <- the commit point
//	5. make the new pack visible to this process (Reindex, index invalidate)
//	6. delete the loose keys that step 1 actually packed
//
// A crash before step 4 leaves the loose objects untouched and authoritative;
// the .idx and .bfilter that may be lying around are named for a pack nothing
// lists, so no reader will ever open them, and the next compaction sweeps them.
// A crash after step 4 and during step 6 leaves both the pack and some loose
// objects; go-git reads either, and the duplicates are removed by the next
// compaction. There is no instant at which an object is in neither place,
// because the deletion of a loose key strictly follows the publication of the
// pack that contains it.
//
// CONCURRENCY
//
// Compaction against a concurrent push to the same repository: compaction packs
// the set of loose keys it listed, and step 6 deletes exactly the keys it
// packed — never "every loose key". An object a push writes after that listing
// is not in the pack and is not deleted, so it survives as a loose object and
// the next compaction picks it up.
//
// Two replicas compacting the same repository at once: both list, and their
// listings may overlap entirely. Each writes its own pack — the packs are named
// for their own contents, so they do not collide — and each deletes only the
// keys it packed. Every deleted key is therefore in at least one published
// pack. The one interleaving that needs care is a replica reading a loose
// object that the other has already deleted, in the middle of building its
// pack; that read fails with a definite not-exist, and buildPackTolerantly
// drops the object and rebuilds rather than failing, precisely because the only
// way that key can be gone is that the other replica already published it in a
// pack of its own. A durable lock is taken to stop the duplicated work from happening
// in the first place, but correctness does not rest on it, which matters
// because the lock has a lease and a compaction of a large repository can
// outlive one.
//
// Compaction against a concurrent read on this replica: the new pack is made
// visible (step 5) before any loose key is deleted (step 6), and both happen
// under the repository's storage lock for the moment it takes to swap the
// pack index. A reader that runs before the swap finds the loose object; one
// that runs after finds the pack.

const (
	// compactionMinLooseObjects is the number of loose objects below which
	// packing is not worth its own round trips.
	compactionMinLooseObjects = 64
	// compactionPackWindow is the delta window the pack encoder searches.
	compactionPackWindow = 10
	// compactionMergeThreshold is the pack count above which a compaction
	// rewrites the existing packs into the new one as well. Every pack costs a
	// resident index and a filter, and go-git loads every index of every pack
	// before it can answer a single packed lookup, so a repository that
	// accumulated one pack per push would give back what packing bought.
	compactionMergeThreshold = 8
	// supersededPackRetention is how long a pack that has been merged into a
	// newer one is kept before its bytes are removed. A request that began
	// before the merge may still be reading it, and a pack is only a few keys,
	// so the safe answer is to let the old one age out.
	supersededPackRetention = time.Hour
	// defaultMultipartThreshold is the size above which a pack is uploaded in
	// parts rather than in one request. Amazon S3 accepts a five gigabyte
	// single upload, but other endpoints that speak the same protocol accept
	// far less, so the point at which a pack switches to a multipart upload is
	// configurable.
	defaultMultipartThreshold = 64 << 20
	multipartThresholdEnv     = "BLEEPHUB_GITSTORE_MULTIPART_BYTES"
	multipartPartSize         = 32 << 20
)

// multipartThreshold reports the single-request upload ceiling.
func multipartThreshold() int64 {
	if raw := strings.TrimSpace(os.Getenv(multipartThresholdEnv)); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultMultipartThreshold
}

// CompactionResult reports what one compaction did, so a caller scheduling it
// can log or meter it.
type CompactionResult struct {
	// Packed is the number of loose objects written into the new pack.
	Packed int
	// Merged is the number of previously packed objects rewritten into it.
	Merged int
	// PackName is the pack-<sha> base name that was published, empty when
	// there was nothing to do.
	PackName string
	// PackBytes is the size of the published packfile.
	PackBytes int64
	// RetiredPacks lists packs whose bytes were removed after their retention
	// window elapsed.
	RetiredPacks []string
	// FilterBytes is the size of the membership filter published beside the
	// pack, which is what a replica keeps resident to answer absence.
	FilterBytes int
}

// Compactor is implemented by a git storage handle that can pack its own loose
// objects. Storage backed by a local filesystem does not implement it: git's
// own maintenance owns that layout, and a loose object on local disk costs one
// file open rather than one network round trip.
type Compactor interface {
	Compact(ctx context.Context) (CompactionResult, error)
}

// CompactRepository packs the loose objects of a repository whose storage
// supports it, and reports an empty result for storage that does not.
func CompactRepository(ctx context.Context, stor gitStorage.Storer) (CompactionResult, error) {
	compactor, ok := stor.(Compactor)
	if !ok {
		return CompactionResult{}, nil
	}
	return compactor.Compact(ctx)
}

// compactionTriggerEnv names the number of objects a repository may accumulate
// in its loose tier before a compaction is started in the background.
const compactionTriggerEnv = "BLEEPHUB_GITSTORE_COMPACT_AFTER"

// defaultCompactionTrigger is deliberately larger than an ordinary push. A
// compaction reads every loose object once, so running one after every push
// would spend that read on a handful of objects; waiting until a repository has
// accumulated a few thousand amortizes it over a batch worth packing.
const defaultCompactionTrigger = 4096

// compactionRunTimeout bounds a background compaction. A monorepo's compaction
// reads every loose object it packs, so the bound is generous.
const compactionRunTimeout = 30 * time.Minute

// noteObjectWritten counts an object into the loose tier and starts a
// compaction once enough have accumulated.
//
// The write path is where the trigger belongs: the loose tier is a memtable,
// and a memtable is flushed when it fills. Only one compaction of a repository
// runs at a time in this process, and a compaction that is already running
// simply absorbs the writes that arrive while it works, because it deletes only
// the keys it listed.
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

// compactionRequested is called when a repository's loose tier has filled.
//
// The signal belongs on the write path — the loose tier is a memtable, and a
// memtable is flushed when it fills — but the goroutine that performs the
// flush does not belong here: this package has no lifecycle to attach one to,
// and a compaction started from a bare `go` statement can outlive the process
// that started it, still writing to storage after shutdown has stopped waiting.
// The owner of the signal is whoever has a lifetime to bound it with. The
// server sets this to its own scheduler, which claims one compaction per
// repository, runs it under a supervised goroutine, and cancels it at
// shutdown. Left unset — in a test, or an embedding with no scheduler — the
// loose tier simply keeps accumulating, which is a slower repository and not a
// broken one.
//
// Not every object reaches storage through a push: the REST git-database
// endpoints write blobs, trees and commits directly, so a repository built
// entirely through the API depends on this signal rather than on the
// post-receive scheduling.
var (
	compactionRequestMu   sync.RWMutex
	compactionRequestFunc func(repo string, stor gitStorage.Storer)
)

// SetCompactionRequestHandler installs the handler that performs a compaction
// when a repository's loose tier fills. Passing nil removes it.
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

	// The new pack is authoritative from here. Making it visible before any
	// loose key disappears is what keeps a concurrent reader on this replica
	// from looking in the one place the object is no longer in.
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

// buildPackTolerantly encodes the pack, and if an object vanished between the
// listing and the read — which can only be another replica's compaction having
// published it in a pack of its own and removed the loose key — drops it and
// tries again. The check is made this way round rather than by probing every
// object first because probing is one object store round trip per object, which
// is the cost compaction exists to remove.
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

// retainPacked narrows the loose keys due for deletion to the ones that
// actually made it into the published pack.
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

// listLooseObjects enumerates every loose object of the repository. One listing
// page carries a thousand keys, so enumerating a million loose objects costs a
// thousand round trips rather than the million a per-object probe would.
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

// listPublishedPacks names every pack that is visible to a reader, which is
// exactly the set whose .pack key exists.
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

// hashesInPacks reads the object ids out of the existing packs' indexes, which
// is what a merging compaction rewrites.
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

// builtPack is a finished pack sitting on local disk, waiting to be published.
type builtPack struct {
	name       string
	packPath   string
	packSize   int64
	index      []byte
	filter     []byte
	filterBits int
}

func (b *builtPack) cleanup() {
	if b == nil || b.packPath == "" {
		return
	}
	_ = os.Remove(b.packPath)
}

// buildPack encodes the objects into a packfile on local disk and derives the
// index and membership filter from the bytes it wrote. The pack is staged
// locally rather than streamed to the object store because its name is the hash
// of its own contents, which is not known until the last byte is written.
func (s *atomicRefStorer) buildPack(hashes []plumbing.Hash) (*builtPack, error) {
	dir, err := compactionScratchDir()
	if err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(dir, "compact-*.pack")
	if err != nil {
		return nil, fmt.Errorf("stage pack: %w", err)
	}
	built := &builtPack{packPath: temp.Name()}

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
	// The index is derived by parsing the bytes that were actually written
	// rather than from the encoder's own bookkeeping, so an index can only
	// describe a pack that exists byte for byte.
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

// compactionScratchDir is where a pack is staged while it is being built. It
// shares the pack cache's directory because both hold pack bytes and both are
// sized against the same local disk budget.
func compactionScratchDir() (string, error) {
	dir := filepath.Join(packCacheDir(), "staging")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("compaction staging directory: %w", err)
	}
	return dir, nil
}

// publishPack uploads the index, the filter and finally the pack. The order is
// the crash-safety argument: see the top of this file.
func (s *atomicRefStorer) publishPack(ctx context.Context, built *builtPack) error {
	base := path.Join("objects", "pack", built.name)
	if err := s.putObject(ctx, base+".idx", built.index); err != nil {
		return err
	}
	if err := s.putObject(ctx, base+".bfilter", built.filter); err != nil {
		return err
	}
	if err := s.uploadPackFile(ctx, base+".pack", built.packPath, built.packSize); err != nil {
		return err
	}
	// The replica that built the pack already holds its bytes, so seeding the
	// shared disk cache from the staged file saves it downloading back what it
	// just uploaded.
	s.seedPackCache(base+".pack", built.packPath, built.packSize)
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

// uploadPackFile publishes the packfile. A pack small enough for one request is
// sent as one, and a large one goes through a multipart upload, which is what
// keeps a monorepo's multi-gigabyte pack from having to be held in memory —
// and which is still atomic, because the object appears only when the
// completion request succeeds.
func (s *atomicRefStorer) uploadPackFile(ctx context.Context, name, sourcePath string, size int64) error {
	key := s.fs.key(name)
	file, err := os.Open(sourcePath)
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

func (s *atomicRefStorer) seedPackCache(name, sourcePath string, size int64) {
	cache := sharedPackDiskCache()
	if cache == nil {
		return
	}
	file, err := os.Open(sourcePath)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	key := s.fs.key(name)
	chunkSize := s.fs.shared().chunkSize
	cache.storeSize(s.fs.bucket, key, chunkSize, size)
	for chunk := int64(0); ; chunk++ {
		// Each chunk gets its own buffer: an admitted chunk is shared with
		// every later reader of it, so a buffer reused for the next chunk
		// would rewrite bytes another goroutine is reading.
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

// adoptPack makes the pack this replica just published visible to the storer,
// and drops the membership snapshots so the next negative answer is taken
// against the new shape of the repository.
func (s *atomicRefStorer) adoptPack() {
	s.mu.Lock()
	if reindexer, ok := s.storer.(interface{ Reindex() }); ok {
		reindexer.Reindex()
	}
	s.mu.Unlock()
	s.fs.repoIndexFor().invalidate()
}

// deleteLooseObjects removes exactly the keys that went into the published
// pack, a thousand at a time.
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

// markSuperseded records that a pack has been rewritten into a newer one. The
// marker rather than the deletion is written because a request that started
// before the merge may still be reading the old pack; retireSupersededPacks
// removes the bytes once the marker has aged.
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

// retireSupersededPacks removes the packs whose supersession marker is older
// than the retention window, using the object store's own record of when the
// marker was written rather than this replica's clock.
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
	// The .pack key is what makes a pack visible, so it goes first: after it
	// is gone no reader will look for the index or the filter.
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
