package bleephub

import (
	"net/http"
	"testing"
)

// The org Code security governance tab POSTs a configuration with a name,
// description, enforcement, advanced_security, and enabled/disabled/not_set
// feature toggles. This pins that exact create body round-trips through the
// OpenAPI shape validator and lists back.
func TestCodeSecurityConfigAuthoring_CreateRoundTrips(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createOrg(t, "csc-org")

	body := map[string]interface{}{
		"name":                            "Baseline",
		"description":                     "Org baseline security configuration",
		"enforcement":                     "enforced",
		"advanced_security":               "disabled",
		"dependency_graph":                "enabled",
		"dependabot_alerts":               "enabled",
		"dependabot_security_updates":     "not_set",
		"code_scanning_default_setup":     "enabled",
		"secret_scanning":                 "enabled",
		"secret_scanning_push_protection": "enabled",
		"private_vulnerability_reporting": "not_set",
	}
	resp := s.post(t, "/api/v3/orgs/csc-org/code-security/configurations", defaultToken, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create code security configuration = %d, want 201", resp.StatusCode)
	}
	created := decodeJSON(t, resp)
	if created["name"] != "Baseline" || created["dependency_graph"] != "enabled" {
		t.Fatalf("unexpected created config: %v", created)
	}

	list := decodeJSONArray(t, s.get(t, "/api/v3/orgs/csc-org/code-security/configurations", defaultToken))
	found := false
	for _, c := range list {
		if c["name"] == "Baseline" {
			found = true
		}
	}
	if !found {
		t.Fatalf("created configuration not listed: %v", list)
	}
}
