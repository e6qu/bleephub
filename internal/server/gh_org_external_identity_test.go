package bleephub

import (
	"net/http"
	"strconv"
	"testing"
)

func TestOrganizationExternalIdentityAndTeamSyncJourney(t *testing.T) {
	createOrgViaAdminAPI(t, "external-id-org")
	team := decodeJSON(t, ghPost(t, "/api/v3/orgs/external-id-org/teams", defaultToken,
		map[string]interface{}{"name": "Identity Team"}))
	teamID := int(team["id"].(float64))
	mappingPath := "/api/v3/orgs/external-id-org/teams/identity-team/team-sync/group-mappings"

	mapped := decodeJSON(t, ghPatch(t, mappingPath, defaultToken, map[string]interface{}{
		"groups": []map[string]interface{}{{
			"group_id": "directory-group-42", "group_name": "Platform Engineers",
			"group_description": "Identity-provider group",
		}},
	}))
	groups := mapped["groups"].([]interface{})
	if len(groups) != 1 || groups[0].(map[string]interface{})["status"] != "synced" {
		t.Fatalf("created group mappings = %#v", mapped)
	}
	legacy := decodeJSON(t, ghGet(t, "/api/v3/teams/"+strconv.Itoa(teamID)+"/team-sync/group-mappings", defaultToken))
	if len(legacy["groups"].([]interface{})) != 1 {
		t.Fatalf("legacy group mappings = %#v", legacy)
	}
	available := decodeJSON(t, ghGet(t, "/api/v3/orgs/external-id-org/team-sync/groups", defaultToken))
	if len(available["groups"].([]interface{})) != 1 {
		t.Fatalf("available team-sync groups = %#v", available)
	}

	external := decodeJSON(t, ghGet(t, "/api/v3/orgs/external-id-org/external-groups", defaultToken))
	externalGroups := external["groups"].([]interface{})
	if len(externalGroups) != 1 {
		t.Fatalf("external groups = %#v", external)
	}
	numericID := int(externalGroups[0].(map[string]interface{})["group_id"].(float64))
	detail := decodeJSON(t, ghGet(t, "/api/v3/orgs/external-id-org/external-group/"+strconv.Itoa(numericID), defaultToken))
	if detail["group_name"] != "Platform Engineers" || len(detail["teams"].([]interface{})) != 1 {
		t.Fatalf("external group detail = %#v", detail)
	}
	linked := decodeJSON(t, ghGet(t, "/api/v3/orgs/external-id-org/teams/identity-team/external-groups", defaultToken))
	if len(linked["groups"].([]interface{})) != 1 {
		t.Fatalf("linked external groups = %#v", linked)
	}

	expectStatus(t, ghDelete(t, "/api/v3/orgs/external-id-org/teams/identity-team/external-groups", defaultToken),
		http.StatusNoContent, "unlink external group")
	linked = decodeJSON(t, ghGet(t, "/api/v3/orgs/external-id-org/teams/identity-team/external-groups", defaultToken))
	if len(linked["groups"].([]interface{})) != 0 {
		t.Fatalf("linked groups after unlink = %#v", linked)
	}
	relinked := decodeJSON(t, ghPatch(t, "/api/v3/orgs/external-id-org/teams/identity-team/external-groups",
		defaultToken, map[string]interface{}{"group_id": numericID}))
	if relinked["group_id"] != float64(numericID) {
		t.Fatalf("relinked external group = %#v", relinked)
	}
	expectStatus(t, ghPatch(t, mappingPath, defaultToken, map[string]interface{}{
		"groups": []map[string]interface{}{{"group_id": "", "group_name": "", "group_description": ""}},
	}), http.StatusUnprocessableEntity, "invalid group mapping")
}
