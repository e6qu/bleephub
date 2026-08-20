package graphqlapi

import (
	"context"

	"github.com/e6qu/bleephub/internal/store"
)

// Minimal no-op seam stubs for tests that construct a Resolver directly
// (NewResolver panics on nil Authz/Events/Pulls — ARCH-005). They deny
// every authorization question, drop every event, and refuse every merge:
// resolver-package tests exercise schema assembly, converters, and
// pagination, never seam behavior, which stays covered by the server
// package's end-to-end tests.

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
func (stubEvents) RepoPayload(*store.Repo) map[string]interface{} { return nil }

// stubPulls refuses every merge.
type stubPulls struct{}

func (stubPulls) PRHeadSha(*store.Repo, *store.PullRequest) string           { return "" }
func (stubPulls) MissingRequiredChecks(*store.Repo, string, string) []string { return nil }
func (stubPulls) CanMergePullRequest(context.Context, *store.Repo, *store.PullRequest) (bool, string) {
	return false, "stubPulls refuses every merge"
}
func (stubPulls) CompletePullRequestMerge(*store.Repo, *store.PullRequest, *store.User, string, string, string, string) (string, string) {
	return "", ""
}
func (stubPulls) BranchProtectionRuleForPR(*store.Repo, string) map[string]interface{} { return nil }
func (stubPulls) ChangedFiles(*store.Repo, *store.PullRequest, string) ([]map[string]interface{}, error) {
	return nil, nil
}
func (stubPulls) MaybeAutoMerge(int) {}

// newStubbedResolver is the test-package analogue of the server's
// newGraphQLResolver: a resolver over a seeded store with the no-op seams.
func newStubbedResolver() *Resolver {
	return NewResolver(Config{
		Store:           newSeededTestStore(),
		Authz:           stubAuthz{},
		Events:          stubEvents{},
		Pulls:           stubPulls{},
		UserFromContext: func(context.Context) *store.User { return nil },
		APIRate:         func(context.Context) RateSnapshot { return RateSnapshot{} },
	})
}
