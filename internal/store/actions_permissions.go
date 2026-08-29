package store

// OrgActionsPermissions models the organization-level Actions settings.
type OrgActionsPermissions struct {
	EnabledRepositories         string          `json:"enabled_repositories"`
	SelectedRepositoriesURL     string          `json:"selected_repositories_url,omitempty"`
	AllowedActions              string          `json:"allowed_actions"`
	SelectedActionsURL          string          `json:"selected_actions_url,omitempty"`
	SelectedRepositoryIDs       []int           `json:"selected_repository_ids,omitempty"`
	ActionsAllowed              *ActionsAllowed `json:"actions_allowed,omitempty"`
	WorkflowPermissions         *WorkflowPermissions
	CacheRetentionLimitDays     int
	CacheStorageLimitGB         int64
	ArtifactAndLogRetentionDays int
	// ForkPRApprovalPolicy names which fork-PR contributors approval is demanded
	// of; whether it is demanded at all is
	// ForkPRWorkflowsPrivateRepos.RequireApprovalForForkPRWorkflows.
	ForkPRApprovalPolicy                 string
	ForkPRWorkflowsPrivateRepos          *ForkPRWorkflowsPrivateRepos
	SelfHostedRunnersEnabledRepositories string
	SelfHostedRunnersSelectedRepoIDs     []int
	MaxCacheRetentionDays                int
	MaxCacheSizeGB                       int
}

type ForkPRWorkflowsPrivateRepos struct {
	RunWorkflowsFromForkPullRequests  bool `json:"run_workflows_from_fork_pull_requests"`
	SendWriteTokensToWorkflows        bool `json:"send_write_tokens_to_workflows"`
	SendSecretsAndVariables           bool `json:"send_secrets_and_variables"`
	RequireApprovalForForkPRWorkflows bool `json:"require_approval_for_fork_pr_workflows"`
}

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

type ActionsAllowed struct {
	GithubOwnedAllowed bool     `json:"github_owned_allowed"`
	VerifiedAllowed    bool     `json:"verified_allowed"`
	PatternsAllowed    []string `json:"patterns_allowed"`
}

type WorkflowPermissions struct {
	DefaultWorkflowPermissions   string `json:"default_workflow_permissions"`
	CanApprovePullRequestReviews bool   `json:"can_approve_pull_request_reviews"`
}

// LookupOrgActionsPermissionsLocked returns an org's stored Actions settings
// without creating them; safe under a read lock. Returns nil when the org has
// never been configured.
func (st *Store) LookupOrgActionsPermissionsLocked(orgLogin string) *OrgActionsPermissions {
	if st.OrgActionsPermissions == nil {
		return nil
	}
	return st.OrgActionsPermissions[orgLogin]
}

// GetOrgActionsPermissionsLocked materializes an org's Actions settings,
// filling defaults for fields whose zero value is invalid. Writes to the store,
// so the caller must hold the WRITE lock (a read lock is a fatal map write).
func (st *Store) GetOrgActionsPermissionsLocked(orgLogin string) *OrgActionsPermissions {
	if st.OrgActionsPermissions == nil {
		st.OrgActionsPermissions = map[string]*OrgActionsPermissions{}
	}
	if p, ok := st.OrgActionsPermissions[orgLogin]; ok && p != nil {
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
		if p.ForkPRContributorApproval == "" {
			p.ForkPRContributorApproval = "first_time_contributors"
		}
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

func (st *Store) persistOrgActionsPermissionsLocked(orgLogin string) {
	if st.Persist == nil {
		return
	}
	if p := st.GetOrgActionsPermissionsLocked(orgLogin); p != nil {
		st.Persist.MustPut("org_actions_permissions", orgLogin, p)
	}
}

// AddOrgSelectedRepo adds a repository to the org's selected list.
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
	// Fresh slice, not in-place s[:0]: a reader handed the old backing array must
	// not see it rewritten (STORE-021).
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

// ListOrgSelectedRepos returns the org's selected repository IDs. Uses the
// non-materializing lookup so a read lock never writes to the store.
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
		ForkPRContributorApproval:   "first_time_contributors",
		ForkPRWorkflowsPrivateRepos: &ForkPRWorkflowsPrivateRepos{},
		ArtifactAndLogRetentionDays: 90,
		CacheRetentionLimitDays:     0,
		CacheStorageLimitGB:         0,
	}
}
