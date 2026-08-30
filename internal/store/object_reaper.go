package store

import (
	"context"
	"strings"
	"time"
)

// Object reaper — reclaim object-store bytes that no live metadata references.
//
// Because every write stores bytes BEFORE committing the metadata row that
// points at them, a crash between the two, or a delete whose object-store call
// failed silently, leaves an object no record references. Those orphans are pure
// storage waste (readers key off metadata, never a bucket listing), but nothing
// reclaims them.
//
// The reaper is a mark-and-sweep: it lists the bucket and removes objects that
// (a) live under a prefix whose full live-set this process can enumerate, (b)
// are referenced by no live metadata, and (c) are older than a grace window (so
// an in-flight byte-first upload whose metadata has not yet committed is never
// swept). It is SAFE-BY-DEFAULT: report-only unless delete is explicitly opted
// in, and it never touches prefixes whose live-set it cannot fully enumerate —
// job/step logs (tracked only transiently) and container-registry blobs — so it
// can never delete a live object it simply failed to account for.

// reapablePrefixes are the object-key prefixes whose complete live-set this
// process can enumerate from in-memory metadata. Anything outside them
// (actions/logs/, packages/registry/) is ignored entirely — neither reported nor
// deleted — because we cannot prove an object there is an orphan.
var reapablePrefixes = []string{
	"actions/artifacts/",
	"actions/caches/",
	"releases/assets/",
	"packages/files/",
	"code-scanning/",
	"attestations/",
	"pages/",
	"lfs/",
}

func withinReapablePrefix(relKey string) bool {
	for _, p := range reapablePrefixes {
		if strings.HasPrefix(relKey, p) {
			return true
		}
	}
	return false
}

// ReapOptions configures one reaper pass.
type ReapOptions struct {
	// Delete removes the orphans found; false (the default) only reports them.
	Delete bool
	// GracePeriod protects objects younger than this from being treated as
	// orphans, so a byte-first upload whose metadata is still committing is safe.
	GracePeriod time.Duration
}

// ReapReport summarizes one pass.
type ReapReport struct {
	Scanned      int
	OrphanCount  int
	OrphanBytes  int64
	DeletedCount int
	DeleteErrors int
	SampleKeys   []string // up to a handful of orphan keys, for the operator's log
	ObjectBacked bool     // false when the object store is not S3-backed (no-op)
}

// ReapOrphanObjects performs one reaper pass. It is a no-op unless the object
// store is S3-backed.
func (st *Store) ReapOrphanObjects(ctx context.Context, opts ReapOptions) (ReapReport, error) {
	s3bs, ok := st.ObjectByteStore.(*S3ActionsByteStore)
	if !ok || s3bs == nil {
		return ReapReport{ObjectBacked: false}, nil
	}

	st.Mu.RLock()
	live := st.liveObjectKeysLocked()
	now := st.CurrentTime()
	st.Mu.RUnlock()

	listings, err := s3bs.listAll(ctx)
	if err != nil {
		return ReapReport{ObjectBacked: true}, err
	}

	report := ReapReport{ObjectBacked: true}
	for _, obj := range listings {
		report.Scanned++
		if !withinReapablePrefix(obj.Key) {
			continue
		}
		if _, isLive := live[obj.Key]; isLive {
			continue
		}
		if opts.GracePeriod > 0 && now.Sub(obj.LastModified) < opts.GracePeriod {
			continue // too young — its metadata may still be committing
		}
		report.OrphanCount++
		report.OrphanBytes += obj.Size
		if len(report.SampleKeys) < 20 {
			report.SampleKeys = append(report.SampleKeys, obj.Key)
		}
		if opts.Delete {
			if err := s3bs.Delete(ctx, obj.Key); err != nil {
				report.DeleteErrors++
			} else {
				report.DeletedCount++
			}
		}
	}
	return report, nil
}

// liveObjectKeysLocked returns the set of object keys (relative to the store
// prefix) that live metadata references, for the reapable subsystems. Callers
// hold st.Mu. It must stay COMPLETE for every reapable prefix: a key it forgets
// is a live object the delete path would destroy.
func (st *Store) liveObjectKeysLocked() map[string]struct{} {
	live := map[string]struct{}{}

	if as := st.ActionsArtifacts; as != nil {
		as.Mu.RLock()
		for id := range as.Artifacts {
			live[ArtifactDataKey(id)] = struct{}{}
		}
		for id := range as.Caches {
			live[CacheDataKey(id)] = struct{}{}
		}
		as.Mu.RUnlock()
	}

	if st.Releases != nil {
		st.Releases.Mu.RLock()
		for _, rel := range st.Releases.ByID {
			for _, asset := range rel.Assets {
				live[ReleaseAssetDataKey(asset.ID)] = struct{}{}
			}
		}
		st.Releases.Mu.RUnlock()
	}

	for _, files := range st.PackageFilesByVersion {
		for _, file := range files {
			if file.StoragePath != "" {
				live[file.StoragePath] = struct{}{}
			}
		}
	}

	for _, db := range st.CodeQLDatabases {
		if db.StoragePath != "" {
			live[db.StoragePath] = struct{}{}
		}
	}
	for _, va := range st.CodeQLVariantAnalyses {
		if va.StoragePath != "" {
			live[va.StoragePath] = struct{}{}
		}
	}

	for _, att := range st.Attestations {
		if att.StoragePath != "" {
			live[att.StoragePath] = struct{}{}
		}
	}

	for repoID := range st.Repos {
		if keys, ok := st.pagesPublicationKeysLocked(repoID); ok {
			for key := range keys {
				live[key] = struct{}{}
			}
		}
	}

	for _, objects := range st.LFSObjects {
		for oid := range objects {
			live[LFSObjectDataKey(oid)] = struct{}{}
		}
	}

	return live
}
