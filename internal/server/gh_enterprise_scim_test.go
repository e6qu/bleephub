package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEnterpriseSCIMUserAndGroupJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseSCIMRoutes()
	base := "/api/v3/scim/v2/enterprises/bleephub"

	rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/Users", map[string]interface{}{
		"schemas":     []string{scimUserSchema},
		"externalId":  "directory-user-1",
		"userName":    "SCIM.User@example.com",
		"displayName": "SCIM User",
		"active":      true,
		"emails": []map[string]interface{}{{
			"value": "scim.user@example.com", "primary": true,
		}},
	})
	createdUser := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || rec.Header().Get("Content-Type") != "application/scim+json" {
		t.Fatalf("create user = %d %q (%q)", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	userID, _ := createdUser["id"].(string)
	if userID == "" || createdUser["userName"] != "scim-user-example-com" {
		t.Fatalf("created SCIM user = %#v", createdUser)
	}
	meta, _ := createdUser["meta"].(map[string]interface{})
	if location, _ := meta["location"].(string); location !=
		"http://example.com/api/v3/scim/v2/enterprises/bleephub/Users/"+userID {
		t.Fatalf("SCIM user location = %q", location)
	}
	backing := s.store.LookupUserByLogin("scim-user-example-com")
	if backing == nil || backing.Email != "scim.user@example.com" || backing.Suspended {
		t.Fatalf("backing user not synchronized: %#v", backing)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/Groups", map[string]interface{}{
		"schemas":     []string{scimGroupSchema},
		"externalId":  "directory-group-1",
		"displayName": "Platform Engineers",
		"members":     []map[string]interface{}{{"value": userID}},
	})
	createdGroup := decodeRecorderObject(t, rec)
	groupID, _ := createdGroup["id"].(string)
	if rec.Code != http.StatusCreated || groupID == "" {
		t.Fatalf("create group = %d %#v", rec.Code, createdGroup)
	}
	team := s.store.GetEnterpriseTeam("platform-engineers")
	if team == nil || len(team.MemberIDs) != 1 || team.MemberIDs[0] != backing.ID {
		t.Fatalf("backing enterprise team not synchronized: %#v", team)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base+"/Users/"+userID, map[string]interface{}{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []map[string]interface{}{{
			"op": "replace",
			"value": map[string]interface{}{
				"displayName": "Renamed User",
				"active":      false,
			},
		}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch user = %d %q", rec.Code, rec.Body.String())
	}
	backing = s.store.LookupUserByLogin("scim-user-example-com")
	if backing.Name != "Renamed User" || !backing.Suspended {
		t.Fatalf("patched backing user = %#v", backing)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base+"/Groups/"+groupID, map[string]interface{}{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []map[string]interface{}{
			{"op": "replace", "path": "displayName", "value": "Platform Core"},
			{"op": "remove", "path": `members[value eq "` + userID + `"]`},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch group = %d %q", rec.Code, rec.Body.String())
	}
	if s.store.GetEnterpriseTeam("platform-engineers") != nil {
		t.Fatal("old team slug remained after SCIM group rename")
	}
	team = s.store.GetEnterpriseTeam("platform-core")
	if team == nil || len(team.MemberIDs) != 0 {
		t.Fatalf("renamed backing team = %#v", team)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		base+`/Users?filter=externalId%20eq%20%22directory-user-1%22`, nil)
	var users map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil ||
		users["totalResults"] != float64(1) {
		t.Fatalf("filtered users = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+`/Groups?filter=unsupported%20eq%20%22x%22`, nil)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/scim+json" {
		t.Fatalf("unsupported group filter = %d %q", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/Users/"+userID, nil)
	if rec.Code != http.StatusNoContent || !backing.Suspended {
		t.Fatalf("delete user = %d, backing=%#v", rec.Code, backing)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/Groups/"+groupID, nil)
	if rec.Code != http.StatusNoContent || s.store.GetEnterpriseTeam("platform-core") != nil {
		t.Fatalf("delete group = %d, team=%#v", rec.Code, s.store.GetEnterpriseTeam("platform-core"))
	}
}

func TestEnterpriseSCIMRejectsInvalidBodiesAndCollidingGroupRename(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseSCIMRoutes()
	base := "/api/v3/scim/v2/enterprises/bleephub"

	rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/Users", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/scim+json" {
		t.Fatalf("missing username = %d %q", rec.Code, rec.Body.String())
	}

	first := enterpriseActionsRequest(t, s, http.MethodPost, base+"/Groups", map[string]interface{}{
		"displayName": "First Group",
	})
	second := enterpriseActionsRequest(t, s, http.MethodPost, base+"/Groups", map[string]interface{}{
		"displayName": "Second Group",
	})
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("create collision fixtures = %d/%d", first.Code, second.Code)
	}
	firstID := decodeRecorderObject(t, first)["id"].(string)
	rec = enterpriseActionsRequest(t, s, http.MethodPut, base+"/Groups/"+firstID, map[string]interface{}{
		"displayName": "Second Group",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("colliding rename = %d %q", rec.Code, rec.Body.String())
	}
	if s.store.GetEnterpriseTeam("first-group") == nil || s.store.GetEnterpriseTeam("second-group") == nil {
		t.Fatalf("slug indexes corrupted after conflict: first=%#v second=%#v",
			s.store.GetEnterpriseTeam("first-group"), s.store.GetEnterpriseTeam("second-group"))
	}
}
