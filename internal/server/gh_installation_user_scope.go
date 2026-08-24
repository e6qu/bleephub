package bleephub

import (
	"net/http"
	"strings"
)

// An installation token authenticates a GitHub App's installation on an
// account. It is not a user credential, and the distinction is load-bearing:
// it is how a client tells "this App did it" from "this person did it".
//
// bleephub materializes a Bot actor for an installation token so the App's
// writes are attributed to `<slug>[bot]` rather than to nobody
// (store.AppBotUser). That actor exists to be an author, not an account — the
// installation holds no profile, no email addresses, no keys, no
// organization memberships of its own. So the endpoints whose subject is *the
// authenticated account* have no answer to give an installation token, and
// GitHub refuses them with `403 Resource not accessible by integration` rather
// than describing the bot as though it were the signed-in user.
//
// Answering them instead was worse than a missing feature. `GET /user`
// returning 200 with login `<slug>[bot]` tells a client it holds a user
// credential when it does not: every write the App makes gets attributed to
// that login as if a person had made it, and a client that branches on
// "can I read /user?" to choose between a user flow and an App flow takes the
// wrong branch outright.
//
// The vendored REST description carries this per operation as
// `x-github.enabledForGitHubApps: false`, and
// TestInstallationTokenRefusalMatchesVendoredContract holds this gate against
// it, so the set below cannot drift from the published contract.

// userAccountScopedPrefix is the route prefix whose subject is, as a rule, the
// authenticated account itself. `/api/v3/user` and everything beneath it
// describe, or act on, whoever the credential belongs to — which an
// installation is not. `/api/v3/users/{username}` is deliberately outside it:
// those name their subject explicitly, so an App reading them is not claiming
// to be anybody.
const userAccountScopedPrefix = "/api/v3/user"

// installationReachableUserRoutes are the routes under that prefix that GitHub
// nonetheless serves to an installation token, keyed by the exact registered
// pattern. Each is one the prefix alone gets wrong:
//
//   - the ones that name their subject in the path rather than taking it from
//     the credential (`/user/{account_id}`, `/user/{user_id}/projectsV2/...`),
//     so there is no "who am I" for the installation to have to answer;
//   - organization membership, which GitHub exposes to an App because the
//     installation's own account standing is what it reports; and
//   - writing social accounts, which GitHub leaves open to Apps even though it
//     closes the matching read — a lopsidedness in the published description
//     that this follows rather than tidies, because the contract is what
//     clients are written against.
//
// TestInstallationTokenRefusalMatchesVendoredContract holds every entry against
// `x-github.enabledForGitHubApps` in the vendored description, in both
// directions, so this set can neither grow past what GitHub opens nor fall
// behind it.
var installationReachableUserRoutes = map[string]bool{
	"GET /api/v3/user/{account_id}":                                  true,
	"GET /api/v3/user/memberships/orgs":                              true,
	"GET /api/v3/user/memberships/orgs/{org}":                        true,
	"PATCH /api/v3/user/memberships/orgs/{org}":                      true,
	"POST /api/v3/user/social_accounts":                              true,
	"DELETE /api/v3/user/social_accounts":                            true,
	"POST /api/v3/user/codespaces/{codespace_name}/publish":          true,
	"POST /api/v3/user/{user_id}/projectsV2/{project_number}/drafts": true,
}

// routeIsUserAccountScoped reports whether a route pattern addresses the
// authenticated account. It matches `/api/v3/user` and `/api/v3/user/...` but
// not `/api/v3/users...`, which is a different collection, and not the routes
// under the prefix that GitHub keeps open to installations.
func routeIsUserAccountScoped(pattern string) bool {
	if installationReachableUserRoutes[pattern] {
		return false
	}
	_, path, ok := strings.Cut(pattern, " ")
	if !ok {
		path = pattern
	}
	return path == userAccountScopedPrefix || strings.HasPrefix(path, userAccountScopedPrefix+"/")
}

// refuseInstallationTokenOnUserRoutes refuses an installation token on a route
// whose subject is the authenticated account, and passes every other
// credential — and every other route — through untouched.
//
// It is applied from Server.route so the refusal cannot be forgotten when a
// route is added: a new `/api/v3/user/...` endpoint inherits it.
func (s *Server) refuseInstallationTokenOnUserRoutes(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if !routeIsUserAccountScoped(pattern) {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Only the installation token itself is refused. A user-to-server
		// token (ghu_) is a user credential the App acts through, and GitHub
		// answers /user for it with the authorizing person — that is the whole
		// point of the user-to-server flow.
		if ghInstallationTokenFromContext(r.Context()) != nil {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		next(w, r)
	}
}
