package bleephub

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// GitHub organization private registries: Dependabot registry credentials
// configured at the organization level. The secret value is sealed against
// the org public key and stored opaque, exactly like organization secrets.

func (s *Server) registerGHPrivateRegistryRoutes() {
	s.route("GET /api/v3/orgs/{org}/private-registries",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleListOrgPrivateRegistries)))
	s.route("POST /api/v3/orgs/{org}/private-registries",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleCreateOrgPrivateRegistry)))
	s.route("GET /api/v3/orgs/{org}/private-registries/public-key",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetOrgPrivateRegistriesPublicKey)))
	s.route("GET /api/v3/orgs/{org}/private-registries/{secret_name}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleGetOrgPrivateRegistry)))
	s.route("PATCH /api/v3/orgs/{org}/private-registries/{secret_name}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleUpdateOrgPrivateRegistry)))
	s.route("DELETE /api/v3/orgs/{org}/private-registries/{secret_name}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleDeleteOrgPrivateRegistry)))
}

var privateRegistryTypes = map[string]bool{
	"maven_repository": true, "nuget_feed": true, "goproxy_server": true,
	"npm_registry": true, "rubygems_server": true, "cargo_registry": true,
	"composer_repository": true, "docker_registry": true, "git_source": true,
	"helm_registry": true, "hex_organization": true, "hex_repository": true,
	"pub_repository": true, "python_index": true, "terraform_registry": true,
}

var privateRegistryAuthTypes = map[string]bool{
	"token": true, "username_password": true, "oidc_azure": true,
	"oidc_aws": true, "oidc_jfrog": true, "oidc_cloudsmith": true, "oidc_gcp": true,
}

func privateRegistryAuthIsOIDC(authType string) bool {
	return strings.HasPrefix(authType, "oidc_")
}

func (s *Server) handleListOrgPrivateRegistries(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	regs := s.store.ListPrivateRegistries(org)
	total := len(regs)
	regs = paginateAndLink(w, r, regs)
	out := make([]map[string]interface{}, 0, len(regs))
	for _, reg := range regs {
		out = append(out, privateRegistryJSON(reg, false))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":    total,
		"configurations": out,
	})
}

func (s *Server) handleCreateOrgPrivateRegistry(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req store.PrivateRegistryRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.RegistryType == nil || !privateRegistryTypes[*req.RegistryType] {
		store.WriteGHValidationError(w, "PrivateRegistry", "registry_type", "invalid")
		return
	}
	if req.URL == nil || *req.URL == "" {
		store.WriteGHValidationError(w, "PrivateRegistry", "url", "missing_field")
		return
	}
	if req.Visibility == nil || !validOrgItemVisibility(*req.Visibility) {
		store.WriteGHValidationError(w, "PrivateRegistry", "visibility", "invalid")
		return
	}
	authType := "token"
	if req.AuthType != nil {
		if !privateRegistryAuthTypes[*req.AuthType] {
			store.WriteGHValidationError(w, "PrivateRegistry", "auth_type", "invalid")
			return
		}
		authType = *req.AuthType
	}
	if privateRegistryAuthIsOIDC(authType) {
		if req.EncryptedValue != nil || req.KeyID != nil {
			store.WriteGHValidationError(w, "PrivateRegistry", "encrypted_value", "invalid")
			return
		}
	} else {
		if req.EncryptedValue == nil || *req.EncryptedValue == "" {
			writeGHError(w, http.StatusUnprocessableEntity, "encrypted_value is required")
			return
		}
		if _, err := base64.StdEncoding.DecodeString(*req.EncryptedValue); err != nil {
			store.WriteGHValidationError(w, "PrivateRegistry", "encrypted_value", "invalid")
			return
		}
		keyID := ""
		if req.KeyID != nil {
			keyID = *req.KeyID
		}
		if !s.validateDependabotSecretKeyID(w, keyID) {
			return
		}
	}
	if authType == "username_password" && (req.Username == nil || *req.Username == "") {
		writeGHError(w, http.StatusUnprocessableEntity, "username is required when auth_type is username_password")
		return
	}
	if len(req.SelectedRepositoryIDs) > 0 && *req.Visibility != "selected" {
		store.WriteGHValidationError(w, "PrivateRegistry", "selected_repository_ids", "invalid")
		return
	}
	for _, id := range req.SelectedRepositoryIDs {
		if s.store.GetRepoByID(id) == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	reg := s.store.CreatePrivateRegistry(org, &req, authType)
	writeJSON(w, http.StatusCreated, privateRegistryJSON(reg, true))
}

func (s *Server) handleGetOrgPrivateRegistriesPublicKey(w http.ResponseWriter, _ *http.Request) {
	s.writeActionsPublicKey(w)
}

func (s *Server) handleGetOrgPrivateRegistry(w http.ResponseWriter, r *http.Request) {
	reg := s.store.GetPrivateRegistry(r.PathValue("org"), r.PathValue("secret_name"))
	if reg == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, privateRegistryJSON(reg, false))
}

func (s *Server) handleUpdateOrgPrivateRegistry(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	reg := s.store.GetPrivateRegistry(org, r.PathValue("secret_name"))
	if reg == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req store.PrivateRegistryRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.RegistryType != nil && !privateRegistryTypes[*req.RegistryType] {
		store.WriteGHValidationError(w, "PrivateRegistry", "registry_type", "invalid")
		return
	}
	// The authentication type cannot change after creation; a provided value
	// must match the existing one.
	if req.AuthType != nil && *req.AuthType != reg.AuthType {
		store.WriteGHValidationError(w, "PrivateRegistry", "auth_type", "invalid")
		return
	}
	if req.Visibility != nil && !validOrgItemVisibility(*req.Visibility) {
		store.WriteGHValidationError(w, "PrivateRegistry", "visibility", "invalid")
		return
	}
	if req.EncryptedValue != nil {
		if privateRegistryAuthIsOIDC(reg.AuthType) {
			store.WriteGHValidationError(w, "PrivateRegistry", "encrypted_value", "invalid")
			return
		}
		if _, err := base64.StdEncoding.DecodeString(*req.EncryptedValue); err != nil {
			store.WriteGHValidationError(w, "PrivateRegistry", "encrypted_value", "invalid")
			return
		}
		keyID := ""
		if req.KeyID != nil {
			keyID = *req.KeyID
		}
		if !s.validateDependabotSecretKeyID(w, keyID) {
			return
		}
	}
	for _, id := range req.SelectedRepositoryIDs {
		if s.store.GetRepoByID(id) == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}
	s.store.UpdatePrivateRegistry(org, reg.Name, &req)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteOrgPrivateRegistry(w http.ResponseWriter, r *http.Request) {
	if !s.store.DeletePrivateRegistry(r.PathValue("org"), r.PathValue("secret_name")) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// privateRegistryJSON renders the org-private-registry-configuration shape;
// withSelected additionally carries selected_repository_ids (the create
// response shape). The sealed secret value is never emitted.
func privateRegistryJSON(reg *store.PrivateRegistryConfiguration, withSelected bool) map[string]interface{} {
	out := map[string]interface{}{
		"name":          reg.Name,
		"registry_type": reg.RegistryType,
		"auth_type":     reg.AuthType,
		"url":           reg.URL,
		"replaces_base": reg.ReplacesBase,
		"visibility":    reg.Visibility,
		"created_at":    reg.CreatedAt.Format(time.RFC3339),
		"updated_at":    reg.UpdatedAt.Format(time.RFC3339),
	}
	if reg.Username != nil {
		out["username"] = *reg.Username
	}
	if withSelected && reg.Visibility == "selected" {
		ids := reg.SelectedRepositoryIDs
		if ids == nil {
			ids = []int{}
		}
		out["selected_repository_ids"] = ids
	}
	for member, value := range map[string]string{
		"tenant_id":                  reg.TenantID,
		"client_id":                  reg.ClientID,
		"aws_region":                 reg.AWSRegion,
		"account_id":                 reg.AccountID,
		"role_name":                  reg.RoleName,
		"domain":                     reg.Domain,
		"domain_owner":               reg.DomainOwner,
		"jfrog_oidc_provider_name":   reg.JfrogOIDCProviderName,
		"audience":                   reg.Audience,
		"identity_mapping_name":      reg.IdentityMappingName,
		"namespace":                  reg.Namespace,
		"service_slug":               reg.ServiceSlug,
		"api_host":                   reg.APIHost,
		"workload_identity_provider": reg.WorkloadIdentityProvider,
		"service_account":            reg.ServiceAccount,
	} {
		if value != "" {
			out[member] = value
		}
	}
	return out
}

// --- store ---
