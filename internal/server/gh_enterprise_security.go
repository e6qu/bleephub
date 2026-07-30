package bleephub

import (
	"net/http"
	"sort"
)

func (s *Server) registerGHEnterpriseSecurityRoutes() {
	s.route("GET /api/v3/enterprises/{enterprise}/code-scanning/alerts", s.requireEnterpriseOwner(s.handleListEnterpriseCodeScanningAlerts))
	s.route("GET /api/v3/enterprises/{enterprise}/secret-scanning/alerts", s.requireEnterpriseOwner(s.handleListEnterpriseSecretScanningAlerts))
	s.route("GET /api/v3/enterprises/{enterprise}/secret-scanning/custom-patterns", s.requireEnterpriseOwner(s.handleListEnterpriseSecretScanningCustomPatterns))
	s.route("POST /api/v3/enterprises/{enterprise}/secret-scanning/custom-patterns", s.requireEnterpriseOwner(s.handleCreateEnterpriseSecretScanningCustomPatterns))
	s.route("DELETE /api/v3/enterprises/{enterprise}/secret-scanning/custom-patterns", s.requireEnterpriseOwner(s.handleDeleteEnterpriseSecretScanningCustomPatterns))
	s.route("PATCH /api/v3/enterprises/{enterprise}/secret-scanning/custom-patterns/{pattern_id}", s.requireEnterpriseOwner(s.handleUpdateEnterpriseSecretScanningCustomPattern))
	s.route("GET /api/v3/enterprises/{enterprise}/secret-scanning/pattern-configurations", s.requireEnterpriseOwner(s.handleListEnterpriseSecretScanningPatternConfigurations))
	s.route("PATCH /api/v3/enterprises/{enterprise}/secret-scanning/pattern-configurations", s.requireEnterpriseOwner(s.handleUpdateEnterpriseSecretScanningPatternConfigurations))
	s.route("GET /api/v3/enterprises/{enterprise}/bypass-requests/push-rules", s.requireEnterpriseOwner(s.handleListEnterprisePushRuleBypassRequests))
	s.route("GET /api/v3/enterprises/{enterprise}/bypass-requests/secret-scanning", s.requireEnterpriseOwner(s.handleListEnterpriseSecretScanningBypassRequests))
	s.route("GET /api/v3/enterprises/{enterprise}/dismissal-requests/secret-scanning", s.requireEnterpriseOwner(s.handleListEnterpriseSecretScanningDismissalRequests))
}

func (s *Server) enterpriseCustomPatternScope() string {
	return customPatternScope("enterprise", s.enterpriseSlug())
}

func (s *Server) handleListEnterpriseSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	s.listCustomPatterns(w, r, s.enterpriseCustomPatternScope())
}

func (s *Server) handleCreateEnterpriseSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	s.createCustomPatterns(w, r, s.enterpriseCustomPatternScope())
}

func (s *Server) handleDeleteEnterpriseSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	s.deleteCustomPatterns(w, r, s.enterpriseCustomPatternScope())
}

func (s *Server) handleUpdateEnterpriseSecretScanningCustomPattern(w http.ResponseWriter, r *http.Request) {
	s.updateCustomPattern(w, r, s.enterpriseCustomPatternScope())
}

func (s *Server) handleListEnterpriseSecretScanningPatternConfigurations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.ListSecretScanningPatternConfigurations(s.enterpriseCustomPatternScope()))
}

func (s *Server) handleUpdateEnterpriseSecretScanningPatternConfigurations(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateSecretScanningPatternConfigurationsForScope(w, r, s.enterpriseCustomPatternScope())
}

func (s *Server) handleListEnterpriseSecretScanningAlerts(w http.ResponseWriter, r *http.Request) {
	var alerts []*SecretScanningAlert
	for _, org := range s.store.ListOrgsAll(0) {
		alerts = append(alerts, s.store.ListSecretScanningAlertsByOrg(org.ID)...)
	}
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].CreatedAt.Equal(alerts[j].CreatedAt) {
			if alerts[i].RepoKey == alerts[j].RepoKey {
				return alerts[i].Number > alerts[j].Number
			}
			return alerts[i].RepoKey < alerts[j].RepoKey
		}
		return alerts[i].CreatedAt.After(alerts[j].CreatedAt)
	})
	alerts = paginateAndLink(w, r, alerts)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(alerts))
	for _, alert := range alerts {
		repo := s.store.GetRepoByFullName(alert.RepoKey)
		if repo == nil {
			continue
		}
		value := secretScanningAlertToJSON(alert, baseURL, repo)
		value["repository"] = simpleRepoJSON(repo, s.store, baseURL)
		out = append(out, value)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListEnterpriseCodeScanningAlerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var alerts []*CodeScanningAlert
	for _, org := range s.store.ListOrgsAll(0) {
		alerts = append(alerts, s.store.ListCodeScanningAlertsByOrg(
			org.ID, q.Get("state"), q.Get("severity"), q.Get("tool_name"), q.Get("sort"), q.Get("direction"))...)
	}
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].CreatedAt.Equal(alerts[j].CreatedAt) {
			if alerts[i].RepoKey == alerts[j].RepoKey {
				return alerts[i].Number > alerts[j].Number
			}
			return alerts[i].RepoKey < alerts[j].RepoKey
		}
		return alerts[i].CreatedAt.After(alerts[j].CreatedAt)
	})
	alerts = paginateAndLink(w, r, alerts)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(alerts))
	for _, alert := range alerts {
		repo := s.store.GetRepoByFullName(alert.RepoKey)
		if repo == nil {
			continue
		}
		value := codeScanningAlertToJSON(alert, baseURL, repo)
		value["repository"] = simpleRepoJSON(repo, s.store, baseURL)
		out = append(out, value)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListEnterprisePushRuleBypassRequests(w http.ResponseWriter, r *http.Request) {
	s.writeSecurityReviewList(w, r, s.store.listSecurityReviewRequests("", "", reviewKindPushBypass))
}

func (s *Server) handleListEnterpriseSecretScanningBypassRequests(w http.ResponseWriter, r *http.Request) {
	s.writeSecurityReviewList(w, r, s.store.listSecurityReviewRequests("", "", reviewKindSecretBypass))
}

func (s *Server) handleListEnterpriseSecretScanningDismissalRequests(w http.ResponseWriter, r *http.Request) {
	s.writeSecurityReviewList(w, r, s.store.listSecurityReviewRequests("", "", reviewKindSecretDismissal))
}
