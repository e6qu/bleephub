package bleephub

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Permission enforcement for /api routes. Level ordering: read < write < admin.

// permissionGrant is the typed authorization fact requirePerm attaches to the
// context after both halves of its decision succeed (credential carries the
// permission, credential reaches every named target). Unexported and
// constructible only by requirePerm, so a handler cannot manufacture authority;
// downstream org resolvers rely on it rather than reinterpreting an installation
// token's synthetic bot user as a human member.
type permissionGrant struct {
	scope store.PermScope
	level store.PermLevel
}

const ctxPermissionGrant contextKey = "gh-permission-grant"

func permissionGrantFromContext(ctx context.Context) (permissionGrant, bool) {
	grant, ok := ctx.Value(ctxPermissionGrant).(permissionGrant)
	return grant, ok
}

func parsePermLevel(s string) store.PermLevel {
	switch strings.ToLower(s) {
	case "admin":
		return store.PermAdmin
	case "write":
		return store.PermWrite
	case "read", "":
		return store.PermRead
	}
	return store.PermRead
}

// requirePerm enforces (scope, level) on the request's auth. Two checks, both
// for every credential shape: the credential's own permission set (may it do
// this kind of thing at all — the only question the permission map can answer),
// then the resource check (is this credential entitled to this target).
// Skipping the second for any shape let an app installed on the attacker's own
// account write to a stranger's private repository.
func (s *Server) requirePerm(scope store.PermScope, level store.PermLevel, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		instTok := ghInstallationTokenFromContext(r.Context())
		utsTok := ghUserToServerTokenFromContext(r.Context())
		jwtApp := ghAppFromContext(r.Context())
		jobTok := ghJobTokenFromContext(r.Context())
		user := ghUserFromContext(r.Context())

		switch {
		case instTok != nil:
			if !hasPerm(instTok.Permissions, scope, level) {
				writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
				return
			}
		case jobTok != nil:
			// A workflow GITHUB_TOKEN carries the workflow's resolved
			// least-privilege permission set; reject an un-granted scope (ACT-014).
			if !hasPerm(jobTok.Perms, scope, level) {
				writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
				return
			}
		case utsTok != nil:
			if !userToServerHasPerm(utsTok, scope, level, s.store) {
				writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
				return
			}
		case jwtApp != nil:
			// JWT auth serves app-meta endpoints only; reject resource-level gates.
			writeGHError(w, http.StatusForbidden, "JWT can only be used for app-meta endpoints")
			return
		case user != nil:
			switch token := ghPersonalAccessTokenFromContext(r.Context()); {
			case token == nil:
				// A browser session carries no scope selection.
			case token.FineGrained:
				if !s.fineGrainedPATAllows(r, token, scope, level) {
					writeGHError(w, http.StatusForbidden, "Resource not accessible by personal access token")
					return
				}
			default:
				if classicScopeGateApplies(r, level) && !classicScopeCovers(token.Scopes, scope, level) {
					denyMissingScope(w, scope)
					return
				}
			}
		default:
			writeGHError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}

		need := resourceCapabilityFor(scope, level, r.Method, r.URL.Path)
		switch s.credentialMayAccessTarget(r, user, instTok, scope, need) {
		case targetMissing:
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		case targetDenied:
			s.denyResourceAccess(w, r, need)
			return
		}

		grant := permissionGrant{scope: scope, level: level}
		ctx := context.WithValue(r.Context(), ctxPermissionGrant, grant)
		next(w, r.WithContext(ctx))
	}
}

// requirePerms composes permission requirements as a conjunction, for endpoints
// whose authorization names both a repository and an organization permission
// (team-repository grants). One "closest" scope would reject valid
// installations or accept under-scoped ones.
func (s *Server) requirePerms(requirements []permissionGrant, next http.HandlerFunc) http.HandlerFunc {
	for index := len(requirements) - 1; index >= 0; index-- {
		requirement := requirements[index]
		next = s.requirePerm(requirement.scope, requirement.level, next)
	}
	return next
}

// authenticatedBrowserRequest resolves the credential on a route served outside
// the /api middleware and puts it back on the request, so credential-aware
// predicates (viewerCanAdminOrg) can see who is asking.
func (s *Server) authenticatedBrowserRequest(r *http.Request) (*http.Request, *store.User) {
	r = r.WithContext(s.authenticateRequest(r))
	return r, ghUserFromContext(r.Context())
}

// targetVerdict is the outcome of the resource half of the gate. Denied and
// missing are separate because they get separate responses: a denial is 403 or
// 404 by level, while a path naming a missing resource is always 404 and never
// confirms which repositories or organizations are real.
type targetVerdict int

const (
	targetAllowed targetVerdict = iota
	targetDenied
	targetMissing
)

// credentialMayAccessTarget routes the resource decision to the check that
// suits the credential. An installation token's principal is the installation,
// not the synthetic bot user on the context, so the user-shaped question must
// not be asked of it.
func (s *Server) credentialMayAccessTarget(r *http.Request, user *store.User, instTok *store.InstallationToken, scope store.PermScope, need store.PermLevel) targetVerdict {
	if verdict := s.namedTargetsResolve(r); verdict != targetAllowed {
		return verdict
	}
	if jobTok := ghJobTokenFromContext(r.Context()); jobTok != nil {
		// A workflow GITHUB_TOKEN reaches exactly its own repository; a request
		// naming a different repo or no repository is outside its reach (ACT-014).
		repo := s.repoFromPATRequest(r)
		if repo == nil || !strings.EqualFold(repo.FullName, jobTok.Repo) {
			return targetDenied
		}
		return targetAllowed
	}
	if instTok != nil {
		if s.installationMayAccessTarget(r, instTok, scope) {
			return targetAllowed
		}
		return targetDenied
	}
	// A ghu_ user-to-server token is the intersection of what the user can reach
	// and what the app is installed on: an app cannot borrow a user's access to a
	// repository it was never installed on, so both halves must hold.
	if uts := ghUserToServerTokenFromContext(r.Context()); uts != nil && uts.AppID != 0 {
		if repo := s.repoFromPATRequest(r); repo != nil {
			// Same public-repository carve-out the installation path gets, so a
			// ghu_ token is not narrower than a ghs_ token of the same app.
			readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead
			if !(readOnly && !repo.Private && publicReadAllowed(scope)) && !s.userToServerReachesRepo(uts, repo) {
				return targetDenied
			}
		}
		// The organization half: checking only the repository let a ghu_ token
		// for an app installed on a user (never the org) mint that org's runner
		// registration token.
		if orgLogin := r.PathValue("org"); orgLogin != "" && !s.userToServerReachesAccount(uts, store.OrganizationAccount, orgLogin) {
			return targetDenied
		}
	}
	if s.principalMayAccessTarget(r, user, need) {
		return targetAllowed
	}
	return targetDenied
}

// namedTargetsResolve reports whether every resource the request path names
// actually exists. "Names no repository" and "names a missing repository" both
// reduce to a nil lookup; treating that nil as "nothing to check" let an
// unrelated account create a working webhook on anyone/does-not-exist.
func (s *Server) namedTargetsResolve(r *http.Request) targetVerdict {
	if repoNamedInRequest(r) && s.repoFromPATRequest(r) == nil {
		return targetMissing
	}
	if orgLogin := r.PathValue("org"); orgLogin != "" && s.store.GetOrg(orgLogin) == nil {
		return targetMissing
	}
	return targetAllowed
}

// repoNamedInRequest reports whether the path names a repository at all,
// distinct from whether that repository resolves.
func repoNamedInRequest(r *http.Request) bool {
	if r.PathValue("owner") != "" && r.PathValue("repo") != "" {
		return true
	}
	return r.PathValue("repository_id") != ""
}

// installationOnAccount reports whether an installation targets the given
// account. The kind matters as much as the login: nothing keeps a user login
// and an org login distinct, so matching on login alone let an app installed on
// a *user* named acme administer the *organization* named acme.
func installationOnAccount(inst *store.Installation, kind store.AccountKind, login string) bool {
	if inst == nil || login == "" {
		return false
	}
	if kind == store.OrganizationAccount && !strings.EqualFold(inst.TargetType, "Organization") {
		return false
	}
	return strings.EqualFold(inst.TargetLogin, login)
}

// userToServerReachesAccount reports whether any installation of the token's app
// is on the account, honouring a token narrowed to specific installations.
func (s *Server) userToServerReachesAccount(tok *store.UserToServerToken, kind store.AccountKind, login string) bool {
	if tok == nil || tok.AppID == 0 {
		return true
	}
	for _, inst := range s.store.ListAppInstallations(tok.AppID) {
		if len(tok.InstallationIDs) > 0 && !containsRepoID(tok.InstallationIDs, inst.ID) {
			continue
		}
		if installationOnAccount(inst, kind, login) {
			return true
		}
	}
	return false
}

// userToServerReachesRepo reports whether any installation of the token's app
// covers the repository, honouring a token narrowed to specific installations.
// A gho_ OAuth-app token has no installation behind it and is unconstrained.
func (s *Server) userToServerReachesRepo(tok *store.UserToServerToken, repo *store.Repo) bool {
	if tok == nil || tok.AppID == 0 || repo == nil {
		return true
	}
	if tok.RepositoryIDs != nil && !slices.Contains(tok.RepositoryIDs, repo.ID) {
		return false
	}
	for _, inst := range s.store.ListAppInstallations(tok.AppID) {
		if len(tok.InstallationIDs) > 0 && !containsRepoID(tok.InstallationIDs, inst.ID) {
			continue
		}
		if installationCovers(inst, nil, repo) {
			return true
		}
	}
	return false
}

// installationMayAccessTarget checks that the installation behind a token covers
// the repository and organization the path names. The permission map was
// checked by the caller; this answers which resources that grant is over.
func (s *Server) installationMayAccessTarget(r *http.Request, tok *store.InstallationToken, scope store.PermScope) bool {
	if repo := s.repoFromPATRequest(r); repo != nil {
		// A public repository is readable by anyone, including an app installed
		// elsewhere — the same carve-out canReadRepoAsUser makes.
		readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead
		if !(readOnly && !repo.Private && publicReadAllowed(scope)) && !s.installationReachesRepo(tok, repo) {
			return false
		}
	}
	if orgLogin := r.PathValue("org"); orgLogin != "" && !s.installationReachesAccount(tok, store.OrganizationAccount, orgLogin) {
		return false
	}
	return true
}

func (s *Server) installationReachesRepo(tok *store.InstallationToken, repo *store.Repo) bool {
	if tok == nil || repo == nil {
		return false
	}
	return installationCovers(s.store.GetInstallation(tok.InstallationID), tok.RepositoryIDs, repo)
}

// installationCovers reports whether an installation covers a repository: it
// must belong to the account the app was installed on, be within a non-"all"
// installation's chosen set, and be within any narrower set the token was scoped
// to. The selection test is deny-by-default so a future selection value cannot
// silently admit every repository.
func installationCovers(inst *store.Installation, tokenRepoIDs []int, repo *store.Repo) bool {
	if inst == nil || repo == nil {
		return false
	}
	owner, _, ok := strings.Cut(repo.FullName, "/")
	if !ok || !strings.EqualFold(owner, inst.TargetLogin) {
		return false
	}
	if inst.RepositorySelection != "all" && !containsRepoID(inst.SelectedRepoIDs, repo.ID) {
		return false
	}
	if len(tokenRepoIDs) > 0 && !containsRepoID(tokenRepoIDs, repo.ID) {
		return false
	}
	return true
}

func (s *Server) installationReachesAccount(tok *store.InstallationToken, kind store.AccountKind, login string) bool {
	if tok == nil {
		return false
	}
	return installationOnAccount(s.store.GetInstallation(tok.InstallationID), kind, login)
}

func containsRepoID(ids []int, id int) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// viewerHasRepoPermission answers, for whichever credential the request carries,
// whether it may act on this repository at this level. For an app credential it
// intersects two independent questions — the permission map (what kind of act)
// and the installation (which repositories) — and neither half is optional. The
// caller supplies the scope because only it knows it (issue triage is
// issues:write, a push is contents:write). Takes a context, not a request, so
// GraphQL resolvers reach the same decision.
func (s *Server) viewerHasRepoPermission(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	return s.viewerMayActOnRepo(ctx, repo, scope, level, repoCapabilityForScope(scope, level))
}

// viewerMayActOnRepo is viewerHasRepoPermission where the standing a bearer
// needs differs from the one the scope implies (editing a code-scanning alert is
// an app's security_events:write but a human's repository admin).
func (s *Server) viewerMayActOnRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, grant, standing store.PermLevel) bool {
	if !s.credentialGrantsRepo(ctx, repo, scope, grant) {
		return false
	}
	if !s.principalHoldsRepoCapability(ctx, repo, standing) {
		return false
	}
	// A repository locked by a migration is frozen for writes. Enforced here
	// because this is the one predicate every repository write asks (REST,
	// GraphQL, both git transports), so the lock holds everywhere and a new
	// handler inherits it. Reads are unaffected — the export itself must read.
	if standing >= store.PermWrite && repo != nil && s.store.RepoLockedForMigration(repo.FullName) {
		return false
	}
	return true
}

// credentialGrantsRepo is the credential half of viewerHasRepoPermission: the
// grant the request's credential carries, intersected with the repositories it
// covers. Separate from the principal half because the GraphQL author exemption
// relaxes the principal and must not relax this — an issue author may retitle
// it, but not through an app that was never granted issues. A browser session
// carries no selection and is decided by the principal half.
func (s *Server) credentialGrantsRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	if repo == nil {
		return false
	}
	// Public data is public to every credential; asked before the grant so
	// scoping app credentials does not remove an anonymous caller's reads.
	if level == store.PermRead && !repo.Private && publicReadAllowed(scope) {
		return true
	}
	if tok := ghInstallationTokenFromContext(ctx); tok != nil {
		if !hasPerm(tok.Permissions, scope, level) {
			return false
		}
		return s.installationReachesRepo(tok, repo)
	}
	// A user-to-server token is intersected here too, not only in
	// credentialMayAccessTarget, for self-gating handlers that never reach
	// requirePerm; otherwise it borrows the bearer's collaborator access and
	// ends up broader than the refused ghs_ token of the same app. Only ghu_ is
	// asked the reach question: a gho_ OAuth-App token has no installation to
	// reach through, and userToServerReachesRepo admits an AppID-less token.
	if uts := ghUserToServerTokenFromContext(ctx); uts != nil {
		if !userToServerHasPerm(uts, scope, level, s.store) {
			return false
		}
		return s.userToServerReachesRepo(uts, repo)
	}
	// A fine-grained PAT names both halves itself. requirePerm consults them via
	// fineGrainedPATAllows, but /api/graphql and the git transports never reach
	// it, so without this arm a token selecting one repository acted on every
	// repository its bearer could reach.
	if token := ghPersonalAccessTokenFromContext(ctx); token != nil {
		if token.FineGrained {
			return s.fineGrainedPATGrantsRepo(token, repo, scope, level)
		}
		// Classic scope selection on classicScopeGateApplies's terms: writes
		// only, since classic scopes are coarse while GitHub's read rules vary.
		return level < store.PermWrite || classicScopeCovers(token.Scopes, scope, level)
	}
	return true
}

// fineGrainedPATGrantsRepo is fineGrainedPATAllows for an already-resolved
// repository, so resolvers and transports holding no *http.Request reach the
// same decision.
func (s *Server) fineGrainedPATGrantsRepo(token *store.Token, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	if fineGrainedPATExpired(token) || !s.fineGrainedPATApproved(token) {
		return false
	}
	if !fineGrainedPATSelectsRepo(token, repo) {
		return false
	}
	return hasPerm(token.Permissions.Repository, scope, level)
}

func fineGrainedPATExpired(token *store.Token) bool {
	return token.ExpiresAt != nil && !token.ExpiresAt.After(time.Now())
}

// fineGrainedPATSelectsRepo reports whether a fine-grained token's resource
// owner and repository selection cover the repository. Allow-by-enumeration:
// an unrecognised selection value grants nothing.
func fineGrainedPATSelectsRepo(token *store.Token, repo *store.Repo) bool {
	owner, _, ok := strings.Cut(repo.FullName, "/")
	if !ok || !strings.EqualFold(owner, token.ResourceOwner) {
		return false
	}
	switch token.RepositorySelection {
	case "all":
		return true
	case "subset":
		return containsRepoID(token.RepositoryIDs, repo.ID)
	}
	return false
}

// principalHoldsRepoCapability is the user half: the capability the bearer holds
// on the repository in their own right. An installation token's bearer is a
// synthetic bot that owns and collaborates on nothing, so the installation arm
// stands in for it.
func (s *Server) principalHoldsRepoCapability(ctx context.Context, repo *store.Repo, need store.PermLevel) bool {
	if repo == nil {
		return false
	}
	if ghInstallationTokenFromContext(ctx) != nil {
		return true
	}
	user := ghUserFromContext(ctx)
	switch {
	case need >= store.PermAdmin:
		return canAdminRepo(s.store, user, repo)
	case need >= store.PermWrite:
		return canPushRepo(s.store, user, repo)
	default:
		return canReadRepoAsUser(s.store, user, repo)
	}
}

// viewerCanReadRepo, viewerCanPushRepo and viewerCanAdminRepo are shorthands for
// the levels on a repository's own contents and settings. A handler whose
// subject is issues, pull requests, actions or another scope must call
// viewerHasRepoPermission with that scope — these would refuse an app GitHub
// grants.
func (s *Server) viewerCanReadRepo(ctx context.Context, repo *store.Repo) bool {
	return s.viewerHasRepoPermission(ctx, repo, store.ScopeMetadata, store.PermRead)
}

func (s *Server) viewerCanPushRepo(ctx context.Context, repo *store.Repo) bool {
	return s.viewerHasRepoPermission(ctx, repo, store.ScopeContents, store.PermWrite)
}

func (s *Server) viewerCanAdminRepo(ctx context.Context, repo *store.Repo) bool {
	return s.viewerHasRepoPermission(ctx, repo, store.ScopeAdministration, store.PermWrite)
}

// credentialGrantsAccount is credentialGrantsRepo for an account-scoped resource
// (org membership, a ProjectV2). Same intersection: a user-to-server token that
// skipped the reach half acted as its bearer on accounts the app was never
// installed on. A browser session is unconstrained; its standing is the
// caller's own.
func (s *Server) credentialGrantsAccount(ctx context.Context, kind store.AccountKind, login string, scope store.PermScope, level store.PermLevel) bool {
	if login == "" {
		return false
	}
	if tok := ghInstallationTokenFromContext(ctx); tok != nil {
		return hasPerm(tok.Permissions, scope, level) && s.installationReachesAccount(tok, kind, login)
	}
	// Grant for both prefixes, reach for ghu_ only — the asymmetry
	// credentialGrantsRepo spells out.
	if uts := ghUserToServerTokenFromContext(ctx); uts != nil {
		return userToServerHasPerm(uts, scope, level, s.store) && s.userToServerReachesAccount(uts, kind, login)
	}
	if token := ghPersonalAccessTokenFromContext(ctx); token != nil && token.FineGrained {
		return s.fineGrainedPATGrantsAccount(token, login, scope, level)
	}
	return true
}

// fineGrainedPATGrantsAccount is fineGrainedPATGrantsRepo for the account half:
// a fine-grained token belongs to exactly one resource owner. Both halves of the
// permission map are consulted because creating a repository under an org is
// granted by the *repository* Administration permission while the org's
// membership is granted by the organization Members permission; with no URL
// pattern to pick from here, narrowing to one half would refuse grants GitHub
// allows.
func (s *Server) fineGrainedPATGrantsAccount(token *store.Token, login string, scope store.PermScope, level store.PermLevel) bool {
	if fineGrainedPATExpired(token) || !s.fineGrainedPATApproved(token) {
		return false
	}
	if !strings.EqualFold(login, token.ResourceOwner) {
		return false
	}
	return hasPerm(token.Permissions.Organization, scope, level) ||
		hasPerm(token.Permissions.Repository, scope, level)
}

// viewerReachesOrg asks only whether the app behind the request is installed on
// the organization. Handlers whose answer is the caller's own membership record
// (including a pending invitation, which viewerIsOrgMember refuses) need this
// half on its own.
func (s *Server) viewerReachesOrg(ctx context.Context, orgLogin string) bool {
	return s.credentialGrantsAccount(ctx, store.OrganizationAccount, orgLogin, store.ScopeMetadata, store.PermRead)
}

// orgRole is the level a caller must hold on an org for viewerHoldsOrgRole.
type orgRole int

const (
	orgRoleMemberLevel orgRole = iota
	orgRoleAdminLevel
)

// viewerHoldsOrgRole is the org counterpart of viewerCanReadRepo: the
// credential-aware answer to "may this request act on this org at this level".
// Handlers must use it rather than the user-scoped org predicates, which cannot
// see the app behind a user-to-server token (an app with no installation was
// minting org webhooks through its bearer's admin role).
//
// An installation token's viewer is a bot with no membership. On a requirePerm
// route the typed grant in the context stands in for human membership (the
// middleware already checked the permission map and target). Outside that
// chokepoint there is no grant, so a bot cannot acquire org standing here.
func (s *Server) viewerHoldsOrgRole(ctx context.Context, orgLogin string, need orgRole) bool {
	if ghInstallationTokenFromContext(ctx) != nil {
		grant, ok := permissionGrantFromContext(ctx)
		return ok && s.credentialGrantsAccount(ctx, store.OrganizationAccount, orgLogin, grant.scope, grant.level)
	}
	if !s.viewerReachesOrg(ctx, orgLogin) {
		return false
	}
	user := ghUserFromContext(ctx)
	if user == nil {
		return false
	}
	if need == orgRoleAdminLevel {
		org := s.store.GetOrg(orgLogin)
		return org != nil && canAdminOrgAsUser(s.store, user, org)
	}
	return isActiveOrgMemberAsUser(s.store, user, orgLogin)
}

func (s *Server) viewerCanAdminOrg(ctx context.Context, orgLogin string) bool {
	return s.viewerHoldsOrgRole(ctx, orgLogin, orgRoleAdminLevel)
}

func (s *Server) viewerIsOrgMember(ctx context.Context, orgLogin string) bool {
	return s.viewerHoldsOrgRole(ctx, orgLogin, orgRoleMemberLevel)
}

// scopeAdministersResource reports whether a scope governs its resource's
// administration rather than its contents — writing a secret, a
// branch-protection rule or a webhook is admin however modest the route's level.
func scopeAdministersResource(scope store.PermScope) bool {
	switch scope {
	case store.ScopeAdministration, store.ScopeOrgAdministration, store.ScopeOrganizationHooks, store.ScopeSecrets,
		store.ScopeDependabotSecrets, store.ScopePATs, store.ScopePATRequests:
		return true
	}
	return false
}

// repoCapabilityForScope is resourceCapabilityFor without the request. The
// outside-contributor downgrade is keyed on a request path and so cannot be
// decided here; callers that need it name the standing themselves.
func repoCapabilityForScope(scope store.PermScope, level store.PermLevel) store.PermLevel {
	if scopeAdministersResource(scope) {
		return store.PermAdmin
	}
	return level
}

// resourceCapabilityFor maps a scope, level and method onto the capability the
// caller must hold on the resource named in the path. Two adjustments, both
// matching GitHub: admin scopes always demand admin, and "write" on
// collaborative scopes gates creation on read (opening an issue, proposing a PR
// from a fork, reviewing) while editing and deleting still require push.
func resourceCapabilityFor(scope store.PermScope, level store.PermLevel, method, path string) store.PermLevel {
	if scopeAdministersResource(scope) {
		return store.PermAdmin
	}
	// A team-repository association endpoint carries {owner}/{repo}, but the repo
	// is the object being assigned, not the authorization boundary; the handler's
	// team maintainer check decides standing, and requiring push here would
	// override an org's read-only base permission.
	if scope == store.ScopeMembers && strings.Contains(path, "/teams/") && strings.Contains(path, "/repos/") {
		return store.PermRead
	}
	// scopeSecurityEvents is deliberately absent: promoting it to admin refused a
	// pushing collaborator the read GitHub grants and turned a read 404 into a
	// 403 existence oracle. scopeMembers is absent for the same reason — a team
	// maintainer manages membership without org admin; those checks belong to the
	// team resolvers.
	if method == http.MethodPost && isOutsideContributorPost(path) {
		return store.PermRead
	}
	// GitHub lets a comment's author edit or delete it without push. Keep the
	// credential grant at write but defer the resource decision to the handler,
	// which inspects the comment owner. Path-specific: labels, milestones,
	// assignments under the same scopes still require push.
	if (method == http.MethodPatch || method == http.MethodDelete) && isAuthorEditableCommentPath(path) {
		return store.PermRead
	}
	return level
}

func isAuthorEditableCommentPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 8 || parts[0] != "api" || parts[1] != "v3" || parts[2] != "repos" {
		return false
	}
	return (parts[5] == "issues" || parts[5] == "pulls") && parts[6] == "comments"
}

// isOutsideContributorPost matches the creations an outside contributor may
// perform with only read access: filing an issue, proposing a PR from a fork,
// commenting, reviewing, reacting, forking. Keyed on the path, not the scope:
// POST .../labels, .../milestones, .../assignees also live under scopeIssues/
// scopePullRequests but require push, so a scope-keyed downgrade let any account
// create labels and assign issues on any public repository.
func isOutsideContributorPost(path string) bool {
	switch {
	case strings.HasSuffix(path, "/issues"),
		strings.HasSuffix(path, "/pulls"),
		strings.HasSuffix(path, "/forks"),
		strings.HasSuffix(path, "/comments"),
		strings.HasSuffix(path, "/reviews"),
		strings.HasSuffix(path, "/reactions"),
		strings.HasSuffix(path, "/replies"):
		return true
	}
	return false
}

// principalMayAccessTarget resolves the repository or organization named in the
// path and reports whether the user holds the required capability. A request
// naming no resource is admitted — the handler's own checks then apply.
func (s *Server) principalMayAccessTarget(r *http.Request, user *store.User, need store.PermLevel) bool {
	if user == nil {
		return false
	}
	if user.SiteAdmin {
		return true
	}
	// The org is checked first and separately: several org routes also carry a
	// caller-supplied {repository_id}, so returning the repository's verdict let
	// someone name a repo they own and administer a stranger's org. Both must
	// pass when both are named. Orgs are gated at admin only; read/write on
	// org-scoped collections varies per endpoint and belongs with the resolvers.
	if need >= store.PermAdmin {
		if orgLogin := r.PathValue("org"); orgLogin != "" {
			if org := s.store.GetOrg(orgLogin); org == nil || !canAdminOrgAsUser(s.store, user, org) {
				return false
			}
		}
	}
	if repo := s.repoFromPATRequest(r); repo != nil {
		switch {
		case need >= store.PermAdmin:
			return canAdminRepo(s.store, user, repo)
		case need >= store.PermWrite:
			return canPushRepo(s.store, user, repo)
		default:
			return canReadRepoAsUser(s.store, user, repo)
		}
	}
	return true
}

// denyResourceAccess answers a failed resource check. A caller who cannot read
// the repository gets 404 whatever the route's level — choosing status by level
// made every write/admin route an existence oracle (403 for a private repo that
// exists, 404 for one that does not). Only a caller who can read but lacks
// standing to change gets the 403 GitHub gives.
func (s *Server) denyResourceAccess(w http.ResponseWriter, r *http.Request, need store.PermLevel) {
	if repo := s.repoFromPATRequest(r); repo != nil && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	switch {
	case need >= store.PermAdmin:
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
	case need >= store.PermWrite:
		writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) fineGrainedPATAllows(r *http.Request, token *store.Token, scope store.PermScope, level store.PermLevel) bool {
	if fineGrainedPATExpired(token) {
		return false
	}
	if repo := s.repoFromPATRequest(r); repo != nil && !repo.Private &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead) && publicReadAllowed(scope) {
		return true
	}
	if !s.fineGrainedPATResourceAllowed(r, token) {
		return false
	}
	permissions := token.Permissions.Repository
	if r.PathValue("org") != "" && r.PathValue("repo") == "" && r.PathValue("repository_id") == "" {
		permissions = token.Permissions.Organization
	}
	return hasPerm(permissions, scope, level)
}

func (s *Server) fineGrainedPATApproved(token *store.Token) bool {
	if s.store.GetOrg(token.ResourceOwner) == nil {
		return true
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, grant := range s.store.OrgPATGrants[token.ResourceOwner] {
		if grant.TokenID == token.FineGrainedID {
			return true
		}
	}
	return false
}

func (s *Server) repoFromPATRequest(r *http.Request) *store.Repo {
	if owner, name := r.PathValue("owner"), r.PathValue("repo"); owner != "" && name != "" {
		return s.store.GetRepo(owner, name)
	}
	if raw := r.PathValue("repository_id"); raw != "" {
		id, err := strconv.Atoi(raw)
		if err == nil {
			return s.store.GetRepoByID(id)
		}
	}
	return nil
}

func (s *Server) fineGrainedPATResourceAllowed(r *http.Request, token *store.Token) bool {
	if !s.fineGrainedPATApproved(token) {
		return false
	}
	if org := r.PathValue("org"); org != "" && !strings.EqualFold(org, token.ResourceOwner) {
		return false
	}
	repo := s.repoFromPATRequest(r)
	if repo == nil {
		// A path naming a missing repository differs from one naming none; only
		// the latter has nothing to check.
		return !repoNamedInRequest(r)
	}
	return fineGrainedPATSelectsRepo(token, repo)
}

// classicScopeGateApplies reports whether a classic token's scope selection is
// checked for this (scope, level) against this request. Two bounds: the perm
// constants model only repository and organization permissions, so they stand in
// only where the request names one (not account-level routes like keys, gists,
// notifications); and the level mapping is trusted for writes only, since
// classic scopes are coarse while GitHub's read rules vary per endpoint.
func classicScopeGateApplies(r *http.Request, level store.PermLevel) bool {
	if level < store.PermWrite {
		return false
	}
	return repoNamedInRequest(r) || r.PathValue("org") != ""
}

// denyMissingScope answers a classic PAT or OAuth token whose scope selection
// does not reach the permission the route needs.
func denyMissingScope(w http.ResponseWriter, scope store.PermScope) {
	writeGHError(w, http.StatusForbidden, "Token does not have the OAuth scopes required for "+string(scope))
}

// classicOAuthScopesOffered returns the classic OAuth scope selection the
// request's credential carries, and whether it carries one. A classic PAT and a
// gho_ OAuth-App token share the scope model; reading only the first let a
// scopeless OAuth token pass self-gating routes a classic token is refused.
func classicOAuthScopesOffered(ctx context.Context) (string, bool) {
	if token := ghPersonalAccessTokenFromContext(ctx); token != nil && !token.FineGrained {
		return token.Scopes, true
	}
	if uts := ghUserToServerTokenFromContext(ctx); uts != nil && uts.AppID == 0 {
		return uts.Scopes, true
	}
	return "", false
}

// enforceFineGrainedPATResource is the per-route credential-selection gate for
// handlers that do their own role checks instead of using requirePerm (e.g.
// DELETE /orgs/{org}). Running after path values are filled keeps a
// selected-repository token from inheriting its owner's wider membership and a
// classic scope selection from being exceeded.
func (s *Server) enforceFineGrainedPATResource(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if !strings.Contains(pattern, " /api/") || (!strings.Contains(pattern, "{repo}") && !strings.Contains(pattern, "{org}") && !strings.Contains(pattern, "{repository_id}")) {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		token := ghPersonalAccessTokenFromContext(r.Context())
		scopes, classic := classicOAuthScopesOffered(r.Context())
		if token == nil && !classic {
			next(w, r)
			return
		}
		scope, level := fineGrainedPATPermissionForPattern(pattern, r.Method)
		if repo := s.repoFromPATRequest(r); repo != nil && !repo.Private &&
			(r.Method == http.MethodGet || r.Method == http.MethodHead) && publicReadAllowed(scope) {
			next(w, r)
			return
		}
		if classic {
			if classicScopeGateApplies(r, level) && !classicScopeCovers(scopes, scope, level) {
				denyMissingScope(w, scope)
				return
			}
			next(w, r)
			return
		}
		if !s.fineGrainedPATResourceAllowed(r, token) {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by personal access token")
			return
		}
		permissions := token.Permissions.Repository
		if r.PathValue("org") != "" && s.repoFromPATRequest(r) == nil && !strings.Contains(pattern, "/orgs/{org}/repos") {
			permissions = token.Permissions.Organization
		}
		if !hasPerm(permissions, scope, level) {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by personal access token")
			return
		}
		next(w, r)
	}
}

func fineGrainedPATPermissionForPattern(pattern, method string) (store.PermScope, store.PermLevel) {
	level := store.PermWrite
	if method == http.MethodGet || method == http.MethodHead {
		level = store.PermRead
	}
	lower := strings.ToLower(pattern)
	if strings.Contains(lower, "/orgs/{org}/repos") {
		return store.ScopeMetadata, level
	}
	if strings.Contains(lower, "/copilot-spaces") {
		return store.ScopeCopilotSpaces, level
	}
	if strings.Contains(lower, "/orgs/{org}/hooks") {
		return store.ScopeOrganizationHooks, level
	}
	// A team route naming a repository is still a team route: classifying by the
	// {repo} made adding a repo to a team demand repository admin rather than org
	// membership.
	if strings.Contains(lower, "/orgs/{org}/teams") {
		return store.ScopeMembers, level
	}
	if strings.Contains(lower, "{org}") && !strings.Contains(lower, "{repo}") && !strings.Contains(lower, "{repository_id}") {
		if strings.Contains(lower, "members") || strings.Contains(lower, "/teams") || strings.Contains(lower, "/invitations") || strings.Contains(lower, "/outside_collaborators") {
			return store.ScopeMembers, level
		}
		return store.ScopeOrgAdministration, level
	}
	for _, candidate := range []struct {
		fragment string
		scope    store.PermScope
	}{
		{"/actions", store.ScopeActions}, {"/issues", store.ScopeIssues}, {"/milestones", store.ScopeIssues}, {"/labels", store.ScopeIssues},
		{"/pulls", store.ScopePullRequests}, {"/checks", store.ScopeChecks}, {"/deployments", store.ScopeDeployments}, {"/environments", store.ScopeDeployments},
		{"/pages", store.ScopePages}, {"/codespaces", store.ScopeCodespaces}, {"/secret-scanning", store.ScopeSecurityEvents}, {"/code-scanning", store.ScopeSecurityEvents},
		{"/dependabot", store.ScopeDependabotSecrets}, {"/reactions", store.ScopeReactions}, {"/projects", store.ScopeProjects},
		{"/contents", store.ScopeContents}, {"/git/", store.ScopeContents}, {"/commits", store.ScopeContents}, {"/branches", store.ScopeContents}, {"/tags", store.ScopeContents}, {"/releases", store.ScopeContents},
		{"/hooks", store.ScopeAdministration}, {"/keys", store.ScopeAdministration}, {"/rules", store.ScopeAdministration},
	} {
		if strings.Contains(lower, candidate.fragment) {
			return candidate.scope, level
		}
	}
	if strings.Contains(lower, "/collaborators") {
		if level == store.PermRead {
			return store.ScopeMetadata, level
		}
		return store.ScopeAdministration, level
	}
	if level == store.PermRead {
		return store.ScopeMetadata, level
	}
	return store.ScopeAdministration, level
}

func (s *Server) filterReposForFineGrainedPAT(r *http.Request, repos []*store.Repo) []*store.Repo {
	token := ghPersonalAccessTokenFromContext(r.Context())
	if token == nil || !token.FineGrained {
		return repos
	}
	filtered := make([]*store.Repo, 0, len(repos))
	for _, repo := range repos {
		if !repo.Private {
			filtered = append(filtered, repo)
			continue
		}
		copy := r.Clone(r.Context())
		owner, name, ok := strings.Cut(repo.FullName, "/")
		if !ok {
			continue
		}
		copy.SetPathValue("owner", owner)
		copy.SetPathValue("repo", name)
		if s.fineGrainedPATResourceAllowed(copy, token) {
			filtered = append(filtered, repo)
		}
	}
	return filtered
}

// hasPerm checks an installation-token permissions map against (scope, level).
// Missing scope = no grant. Admin implies write, write implies read.
func hasPerm(perms map[string]string, scope store.PermScope, level store.PermLevel) bool {
	got, ok := perms[string(scope)]
	if !ok {
		// metadata:read is auto-granted on every installation, per GitHub.
		if scope == store.ScopeMetadata && level == store.PermRead {
			return true
		}
		return false
	}
	return parsePermLevel(got) >= level
}

// userToServerHasPerm dispatches a user-to-server token to either the App
// installation permissions map (ghu_) or the classic OAuth scopes (gho_).
func userToServerHasPerm(tok *store.UserToServerToken, scope store.PermScope, level store.PermLevel, st *store.Store) bool {
	if tok.AppID > 0 {
		if tok.Permissions != nil && !hasPerm(tok.Permissions, scope, level) {
			return false
		}
		// ghu_: use the installation's permissions. A token scoped to specific
		// installations checks exactly those; an unscoped token checks any.
		// Every candidate is asked, not just the first: returning the first's
		// verdict decided authorization by map iteration order once an app was
		// installed in more than one place, and the reach half scans the same set.
		st.Mu.RLock()
		defer st.Mu.RUnlock()
		if len(tok.InstallationIDs) > 0 {
			for _, id := range tok.InstallationIDs {
				if inst := st.Installations[id]; inst != nil && inst.AppID == tok.AppID && hasPerm(inst.Permissions, scope, level) {
					return true
				}
			}
			return false
		}
		for _, inst := range st.Installations {
			if inst.AppID == tok.AppID && hasPerm(inst.Permissions, scope, level) {
				return true
			}
		}
		return false
	}
	// gho_: classic OAuth scopes.
	return classicScopeCovers(tok.Scopes, scope, level)
}

// validPermLevelString reports whether s is a permission level the App API
// accepts in request bodies.
func validPermLevelString(s string) bool {
	switch strings.ToLower(s) {
	case "read", "write", "admin":
		return true
	}
	return false
}

// validateRequestedPermissions checks a token-mint request's permissions map
// against the installation's grants: every requested scope must be granted at
// >= the requested level (metadata:read is implicit). Returns the first
// offending scope and false on escalation or an unknown level.
func validateRequestedPermissions(requested, granted map[string]string) (string, bool) {
	for scope, level := range requested {
		if !validPermLevelString(level) {
			return scope, false
		}
		grantedLevel, ok := granted[scope]
		if !ok {
			if store.PermScope(scope) == store.ScopeMetadata && parsePermLevel(level) == store.PermRead {
				continue
			}
			return scope, false
		}
		if parsePermLevel(level) > parsePermLevel(grantedLevel) {
			return scope, false
		}
	}
	return "", true
}

// classicScopeGrant: holding oauth confers a fine-grained permission up to upTo.
type classicScopeGrant struct {
	oauth string
	upTo  store.PermLevel
}

// classicScopeGrants maps every gated permission to the classic OAuth scopes
// GitHub accepts for it. Deny-by-default: a permission with no entry is granted
// to no classic credential, and startup panics on a missing permScope. `repo` is
// deliberately broad, matching GitHub's documented full-access scope.
var classicScopeGrants = map[store.PermScope][]classicScopeGrant{
	store.ScopeMetadata:          {{"repo", store.PermAdmin}, {"public_repo", store.PermAdmin}},
	store.ScopeContents:          {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}},
	store.ScopeIssues:            {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}},
	store.ScopeDiscussions:       {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}, {"write:discussion", store.PermWrite}, {"read:discussion", store.PermRead}},
	store.ScopePullRequests:      {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}},
	store.ScopePages:             {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}},
	store.ScopeChecks:            {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}},
	store.ScopeReactions:         {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}},
	store.ScopeActions:           {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}, {"workflow", store.PermWrite}},
	store.ScopeDeployments:       {{"repo", store.PermWrite}, {"public_repo", store.PermWrite}, {"repo_deployment", store.PermWrite}},
	store.ScopeSecrets:           {{"repo", store.PermAdmin}},
	store.ScopeDependabotSecrets: {{"repo", store.PermAdmin}},
	store.ScopeSecurityEvents:    {{"repo", store.PermWrite}, {"security_events", store.PermWrite}},
	store.ScopeCodespaces:        {{"repo", store.PermWrite}, {"codespace", store.PermWrite}},
	store.ScopeAdministration: {
		{"repo", store.PermAdmin}, {"public_repo", store.PermWrite},
		{"admin:repo_hook", store.PermWrite}, {"delete_repo", store.PermAdmin},
	},
	store.ScopeMembers: {
		{"admin:org", store.PermAdmin}, {"write:org", store.PermWrite}, {"read:org", store.PermRead},
		{"repo", store.PermWrite},
	},
	store.ScopeOrgAdministration: {
		{"admin:org", store.PermAdmin}, {"write:org", store.PermWrite}, {"read:org", store.PermRead},
	},
	store.ScopeOrganizationHooks: {
		{"admin:org_hook", store.PermAdmin},
	},
	store.ScopeProjects: {
		{"project", store.PermAdmin}, {"read:project", store.PermRead},
		{"repo", store.PermWrite}, {"public_repo", store.PermWrite},
		{"admin:org", store.PermAdmin}, {"write:org", store.PermWrite}, {"read:org", store.PermRead},
	},
	store.ScopePATRequests: {{"admin:org", store.PermAdmin}},
	store.ScopePATs:        {{"admin:org", store.PermAdmin}},
	store.ScopeCopilotSpaces: {
		{"admin:org", store.PermWrite}, {"write:org", store.PermWrite}, {"read:org", store.PermRead},
		{"user", store.PermWrite}, {"read:user", store.PermRead}, {"repo", store.PermWrite},
	},
}

// allPermScopes enumerates every permission constant.
// TestClassicScopeGrantsCoverEveryPermission keeps it in step with the const
// block; init below panics when a listed permission has no classic mapping.
var allPermScopes = []store.PermScope{
	store.ScopeMetadata, store.ScopeContents, store.ScopeIssues, store.ScopeDiscussions, store.ScopePullRequests, store.ScopeActions,
	store.ScopeChecks, store.ScopeSecrets, store.ScopeDeployments, store.ScopeAdministration, store.ScopeMembers,
	store.ScopeOrgAdministration, store.ScopeOrganizationHooks, store.ScopeSecurityEvents, store.ScopeDependabotSecrets, store.ScopeCodespaces,
	store.ScopeReactions, store.ScopeProjects, store.ScopePages, store.ScopePATRequests, store.ScopePATs, store.ScopeCopilotSpaces,
}

func init() {
	for _, scope := range allPermScopes {
		if _, ok := classicScopeGrants[scope]; !ok {
			panic("bleephub: permission " + string(scope) + " has no classic OAuth scope mapping")
		}
	}
}

// parseClassicScopes splits a scope string into its members. Both GitHub
// separators appear: X-OAuth-Scopes and stored PATs are comma-separated, the
// OAuth `scope` request parameter is space-separated.
func parseClassicScopes(scopes string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, s := range strings.FieldsFunc(scopes, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		set[s] = struct{}{}
	}
	return set
}

// classicScopeCovers reports whether a classic OAuth scope string grants
// (scope, level). The empty scope string is a real credential shape — a
// scopeless classic PAT reads public information only, which is what the
// metadata carve-out below gives it.
func classicScopeCovers(scopes string, scope store.PermScope, level store.PermLevel) bool {
	if scope == store.ScopeMetadata && level == store.PermRead {
		return true
	}
	set := parseClassicScopes(scopes)
	for _, grant := range classicScopeGrants[scope] {
		if _, held := set[grant.oauth]; held && level <= grant.upTo {
			return true
		}
	}
	return false
}

// publicReadAllowed reports whether a scope's data is genuinely public when the
// repository is. A public repository is not the same as every scope on it being
// public: Actions secrets/variables listings live under scopeSecrets:read and
// return values, so a blanket public-repo bypass would leak them. Keyed on the
// scope, not the repository alone.
func publicReadAllowed(scope store.PermScope) bool {
	switch scope {
	case store.ScopeMetadata, store.ScopeContents, store.ScopeIssues, store.ScopeDiscussions, store.ScopePullRequests,
		store.ScopePages, store.ScopeChecks, store.ScopeReactions, store.ScopeProjects, store.ScopeDeployments:
		return true
	}
	return false
}
