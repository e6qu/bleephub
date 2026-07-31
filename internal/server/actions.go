package bleephub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// ActionCache stores downloaded action tarballs in memory.
type ActionCache struct {
	mu      sync.RWMutex
	entries map[string]*ActionCacheEntry
	order   []string
}

const maxActionCacheEntries = 128

type ActionCacheEntry struct {
	Data        []byte
	ResolvedSha string
	FetchedAt   time.Time
}

func NewActionCache() *ActionCache {
	return &ActionCache{
		entries: make(map[string]*ActionCacheEntry),
	}
}

func (ac *ActionCache) Get(key string) *ActionCacheEntry {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	entry := ac.entries[key]
	if entry == nil {
		return nil
	}
	clone := *entry
	clone.Data = append([]byte(nil), entry.Data...)
	return &clone
}

func (ac *ActionCache) Put(key string, entry *ActionCacheEntry) {
	if entry == nil {
		return
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if _, exists := ac.entries[key]; !exists {
		ac.order = append(ac.order, key)
	}
	clone := *entry
	clone.Data = append([]byte(nil), entry.Data...)
	ac.entries[key] = &clone
	for len(ac.entries) > maxActionCacheEntries {
		oldest := ac.order[0]
		ac.order = ac.order[1:]
		delete(ac.entries, oldest)
	}
}

func (s *Server) registerActionRoutes() {
	// Tarball proxy — serves cached action tarballs. This streams repository
	// content, so it takes the job's runtime token and resolves the action
	// repository's visibility before it serves a byte. The runner presents
	// that token as basic auth here, which is why the gate is not the plain
	// bearer one the rest of the runner protocol uses.
	s.route("GET /_apis/v1/actions/tarball/{owner}/{repo}/{ref...}", s.requireActionArchiveToken(s.handleActionTarball))
}

// handleActionDownloadInfo returns tarball URLs for requested actions, each
// with the credential the runner is to present when it fetches one.
//
// The archive download is made by a plain HTTP client that has no credential
// of its own: it sends the token named here, and nothing at all when the
// response names none. So the answer carries a job runtime token for the plan
// the caller already authenticated as, with the expiry that token really has.
func (s *Server) handleActionDownloadInfo(w http.ResponseWriter, r *http.Request) {
	serverURL := s.baseURL(r)

	caller, err := s.callerRunner(r)
	if err != nil || !caller.IsJobToken() {
		writeGHError(w, http.StatusForbidden, "This route requires a job runtime token")
		return
	}
	downloadToken := makeJWT(caller.Claims.Sub, runnerAudJob)
	downloadExpiry := s.currentTime().Add(runnerTokenTTL).Format(time.RFC3339)

	var body struct {
		Actions []struct {
			NameWithOwner string `json:"nameWithOwner"`
			Ref           string `json:"ref"`
		} `json:"actions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.logger.Debug().Err(err).Msg("action download info: malformed body")
		writeGHError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	if len(body.Actions) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"actions": map[string]interface{}{},
		})
		return
	}

	actions := make(map[string]interface{}, len(body.Actions))
	for _, a := range body.Actions {
		if a.NameWithOwner == "" || a.Ref == "" {
			writeGHError(w, http.StatusBadRequest, "Each action requires nameWithOwner and ref")
			return
		}
		key := a.NameWithOwner + "@" + a.Ref

		resolvedSha := s.resolveActionSha(a.NameWithOwner, a.Ref)
		if resolvedSha == "0000000000000000000000000000000000000000" {
			writeGHError(w, http.StatusUnprocessableEntity,
				"action "+key+" is not resolvable from bleephub git storage")
			return
		}

		tarballURL := fmt.Sprintf("%s/_apis/v1/actions/tarball/%s/%s",
			serverURL, a.NameWithOwner, a.Ref)
		zipballURL := tarballURL // runner uses tarball, but we provide both

		actions[key] = map[string]interface{}{
			"nameWithOwner":         a.NameWithOwner,
			"resolvedNameWithOwner": a.NameWithOwner,
			"resolvedSha":           resolvedSha,
			"ref":                   a.Ref,
			"tarballUrl":            tarballURL,
			"zipballUrl":            zipballURL,
			"authentication": map[string]interface{}{
				"expiresAt": downloadExpiry,
				"token":     downloadToken,
			},
		}
	}

	s.logger.Debug().Int("count", len(actions)).Msg("action download info")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"actions": actions,
	})
}

func (s *Server) resolveActionSha(nameWithOwner, ref string) string {
	const zeroSha = "0000000000000000000000000000000000000000"
	parts := strings.Split(nameWithOwner, "/")
	if len(parts) != 2 {
		return zeroSha
	}
	stor := s.store.GetGitStorage(parts[0], parts[1])
	if stor == nil {
		return zeroSha
	}
	if sha := resolveActionRefSha(stor, ref); sha != zeroSha {
		return sha
	}
	return zeroSha
}

// handleActionTarball serves a cached action tarball or builds it from a
// bleephub-hosted repository. Repositories absent from bleephub fail loudly.
func (s *Server) handleActionTarball(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	ref := r.PathValue("ref")

	if owner == "" || repo == "" || ref == "" {
		http.Error(w, "invalid action path", http.StatusBadRequest)
		return
	}

	nameWithOwner := owner + "/" + repo
	// Resolve the action's repository before anything is served, including a
	// cache hit: a private repository's tree only goes to a job entitled to
	// it. Repositories bleephub does not host are 404, never a passthrough.
	actionRepo := s.store.GetRepo(owner, repo)
	if actionRepo == nil {
		http.Error(w, "action repository "+nameWithOwner+" is not hosted in bleephub", http.StatusNotFound)
		return
	}
	if actionRepo.Private {
		caller, err := s.callerRunner(r)
		if err != nil || !caller.Scope.coversRepo(actionRepo.FullName) {
			s.logger.Warn().Str("action", nameWithOwner).Msg("action tarball denied: private repository outside the job's scope")
			http.Error(w, "action repository "+nameWithOwner+" is not hosted in bleephub", http.StatusNotFound)
			return
		}
	}

	resolvedSHA := s.resolveActionSha(nameWithOwner, ref)
	if resolvedSHA == "0000000000000000000000000000000000000000" {
		http.Error(w, "action ref "+ref+" is not resolvable in "+nameWithOwner, http.StatusNotFound)
		return
	}
	key := nameWithOwner + "@" + resolvedSHA
	if entry := s.actionCache.Get(key); entry != nil {
		s.logger.Debug().Str("key", key).Msg("serving cached action tarball")
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(entry.Data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(entry.Data)
		return
	}

	// Actions hosted on bleephub serve from their own git storage.
	// Repositories that are not present locally fail loudly instead of
	// reaching out to github.com behind the runner's back.
	if entry, err := s.localActionTarball(owner, repo, ref); err == nil && entry != nil {
		entry.FetchedAt = s.currentTime()
		s.actionCache.Put(key, entry)
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(entry.Data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(entry.Data)
		return
	} else if err != nil {
		s.logger.Error().Err(err).Str("key", key).Msg("local action tarball failed")
		http.Error(w, "failed to build action tarball: "+err.Error(), http.StatusBadGateway)
		return
	}

	http.Error(w, "action repository "+nameWithOwner+" is not hosted in bleephub", http.StatusNotFound)
}

// localActionTarball builds a GitHub-layout tarball (single top-level
// "<owner>-<repo>-<sha>/" directory, like codeload's) from a repo hosted
// on this server. (nil, nil) means the repo is not hosted in bleephub.
func (s *Server) localActionTarball(owner, repo, ref string) (*ActionCacheEntry, error) {
	stor := s.store.GetGitStorage(owner, repo)
	if stor == nil {
		return nil, nil
	}
	sha := resolveActionRefSha(stor, ref)
	if sha == "0000000000000000000000000000000000000000" {
		return nil, fmt.Errorf("ref %q not found in %s/%s", ref, owner, repo)
	}

	commit, err := object.GetCommit(stor, plumbing.NewHash(sha))
	if err != nil {
		return nil, fmt.Errorf("resolve commit %s: %w", sha, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}

	prefix := fmt.Sprintf("%s-%s-%s/", owner, repo, sha[:7])
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err = tree.Files().ForEach(func(f *object.File) error {
		content, err := f.Contents()
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if f.Mode == filemode.Executable {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: prefix + f.Name,
			Mode: mode,
			Size: int64(len(content)),
		}); err != nil {
			return err
		}
		_, err = tw.Write([]byte(content))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("build tarball: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return &ActionCacheEntry{Data: buf.Bytes(), ResolvedSha: sha}, nil
}

func resolveActionRefSha(stor gitStorage.Storer, ref string) string {
	const zeroSha = "0000000000000000000000000000000000000000"
	if ref == "" {
		return resolveRefSha(stor, "")
	}
	for _, name := range []string{ref, "refs/heads/" + ref, "refs/tags/" + ref} {
		if sha := strictGitRefSha(stor, plumbing.ReferenceName(name)); sha != zeroSha {
			return sha
		}
	}
	if len(ref) == 40 {
		if _, err := object.GetCommit(stor, plumbing.NewHash(ref)); err == nil {
			return ref
		}
	}
	return zeroSha
}

func strictGitRefSha(stor gitStorage.Storer, name plumbing.ReferenceName) string {
	const zeroSha = "0000000000000000000000000000000000000000"
	r, err := stor.Reference(name)
	if err != nil {
		return zeroSha
	}
	if r.Type() == plumbing.SymbolicReference {
		target, err := stor.Reference(r.Target())
		if err != nil || target.Hash().IsZero() {
			return zeroSha
		}
		return target.Hash().String()
	}
	if r.Hash().IsZero() {
		return zeroSha
	}
	return r.Hash().String()
}
