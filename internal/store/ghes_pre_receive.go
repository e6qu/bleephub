package store

import "time"

type GHESPreReceiveEnvironment struct {
	ID                 int                     `json:"id"`
	Name               string                  `json:"name"`
	ImageURL           string                  `json:"image_url"`
	DefaultEnvironment bool                    `json:"default_environment"`
	CreatedAt          time.Time               `json:"created_at"`
	Download           *GHESPreReceiveDownload `json:"download,omitempty"`
}

type GHESPreReceiveHook struct {
	ID                           int    `json:"id"`
	Name                         string `json:"name"`
	Script                       string `json:"script"`
	ScriptRepositoryID           int    `json:"script_repository_id"`
	EnvironmentID                int    `json:"environment_id"`
	Enforcement                  string `json:"enforcement"`
	AllowDownstreamConfiguration bool   `json:"allow_downstream_configuration"`
}

type GHESPreReceiveOverride struct {
	Enforcement                  string `json:"enforcement"`
	AllowDownstreamConfiguration bool   `json:"allow_downstream_configuration"`
}

type GHESPreReceiveDownload struct {
	State        string     `json:"state"`
	DownloadedAt *time.Time `json:"downloaded_at,omitempty"`
	Message      *string    `json:"message,omitempty"`
}
