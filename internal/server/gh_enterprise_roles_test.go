package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEnterpriseRoleAssignmentJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseRoleRoutes()
	s.registerGHEnterpriseSCIMRoutes()
	base := "/api/v3/enterprises/bleephub/enterprise-roles"

	list := decodeRecorderObject(t, enterpriseActionsRequest(t, s, http.MethodGet, base, nil))
	if list["total_count"] != float64(2) {
		t.Fatalf("enterprise role catalog = %#v", list)
	}

	userRec := enterpriseActionsRequest(t, s, http.MethodPost,
		"/api/v3/scim/v2/enterprises/bleephub/Users", map[string]interface{}{
			"userName": "enterprise-role-user", "active": true,
		})
	userID := decodeRecorderObject(t, userRec)["id"].(string)
	scimUser := s.store.EnterpriseSettings.SCIMUsers[userID]
	team := s.store.CreateEnterpriseTeam("Role Team", "", "", nil, "")
	s.store.AddEnterpriseTeamMember(team, scimUser.UserID)

	rec := enterpriseActionsRequest(t, s, http.MethodPut,
		base+"/teams/role-team/8030", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("assign team role = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPut,
		base+"/users/admin/8031", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("assign user role = %d %q", rec.Code, rec.Body.String())
	}

	var teams []map[string]interface{}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/8030/teams", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &teams); err != nil || len(teams) != 1 ||
		teams[0]["slug"] != "role-team" {
		t.Fatalf("role teams = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	var users []map[string]interface{}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/8030/users", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil || len(users) != 1 ||
		users[0]["assignment"] != "indirect" {
		t.Fatalf("inherited role users = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/8031/users", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil || len(users) != 1 ||
		users[0]["login"] != "admin" || users[0]["assignment"] != "direct" {
		t.Fatalf("direct role users = %d %q: %v", rec.Code, rec.Body.String(), err)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete,
		base+"/teams/role-team/8030", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke team role = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete,
		base+"/users/admin", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke all user roles = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/8031/users", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil || len(users) != 0 {
		t.Fatalf("revoked role users = %d %q: %v", rec.Code, rec.Body.String(), err)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPut,
		base+"/teams/missing/8030", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing team assignment = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPut,
		base+"/users/admin/999999", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing role assignment = %d %q", rec.Code, rec.Body.String())
	}
}
