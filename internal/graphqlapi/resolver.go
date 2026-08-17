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
}

// Pulls is the merge gate plus the PR file-diff renderer, shared with the
// REST merge path so GraphQL cannot become a way around branch protection.
type Pulls interface {
	PRHeadSha(repo *store.Repo, pr *store.PullRequest) string
	// MissingRequiredChecks reports the required status checks not yet
	// satisfied for merging headSha into baseBranch (the server's
	// evaluateChecksForMerge, narrowed to what the resolver consumes).
	MissingRequiredChecks(repo *store.Repo, baseBranch, headSha string) []string
	CanMergePullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string)
	CompletePullRequestMerge(repo *store.Repo, pr *store.PullRequest, user *store.User, method, commitTitle, commitMessage, expectedHead string) (string, string)
	BranchProtectionRuleForPR(repo *store.Repo, baseBranch string) map[string]interface{}
	ChangedFiles(repo *store.Repo, pr *store.PullRequest, baseURL string) ([]map[string]interface{}, error)
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
	userFromContext func(ctx context.Context) *store.User
	apiRateFn       func(ctx context.Context) RateSnapshot
	buildCommit     func() string

	graphqlTypes  graphQLTypeRegistry
	graphqlSchema graphql.Schema
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

func (s *Resolver) buildIssuesPayload(repo *store.Repo, issue *store.Issue, sender *store.User, action string) map[string]interface{} {
	return s.events.BuildIssuesPayload(repo, issue, sender, action)
}

func (s *Resolver) buildPullRequestPayload(repo *store.Repo, pr *store.PullRequest, sender *store.User, action string) map[string]interface{} {
	return s.events.BuildPullRequestPayload(repo, pr, sender, action)
}

func (s *Resolver) repoPayload(repo *store.Repo) map[string]interface{} {
	return s.events.RepoPayload(repo)
}

func (s *Resolver) prHeadSha(repo *store.Repo, pr *store.PullRequest) string {
	return s.pulls.PRHeadSha(repo, pr)
}

func (s *Resolver) missingRequiredChecks(repo *store.Repo, baseBranch, headSha string) []string {
	return s.pulls.MissingRequiredChecks(repo, baseBranch, headSha)
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

func (s *Resolver) ghUserFromContext(ctx context.Context) *store.User {
	return s.userFromContext(ctx)
}

func (s *Resolver) apiRate(ctx context.Context) RateSnapshot {
	return s.apiRateFn(ctx)
}
