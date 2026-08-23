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

// The freshness window bounds how stale a negative answer may be.
//
// The index answers "this repository does not have that object" out of a
// listing it took itself. A listing is a snapshot, so a replica that has not
// re-listed recently could miss an object another replica has just written. The
// index therefore refuses to give a negative answer from a snapshot older than
// this, and re-lists first — which means every "no" is backed by an object
// store listing taken within this window, and S3 list-after-write is strongly
// consistent, so within the window the snapshot is the truth.
//
// The window exists at all because a clone of a packed repository asks "is this
// loose?" once per object and the answer is no every time; without it each of
// those would cost the round trip the index exists to remove. A quarter of a
// second collapses that storm into four listings a second while keeping the
// staleness bound below the latency of the request that would observe it.
//
// It is only a cross-replica bound. This process's own writes and deletions
// update the index as they happen, so a single writer is never stale about
// itself, at any window.
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

// repoObjectIndex is one repository's answer to "could this object be here",
// assembled from the two tiers the storage has.
//
// The loose tier is a cuckoo filter per fanout directory, because that is the
// granularity a listing refreshes at: one ListObjectsV2 covers objects/ab/ and
// nothing else, so a stale answer can be repaired for a two hundred and
// fifty-sixth of the repository at a time. The pack tier is one binary fuse
// filter per pack, fetched beside the pack.
//
// Every answer this type gives is negative-only. maybeLoose and maybePacked
// return true whenever they are not certain, and their callers treat true as
// "go and look properly".
type repoObjectIndex struct {
	prefix string
	// freshness is read once, when the repository is first touched, so the hot
	// path never consults the environment.
	freshness time.Duration

	mu      sync.Mutex
	fanouts map[string]*fanoutSnapshot
	// packs maps a published pack to its membership filter. A nil filter means
	// the pack is there but nothing is known about what is in it, so it can
	// rule nothing out and every probe against this repository must fall
	// through to the real lookup.
	packs    map[string]*binaryFuseFilter
	packedAt time.Time
	// packsKnown records that the pack directory has been listed at least
	// once. Until it has, nothing may be answered negatively.
	packsKnown bool
	// roots is the set of objects/XX/ directories that exist at all, from a
	// single listing of objects/ with a delimiter. A repository that has just
	// been compacted has none, so one listing proves that every one of the two
	// hundred and fifty-six possible fanout directories is empty — which is
	// what keeps a clone of a packed repository from paying a listing per
	// directory to learn the same thing.
	roots   map[string]bool
	rootsAt time.Time
}

// fanoutSnapshot is the loose-object membership of one objects/XX/ directory,
// with the instant it was taken.
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

// repoIndexFor returns the index for the repository rooted at this
// filesystem's prefix, creating it on first use.
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
// and object id, or reports that it is not a loose object path at all.
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

// looseObjectAbsent answers whether a loose-object read can be skipped. The
// second return value reports whether an answer was available at all; when it
// is false the caller must do the read. A true/true result is a proof of
// absence backed by a listing taken within objectIndexFreshness.
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
		// The directory does not exist, so nothing is in it.
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
		// A listing that failed is not evidence of absence. Fall through to
		// the read, which will report the real error or the real object.
		return false, false
	}
	return !refreshed.contains(key), true
}

// refreshRoots lists objects/ with a delimiter, which returns one entry per
// fanout directory that holds anything rather than one entry per object.
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

// noteLooseWrite records an object this process has just written. Doing it on
// the write path is what makes a single writer never stale about itself,
// whatever the freshness window is set to.
func (f *S3FS) noteLooseWrite(name string) {
	fanout, key, ok := looseObjectPath(name)
	if !ok {
		return
	}
	index := f.repoIndexFor()
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.roots != nil {
		// The directory now exists, and the shortcut that proves an absent
		// directory empty must not outlive the write that created it.
		index.roots[fanout] = true
	}
	snapshot := index.fanouts[fanout]
	if snapshot == nil {
		return
	}
	snapshot.filter.insert(key)
}

// noteLooseRemoved records an object this process has just deleted. Only a
// deletion this process performed may be recorded: clearing a fingerprint the
// filter did not put there could evict a colliding key and turn its answer into
// a false negative.
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

// invalidate drops every snapshot for this repository, so the next negative
// answer is taken from a fresh listing. Compaction calls it after the pack it
// wrote becomes visible.
func (i *repoObjectIndex) invalidate() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.fanouts = map[string]*fanoutSnapshot{}
	i.packs = map[string]*binaryFuseFilter{}
	i.packsKnown = false
	i.roots = nil
}

// maybePresent reports whether the repository could hold this object at all,
// consulting the pack filters and the loose snapshots and nothing else. False
// is a proof of absence; true means the caller must look properly.
//
// It is the only path that can answer a "do you have this object" without an
// object store round trip, and it is also the only path that can answer it
// without loading a pack index — which for a repository with many packs is tens
// of megabytes per pack that never has to be fetched or decoded.
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

	// Loose objects are spread over the fanout directories, so absence has to
	// be proved in the one directory that could hold this object.
	fanout := hex.EncodeToString(key[:1])
	absent, answered := i.looseAbsent(fs, fanout, key)
	if !answered {
		return true
	}
	return !absent
}

// anyPackMayHoldLocked reports whether any published pack could hold the key. A
// pack whose filter is absent answers yes, because a pack that cannot rule
// anything out must not be allowed to rule this out either.
func (i *repoObjectIndex) anyPackMayHoldLocked(key oidKey) bool {
	for _, filter := range i.packs {
		if filter == nil || filter.contains(key) {
			return true
		}
	}
	return false
}

// refreshPacks re-lists objects/pack/ and fetches the membership filter of any
// pack whose filter is not yet resident. A pack with no filter beside it is
// treated as possibly containing everything, which is the safe direction: it
// costs a real index lookup, never a wrong answer.
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
			// A pack written by something that does not publish a filter — or
			// one whose filter could not be read — is recorded as unknowable,
			// which makes every probe against this repository fall through to
			// the real lookup rather than risking a wrong negative.
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

// loadPackFilter reads the filter written beside a pack. A missing filter is
// not an error the caller has to handle differently from a failure: both mean
// the pack cannot rule an object out.
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
// bucket-absolute prefix.
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
