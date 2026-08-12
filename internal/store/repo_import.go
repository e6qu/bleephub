package store

import "time"

// Source Import API (sunset on github.com, still real on GitHub Enterprise
// Server).
// Endpoints:
//
//	GET/PUT/PATCH/DELETE /repos/{o}/{r}/import
//	PATCH /repos/{o}/{r}/import/lfs
//	GET   /repos/{o}/{r}/import/authors
//	PATCH /repos/{o}/{r}/import/authors/{author_id}
//	GET   /repos/{o}/{r}/import/large_files
//
// The import is a real git fetch: PUT (and PATCH restarts) fetch vcs_url's
// refs into the repository's git storage over any transport go-git speaks,
// synchronously. The status field reflects what actually happened —
// "complete" only after a successful fetch, "auth_failed"/"error" with the
// transport's failure otherwise, and "error" with an explanatory message
// for VCS types bleephub cannot really import (everything but git).
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
