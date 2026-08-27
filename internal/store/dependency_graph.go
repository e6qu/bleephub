package store

import (
	"strconv"
	"time"

	"github.com/google/uuid"
)

// DependencySnapshot is a submitted dependency snapshot, mirroring the
// dependency submission API's snapshot object.
type DependencySnapshot struct {
	ID        int                          `json:"id"`
	RepoID    int                          `json:"repo_id"`
	Version   int                          `json:"version"`
	Ref       string                       `json:"ref"`
	Sha       string                       `json:"sha"`
	Job       SnapshotJob                  `json:"job"`
	Detector  SnapshotDetector             `json:"detector"`
	Scanned   string                       `json:"scanned"`
	Manifests map[string]*SnapshotManifest `json:"manifests,omitempty"`
	// Result is SUCCESS, ACCEPTED, or INVALID. An INVALID snapshot is stored
	// but never contributes to the repository's dependency set.
	Result    string    `json:"result"`
	CreatedAt time.Time `json:"created_at"`
}

type SnapshotManifest struct {
	Name string `json:"name"`
	File *struct {
		SourceLocation string `json:"source_location"`
	} `json:"file,omitempty"`
	Resolved map[string]*SnapshotDependency `json:"resolved,omitempty"`
}

// SBOMExport is a generated SBOM report export addressed by UUID.
type SBOMExport struct {
	UUID      string    `json:"uuid"`
	RepoID    int       `json:"repo_id"`
	CreatedAt time.Time `json:"created_at"`
}

// AddDependencySnapshot appends a snapshot for the repository.
func (st *Store) AddDependencySnapshot(snap *DependencySnapshot) *DependencySnapshot {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	snap.ID = st.NextDependencySnapshotID
	st.NextDependencySnapshotID++
	snap.CreatedAt = time.Now().UTC()
	st.DependencySnapshots[snap.RepoID] = append(st.DependencySnapshots[snap.RepoID], snap)
	if st.Persist != nil {
		st.Persist.MustPut("dependency_snapshots", strconv.Itoa(snap.RepoID), st.DependencySnapshots[snap.RepoID])
	}
	return snap
}

// ListDependencySnapshots returns the repo's snapshots, oldest first.
func (st *Store) ListDependencySnapshots(repoID int) []*DependencySnapshot {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*DependencySnapshot, len(st.DependencySnapshots[repoID]))
	copy(out, st.DependencySnapshots[repoID])
	return snapshotDependencySnapshots(out)
}

// AddSBOMExport records a generated SBOM export.
func (st *Store) AddSBOMExport(repoID int) *SBOMExport {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	exp := &SBOMExport{
		UUID:      uuid.New().String(),
		RepoID:    repoID,
		CreatedAt: time.Now().UTC(),
	}
	st.SBOMExports[exp.UUID] = exp
	if st.Persist != nil {
		st.Persist.MustPut("sbom_exports", exp.UUID, exp)
	}
	return exp
}

// GetSBOMExport returns an export by UUID, or nil.
func (st *Store) GetSBOMExport(uuid string) *SBOMExport {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.SBOMExports[uuid]
}

type SnapshotDependency struct {
	PackageURL   string   `json:"package_url"`
	Relationship string   `json:"relationship,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type SnapshotDetector struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"`
}

type SnapshotJob struct {
	ID         string `json:"id"`
	Correlator string `json:"correlator"`
	HTMLURL    string `json:"html_url,omitempty"`
}
