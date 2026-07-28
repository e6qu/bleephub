package bleephub

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const ctxUser contextKey = "gh-user"
const ctxApp contextKey = "gh-app"
const ctxInstallation contextKey = "gh-installation"
const ctxInstallationToken contextKey = "gh-installation-token"
const ctxUserToServerToken contextKey = "gh-uts-token"
const ctxPersonalAccessToken contextKey = "gh-personal-access-token"
const ctxSuspendedInstallation contextKey = "gh-suspended-installation"
const ctxSuspendedUser contextKey = "gh-suspended-user"
const ctxGitHubAPIVersion contextKey = "gh-api-version"

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
const ctxInvalidCredential contextKey = "gh-invalid-credential"

// GitHub token prefixes. Each prefix selects a different lookup table and
// auth shape in authenticateRequest; using the named constants keeps the
// middleware, stores and handlers agreeing on the exact prefix bytes.
const (
	tokenPrefixInstallation = "ghs_" // installation access token
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
		rw := &ghResponseWriter{
			ResponseWriter: w,
			token:          token,
			path:           path,
			apiVersion:     apiVersion,
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
func (s *Server) authenticateRequest(r *http.Request) context.Context {
	ctx := r.Context()
	var user *User
	if auth := r.Header.Get("Authorization"); auth != "" {
		credentialResolved := false
		scheme, cred := authScheme(auth)
		var tokenStr string
		if scheme == "token" || scheme == "bearer" {
			tokenStr = cred
		}
		if tokenStr != "" {
			switch {
			case looksLikeJWT(tokenStr):
				if app, err := s.store.parseAndVerifyAppJWT(tokenStr); err == nil {
					ctx = context.WithValue(ctx, ctxApp, app)
					credentialResolved = true
				}
			case strings.HasPrefix(tokenStr, tokenPrefixInstallation):
				if instToken, inst := s.store.LookupInstallationToken(tokenStr); instToken != nil {
					credentialResolved = true
					if inst != nil && inst.SuspendedAt != nil {
						ctx = context.WithValue(ctx, ctxSuspendedInstallation, true)
						break
					}
					ctx = context.WithValue(ctx, ctxInstallation, inst)
					ctx = context.WithValue(ctx, ctxInstallationToken, instToken)
					app := s.store.GetApp(instToken.AppID)
					if app != nil {
						ctx = context.WithValue(ctx, ctxUser, appBotUser(app))
					}
				}
			case strings.HasPrefix(tokenStr, tokenPrefixOAuthUser), strings.HasPrefix(tokenStr, tokenPrefixAppUser):
				if utsTok, u := s.store.LookupUserToServerToken(tokenStr); utsTok != nil {
					credentialResolved = true
					ctx = context.WithValue(ctx, ctxUserToServerToken, utsTok)
					if u != nil {
						ctx = context.WithValue(ctx, ctxUser, u)
						user = u
					}
				}
			case strings.HasPrefix(tokenStr, tokenPrefixRefresh):
				// A refresh token is never an access credential.
			default:
				if token, resolved := s.store.LookupToken(tokenStr); token != nil {
					credentialResolved = true
					user = resolved
					ctx = context.WithValue(ctx, ctxPersonalAccessToken, token)
				}
			}
		} else if scheme == "basic" {
			decoded, err := base64.StdEncoding.DecodeString(cred)
			if err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				// An empty password is the anonymous Basic form the OCI
				// registry clients send; it offers no credential to reject.
				credentialResolved = len(parts) == 2 && parts[1] == ""
				if len(parts) == 2 && parts[1] != "" {
					if token, resolved := s.store.LookupToken(parts[1]); token != nil {
						credentialResolved = true
						user = resolved
						ctx = context.WithValue(ctx, ctxPersonalAccessToken, token)
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
	if user == nil {
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
	wroteHeader bool
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

		now := time.Now()
		h.Set("X-RateLimit-Limit", "5000")
		h.Set("X-RateLimit-Remaining", "4999")
		h.Set("X-RateLimit-Used", "1")
		h.Set("X-RateLimit-Reset", fmt.Sprintf("%d", now.Unix()+3600))

		resource := "core"
		if strings.HasPrefix(rw.path, "/api/graphql") {
			resource = "graphql"
		}
		h.Set("X-RateLimit-Resource", resource)
		h.Set("X-GitHub-Request-Id", uuid.New().String())
		apiVersion := rw.apiVersion
		if apiVersion == "" {
			apiVersion = defaultGitHubAPIVersion
		}
		h.Set("X-GitHub-Api-Version", apiVersion)
		h.Set("X-GitHub-Media-Type", "github.v3; format=json")
	}
	rw.ResponseWriter.WriteHeader(code)
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
