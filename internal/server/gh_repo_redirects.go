package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// Redirecting a repository that moved: a renamed repo's vacated name answers
// with a 301 to its new name so stored URLs, bookmarked API paths, and existing
// clones keep working. The redirect covers the whole subtree, since the repo —
// not the endpoint — is what moved.

// repoRedirectPrefixes are the request-path prefixes whose next two segments
// name a repository.
var repoRedirectPrefixes = []string{"/api/v3/repos/", "/repos/"}

// repoRedirectMiddleware answers a request to a repo's former name with its new
// address. It runs inside the authenticated chain so a repo the viewer cannot
// see falls through to the handler's 404 rather than disclosing its existence.
func (s *Server) repoRedirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if target, rest, ok := s.movedRepoForPath(r, r.URL.Path); ok {
			s.writeRepoMoved(w, r, target, rest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// movedRepoForPath resolves a path to the repo a former name now points to,
// plus the remainder after "owner/name". False when the path names no repo, is
// still live under the name used, or whose target the viewer may not read.
func (s *Server) movedRepoForPath(r *http.Request, path string) (*store.Repo, string, bool) {
	var tail string
	matched := false
	for _, prefix := range repoRedirectPrefixes {
		if strings.HasPrefix(path, prefix) {
			tail = strings.TrimPrefix(path, prefix)
			matched = true
			break
		}
	}
	if !matched {
		return nil, "", false
	}
	owner, remainder, _ := strings.Cut(tail, "/")
	name, rest, hasRest := strings.Cut(remainder, "/")
	if owner == "" || name == "" {
		return nil, "", false
	}
	if hasRest {
		rest = "/" + rest
	}
	target := s.store.RedirectedRepo(owner + "/" + name)
	if target == nil {
		return nil, "", false
	}
	if target.Private && !s.viewerCanReadRepo(r.Context(), target) {
		return nil, "", false
	}
	return target, rest, true
}

// writeRepoMoved emits the moved_permanently response. The Location is built
// from the stored repo's canonical full name; the only request-derived part is
// the trailing sub-resource path.
func (s *Server) writeRepoMoved(w http.ResponseWriter, r *http.Request, target *store.Repo, rest string) {
	location := s.baseURL(r) + "/api/v3/repos/" + target.FullName + rest
	if raw := r.URL.RawQuery; raw != "" {
		location += "?" + raw
	}
	w.Header().Set("Location", location)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMovedPermanently)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message":           "Moved Permanently",
		"url":               s.baseURL(r) + "/api/v3/repositories/" + strconv.Itoa(target.ID),
		"documentation_url": "https://docs.github.com/rest/guides/best-practices-for-using-the-rest-api#follow-redirects",
	})
}

// redirectMovedGitRepo answers a git smart-HTTP request to a repo's former name
// with the same path under its new one, so a clone made before a rename keeps
// fetching.
func (s *Server) redirectMovedGitRepo(w http.ResponseWriter, r *http.Request, owner, repoName string) bool {
	if s.store.GetRepo(owner, repoName) != nil {
		return false
	}
	// A wiki is served from `<repo>.wiki.git`, so a moved repo moves its wiki
	// remote too.
	name, isWiki := strings.CutSuffix(repoName, ".wiki")
	target := s.store.RedirectedRepo(owner + "/" + name)
	if target == nil {
		return false
	}
	ctx, _ := s.authenticateGitRequest(r)
	if target.Private && !s.viewerCanReadRepo(ctx, target) {
		return false
	}
	suffix := gitRequestSuffix(r.URL.Path)
	if suffix == "" {
		return false
	}
	moved := target.FullName
	if isWiki {
		moved += ".wiki"
	}
	// Same-origin path, built only from server-chosen values: the stored repo's
	// canonical name, a fixed smart-HTTP suffix, and the service matched to one
	// of the two literals. Nothing the client sends reaches it verbatim, so a
	// forged Host header cannot redirect to another origin.
	location := "/" + moved + ".git" + suffix
	if service := gitAdvertisedService(r.URL.Query().Get("service")); service != "" {
		location += "?service=" + service
	}
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusMovedPermanently)
	return true
}

// gitAdvertisedService returns the matching smart-HTTP service literal, or ""
// for anything else. Returning the literal (not the argument) keeps a
// server-chosen constant — not client bytes — flowing into the redirect target.
func gitAdvertisedService(service string) string {
	switch service {
	case "git-upload-pack":
		return "git-upload-pack"
	case "git-receive-pack":
		return "git-receive-pack"
	default:
		return ""
	}
}

// gitRequestSuffix returns the smart-HTTP endpoint suffix a path ends in, or "".
// Rebuilding from this fixed set, not the request's trailing bytes, keeps the
// redirect target server-chosen.
func gitRequestSuffix(path string) string {
	for _, suffix := range []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"} {
		if strings.HasSuffix(path, suffix) {
			return suffix
		}
	}
	return ""
}
