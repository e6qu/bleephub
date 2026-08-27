package bleephub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// maxJSONBodyBytes caps a JSON API request body, stopping an unbounded body from
// exhausting memory during decode.
const maxJSONBodyBytes = 25 << 20 // 25 MiB

// maxBlobJSONBodyBytes caps POST /git/blobs, whose body is a whole file (GitHub
// accepts blobs up to 100 MB, ~133 MB base64). The effective ceiling is still
// maxStructuredRequestBody: requestBodyLimitMiddleware wraps every JSON body in
// that reader first, so this only raises the route to the shared limit, not to
// GitHub's 100 MB.
const maxBlobJSONBodyBytes = maxStructuredRequestBody

// maxUploadBytes caps a binary upload body a handler buffers in memory (release
// assets, container blobs, CodeQL databases). Generous but bounded.
const maxUploadBytes = 2 << 30 // 2 GiB

// requestBodyLimit is one row of the request-body-size registry.
type requestBodyLimit struct {
	name  string
	bytes int64
	scope string
}

// requestBodyLimits is the single auditable inventory of every request-body cap
// (CORE-009): the shared pipeline bounds JSON/form/GraphQL, and the entries below
// are endpoint-specific streaming caps that exceed it or bound a non-JSON body.
// Add a new binary/upload route's cap here; TestRequestBodyLimits keeps it honest.
var requestBodyLimits = []requestBodyLimit{
	{"structured (shared pipeline)", maxStructuredRequestBody, "every application/json, form and GraphQL request"},
	{"json decode helpers", maxJSONBodyBytes, "decodeJSONBody / readLimitedBody JSON handlers, container manifests"},
	{"git blob create", maxBlobJSONBodyBytes, "POST /repos/{owner}/{repo}/git/blobs (a whole file, base64-inflated)"},
	{"binary upload", maxUploadBytes, "release assets, container blobs, CodeQL databases + variant packs, SARIF, attestations, package files"},
	{"artifact chunk", maxArtifactChunkBytes, "Actions artifact chunk upload"},
	{"git upload-pack", uploadPackRequestCap, "smart-HTTP fetch negotiation"},
	{"actions timeline", timelineRequestCap, "Actions timeline record batch"},
	{"backchannel logout", maxBackChannelLogoutBytes, "OIDC back-channel logout token POST"},
}

// decodeJSONBody decodes the JSON body into v, refusing a body over
// maxJSONBodyBytes; on failure it writes a GitHub-style response and returns false.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return jsonDecodeFailed(w, err)
	}
	return true
}

// decodeJSONBodyOversizeAware is decodeJSONBody with a route-specific cap for JSON
// routes whose payload is a whole file (POST /git/blobs). Over-limit is reported
// through onOversize, since those operations do not document 413.
func decodeJSONBodyOversizeAware(w http.ResponseWriter, r *http.Request, limit int64, v interface{}, onOversize func(http.ResponseWriter)) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			onOversize(w)
			return false
		}
		return jsonDecodeFailed(w, err)
	}
	return true
}

// decodeJSONBodyOptional decodes like decodeJSONBody but tolerates an absent body,
// for endpoints where real GitHub accepts none (PUT membership: go-github sends no
// body when called without options).
func decodeJSONBodyOptional(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && err != io.EOF {
		return jsonDecodeFailed(w, err)
	}
	return true
}

// jsonDecodeFailed maps a decode error: 413 when over the cap, 400 otherwise.
func jsonDecodeFailed(w http.ResponseWriter, err error) bool {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeGHError(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return false
	}
	writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
	return false
}

// readLimitedBody reads the whole body, refusing more than limit bytes (writing
// 413). For binary/blob routes that consume r.Body directly.
func readLimitedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeGHError(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return nil, false
		}
		writeGHError(w, http.StatusBadRequest, "Could not read request body")
		return nil, false
	}
	return data, true
}

func (s *Server) registerGHRestRoutes() {
	s.route("GET /api/v3/", s.handleGHApiRoot)
	s.route("GET /api/v3/user", s.handleGHUser)
	s.route("GET /api/v3/users/{username}", s.handleGHUserByLogin)
	s.route("GET /api/v3/rate_limit", s.handleGHRateLimit)
}

// handleGHApiRoot returns the API root meta information.
func (s *Server) handleGHApiRoot(w http.ResponseWriter, r *http.Request) {
	// Exact match for /api/v3/ only.
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v3")
	if trimmed != "/" && trimmed != "" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"current_user_url":                     "/api/v3/user",
		"current_user_authorizations_html_url": "/settings/connections/applications{/client_id}",
		"authorizations_url":                   "/api/v3/authorizations",
		"code_search_url":                      "/api/v3/search/code?q={query}{&page,per_page,sort,order}",
		"commit_search_url":                    "/api/v3/search/commits?q={query}{&page,per_page,sort,order}",
		"emails_url":                           "/api/v3/user/emails",
		"emojis_url":                           "/api/v3/emojis",
		"events_url":                           "/api/v3/events",
		"feeds_url":                            "/api/v3/feeds",
		"followers_url":                        "/api/v3/user/followers",
		"following_url":                        "/api/v3/user/following{/target}",
		"gists_url":                            "/api/v3/gists{/gist_id}",
		"hub_url":                              "/api/v3/hub",
		"issue_search_url":                     "/api/v3/search/issues?q={query}{&page,per_page,sort,order}",
		"issues_url":                           "/api/v3/issues",
		"keys_url":                             "/api/v3/user/keys",
		"label_search_url":                     "/api/v3/search/labels?q={query}&repository_id={repository_id}{&page,per_page}",
		"notifications_url":                    "/api/v3/notifications",
		"organization_url":                     "/api/v3/orgs/{org}",
		"organization_repositories_url":        "/api/v3/orgs/{org}/repos{?type,page,per_page,sort}",
		"organization_teams_url":               "/api/v3/orgs/{org}/teams",
		"public_gists_url":                     "/api/v3/gists/public",
		"rate_limit_url":                       "/api/v3/rate_limit",
		"repository_url":                       "/api/v3/repos/{owner}/{repo}",
		"repository_search_url":                "/api/v3/search/repositories?q={query}{&page,per_page,sort,order}",
		"current_user_repositories_url":        "/api/v3/user/repos{?type,page,per_page,sort}",
		"starred_url":                          "/api/v3/user/starred{/owner}{/repo}",
		"starred_gists_url":                    "/api/v3/gists/starred",
		"user_url":                             "/api/v3/users/{user}",
		"user_organizations_url":               "/api/v3/user/orgs",
		"user_repositories_url":                "/api/v3/users/{user}/repos{?type,page,per_page,sort}",
		"user_search_url":                      "/api/v3/search/users?q={query}{&page,per_page,sort,order}",
	})
}

// handleGHUser returns the authenticated user in the private-user shape.
func (s *Server) handleGHUser(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	writeJSON(w, http.StatusOK, s.privateUserJSON(user))
}

func (s *Server) handleGHUserByLogin(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("username")
	user := s.store.LookupUserByLogin(login)
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.fullUserJSON(user, s.baseURL(r)))
}

func (s *Server) handleGHRateLimit(w http.ResponseWriter, r *http.Request) {
	resources := make(map[string]interface{}, len(apiRateResponseResources))
	names := append([]string(nil), apiRateResponseResources...)
	sort.Strings(names)
	for _, resource := range names {
		resources[resource] = rateSnapshotJSON(s.rateLimitSnapshot(r, resource, false))
	}
	core := resources["core"]
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"resources": resources,
		"rate":      core,
	})
}

func writeGHError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message":           message,
		"documentation_url": "https://docs.github.com/rest",
	})
}

// writeGHValidationErrorMessage is writeGHValidationError with the error item's
// optional human-readable message (e.g. issue-field-values).
func writeGHValidationErrorMessage(w http.ResponseWriter, resource, field, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message":           "Validation Failed",
		"documentation_url": "https://docs.github.com/rest",
		"errors": []map[string]string{
			{
				"resource": resource,
				"field":    field,
				"code":     code,
				"message":  message,
			},
		},
	})
}

// mutated turns a nil store-mutator result — the target was deleted between
// resolve and mutate — into a 404 rather than a nil-pointer render.
func mutated[T any](w http.ResponseWriter, v *T) bool {
	if v == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return false
	}
	return true
}

// fullUserJSON converts a User to GitHub's public-user shape (simple-user plus
// profile fields and counters). Unset company/location/twitter_username are null;
// gists are not a bleephub feature, so public_gists is 0.
func (s *Server) fullUserJSON(u *store.User, baseURL string) map[string]interface{} {
	if u == nil {
		u = store.GhostUser()
	}
	out := store.UserToJSON(u, baseURL)
	out["bio"] = u.Bio
	out["blog"] = u.Blog
	out["company"] = nullableString(u.Company)
	out["location"] = nullableString(u.Location)
	out["twitter_username"] = nullableString(u.TwitterUsername)
	if u.Hireable != nil {
		out["hireable"] = *u.Hireable
	} else {
		out["hireable"] = nil
	}
	out["followers"] = s.store.CountFollowers(u.Login)
	out["following"] = s.store.CountFollowing(u.Login)
	out["public_repos"] = s.store.CountPublicRepos(u.Login)
	out["public_gists"] = 0
	out["created_at"] = u.CreatedAt.Format(time.RFC3339)
	out["updated_at"] = u.UpdatedAt.Format(time.RFC3339)
	return out
}
