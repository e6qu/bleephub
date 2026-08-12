package graphqlapi

import "context"

// Minimal no-op seam stubs for tests that construct a Resolver directly
// (NewResolver panics on nil Authz/Events/Pulls — ARCH-005). They deny
// every authorization question, drop every event, and refuse every merge:
// resolver-package tests exercise schema assembly, converters, and
// pagination, never seam behavior, which stays covered by the server
// package's end-to-end tests.

// stubAuthz denies everything.
type stubAuthz struct{}

func (stubAuthz) ViewerCanReadRepo(context.Context, *Repo) bool  { return false }
func (stubAuthz) ViewerCanPushRepo(context.Context, *Repo) bool  { return false }
func (stubAuthz) ViewerCanAdminRepo(context.Context, *Repo) bool { return false }
func (stubAuthz) ViewerHasRepoPermission(context.Context, *Repo, permScope, permLevel) bool {
	return false
}
func (stubAuthz) ViewerMayActOnRepo(context.Context, *Repo, permScope, permLevel, permLevel) bool {
	return false
}
func (stubAuthz) CredentialGrantsRepo(context.Context, *Repo, permScope, permLevel) bool {
	return false
}
func (stubAuthz) CredentialGrantsAccount(context.Context, accountKind, string, permScope, permLevel) bool {
	return false
}
func (stubAuthz) PrincipalHoldsRepoCapability(context.Context, *Repo, permLevel) bool { return false }
func (stubAuthz) ViewerIsOrgMember(context.Context, string) bool                      { return false }
func (stubAuthz) VisibleRepos(context.Context, []*Repo) []*Repo                       { return nil }
func (stubAuthz) CanReadProjectV2(context.Context, *User, *projectV2Owner, *ProjectV2) bool {
	return false
}
func (stubAuthz) CanWriteProjectV2(context.Context, *User, *projectV2Owner) bool { return false }

// stubEvents drops every webhook event.
type stubEvents struct{}

func (stubEvents) EmitWebhookEvent(string, string, string, interface{}) {}
func (stubEvents) BuildIssuesPayload(*Repo, *Issue, *User, string) map[string]interface{} {
	return nil
}
func (stubEvents) BuildPullRequestPayload(*Repo, *PullRequest, *User, string) map[string]interface{} {
	return nil
}
func (stubEvents) RepoPayload(*Repo) map[string]interface{} { return nil }

// stubPulls refuses every merge.
type stubPulls struct{}

func (stubPulls) PRHeadSha(*Repo, *PullRequest) string                 { return "" }
func (stubPulls) MissingRequiredChecks(*Repo, string, string) []string { return nil }
func (stubPulls) CanMergePullRequest(context.Context, *Repo, *PullRequest) (bool, string) {
	return false, "stubPulls refuses every merge"
}
func (stubPulls) CompletePullRequestMerge(*Repo, *PullRequest, *User, string, string, string) (string, string) {
	return "", ""
}
func (stubPulls) BranchProtectionRuleForPR(*Repo, string) map[string]interface{} { return nil }
func (stubPulls) ChangedFiles(*Repo, *PullRequest, string) ([]map[string]interface{}, error) {
	return nil, nil
}

// newStubbedResolver is the test-package analogue of the server's
// newGraphQLResolver: a resolver over a seeded store with the no-op seams.
func newStubbedResolver() *Resolver {
	return NewResolver(Config{
		Store:  newSeededTestStore(),
		Authz:  stubAuthz{},
		Events: stubEvents{},
		Pulls:  stubPulls{},
	})
}
