package store

// GitHub Enterprise Importer (GEI): the entities behind GitHub's GraphQL
// migration surface (data moving *in*, versus the REST exports in
// store_migrations.go). Everything is keyed by owning organization, giving the
// tenant isolation the authorization rules depend on.
//
// STORE-021: every getter and List* returns a detached snapshot;
// Find*ByNodeID returns the LIVE row (the write path's lookup).

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Global-id prefixes for the GEI entities.
const (
	MigrationSourceNodeIDPrefix     = "MS_kgDO"
	RepositoryMigrationNodeIDPrefix = "RM_kgDO"
	OrgMigrationNodeIDPrefix        = "OM_kgDO"
)

// MigrationSourceType values — GitHub's MigrationSourceType enum.
const (
	MigrationSourceTypeAzureDevOps     = "AZURE_DEVOPS"
	MigrationSourceTypeBitbucketServer = "BITBUCKET_SERVER"
	MigrationSourceTypeGitHubArchive   = "GITHUB_ARCHIVE"
	MigrationSourceTypeGitLab          = "GITLAB"
)

// ValidMigrationSourceType reports whether value is a member of GitHub's
// MigrationSourceType enum.
func ValidMigrationSourceType(value string) bool {
	switch value {
	case MigrationSourceTypeAzureDevOps, MigrationSourceTypeBitbucketServer,
		MigrationSourceTypeGitHubArchive, MigrationSourceTypeGitLab:
		return true
	}
	return false
}

// GEIMigrationState values — GitHub's MigrationState enum. A repository
// migration goes QUEUED → IN_PROGRESS → SUCCEEDED/FAILED/FAILED_VALIDATION.
const (
	GEIMigrationStateNotStarted        = "NOT_STARTED"
	GEIMigrationStateQueued            = "QUEUED"
	GEIMigrationStatePendingValidation = "PENDING_VALIDATION"
	GEIMigrationStateFailedValidation  = "FAILED_VALIDATION"
	GEIMigrationStateInProgress        = "IN_PROGRESS"
	GEIMigrationStateSucceeded         = "SUCCEEDED"
	GEIMigrationStateFailed            = "FAILED"
)

// Extra states for an organization migration: OrganizationMigrationState is
// MigrationState plus these three phases.
const (
	OrgMigrationStatePreRepoMigration  = "PRE_REPO_MIGRATION"
	OrgMigrationStateRepoMigration     = "REPO_MIGRATION"
	OrgMigrationStatePostRepoMigration = "POST_REPO_MIGRATION"
)

// GEIMigrationTerminal reports whether a migration state admits no further
// transitions.
func GEIMigrationTerminal(state string) bool {
	switch state {
	case GEIMigrationStateSucceeded, GEIMigrationStateFailed, GEIMigrationStateFailedValidation:
		return true
	}
	return false
}

// MigrationSource is a place repositories are migrated from. Its stored
// credentials are never served — only the in-process migration worker reads
// them.
type MigrationSource struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
	// OwnerOrgID is the org every read/write of this source is authorized
	// against.
	OwnerOrgID int    `json:"owner_org_id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	URL        string `json:"url"`
	// AccessToken authenticates against the source; GitHubPAT against the
	// target for signed archive URLs.
	AccessToken string    `json:"access_token,omitempty"`
	GitHubPAT   string    `json:"github_pat,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RepositoryMigration is one repository coming across from a MigrationSource.
type RepositoryMigration struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
	// OwnerOrgID is the organization the imported repository lands in.
	OwnerOrgID     int    `json:"owner_org_id"`
	SourceID       int    `json:"source_id"`
	RepositoryName string `json:"repository_name"`
	SourceURL      string `json:"source_url"`
	State          string `json:"state"`
	FailureReason  string `json:"failure_reason,omitempty"`
	// WarningsCount is len(WarningLog): recoverable problems continued past.
	WarningsCount   int      `json:"warnings_count"`
	WarningLog      []string `json:"warning_log,omitempty"`
	ContinueOnError bool     `json:"continue_on_error"`
	LockSource      bool     `json:"lock_source"`
	SkipReleases    bool     `json:"skip_releases"`
	// TargetRepoVisibility is public/private/internal; empty keeps the
	// source's visibility, which for an un-interrogable source means private.
	TargetRepoVisibility string `json:"target_repo_visibility,omitempty"`
	GitArchiveURL        string `json:"git_archive_url,omitempty"`
	MetadataArchiveURL   string `json:"metadata_archive_url,omitempty"`
	// MigrationLogKey is empty until the migration reaches a terminal state.
	MigrationLogKey string `json:"migration_log_key,omitempty"`
	// OrgMigrationID links back to the org migration that fanned this out; 0
	// if standalone.
	OrgMigrationID int `json:"org_migration_id,omitempty"`
	// StartedByUserID owns everything the migration creates in the target.
	StartedByUserID int `json:"started_by_user_id,omitempty"`
	// TargetRepoID is the repository this migration created. A resumed
	// migration continues into that repo by ID, not by name: name-matching
	// would let someone pre-plant a repo under a queued migration's name and
	// receive its contents.
	TargetRepoID int `json:"target_repo_id,omitempty"`
	// SourceRepoLock is the full name of the repo on *this* instance that
	// lock_source froze, or "" when the source is elsewhere. Held until the
	// migration is terminal, so its state is the only thing that releases it.
	SourceRepoLock string    `json:"source_repo_lock,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// OrganizationMigration is a whole organization coming across, fanning out
// into repository migrations.
type OrganizationMigration struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
	// EnterpriseID is the enterprise the target organization belongs to.
	EnterpriseID  int    `json:"enterprise_id"`
	SourceOrgURL  string `json:"source_org_url"`
	SourceOrgName string `json:"source_org_name"`
	TargetOrgName string `json:"target_org_name"`
	// TargetOrgID is 0 until the target organization has been created.
	TargetOrgID   int    `json:"target_org_id,omitempty"`
	State         string `json:"state"`
	FailureReason string `json:"failure_reason,omitempty"`
	// TotalRepositoriesCount is nil until the source has been enumerated.
	TotalRepositoriesCount     *int   `json:"total_repositories_count,omitempty"`
	RemainingRepositoriesCount *int   `json:"remaining_repositories_count,omitempty"`
	SourceAccessToken          string `json:"source_access_token,omitempty"`
	// StartedByUserID owns the target org and everything created under it.
	StartedByUserID int       `json:"started_by_user_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// OrgMigratorRole is one grant of the org migrator role. Actor is a user login
// or team slug, per ActorType ("USER" | "TEAM").
type OrgMigratorRole struct {
	OrgID     int       `json:"org_id"`
	ActorType string    `json:"actor_type"`
	Actor     string    `json:"actor"`
	GrantedBy int       `json:"granted_by"`
	CreatedAt time.Time `json:"created_at"`
}

// OrgMigratorRoleKey is the map and persistence key for one grant.
func OrgMigratorRoleKey(orgID int, actorType, actor string) string {
	return strconv.Itoa(orgID) + "/" + strings.ToUpper(actorType) + "/" + strings.ToLower(actor)
}

// --- clones (STORE-021) ---

func cloneMigrationSource(src *MigrationSource) *MigrationSource {
	if src == nil {
		return nil
	}
	c := *src
	return &c
}

func cloneRepositoryMigration(m *RepositoryMigration) *RepositoryMigration {
	if m == nil {
		return nil
	}
	c := *m
	c.WarningLog = append([]string(nil), m.WarningLog...)
	return &c
}

func cloneOrganizationMigration(m *OrganizationMigration) *OrganizationMigration {
	if m == nil {
		return nil
	}
	c := *m
	c.TotalRepositoriesCount = copyIntPtr(m.TotalRepositoriesCount)
	c.RemainingRepositoriesCount = copyIntPtr(m.RemainingRepositoriesCount)
	return &c
}

func copyIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func cloneOrgMigratorRole(r *OrgMigratorRole) *OrgMigratorRole {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// --- persistence ---

func (st *Store) persistMigrationSourceLocked(src *MigrationSource) {
	if st.Persist != nil {
		st.Persist.MustPut("migration_sources", strconv.Itoa(src.ID), src)
	}
}

func (st *Store) persistRepositoryMigrationLocked(m *RepositoryMigration) {
	if st.Persist != nil {
		st.Persist.MustPut("repository_migrations", strconv.Itoa(m.ID), m)
	}
}

func (st *Store) persistOrganizationMigrationLocked(m *OrganizationMigration) {
	if st.Persist != nil {
		st.Persist.MustPut("organization_migrations", strconv.Itoa(m.ID), m)
	}
}

func (st *Store) persistOrgMigratorRoleLocked(r *OrgMigratorRole) {
	if st.Persist != nil {
		st.Persist.MustPut("org_migrator_roles", OrgMigratorRoleKey(r.OrgID, r.ActorType, r.Actor), r)
	}
}

// --- migration sources ---

// CreateMigrationSource records a place to migrate from and returns a detached
// snapshot.
func (st *Store) CreateMigrationSource(ownerOrgID int, name, sourceType, url, accessToken, githubPAT string) *MigrationSource {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	id := st.NextMigrationSourceID
	st.NextMigrationSourceID++
	src := &MigrationSource{
		ID:          id,
		NodeID:      fmt.Sprintf("%s%08d", MigrationSourceNodeIDPrefix, id),
		OwnerOrgID:  ownerOrgID,
		Name:        name,
		Type:        sourceType,
		URL:         url,
		AccessToken: accessToken,
		GitHubPAT:   githubPAT,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	st.MigrationSources[id] = src
	st.persistMigrationSourceLocked(src)
	return cloneMigrationSource(src)
}

// GetMigrationSource returns a detached snapshot by database id, or nil.
func (st *Store) GetMigrationSource(id int) *MigrationSource {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneMigrationSource(st.MigrationSources[id])
}

// FindMigrationSourceByNodeID resolves a source global id to the LIVE row.
func FindMigrationSourceByNodeID(st *Store, nodeID string) *MigrationSource {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, MigrationSourceNodeIDPrefix); ok {
		if src := st.MigrationSources[id]; src != nil && src.NodeID == nodeID {
			return src
		}
	}
	return nil
}

// ListMigrationSources returns detached snapshots of one organization's
// sources, ordered by database id.
func (st *Store) ListMigrationSources(ownerOrgID int) []*MigrationSource {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*MigrationSource
	for _, src := range st.MigrationSources {
		if src.OwnerOrgID == ownerOrgID {
			out = append(out, cloneMigrationSource(src))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// --- repository migrations ---

// NewRepositoryMigration is the create input for a repository migration.
type NewRepositoryMigration struct {
	OwnerOrgID           int
	SourceID             int
	RepositoryName       string
	SourceURL            string
	ContinueOnError      bool
	LockSource           bool
	SkipReleases         bool
	TargetRepoVisibility string
	GitArchiveURL        string
	MetadataArchiveURL   string
	OrgMigrationID       int
	StartedByUserID      int
}

// CreateRepositoryMigration queues a repository migration and returns a
// detached snapshot.
func (st *Store) CreateRepositoryMigration(in NewRepositoryMigration) *RepositoryMigration {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return cloneRepositoryMigration(st.createRepositoryMigrationLocked(in))
}

func (st *Store) createRepositoryMigrationLocked(in NewRepositoryMigration) *RepositoryMigration {
	now := st.CurrentTime()
	id := st.NextRepositoryMigrationID
	st.NextRepositoryMigrationID++
	m := &RepositoryMigration{
		ID:                   id,
		NodeID:               fmt.Sprintf("%s%08d", RepositoryMigrationNodeIDPrefix, id),
		OwnerOrgID:           in.OwnerOrgID,
		SourceID:             in.SourceID,
		RepositoryName:       in.RepositoryName,
		SourceURL:            in.SourceURL,
		State:                GEIMigrationStateQueued,
		ContinueOnError:      in.ContinueOnError,
		LockSource:           in.LockSource,
		SkipReleases:         in.SkipReleases,
		TargetRepoVisibility: in.TargetRepoVisibility,
		GitArchiveURL:        in.GitArchiveURL,
		MetadataArchiveURL:   in.MetadataArchiveURL,
		OrgMigrationID:       in.OrgMigrationID,
		StartedByUserID:      in.StartedByUserID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	st.RepositoryMigrations[id] = m
	st.persistRepositoryMigrationLocked(m)
	return m
}

// GetRepositoryMigration returns a detached snapshot by database id, or nil.
func (st *Store) GetRepositoryMigration(id int) *RepositoryMigration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneRepositoryMigration(st.RepositoryMigrations[id])
}

// FindRepositoryMigrationByNodeID resolves a repository migration global id to
// the LIVE row.
func FindRepositoryMigrationByNodeID(st *Store, nodeID string) *RepositoryMigration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, RepositoryMigrationNodeIDPrefix); ok {
		if m := st.RepositoryMigrations[id]; m != nil && m.NodeID == nodeID {
			return m
		}
	}
	return nil
}

// ListRepositoryMigrations returns detached snapshots of one organization's
// repository migrations, oldest first.
func (st *Store) ListRepositoryMigrations(ownerOrgID int) []*RepositoryMigration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*RepositoryMigration
	for _, m := range st.RepositoryMigrations {
		if m.OwnerOrgID == ownerOrgID {
			out = append(out, cloneRepositoryMigration(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListRepositoryMigrationsForOrgMigration returns detached snapshots of the
// repository migrations one organization migration fanned out into.
func (st *Store) ListRepositoryMigrationsForOrgMigration(orgMigrationID int) []*RepositoryMigration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*RepositoryMigration
	for _, m := range st.RepositoryMigrations {
		if m.OrgMigrationID == orgMigrationID {
			out = append(out, cloneRepositoryMigration(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ClaimRepositoryMigration moves a queued migration into IN_PROGRESS and
// reports whether this caller claimed it.
func (st *Store) ClaimRepositoryMigration(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.RepositoryMigrations[id]
	if m == nil || m.State != GEIMigrationStateQueued {
		return false
	}
	m.State = GEIMigrationStateInProgress
	m.UpdatedAt = st.CurrentTime()
	st.persistRepositoryMigrationLocked(m)
	return true
}

// SetRepositoryMigrationState records a state and reason. It refuses to leave a
// terminal state, so a worker finishing after an abort cannot overwrite it.
func (st *Store) SetRepositoryMigrationState(id int, state, failureReason string) *RepositoryMigration {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.RepositoryMigrations[id]
	if m == nil || GEIMigrationTerminal(m.State) {
		return nil
	}
	m.State = state
	m.FailureReason = failureReason
	m.UpdatedAt = st.CurrentTime()
	st.persistRepositoryMigrationLocked(m)
	return cloneRepositoryMigration(m)
}

// RecordRepositoryMigrationWarning appends one recoverable problem to a
// migration's log and bumps its warning count.
func (st *Store) RecordRepositoryMigrationWarning(id int, warning string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.RepositoryMigrations[id]
	if m == nil {
		return false
	}
	m.WarningLog = append(m.WarningLog, warning)
	m.WarningsCount = len(m.WarningLog)
	m.UpdatedAt = st.CurrentTime()
	st.persistRepositoryMigrationLocked(m)
	return true
}

// SetRepositoryMigrationTargetRepo records the repository a migration created.
func (st *Store) SetRepositoryMigrationTargetRepo(id, repoID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.RepositoryMigrations[id]
	if m == nil {
		return false
	}
	m.TargetRepoID = repoID
	m.UpdatedAt = st.CurrentTime()
	st.persistRepositoryMigrationLocked(m)
	return true
}

// SetRepositoryMigrationSourceLock records the lock_source freeze. It refuses
// once terminal, so a late worker cannot re-freeze an unmigrated repository.
func (st *Store) SetRepositoryMigrationSourceLock(id int, fullName string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.RepositoryMigrations[id]
	if m == nil || GEIMigrationTerminal(m.State) {
		return false
	}
	m.SourceRepoLock = fullName
	m.UpdatedAt = st.CurrentTime()
	st.persistRepositoryMigrationLocked(m)
	return true
}

// SetRepositoryMigrationLogKey records where the migration's log was stored.
func (st *Store) SetRepositoryMigrationLogKey(id int, key string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.RepositoryMigrations[id]
	if m == nil {
		return false
	}
	m.MigrationLogKey = key
	m.UpdatedAt = st.CurrentTime()
	st.persistRepositoryMigrationLocked(m)
	return true
}

// AbortQueuedRepositoryMigrations fails every QUEUED migration of an org and
// returns how many. In-progress migrations are left alone (GitHub's
// abortQueuedMigrations names the queue).
func (st *Store) AbortQueuedRepositoryMigrations(ownerOrgID int, reason string) int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	aborted := 0
	now := st.CurrentTime()
	for _, m := range st.RepositoryMigrations {
		if m.OwnerOrgID != ownerOrgID || m.State != GEIMigrationStateQueued {
			continue
		}
		m.State = GEIMigrationStateFailed
		m.FailureReason = reason
		m.UpdatedAt = now
		st.persistRepositoryMigrationLocked(m)
		aborted++
	}
	return aborted
}

// ListUnfinishedRepositoryMigrations returns the ids of every repository
// migration not in a terminal state, ascending.
func (st *Store) ListUnfinishedRepositoryMigrations() []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []int
	for id, m := range st.RepositoryMigrations {
		if !GEIMigrationTerminal(m.State) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

// RequeueRepositoryMigration returns an in-progress migration to the queue, so
// work a dead process left behind becomes claimable again.
func (st *Store) RequeueRepositoryMigration(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.RepositoryMigrations[id]
	if m == nil || m.State != GEIMigrationStateInProgress {
		return false
	}
	m.State = GEIMigrationStateQueued
	m.UpdatedAt = st.CurrentTime()
	st.persistRepositoryMigrationLocked(m)
	return true
}

// --- organization migrations ---

// CreateOrganizationMigration queues an organization migration and returns a
// detached snapshot.
func (st *Store) CreateOrganizationMigration(enterpriseID int, sourceOrgURL, sourceOrgName, targetOrgName, sourceAccessToken string, startedByUserID int) *OrganizationMigration {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	id := st.NextOrganizationMigrationID
	st.NextOrganizationMigrationID++
	m := &OrganizationMigration{
		ID:                id,
		NodeID:            fmt.Sprintf("%s%08d", OrgMigrationNodeIDPrefix, id),
		EnterpriseID:      enterpriseID,
		SourceOrgURL:      sourceOrgURL,
		SourceOrgName:     sourceOrgName,
		TargetOrgName:     targetOrgName,
		State:             GEIMigrationStateQueued,
		SourceAccessToken: sourceAccessToken,
		StartedByUserID:   startedByUserID,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	st.OrganizationMigrations[id] = m
	st.persistOrganizationMigrationLocked(m)
	return cloneOrganizationMigration(m)
}

// GetOrganizationMigration returns a detached snapshot by database id, or nil.
func (st *Store) GetOrganizationMigration(id int) *OrganizationMigration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneOrganizationMigration(st.OrganizationMigrations[id])
}

// FindOrganizationMigrationByNodeID resolves an organization migration global
// id to the LIVE row.
func FindOrganizationMigrationByNodeID(st *Store, nodeID string) *OrganizationMigration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, OrgMigrationNodeIDPrefix); ok {
		if m := st.OrganizationMigrations[id]; m != nil && m.NodeID == nodeID {
			return m
		}
	}
	return nil
}

// ListOrganizationMigrations returns detached snapshots of one enterprise's
// organization migrations, oldest first.
func (st *Store) ListOrganizationMigrations(enterpriseID int) []*OrganizationMigration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*OrganizationMigration
	for _, m := range st.OrganizationMigrations {
		if m.EnterpriseID == enterpriseID {
			out = append(out, cloneOrganizationMigration(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ClaimOrganizationMigration moves a queued organization migration into
// IN_PROGRESS and reports whether this caller claimed it.
func (st *Store) ClaimOrganizationMigration(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.OrganizationMigrations[id]
	if m == nil || m.State != GEIMigrationStateQueued {
		return false
	}
	m.State = GEIMigrationStateInProgress
	m.UpdatedAt = st.CurrentTime()
	st.persistOrganizationMigrationLocked(m)
	return true
}

// UpdateOrganizationMigration applies a state transition and whatever progress
// accompanies it. It refuses to move out of a terminal state.
func (st *Store) UpdateOrganizationMigration(id int, mutate func(*OrganizationMigration)) *OrganizationMigration {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.OrganizationMigrations[id]
	if m == nil || GEIMigrationTerminal(m.State) {
		return nil
	}
	mutate(m)
	m.UpdatedAt = st.CurrentTime()
	st.persistOrganizationMigrationLocked(m)
	return cloneOrganizationMigration(m)
}

// ListUnfinishedOrganizationMigrations returns the ids of every organization
// migration not in a terminal state, ascending.
func (st *Store) ListUnfinishedOrganizationMigrations() []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []int
	for id, m := range st.OrganizationMigrations {
		if !GEIMigrationTerminal(m.State) {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

// RequeueOrganizationMigration returns a mid-flight organization migration to
// the queue so a restarted process picks it up again.
func (st *Store) RequeueOrganizationMigration(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.OrganizationMigrations[id]
	if m == nil || GEIMigrationTerminal(m.State) || m.State == GEIMigrationStateQueued {
		return false
	}
	m.State = GEIMigrationStateQueued
	m.UpdatedAt = st.CurrentTime()
	st.persistOrganizationMigrationLocked(m)
	return true
}

// --- the organization migrator role ---

// SetOrgMigratorRole grants or revokes the migrator role for one actor on one
// organization. It reports whether the grant set changed.
func (st *Store) SetOrgMigratorRole(orgID int, actorType, actor string, grantedBy int, granted bool) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	key := OrgMigratorRoleKey(orgID, actorType, actor)
	existing := st.OrgMigratorRoles[key]
	if granted {
		if existing != nil {
			return false
		}
		role := &OrgMigratorRole{
			OrgID:     orgID,
			ActorType: strings.ToUpper(actorType),
			Actor:     actor,
			GrantedBy: grantedBy,
			CreatedAt: st.CurrentTime(),
		}
		st.OrgMigratorRoles[key] = role
		st.persistOrgMigratorRoleLocked(role)
		return true
	}
	if existing == nil {
		return false
	}
	delete(st.OrgMigratorRoles, key)
	if st.Persist != nil {
		st.Persist.MustDelete("org_migrator_roles", key)
	}
	return true
}

// ListOrgMigratorRoles returns detached snapshots of one organization's
// migrator grants, ordered by actor type then actor.
func (st *Store) ListOrgMigratorRoles(orgID int) []*OrgMigratorRole {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*OrgMigratorRole
	for _, role := range st.OrgMigratorRoles {
		if role.OrgID == orgID {
			out = append(out, cloneOrgMigratorRole(role))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ActorType != out[j].ActorType {
			return out[i].ActorType < out[j].ActorType
		}
		return strings.ToLower(out[i].Actor) < strings.ToLower(out[j].Actor)
	})
	return out
}

// UserHoldsOrgMigratorRole reports whether a migrator grant reaches the user
// on this org — directly, through a team, or through the owning enterprise's
// grant. Every path is scoped to this org, so a migrator on one tenant is
// nothing on another.
func (st *Store) UserHoldsOrgMigratorRole(orgID int, user *User) bool {
	if user == nil {
		return false
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	org := st.Orgs[orgID]
	if org == nil {
		return false
	}
	if st.OrgMigratorRoles[OrgMigratorRoleKey(orgID, "USER", user.Login)] != nil {
		return true
	}
	for _, role := range st.OrgMigratorRoles {
		if role.OrgID != orgID || role.ActorType != "TEAM" {
			continue
		}
		team := st.TeamsBySlug[TeamSlugKey(org.Login, strings.ToLower(role.Actor))]
		if team == nil {
			continue
		}
		for _, memberID := range team.MemberIDs {
			if memberID == user.ID {
				return true
			}
		}
	}
	if enterpriseID := st.enterpriseIDForOrgLocked(orgID); enterpriseID != 0 {
		if e := st.Enterprises[enterpriseID]; e != nil {
			for _, login := range e.MigratorLogins {
				if strings.EqualFold(login, user.Login) {
					return true
				}
			}
		}
	}
	return false
}

// --- persistence load ---

// loadGEIMigrationBuckets restores the GEI layer during construction (no lock:
// the store is not yet reachable).
func (st *Store) loadGEIMigrationBuckets() error {
	if err := st.loadBucket("migration_sources", func(raw []byte) error {
		var src MigrationSource
		if err := LoadJSON(raw, &src); err != nil {
			return err
		}
		st.MigrationSources[src.ID] = &src
		if src.ID >= st.NextMigrationSourceID {
			st.NextMigrationSourceID = src.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("repository_migrations", func(raw []byte) error {
		var m RepositoryMigration
		if err := LoadJSON(raw, &m); err != nil {
			return err
		}
		st.RepositoryMigrations[m.ID] = &m
		if m.ID >= st.NextRepositoryMigrationID {
			st.NextRepositoryMigrationID = m.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("organization_migrations", func(raw []byte) error {
		var m OrganizationMigration
		if err := LoadJSON(raw, &m); err != nil {
			return err
		}
		st.OrganizationMigrations[m.ID] = &m
		if m.ID >= st.NextOrganizationMigrationID {
			st.NextOrganizationMigrationID = m.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	return st.loadBucket("org_migrator_roles", func(raw []byte) error {
		var role OrgMigratorRole
		if err := LoadJSON(raw, &role); err != nil {
			return err
		}
		st.OrgMigratorRoles[OrgMigratorRoleKey(role.OrgID, role.ActorType, role.Actor)] = &role
		return nil
	})
}

// RepositoryMigrationLogObjectKey is where a repository migration's log lives
// in the object byte store.
func RepositoryMigrationLogObjectKey(id int) string {
	return fmt.Sprintf("migrations/logs/repository-%d.log", id)
}
