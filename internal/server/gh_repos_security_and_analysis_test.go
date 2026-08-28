package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func saStatus(t *testing.T, repoJSON map[string]interface{}, key string) string {
	t.Helper()
	sa, ok := repoJSON["security_and_analysis"].(map[string]interface{})
	if !ok {
		t.Fatalf("security_and_analysis missing or not an object: %v", repoJSON["security_and_analysis"])
	}
	obj, ok := sa[key].(map[string]interface{})
	if !ok {
		t.Fatalf("security_and_analysis.%s missing: %v", key, sa)
	}
	status, _ := obj["status"].(string)
	return status
}

// TestRepoSecurityAndAnalysis_DefaultsAndPatch verifies the repo payload's
// security_and_analysis block: every toggle defaults to disabled, PATCH
// /repos/{owner}/{repo} flips each documented toggle, GET reflects the new
// state, and dependabot_security_updates mirrors the automated-security-fixes
// endpoint rather than the PATCH body.
func TestRepoSecurityAndAnalysis_DefaultsAndPatch(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoPath := "/api/v3/repos/admin/sec-analysis"
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "sec-analysis", "private": true,
	}).Body.Close()

	repoJSON := decodeJSON(t, s.get(t, repoPath, defaultToken))
	for _, key := range []string{
		"advanced_security",
		"dependabot_security_updates",
		"secret_scanning",
		"secret_scanning_push_protection",
		"secret_scanning_non_provider_patterns",
	} {
		if got := saStatus(t, repoJSON, key); got != "disabled" {
			t.Errorf("default %s = %q, want disabled", key, got)
		}
	}

	// Flip each toggle on with its own PATCH, so a partial-object update is
	// exercised and the other toggles must stay untouched.
	patchToggles := []string{
		"advanced_security",
		"secret_scanning",
		"secret_scanning_push_protection",
		"secret_scanning_non_provider_patterns",
	}
	for i, key := range patchToggles {
		resp := s.patch(t, repoPath, defaultToken, map[string]interface{}{
			"security_and_analysis": map[string]interface{}{
				key: map[string]interface{}{"status": "enabled"},
			},
		})
		patched := decodeJSON(t, resp)
		if got := saStatus(t, patched, key); got != "enabled" {
			t.Fatalf("PATCH response %s = %q, want enabled", key, got)
		}
		repoJSON = decodeJSON(t, s.get(t, repoPath, defaultToken))
		for j, other := range patchToggles {
			want := "disabled"
			if j <= i {
				want = "enabled"
			}
			if got := saStatus(t, repoJSON, other); got != want {
				t.Errorf("after enabling %s: %s = %q, want %q", key, other, got, want)
			}
		}
	}

	s.patch(t, repoPath, defaultToken, map[string]interface{}{
		"security_and_analysis": map[string]interface{}{
			"secret_scanning": map[string]interface{}{"status": "disabled"},
		},
	}).Body.Close()
	repoJSON = decodeJSON(t, s.get(t, repoPath, defaultToken))
	if got := saStatus(t, repoJSON, "secret_scanning"); got != "disabled" {
		t.Errorf("secret_scanning after disable = %q, want disabled", got)
	}
	if got := saStatus(t, repoJSON, "secret_scanning_push_protection"); got != "enabled" {
		t.Errorf("secret_scanning_push_protection should stay enabled, got %q", got)
	}

	// dependabot_security_updates is not a PATCH toggle: it mirrors the
	// automated-security-fixes endpoint, exactly as on real GitHub.
	s.put(t, repoPath+"/automated-security-fixes", defaultToken, nil).Body.Close()
	repoJSON = decodeJSON(t, s.get(t, repoPath, defaultToken))
	if got := saStatus(t, repoJSON, "dependabot_security_updates"); got != "enabled" {
		t.Errorf("dependabot_security_updates after PUT automated-security-fixes = %q, want enabled", got)
	}
	s.delete(t, repoPath+"/automated-security-fixes", defaultToken).Body.Close()
	repoJSON = decodeJSON(t, s.get(t, repoPath, defaultToken))
	if got := saStatus(t, repoJSON, "dependabot_security_updates"); got != "disabled" {
		t.Errorf("dependabot_security_updates after DELETE automated-security-fixes = %q, want disabled", got)
	}
}

// TestRepoSecurityAndAnalysis_NonAdminRefused verifies a viewer without admin
// rights cannot flip security_and_analysis via PATCH (the whole repo PATCH is
// admin-gated, like has_wiki and the other settings).
func TestRepoSecurityAndAnalysis_NonAdminRefused(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoPath := "/api/v3/repos/admin/sec-analysis-authz"
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "sec-analysis-authz",
	}).Body.Close()

	s.store.Mu.Lock()
	other := &store.User{ID: s.store.NextUser, Login: "sec-analysis-other", Type: "User"}
	s.store.NextUser++
	s.store.Users[other.ID] = other
	s.store.UsersByLogin[other.Login] = other
	otherTok := &store.Token{Value: "ghp_secanalysisother0000000000000000000000", UserID: other.ID, Scopes: "repo"}
	s.store.Tokens[otherTok.Value] = otherTok
	s.store.Mu.Unlock()

	resp := s.patch(t, repoPath, otherTok.Value, map[string]interface{}{
		"security_and_analysis": map[string]interface{}{
			"secret_scanning": map[string]interface{}{"status": "enabled"},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("non-admin PATCH: expected 403, got %d", resp.StatusCode)
	}

	repoJSON := decodeJSON(t, s.get(t, repoPath, defaultToken))
	if got := saStatus(t, repoJSON, "secret_scanning"); got != "disabled" {
		t.Errorf("secret_scanning after refused PATCH = %q, want disabled", got)
	}
}
