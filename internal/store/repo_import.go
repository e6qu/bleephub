package store

import "time"

// RepoImport backs the Source Import API (sunset on github.com, still real on
// GitHub Enterprise Server). The import is a real synchronous git fetch: PUT
// (and PATCH restarts) fetch vcs_url's refs into the repository's git storage.
// Status reflects what happened — "complete" only after a successful fetch,
// "auth_failed"/"error" on transport failure, and "error" for VCS types other
// than git, which bleephub cannot import.
type RepoImport struct {
	RepoID          int             `json:"repo_id"`
	VCS             string          `json:"vcs"` // empty until detected/declared
	VCSURL          string          `json:"vcs_url"`
	VCSUsername     string          `json:"vcs_username,omitempty"`
	VCSPassword     string          `json:"vcs_password,omitempty"`
	TFVCProject     string          `json:"tfvc_project,omitempty"`
	Status          string          `json:"status"`
	StatusText      string          `json:"status_text,omitempty"`
	FailedStep      string          `json:"failed_step,omitempty"`
	ErrorMessage    string          `json:"error_message,omitempty"`
	ImportPercent   *int            `json:"import_percent"`
	CommitCount     *int            `json:"commit_count"`
	AuthorsCount    *int            `json:"authors_count"`
	UseLFS          bool            `json:"use_lfs"`
	HasLargeFiles   bool            `json:"has_large_files"`
	LargeFilesSize  int             `json:"large_files_size"`
	LargeFilesCount int             `json:"large_files_count"`
	Authors         []*PorterAuthor `json:"authors"`
	NextAuthorID    int             `json:"next_author_id"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}
