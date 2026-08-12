package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// NetworkConfiguration is a hosted compute network configuration.
type NetworkConfiguration struct {
	ID                         string    `json:"id"`
	OrgLogin                   string    `json:"org_login"`
	Name                       string    `json:"name"`
	ComputeService             string    `json:"compute_service"`
	NetworkSettingsIDs         []string  `json:"network_settings_ids"`
	FailoverNetworkSettingsIDs []string  `json:"failover_network_settings_ids"`
	FailoverNetworkEnabled     bool      `json:"failover_network_enabled"`
	CreatedOn                  time.Time `json:"created_on"`
}

// NetworkSettingsResource is a hosted compute network settings resource.
type NetworkSettingsResource struct {
	ID                     string `json:"id"`
	OrgLogin               string `json:"org_login"`
	Name                   string `json:"name"`
	SubnetID               string `json:"subnet_id"`
	Region                 string `json:"region"`
	NetworkConfigurationID string `json:"network_configuration_id"`
}

// ListNetworkConfigurations returns the org's configurations sorted by
// creation time then ID.
func (st *Store) ListNetworkConfigurations(orgLogin string) []*NetworkConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	m := st.OrgNetworkConfigurations[orgLogin]
	out := make([]*NetworkConfiguration, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedOn.Equal(out[j].CreatedOn) {
			return out[i].CreatedOn.Before(out[j].CreatedOn)
		}
		return out[i].ID < out[j].ID
	})
	return snapshotNetworkConfigurations(out)
}

// GetNetworkConfiguration returns a configuration by ID, or nil.
func (st *Store) GetNetworkConfiguration(orgLogin, id string) *NetworkConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.OrgNetworkConfigurations[orgLogin][id]
}

// relinkNetworkSettingsLocked points the settings resources the
// configuration references back at it, and clears stale back-references.
func (st *Store) relinkNetworkSettingsLocked(orgLogin string, c *NetworkConfiguration) {
	linked := map[string]bool{}
	for _, id := range c.NetworkSettingsIDs {
		linked[id] = true
	}
	for _, id := range c.FailoverNetworkSettingsIDs {
		linked[id] = true
	}
	for id, res := range st.OrgNetworkSettings[orgLogin] {
		switch {
		case linked[id]:
			res.NetworkConfigurationID = c.ID
		case res.NetworkConfigurationID == c.ID:
			res.NetworkConfigurationID = ""
		}
	}
	if st.Persist != nil {
		st.Persist.MustPut("org_network_settings", orgLogin, st.OrgNetworkSettings[orgLogin])
	}
}

// CreateNetworkConfiguration creates a configuration and links its settings
// resources.
func (st *Store) CreateNetworkConfiguration(orgLogin string, req *NetworkConfigurationRequest) (*NetworkConfiguration, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	id, err := newHostedComputeID()
	if err != nil {
		return nil, err
	}
	c := &NetworkConfiguration{
		ID:                         id,
		OrgLogin:                   orgLogin,
		Name:                       *req.Name,
		ComputeService:             "none",
		NetworkSettingsIDs:         req.NetworkSettingsIDs,
		FailoverNetworkSettingsIDs: req.FailoverNetworkSettingsIDs,
		CreatedOn:                  st.CurrentTime(),
	}
	if req.ComputeService != nil {
		c.ComputeService = *req.ComputeService
	}
	if req.FailoverNetworkEnabled != nil {
		c.FailoverNetworkEnabled = *req.FailoverNetworkEnabled
	}
	if st.OrgNetworkConfigurations[orgLogin] == nil {
		st.OrgNetworkConfigurations[orgLogin] = map[string]*NetworkConfiguration{}
	}
	st.OrgNetworkConfigurations[orgLogin][c.ID] = c
	st.relinkNetworkSettingsLocked(orgLogin, c)
	if st.Persist != nil {
		st.Persist.MustPut("org_network_configurations", orgLogin, st.OrgNetworkConfigurations[orgLogin])
	}
	return c, nil
}

// UpdateNetworkConfiguration applies provided members and relinks settings.
func (st *Store) UpdateNetworkConfiguration(orgLogin, id string, req *NetworkConfigurationRequest) *NetworkConfiguration {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c := st.OrgNetworkConfigurations[orgLogin][id]
	if c == nil {
		return nil
	}
	if req.Name != nil {
		c.Name = *req.Name
	}
	if req.ComputeService != nil {
		c.ComputeService = *req.ComputeService
	}
	if req.NetworkSettingsIDs != nil {
		c.NetworkSettingsIDs = req.NetworkSettingsIDs
	}
	if req.FailoverNetworkSettingsIDs != nil {
		c.FailoverNetworkSettingsIDs = req.FailoverNetworkSettingsIDs
	}
	if req.FailoverNetworkEnabled != nil {
		c.FailoverNetworkEnabled = *req.FailoverNetworkEnabled
	}
	st.relinkNetworkSettingsLocked(orgLogin, c)
	if st.Persist != nil {
		st.Persist.MustPut("org_network_configurations", orgLogin, st.OrgNetworkConfigurations[orgLogin])
	}
	return c
}

// DeleteNetworkConfiguration removes a configuration and unlinks its
// settings resources. Returns true when it existed.
func (st *Store) DeleteNetworkConfiguration(orgLogin, id string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c := st.OrgNetworkConfigurations[orgLogin][id]
	if c == nil {
		return false
	}
	delete(st.OrgNetworkConfigurations[orgLogin], id)
	for _, res := range st.OrgNetworkSettings[orgLogin] {
		if res.NetworkConfigurationID == id {
			res.NetworkConfigurationID = ""
		}
	}
	// One transaction: dropping the configuration and detaching it from every
	// settings resource commit together, so a crash cannot leave a settings
	// resource pointing at a deleted configuration (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("org_network_configurations", orgLogin, st.OrgNetworkConfigurations[orgLogin])
	batch.Put("org_network_settings", orgLogin, st.OrgNetworkSettings[orgLogin])
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "org_network_configurations", Err: err})
	}
	return true
}

// GetNetworkSettings returns a settings resource by ID, or nil.
func (st *Store) GetNetworkSettings(orgLogin, id string) *NetworkSettingsResource {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.OrgNetworkSettings[orgLogin][id]
}

// CreateNetworkSettings provisions a settings resource for the org.
func (st *Store) CreateNetworkSettings(orgLogin, name, subnetID, region string) (*NetworkSettingsResource, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	id, err := newHostedComputeID()
	if err != nil {
		return nil, err
	}
	res := &NetworkSettingsResource{
		ID:       id,
		OrgLogin: orgLogin,
		Name:     name,
		SubnetID: subnetID,
		Region:   region,
	}
	if st.OrgNetworkSettings[orgLogin] == nil {
		st.OrgNetworkSettings[orgLogin] = map[string]*NetworkSettingsResource{}
	}
	st.OrgNetworkSettings[orgLogin][res.ID] = res
	if st.Persist != nil {
		st.Persist.MustPut("org_network_settings", orgLogin, st.OrgNetworkSettings[orgLogin])
	}
	return res, nil
}

type NetworkConfigurationRequest struct {
	Name                       *string  `json:"name"`
	ComputeService             *string  `json:"compute_service"`
	NetworkSettingsIDs         []string `json:"network_settings_ids"`
	FailoverNetworkSettingsIDs []string `json:"failover_network_settings_ids"`
	FailoverNetworkEnabled     *bool    `json:"failover_network_enabled"`
}

// newHostedComputeID mints the uppercase-hex resource IDs hosted compute
// networking uses.
func newHostedComputeID() (string, error) {
	return NewHostedComputeIDFromReader(rand.Reader)
}

func NewHostedComputeIDFromReader(random io.Reader) (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("generate hosted compute resource id: %w", err)
	}
	return strings.ToUpper(hex.EncodeToString(buf)), nil
}
