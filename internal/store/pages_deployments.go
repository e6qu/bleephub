package store

import (
	"strconv"
	"time"
)

// PagesDeploymentRecord is one Pages deployment. The publish is synchronous
// (no CDN tier), so a stored deployment is already terminal ("succeed") and
// cancellation, which needs a non-terminal deployment, is never observable.
type PagesDeploymentRecord struct {
	ID           int       `json:"id"`
	RepoID       int       `json:"repo_id"`
	Status       string    `json:"status"`
	Environment  string    `json:"environment"`
	BuildVersion string    `json:"pages_build_version"`
	ArtifactSize int64     `json:"artifact_size"`
	ArtifactSHA  string    `json:"artifact_sha256"`
	ArtifactKey  string    `json:"artifact_object_key"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CreatePagesDeployment records a Pages deployment for a repository.
func (st *Store) CreatePagesDeployment(repoID int, environment, buildVersion, status string, artifactSize int64, artifactSHA, artifactKey string) *PagesDeploymentRecord {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := time.Now().UTC()
	d := &PagesDeploymentRecord{
		ID:           st.NextPagesDeploymentID,
		RepoID:       repoID,
		Status:       status,
		Environment:  environment,
		BuildVersion: buildVersion,
		ArtifactSize: artifactSize,
		ArtifactSHA:  artifactSHA,
		ArtifactKey:  artifactKey,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	st.NextPagesDeploymentID++
	if st.PagesDeployments[repoID] == nil {
		st.PagesDeployments[repoID] = map[int]*PagesDeploymentRecord{}
	}
	st.PagesDeployments[repoID][d.ID] = d
	if st.Persist != nil {
		st.Persist.MustPut("pages_deployments", strconv.Itoa(repoID), st.PagesDeployments[repoID])
	}
	return d
}

// GetPagesDeployment returns a Pages deployment by repo and ID, or nil.
func (st *Store) GetPagesDeployment(repoID, id int) *PagesDeploymentRecord {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.PagesDeployments[repoID][id]
}

// GetPagesDeploymentByIdentifier returns a Pages deployment by its internal
// numeric record ID or by GitHub's public pages_build_version identifier.
func (st *Store) GetPagesDeploymentByIdentifier(repoID int, ident string) *PagesDeploymentRecord {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	byID := st.PagesDeployments[repoID]
	if byID == nil {
		return nil
	}
	if id, err := strconv.Atoi(ident); err == nil {
		if d := byID[id]; d != nil {
			return d
		}
	}
	for _, d := range byID {
		if d.BuildVersion == ident {
			return d
		}
	}
	return nil
}

// SetPagesDeploymentStatus transitions a deployment's status, or false if it
// does not exist.
func (st *Store) SetPagesDeploymentStatus(repoID, id int, status string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	d := st.PagesDeployments[repoID][id]
	if d == nil {
		return false
	}
	d.Status = status
	d.UpdatedAt = time.Now().UTC()
	if st.Persist != nil {
		st.Persist.MustPut("pages_deployments", strconv.Itoa(repoID), st.PagesDeployments[repoID])
	}
	return true
}
