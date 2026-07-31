package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEnterpriseCopilotSeatJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseCopilotV2Routes()
	admin := s.store.LookupUserByLogin("admin")
	team := s.store.CreateEnterpriseTeam("Copilot Team", "", "", nil, "")
	s.store.AddEnterpriseTeamMember(team, admin.ID)
	base := "/api/v3/enterprises/bleephub/copilot"

	rec := enterpriseActionsRequest(t, s, http.MethodPost, base+"/billing/selected_users",
		map[string]interface{}{"selected_usernames": []string{"admin"}})
	if got := decodeRecorderObject(t, rec)["seats_created"]; rec.Code != http.StatusCreated || got != float64(1) {
		t.Fatalf("add direct seat = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, base+"/billing/selected_enterprise_teams",
		map[string]interface{}{"selected_enterprise_teams": []string{"Copilot Team"}})
	if got := decodeRecorderObject(t, rec)["seats_created"]; rec.Code != http.StatusCreated || got != float64(1) {
		t.Fatalf("add team seat = %d %q", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/billing/seats", nil)
	seats := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || seats["total_seats"] != float64(1) ||
		len(seats["seats"].([]interface{})) != 2 {
		t.Fatalf("list seats = %d %#v", rec.Code, seats)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		"/api/v3/enterprises/bleephub/members/admin/copilot", nil)
	member := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || member["total_seats"] != float64(2) {
		t.Fatalf("member seats = %d %#v", rec.Code, member)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/billing/selected_users",
		map[string]interface{}{"selected_usernames": []string{"admin"}})
	if got := decodeRecorderObject(t, rec)["seats_cancelled"]; rec.Code != http.StatusOK || got != float64(1) {
		t.Fatalf("cancel direct seat = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/billing/selected_enterprise_teams",
		map[string]interface{}{"selected_enterprise_teams": []string{"copilot-team"}})
	if got := decodeRecorderObject(t, rec)["seats_cancelled"]; rec.Code != http.StatusOK || got != float64(1) {
		t.Fatalf("cancel team seat = %d %q", rec.Code, rec.Body.String())
	}
}

func TestEnterpriseCopilotPolicyAndCustomAgentJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseCopilotV2Routes()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "agent-source-org", "Agent Source", "")
	repo := s.store.CreateOrgRepo(org, admin, ".github-private", "", false)
	commitFilesToStorage(t, s, repo.FullName, map[string]string{
		"agents/security-reviewer.md": "# Security Reviewer\n",
		"README.md":                   "private agent definitions\n",
	})
	base := "/api/v3/enterprises/bleephub/copilot"

	rec := enterpriseActionsRequest(t, s, http.MethodPut, base+"/content_exclusion",
		map[string]interface{}{"agent-source-org/.github-private": []interface{}{"secrets/**"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set content exclusion = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/content_exclusion", nil)
	if rules := decodeRecorderObject(t, rec); len(rules) != 1 {
		t.Fatalf("get content exclusion = %d %#v", rec.Code, rules)
	}

	createRuleset := false
	rec = enterpriseActionsRequest(t, s, http.MethodPut, base+"/custom-agents/source",
		map[string]interface{}{"organization_id": org.ID, "create_ruleset": createRuleset})
	source := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK ||
		source["organization"].(map[string]interface{})["login"] != org.Login {
		t.Fatalf("set agent source = %d %#v", rec.Code, source)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/custom-agents", nil)
	agents := decodeRecorderObject(t, rec)
	items, _ := agents["custom_agents"].([]interface{})
	if rec.Code != http.StatusOK || len(items) != 1 ||
		items[0].(map[string]interface{})["name"] != "Security Reviewer" {
		t.Fatalf("custom agents = %d %#v", rec.Code, agents)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/usage-records", nil)
	var usage []interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &usage); rec.Code != http.StatusOK || err != nil || len(usage) != 0 {
		t.Fatalf("usage records = %d %q: %v", rec.Code, rec.Body.String(), err)
	}

	app, err := s.store.CreateAppE(admin.ID, "AI Controls Reader", "", map[string]string{
		"enterprise_ai_controls": "read",
	}, nil)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	installation := s.store.CreateInstallation(app.ID, "Enterprise", 1, "bleephub", app.Permissions, nil)
	token := s.store.CreateInstallationToken(installation.ID, app.ID, installation.Permissions, nil)
	rec = enterpriseBearerRequest(t, s, http.MethodGet, base+"/custom-agents/source", nil, token.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("IAT read agent source = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseBearerRequest(t, s, http.MethodDelete, base+"/custom-agents/source", nil, token.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only IAT delete agent source = %d %q", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/custom-agents/source", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete agent source = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/custom-agents", nil)
	if agents := decodeRecorderObject(t, rec); agents["custom_agents"] != nil {
		t.Fatalf("agents after source removal = %#v", agents)
	}
}
