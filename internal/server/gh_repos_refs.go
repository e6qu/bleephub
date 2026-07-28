package bleephub

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

func (s *Server) registerGHRepoRefRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/branches", s.handleListBranches)
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}", s.handleGetBranch)
	s.route("GET /api/v3/repos/{owner}/{repo}/tags", s.handleListTags)
}

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	base := s.baseURL(r)
	var tags []map[string]interface{}
	refs, err := stor.IterReferences()
	if err != nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if !ref.Name().IsTag() {
			return nil
		}
		target := peelRepositoryTagTarget(stor, ref.Hash())
		if target.IsZero() {
			return nil
		}
		tagName := ref.Name().Short()
		tags = append(tags, map[string]interface{}{
			"name":        tagName,
			"zipball_url": base + "/" + repo.FullName + "/legacy.zip/refs/tags/" + tagName,
			"tarball_url": base + "/" + repo.FullName + "/legacy.tar.gz/refs/tags/" + tagName,
			"commit": map[string]interface{}{
				"sha": target.String(),
				"url": base + "/api/v3/repos/" + repo.FullName + "/commits/" + target.String(),
			},
			"node_id": nodeIDForTag(repo, tagName),
		})
		return nil
	})
	sort.Slice(tags, func(i, j int) bool {
		return fmt.Sprint(tags[i]["name"]) < fmt.Sprint(tags[j]["name"])
	})

	if tags == nil {
		tags = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, tags))
}

func peelRepositoryTagTarget(stor storer.EncodedObjectStorer, hash plumbing.Hash) plumbing.Hash {
	if hash.IsZero() {
		return plumbing.ZeroHash
	}
	seen := map[plumbing.Hash]bool{}
	for {
		if hash.IsZero() || seen[hash] {
			return plumbing.ZeroHash
		}
		seen[hash] = true
		tag, err := object.GetTag(stor, hash)
		if err != nil {
			return hash
		}
		hash = tag.Target
	}
}

func nodeIDForTag(repo *Repo, tagName string) string {
	return encodeNodeID("Tag", repo.ID, tagName)
}

func (s *Server) handleListRefs(w http.ResponseWriter, r *http.Request) {
	s.handleGetRefs(w, r)
}

func (s *Server) handleGetRefs(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	refPath := r.PathValue("ref")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	base := s.baseURL(r)

	// Empty path means list all refs.
	if refPath == "" {
		s.listRefs(w, r, base, repo.FullName, stor, "")
		return
	}

	// refPath may be a namespace like "heads" or "heads/main", or a deeper
	// path like "heads/feature/foo". GitHub first tries to resolve the exact
	// path as a single reference; if that fails, it treats the path as a
	// namespace and lists everything underneath.
	fullRef := plumbing.ReferenceName("refs/" + refPath)
	if ref, err := stor.Reference(fullRef); err == nil {
		writeJSON(w, http.StatusOK, refToJSON(stor, base, repo.FullName, ref))
		return
	}

	prefix := "refs/" + refPath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	// If the requested path looks like a leaf (no trailing slash and at least
	// two segments), and there is nothing under it, return 404 to match the
	// GitHub behavior for a missing single ref. Otherwise return the listing.
	segments := strings.Split(strings.TrimSuffix(refPath, "/"), "/")
	looksLikeSingleRef := len(segments) >= 2

	refs, err := stor.IterReferences()
	if err != nil {
		if looksLikeSingleRef {
			writeGHError(w, http.StatusNotFound, "Not Found")
		} else {
			writeJSON(w, http.StatusOK, []interface{}{})
		}
		return
	}

	var items []map[string]interface{}
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference || !strings.HasPrefix(string(ref.Name()), prefix) {
			return nil
		}
		items = append(items, refToJSON(stor, base, repo.FullName, ref))
		return nil
	})

	if len(items) == 0 && looksLikeSingleRef {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if items == nil {
		items = []map[string]interface{}{}
	}
	sort.Slice(items, func(i, j int) bool {
		return fmt.Sprint(items[i]["ref"]) < fmt.Sprint(items[j]["ref"])
	})
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listRefs(w http.ResponseWriter, r *http.Request, baseURL, fullName string, stor gitStorage.Storer, prefix string) {
	refs, err := stor.IterReferences()
	if err != nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	var items []map[string]interface{}
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference || !strings.HasPrefix(string(ref.Name()), "refs/") {
			return nil
		}
		if prefix != "" && !strings.HasPrefix(string(ref.Name()), prefix) {
			return nil
		}
		items = append(items, refToJSON(stor, baseURL, fullName, ref))
		return nil
	})

	if items == nil {
		items = []map[string]interface{}{}
	}
	sort.Slice(items, func(i, j int) bool {
		return fmt.Sprint(items[i]["ref"]) < fmt.Sprint(items[j]["ref"])
	})
	writeJSON(w, http.StatusOK, items)
}

func refToJSON(stor gitStorage.Storer, baseURL, fullName string, ref *plumbing.Reference) map[string]interface{} {
	hash, err := resolvedReferenceHash(stor, ref, map[plumbing.ReferenceName]bool{})
	if err != nil {
		hash = ref.Hash()
	}
	objectType := gitObjectTypeName(stor, hash)
	refPath := strings.TrimPrefix(ref.Name().String(), "refs/")
	return map[string]interface{}{
		"ref":     string(ref.Name()),
		"node_id": encodeNodeID("Ref", 0, string(ref.Name())),
		"url":     baseURL + "/api/v3/repos/" + fullName + "/git/refs/" + refPath,
		"object": map[string]interface{}{
			"sha":  hash.String(),
			"type": objectType,
			"url":  baseURL + "/api/v3/repos/" + fullName + "/git/" + objectType + "s/" + hash.String(),
		},
	}
}

func gitObjectTypeName(stor gitStorage.Storer, hash plumbing.Hash) string {
	encoded, err := stor.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		return "commit"
	}
	return objectTypeName(encoded.Type())
}

// encodeNodeID returns a deterministic base64 GraphQL global node id for the
// given type and local identifier. It mirrors the shape GitHub uses for opaque
// node IDs without requiring a persistent node-id table.
func encodeNodeID(typ string, id int, suffix string) string {
	var payload string
	if suffix != "" {
		payload = fmt.Sprintf("%s:%d:%s", typ, id, suffix)
	} else {
		payload = fmt.Sprintf("%s:%d", typ, id)
	}
	return base64.StdEncoding.EncodeToString([]byte(payload))
}

func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	refs, err := stor.IterReferences()
	if err != nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	// The list response is the short-branch shape: commit carries
	// exactly {sha, url} (the full commit object belongs to the
	// single-branch endpoint).
	base := s.baseURL(r)
	var protectedFilter *bool
	switch raw := r.URL.Query().Get("protected"); raw {
	case "":
	case "true":
		value := true
		protectedFilter = &value
	case "false":
		value := false
		protectedFilter = &value
	default:
		writeGHValidationError(w, "Branch", "protected", "invalid")
		return
	}
	var branches []map[string]interface{}
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if !ref.Name().IsBranch() {
			return nil
		}
		if ref.Hash().IsZero() {
			return nil
		}
		branchName := ref.Name().Short()
		protected, protection, protectionURL := s.branchProtectionShape(repo, branchName, base)
		if protectedFilter != nil && protected != *protectedFilter {
			return nil
		}
		item := map[string]interface{}{
			"name":      branchName,
			"protected": protected,
			"commit": map[string]interface{}{
				"sha": ref.Hash().String(),
				"url": base + "/api/v3/repos/" + repo.FullName + "/commits/" + ref.Hash().String(),
			},
		}
		if protected {
			item["protection"] = protection
			item["protection_url"] = protectionURL
		}
		branches = append(branches, item)
		return nil
	})

	if branches == nil {
		branches = []map[string]interface{}{}
	}
	sort.Slice(branches, func(i, j int) bool {
		left, _ := branches[i]["name"].(string)
		right, _ := branches[j]["name"].(string)
		return left < right
	})
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, branches))
}

func (s *Server) handleGetBranch(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	branch := r.PathValue("branch")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Branch not found")
		return
	}

	ref, err := stor.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Branch not found")
		return
	}

	base := s.baseURL(r)
	protected, protection, protectionURL := s.branchProtectionShape(repo, branch, base)
	branchURL := base + "/api/v3/repos/" + repo.FullName + "/branches/" + branch
	result := map[string]interface{}{
		"name":           branch,
		"protected":      protected,
		"protection":     protection,
		"protection_url": protectionURL,
		"_links": map[string]interface{}{
			"self": branchURL,
			"html": base + "/" + repo.FullName + "/tree/" + branch,
		},
		"commit": map[string]interface{}{
			"sha": ref.Hash().String(),
		},
	}

	if commit := resolveCommit(stor, ref.Hash()); commit != nil {
		result["commit"] = commitToJSON(commit, repo, base)
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteRef(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	refPath := r.PathValue("ref")

	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	stor := s.store.GetGitStorage(owner, repoName)
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// refPath is like "heads/branch-name" or "tags/v1.0"
	fullRef := plumbing.ReferenceName("refs/" + refPath)
	if !validFullyQualifiedGitRef(fullRef.String()) {
		writeGHValidationError(w, "Reference", "ref", "invalid")
		return
	}
	oldRef, err := stor.Reference(fullRef)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Reference does not exist")
		return
	}

	if err := stor.RemoveReference(fullRef); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if fullRef.IsBranch() {
		s.afterCommittedRefUpdate(repo, ghUserFromContext(r.Context()), fullRef.String(), oldRef.Hash().String(), plumbing.ZeroHash.String(), s.baseURL(r))
	}

	w.WriteHeader(http.StatusNoContent)
}

// resolveCommit looks up a commit object from storage by hash.
func resolveCommit(stor storer.EncodedObjectStorer, hash plumbing.Hash) *object.Commit {
	obj, err := object.GetCommit(stor, hash)
	if err != nil {
		return nil
	}
	return obj
}

// commitSummary converts a commit to a JSON map.
func commitSummary(c *object.Commit) map[string]interface{} {
	return map[string]interface{}{
		"sha": c.Hash.String(),
		"commit": map[string]interface{}{
			"message": strings.TrimSpace(c.Message),
			"author": map[string]interface{}{
				"name":  c.Author.Name,
				"email": c.Author.Email,
				"date":  c.Author.When.Format(time.RFC3339),
			},
			"committer": map[string]interface{}{
				"name":  c.Committer.Name,
				"email": c.Committer.Email,
				"date":  c.Committer.When.Format(time.RFC3339),
			},
			"tree": map[string]interface{}{
				"sha": c.TreeHash.String(),
			},
		},
	}
}
