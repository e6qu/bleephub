package bleephub

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
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
		writeGHError(w, http.StatusInternalServerError, "Git storage unavailable")
		return
	}

	base := s.baseURL(r)
	var tags []map[string]interface{}
	refs, err := stor.IterReferences()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git reference lookup failed")
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
			"name": tagName,
			// Advertise the API-shaped archive endpoints (which 302 to
			// codeload), not bleephub's internal codeload URL.
			"zipball_url": base + "/api/v3/repos/" + repo.FullName + "/zipball/refs/tags/" + tagName,
			"tarball_url": base + "/api/v3/repos/" + repo.FullName + "/tarball/refs/tags/" + tagName,
			"commit": map[string]interface{}{
				"sha": target.String(),
				"url": base + "/api/v3/repos/" + repo.FullName + "/commits/" + target.String(),
			},
			"node_id": nodeIDForTag(repo, tagName),
		})
		return nil
	})
	// Version-aware descending order, so a client reading tags[0] as "latest"
	// gets v2.0.0 not v1.0.0 (and v1.38 before v1.9, not plain text order).
	sort.Slice(tags, func(i, j int) bool {
		return compareTagNames(fmt.Sprint(tags[i]["name"]), fmt.Sprint(tags[j]["name"])) > 0
	})

	if tags == nil {
		tags = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, tags))
}

// compareTagNames orders tag names version-aware: embedded numbers compare as
// numbers (v1.38 > v1.9), a prerelease sorts below its release (v1.36.0 >
// v1.36.0-rc.1), else a byte compare. Returns >0 when a sorts after b.
func compareTagNames(a, b string) int {
	ar, br := versionRuns(a), versionRuns(b)
	for i := 0; i < len(ar) && i < len(br); i++ {
		x, y := ar[i], br[i]
		xNum, yNum := isDigitRun(x), isDigitRun(y)
		if xNum && yNum {
			if c := compareNumericRuns(x, y); c != 0 {
				return c
			}
			continue
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	if len(ar) == len(br) {
		return 0
	}
	// One name is a prefix of the other. A '-' continuation (semver prerelease)
	// ranks below the shorter name; any other continuation ranks above it.
	longer, sign := br, -1
	if len(ar) > len(br) {
		longer, sign = ar, 1
	}
	if strings.HasPrefix(longer[min(len(ar), len(br))], "-") {
		sign = -sign
	}
	return sign
}

// versionRuns splits a name into alternating runs of digits and non-digits.
func versionRuns(s string) []string {
	var runs []string
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || isASCIIDigit(s[i]) != isASCIIDigit(s[start]) {
			runs = append(runs, s[start:i])
			start = i
		}
	}
	return runs
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

func isDigitRun(s string) bool { return s != "" && isASCIIDigit(s[0]) }

// compareNumericRuns compares two all-digit runs as unbounded-width numbers.
func compareNumericRuns(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
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

func nodeIDForTag(repo *store.Repo, tagName string) string {
	return encodeNodeID("Tag", repo.ID, tagName)
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

	if refPath == "" {
		s.listRefs(w, r, base, repo.FullName, stor, "")
		return
	}

	// Try the exact path as a single reference first; on miss, treat it as a
	// namespace and list everything underneath.
	fullRef := plumbing.ReferenceName("refs/" + refPath)
	if ref, err := stor.Reference(fullRef); err == nil {
		writeJSON(w, http.StatusOK, refToJSON(stor, base, repo.FullName, ref))
		return
	}

	prefix := "refs/" + refPath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	refs, err := stor.IterReferences()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git reference lookup failed")
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

	// An empty namespace is a 404 here, not []; only the modern
	// GET /git/matching-refs/{ref} returns [] for an empty match.
	if len(items) == 0 {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	sort.Slice(items, func(i, j int) bool {
		return fmt.Sprint(items[i]["ref"]) < fmt.Sprint(items[j]["ref"])
	})
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, items))
}

func (s *Server) listRefs(w http.ResponseWriter, r *http.Request, baseURL, fullName string, stor gitStorage.Storer, prefix string) {
	refs, err := stor.IterReferences()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git reference lookup failed")
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
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, items))
}

func refToJSON(stor gitStorage.Storer, baseURL, fullName string, ref *plumbing.Reference) map[string]interface{} {
	hash, err := store.ResolvedReferenceHash(stor, ref, map[plumbing.ReferenceName]bool{})
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

// encodeNodeID returns a deterministic base64 node id for the type and local
// identifier, avoiding a persistent node-id table.
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
		writeGHError(w, http.StatusInternalServerError, "Git storage unavailable")
		return
	}

	refs, err := stor.IterReferences()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git reference lookup failed")
		return
	}

	// Short-branch shape: commit carries only {sha, url}; the full commit
	// object belongs to the single-branch endpoint.
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
		store.WriteGHValidationError(w, "Branch", "protected", "invalid")
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
		writeGHError(w, http.StatusInternalServerError, "Git storage unavailable")
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
		result["commit"] = commitToJSON(commit, repo, s.store, base)
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteRef(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	stor := s.store.GetGitStorage(r.PathValue("owner"), r.PathValue("repo"))
	if stor == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	fullRef := plumbing.ReferenceName("refs/" + r.PathValue("ref"))
	if failure := s.deleteGitRef(r.Context(), repo, stor, ghUserFromContext(r.Context()), fullRef, s.baseURL(r)); failure != nil {
		failure.write(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func resolveCommit(stor storer.EncodedObjectStorer, hash plumbing.Hash) *object.Commit {
	obj, err := object.GetCommit(stor, hash)
	if err != nil {
		return nil
	}
	return obj
}
