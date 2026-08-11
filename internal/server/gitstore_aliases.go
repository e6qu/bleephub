package bleephub

import (
	"github.com/e6qu/bleephub/internal/gitstore"
)

// ARCH-001: the git data layer moved to internal/gitstore. These aliases keep
// the server package's historical names so the existing call sites (and the
// ref-CAS source contract test, which checks handlers call these helpers by
// name) stay unchanged. Types are true aliases; functions are forwarded
// through vars so call syntax is identical.
type s3FS = gitstore.S3FS

var (
	// GitDataDir and IsS3GitStorage are the storage-configuration probes the
	// server consults at startup and per request.
	GitDataDir     = gitstore.GitDataDir
	IsS3GitStorage = gitstore.IsS3GitStorage

	newS3FS                        = gitstore.NewS3FS
	getS3FS                        = gitstore.GetS3FS
	openOrInitGitStorage           = gitstore.OpenOrInitGitStorage
	validateRepoStorageFullName    = gitstore.ValidateRepoStorageFullName
	repoGitDirPath                 = gitstore.RepoGitDirPath
	initializeRepositoryReferences = gitstore.InitializeRepositoryReferences
	createReferenceIfAbsent        = gitstore.CreateReferenceIfAbsent
	removeReferenceCAS             = gitstore.RemoveReferenceCAS
	setGitObjectLocker             = gitstore.SetGitObjectLocker

	errReferenceAlreadyExists = gitstore.ErrReferenceAlreadyExists
)
