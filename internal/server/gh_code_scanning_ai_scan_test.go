package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func aiScan(t *testing.T, resp *http.Response) string {
	t.Helper()
	value, _ := decodeBody(t, resp, http.StatusOK)["pr_scan"].(string)
	return value
}

// TestAIScanIsBoundedByTheOrganization covers the two levels and the rule that
// joins them: an organization's repositories inherit its setting, may opt out
// of it, and may never opt in past it.
func TestAIScanIsBoundedByTheOrganization(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "ai-scan-org", "AI Scan", "")
	repo := s.store.CreateOrgRepo(org, admin, "scanned", "", false)
	orgPath := "/api/v3/orgs/" + org.Login + "/code-scanning/ai-scan"
	repoPath := "/api/v3/repos/" + repo.FullName + "/code-scanning/ai-scan"
	enabled, disabled := map[string]string{"pr_scan": "enabled"}, map[string]string{"pr_scan": "disabled"}

	// Off by default at both levels, and a repository cannot opt in past that.
	if got := aiScan(t, s.get(t, orgPath, defaultToken)); got != "disabled" {
		t.Fatalf("default organization setting = %q", got)
	}
	if got := aiScan(t, s.get(t, repoPath, defaultToken)); got != "disabled" {
		t.Fatalf("default repository setting = %q", got)
	}
	expectStatus(t, s.patch(t, repoPath, defaultToken, enabled), http.StatusUnprocessableEntity,
		"enable a repository whose organization has it disabled")
	if got := aiScan(t, s.get(t, repoPath, defaultToken)); got != "disabled" {
		t.Fatalf("a refused enable changed the repository to %q", got)
	}

	// Enabling the organization enables its repositories, which may opt out.
	if got := aiScan(t, s.patch(t, orgPath, defaultToken, enabled)); got != "enabled" {
		t.Fatalf("organization after enabling = %q", got)
	}
	if got := aiScan(t, s.get(t, repoPath, defaultToken)); got != "enabled" {
		t.Fatalf("repository of an enabled organization = %q, want it inherited", got)
	}
	if got := aiScan(t, s.patch(t, repoPath, defaultToken, disabled)); got != "disabled" {
		t.Fatalf("repository after opting out = %q", got)
	}
	if got := aiScan(t, s.get(t, orgPath, defaultToken)); got != "enabled" {
		t.Fatalf("a repository opting out changed the organization to %q", got)
	}
	if got := aiScan(t, s.patch(t, repoPath, defaultToken, enabled)); got != "enabled" {
		t.Fatalf("repository after opting back in = %q", got)
	}

	// Disabling the organization overrides the repository's own choice.
	aiScan(t, s.patch(t, orgPath, defaultToken, disabled))
	if got := aiScan(t, s.get(t, repoPath, defaultToken)); got != "disabled" {
		t.Fatalf("repository of a disabled organization = %q", got)
	}
}

// TestAIScanOnAPersonalRepository covers a repository with no organization
// above it: its own choice is the whole setting.
func TestAIScanOnAPersonalRepository(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.store.CreateRepo(s.store.LookupUserByLogin("admin"), "ai-scan-personal", "", true)
	path := "/api/v3/repos/" + repo.FullName + "/code-scanning/ai-scan"
	if got := aiScan(t, s.get(t, path, defaultToken)); got != "disabled" {
		t.Fatalf("default = %q", got)
	}
	if got := aiScan(t, s.patch(t, path, defaultToken, map[string]string{"pr_scan": "enabled"})); got != "enabled" {
		t.Fatalf("after enabling = %q", got)
	}

	// The update schema admits `pr_scan` alone, and at least one member.
	for name, body := range map[string]interface{}{
		"an empty object":   map[string]string{},
		"an unknown value":  map[string]string{"pr_scan": "sometimes"},
		"a non-string":      map[string]bool{"pr_scan": true},
		"an unknown member": map[string]string{"pr_scan": "disabled", "push_scan": "enabled"},
	} {
		expectStatus(t, s.patch(t, path, defaultToken, body), http.StatusUnprocessableEntity, name)
	}
	if got := aiScan(t, s.get(t, path, defaultToken)); got != "enabled" {
		t.Fatalf("a refused update changed the setting to %q", got)
	}

	// A stranger cannot tell the private repository exists.
	_, strangerToken := s.newUser(t, "ai-scan-stranger")
	expectStatus(t, s.get(t, path, strangerToken), http.StatusNotFound, "stranger reads a private repository's setting")
	expectStatus(t, s.patch(t, path, strangerToken, map[string]string{"pr_scan": "disabled"}), http.StatusNotFound, "stranger changes it")
}

// TestOrganizationAIScanIsForOwnersAndSecurityManagers covers the standings
// GitHub names for the organization setting, in both directions.
func TestOrganizationAIScanIsForOwnersAndSecurityManagers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "ai-scan-roles", "AI Scan Roles", "")
	path := "/api/v3/orgs/" + org.Login + "/code-scanning/ai-scan"
	manager, managerToken := s.newUser(t, "ai-scan-manager")
	member, memberToken := s.newUser(t, "ai-scan-member")
	for _, user := range []*store.User{manager, member} {
		s.store.SetMembership(org.Login, user.ID, store.OrgRoleMember, store.MembershipStateActive)
	}
	s.store.AssignOrgRoleToUser(org.Login, securityManagerOrgRoleID, manager.ID)
	// GitHub: "security managers need `write:org`" on a classic token.
	s.store.Mu.Lock()
	s.store.Tokens[managerToken].Scopes = "write:org"
	s.store.Mu.Unlock()

	if got := aiScan(t, s.patch(t, path, managerToken, map[string]string{"pr_scan": "enabled"})); got != "enabled" {
		t.Fatalf("security manager's update = %q", got)
	}
	if got := aiScan(t, s.get(t, path, managerToken)); got != "enabled" {
		t.Fatalf("security manager's read = %q", got)
	}

	expectStatus(t, s.get(t, path, memberToken), http.StatusForbidden, "plain member reads the setting")
	expectStatus(t, s.patch(t, path, memberToken, map[string]string{"pr_scan": "disabled"}), http.StatusForbidden, "plain member changes the setting")
	if got := s.store.OrgCodeScanningAIScan(org.Login); got != "enabled" {
		t.Fatalf("a refused update changed the organization to %q", got)
	}
	expectStatus(t, s.get(t, "/api/v3/orgs/no-such-org/code-scanning/ai-scan", defaultToken), http.StatusNotFound, "unknown organization")
	expectStatus(t, s.get(t, path, ""), http.StatusUnauthorized, "anonymous caller reads the setting")
}
