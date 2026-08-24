package bleephub

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/e6qu/bleephub/internal/gitstore"
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
			// github.com advertises the API-shaped archive endpoints here
			// (https://api.github.com/repos/{o}/{r}/zipball/refs/tags/{tag}),
			// which 302 to codeload. Advertising bleephub's internal codeload
			// URL instead handed clients a link that skips the documented
			// endpoint entirely.
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
	// github.com lists the newest version first, not ascending by name: a
	// client reading tags[0] as "latest" must get v2.0.0, not v1.0.0. The
	// order is a version-aware descending compare — kubernetes/kubernetes
	// answers v1.38.0-alpha.0 before v1.36.4 (plain descending text would
	// put v1.9.x first), nodejs/node answers every v26.x before every v25.x,
	// and golang/go answers weekly.* before release.* before go1.* (so it is
	// not chronological either).
	sort.Slice(tags, func(i, j int) bool {
		return compareTagNames(fmt.Sprint(tags[i]["name"]), fmt.Sprint(tags[j]["name"])) > 0
	})

	if tags == nil {
		tags = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, tags))
}

// compareTagNames orders two tag names the way github.com's tag list does:
// version-aware, so embedded numbers compare as numbers (v1.38 > v1.9) and a
// prerelease sorts below the release it qualifies (v1.36.0 > v1.36.0-rc.1),
// with everything else falling back to a byte compare (weekly.* > release.* >
// go1.*). It returns >0 when a sorts after b, so handleListTags can sort
// descending — newest first, which is what a client reading tags[0] expects.
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
	// One name is a prefix of the other. A '-' starts a semver prerelease, and
	// a prerelease ranks below its release; any other continuation ranks above
	// the shorter name.
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

// compareNumericRuns compares two all-digit runs as numbers of unbounded
// width, so no tag name can overflow the comparison.
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

	// A namespace holding no refs is a 404 on github.com, whatever its depth —
	// GET /git/refs/tags on a repository without tags, GET /git/refs/bogusns
	// and GET /git/refs/HEAD all answer 404, not an empty array. (Only the
	// modern GET /git/matching-refs/{ref} returns [] for an empty match.)
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
	// GitHub paginates the ref listing at 30 per page with Link headers, the
	// same as the modern GET /git/matching-refs/{ref} this legacy path shares
	// its shape with.
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
		writeGHError(w, http.StatusInternalServerError, "Git storage unavailable")
		return
	}

	refs, err := stor.IterReferences()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git reference lookup failed")
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
		store.WriteGHValidationError(w, "Reference", "ref", "invalid")
		return
	}
	oldRef, err := stor.Reference(fullRef)
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Reference does not exist")
		return
	}

	if err := gitstore.RemoveReferenceCAS(stor, oldRef); err != nil {
		if errors.Is(err, gitStorage.ErrReferenceHasChanged) {
			writeGHError(w, http.StatusConflict, "Reference changed while it was being deleted")
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if fullRef.IsBranch() {
		s.afterCommittedRefUpdate(repo, ghUserFromContext(r.Context()), fullRef.String(), oldRef.Hash().String(), plumbing.ZeroHash.String(), s.baseURL(r))
	}
	// The `delete` event fires for a removed branch or tag.
	s.emitWebhookEvent(repo.FullName, "delete", "", buildRefLifecyclePayload(repo, fullRef, ghUserFromContext(r.Context()), s.baseURL(r)))

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
