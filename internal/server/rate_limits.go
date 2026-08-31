package bleephub

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// apiRateWindow is one credential/resource primary-rate-limit window. The map
// key holds only a SHA-256 digest of the credential (or the anonymous address),
// never the token itself.
type apiRateWindow struct {
	Limit     int
	Used      int
	Reset     time.Time
	unbounded bool // test fixtures that intentionally reuse one identity
}

type apiRateSnapshot struct {
	Resource  string
	Limit     int
	Used      int
	Remaining int
	Reset     int64
	Exceeded  bool
}

var apiRateResourceLimits = map[string]int{
	"actions_runner_registration": 10000,
	"code_scanning_autofix":       10,
	"code_scanning_upload":        500,
	"code_search":                 10,
	// copilot_usage_records and dependency_sbom are reported buckets in their own
	// right (GitHub's general-purpose 5000/hour default). Without their own entry
	// here rateWindowKeyAndLimit collapses them onto the core key, so /rate_limit
	// reports their used/remaining bleeding from unrelated core traffic.
	"copilot_usage_records": 5000,
	"core":                  5000,
	"dependency_sbom":       5000,
	"dependency_snapshots":  100,
	"graphql":               5000,
	"integration_manifest":  5000,
	"scim":                  15000,
	"search":                30,
	"source_import":         100,
	// auth: internal IP-scoped per-minute anti-brute-force budget for the sign-in
	// endpoints. Not GitHub-exposed; never appears in /rate_limit.
	"auth": authFlowRateLimit,
}

// authFlowRateLimit bounds sign-in attempts per client IP per minute — above a
// shared-NAT office's login rate, below an automated guesser's.
const authFlowRateLimit = 60

// apiRateResponseResources is the exact set the current /rate_limit response
// schema permits. code_scanning_upload has its own bucket but is not a
// permitted property here.
var apiRateResponseResources = []string{
	"actions_runner_registration",
	"code_scanning_autofix",
	"code_search",
	"copilot_usage_records",
	"core",
	"dependency_sbom",
	"dependency_snapshots",
	"graphql",
	"integration_manifest",
	"scim",
	"search",
	"source_import",
}

// containsPathSegments reports whether path contains "/a/b" as whole segments.
// A plain substring test misfires on free-text repo names: `/repos/octo/import`
// contains "/import" without being the import endpoint.
func containsPathSegments(path, segments string) bool {
	for offset := 0; ; {
		index := strings.Index(path[offset:], segments)
		if index < 0 {
			return false
		}
		end := offset + index + len(segments)
		if end == len(path) || path[end] == '/' {
			return true
		}
		offset = end
	}
}

func apiRateResource(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/graphql"):
		return "graphql"
	case strings.HasPrefix(path, "/api/v3/search/code"):
		return "code_search"
	case strings.HasPrefix(path, "/api/v3/search/"):
		return "search"
	case containsPathSegments(path, "/actions/runners/registration-token"),
		containsPathSegments(path, "/actions/runners/remove-token"),
		path == "/api/v3/actions/runner-registration":
		return "actions_runner_registration"
	case containsPathSegments(path, "/import"):
		return "source_import"
	case containsPathSegments(path, "/dependency-graph/snapshots"):
		return "dependency_snapshots"
	case containsPathSegments(path, "/dependency-graph/sbom"):
		return "dependency_sbom"
	case containsPathSegments(path, "/copilot/usage-records"):
		return "copilot_usage_records"
	case containsPathSegments(path, "/code-scanning/sarifs"):
		return "code_scanning_upload"
	case strings.Contains(path, "/code-scanning/alerts/") && strings.HasSuffix(path, "/autofix"):
		return "code_scanning_autofix"
	case strings.HasPrefix(path, "/api/v3/scim/"):
		return "scim"
	case strings.HasPrefix(path, "/api/v3/app-manifests/"):
		return "integration_manifest"
	default:
		return "core"
	}
}

func apiRateWindowDuration(resource string) time.Duration {
	switch resource {
	case "search", "code_search", "dependency_snapshots", "code_scanning_autofix", "auth":
		return time.Minute
	default:
		return time.Hour
	}
}

func apiRateIdentity(r *http.Request) string {
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		scheme, credential := authScheme(authorization)
		identity := strings.TrimSpace(authorization)
		// "token" and "Bearer" present the same credential; key on the credential
		// so alternating schemes/casing cannot double the budget.
		if credential != "" && (scheme == "token" || scheme == "bearer") {
			identity = credential
		}
		sum := sha256.Sum256([]byte(identity))
		return fmt.Sprintf("auth:%x", sum)
	}
	// Key a browser session by the resolved principal, not the cookie, so extra
	// sessions cannot multiply the authenticated budget.
	if user := ghUserFromContext(r.Context()); user != nil {
		return fmt.Sprintf("user:%d", user.ID)
	}
	host := r.RemoteAddr
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	// Trust the leftmost X-Forwarded-For only when the direct peer is
	// loopback/private (arrived through our own proxy, which sanitizes the
	// header). A public peer's header stays untrusted, else it could mint fresh
	// budgets at will.
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
			host = forwarded
		}
	}
	if host == "" {
		host = "unknown"
	}
	return "anonymous:" + host
}

// isBrowserSessionRequest reports whether the request was authenticated by the
// browser session cookie: no Authorization header, principal still resolved.
// Tokens and Basic auth carry an Authorization header, so they never match.
func isBrowserSessionRequest(r *http.Request) bool {
	return r.Header.Get("Authorization") == "" && ghUserFromContext(r.Context()) != nil
}

// rateWindowKeyAndLimit resolves the per-identity window key, limit, and
// effective resource (unknown resources fall back to core). Shared by the
// consume path and the 304 refund path.
func (s *Server) rateWindowKeyAndLimit(r *http.Request, resource string) (key string, limit int, effective string) {
	limit, ok := apiRateResourceLimits[resource]
	if !ok {
		resource, limit = "core", apiRateResourceLimits["core"]
	}
	// Unauthenticated budgets are IP-scoped and smaller: core 5000→60/hour,
	// search 30→10/minute.
	if r.Header.Get("Authorization") == "" && ghUserFromContext(r.Context()) == nil {
		switch resource {
		case "core":
			limit = 60
		case "search", "code_search":
			limit = 10
		}
	}
	return apiRateIdentity(r) + "\x1f" + resource, limit, resource
}

// refundRateLimit returns one consumed unit to the caller's window. A 304 is
// not billed on GitHub, but middleware consumes before the handler decides
// not-modified, so the unit is handed back here.
func (s *Server) refundRateLimit(r *http.Request, resource string) apiRateSnapshot {
	key, limit, effective := s.rateWindowKeyAndLimit(r, resource)
	// A browser-session request never consumed a core unit, so nothing to refund.
	if effective == "core" && isBrowserSessionRequest(r) {
		return s.rateLimitSnapshot(r, resource, false)
	}
	now := time.Now().UTC()
	s.rateLimitsMu.Lock()
	defer s.rateLimitsMu.Unlock()
	window := s.rateLimits[key]
	if window == nil || !now.Before(window.Reset) {
		return apiRateSnapshot{Resource: effective, Limit: limit, Remaining: limit,
			Reset: now.Add(apiRateWindowDuration(effective)).Unix()}
	}
	if window.Used > 0 && !window.unbounded {
		window.Used--
	}
	return apiRateSnapshot{
		Resource:  effective,
		Limit:     window.Limit,
		Used:      window.Used,
		Remaining: max(window.Limit-window.Used, 0),
		Reset:     window.Reset.Unix(),
	}
}

func (s *Server) rateLimitSnapshot(r *http.Request, resource string, consume bool) apiRateSnapshot {
	key, limit, resource := s.rateWindowKeyAndLimit(r, resource)
	// The SPA fires 16-23 calls per page; billing them against core would cap a
	// browser user at a few hundred page views per hour. A session request
	// observes the core window read-only (honest headers, remaining pinned).
	// Non-core budgets (search, graphql, ...) still bill the session normally.
	if consume && resource == "core" && isBrowserSessionRequest(r) {
		consume = false
	}
	now := time.Now().UTC()

	s.rateLimitsMu.Lock()
	if s.rateLimits == nil {
		s.rateLimits = map[string]*apiRateWindow{}
	}
	window := s.rateLimits[key]
	if window == nil || !now.Before(window.Reset) {
		window = &apiRateWindow{Limit: limit, Reset: now.Add(apiRateWindowDuration(resource))}
		s.rateLimits[key] = window
	}
	exceeded := false
	if consume {
		if window.Used >= window.Limit && !window.unbounded {
			exceeded = true
		} else {
			window.Used++
		}
	}
	snapshot := apiRateSnapshot{
		Resource:  resource,
		Limit:     window.Limit,
		Used:      window.Used,
		Remaining: max(window.Limit-window.Used, 0),
		Reset:     window.Reset.Unix(),
		Exceeded:  exceeded,
	}
	s.rateLimitsMu.Unlock()
	return snapshot
}

// reapExpiredRateLimitWindows deletes per-identity rate windows whose reset has
// passed. Without it s.rateLimits grows one permanent entry per
// identity×resource forever — every distinct token, session, or client IP
// (and a scanner rotating either) leaks memory that is never reclaimed. A
// past-reset window is already recreated fresh on next access (same condition
// as the snapshot/refund paths), so deleting it here loses nothing.
func (s *Server) reapExpiredRateLimitWindows(now time.Time) {
	s.rateLimitsMu.Lock()
	defer s.rateLimitsMu.Unlock()
	for key, window := range s.rateLimits {
		if window == nil || !now.Before(window.Reset) {
			delete(s.rateLimits, key)
		}
	}
}

// rateLimitAuthFlow throttles the sign-in endpoints per client IP against
// brute force, refusing with a 403 + Retry-After. It does not gate the
// device/OAuth token-exchange endpoints, which legitimately poll.
func (s *Server) rateLimitAuthFlow(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authFlowRateLimitExempt(r) {
			snapshot := s.rateLimitSnapshot(r, "auth", true)
			if snapshot.Exceeded {
				seconds := max(int(time.Until(time.Unix(snapshot.Reset, 0)).Seconds()), 1)
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeGHError(w, http.StatusForbidden, "Too many authentication attempts. Please wait and try again.")
				return
			}
		}
		next(w, r)
	}
}

// authFlowRateLimitExempt exempts local/dev/e2e traffic. Production auth arrives
// through the proxy (tagged with X-Forwarded-For and limited per client); only a
// direct loopback peer carrying no forwarded client is exempt.
func authFlowRateLimitExempt(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func rateSnapshotJSON(snapshot apiRateSnapshot) map[string]interface{} {
	return map[string]interface{}{
		"limit":     snapshot.Limit,
		"used":      snapshot.Used,
		"remaining": snapshot.Remaining,
		"reset":     snapshot.Reset,
	}
}
