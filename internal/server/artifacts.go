package bleephub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxCacheEntryBytes is GitHub's per-entry Actions cache limit.
const maxCacheEntryBytes = 10 << 30

// maxArtifactChunkBytes is GitHub's per-artifact size limit, applied to a
// single upload body so a client cannot stream an unbounded one.
const maxArtifactChunkBytes = 10 << 30

// artifactByIDForCaller returns an artifact's metadata regardless of
// finalization, for the access checks that must answer before the body is
// read. Data is left out: only the ownership fields matter here.
func (s *Server) artifactByIDForCaller(id int64) (*Artifact, bool) {
	s.artifactStore.Mu.RLock()
	defer s.artifactStore.Mu.RUnlock()
	art, ok := s.artifactStore.Artifacts[id]
	if !ok {
		return nil, false
	}
	meta := *art
	meta.Data = nil
	return &meta, true
}

func (s *Server) registerArtifactRoutes() {
	// Twirp-style artifact service (JSON over HTTP, @actions/artifact v4).
	// The toolkit calls these with the job's runtime token.
	s.route("POST /twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", s.requireJobToken(s.handleCreateArtifact))
	s.route("POST /twirp/github.actions.results.api.v1.ArtifactService/FinalizeArtifact", s.requireJobToken(s.handleFinalizeArtifact))
	s.route("POST /twirp/github.actions.results.api.v1.ArtifactService/ListArtifacts", s.requireJobToken(s.handleListArtifacts))
	s.route("POST /twirp/github.actions.results.api.v1.ArtifactService/GetSignedArtifactURL", s.requireJobToken(s.handleGetSignedArtifactURL))

	// Artifact upload/download blob endpoints. Download is also where the
	// REST `.../artifacts/{id}/zip` redirect lands, so it additionally accepts
	// a GitHub credential with read access to the owning repository.
	s.route("PUT /_apis/v1/artifacts/{artifactId}/upload", s.requireJobToken(s.handleUploadArtifact))
	s.route("GET /_apis/v1/artifacts/{artifactId}/download", s.handleDownloadArtifact)

	// Actions cache API used by actions/cache. The @actions/cache toolkit
	// reserves at the plural `caches` path (getCacheApiUrl('caches')) and
	// looks up at the singular `cache?keys=`.
	s.route("POST /_apis/artifactcache/caches", s.requireJobToken(s.handleCacheReserve))
	s.route("GET /_apis/artifactcache/cache", s.requireJobToken(s.handleCacheLookup))
	s.route("PATCH /_apis/artifactcache/caches/{cacheId}", s.requireJobToken(s.handleCacheUpload))
	s.route("POST /_apis/artifactcache/caches/{cacheId}", s.requireJobToken(s.handleCacheFinalize))
	// Cache download is the archiveLocation URL the toolkit fetches with an
	// unauthenticated client, exactly as it fetches real GitHub's pre-signed
	// blob URL; the unguessable `sig` query parameter is its credential.
	s.route("GET /_apis/artifactcache/caches/{cacheId}", s.handleCacheDownload)

	// Public GitHub Actions cache REST surface (the `gh` CLI + the
	// actions/github-script management calls hit these). Repo-scoped by the
	// {owner}/{repo} path params, backed by the same CacheEntry store the
	// @actions/cache toolkit writes to.
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/caches", s.handleListRepoCaches)
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/caches",
		s.requirePerm(scopeActions, permWrite, s.handleDeleteRepoCachesByKey))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/caches/{cache_id}",
		s.requirePerm(scopeActions, permWrite, s.handleDeleteRepoCacheByID))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/cache/usage", s.handleRepoCacheUsage)
}

// repoCacheJSON renders a CacheEntry in GitHub's ActionsCacheList item shape.
func repoCacheJSON(entry *CacheEntry) map[string]any {
	created := entry.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
	lastAccessed := entry.LastAccessedAt
	if lastAccessed.IsZero() {
		lastAccessed = entry.CreatedAt
	}
	return map[string]any{
		"id":               entry.ID,
		"ref":              "refs/heads/main",
		"key":              entry.Key,
		"version":          entry.Version,
		"last_accessed_at": lastAccessed.UTC().Format("2006-01-02T15:04:05Z"),
		"created_at":       created,
		"size_in_bytes":    entry.Size,
	}
}

// handleListRepoCaches — GET .../actions/caches.
func (s *Server) handleListRepoCaches(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	repo := repoFullName(r)
	entries := s.artifactStore.FinalizedRepoCaches(repo)
	if key := r.URL.Query().Get("key"); key != "" {
		filtered := entries[:0:0]
		for _, e := range entries {
			if e.Key == key {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	page := paginateAndLink(w, r, entries)
	caches := make([]map[string]any, 0, len(page))
	for _, e := range page {
		caches = append(caches, repoCacheJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":    len(entries),
		"actions_caches": caches,
	})
}

// handleDeleteRepoCachesByKey — DELETE .../actions/caches?key=&ref=.
// GitHub deletes every cache matching the key (optionally narrowed by
// ref) and returns the deleted entries. ref isn't tracked per-entry, so
// it's accepted and matched leniently (bleephub stores one ref).
func (s *Server) handleDeleteRepoCachesByKey(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	key := r.URL.Query().Get("key")
	if key == "" {
		writeGHValidationError(w, "Cache", "key", "missing_field")
		return
	}
	var deleted []*CacheEntry
	s.artifactStore.Mu.Lock()
	for _, entry := range s.artifactStore.Caches {
		if entry.Repo != repo || entry.Key != key {
			continue
		}
		deleted = append(deleted, entry)
	}
	s.artifactStore.Mu.Unlock()
	for _, entry := range deleted {
		if err := s.removeCacheBytes(r.Context(), entry.ID); err != nil {
			writeGHError(w, http.StatusInternalServerError, "cache byte-store delete: "+err.Error())
			return
		}
	}
	s.artifactStore.Mu.Lock()
	for _, entry := range deleted {
		delete(s.artifactStore.Caches, entry.ID)
		delete(s.artifactStore.CacheIndex, cacheLookupKey(entry.Repo, entry.Key, entry.Version))
	}
	s.artifactStore.Mu.Unlock()
	sort.Slice(deleted, func(i, j int) bool { return deleted[i].ID < deleted[j].ID })
	caches := make([]map[string]any, 0, len(deleted))
	for _, e := range deleted {
		caches = append(caches, repoCacheJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":    len(deleted),
		"actions_caches": caches,
	})
}

// handleDeleteRepoCacheByID — DELETE .../actions/caches/{cache_id}.
func (s *Server) handleDeleteRepoCacheByID(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	id, err := strconv.ParseInt(r.PathValue("cache_id"), 10, 64)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid cache_id")
		return
	}
	s.artifactStore.Mu.Lock()
	entry := s.artifactStore.Caches[id]
	if entry == nil || entry.Repo != repo {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.artifactStore.Mu.Unlock()
	if err := s.removeCacheBytes(r.Context(), id); err != nil {
		writeGHError(w, http.StatusInternalServerError, "cache byte-store delete: "+err.Error())
		return
	}
	s.artifactStore.Mu.Lock()
	delete(s.artifactStore.Caches, id)
	delete(s.artifactStore.CacheIndex, cacheLookupKey(entry.Repo, entry.Key, entry.Version))
	s.artifactStore.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleRepoCacheUsage — GET .../actions/cache/usage.
func (s *Server) handleRepoCacheUsage(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRepoReadable(w, r) {
		return
	}
	repo := repoFullName(r)
	entries := s.artifactStore.FinalizedRepoCaches(repo)
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"full_name":                   repo,
		"active_caches_size_in_bytes": total,
		"active_caches_count":         len(entries),
	})
}

// removeCacheFromDisk deletes a cache's on-disk copy. No-op in in-memory mode.
// evictRepoCacheOverLimit enforces the per-repository cache budget after a
// finalize. When a repo's finalized caches exceed maxRepoCacheBytes it deletes
// least-recently-used entries (oldest LastAccessedAt first, id breaking ties)
// until the repo is back under budget — GitHub's cache eviction policy. Byte
// deletes run outside the lock, matching the delete handlers' pattern.
func (s *Server) evictRepoCacheOverLimit(ctx context.Context, repo string) {
	as := s.artifactStore
	as.Mu.Lock()
	budget := as.MaxRepoCacheBytes
	if budget <= 0 {
		as.Mu.Unlock()
		return
	}
	var total int64
	entries := make([]*CacheEntry, 0)
	for _, e := range as.Caches {
		if e.Repo == repo && e.Finalized {
			total += e.Size
			entries = append(entries, e)
		}
	}
	if total <= budget {
		as.Mu.Unlock()
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].LastAccessedAt.Equal(entries[j].LastAccessedAt) {
			return entries[i].LastAccessedAt.Before(entries[j].LastAccessedAt)
		}
		return entries[i].ID < entries[j].ID
	})
	victims := make([]*CacheEntry, 0)
	for _, e := range entries {
		if total <= budget {
			break
		}
		victims = append(victims, e)
		total -= e.Size
	}
	as.Mu.Unlock()

	for _, e := range victims {
		if err := s.removeCacheBytes(ctx, e.ID); err != nil {
			s.logger.Warn().Err(err).Int64("id", e.ID).Str("repo", repo).Msg("evict over-budget repo cache")
			continue
		}
		as.Mu.Lock()
		delete(as.Caches, e.ID)
		delete(as.CacheIndex, cacheLookupKey(e.Repo, e.Key, e.Version))
		as.Mu.Unlock()
	}
}

func (s *Server) removeCacheBytes(ctx context.Context, id int64) error {
	if s.artifactStore.DataDir != "" {
		if err := os.RemoveAll(filepath.Join(s.artifactStore.DataDir, "caches", strconv.FormatInt(id, 10))); err != nil {
			return err
		}
	}
	if s.artifactStore.ByteStore != nil {
		if err := s.artifactStore.ByteStore.Delete(ctx, cacheDataKey(id)); err != nil {
			return err
		}
	}
	if s.artifactStore.Persist != nil {
		if err := s.artifactStore.Persist.Delete(actionsCachesBucket, strconv.FormatInt(id, 10)); err != nil {
			return err
		}
	}
	return nil
}

// --- Artifact Twirp handlers ---

// runArtifactScope resolves the workflow run an artifact call names and
// checks it against the caller's job scope. An absent or unknown run id is an
// error: it must never widen the call to every run on the instance.
func (s *Server) runArtifactScope(w http.ResponseWriter, r *http.Request, backendID string) (*Workflow, bool) {
	if backendID == "" {
		writeGHError(w, http.StatusBadRequest, "workflow_run_backend_id is required")
		return nil, false
	}
	wf := s.findWorkflowByBackendID(backendID)
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	caller, err := s.callerRunner(r)
	if err != nil {
		s.challengeRunnerAuth(w, r, err)
		return nil, false
	}
	if !caller.Scope.CoversRepo(wf.RepoFullName) {
		writeGHError(w, http.StatusForbidden, "Not entitled to this workflow run")
		return nil, false
	}
	return wf, true
}

func (s *Server) handleCreateArtifact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
		WorkflowRunBackendIDAlt string `json:"workflowRunBackendId"`
		Name                    string `json:"name"`
		Version                 int    `json:"version"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.WorkflowRunBackendID = coalesceStr(req.WorkflowRunBackendID, req.WorkflowRunBackendIDAlt)

	wf, ok := s.runArtifactScope(w, r, req.WorkflowRunBackendID)
	if !ok {
		return
	}
	repoFullName := wf.RepoFullName
	githubRunID := wf.RunID

	s.artifactStore.Mu.Lock()
	id, err := s.artifactStore.ReserveID(actionsArtifactsBucket, &s.artifactStore.NextID)
	if err != nil {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusInternalServerError, "reserve artifact identifier: "+err.Error())
		return
	}
	art := &Artifact{
		ID:                   id,
		Name:                 req.Name,
		RunID:                req.WorkflowRunBackendID,
		GitHubRunID:          githubRunID,
		RepoFullName:         repoFullName,
		WorkflowRunBackendID: req.WorkflowRunBackendID,
		CreatedAt:            time.Now(),
	}
	s.artifactStore.Artifacts[id] = art
	if err := s.artifactStore.PersistMeta(art); err != nil {
		delete(s.artifactStore.Artifacts, id)
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusInternalServerError, "persist artifact metadata: "+err.Error())
		return
	}
	s.artifactStore.Mu.Unlock()

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	uploadURL := fmt.Sprintf("%s://%s/_apis/v1/artifacts/%d/upload", scheme, r.Host, id)

	s.logger.Debug().Str("name", req.Name).Int64("id", id).Msg("artifact created")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":                true,
		"signed_upload_url": uploadURL,
	})
}

func (s *Server) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("artifactId")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid artifact ID", http.StatusBadRequest)
		return
	}

	caller, err := s.callerRunner(r)
	if err != nil {
		s.challengeRunnerAuth(w, r, err)
		return
	}
	art, ok := s.artifactByIDForCaller(id)
	if !ok || !caller.Scope.CoversRepo(art.RepoFullName) {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxArtifactChunkBytes))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.artifactStore.Mu.Lock()
	art, ok = s.artifactStore.Artifacts[id]
	if ok {
		if int64(len(art.Data))+int64(len(data)) > maxArtifactChunkBytes {
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("artifact exceeds the %d byte limit", int64(maxArtifactChunkBytes)))
			return
		}
		previousSize := len(art.Data)
		art.Data = append(art.Data, data...)
		art.Size = int64(len(art.Data))
		if err := s.artifactStore.WriteArtifactData(r.Context(), art); err != nil {
			art.Data = art.Data[:previousSize]
			art.Size = int64(previousSize)
			s.artifactStore.Mu.Unlock()
			http.Error(w, "artifact byte-store write: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.artifactStore.PersistMeta(art); err != nil {
			art.Data = art.Data[:previousSize]
			art.Size = int64(previousSize)
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusInternalServerError, "persist artifact upload metadata: "+err.Error())
			return
		}
	}
	s.artifactStore.Mu.Unlock()

	if !ok {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}

	s.logger.Debug().Int64("id", id).Int("bytes", len(data)).Msg("artifact chunk uploaded")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleFinalizeArtifact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                    string `json:"name"`
		Size                    int64  `json:"size"`
		WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
		WorkflowRunBackendIDAlt string `json:"workflowRunBackendId"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	workflowRunBackendID := coalesceStr(req.WorkflowRunBackendID, req.WorkflowRunBackendIDAlt)
	if _, ok := s.runArtifactScope(w, r, workflowRunBackendID); !ok {
		return
	}

	s.artifactStore.Mu.Lock()
	found := s.artifactStore.FindArtifactByNameLocked(req.Name, workflowRunBackendID, false)
	if found != nil {
		if req.Size < 0 || req.Size != found.Size {
			actual := found.Size
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusBadRequest, fmt.Sprintf("artifact size %d does not match %d bytes uploaded", req.Size, actual))
			return
		}
		found.Finalized = true
		digest := sha256.Sum256(found.Data)
		found.Digest = fmt.Sprintf("sha256:%x", digest)
		if err := s.artifactStore.PersistMeta(found); err != nil {
			found.Finalized = false
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusInternalServerError, "persist artifact metadata: "+err.Error())
			return
		}
	}
	s.artifactStore.Mu.Unlock()

	if found == nil {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}

	s.logger.Debug().Str("name", req.Name).Int64("id", found.ID).Msg("artifact finalized")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":          true,
		"artifact_id": found.ID,
	})
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	// The @actions/artifact v4 client scopes ListArtifacts to its own run via
	// workflow_run_backend_id, and so must this handler: an absent run id is a
	// bad request, never a listing of every artifact on the instance.
	var req struct {
		WorkflowRunBackendID string `json:"workflow_run_backend_id"`
		NameFilter           *struct {
			Value string `json:"value"`
		} `json:"name_filter"`
		IDFilter *struct {
			Value string `json:"value"`
		} `json:"id_filter"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if _, ok := s.runArtifactScope(w, r, req.WorkflowRunBackendID); !ok {
		return
	}

	s.artifactStore.Mu.RLock()
	var list []map[string]interface{}
	for _, art := range s.artifactStore.Artifacts {
		if !art.Finalized {
			continue
		}
		if art.WorkflowRunBackendID != req.WorkflowRunBackendID {
			continue
		}
		if req.NameFilter != nil && req.NameFilter.Value != "" && art.Name != req.NameFilter.Value {
			continue
		}
		if req.IDFilter != nil && req.IDFilter.Value != "" && strconv.FormatInt(art.ID, 10) != req.IDFilter.Value {
			continue
		}
		list = append(list, map[string]interface{}{
			"name":        art.Name,
			"id":          art.ID,
			"size":        art.Size,
			"created_at":  art.CreatedAt.UTC().Format(time.RFC3339),
			"database_id": art.ID,
		})
	}
	s.artifactStore.Mu.RUnlock()

	if list == nil {
		list = []map[string]interface{}{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"artifacts": list,
	})
}

func (s *Server) handleGetSignedArtifactURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                    string `json:"name"`
		WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
		WorkflowRunBackendIDAlt string `json:"workflowRunBackendId"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	workflowRunBackendID := coalesceStr(req.WorkflowRunBackendID, req.WorkflowRunBackendIDAlt)
	if _, ok := s.runArtifactScope(w, r, workflowRunBackendID); !ok {
		return
	}

	s.artifactStore.Mu.RLock()
	found := s.artifactStore.FindArtifactByNameLocked(req.Name, workflowRunBackendID, true)
	s.artifactStore.Mu.RUnlock()

	if found == nil {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	downloadURL := fmt.Sprintf("%s://%s/_apis/v1/artifacts/%d/download", scheme, r.Host, found.ID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":       found.Name,
		"signed_url": downloadURL,
	})
}

// mayReadArtifact resolves the repository an artifact belongs to and answers
// whether this caller may read it. A job runtime token reads only its own
// repository's artifacts; every other caller is resolved to a GitHub identity
// and needs read access on the owning repository, so an anonymous caller gets
// public repositories only. An artifact with no owning repository is
// unreadable rather than public — a sequential id must never be a wildcard.
func (s *Server) mayReadArtifact(r *http.Request, art *Artifact) bool {
	if art.RepoFullName == "" {
		return false
	}
	if _, ok := bearerCredential(r); ok {
		if principal, err := s.authenticateRunner(r); err == nil {
			return principal.Scope.CoversRepo(art.RepoFullName)
		}
	}
	repo := s.store.GetRepoByFullName(art.RepoFullName)
	if repo == nil {
		return false
	}
	return s.viewerCanReadRepo(s.authenticateRequest(r), repo)
}

func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("artifactId")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid artifact ID", http.StatusBadRequest)
		return
	}

	s.artifactStore.Mu.RLock()
	art, ok := s.artifactStore.Artifacts[id]
	s.artifactStore.Mu.RUnlock()

	if !ok || !art.Finalized {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	if !s.mayReadArtifact(r, art) {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	data := art.Data
	if len(data) == 0 && art.Size > 0 && s.artifactStore.ByteStore != nil {
		var err error
		data, err = s.artifactStore.ByteStore.Get(r.Context(), artifactDataKey(art.ID))
		if err != nil {
			http.Error(w, "artifact byte-store read: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// --- Actions cache ---

func (s *Server) handleCacheReserve(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.cacheScopeRepo(w, r)
	if !ok {
		return
	}
	var req struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Key == "" || req.Version == "" {
		writeGHValidationError(w, "Cache", "key", "missing_field")
		return
	}

	s.artifactStore.Mu.Lock()
	if id, ok := s.artifactStore.CacheIndex[cacheLookupKey(repo, req.Key, req.Version)]; ok {
		entry := s.artifactStore.Caches[id]
		s.artifactStore.Mu.Unlock()
		if entry != nil && entry.Finalized {
			writeGHError(w, http.StatusConflict, "Cache already exists")
			return
		}
		writeGHError(w, http.StatusConflict, "Cache reservation already exists")
		return
	}
	downloadToken, err := newCacheDownloadToken()
	if err != nil {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := s.artifactStore.ReserveID(actionsCachesBucket, &s.artifactStore.NextCacheID)
	if err != nil {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusInternalServerError, "reserve cache identifier: "+err.Error())
		return
	}
	entry := &CacheEntry{
		ID:             id,
		Repo:           repo,
		Key:            req.Key,
		Version:        req.Version,
		DownloadToken:  downloadToken,
		CreatedAt:      time.Now(),
		LastAccessedAt: time.Now(),
	}
	s.artifactStore.Caches[id] = entry
	s.artifactStore.CacheIndex[cacheLookupKey(repo, req.Key, req.Version)] = id
	if err := s.artifactStore.PersistCacheMeta(entry); err != nil {
		delete(s.artifactStore.Caches, id)
		delete(s.artifactStore.CacheIndex, cacheLookupKey(repo, req.Key, req.Version))
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusInternalServerError, "persist cache metadata: "+err.Error())
		return
	}
	s.artifactStore.Mu.Unlock()

	s.logger.Debug().Int64("id", id).Str("repo", repo).Str("key", req.Key).Msg("cache reserved")
	writeJSON(w, http.StatusOK, map[string]interface{}{"cacheId": id})
}

func (s *Server) handleCacheLookup(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.cacheScopeRepo(w, r)
	if !ok {
		return
	}
	version := r.URL.Query().Get("version")
	keys := splitCacheKeys(r.URL.Query().Get("keys"))
	if version == "" || len(keys) == 0 {
		writeGHValidationError(w, "Cache", "keys", "missing_field")
		return
	}

	s.artifactStore.Mu.Lock()
	entry := s.lookupFinalizedCacheLocked(repo, keys, version)
	if entry != nil {
		entry.LastAccessedAt = time.Now().UTC()
		if err := s.artifactStore.PersistCacheMeta(entry); err != nil {
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusInternalServerError, "persist cache access metadata: "+err.Error())
			return
		}
	}
	s.artifactStore.Mu.Unlock()
	if entry == nil {
		s.logger.Debug().Str("repo", repo).Strs("keys", keys).Str("version", version).Msg("cache lookup miss")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	archiveURL := fmt.Sprintf("%s://%s/_apis/artifactcache/caches/%d?sig=%s", scheme, r.Host, entry.ID, entry.DownloadToken)
	s.logger.Debug().Int64("id", entry.ID).Str("key", entry.Key).Msg("cache lookup hit")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"archiveLocation": archiveURL,
		"cacheKey":        entry.Key,
	})
}

func (s *Server) lookupFinalizedCacheLocked(repo string, keys []string, version string) *CacheEntry {
	for _, key := range keys {
		if id, ok := s.artifactStore.CacheIndex[cacheLookupKey(repo, key, version)]; ok {
			entry := s.artifactStore.Caches[id]
			if entry != nil && entry.Finalized {
				return entry
			}
		}
	}
	for _, key := range keys {
		var newest *CacheEntry
		for _, entry := range s.artifactStore.Caches {
			if entry.Repo != repo || entry.Version != version || !entry.Finalized || !strings.HasPrefix(entry.Key, key) {
				continue
			}
			if newest == nil || entry.CreatedAt.After(newest.CreatedAt) {
				newest = entry
			}
		}
		if newest != nil {
			return newest
		}
	}
	return nil
}

func (s *Server) handleCacheUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := parseCacheID(w, r)
	if !ok {
		return
	}
	repo, ok := s.cacheScopeRepo(w, r)
	if !ok {
		return
	}
	start, end, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, err.Error())
		return
	}
	declared := end - start + 1
	if end+1 > maxCacheEntryBytes {
		writeGHError(w, http.StatusBadRequest, fmt.Sprintf("Content-Range end %d exceeds the %d byte cache entry limit", end, int64(maxCacheEntryBytes)))
		return
	}
	// The declared range bounds the read: a body longer than the range it
	// claims is rejected by the length check below rather than buffered.
	chunk, err := io.ReadAll(http.MaxBytesReader(w, r.Body, declared+1))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(chunk)) != declared {
		writeGHError(w, http.StatusBadRequest, fmt.Sprintf("Content-Range bytes %d-%d does not match body length %d", start, end, len(chunk)))
		return
	}

	s.artifactStore.Mu.Lock()
	entry := s.artifactStore.Caches[id]
	if entry == nil || entry.Repo != repo {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Cache not found")
		return
	}
	if entry.Finalized {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusConflict, "Cache already finalized")
		return
	}
	if entry.Received+declared > maxCacheEntryBytes {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusBadRequest, fmt.Sprintf("cache entry exceeds the %d byte limit", int64(maxCacheEntryBytes)))
		return
	}
	entry.Chunks = append(entry.Chunks, cacheChunk{Start: start, Data: chunk})
	entry.Received += declared
	previousSize := entry.Size
	if end+1 > entry.Size {
		entry.Size = end + 1
	}
	if err := s.artifactStore.WriteCacheChunkToDisk(entry, chunk, start); err != nil {
		entry.Chunks = entry.Chunks[:len(entry.Chunks)-1]
		entry.Received -= declared
		entry.Size = previousSize
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusInternalServerError, "cache byte-store write: "+err.Error())
		return
	}
	if err := s.artifactStore.PersistCacheMeta(entry); err != nil {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusInternalServerError, "persist cache metadata: "+err.Error())
		return
	}
	s.artifactStore.Mu.Unlock()

	s.logger.Debug().Int64("id", id).Int64("start", start).Int64("end", end).Msg("cache chunk uploaded")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCacheFinalize(w http.ResponseWriter, r *http.Request) {
	id, ok := parseCacheID(w, r)
	if !ok {
		return
	}
	repo, ok := s.cacheScopeRepo(w, r)
	if !ok {
		return
	}
	var req struct {
		Size int64 `json:"size"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}

	s.artifactStore.Mu.Lock()
	entry := s.artifactStore.Caches[id]
	if entry == nil || entry.Repo != repo {
		s.artifactStore.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Cache not found")
		return
	}
	if !entry.Finalized {
		// The declared size may not exceed the bytes that actually arrived —
		// that is what bounds the buffer assembleCacheChunks allocates.
		// Overlapping chunks make `received` larger than the archive, never
		// smaller.
		if req.Size > entry.Received || req.Size != entry.Size {
			uploaded, ranged := entry.Received, entry.Size
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusBadRequest, fmt.Sprintf("Cache size %d does not match %d bytes uploaded across ranges ending at %d", req.Size, uploaded, ranged))
			return
		}
		data, err := assembleCacheChunks(entry.Chunks, req.Size)
		if err != nil {
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusBadRequest, err.Error())
			return
		}
		entry.Data = data
		entry.Size = req.Size
		entry.Chunks = nil
		if err := s.artifactStore.WriteCacheDataAt(entry, entry.Data, 0); err != nil {
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusInternalServerError, "cache byte-store write: "+err.Error())
			return
		}
		entry.Finalized = true
		if err := s.artifactStore.PersistCacheMeta(entry); err != nil {
			entry.Finalized = false
			s.artifactStore.Mu.Unlock()
			writeGHError(w, http.StatusInternalServerError, "persist cache metadata: "+err.Error())
			return
		}
	}
	s.artifactStore.Mu.Unlock()

	// Enforce the per-repository cache budget, evicting LRU entries if this
	// finalize pushed the repo over it.
	s.evictRepoCacheOverLimit(r.Context(), repo)

	s.logger.Debug().Int64("id", id).Int64("size", entry.Size).Msg("cache finalized")
	w.WriteHeader(http.StatusOK)
}

// handleCacheDownload serves the archiveLocation URL handed out by lookup.
// The cache toolkit fetches it without the runtime token (on real GitHub it
// is a pre-signed blob URL), so access is gated by the unguessable sig query
// parameter instead of bearer auth.
func (s *Server) handleCacheDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := parseCacheID(w, r)
	if !ok {
		return
	}

	s.artifactStore.Mu.RLock()
	entry := s.artifactStore.Caches[id]
	s.artifactStore.Mu.RUnlock()
	if entry == nil || !entry.Finalized {
		writeGHError(w, http.StatusNotFound, "Cache not found")
		return
	}
	if sig := r.URL.Query().Get("sig"); entry.DownloadToken == "" || !secretEqual(sig, entry.DownloadToken) {
		writeGHError(w, http.StatusNotFound, "Cache not found")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	data := entry.Data
	if len(data) == 0 && entry.Size > 0 && s.artifactStore.ByteStore != nil {
		var err error
		data, err = s.artifactStore.ByteStore.Get(r.Context(), cacheDataKey(entry.ID))
		if err != nil {
			writeGHError(w, http.StatusInternalServerError, "cache byte-store read: "+err.Error())
			return
		}
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// assembleCacheChunks lays the received chunks into one buffer of exactly
// size bytes. Chunks may arrive in any order and may overlap — the toolkit
// uploads them concurrently — but they must cover [0, size) with no hole: a
// gap would mean serving back an archive padded with zeroes the client never
// uploaded, so it is an error instead.
func assembleCacheChunks(chunks []cacheChunk, size int64) ([]byte, error) {
	ordered := append([]cacheChunk(nil), chunks...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Start < ordered[j].Start })
	var covered int64
	for _, c := range ordered {
		if c.Start > covered {
			return nil, fmt.Errorf("cache archive has a hole at byte %d: the next chunk starts at %d", covered, c.Start)
		}
		if end := c.Start + int64(len(c.Data)); end > covered {
			covered = end
		}
	}
	if covered != size {
		return nil, fmt.Errorf("cache chunks cover %d bytes, declared size is %d", covered, size)
	}
	data := make([]byte, size)
	for _, c := range ordered {
		copy(data[c.Start:], c.Data)
	}
	return data, nil
}

func newCacheDownloadToken() (string, error) {
	return newCacheDownloadTokenFromReader(rand.Reader)
}

func newCacheDownloadTokenFromReader(random io.Reader) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("generate cache download token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// cacheScopeRepo resolves the repository an Actions cache request acts for.
// The @actions/cache toolkit authenticates every cache call with the job's
// runtime token, which requireJobToken has already verified; the principal's
// scope is the repository the dispatched job runs as.
func (s *Server) cacheScopeRepo(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller, err := s.callerRunner(r)
	if err != nil || caller.Scope.Repo == "" {
		s.logger.Debug().Err(err).Str("path", r.URL.Path).Msg("cache request rejected")
		writeGHError(w, http.StatusUnauthorized, "Must authenticate to access cache")
		return "", false
	}
	return caller.Scope.Repo, true
}

// repoForJobScope maps a verified job runtime token's sub — the plan
// scopeIdentifier of a dispatched job — to the repository that job runs as.
// The plan must exist: a token naming no plan is a token for nothing.
//
// An operator-submitted job (/internal/exec/submit) names no repository, and
// answers "" rather than an error. That empty scope is the narrowest one
// there is, not a wildcard — runnerScope.coversRepo is false for every
// repository — so such a token reaches its own plan and nothing repository
// scoped. Failing here instead left those jobs unable to report a timeline,
// a log line or their own completion.
func (s *Server) repoForJobScope(scopeID string) (string, error) {
	if scopeID == "" {
		return "", fmt.Errorf("job token carries no plan scope")
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	// Dispatch-time record first: O(1), no message parse, and it outlives the
	// GC of the secret-bearing job message.
	if planID, ok := s.store.PlanIDByScope[scopeID]; ok {
		if ps, ok := s.store.PlanScopes[planID]; ok && ps.ScopeID == scopeID {
			return ps.Repo, nil
		}
	}
	// Fallback for jobs seeded outside the dispatch path (tests): parse each
	// job's message.
	for _, job := range s.store.Jobs {
		if plan, repo := jobMessageScopeAndRepo(job.Message); plan != "" && plan == scopeID {
			return repo, nil
		}
	}
	return "", fmt.Errorf("no job with plan scope %q", scopeID)
}

// parseContentRange parses the "bytes <start>-<end>/<total>" header the
// @actions/cache toolkit sends on every ranged chunk PATCH (total is "*").
func parseContentRange(header string) (start, end int64, err error) {
	if header == "" {
		return 0, 0, fmt.Errorf("Content-Range header is required")
	}
	spec, ok := strings.CutPrefix(header, "bytes ")
	if !ok {
		return 0, 0, fmt.Errorf("invalid Content-Range %q: expected bytes <start>-<end>/<total>", header)
	}
	spec, _, _ = strings.Cut(spec, "/")
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("invalid Content-Range %q: expected bytes <start>-<end>/<total>", header)
	}
	start, err = strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid Content-Range start %q", startStr)
	}
	end, err = strconv.ParseInt(endStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid Content-Range end %q", endStr)
	}
	if start < 0 || end < start {
		return 0, 0, fmt.Errorf("invalid Content-Range %d-%d", start, end)
	}
	return start, end, nil
}

func splitCacheKeys(raw string) []string {
	parts := strings.Split(raw, ",")
	keys := make([]string, 0, len(parts))
	for _, part := range parts {
		key := strings.TrimSpace(part)
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func parseCacheID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("cacheId"), 10, 64)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid cache id")
		return 0, false
	}
	return id, true
}
