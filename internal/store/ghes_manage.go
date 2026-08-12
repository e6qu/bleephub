package store

type GHESManagementState struct {
	SSHKeys      []string                 `json:"ssh_keys"`
	Settings     map[string]interface{}   `json:"settings"`
	License      map[string]interface{}   `json:"license,omitempty"`
	Maintenance  GHESMaintenanceState     `json:"maintenance"`
	ConfigStatus string                   `json:"config_status"`
	ConfigRunID  string                   `json:"config_run_id,omitempty"`
	ConfigEvents []map[string]interface{} `json:"config_events,omitempty"`
	Initialized  bool                     `json:"initialized"`
}

func defaultGHESManagementState() *GHESManagementState {
	return &GHESManagementState{
		SSHKeys: []string{},
		Settings: map[string]interface{}{
			"private_mode": true, "public_pages": false, "subdomain_isolation": true,
			"signup_enabled": false, "github_hostname": "bleephub.local",
			"auth_mode": "default", "expire_sessions": false,
		},
		Maintenance:  GHESMaintenanceState{IPExceptionList: []string{}},
		ConfigStatus: "idle",
		ConfigEvents: []map[string]interface{}{},
	}
}

type GHESMaintenanceState struct {
	Enabled                bool     `json:"enabled"`
	ScheduledTime          string   `json:"scheduled_time,omitempty"`
	IPExceptionList        []string `json:"ip_exception_list"`
	MaintenanceModeMessage string   `json:"maintenance_mode_message"`
}
