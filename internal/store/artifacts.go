package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ArtifactStore holds artifact/cache metadata for @actions/artifact v4 and the
// byte backend for artifact/cache/log content. Persisted startup requires
// byteStore so durable bytes reach object storage, not local disk.
type ArtifactStore struct {
	Mu          sync.RWMutex          `json:"-"`
	Artifacts   map[int64]*Artifact   `json:"-"`
	NextID      int64                 `json:"-"`
	Caches      map[int64]*CacheEntry `json:"-"`
	CacheIndex  map[string]int64      `json:"-"`
	NextCacheID int64                 `json:"-"`
	logPlans    map[int]string        // logID → the plan that reserved it
	DataDir     string                `json:"-"` // empty = in-memory mode
	// stageDir roots in-progress upload scratch files. It is DataDir when set, else
	// a per-store temp dir so two stores (e.g. two test servers) reusing the same
	// artifact id never collide on a shared os.TempDir path.
	stageDir  string
	ByteStore ActionsByteStore `json:"-"`
	Persist   *Persistence     `json:"-"`
	revision  int64
	refreshMu sync.Mutex
	// MaxRepoCacheBytes is the per-repo cache budget; finalizing over it evicts
	// LRU finalized entries. A field so tests can drive eviction with small caches.
	MaxRepoCacheBytes int64 `json:"-"`
}

func (as *ArtifactStore) ClaimLog(logID int, planID string) {
	as.Mu.Lock()
	defer as.Mu.Unlock()
	as.logPlans[logID] = planID
}

// LogBelongsToPlan reports whether planID reserved logID. An unreserved log id
// belongs to nobody, so uploads to it are refused.
func (as *ArtifactStore) LogBelongsToPlan(logID int, planID string) bool {
	if planID == "" {
		return false
	}
	as.Mu.RLock()
	defer as.Mu.RUnlock()
	owner, ok := as.logPlans[logID]
	return ok && owner == planID
}

func NewArtifactStoreWithByteStore(dataDir string, byteStore ActionsByteStore) *ArtifactStore {
	store := &ArtifactStore{
		Artifacts:         make(map[int64]*Artifact),
		NextID:            1,
		Caches:            make(map[int64]*CacheEntry),
		CacheIndex:        make(map[string]int64),
		NextCacheID:       1,
		logPlans:          make(map[int]string),
		DataDir:           dataDir,
		ByteStore:         byteStore,
		MaxRepoCacheBytes: defaultMaxRepoCacheBytes,
	}
	store.stageDir = dataDir
	if dataDir == "" {
		if tmp, err := os.MkdirTemp("", "bleephub-artifact-stage-"); err == nil {
			store.stageDir = tmp
		} else {
			store.stageDir = os.TempDir()
		}
	}
	if dataDir != "" {
		store.recoverFromDisk()
	}
	return store
}

// SetPersistence moves Actions artifact/cache metadata onto the durable
// SQLite/dqlite store. Local metadata migrates only when the durable buckets are
// empty; once durable metadata exists it is authoritative, so a stale replica
// cannot overwrite newer shared records.
func (as *ArtifactStore) SetPersistence(p *Persistence) error {
	if p == nil {
		return nil
	}
	artifactRows, err := p.List(ActionsArtifactsBucket)
	if err != nil {
		return fmt.Errorf("load Actions artifact metadata: %w", err)
	}
	cacheRows, err := p.List(ActionsCachesBucket)
	if err != nil {
		return fmt.Errorf("load Actions cache metadata: %w", err)
	}

	as.Mu.Lock()
	defer as.Mu.Unlock()
	as.Persist = p
	if len(artifactRows) == 0 {
		for id, art := range as.Artifacts {
			if !art.Finalized {
				continue
			}
			as.PopulateArtifactDigest(art)
			if err := p.Put(ActionsArtifactsBucket, strconv.FormatInt(id, 10), art); err != nil {
				return fmt.Errorf("migrate Actions artifact %d metadata: %w", id, err)
			}
		}
	} else {
		as.Artifacts = make(map[int64]*Artifact, len(artifactRows))
		for key, raw := range artifactRows {
			var art Artifact
			if err := json.Unmarshal(raw, &art); err != nil {
				return fmt.Errorf("decode Actions artifact %s metadata: %w", key, err)
			}
			if !art.Finalized {
				continue
			}
			if art.Digest == "" {
				as.PopulateArtifactDigest(&art)
				if art.Digest != "" {
					if err := p.Put(ActionsArtifactsBucket, key, &art); err != nil {
						return fmt.Errorf("persist Actions artifact %s digest: %w", key, err)
					}
				}
			}
			as.Artifacts[art.ID] = &art
			if art.ID >= as.NextID {
				as.NextID = art.ID + 1
			}
		}
	}
	if len(cacheRows) == 0 {
		for id, entry := range as.Caches {
			if !entry.Finalized {
				continue
			}
			if err := p.Put(ActionsCachesBucket, strconv.FormatInt(id, 10), entry); err != nil {
				return fmt.Errorf("migrate Actions cache %d metadata: %w", id, err)
			}
		}
	} else {
		as.Caches = make(map[int64]*CacheEntry, len(cacheRows))
		as.CacheIndex = make(map[string]int64, len(cacheRows))
		for key, raw := range cacheRows {
			var entry CacheEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				return fmt.Errorf("decode Actions cache %s metadata: %w", key, err)
			}
			if !entry.Finalized {
				continue
			}
			as.Caches[entry.ID] = &entry
			as.CacheIndex[CacheLookupKey(entry.Repo, entry.Key, entry.Version)] = entry.ID
			if entry.ID >= as.NextCacheID {
				as.NextCacheID = entry.ID + 1
			}
		}
	}
	if highWater, err := p.KeyHighWater(ActionsArtifactsBucket); err != nil {
		return fmt.Errorf("load Actions artifact identifier: %w", err)
	} else if highWater > as.NextID {
		as.NextID = highWater
	}
	if highWater, err := p.KeyHighWater(ActionsCachesBucket); err != nil {
		return fmt.Errorf("load Actions cache identifier: %w", err)
	} else if highWater > as.NextCacheID {
		as.NextCacheID = highWater
	}
	revision, err := p.StateRevision()
	if err != nil {
		return fmt.Errorf("load Actions metadata revision: %w", err)
	}
	as.revision = revision
	return nil
}

// RefreshFromPersistenceIfStale pulls Actions metadata written by another dqlite
// replica into this process. In-flight local uploads are preserved; only
// finalized rows are durable and replaceable.
func (as *ArtifactStore) RefreshFromPersistenceIfStale() error {
	as.Mu.RLock()
	persist := as.Persist
	loadedRevision := as.revision
	as.Mu.RUnlock()
	if persist == nil || persist.OwnedExclusively() {
		return nil
	}
	revision, err := persist.StateRevision()
	if err != nil {
		return fmt.Errorf("read Actions metadata revision: %w", err)
	}
	if revision <= loadedRevision {
		return nil
	}

	as.refreshMu.Lock()
	defer as.refreshMu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		before, err := persist.StateRevision()
		if err != nil {
			return fmt.Errorf("read Actions metadata revision before reload: %w", err)
		}
		artifactRows, err := persist.List(ActionsArtifactsBucket)
		if err != nil {
			return fmt.Errorf("load Actions artifact metadata: %w", err)
		}
		cacheRows, err := persist.List(ActionsCachesBucket)
		if err != nil {
			return fmt.Errorf("load Actions cache metadata: %w", err)
		}
		artifactHighWater, err := persist.KeyHighWater(ActionsArtifactsBucket)
		if err != nil {
			return fmt.Errorf("load Actions artifact identifier: %w", err)
		}
		cacheHighWater, err := persist.KeyHighWater(ActionsCachesBucket)
		if err != nil {
			return fmt.Errorf("load Actions cache identifier: %w", err)
		}
		artifacts, caches, err := decodeDurableActionsMetadata(artifactRows, cacheRows)
		if err != nil {
			return err
		}

		as.Mu.Lock()
		latest, err := persist.StateRevision()
		if err != nil {
			as.Mu.Unlock()
			return fmt.Errorf("verify Actions metadata revision: %w", err)
		}
		if latest != before {
			as.Mu.Unlock()
			continue
		}
		for id, artifact := range as.Artifacts {
			if !artifact.Finalized {
				artifacts[id] = artifact
			}
		}
		for id, cache := range as.Caches {
			if !cache.Finalized {
				caches[id] = cache
			}
		}
		as.Artifacts = artifacts
		as.Caches = caches
		as.CacheIndex = make(map[string]int64, len(caches))
		as.NextID = 1
		as.NextCacheID = 1
		for id := range artifacts {
			if id >= as.NextID {
				as.NextID = id + 1
			}
		}
		for id, cache := range caches {
			if cache.Finalized {
				as.CacheIndex[CacheLookupKey(cache.Repo, cache.Key, cache.Version)] = id
			}
			if id >= as.NextCacheID {
				as.NextCacheID = id + 1
			}
		}
		if artifactHighWater > as.NextID {
			as.NextID = artifactHighWater
		}
		if cacheHighWater > as.NextCacheID {
			as.NextCacheID = cacheHighWater
		}
		as.revision = before
		as.Mu.Unlock()
		return nil
	}
	return errors.New("actions metadata kept changing during three replica snapshot attempts")
}

func decodeDurableActionsMetadata(artifactRows, cacheRows map[string][]byte) (map[int64]*Artifact, map[int64]*CacheEntry, error) {
	artifacts := make(map[int64]*Artifact, len(artifactRows))
	for key, raw := range artifactRows {
		var artifact Artifact
		if err := json.Unmarshal(raw, &artifact); err != nil {
			return nil, nil, fmt.Errorf("decode Actions artifact %s metadata: %w", key, err)
		}
		if artifact.Finalized {
			artifacts[artifact.ID] = &artifact
		}
	}
	caches := make(map[int64]*CacheEntry, len(cacheRows))
	for key, raw := range cacheRows {
		var cache CacheEntry
		if err := json.Unmarshal(raw, &cache); err != nil {
			return nil, nil, fmt.Errorf("decode Actions cache %s metadata: %w", key, err)
		}
		if cache.Finalized {
			caches[cache.ID] = &cache
		}
	}
	return artifacts, caches, nil
}

func (as *ArtifactStore) PopulateArtifactDigest(art *Artifact) {
	if art == nil || art.Digest != "" {
		return
	}
	data := art.Data
	if len(data) == 0 && art.Size > 0 {
		if stored, err := as.readBytes(
			context.Background(),
			ArtifactDataKey(art.ID),
			filepath.Join(as.DataDir, "artifacts", strconv.FormatInt(art.ID, 10), "data"),
		); err == nil {
			data = stored
			art.Data = stored
		}
	}
	if art.Size > 0 && len(data) == 0 {
		return
	}
	digest := sha256.Sum256(data)
	art.Digest = fmt.Sprintf("sha256:%x", digest)
}

func (as *ArtifactStore) ReserveID(bucket string, local *int64) (int64, error) {
	id := *local
	if as.Persist != nil {
		reserved, err := as.Persist.AllocateCounterValue(bucketKeyCounter(bucket), id)
		if err != nil {
			return 0, err
		}
		id = reserved
	}
	*local = id + 1
	return id, nil
}

func (as *ArtifactStore) recoverFromDisk() {
	as.recoverArtifactsFromDisk()
	as.recoverCachesFromDisk()
}

func (as *ArtifactStore) recoverArtifactsFromDisk() {
	artDir := filepath.Join(as.DataDir, "artifacts")
	entries, err := os.ReadDir(artDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}
		metaPath := filepath.Join(artDir, entry.Name(), "meta.json")
		// #nosec G304 -- entry.Name was parsed as a base-10 artifact ID above.
		metaBytes, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var art Artifact
		if err := json.Unmarshal(metaBytes, &art); err != nil {
			continue
		}
		if data, err := as.readBytes(context.Background(), ArtifactDataKey(id), filepath.Join(artDir, entry.Name(), "data")); err == nil {
			art.Data = data
		}
		as.Artifacts[id] = &art
		if id >= as.NextID {
			as.NextID = id + 1
		}
	}
}

func (as *ArtifactStore) recoverCachesFromDisk() {
	cacheDir := filepath.Join(as.DataDir, "caches")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}
		metaPath := filepath.Join(cacheDir, entry.Name(), "meta.json")
		// #nosec G304 -- entry.Name was parsed as a base-10 cache ID above.
		metaBytes, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var cacheEntry CacheEntry
		if err := json.Unmarshal(metaBytes, &cacheEntry); err != nil {
			continue
		}
		if data, err := as.readBytes(context.Background(), CacheDataKey(id), filepath.Join(cacheDir, entry.Name(), "data")); err == nil {
			cacheEntry.Data = data
		}
		as.Caches[id] = &cacheEntry
		as.CacheIndex[CacheLookupKey(cacheEntry.Repo, cacheEntry.Key, cacheEntry.Version)] = id
		if id >= as.NextCacheID {
			as.NextCacheID = id + 1
		}
	}
}

// PersistMeta writes finalized artifact metadata to durable persistence, and to
// the local disk copy when configured.
func (as *ArtifactStore) PersistMeta(art *Artifact) error {
	if as.Persist != nil && art.Finalized {
		if err := as.Persist.Put(ActionsArtifactsBucket, strconv.FormatInt(art.ID, 10), art); err != nil {
			return err
		}
	}
	if as.DataDir == "" {
		return nil
	}
	dir := filepath.Join(as.DataDir, "artifacts", strconv.FormatInt(art.ID, 10))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(art)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o600)
}

func (as *ArtifactStore) WriteArtifactData(ctx context.Context, art *Artifact) error {
	return as.writeBytes(ctx, ArtifactDataKey(art.ID), filepath.Join(as.DataDir, "artifacts", strconv.FormatInt(art.ID, 10), "data"), art.Data)
}

// artifactStagePath is the local scratch file an in-progress chunked upload
// appends to. Staging first, then writing the assembled artifact to its durable
// home exactly once at finalize, avoids rewriting the whole (growing) blob to the
// object store on every chunk — the O(chunks²) byte amplification the naive
// append-and-rewrite caused — and keeps the whole artifact off the heap.
func (as *ArtifactStore) artifactStagePath(id int64) string {
	dir := as.stageDir
	if dir == "" {
		dir = as.DataDir
	}
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "artifacts", strconv.FormatInt(id, 10), "upload.part")
}

// AppendArtifactChunk appends one upload chunk to the artifact's staging file
// (O(chunk), not O(total)). Callers serialize on ArtifactStore.Mu.
func (as *ArtifactStore) AppendArtifactChunk(id int64, chunk []byte) error {
	path := as.artifactStagePath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	// #nosec G304 -- path is the configured/temp root joined with a numeric ID.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(chunk); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// DiscardArtifactStaging removes a staging file for an upload that never
// finalized. Best-effort.
func (as *ArtifactStore) DiscardArtifactStaging(id int64) {
	_ = os.Remove(as.artifactStagePath(id))
}

// FinalizeArtifactUpload moves the staged bytes to the artifact's durable home
// (object store, else the data dir, else memory) exactly once, computing the
// SHA-256 digest, and removes the staging file. It sets art.Size, art.Digest,
// and — for object-backed stores — leaves art.Data nil so the finalized artifact
// does not pin its payload in RAM. Callers hold ArtifactStore.Mu.
func (as *ArtifactStore) FinalizeArtifactUpload(ctx context.Context, art *Artifact) error {
	stage := as.artifactStagePath(art.ID)
	if as.ByteStore != nil {
		// #nosec G304 -- staging path is internal (numeric ID under a fixed root).
		f, err := os.Open(stage)
		if err != nil {
			if os.IsNotExist(err) {
				return as.finalizeEmptyArtifact(ctx, art)
			}
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, f); err != nil {
			return err
		}
		sum := hasher.Sum(nil)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := as.ByteStore.PutStreamHashed(ctx, ArtifactDataKey(art.ID), f, info.Size(), sum); err != nil {
			return err
		}
		art.Data = nil
		art.Size = info.Size()
		art.Digest = "sha256:" + hex.EncodeToString(sum)
		_ = os.Remove(stage)
		return nil
	}
	// Filesystem / in-memory backends: load the staged bytes and write them to
	// their home once (WriteArtifactData → data dir, or kept in art.Data for the
	// memoryless dev server, which reads Data directly).
	// #nosec G304 -- internal staging path.
	data, err := os.ReadFile(stage)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	art.Data = data
	art.Size = int64(len(data))
	sum := sha256.Sum256(data)
	art.Digest = "sha256:" + hex.EncodeToString(sum[:])
	if err := as.WriteArtifactData(ctx, art); err != nil {
		return err
	}
	_ = os.Remove(stage)
	return nil
}

func (as *ArtifactStore) finalizeEmptyArtifact(ctx context.Context, art *Artifact) error {
	sum := sha256.Sum256(nil)
	if err := as.ByteStore.PutStreamHashed(ctx, ArtifactDataKey(art.ID), bytes.NewReader(nil), 0, sum[:]); err != nil {
		return err
	}
	art.Data = nil
	art.Size = 0
	art.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return nil
}

func (as *ArtifactStore) writeCacheData(ctx context.Context, entry *CacheEntry) error {
	return as.writeBytes(ctx, CacheDataKey(entry.ID), filepath.Join(as.DataDir, "caches", strconv.FormatInt(entry.ID, 10), "data"), entry.Data)
}

func (as *ArtifactStore) WriteLogData(ctx context.Context, logID int, data []byte) error {
	return as.writeBytes(ctx, LogDataKey(logID), "", data)
}

// ReleaseLogClaimsForPlans drops the log-container claims held by the given
// plans and reports the released log ids. Only the in-memory claim registry is
// touched; durable byte-store log objects are untouched.
func (as *ArtifactStore) ReleaseLogClaimsForPlans(planIDs []string) []int {
	if len(planIDs) == 0 {
		return nil
	}
	planSet := make(map[string]bool, len(planIDs))
	for _, planID := range planIDs {
		if planID != "" {
			planSet[planID] = true
		}
	}
	var logIDs []int
	as.Mu.Lock()
	for logID, planID := range as.logPlans {
		if planSet[planID] {
			delete(as.logPlans, logID)
			logIDs = append(logIDs, logID)
		}
	}
	as.Mu.Unlock()
	return logIDs
}

func (as *ArtifactStore) DeleteLogData(ctx context.Context, logID int) error {
	// Release the claim so it does not outlive the bytes it guards.
	as.Mu.Lock()
	delete(as.logPlans, logID)
	as.Mu.Unlock()
	if as.ByteStore == nil {
		return nil
	}
	return as.ByteStore.Delete(ctx, LogDataKey(logID))
}

func (as *ArtifactStore) writeBytes(ctx context.Context, objectKey, localPath string, data []byte) error {
	if as.ByteStore != nil {
		if err := as.ByteStore.Put(ctx, objectKey, data); err != nil {
			return err
		}
	}
	if as.DataDir == "" || localPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return err
	}
	return os.WriteFile(localPath, data, 0o600)
}

func (as *ArtifactStore) readBytes(ctx context.Context, objectKey, localPath string) ([]byte, error) {
	if as.ByteStore != nil {
		return as.ByteStore.Get(ctx, objectKey)
	}
	// #nosec G304,G703 -- localPath is assembled internally from the configured
	// artifact root and a numeric store ID, never from request path text.
	return os.ReadFile(localPath)
}

func (as *ArtifactStore) FinalizedArtifacts() []*Artifact {
	as.Mu.RLock()
	defer as.Mu.RUnlock()

	out := make([]*Artifact, 0, len(as.Artifacts))
	for _, art := range as.Artifacts {
		if !art.Finalized {
			continue
		}
		copyArt := *art
		copyArt.Data = append([]byte(nil), art.Data...)
		out = append(out, &copyArt)
	}
	return out
}

func (as *ArtifactStore) ArtifactByID(id int64) (*Artifact, bool) {
	as.Mu.RLock()
	defer as.Mu.RUnlock()

	art, ok := as.Artifacts[id]
	if !ok || !art.Finalized {
		return nil, false
	}
	copyArt := *art
	copyArt.Data = append([]byte(nil), art.Data...)
	return &copyArt, true
}

func (as *ArtifactStore) DeleteArtifact(ctx context.Context, id int64) (bool, error) {
	as.Mu.RLock()
	_, ok := as.Artifacts[id]
	as.Mu.RUnlock()
	if !ok {
		return false, nil
	}
	if ok && as.DataDir != "" {
		if err := os.RemoveAll(filepath.Join(as.DataDir, "artifacts", strconv.FormatInt(id, 10))); err != nil {
			return true, err
		}
	}
	if ok && as.ByteStore != nil {
		if err := as.ByteStore.Delete(ctx, ArtifactDataKey(id)); err != nil {
			return true, err
		}
	}
	if as.Persist != nil {
		if err := as.Persist.Delete(ActionsArtifactsBucket, strconv.FormatInt(id, 10)); err != nil {
			return true, err
		}
	}
	as.Mu.Lock()
	delete(as.Artifacts, id)
	as.Mu.Unlock()
	return true, nil
}

func (as *ArtifactStore) RenameRepository(oldFullName, newFullName string) error {
	as.Mu.Lock()
	defer as.Mu.Unlock()
	for _, art := range as.Artifacts {
		if art.RepoFullName != oldFullName {
			continue
		}
		art.RepoFullName = newFullName
		if err := as.PersistMeta(art); err != nil {
			return fmt.Errorf("persist renamed artifact %d: %w", art.ID, err)
		}
	}
	for _, entry := range as.Caches {
		if entry.Repo != oldFullName {
			continue
		}
		delete(as.CacheIndex, CacheLookupKey(entry.Repo, entry.Key, entry.Version))
		entry.Repo = newFullName
		as.CacheIndex[CacheLookupKey(entry.Repo, entry.Key, entry.Version)] = entry.ID
		if err := as.PersistCacheMeta(entry); err != nil {
			return fmt.Errorf("persist renamed cache %d: %w", entry.ID, err)
		}
	}
	return nil
}

// prepareRepositoryDeletion holds the artifact index stable while the caller
// commits the durable intent. The returned closure stages metadata deletion when
// given a batch, or aborts without mutation when given nil; either way it
// releases the lock.
func (as *ArtifactStore) prepareRepositoryDeletion(repoFullName string, logIDs map[int]bool, record *PendingDeletion) func(*PersistBatch) {
	as.Mu.Lock()
	for id, art := range as.Artifacts {
		if art.RepoFullName != repoFullName {
			continue
		}
		if as.ByteStore != nil {
			record.ActionsObjectKeys = append(record.ActionsObjectKeys, ArtifactDataKey(id))
		}
		if as.DataDir != "" {
			record.ActionsDirectories = append(record.ActionsDirectories,
				filepath.Join(as.DataDir, "artifacts", strconv.FormatInt(id, 10)))
		}
	}
	for id, entry := range as.Caches {
		if entry.Repo != repoFullName {
			continue
		}
		if as.ByteStore != nil {
			record.ActionsObjectKeys = append(record.ActionsObjectKeys, CacheDataKey(id))
		}
		if as.DataDir != "" {
			record.ActionsDirectories = append(record.ActionsDirectories,
				filepath.Join(as.DataDir, "caches", strconv.FormatInt(id, 10)))
		}
	}
	if as.ByteStore != nil {
		for logID := range logIDs {
			record.ActionsObjectKeys = append(record.ActionsObjectKeys, LogDataKey(logID))
		}
	}
	return func(batch *PersistBatch) {
		defer as.Mu.Unlock()
		if batch == nil {
			return
		}
		for id, art := range as.Artifacts {
			if art.RepoFullName != repoFullName {
				continue
			}
			delete(as.Artifacts, id)
			batch.Delete(ActionsArtifactsBucket, strconv.FormatInt(id, 10))
		}
		for id, entry := range as.Caches {
			if entry.Repo != repoFullName {
				continue
			}
			delete(as.CacheIndex, CacheLookupKey(entry.Repo, entry.Key, entry.Version))
			delete(as.Caches, id)
			batch.Delete(ActionsCachesBucket, strconv.FormatInt(id, 10))
		}
		for logID := range logIDs {
			delete(as.logPlans, logID)
		}
	}
}

func (as *ArtifactStore) PersistCacheMeta(entry *CacheEntry) error {
	if as.Persist != nil && entry.Finalized {
		if err := as.Persist.Put(ActionsCachesBucket, strconv.FormatInt(entry.ID, 10), entry); err != nil {
			return err
		}
	}
	if as.DataDir == "" {
		return nil
	}
	dir := filepath.Join(as.DataDir, "caches", strconv.FormatInt(entry.ID, 10))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o600)
}

// WriteCacheDataAt writes a ranged chunk to the cache's on-disk data file at its
// Content-Range offset, for restart recovery (entry.Data is authoritative
// in-process).
func (as *ArtifactStore) WriteCacheDataAt(entry *CacheEntry, chunk []byte, offset int64) error {
	if as.ByteStore != nil {
		return as.writeCacheData(context.Background(), entry)
	}
	return as.WriteCacheChunkToDisk(entry, chunk, offset)
}

// WriteCacheChunkToDisk lands one ranged chunk at its Content-Range offset in
// the cache's local data file. No-op in in-memory mode.
func (as *ArtifactStore) WriteCacheChunkToDisk(entry *CacheEntry, chunk []byte, offset int64) error {
	if as.DataDir == "" {
		return nil
	}
	dir := filepath.Join(as.DataDir, "caches", strconv.FormatInt(entry.ID, 10))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	// #nosec G304 -- the path contains only the configured artifact root and
	// the decimal internal cache ID.
	f, err := os.OpenFile(filepath.Join(dir, "data"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, writeErr := f.WriteAt(chunk, offset); writeErr != nil {
		if closeErr := f.Close(); closeErr != nil {
			return errors.Join(writeErr, closeErr)
		}
		return writeErr
	}
	return f.Close()
}

// FinalizedRepoCaches returns every finalized cache for repo, ordered by id.
func (as *ArtifactStore) FinalizedRepoCaches(repo string) []*CacheEntry {
	as.Mu.RLock()
	defer as.Mu.RUnlock()
	out := make([]*CacheEntry, 0)
	for _, entry := range as.Caches {
		if entry.Repo == repo && entry.Finalized {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (as *ArtifactStore) FindArtifactByNameLocked(name, workflowRunBackendID string, finalized bool) *Artifact {
	var found *Artifact
	for _, art := range as.Artifacts {
		if art.Name != name || art.Finalized != finalized {
			continue
		}
		if workflowRunBackendID != "" && art.WorkflowRunBackendID != workflowRunBackendID {
			continue
		}
		if found == nil || art.ID < found.ID {
			found = art
		}
	}
	return found
}

type Artifact struct {
	ID                   int64     `json:"id"`
	Name                 string    `json:"name"`
	Size                 int64     `json:"size"`
	Data                 []byte    `json:"-"`
	Finalized            bool      `json:"finalized"`
	RunID                string    `json:"runId"`
	GitHubRunID          int       `json:"githubRunId"`
	RepoFullName         string    `json:"repoFullName"`
	WorkflowRunBackendID string    `json:"workflowRunBackendId"`
	CreatedAt            time.Time `json:"createdAt"`
	Digest               string    `json:"digest,omitempty"`
}

// CacheEntry is one immutable Actions dependency cache archive, scoped to the
// repo whose run created it. DownloadToken stands in for GitHub's pre-signed
// archive URL: the toolkit fetches it unauthenticated, so it must be unguessable.
type CacheEntry struct {
	ID             int64     `json:"id"`
	Repo           string    `json:"repo"`
	Key            string    `json:"key"`
	Version        string    `json:"version"`
	Size           int64     `json:"size"`
	Data           []byte    `json:"-"`
	Finalized      bool      `json:"finalized"`
	DownloadToken  string    `json:"downloadToken"`
	CreatedAt      time.Time `json:"createdAt"`
	LastAccessedAt time.Time `json:"lastAccessedAt"`

	// Chunks holds the ranged bodies received for an unfinalized reservation;
	// finalize tiles them into Data. Buffering only what arrived bounds the memory
	// a client can make this server allocate by the bytes it uploaded, not the
	// Content-Range it declared.
	Chunks   []CacheChunk `json:"-"`
	Received int64        `json:"-"`
}

func CacheLookupKey(repo, key, version string) string {
	return repo + "\x00" + key + "\x00" + version
}

// defaultMaxRepoCacheBytes is GitHub's default per-repository Actions cache limit.
const defaultMaxRepoCacheBytes = 10 << 30

// JobMessageScopeAndRepo reads a dispatched job message's plan scopeIdentifier
// and repository. An operator-submitted job carries no repo and yields "".
func JobMessageScopeAndRepo(message string) (scopeID, repo string) {
	var msg struct {
		Plan struct {
			ScopeIdentifier string `json:"scopeIdentifier"`
		} `json:"plan"`
		ContextData struct {
			GitHub struct {
				D []struct {
					K string          `json:"k"`
					V json.RawMessage `json:"v"`
				} `json:"d"`
			} `json:"github"`
		} `json:"contextData"`
	}
	if err := json.Unmarshal([]byte(message), &msg); err != nil {
		return "", ""
	}
	for _, kv := range msg.ContextData.GitHub.D {
		if kv.K != "repository" {
			continue
		}
		if err := json.Unmarshal(kv.V, &repo); err != nil {
			repo = ""
		}
		break
	}
	return msg.Plan.ScopeIdentifier, repo
}

// CacheChunk is one ranged upload body and its start offset.
type CacheChunk struct {
	Start int64  `json:"-"`
	Data  []byte `json:"-"`
}

const (
	ActionsArtifactsBucket = "actions_artifacts"
	ActionsCachesBucket    = "actions_caches"
)
