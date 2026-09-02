package bleephub

import (
	"testing"
)

// TestOIDCCustomSubjectTemplate pins that a configured include_claim_keys
// customization actually rewrites the OIDC token's subject (repo scope), which
// was previously stored but silently ignored.
func TestOIDCCustomSubjectTemplate(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "oidc-custom", false)
	repo := "admin/oidc-custom"

	s.store.Misc.Mu.Lock()
	if s.store.Misc.OidcClaimKeys == nil {
		s.store.Misc.OidcClaimKeys = map[string][]string{}
	}
	s.store.Misc.OidcClaimKeys["repo:"+repo] = []string{"repository_owner", "ref"}
	s.store.Misc.Mu.Unlock()

	claims := decodeOIDCToken(t, s.Server, repo, fullOIDCQuery(repo))
	if got := claims["sub"]; got != "repository_owner:admin:ref:refs/heads/main" {
		t.Fatalf("custom sub = %v, want repository_owner:admin:ref:refs/heads/main", got)
	}
}
