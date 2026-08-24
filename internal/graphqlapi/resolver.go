// Package graphqlapi is the GraphQL resolver layer: schema assembly, the
// type registry, every query/mutation resolver family (repos, issues,
// pulls, discussions, orgs, meta, moderation, Projects v2), the mutation
// authorization policy table, and Relay connection pagination.
//
// The package depends on internal/store (data layer), internal/gitstore
// and graphql-go only — never on internal/server. Everything the resolver
// layer needs from the HTTP layer (authorization predicates, webhook
// payload emission, the merge gate, the authenticated principal) is
// injected through [Config] seams, so the compiler enforces the layering
// (ARCH-003, following the ARCH-002 Engine pattern).
package graphqlapi

import (
	"context"

	"github.com/graphql-go/graphql"
	"github.com/rs/zerolog"

	"github.com/e6qu/bleephub/internal/store"
)

// Authz is the resolver layer's view of the server's authorization
// predicates, satisfied by the server. Every repository-visibility,
// credential-grant, and Projects-v2 access decision is delegated here so
// GraphQL and REST answer authorization questions from one implementation.
type Authz interface {
	ViewerCanReadRepo(ctx context.Context, repo *store.Repo) bool
	ViewerCanPushRepo(ctx context.Context, repo *store.Repo) bool
	ViewerCanAdminRepo(ctx context.Context, repo *store.Repo) bool
	ViewerHasRepoPermission(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool
	ViewerMayActOnRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, grant, standing store.PermLevel) bool
	CredentialGrantsRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool
	CredentialGrantsAccount(ctx context.Context, kind store.AccountKind, login string, scope store.PermScope, level store.PermLevel) bool
	PrincipalHoldsRepoCapability(ctx context.Context, repo *store.Repo, need store.PermLevel) bool
	ViewerIsOrgMember(ctx context.Context, orgLogin string) bool
	// ViewerCanAdminAccount reports whether the request may administer the
	// user or organization account named by login — the user themselves, an
	// owner of the organization, or a site administrator. GitHub Sponsors
	// gates listing management, tier management, payout figures and the
	// visibility of private sponsorships on exactly this standing.
	ViewerCanAdminAccount(ctx context.Context, login string) bool
	// ViewerMayMigrateOrg reports whether the request may start, read or
	// download an organization's migrations: an owner of that organization,
	// or a principal granted the migrator role on it. It is delegated rather
	// than recomposed here so the REST migration surface and the GraphQL one
	// cannot drift on who a migration is open to.
	ViewerMayMigrateOrg(ctx context.Context, org *store.Org) bool
	VisibleRepos(ctx context.Context, repos []*store.Repo) []*store.Repo
	CanReadProjectV2(ctx context.Context, user *store.User, owner *store.ProjectV2Owner, p *store.ProjectV2) bool
	CanWriteProjectV2(ctx context.Context, user *store.User, owner *store.ProjectV2Owner) bool
}

// Events receives the webhook events GraphQL mutations emit and renders
// their payloads. The payload builders stay in the HTTP layer (they render
// the same repo/issue/PR JSON the REST surface serves).
type Events interface {
	EmitWebhookEvent(repoKey, eventType, action string, payload interface{})
	BuildIssuesPayload(repo *store.Repo, issue *store.Issue, sender *store.User, action string) map[string]interface{}
	BuildPullRequestPayload(repo *store.Repo, pr *store.PullRequest, sender *store.User, action string) map[string]interface{}
	RepoPayload(repo *store.Repo) map[string]interface{}
	// SenderPayload renders the `sender` account a webhook body carries, in the
	// same absolute-hypermedia `simple-user` shape the REST surface serves. It
	// lives behind the seam because only the HTTP layer knows the instance's
	// public origin.
	SenderPayload(user *store.User) map[string]interface{}
	// EmitIssueChanges / EmitPullRequestChanges fan one mutation out into
	// GitHub's per-change actions (edited, labeled, assigned, milestoned, …).
	// The derivation lives behind the seam so the REST handlers and these
	// resolvers cannot drift on which actions a change produces.
	EmitIssueChanges(repo *store.Repo, issue *store.Issue, sender *store.User, change store.SubjectChange)
	EmitPullRequestChanges(repo *store.Repo, pr *store.PullRequest, sender *store.User, change store.SubjectChange)
	// EmitProjectV2Event delivers the projects_v2 event family. A project
	// belongs to an account rather than a repository, so these are delivered
	// to the owning organization's hooks and carry no repository — which is
	// why they cannot go through EmitWebhookEvent's repo-keyed path.
	EmitProjectV2Event(event store.ProjectV2Event)
	// EmitSponsorshipEvent delivers the `sponsorship` event family for a
	// billing-lifecycle transition. A sponsorship belongs to an account
	// rather than a repository, so like the projects_v2 events it cannot go
	// through EmitWebhookEvent's repo-keyed path.
	EmitSponsorshipEvent(action string, transition *store.SponsorsTransition, sender *store.User)
}

// Pulls is the merge gate plus the PR file-diff renderer, shared with the
// REST merge path so GraphQL cannot become a way around branch protection.
type Pulls interface {
	PRHeadSha(repo *store.Repo, pr *store.PullRequest) string
	// MissingRequiredChecks reports the required status checks not yet
	// satisfied for merging headSha into baseBranch (the server's
	// evaluateChecksForMerge, narrowed to what the resolver consumes).
	MissingRequiredChecks(repo *store.Repo, baseBranch, headSha string) []string
	// RequiredStatusCheckContexts reports every context branch protection
	// demands before baseBranch may be merged into, whether satisfied or
	// not. StatusContext.isRequired / CheckRun.isRequired answer from it, so
	// what `gh pr checks` marks required is the same set the merge gate
	// enforces rather than a second opinion about it.
	RequiredStatusCheckContexts(repo *store.Repo, baseBranch string) []string
	CanMergePullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string)
	CompletePullRequestMerge(repo *store.Repo, pr *store.PullRequest, user *store.User, method, commitTitle, commitMessage, expectedHead string) (string, string)
	BranchProtectionRuleForPR(repo *store.Repo, baseBranch string) map[string]interface{}
	ChangedFiles(repo *store.Repo, pr *store.PullRequest, baseURL string) ([]map[string]interface{}, error)
	// MaybeAutoMerge re-evaluates a pull request's armed auto-merge request
	// after a review lands or is dismissed through GraphQL — the merge
	// itself runs through the server's REST-shared merge gate.
	MaybeAutoMerge(prID int)
	// AutoRequestCodeOwners requests the CODEOWNERS owners of a newly opened
	// pull request's changed files as reviewers. A pull request opened through
	// GraphQL collects the same reviewers one opened through REST does.
	AutoRequestCodeOwners(repo *store.Repo, pr *store.PullRequest, sender *store.User)
}

// Migrations starts the GitHub Enterprise Importer's workers. The mutations
// that queue a migration live here, but the work itself is the server's: it
// dials the source, writes git storage and creates repositories, none of which
// the resolver layer may reach (ARCH-003). The resolver records the migration
// and asks for it to be run.
type Migrations interface {
	// StartRepositoryMigration runs the queued repository migration with this
	// database id on a supervised background goroutine.
	StartRepositoryMigration(id int)
	// StartOrganizationMigration does the same for an organization migration.
	StartOrganizationMigration(id int)
	// RepositoryMigrationLogURL is where the migration's log can be read, or
	// "" when it has not produced one. It is a path on this server behind the
	// same authorization the migration is behind rather than a signed URL, so
	// it is not a credential that outlives the caller's access.
	RepositoryMigrationLogURL(m *store.RepositoryMigration) string
}

// RateSnapshot is the API rate-limit accounting the rateLimit root field
// reports (the server's per-request snapshot, narrowed to what the
// resolver renders).
type RateSnapshot struct {
	Limit     int
	Used      int
	Remaining int
	Reset     int64
}

// Config carries the resolver layer's dependencies and injected seams.
type Config struct {
	Store  *store.Store
	Logger zerolog.Logger
	// Authz answers authorization questions (implemented by the server).
	Authz Authz
	// Events emits webhook events and renders their payloads.
	Events Events
	// Pulls is the merge gate and PR diff renderer.
	Pulls Pulls
	// Migrations starts the GitHub Enterprise Importer's workers.
	Migrations Migrations
	// UserFromContext extracts the already-authenticated user from the
	// request context. Authentication itself stays in the HTTP layer; the
	// resolver layer only reads the principal middleware attached.
	UserFromContext func(ctx context.Context) *store.User
	// APIRate reads the request's rate-limit snapshot from the context.
	APIRate func(ctx context.Context) RateSnapshot
	// BuildCommit reports the server build's commit SHA (Query.meta). It is
	// a func because the server may receive its build identity (options)
	// after the resolver is constructed.
	BuildCommit func() string
}

// Resolver owns the assembled GraphQL schema, its type registry, and every
// resolver closure. One Resolver is built per server instance.
type Resolver struct {
	store           *store.Store
	logger          zerolog.Logger
	authz           Authz
	events          Events
	pulls           Pulls
	migrations      Migrations
	userFromContext func(ctx context.Context) *store.User
	apiRateFn       func(ctx context.Context) RateSnapshot
	buildCommit     func() string

	graphqlTypes  graphQLTypeRegistry
	graphqlSchema graphql.Schema

	// The enterprise family's two memoized types: the organization-membership
	// connection two enterprise types both name, and the invitation ordering
	// inputs, keyed by GitHub's input-object name.
	enterpriseOrgMembershipConnMemo *graphql.Object
	enterpriseOrderInputs           map[string]*graphql.InputObject
}

// NewResolver builds a resolver and assembles the schema. It panics when
// the schema cannot be built or a mutation lacks an authorization policy
// row — both are programming errors that must stop the process at startup
// rather than ship an open mutation. It likewise panics when a required
// seam is nil: Authz, Events, and Pulls are dereferenced without nil guards
// by the resolver closures, so a resolver constructed without them would
// serve requests until the first authorization check, webhook emission, or
// merge crashes it mid-query instead of failing at wiring time.
func NewResolver(cfg Config) *Resolver {
	if cfg.Authz == nil {
		panic("graphqlapi.NewResolver: Config.Authz is nil — every repository-visibility and Projects-v2 access decision delegates to it; wire the server's authz seam or a stub")
	}
	if cfg.Events == nil {
		panic("graphqlapi.NewResolver: Config.Events is nil — every mutation's webhook emission and payload rendering delegates to it; wire the server's event seam or a no-op stub")
	}
	if cfg.Pulls == nil {
		panic("graphqlapi.NewResolver: Config.Pulls is nil — the merge gate and PR diff renderer delegate to it; wire the server's pulls seam or a stub")
	}
	if cfg.Migrations == nil {
		panic("graphqlapi.NewResolver: Config.Migrations is nil — the migration mutations dereference it to run the work they queue; wire the server's migration seam or a stub")
	}
	if cfg.UserFromContext == nil {
		panic("graphqlapi.NewResolver: Config.UserFromContext is nil — viewer resolution dereferences it on every request; wire the server's context extractor or a stub")
	}
	if cfg.APIRate == nil {
		panic("graphqlapi.NewResolver: Config.APIRate is nil — the rateLimit resolver dereferences it; wire the server's rate snapshot or a stub")
	}
	r := &Resolver{
		store:           cfg.Store,
		logger:          cfg.Logger,
		authz:           cfg.Authz,
		events:          cfg.Events,
		pulls:           cfg.Pulls,
		migrations:      cfg.Migrations,
		userFromContext: cfg.UserFromContext,
		apiRateFn:       cfg.APIRate,
		buildCommit:     cfg.BuildCommit,
	}
	if r.buildCommit == nil {
		r.buildCommit = func() string { return "" }
	}
	r.initGraphQLSchema()
	return r
}

// Schema is the assembled GraphQL schema, ready for graphql.Execute.
func (s *Resolver) Schema() graphql.Schema { return s.graphqlSchema }

// ViewerCanReadProjectContent reports whether the request may read the
// issue or pull request a project item points at; the Projects v2 REST
// surface shares this predicate.
func (s *Resolver) ViewerCanReadProjectContent(ctx context.Context, contentType string, contentID int) bool {
	return s.viewerCanReadProjectContent(ctx, contentType, contentID)
}

// EncodeCursor / DecodeCursor expose the GraphQL connection cursor codec
// to the two REST endpoints that reuse the cursor format
// (actions concurrency, Projects v2 items).
func EncodeCursor(idx int) string { return encodeCursor(idx) }

// DecodeCursor decodes a connection cursor to its index (zero when invalid).
func DecodeCursor(s string) int { return decodeCursor(s) }

// --- seam delegators -------------------------------------------------------
//
// The moved resolver code keeps its historical spellings; each unexported
// method below forwards to the injected seam.

func (s *Resolver) viewerCanReadRepo(ctx context.Context, repo *store.Repo) bool {
	return s.authz.ViewerCanReadRepo(ctx, repo)
}

func (s *Resolver) viewerCanPushRepo(ctx context.Context, repo *store.Repo) bool {
	return s.authz.ViewerCanPushRepo(ctx, repo)
}

func (s *Resolver) viewerCanAdminRepo(ctx context.Context, repo *store.Repo) bool {
	return s.authz.ViewerCanAdminRepo(ctx, repo)
}

func (s *Resolver) viewerHasRepoPermission(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	return s.authz.ViewerHasRepoPermission(ctx, repo, scope, level)
}

func (s *Resolver) viewerMayActOnRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, grant, standing store.PermLevel) bool {
	return s.authz.ViewerMayActOnRepo(ctx, repo, scope, grant, standing)
}

func (s *Resolver) credentialGrantsRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	return s.authz.CredentialGrantsRepo(ctx, repo, scope, level)
}

func (s *Resolver) credentialGrantsAccount(ctx context.Context, kind store.AccountKind, login string, scope store.PermScope, level store.PermLevel) bool {
	return s.authz.CredentialGrantsAccount(ctx, kind, login, scope, level)
}

func (s *Resolver) principalHoldsRepoCapability(ctx context.Context, repo *store.Repo, need store.PermLevel) bool {
	return s.authz.PrincipalHoldsRepoCapability(ctx, repo, need)
}

func (s *Resolver) viewerIsOrgMember(ctx context.Context, orgLogin string) bool {
	return s.authz.ViewerIsOrgMember(ctx, orgLogin)
}

func (s *Resolver) viewerCanAdminAccount(ctx context.Context, login string) bool {
	return s.authz.ViewerCanAdminAccount(ctx, login)
}

func (s *Resolver) viewerMayMigrateOrg(ctx context.Context, org *store.Org) bool {
	return s.authz.ViewerMayMigrateOrg(ctx, org)
}

func (s *Resolver) visibleRepos(ctx context.Context, repos []*store.Repo) []*store.Repo {
	return s.authz.VisibleRepos(ctx, repos)
}

func (s *Resolver) canReadProjectV2(ctx context.Context, user *store.User, owner *store.ProjectV2Owner, p *store.ProjectV2) bool {
	return s.authz.CanReadProjectV2(ctx, user, owner, p)
}

func (s *Resolver) canWriteProjectV2(ctx context.Context, user *store.User, owner *store.ProjectV2Owner) bool {
	return s.authz.CanWriteProjectV2(ctx, user, owner)
}

func (s *Resolver) emitWebhookEvent(repoKey, eventType, action string, payload interface{}) {
	s.events.EmitWebhookEvent(repoKey, eventType, action, payload)
}

func (s *Resolver) emitProjectV2Event(event store.ProjectV2Event) {
	s.events.EmitProjectV2Event(event)
}

func (s *Resolver) emitSponsorshipEvent(action string, transition *store.SponsorsTransition, sender *store.User) {
	s.events.EmitSponsorshipEvent(action, transition, sender)
}

func (s *Resolver) buildIssuesPayload(repo *store.Repo, issue *store.Issue, sender *store.User, action string) map[string]interface{} {
	return s.events.BuildIssuesPayload(repo, issue, sender, action)
}

func (s *Resolver) buildPullRequestPayload(repo *store.Repo, pr *store.PullRequest, sender *store.User, action string) map[string]interface{} {
	return s.events.BuildPullRequestPayload(repo, pr, sender, action)
}

func (s *Resolver) repoPayload(repo *store.Repo) map[string]interface{} {
	return s.events.RepoPayload(repo)
}

func (s *Resolver) senderPayload(user *store.User) map[string]interface{} {
	return s.events.SenderPayload(user)
}

func (s *Resolver) emitIssueChanges(repo *store.Repo, issue *store.Issue, sender *store.User, change store.SubjectChange) {
	s.events.EmitIssueChanges(repo, issue, sender, change)
}

func (s *Resolver) emitPullRequestChanges(repo *store.Repo, pr *store.PullRequest, sender *store.User, change store.SubjectChange) {
	s.events.EmitPullRequestChanges(repo, pr, sender, change)
}

func (s *Resolver) prHeadSha(repo *store.Repo, pr *store.PullRequest) string {
	return s.pulls.PRHeadSha(repo, pr)
}

func (s *Resolver) missingRequiredChecks(repo *store.Repo, baseBranch, headSha string) []string {
	return s.pulls.MissingRequiredChecks(repo, baseBranch, headSha)
}

func (s *Resolver) requiredStatusCheckContexts(repo *store.Repo, baseBranch string) []string {
	return s.pulls.RequiredStatusCheckContexts(repo, baseBranch)
}

func (s *Resolver) canMergePullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string) {
	return s.pulls.CanMergePullRequest(ctx, repo, pr)
}

func (s *Resolver) completePullRequestMerge(repo *store.Repo, pr *store.PullRequest, user *store.User, method, commitTitle, commitMessage, expectedHead string) (string, string) {
	return s.pulls.CompletePullRequestMerge(repo, pr, user, method, commitTitle, commitMessage, expectedHead)
}

func (s *Resolver) branchProtectionRuleForPR(repo *store.Repo, baseBranch string) map[string]interface{} {
	return s.pulls.BranchProtectionRuleForPR(repo, baseBranch)
}

func (s *Resolver) pullRequestChangedFiles(repo *store.Repo, pr *store.PullRequest, baseURL string) ([]map[string]interface{}, error) {
	return s.pulls.ChangedFiles(repo, pr, baseURL)
}

func (s *Resolver) maybeAutoMerge(prID int) {
	s.pulls.MaybeAutoMerge(prID)
}

func (s *Resolver) autoRequestCodeOwners(repo *store.Repo, pr *store.PullRequest, sender *store.User) {
	s.pulls.AutoRequestCodeOwners(repo, pr, sender)
}

func (s *Resolver) startRepositoryMigration(id int) {
	s.migrations.StartRepositoryMigration(id)
}

func (s *Resolver) startOrganizationMigration(id int) {
	s.migrations.StartOrganizationMigration(id)
}

func (s *Resolver) repositoryMigrationLogURL(m *store.RepositoryMigration) string {
	return s.migrations.RepositoryMigrationLogURL(m)
}

func (s *Resolver) ghUserFromContext(ctx context.Context) *store.User {
	return s.userFromContext(ctx)
}

func (s *Resolver) apiRate(ctx context.Context) RateSnapshot {
	return s.apiRateFn(ctx)
}
