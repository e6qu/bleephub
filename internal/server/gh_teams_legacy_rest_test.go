package bleephub

import (
	"net/http"
	"testing"
)

// createLegacyTestTeam creates an org + team through the slug surface
// and returns the team's numeric ID for the legacy endpoints.
func createLegacyTestTeam(t *testing.T, s *isolatedServer, orgLogin, teamName string) int {
	t.Helper()
	s.createOrg(t, orgLogin)
	resp := s.post(t, "/api/v3/orgs/"+orgLogin+"/teams", defaultToken, map[string]interface{}{"name": teamName})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create team %s: %d", teamName, resp.StatusCode)
	}
	team := decodeJSON(t, resp)
	return int(team["id"].(float64))
}

func TestLegacyTeamGetUpdateDelete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	teamID := createLegacyTestTeam(t, s, "legacy-crud-org", "Legacy Team")

	got := s.get(t, "/api/v3/teams/"+itoa(teamID), defaultToken)
	if got.StatusCode != http.StatusOK {
		got.Body.Close()
		t.Fatalf("GET legacy team: %d", got.StatusCode)
	}
	full := decodeJSON(t, got)
	if full["slug"] != "legacy-team" || full["name"] != "Legacy Team" {
		t.Fatalf("legacy team = %v", full)
	}
	// team-full carries the embedded organization and counters.
	if orgObj, ok := full["organization"].(map[string]interface{}); !ok || orgObj["login"] != "legacy-crud-org" {
		t.Fatalf("legacy team organization = %v", full["organization"])
	}
	if full["members_count"] != float64(1) {
		t.Fatalf("members_count = %v, want 1 (creator auto-maintainer)", full["members_count"])
	}

	// A rename through the legacy surface re-keys the slug surface too —
	// both address the same store entity.
	patched := s.patch(t, "/api/v3/teams/"+itoa(teamID), defaultToken,
		map[string]interface{}{"name": "Legacy Squad", "description": "renamed", "privacy": "secret"})
	if patched.StatusCode != http.StatusOK {
		patched.Body.Close()
		t.Fatalf("PATCH legacy team: %d", patched.StatusCode)
	}
	renamed := decodeJSON(t, patched)
	if renamed["slug"] != "legacy-squad" || renamed["description"] != "renamed" || renamed["privacy"] != "secret" {
		t.Fatalf("renamed team = %v", renamed)
	}
	expectStatus(t, s.get(t, "/api/v3/orgs/legacy-crud-org/teams/legacy-squad", defaultToken),
		http.StatusOK, "slug surface after legacy rename")
	expectStatus(t, s.patch(t, "/api/v3/teams/"+itoa(teamID), defaultToken,
		map[string]interface{}{"privacy": "invisible"}), http.StatusUnprocessableEntity, "bad privacy enum")

	// Child teams via the legacy list.
	childResp := s.post(t, "/api/v3/orgs/legacy-crud-org/teams", defaultToken,
		map[string]interface{}{"name": "Legacy Child", "parent_team_id": teamID})
	if childResp.StatusCode != http.StatusCreated {
		childResp.Body.Close()
		t.Fatalf("create child team: %d", childResp.StatusCode)
	}
	child := decodeJSON(t, childResp)
	childID := int(child["id"].(float64))
	children := decodeJSONArray(t, s.get(t, "/api/v3/teams/"+itoa(teamID)+"/teams", defaultToken))
	if len(children) != 1 || children[0]["slug"] != "legacy-child" {
		t.Fatalf("legacy child teams = %v", children)
	}

	expectStatus(t, s.delete(t, "/api/v3/teams/"+itoa(childID), defaultToken), http.StatusNoContent, "delete child team")
	expectStatus(t, s.get(t, "/api/v3/teams/"+itoa(childID), defaultToken), http.StatusNotFound, "get deleted team")
	expectStatus(t, s.get(t, "/api/v3/teams/999999", defaultToken), http.StatusNotFound, "unknown team id")
	expectStatus(t, s.get(t, "/api/v3/teams/not-a-number", defaultToken), http.StatusNotFound, "non-numeric team id")

	// The org's teams are invisible to non-members.
	_, outsiderToken := s.newUser(t, "legacy-outsider")
	expectStatus(t, s.get(t, "/api/v3/teams/"+itoa(teamID), outsiderToken), http.StatusNotFound, "outsider reads team")
	expectStatus(t, s.delete(t, "/api/v3/teams/"+itoa(teamID), outsiderToken), http.StatusForbidden, "outsider deletes team")
}

func TestLegacyTeamMembersAndMemberships(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	teamID := createLegacyTestTeam(t, s, "legacy-mem-org", "Mem Team")

	_, memberToken := s.newUser(t, "legacy-member")
	s.activateOrgMember(t, "legacy-mem-org", "legacy-member", memberToken)
	s.newUser(t, "legacy-nonmember")

	// Legacy add-member requires existing org membership.
	expectStatus(t, s.put(t, "/api/v3/teams/"+itoa(teamID)+"/members/legacy-member", defaultToken, nil),
		http.StatusNoContent, "legacy add member")
	expectStatus(t, s.put(t, "/api/v3/teams/"+itoa(teamID)+"/members/legacy-nonmember", defaultToken, nil),
		http.StatusUnprocessableEntity, "legacy add non-org-member")
	expectStatus(t, s.put(t, "/api/v3/teams/"+itoa(teamID)+"/members/nobody-here", defaultToken, nil),
		http.StatusNotFound, "legacy add unknown user")

	expectStatus(t, s.get(t, "/api/v3/teams/"+itoa(teamID)+"/members/legacy-member", defaultToken),
		http.StatusNoContent, "legacy member check")
	members := decodeJSONArray(t, s.get(t, "/api/v3/teams/"+itoa(teamID)+"/members", defaultToken))
	logins := map[string]bool{}
	for _, m := range members {
		logins[m["login"].(string)] = true
	}
	if !logins["legacy-member"] {
		t.Fatalf("legacy members list = %v", members)
	}

	// Membership read: active org member reads active.
	ms := s.get(t, "/api/v3/teams/"+itoa(teamID)+"/memberships/legacy-member", defaultToken)
	if ms.StatusCode != http.StatusOK {
		ms.Body.Close()
		t.Fatalf("legacy get membership: %d", ms.StatusCode)
	}
	membership := decodeJSON(t, ms)
	if membership["role"] != "member" || membership["state"] != "active" {
		t.Fatalf("legacy membership = %v", membership)
	}
	if _, hasUser := membership["user"]; hasUser {
		t.Fatalf("legacy team-membership carries undocumented user member: %v", membership)
	}

	// Membership PUT invites a non-org-member (pending) and can promote
	// to maintainer.
	put := s.put(t, "/api/v3/teams/"+itoa(teamID)+"/memberships/legacy-nonmember", defaultToken, nil)
	if put.StatusCode != http.StatusOK {
		put.Body.Close()
		t.Fatalf("legacy put membership: %d", put.StatusCode)
	}
	if invited := decodeJSON(t, put); invited["state"] != "pending" {
		t.Fatalf("invited membership = %v", invited)
	}
	promote := s.put(t, "/api/v3/teams/"+itoa(teamID)+"/memberships/legacy-member", defaultToken,
		map[string]interface{}{"role": "maintainer"})
	if promote.StatusCode != http.StatusOK {
		promote.Body.Close()
		t.Fatalf("legacy promote: %d", promote.StatusCode)
	}
	if promoted := decodeJSON(t, promote); promoted["role"] != "maintainer" {
		t.Fatalf("promoted membership = %v", promoted)
	}
	expectStatus(t, s.put(t, "/api/v3/teams/"+itoa(teamID)+"/memberships/legacy-member", defaultToken,
		map[string]interface{}{"role": "overlord"}), http.StatusUnprocessableEntity, "bad membership role")

	// Removal via both endpoint families.
	expectStatus(t, s.delete(t, "/api/v3/teams/"+itoa(teamID)+"/memberships/legacy-nonmember", defaultToken),
		http.StatusNoContent, "legacy delete membership")
	expectStatus(t, s.get(t, "/api/v3/teams/"+itoa(teamID)+"/memberships/legacy-nonmember", defaultToken),
		http.StatusNotFound, "membership after delete")
	expectStatus(t, s.delete(t, "/api/v3/teams/"+itoa(teamID)+"/members/legacy-member", defaultToken),
		http.StatusNoContent, "legacy delete member")
	expectStatus(t, s.get(t, "/api/v3/teams/"+itoa(teamID)+"/members/legacy-member", defaultToken),
		http.StatusNotFound, "member check after delete")
}

func TestLegacyTeamRepos(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	teamID := createLegacyTestTeam(t, s, "legacy-repo-org", "Repo Team")
	expectStatus(t, s.post(t, "/api/v3/orgs/legacy-repo-org/repos", defaultToken,
		map[string]interface{}{"name": "legacy-repo"}), http.StatusCreated, "create org repo")

	expectStatus(t, s.put(t, "/api/v3/teams/"+itoa(teamID)+"/repos/legacy-repo-org/legacy-repo", defaultToken,
		map[string]interface{}{"permission": "push"}), http.StatusNoContent, "legacy add team repo")
	expectStatus(t, s.put(t, "/api/v3/teams/"+itoa(teamID)+"/repos/legacy-repo-org/no-such-repo", defaultToken, nil),
		http.StatusNotFound, "legacy add unknown repo")
	expectStatus(t, s.put(t, "/api/v3/teams/"+itoa(teamID)+"/repos/legacy-repo-org/legacy-repo", defaultToken,
		map[string]interface{}{"permission": "own"}), http.StatusUnprocessableEntity, "legacy add bad permission")

	repos := decodeJSONArray(t, s.get(t, "/api/v3/teams/"+itoa(teamID)+"/repos", defaultToken))
	if len(repos) != 1 || repos[0]["full_name"] != "legacy-repo-org/legacy-repo" {
		t.Fatalf("legacy team repos = %v", repos)
	}

	// Plain check answers 204; the repository media type answers the
	// team-repository body with the permission-derived role name.
	expectStatus(t, s.get(t, "/api/v3/teams/"+itoa(teamID)+"/repos/legacy-repo-org/legacy-repo", defaultToken),
		http.StatusNoContent, "legacy check team repo")
	req, err := http.NewRequest("GET", s.baseURL+"/api/v3/teams/"+itoa(teamID)+"/repos/legacy-repo-org/legacy-repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+defaultToken)
	req.Header.Set("Accept", "application/vnd.github.v3.repository+json")
	mediaResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if mediaResp.StatusCode != http.StatusOK {
		mediaResp.Body.Close()
		t.Fatalf("legacy check with repository media type: %d", mediaResp.StatusCode)
	}
	teamRepo := decodeJSON(t, mediaResp)
	if teamRepo["role_name"] != "write" {
		t.Fatalf("team repository role_name = %v, want write", teamRepo["role_name"])
	}

	expectStatus(t, s.delete(t, "/api/v3/teams/"+itoa(teamID)+"/repos/legacy-repo-org/legacy-repo", defaultToken),
		http.StatusNoContent, "legacy remove team repo")
	expectStatus(t, s.get(t, "/api/v3/teams/"+itoa(teamID)+"/repos/legacy-repo-org/legacy-repo", defaultToken),
		http.StatusNotFound, "legacy check after remove")
}
