package bleephub

import (
	"net/http"
	"strings"
)

// An installation token authenticates a GitHub App's installation, not a user.
// bleephub materializes a Bot actor (store.AppBotUser) to author the App's
// writes as `<slug>[bot]`, but the installation holds no account, so endpoints
// whose subject is the authenticated account refuse it with
// `403 Resource not accessible by integration` rather than describing the bot as
// the signed-in user. The vendored REST description carries this per operation
// as `x-github.enabledForGitHubApps: false`, and
// TestInstallationTokenRefusalMatchesVendoredContract holds the set below to it.

// userAccountScopedPrefix covers routes whose subject is the authenticated
// account itself. `/api/v3/users/{username}` is deliberately outside it: those
// name their subject explicitly.
const userAccountScopedPrefix = "/api/v3/user"

// installationReachableUserRoutes are routes under the prefix that GitHub still
// serves to an installation token: they name their subject in the path, report
// the installation's own org standing, or write social accounts (a lopsidedness
// the published description keeps — the matching read stays closed).
// TestInstallationTokenRefusalMatchesVendoredContract holds every entry against
// `x-github.enabledForGitHubApps` in both directions.
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

// routeIsUserAccountScoped reports whether a pattern addresses the authenticated
// account: `/api/v3/user` or `/api/v3/user/...`, excluding `/api/v3/users...` and
// the installation-reachable routes.
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

// refuseInstallationTokenOnUserRoutes refuses an installation token on an
// account-scoped route and passes everything else through. Applied from
// Server.route so a new `/api/v3/user/...` endpoint inherits it.
func (s *Server) refuseInstallationTokenOnUserRoutes(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if !routeIsUserAccountScoped(pattern) {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Refuse only the installation token itself. A user-to-server token
		// (ghu_) is a user credential the App acts through, and GitHub answers
		// /user for it.
		if ghInstallationTokenFromContext(r.Context()) != nil {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		next(w, r)
	}
}
