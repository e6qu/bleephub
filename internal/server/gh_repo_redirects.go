package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// Redirecting a repository that moved.
//
// The vendored contract documents `301 Moved Permanently` (components/
// responses/moved_permanently, schema basic-error) on nineteen operations
// under /repos/{owner}/{repo}, starting with GET /repos/{owner}/{repo}. It is
// what a rename means on GitHub: the vacated name is kept and answers with the
// address of the repository under its new name, so a stored URL, a bookmarked
// API path and — through the same redirect on the git transports — a clone
// already on a developer's machine all keep working.
//
// The redirect covers the whole subtree rather than the single operation the
// client tripped over, because the repository, not the endpoint, is what moved.

// repoRedirectPrefixes are the request-path prefixes whose next two segments
// name a repository: the REST API, and the uploads-host form the CodeQL action
// posts to.
var repoRedirectPrefixes = []string{"/api/v3/repos/", "/repos/"}

// repoRedirectMiddleware answers a request addressed to a repository's former
// name with the address it moved to. It runs inside the authenticated chain so
// a repository the viewer cannot see falls through to the handler's 404 rather
// than having its existence disclosed by a redirect.
func (s *Server) repoRedirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if target, rest, ok := s.movedRepoForPath(r, r.URL.Path); ok {
			s.writeRepoMoved(w, r, target, rest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// movedRepoForPath resolves a request path to the repository a former name now
// names, together with the path remainder that follows "owner/name". It reports
// false for a path that names no repository, one that is still live under the
// name used, and one whose target the viewer may not read.
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

// writeRepoMoved emits the documented moved_permanently response: the
// basic-error body, and the Location header clients follow. The location is
// built from the stored repository's canonical full name, so the only
// request-derived part of it is the sub-resource path that follows.
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

// redirectMovedGitRepo answers a git smart-HTTP request addressed to a
// repository's former name with the same path under its new one. Without it a
// clone made before a rename fails on its next fetch, which is the one thing a
// rename is not supposed to break.
func (s *Server) redirectMovedGitRepo(w http.ResponseWriter, r *http.Request, owner, repoName string) bool {
	if s.store.GetRepo(owner, repoName) != nil {
		return false
	}
	// A wiki is served from `<repo>.wiki.git`, so a moved repository moves its
	// wiki remote with it and a stale wiki clone needs the same redirect.
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
	// The location is same-origin, so it is written as a path rather than
	// built on top of the request's own host. Every part of it is a value this
	// server chose: the stored repository's canonical name, one of the three
	// smart-HTTP endpoints, and the service the client named matched against
	// the two that exist. Nothing a client sends reaches it verbatim, so a
	// forged Host header cannot turn a rename redirect into a redirect at
	// another origin — and the redirect keeps working behind a proxy that
	// terminates on a different address than the one it forwards.
	location := "/" + moved + ".git" + suffix
	if service := gitAdvertisedService(r.URL.Query().Get("service")); service != "" {
		location += "?service=" + service
	}
	// Emitted the same way as the REST half above: the header and the status,
	// with no HTML courtesy body. A git client reads the Location and nothing
	// else, and the two halves of this feature answering a move through one
	// pattern is what keeps them from drifting apart.
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusMovedPermanently)
	return true
}

// gitAdvertisedService returns the smart-HTTP service a ref advertisement
// named, or "" when it named neither of the two that exist.
//
// It returns the matching literal rather than the argument it matched, so what
// reaches the redirect is a constant this file chose and not a string the
// client sent. The two are equal when the match succeeds, which is exactly why
// echoing the argument back would read as safe while still carrying the
// request's bytes into a redirect target.
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

// gitRequestSuffix returns the smart-HTTP endpoint a request path ends in, or
// "" when it ends in none. Rebuilding the redirect target from this fixed set
// rather than from the request's own trailing bytes keeps the redirect a
// server-chosen address.
func gitRequestSuffix(path string) string {
	for _, suffix := range []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"} {
		if strings.HasSuffix(path, suffix) {
			return suffix
		}
	}
	return ""
}
