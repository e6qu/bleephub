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

// apiRateWindow is one credential/resource primary-rate-limit window. Tokens
// are never retained: the map key contains only a SHA-256 digest of the
// presented authorization value (or the anonymous remote address).
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
	"core":                        5000,
	"dependency_snapshots":        100,
	"graphql":                     5000,
	"integration_manifest":        5000,
	"scim":                        15000,
	"search":                      30,
	"source_import":               100,
	// auth is an internal, IP-scoped per-minute budget for the unauthenticated
	// credential-verifying auth-flow endpoints (password and token sign-in). It
	// throttles brute-force guessing without blocking a human's few attempts. It
	// is not a GitHub-exposed resource and never appears in /rate_limit.
	"auth": authFlowRateLimit,
}

// authFlowRateLimit bounds credential-verifying auth-flow attempts per client
// IP (or presented credential) per minute. Set well above the login rate a
// shared-NAT office produces, yet far below an automated guesser's, so it
// throttles brute force without locking out legitimate users behind one IP.
const authFlowRateLimit = 60

// apiRateResponseResources is the exact object currently described by
// GitHub's REST schema. code_scanning_upload has a distinct request-header
// bucket on GHES versions, but is not a permitted property in the current
// /rate_limit response schema.
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

func apiRateResource(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/graphql"):
		return "graphql"
	case strings.HasPrefix(path, "/api/v3/search/code"):
		return "code_search"
	case strings.HasPrefix(path, "/api/v3/search/"):
		return "search"
	case strings.Contains(path, "/actions/runners/registration-token"),
		strings.Contains(path, "/actions/runners/remove-token"),
		path == "/api/v3/actions/runner-registration":
		return "actions_runner_registration"
	case strings.Contains(path, "/import"):
		return "source_import"
	case strings.Contains(path, "/dependency-graph/snapshots"):
		return "dependency_snapshots"
	case strings.Contains(path, "/code-scanning/sarifs"):
		return "code_scanning_upload"
	case strings.Contains(path, "/code-scanning/alerts/") && strings.HasSuffix(path, "/autofix"):
		return "code_scanning_autofix"
	case strings.Contains(path, "/scim/"):
		return "scim"
	case strings.Contains(path, "/app-manifests/"):
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
		// "token" and "Bearer" are interchangeable ways to present the same
		// GitHub credential. Keying on the raw header would let a client double
		// its budget merely by alternating schemes (or their casing).
		if credential != "" && (scheme == "token" || scheme == "bearer") {
			identity = credential
		}
		sum := sha256.Sum256([]byte(identity))
		return fmt.Sprintf("auth:%x", sum)
	}
	// A browser session is an authenticated user credential even though its
	// opaque HttpOnly cookie deliberately never becomes an Authorization
	// header. Key it by the resolved principal, not by the cookie secret: this
	// gives every browser session the authenticated budget and prevents a user
	// from multiplying that budget by opening more sessions.
	if user := ghUserFromContext(r.Context()); user != nil {
		return fmt.Sprintf("user:%d", user.ID)
	}
	host := r.RemoteAddr
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	// Behind the TLS-terminating reverse proxy every anonymous request
	// carries the proxy's address as RemoteAddr, which would collapse all
	// proxy-fronted anonymous callers into one shared budget. There is no
	// configured trusted-proxy list, so honor the leftmost X-Forwarded-For
	// entry only when the direct peer is loopback or private — i.e. the
	// request can only have arrived through our own proxy, which sanitizes
	// any client-supplied X-Forwarded-For before appending the real client.
	// A public peer's header stays untrusted: it could mint fresh budgets at
	// will.
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

func (s *Server) rateLimitSnapshot(r *http.Request, resource string, consume bool) apiRateSnapshot {
	limit, ok := apiRateResourceLimits[resource]
	if !ok {
		resource, limit = "core", apiRateResourceLimits["core"]
	}
	// GitHub's unauthenticated core budget is IP-scoped and deliberately much
	// smaller than an authenticated user's budget.
	if resource == "core" && r.Header.Get("Authorization") == "" && ghUserFromContext(r.Context()) == nil {
		limit = 60
	}
	key := apiRateIdentity(r) + "\x1f" + resource
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

// rateLimitAuthFlow throttles the unauthenticated credential-verifying auth
// endpoints (password and token sign-in) per client IP, so they cannot be
// brute-forced. It deliberately does not gate the device/OAuth token-exchange
// endpoints, which legitimately poll and carry their own interval/slow_down
// handling. The refusal matches the primary-rate-limit shape used elsewhere: a
// 403 with Retry-After.
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

// authFlowRateLimitExempt reports whether a request is local/dev/e2e traffic
// rather than the remote brute-force vector the auth-flow limiter guards.
// Production auth always arrives through the reverse proxy, which tags
// X-Forwarded-For with the real client (then keyed and limited per that
// client); only a direct loopback peer carrying no forwarded client — the local
// binary, dev, and the e2e harness hitting 127.0.0.1 — is exempt.
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
