package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ErrReleaseAssetNameExists reports a duplicate asset name; GitHub 422s it.
var ErrReleaseAssetNameExists = errors.New("release asset name already exists")

// Release is a tagged release on a repo. AuthorID/RepoID carry json names so
// persistence round-trips the linkage; client responses never marshal this
// struct (releaseToJSON emits an explicit map).
type Release struct {
	ID              int             `json:"id"`
	NodeID          string          `json:"node_id"`
	TagName         string          `json:"tag_name"`
	TargetCommitish string          `json:"target_commitish"`
	Name            string          `json:"name"`
	Body            string          `json:"body"`
	Draft           bool            `json:"draft"`
	Prerelease      bool            `json:"prerelease"`
	AuthorID        int             `json:"author_id"`
	RepoID          int             `json:"repo_id"`
	Assets          []*ReleaseAsset `json:"-"`
	CreatedAt       time.Time       `json:"created_at"`
	PublishedAt     *time.Time      `json:"published_at"`
	// ExcludeFromLatest (make_latest:"false") drops the release from Latest().
	// Persisted, but absent from the API response, as on GitHub.
	ExcludeFromLatest bool `json:"exclude_from_latest,omitempty"`
	// DiscussionNumber links to a discussion (via discussion_category_name),
	// zero when none; surfaced as discussion_url.
	DiscussionNumber int `json:"discussion_number,omitempty"`
}

type ReleaseAsset struct {
	ID            int       `json:"id"`
	NodeID        string    `json:"node_id"`
	Name          string    `json:"name"`
	Label         string    `json:"label"`
	State         string    `json:"state"`
	ContentType   string    `json:"content_type"`
	Digest        string    `json:"digest"`
	Size          int       `json:"size"`
	DownloadCount int       `json:"download_count"`
	UploaderID    int       `json:"-"`
	ReleaseID     int       `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type ReleaseStore struct {
	Mu           sync.RWMutex       `json:"-"`
	ByID         map[int]*Release   `json:"-"`
	ByRepo       map[int][]*Release `json:"-"`
	assetByID    map[int]*ReleaseAsset
	assetData    map[int][]byte
	assetDataDir string
	ByteStore    ActionsByteStore `json:"-"`
	NextID       int              `json:"-"`
	nextAsset    int
	Persist      *Persistence `json:"-"`
}

func newReleaseStore(p *Persistence) *ReleaseStore {
	rs := &ReleaseStore{
		ByID:      map[int]*Release{},
		ByRepo:    map[int][]*Release{},
		assetByID: map[int]*ReleaseAsset{},
		assetData: map[int][]byte{},
		NextID:    1,
		nextAsset: 1,
		Persist:   p,
	}
	if d := os.Getenv("BLEEPHUB_DATA_DIR"); d != "" {
		rs.assetDataDir = filepath.Join(d, "release_assets")
	}
	return rs
}

func (rs *ReleaseStore) Create(repoID, authorID int, tagName, target, name, body string, draft, prerelease, excludeFromLatest bool) *Release {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	now := time.Now().UTC()
	id := rs.NextID
	rs.NextID++
	r := &Release{
		ID:                id,
		NodeID:            fmt.Sprintf("RE_kgDO%08d", id),
		TagName:           tagName,
		TargetCommitish:   target,
		Name:              name,
		Body:              body,
		Draft:             draft,
		Prerelease:        prerelease,
		AuthorID:          authorID,
		RepoID:            repoID,
		CreatedAt:         now,
		ExcludeFromLatest: excludeFromLatest,
	}
	if !draft {
		r.PublishedAt = &now
	}
	rs.ByID[id] = r
	rs.ByRepo[repoID] = append(rs.ByRepo[repoID], r)
	if rs.Persist != nil {
		rs.Persist.MustPut("releases", strconv.Itoa(id), r)
	}
	return r
}

// cloneRelease detaches a release from the store (STORE-021); Assets and
// PublishedAt are its only reference fields.
func cloneRelease(r *Release) *Release {
	if r == nil {
		return nil
	}
	clone := *r
	if r.PublishedAt != nil {
		published := *r.PublishedAt
		clone.PublishedAt = &published
	}
	if r.Assets != nil {
		clone.Assets = make([]*ReleaseAsset, len(r.Assets))
		for i, asset := range r.Assets {
			clone.Assets[i] = cloneReleaseAsset(asset)
		}
	}
	return &clone
}

// cloneReleaseAsset detaches one asset; ReleaseAsset is all-value.
func cloneReleaseAsset(a *ReleaseAsset) *ReleaseAsset {
	if a == nil {
		return nil
	}
	clone := *a
	return &clone
}

func (rs *ReleaseStore) Get(id int) *Release {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	return cloneRelease(rs.ByID[id])
}

func (rs *ReleaseStore) GetByTag(repoID int, tag string) *Release {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	for _, r := range rs.ByRepo[repoID] {
		if r.TagName == tag {
			return cloneRelease(r)
		}
	}
	return nil
}

// Latest returns the newest non-draft non-prerelease release.
func (rs *ReleaseStore) Latest(repoID int) *Release {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	var latest *Release
	for _, r := range rs.ByRepo[repoID] {
		if r.Draft || r.Prerelease || r.ExcludeFromLatest {
			continue
		}
		if latest == nil || r.CreatedAt.After(latest.CreatedAt) {
			latest = r
		}
	}
	return cloneRelease(latest)
}

func (rs *ReleaseStore) List(repoID int) []*Release {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	out := make([]*Release, len(rs.ByRepo[repoID]))
	for i, r := range rs.ByRepo[repoID] {
		out[i] = cloneRelease(r)
	}
	// GitHub lists newest-first; id-desc is stable across restarts (the
	// persistence loader rebuilds byRepo in map-iteration order).
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (rs *ReleaseStore) Update(id int, fn func(*Release)) bool {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	r := rs.ByID[id]
	if r == nil {
		return false
	}
	wasDraft := r.Draft
	fn(r)
	if wasDraft && !r.Draft && r.PublishedAt == nil {
		now := time.Now().UTC()
		r.PublishedAt = &now
	}
	if rs.Persist != nil {
		rs.Persist.MustPut("releases", strconv.Itoa(id), r)
	}
	return true
}

// DeleteAllForRepo purges every release for a repo, in memory and on disk, so a
// recreated same-name repo can't inherit them after a restart.
func (rs *ReleaseStore) DeleteAllForRepo(repoID int) error {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	batch := NewPersistBatch(rs.Persist)
	for _, r := range rs.ByRepo[repoID] {
		if err := rs.deleteReleaseBatchLocked(r, batch); err != nil {
			return err
		}
	}
	if err := batch.Commit(); err != nil {
		return fmt.Errorf("persist releases deletion: %w", err)
	}
	delete(rs.ByRepo, repoID)
	return nil
}

// appendRepoCleanup snapshots asset locations into a durable deletion intent so
// the repo cascade can commit metadata without I/O while holding its lock.
func (rs *ReleaseStore) appendRepoCleanup(repoID int, record *PendingDeletion) {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	for _, release := range rs.ByRepo[repoID] {
		for _, asset := range release.Assets {
			switch {
			case rs.ByteStore != nil:
				record.ReleaseAssetObjects = append(record.ReleaseAssetObjects, ReleaseAssetDataKey(asset.ID))
			case rs.assetDataDir != "":
				record.ReleaseAssetFiles = append(record.ReleaseAssetFiles, rs.assetFilePath(asset.ID))
			}
		}
	}
}

// deleteAllForRepoBatch removes release metadata in memory and stages every
// durable row into the cascade's transaction; asset bytes go later.
func (rs *ReleaseStore) deleteAllForRepoBatch(repoID int, batch *PersistBatch) map[int]bool {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	ids := make(map[int]bool, len(rs.ByRepo[repoID]))
	for _, release := range rs.ByRepo[repoID] {
		ids[release.ID] = true
		for _, asset := range release.Assets {
			delete(rs.assetByID, asset.ID)
			delete(rs.assetData, asset.ID)
			batch.Delete("release_assets", strconv.Itoa(asset.ID))
		}
		delete(rs.ByID, release.ID)
		batch.Delete("releases", strconv.Itoa(release.ID))
	}
	delete(rs.ByRepo, repoID)
	return ids
}

func (rs *ReleaseStore) IDsForRepo(repoID int) map[int]bool {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	ids := make(map[int]bool, len(rs.ByRepo[repoID]))
	for _, r := range rs.ByRepo[repoID] {
		ids[r.ID] = true
	}
	return ids
}

// Delete removes a release, its asset rows, and its reactions in one
// transaction, so a crash can't drop the reactions while the release survives
// (STORE-001/002).
func (rs *ReleaseStore) Delete(id int, reactions *ReactionStore) (bool, error) {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	r := rs.ByID[id]
	if r == nil {
		return false, nil
	}
	batch := NewPersistBatch(rs.Persist)
	if err := rs.deleteReleaseBatchLocked(r, batch); err != nil {
		return true, err
	}
	if reactions != nil {
		reactions.DeleteParentsBatch("release", map[int]bool{id: true}, batch)
	}
	if err := batch.Commit(); err != nil {
		return true, fmt.Errorf("persist release deletion: %w", err)
	}
	src := rs.ByRepo[r.RepoID]
	for i, x := range src {
		if x.ID == id {
			rs.ByRepo[r.RepoID] = append(src[:i], src[i+1:]...)
			break
		}
	}
	return true, nil
}

func (rs *ReleaseStore) assetFilePath(id int) string {
	if rs.assetDataDir == "" {
		return ""
	}
	return filepath.Join(rs.assetDataDir, strconv.Itoa(id))
}

func (rs *ReleaseStore) saveAssetDataLocked(id int, data []byte) error {
	if rs.ByteStore != nil {
		return rs.ByteStore.Put(context.Background(), ReleaseAssetDataKey(id), data)
	}
	if rs.assetDataDir != "" {
		if err := os.MkdirAll(rs.assetDataDir, 0o750); err != nil {
			return err
		}
		return os.WriteFile(rs.assetFilePath(id), data, 0o600)
	}
	rs.assetData[id] = data
	return nil
}

func (rs *ReleaseStore) loadAssetDataLocked(id int) ([]byte, bool) {
	if rs.ByteStore != nil {
		data, err := rs.ByteStore.Get(context.Background(), ReleaseAssetDataKey(id))
		if err != nil {
			return nil, false
		}
		return data, true
	}
	if rs.assetDataDir != "" {
		path := rs.assetFilePath(id)
		// #nosec G304 -- assetFilePath appends only the decimal internal ID.
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		return data, true
	}
	data, ok := rs.assetData[id]
	return data, ok
}

func (rs *ReleaseStore) removeAssetDataLocked(id int) error {
	if rs.ByteStore != nil {
		return rs.ByteStore.Delete(context.Background(), ReleaseAssetDataKey(id))
	}
	if rs.assetDataDir != "" {
		if err := os.Remove(rs.assetFilePath(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	delete(rs.assetData, id)
	return nil
}

func (rs *ReleaseStore) deleteAssetLocked(a *ReleaseAsset) error {
	if err := rs.removeAssetDataLocked(a.ID); err != nil {
		return err
	}
	delete(rs.assetByID, a.ID)
	if rel := rs.ByID[a.ReleaseID]; rel != nil {
		src := rel.Assets
		for i, x := range src {
			if x.ID == a.ID {
				rel.Assets = append(src[:i], src[i+1:]...)
				break
			}
		}
	}
	if rs.Persist != nil {
		rs.Persist.MustDelete("release_assets", strconv.Itoa(a.ID))
	}
	return nil
}

// deleteAssetBatchLocked removes an asset's bytes and in-memory state, staging
// its row deletion into batch so a release and all its assets drop in one
// transaction.
func (rs *ReleaseStore) deleteAssetBatchLocked(a *ReleaseAsset, batch *PersistBatch) error {
	if err := rs.removeAssetDataLocked(a.ID); err != nil {
		return err
	}
	delete(rs.assetByID, a.ID)
	if rel := rs.ByID[a.ReleaseID]; rel != nil {
		src := rel.Assets
		for i, x := range src {
			if x.ID == a.ID {
				rel.Assets = append(src[:i], src[i+1:]...)
				break
			}
		}
	}
	batch.Delete("release_assets", strconv.Itoa(a.ID))
	return nil
}

// deleteReleaseBatchLocked stages a release row and its asset rows into batch
// so a crash can't leave the release split from its assets; caller commits.
func (rs *ReleaseStore) deleteReleaseBatchLocked(r *Release, batch *PersistBatch) error {
	// Iterate a copy: deleteAssetBatchLocked mutates r.Assets in place.
	for _, a := range append([]*ReleaseAsset(nil), r.Assets...) {
		if err := rs.deleteAssetBatchLocked(a, batch); err != nil {
			return err
		}
	}
	delete(rs.ByID, r.ID)
	batch.Delete("releases", strconv.Itoa(r.ID))
	return nil
}

func (rs *ReleaseStore) CreateReleaseAsset(releaseID, uploaderID int, name, label, contentType string, data []byte) (*ReleaseAsset, error) {
	if name == "" {
		return nil, fmt.Errorf("name required")
	}
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	if rs.ByID[releaseID] == nil {
		return nil, fmt.Errorf("release not found")
	}
	// A release can't carry two assets with the same name; GitHub 422s.
	for _, existing := range rs.ByID[releaseID].Assets {
		if existing.Name == name {
			return nil, ErrReleaseAssetNameExists
		}
	}
	now := time.Now().UTC()
	id := rs.nextAsset
	rs.nextAsset++
	sum := sha256.Sum256(data)
	asset := &ReleaseAsset{
		ID:          id,
		NodeID:      fmt.Sprintf("RA_kgDO%08d", id),
		ReleaseID:   releaseID,
		Name:        name,
		Label:       label,
		State:       "uploaded",
		ContentType: contentType,
		Digest:      "sha256:" + hex.EncodeToString(sum[:]),
		Size:        len(data),
		UploaderID:  uploaderID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	rs.assetByID[id] = asset
	rs.ByID[releaseID].Assets = append(rs.ByID[releaseID].Assets, asset)
	if err := rs.saveAssetDataLocked(id, data); err != nil {
		_ = rs.deleteAssetLocked(asset)
		return nil, err
	}
	if rs.Persist != nil {
		rs.Persist.MustPut("release_assets", strconv.Itoa(id), asset)
	}
	return asset, nil
}

func (rs *ReleaseStore) GetReleaseAsset(id int) *ReleaseAsset {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	return cloneReleaseAsset(rs.assetByID[id])
}

func (rs *ReleaseStore) GetAssetData(id int) ([]byte, bool) {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	return rs.loadAssetDataLocked(id)
}

func (rs *ReleaseStore) UpdateReleaseAsset(id int, name, label string) *ReleaseAsset {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	a := rs.assetByID[id]
	if a == nil {
		return nil
	}
	if name != "" {
		a.Name = name
	}
	a.Label = label
	a.UpdatedAt = time.Now().UTC()
	if rs.Persist != nil {
		rs.Persist.MustPut("release_assets", strconv.Itoa(id), a)
	}
	return a
}

func (rs *ReleaseStore) DeleteReleaseAsset(id int) (bool, error) {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	a := rs.assetByID[id]
	if a == nil {
		return false, nil
	}
	if err := rs.deleteAssetLocked(a); err != nil {
		return true, err
	}
	return true, nil
}

func (rs *ReleaseStore) ListReleaseAssets(releaseID int) []*ReleaseAsset {
	rs.Mu.RLock()
	defer rs.Mu.RUnlock()
	rel := rs.ByID[releaseID]
	if rel == nil {
		return nil
	}
	out := make([]*ReleaseAsset, len(rel.Assets))
	for i, asset := range rel.Assets {
		out[i] = cloneReleaseAsset(asset)
	}
	return out
}

func (rs *ReleaseStore) IncrementAssetDownloads(id int) bool {
	rs.Mu.Lock()
	defer rs.Mu.Unlock()
	a := rs.assetByID[id]
	if a == nil {
		return false
	}
	a.DownloadCount++
	if rs.Persist != nil {
		rs.Persist.MustPut("release_assets", strconv.Itoa(id), a)
	}
	return true
}

func CoalesceStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
