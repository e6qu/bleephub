package store

import (
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Organization artifact metadata: storage records (where an artifact lives) and
// deployment records (where it runs), keyed by subject digest.

// ArtifactStorageRecord records where a digest-identified artifact is stored.
type ArtifactStorageRecord struct {
	ID               int       `json:"id"`
	OrgID            int       `json:"org_id"`
	Name             string    `json:"name"`
	Digest           string    `json:"digest"`
	Version          string    `json:"version"`
	ArtifactURL      string    `json:"artifact_url"`
	Path             string    `json:"path"`
	RegistryURL      string    `json:"registry_url"`
	Repository       string    `json:"repository"`
	Status           string    `json:"status"` // active, eol, deleted
	GitHubRepository string    `json:"github_repository"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ArtifactDeploymentRecord records one deployment, identified by (logical env,
// physical env, cluster, deployment name); repeated posts update it in place.
type ArtifactDeploymentRecord struct {
	ID                  int               `json:"id"`
	OrgID               int               `json:"org_id"`
	Name                string            `json:"name"`
	Digest              string            `json:"digest"`
	Version             string            `json:"version"`
	Status              string            `json:"status"` // deployed, decommissioned
	LogicalEnvironment  string            `json:"logical_environment"`
	PhysicalEnvironment string            `json:"physical_environment"`
	Cluster             string            `json:"cluster"`
	DeploymentName      string            `json:"deployment_name"`
	Tags                map[string]string `json:"tags"`
	RuntimeRisks        []string          `json:"runtime_risks"`
	GitHubRepository    string            `json:"github_repository"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

// ArtifactDeploymentJob records one bulk cluster update. The batch is applied
// before the 202, so a new job is already completed; it persists to preserve
// GitHub's polling contract.
type ArtifactDeploymentJob struct {
	ID         int       `json:"job_id"`
	OrgID      int       `json:"org_id"`
	Cluster    string    `json:"cluster"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	TotalCount int       `json:"total_count"`
	Errors     []any     `json:"errors"`
}

var ArtifactDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// CreateArtifactStorageRecord appends a storage record for the org.
func (st *Store) CreateArtifactStorageRecord(rec *ArtifactStorageRecord) *ArtifactStorageRecord {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	rec.ID = st.NextArtifactStorageRecordID
	st.NextArtifactStorageRecordID++
	now := st.CurrentTime()
	rec.CreatedAt = now
	rec.UpdatedAt = now
	st.ArtifactStorageRecords[rec.ID] = rec
	if st.Persist != nil {
		st.Persist.MustPut("artifact_storage_records", strconv.Itoa(rec.ID), rec)
	}
	return rec
}

// ListArtifactStorageRecords returns the org's storage records for a
// digest (any digest when empty), ascending by ID.
func (st *Store) ListArtifactStorageRecords(orgID int, digest string) []*ArtifactStorageRecord {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*ArtifactStorageRecord, 0)
	for _, rec := range st.ArtifactStorageRecords {
		if rec.OrgID != orgID {
			continue
		}
		if digest != "" && rec.Digest != digest {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

// UpsertArtifactDeploymentRecord creates or updates the deployment
// record identified by (org, logical env, physical env, cluster,
// deployment name).
func (st *Store) UpsertArtifactDeploymentRecord(rec *ArtifactDeploymentRecord) *ArtifactDeploymentRecord {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	for _, existing := range st.ArtifactDeploymentRecords {
		if existing.OrgID == rec.OrgID &&
			existing.LogicalEnvironment == rec.LogicalEnvironment &&
			existing.PhysicalEnvironment == rec.PhysicalEnvironment &&
			existing.Cluster == rec.Cluster &&
			existing.DeploymentName == rec.DeploymentName {
			existing.Name = rec.Name
			existing.Digest = rec.Digest
			existing.Version = rec.Version
			existing.Status = rec.Status
			existing.Tags = rec.Tags
			existing.RuntimeRisks = rec.RuntimeRisks
			existing.GitHubRepository = rec.GitHubRepository
			existing.UpdatedAt = now
			if st.Persist != nil {
				st.Persist.MustPut("artifact_deployment_records", strconv.Itoa(existing.ID), existing)
			}
			return existing
		}
	}
	rec.ID = st.NextArtifactDeploymentRecordID
	st.NextArtifactDeploymentRecordID++
	rec.CreatedAt = now
	rec.UpdatedAt = now
	st.ArtifactDeploymentRecords[rec.ID] = rec
	if st.Persist != nil {
		st.Persist.MustPut("artifact_deployment_records", strconv.Itoa(rec.ID), rec)
	}
	return rec
}

// ListArtifactDeploymentRecords returns the org's deployment records
// for a digest (any digest when empty), ascending by ID.
func (st *Store) ListArtifactDeploymentRecords(orgID int, digest string) []*ArtifactDeploymentRecord {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*ArtifactDeploymentRecord, 0)
	for _, rec := range st.ArtifactDeploymentRecords {
		if rec.OrgID != orgID {
			continue
		}
		if digest != "" && rec.Digest != digest {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotArtifactDeploymentRecords(out)
}

func (st *Store) CreateArtifactDeploymentJob(job *ArtifactDeploymentJob) *ArtifactDeploymentJob {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	job.ID = st.NextArtifactDeploymentJobID
	st.NextArtifactDeploymentJobID++
	if job.StartedAt.IsZero() {
		job.StartedAt = st.CurrentTime()
	}
	if job.Status == "" {
		job.Status = "completed"
	}
	if job.Errors == nil {
		job.Errors = []any{}
	}
	st.ArtifactDeploymentJobs[job.ID] = job
	if st.Persist != nil {
		st.Persist.MustPut("artifact_deployment_jobs", strconv.Itoa(job.ID), job)
	}
	return job
}

func (st *Store) GetArtifactDeploymentJob(orgID, id int, cluster string) *ArtifactDeploymentJob {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	job := st.ArtifactDeploymentJobs[id]
	if job == nil || job.OrgID != orgID || job.Cluster != cluster {
		return nil
	}
	copy := *job
	copy.Errors = append([]any{}, job.Errors...)
	return &copy
}
