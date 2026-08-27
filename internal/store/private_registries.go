package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PrivateRegistryConfiguration is an org private registry configuration.
type PrivateRegistryConfiguration struct {
	Name                     string    `json:"name"`
	RegistryType             string    `json:"registry_type"`
	AuthType                 string    `json:"auth_type"`
	URL                      string    `json:"url"`
	Username                 *string   `json:"username"`
	ReplacesBase             bool      `json:"replaces_base"`
	Visibility               string    `json:"visibility"`
	SelectedRepositoryIDs    []int     `json:"selected_repository_ids"`
	EncryptedValue           string    `json:"-"` // opaque sealed box; never emitted
	KeyID                    string    `json:"key_id"`
	TenantID                 string    `json:"tenant_id"`
	ClientID                 string    `json:"client_id"`
	AWSRegion                string    `json:"aws_region"`
	AccountID                string    `json:"account_id"`
	RoleName                 string    `json:"role_name"`
	Domain                   string    `json:"domain"`
	DomainOwner              string    `json:"domain_owner"`
	JfrogOIDCProviderName    string    `json:"jfrog_oidc_provider_name"`
	Audience                 string    `json:"audience"`
	IdentityMappingName      string    `json:"identity_mapping_name"`
	Namespace                string    `json:"namespace"`
	ServiceSlug              string    `json:"service_slug"`
	APIHost                  string    `json:"api_host"`
	WorkloadIdentityProvider string    `json:"workload_identity_provider"`
	ServiceAccount           string    `json:"service_account"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

// privateRegistryConfigurationPersist mirrors PrivateRegistryConfiguration but
// serializes EncryptedValue so persistence round-trips it (the API struct's
// json:"-" keeps it out of responses).
type privateRegistryConfigurationPersist struct {
	Name                     string    `json:"name"`
	RegistryType             string    `json:"registry_type"`
	AuthType                 string    `json:"auth_type"`
	URL                      string    `json:"url"`
	Username                 *string   `json:"username"`
	ReplacesBase             bool      `json:"replaces_base"`
	Visibility               string    `json:"visibility"`
	SelectedRepositoryIDs    []int     `json:"selected_repository_ids"`
	EncryptedValue           string    `json:"encrypted_value"`
	KeyID                    string    `json:"key_id"`
	TenantID                 string    `json:"tenant_id"`
	ClientID                 string    `json:"client_id"`
	AWSRegion                string    `json:"aws_region"`
	AccountID                string    `json:"account_id"`
	RoleName                 string    `json:"role_name"`
	Domain                   string    `json:"domain"`
	DomainOwner              string    `json:"domain_owner"`
	JfrogOIDCProviderName    string    `json:"jfrog_oidc_provider_name"`
	Audience                 string    `json:"audience"`
	IdentityMappingName      string    `json:"identity_mapping_name"`
	Namespace                string    `json:"namespace"`
	ServiceSlug              string    `json:"service_slug"`
	APIHost                  string    `json:"api_host"`
	WorkloadIdentityProvider string    `json:"workload_identity_provider"`
	ServiceAccount           string    `json:"service_account"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

func privateRegistryFromPersist(p *privateRegistryConfigurationPersist) *PrivateRegistryConfiguration {
	return &PrivateRegistryConfiguration{
		Name:                     p.Name,
		RegistryType:             p.RegistryType,
		AuthType:                 p.AuthType,
		URL:                      p.URL,
		Username:                 p.Username,
		ReplacesBase:             p.ReplacesBase,
		Visibility:               p.Visibility,
		SelectedRepositoryIDs:    p.SelectedRepositoryIDs,
		EncryptedValue:           p.EncryptedValue,
		KeyID:                    p.KeyID,
		TenantID:                 p.TenantID,
		ClientID:                 p.ClientID,
		AWSRegion:                p.AWSRegion,
		AccountID:                p.AccountID,
		RoleName:                 p.RoleName,
		Domain:                   p.Domain,
		DomainOwner:              p.DomainOwner,
		JfrogOIDCProviderName:    p.JfrogOIDCProviderName,
		Audience:                 p.Audience,
		IdentityMappingName:      p.IdentityMappingName,
		Namespace:                p.Namespace,
		ServiceSlug:              p.ServiceSlug,
		APIHost:                  p.APIHost,
		WorkloadIdentityProvider: p.WorkloadIdentityProvider,
		ServiceAccount:           p.ServiceAccount,
		CreatedAt:                p.CreatedAt,
		UpdatedAt:                p.UpdatedAt,
	}
}

// ListPrivateRegistries returns the org's registries sorted by name.
func (st *Store) ListPrivateRegistries(orgLogin string) []*PrivateRegistryConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	m := st.OrgPrivateRegistries[orgLogin]
	out := make([]*PrivateRegistryConfiguration, 0, len(m))
	for _, reg := range m {
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return snapshotPrivateRegistryConfigurations(out)
}

// GetPrivateRegistry returns a registry by configuration name, or nil.
func (st *Store) GetPrivateRegistry(orgLogin, name string) *PrivateRegistryConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.OrgPrivateRegistries[orgLogin][name]
}

// PersistPrivateRegistries saves the org's registry map via the persist shape,
// which serializes EncryptedValue.
func (st *Store) PersistPrivateRegistries(orgLogin string) {
	if st.Persist == nil {
		return
	}
	m := st.OrgPrivateRegistries[orgLogin]
	if len(m) == 0 {
		st.Persist.MustDelete("org_private_registries", orgLogin)
		return
	}
	out := make(map[string]*privateRegistryConfigurationPersist, len(m))
	for name, reg := range m {
		out[name] = privateRegistryToPersist(reg)
	}
	st.Persist.MustPut("org_private_registries", orgLogin, out)
}

// CreatePrivateRegistry materializes a configuration, naming it from the
// registry type as GitHub does (MAVEN_REPOSITORY_SECRET, ...), suffixed on collision.
func (st *Store) CreatePrivateRegistry(orgLogin string, req *PrivateRegistryRequest, authType string) *PrivateRegistryConfiguration {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgPrivateRegistries[orgLogin] == nil {
		st.OrgPrivateRegistries[orgLogin] = map[string]*PrivateRegistryConfiguration{}
	}
	base := strings.ToUpper(*req.RegistryType) + "_SECRET"
	name := base
	for i := 2; st.OrgPrivateRegistries[orgLogin][name] != nil; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	now := time.Now().UTC()
	reg := &PrivateRegistryConfiguration{
		Name:      name,
		AuthType:  authType,
		CreatedAt: now,
		UpdatedAt: now,
	}
	applyPrivateRegistryRequest(reg, req)
	st.OrgPrivateRegistries[orgLogin][name] = reg
	st.PersistPrivateRegistries(orgLogin)
	return reg
}

// UpdatePrivateRegistry applies the request to an existing configuration.
func (st *Store) UpdatePrivateRegistry(orgLogin, name string, req *PrivateRegistryRequest) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	reg := st.OrgPrivateRegistries[orgLogin][name]
	if reg == nil {
		return
	}
	applyPrivateRegistryRequest(reg, req)
	reg.UpdatedAt = time.Now().UTC()
	st.PersistPrivateRegistries(orgLogin)
}

// DeletePrivateRegistry removes a configuration, returning true when it existed.
func (st *Store) DeletePrivateRegistry(orgLogin, name string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgPrivateRegistries[orgLogin][name] == nil {
		return false
	}
	delete(st.OrgPrivateRegistries[orgLogin], name)
	st.PersistPrivateRegistries(orgLogin)
	return true
}

// applyPrivateRegistryRequest copies every provided member onto the configuration.
func applyPrivateRegistryRequest(reg *PrivateRegistryConfiguration, req *PrivateRegistryRequest) {
	if req.RegistryType != nil {
		reg.RegistryType = *req.RegistryType
	}
	if req.URL != nil {
		reg.URL = *req.URL
	}
	if req.Username != nil {
		reg.Username = req.Username
	}
	if req.ReplacesBase != nil {
		reg.ReplacesBase = *req.ReplacesBase
	}
	if req.EncryptedValue != nil {
		reg.EncryptedValue = *req.EncryptedValue
	}
	if req.KeyID != nil {
		reg.KeyID = *req.KeyID
	}
	if req.Visibility != nil {
		reg.Visibility = *req.Visibility
		if *req.Visibility != "selected" {
			reg.SelectedRepositoryIDs = nil
		}
	}
	if req.SelectedRepositoryIDs != nil {
		reg.SelectedRepositoryIDs = req.SelectedRepositoryIDs
	}
	for dst, src := range map[*string]*string{
		&reg.TenantID:                 req.TenantID,
		&reg.ClientID:                 req.ClientID,
		&reg.AWSRegion:                req.AWSRegion,
		&reg.AccountID:                req.AccountID,
		&reg.RoleName:                 req.RoleName,
		&reg.Domain:                   req.Domain,
		&reg.DomainOwner:              req.DomainOwner,
		&reg.JfrogOIDCProviderName:    req.JfrogOIDCProviderName,
		&reg.Audience:                 req.Audience,
		&reg.IdentityMappingName:      req.IdentityMappingName,
		&reg.Namespace:                req.Namespace,
		&reg.ServiceSlug:              req.ServiceSlug,
		&reg.APIHost:                  req.APIHost,
		&reg.WorkloadIdentityProvider: req.WorkloadIdentityProvider,
		&reg.ServiceAccount:           req.ServiceAccount,
	} {
		if src != nil {
			*dst = *src
		}
	}
}

type PrivateRegistryRequest struct {
	RegistryType             *string `json:"registry_type"`
	URL                      *string `json:"url"`
	Username                 *string `json:"username"`
	ReplacesBase             *bool   `json:"replaces_base"`
	EncryptedValue           *string `json:"encrypted_value"`
	KeyID                    *string `json:"key_id"`
	Visibility               *string `json:"visibility"`
	SelectedRepositoryIDs    []int   `json:"selected_repository_ids"`
	AuthType                 *string `json:"auth_type"`
	TenantID                 *string `json:"tenant_id"`
	ClientID                 *string `json:"client_id"`
	AWSRegion                *string `json:"aws_region"`
	AccountID                *string `json:"account_id"`
	RoleName                 *string `json:"role_name"`
	Domain                   *string `json:"domain"`
	DomainOwner              *string `json:"domain_owner"`
	JfrogOIDCProviderName    *string `json:"jfrog_oidc_provider_name"`
	Audience                 *string `json:"audience"`
	IdentityMappingName      *string `json:"identity_mapping_name"`
	Namespace                *string `json:"namespace"`
	ServiceSlug              *string `json:"service_slug"`
	APIHost                  *string `json:"api_host"`
	WorkloadIdentityProvider *string `json:"workload_identity_provider"`
	ServiceAccount           *string `json:"service_account"`
}

func privateRegistryToPersist(reg *PrivateRegistryConfiguration) *privateRegistryConfigurationPersist {
	return &privateRegistryConfigurationPersist{
		Name:                     reg.Name,
		RegistryType:             reg.RegistryType,
		AuthType:                 reg.AuthType,
		URL:                      reg.URL,
		Username:                 reg.Username,
		ReplacesBase:             reg.ReplacesBase,
		Visibility:               reg.Visibility,
		SelectedRepositoryIDs:    reg.SelectedRepositoryIDs,
		EncryptedValue:           reg.EncryptedValue,
		KeyID:                    reg.KeyID,
		TenantID:                 reg.TenantID,
		ClientID:                 reg.ClientID,
		AWSRegion:                reg.AWSRegion,
		AccountID:                reg.AccountID,
		RoleName:                 reg.RoleName,
		Domain:                   reg.Domain,
		DomainOwner:              reg.DomainOwner,
		JfrogOIDCProviderName:    reg.JfrogOIDCProviderName,
		Audience:                 reg.Audience,
		IdentityMappingName:      reg.IdentityMappingName,
		Namespace:                reg.Namespace,
		ServiceSlug:              reg.ServiceSlug,
		APIHost:                  reg.APIHost,
		WorkloadIdentityProvider: reg.WorkloadIdentityProvider,
		ServiceAccount:           reg.ServiceAccount,
		CreatedAt:                reg.CreatedAt,
		UpdatedAt:                reg.UpdatedAt,
	}
}
