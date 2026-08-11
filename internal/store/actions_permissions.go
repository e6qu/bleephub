package store

// OrgActionsPermissions models the organization-level Actions settings.
type OrgActionsPermissions struct {
	EnabledRepositories     string          `json:"enabled_repositories"`
	SelectedRepositoriesURL string          `json:"selected_repositories_url,omitempty"`
	AllowedActions          string          `json:"allowed_actions"`
	SelectedActionsURL      string          `json:"selected_actions_url,omitempty"`
	SelectedRepositoryIDs   []int           `json:"selected_repository_ids,omitempty"`
	ActionsAllowed          *ActionsAllowed `json:"actions_allowed,omitempty"`
	WorkflowPermissions     *WorkflowPermissions
	CacheRetentionLimitDays int
	CacheStorageLimitGB     int64
	// ArtifactAndLogRetentionDays is the org-wide artifact/log retention
	// setting (GET/PUT /orgs/{org}/actions/permissions/artifact-and-log-retention).
	ArtifactAndLogRetentionDays int
	// ForkPRApprovalPolicy controls when fork PR workflows require
	// maintainer approval (actions-fork-pr-contributor-approval enum).
	ForkPRApprovalPolicy string
	// ForkPRWorkflowsPrivateRepos holds the org's fork-PR-workflow policy
	// for private repositories (four booleans).
	ForkPRWorkflowsPrivateRepos *ForkPRWorkflowsPrivateRepos
	// SelfHostedRunnersEnabledRepositories is the org policy controlling
	// which repositories may use repository-level self-hosted runners
	// (all | selected | none) with its selected repository ids.
	SelfHostedRunnersEnabledRepositories string
	SelfHostedRunnersSelectedRepoIDs     []int
	// MaxCacheRetentionDays / MaxCacheSizeGB back the
	// /organizations/{org}/actions/cache/{retention,storage}-limit
	// policy endpoints.
	MaxCacheRetentionDays int
	MaxCacheSizeGB        int
}

// ForkPRWorkflowsPrivateRepos is the actions-fork-pr-workflows-private-repos
// settings shape.
type ForkPRWorkflowsPrivateRepos struct {
	RunWorkflowsFromForkPullRequests  bool `json:"run_workflows_from_fork_pull_requests"`
	SendWriteTokensToWorkflows        bool `json:"send_write_tokens_to_workflows"`
	SendSecretsAndVariables           bool `json:"send_secrets_and_variables"`
	RequireApprovalForForkPRWorkflows bool `json:"require_approval_for_fork_pr_workflows"`
}

// RepoActionsPermissions models the repository-level Actions settings.
type RepoActionsPermissions struct {
	Enabled                     bool            `json:"enabled"`
	AllowedActions              string          `json:"allowed_actions"`
	SelectedActionsURL          string          `json:"selected_actions_url,omitempty"`
	ActionsAllowed              *ActionsAllowed `json:"actions_allowed,omitempty"`
	AccessLevel                 string          `json:"access_level"`
	WorkflowPermissions         *WorkflowPermissions
	ForkPRContributorApproval   string                       `json:"fork_pull_request_member_approval"`
	ForkPRWorkflowsPrivateRepos *ForkPRWorkflowsPrivateRepos `json:"fork_pull_request_workflows_private_repos,omitempty"`
	ArtifactAndLogRetentionDays int                          `json:"artifact_and_log_retention_days"`
	CacheRetentionLimitDays     int
	CacheStorageLimitGB         int64
}

// ActionsAllowed is the "selected actions" allow-list shape.
type ActionsAllowed struct {
	GithubOwnedAllowed bool     `json:"github_owned_allowed"`
	VerifiedAllowed    bool     `json:"verified_allowed"`
	PatternsAllowed    []string `json:"patterns_allowed"`
}

// WorkflowPermissions is the default workflow-token permissions shape.
type WorkflowPermissions struct {
	DefaultWorkflowPermissions   string `json:"default_workflow_permissions"`
	CanApprovePullRequestReviews bool   `json:"can_approve_pull_request_reviews"`
}

// lookupOrgActionsPermissionsLocked returns an org's stored Actions settings
// without creating or amending them, and is therefore safe to call while
// holding only a read lock. It returns nil when the org has never been
// configured; callers that need a value should fall back to
// DefaultOrgActionsPermissions rather than materializing one here.
func (st *Store) LookupOrgActionsPermissionsLocked(orgLogin string) *OrgActionsPermissions {
	if st.OrgActionsPermissions == nil {
		return nil
	}
	return st.OrgActionsPermissions[orgLogin]
}

// getOrgActionsPermissionsLocked materializes an org's Actions settings,
// creating the entry and filling in defaults for fields whose zero value is not
// a valid configuration. It writes to the store, so the caller must hold the
// WRITE lock — a read lock here is a concurrent map write, which is fatal and
// unrecoverable rather than a recoverable panic.
func (st *Store) GetOrgActionsPermissionsLocked(orgLogin string) *OrgActionsPermissions {
	if st.OrgActionsPermissions == nil {
		st.OrgActionsPermissions = map[string]*OrgActionsPermissions{}
	}
	if p, ok := st.OrgActionsPermissions[orgLogin]; ok && p != nil {
		// Materialize defaults for settings whose zero value is not a
		// valid configuration (enum-shaped policies and limits).
		if p.ArtifactAndLogRetentionDays == 0 {
			p.ArtifactAndLogRetentionDays = 90
		}
		if p.ForkPRApprovalPolicy == "" {
			p.ForkPRApprovalPolicy = "first_time_contributors"
		}
		if p.SelfHostedRunnersEnabledRepositories == "" {
			p.SelfHostedRunnersEnabledRepositories = "all"
		}
		if p.MaxCacheRetentionDays == 0 {
			p.MaxCacheRetentionDays = 90
		}
		if p.MaxCacheSizeGB == 0 {
			p.MaxCacheSizeGB = 10
		}
		return p
	}
	p := DefaultOrgActionsPermissions()
	st.OrgActionsPermissions[orgLogin] = p
	return p
}

// GetOrgActionsPermissions returns the org's Actions settings, materializing
// defaults on first read.
func (st *Store) GetOrgActionsPermissions(orgLogin string) *OrgActionsPermissions {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.GetOrgActionsPermissionsLocked(orgLogin)
}

// SetOrgActionsPermissions stores the org's Actions settings and persists.
func (st *Store) SetOrgActionsPermissions(orgLogin string, p *OrgActionsPermissions) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.OrgActionsPermissions == nil {
		st.OrgActionsPermissions = map[string]*OrgActionsPermissions{}
	}
	st.OrgActionsPermissions[orgLogin] = p
	if st.Persist != nil {
		st.Persist.MustPut("org_actions_permissions", orgLogin, p)
	}
}

func (st *Store) getRepoActionsPermissionsLocked(repoKey string) *RepoActionsPermissions {
	if st.RepoActionsPermissions == nil {
		st.RepoActionsPermissions = map[string]*RepoActionsPermissions{}
	}
	if p, ok := st.RepoActionsPermissions[repoKey]; ok && p != nil {
		return p
	}
	p := DefaultRepoActionsPermissions()
	st.RepoActionsPermissions[repoKey] = p
	return p
}

// GetRepoActionsPermissions returns the repo's Actions settings, materializing
// defaults on first read.
func (st *Store) GetRepoActionsPermissions(repoKey string) *RepoActionsPermissions {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.getRepoActionsPermissionsLocked(repoKey)
}

// SetRepoActionsPermissions stores the repo's Actions settings and persists.
func (st *Store) SetRepoActionsPermissions(repoKey string, p *RepoActionsPermissions) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.RepoActionsPermissions == nil {
		st.RepoActionsPermissions = map[string]*RepoActionsPermissions{}
	}
	st.RepoActionsPermissions[repoKey] = p
	if st.Persist != nil {
		st.Persist.MustPut("repo_actions_permissions", repoKey, p)
	}
}

// persistOrgActionsPermissionsLocked writes the org permissions when the store
// lock is already held.
func (st *Store) persistOrgActionsPermissionsLocked(orgLogin string) {
	if st.Persist == nil {
		return
	}
	if p := st.GetOrgActionsPermissionsLocked(orgLogin); p != nil {
		st.Persist.MustPut("org_actions_permissions", orgLogin, p)
	}
}

// AddOrgSelectedRepo adds a repository to the org's selected list (no-op if
// already present).
func (st *Store) AddOrgSelectedRepo(orgLogin string, repoID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := st.GetOrgActionsPermissionsLocked(orgLogin)
	for _, id := range p.SelectedRepositoryIDs {
		if id == repoID {
			return
		}
	}
	p.SelectedRepositoryIDs = append(p.SelectedRepositoryIDs, repoID)
	st.persistOrgActionsPermissionsLocked(orgLogin)
}

// RemoveOrgSelectedRepo drops a repository from the org's selected list.
func (st *Store) RemoveOrgSelectedRepo(orgLogin string, repoID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := st.GetOrgActionsPermissionsLocked(orgLogin)
	// A fresh slice rather than the in-place s[:0] idiom: the old backing
	// array is still referenced by any list a reader was handed earlier, and
	// rewriting it in place mutates their copy retroactively.
	kept := make([]int, 0, len(p.SelectedRepositoryIDs))
	for _, id := range p.SelectedRepositoryIDs {
		if id != repoID {
			kept = append(kept, id)
		}
	}
	p.SelectedRepositoryIDs = kept
	st.persistOrgActionsPermissionsLocked(orgLogin)
}

// SetOrgSelectedRepos replaces the org's selected repository list.
func (st *Store) SetOrgSelectedRepos(orgLogin string, repoIDs []int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := st.GetOrgActionsPermissionsLocked(orgLogin)
	p.SelectedRepositoryIDs = append([]int(nil), repoIDs...)
	st.persistOrgActionsPermissionsLocked(orgLogin)
}

// ListOrgSelectedRepos returns the org's selected repository IDs. An org that
// has never been configured has no selected list; reporting that as empty is
// correct and, unlike materializing a default here, does not write to the
// store from underneath a read lock.
func (st *Store) ListOrgSelectedRepos(orgLogin string) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	p := st.LookupOrgActionsPermissionsLocked(orgLogin)
	if p == nil {
		return nil
	}
	out := make([]int, len(p.SelectedRepositoryIDs))
	copy(out, p.SelectedRepositoryIDs)
	return out
}

// DefaultOrgActionsPermissions returns the GitHub-default org settings.
func DefaultOrgActionsPermissions() *OrgActionsPermissions {
	return &OrgActionsPermissions{
		EnabledRepositories:                  "all",
		AllowedActions:                       "all",
		SelectedRepositoryIDs:                []int{},
		CacheRetentionLimitDays:              90,
		CacheStorageLimitGB:                  0,
		ArtifactAndLogRetentionDays:          90,
		ForkPRApprovalPolicy:                 "first_time_contributors",
		SelfHostedRunnersEnabledRepositories: "all",
		MaxCacheRetentionDays:                90,
		MaxCacheSizeGB:                       10,
	}
}

// DefaultRepoActionsPermissions returns the GitHub-default repo settings.
func DefaultRepoActionsPermissions() *RepoActionsPermissions {
	return &RepoActionsPermissions{
		Enabled:                     true,
		AllowedActions:              "all",
		AccessLevel:                 "none",
		ForkPRContributorApproval:   "none",
		ForkPRWorkflowsPrivateRepos: &ForkPRWorkflowsPrivateRepos{},
		ArtifactAndLogRetentionDays: 90,
		CacheRetentionLimitDays:     0,
		CacheStorageLimitGB:         0,
	}
}
