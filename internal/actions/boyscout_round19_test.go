package actions

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestEnvironmentAllowsRefUnderBranchPolicy pins that a deployment branch policy
// gates which refs may deploy to an environment: with custom_branch_policies,
// only refs matching a configured pattern are allowed; with protected_branches,
// only a protected branch; a nil policy allows any ref.
func TestEnvironmentAllowsRefUnderBranchPolicy(t *testing.T) {
	e := newTestEngine()
	st := e.store
	owner := &store.User{ID: st.NextUser, Login: "deployer", Type: "User"}
	st.NextUser++
	st.Users[owner.ID] = owner
	st.UsersByLogin[owner.Login] = owner
	repo := st.CreateRepo(owner, "deployable", "", false)

	env := st.Deployments.UpsertEnvironment(repo.ID, "production")

	allows := func(ref string) bool {
		st.Mu.Lock()
		defer st.Mu.Unlock()
		return e.environmentAllowsRefLocked(repo.ID, &store.Workflow{RepoFullName: repo.FullName, Ref: ref}, st.Deployments.GetEnvironment(repo.ID, "production"))
	}

	// No policy: any ref may deploy.
	if !allows("refs/heads/feature") {
		t.Fatal("a nil branch policy should allow any ref")
	}

	// custom_branch_policies: only release/* branches.
	st.Deployments.SetEnvironmentBranchPolicyConfig(repo.ID, "production", &store.DeploymentBranchPolicy{CustomBranchPolicies: true})
	if created, _ := st.CreateEnvBranchPolicy(env.ID, "release/*", "branch"); created == nil {
		t.Fatal("could not seed a custom branch policy")
	}
	if !allows("refs/heads/release/1") {
		t.Fatal("release/1 should be allowed by the custom policy")
	}
	if allows("refs/heads/feature") {
		t.Fatal("feature must be blocked by the custom policy")
	}
	if allows("refs/tags/v1") {
		t.Fatal("a tag must be blocked when only a branch pattern is configured")
	}

	// protected_branches: only a branch with protection.
	st.Deployments.SetEnvironmentBranchPolicyConfig(repo.ID, "production", &store.DeploymentBranchPolicy{ProtectedBranches: true})
	if allows("refs/heads/main") {
		t.Fatal("main is not protected, so protected_branches must block it")
	}
	st.SetBranchProtection(repo.ID, "main", &store.BranchProtection{Enabled: true})
	if !allows("refs/heads/main") {
		t.Fatal("a protected main should be allowed under protected_branches")
	}
}
