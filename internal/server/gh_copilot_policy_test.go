package bleephub

import (
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// copilotPolicyFixture is an organization with one Copilot seat and a
// member token that holds no administrative standing.
type copilotPolicyFixture struct {
	*isolatedServer
	org          string
	member       *store.User
	memberToken  string
	day          string
	seatUsername string
}

func newCopilotPolicyFixture(t *testing.T) *copilotPolicyFixture {
	t.Helper()
	s := newIsolatedServer(t)
	org := s.createTestOrg(t)
	member := s.createTestUser(t, "copilot-policy-member-"+org)
	token := s.store.CreateToken(member.ID, "repo,user").Value
	s.activateOrgMember(t, org, member.Login, token)
	// Give the member a real seat: usage may only be attributed to a
	// member the organization is billed for.
	resp := s.post(t, "/api/v3/orgs/"+org+"/copilot/billing/selected_users", defaultToken,
		map[string]interface{}{"selected_usernames": []string{member.Login}})
	decodeJSONWithStatus(t, resp, http.StatusCreated)
	return &copilotPolicyFixture{
		isolatedServer: s, org: org, member: member, memberToken: token,
		day: fixedTestTime.UTC().Format("2006-01-02"), seatUsername: member.Login,
	}
}

func TestCopilotOrganizationPolicyIsStoredNotAssumed(t *testing.T) {
	t.Parallel()
	f := newCopilotPolicyFixture(t)

	billing := decodeJSONWithStatus(t, f.get(t, "/api/v3/orgs/"+f.org+"/copilot/billing", defaultToken), http.StatusOK)
	if billing["plan_type"] != store.CopilotPlanBusiness || billing["cli"] != store.CopilotFeatureEnabled {
		t.Fatalf("default Copilot policy = %v", billing)
	}

	updated := decodeJSONWithStatus(t, f.put(t, "/ui-data/orgs/"+f.org+"/copilot/policy", defaultToken, map[string]interface{}{
		"plan_type":               store.CopilotPlanEnterprise,
		"cli":                     store.CopilotFeatureDisabled,
		"public_code_suggestions": store.CopilotSuggestionsBlock,
	}), http.StatusOK)
	if updated["plan_type"] != store.CopilotPlanEnterprise || updated["cli"] != store.CopilotFeatureDisabled {
		t.Fatalf("updated policy = %v", updated)
	}

	// The documented REST endpoint reports what was configured.
	billing = decodeJSONWithStatus(t, f.get(t, "/api/v3/orgs/"+f.org+"/copilot/billing", defaultToken), http.StatusOK)
	if billing["plan_type"] != store.CopilotPlanEnterprise ||
		billing["cli"] != store.CopilotFeatureDisabled ||
		billing["public_code_suggestions"] != store.CopilotSuggestionsBlock ||
		billing["ide_chat"] != store.CopilotFeatureEnabled {
		t.Fatalf("billing after the policy change = %v", billing)
	}
	// So does the per-seat plan_type.
	seats := decodeJSONWithStatus(t, f.get(t, "/api/v3/orgs/"+f.org+"/copilot/billing/seats", defaultToken), http.StatusOK)
	row := seats["seats"].([]interface{})[0].(map[string]interface{})
	if row["plan_type"] != store.CopilotPlanEnterprise {
		t.Fatalf("seat plan_type = %v", row["plan_type"])
	}

	// A value GitHub does not define is refused rather than stored.
	assertSponsorsStatus(t, f.put(t, "/ui-data/orgs/"+f.org+"/copilot/policy", defaultToken,
		map[string]interface{}{"seat_management_setting": "whatever"}), http.StatusUnprocessableEntity)

	// Seat administration is org-owner-only, and so is the policy.
	assertSponsorsStatus(t, f.get(t, "/ui-data/orgs/"+f.org+"/copilot/policy", f.memberToken), http.StatusForbidden)
	assertSponsorsStatus(t, f.put(t, "/ui-data/orgs/"+f.org+"/copilot/policy", f.memberToken,
		map[string]interface{}{"cli": store.CopilotFeatureEnabled}), http.StatusForbidden)
}

func TestCopilotUsageDrivesSeatActivityAndMetrics(t *testing.T) {
	t.Parallel()
	f := newCopilotPolicyFixture(t)

	// Before any usage the seat is honestly inactive and there are no
	// metrics days.
	seats := decodeJSONWithStatus(t, f.get(t, "/api/v3/orgs/"+f.org+"/copilot/billing/seats", defaultToken), http.StatusOK)
	row := seats["seats"].([]interface{})[0].(map[string]interface{})
	if row["last_activity_at"] != nil || row["last_activity_editor"] != nil {
		t.Fatalf("an unused seat must report null activity: %v", row)
	}
	if len(decodeJSONArrayWithStatus(t, f.get(t, "/api/v3/orgs/"+f.org+"/copilot/metrics", defaultToken), http.StatusOK)) != 0 {
		t.Fatal("metrics must be empty before any usage is recorded")
	}

	for _, usage := range []map[string]interface{}{
		{"username": f.seatUsername, "day": f.day, "editor": "vscode", "language": "go",
			"suggestions": 10, "acceptances": 4, "lines_suggested": 30, "lines_accepted": 12},
		{"username": f.seatUsername, "day": f.day, "editor": "vscode", "language": "typescript",
			"suggestions": 6, "acceptances": 3, "lines_suggested": 12, "lines_accepted": 6},
		{"username": f.seatUsername, "day": f.day, "editor": "neovim", "language": "go",
			"suggestions": 2, "acceptances": 1, "lines_suggested": 4, "lines_accepted": 2,
			"chat_turns": 5, "chat_acceptances": 2},
	} {
		decodeJSONWithStatus(t, f.post(t, "/ui-data/orgs/"+f.org+"/copilot/usage", defaultToken, usage), http.StatusCreated)
	}

	metrics := decodeJSONArrayWithStatus(t, f.get(t, "/api/v3/orgs/"+f.org+"/copilot/metrics", defaultToken), http.StatusOK)
	if len(metrics) != 1 {
		t.Fatalf("metrics = %v, want one day", metrics)
	}
	day := metrics[0]
	if day["date"] != f.day || day["total_active_users"].(float64) != 1 || day["total_engaged_users"].(float64) != 1 {
		t.Fatalf("metrics day = %v", day)
	}
	completions := day["copilot_ide_code_completions"].(map[string]interface{})
	editors := completions["editors"].([]interface{})
	if len(editors) != 2 {
		t.Fatalf("editors = %v, want neovim and vscode", editors)
	}
	// Editors are ordered so pagination and diffs are stable.
	if editors[0].(map[string]interface{})["name"] != "neovim" || editors[1].(map[string]interface{})["name"] != "vscode" {
		t.Fatalf("editor order = %v", editors)
	}
	vscode := editors[1].(map[string]interface{})
	languages := vscode["models"].([]interface{})[0].(map[string]interface{})["languages"].([]interface{})
	totalSuggestions := 0
	for _, raw := range languages {
		totalSuggestions += int(raw.(map[string]interface{})["total_code_suggestions"].(float64))
	}
	if totalSuggestions != 16 {
		t.Fatalf("vscode suggestions = %d, want 10 + 6", totalSuggestions)
	}
	chat := day["copilot_ide_chat"].(map[string]interface{})
	chatModel := chat["editors"].([]interface{})[0].(map[string]interface{})["models"].([]interface{})[0].(map[string]interface{})
	if chatModel["total_chats"].(float64) != 5 || chatModel["total_chat_insertion_events"].(float64) != 2 {
		t.Fatalf("chat metrics = %v", chatModel)
	}

	// The seat now reports real activity, and the billing breakdown counts
	// it as active this cycle.
	seats = decodeJSONWithStatus(t, f.get(t, "/api/v3/orgs/"+f.org+"/copilot/billing/seats", defaultToken), http.StatusOK)
	row = seats["seats"].([]interface{})[0].(map[string]interface{})
	if row["last_activity_editor"] != "neovim" || row["last_activity_at"] == nil {
		t.Fatalf("seat activity after usage = %v", row)
	}
	billing := decodeJSONWithStatus(t, f.get(t, "/api/v3/orgs/"+f.org+"/copilot/billing", defaultToken), http.StatusOK)
	breakdown := billing["seat_breakdown"].(map[string]interface{})
	if breakdown["active_this_cycle"].(float64) != 1 || breakdown["inactive_this_cycle"].(float64) != 0 {
		t.Fatalf("seat breakdown = %v", breakdown)
	}

	// The single-day report is now downloadable for that day and still 204
	// for a day with no activity.
	report := decodeJSONWithStatus(t, f.get(t,
		"/api/v3/orgs/"+f.org+"/copilot/metrics/reports/organization-1-day?day="+f.day, defaultToken), http.StatusOK)
	if len(report["download_links"].([]interface{})) != 1 {
		t.Fatalf("one-day report = %v", report)
	}
	assertSponsorsStatus(t, f.get(t,
		"/api/v3/orgs/"+f.org+"/copilot/metrics/reports/organization-1-day?day=2000-01-01", defaultToken), http.StatusNoContent)
}

func TestCopilotUsageRefusesUnbilledMembersAndImpossibleCounts(t *testing.T) {
	t.Parallel()
	f := newCopilotPolicyFixture(t)
	unbilled := f.createTestUser(t, "copilot-unbilled-"+f.org)

	assertSponsorsStatus(t, f.post(t, "/ui-data/orgs/"+f.org+"/copilot/usage", defaultToken,
		map[string]interface{}{"username": unbilled.Login, "editor": "vscode", "suggestions": 1}), http.StatusUnprocessableEntity)
	assertSponsorsStatus(t, f.post(t, "/ui-data/orgs/"+f.org+"/copilot/usage", defaultToken,
		map[string]interface{}{"username": f.seatUsername, "editor": "vscode", "suggestions": 1, "acceptances": 5}), http.StatusUnprocessableEntity)
	assertSponsorsStatus(t, f.post(t, "/ui-data/orgs/"+f.org+"/copilot/usage", defaultToken,
		map[string]interface{}{"username": f.seatUsername, "suggestions": 1}), http.StatusUnprocessableEntity)
	// Recording usage is an owner action, not a member one.
	assertSponsorsStatus(t, f.post(t, "/ui-data/orgs/"+f.org+"/copilot/usage", f.memberToken,
		map[string]interface{}{"username": f.seatUsername, "editor": "vscode", "suggestions": 1}), http.StatusForbidden)
}

func TestCopilotEndpointsAreScopedToTheViewer(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	other := s.createTestUser(t, "copilot-endpoints-other")
	otherToken := s.store.CreateToken(other.ID, "repo,user").Value

	query := `query($login: String!) {
	  viewer { login copilotEndpoints { api exp originTracker proxy telemetry } }
	  user(login: $login) { login copilotEndpoints { api } }
	}`
	data := sponsorsGraphQLAs(t, s, otherToken, query, map[string]interface{}{"login": "admin"})
	viewer := data["viewer"].(map[string]interface{})
	endpoints := viewer["copilotEndpoints"].(map[string]interface{})
	for _, key := range []string{"api", "exp", "originTracker", "proxy", "telemetry"} {
		value, _ := endpoints[key].(string)
		if !strings.Contains(value, "/copilot/") {
			t.Fatalf("copilotEndpoints.%s = %q, want this instance's Copilot endpoint", key, endpoints[key])
		}
	}
	if data["user"].(map[string]interface{})["copilotEndpoints"] != nil {
		t.Fatal("one account's Copilot endpoints must not be readable through another's profile")
	}
}
