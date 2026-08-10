package bleephub

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

type Repo struct {
	ID                                   int          `json:"id"`
	NodeID                               string       `json:"node_id"`
	Name                                 string       `json:"name"`
	FullName                             string       `json:"full_name"`
	Description                          string       `json:"description"`
	Homepage                             string       `json:"homepage"`
	DefaultBranch                        string       `json:"default_branch"`
	Visibility                           string       `json:"visibility"`
	Language                             string       `json:"language"`
	Owner                                *User        `json:"-"`
	OwnerID                              int          `json:"owner_id"`   // serialized so Owner can be relinked on reload
	OwnerType                            string       `json:"owner_type"` // "User" or "Organization"
	Private                              bool         `json:"private"`
	Fork                                 bool         `json:"fork"`
	Archived                             bool         `json:"archived"`
	ArchivedAt                           *time.Time   `json:"archived_at,omitempty"`
	IsTemplate                           bool         `json:"is_template"`
	WebCommitSignoffRequired             bool         `json:"web_commit_signoff_required"`
	HasIssues                            bool         `json:"has_issues"`
	HasProjects                          bool         `json:"has_projects"`
	HasWiki                              bool         `json:"has_wiki"`
	HasDiscussions                       *bool        `json:"has_discussions"`
	HasPullRequests                      bool         `json:"has_pull_requests"`
	AllowSquashMerge                     bool         `json:"allow_squash_merge"`
	AllowMergeCommit                     bool         `json:"allow_merge_commit"`
	AllowRebaseMerge                     bool         `json:"allow_rebase_merge"`
	AllowAutoMerge                       bool         `json:"allow_auto_merge"`
	AllowUpdateBranch                    bool         `json:"allow_update_branch"`
	DeleteBranchOnMerge                  bool         `json:"delete_branch_on_merge"`
	UseSquashPRTitleAsDefault            bool         `json:"use_squash_pr_title_as_default"`
	SquashMergeCommitTitle               string       `json:"squash_merge_commit_title"`
	SquashMergeCommitMessage             string       `json:"squash_merge_commit_message"`
	MergeCommitTitle                     string       `json:"merge_commit_title"`
	MergeCommitMessage                   string       `json:"merge_commit_message"`
	PullRequestCreationPolicy            string       `json:"pull_request_creation_policy"`
	LicenseKey                           string       `json:"license_key"`
	LicenseName                          string       `json:"license_name"`
	LicenseSPDX                          string       `json:"license_spdx"`
	StargazersCount                      int          `json:"stargazers_count"`
	Topics                               []string     `json:"topics"`
	Stargazers                           map[int]bool `json:"stargazers,omitempty"`
	ParentID                             int          `json:"parent_id"`
	SourceID                             int          `json:"source_id"`
	TemplateRepoID                       int          `json:"template_repo_id,omitempty"`
	NextIssueNumber                      int          `json:"-"`
	NextMilestoneNumber                  int          `json:"-"`
	AutomatedSecurityFixesEnabled        bool         `json:"automated_security_fixes_enabled"`
	PrivateVulnerabilityReportingEnabled bool         `json:"private_vulnerability_reporting_enabled"`
	VulnerabilityAlertsEnabled           bool         `json:"vulnerability_alerts_enabled"`
	InteractionLimit                     string       `json:"interaction_limit"`
	InteractionLimitExpiry               *time.Time   `json:"interaction_limit_expiry,omitempty"`
	LFSEnabled                           bool         `json:"lfs_enabled,omitempty"`
	CreatedAt                            time.Time    `json:"created_at"`
	UpdatedAt                            time.Time    `json:"updated_at"`
	PushedAt                             time.Time    `json:"pushed_at"`
}

func (st *Store) CreateRepo(owner *User, name, description string, private bool) *Repo {
	return st.createRepo(owner.Login+"/"+name, name, description, private, owner.ID, "User", owner)
}

func (st *Store) createRepo(fullName, name, description string, private bool, ownerID int, ownerType string, owner *User) *Repo {
	st.mu.Lock()
	if st.pendingRepoCreations == nil {
		st.pendingRepoCreations = make(map[string]bool)
	}
	if st.ReposByName[fullName] != nil || st.pendingRepoCreations[fullName] {
		st.mu.Unlock()
		return nil
	}
	st.pendingRepoCreations[fullName] = true
	st.mu.Unlock()

	// Storage initialization can include filesystem or S3 I/O. The pending
	// name reservation preserves duplicate-create atomicity while ordinary
	// reads and unrelated mutations continue.
	openStorage := st.repoStorageOpen
	if openStorage == nil {
		openStorage = openOrInitGitStorage
	}
	stor, err := openStorage(context.Background(), fullName)
	if err != nil {
		st.mu.Lock()
		delete(st.pendingRepoCreations, fullName)
		st.mu.Unlock()
		st.logger.Error().Str("repo", fullName).Err(err).Msg("create repo: open git storage failed")
		return nil
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.pendingRepoCreations, fullName)
	if st.ReposByName[fullName] != nil {
		return nil
	}
	return st.createRepoLocked(fullName, name, description, private, ownerID, ownerType, owner, stor)
}

// createRepoLocked creates a repo record around prepared git storage. Caller
// must hold st.mu. A nil storer is retained only for the Codespaces publish
// transaction, whose broader two-entity conversion is tracked separately.
func (st *Store) createRepoLocked(fullName, name, description string, private bool, ownerID int, ownerType string, owner *User, stor gitStorage.Storer) *Repo {
	if _, exists := st.ReposByName[fullName]; exists {
		return nil
	}

	now := st.currentTime()
	visibility := "public"
	if private {
		visibility = "private"
	}

	repoID := st.reserveGlobalID("next_repo", &st.NextRepo)
	repo := &Repo{
		ID:                        repoID,
		NodeID:                    fmt.Sprintf("R_kgDO%08d", repoID),
		Name:                      name,
		FullName:                  fullName,
		Description:               description,
		DefaultBranch:             "main",
		Visibility:                visibility,
		Owner:                     owner,
		OwnerID:                   ownerID,
		OwnerType:                 ownerType,
		Private:                   private,
		HasIssues:                 true,
		HasProjects:               false,
		HasWiki:                   false,
		HasDiscussions:            boolPointer(true),
		HasPullRequests:           true,
		AllowSquashMerge:          true,
		AllowMergeCommit:          true,
		AllowRebaseMerge:          true,
		PullRequestCreationPolicy: "all",
		Topics:                    []string{},
		Stargazers:                map[int]bool{},
		NextIssueNumber:           1,
		NextMilestoneNumber:       1,
		CreatedAt:                 now,
		UpdatedAt:                 now,
	}
	if stor == nil {
		var err error
		openStorage := st.repoStorageOpen
		if openStorage == nil {
			openStorage = openOrInitGitStorage
		}
		stor, err = openStorage(context.Background(), fullName)
		if err != nil {
			st.logger.Error().Str("repo", fullName).Err(err).Msg("create repo: open git storage failed")
			return nil
		}
	}

	batch := newPersistBatch(st.persist)
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", key: strconv.Itoa(repo.ID), err: err})
	}
	st.Repos[repo.ID] = repo
	st.ReposByName[fullName] = repo
	st.GitStorages[fullName] = stor

	st.ensureDefaultDiscussionCategoriesLocked(repo.ID)

	return repo
}

func (st *Store) GetRepo(owner, name string) *Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneRepo(st.ReposByName[owner+"/"+name])
}

// GetRepoByFullName resolves an "owner/name" key under the read lock.
//
// Handlers must use this rather than indexing ReposByName directly. That map is
// written under the write lock by repository create, rename and delete, and an
// unsynchronized read racing one of those is a concurrent map read and map
// write — which the runtime reports as a fatal error and kills the process,
// rather than a panic a recovery middleware could turn into a 500.
func (st *Store) GetRepoByFullName(fullName string) *Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneRepo(st.ReposByName[fullName])
}

func (st *Store) GetRepoByID(id int) *Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneRepo(st.Repos[id])
}

func (st *Store) UpdateRepo(owner, name string, fn func(*Repo)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	repoKey := owner + "/" + name
	current, ok := st.ReposByName[repoKey]
	if !ok {
		return false
	}
	repo := cloneRepo(current)
	fn(repo)
	repo.UpdatedAt = st.currentTime()
	batch := newPersistBatch(st.persist)
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", key: strconv.Itoa(repo.ID), err: err})
	}
	st.Repos[repo.ID] = repo
	st.ReposByName[repoKey] = repo
	return true
}

// ForkRepo creates a fork of sourceRepo owned by owner. It copies the git
// storage and records parent/source linkage. Returns nil if the source repo
// does not exist or the target name is already taken.
func (st *Store) ForkRepo(owner *User, sourceRepo *Repo, name string) *Repo {
	st.mu.Lock()
	fullName := owner.Login + "/" + name
	if st.pendingRepoCreations == nil {
		st.pendingRepoCreations = make(map[string]bool)
	}
	if st.ReposByName[fullName] != nil || st.pendingRepoCreations[fullName] {
		st.mu.Unlock()
		return nil
	}
	srcStor, ok := st.GitStorages[sourceRepo.FullName]
	if !ok {
		st.mu.Unlock()
		return nil
	}
	liveSource := st.Repos[sourceRepo.ID]
	if liveSource == nil || liveSource.FullName != sourceRepo.FullName {
		st.mu.Unlock()
		return nil
	}
	source := cloneRepo(liveSource)
	st.pendingRepoCreations[fullName] = true
	st.mu.Unlock()

	// Opening a backend and copying the complete object graph may involve S3
	// and must not hold the global store lock. The target reservation prevents
	// a create/fork race from publishing the same full name.
	openStorage := st.repoStorageOpen
	if openStorage == nil {
		openStorage = openOrInitGitStorage
	}
	stor, err := openStorage(context.Background(), fullName)
	if err == nil {
		err = copyGitStorage(srcStor, stor)
	}
	if err != nil {
		st.mu.Lock()
		delete(st.pendingRepoCreations, fullName)
		st.mu.Unlock()
		st.logger.Error().Str("repo", fullName).Err(err).Msg("fork repo: copy git storage failed")
		return nil
	}

	st.mu.Lock()
	if st.Repos[source.ID] != liveSource || st.ReposByName[fullName] != nil {
		st.mu.Unlock()
		_ = deleteRepoGitStorage(fullName)
		st.mu.Lock()
		delete(st.pendingRepoCreations, fullName)
		st.mu.Unlock()
		return nil
	}
	defer st.mu.Unlock()
	delete(st.pendingRepoCreations, fullName)

	sourceID := source.ID
	if source.SourceID != 0 {
		sourceID = source.SourceID
	}

	now := st.currentTime()
	repoID := st.reserveGlobalID("next_repo", &st.NextRepo)
	repo := &Repo{
		ID:                        repoID,
		NodeID:                    fmt.Sprintf("R_kgDO%08d", repoID),
		Name:                      name,
		FullName:                  fullName,
		Description:               source.Description,
		Homepage:                  source.Homepage,
		DefaultBranch:             source.DefaultBranch,
		Visibility:                source.Visibility,
		Language:                  source.Language,
		Owner:                     owner,
		OwnerID:                   owner.ID,
		OwnerType:                 "User",
		Private:                   source.Private,
		Fork:                      true,
		Archived:                  source.Archived,
		ArchivedAt:                cloneTimePtr(source.ArchivedAt),
		ParentID:                  source.ID,
		SourceID:                  sourceID,
		HasIssues:                 source.HasIssues,
		HasProjects:               source.HasProjects,
		HasWiki:                   source.HasWiki,
		HasDiscussions:            boolPointer(repoHasDiscussions(source)),
		HasPullRequests:           source.HasPullRequests,
		AllowSquashMerge:          source.AllowSquashMerge,
		AllowMergeCommit:          source.AllowMergeCommit,
		AllowRebaseMerge:          source.AllowRebaseMerge,
		AllowAutoMerge:            source.AllowAutoMerge,
		AllowUpdateBranch:         source.AllowUpdateBranch,
		DeleteBranchOnMerge:       source.DeleteBranchOnMerge,
		UseSquashPRTitleAsDefault: source.UseSquashPRTitleAsDefault,
		SquashMergeCommitTitle:    source.SquashMergeCommitTitle,
		SquashMergeCommitMessage:  source.SquashMergeCommitMessage,
		MergeCommitTitle:          source.MergeCommitTitle,
		MergeCommitMessage:        source.MergeCommitMessage,
		PullRequestCreationPolicy: source.PullRequestCreationPolicy,
		LicenseKey:                source.LicenseKey,
		LicenseName:               source.LicenseName,
		LicenseSPDX:               source.LicenseSPDX,
		Topics:                    append([]string(nil), source.Topics...),
		Stargazers:                map[int]bool{},
		NextIssueNumber:           1,
		NextMilestoneNumber:       1,
		CreatedAt:                 now,
		UpdatedAt:                 now,
		PushedAt:                  source.PushedAt,
	}
	batch := newPersistBatch(st.persist)
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", key: strconv.Itoa(repo.ID), err: err})
	}

	st.Repos[repo.ID] = repo
	st.ReposByName[fullName] = repo
	st.GitStorages[fullName] = stor

	st.ensureDefaultDiscussionCategoriesLocked(repo.ID)
	return repo
}

func boolPointer(v bool) *bool {
	return &v
}

func repoHasDiscussions(repo *Repo) bool {
	if repo == nil || repo.HasDiscussions == nil {
		return true
	}
	return *repo.HasDiscussions
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	clone := t.UTC()
	return &clone
}

// cloneRepo returns a deep copy of a repository detached from the stored row:
// every mutable reference field (time pointers, the *bool, the topics slice and
// the stargazers map) is copied so a caller can neither observe a concurrent
// in-place mutation (e.g. StarRepo writing the Stargazers map under the write
// lock) nor leak a write back into the store. The Owner *User is a shared
// cross-entity pointer kept shallow — detaching users is GetUser's concern.
// It backs both the copy-on-write UpdateRepo path and the read getters.
func cloneRepo(repo *Repo) *Repo {
	if repo == nil {
		return nil
	}
	clone := *repo
	clone.ArchivedAt = cloneTimePtr(repo.ArchivedAt)
	clone.InteractionLimitExpiry = cloneTimePtr(repo.InteractionLimitExpiry)
	if repo.HasDiscussions != nil {
		v := *repo.HasDiscussions
		clone.HasDiscussions = &v
	}
	clone.Topics = append([]string(nil), repo.Topics...)
	clone.Stargazers = make(map[int]bool, len(repo.Stargazers))
	for userID, starred := range repo.Stargazers {
		clone.Stargazers[userID] = starred
	}
	return &clone
}

// snapshotRepos returns detached clones of a repo list so a caller iterating and
// rendering them can't race an in-place element mutation — most acutely
// StarRepo/UnstarRepo writing an element's Stargazers map, a fatal concurrent
// map access (STORE-021). Named to avoid colliding with the test-only
// cloneRepoSlice.
func snapshotRepos(in []*Repo) []*Repo {
	if in == nil {
		return nil
	}
	out := make([]*Repo, len(in))
	for i, r := range in {
		out[i] = cloneRepo(r)
	}
	return out
}

// snapshot* helpers detach a list of store pointers (STORE-021): a caller
// iterating and rendering them can't race an in-place element mutation, and a
// value copy can't leak a write back into the stored row. Each reuses the
// element's existing clone helper.
func snapshotIssues(in []*Issue) []*Issue {
	if in == nil {
		return nil
	}
	out := make([]*Issue, len(in))
	for i, x := range in {
		out[i] = cloneIssue(x)
	}
	return out
}

func snapshotPullRequests(in []*PullRequest) []*PullRequest {
	if in == nil {
		return nil
	}
	out := make([]*PullRequest, len(in))
	for i, x := range in {
		out[i] = clonePullRequest(x)
	}
	return out
}

func snapshotComments(in []*Comment) []*Comment {
	if in == nil {
		return nil
	}
	out := make([]*Comment, len(in))
	for i, x := range in {
		out[i] = cloneComment(x)
	}
	return out
}

func snapshotOrgs(in []*Org) []*Org {
	if in == nil {
		return nil
	}
	out := make([]*Org, len(in))
	for i, x := range in {
		out[i] = cloneOrg(x)
	}
	return out
}

func snapshotTeams(in []*Team) []*Team {
	if in == nil {
		return nil
	}
	out := make([]*Team, len(in))
	for i, x := range in {
		out[i] = cloneTeam(x)
	}
	return out
}

func snapshotMilestones(in []*Milestone) []*Milestone {
	if in == nil {
		return nil
	}
	out := make([]*Milestone, len(in))
	for i, x := range in {
		out[i] = cloneMilestone(x)
	}
	return out
}

func snapshotAttestations(in []*Attestation) []*Attestation {
	if in == nil {
		return nil
	}
	out := make([]*Attestation, len(in))
	for i, x := range in {
		out[i] = cloneAttestation(x)
	}
	return out
}

func snapshotCodeScanningAlerts(in []*CodeScanningAlert) []*CodeScanningAlert {
	if in == nil {
		return nil
	}
	out := make([]*CodeScanningAlert, len(in))
	for i, x := range in {
		out[i] = cloneCodeScanningAlert(x)
	}
	return out
}

func snapshotCodeSecurityConfigurations(in []*CodeSecurityConfiguration) []*CodeSecurityConfiguration {
	if in == nil {
		return nil
	}
	out := make([]*CodeSecurityConfiguration, len(in))
	for i, x := range in {
		out[i] = cloneCodeSecurityConfiguration(x)
	}
	return out
}

func snapshotDependabotAlerts(in []*DependabotAlert) []*DependabotAlert {
	if in == nil {
		return nil
	}
	out := make([]*DependabotAlert, len(in))
	for i, x := range in {
		out[i] = cloneDependabotAlert(x)
	}
	return out
}

func snapshotDiscussions(in []*Discussion) []*Discussion {
	if in == nil {
		return nil
	}
	out := make([]*Discussion, len(in))
	for i, x := range in {
		out[i] = cloneDiscussion(x)
	}
	return out
}

func snapshotEnterpriseTeams(in []*EnterpriseTeam) []*EnterpriseTeam {
	if in == nil {
		return nil
	}
	out := make([]*EnterpriseTeam, len(in))
	for i, x := range in {
		out[i] = cloneEnterpriseTeam(x)
	}
	return out
}

func snapshotOrgInvitations(in []*OrgInvitation) []*OrgInvitation {
	if in == nil {
		return nil
	}
	out := make([]*OrgInvitation, len(in))
	for i, x := range in {
		out[i] = cloneOrgInvitation(x)
	}
	return out
}

func snapshotPackages(in []*Package) []*Package {
	if in == nil {
		return nil
	}
	out := make([]*Package, len(in))
	for i, x := range in {
		out[i] = clonePackage(x)
	}
	return out
}

func snapshotPackageVersions(in []*PackageVersion) []*PackageVersion {
	if in == nil {
		return nil
	}
	out := make([]*PackageVersion, len(in))
	for i, x := range in {
		out[i] = clonePackageVersion(x)
	}
	return out
}

func snapshotSecretScanningAlerts(in []*SecretScanningAlert) []*SecretScanningAlert {
	if in == nil {
		return nil
	}
	out := make([]*SecretScanningAlert, len(in))
	for i, x := range in {
		out[i] = cloneSecretScanningAlert(x)
	}
	return out
}

func snapshotReviews(in []*PullRequestReview) []*PullRequestReview {
	if in == nil {
		return nil
	}
	out := make([]*PullRequestReview, len(in))
	for i, x := range in {
		out[i] = cloneReview(x)
	}
	return out
}

func snapshotEnterpriseCodeSecurityConfigs(in []*EnterpriseCodeSecurityConfiguration) []*EnterpriseCodeSecurityConfiguration {
	if in == nil {
		return nil
	}
	out := make([]*EnterpriseCodeSecurityConfiguration, len(in))
	for i, x := range in {
		out[i] = cloneEnterpriseCodeSecurityConfig(x)
	}
	return out
}

// RenameRepo renames owner/name to owner/newName, moving every map keyed by
// the repo full name and updating embedded repo-name strings. It returns true
// on success.
// RenameRepo renames owner/name to owner/newName, moving its git bytes. For
// filesystem and in-memory storage the move is constant-time and stays under the
// store lock; for S3 the object-prefix copy is slow, so it runs outside the lock
// behind a target reservation and a crash-recoverable intent (STORE-013).
func (st *Store) RenameRepo(owner, name, newName string) bool {
	if st.renameNeedsSlowMove() {
		return st.renameRepoS3(owner, name, newName)
	}
	return st.renameRepoUnderLock(owner, name, newName)
}

func (st *Store) renameRepoUnderLock(owner, name, newName string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	oldFull := owner + "/" + name
	newFull := owner + "/" + newName
	if oldFull == newFull {
		return true
	}
	repo, ok := st.ReposByName[oldFull]
	if !ok {
		return false
	}
	if _, exists := st.ReposByName[newFull]; exists {
		return false
	}

	// Move the bytes and rebind the storer first; if either fails, abort before
	// mutating in-memory indexes. A path-bound storer keeps addressing the
	// prefix the bytes just left, so it has to be reopened against the new one.
	// An in-memory storer holds the objects itself and is simply re-keyed.
	if err := moveRepoGitStorage(oldFull, newFull); err != nil {
		st.logger.Error().Str("from", oldFull).Str("to", newFull).Err(err).Msg("rename repo failed")
		return false
	}
	stor := st.GitStorages[oldFull]
	if stor != nil && repoGitStorageIsPathBound() {
		reopened, err := openOrInitGitStorage(context.Background(), newFull)
		if err != nil {
			st.logger.Error().Str("from", oldFull).Str("to", newFull).Err(err).Msg("rename repo: reopen git storage failed")
			return false
		}
		stor = reopened
	}

	repo.Name = newName
	repo.FullName = newFull
	repo.UpdatedAt = st.currentTime()

	st.ReposByName[newFull] = repo
	delete(st.ReposByName, oldFull)

	if stor != nil {
		st.GitStorages[newFull] = stor
		delete(st.GitStorages, oldFull)
	}
	// Re-key the repos row and every subresource bucket in one transaction, so a
	// crash can never leave the repository split across its old and new names.
	batch := newPersistBatch(st.persist)
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	st.moveRepoKeyLocked(batch, oldFull, newFull)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", err: err})
	}

	return true
}

// renameRepoS3 performs a rename whose object-prefix copy is too slow to hold
// the store lock. It reserves the target name and records a rename intent under
// the lock, copies the object graph outside the lock (both prefixes coexist, so
// old-name readers keep working), swaps the metadata under the lock, then purges
// the old prefix outside the lock. A crash at any point is finished by
// finishInterruptedRenames.
func (st *Store) renameRepoS3(owner, name, newName string) bool {
	oldFull := owner + "/" + name
	newFull := owner + "/" + newName

	st.mu.Lock()
	if oldFull == newFull {
		st.mu.Unlock()
		return true
	}
	repo, ok := st.ReposByName[oldFull]
	if !ok {
		st.mu.Unlock()
		return false
	}
	if st.pendingRepoCreations == nil {
		st.pendingRepoCreations = make(map[string]bool)
	}
	if st.ReposByName[newFull] != nil || st.pendingRepoCreations[newFull] {
		st.mu.Unlock()
		return false
	}
	repoID := repo.ID
	st.pendingRepoCreations[newFull] = true
	intent := pendingRename{From: oldFull, To: newFull, StartedAt: st.currentTime()}
	if err := st.persist.Put(pendingRenamesBucket, pendingRepoRenameKey(newFull), intent); err != nil {
		delete(st.pendingRepoCreations, newFull)
		st.mu.Unlock()
		st.logger.Error().Str("from", oldFull).Str("to", newFull).Err(err).Msg("rename repo: record intent failed")
		return false
	}
	st.mu.Unlock()

	// Outside the lock: copy the object graph. The source stays intact, so a
	// reader resolving the old name during the copy still finds its bytes.
	if err := st.copyRepoPrefixBytes(oldFull, newFull); err != nil {
		st.abortRenameReservation(newFull)
		st.logger.Error().Str("from", oldFull).Str("to", newFull).Err(err).Msg("rename repo: copy object prefix failed")
		return false
	}

	// Re-lock and re-validate: the repo must still be the same one at the old
	// name (a concurrent delete or competing rename may have moved it).
	st.mu.Lock()
	live := st.Repos[repoID]
	if live == nil || live.FullName != oldFull || (st.ReposByName[newFull] != nil && st.ReposByName[newFull] != live) {
		st.mu.Unlock()
		st.abortRenameReservation(newFull)
		return false
	}
	stor := st.GitStorages[oldFull]
	if stor != nil && repoGitStorageIsPathBound() {
		reopened, err := openOrInitGitStorage(context.Background(), newFull)
		if err != nil {
			st.mu.Unlock()
			st.abortRenameReservation(newFull)
			st.logger.Error().Str("from", oldFull).Str("to", newFull).Err(err).Msg("rename repo: reopen git storage failed")
			return false
		}
		stor = reopened
	}

	live.Name = newName
	live.FullName = newFull
	live.UpdatedAt = st.currentTime()
	st.ReposByName[newFull] = live
	delete(st.ReposByName, oldFull)
	if stor != nil {
		st.GitStorages[newFull] = stor
		delete(st.GitStorages, oldFull)
	}
	batch := newPersistBatch(st.persist)
	batch.Put("repos", strconv.Itoa(live.ID), live)
	st.moveRepoKeyLocked(batch, oldFull, newFull)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", err: err})
	}
	delete(st.pendingRepoCreations, newFull)
	st.mu.Unlock()

	// The metadata now points at the new name, so the old prefix is unreferenced.
	// Purge it, then clear the intent; a crash before either is finished by
	// recovery. Keep the intent if the purge fails so recovery can retry it.
	if err := st.deleteRepoPrefixBytes(oldFull); err != nil {
		st.logger.Warn().Str("from", oldFull).Str("to", newFull).Err(err).Msg("rename repo: purge old object prefix deferred to recovery")
		return true
	}
	if err := st.persist.Delete(pendingRenamesBucket, pendingRepoRenameKey(newFull)); err != nil {
		st.logger.Warn().Str("to", newFull).Err(err).Msg("rename repo: clear intent deferred to recovery")
	}
	return true
}

// abortRenameReservation unwinds a rename that did not publish: it drops the
// target reservation, purges the (partial or unpublished) new-name copy, and
// clears the intent — keeping the intent for recovery if the purge fails.
func (st *Store) abortRenameReservation(newFull string) {
	st.mu.Lock()
	delete(st.pendingRepoCreations, newFull)
	st.mu.Unlock()
	if err := st.deleteRepoPrefixBytes(newFull); err != nil {
		return // leave the intent for finishInterruptedRenames
	}
	_ = st.persist.Delete(pendingRenamesBucket, pendingRepoRenameKey(newFull))
}

// DeleteRepo removes a repository, its cascade and its bytes. The metadata
// goes first and atomically; the object bytes go afterwards, outside the store
// lock, guarded by a recorded deletion intent that a later start can finish.
func (st *Store) DeleteRepo(owner, name string) (bool, error) {
	fullName := owner + "/" + name
	existed, intent, err := st.deleteRepoMetadata(owner, name)
	if err != nil || !existed {
		return existed, err
	}
	return true, st.purgeDeletedRepoBytes(fullName, intent)
}

// purgeDeletedRepoBytes destroys the git bytes of an already-unregistered
// repository and clears its deletion intent. Nothing can reach the repository
// at this point, so the object-store round trip does not need the store lock.
func (st *Store) purgeDeletedRepoBytes(fullName string, fallback pendingDeletion) error {
	raw, err := st.persist.Get(pendingDeletionsBucket, pendingRepoDeletionKey(fullName))
	if err != nil {
		return fmt.Errorf("delete repo %s: load deletion intent: %w", fullName, err)
	}
	record := fallback
	if len(raw) > 0 {
		if err := loadJSON(raw, &record); err != nil {
			return fmt.Errorf("delete repo %s: decode deletion intent: %w", fullName, err)
		}
	}
	if record.Name == "" {
		record.Name = fullName
	}
	if err := st.cleanupDeletedRepo(record); err != nil {
		return fmt.Errorf("delete repo %s external data: %w", fullName, err)
	}
	if err := deleteRepoGitStorage(fullName); err != nil {
		return fmt.Errorf("delete repo %s git storage: %w", fullName, err)
	}
	if err := st.persist.Delete(pendingDeletionsBucket, pendingRepoDeletionKey(fullName)); err != nil {
		return fmt.Errorf("delete repo %s: clear deletion intent: %w", fullName, err)
	}
	return nil
}

func (st *Store) cleanupDeletedRepo(record pendingDeletion) error {
	for _, runtime := range record.CodespaceRuntimes {
		if runtime.ContainerID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			err := dockerRemoveContainer(ctx, runtime.ContainerID)
			cancel()
			if err != nil {
				return fmt.Errorf("remove codespace container %s: %w", runtime.ContainerID, err)
			}
		}
		switch classifyCodespaceWorkspace(runtime.WorkspaceMount) {
		case codespaceWorkspaceNone, codespaceWorkspaceBorrowed:
		case codespaceWorkspaceScratch:
			if err := os.RemoveAll(runtime.WorkspaceMount); err != nil {
				return fmt.Errorf("remove codespace workspace: %w", err)
			}
		default:
			return fmt.Errorf("refusing to remove codespace workspace outside the temporary directory: %s", runtime.WorkspaceMount)
		}
	}
	for _, key := range record.ObjectKeys {
		if st.ObjectByteStore == nil {
			return fmt.Errorf("object %s requires configured object storage", key)
		}
		if err := st.ObjectByteStore.Delete(context.Background(), key); err != nil {
			return fmt.Errorf("delete object %s: %w", key, err)
		}
	}
	for _, key := range record.ReleaseAssetObjects {
		if st.Releases.byteStore == nil {
			return fmt.Errorf("release asset object %s requires configured object storage", key)
		}
		if err := st.Releases.byteStore.Delete(context.Background(), key); err != nil {
			return fmt.Errorf("delete release asset object %s: %w", key, err)
		}
	}
	for _, key := range record.ActionsObjectKeys {
		if st.actionsArtifacts == nil || st.actionsArtifacts.byteStore == nil {
			return fmt.Errorf("actions object %s requires configured object storage", key)
		}
		if err := st.actionsArtifacts.byteStore.Delete(context.Background(), key); err != nil {
			return fmt.Errorf("delete Actions object %s: %w", key, err)
		}
	}
	for _, path := range append(record.LocalFiles, record.ReleaseAssetFiles...) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete file %s: %w", path, err)
		}
	}
	for _, directory := range record.ActionsDirectories {
		if err := os.RemoveAll(directory); err != nil {
			return fmt.Errorf("delete Actions directory %s: %w", directory, err)
		}
	}
	return nil
}

func (st *Store) deleteRepoMetadata(owner, name string) (bool, pendingDeletion, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.deleteRepoLocked(owner, name)
}

// deleteRepoLocked purges the repository from memory and, in one transaction,
// from the database. Caller must hold st.mu and must call
// purgeDeletedRepoBytes afterwards to finish the deletion.
func (st *Store) deleteRepoLocked(owner, name string) (bool, pendingDeletion, error) {
	fullName := owner + "/" + name
	repo, ok := st.ReposByName[fullName]
	if !ok {
		return false, pendingDeletion{}, nil
	}

	intent := st.repoDeletionIntentLocked(repo)
	planIDs, logIDs := st.repoWorkflowCleanupIDsLocked(fullName)
	var completeActionsDeletion func(*persistBatch)
	if st.actionsArtifacts != nil {
		completeActionsDeletion = st.actionsArtifacts.prepareRepositoryDeletion(fullName, logIDs, &intent)
	}
	sort.Strings(intent.ActionsObjectKeys)
	sort.Strings(intent.ActionsDirectories)
	if err := st.persist.Put(pendingDeletionsBucket, pendingRepoDeletionKey(fullName), intent); err != nil {
		if completeActionsDeletion != nil {
			completeActionsDeletion(nil)
		}
		return true, pendingDeletion{}, fmt.Errorf("delete repo %s: record deletion intent: %w", fullName, err)
	}
	batch := newPersistBatch(st.persist)
	if completeActionsDeletion != nil {
		completeActionsDeletion(batch)
	}
	for planID := range planIDs {
		delete(st.TimelineRecords, planID)
		batch.Delete("timeline_records", planID)
	}
	for logID := range logIDs {
		delete(st.LogFiles, logID)
	}

	delete(st.Repos, repo.ID)
	delete(st.ReposByName, fullName)
	delete(st.GitStorages, fullName)
	batch.Delete("repos", strconv.Itoa(repo.ID))

	// Cascade: purge everything keyed to this repo from memory AND the DB.
	// Hook IDs, issue numbers and release IDs restart from the surviving
	// maxima after a reload, so leftovers would be inherited by a recreated
	// same-name repo.
	for _, h := range st.Hooks[fullName] {
		delete(st.HookDeliveries, h.ID)
		batch.Delete("hook_deliveries", strconv.Itoa(h.ID))
	}
	delete(st.Hooks, fullName)
	delete(st.RepoSecrets, fullName)
	delete(st.RepoVariables, fullName)
	delete(st.RepoCollaborators, fullName)
	delete(st.RepoAutolinks, fullName)
	delete(st.RepoInvitations, fullName)
	delete(st.RepoDeployKeys, fullName)
	delete(st.CheckSuitePrefs, fullName)
	delete(st.RepoActionsPermissions, fullName)
	delete(st.DependabotSecrets, fullName)
	delete(st.CodeScanningDefaultSetups, fullName)
	delete(st.CodeQualitySetups, fullName)
	delete(st.RepoCustomPropertyValues, fullName)
	delete(st.RepoImmutableReleases, fullName)
	delete(st.AgentsRepoSecrets, fullName)
	delete(st.AgentsRepoVariables, fullName)
	delete(st.SecretScanningPushPlaceholders, fullName)
	delete(st.SecretScanningPushBypasses, fullName)
	st.deleteRepoFullNameReferencesLocked(batch, fullName)
	st.deleteNotificationRepoKeyBatchLocked(batch, fullName)
	st.CommitStatuses.deleteRepoKeyBatch(fullName, batch)
	commitCommentIDs := st.CommitComments.IDsForRepo(repo.ID)
	st.CommitComments.deleteRepoBatch(repo.ID, batch)
	st.Reactions.DeleteParentsBatch("commit_comment", commitCommentIDs, batch)
	for id, alert := range st.SecretScanningAlerts {
		if alert.RepoKey == fullName {
			delete(st.SecretScanningAlerts, id)
			batch.Delete("secret_scanning_alerts", strconv.Itoa(id))
		}
	}
	delete(st.SecretScanningAlertsByRepo, fullName)
	delete(st.SecretScanningNextNumber, fullName)
	batch.Delete("hooks", fullName)
	batch.Delete("repo_secrets", fullName)
	batch.Delete("repo_variables", fullName)
	batch.Delete("repo_collaborators", fullName)
	batch.Delete("repo_autolinks", fullName)
	batch.Delete("repo_invitations", fullName)
	batch.Delete("repo_deploy_keys", fullName)
	batch.Delete("check_suite_prefs", fullName)
	batch.Delete("repo_actions_permissions", fullName)
	batch.Delete("dependabot_secrets", fullName)
	batch.Delete("code_scanning_default_setups", fullName)
	batch.Delete("code_quality_setups", fullName)
	batch.Delete("repo_custom_property_values", fullName)
	batch.Delete("repo_immutable_releases", fullName)
	batch.Delete("agents_repo_secrets", fullName)
	batch.Delete("agents_repo_variables", fullName)
	batch.Delete("secret_scanning_push_placeholders", fullName)
	batch.Delete("secret_scanning_push_bypasses", fullName)
	for id, suite := range st.CheckSuites {
		if suite.RepoKey == fullName {
			delete(st.CheckSuites, id)
			batch.Delete("check_suites", strconv.FormatInt(id, 10))
		}
	}
	for id, run := range st.CheckRuns {
		if run.RepoKey == fullName {
			delete(st.CheckRuns, id)
			batch.Delete("check_runs", strconv.FormatInt(id, 10))
		}
	}
	for id, wf := range st.Workflows {
		if wf.RepoFullName == fullName {
			delete(st.Workflows, id)
			batch.Delete("workflows", id)
			delete(st.WorkflowAttempts, wf.RunID)
			batch.Delete("workflow_attempts", strconv.Itoa(wf.RunID))
		}
	}
	for runID, attempts := range st.WorkflowAttempts {
		kept := attempts[:0]
		for _, wf := range attempts {
			if wf.RepoFullName != fullName {
				kept = append(kept, wf)
			}
		}
		if len(kept) == len(attempts) {
			continue
		}
		st.WorkflowAttempts[runID] = kept
		if len(kept) == 0 {
			batch.Delete("workflow_attempts", strconv.Itoa(runID))
		} else {
			batch.Put("workflow_attempts", strconv.Itoa(runID), kept)
		}
	}
	for id, alert := range st.CodeScanningAlerts {
		if alert.RepoKey == fullName {
			delete(st.CodeScanningAlerts, id)
			batch.Delete("code_scanning_alerts", strconv.Itoa(id))
		}
	}
	delete(st.CodeScanningAlertsByRepo, fullName)
	delete(st.CodeScanningNextNumber, fullName)
	for id, analysis := range st.CodeScanningAnalyses {
		if analysis.RepoKey == fullName {
			delete(st.CodeScanningAnalyses, id)
			batch.Delete("code_scanning_analyses", strconv.Itoa(id))
		}
	}
	delete(st.CodeScanningAnalysesByRepo, fullName)
	for key, fix := range st.CodeScanningAutofixes {
		if fix.RepoKey == fullName {
			delete(st.CodeScanningAutofixes, key)
			batch.Delete("code_scanning_autofixes", key)
		}
	}
	for id, up := range st.SARIFUploads {
		if up.RepoKey == fullName {
			delete(st.SARIFUploads, id)
			batch.Delete("sarif_uploads", id)
		}
	}
	for id, db := range st.CodeQLDatabases {
		if db.RepoKey == fullName {
			delete(st.CodeQLDatabases, id)
			batch.Delete("codeql_databases", strconv.Itoa(id))
		}
	}
	delete(st.CodeQLDatabasesByRepo, fullName)
	for id, va := range st.CodeQLVariantAnalyses {
		if va.ControllerRepoKey == fullName {
			delete(st.CodeQLVariantAnalyses, id)
			batch.Delete("codeql_variant_analyses", strconv.Itoa(id))
		}
	}
	for id, attestation := range st.Attestations {
		if attestation.RepoID == repo.ID {
			delete(st.Attestations, id)
			batch.Delete("attestations", strconv.Itoa(id))
		}
	}
	for id, alert := range st.DependabotAlerts {
		if alert.RepoKey == fullName {
			delete(st.DependabotAlerts, id)
			batch.Delete("dependabot_alerts", strconv.Itoa(id))
		}
	}
	delete(st.DependabotAlertsByRepo, fullName)
	delete(st.DependabotNextNumber, fullName)
	for id, rs := range st.Rulesets {
		if rs.RepoID == repo.ID {
			delete(st.Rulesets, id)
			batch.Delete("repo_rulesets", strconv.Itoa(id))
		}
	}
	for id, suite := range st.RulesetSuites {
		if suite.RepositoryID == repo.ID {
			delete(st.RulesetSuites, id)
			batch.Delete("ruleset_suites", strconv.Itoa(id))
		}
	}
	for id, project := range st.ProjectClassic {
		if project.RepoKey == fullName {
			delete(st.ProjectClassic, id)
			batch.Delete("projects_classic", strconv.Itoa(id))
			for columnID, column := range st.ProjectColumns {
				if column.ProjectID == id {
					delete(st.ProjectColumns, columnID)
					batch.Delete("project_columns", strconv.Itoa(columnID))
					for cardID, card := range st.ProjectCards {
						if card.ColumnID == columnID {
							delete(st.ProjectCards, cardID)
							batch.Delete("project_cards", strconv.Itoa(cardID))
						}
					}
				}
			}
		}
	}
	for id, cs := range st.Codespaces {
		if cs.RepoKey == fullName {
			delete(st.Codespaces, id)
			delete(st.CodespacesByName, cs.Name)
			batch.Delete("codespaces", strconv.Itoa(id))
		}
	}
	// Enumerate the authoritative package set filtered by owner rather than the
	// PackagesByOwnerKey secondary index: a soft-deleted package is removed from
	// that index but kept in st.Packages, so iterating the index left its rows
	// (and, in repoDeletionIntentLocked, its file bytes) orphaned when the
	// owning repo was deleted (STORE-028).
	for pkgID, pkg := range st.Packages {
		if pkg.OwnerKey != fullName {
			continue
		}
		delete(st.Packages, pkgID)
		batch.Delete("packages", strconv.Itoa(pkg.ID))
		for versionID := range st.PackageVersionsByPackage[pkg.ID] {
			delete(st.PackageVersions, versionID)
			batch.Delete("package_versions", strconv.Itoa(versionID))
			for fileID, file := range st.PackageFiles {
				if file.VersionID == versionID {
					delete(st.PackageFiles, fileID)
					batch.Delete("package_files", strconv.Itoa(fileID))
				}
			}
			delete(st.PackageFilesByVersion, versionID)
		}
		delete(st.PackageVersionsByPackage, pkg.ID)
	}
	delete(st.PackagesByOwnerKey, fullName)
	for id, alert := range st.SecurityAdvisories {
		if alert.RepoID == repo.ID {
			delete(st.SecurityAdvisories, id)
			batch.Delete("security_advisories", strconv.Itoa(id))
		}
	}
	delete(st.SecurityAdvisoriesByRepo, fullName)
	batch.Delete("security_advisories", fullName)
	st.deleteRepoIDReferencesLocked(batch, repo.ID)
	for _, envID := range st.Deployments.DeleteRepoBatch(repo.ID, batch) {
		if _, ok := st.EnvBranchPolicies[envID]; ok {
			delete(st.EnvBranchPolicies, envID)
			batch.Delete("env_branch_policies", strconv.Itoa(envID))
		}
		if _, ok := st.EnvProtectionRules[envID]; ok {
			delete(st.EnvProtectionRules, envID)
			batch.Delete("env_protection_rules", strconv.Itoa(envID))
		}
	}
	for k := range st.EnvSecrets {
		repoKey, _, found := strings.Cut(k, "\x1f")
		if found && repoKey == fullName {
			delete(st.EnvSecrets, k)
			batch.Delete("env_secrets", k)
		}
	}
	for k := range st.EnvVariables {
		repoKey, _, found := strings.Cut(k, "\x1f")
		if found && repoKey == fullName {
			delete(st.EnvVariables, k)
			batch.Delete("env_variables", k)
		}
	}
	issueIDs := map[int]bool{}
	for _, issue := range st.Issues {
		if issue.RepoID == repo.ID {
			issueIDs[issue.ID] = true
		}
	}
	prIDs := map[int]bool{}
	for _, pr := range st.PullRequests {
		if pr.RepoID == repo.ID {
			prIDs[pr.ID] = true
		}
	}
	st.deleteRepoIssueAndPullChildrenLocked(batch, repo.ID, issueIDs, prIDs)
	for id, label := range st.Labels {
		if label.RepoID == repo.ID {
			delete(st.Labels, id)
			batch.Delete("labels", strconv.Itoa(id))
		}
	}
	for id, milestone := range st.Milestones {
		if milestone.RepoID == repo.ID {
			delete(st.Milestones, id)
			batch.Delete("milestones", strconv.Itoa(id))
		}
	}
	for id, issue := range st.Issues {
		if issue.RepoID == repo.ID {
			delete(st.Issues, id)
			st.unindexIssueLocked(issue)
			batch.Delete("issues", strconv.Itoa(id))
		}
	}
	delete(st.IssuesByRepo, repo.ID)
	for id, pr := range st.PullRequests {
		if pr.RepoID == repo.ID {
			delete(st.PullRequests, id)
			st.unindexPullLocked(pr)
			batch.Delete("pull_requests", strconv.Itoa(id))
		}
	}
	delete(st.PullsByRepo, repo.ID)
	releaseIDs := st.Releases.deleteAllForRepoBatch(repo.ID, batch)
	st.Reactions.DeleteParentsBatch("release", releaseIDs, batch)

	// Discussion surfaces — comments first because they reference discussions.
	for id, c := range st.DiscussionComments {
		if d := st.Discussions[c.DiscussionID]; d != nil && d.RepoID == repo.ID {
			delete(st.DiscussionComments, id)
			batch.Delete("discussion_comments", strconv.Itoa(id))
		}
	}
	for id, d := range st.Discussions {
		if d.RepoID == repo.ID {
			delete(st.Discussions, id)
			batch.Delete("discussions", strconv.Itoa(id))
		}
	}
	for id, cat := range st.DiscussionCategories {
		if cat.RepoID == repo.ID {
			delete(st.DiscussionCategories, id)
			batch.Delete("discussion_categories", strconv.Itoa(id))
		}
	}

	// Misc surfaces: branch protection is keyed "repoID:branch", pages
	// builds by "owner/name".
	st.Misc.mu.Lock()
	bpPrefix := strconv.Itoa(repo.ID) + ":"
	for key := range st.Misc.branchProtection {
		if strings.HasPrefix(key, bpPrefix) {
			delete(st.Misc.branchProtection, key)
			batch.Delete("branch_protection", key)
		}
	}
	delete(st.Misc.pagesBuilds, fullName)
	batch.Delete("pages_builds", fullName)
	st.Misc.mu.Unlock()

	if err := batch.Commit(); err != nil {
		return true, pendingDeletion{}, fmt.Errorf("delete repo %s: %w", fullName, err)
	}
	return true, intent, nil
}

func (st *Store) repoWorkflowCleanupIDsLocked(fullName string) (map[string]bool, map[int]bool) {
	planIDs := map[string]bool{}
	addWorkflow := func(workflow *Workflow) {
		if workflow == nil || workflow.RepoFullName != fullName {
			return
		}
		for _, job := range workflow.Jobs {
			if job != nil && job.PlanID != "" {
				planIDs[job.PlanID] = true
			}
		}
	}
	for _, workflow := range st.Workflows {
		addWorkflow(workflow)
	}
	for _, attempts := range st.WorkflowAttempts {
		for _, workflow := range attempts {
			addWorkflow(workflow)
		}
	}
	logIDs := map[int]bool{}
	for planID := range planIDs {
		for _, record := range st.TimelineRecords[planID] {
			if record != nil && record.Log != nil && record.Log.ID != 0 {
				logIDs[record.Log.ID] = true
			}
		}
	}
	return planIDs, logIDs
}

func (st *Store) repoDeletionIntentLocked(repo *Repo) pendingDeletion {
	record := pendingDeletion{
		Kind:      "repo",
		Name:      repo.FullName,
		StartedAt: st.currentTime(),
	}
	addObject := func(key string) {
		if key != "" {
			record.ObjectKeys = append(record.ObjectKeys, key)
		}
	}
	addPackageFile := func(path string) {
		if path == "" {
			return
		}
		if st.ObjectByteStore != nil {
			addObject(path)
		} else {
			record.LocalFiles = append(record.LocalFiles, path)
		}
	}
	for _, cs := range st.Codespaces {
		if cs.RepoKey == repo.FullName {
			record.CodespaceRuntimes = append(record.CodespaceRuntimes, pendingCodespaceRuntime{
				ContainerID: cs.ContainerID, WorkspaceMount: cs.WorkspaceMount,
			})
		}
	}
	// Authoritative set filtered by owner (soft-deleted packages included), so
	// their file bytes are still scheduled for external cleanup (STORE-028).
	for _, pkg := range st.Packages {
		if pkg.OwnerKey != repo.FullName {
			continue
		}
		for versionID := range st.PackageVersionsByPackage[pkg.ID] {
			for _, file := range st.PackageFilesByVersion[versionID] {
				addPackageFile(file.StoragePath)
			}
		}
	}
	if st.ObjectByteStore != nil {
		for _, db := range st.CodeQLDatabasesByRepo[repo.FullName] {
			addObject(db.StoragePath)
		}
		for _, analysis := range st.CodeQLVariantAnalyses {
			if analysis.ControllerRepoKey == repo.FullName {
				addObject(analysis.StoragePath)
			}
		}
		for _, attestation := range st.Attestations {
			if attestation.RepoID == repo.ID {
				addObject(attestation.StoragePath)
			}
		}
	}
	pageKeys, _ := st.pagesPublicationKeysLocked(repo.ID)
	for key := range pageKeys {
		addObject(key)
	}
	st.Releases.appendRepoCleanup(repo.ID, &record)
	sort.Strings(record.ObjectKeys)
	sort.Strings(record.LocalFiles)
	sort.Strings(record.ReleaseAssetObjects)
	sort.Strings(record.ReleaseAssetFiles)
	sort.Strings(record.ActionsObjectKeys)
	sort.Strings(record.ActionsDirectories)
	return record
}

// repoGitStorageIsPathBound reports whether a repository's storer addresses
// its bytes by path. An in-memory storer does not, so it survives a rename
// while a path-bound one has to be reopened against the new location.
func repoGitStorageIsPathBound() bool {
	return GitDataDir() != "" || IsS3GitStorage()
}

func moveRepoGitStorage(oldFull, newFull string) error {
	if err := validateRepoStorageFullName(oldFull); err != nil {
		return err
	}
	if err := validateRepoStorageFullName(newFull); err != nil {
		return err
	}
	if gitDir := GitDataDir(); gitDir != "" {
		oldDir, err := repoGitDirPath(gitDir, oldFull)
		if err != nil {
			return err
		}
		newDir, err := repoGitDirPath(gitDir, newFull)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(newDir), 0o750); err != nil {
			return fmt.Errorf("create git directory %s: %w", filepath.Dir(newDir), err)
		}
		if err := os.Rename(oldDir, newDir); err != nil {
			return fmt.Errorf("move git directory %s -> %s: %w", oldDir, newDir, err)
		}
	}
	if !IsS3GitStorage() {
		return nil
	}
	s3fs, err := getS3FS(context.Background())
	if err != nil {
		return fmt.Errorf("resolve S3 git storage: %w", err)
	}
	if s3fs == nil {
		return nil
	}
	if err := s3fs.renameRepoPrefix(oldFull, newFull); err != nil {
		return fmt.Errorf("move S3 object prefix: %w", err)
	}
	return nil
}

func deleteRepoGitStorage(fullName string) error {
	if err := validateRepoStorageFullName(fullName); err != nil {
		return err
	}
	if gitDir := GitDataDir(); gitDir != "" {
		repoDir, err := repoGitDirPath(gitDir, fullName)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(repoDir); err != nil {
			return fmt.Errorf("remove filesystem git directory %s: %w", repoDir, err)
		}
	}
	if !IsS3GitStorage() {
		return nil
	}
	s3fs, err := getS3FS(context.Background())
	if err != nil {
		return fmt.Errorf("resolve S3 git storage: %w", err)
	}
	if s3fs == nil {
		return nil
	}
	if err := s3fs.deleteRepoPrefix(fullName); err != nil {
		return fmt.Errorf("purge S3 object prefix: %w", err)
	}
	return nil
}

// renameNeedsSlowMove reports whether a rename must move its object bytes
// through a slow backend (S3 — filesystem and in-memory moves are constant-time),
// in which case RenameRepo copies outside the store lock. A test seam forces the
// slow path without a live S3 backend.
func (st *Store) renameNeedsSlowMove() bool {
	if st.repoPrefixCopy != nil {
		return true
	}
	return IsS3GitStorage() && GitDataDir() == ""
}

// copyRepoPrefixBytes/deleteRepoPrefixBytes run the slow object-store prefix
// moves through the test seam when set, else the real S3 helpers.
func (st *Store) copyRepoPrefixBytes(oldFull, newFull string) error {
	if st.repoPrefixCopy != nil {
		return st.repoPrefixCopy(oldFull, newFull)
	}
	return copyRepoGitStorageS3(oldFull, newFull)
}

func (st *Store) deleteRepoPrefixBytes(fullName string) error {
	if st.repoPrefixDelete != nil {
		return st.repoPrefixDelete(fullName)
	}
	return deleteRepoGitStorageS3(fullName)
}

// copyRepoGitStorageS3 copies an S3 object prefix without deleting the source.
func copyRepoGitStorageS3(oldFull, newFull string) error {
	if err := validateRepoStorageFullName(oldFull); err != nil {
		return err
	}
	if err := validateRepoStorageFullName(newFull); err != nil {
		return err
	}
	s3fs, err := getS3FS(context.Background())
	if err != nil {
		return fmt.Errorf("resolve S3 git storage: %w", err)
	}
	if s3fs == nil {
		return nil
	}
	if err := s3fs.copyRepoPrefix(oldFull, newFull); err != nil {
		return fmt.Errorf("copy S3 object prefix: %w", err)
	}
	return nil
}

// deleteRepoGitStorageS3 purges an S3 object prefix (no filesystem side effects).
func deleteRepoGitStorageS3(fullName string) error {
	if err := validateRepoStorageFullName(fullName); err != nil {
		return err
	}
	s3fs, err := getS3FS(context.Background())
	if err != nil {
		return fmt.Errorf("resolve S3 git storage: %w", err)
	}
	if s3fs == nil {
		return nil
	}
	if err := s3fs.deleteRepoPrefix(fullName); err != nil {
		return fmt.Errorf("purge S3 object prefix: %w", err)
	}
	return nil
}

func (st *Store) deleteRepoIDReferencesLocked(batch *persistBatch, repoID int) {
	delete(st.RepoImports, repoID)
	delete(st.DependencySnapshots, repoID)
	delete(st.PagesDeployments, repoID)
	batch.Delete("repo_imports", strconv.Itoa(repoID))
	batch.Delete("dependency_snapshots", strconv.Itoa(repoID))
	batch.Delete("pages_deployments", strconv.Itoa(repoID))
	for id, exp := range st.SBOMExports {
		if exp.RepoID == repoID {
			delete(st.SBOMExports, id)
			batch.Delete("sbom_exports", id)
		}
	}
	for id, task := range st.AgentTasks {
		if task.RepoID == repoID {
			delete(st.AgentTasks, id)
			batch.Delete("agent_tasks", id)
		}
	}
	if st.EnterpriseSettings != nil {
		if kept, changed := removeRepoIDFromList(st.EnterpriseSettings.DependabotAccessibleRepoIDs, repoID); changed {
			st.EnterpriseSettings.DependabotAccessibleRepoIDs = kept
			batch.Put("enterprise_settings", "enterprise", st.EnterpriseSettings)
		}
	}
	for id, a := range st.RepoActivities {
		if a.RepoID == repoID {
			delete(st.RepoActivities, id)
			batch.Delete("repo_activity", strconv.Itoa(id))
		}
	}
	for key, bucket := range st.RepoCloneTraffic {
		if bucket.RepoID == repoID {
			delete(st.RepoCloneTraffic, key)
			batch.Delete("repo_traffic_clones", key)
		}
	}
	for key, sub := range st.RepoSubscriptions {
		if sub != nil && sub.RepoID == repoID {
			delete(st.RepoSubscriptions, key)
			batch.Delete("repo_subscriptions", key)
		}
	}
	delete(st.EnterpriseCodeSecurityRepoConfigs, repoID)
	batch.Delete("enterprise_code_security_attachments", strconv.Itoa(repoID))
	for orgLogin, attachments := range st.CodeSecurityRepoAttachments {
		if _, ok := attachments[repoID]; !ok {
			continue
		}
		delete(attachments, repoID)
		if len(attachments) == 0 {
			delete(st.CodeSecurityRepoAttachments, orgLogin)
			batch.Delete("code_security_repo_attachments", orgLogin)
			continue
		}
		batch.Put("code_security_repo_attachments", orgLogin, attachments)
	}
	for orgLogin, ids := range st.DependabotRepositoryAccess {
		if kept, changed := removeRepoIDFromList(ids, repoID); changed {
			if len(kept) == 0 {
				delete(st.DependabotRepositoryAccess, orgLogin)
				batch.Delete("dependabot_repo_access", orgLogin)
			} else {
				st.DependabotRepositoryAccess[orgLogin] = kept
				batch.Put("dependabot_repo_access", orgLogin, kept)
			}
		}
	}
	for _, inst := range st.Installations {
		if kept, changed := removeRepoIDFromList(inst.SelectedRepoIDs, repoID); changed {
			inst.SelectedRepoIDs = kept
			batch.Put("installations", strconv.Itoa(inst.ID), inst)
		}
	}
	for token, t := range st.InstallationTokens {
		if kept, changed := removeRepoIDFromList(t.RepositoryIDs, repoID); changed {
			t.RepositoryIDs = kept
			batch.Put("installation_tokens", token, t)
		}
	}
	for orgLogin, p := range st.OrgActionsPermissions {
		changed := false
		if kept, ok := removeRepoIDFromList(p.SelectedRepositoryIDs, repoID); ok {
			p.SelectedRepositoryIDs = kept
			changed = true
		}
		if kept, ok := removeRepoIDFromList(p.SelfHostedRunnersSelectedRepoIDs, repoID); ok {
			p.SelfHostedRunnersSelectedRepoIDs = kept
			changed = true
		}
		if changed {
			batch.Put("org_actions_permissions", orgLogin, p)
		}
	}
	for _, g := range st.RunnerGroups {
		if kept, changed := removeRepoIDFromList(g.SelectedRepoIDs, repoID); changed {
			g.SelectedRepoIDs = kept
			batch.Put("runner_groups", strconv.Itoa(g.ID), g)
		}
	}
	for orgLogin, m := range st.OrgSecrets {
		if removeRepoIDFromOrgSecrets(m, repoID) {
			batch.Put("org_secrets", orgLogin, m)
		}
	}
	for orgLogin, m := range st.OrgVariables {
		if removeRepoIDFromActionsVariables(m, repoID) {
			batch.Put("org_variables", orgLogin, m)
		}
	}
	for orgLogin, m := range st.AgentsOrgSecrets {
		if removeRepoIDFromOrgSecrets(m, repoID) {
			batch.Put("agents_org_secrets", orgLogin, m)
		}
	}
	for orgLogin, m := range st.AgentsOrgVariables {
		if removeRepoIDFromActionsVariables(m, repoID) {
			batch.Put("agents_org_variables", orgLogin, m)
		}
	}
	for orgLogin, m := range st.DependabotOrgSecrets {
		changed := false
		for _, sec := range m {
			if kept, ok := removeRepoIDFromList(sec.SelectedRepoIDs, repoID); ok {
				sec.SelectedRepoIDs = kept
				changed = true
			}
		}
		if changed {
			batch.Put("dependabot_org_secrets", orgLogin, m)
		}
	}
	for scope, m := range st.CodespaceSecrets {
		changed := false
		for _, sec := range m {
			if kept, ok := removeRepoIDFromList(sec.SelectedRepoIDs, repoID); ok {
				sec.SelectedRepoIDs = kept
				changed = true
			}
		}
		if changed {
			if len(m) == 0 {
				batch.Delete("codespace_secrets", scope)
			} else {
				batch.Put("codespace_secrets", scope, m)
			}
		}
	}
	for orgLogin, p := range st.CopilotCodingAgentPerms {
		if kept, changed := removeRepoIDFromList(p.SelectedRepositoryIDs, repoID); changed {
			p.SelectedRepositoryIDs = kept
			batch.Put("copilot_coding_agent_permissions", p.OrgLogin, p)
			st.CopilotCodingAgentPerms[orgLogin] = p
		}
	}
	for orgLogin, regs := range st.OrgPrivateRegistries {
		changed := false
		for _, reg := range regs {
			if kept, ok := removeRepoIDFromList(reg.SelectedRepositoryIDs, repoID); ok {
				reg.SelectedRepositoryIDs = kept
				changed = true
			}
		}
		if changed {
			batch.Put("org_private_registries", orgLogin, regs)
		}
	}
	for orgLogin, settings := range st.OrgImmutableReleases {
		if kept, changed := removeRepoIDFromList(settings.SelectedRepositoryIDs, repoID); changed {
			settings.SelectedRepositoryIDs = kept
			batch.Put("org_immutable_releases", orgLogin, settings)
		}
	}
	for id, va := range st.CodeQLVariantAnalyses {
		if va.ControllerRepoKey != "" {
			if repo := st.ReposByName[va.ControllerRepoKey]; repo != nil && repo.ID == repoID {
				delete(st.CodeQLVariantAnalyses, id)
				batch.Delete("codeql_variant_analyses", strconv.Itoa(id))
				continue
			}
		}
		changed := false
		scanned := va.ScannedRepositories[:0]
		for _, task := range va.ScannedRepositories {
			if task.RepoID == repoID {
				changed = true
				continue
			}
			scanned = append(scanned, task)
		}
		if changed {
			va.ScannedRepositories = scanned
		}
		if kept, ok := removeRepoIDFromList(va.NoCodeQLDBRepos, repoID); ok {
			va.NoCodeQLDBRepos = kept
			changed = true
		}
		if changed {
			batch.Put("codeql_variant_analyses", strconv.Itoa(id), va)
		}
	}
}

func (st *Store) deleteRepoFullNameReferencesLocked(batch *persistBatch, fullName string) {
	for _, team := range st.Teams {
		changed := false
		kept := team.RepoNames[:0]
		for _, repoName := range team.RepoNames {
			if repoName == fullName {
				changed = true
				continue
			}
			kept = append(kept, repoName)
		}
		if changed {
			team.RepoNames = kept
			if team.RepoPermissions != nil {
				delete(team.RepoPermissions, fullName)
			}
			team.UpdatedAt = st.currentTime()
			batch.Put("teams", strconv.Itoa(team.ID), team)
		}
	}
	for id, rec := range st.ArtifactStorageRecords {
		if rec.GitHubRepository == fullName {
			delete(st.ArtifactStorageRecords, id)
			batch.Delete("artifact_storage_records", strconv.Itoa(id))
		}
	}
	for id, rec := range st.ArtifactDeploymentRecords {
		if rec.GitHubRepository == fullName {
			delete(st.ArtifactDeploymentRecords, id)
			batch.Delete("artifact_deployment_records", strconv.Itoa(id))
		}
	}
}

func removeRepoIDFromList(ids []int, repoID int) ([]int, bool) {
	kept := ids[:0]
	changed := false
	for _, id := range ids {
		if id == repoID {
			changed = true
			continue
		}
		kept = append(kept, id)
	}
	return kept, changed
}

func removeRepoIDFromOrgSecrets(secrets map[string]*OrgSecret, repoID int) bool {
	changed := false
	for _, sec := range secrets {
		if kept, ok := removeRepoIDFromList(sec.SelectedRepoIDs, repoID); ok {
			sec.SelectedRepoIDs = kept
			changed = true
		}
	}
	return changed
}

func removeRepoIDFromActionsVariables(vars map[string]*ActionsVariable, repoID int) bool {
	changed := false
	for _, v := range vars {
		if kept, ok := removeRepoIDFromList(v.SelectedRepoIDs, repoID); ok {
			v.SelectedRepoIDs = kept
			changed = true
		}
	}
	return changed
}

func (st *Store) deleteRepoIssueAndPullChildrenLocked(batch *persistBatch, repoID int, issueIDs, prIDs map[int]bool) {
	if len(issueIDs) == 0 && len(prIDs) == 0 {
		return
	}
	for issueID := range issueIDs {
		delete(st.IssueFieldValues, issueID)
		batch.Delete("issue_field_values", strconv.Itoa(issueID))
	}
	st.ProjectsV2.DeleteContentItemsBatch("Issue", issueIDs, batch)
	st.ProjectsV2.DeleteContentItemsBatch("PullRequest", prIDs, batch)
	threadIDs := make([]string, 0, len(issueIDs)+len(prIDs))
	for issueID := range issueIDs {
		threadIDs = append(threadIDs, notificationThreadID("Issue", issueID))
	}
	for prID := range prIDs {
		threadIDs = append(threadIDs, notificationThreadID("PullRequest", prID))
	}
	st.deleteNotificationThreadStateBatchLocked(batch, threadIDs)
	for id, c := range st.Comments {
		if (c.ParentType == "issue" && issueIDs[c.IssueID]) || (c.ParentType == "pull_request" && prIDs[c.IssueID]) {
			delete(st.Comments, id)
			key := commentCountKey(c.ParentType, c.IssueID)
			delete(st.CommentCounts, key)
			delete(st.CommentsByParent, key) // whole parent is being deleted
			st.Reactions.DeleteParent(c.ParentType+"_comment", id)
			batch.Delete("comments", strconv.Itoa(id))
		}
	}
	for id, e := range st.IssueEvents {
		if e.RepoID == repoID || (e.ParentType == "issue" && issueIDs[e.IssueID]) || (e.ParentType == "pull_request" && prIDs[e.IssueID]) {
			delete(st.IssueEvents, id)
			batch.Delete("issue_events", strconv.Itoa(id))
		}
	}
	for parentID, children := range st.SubIssueLists {
		if issueIDs[parentID] {
			for _, childID := range children {
				delete(st.SubIssueParent, childID)
			}
			delete(st.SubIssueLists, parentID)
			batch.Delete("sub_issues", strconv.Itoa(parentID))
			continue
		}
		kept := children[:0]
		changed := false
		for _, childID := range children {
			if issueIDs[childID] {
				delete(st.SubIssueParent, childID)
				changed = true
				continue
			}
			kept = append(kept, childID)
		}
		if !changed {
			continue
		}
		if len(kept) == 0 {
			delete(st.SubIssueLists, parentID)
			batch.Delete("sub_issues", strconv.Itoa(parentID))
		} else {
			st.SubIssueLists[parentID] = kept
			batch.Put("sub_issues", strconv.Itoa(parentID), kept)
		}
	}
	for childID, parentID := range st.SubIssueParent {
		if issueIDs[childID] || issueIDs[parentID] {
			delete(st.SubIssueParent, childID)
		}
	}
	for issueID, blockers := range st.IssueBlockedBy {
		if issueIDs[issueID] {
			delete(st.IssueBlockedBy, issueID)
			batch.Delete("issue_blocked_by", strconv.Itoa(issueID))
			continue
		}
		kept := blockers[:0]
		changed := false
		for _, blockerID := range blockers {
			if issueIDs[blockerID] {
				changed = true
				continue
			}
			kept = append(kept, blockerID)
		}
		if !changed {
			continue
		}
		if len(kept) == 0 {
			delete(st.IssueBlockedBy, issueID)
			batch.Delete("issue_blocked_by", strconv.Itoa(issueID))
		} else {
			st.IssueBlockedBy[issueID] = kept
			batch.Put("issue_blocked_by", strconv.Itoa(issueID), kept)
		}
	}
	for id, r := range st.PRReviews {
		if prIDs[r.PRID] {
			delete(st.PRReviews, id)
			batch.Delete("pr_reviews", strconv.Itoa(id))
		}
	}
	for prID := range prIDs {
		delete(st.PRReviewsByPR, prID)
		st.Reactions.DeleteParentsBatch("pull_request_comment", st.PRReviewComments.IDsForPR(prID), batch)
		st.PRReviewComments.DeleteForPRBatch(prID, batch)
	}
	st.Reactions.DeleteParentsBatch("issue", issueIDs, batch)
	st.Reactions.DeleteParentsBatch("pull_request", prIDs, batch)
}

// ListForks returns all repositories whose ParentID or SourceID matches
// sourceRepoID, sorted/paged according to opts.
func (st *Store) ListForks(sourceRepoID int, opts RepoListOptions) []*Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()

	var repos []*Repo
	for _, r := range st.Repos {
		if r.Fork && (r.ParentID == sourceRepoID || r.SourceID == sourceRepoID) {
			repos = append(repos, r)
		}
	}
	return snapshotRepos(filterSortPaginateRepos(repos, opts))
}

// CountForks returns how many repositories were forked from the given
// repository (matched by ParentID or SourceID lineage).
func (st *Store) CountForks(sourceRepoID int) int {
	st.mu.RLock()
	defer st.mu.RUnlock()

	n := 0
	for _, r := range st.Repos {
		if r.Fork && (r.ParentID == sourceRepoID || r.SourceID == sourceRepoID) {
			n++
		}
	}
	return n
}

func (st *Store) ListReposByOwner(login string) []*Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()

	prefix := login + "/"
	var repos []*Repo
	for k, r := range st.ReposByName {
		if strings.HasPrefix(k, prefix) {
			repos = append(repos, r)
		}
	}
	return snapshotRepos(repos)
}

// RepoListOptions controls filtering, sorting and pagination for repo list
// endpoints. A zero value applies GitHub's defaults. Set NoPaginate when the
// caller will paginate itself (e.g. REST handlers use paginateAndLink).
type RepoListOptions struct {
	Type        string // org: all/public/private/forks/sources/member; user: all/owner/member
	Visibility  string // all/public/private
	Affiliation string // owner,collaborator,organization_member
	Sort        string // created/updated/pushed/full_name
	Direction   string // asc/desc
	PerPage     int
	Page        int
	NoPaginate  bool
}

func (o RepoListOptions) normalize() RepoListOptions {
	if !o.NoPaginate {
		if o.PerPage <= 0 {
			o.PerPage = 30
		}
		if o.PerPage > 100 {
			o.PerPage = 100
		}
		if o.Page <= 0 {
			o.Page = 1
		}
	}
	if o.Sort == "" {
		o.Sort = "created"
	}
	if o.Direction == "" {
		if o.Sort == "full_name" {
			o.Direction = "asc"
		} else {
			o.Direction = "desc"
		}
	}
	return o
}

// ListReposForOrg returns repos owned by an organization, filtered/sorted/paged.
func (st *Store) ListReposForOrg(org string, opts RepoListOptions) []*Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()

	prefix := org + "/"
	var repos []*Repo
	for k, r := range st.ReposByName {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if r.OwnerType != "Organization" {
			continue
		}
		repos = append(repos, r)
	}
	return snapshotRepos(filterSortPaginateRepos(repos, opts))
}

// ListReposForUser returns public repos owned by a user, filtered/sorted/paged.
func (st *Store) ListReposForUser(user *User, opts RepoListOptions) []*Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()

	prefix := user.Login + "/"
	var repos []*Repo
	for k, r := range st.ReposByName {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if r.OwnerType != "User" {
			continue
		}
		if r.Private {
			continue
		}
		repos = append(repos, r)
	}
	return snapshotRepos(filterSortPaginateRepos(repos, opts))
}

// ListReposForAuthUser returns repos the authenticated user can access.
// Affiliation controls owner/collaborator/org-member inclusion.
func (st *Store) ListReposForAuthUser(user *User, opts RepoListOptions) []*Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()

	affiliation := opts.Affiliation
	if affiliation == "" {
		affiliation = "owner,collaborator,organization_member"
	}
	includeOwner := strings.Contains(affiliation, "owner")
	includeCollab := strings.Contains(affiliation, "collaborator")
	includeOrgMember := strings.Contains(affiliation, "organization_member")

	seen := make(map[int]bool)
	var repos []*Repo

	// owner affiliation
	if includeOwner {
		prefix := user.Login + "/"
		for k, r := range st.ReposByName {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			if r.OwnerType != "User" {
				continue
			}
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			repos = append(repos, r)
		}
	}

	// collaborator affiliation: repositories where the user has been added as
	// a collaborator.
	if includeCollab {
		for fullName, perms := range st.RepoCollaborators {
			if _, ok := perms[user.Login]; !ok {
				continue
			}
			repo := st.ReposByName[fullName]
			if repo == nil || seen[repo.ID] {
				continue
			}
			seen[repo.ID] = true
			repos = append(repos, repo)
		}
	}

	// organization_member affiliation: every repository owned by an org the
	// user is a member of.
	if includeOrgMember {
		for _, org := range st.Orgs {
			if st.Memberships[membershipKey(org.Login, user.ID)] == nil {
				continue
			}
			prefix := org.Login + "/"
			for k, r := range st.ReposByName {
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				if seen[r.ID] {
					continue
				}
				seen[r.ID] = true
				repos = append(repos, r)
			}
		}
	}

	return snapshotRepos(filterSortPaginateRepos(repos, opts))
}

func filterSortPaginateRepos(repos []*Repo, opts RepoListOptions) []*Repo {
	repos = filterSortRepos(repos, opts)
	if opts.NoPaginate {
		return repos
	}

	opts = opts.normalize()

	// paginate
	start := (opts.Page - 1) * opts.PerPage
	if start > len(repos) {
		return []*Repo{}
	}
	end := start + opts.PerPage
	if end > len(repos) {
		end = len(repos)
	}
	return repos[start:end]
}

// filterSortRepos applies filtering and sorting without pagination.
func filterSortRepos(repos []*Repo, opts RepoListOptions) []*Repo {
	opts = opts.normalize()

	// visibility filter
	switch opts.Visibility {
	case "public":
		filtered := repos[:0]
		for _, r := range repos {
			if !r.Private {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	case "private":
		filtered := repos[:0]
		for _, r := range repos {
			if r.Private {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	}

	// type filter
	switch opts.Type {
	case "public":
		filtered := repos[:0]
		for _, r := range repos {
			if !r.Private {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	case "private":
		filtered := repos[:0]
		for _, r := range repos {
			if r.Private {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	case "forks":
		filtered := repos[:0]
		for _, r := range repos {
			if r.Fork {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	case "sources":
		filtered := repos[:0]
		for _, r := range repos {
			if !r.Fork {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	case "owner":
		// For user endpoints this means repos owned by the user; auth user
		// endpoints use affiliation instead. Keep all repos already scoped.
	case "member":
		// bleephub does not model team-based repo membership; empty.
		repos = repos[:0]
	}

	// sort
	sort.SliceStable(repos, func(i, j int) bool {
		var less bool
		switch opts.Sort {
		case "updated":
			less = repos[i].UpdatedAt.Before(repos[j].UpdatedAt)
		case "pushed":
			less = repos[i].PushedAt.Before(repos[j].PushedAt)
		case "full_name":
			less = repos[i].FullName < repos[j].FullName
		case "stargazers": // forks endpoint: stargazers/watchers both order by star count
			less = repos[i].StargazersCount < repos[j].StargazersCount
		default: // "created"
			less = repos[i].CreatedAt.Before(repos[j].CreatedAt)
		}
		if opts.Direction == "asc" {
			return less
		}
		return !less
	})

	return repos
}

func (st *Store) GetGitStorage(owner, name string) gitStorage.Storer {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.GitStorages[owner+"/"+name]
}

func (st *Store) GitStorageForRepoID(repoID int) (gitStorage.Storer, string) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	repo := st.Repos[repoID]
	if repo == nil {
		return nil, ""
	}
	return st.GitStorages[repo.FullName], repo.FullName
}

// RepoSize returns the on-disk size of the repository's git storage in
// kilobytes, matching GitHub's `size` field unit. For in-memory storage the
// result is 0; for S3-backed storage the result is also 0 until a real
// list-objects sum is implemented.
func (st *Store) RepoSize(fullName string) int64 {
	if IsS3GitStorage() {
		return 0
	}
	gitDir := GitDataDir()
	if gitDir == "" {
		return 0
	}
	repoDir, err := repoGitDirPath(gitDir, fullName)
	if err != nil {
		return 0
	}
	var total int64
	_ = filepath.Walk(repoDir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total / 1024
}

// RepoPermission is the access level a collaborator has on a repo.
type RepoPermission string

const (
	RepoPermPull  RepoPermission = "pull"
	RepoPermPush  RepoPermission = "push"
	RepoPermAdmin RepoPermission = "admin"
)

// AddRepoCollaborator grants permission to login on the repo. Only pull,
// push, and admin are accepted (matches GitHub). Returns true if the repo
// exists and the user exists.
func (st *Store) AddRepoCollaborator(owner, name, login, permission string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	fullName := owner + "/" + name
	repo, ok := st.ReposByName[fullName]
	if !ok {
		return false
	}
	u := st.UsersByLogin[login]
	if u == nil {
		return false
	}
	perm := normalizeRepoPermission(permission)
	if st.RepoCollaborators[fullName] == nil {
		st.RepoCollaborators[fullName] = map[string]string{}
	}
	st.RepoCollaborators[fullName][login] = perm
	repo.UpdatedAt = st.currentTime()
	// One transaction: the collaborator set and the repo's updated_at must not
	// disagree across a crash mid-persist.
	batch := newPersistBatch(st.persist)
	batch.Put("repo_collaborators", fullName, st.RepoCollaborators[fullName])
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repo_collaborators", err: err})
	}
	return true
}

// RemoveRepoCollaborator removes a collaborator from the repo. Returns true
// if one was removed.
func (st *Store) RemoveRepoCollaborator(owner, name, login string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	fullName := owner + "/" + name
	repo, ok := st.ReposByName[fullName]
	if !ok {
		return false
	}
	if st.RepoCollaborators[fullName] == nil {
		return false
	}
	if _, ok := st.RepoCollaborators[fullName][login]; !ok {
		return false
	}
	delete(st.RepoCollaborators[fullName], login)
	repo.UpdatedAt = st.currentTime()
	// One transaction: the collaborator set and the repo's updated_at must not
	// disagree across a crash mid-persist.
	batch := newPersistBatch(st.persist)
	batch.Put("repo_collaborators", fullName, st.RepoCollaborators[fullName])
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repo_collaborators", err: err})
	}
	return true
}

// GetRepoCollaboratorPermission returns the permission string for a
// collaborator, or "" if none.
func (st *Store) GetRepoCollaboratorPermission(owner, name, login string) string {
	st.mu.RLock()
	defer st.mu.RUnlock()

	fullName := owner + "/" + name
	if st.RepoCollaborators[fullName] == nil {
		return ""
	}
	return st.RepoCollaborators[fullName][login]
}

// ListRepoCollaborators returns the collaborators of a repo.
func (st *Store) ListRepoCollaborators(owner, name string) map[string]string {
	st.mu.RLock()
	defer st.mu.RUnlock()

	fullName := owner + "/" + name
	out := make(map[string]string, len(st.RepoCollaborators[fullName]))
	for k, v := range st.RepoCollaborators[fullName] {
		out[k] = v
	}
	return out
}

func normalizeRepoPermission(p string) string {
	switch strings.ToLower(p) {
	case "admin":
		return "admin"
	case "push", "write":
		return "push"
	case "pull", "read", "":
		return "pull"
	default:
		return "pull"
	}
}

// StarRepo adds userID to the repo's stargazers and records the starred repo
// on the user. Idempotent. Returns true if the star was newly added.
func (st *Store) StarRepo(userID int, owner, name string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	fullName := owner + "/" + name
	repo, ok := st.ReposByName[fullName]
	if !ok {
		return false
	}
	user, ok := st.Users[userID]
	if !ok {
		return false
	}
	if repo.Stargazers == nil {
		repo.Stargazers = map[int]bool{}
	}
	if user.StarredRepos == nil {
		user.StarredRepos = map[string]bool{}
	}
	if repo.Stargazers[userID] {
		return false
	}
	repo.Stargazers[userID] = true
	repo.StargazersCount = len(repo.Stargazers)
	repo.UpdatedAt = st.currentTime()
	user.StarredRepos[fullName] = true
	user.UpdatedAt = st.currentTime()
	// One transaction: the repo's stargazer count and the user's starred list
	// must never disagree across a crash mid-persist.
	batch := newPersistBatch(st.persist)
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	batch.Put("users", strconv.Itoa(user.ID), user)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", err: err})
	}
	return true
}

// UnstarRepo removes userID from the repo's stargazers. Returns true if a
// star was actually removed.
func (st *Store) UnstarRepo(userID int, owner, name string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	fullName := owner + "/" + name
	repo, ok := st.ReposByName[fullName]
	if !ok {
		return false
	}
	user, ok := st.Users[userID]
	if !ok {
		return false
	}
	if repo.Stargazers == nil || !repo.Stargazers[userID] {
		return false
	}
	delete(repo.Stargazers, userID)
	repo.StargazersCount = len(repo.Stargazers)
	repo.UpdatedAt = st.currentTime()
	if user.StarredRepos != nil {
		delete(user.StarredRepos, fullName)
	}
	user.UpdatedAt = st.currentTime()
	// One transaction: the repo's stargazer count and the user's starred list
	// must never disagree across a crash mid-persist.
	batch := newPersistBatch(st.persist)
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	batch.Put("users", strconv.Itoa(user.ID), user)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", err: err})
	}
	return true
}

// IsRepoStarredBy reports whether userID has starred the repo.
func (st *Store) IsRepoStarredBy(userID int, owner, name string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()

	repo, ok := st.ReposByName[owner+"/"+name]
	if !ok || repo.Stargazers == nil {
		return false
	}
	return repo.Stargazers[userID]
}

// ListRepoStargazers returns the user IDs who starred the repo, sorted
// ascending.
func (st *Store) ListRepoStargazers(owner, name string) []int {
	st.mu.RLock()
	defer st.mu.RUnlock()

	repo, ok := st.ReposByName[owner+"/"+name]
	if !ok || repo.Stargazers == nil {
		return nil
	}
	out := make([]int, 0, len(repo.Stargazers))
	for id := range repo.Stargazers {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// ListStarredRepos returns the full names of repos starred by userID.
func (st *Store) ListStarredRepos(userID int) []string {
	st.mu.RLock()
	defer st.mu.RUnlock()

	user, ok := st.Users[userID]
	if !ok || user.StarredRepos == nil {
		return nil
	}
	out := make([]string, 0, len(user.StarredRepos))
	for name := range user.StarredRepos {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RepoDeployKey represents a deploy key configured on a repository.
type RepoDeployKey struct {
	ID        int       `json:"id"`
	NodeID    string    `json:"node_id"`
	RepoID    int       `json:"repo_id"`
	Title     string    `json:"title"`
	Key       string    `json:"key"`
	ReadOnly  bool      `json:"read_only"`
	Verified  bool      `json:"verified"`
	CreatedAt time.Time `json:"created_at"`
}

// ListRepoDeployKeys returns deploy keys for a repo, sorted by ID.
func (st *Store) ListRepoDeployKeys(repoID int) []*RepoDeployKey {
	st.mu.RLock()
	defer st.mu.RUnlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return nil
	}
	keys := st.RepoDeployKeys[repo.FullName]
	out := make([]*RepoDeployKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

// GetRepoDeployKey returns a deploy key by ID.
func (st *Store) GetRepoDeployKey(id int) *RepoDeployKey {
	st.mu.RLock()
	defer st.mu.RUnlock()

	for _, keys := range st.RepoDeployKeys {
		if k := keys[id]; k != nil {
			return k
		}
	}
	return nil
}

// CreateRepoDeployKey adds a deploy key to a repo.
func (st *Store) CreateRepoDeployKey(repoID int, title, key string, readOnly bool) *RepoDeployKey {
	st.mu.Lock()
	defer st.mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return nil
	}
	if st.RepoDeployKeys[repo.FullName] == nil {
		st.RepoDeployKeys[repo.FullName] = map[int]*RepoDeployKey{}
	}
	k := &RepoDeployKey{
		ID:        st.NextDeployKeyID,
		NodeID:    fmt.Sprintf("RkEAxNNa%07d", st.NextDeployKeyID),
		RepoID:    repoID,
		Title:     title,
		Key:       key,
		ReadOnly:  readOnly,
		Verified:  true,
		CreatedAt: st.currentTime(),
	}
	st.RepoDeployKeys[repo.FullName][k.ID] = k
	st.NextDeployKeyID++
	repo.UpdatedAt = st.currentTime()
	// One transaction: the deploy-key set and the repo's updated_at must not
	// disagree across a crash mid-persist.
	batch := newPersistBatch(st.persist)
	batch.Put("repo_deploy_keys", repo.FullName, st.RepoDeployKeys[repo.FullName])
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repo_deploy_keys", err: err})
	}
	return k
}

// DeleteRepoDeployKey removes a deploy key by ID.
func (st *Store) DeleteRepoDeployKey(id int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	for repoKey, keys := range st.RepoDeployKeys {
		if k := keys[id]; k != nil {
			delete(keys, id)
			// One transaction: the deploy-key set and the repo's updated_at must
			// not disagree across a crash mid-persist.
			batch := newPersistBatch(st.persist)
			if repo := st.Repos[k.RepoID]; repo != nil {
				repo.UpdatedAt = st.currentTime()
				batch.Put("repos", strconv.Itoa(repo.ID), repo)
			}
			batch.Put("repo_deploy_keys", repoKey, st.RepoDeployKeys[repoKey])
			if err := batch.Commit(); err != nil {
				panic(&persistenceFailure{op: "batch", bucket: "repo_deploy_keys", err: err})
			}
			return true
		}
	}
	return false
}

// RepoSubscription records a user's watch subscription for a repo.
type RepoSubscription struct {
	UserID     int       `json:"user_id"`
	RepoID     int       `json:"repo_id"`
	Subscribed bool      `json:"subscribed"`
	Ignored    bool      `json:"ignored"`
	CreatedAt  time.Time `json:"created_at"`
}

func repoSubscriptionKey(userID, repoID int) string {
	return strconv.Itoa(userID) + ":" + strconv.Itoa(repoID)
}

// SetRepoSubscription creates or updates a subscription.
func (st *Store) SetRepoSubscription(userID int, repoID int, subscribed bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.Repos[repoID] == nil {
		return false
	}
	key := repoSubscriptionKey(userID, repoID)
	sub := &RepoSubscription{
		UserID:     userID,
		RepoID:     repoID,
		Subscribed: subscribed,
		Ignored:    false,
		CreatedAt:  st.currentTime(),
	}
	if existing := st.RepoSubscriptions[key]; existing != nil {
		sub.CreatedAt = existing.CreatedAt
	}
	st.RepoSubscriptions[key] = sub
	if st.persist != nil {
		st.persist.MustPut("repo_subscriptions", key, sub)
	}
	return true
}

// DeleteRepoSubscription removes a subscription.
func (st *Store) DeleteRepoSubscription(userID int, repoID int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	key := repoSubscriptionKey(userID, repoID)
	if st.RepoSubscriptions[key] == nil {
		return false
	}
	delete(st.RepoSubscriptions, key)
	if st.persist != nil {
		st.persist.MustDelete("repo_subscriptions", key)
	}
	return true
}

// GetRepoSubscription returns a subscription or nil.
func (st *Store) GetRepoSubscription(userID int, repoID int) *RepoSubscription {
	st.mu.RLock()
	defer st.mu.RUnlock()

	// SetRepoSubscription swaps in a fresh row rather than mutating in place, so
	// this is already race-free; return a detached value copy anyway (the struct
	// has no reference fields) to keep the getter safe against a future in-place
	// writer.
	sub := st.RepoSubscriptions[repoSubscriptionKey(userID, repoID)]
	if sub == nil {
		return nil
	}
	clone := *sub
	return &clone
}

// ListRepoSubscriptionsForUser returns the repositories subscribed by userID.
func (st *Store) ListRepoSubscriptionsForUser(userID int) []*Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*Repo, 0)
	for key, sub := range st.RepoSubscriptions {
		if sub == nil || sub.UserID != userID || !sub.Subscribed {
			continue
		}
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		repoID, _ := strconv.Atoi(parts[1])
		if repo := st.Repos[repoID]; repo != nil {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotRepos(out)
}

// TransferRepo transfers ownership of a repository to a new owner account.
// It returns true on success.
func (st *Store) TransferRepo(owner, name, newOwner string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	oldFull := owner + "/" + name
	newFull := newOwner + "/" + name
	if oldFull == newFull {
		return true
	}
	repo, ok := st.ReposByName[oldFull]
	if !ok {
		return false
	}
	if _, exists := st.ReposByName[newFull]; exists {
		return false
	}

	newOwnerUser := st.UsersByLogin[newOwner]
	var newOwnerOrg *Org
	if newOwnerUser == nil {
		newOwnerOrg = st.OrgsByLogin[newOwner]
	}
	if newOwnerUser == nil && newOwnerOrg == nil {
		return false
	}

	if err := moveRepoGitStorage(oldFull, newFull); err != nil {
		st.logger.Error().Str("from", oldFull).Str("to", newFull).Err(err).Msg("transfer repo failed")
		return false
	}
	stor := st.GitStorages[oldFull]
	if stor != nil && repoGitStorageIsPathBound() {
		reopened, err := openOrInitGitStorage(context.Background(), newFull)
		if err != nil {
			st.logger.Error().Str("from", oldFull).Str("to", newFull).Err(err).Msg("transfer repo: reopen git storage failed")
			return false
		}
		stor = reopened
	}

	repo.FullName = newFull
	repo.UpdatedAt = st.currentTime()
	if newOwnerOrg != nil {
		repo.OwnerType = "Organization"
		repo.OwnerID = newOwnerOrg.ID
		repo.Owner = nil
	} else if newOwnerUser != nil {
		repo.OwnerType = "User"
		repo.OwnerID = newOwnerUser.ID
		repo.Owner = newOwnerUser
	}

	st.ReposByName[newFull] = repo
	delete(st.ReposByName, oldFull)

	if stor != nil {
		st.GitStorages[newFull] = stor
		delete(st.GitStorages, oldFull)
	}

	// Re-key the repos row and every subresource bucket in one transaction, so a
	// crash can never leave the repository split across its old and new names.
	batch := newPersistBatch(st.persist)
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	st.moveRepoKeyLocked(batch, oldFull, newFull)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", err: err})
	}
	return true
}

// moveRepoKeyLocked renames all in-memory maps keyed by repo full name from
// oldFull to newFull, staging every durable re-key into batch so the whole move
// commits in one transaction. Caller must hold st.mu.
func (st *Store) moveRepoKeyLocked(batch *persistBatch, oldFull, newFull string) {
	st.moveNotificationRepoKeyBatchLocked(batch, oldFull, newFull)
	if v := st.RepoSecrets[oldFull]; v != nil {
		st.RepoSecrets[newFull] = v
		delete(st.RepoSecrets, oldFull)
		if st.persist != nil {
			batch.Put("repo_secrets", newFull, v)
			batch.Delete("repo_secrets", oldFull)
		}
	}
	if v := st.RepoVariables[oldFull]; v != nil {
		st.RepoVariables[newFull] = v
		delete(st.RepoVariables, oldFull)
		if st.persist != nil {
			batch.Put("repo_variables", newFull, v)
			batch.Delete("repo_variables", oldFull)
		}
	}
	if v := st.RepoCollaborators[oldFull]; v != nil {
		st.RepoCollaborators[newFull] = v
		delete(st.RepoCollaborators, oldFull)
		if st.persist != nil {
			batch.Put("repo_collaborators", newFull, v)
			batch.Delete("repo_collaborators", oldFull)
		}
	}
	for _, team := range st.Teams {
		changed := false
		for i, repoName := range team.RepoNames {
			if repoName == oldFull {
				team.RepoNames[i] = newFull
				changed = true
			}
		}
		if team.RepoPermissions != nil {
			if perm, ok := team.RepoPermissions[oldFull]; ok {
				team.RepoPermissions[newFull] = perm
				delete(team.RepoPermissions, oldFull)
				changed = true
			}
		}
		if changed {
			team.UpdatedAt = st.currentTime()
			if st.persist != nil {
				batch.Put("teams", strconv.Itoa(team.ID), team)
			}
		}
	}
	for _, rec := range st.ArtifactStorageRecords {
		if rec.GitHubRepository == oldFull {
			rec.GitHubRepository = newFull
			rec.UpdatedAt = st.currentTime()
			if st.persist != nil {
				batch.Put("artifact_storage_records", strconv.Itoa(rec.ID), rec)
			}
		}
	}
	for _, rec := range st.ArtifactDeploymentRecords {
		if rec.GitHubRepository == oldFull {
			rec.GitHubRepository = newFull
			rec.UpdatedAt = st.currentTime()
			if st.persist != nil {
				batch.Put("artifact_deployment_records", strconv.Itoa(rec.ID), rec)
			}
		}
	}
	if v := st.Hooks[oldFull]; v != nil {
		st.Hooks[newFull] = v
		delete(st.Hooks, oldFull)
		if st.persist != nil {
			batch.Put("hooks", newFull, v)
			batch.Delete("hooks", oldFull)
		}
	}
	if v := st.CheckSuitePrefs[oldFull]; v != nil {
		st.CheckSuitePrefs[newFull] = v
		delete(st.CheckSuitePrefs, oldFull)
		if st.persist != nil {
			batch.Put("check_suite_prefs", newFull, v)
			batch.Delete("check_suite_prefs", oldFull)
		}
	}
	for _, suite := range st.CheckSuites {
		if suite.RepoKey == oldFull {
			suite.RepoKey = newFull
			if st.persist != nil {
				batch.Put("check_suites", strconv.FormatInt(suite.ID, 10), suite)
			}
		}
	}
	for _, run := range st.CheckRuns {
		if run.RepoKey == oldFull {
			run.RepoKey = newFull
			if st.persist != nil {
				batch.Put("check_runs", strconv.FormatInt(run.ID, 10), run)
			}
		}
	}
	for _, wf := range st.Workflows {
		if wf.RepoFullName == oldFull {
			wf.RepoFullName = newFull
			batch.Put("workflows", wf.ID, wf)
		}
	}
	for runID, attempts := range st.WorkflowAttempts {
		changed := false
		for _, wf := range attempts {
			if wf.RepoFullName == oldFull {
				wf.RepoFullName = newFull
				changed = true
			}
		}
		if changed {
			if len(attempts) == 0 {
				batch.Delete("workflow_attempts", strconv.Itoa(runID))
			} else {
				batch.Put("workflow_attempts", strconv.Itoa(runID), attempts)
			}
		}
	}
	st.CommitStatuses.moveRepoKeyBatch(oldFull, newFull, batch)
	if v := st.RepoAutolinks[oldFull]; v != nil {
		for _, a := range v {
			a.RepoKey = newFull
		}
		st.RepoAutolinks[newFull] = v
		delete(st.RepoAutolinks, oldFull)
		if st.persist != nil {
			batch.Put("repo_autolinks", newFull, v)
			batch.Delete("repo_autolinks", oldFull)
		}
	}
	if v := st.RepoInvitations[oldFull]; v != nil {
		for _, inv := range v {
			inv.RepoKey = newFull
		}
		st.RepoInvitations[newFull] = v
		delete(st.RepoInvitations, oldFull)
		if st.persist != nil {
			batch.Put("repo_invitations", newFull, v)
			batch.Delete("repo_invitations", oldFull)
		}
	}
	if v := st.RepoDeployKeys[oldFull]; v != nil {
		st.RepoDeployKeys[newFull] = v
		delete(st.RepoDeployKeys, oldFull)
		if st.persist != nil {
			batch.Put("repo_deploy_keys", newFull, v)
			batch.Delete("repo_deploy_keys", oldFull)
		}
	}
	if v := st.SecretScanningAlertsByRepo[oldFull]; v != nil {
		st.SecretScanningAlertsByRepo[newFull] = v
		for _, alert := range v {
			alert.RepoKey = newFull
			if st.persist != nil {
				batch.Put("secret_scanning_alerts", strconv.Itoa(alert.ID), alert)
			}
		}
		delete(st.SecretScanningAlertsByRepo, oldFull)
	}
	if v := st.SecretScanningNextNumber[oldFull]; v != 0 {
		st.SecretScanningNextNumber[newFull] = v
		delete(st.SecretScanningNextNumber, oldFull)
	}
	if v := st.CodeScanningAlertsByRepo[oldFull]; v != nil {
		st.CodeScanningAlertsByRepo[newFull] = v
		for _, alert := range v {
			alert.RepoKey = newFull
			if st.persist != nil {
				batch.Put("code_scanning_alerts", strconv.Itoa(alert.ID), alert)
			}
		}
		delete(st.CodeScanningAlertsByRepo, oldFull)
		if st.persist != nil {
			batch.Delete("code_scanning_alerts", oldFull)
		}
	}
	if v := st.CodeScanningNextNumber[oldFull]; v != 0 {
		st.CodeScanningNextNumber[newFull] = v
		delete(st.CodeScanningNextNumber, oldFull)
	}
	if v := st.CodeScanningAnalysesByRepo[oldFull]; v != nil {
		st.CodeScanningAnalysesByRepo[newFull] = v
		for _, a := range v {
			a.RepoKey = newFull
			if st.persist != nil {
				batch.Put("code_scanning_analyses", strconv.Itoa(a.ID), a)
			}
		}
		delete(st.CodeScanningAnalysesByRepo, oldFull)
		if st.persist != nil {
			batch.Delete("code_scanning_analyses", oldFull)
		}
	}
	if v := st.DependabotAlertsByRepo[oldFull]; v != nil {
		st.DependabotAlertsByRepo[newFull] = v
		for _, alert := range v {
			alert.RepoKey = newFull
			if st.persist != nil {
				batch.Put("dependabot_alerts", strconv.Itoa(alert.ID), alert)
			}
		}
		delete(st.DependabotAlertsByRepo, oldFull)
		if st.persist != nil {
			batch.Delete("dependabot_alerts", oldFull)
		}
	}
	if v := st.DependabotNextNumber[oldFull]; v != 0 {
		st.DependabotNextNumber[newFull] = v
		delete(st.DependabotNextNumber, oldFull)
	}
	if v := st.DependabotSecrets[oldFull]; v != nil {
		st.DependabotSecrets[newFull] = v
		delete(st.DependabotSecrets, oldFull)
		if st.persist != nil {
			batch.Put("dependabot_secrets", newFull, v)
			batch.Delete("dependabot_secrets", oldFull)
		}
	}
	if v := st.CodeScanningDefaultSetups[oldFull]; v != nil {
		v.RepoKey = newFull
		st.CodeScanningDefaultSetups[newFull] = v
		delete(st.CodeScanningDefaultSetups, oldFull)
		if st.persist != nil {
			batch.Put("code_scanning_default_setups", newFull, v)
			batch.Delete("code_scanning_default_setups", oldFull)
		}
	}
	if v := st.CodeQualitySetups[oldFull]; v != nil {
		v.RepoFullName = newFull
		st.CodeQualitySetups[newFull] = v
		delete(st.CodeQualitySetups, oldFull)
		if st.persist != nil {
			batch.Put("code_quality_setups", newFull, v)
			batch.Delete("code_quality_setups", oldFull)
		}
	}
	if v := st.RepoCustomPropertyValues[oldFull]; v != nil {
		st.RepoCustomPropertyValues[newFull] = v
		delete(st.RepoCustomPropertyValues, oldFull)
		if st.persist != nil {
			batch.Put("repo_custom_property_values", newFull, v)
			batch.Delete("repo_custom_property_values", oldFull)
		}
	}
	if enabled, ok := st.RepoImmutableReleases[oldFull]; ok {
		st.RepoImmutableReleases[newFull] = enabled
		delete(st.RepoImmutableReleases, oldFull)
		if st.persist != nil {
			batch.Put("repo_immutable_releases", newFull, enabled)
			batch.Delete("repo_immutable_releases", oldFull)
		}
	}
	if v := st.AgentsRepoSecrets[oldFull]; v != nil {
		st.AgentsRepoSecrets[newFull] = v
		delete(st.AgentsRepoSecrets, oldFull)
		if st.persist != nil {
			batch.Put("agents_repo_secrets", newFull, v)
			batch.Delete("agents_repo_secrets", oldFull)
		}
	}
	if v := st.AgentsRepoVariables[oldFull]; v != nil {
		st.AgentsRepoVariables[newFull] = v
		delete(st.AgentsRepoVariables, oldFull)
		if st.persist != nil {
			batch.Put("agents_repo_variables", newFull, v)
			batch.Delete("agents_repo_variables", oldFull)
		}
	}
	if v := st.SecretScanningPushPlaceholders[oldFull]; v != nil {
		for _, ph := range v {
			ph.RepoKey = newFull
		}
		st.SecretScanningPushPlaceholders[newFull] = v
		delete(st.SecretScanningPushPlaceholders, oldFull)
		if st.persist != nil {
			batch.Put("secret_scanning_push_placeholders", newFull, v)
			batch.Delete("secret_scanning_push_placeholders", oldFull)
		}
	}
	if v := st.SecretScanningPushBypasses[oldFull]; v != nil {
		for _, bypass := range v {
			bypass.RepoKey = newFull
		}
		st.SecretScanningPushBypasses[newFull] = v
		delete(st.SecretScanningPushBypasses, oldFull)
		if st.persist != nil {
			batch.Put("secret_scanning_push_bypasses", newFull, v)
			batch.Delete("secret_scanning_push_bypasses", oldFull)
		}
	}

	if v := st.SecurityAdvisoriesByRepo[oldFull]; v != nil {
		st.SecurityAdvisoriesByRepo[newFull] = v
		for _, a := range v {
			if st.persist != nil {
				batch.Put("security_advisories", strconv.Itoa(a.ID), a)
			}
		}
		delete(st.SecurityAdvisoriesByRepo, oldFull)
		if st.persist != nil {
			batch.Delete("security_advisories", oldFull)
		}
	}
	for key, fix := range st.CodeScanningAutofixes {
		if fix.RepoKey == oldFull {
			newKey := autofixKey(newFull, fix.AlertNumber)
			fix.RepoKey = newFull
			st.CodeScanningAutofixes[newKey] = fix
			delete(st.CodeScanningAutofixes, key)
			if st.persist != nil {
				batch.Put("code_scanning_autofixes", newKey, fix)
				batch.Delete("code_scanning_autofixes", key)
			}
		}
	}
	for _, up := range st.SARIFUploads {
		if up.RepoKey == oldFull {
			up.RepoKey = newFull
			if st.persist != nil {
				batch.Put("sarif_uploads", up.ID, up)
			}
		}
	}
	if v := st.CodeQLDatabasesByRepo[oldFull]; v != nil {
		st.CodeQLDatabasesByRepo[newFull] = v
		for _, db := range v {
			db.RepoKey = newFull
			if st.persist != nil {
				batch.Put("codeql_databases", strconv.Itoa(db.ID), db)
			}
		}
		delete(st.CodeQLDatabasesByRepo, oldFull)
	}
	for _, va := range st.CodeQLVariantAnalyses {
		changed := false
		if va.ControllerRepoKey == oldFull {
			va.ControllerRepoKey = newFull
			changed = true
		}
		for i := range va.ScannedRepositories {
			if va.ScannedRepositories[i].FullName == oldFull {
				va.ScannedRepositories[i].FullName = newFull
				changed = true
			}
		}
		for i, name := range va.NotFoundRepos {
			if name == oldFull {
				va.NotFoundRepos[i] = newFull
				changed = true
			}
		}
		if changed {
			batch.Put("codeql_variant_analyses", strconv.Itoa(va.ID), va)
		}
	}
	for _, rs := range st.Rulesets {
		if rs.RepoID == 0 || rs.Source != oldFull {
			continue
		}
		rs.Source = newFull
		if st.persist != nil {
			batch.Put("repo_rulesets", strconv.Itoa(rs.ID), rs)
		}
	}
	renamedRepo := st.ReposByName[newFull]
	for _, suite := range st.RulesetSuites {
		if renamedRepo != nil && suite.RepositoryID == renamedRepo.ID {
			suite.RepositoryName = renamedRepo.Name
			if st.persist != nil {
				batch.Put("ruleset_suites", strconv.Itoa(suite.ID), suite)
			}
		}
	}
	for _, project := range st.ProjectClassic {
		if project.RepoKey == oldFull {
			project.RepoKey = newFull
			if st.persist != nil {
				batch.Put("projects_classic", strconv.Itoa(project.ID), project)
			}
		}
	}
	for _, cs := range st.Codespaces {
		if cs.RepoKey == oldFull {
			cs.RepoKey = newFull
			cs.UpdatedAt = st.currentTime()
			if st.persist != nil {
				batch.Put("codespaces", strconv.Itoa(cs.ID), cs)
			}
		}
	}
	if pkgs := st.PackagesByOwnerKey[oldFull]; len(pkgs) > 0 {
		st.PackagesByOwnerKey[newFull] = pkgs
		delete(st.PackagesByOwnerKey, oldFull)
		for _, pkg := range pkgs {
			pkg.OwnerKey = newFull
			pkg.UpdatedAt = st.currentTime()
			if st.persist != nil {
				batch.Put("packages", strconv.Itoa(pkg.ID), pkg)
			}
		}
	}

	for k, v := range st.EnvSecrets {
		repoKey, envName, found := strings.Cut(k, "\x1f")
		if found && repoKey == oldFull {
			newKey := newFull + "\x1f" + envName
			st.EnvSecrets[newKey] = v
			delete(st.EnvSecrets, k)
			if st.persist != nil {
				batch.Put("env_secrets", newKey, v)
				batch.Delete("env_secrets", k)
			}
		}
	}
	for k, v := range st.EnvVariables {
		repoKey, envName, found := strings.Cut(k, "\x1f")
		if found && repoKey == oldFull {
			newKey := newFull + "\x1f" + envName
			st.EnvVariables[newKey] = v
			delete(st.EnvVariables, k)
			if st.persist != nil {
				batch.Put("env_variables", newKey, v)
				batch.Delete("env_variables", k)
			}
		}
	}

	for _, wf := range st.Workflows {
		if wf.RepoFullName == oldFull {
			wf.RepoFullName = newFull
		}
	}
	// STORE-029: a workflow file's ID (map key and persistence key) is
	// stableWorkflowFileID(RepoFullName, Path), so a rename must re-key it. Only
	// rewriting the field left the row keyed by the old-name hash while the next
	// registration under the new name derived a different hash and inserted a
	// duplicate. Snapshot first — never insert into a map being ranged.
	type workflowFileMove struct {
		oldID int64
		newID int64
		wf    *WorkflowFile
	}
	var wfMoves []workflowFileMove
	for oldID, wf := range st.WorkflowFiles {
		if wf.RepoFullName == oldFull {
			wfMoves = append(wfMoves, workflowFileMove{oldID: oldID, newID: stableWorkflowFileID(newFull, wf.Path), wf: wf})
		}
	}
	for _, m := range wfMoves {
		m.wf.RepoFullName = newFull
		m.wf.ID = m.newID
		delete(st.WorkflowFiles, m.oldID)
		st.WorkflowFiles[m.newID] = m.wf
		if st.persist != nil {
			batch.Delete("workflow_files", strconv.FormatInt(m.oldID, 10))
			batch.Put("workflow_files", strconv.FormatInt(m.newID, 10), m.wf)
		}
	}

	st.Misc.mu.Lock()
	if v := st.Misc.pagesBuilds[oldFull]; v != nil {
		st.Misc.pagesBuilds[newFull] = v
		delete(st.Misc.pagesBuilds, oldFull)
		if st.persist != nil {
			batch.Put("pages_builds", newFull, v)
			batch.Delete("pages_builds", oldFull)
		}
	}
	st.Misc.mu.Unlock()
}

// RenameBranch renames a git branch in a repository.
func (st *Store) RenameBranch(repoID int, branch, newName string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return false
	}
	owner, name, ok := splitRepoFullName(repo.FullName)
	if !ok {
		return false
	}
	stor := st.GitStorages[owner+"/"+name]
	if stor == nil {
		return false
	}

	oldRef := plumbing.NewBranchReferenceName(branch)
	newRef := plumbing.NewBranchReferenceName(newName)
	ref, err := stor.Reference(oldRef)
	if err != nil {
		return false
	}
	if _, err := stor.Reference(newRef); err == nil {
		return false
	}
	if err := stor.SetReference(plumbing.NewHashReference(newRef, ref.Hash())); err != nil {
		return false
	}
	if err := stor.RemoveReference(oldRef); err != nil {
		return false
	}
	// One transaction: re-keying the branch protection to the new branch name
	// and the repo row's default-branch/updated-at write commit together (both
	// target the same durable store), so a crash can never duplicate the protection
	// under both names or persist a repo whose recorded default branch disagrees
	// with the protection re-key (STORE-001/002).
	batch := newPersistBatch(st.persist)
	if repo.DefaultBranch == branch {
		repo.DefaultBranch = newName
	}
	oldProtectionKey := bpKey(repo.ID, branch)
	newProtectionKey := bpKey(repo.ID, newName)
	st.Misc.mu.Lock()
	if protection, ok := st.Misc.branchProtection[oldProtectionKey]; ok {
		st.Misc.branchProtection[newProtectionKey] = protection
		delete(st.Misc.branchProtection, oldProtectionKey)
		batch.Put("branch_protection", newProtectionKey, protection)
		batch.Delete("branch_protection", oldProtectionKey)
	}
	st.Misc.mu.Unlock()
	repo.UpdatedAt = st.currentTime()
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "repos", err: err})
	}
	return true
}

// SetRepoFlag sets a boolean flag field on a repo by name.
func (st *Store) SetRepoFlag(repoID int, field string, value bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return false
	}
	switch field {
	case "automated_security_fixes_enabled":
		repo.AutomatedSecurityFixesEnabled = value
	case "private_vulnerability_reporting_enabled":
		repo.PrivateVulnerabilityReportingEnabled = value
	case "vulnerability_alerts_enabled":
		repo.VulnerabilityAlertsEnabled = value
	default:
		return false
	}
	repo.UpdatedAt = st.currentTime()
	if st.persist != nil {
		st.persist.MustPut("repos", strconv.Itoa(repo.ID), repo)
	}
	return true
}

// SetRepoInteractionLimit sets the interaction limit for a repo.
func (st *Store) SetRepoInteractionLimit(repoID int, limit string, expiry *time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return false
	}
	repo.InteractionLimit = limit
	repo.InteractionLimitExpiry = expiry
	repo.UpdatedAt = st.currentTime()
	if st.persist != nil {
		st.persist.MustPut("repos", strconv.Itoa(repo.ID), repo)
	}
	return true
}
