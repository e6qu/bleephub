package bleephub

import (
	"net/http"
	"strings"
	"unicode"

	"github.com/e6qu/bleephub/internal/store"
)

// validCustomPropertyName rejects surrounding/embedded whitespace and control
// characters — a name is a URL path segment on the values/schema endpoints, so
// an invalid one is a 422, not a silently stored definition.
func validCustomPropertyName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Organization custom properties: typed definitions declared once (the schema)
// and assigned per repository (the values).

func (s *Server) registerGHCustomPropertyRoutes() {
	s.route("GET /api/v3/orgs/{org}/properties/schema",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetOrgCustomProperties)))
	s.route("PATCH /api/v3/orgs/{org}/properties/schema",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleBatchUpsertOrgCustomProperties)))
	s.route("GET /api/v3/orgs/{org}/properties/schema/{custom_property_name}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetOrgCustomProperty)))
	s.route("PUT /api/v3/orgs/{org}/properties/schema/{custom_property_name}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleUpsertOrgCustomProperty)))
	s.route("DELETE /api/v3/orgs/{org}/properties/schema/{custom_property_name}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleDeleteOrgCustomProperty)))
	s.route("GET /api/v3/orgs/{org}/properties/values",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleListOrgRepoCustomPropertyValues)))
	s.route("PATCH /api/v3/orgs/{org}/properties/values",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleBatchSetOrgRepoCustomPropertyValues)))
	s.route("GET /api/v3/repos/{owner}/{repo}/properties/values", s.handleGetRepoCustomPropertyValues)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/properties/values",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleSetRepoCustomPropertyValues))
}

var customPropertyValueTypes = map[string]bool{
	"string": true, "single_select": true, "multi_select": true, "true_false": true, "url": true,
}

// customPropertyPayload is the wire shape of a definition write.
type customPropertyPayload struct {
	PropertyName          string      `json:"property_name"`
	ValueType             string      `json:"value_type"`
	Required              bool        `json:"required"`
	DefaultValue          interface{} `json:"default_value"`
	Description           *string     `json:"description"`
	AllowedValues         []string    `json:"allowed_values"`
	ValuesEditableBy      *string     `json:"values_editable_by"`
	RequireExplicitValues bool        `json:"require_explicit_values"`
}

// toCustomProperty validates the payload and materializes the definition.
func (p *customPropertyPayload) toCustomProperty(w http.ResponseWriter, name string) *store.CustomProperty {
	return p.toCustomPropertyFor(w, name, "org_actors", "org_actors", "org_and_repo_actors")
}

func (p *customPropertyPayload) toCustomPropertyFor(
	w http.ResponseWriter,
	name string,
	defaultEditableBy string,
	editableValues ...string,
) *store.CustomProperty {
	if name == "" {
		store.WriteGHValidationError(w, "CustomProperty", "property_name", "missing_field")
		return nil
	}
	if !validCustomPropertyName(name) {
		store.WriteGHValidationError(w, "CustomProperty", "property_name", "invalid")
		return nil
	}
	if !customPropertyValueTypes[p.ValueType] {
		store.WriteGHValidationError(w, "CustomProperty", "value_type", "invalid")
		return nil
	}
	isSelect := p.ValueType == "single_select" || p.ValueType == "multi_select"
	if !isSelect && len(p.AllowedValues) > 0 {
		store.WriteGHValidationError(w, "CustomProperty", "allowed_values", "invalid")
		return nil
	}
	if isSelect && len(p.AllowedValues) > 200 {
		store.WriteGHValidationError(w, "CustomProperty", "allowed_values", "invalid")
		return nil
	}
	if p.Required && p.DefaultValue == nil {
		store.WriteGHValidationError(w, "CustomProperty", "default_value", "missing_field")
		return nil
	}
	if p.DefaultValue != nil {
		if err := validateCustomPropertyValue(&store.CustomProperty{ValueType: p.ValueType, AllowedValues: p.AllowedValues}, p.DefaultValue); err != nil {
			store.WriteGHValidationError(w, "CustomProperty", "default_value", "invalid")
			return nil
		}
	}
	editableBy := defaultEditableBy
	if p.ValuesEditableBy != nil {
		valid := false
		for _, value := range editableValues {
			if *p.ValuesEditableBy == value {
				valid = true
				break
			}
		}
		if !valid {
			store.WriteGHValidationError(w, "CustomProperty", "values_editable_by", "invalid")
			return nil
		}
		editableBy = *p.ValuesEditableBy
	}
	return &store.CustomProperty{
		PropertyName:          name,
		ValueType:             p.ValueType,
		Required:              p.Required,
		DefaultValue:          p.DefaultValue,
		Description:           p.Description,
		AllowedValues:         p.AllowedValues,
		ValuesEditableBy:      editableBy,
		RequireExplicitValues: p.RequireExplicitValues,
	}
}

func (s *Server) handleGetOrgCustomProperties(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	props := s.store.ListCustomProperties(org)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(props))
	for _, p := range props {
		out = append(out, s.customPropertyJSONForOrg(p, org, base))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBatchUpsertOrgCustomProperties(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
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
	defs := make([]*store.CustomProperty, 0, len(req.Properties))
	for i := range req.Properties {
		def := req.Properties[i].toCustomProperty(w, req.Properties[i].PropertyName)
		if def == nil {
			return
		}
		defs = append(defs, def)
	}
	for _, def := range defs {
		s.store.UpsertCustomProperty(org, def)
	}
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(defs))
	for _, def := range defs {
		out = append(out, customPropertyJSON(def, org, base))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetOrgCustomProperty(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	name := r.PathValue("custom_property_name")
	p := s.store.GetCustomProperty(org, name)
	if p == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.customPropertyJSONForOrg(p, org, s.baseURL(r)))
}

func (s *Server) handleUpsertOrgCustomProperty(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	name := r.PathValue("custom_property_name")
	var req customPropertyPayload
	if !decodeJSONBody(w, r, &req) {
		return
	}
	def := req.toCustomProperty(w, name)
	if def == nil {
		return
	}
	s.store.UpsertCustomProperty(org, def)
	writeJSON(w, http.StatusOK, customPropertyJSON(def, org, s.baseURL(r)))
}

func (s *Server) handleDeleteOrgCustomProperty(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	name := r.PathValue("custom_property_name")
	if !s.store.DeleteCustomProperty(org, name) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgRepoCustomPropertyValues(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	query := r.URL.Query().Get("repository_query")
	repos := s.store.ListOrgReposForProperties(org, query)
	entries := make([]map[string]interface{}, 0, len(repos))
	for _, repo := range repos {
		entries = append(entries, map[string]interface{}{
			"repository_id":        repo.ID,
			"repository_name":      repo.Name,
			"repository_full_name": repo.FullName,
			"properties":           s.store.EffectiveRepoCustomPropertyValues(org, repo.FullName),
		})
	}
	entries = paginateAndLink(w, r, entries)
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleBatchSetOrgRepoCustomPropertyValues(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req struct {
		RepositoryNames []string                           `json:"repository_names"`
		Properties      []store.CustomPropertyValuePayload `json:"properties"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.RepositoryNames) == 0 || len(req.RepositoryNames) > 30 {
		store.WriteGHValidationError(w, "CustomPropertyValues", "repository_names", "invalid")
		return
	}
	if req.Properties == nil {
		store.WriteGHValidationError(w, "CustomPropertyValues", "properties", "missing_field")
		return
	}
	repoKeys := make([]string, 0, len(req.RepositoryNames))
	for _, name := range req.RepositoryNames {
		repo := s.store.GetRepo(org, name)
		if repo == nil {
			store.WriteGHValidationError(w, "CustomPropertyValues", "repository_names", "invalid")
			return
		}
		repoKeys = append(repoKeys, repo.FullName)
	}
	if !s.applyCustomPropertyValues(w, org, repoKeys, req.Properties) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoCustomPropertyValues(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.store.EffectiveRepoCustomPropertyValues(owner, repo.FullName))
}

func (s *Server) handleSetRepoCustomPropertyValues(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}
	var req struct {
		Properties []store.CustomPropertyValuePayload `json:"properties"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Properties == nil {
		store.WriteGHValidationError(w, "CustomPropertyValues", "properties", "missing_field")
		return
	}
	if !s.applyCustomPropertyValues(w, owner, []string{repo.FullName}, req.Properties) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyCustomPropertyValues validates every value against the org schema and
// applies the batch to each repo; a null value unsets the property.
func (s *Server) applyCustomPropertyValues(w http.ResponseWriter, org string, repoKeys []string, values []store.CustomPropertyValuePayload) bool {
	for _, v := range values {
		def := s.store.GetCustomProperty(org, v.PropertyName)
		if def == nil {
			store.WriteGHValidationError(w, "CustomPropertyValues", "property_name", "invalid")
			return false
		}
		if v.Value == nil {
			continue
		}
		if err := validateCustomPropertyValue(def, v.Value); err != nil {
			store.WriteGHValidationError(w, "CustomPropertyValues", def.PropertyName, "invalid")
			return false
		}
	}
	for _, repoKey := range repoKeys {
		s.store.SetRepoCustomPropertyValues(repoKey, values)
	}
	return true
}

// validateCustomPropertyValue delegates to the store so the GraphQL mutations
// enforce the same value-type and allowed-values rules.
func validateCustomPropertyValue(def *store.CustomProperty, value interface{}) error {
	return store.ValidateCustomPropertyValue(def, value)
}

func customPropertyJSON(p *store.CustomProperty, org, baseURL string) map[string]interface{} {
	var desc interface{}
	if p.Description != nil {
		desc = *p.Description
	}
	out := map[string]interface{}{
		"property_name":           p.PropertyName,
		"url":                     baseURL + "/api/v3/orgs/" + org + "/properties/schema/" + p.PropertyName,
		"source_type":             "organization",
		"value_type":              p.ValueType,
		"required":                p.Required,
		"default_value":           p.DefaultValue,
		"description":             desc,
		"values_editable_by":      p.ValuesEditableBy,
		"require_explicit_values": p.RequireExplicitValues,
	}
	if p.ValueType == "single_select" || p.ValueType == "multi_select" {
		av := p.AllowedValues
		if av == nil {
			av = []string{}
		}
		out["allowed_values"] = av
	} else {
		out["allowed_values"] = nil
	}
	return out
}

func (s *Server) customPropertyJSONForOrg(p *store.CustomProperty, org, baseURL string) map[string]interface{} {
	s.store.Mu.RLock()
	enterpriseOwned := s.store.OrgCustomProperties[org][p.PropertyName] == nil &&
		s.store.EnterpriseSettings.RepositoryCustomProperties[p.PropertyName] != nil
	s.store.Mu.RUnlock()
	if enterpriseOwned {
		return enterprisePropertyJSON(p, baseURL, s.enterpriseSlug(), "properties")
	}
	return customPropertyJSON(p, org, baseURL)
}

// --- store ---
