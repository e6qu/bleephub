package store

import "time"

// HostedRunner is one GitHub-hosted runner (the actions-hosted-runner
// resource) configured in an org or enterprise.
type HostedRunner struct {
	ID               int        `json:"id"`
	Org              string     `json:"org"`
	Enterprise       string     `json:"enterprise,omitempty"`
	Name             string     `json:"name"`
	RunnerGroupID    int        `json:"runner_group_id"`
	ImageID          string     `json:"image_id"`
	ImageSource      string     `json:"image_source"` // github | partner | custom
	ImageVersion     string     `json:"image_version,omitempty"`
	ImageSizeGB      int        `json:"image_size_gb"`
	ImageDisplayName string     `json:"image_display_name"`
	Platform         string     `json:"platform"`
	MachineSizeID    string     `json:"machine_size_id"`
	MaximumRunners   int        `json:"maximum_runners"`
	PublicIPEnabled  bool       `json:"public_ip_enabled"`
	ImageGen         bool       `json:"image_gen"`
	LastActiveOn     *time.Time `json:"last_active_on,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

// HostedRunnerCustomImage is one custom runner image definition (the
// actions-hosted-runner-custom-image resource) with its versions.
type HostedRunnerCustomImage struct {
	ID         int                               `json:"id"`
	Org        string                            `json:"org"`
	Enterprise string                            `json:"enterprise,omitempty"`
	Name       string                            `json:"name"`
	Platform   string                            `json:"platform"`
	State      string                            `json:"state"`
	Versions   []*HostedRunnerCustomImageVersion `json:"versions"`
}
