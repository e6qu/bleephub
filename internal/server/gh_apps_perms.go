package bleephub

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
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

type permLevel int

const (
	permRead permLevel = iota
	permWrite
	permAdmin
)

// permScope is a GitHub fine-grained permission name. The values are the
// exact keys used in an installation token's Permissions map and in the
// App API, so they must not change — but call sites reference the named
// constants, making a mistyped scope a compile error rather than a silent
// always-deny gate.
type permScope string

const (
	scopeMetadata          permScope = "metadata"
	scopeContents          permScope = "contents"
	scopeIssues            permScope = "issues"
	scopePullRequests      permScope = "pull_requests"
	scopeActions           permScope = "actions"
	scopeChecks            permScope = "checks"
	scopeSecrets           permScope = "secrets"
	scopeDeployments       permScope = "deployments"
	scopeAdministration    permScope = "administration"
	scopeMembers           permScope = "members"
	scopeOrgAdministration permScope = "organization_administration"
	scopeSecurityEvents    permScope = "security_events"
	scopeDependabotSecrets permScope = "dependabot_secrets"
	scopeCodespaces        permScope = "codespaces"
	scopeReactions         permScope = "reactions"
	scopeProjects          permScope = "projects"
	scopePages             permScope = "pages"
	scopePATRequests       permScope = "organization_personal_access_token_requests"
	scopePATs              permScope = "organization_personal_access_tokens"
)

func parsePermLevel(s string) permLevel {
	switch strings.ToLower(s) {
	case "admin":
		return permAdmin
	case "write":
		return permWrite
	case "read", "":
		return permRead
	}
	return permRead
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
func (s *Server) requirePerm(scope permScope, level permLevel, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Personal access token path, detected by: user present, no
		// installation token, no user-to-server token, no JWT-app. The PAT
		// itself sits in the auth header; middleware already resolved it into
		// ctxUser.
		instTok := ghInstallationTokenFromContext(r.Context())
		utsTok := ghUserToServerTokenFromContext(r.Context())
		jwtApp := ghAppFromContext(r.Context())
		user := ghUserFromContext(r.Context())

		switch {
		case instTok != nil:
			if !hasPerm(instTok.Permissions, scope, level) {
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
			denyResourceAccess(w, need)
			return
		}

		next(w, r)
	}
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
func (s *Server) credentialMayAccessTarget(r *http.Request, user *User, instTok *InstallationToken, scope permScope, need permLevel) targetVerdict {
	if verdict := s.namedTargetsResolve(r); verdict != targetAllowed {
		return verdict
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
		if orgLogin := r.PathValue("org"); orgLogin != "" && !s.userToServerReachesOrg(uts, orgLogin) {
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

// userToServerReachesRepo reports whether any installation of the token's app
// covers the repository, honouring a token narrowed to specific installations.
//
// A gho_ OAuth-app token has no app installation behind it and acts purely as
// the user, so it is not constrained here.
// userToServerReachesOrg reports whether any installation of the token's app is
// installed on the organization, honouring a token narrowed to specific
// installations. Mirrors userToServerReachesRepo for the org half.
func (s *Server) userToServerReachesOrg(tok *UserToServerToken, orgLogin string) bool {
	if tok == nil || tok.AppID == 0 {
		return true
	}
	for _, inst := range s.store.ListAppInstallations(tok.AppID) {
		if len(tok.InstallationIDs) > 0 && !containsRepoID(tok.InstallationIDs, inst.ID) {
			continue
		}
		if strings.EqualFold(inst.TargetType, "Organization") && strings.EqualFold(inst.TargetLogin, orgLogin) {
			return true
		}
	}
	return false
}

func (s *Server) userToServerReachesRepo(tok *UserToServerToken, repo *Repo) bool {
	if tok == nil || tok.AppID == 0 || repo == nil {
		return true
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
func (s *Server) installationMayAccessTarget(r *http.Request, tok *InstallationToken, scope permScope) bool {
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
	if orgLogin := r.PathValue("org"); orgLogin != "" && !s.installationReachesOrg(tok, orgLogin) {
		return false
	}
	return true
}

// installationReachesRepo reports whether an installation covers a repository:
// the repository must belong to the account the app was installed on, must be
// within a `selected` installation's chosen set, and must be within any
// narrower set the token itself was scoped to.
func (s *Server) installationReachesRepo(tok *InstallationToken, repo *Repo) bool {
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
func installationCovers(inst *Installation, tokenRepoIDs []int, repo *Repo) bool {
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

// installationReachesOrg reports whether an installation is installed on the
// organization the path names.
func (s *Server) installationReachesOrg(tok *InstallationToken, orgLogin string) bool {
	if tok == nil {
		return false
	}
	inst := s.store.GetInstallation(tok.InstallationID)
	if inst == nil {
		return false
	}
	// The target type matters as much as the login. Nothing in the store keeps
	// a user login and an organization login distinct — CreateOrg checks only
	// OrgsByLogin, and user creation only UsersByLogin — so matching on the
	// login alone let an app installed on a *user* named `acme` administer the
	// *organization* named `acme`, up to and including minting a runner
	// registration token for it.
	if !strings.EqualFold(inst.TargetType, "Organization") {
		return false
	}
	return strings.EqualFold(inst.TargetLogin, orgLogin)
}

func containsRepoID(ids []int, id int) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// viewerCanReadRepo answers repository readability for whichever credential the
// request carries. Read handlers must use this rather than canReadRepoAsUser
// directly, for the same reason credentialMayAccessTarget exists: an
// installation token's viewer is an installation, and the bot user standing in
// for it can read nothing.
//
// It takes a context rather than a request so GraphQL resolvers, which hold a
// p.Context and no *http.Request, reach the same decision.
func (s *Server) viewerCanReadRepo(ctx context.Context, repo *Repo) bool {
	if tok := ghInstallationTokenFromContext(ctx); tok != nil {
		// A public repository is readable by anyone; only private ones need the
		// installation to actually cover them.
		return (repo != nil && !repo.Private) || s.installationReachesRepo(tok, repo)
	}
	// A ghu_ token has to be intersected here as well, not only in
	// credentialMayAccessTarget. That covers requirePerm routes; handlers that
	// gate themselves land here, and without this arm a user-to-server token is
	// indistinguishable from a session, borrows the bearer's own collaborator
	// access, and ends up reading private repositories the app was never
	// installed on — strictly broader than the ghs_ token of the same app,
	// which is refused.
	if uts := ghUserToServerTokenFromContext(ctx); uts != nil && uts.AppID != 0 {
		if repo != nil && repo.Private && !s.userToServerReachesRepo(uts, repo) {
			return false
		}
	}
	// The user-scoped predicate is the last arm, never a peer of the above: this
	// is the one place in the package allowed to ask it on a request's behalf.
	return canReadRepoAsUser(s.store, ghUserFromContext(ctx), repo)
}

// viewerCanPushRepo is viewerCanReadRepo for the write half. The git
// smart-HTTP routes are served outside the /api middleware, so
// credentialMayAccessTarget never sees them and a user-to-server token would
// otherwise push with its bearer's own access to a repository the app was never
// installed on.
func (s *Server) viewerCanPushRepo(ctx context.Context, repo *Repo) bool {
	if tok := ghInstallationTokenFromContext(ctx); tok != nil {
		return s.installationReachesRepo(tok, repo)
	}
	if uts := ghUserToServerTokenFromContext(ctx); uts != nil && uts.AppID != 0 {
		if !s.userToServerReachesRepo(uts, repo) {
			return false
		}
	}
	return canPushRepo(s.store, ghUserFromContext(ctx), repo)
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
// An installation token's viewer is a bot with no membership, so it is denied
// by the user arm below; installations reach org resources through requirePerm,
// which checks their permission map first.
func (s *Server) viewerHoldsOrgRole(ctx context.Context, orgLogin string, need orgRole) bool {
	if orgLogin == "" {
		return false
	}
	if uts := ghUserToServerTokenFromContext(ctx); uts != nil && uts.AppID != 0 {
		if !s.userToServerReachesOrg(uts, orgLogin) {
			return false
		}
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
func resourceCapabilityFor(scope permScope, level permLevel, method, path string) permLevel {
	switch scope {
	case scopeAdministration, scopeOrgAdministration, scopeSecrets,
		scopeDependabotSecrets, scopePATs, scopePATRequests:
		return permAdmin
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
		return permRead
	}
	return level
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
func (s *Server) principalMayAccessTarget(r *http.Request, user *User, need permLevel) bool {
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
	if need >= permAdmin {
		if orgLogin := r.PathValue("org"); orgLogin != "" {
			if org := s.store.GetOrg(orgLogin); org == nil || !canAdminOrgAsUser(s.store, user, org) {
				return false
			}
		}
	}
	if repo := s.repoFromPATRequest(r); repo != nil {
		switch {
		case need >= permAdmin:
			return canAdminRepo(s.store, user, repo)
		case need >= permWrite:
			return canPushRepo(s.store, user, repo)
		default:
			return canReadRepoAsUser(s.store, user, repo)
		}
	}
	return true
}

// denyResourceAccess answers a failed resource check. A denied read is
// answered 404 so the response cannot be used to prove a private resource
// exists; write and admin denials are 403, as on GitHub.
func denyResourceAccess(w http.ResponseWriter, need permLevel) {
	switch {
	case need >= permAdmin:
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
	case need >= permWrite:
		writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) fineGrainedPATAllows(r *http.Request, token *Token, scope permScope, level permLevel) bool {
	if token.ExpiresAt != nil && !token.ExpiresAt.After(time.Now()) {
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

func (s *Server) fineGrainedPATApproved(token *Token) bool {
	if s.store.GetOrg(token.ResourceOwner) == nil {
		return true
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	for _, grant := range s.store.OrgPATGrants[token.ResourceOwner] {
		if grant.TokenID == token.FineGrainedID {
			return true
		}
	}
	return false
}

func (s *Server) repoFromPATRequest(r *http.Request) *Repo {
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

func (s *Server) fineGrainedPATResourceAllowed(r *http.Request, token *Token) bool {
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
	owner, _, ok := strings.Cut(repo.FullName, "/")
	if !ok || !strings.EqualFold(owner, token.ResourceOwner) {
		return false
	}
	switch token.RepositorySelection {
	case "all":
		return true
	case "subset":
		for _, id := range token.RepositoryIDs {
			if id == repo.ID {
				return true
			}
		}
	}
	return false
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
func classicScopeGateApplies(r *http.Request, level permLevel) bool {
	if level < permWrite {
		return false
	}
	return repoNamedInRequest(r) || r.PathValue("org") != ""
}

// denyMissingScope answers a classic PAT or OAuth token whose scope selection
// does not reach the permission the route needs.
func denyMissingScope(w http.ResponseWriter, scope permScope) {
	writeGHError(w, http.StatusForbidden, "Token does not have the OAuth scopes required for "+string(scope))
}

// enforceFineGrainedPATResource is the per-route personal-access-token gate for
// handlers that perform their own role checks instead of using requirePerm. It
// runs after ServeMux has filled path values, so a selected-repository token
// cannot inherit its owner's wider membership through those handlers, and a
// classic token cannot exceed its scope selection through them either —
// `DELETE /orgs/{org}` is one of the handlers that never reaches requirePerm.
func (s *Server) enforceFineGrainedPATResource(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if !strings.Contains(pattern, " /api/") || (!strings.Contains(pattern, "{repo}") && !strings.Contains(pattern, "{org}") && !strings.Contains(pattern, "{repository_id}")) {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		token := ghPersonalAccessTokenFromContext(r.Context())
		if token == nil {
			next(w, r)
			return
		}
		scope, level := fineGrainedPATPermissionForPattern(pattern, r.Method)
		if repo := s.repoFromPATRequest(r); repo != nil && !repo.Private &&
			(r.Method == http.MethodGet || r.Method == http.MethodHead) && publicReadAllowed(scope) {
			next(w, r)
			return
		}
		if !token.FineGrained {
			if classicScopeGateApplies(r, level) && !classicScopeCovers(token.Scopes, scope, level) {
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

func fineGrainedPATPermissionForPattern(pattern, method string) (permScope, permLevel) {
	level := permWrite
	if method == http.MethodGet || method == http.MethodHead {
		level = permRead
	}
	lower := strings.ToLower(pattern)
	if strings.Contains(lower, "/orgs/{org}/repos") {
		return scopeMetadata, level
	}
	// A team route naming a repository is still a team route: the repository
	// in the path is the team's grant, not the resource being administered.
	// Classified by the {repo} in the path, adding a repository to a team
	// demanded repository administration rather than organization membership.
	if strings.Contains(lower, "/orgs/{org}/teams") {
		return scopeMembers, level
	}
	if strings.Contains(lower, "{org}") && !strings.Contains(lower, "{repo}") && !strings.Contains(lower, "{repository_id}") {
		if strings.Contains(lower, "members") || strings.Contains(lower, "/teams") || strings.Contains(lower, "/invitations") || strings.Contains(lower, "/outside_collaborators") {
			return scopeMembers, level
		}
		return scopeOrgAdministration, level
	}
	for _, candidate := range []struct {
		fragment string
		scope    permScope
	}{
		{"/actions", scopeActions}, {"/issues", scopeIssues}, {"/milestones", scopeIssues}, {"/labels", scopeIssues},
		{"/pulls", scopePullRequests}, {"/checks", scopeChecks}, {"/deployments", scopeDeployments}, {"/environments", scopeDeployments},
		{"/pages", scopePages}, {"/codespaces", scopeCodespaces}, {"/secret-scanning", scopeSecurityEvents}, {"/code-scanning", scopeSecurityEvents},
		{"/dependabot", scopeDependabotSecrets}, {"/reactions", scopeReactions}, {"/projects", scopeProjects},
		{"/contents", scopeContents}, {"/git/", scopeContents}, {"/commits", scopeContents}, {"/branches", scopeContents}, {"/tags", scopeContents}, {"/releases", scopeContents},
		{"/collaborators", scopeAdministration}, {"/hooks", scopeAdministration}, {"/keys", scopeAdministration}, {"/rules", scopeAdministration},
	} {
		if strings.Contains(lower, candidate.fragment) {
			return candidate.scope, level
		}
	}
	if level == permRead {
		return scopeMetadata, level
	}
	return scopeAdministration, level
}

func (s *Server) filterReposForFineGrainedPAT(r *http.Request, repos []*Repo) []*Repo {
	token := ghPersonalAccessTokenFromContext(r.Context())
	if token == nil || !token.FineGrained {
		return repos
	}
	filtered := make([]*Repo, 0, len(repos))
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
func hasPerm(perms map[string]string, scope permScope, level permLevel) bool {
	if perms == nil {
		return false
	}
	got, ok := perms[string(scope)]
	if !ok {
		// "metadata" is auto-granted on every installation per real GH; honour it
		// for readability checks.
		if scope == scopeMetadata && level == permRead {
			return true
		}
		return false
	}
	return parsePermLevel(got) >= level
}

// userToServerHasPerm dispatches a user-to-server token to either the App
// installation permissions map (ghu_) or the classic OAuth scopes (gho_).
func userToServerHasPerm(tok *UserToServerToken, scope permScope, level permLevel, st *Store) bool {
	if tok.AppID > 0 {
		// ghu_: use the installation's permissions. A token scoped to
		// specific installations (POST /applications/{cid}/token/scoped)
		// must be checked against exactly those; an unscoped token checks
		// any installation of the app.
		st.mu.RLock()
		defer st.mu.RUnlock()
		if len(tok.InstallationIDs) > 0 {
			for _, id := range tok.InstallationIDs {
				if inst := st.Installations[id]; inst != nil && inst.AppID == tok.AppID {
					return hasPerm(inst.Permissions, scope, level)
				}
			}
			return false
		}
		for _, inst := range st.Installations {
			if inst.AppID == tok.AppID {
				return hasPerm(inst.Permissions, scope, level)
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
			if permScope(scope) == scopeMetadata && parsePermLevel(level) == permRead {
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
	upTo  permLevel
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
var classicScopeGrants = map[permScope][]classicScopeGrant{
	scopeMetadata:          {{"repo", permAdmin}, {"public_repo", permAdmin}},
	scopeContents:          {{"repo", permWrite}, {"public_repo", permWrite}},
	scopeIssues:            {{"repo", permWrite}, {"public_repo", permWrite}},
	scopePullRequests:      {{"repo", permWrite}, {"public_repo", permWrite}},
	scopePages:             {{"repo", permWrite}, {"public_repo", permWrite}},
	scopeChecks:            {{"repo", permWrite}, {"public_repo", permWrite}},
	scopeReactions:         {{"repo", permWrite}, {"public_repo", permWrite}},
	scopeActions:           {{"repo", permWrite}, {"public_repo", permWrite}, {"workflow", permWrite}},
	scopeDeployments:       {{"repo", permWrite}, {"public_repo", permWrite}, {"repo_deployment", permWrite}},
	scopeSecrets:           {{"repo", permAdmin}},
	scopeDependabotSecrets: {{"repo", permAdmin}},
	scopeSecurityEvents:    {{"repo", permWrite}, {"security_events", permWrite}},
	scopeCodespaces:        {{"repo", permWrite}, {"codespace", permWrite}},
	scopeAdministration: {
		{"repo", permAdmin}, {"public_repo", permWrite},
		{"admin:repo_hook", permWrite}, {"delete_repo", permAdmin},
	},
	scopeMembers: {
		{"admin:org", permAdmin}, {"write:org", permWrite}, {"read:org", permRead},
		{"repo", permWrite},
	},
	scopeOrgAdministration: {
		{"admin:org", permAdmin}, {"write:org", permWrite}, {"read:org", permRead},
	},
	scopeProjects: {
		{"project", permAdmin}, {"read:project", permRead},
		{"repo", permWrite}, {"public_repo", permWrite},
		{"admin:org", permAdmin}, {"write:org", permWrite}, {"read:org", permRead},
	},
	scopePATRequests: {{"admin:org", permAdmin}},
	scopePATs:        {{"admin:org", permAdmin}},
}

// allPermScopes enumerates every permission constant declared above.
// TestClassicScopeGrantsCoverEveryPermission keeps it in step with the const
// block, and init below refuses to start when a listed permission has no
// classic mapping — the alternative being a gate that quietly denies (or, in
// the shape this replaced, quietly admits) whatever nobody thought about.
var allPermScopes = []permScope{
	scopeMetadata, scopeContents, scopeIssues, scopePullRequests, scopeActions,
	scopeChecks, scopeSecrets, scopeDeployments, scopeAdministration, scopeMembers,
	scopeOrgAdministration, scopeSecurityEvents, scopeDependabotSecrets, scopeCodespaces,
	scopeReactions, scopeProjects, scopePages, scopePATRequests, scopePATs,
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
func classicScopeCovers(scopes string, scope permScope, level permLevel) bool {
	if scope == scopeMetadata && level == permRead {
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
func publicReadAllowed(scope permScope) bool {
	switch scope {
	case scopeMetadata, scopeContents, scopeIssues, scopePullRequests,
		scopePages, scopeChecks, scopeReactions, scopeProjects, scopeDeployments:
		return true
	}
	return false
}
