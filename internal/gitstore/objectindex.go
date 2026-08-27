package gitstore

import (
	"context"
	"encoding/hex"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The freshness window bounds how stale a cross-replica negative answer may be:
// the index refuses to answer "absent" from a snapshot older than this and
// re-lists first, and S3 list-after-write is strongly consistent, so within the
// window the snapshot is the truth. The window batches the per-object "is this
// loose?" probes a clone of a packed repository makes into a few listings a
// second. This process's own writes/deletes update the index immediately, so a
// single writer is never stale about itself.
const (
	objectIndexFreshnessEnv     = "BLEEPHUB_GITSTORE_INDEX_FRESHNESS"
	defaultObjectIndexFreshness = 250 * time.Millisecond
)

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(raw); err == nil && parsed >= 0 {
		return parsed
	}
	if millis, err := strconv.Atoi(raw); err == nil && millis >= 0 {
		return time.Duration(millis) * time.Millisecond
	}
	return fallback
}

// repoObjectIndex answers "could this object be here" for one repository from
// two tiers: a cuckoo filter per objects/XX/ fanout directory (the granularity
// one ListObjectsV2 refreshes) and one binary fuse filter per pack. Every
// answer is negative-only; true means the caller must look properly.
type repoObjectIndex struct {
	prefix string
	// freshness is read once at first touch so the hot path never consults the
	// environment.
	freshness time.Duration

	mu      sync.Mutex
	fanouts map[string]*fanoutSnapshot
	// packs maps a published pack to its membership filter. A nil filter means
	// the pack exists but rules nothing out, forcing every probe through to the
	// real lookup.
	packs    map[string]*binaryFuseFilter
	packedAt time.Time
	// packsKnown gates negative answers: until the pack directory has been
	// listed once, nothing may be answered negatively.
	packsKnown bool
	// roots is the set of objects/XX/ directories that exist, from one
	// delimited listing of objects/. A just-compacted repository has none, so
	// one listing proves all 256 fanout directories empty.
	roots   map[string]bool
	rootsAt time.Time
}

// fanoutSnapshot is the loose-object membership of one objects/XX/ directory
// and the instant it was taken.
type fanoutSnapshot struct {
	filter *cuckooFilter
	takenT time.Time
}

func newRepoObjectIndex(prefix string) *repoObjectIndex {
	return &repoObjectIndex{
		prefix:    prefix,
		freshness: envDuration(objectIndexFreshnessEnv, defaultObjectIndexFreshness),
		fanouts:   map[string]*fanoutSnapshot{},
		packs:     map[string]*binaryFuseFilter{},
	}
}

// repoIndexFor returns the index for this filesystem's prefix, creating it on
// first use.
func (f *S3FS) repoIndexFor() *repoObjectIndex {
	shared := f.shared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	index, ok := shared.objects[f.prefix]
	if !ok {
		index = newRepoObjectIndex(f.prefix)
		shared.objects[f.prefix] = index
	}
	return index
}

// looseObjectPath splits a repository-relative path into its fanout directory
// and object id, or reports that it is not a loose object path.
func looseObjectPath(name string) (fanout string, key oidKey, ok bool) {
	cleaned := path.Clean(name)
	parts := strings.Split(cleaned, "/")
	if len(parts) != 3 || parts[0] != "objects" || len(parts[1]) != 2 {
		return "", oidKey{}, false
	}
	raw, err := hex.DecodeString(parts[1] + parts[2])
	if err != nil || (len(raw) != 20 && len(raw) != 32) {
		return "", oidKey{}, false
	}
	return parts[1], oidKeyFrom(raw), true
}

// looseObjectAbsent reports whether a loose-object read can be skipped.
// answered is false when no answer was available and the caller must read.
func (f *S3FS) looseObjectAbsent(name string) (absent bool, answered bool) {
	fanout, key, ok := looseObjectPath(name)
	if !ok {
		return false, false
	}
	index := f.repoIndexFor()
	return index.looseAbsent(f, fanout, key)
}

func (i *repoObjectIndex) looseAbsent(fs *S3FS, fanout string, key oidKey) (bool, bool) {
	i.mu.Lock()
	snapshot := i.fanouts[fanout]
	fresh := snapshot != nil && time.Since(snapshot.takenT) <= i.freshness
	if snapshot != nil && snapshot.filter.contains(key) {
		i.mu.Unlock()
		return false, true
	}
	rootsFresh := i.roots != nil && time.Since(i.rootsAt) <= i.freshness
	rootExists := i.roots[fanout]
	i.mu.Unlock()

	if fresh {
		return true, true
	}
	if rootsFresh && !rootExists {
		return true, true
	}
	if !rootsFresh {
		roots, err := i.refreshRoots(fs)
		if err != nil {
			return false, false
		}
		if !roots[fanout] {
			return true, true
		}
	}

	refreshed, err := i.refreshFanout(fs, fanout)
	if err != nil {
		// A failed listing is not evidence of absence; fall through to the read.
		return false, false
	}
	return !refreshed.contains(key), true
}

// refreshRoots lists objects/ with a delimiter, yielding one entry per
// non-empty fanout directory rather than one per object.
func (i *repoObjectIndex) refreshRoots(fs *S3FS) (map[string]bool, error) {
	prefix := fs.key("objects") + "/"
	names, err := listCommonPrefixes(fs, prefix)
	if err != nil {
		return nil, err
	}
	roots := make(map[string]bool, len(names))
	for _, name := range names {
		trimmed := strings.TrimSuffix(strings.TrimPrefix(name, prefix), "/")
		if len(trimmed) == 2 {
			roots[trimmed] = true
		}
	}
	i.mu.Lock()
	i.roots = roots
	i.rootsAt = time.Now()
	i.mu.Unlock()
	return roots, nil
}

// refreshFanout re-lists one objects/XX/ directory and rebuilds its filter.
func (i *repoObjectIndex) refreshFanout(fs *S3FS, fanout string) (*cuckooFilter, error) {
	prefix := fs.key(path.Join("objects", fanout)) + "/"
	names, err := listKeys(fs, prefix)
	if err != nil {
		return nil, err
	}
	filter := newCuckooFilter(max(len(names), 16))
	for _, name := range names {
		base := path.Base(name)
		raw, err := hex.DecodeString(fanout + base)
		if err != nil || (len(raw) != 20 && len(raw) != 32) {
			continue
		}
		filter.insert(oidKeyFrom(raw))
	}
	i.mu.Lock()
	i.fanouts[fanout] = &fanoutSnapshot{filter: filter, takenT: time.Now()}
	i.mu.Unlock()
	return filter, nil
}

// noteLooseWrite records an object this process just wrote, keeping a single
// writer never stale about itself regardless of the freshness window.
func (f *S3FS) noteLooseWrite(name string) {
	fanout, key, ok := looseObjectPath(name)
	if !ok {
		return
	}
	index := f.repoIndexFor()
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.roots != nil {
		// The directory now exists; the empty-directory shortcut must not
		// outlive the write that created it.
		index.roots[fanout] = true
	}
	snapshot := index.fanouts[fanout]
	if snapshot == nil {
		return
	}
	snapshot.filter.insert(key)
}

// noteLooseRemoved records an object this process just deleted. Only this
// process's own deletions may be recorded: clearing a fingerprint the filter
// did not put there could evict a colliding key into a false negative.
func (f *S3FS) noteLooseRemoved(name string) {
	fanout, key, ok := looseObjectPath(name)
	if !ok {
		return
	}
	index := f.repoIndexFor()
	index.mu.Lock()
	defer index.mu.Unlock()
	if snapshot := index.fanouts[fanout]; snapshot != nil {
		snapshot.filter.remove(key)
	}
}

// invalidate drops every snapshot so the next negative answer comes from a
// fresh listing. Compaction calls it after its new pack becomes visible.
func (i *repoObjectIndex) invalidate() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.fanouts = map[string]*fanoutSnapshot{}
	i.packs = map[string]*binaryFuseFilter{}
	i.packsKnown = false
	i.roots = nil
}

// maybePresent reports whether the repository could hold this object,
// consulting only the pack filters and loose snapshots. False is proof of
// absence; true means the caller must look properly. It is the only path that
// answers without an object store round trip or loading a pack index.
func (i *repoObjectIndex) maybePresent(fs *S3FS, key oidKey) bool {
	i.mu.Lock()
	packsFresh := i.packsKnown && time.Since(i.packedAt) <= i.freshness
	maybe := i.anyPackMayHoldLocked(key)
	i.mu.Unlock()
	if maybe {
		return true
	}

	if !packsFresh {
		if err := i.refreshPacks(fs); err != nil {
			return true
		}
		i.mu.Lock()
		maybe = i.anyPackMayHoldLocked(key)
		i.mu.Unlock()
		if maybe {
			return true
		}
	}

	fanout := hex.EncodeToString(key[:1])
	absent, answered := i.looseAbsent(fs, fanout, key)
	if !answered {
		return true
	}
	return !absent
}

// anyPackMayHoldLocked reports whether any published pack could hold the key. A
// pack with no filter answers yes, since it can rule nothing out.
func (i *repoObjectIndex) anyPackMayHoldLocked(key oidKey) bool {
	for _, filter := range i.packs {
		if filter == nil || filter.contains(key) {
			return true
		}
	}
	return false
}

// refreshPacks re-lists objects/pack/ and fetches the membership filter of any
// pack not yet resident. A pack with no filter beside it is treated as possibly
// containing everything: a real index lookup, never a wrong answer.
func (i *repoObjectIndex) refreshPacks(fs *S3FS) error {
	prefix := fs.key(path.Join("objects", "pack")) + "/"
	names, err := listKeys(fs, prefix)
	if err != nil {
		return err
	}

	present := map[string]bool{}
	for _, name := range names {
		base := path.Base(name)
		if !strings.HasPrefix(base, "pack-") || !strings.HasSuffix(base, ".pack") {
			continue
		}
		present[strings.TrimSuffix(base, ".pack")] = true
	}

	i.mu.Lock()
	for name := range i.packs {
		if !present[name] {
			delete(i.packs, name)
		}
	}
	missing := make([]string, 0, len(present))
	for name := range present {
		if _, ok := i.packs[name]; !ok {
			missing = append(missing, name)
		}
	}
	i.mu.Unlock()

	loaded := map[string]*binaryFuseFilter{}
	for _, name := range missing {
		filter, err := loadPackFilter(fs, name)
		if err != nil {
			// No readable filter: record the pack as unknowable so probes fall
			// through to the real lookup rather than risk a wrong negative.
			filter = nil
		}
		loaded[name] = filter
	}

	i.mu.Lock()
	for name, filter := range loaded {
		i.packs[name] = filter
	}
	i.packsKnown = len(i.packs) == len(present)
	i.packedAt = time.Now()
	i.mu.Unlock()
	return nil
}

// loadPackFilter reads the filter written beside a pack.
func loadPackFilter(fs *S3FS, packName string) (*binaryFuseFilter, error) {
	name := path.Join("objects", "pack", packName+".bfilter")
	file, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return decodeBinaryFuseFilter(raw)
}

// listCommonPrefixes enumerates the immediate subdirectories of a
// bucket-absolute prefix, following continuation tokens.
func listCommonPrefixes(fs *S3FS, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var prefixes []string
	var continuation *string
	for {
		resp, err := fs.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(fs.bucket),
			Prefix:            aws.String(prefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: continuation,
		})
		if err != nil {
			return nil, err
		}
		for _, common := range resp.CommonPrefixes {
			prefixes = append(prefixes, aws.ToString(common.Prefix))
		}
		if !aws.ToBool(resp.IsTruncated) {
			return prefixes, nil
		}
		continuation = resp.NextContinuationToken
	}
}

// listKeys enumerates every key directly under a bucket-absolute prefix,
// following continuation tokens.
func listKeys(fs *S3FS, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var keys []string
	var continuation *string
	for {
		resp, err := fs.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(fs.bucket),
			Prefix:            aws.String(prefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: continuation,
		})
		if err != nil {
			return nil, err
		}
		for _, obj := range resp.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
		if !aws.ToBool(resp.IsTruncated) {
			return keys, nil
		}
		continuation = resp.NextContinuationToken
	}
}
