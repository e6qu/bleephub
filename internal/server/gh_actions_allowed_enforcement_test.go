package bleephub

import (
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

const allowedActionsWorkflowYAML = `name: uses-actions
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: thirdparty/deploy-tool/sub/path@v2
      - uses: ./.github/actions/local
      - run: echo hi
`

// TestAllowedActionsGovernWhatAWorkflowMayUse covers the Actions permission
// that says which actions a workflow may use, in both directions and at each
// setting. It was stored and returned by the API and consulted nowhere, so any
// action ran whatever it said. A refused run is a startup failure, as on
// GitHub, and says which action was refused.
func TestAllowedActionsGovernWhatAWorkflowMayUse(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	const repoKey = "allowedowner/allowed-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/uses.yml", allowedActionsWorkflowYAML)

	// latest returns the newest run of the repository.
	latest := func() *store.Workflow {
		t.Helper()
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		var newest *store.Workflow
		for _, run := range s.store.Workflows {
			if run.RepoFullName == repoKey && (newest == nil || run.RunID > newest.RunID) {
				newest = run
			}
		}
		if newest == nil {
			t.Fatal("no run was created")
		}
		return newest
	}
	set := func(allowed string, chosen *store.ActionsAllowed) {
		s.store.SetRepoActionsPermissions(repoKey, &store.RepoActionsPermissions{Enabled: true, AllowedActions: allowed, ActionsAllowed: chosen})
	}
	push := func() *store.Workflow {
		s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)
		return latest()
	}

	for _, tc := range []struct {
		name    string
		allowed string
		chosen  *store.ActionsAllowed
		refused string // a fragment of the refused action's name, or "" when the run must start
	}{
		{"all", "all", nil, ""},
		{"local only", "local_only", nil, "actions/checkout"},
		{"selected, nothing chosen", "selected", &store.ActionsAllowed{}, "actions/checkout"},
		{"selected, GitHub's own", "selected", &store.ActionsAllowed{GithubOwnedAllowed: true}, "thirdparty/deploy-tool"},
		{"selected, GitHub's own and a pattern for another owner", "selected",
			&store.ActionsAllowed{GithubOwnedAllowed: true, PatternsAllowed: []string{"someone-else/*"}}, "thirdparty/deploy-tool"},
		{"selected, a pinned ref that is not the one used", "selected",
			&store.ActionsAllowed{GithubOwnedAllowed: true, PatternsAllowed: []string{"thirdparty/deploy-tool/sub/path@v1"}}, "thirdparty/deploy-tool"},
		{"selected, GitHub's own and the owner's wildcard", "selected",
			&store.ActionsAllowed{GithubOwnedAllowed: true, PatternsAllowed: []string{"thirdparty/*"}}, ""},
		{"selected, patterns for both", "selected",
			&store.ActionsAllowed{PatternsAllowed: []string{"actions/checkout@*", "thirdparty/deploy-tool/sub/path@v2"}}, ""},
	} {
		set(tc.allowed, tc.chosen)
		run := push()
		failed := run.Result == store.ResultStartupFailure
		if failed != (tc.refused != "") {
			t.Errorf("%s: startup failure = %v, want %v", tc.name, failed, tc.refused != "")
			continue
		}
		if tc.refused != "" && !strings.Contains(run.StartupError, tc.refused) {
			t.Errorf("%s: the run's failure %q does not name %s", tc.name, run.StartupError, tc.refused)
		}
	}
}

func TestActionUseRefusalAcrossLevels(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "allowed-levels", "Allowed Levels", "")
	repo := s.store.CreateOrgRepo(org, admin, "inside", "", false)
	const third = "thirdparty/tool@v1"

	if got := s.store.ActionUseRefusal(repo, third); got != "" {
		t.Fatalf("with nothing configured, %s was refused: %s", third, got)
	}
	// The organization restricts; the repository, left at `all`, cannot widen it.
	orgPolicy := s.store.GetOrgActionsPermissions(org.Login)
	orgPolicy.AllowedActions = "local_only"
	s.store.SetOrgActionsPermissions(org.Login, orgPolicy)
	s.store.SetRepoActionsPermissions(repo.FullName, &store.RepoActionsPermissions{Enabled: true, AllowedActions: "all"})
	if got := s.store.ActionUseRefusal(repo, third); !strings.Contains(got, "organization") {
		t.Errorf("an organization's local_only was widened by its repository: %q", got)
	}
	// The organization's own actions, a path in the repository and an image pass.
	for _, uses := range []string{"allowed-levels/shared-actions/build@main", "Allowed-Levels/x@v1", "./local", "docker://alpine:3"} {
		if got := s.store.ActionUseRefusal(repo, uses); got != "" {
			t.Errorf("%s was refused under local_only: %s", uses, got)
		}
	}
	// A verified creator is an organization with a verified domain.
	creator := s.store.CreateOrg(admin, "thirdparty", "Third Party", "")
	orgPolicy.AllowedActions = "selected"
	orgPolicy.ActionsAllowed = &store.ActionsAllowed{VerifiedAllowed: true}
	s.store.SetOrgActionsPermissions(org.Login, orgPolicy)
	if got := s.store.ActionUseRefusal(repo, third); got == "" {
		t.Error("an unverified creator's action was admitted as verified")
	}
	domain, err := s.store.CreateVerifiableDomain(store.VerifiableDomainOwnerOrganization, creator.ID, "thirdparty.example")
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if _, err := s.store.VerifyVerifiableDomain(domain.ID); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if got := s.store.ActionUseRefusal(repo, third); got != "" {
		t.Errorf("a verified creator's action was refused: %s", got)
	}
}
