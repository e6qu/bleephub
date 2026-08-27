package bleephub

import (
	"net/http"
	"regexp"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Hosted-compute networking: orgs bind Actions compute to network settings
// resources. GitHub provisions the settings resources via Azure onboarding,
// not the REST API; bleephub seeds them via /internal/orgs/{org}/network-settings.

func (s *Server) registerGHNetworkConfigurationRoutes() {
	s.route("GET /api/v3/orgs/{org}/settings/network-configurations",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleListOrgNetworkConfigurations)))
	s.route("POST /api/v3/orgs/{org}/settings/network-configurations",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleCreateOrgNetworkConfiguration)))
	s.route("GET /api/v3/orgs/{org}/settings/network-configurations/{network_configuration_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetOrgNetworkConfiguration)))
	s.route("PATCH /api/v3/orgs/{org}/settings/network-configurations/{network_configuration_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleUpdateOrgNetworkConfiguration)))
	s.route("DELETE /api/v3/orgs/{org}/settings/network-configurations/{network_configuration_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleDeleteOrgNetworkConfiguration)))
	s.route("GET /api/v3/orgs/{org}/settings/network-settings/{network_settings_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetOrgNetworkSettings)))

	// Stands in for the Azure onboarding flow that provisions network settings.
	s.route("POST /internal/orgs/{org}/network-settings",
		s.requireSiteAdminHandler(s.orgGated(s.handleSeedOrgNetworkSettings)))
}

var networkConfigurationNameRE = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,100}$`)

func (s *Server) handleListOrgNetworkConfigurations(w http.ResponseWriter, r *http.Request) {
	s.handleListNetworkConfigurationsForScope(w, r, r.PathValue("org"))
}

func (s *Server) handleListNetworkConfigurationsForScope(w http.ResponseWriter, r *http.Request, scope string) {
	configs := s.store.ListNetworkConfigurations(scope)
	total := len(configs)
	configs = paginateAndLink(w, r, configs)
	out := make([]map[string]interface{}, 0, len(configs))
	for _, c := range configs {
		out = append(out, networkConfigurationJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":            total,
		"network_configurations": out,
	})
}

// validateNetworkConfigurationRequest checks the shared create/update
// constraints, writing the validation error on failure.
func (s *Server) validateNetworkConfigurationRequest(w http.ResponseWriter, org string, req *store.NetworkConfigurationRequest, settingsRequired bool) bool {
	if req.Name != nil && !networkConfigurationNameRE.MatchString(*req.Name) {
		store.WriteGHValidationError(w, "NetworkConfiguration", "name", "invalid")
		return false
	}
	if req.ComputeService != nil && *req.ComputeService != "none" && *req.ComputeService != "actions" {
		store.WriteGHValidationError(w, "NetworkConfiguration", "compute_service", "invalid")
		return false
	}
	if settingsRequired && len(req.NetworkSettingsIDs) != 1 {
		store.WriteGHValidationError(w, "NetworkConfiguration", "network_settings_ids", "invalid")
		return false
	}
	if len(req.NetworkSettingsIDs) > 1 || len(req.FailoverNetworkSettingsIDs) > 1 {
		store.WriteGHValidationError(w, "NetworkConfiguration", "network_settings_ids", "invalid")
		return false
	}
	for _, id := range append(append([]string{}, req.NetworkSettingsIDs...), req.FailoverNetworkSettingsIDs...) {
		if s.store.GetNetworkSettings(org, id) == nil {
			store.WriteGHValidationError(w, "NetworkConfiguration", "network_settings_ids", "invalid")
			return false
		}
	}
	return true
}

func (s *Server) handleCreateOrgNetworkConfiguration(w http.ResponseWriter, r *http.Request) {
	s.handleCreateNetworkConfigurationForScope(w, r, r.PathValue("org"))
}

func (s *Server) handleCreateNetworkConfigurationForScope(w http.ResponseWriter, r *http.Request, scope string) {
	var req store.NetworkConfigurationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == nil {
		store.WriteGHValidationError(w, "NetworkConfiguration", "name", "missing_field")
		return
	}
	if !s.validateNetworkConfigurationRequest(w, scope, &req, true) {
		return
	}
	c, err := s.store.CreateNetworkConfiguration(scope, &req)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, networkConfigurationJSON(c))
}

func (s *Server) handleGetOrgNetworkConfiguration(w http.ResponseWriter, r *http.Request) {
	s.handleGetNetworkConfigurationForScope(w, r, r.PathValue("org"))
}

func (s *Server) handleGetNetworkConfigurationForScope(w http.ResponseWriter, r *http.Request, scope string) {
	c := s.store.GetNetworkConfiguration(scope, r.PathValue("network_configuration_id"))
	if c == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, networkConfigurationJSON(c))
}

func (s *Server) handleUpdateOrgNetworkConfiguration(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateNetworkConfigurationForScope(w, r, r.PathValue("org"))
}

func (s *Server) handleUpdateNetworkConfigurationForScope(w http.ResponseWriter, r *http.Request, scope string) {
	c := s.store.GetNetworkConfiguration(scope, r.PathValue("network_configuration_id"))
	if c == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req store.NetworkConfigurationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !s.validateNetworkConfigurationRequest(w, scope, &req, false) {
		return
	}
	updated := s.store.UpdateNetworkConfiguration(scope, c.ID, &req)
	if !mutated(w, updated) {
		return
	}
	writeJSON(w, http.StatusOK, networkConfigurationJSON(updated))
}

func (s *Server) handleDeleteOrgNetworkConfiguration(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteNetworkConfigurationForScope(w, r, r.PathValue("org"))
}

func (s *Server) handleDeleteNetworkConfigurationForScope(w http.ResponseWriter, r *http.Request, scope string) {
	if !s.store.DeleteNetworkConfiguration(scope, r.PathValue("network_configuration_id")) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetOrgNetworkSettings(w http.ResponseWriter, r *http.Request) {
	s.handleGetNetworkSettingsForScope(w, r, r.PathValue("org"))
}

func (s *Server) handleGetNetworkSettingsForScope(w http.ResponseWriter, r *http.Request, scope string) {
	res := s.store.GetNetworkSettings(scope, r.PathValue("network_settings_id"))
	if res == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, networkSettingsJSON(res))
}

func (s *Server) handleSeedOrgNetworkSettings(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req struct {
		Name     string `json:"name"`
		SubnetID string `json:"subnet_id"`
		Region   string `json:"region"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" || req.SubnetID == "" || req.Region == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "name, subnet_id, and region are required")
		return
	}
	res, err := s.store.CreateNetworkSettings(org, req.Name, req.SubnetID, req.Region)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, networkSettingsJSON(res))
}

func networkConfigurationJSON(c *store.NetworkConfiguration) map[string]interface{} {
	settingsIDs := c.NetworkSettingsIDs
	if settingsIDs == nil {
		settingsIDs = []string{}
	}
	out := map[string]interface{}{
		"id":                       c.ID,
		"name":                     c.Name,
		"compute_service":          c.ComputeService,
		"network_settings_ids":     settingsIDs,
		"failover_network_enabled": c.FailoverNetworkEnabled,
		"created_on":               c.CreatedOn.Format(time.RFC3339),
	}
	if len(c.FailoverNetworkSettingsIDs) > 0 {
		out["failover_network_settings_ids"] = c.FailoverNetworkSettingsIDs
	}
	return out
}

func networkSettingsJSON(res *store.NetworkSettingsResource) map[string]interface{} {
	out := map[string]interface{}{
		"id":        res.ID,
		"name":      res.Name,
		"subnet_id": res.SubnetID,
		"region":    res.Region,
	}
	if res.NetworkConfigurationID != "" {
		out["network_configuration_id"] = res.NetworkConfigurationID
	}
	return out
}
