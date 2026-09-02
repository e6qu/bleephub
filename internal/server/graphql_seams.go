package bleephub

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"

	"github.com/e6qu/bleephub/internal/graphqlapi"
	"github.com/e6qu/bleephub/internal/store"
)

// graphqlSeams adapts the server's authz predicates, webhook emission, and merge
// gate to the interfaces the GraphQL resolver layer consumes (ARCH-003).
type graphqlSeams struct {
	s *Server
}

func (s *Server) newGraphQLResolver() *graphqlapi.Resolver {
	seams := graphqlSeams{s: s}
	return graphqlapi.NewResolver(graphqlapi.Config{
		Store:           s.store,
		Logger:          s.logger,
		Authz:           seams,
		Events:          seams,
		Pulls:           seams,
		Migrations:      seams,
		Repos:           seams,
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

// graphqlapi.Authz

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

func (a graphqlSeams) ContentInteractionRefused(actor *store.User, repo *store.Repo) (string, bool) {
	_, msg, refused := a.s.contentInteractionRefusal(actor, repo)
	return msg, refused
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

// ViewerMayMigrateOrg reuses the REST migration predicate so both surfaces admit the same principals.
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

// graphqlapi.Events

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

// EmitCheckRunEvent / EmitCheckSuiteEvent reuse the REST checks emitters, so a GraphQL rerequest fires the identical payload.
func (a graphqlSeams) EmitCheckRunEvent(repoKey string, checkRunID int64, action string) {
	a.s.CheckRunEvent(repoKey, checkRunID, action)
}

func (a graphqlSeams) EmitCheckSuiteEvent(repoKey string, suiteID int64, action string) {
	a.s.CheckSuiteEvent(repoKey, suiteID, action)
}

// EmitDeploymentEvent / EmitDeploymentStatusEvent reuse the REST deployment
// payload builders.
func (a graphqlSeams) EmitDeploymentEvent(repo *store.Repo, d *store.Deployment, sender *store.User, action string) {
	a.s.emitWebhookEvent(repo.FullName, "deployment", action,
		buildDeploymentEventPayload(repo, d, sender, action, a.s.publicOrigin()))
}

func (a graphqlSeams) EmitDeploymentStatusEvent(repo *store.Repo, d *store.Deployment, status *store.DeploymentStatus, sender *store.User) {
	a.s.emitWebhookEvent(repo.FullName, "deployment_status", "created",
		buildDeploymentStatusEventPayload(repo, d, status, sender, a.s.publicOrigin()))
}

// graphqlapi.Pulls

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

func (a graphqlSeams) MergeQueueEligible(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string) {
	return a.s.mergeQueueEligible(ctx, repo, pr)
}

func (a graphqlSeams) CompletePullRequestMerge(repo *store.Repo, pr *store.PullRequest, user *store.User, method, commitTitle, commitMessage, expectedHead string) (string, string) {
	return a.s.completePullRequestMerge(repo, pr, user, method, commitTitle, commitMessage, expectedHead)
}

// MaybeAutoMergeRepo re-evaluates every armed auto-merge after a GraphQL
// branch-protection change, as the REST protection handlers do.
func (a graphqlSeams) MaybeAutoMergeRepo(repo *store.Repo) {
	a.s.maybeAutoMergeRepo(repo)
}

func (a graphqlSeams) ChangedFiles(repo *store.Repo, pr *store.PullRequest, baseURL string) ([]map[string]interface{}, error) {
	return pullRequestChangedFiles(a.s.store, repo, pr, baseURL)
}

// UpdatePullRequestBranch brings a PR's head branch up to date with its base through the same helper PUT /pulls/{n}/update-branch uses.
func (a graphqlSeams) UpdatePullRequestBranch(repo *store.Repo, pr *store.PullRequest, user *store.User, expectedHeadOid, method string) error {
	return a.s.updatePullRequestBranch(repo, pr, user, expectedHeadOid, method, a.s.externalURL)
}

func (a graphqlSeams) MaybeAutoMerge(prID int) {
	a.s.maybeAutoMergePR(prID)
}

// MaybeAutoMergeHeadSHA releases any armed auto-merge waiting on this commit, through the same helper the REST checks routes call.
func (a graphqlSeams) MaybeAutoMergeHeadSHA(repo *store.Repo, headSha string) {
	a.s.maybeAutoMergeHeadSHA(repo, headSha)
}

func (a graphqlSeams) AutoRequestCodeOwners(repo *store.Repo, pr *store.PullRequest, sender *store.User) {
	a.s.autoRequestCodeOwners(repo, pr, sender)
}

func (a graphqlSeams) AdvanceMergeQueue(repo *store.Repo, baseBranch string) {
	a.s.advanceMergeQueue(repo, baseBranch)
}

// graphqlapi.Migrations

// StartRepositoryMigration and StartOrganizationMigration hand a recorded
// migration to the workers that perform it. The resolver layer may not dial a
// source or write git storage (ARCH-003), so queueing and running split here.
func (a graphqlSeams) StartRepositoryMigration(id int) {
	a.s.startGEIRepositoryMigration(id)
}

func (a graphqlSeams) StartOrganizationMigration(id int) {
	a.s.startGEIOrganizationMigration(id)
}

// RepositoryMigrationLogURL returns the log path, served behind the same
// migrator authorization as the migration itself — not a signed URL a caller
// could keep after losing org access (which is what GitHub returns).
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

// graphqlapi.Repos

// RenameRepository renames through the same helper PATCH /repos/{owner}/{repo}
// uses, so full-name-embedding metadata moves either way.
func (a graphqlSeams) RenameRepository(repo *store.Repo, newName string) error {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return fmt.Errorf("Repository name is invalid")
	}
	return a.s.renameRepository(owner, name, newName)
}

// CreateGitRef writes a reference for the createRef mutation through the same
// helper POST git/refs uses, so branch protection, secret scanning and the push
// machinery cannot diverge between surfaces.
func (a graphqlSeams) CreateGitRef(ctx context.Context, repo *store.Repo, sender *store.User, qualifiedName, oid string) error {
	stor := a.storageFor(repo)
	if stor == nil {
		return fmt.Errorf("the repository has no git storage")
	}
	if failure := a.s.createGitRef(ctx, repo, stor, sender,
		plumbing.ReferenceName(qualifiedName), plumbing.NewHash(oid), a.s.externalURL); failure != nil {
		return fmt.Errorf("%s", failure.message)
	}
	return nil
}

// UpdateGitRef moves a reference for the updateRef mutation; force is the
// non-fast-forward override, as in PATCH git/refs/{ref}.
func (a graphqlSeams) UpdateGitRef(ctx context.Context, repo *store.Repo, sender *store.User, qualifiedName, oid string, force bool) error {
	stor := a.storageFor(repo)
	if stor == nil {
		return fmt.Errorf("the repository has no git storage")
	}
	if failure := a.s.updateGitRef(ctx, repo, stor, sender,
		plumbing.ReferenceName(qualifiedName), plumbing.NewHash(oid), force, a.s.externalURL); failure != nil {
		return fmt.Errorf("%s", failure.message)
	}
	return nil
}

// DeleteGitRef removes a reference for the deleteRef mutation.
func (a graphqlSeams) DeleteGitRef(ctx context.Context, repo *store.Repo, sender *store.User, qualifiedName string) error {
	stor := a.storageFor(repo)
	if stor == nil {
		return fmt.Errorf("the repository has no git storage")
	}
	if failure := a.s.deleteGitRef(ctx, repo, stor, sender,
		plumbing.ReferenceName(qualifiedName), a.s.externalURL); failure != nil {
		return fmt.Errorf("%s", failure.message)
	}
	return nil
}

// MergeBranch merges head into base for the mergeBranch mutation, returning the
// merge commit's oid or "" when head was already an ancestor of base (the
// already-merged answer POST /merges encodes as its 204).
func (a graphqlSeams) MergeBranch(ctx context.Context, repo *store.Repo, sender *store.User, base, head, commitMessage, authorEmail string) (string, error) {
	hash, failure := a.s.mergeBranchRefs(repo, sender, base, head, commitMessage, authorEmail)
	if failure != nil {
		return "", fmt.Errorf("%s", failure.message)
	}
	if hash.IsZero() {
		return "", nil
	}
	return hash.String(), nil
}

// CreateCommitOnBranch writes the multi-file commit for the
// createCommitOnBranch mutation, joining headline and optional body with a
// blank line per git convention.
func (a graphqlSeams) CreateCommitOnBranch(ctx context.Context, repo *store.Repo, sender *store.User, qualifiedName, expectedHeadOid string,
	additions map[string][]byte, deletions []string, headline, body string) (string, error) {
	stor := a.storageFor(repo)
	if stor == nil {
		return "", fmt.Errorf("the repository has no git storage")
	}
	branch := strings.TrimPrefix(qualifiedName, "refs/heads/")
	message := headline
	if body != "" {
		message += "\n\n" + body
	}
	hash, failure := a.s.createCommitOnBranch(ctx, repo, stor, sender, branch, expectedHeadOid, additions, deletions, message, a.s.externalURL)
	if failure != nil {
		return "", fmt.Errorf("%s", failure.message)
	}
	return hash.String(), nil
}

// RevertPullRequest creates the revert branch and opens the PR that undoes a
// merged one, returning the new PR's database id.
func (a graphqlSeams) RevertPullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest, sender *store.User, title, body string, draft bool) (int, error) {
	branch, err := a.s.createRevertBranch(ctx, repo, pr, sender, a.s.externalURL)
	if err != nil {
		return 0, err
	}
	revert, err := a.s.store.CreatePullRequestChecked(repo.ID, sender.ID, title, body, branch, pr.BaseRefName, draft, nil, nil, 0, store.PullRequestOptions{
		HeadRepoID: repo.ID,
	})
	if err != nil {
		return 0, err
	}
	if revert == nil {
		return 0, fmt.Errorf("pull request creation failed")
	}
	a.s.autoRequestCodeOwners(repo, revert, sender)
	return revert.ID, nil
}

// ReviewPendingDeployments applies a deployment review through the same
// actions-engine path POST /actions/runs/{id}/pending_deployments runs.
func (a graphqlSeams) ReviewPendingDeployments(ctx context.Context, wf *store.Workflow, envIDs []int, state, comment string, reviewer *store.User) ([]string, error) {
	return a.s.reviewPendingDeployments(ctx, wf, envIDs, state, comment, reviewer)
}

// storageFor resolves a repository's git storage from its full name.
func (a graphqlSeams) storageFor(repo *store.Repo) gitStorage.Storer {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return nil
	}
	return a.s.store.GetGitStorage(owner, name)
}

// GenerateFromTemplate creates a repository from a template for the
// cloneTemplateRepository mutation, mirroring the REST generate route's storage
// copy, owner resolution, default-branch sync, rollback, and audit event.
func (a graphqlSeams) GenerateFromTemplate(ctx context.Context, template *store.Repo, sender *store.User, ownerLogin, name, description string, includeAllBranches, private bool) (*store.Repo, error) {
	templateOwner, templateName, ok := store.SplitRepoFullName(template.FullName)
	if !ok {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return nil, fmt.Errorf("Repository name is invalid")
	}
	var repo *store.Repo
	switch {
	case ownerLogin == "" || strings.EqualFold(ownerLogin, sender.Login):
		repo = a.s.store.CreateRepo(sender, name, description, private)
	default:
		org := a.s.store.GetOrg(ownerLogin)
		if org == nil || !a.s.viewerIsOrgMember(ctx, org.Login) {
			return nil, fmt.Errorf("you may only generate repositories for your own account or an organization you are a member of")
		}
		repo = a.s.store.CreateOrgRepo(org, sender, name, description, private)
	}
	if repo == nil {
		return nil, fmt.Errorf("a repository named %q already exists", name)
	}
	newOwner, _, _ := store.SplitRepoFullName(repo.FullName)
	if template.DefaultBranch != repo.DefaultBranch {
		a.s.store.UpdateRepo(newOwner, repo.Name, func(rp *store.Repo) {
			rp.DefaultBranch = template.DefaultBranch
		})
	}
	sig := repoSignature(store.CoalesceStr(sender.Name, sender.Login), store.CoalesceStr(sender.Email, sender.Login+"@bleephub.local"))
	if err := generateFromTemplateStorage(
		a.s.store.GetGitStorage(templateOwner, templateName),
		a.s.store.GetGitStorage(newOwner, repo.Name),
		template.DefaultBranch, includeAllBranches, sig); err != nil {
		if _, deleteErr := a.s.store.DeleteRepo(newOwner, repo.Name); deleteErr != nil {
			return nil, fmt.Errorf("repository rollback failed: %w", deleteErr)
		}
		return nil, fmt.Errorf("could not generate repository from template: %w", err)
	}
	a.s.store.UpdateRepo(newOwner, repo.Name, func(rp *store.Repo) {
		rp.TemplateRepoID = template.ID
		rp.PushedAt = a.s.currentTime()
	})
	repo = a.s.store.GetRepoByFullName(repo.FullName)
	a.s.recordAuditEvent("repo.generate", sender.Login, "", map[string]interface{}{
		"repo": repo.FullName, "repo_id": repo.ID, "template": template.FullName,
	})
	return repo, nil
}
