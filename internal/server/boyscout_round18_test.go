package bleephub

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestTeamCreateRequiresRepoAdmin pins the fix for a CRITICAL privilege
// escalation: any org member could seed a private repo they cannot administer
// into a new team and gain access via the team's default permission. Adding a
// repo to a team now requires admin on that repo, and the team default
// permission "admin" (which conferred repo-admin) is rejected.
func TestTeamCreateRequiresRepoAdmin(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "acme")
	s.seedOrgRepo(t, org, "secret", true) // private
	stranger, strangerTok := s.newUser(t, "acme-outsider")
	s.store.SetMembership("acme", stranger.ID, store.OrgRoleMember, store.MembershipStateActive)

	// A plain member with no admin on acme/secret cannot link it into a team.
	resp := s.post(t, "/api/v3/orgs/acme/teams", strangerTok, map[string]interface{}{
		"name": "pwn", "permission": "push", "repo_names": []string{"acme/secret"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member linking a repo they don't admin = %d, want 403", resp.StatusCode)
	}

	// An org owner (admin, who admins every org repo) may.
	resp = s.post(t, "/api/v3/orgs/acme/teams", defaultToken, map[string]interface{}{
		"name": "devs", "permission": "push", "repo_names": []string{"acme/secret"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("owner creating a team with a repo they admin = %d, want 201", resp.StatusCode)
	}

	// The team default permission "admin" is not a valid team-level value.
	resp = s.post(t, "/api/v3/orgs/acme/teams", defaultToken, map[string]interface{}{
		"name": "admins", "permission": "admin",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("team default permission 'admin' = %d, want 422", resp.StatusCode)
	}
}

// TestLastOrgOwnerGuard pins that a non-owner member can be removed, but the
// sole owner cannot be removed or demoted (either would orphan the org).
func TestLastOrgOwnerGuard(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedTestOrg(t, "guarded") // admin is the sole owner
	member, _ := s.newUser(t, "guard-member")
	s.store.SetMembership("guarded", member.ID, store.OrgRoleMember, store.MembershipStateActive)

	resp := s.delete(t, "/api/v3/orgs/guarded/members/guard-member", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("removing a non-owner member = %d, want 204", resp.StatusCode)
	}

	resp = s.delete(t, "/api/v3/orgs/guarded/members/admin", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("removing the last owner = %d, want 403", resp.StatusCode)
	}

	resp = s.put(t, "/api/v3/orgs/guarded/memberships/admin", defaultToken, map[string]interface{}{"role": "member"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demoting the last owner = %d, want 403", resp.StatusCode)
	}
}

// TestOIDCTokenRequiresIdTokenWrite pins that minting an OIDC token requires the
// workflow to carry `permissions: id-token: write`; a job token without it is
// refused, one with it succeeds.
func TestOIDCTokenRequiresIdTokenWrite(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "oidc-repo", false)
	repo := "admin/oidc-repo"
	query := "repo=" + repo +
		"&ref=refs/heads/main&sha=0123456789abcdef0123456789abcdef01234567" +
		"&run_id=42&run_number=7&run_attempt=1&workflow=CI&workflow_file=ci.yml&event_name=push"

	mint := func(token string) int {
		req := httptest.NewRequest("GET", "/token?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w.Code
	}

	// testJobToken registers the job and grants id-token: write → mint succeeds.
	withPerm, scopeID := testJobToken(t, s.Server, repo)
	if code := mint(withPerm); code != http.StatusOK {
		t.Fatalf("mint with id-token:write = %d, want 200", code)
	}

	// A token for the same job but without id-token:write is refused.
	bare := makeJWT(scopeID, runnerAudJob)
	if code := mint(bare); code == http.StatusOK {
		t.Fatalf("mint without id-token:write succeeded (want a non-200 refusal)")
	}
}
