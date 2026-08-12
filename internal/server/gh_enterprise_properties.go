package bleephub

import (
	"net/http"
	"sort"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHEnterprisePropertyRoutes() {
	s.route("GET /api/v3/enterprises/{enterprise}/properties/schema", s.requireEnterpriseMember(s.handleListEnterpriseRepositoryProperties))
	s.route("PATCH /api/v3/enterprises/{enterprise}/properties/schema", s.requireEnterpriseOwner(s.handleBatchUpsertEnterpriseRepositoryProperties))
	s.route("GET /api/v3/enterprises/{enterprise}/properties/schema/{custom_property_name}", s.requireEnterpriseMember(s.handleGetEnterpriseRepositoryProperty))
	s.route("PUT /api/v3/enterprises/{enterprise}/properties/schema/{custom_property_name}", s.requireEnterpriseOwner(s.handleUpsertEnterpriseRepositoryProperty))
	s.route("DELETE /api/v3/enterprises/{enterprise}/properties/schema/{custom_property_name}", s.requireEnterpriseOwner(s.handleDeleteEnterpriseRepositoryProperty))
	s.route("PUT /api/v3/enterprises/{enterprise}/properties/schema/organizations/{org}/{custom_property_name}/promote", s.requireEnterpriseOwner(s.handlePromoteOrganizationRepositoryProperty))

	s.route("GET /api/v3/enterprises/{enterprise}/org-properties/schema", s.requireEnterpriseMember(s.handleListEnterpriseOrganizationProperties))
	s.route("PATCH /api/v3/enterprises/{enterprise}/org-properties/schema", s.requireEnterpriseOwner(s.handleBatchUpsertEnterpriseOrganizationProperties))
	s.route("GET /api/v3/enterprises/{enterprise}/org-properties/schema/{custom_property_name}", s.requireEnterpriseMember(s.handleGetEnterpriseOrganizationProperty))
	s.route("PUT /api/v3/enterprises/{enterprise}/org-properties/schema/{custom_property_name}", s.requireEnterpriseOwner(s.handleUpsertEnterpriseOrganizationProperty))
	s.route("DELETE /api/v3/enterprises/{enterprise}/org-properties/schema/{custom_property_name}", s.requireEnterpriseOwner(s.handleDeleteEnterpriseOrganizationProperty))
	s.route("GET /api/v3/enterprises/{enterprise}/org-properties/values", s.requireEnterpriseMember(s.handleListEnterpriseOrganizationPropertyValues))
	s.route("PATCH /api/v3/enterprises/{enterprise}/org-properties/values", s.requireEnterpriseOwner(s.handleSetEnterpriseOrganizationPropertyValues))
}

func enterprisePropertyJSON(p *store.CustomProperty, baseURL, enterprise, family string) map[string]interface{} {
	out := customPropertyJSON(p, enterprise, baseURL)
	out["url"] = baseURL + "/api/v3/enterprises/" + enterprise + "/" + family + "/schema/" + p.PropertyName
	out["source_type"] = "enterprise"
	return out
}

func sortedEnterpriseProperties(properties map[string]*store.CustomProperty) []*store.CustomProperty {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*store.CustomProperty, 0, len(names))
	for _, name := range names {
		out = append(out, properties[name])
	}
	return out
}

func (s *Server) listEnterpriseProperties(w http.ResponseWriter, r *http.Request, family string, organization bool) {
	s.store.Mu.RLock()
	properties := s.store.EnterpriseSettings.RepositoryCustomProperties
	if organization {
		properties = s.store.EnterpriseSettings.OrganizationCustomProperties
	}
	sorted := sortedEnterpriseProperties(properties)
	out := make([]map[string]interface{}, 0, len(sorted))
	for _, property := range sorted {
		out = append(out, enterprisePropertyJSON(property, s.baseURL(r), s.enterpriseSlug(), family))
	}
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListEnterpriseRepositoryProperties(w http.ResponseWriter, r *http.Request) {
	s.listEnterpriseProperties(w, r, "properties", false)
}

func (s *Server) handleListEnterpriseOrganizationProperties(w http.ResponseWriter, r *http.Request) {
	s.listEnterpriseProperties(w, r, "org-properties", true)
}

func (s *Server) getEnterpriseProperty(w http.ResponseWriter, r *http.Request, family string, organization bool) {
	name := r.PathValue("custom_property_name")
	s.store.Mu.RLock()
	properties := s.store.EnterpriseSettings.RepositoryCustomProperties
	if organization {
		properties = s.store.EnterpriseSettings.OrganizationCustomProperties
	}
	property := properties[name]
	if property == nil {
		s.store.Mu.RUnlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := enterprisePropertyJSON(property, s.baseURL(r), s.enterpriseSlug(), family)
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetEnterpriseRepositoryProperty(w http.ResponseWriter, r *http.Request) {
	s.getEnterpriseProperty(w, r, "properties", false)
}

func (s *Server) handleGetEnterpriseOrganizationProperty(w http.ResponseWriter, r *http.Request) {
	s.getEnterpriseProperty(w, r, "org-properties", true)
}

func enterprisePropertyFromPayload(w http.ResponseWriter, payload *customPropertyPayload, name string, organization bool) *store.CustomProperty {
	if organization {
		return payload.toCustomPropertyFor(w, name, "enterprise_actors", "enterprise_actors", "enterprise_and_org_actors")
	}
	return payload.toCustomPropertyFor(w, name, "org_actors", "org_actors", "org_and_repo_actors")
}

func (s *Server) upsertEnterpriseProperty(w http.ResponseWriter, r *http.Request, family string, organization bool) {
	var req customPropertyPayload
	if !decodeJSONBody(w, r, &req) {
		return
	}
	property := enterprisePropertyFromPayload(w, &req, r.PathValue("custom_property_name"), organization)
	if property == nil {
		return
	}
	s.store.Mu.Lock()
	properties := s.store.EnterpriseSettings.RepositoryCustomProperties
	if organization {
		properties = s.store.EnterpriseSettings.OrganizationCustomProperties
	}
	properties[property.PropertyName] = property
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, enterprisePropertyJSON(property, s.baseURL(r), s.enterpriseSlug(), family))
}

func (s *Server) handleUpsertEnterpriseRepositoryProperty(w http.ResponseWriter, r *http.Request) {
	s.upsertEnterpriseProperty(w, r, "properties", false)
}

func (s *Server) handleUpsertEnterpriseOrganizationProperty(w http.ResponseWriter, r *http.Request) {
	s.upsertEnterpriseProperty(w, r, "org-properties", true)
}

func (s *Server) batchUpsertEnterpriseProperties(w http.ResponseWriter, r *http.Request, family string, organization bool) {
	var req struct {
		Properties []customPropertyPayload `json:"properties"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.Properties) == 0 {
		store.WriteGHValidationError(w, "CustomProperty", "properties", "missing_field")
		return
	}
	definitions := make([]*store.CustomProperty, 0, len(req.Properties))
	for i := range req.Properties {
		definition := enterprisePropertyFromPayload(w, &req.Properties[i], req.Properties[i].PropertyName, organization)
		if definition == nil {
			return
		}
		definitions = append(definitions, definition)
	}
	s.store.Mu.Lock()
	properties := s.store.EnterpriseSettings.RepositoryCustomProperties
	if organization {
		properties = s.store.EnterpriseSettings.OrganizationCustomProperties
	}
	for _, definition := range definitions {
		properties[definition.PropertyName] = definition
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	out := make([]map[string]interface{}, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, enterprisePropertyJSON(definition, s.baseURL(r), s.enterpriseSlug(), family))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBatchUpsertEnterpriseRepositoryProperties(w http.ResponseWriter, r *http.Request) {
	s.batchUpsertEnterpriseProperties(w, r, "properties", false)
}

func (s *Server) handleBatchUpsertEnterpriseOrganizationProperties(w http.ResponseWriter, r *http.Request) {
	s.batchUpsertEnterpriseProperties(w, r, "org-properties", true)
}

func (s *Server) deleteEnterpriseProperty(w http.ResponseWriter, r *http.Request, organization bool) {
	name := r.PathValue("custom_property_name")
	s.store.Mu.Lock()
	properties := s.store.EnterpriseSettings.RepositoryCustomProperties
	if organization {
		properties = s.store.EnterpriseSettings.OrganizationCustomProperties
	}
	if properties[name] == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(properties, name)
	if organization {
		for _, values := range s.store.EnterpriseSettings.OrganizationPropertyValues {
			delete(values, name)
		}
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteEnterpriseRepositoryProperty(w http.ResponseWriter, r *http.Request) {
	s.deleteEnterpriseProperty(w, r, false)
}

func (s *Server) handleDeleteEnterpriseOrganizationProperty(w http.ResponseWriter, r *http.Request) {
	s.deleteEnterpriseProperty(w, r, true)
}

func (s *Server) handlePromoteOrganizationRepositoryProperty(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	name := r.PathValue("custom_property_name")
	property := s.store.GetCustomProperty(org.Login, name)
	if property == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	copy := *property
	copy.AllowedValues = append([]string(nil), property.AllowedValues...)
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.RepositoryCustomProperties[name] = &copy
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, enterprisePropertyJSON(&copy, s.baseURL(r), s.enterpriseSlug(), "properties"))
}

func (s *Server) handleListEnterpriseOrganizationPropertyValues(w http.ResponseWriter, r *http.Request) {
	orgs := s.store.ListOrgsAll(0)
	s.store.Mu.RLock()
	out := make([]map[string]interface{}, 0, len(orgs))
	for _, org := range orgs {
		values := s.store.EnterpriseSettings.OrganizationPropertyValues[org.Login]
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		properties := make([]map[string]interface{}, 0, len(names))
		for _, name := range names {
			properties = append(properties, map[string]interface{}{"property_name": name, "value": values[name]})
		}
		out = append(out, map[string]interface{}{
			"organization_id": org.ID, "organization_login": org.Login, "properties": properties,
		})
	}
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleSetEnterpriseOrganizationPropertyValues(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrganizationLogins []string                           `json:"organization_logins"`
		Properties         []store.CustomPropertyValuePayload `json:"properties"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.OrganizationLogins) == 0 {
		store.WriteGHValidationError(w, "CustomPropertyValues", "organization_logins", "missing_field")
		return
	}
	if len(req.Properties) == 0 {
		store.WriteGHValidationError(w, "CustomPropertyValues", "properties", "missing_field")
		return
	}
	s.store.Mu.RLock()
	for _, login := range req.OrganizationLogins {
		if s.store.OrgsByLogin[login] == nil {
			s.store.Mu.RUnlock()
			store.WriteGHValidationError(w, "CustomPropertyValues", "organization_logins", "invalid")
			return
		}
	}
	for _, value := range req.Properties {
		definition := s.store.EnterpriseSettings.OrganizationCustomProperties[value.PropertyName]
		if definition == nil || validateCustomPropertyValue(definition, value.Value) != nil {
			s.store.Mu.RUnlock()
			store.WriteGHValidationError(w, "CustomPropertyValues", "properties", "invalid")
			return
		}
	}
	s.store.Mu.RUnlock()
	s.store.Mu.Lock()
	for _, login := range req.OrganizationLogins {
		values := s.store.EnterpriseSettings.OrganizationPropertyValues[login]
		if values == nil {
			values = map[string]interface{}{}
			s.store.EnterpriseSettings.OrganizationPropertyValues[login] = values
		}
		for _, value := range req.Properties {
			if value.Value == nil {
				delete(values, value.PropertyName)
			} else {
				values[value.PropertyName] = store.CloneCustomPropertyValue(value.Value)
			}
		}
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
