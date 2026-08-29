// Package graphqlapi is the GraphQL resolver layer: schema assembly, the type
// registry, the query/mutation resolver families, the mutation authorization
// policy table, and Relay connection pagination.
//
// It depends only on internal/store, internal/gitstore and graphql-go, never
// internal/server; HTTP-layer needs are injected through [Config] seams so the
// compiler enforces the layering (ARCH-003).
package graphqlapi

import (
	"context"

	"github.com/graphql-go/graphql"
	"github.com/rs/zerolog"

	"github.com/e6qu/bleephub/internal/store"
)

// Authz delegates every authorization decision to the server, so GraphQL and
// REST answer from one implementation.
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
	// account named by login — the user themselves, an org owner, or a site
	// admin. Sponsors gates listing/tier management, payout figures and
	// private-sponsorship visibility on this standing.
	ViewerCanAdminAccount(ctx context.Context, login string) bool
	// ViewerMayMigrateOrg reports whether the request may start, read or
	// download an org's migrations (an owner or a granted migrator).
	ViewerMayMigrateOrg(ctx context.Context, org *store.Org) bool
	VisibleRepos(ctx context.Context, repos []*store.Repo) []*store.Repo
	CanReadProjectV2(ctx context.Context, user *store.User, owner *store.ProjectV2Owner, p *store.ProjectV2) bool
	CanWriteProjectV2(ctx context.Context, user *store.User, owner *store.ProjectV2Owner) bool
}

// Events emits the webhook events GraphQL mutations produce. The payload
// builders stay in the HTTP layer, which renders the same repo/issue/PR JSON
// the REST surface serves and knows the instance's public origin.
type Events interface {
	EmitWebhookEvent(repoKey, eventType, action string, payload interface{})
	BuildIssuesPayload(repo *store.Repo, issue *store.Issue, sender *store.User, action string) map[string]interface{}
	BuildPullRequestPayload(repo *store.Repo, pr *store.PullRequest, sender *store.User, action string) map[string]interface{}
	RepoPayload(repo *store.Repo) map[string]interface{}
	SenderPayload(user *store.User) map[string]interface{}
	// EmitIssueChanges / EmitPullRequestChanges fan one mutation out into GitHub's
	// per-change actions (edited, labeled, assigned, milestoned, …), behind the
	// seam so REST and GraphQL cannot drift on the derivation.
	EmitIssueChanges(repo *store.Repo, issue *store.Issue, sender *store.User, change store.SubjectChange)
	EmitPullRequestChanges(repo *store.Repo, pr *store.PullRequest, sender *store.User, change store.SubjectChange)
	// EmitProjectV2Event / EmitSponsorshipEvent deliver families whose subject
	// belongs to an account, not a repository, so they cannot go through
	// EmitWebhookEvent's repo-keyed path.
	EmitProjectV2Event(event store.ProjectV2Event)
	EmitSponsorshipEvent(action string, transition *store.SponsorsTransition, sender *store.User)
	// EmitCheckRunEvent / EmitCheckSuiteEvent render the run and suite through
	// the HTTP layer's own JSON shapes, which the resolver layer cannot reach.
	EmitCheckRunEvent(repoKey string, checkRunID int64, action string)
	EmitCheckSuiteEvent(repoKey string, suiteID int64, action string)
	// EmitDeploymentStatusEvent's action is the status's own state, per GitHub.
	EmitDeploymentEvent(repo *store.Repo, d *store.Deployment, sender *store.User, action string)
	EmitDeploymentStatusEvent(repo *store.Repo, d *store.Deployment, status *store.DeploymentStatus, sender *store.User)
}

// Pulls is the merge gate plus the PR file-diff renderer, shared with the REST
// merge path so GraphQL cannot become a way around branch protection.
type Pulls interface {
	PRHeadSha(repo *store.Repo, pr *store.PullRequest) string
	MissingRequiredChecks(repo *store.Repo, baseBranch, headSha string) []string
	// RequiredStatusCheckContexts reports every context branch protection
	// demands, so StatusContext.isRequired / CheckRun.isRequired answer with
	// the same set the merge gate enforces.
	RequiredStatusCheckContexts(repo *store.Repo, baseBranch string) []string
	CanMergePullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string)
	CompletePullRequestMerge(repo *store.Repo, pr *store.PullRequest, user *store.User, method, commitTitle, commitMessage, expectedHead string) (string, string)
	ChangedFiles(repo *store.Repo, pr *store.PullRequest, baseURL string) ([]map[string]interface{}, error)
	// MaybeAutoMerge / MaybeAutoMergeRepo / MaybeAutoMergeHeadSHA re-evaluate
	// armed auto-merges after the event they wait for lands through GraphQL (a
	// review, a branch-protection change, a completed check); the merge runs
	// through the REST-shared merge gate.
	MaybeAutoMerge(prID int)
	MaybeAutoMergeRepo(repo *store.Repo)
	// UpdatePullRequestBranch brings a head branch up to date with its base. It
	// is a git write, so it lives behind the seam; PUT /pulls/{n}/update-branch
	// performs the same one.
	UpdatePullRequestBranch(repo *store.Repo, pr *store.PullRequest, user *store.User, expectedHeadOid, method string) error
	AutoRequestCodeOwners(repo *store.Repo, pr *store.PullRequest, sender *store.User)
	MaybeAutoMergeHeadSHA(repo *store.Repo, headSha string)
}

// Migrations starts the GitHub Enterprise Importer's workers. The resolver
// records a migration and asks for it to run; the work — dialing the source,
// writing git storage, creating repositories — is the server's (ARCH-003).
type Migrations interface {
	// StartRepositoryMigration runs the queued migration with this id on a
	// supervised background goroutine.
	StartRepositoryMigration(id int)
	StartOrganizationMigration(id int)
	// RepositoryMigrationLogURL is a path behind the migration's own authz (not
	// a signed URL that outlives the caller's access), or "" when none exists.
	RepositoryMigrationLogURL(m *store.RepositoryMigration) string
}

// Repos is the repository machinery in the HTTP layer that reaches state the
// resolver may not (ARCH-003). Mutations ask here rather than reimplement, so
// GraphQL and REST cannot drift on what each effect carries with it.
type Repos interface {
	// RenameRepository carries every record embedding the full name across;
	// on error nothing moved.
	RenameRepository(repo *store.Repo, newName string) error
	// CreateGitRef, UpdateGitRef and DeleteGitRef carry everything the git-refs
	// REST routes do (branch-protection refusal, push-protection, fast-forward
	// test, compare-and-set, push machinery, webhooks). Reaching the storer
	// directly would bypass branch protection, so the ref mutations ask here.
	CreateGitRef(ctx context.Context, repo *store.Repo, sender *store.User, qualifiedName, oid string) error
	UpdateGitRef(ctx context.Context, repo *store.Repo, sender *store.User, qualifiedName, oid string, force bool) error
	DeleteGitRef(ctx context.Context, repo *store.Repo, sender *store.User, qualifiedName string) error
	// MergeBranch answers the merge commit's oid, or "" when head was already
	// an ancestor of base — POST /repos/{owner}/{repo}/merges.
	MergeBranch(ctx context.Context, repo *store.Repo, sender *store.User, base, head, commitMessage, authorEmail string) (string, error)
	// CreateCommitOnBranch is the multi-file form of the contents API's commit,
	// requiring the head to currently be expectedHeadOid.
	CreateCommitOnBranch(ctx context.Context, repo *store.Repo, sender *store.User, qualifiedName, expectedHeadOid string,
		additions map[string][]byte, deletions []string, headline, body string) (string, error)
	// GenerateFromTemplate mirrors POST .../generate; the caller may generate
	// under their own account or an org they are an active member of.
	GenerateFromTemplate(ctx context.Context, template *store.Repo, sender *store.User, ownerLogin, name, description string, includeAllBranches, private bool) (*store.Repo, error)
	// RevertPullRequest opens a PR undoing a merged one and answers its id.
	RevertPullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest, sender *store.User, title, body string, draft bool) (int, error)
	// ReviewPendingDeployments releases or fails a run's reviewer-protected
	// deployments — POST /actions/runs/{id}/pending_deployments — and refuses a
	// self-review where preventSelfReview is set.
	ReviewPendingDeployments(ctx context.Context, wf *store.Workflow, envIDs []int, state, comment string, reviewer *store.User) ([]string, error)
}

// RateSnapshot is the rate-limit accounting the rateLimit root field reports.
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
	// Repos is the repository machinery the mutation surface reaches for.
	Repos Repos
	// UserFromContext reads the already-authenticated principal; authentication
	// stays in the HTTP layer.
	UserFromContext func(ctx context.Context) *store.User
	// APIRate reads the request's rate-limit snapshot from the context.
	APIRate func(ctx context.Context) RateSnapshot
	// BuildCommit reports the build's commit SHA (Query.meta). A func because
	// the server may receive its build identity after the resolver is built.
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
	repos           Repos
	userFromContext func(ctx context.Context) *store.User
	apiRateFn       func(ctx context.Context) RateSnapshot
	buildCommit     func() string

	graphqlTypes  graphQLTypeRegistry
	graphqlSchema graphql.Schema

	// extraSchemaTypes holds schema-fidelity shells reachable only through an
	// interface/union possible-type or not at all, spread into graphql.NewSchema's
	// Types so introspection lists them. registerExtraSchemaType appends here from
	// any family builder.
	extraSchemaTypes []graphql.Type

	actionsTypes *actionsFamilyTypes

	enterpriseOrgMembershipConnMemo *graphql.Object
	enterpriseOrderInputs           map[string]*graphql.InputObject

	// graphql-go rejects two objects of one name, so mutation types more than one
	// family names (CheckRunOutput, ProjectColumn, …) are minted once through
	// these memos and looked up by GitHub's spelling.
	mutationObjects    map[string]*graphql.Object
	mutationInputs     map[string]*graphql.InputObject
	mutationInterfaces map[string]*graphql.Interface
	mutationUnions     map[string]*graphql.Union
}

// NewResolver builds a resolver and assembles the schema. It panics at wiring
// time when the schema cannot be built, a mutation lacks an authz policy row,
// or a required seam is nil — the resolver closures dereference the seams
// without nil guards, so a missing one must fail at startup, not mid-query.
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
	if cfg.Repos == nil {
		panic("graphqlapi.NewResolver: Config.Repos is nil — the repository mutation surface dereferences it to rename and instantiate repositories; wire the server's repository seam or a stub")
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
		repos:           cfg.Repos,
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

// ViewerCanReadProjectContent reports whether the request may read the issue or
// PR a project item points at; the Projects v2 REST surface shares it.
func (s *Resolver) ViewerCanReadProjectContent(ctx context.Context, contentType string, contentID int) bool {
	return s.viewerCanReadProjectContent(ctx, contentType, contentID)
}

// EncodeCursor / DecodeCursor expose the connection cursor codec to the two
// REST endpoints that reuse the format (actions concurrency, Projects v2 items).
func EncodeCursor(idx int) string { return encodeCursor(idx) }

// DecodeCursor decodes a connection cursor to its index (zero when invalid).
func DecodeCursor(s string) int { return decodeCursor(s) }

// seam delegators
//
// Each unexported method forwards to the injected seam under the resolver
// code's historical spelling.

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

func (s *Resolver) maybeAutoMergeRepo(repo *store.Repo) {
	s.pulls.MaybeAutoMergeRepo(repo)
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
