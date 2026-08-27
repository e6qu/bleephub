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

	"github.com/e6qu/bleephub/internal/store"
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
const ctxJobToken contextKey = "gh-job-token" // #nosec G101 -- typed context key, not a credential.
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

// ctxInvalidCredential marks a request whose Authorization header resolved to
// nothing. A presented-but-unverified credential is rejected, never downgraded
// to anonymous.
const ctxInvalidCredential contextKey = "gh-invalid-credential" // #nosec G101 -- typed context key, not a credential.

func ghUserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxUser).(*store.User)
	return u
}

// contextWithUser carries an authenticated user on a context built outside the
// HTTP middleware (the SSH transport has no request). A nil user is left off so
// ghUserFromContext returns nil rather than a typed nil.
func contextWithUser(ctx context.Context, user *store.User) context.Context {
	if user == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxUser, user)
}

func ghAppFromContext(ctx context.Context) *store.App {
	a, _ := ctx.Value(ctxApp).(*store.App)
	return a
}

// ghInstallationFromContext returns the installation when the request
// authenticated with a ghs_ installation token, nil otherwise.
func ghInstallationFromContext(ctx context.Context) *store.Installation {
	i, _ := ctx.Value(ctxInstallation).(*store.Installation)
	return i
}

func ghInstallationTokenFromContext(ctx context.Context) *store.InstallationToken {
	t, _ := ctx.Value(ctxInstallationToken).(*store.InstallationToken)
	return t
}

func ghUserToServerTokenFromContext(ctx context.Context) *store.UserToServerToken {
	t, _ := ctx.Value(ctxUserToServerToken).(*store.UserToServerToken)
	return t
}

// jobTokenPrincipal is the caller a workflow's GITHUB_TOKEN authenticates as: a
// single repository plus its least-privilege permission set (ACT-014).
// requirePerm gates access to Repo alone, at the granted scopes/levels.
type jobTokenPrincipal struct {
	Repo  string
	Perms map[string]string
}

func ghJobTokenFromContext(ctx context.Context) *jobTokenPrincipal {
	t, _ := ctx.Value(ctxJobToken).(*jobTokenPrincipal)
	return t
}

func ghPersonalAccessTokenFromContext(ctx context.Context) *store.Token {
	t, _ := ctx.Value(ctxPersonalAccessToken).(*store.Token)
	return t
}

// credentialConveysSiteAdmin reports whether the credential SHAPE may exercise
// site-admin/enterprise-owner authority. Only broad credentials (browser
// session, classic PAT) qualify; a fine-grained PAT, a user-to-server or
// installation token, or a bare app JWT never confers appliance administration
// even when its user record is a SiteAdmin, since the admin gates check only
// user.SiteAdmin.
func credentialConveysSiteAdmin(ctx context.Context) bool {
	if pat := ghPersonalAccessTokenFromContext(ctx); pat != nil && pat.FineGrained {
		return false
	}
	return ghUserToServerTokenFromContext(ctx) == nil &&
		ghInstallationTokenFromContext(ctx) == nil &&
		ghAppFromContext(ctx) == nil
}

// ghHeadersMiddleware injects GitHub response headers on the REST surface and
// sets the authenticated user in request context.
func (s *Server) ghHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// /repos/ and /code-scanning/ are the uploads-host paths the CodeQL
		// Action posts to; the runner protocol (/_apis/) stays unaffected.
		if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/repos/") && !strings.HasPrefix(path, "/code-scanning/") {
			next.ServeHTTP(w, r)
			return
		}

		ctx := s.authenticateRequest(r)
		r = r.WithContext(ctx)
		apiVersion := defaultGitHubAPIVersion
		// Calendar versions apply to REST only; GraphQL has one evolving schema,
		// so a REST version header must not retire a valid /api/graphql request.
		isVersionedREST := path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") ||
			strings.HasPrefix(path, "/repos/") || strings.HasPrefix(path, "/code-scanning/")
		if isVersionedREST {
			if requested := r.Header.Get("X-GitHub-Api-Version"); requested != "" {
				apiVersion = requested
			}

			// An unsupported explicit version is 410, not a silent run against a
			// different contract.
			if !isSupportedGitHubAPIVersion(apiVersion) {
				rw := &ghResponseWriter{ResponseWriter: w, path: path, apiVersion: apiVersion}
				writeGHError(rw, http.StatusGone, "The requested API version is no longer supported.")
				return
			}
		}
		ctx = context.WithValue(ctx, ctxGitHubAPIVersion, apiVersion)
		r = r.WithContext(ctx)

		// A presented-but-unresolved credential is a 401, never a downgrade to
		// anonymous.
		if bad, _ := ctx.Value(ctxInvalidCredential).(bool); bad {
			writeGHError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}

		// A suspended installation's tokens are dead across the whole API
		// surface, not just for minting new tokens.
		if susp, _ := ctx.Value(ctxSuspendedInstallation).(bool); susp {
			writeGHError(w, http.StatusForbidden, "This installation has been suspended")
			return
		}
		if susp, _ := ctx.Value(ctxSuspendedUser).(bool); susp {
			writeGHError(w, http.StatusForbidden, "This account has been suspended")
			return
		}

		var token *store.Token
		if pat := ghPersonalAccessTokenFromContext(ctx); pat != nil {
			token = pat
		} else if uts := ghUserToServerTokenFromContext(ctx); uts != nil {
			// Materialize a transient token so X-OAuth-Scopes can be emitted for
			// user-to-server tokens.
			token = &store.Token{Value: uts.Token, UserID: uts.UserID, Scopes: uts.Scopes}
		}

		resource := apiRateResource(path)
		// GET /rate_limit observes the core window without consuming it.
		consumed := path != "/api/v3/rate_limit"
		rate := s.rateLimitSnapshot(r, resource, consumed)
		ctx = context.WithValue(ctx, ctxAPIRateLimit, rate)
		r = r.WithContext(ctx)
		refundReq := r
		rw := &ghResponseWriter{
			ResponseWriter: w,
			token:          token,
			path:           path,
			apiVersion:     apiVersion,
			method:         r.Method,
			ifNoneMatch:    r.Header.Get("If-None-Match"),
			rateLimit:      rate,
		}
		// A conditional GET that ends in 304 is not billed; refund the unit.
		if consumed {
			rw.refundRate = func() apiRateSnapshot { return s.refundRateLimit(refundReq, resource) }
		}
		if rate.Exceeded {
			seconds := max(int(time.Until(time.Unix(rate.Reset, 0)).Seconds()), 1)
			rw.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeGHError(rw, http.StatusForbidden, "API rate limit exceeded")
			return
		}
		if field := invalidRESTPaginationQuery(r); strings.HasPrefix(path, "/api/v3/") && field != "" {
			store.WriteGHValidationError(rw, "Pagination", field, "invalid")
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// authScheme splits an Authorization header into its lower-cased scheme and the
// credential; HTTP auth schemes are case-insensitive (RFC 7235).
func authScheme(auth string) (scheme, credential string) {
	s, cred, found := strings.Cut(auth, " ")
	if !found {
		return "", ""
	}
	return strings.ToLower(s), cred
}

// resolveBearerCredential resolves one token string (bearer/token scheme, or
// the password half of Basic under the x-access-token convention) to a
// credential, returning the augmented context, the human principal the token
// belongs to (nil for app/installation/runner credentials), and whether it
// resolved. It does NOT place the resolved user on the context: a suspended
// user must not gain a principal, so the sole caller decides after checking
// user.Suspended.
func (s *Server) resolveBearerCredential(ctx context.Context, tokenStr string) (context.Context, *store.User, bool) {
	switch {
	case looksLikeJWT(tokenStr):
		if app, err := s.store.ParseAndVerifyAppJWT(tokenStr); err == nil {
			return context.WithValue(ctx, ctxApp, app), nil, true
		}
		// A workflow's GITHUB_TOKEN is an HS256 job token, not an App JWT.
		// Recognize it as a repo-scoped least-privilege principal so its calls
		// are gated by the workflow's permissions (ACT-014). A session token (no
		// Repo) is left for the runner-protocol gate.
		if claims, err := parseRunnerToken(tokenStr); err == nil && claims.Aud == runnerAudJob && claims.Repo != "" {
			principal := &jobTokenPrincipal{Repo: claims.Repo, Perms: claims.Perms}
			// Acts as github-actions[bot] for attribution; the gate is governed
			// by the jobTokenPrincipal, not this user.
			return context.WithValue(ctx, ctxJobToken, principal), store.ActionsBotUser(), true
		}
		return ctx, nil, false
	case strings.HasPrefix(tokenStr, store.TokenPrefixInstallation):
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
			ctx = context.WithValue(ctx, ctxUser, store.AppBotUser(app))
		}
		return ctx, nil, true
	case strings.HasPrefix(tokenStr, store.TokenPrefixOAuthUser), strings.HasPrefix(tokenStr, store.TokenPrefixAppUser):
		if utsTok, u := s.store.LookupUserToServerToken(tokenStr); utsTok != nil {
			return context.WithValue(ctx, ctxUserToServerToken, utsTok), u, true
		}
		return ctx, nil, false
	case strings.HasPrefix(tokenStr, store.TokenPrefixRefresh):
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
	var user *store.User
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
				// An empty password is the anonymous Basic form OCI registry
				// clients send; it offers no credential to reject.
				credentialResolved = len(parts) == 2 && parts[1] == ""
				if len(parts) == 2 && parts[1] != "" {
					// The password half carries the bearer token, so resolve it
					// through the same switch as the token scheme.
					if c2, u, ok := s.resolveBearerCredential(ctx, parts[1]); ok {
						ctx, user, credentialResolved = c2, u, true
					} else if s.clientCredentialsVerify(parts[0], parts[1]) {
						// client_id:client_secret for OAuth app management
						// endpoints, which re-verify it themselves.
						credentialResolved = true
					}
				}
			}
		}
		// Runner-protocol credentials resolve to no principal but are real
		// credentials this server minted and verifies, not rejects.
		if !credentialResolved && runnerCredentialVerifies(runnerCredentialOffered(scheme, cred)) {
			credentialResolved = true
		}
		if !credentialResolved {
			ctx = context.WithValue(ctx, ctxInvalidCredential, true)
		}
	}
	// Fall back to the browser session only when NO Authorization header was
	// offered; a request presenting a token is judged on that token alone, so a
	// revoked bearer is never silently served as the cookie's user.
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

// runnerCredentialOffered returns the credential a request offers the runner
// protocol, or "". Registration/removal tokens arrive under the RemoteAuth
// scheme, which the user-facing resolution never inspects.
func runnerCredentialOffered(scheme, credential string) string {
	switch scheme {
	case "token", "bearer", "remoteauth":
		return credential
	}
	return ""
}

// runnerCredentialVerifies reports whether a bearer credential is one this
// server signs for the runner protocol: an agent session/job token, or a
// registration/removal token.
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
	token       *store.Token
	path        string
	apiVersion  string
	method      string
	ifNoneMatch string
	rateLimit   apiRateSnapshot
	// refundRate refunds the consumed rate-limit unit and returns the
	// post-refund snapshot; set only when a unit was consumed, invoked on a 304.
	refundRate  func() apiRateSnapshot
	wroteHeader bool
}

func (rw *ghResponseWriter) conditionalJSON(etag string, status int) bool {
	if rw.method != http.MethodGet || status != http.StatusOK {
		return false
	}
	rw.Header().Set("ETag", etag)
	return etagMatches(rw.ifNoneMatch, etag)
}

// etagMatches reports whether an If-None-Match value matches etag, honouring
// the wildcard (`*`) and weak-validator (`W/`) forms.
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
		// A 304 is not billed: refund the unit so this response's X-RateLimit-*
		// headers reflect the non-consumption.
		if code == http.StatusNotModified && rw.refundRate != nil {
			rw.rateLimit = rw.refundRate()
		}
		h := rw.Header()

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
		// A handler that served a custom media type already set this header.
		if h.Get("X-GitHub-Media-Type") == "" {
			h.Set("X-GitHub-Media-Type", "github.v3; format=json")
		}

		// Activity feeds and the notifications list advertise a polling interval
		// (REST-031). GET only, never overriding a handler that set one.
		if rw.method == http.MethodGet && h.Get("X-Poll-Interval") == "" {
			if interval := pollIntervalForPath(rw.path); interval > 0 {
				h.Set("X-Poll-Interval", strconv.Itoa(interval))
			}
		}
	}
	rw.ResponseWriter.WriteHeader(code)
}

// pollIntervalForPath returns the advertised X-Poll-Interval in seconds for the
// activity event feeds and notifications list, 0 otherwise. `.../issues/events`
// is a plain list, not an activity feed, and carries none.
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

// writeLastModified sets Last-Modified to newest and, on a matching
// If-Modified-Since, writes a bodyless 304 and returns true so the handler stops
// (REST-031). A zero newest advertises nothing and never 304s. Per RFC 7232
// §3.3, If-None-Match takes precedence, so If-Modified-Since is ignored whenever
// one is present.
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
// wrapped writer.
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
