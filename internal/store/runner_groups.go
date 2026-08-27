package store

import "time"

// RunnerGroup models an organization or enterprise runner group. Scope is part
// of the persisted identity: ids are globally unique, so a group must never
// become visible through a different owner sharing the backing store.
type RunnerGroup struct {
	ID                       int         `json:"id"`
	Name                     string      `json:"name"`
	Visibility               string      `json:"visibility"` // all | selected | private
	Default                  bool        `json:"default"`
	AllowsPublicRepositories bool        `json:"allows_public_repositories"`
	SelectedRepoIDs          []int       `json:"selected_repository_ids,omitempty"`
	SelectedOrgIDs           []int       `json:"selected_organization_ids,omitempty"`
	RestrictedToWorkflows    bool        `json:"restricted_to_workflows,omitempty"`
	SelectedWorkflows        []string    `json:"selected_workflows,omitempty"`
	NetworkConfigurationID   string      `json:"network_configuration_id,omitempty"`
	Scope                    RunnerScope `json:"scope"`
	CreatedAt                time.Time   `json:"created_at"`
}
