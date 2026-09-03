package bleephub

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestEnvPolicyGetsHidePrivateRepo pins that the environment deployment-policy
// GET endpoints hide a private repository's configuration from a caller with no
// read access (404), like the sibling environment reads — they previously used
// the write-path repo lookup with no visibility check and returned 200.
func TestEnvPolicyGetsHidePrivateRepo(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	priv := s.seedRepo(t, "env-secret", true) // private, admin-owned
	env := s.store.Deployments.UpsertEnvironment(priv.ID, "production")
	s.store.CreateEnvBranchPolicy(env.ID, "main", "branch")
	_, outsiderTok := s.newUser(t, "env-outsider")

	base := "/api/v3/repos/admin/env-secret/environments/production"
	for _, path := range []string{
		"/deployment-branch-policies",
		"/deployment_protection_rules",
		"/deployment_protection_rules/apps",
	} {
		resp := s.get(t, base+path, outsiderTok)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("outsider GET %s on a private repo = %d, want 404 (config leak)", path, resp.StatusCode)
		}
	}

	// The owner still reads its own environment config.
	resp := s.get(t, base+"/deployment-branch-policies", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner GET deployment-branch-policies = %d, want 200", resp.StatusCode)
	}
}

// TestRunnerJSONReportsEphemeral pins that a runner's ephemeral flag is rendered
// from its real state, not hardcoded false, so a JIT / --ephemeral runner is
// reported as ephemeral.
func TestRunnerJSONReportsEphemeral(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "eph-runner", false)
	_, agent := testAgentSession(t, s.Server, store.RunnerScope{Repo: repo.FullName})
	s.store.Mu.Lock()
	s.store.Agents[agent.ID].Ephemeral = true
	s.store.Mu.Unlock()

	resp := s.get(t, fmt.Sprintf("/api/v3/repos/admin/eph-runner/actions/runners/%d", agent.ID), defaultToken)
	runner := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get runner = %d, want 200", resp.StatusCode)
	}
	if runner["ephemeral"] != true {
		t.Fatalf("ephemeral = %v, want true", runner["ephemeral"])
	}
}
