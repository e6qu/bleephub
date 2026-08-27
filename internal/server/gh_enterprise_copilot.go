package bleephub

import (
	"net/http"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHEnterpriseCopilotRoutes() {
	s.route("PUT /api/v3/enterprises/{enterprise}/copilot/policies/coding_agent", s.requireEnterpriseOwner(s.handleSetEnterpriseCopilotCodingAgentPolicy))
	s.route("POST /api/v3/enterprises/{enterprise}/copilot/policies/coding_agent/organizations", s.requireEnterpriseOwner(s.handleAddEnterpriseCopilotCodingAgentOrgs))
	s.route("DELETE /api/v3/enterprises/{enterprise}/copilot/policies/coding_agent/organizations", s.requireEnterpriseOwner(s.handleRemoveEnterpriseCopilotCodingAgentOrgs))

	s.route("GET /api/v3/enterprises/{enterprise}/copilot/metrics/reports/enterprise-1-day", s.requireEnterpriseOwner(s.handleEnterpriseCopilotOneDayReport))
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/metrics/reports/enterprise-28-day/latest", s.requireEnterpriseOwner(s.handleEnterpriseCopilotLatest28DayReport))
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/metrics/reports/user-teams-1-day", s.requireEnterpriseOwner(s.handleEnterpriseCopilotOneDayReport))
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/metrics/reports/users-1-day", s.requireEnterpriseOwner(s.handleEnterpriseCopilotOneDayReport))
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/metrics/reports/repos-1-day", s.requireEnterpriseOwner(s.handleEnterpriseCopilotOneDayReport))
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/metrics/reports/users-28-day/latest", s.requireEnterpriseOwner(s.handleEnterpriseCopilotLatest28DayReport))
}

func (s *Server) handleSetEnterpriseCopilotCodingAgentPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PolicyState string `json:"policy_state"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.PolicyState {
	case "enabled_for_all_orgs", "disabled_for_all_orgs", "enabled_for_selected_orgs", "configured_by_org_admins":
	default:
		writeGHError(w, http.StatusBadRequest, "Invalid request. policy_state must be one of enabled_for_all_orgs, disabled_for_all_orgs, enabled_for_selected_orgs, configured_by_org_admins.")
		return
	}
	s.store.SetEnterpriseCopilotCodingAgentPolicy(req.PolicyState)
	w.WriteHeader(http.StatusNoContent)
}

// copilotCodingAgentOrgSelection is the org selection body: explicit logins
// and/or custom property filters.
type copilotCodingAgentOrgSelection struct {
	Organizations    []string `json:"organizations"`
	CustomProperties []struct {
		PropertyName string   `json:"property_name"`
		Values       []string `json:"values"`
	} `json:"custom_properties"`
}

// resolveCopilotCodingAgentOrgs resolves the selection to existing org logins,
// silently skipping the rest. bleephub stores no org custom property values,
// so property filters match nothing.
func (s *Server) resolveCopilotCodingAgentOrgs(sel copilotCodingAgentOrgSelection) []string {
	var out []string
	for _, login := range sel.Organizations {
		if org := s.store.GetOrg(login); org != nil {
			out = append(out, org.Login)
		}
	}
	return out
}

// requireSelectedOrgsPolicy 400s unless the policy is enabled_for_selected_orgs,
// the precondition for editing the org selection.
func (s *Server) requireSelectedOrgsPolicy(w http.ResponseWriter) bool {
	s.store.Mu.RLock()
	policy := s.store.EnterpriseSettings.CopilotCodingAgentPolicy
	s.store.Mu.RUnlock()
	if policy != "enabled_for_selected_orgs" {
		writeGHError(w, http.StatusBadRequest, "The enterprise's coding agent policy must be set to enabled_for_selected_orgs before using this endpoint.")
		return false
	}
	return true
}

func (s *Server) handleAddEnterpriseCopilotCodingAgentOrgs(w http.ResponseWriter, r *http.Request) {
	var req copilotCodingAgentOrgSelection
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !s.requireSelectedOrgsPolicy(w) {
		return
	}
	s.store.AddEnterpriseCopilotCodingAgentOrgs(s.resolveCopilotCodingAgentOrgs(req))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveEnterpriseCopilotCodingAgentOrgs(w http.ResponseWriter, r *http.Request) {
	var req copilotCodingAgentOrgSelection
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !s.requireSelectedOrgsPolicy(w) {
		return
	}
	s.store.RemoveEnterpriseCopilotCodingAgentOrgs(s.resolveCopilotCodingAgentOrgs(req))
	w.WriteHeader(http.StatusNoContent)
}

// Copilot usage metrics reports. bleephub records no Copilot usage events, so
// every report is empty (the documented shape with no download artifacts)
// rather than fabricated.

func (s *Server) handleEnterpriseCopilotOneDayReport(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("day")
	if day == "" {
		store.WriteGHValidationError(w, "CopilotUsageMetricsReport", "day", "missing")
		return
	}
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil {
		store.WriteGHValidationError(w, "CopilotUsageMetricsReport", "day", "invalid")
		return
	}
	// A report can only exist for a day that has already passed.
	if parsed.After(s.currentTime().Truncate(24 * time.Hour)) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"download_links": []string{},
		"report_day":     parsed.Format("2006-01-02"),
	})
}

func (s *Server) handleEnterpriseCopilotLatest28DayReport(w http.ResponseWriter, r *http.Request) {
	// Covers the 28 full days ending yesterday.
	end := s.currentTime().Truncate(24*time.Hour).AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -27)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"download_links":   []string{},
		"report_start_day": start.Format("2006-01-02"),
		"report_end_day":   end.Format("2006-01-02"),
	})
}
