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

// permission enforcement decorator.
// `requirePerm` wraps an http.HandlerFunc, returning 403 if the request's
// auth shape lacks the required permission/level.
//
// Authentication shapes handled:
//
//   - Classic PAT (Tokens map, no installation context)
//     Checked against its classic OAuth scope selection — see
//     classicScopeGateApplies for the bounds of that check.
//   - Fine-grained PAT (Tokens map + ctxPersonalAccessToken)
//     Checked against approval, expiration, resource owner, selected
//     repositories, and the requested permission map.
//   - GitHub App JWT (ghAppFromContext)
//     App auth is meta-level (manages installations); bypass for app-meta
//     endpoints which use this gate at most for read; gate by caller.
//   - Installation token (ghs_, ctxInstallation + ctxInstallationToken)
//     Checked against InstallationToken.Permissions[scope] >= required level.
//   - User-to-server (ghu_/gho_, ctxUserToServerToken)
//     For ghu_ (AppID > 0): looked up via Installation.Permissions of the
//     installation tied to the user's authorization for this app.
//     For gho_ (OAuthAppClientID set): mapped from classic Scopes string
//     ("repo" → contents:write, "read:org" → members:read, etc.).
//
// Level ordering: read < write < admin. "admin" implies write; "write" implies read.
// (permLevel itself lives in internal/store as PermLevel — ARCH-003.)

// permissionGrant is the typed authorization fact requirePerm passes to the
// handler after both halves of its decision have succeeded: the credential
// carries this permission and the credential reaches every named target.
//
// Keeping this in the request context prevents downstream organization
// resolvers from reinterpreting an installation token's synthetic bot user as
// a human organization member. It is deliberately unexported and can only be
// constructed by requirePerm, so a handler cannot accidentally manufacture
// authority by attaching an arbitrary scope to a request.
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

// requirePerm returns a wrapper that enforces (scope, level) on the request's auth.
//
// Two decisions are made, and both are made for every credential shape. First
// the credential's own permission set is checked — the installation's grant,
// the user-to-server grant, the fine-grained PAT's grant. That answers "may
// this credential do this kind of thing at all", and it is the only question
// the permissions map can answer, because the map does not know which
// repository or organization the request names.
//
// Then the resource check answers the question the map cannot: is *this*
// credential entitled to *this* target. Skipping it for a credential shape is
// what let an app installed on the attacker's own account write to a stranger's
// private repository — the grant was real, it just was not a grant over that
// repository.
//
// Usage:
//
//	s.route("PATCH /api/v3/repos/{owner}/{repo}", s.requirePerm(scopeContents, permWrite, s.handleUpdateRepo))
func (s *Server) requirePerm(scope store.PermScope, level store.PermLevel, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Personal access token path, detected by: user present, no
		// installation token, no user-to-server token, no JWT-app. The PAT
		// itself sits in the auth header; middleware already resolved it into
		// ctxUser.
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
			// least-privilege permission set; a scope it was not granted is
			// forbidden, exactly as GitHub rejects an under-scoped token (ACT-014).
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
			// JWT auth is for app-meta endpoints only; reject on resource-level gates.
			writeGHError(w, http.StatusForbidden, "JWT can only be used for app-meta endpoints")
			return
		case user != nil:
			switch token := ghPersonalAccessTokenFromContext(r.Context()); {
			case token == nil:
				// A browser session carries no scope selection to check.
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

// requirePerms composes permission requirements as a conjunction. GitHub has
// endpoints whose authorization contract names both a repository permission
// and an organization permission (team-repository grants are one example).
// Modeling those as one "closest" scope either rejects valid installations or
// silently accepts under-scoped ones.
func (s *Server) requirePerms(requirements []permissionGrant, next http.HandlerFunc) http.HandlerFunc {
	for index := len(requirements) - 1; index >= 0; index-- {
		requirement := requirements[index]
		next = s.requirePerm(requirement.scope, requirement.level, next)
	}
	return next
}

// authenticatedBrowserRequest resolves the credential on a route served outside
// the /api middleware and puts it back on the request.
//
// These handlers used to keep the resolved context to themselves and pass only
// the *User onward, so every credential-aware predicate they then called was
// handed a request carrying no credential at all: viewerCanAdminOrg could not
// see who was asking and refused an organization owner the installation flow.
func (s *Server) authenticatedBrowserRequest(r *http.Request) (*http.Request, *store.User) {
	r = r.WithContext(s.authenticateRequest(r))
	return r, ghUserFromContext(r.Context())
}

// targetVerdict is the outcome of the resource half of the gate. "Denied" and
// "missing" are separate answers because they get separate responses: a denial
// is 403 or 404 depending on the level asked for, while a path naming a
// resource that does not exist is always 404 and never confirms which
// repositories or organizations are real.
type targetVerdict int

const (
	targetAllowed targetVerdict = iota
	targetDenied
	targetMissing
)

// credentialMayAccessTarget routes the resource decision to the check that
// suits the credential.
//
// An installation token's principal is the installation, not the synthetic bot
// user the middleware puts on the context: that bot is a collaborator on
// nothing, so asking the user-shaped question about it answers "no" for reads
// the app is entitled to and — because the question was not asked at all —
// "yes" for writes it is not.
func (s *Server) credentialMayAccessTarget(r *http.Request, user *store.User, instTok *store.InstallationToken, scope store.PermScope, need store.PermLevel) targetVerdict {
	if verdict := s.namedTargetsResolve(r); verdict != targetAllowed {
		return verdict
	}
	if jobTok := ghJobTokenFromContext(r.Context()); jobTok != nil {
		// A workflow GITHUB_TOKEN reaches exactly its own repository. A request
		// that names a different repo — or names no repository at all (org- or
		// user-level routes) — is outside the token's reach (ACT-014).
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
	// A ghu_ user-to-server token is the intersection of what the user can
	// reach and what the app is installed on — it is not simply the user. On
	// GitHub an app cannot borrow a user's access to a repository it was never
	// installed on, so both halves have to hold.
	if uts := ghUserToServerTokenFromContext(r.Context()); uts != nil && uts.AppID != 0 {
		if repo := s.repoFromPATRequest(r); repo != nil {
			// Same public-repository carve-out the installation path gets. A
			// ghu_ token must not end up narrower than a ghs_ token of the
			// same app on data publicReadAllowed itself calls public.
			readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead
			if !(readOnly && !repo.Private && publicReadAllowed(scope)) && !s.userToServerReachesRepo(uts, repo) {
				return targetDenied
			}
		}
		// And the organization half. Checking only the repository left
		// org-only routes falling through to the user's own capability, so a
		// ghu_ token for an app installed on a user — never on the org — minted
		// that org's runner registration token. installationReachesOrg exists
		// precisely for this and was unreachable from here.
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
// actually exists.
//
// The two questions "the path names no repository" and "the path names a
// repository that is not there" both reduced to a nil lookup, and the gate read
// that nil as "nothing to check" and admitted the request. Handlers that key
// off the raw path values rather than a resolved record then operated under a
// caller-fabricated key: an unrelated account created a working webhook on
// `anyone/does-not-exist`. The path values already tell the two apart.
func (s *Server) namedTargetsResolve(r *http.Request) targetVerdict {
	if repoNamedInRequest(r) && s.repoFromPATRequest(r) == nil {
		return targetMissing
	}
	if orgLogin := r.PathValue("org"); orgLogin != "" && s.store.GetOrg(orgLogin) == nil {
		return targetMissing
	}
	return targetAllowed
}

// repoNamedInRequest reports whether the path names a repository at all, which
// is a different question from whether that repository resolves.
func repoNamedInRequest(r *http.Request) bool {
	if r.PathValue("owner") != "" && r.PathValue("repo") != "" {
		return true
	}
	return r.PathValue("repository_id") != ""
}

// accountKind narrows an installation-reach test to one kind of account.
// organizationAccount exists because the target type matters as much as the
// login: nothing in the store keeps a user login and an organization login
// distinct — CreateOrg checks only OrgsByLogin, and user creation only
// UsersByLogin — so matching on the login alone let an app installed on a *user*
// named `acme` administer the *organization* named `acme`, up to and including
// minting a runner registration token for it. anyAccount is for resources that
// hang off an account of either kind, such as a ProjectV2.
// (accountKind itself lives in internal/store as AccountKind — ARCH-003.)

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
// Mirrors userToServerReachesRepo for the account half.
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
//
// A gho_ OAuth-app token has no app installation behind it and acts purely as
// the user, so it is not constrained here.
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

// installationMayAccessTarget checks that the installation behind a token
// actually covers the repository and organization the path names. The token's
// permission map was already checked by the caller; this answers which
// resources that grant is over.
func (s *Server) installationMayAccessTarget(r *http.Request, tok *store.InstallationToken, scope store.PermScope) bool {
	if repo := s.repoFromPATRequest(r); repo != nil {
		// A public repository is readable by anyone, including an app whose
		// installation is elsewhere — the same carve-out canReadRepoAsUser and the
		// fine-grained PAT checks already make. Without it, scoping the write
		// path also removed every read an app could perform against a public
		// repository it is not installed on.
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

// installationReachesRepo reports whether an installation covers a repository:
// the repository must belong to the account the app was installed on, must be
// within a `selected` installation's chosen set, and must be within any
// narrower set the token itself was scoped to.
func (s *Server) installationReachesRepo(tok *store.InstallationToken, repo *store.Repo) bool {
	if tok == nil || repo == nil {
		return false
	}
	return installationCovers(s.store.GetInstallation(tok.InstallationID), tok.RepositoryIDs, repo)
}

// installationCovers is the shared predicate: the repository must belong to the
// account the app was installed on, be within a non-"all" installation's chosen
// set, and be within any narrower set the token itself was scoped to.
//
// The selection test is spelled deny-by-default. Reading it as
// `== "selected"` would admit every repository for any future selection value
// the field grows, which is the wrong way round for an access check.
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

// installationReachesAccount reports whether the installation behind a token is
// on the account the request names.
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

// viewerHasRepoPermission answers, for whichever credential the request
// carries, "may it do this kind of thing to this repository at this level".
//
// It is an intersection of two independent questions, and for an app credential
// neither half is optional. The permission map says what kind of act the grant
// covers and knows nothing about repositories; the installation says which
// repositories the grant is over and knows nothing about acts. A version that
// asked only the second admitted an app holding nothing but metadata:read to
// every push and admin mutation on the GraphQL lane and to both git transports.
//
// The scope belongs to the caller because only the caller knows it: GitHub
// grants issue triage at issues:write and a push at contents:write, so one
// scope chosen here would refuse apps GitHub allows.
//
// It takes a context rather than a request so GraphQL resolvers, which hold a
// p.Context and no *http.Request, reach the same decision.
func (s *Server) viewerHasRepoPermission(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	return s.viewerMayActOnRepo(ctx, repo, scope, level, repoCapabilityForScope(scope, level))
}

// viewerMayActOnRepo is viewerHasRepoPermission where the standing a bearer
// needs is not the one the scope implies. Editing a code-scanning alert is
// granted to an app at security_events:write while this server demands
// repository admin of a human, and folding the two together would either refuse
// every app GitHub allows or admit every collaborator.
func (s *Server) viewerMayActOnRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, grant, standing store.PermLevel) bool {
	if !s.credentialGrantsRepo(ctx, repo, scope, grant) {
		return false
	}
	return s.principalHoldsRepoCapability(ctx, repo, standing)
}

// credentialGrantsRepo is the credential half of viewerHasRepoPermission: the
// grant the request's credential carries, intersected with the repositories
// that grant is over. Only a browser session reaches the bottom of it: a
// session carries no selection of its own, so it is decided entirely by the
// principal half.
//
// It is separate from that half because the author exemption on the GraphQL
// mutation lane relaxes the principal and must not relax this: the author of an
// issue may retitle it, but not through an app that was never granted issues.
func (s *Server) credentialGrantsRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	if repo == nil {
		return false
	}
	// Data that is public is public to every credential, which is the carve-out
	// installationMayAccessTarget and the fine-grained PAT checks already make.
	// It is asked before the grant so that scoping the app credentials did not
	// also remove the reads an anonymous caller may perform.
	if level == store.PermRead && !repo.Private && publicReadAllowed(scope) {
		return true
	}
	if tok := ghInstallationTokenFromContext(ctx); tok != nil {
		if !hasPerm(tok.Permissions, scope, level) {
			return false
		}
		return s.installationReachesRepo(tok, repo)
	}
	// A user-to-server token has to be intersected here as well, not only in
	// credentialMayAccessTarget. That covers requirePerm routes; handlers that
	// gate themselves land here, and without this arm the token is
	// indistinguishable from a session, borrows the bearer's own collaborator
	// access, and ends up broader than the ghs_ token of the same app, which is
	// refused.
	//
	// Both prefixes are asked the grant question and only ghu_ is asked the
	// reach question, which is not an oversight: a gho_ OAuth-App token carries
	// classic scopes that userToServerHasPerm knows how to evaluate, but it has
	// no installation anywhere to reach through, so a reach test would refuse
	// every OAuth app outright. userToServerReachesRepo encodes that half and
	// admits an AppID-less token for exactly this reason.
	if uts := ghUserToServerTokenFromContext(ctx); uts != nil {
		if !userToServerHasPerm(uts, scope, level, s.store) {
			return false
		}
		return s.userToServerReachesRepo(uts, repo)
	}
	// A fine-grained PAT names both halves itself — the repositories it selects
	// and the permission map over them — and neither is the bearer's own
	// standing. requirePerm consults them through fineGrainedPATAllows, but
	// /api/graphql and the git transports never reach requirePerm, so without
	// this arm a token selecting one repository acted on every repository its
	// bearer could reach.
	if token := ghPersonalAccessTokenFromContext(ctx); token != nil {
		if token.FineGrained {
			return s.fineGrainedPATGrantsRepo(token, repo, scope, level)
		}
		// A classic token's scope selection, on the same terms
		// classicScopeGateApplies sets for the routed gate: writes only, because
		// the classic scopes are coarse and unambiguous there while GitHub's read
		// rules vary per endpoint. The "names a repository" half of that test is
		// satisfied by construction here.
		return level < store.PermWrite || classicScopeCovers(token.Scopes, scope, level)
	}
	return true
}

// fineGrainedPATGrantsRepo is fineGrainedPATAllows for a repository already
// resolved, so the resolvers and transports that hold no *http.Request reach
// the same decision the routed gate makes.
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
// owner and repository selection cover the repository.
//
// The selection test is spelled allow-by-enumeration for the same reason
// installationCovers is: an unrecognised selection value grants nothing.
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
// on the repository in their own right. An installation token's bearer is the
// synthetic bot the middleware puts on the context, which owns nothing and
// collaborates on nothing, so the installation arm above stands in for it.
func (s *Server) principalHoldsRepoCapability(ctx context.Context, repo *store.Repo, need store.PermLevel) bool {
	if repo == nil {
		return false
	}
	if ghInstallationTokenFromContext(ctx) != nil {
		return true
	}
	// The user-scoped predicates are reached from here and nowhere else: this is
	// the one place in the package allowed to ask them on a request's behalf.
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

// viewerCanReadRepo, viewerCanPushRepo and viewerCanAdminRepo are the three
// shorthands for the levels most handlers want, each naming the scope its level
// corresponds to on a repository's own contents and settings. A handler whose
// subject is issues, pull requests, actions or any other scope must call
// viewerHasRepoPermission with that scope instead — these would refuse an app
// GitHub grants.
func (s *Server) viewerCanReadRepo(ctx context.Context, repo *store.Repo) bool {
	return s.viewerHasRepoPermission(ctx, repo, store.ScopeMetadata, store.PermRead)
}

func (s *Server) viewerCanPushRepo(ctx context.Context, repo *store.Repo) bool {
	return s.viewerHasRepoPermission(ctx, repo, store.ScopeContents, store.PermWrite)
}

func (s *Server) viewerCanAdminRepo(ctx context.Context, repo *store.Repo) bool {
	return s.viewerHasRepoPermission(ctx, repo, store.ScopeAdministration, store.PermWrite)
}

// credentialGrantsAccount is credentialGrantsRepo for a resource that belongs to
// an account rather than to a repository: an organization's membership, a
// ProjectV2. Same intersection, same reason — the permission map says what kind
// of act the grant covers, the installation says which account it is over, and
// a user-to-server token that skipped the second half acted as its bearer on
// accounts the app was never installed on.
//
// A browser session carries no selection of its own and is unconstrained here;
// its standing is the caller's own.
func (s *Server) credentialGrantsAccount(ctx context.Context, kind store.AccountKind, login string, scope store.PermScope, level store.PermLevel) bool {
	if login == "" {
		return false
	}
	if tok := ghInstallationTokenFromContext(ctx); tok != nil {
		return hasPerm(tok.Permissions, scope, level) && s.installationReachesAccount(tok, kind, login)
	}
	// Grant for both prefixes, reach for ghu_ only — the same asymmetry
	// credentialGrantsRepo spells out, and userToServerReachesAccount encodes
	// the gho_ side of it.
	if uts := ghUserToServerTokenFromContext(ctx); uts != nil {
		return userToServerHasPerm(uts, scope, level, s.store) && s.userToServerReachesAccount(uts, kind, login)
	}
	if token := ghPersonalAccessTokenFromContext(ctx); token != nil && token.FineGrained {
		return s.fineGrainedPATGrantsAccount(token, login, scope, level)
	}
	return true
}

// fineGrainedPATGrantsAccount is fineGrainedPATGrantsRepo for the account half:
// a fine-grained token belongs to exactly one resource owner, and a token
// belonging to somebody else reaches this account through no permission it
// holds.
//
// Both halves of the permission map are consulted because which half carries a
// name is a fact about the resource, not about the token: creating a repository
// under an organization is granted by the *repository* Administration
// permission, while that organization's membership is granted by the
// organization Members permission. The routed gate picks a half per URL
// pattern; here there is no pattern to pick from, and narrowing to one half
// would refuse grants GitHub allows.
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
// — including a pending invitation, which viewerIsOrgMember rightly refuses —
// need this on its own, because the membership they are about to return is the
// other half of the decision.
func (s *Server) viewerReachesOrg(ctx context.Context, orgLogin string) bool {
	return s.credentialGrantsAccount(ctx, store.OrganizationAccount, orgLogin, store.ScopeMetadata, store.PermRead)
}

// orgRole is the level a caller must hold on an organization for
// viewerHoldsOrgRole to admit it.
type orgRole int

const (
	orgRoleMemberLevel orgRole = iota
	orgRoleAdminLevel
)

// viewerHoldsOrgRole is the organization counterpart of viewerCanReadRepo: the
// single credential-aware answer to "may this request act on this org at this
// level". Handlers must use it rather than the user-scoped org predicates,
// which cannot see the app behind a user-to-server token — an
// app with no installation anywhere was minting org webhooks through its
// bearer's admin role.
//
// An installation token's viewer is a bot with no membership. On a
// requirePerm route the typed grant in the context stands in for human
// membership: the middleware has already checked both the permission map and
// the installation's target, and this re-check keeps the resolver safe even if
// its organization argument did not come from the request path. Outside that
// chokepoint there is no grant, so a synthetic bot cannot acquire organization
// standing merely by reaching this helper.
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

// viewerCanAdminOrg and viewerIsOrgMember are the two names call sites use;
// both resolve to the one predicate above.
func (s *Server) viewerCanAdminOrg(ctx context.Context, orgLogin string) bool {
	return s.viewerHoldsOrgRole(ctx, orgLogin, orgRoleAdminLevel)
}

func (s *Server) viewerIsOrgMember(ctx context.Context, orgLogin string) bool {
	return s.viewerHoldsOrgRole(ctx, orgLogin, orgRoleMemberLevel)
}

// scopeAdministersResource reports whether a permission scope governs the
// administration of its resource rather than its contents. Writing a secret, a
// branch-protection rule or a webhook is an administrative act however modestly
// the route declared its level.
func scopeAdministersResource(scope store.PermScope) bool {
	switch scope {
	case store.ScopeAdministration, store.ScopeOrgAdministration, store.ScopeOrganizationHooks, store.ScopeSecrets,
		store.ScopeDependabotSecrets, store.ScopePATs, store.ScopePATRequests:
		return true
	}
	return false
}

// repoCapabilityForScope is resourceCapabilityFor without the request: the
// standing a bearer needs on a repository for a grant of (scope, level). The
// outside-contributor downgrade is keyed on a request path and so cannot be
// decided here; callers that need it name the standing themselves.
func repoCapabilityForScope(scope store.PermScope, level store.PermLevel) store.PermLevel {
	if scopeAdministersResource(scope) {
		return store.PermAdmin
	}
	return level
}

// resourceCapabilityFor maps a permission scope, level and request method onto
// the capability the caller must hold on the resource named in the path.
//
// Two adjustments to the declared level, both matching GitHub:
//
// Scopes that administer a resource demand admin whatever level they were
// declared with — writing a secret or a branch-protection rule is an
// administrative act, not a write to repository contents.
//
// And "write" on the collaborative scopes does not mean repository write.
// Anyone who can read a repository may open an issue on it, propose a pull
// request from a fork, or submit a review; those are how outside contributors
// participate, and demanding push access for them would wall off the normal
// contribution path. Creating is therefore gated on read. Editing and deleting
// still require push, which is what keeps a stranger from deleting labels or
// closing other people's issues.
func resourceCapabilityFor(scope store.PermScope, level store.PermLevel, method, path string) store.PermLevel {
	if scopeAdministersResource(scope) {
		return store.PermAdmin
	}
	// Team-repository association endpoints carry {owner}/{repo} in their
	// path, but the repository is the object being assigned to the team, not
	// the caller's authorization boundary. Team membership/maintainer checks
	// in the handler decide the standing; requiring repository push here would
	// incorrectly override an org's read-only base permission.
	if scope == store.ScopeMembers && strings.Contains(path, "/teams/") && strings.Contains(path, "/repos/") {
		return store.PermRead
	}
	// scopeSecurityEvents is not here either, and was briefly. Promoting it to
	// admin refused a collaborator with push the alert reads GitHub grants at
	// security_events:read — and worse, it routed those denials into the 403
	// arm of denyResourceAccess, which reintroduced exactly the existence
	// oracle the 404-on-read rule three lines below exists to prevent: 403 for
	// a private repository that exists, 404 for one that does not.
	// scopeMembers is deliberately NOT in that list, though it governs team and
	// membership changes. On GitHub a team maintainer manages their own team's
	// membership without being an organization admin, and a non-member reading
	// teams gets 404 rather than 403. Demanding org admin here refused the
	// maintainer and turned the non-member's 404 into a 403 that confirms the
	// team exists. Those checks belong to the team resolvers, which already
	// distinguish maintainer from member from outsider.
	if method == http.MethodPost && isOutsideContributorPost(path) {
		return store.PermRead
	}
	// GitHub lets a comment's author edit or delete that comment without
	// repository push access. Keep the credential grant at write (the route's
	// declared level is still checked above) but defer the resource decision to
	// the handler, which can inspect the comment owner. This is deliberately
	// path-specific: labels, milestones, assignments, and other writes under
	// the same Issues/Pull requests scopes continue to require push.
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

// isOutsideContributorPost matches the few creations an outside contributor
// legitimately performs with only read access: filing an issue, proposing a
// pull request from a fork, commenting, reviewing, reacting, forking.
//
// This used to be keyed on the scope rather than the path, which was too
// coarse. `POST .../labels`, `.../milestones`, `.../issues/{n}/assignees` and
// `.../pulls/{n}/requested_reviewers` are all registered under scopeIssues or
// scopePullRequests at write level, and all of them require push on GitHub —
// so downgrading every POST under those scopes let any signed-in account create
// labels and assign issues on any public repository.
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
// request path and reports whether the user holds the required capability on
// it. A request naming no resource carries no resource decision to make and is
// admitted — the handler's own checks then apply.
func (s *Server) principalMayAccessTarget(r *http.Request, user *store.User, need store.PermLevel) bool {
	if user == nil {
		return false
	}
	if user.SiteAdmin {
		return true
	}
	// The organization is checked first, and separately, because the two are
	// not alternatives. Several org routes also carry {repository_id}, and that
	// id comes from the caller — so resolving the repository and returning its
	// verdict let someone name a repository they own and thereby administer a
	// stranger's organization. Both must pass when both are named.
	//
	// Organizations are gated at admin only. Read and write access to
	// org-scoped collections varies per endpoint on GitHub, so tightening those
	// belongs with the per-family resolvers rather than in this wrapper.
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

// denyResourceAccess answers a failed resource check.
//
// A caller who cannot read the repository is answered 404 whatever level the
// route asked for. Choosing the status by the required level instead made every
// write and admin route an existence oracle: an unrelated caller got 403 for a
// private repository that exists and 404 for one that does not, so the pair of
// answers proved which private names are real. Only a caller who can read the
// repository, and merely lacks the standing to change it, gets the 403 GitHub
// gives.
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
		// A path naming a repository that does not exist is not the same as a
		// path naming no repository, and only the latter has nothing to check.
		return !repoNamedInRequest(r)
	}
	return fineGrainedPATSelectsRepo(token, repo)
}

// classicScopeGateApplies reports whether a classic token's scope selection is
// checked for this (scope, level) against this request.
//
// Two deliberate bounds, both about what the permission constants can honestly
// say. They model repository and organization permissions, so they stand in
// for a classic scope selection only where the request names one of those —
// account-level routes (keys, gists, notifications) answer to classic scopes
// this model does not carry. And the level mapping is trustworthy for writes,
// where the classic scopes are coarse and unambiguous, while GitHub's read
// rules vary per endpoint; a wrong read denial also turns into a 404 that
// hides a resource the caller can legitimately see.
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
// request's credential carries, and whether it carries one at all.
//
// A classic PAT and a gho_ OAuth-App user token are the same scope model, and
// only the first was being read here. That is how an OAuth token holding no
// scopes at all passed the self-gating routes a classic token of the same shape
// is refused, organization webhook creation among them.
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
// handlers that perform their own role checks instead of using requirePerm. It
// runs after ServeMux has filled path values, so a selected-repository token
// cannot inherit its owner's wider membership through those handlers, and a
// classic scope selection cannot be exceeded through them either —
// `DELETE /orgs/{org}` is one of the handlers that never reaches requirePerm.
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
	// A team route naming a repository is still a team route: the repository
	// in the path is the team's grant, not the resource being administered.
	// Classified by the {repo} in the path, adding a repository to a team
	// demanded repository administration rather than organization membership.
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
		// "metadata" is auto-granted on every installation per real GH; honour it
		// for readability checks.
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
		// ghu_: use the installation's permissions. A token scoped to
		// specific installations (POST /applications/{cid}/token/scoped)
		// must be checked against exactly those; an unscoped token checks
		// any installation of the app.
		//
		// Every candidate installation is asked, not just the first one
		// reached. Returning the first candidate's verdict decided an
		// authorization question by Go's map iteration order once an app was
		// installed in more than one place: the same token and the same scope
		// answered differently from one request to the next. The reach half
		// already scans every installation this way, and the two must range
		// over the same set or their intersection is not the one either of
		// them describes.
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
	// gho_: classic OAuth scopes → perm mapping.
	return classicScopeCovers(tok.Scopes, scope, level)
}

// validPermLevelString reports whether s is one of the permission levels the
// App API accepts in request bodies.
func validPermLevelString(s string) bool {
	switch strings.ToLower(s) {
	case "read", "write", "admin":
		return true
	}
	return false
}

// validateRequestedPermissions checks a token-mint request's permissions map
// against the installation's granted permissions: every requested scope must
// be granted at >= the requested level (metadata:read is implicitly granted
// on every installation). Returns the first offending scope and false on
// escalation or an unknown level value.
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

// classicScopeGrant is one classic OAuth scope's reach over a fine-grained
// permission: holding oauth confers that permission up to upTo.
type classicScopeGrant struct {
	oauth string
	upTo  store.PermLevel
}

// classicScopeGrants maps every permission this server gates on to the classic
// OAuth scopes real GitHub accepts for it (the "Scopes for OAuth apps"
// table). Deny-by-default: a permission with no entry is granted to no classic
// credential, and cannot be introduced by accident because startup refuses a
// permScope that is missing here.
//
// `repo` is deliberately broad. GitHub documents it as full access to public
// and private repositories including collaborators, webhooks and deployment
// statuses, plus management of organization-owned projects, invitations and
// team memberships, plus deletion of repositories the holder owns.
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

// allPermScopes enumerates every permission constant declared above.
// TestClassicScopeGrantsCoverEveryPermission keeps it in step with the const
// block, and init below refuses to start when a listed permission has no
// classic mapping — the alternative being a gate that quietly denies (or, in
// the shape this replaced, quietly admits) whatever nobody thought about.
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

// parseClassicScopes splits a scope string into its members. Both separators
// GitHub uses appear in this codebase: X-OAuth-Scopes and stored PATs are
// comma-separated, while the OAuth `scope` request parameter is
// space-separated, and a space-separated string read as one scope name grants
// nothing at all.
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
// (scope, level).
//
// The empty scope string is a real GitHub credential shape, not an internal
// escape hatch: a classic PAT with no scopes selected reads public information
// and nothing else. That is exactly what the scopeMetadata carve-out below
// gives it.
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
// repository is.
//
// "Public repository" is not the same as "every scope on it is public". The
// listing endpoints for Actions secrets and variables are registered under
// scopeSecrets at read level, and they return secret names and variable
// *values* — a repository being public says nothing about those. A blanket
// public-repo bypass therefore hands any credential holding secrets:read the
// variables of every public repository on the instance, which is why this is
// keyed on the scope rather than on the repository alone.
func publicReadAllowed(scope store.PermScope) bool {
	switch scope {
	case store.ScopeMetadata, store.ScopeContents, store.ScopeIssues, store.ScopeDiscussions, store.ScopePullRequests,
		store.ScopePages, store.ScopeChecks, store.ScopeReactions, store.ScopeProjects, store.ScopeDeployments:
		return true
	}
	return false
}
