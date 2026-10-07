package bleephub

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/e6qu/bleephub/internal/store"
)

// External custom properties for repositories: a GitHub App installation,
// once registered with the organization under a display name, writes its own
// property values onto the organization's repositories. The properties have
// no declared schema; writing a value defines the property.

var externalPropertyDisplayName = regexp.MustCompile(`^[a-zA-Z0-9]{1,15}$`)

func (s *Server) registerGHExternalPropertyRoutes() {
	gate := func(level store.PermLevel, h http.HandlerFunc) http.HandlerFunc {
		return s.requirePerm(store.ScopeOrgExternalProperties, level, s.orgGated(h))
	}
	s.route("GET /api/v3/orgs/{org}/properties/installations", gate(store.PermAdmin, s.handleListExternalPropertyInstallations))
	s.route("POST /api/v3/orgs/{org}/properties/installations", gate(store.PermAdmin, s.handleRegisterExternalPropertyInstallation))
	s.route("GET /api/v3/orgs/{org}/properties/installations/schema", gate(store.PermRead, s.installationOnly(s.handleListExternalProperties)))
	s.route("PATCH /api/v3/orgs/{org}/properties/installations/values", gate(store.PermWrite, s.installationOnly(s.handleSetExternalPropertyValues)))
	s.route("PATCH /api/v3/orgs/{org}/properties/installations/values/{property_name}", gate(store.PermWrite, s.installationOnly(s.handleSetExternalPropertyRepoValues)))
	s.route("DELETE /api/v3/orgs/{org}/properties/installations/values/{property_name}", gate(store.PermWrite, s.installationOnly(s.handleDeleteExternalProperty)))
}

// installationOnly admits only a GitHub App installation registered with the
// organization: the values belong to the installation that writes them.
func (s *Server) installationOnly(h func(http.ResponseWriter, *http.Request, string, *store.Installation)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst := ghInstallationFromContext(r.Context())
		if ghInstallationTokenFromContext(r.Context()) == nil || inst == nil {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		org := s.store.GetOrg(r.PathValue("org"))
		if s.store.GetExternalPropertyInstallation(org.Login, inst.ID) == nil {
			store.WriteGHValidationError(w, "ExternalCustomProperty", "installation_id", "invalid")
			return
		}
		h(w, r, org.Login, inst)
	}
}

func externalPropertyInstallationJSON(reg *store.ExternalPropertyInstallation) map[string]interface{} {
	return map[string]interface{}{
		"display_name": reg.DisplayName,
		"installation": map[string]interface{}{"id": reg.InstallationID},
	}
}

func (s *Server) handleListExternalPropertyInstallations(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	// An installation sees only its own registration; a person sees them all.
	self := ghInstallationFromContext(r.Context())
	out := []map[string]interface{}{}
	for _, reg := range s.store.ListExternalPropertyInstallations(org.Login) {
		if self != nil && reg.InstallationID != self.ID {
			continue
		}
		out = append(out, externalPropertyInstallationJSON(reg))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRegisterExternalPropertyInstallation(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	var req struct {
		InstallationID *int    `json:"installation_id"`
		DisplayName    *string `json:"display_name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.DisplayName == nil {
		store.WriteGHValidationError(w, "ExternalCustomPropertyInstallation", "display_name", "missing_field")
		return
	}
	if !externalPropertyDisplayName.MatchString(*req.DisplayName) {
		store.WriteGHValidationError(w, "ExternalCustomPropertyInstallation", "display_name", "invalid")
		return
	}
	// An installation registers itself by default; anyone else names one.
	self := ghInstallationFromContext(r.Context())
	installationID := 0
	switch {
	case req.InstallationID != nil:
		installationID = *req.InstallationID
	case self != nil:
		installationID = self.ID
	default:
		store.WriteGHValidationError(w, "ExternalCustomPropertyInstallation", "installation_id", "missing_field")
		return
	}
	inst := s.store.GetInstallation(installationID)
	if inst == nil || !installationOnAccount(inst, store.OrganizationAccount, org.Login) {
		store.WriteGHValidationError(w, "ExternalCustomPropertyInstallation", "installation_id", "invalid")
		return
	}
	// The installation being registered, not the caller, must hold write.
	if !hasPerm(inst.Permissions, store.ScopeOrgExternalProperties, store.PermWrite) {
		store.WriteGHValidationError(w, "ExternalCustomPropertyInstallation", "installation_id", "invalid")
		return
	}
	reg, err := s.store.RegisterExternalPropertyInstallation(org.Login, inst.ID, *req.DisplayName)
	switch {
	case errors.Is(err, store.ErrExternalPropertyInstallationRegistered):
		store.WriteGHValidationError(w, "ExternalCustomPropertyInstallation", "installation_id", "already_exists")
		return
	case errors.Is(err, store.ErrExternalPropertyDisplayNameTaken):
		store.WriteGHValidationError(w, "ExternalCustomPropertyInstallation", "display_name", "already_exists")
		return
	}
	writeJSON(w, http.StatusCreated, externalPropertyInstallationJSON(reg))
}

func (s *Server) handleListExternalProperties(w http.ResponseWriter, r *http.Request, org string, inst *store.Installation) {
	out := []map[string]interface{}{}
	for _, name := range s.store.ExternalPropertyNames(org, inst.ID) {
		out = append(out, map[string]interface{}{"property_name": name})
	}
	writeJSON(w, http.StatusOK, out)
}

// externalPropertyValueValid admits what custom-property-value allows: a
// string, a list of strings, or null.
func externalPropertyValueValid(value interface{}) bool {
	switch v := value.(type) {
	case nil, string:
		return true
	case []interface{}:
		for _, item := range v {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}

func (s *Server) handleSetExternalPropertyValues(w http.ResponseWriter, r *http.Request, org string, inst *store.Installation) {
	var req struct {
		RepositoryNames []string                           `json:"repository_names"`
		Properties      []store.CustomPropertyValuePayload `json:"properties"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.RepositoryNames) == 0 || len(req.RepositoryNames) > 30 {
		store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "repository_names", "invalid")
		return
	}
	if req.Properties == nil {
		store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "properties", "missing_field")
		return
	}
	for _, p := range req.Properties {
		if !validCustomPropertyName(p.PropertyName) || !externalPropertyValueValid(p.Value) {
			store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "properties", "invalid")
			return
		}
	}
	var values []store.ExternalPropertyValue
	for _, name := range req.RepositoryNames {
		repo := s.store.GetRepo(org, name)
		if repo == nil {
			store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "repository_names", "invalid")
			return
		}
		for _, p := range req.Properties {
			values = append(values, store.ExternalPropertyValue{PropertyName: p.PropertyName, RepoID: repo.ID, Value: p.Value})
		}
	}
	s.store.SetExternalPropertyValues(org, inst.ID, values)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetExternalPropertyRepoValues(w http.ResponseWriter, r *http.Request, org string, inst *store.Installation) {
	name := r.PathValue("property_name")
	if !validCustomPropertyName(name) {
		store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "property_name", "invalid")
		return
	}
	var req struct {
		RepositoryValues []struct {
			RepositoryName *string     `json:"repository_name"`
			Value          interface{} `json:"value"`
		} `json:"repository_values"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.RepositoryValues) == 0 || len(req.RepositoryValues) > 100 {
		store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "repository_values", "invalid")
		return
	}
	values := make([]store.ExternalPropertyValue, 0, len(req.RepositoryValues))
	for _, entry := range req.RepositoryValues {
		if entry.RepositoryName == nil {
			store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "repository_name", "missing_field")
			return
		}
		if _, ok := entry.Value.(string); entry.Value != nil && !ok {
			store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "value", "invalid")
			return
		}
		repo := s.store.GetRepo(org, *entry.RepositoryName)
		if repo == nil {
			store.WriteGHValidationError(w, "ExternalCustomPropertyValues", "repository_name", "invalid")
			return
		}
		values = append(values, store.ExternalPropertyValue{PropertyName: name, RepoID: repo.ID, Value: entry.Value})
	}
	s.store.SetExternalPropertyValues(org, inst.ID, values)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteExternalProperty(w http.ResponseWriter, r *http.Request, org string, inst *store.Installation) {
	if !s.store.DeleteExternalProperty(org, inst.ID, r.PathValue("property_name")) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
