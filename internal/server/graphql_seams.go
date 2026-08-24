package bleephub

import (
	"context"
	"fmt"

	"github.com/e6qu/bleephub/internal/graphqlapi"
	"github.com/e6qu/bleephub/internal/store"
)

// graphqlSeams adapts the server's authorization predicates, webhook
// emission, and merge gate to the seam interfaces the GraphQL resolver
// layer consumes (ARCH-003). The adapter exists so the Server's own method
// set keeps its unexported spellings.
type graphqlSeams struct {
	s *Server
}

// newGraphQLResolver wires the resolver layer to this server's store and
// injected seams.
func (s *Server) newGraphQLResolver() *graphqlapi.Resolver {
	seams := graphqlSeams{s: s}
	return graphqlapi.NewResolver(graphqlapi.Config{
		Store:           s.store,
		Logger:          s.logger,
		Authz:           seams,
		Events:          seams,
		Pulls:           seams,
		Migrations:      seams,
		UserFromContext: ghUserFromContext,
		APIRate: func(ctx context.Context) graphqlapi.RateSnapshot {
			rate, _ := ctx.Value(ctxAPIRateLimit).(apiRateSnapshot)
			return graphqlapi.RateSnapshot{
				Limit:     rate.Limit,
				Used:      rate.Used,
				Remaining: rate.Remaining,
				Reset:     rate.Reset,
			}
		},
		BuildCommit: func() string { return s.build.Commit },
	})
}

// --- graphqlapi.Authz ------------------------------------------------------

func (a graphqlSeams) ViewerCanReadRepo(ctx context.Context, repo *store.Repo) bool {
	return a.s.viewerCanReadRepo(ctx, repo)
}

func (a graphqlSeams) ViewerCanPushRepo(ctx context.Context, repo *store.Repo) bool {
	return a.s.viewerCanPushRepo(ctx, repo)
}

func (a graphqlSeams) ViewerCanAdminRepo(ctx context.Context, repo *store.Repo) bool {
	return a.s.viewerCanAdminRepo(ctx, repo)
}

func (a graphqlSeams) ViewerHasRepoPermission(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	return a.s.viewerHasRepoPermission(ctx, repo, scope, level)
}

func (a graphqlSeams) ViewerMayActOnRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, grant, standing store.PermLevel) bool {
	return a.s.viewerMayActOnRepo(ctx, repo, scope, grant, standing)
}

func (a graphqlSeams) CredentialGrantsRepo(ctx context.Context, repo *store.Repo, scope store.PermScope, level store.PermLevel) bool {
	return a.s.credentialGrantsRepo(ctx, repo, scope, level)
}

func (a graphqlSeams) CredentialGrantsAccount(ctx context.Context, kind store.AccountKind, login string, scope store.PermScope, level store.PermLevel) bool {
	return a.s.credentialGrantsAccount(ctx, kind, login, scope, level)
}

func (a graphqlSeams) PrincipalHoldsRepoCapability(ctx context.Context, repo *store.Repo, need store.PermLevel) bool {
	return a.s.principalHoldsRepoCapability(ctx, repo, need)
}

func (a graphqlSeams) ViewerIsOrgMember(ctx context.Context, orgLogin string) bool {
	return a.s.viewerIsOrgMember(ctx, orgLogin)
}

func (a graphqlSeams) ViewerCanAdminAccount(ctx context.Context, login string) bool {
	return a.s.viewerCanAdminAccount(ctx, login)
}

// ViewerMayMigrateOrg is the REST migration surface's own predicate, so the
// GraphQL migration surface admits exactly the principals the REST one does.
func (a graphqlSeams) ViewerMayMigrateOrg(ctx context.Context, org *store.Org) bool {
	return a.s.viewerMayMigrateOrg(ctx, org)
}

func (a graphqlSeams) VisibleRepos(ctx context.Context, repos []*store.Repo) []*store.Repo {
	return a.s.visibleRepos(ctx, repos)
}

func (a graphqlSeams) CanReadProjectV2(ctx context.Context, user *store.User, owner *store.ProjectV2Owner, p *store.ProjectV2) bool {
	return a.s.canReadProjectV2(ctx, user, owner, p)
}

func (a graphqlSeams) CanWriteProjectV2(ctx context.Context, user *store.User, owner *store.ProjectV2Owner) bool {
	return a.s.canWriteProjectV2(ctx, user, owner)
}

// --- graphqlapi.Events -----------------------------------------------------

func (a graphqlSeams) EmitWebhookEvent(repoKey, eventType, action string, payload interface{}) {
	a.s.emitWebhookEvent(repoKey, eventType, action, payload)
}

func (a graphqlSeams) EmitProjectV2Event(event store.ProjectV2Event) {
	a.s.emitProjectV2Event(event)
}

func (a graphqlSeams) EmitSponsorshipEvent(action string, transition *store.SponsorsTransition, sender *store.User) {
	a.s.emitSponsorshipEvent(action, transition, sender)
}

func (a graphqlSeams) BuildIssuesPayload(repo *store.Repo, issue *store.Issue, sender *store.User, action string) map[string]interface{} {
	return buildIssuesPayload(a.s.store, repo, issue, sender, action, a.s.publicOrigin())
}

func (a graphqlSeams) BuildPullRequestPayload(repo *store.Repo, pr *store.PullRequest, sender *store.User, action string) map[string]interface{} {
	return buildPullRequestPayload(a.s.store, repo, pr, sender, action, a.s.publicOrigin())
}

func (a graphqlSeams) RepoPayload(repo *store.Repo) map[string]interface{} {
	return repoPayload(repo, a.s.publicOrigin())
}

func (a graphqlSeams) SenderPayload(user *store.User) map[string]interface{} {
	return senderPayload(user, a.s.publicOrigin())
}

func (a graphqlSeams) EmitIssueChanges(repo *store.Repo, issue *store.Issue, sender *store.User, change store.SubjectChange) {
	a.s.issueEmitter(repo, issue, sender).emitChanges(change)
}

func (a graphqlSeams) EmitPullRequestChanges(repo *store.Repo, pr *store.PullRequest, sender *store.User, change store.SubjectChange) {
	a.s.pullRequestEmitter(repo, pr, sender).emitChanges(change)
}

// --- graphqlapi.Pulls ------------------------------------------------------

func (a graphqlSeams) PRHeadSha(repo *store.Repo, pr *store.PullRequest) string {
	return a.s.prHeadSha(repo, pr)
}

func (a graphqlSeams) MissingRequiredChecks(repo *store.Repo, baseBranch, headSha string) []string {
	return a.s.evaluateChecksForMerge(repo, baseBranch, headSha).MissingRequired
}

func (a graphqlSeams) RequiredStatusCheckContexts(repo *store.Repo, baseBranch string) []string {
	return a.s.requiredCheckContexts(repo.ID, baseBranch)
}

func (a graphqlSeams) CanMergePullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string) {
	return a.s.canMergePullRequest(ctx, repo, pr)
}

func (a graphqlSeams) CompletePullRequestMerge(repo *store.Repo, pr *store.PullRequest, user *store.User, method, commitTitle, commitMessage, expectedHead string) (string, string) {
	return a.s.completePullRequestMerge(repo, pr, user, method, commitTitle, commitMessage, expectedHead)
}

func (a graphqlSeams) BranchProtectionRuleForPR(repo *store.Repo, baseBranch string) map[string]interface{} {
	return a.s.branchProtectionRuleForPR(repo, baseBranch)
}

func (a graphqlSeams) ChangedFiles(repo *store.Repo, pr *store.PullRequest, baseURL string) ([]map[string]interface{}, error) {
	return pullRequestChangedFiles(a.s.store, repo, pr, baseURL)
}

func (a graphqlSeams) MaybeAutoMerge(prID int) {
	a.s.maybeAutoMergePR(prID)
}

func (a graphqlSeams) AutoRequestCodeOwners(repo *store.Repo, pr *store.PullRequest, sender *store.User) {
	a.s.autoRequestCodeOwners(repo, pr, sender)
}

// --- graphqlapi.Migrations --------------------------------------------------

// StartRepositoryMigration and StartOrganizationMigration hand a migration the
// GraphQL layer has recorded to the workers that actually perform it. The
// resolver layer may not dial a source or write git storage (ARCH-003), so
// queueing and running are separated at exactly this seam.
func (a graphqlSeams) StartRepositoryMigration(id int) {
	a.s.startGEIRepositoryMigration(id)
}

func (a graphqlSeams) StartOrganizationMigration(id int) {
	a.s.startGEIOrganizationMigration(id)
}

// RepositoryMigrationLogURL is where a migration's log is served from.
//
// GitHub answers migrationLogUrl with a signed URL that expires a day after
// the migration ends. Bleephub answers with a path on this server behind the
// same migrator authorization the migration itself is behind, so there is no
// URL a caller can keep once their access to the organization ends — a signed
// URL would be exactly that.
func (a graphqlSeams) RepositoryMigrationLogURL(m *store.RepositoryMigration) string {
	if m == nil || m.MigrationLogKey == "" {
		return ""
	}
	org := a.s.store.GetOrgByID(m.OwnerOrgID)
	if org == nil {
		return ""
	}
	return a.s.externalURL + fmt.Sprintf("/ui-data/orgs/%s/migrations/repositories/%d/log", org.Login, m.ID)
}
