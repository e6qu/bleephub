package bleephub

import (
	"net/http"
	"testing"
)

// TestEnvironmentTeamReviewersRender pins the vendored `environment` schema's
// reviewer union: a Team reviewer renders its resolved team object (id, slug,
// name, ...) beside a User reviewer's user object.
func TestEnvironmentTeamReviewersRender(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.createTestOrg(t)
	repo, _ := s.createOrgRepoForGovernance(t, org)

	team := decodeJSONWithStatus(t, s.post(t, "/api/v3/orgs/"+org+"/teams", defaultToken,
		map[string]interface{}{"name": "Deploy Approvers"}), http.StatusCreated)
	teamID := int(team["id"].(float64))
	teamSlug := team["slug"].(string)
	admin := s.store.LookupUserByLogin("admin")

	decodeJSONWithStatus(t, s.put(t, repo.path()+"/environments/production", defaultToken,
		map[string]interface{}{
			"reviewers": []map[string]interface{}{
				{"type": "Team", "id": teamID},
				{"type": "User", "id": admin.ID},
			},
		}), http.StatusOK)

	env := decodeJSONWithStatus(t, s.get(t, repo.path()+"/environments/production", defaultToken), http.StatusOK)
	rules, _ := env["protection_rules"].([]interface{})
	var reviewers []interface{}
	for _, rule := range rules {
		m, _ := rule.(map[string]interface{})
		if m["type"] == "required_reviewers" {
			reviewers, _ = m["reviewers"].([]interface{})
		}
	}
	if len(reviewers) != 2 {
		t.Fatalf("required_reviewers has %d reviewers, want 2: %v", len(reviewers), rules)
	}

	teamEntry, _ := reviewers[0].(map[string]interface{})
	if teamEntry["type"] != "Team" {
		t.Fatalf("reviewers[0].type = %v, want Team", teamEntry["type"])
	}
	teamObj, _ := teamEntry["reviewer"].(map[string]interface{})
	if teamObj == nil {
		t.Fatalf("Team reviewer did not resolve to a team object: %v", teamEntry)
	}
	if int(teamObj["id"].(float64)) != teamID || teamObj["slug"] != teamSlug || teamObj["name"] != "Deploy Approvers" {
		t.Errorf("team reviewer = id %v slug %v name %v, want %d/%s/Deploy Approvers",
			teamObj["id"], teamObj["slug"], teamObj["name"], teamID, teamSlug)
	}
	if _, hasParent := teamObj["parent"]; !hasParent {
		t.Errorf("team reviewer lacks the team schema's parent member: %v", teamObj)
	}

	userEntry, _ := reviewers[1].(map[string]interface{})
	userObj, _ := userEntry["reviewer"].(map[string]interface{})
	if userEntry["type"] != "User" || userObj == nil || userObj["login"] != "admin" {
		t.Errorf("User reviewer = %v, want the resolved admin user", userEntry)
	}
}
