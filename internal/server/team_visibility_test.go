package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestTeamVisibilityNonMember: an org's teams (and their members/repo grants)
// 404 for a non-member caller, as on real GitHub, but a member sees them.
func TestTeamVisibilityNonMember(t *testing.T) {
	s := newTestServer()
	s.registerGHTeamRoutes()
	s.registerGHMemberRoutes()

	admin := s.store.LookupUserByLogin("admin")
	if s.store.CreateOrg(admin, "team-org", "Team Org", "") == nil {
		t.Fatal("CreateOrg nil")
	}
	team := s.store.CreateTeam("team-org", "Secret Squad", store.TeamOptions{})
	if team == nil {
		t.Fatal("CreateTeam nil")
	}

	outsider := seedTestUser(s, "team-outsider")
	outTok := s.store.CreateToken(outsider.ID, "read:org")

	gated := []string{
		"/api/v3/orgs/team-org/teams",
		"/api/v3/orgs/team-org/teams/secret-squad",
		"/api/v3/orgs/team-org/teams/secret-squad/members",
		"/api/v3/orgs/team-org/teams/secret-squad/repos",
	}
	for _, p := range gated {
		w := tokenRequest(s, "GET", p, outTok.Value)
		if w.Code != http.StatusNotFound {
			t.Errorf("outsider GET %s = %d, want 404 (body=%s)", p, w.Code, w.Body.String())
		}
		// Member (admin) is allowed.
		wa := tokenRequest(s, "GET", p, store.AdminToken())
		if wa.Code == http.StatusNotFound {
			t.Errorf("member GET %s = 404, want visible", p)
		}
	}

	w := tokenRequest(s, "GET", "/api/v3/orgs/team-org/teams", store.AdminToken())
	var teams []map[string]any
	json.Unmarshal(w.Body.Bytes(), &teams)
	if len(teams) == 0 {
		t.Error("member team list empty, want the seeded team")
	}
}
