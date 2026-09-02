package graphqlapi

import (
	"context"
	"errors"

	"github.com/e6qu/bleephub/internal/store"
)

// No-op seam stubs for tests that construct a Resolver directly (NewResolver
// panics on nil Authz/Events/Pulls — ARCH-005). Resolver-package tests exercise
// schema assembly, converters, and pagination; the server package covers seam
// behavior end to end.

// stubAuthz denies everything.
type stubAuthz struct{}

func (stubAuthz) ViewerCanReadRepo(context.Context, *store.Repo) bool  { return false }
func (stubAuthz) ViewerCanPushRepo(context.Context, *store.Repo) bool  { return false }
func (stubAuthz) ViewerCanAdminRepo(context.Context, *store.Repo) bool { return false }
func (stubAuthz) ViewerHasRepoPermission(context.Context, *store.Repo, store.PermScope, store.PermLevel) bool {
	return false
}
func (stubAuthz) ViewerMayActOnRepo(context.Context, *store.Repo, store.PermScope, store.PermLevel, store.PermLevel) bool {
	return false
}
func (stubAuthz) CredentialGrantsRepo(context.Context, *store.Repo, store.PermScope, store.PermLevel) bool {
	return false
}
func (stubAuthz) CredentialGrantsAccount(context.Context, store.AccountKind, string, store.PermScope, store.PermLevel) bool {
	return false
}
func (stubAuthz) PrincipalHoldsRepoCapability(context.Context, *store.Repo, store.PermLevel) bool {
	return false
}
func (stubAuthz) ViewerIsOrgMember(context.Context, string) bool            { return false }
func (stubAuthz) ViewerCanAdminAccount(context.Context, string) bool        { return false }
func (stubAuthz) ViewerMayMigrateOrg(context.Context, *store.Org) bool      { return false }
func (stubAuthz) VisibleRepos(context.Context, []*store.Repo) []*store.Repo { return nil }
func (stubAuthz) CanReadProjectV2(context.Context, *store.User, *store.ProjectV2Owner, *store.ProjectV2) bool {
	return false
}
func (stubAuthz) CanWriteProjectV2(context.Context, *store.User, *store.ProjectV2Owner) bool {
	return false
}

// stubEvents drops every webhook event.
type stubEvents struct{}

func (stubEvents) EmitWebhookEvent(string, string, string, interface{}) {}
func (stubEvents) BuildIssuesPayload(*store.Repo, *store.Issue, *store.User, string) map[string]interface{} {
	return nil
}
func (stubEvents) BuildPullRequestPayload(*store.Repo, *store.PullRequest, *store.User, string) map[string]interface{} {
	return nil
}
func (stubEvents) RepoPayload(*store.Repo) map[string]interface{}                               { return nil }
func (stubEvents) SenderPayload(*store.User) map[string]interface{}                             { return nil }
func (stubEvents) EmitIssueChanges(*store.Repo, *store.Issue, *store.User, store.SubjectChange) {}
func (stubEvents) EmitPullRequestChanges(*store.Repo, *store.PullRequest, *store.User, store.SubjectChange) {
}
func (stubEvents) EmitProjectV2Event(store.ProjectV2Event)                                 {}
func (stubEvents) EmitSponsorshipEvent(string, *store.SponsorsTransition, *store.User)     {}
func (stubEvents) EmitCheckRunEvent(string, int64, string)                                 {}
func (stubEvents) EmitCheckSuiteEvent(string, int64, string)                               {}
func (stubEvents) EmitDeploymentEvent(*store.Repo, *store.Deployment, *store.User, string) {}
func (stubEvents) EmitDeploymentStatusEvent(*store.Repo, *store.Deployment, *store.DeploymentStatus, *store.User) {
}

// stubPulls refuses every merge.
type stubPulls struct{}

func (stubPulls) PRHeadSha(*store.Repo, *store.PullRequest) string           { return "" }
func (stubPulls) MissingRequiredChecks(*store.Repo, string, string) []string { return nil }
func (stubPulls) RequiredStatusCheckContexts(*store.Repo, string) []string   { return nil }
func (stubPulls) CanMergePullRequest(context.Context, *store.Repo, *store.PullRequest) (bool, string) {
	return false, "stubPulls refuses every merge"
}
func (stubPulls) MergeQueueEligible(context.Context, *store.Repo, *store.PullRequest) (bool, string) {
	return true, ""
}
func (stubPulls) CompletePullRequestMerge(*store.Repo, *store.PullRequest, *store.User, string, string, string, string) (string, string) {
	return "", ""
}
func (stubPulls) ChangedFiles(*store.Repo, *store.PullRequest, string) ([]map[string]interface{}, error) {
	return nil, nil
}
func (stubPulls) UpdatePullRequestBranch(*store.Repo, *store.PullRequest, *store.User, string, string) error {
	return errors.New("stubPulls updates no branch")
}
func (stubPulls) MaybeAutoMerge(int)                                                 {}
func (stubPulls) MaybeAutoMergeRepo(*store.Repo)                                     {}
func (stubPulls) AutoRequestCodeOwners(*store.Repo, *store.PullRequest, *store.User) {}
func (stubPulls) MaybeAutoMergeHeadSHA(*store.Repo, string)                          {}
func (stubPulls) AdvanceMergeQueue(*store.Repo, string)                              {}

// stubMigrations queues nothing; the server package drives the real workers end to end.
type stubMigrations struct{}

func (stubMigrations) StartRepositoryMigration(int)                                {}
func (stubMigrations) StartOrganizationMigration(int)                              {}
func (stubMigrations) RepositoryMigrationLogURL(*store.RepositoryMigration) string { return "" }

// stubRepos refuses every repository move; the server package drives these against the real artifact store.
type stubRepos struct{}

func (stubRepos) RenameRepository(*store.Repo, string) error {
	return errors.New("stubRepos renames nothing")
}

func (stubRepos) CreateGitRef(context.Context, *store.Repo, *store.User, string, string) error {
	return errors.New("stubRepos writes no refs")
}

func (stubRepos) UpdateGitRef(context.Context, *store.Repo, *store.User, string, string, bool) error {
	return errors.New("stubRepos writes no refs")
}

func (stubRepos) DeleteGitRef(context.Context, *store.Repo, *store.User, string) error {
	return errors.New("stubRepos writes no refs")
}

func (stubRepos) MergeBranch(context.Context, *store.Repo, *store.User, string, string, string, string) (string, error) {
	return "", errors.New("stubRepos merges nothing")
}

func (stubRepos) CreateCommitOnBranch(context.Context, *store.Repo, *store.User, string, string, map[string][]byte, []string, string, string) (string, error) {
	return "", errors.New("stubRepos commits nothing")
}

func (stubRepos) GenerateFromTemplate(context.Context, *store.Repo, *store.User, string, string, string, bool, bool) (*store.Repo, error) {
	return nil, errors.New("stubRepos generates nothing")
}

func (stubRepos) RevertPullRequest(context.Context, *store.Repo, *store.PullRequest, *store.User, string, string, bool) (int, error) {
	return 0, errors.New("stubRepos reverts nothing")
}

func (stubRepos) ReviewPendingDeployments(context.Context, *store.Workflow, []int, string, string, *store.User) ([]string, error) {
	return nil, errors.New("stubRepos reviews nothing")
}

func newStubbedResolver() *Resolver {
	return NewResolver(Config{
		Store:           newSeededTestStore(),
		Authz:           stubAuthz{},
		Events:          stubEvents{},
		Pulls:           stubPulls{},
		Migrations:      stubMigrations{},
		Repos:           stubRepos{},
		UserFromContext: func(context.Context) *store.User { return nil },
		APIRate:         func(context.Context) RateSnapshot { return RateSnapshot{} },
	})
}
