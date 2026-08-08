package bleephub

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type contextKey string

// #nosec G101 -- typed context key, not a credential.
const ctxUser contextKey = "gh-user"

// #nosec G101 -- typed context key, not a credential.
const ctxApp contextKey = "gh-app"
const ctxInstallation contextKey = "gh-installation"
const ctxInstallationToken contextKey = "gh-installation-token"
const ctxUserToServerToken contextKey = "gh-uts-token"               // #nosec G101 -- typed context key, not a credential.
const ctxPersonalAccessToken contextKey = "gh-personal-access-token" // #nosec G101 -- typed context key, not a credential.
const ctxSuspendedInstallation contextKey = "gh-suspended-installation"
const ctxSuspendedUser contextKey = "gh-suspended-user"
const ctxGitHubAPIVersion contextKey = "gh-api-version"
const ctxAPIRateLimit contextKey = "gh-api-rate-limit"

const defaultGitHubAPIVersion = "2022-11-28"

var supportedGitHubAPIVersions = []string{defaultGitHubAPIVersion, "2026-03-10"}

func isSupportedGitHubAPIVersion(version string) bool {
	for _, supported := range supportedGitHubAPIVersions {
		if version == supported {
			return true
		}
	}
	return false
}

func githubAPIVersionFromContext(ctx context.Context) string {
	version, _ := ctx.Value(ctxGitHubAPIVersion).(string)
	if version == "" {
		return defaultGitHubAPIVersion
	}
	return version
}

// ctxInvalidCredential marks a request that presented an Authorization header
// which resolved to nothing. Absent credentials are anonymous; presented ones
// that do not verify are rejected, never downgraded to anonymous.
const ctxInvalidCredential contextKey = "gh-invalid-credential" // #nosec G101 -- typed context key, not a credential.

// GitHub token prefixes. Each prefix selects a different lookup table and
// auth shape in authenticateRequest; using the named constants keeps the
// middleware, stores and handlers agreeing on the exact prefix bytes.
const (
	// #nosec G101 -- public token type prefix, not a credential.
	tokenPrefixInstallation = "ghs_"
	tokenPrefixOAuthUser    = "gho_" // classic OAuth-App user token
	tokenPrefixAppUser      = "ghu_" // GitHub-App user-to-server token
	tokenPrefixRefresh      = "ghr_" // refresh token (never valid as auth)
)

// ghUserFromContext extracts the authenticated user from the request context.
func ghUserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxUser).(*User)
	return u
}

// contextWithUser carries an authenticated user on a context built outside the
// HTTP middleware — the SSH transport has no request to attach one to. A nil
// user is left off entirely so ghUserFromContext keeps returning nil rather
// than a typed nil.
func contextWithUser(ctx context.Context, user *User) context.Context {
	if user == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxUser, user)
}

// ghAppFromContext extracts the JWT-authenticated app from the request context.
func ghAppFromContext(ctx context.Context) *App {
	a, _ := ctx.Value(ctxApp).(*App)
	return a
}

// ghInstallationFromContext extracts the installation associated with the request,
// if authenticated by a ghs_ installation token. Returns nil for other auth shapes.
// Consumed by gh_apps_rest.go (installation introspection) and the permission
// decorator.
func ghInstallationFromContext(ctx context.Context) *Installation {
	i, _ := ctx.Value(ctxInstallation).(*Installation)
	return i
}

// ghInstallationTokenFromContext extracts the installation token used to authenticate
// the request, if any. Consumed by gh_apps_perms.go (permission decorator) and
// gh_apps_rest.go (introspection endpoints).
func ghInstallationTokenFromContext(ctx context.Context) *InstallationToken {
	t, _ := ctx.Value(ctxInstallationToken).(*InstallationToken)
	return t
}

// ghUserToServerTokenFromContext extracts the gho_/ghu_ token used to authenticate,
// if any. Consumed by gh_apps_perms.go (permission decorator's user-to-server path).
func ghUserToServerTokenFromContext(ctx context.Context) *UserToServerToken {
	t, _ := ctx.Value(ctxUserToServerToken).(*UserToServerToken)
	return t
}

func ghPersonalAccessTokenFromContext(ctx context.Context) *Token {
	t, _ := ctx.Value(ctxPersonalAccessToken).(*Token)
	return t
}

// credentialConveysSiteAdmin reports whether the request's credential SHAPE is
// allowed to exercise site-administrator (or enterprise-owner) authority. A
// browser session or a classic PAT — the account's broad credentials — may;
// a fine-grained PAT, an OAuth/GitHub-App user-to-server token, an installation
// token, or a bare app JWT is deliberately narrow or delegated and must never
// confer appliance administration, even when its user record is a SiteAdmin.
// Without this, a site admin who authorizes any app, or mints a fine-grained
// PAT scoped to a single repo, would hand that narrow credential full control
// of the instance (the admin gates check only user.SiteAdmin).
func credentialConveysSiteAdmin(ctx context.Context) bool {
	if pat := ghPersonalAccessTokenFromContext(ctx); pat != nil && pat.FineGrained {
		return false
	}
	return ghUserToServerTokenFromContext(ctx) == nil &&
		ghInstallationTokenFromContext(ctx) == nil &&
		ghAppFromContext(ctx) == nil
}

// ghHeadersMiddleware injects GitHub-compatible response headers on /api/ routes
// and sets the authenticated user in request context.
func (s *Server) ghHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Activate for the REST API plus the uploads-host and authenticated
		// CodeQL storage paths. The official CodeQL Action posts database
		// bundles to /repos/... on uploads.github.com rather than /api/v3/.
		// Runner protocol (/_apis/) remains unaffected.
		if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/repos/") && !strings.HasPrefix(path, "/code-scanning/") {
			next.ServeHTTP(w, r)
			return
		}

		ctx := s.authenticateRequest(r)
		r = r.WithContext(ctx)
		apiVersion := defaultGitHubAPIVersion
		// Calendar API versions apply to REST. GraphQL has one continuously
		// evolving schema, so a REST version header must not accidentally
		// retire an otherwise valid /api/graphql request.
		isVersionedREST := path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") ||
			strings.HasPrefix(path, "/repos/") || strings.HasPrefix(path, "/code-scanning/")
		if isVersionedREST {
			if requested := r.Header.Get("X-GitHub-Api-Version"); requested != "" {
				apiVersion = requested
			}

			// GitHub keeps calendar API versions alive for a published support
			// window. A caller that explicitly asks for anything else receives
			// 410 rather than silently running against a different contract.
			if !isSupportedGitHubAPIVersion(apiVersion) {
				rw := &ghResponseWriter{ResponseWriter: w, path: path, apiVersion: apiVersion}
				writeGHError(rw, http.StatusGone, "The requested API version is no longer supported.")
				return
			}
		}
		ctx = context.WithValue(ctx, ctxGitHubAPIVersion, apiVersion)
		r = r.WithContext(ctx)

		// A credential that was presented and did not resolve is an error, not
		// an anonymous request: continuing would silently downgrade a revoked,
		// expired or forged token to "no credential" and answer whatever the
		// public surface allows.
		if bad, _ := ctx.Value(ctxInvalidCredential).(bool); bad {
			writeGHError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}

		// A suspended installation's tokens are dead for the entire API
		// surface (real GitHub fails every request made with the app's
		// credentials while suspended), not just for minting new tokens.
		if susp, _ := ctx.Value(ctxSuspendedInstallation).(bool); susp {
			writeGHError(w, http.StatusForbidden, "This installation has been suspended")
			return
		}
		if susp, _ := ctx.Value(ctxSuspendedUser).(bool); susp {
			writeGHError(w, http.StatusForbidden, "This account has been suspended")
			return
		}

		// X-OAuth-Scopes reports the scopes of whichever credential
		// authenticated the request; authenticateRequest already resolved it,
		// so re-parsing the header here would be a second, divergent parse.
		var token *Token
		if pat := ghPersonalAccessTokenFromContext(ctx); pat != nil {
			token = pat
		} else if uts := ghUserToServerTokenFromContext(ctx); uts != nil {
			// Materialize a transient classic token so the response writer can
			// emit X-OAuth-Scopes for OAuth/GitHub-App user-to-server tokens,
			// matching real GitHub.
			token = &Token{Value: uts.Token, UserID: uts.UserID, Scopes: uts.Scopes}
		}

		// Wrap response writer to inject headers
		resource := apiRateResource(path)
		// GitHub documents GET /rate_limit as not consuming the primary rate
		// limit; it observes the current core window instead.
		rate := s.rateLimitSnapshot(r, resource, path != "/api/v3/rate_limit")
		ctx = context.WithValue(ctx, ctxAPIRateLimit, rate)
		r = r.WithContext(ctx)
		rw := &ghResponseWriter{
			ResponseWriter: w,
			token:          token,
			path:           path,
			apiVersion:     apiVersion,
			method:         r.Method,
			ifNoneMatch:    r.Header.Get("If-None-Match"),
			rateLimit:      rate,
		}
		if rate.Exceeded {
			seconds := max(int(time.Until(time.Unix(rate.Reset, 0)).Seconds()), 1)
			rw.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeGHError(rw, http.StatusForbidden, "API rate limit exceeded")
			return
		}
		if field := invalidRESTPaginationQuery(r); strings.HasPrefix(path, "/api/v3/") && field != "" {
			writeGHValidationError(rw, "Pagination", field, "invalid")
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// authScheme splits an Authorization header into its lower-cased scheme and
// the credential. HTTP auth schemes are case-insensitive (RFC 7235), and
// GitHub accepts "token"/"Bearer"/"Basic" in any case (octokit sends "bearer"),
// so match case-insensitively rather than on an exact-case prefix.
func authScheme(auth string) (scheme, credential string) {
	s, cred, found := strings.Cut(auth, " ")
	if !found {
		return "", ""
	}
	return strings.ToLower(s), cred
}

// authenticateRequest parses the Authorization header and returns a context
// with the authenticated user/app/installation set. Used by both /api/
// middleware and git HTTP handlers.
//
// A header that resolves to nothing sets ctxInvalidCredential rather than
// falling through to the anonymous path; ghHeadersMiddleware turns that into
// the 401 the caller earned. Callers outside that middleware must consult the
// flag themselves — resolving to no principal is not the same answer as
// "no credential was offered".
// resolveBearerCredential resolves one token string (from an Authorization
// "token"/"bearer" scheme, or from the password half of Basic — the
// x-access-token convention) to a credential. It returns the augmented context,
// the human principal the token belongs to (nil for app/installation/runner
// credentials, which carry no personal user), and whether the token resolved to
// anything. It deliberately does NOT place a resolved user on the context: a
// suspended user must not gain a principal, so the sole caller decides that
// after checking user.Suspended.
func (s *Server) resolveBearerCredential(ctx context.Context, tokenStr string) (context.Context, *User, bool) {
	switch {
	case looksLikeJWT(tokenStr):
		if app, err := s.store.parseAndVerifyAppJWT(tokenStr); err == nil {
			return context.WithValue(ctx, ctxApp, app), nil, true
		}
		return ctx, nil, false
	case strings.HasPrefix(tokenStr, tokenPrefixInstallation):
		instToken, inst := s.store.LookupInstallationToken(tokenStr)
		if instToken == nil {
			return ctx, nil, false
		}
		if inst != nil && inst.SuspendedAt != nil {
			return context.WithValue(ctx, ctxSuspendedInstallation, true), nil, true
		}
		ctx = context.WithValue(ctx, ctxInstallation, inst)
		ctx = context.WithValue(ctx, ctxInstallationToken, instToken)
		if app := s.store.GetApp(instToken.AppID); app != nil {
			ctx = context.WithValue(ctx, ctxUser, appBotUser(app))
		}
		return ctx, nil, true
	case strings.HasPrefix(tokenStr, tokenPrefixOAuthUser), strings.HasPrefix(tokenStr, tokenPrefixAppUser):
		if utsTok, u := s.store.LookupUserToServerToken(tokenStr); utsTok != nil {
			return context.WithValue(ctx, ctxUserToServerToken, utsTok), u, true
		}
		return ctx, nil, false
	case strings.HasPrefix(tokenStr, tokenPrefixRefresh):
		// A refresh token is never an access credential.
		return ctx, nil, false
	default:
		if token, resolved := s.store.LookupToken(tokenStr); token != nil {
			return context.WithValue(ctx, ctxPersonalAccessToken, token), resolved, true
		}
		return ctx, nil, false
	}
}

func (s *Server) authenticateRequest(r *http.Request) context.Context {
	ctx := r.Context()
	var user *User
	authOffered := false
	if auth := r.Header.Get("Authorization"); auth != "" {
		authOffered = true
		credentialResolved := false
		scheme, cred := authScheme(auth)
		switch {
		case (scheme == "token" || scheme == "bearer") && cred != "":
			ctx, user, credentialResolved = s.resolveBearerCredential(ctx, cred)
		case scheme == "basic":
			if decoded, err := base64.StdEncoding.DecodeString(cred); err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				// An empty password is the anonymous Basic form the OCI
				// registry clients send; it offers no credential to reject.
				credentialResolved = len(parts) == 2 && parts[1] == ""
				if len(parts) == 2 && parts[1] != "" {
					// The password half carries the bearer token (git and OCI
					// clients send `x-access-token:<token>` or `<anything>:<pat>`),
					// so resolve it through the same switch as the token scheme —
					// ghs_/ghu_/gho_ passwords authenticate identically here.
					if c2, u, ok := s.resolveBearerCredential(ctx, parts[1]); ok {
						ctx, user, credentialResolved = c2, u, true
					} else if s.clientCredentialsVerify(parts[0], parts[1]) {
						// client_id:client_secret for the OAuth app management
						// endpoints. It authenticates an app, not a user, and
						// those handlers verify it again themselves.
						credentialResolved = true
					}
				}
			}
		}
		// Runner protocol credentials authenticate a runner rather than a
		// user, so they resolve to no principal here — but they are real
		// credentials this server minted and verifies, not rejects.
		if !credentialResolved && runnerCredentialVerifies(runnerCredentialOffered(scheme, cred)) {
			credentialResolved = true
		}
		if !credentialResolved {
			ctx = context.WithValue(ctx, ctxInvalidCredential, true)
		}
	}
	// A browser session augments the request only when NO Authorization header
	// was offered. A request that presents an explicit (even invalid) token is
	// judged on that token alone: falling back to the cookie would let a revoked
	// or expired bearer be silently served as the cookie's user.
	if user == nil && !authOffered {
		if session := s.sessionFromRequest(r); session != nil {
			user = s.store.GetUserByID(session.UserID)
		}
	}
	if user != nil {
		if user.Suspended {
			ctx = context.WithValue(ctx, ctxSuspendedUser, true)
		} else {
			ctx = context.WithValue(ctx, ctxUser, user)
		}
	}
	return ctx
}

// runnerCredentialOffered returns the credential a request offers to the
// runner protocol, or "" when it offers none. Alongside bearer and token,
// actions/runner presents its registration and removal tokens under the
// RemoteAuth scheme — a scheme that names no user credential, so the
// user-facing resolution above never looks at it.
func runnerCredentialOffered(scheme, credential string) string {
	switch scheme {
	case "token", "bearer", "remoteauth":
		return credential
	}
	return ""
}

// runnerCredentialVerifies reports whether a bearer credential is one of the
// runner-protocol credentials this server signs: an agent session or job
// token, or a registration/removal token.
func runnerCredentialVerifies(token string) bool {
	if token == "" {
		return false
	}
	if _, err := parseRunnerToken(token); err == nil {
		return true
	}
	var claims runnerRegistrationClaims
	return parseSignedBlob("A", token, &claims) == nil
}

// clientCredentialsVerify reports whether a Basic username/password pair is a
// registered OAuth App or GitHub App client id and secret.
func (s *Server) clientCredentialsVerify(clientID, secret string) bool {
	return s.store.VerifyOAuthAppSecret(clientID, secret) != nil ||
		s.store.VerifyAppClientSecret(clientID, secret) != nil
}

// ghResponseWriter injects GitHub API headers before the first write.
type ghResponseWriter struct {
	http.ResponseWriter
	token       *Token
	path        string
	apiVersion  string
	method      string
	ifNoneMatch string
	rateLimit   apiRateSnapshot
	wroteHeader bool
}

func (rw *ghResponseWriter) conditionalJSON(etag string, status int) bool {
	if rw.method != http.MethodGet || status != http.StatusOK {
		return false
	}
	rw.Header().Set("ETag", etag)
	return etagMatches(rw.ifNoneMatch, etag)
}

// etagMatches reports whether an If-None-Match header value matches etag,
// honouring the wildcard (`*`) and weak-validator (`W/`) forms GitHub accepts.
func etagMatches(ifNoneMatch, etag string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func (rw *ghResponseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
		h := rw.Header()

		// Upgrade Content-Type to include charset
		if ct := h.Get("Content-Type"); ct == "application/json" {
			h.Set("Content-Type", "application/json; charset=utf-8")
		}

		if rw.token != nil {
			h.Set("X-OAuth-Scopes", rw.token.Scopes)
		}
		h.Set("X-Accepted-OAuth-Scopes", "")

		rate := rw.rateLimit
		if rate.Limit == 0 {
			resource := apiRateResource(rw.path)
			rate = apiRateSnapshot{
				Resource: resource, Limit: apiRateResourceLimits[resource],
				Used: 1, Remaining: apiRateResourceLimits[resource] - 1,
				Reset: time.Now().Add(apiRateWindowDuration(resource)).Unix(),
			}
		}
		h.Set("X-RateLimit-Limit", fmt.Sprintf("%d", rate.Limit))
		h.Set("X-RateLimit-Remaining", fmt.Sprintf("%d", rate.Remaining))
		h.Set("X-RateLimit-Used", fmt.Sprintf("%d", rate.Used))
		h.Set("X-RateLimit-Reset", fmt.Sprintf("%d", rate.Reset))
		h.Set("X-RateLimit-Resource", rate.Resource)
		h.Set("X-GitHub-Request-Id", uuid.New().String())
		apiVersion := rw.apiVersion
		if apiVersion == "" {
			apiVersion = defaultGitHubAPIVersion
		}
		h.Set("X-GitHub-Api-Version", apiVersion)
		h.Set("X-GitHub-Media-Type", "github.v3; format=json")

		// Activity feeds and the notifications list advertise a polling
		// interval so clients pace their (now conditional, ETag-backed) polling
		// (REST-031). Only on GET, and never override a handler that already
		// set one.
		if rw.method == http.MethodGet && h.Get("X-Poll-Interval") == "" {
			if interval := pollIntervalForPath(rw.path); interval > 0 {
				h.Set("X-Poll-Interval", strconv.Itoa(interval))
			}
		}
	}
	rw.ResponseWriter.WriteHeader(code)
}

// pollIntervalForPath returns GitHub's advertised polling interval in seconds
// for the activity event feeds and the notifications list — the endpoints that
// carry X-Poll-Interval — and 0 for everything else. The issues-events resource
// (`.../issues/events`) is a plain list, not an activity feed, and carries none.
func pollIntervalForPath(path string) int {
	if strings.Contains(path, "/issues/events") {
		return 0
	}
	switch {
	case path == "/api/v3/events",
		path == "/api/v3/notifications",
		strings.HasSuffix(path, "/events"),
		strings.HasSuffix(path, "/events/public"),
		strings.HasSuffix(path, "/received_events"),
		strings.HasSuffix(path, "/received_events/public"),
		strings.HasSuffix(path, "/notifications"):
		return 60
	}
	return 0
}

// writeLastModified advertises the modification time of a feed and, when the
// caller sent a matching If-Modified-Since, short-circuits with a bodyless 304
// (REST-031). It sets Last-Modified to newest (truncated to HTTP second
// precision) and returns true when it wrote the 304, so the handler must stop.
//
// A zero newest (an empty feed) advertises nothing and never 304s. Per RFC 7232
// §3.3 If-None-Match takes precedence, so If-Modified-Since is ignored whenever
// an If-None-Match is present — writeJSON's ETag path evaluates that instead.
func writeLastModified(w http.ResponseWriter, r *http.Request, newest time.Time) bool {
	if newest.IsZero() {
		return false
	}
	newest = newest.UTC().Truncate(time.Second)
	w.Header().Set("Last-Modified", newest.Format(http.TimeFormat))
	if r.Header.Get("If-None-Match") != "" {
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if since, err := http.ParseTime(ims); err == nil && !newest.After(since) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

// Unwrap lets net/http's ResponseController reach optional interfaces on the
// real writer even though GitHub headers are injected through this wrapper.
func (rw *ghResponseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func (rw *ghResponseWriter) Flush() {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(rw.ResponseWriter).Flush()
}

func (rw *ghResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(rw.ResponseWriter).Hijack()
}

func (rw *ghResponseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}
