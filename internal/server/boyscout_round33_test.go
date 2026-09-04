package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestSecretTeamHiddenFromNonMembers pins that a secret team is invisible on the
// team list/get surfaces to an org member who is neither an owner nor a member of
// that team, while an owner and the team's own members still see it. The list/get
// handlers previously returned every team including secret ones to any org member.
func TestSecretTeamHiddenFromNonMembers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createOrg(t, "acme")
	// A regular (non-owner) org member.
	alice, aliceTok := s.newUser(t, "alice")
	s.store.SetMembership("acme", alice.ID, store.OrgRoleMember, store.MembershipStateActive)

	closed := s.store.CreateTeam("acme", "public-team", store.TeamOptions{Privacy: store.TeamPrivacyClosed})
	secret := s.store.CreateTeam("acme", "secret-ops", store.TeamOptions{Privacy: store.TeamPrivacySecret})
	if closed == nil || secret == nil {
		t.Fatal("team creation failed")
	}

	listSlugs := func(tok string) map[string]bool {
		out := map[string]bool{}
		for _, tm := range decodeJSONArray(t, s.get(t, "/api/v3/orgs/acme/teams", tok)) {
			out[tm["slug"].(string)] = true
		}
		return out
	}

	// Alice (non-member of secret-ops) sees the closed team but not the secret one.
	seen := listSlugs(aliceTok)
	if !seen["public-team"] {
		t.Fatal("closed team should be visible to an org member")
	}
	if seen["secret-ops"] {
		t.Fatal("secret team leaked to a non-member org member in the list")
	}
	resp := s.get(t, "/api/v3/orgs/acme/teams/secret-ops", aliceTok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-member GET secret team = %d, want 404", resp.StatusCode)
	}

	// The org owner (admin, the org creator) sees the secret team.
	if !listSlugs(defaultToken)["secret-ops"] {
		t.Fatal("org owner should see the secret team")
	}
	resp = s.get(t, "/api/v3/orgs/acme/teams/secret-ops", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("org owner GET secret team = %d, want 200", resp.StatusCode)
	}

	// Once alice is a member of the secret team, she sees it too.
	if !s.store.SetTeamMembership("acme", "secret-ops", alice.ID, store.TeamRoleMember) {
		t.Fatal("add alice to secret team failed")
	}
	if !listSlugs(aliceTok)["secret-ops"] {
		t.Fatal("a member of the secret team must see it")
	}
	resp = s.get(t, "/api/v3/orgs/acme/teams/secret-ops", aliceTok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member GET secret team = %d, want 200", resp.StatusCode)
	}
}
