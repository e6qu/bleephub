package bleephub

import (
	"sort"
	"strconv"
	"time"
)

// Enterprise-scoped state. bleephub plays the role of a single GitHub
// Enterprise Server instance, so exactly one enterprise exists; its slug is
// configuration (BLEEPHUB_ENTERPRISE_SLUG), not store state. Everything the
// enterprise REST surfaces mutate — enterprise teams, code security
// configurations, Dependabot repository access, GitHub Actions cache limits,
// Actions OIDC custom property inclusions, and the Copilot coding agent
// policy — lives here and persists.

// EnterpriseTeam is a team scoped to the enterprise rather than to one
// organization. Membership is direct (user IDs); organization assignments
// depend on OrganizationSelectionType: "disabled" assigns none, "all"
// assigns every organization on the instance, "selected" assigns exactly
// SelectedOrgLogins.
type EnterpriseTeam struct {
	ID                        int       `json:"id"`
	Name                      string    `json:"name"`
	Description               string    `json:"description"`
	Slug                      string    `json:"slug"`
	OrganizationSelectionType string    `json:"organization_selection_type"`
	GroupID                   *string   `json:"group_id"`
	NotificationSetting       string    `json:"notification_setting"`
	MemberIDs                 []int     `json:"member_ids"`
	SelectedOrgLogins         []string  `json:"selected_org_logins"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

// EnterpriseCodeSecurityConfiguration mirrors GitHub's
// code-security-configuration schema with target_type "enterprise". Feature
// fields hold the enabled/disabled/not_set enum values the API accepts.
type EnterpriseCodeSecurityConfiguration struct {
	ID                                    int       `json:"id"`
	Name                                  string    `json:"name"`
	Description                           string    `json:"description"`
	AdvancedSecurity                      string    `json:"advanced_security"`
	DependencyGraph                       string    `json:"dependency_graph"`
	DependencyGraphAutosubmitAction       string    `json:"dependency_graph_autosubmit_action"`
	DependencyGraphAutosubmitLabeled      bool      `json:"dependency_graph_autosubmit_labeled_runners"`
	DependabotAlerts                      string    `json:"dependabot_alerts"`
	DependabotSecurityUpdates             string    `json:"dependabot_security_updates"`
	CodeScanningAllowAdvanced             *bool     `json:"code_scanning_allow_advanced"`
	CodeScanningDefaultSetup              string    `json:"code_scanning_default_setup"`
	CodeScanningRunnerType                *string   `json:"code_scanning_runner_type"`
	CodeScanningRunnerLabel               *string   `json:"code_scanning_runner_label"`
	CodeScanningDelegatedAlertDismissal   string    `json:"code_scanning_delegated_alert_dismissal"`
	SecretScanning                        string    `json:"secret_scanning"`
	SecretScanningPushProtection          string    `json:"secret_scanning_push_protection"`
	SecretScanningValidityChecks          string    `json:"secret_scanning_validity_checks"`
	SecretScanningNonProviderPatterns     string    `json:"secret_scanning_non_provider_patterns"`
	SecretScanningGenericSecrets          string    `json:"secret_scanning_generic_secrets"`
	SecretScanningDelegatedAlertDismissal string    `json:"secret_scanning_delegated_alert_dismissal"`
	SecretScanningExtendedMetadata        string    `json:"secret_scanning_extended_metadata"`
	PrivateVulnerabilityReporting         string    `json:"private_vulnerability_reporting"`
	Enforcement                           string    `json:"enforcement"`
	DefaultForNewRepos                    string    `json:"default_for_new_repos"` // "none" unless set via the defaults endpoint
	CreatedAt                             time.Time `json:"created_at"`
	UpdatedAt                             time.Time `json:"updated_at"`
}

// EnterpriseCodeSecurityAttachment records one repository's attachment to an
// enterprise code security configuration. Persisted individually so the
// repoID→configID association survives reload; a repository has at most one
// attached configuration.
type EnterpriseCodeSecurityAttachment struct {
	RepoID   int `json:"repo_id"`
	ConfigID int `json:"config_id"`
}

// DependabotDefaultLevel is an enterprise's Dependabot default repository
// access level. The empty value means "never set" (serialised as null); the
// two real values are the constants below. A typed string marshals to JSON
// identically to a plain string.
type DependabotDefaultLevel string

const (
	DependabotDefaultLevelPublic   DependabotDefaultLevel = "public"
	DependabotDefaultLevelInternal DependabotDefaultLevel = "internal"
)

// EnterpriseSettings holds the singleton enterprise-level settings the REST
// surfaces mutate. Persisted as one row under the "enterprise_settings"
// bucket; zero-value fields fall back to defaultEnterpriseSettings values on
// first access paths that seed them in NewStore.
type EnterpriseSettings struct {
	// Enterprise administration settings.
	Announcement                    *EnterpriseAnnouncement                    `json:"announcement,omitempty"`
	AccessRestrictionsEnabled       bool                                       `json:"access_restrictions_enabled"`
	CodeSecurityAndAnalysis         EnterpriseCodeSecurity                     `json:"code_security_and_analysis"`
	AuditLogStreams                 []*EnterpriseAuditLogStream                `json:"audit_log_streams,omitempty"`
	NextAuditLogStreamID            int                                        `json:"next_audit_log_stream_id"`
	RepositoryCustomProperties      map[string]*CustomProperty                 `json:"repository_custom_properties,omitempty"`
	OrganizationCustomProperties    map[string]*CustomProperty                 `json:"organization_custom_properties,omitempty"`
	OrganizationPropertyValues      map[string]map[string]interface{}          `json:"organization_property_values,omitempty"`
	SCIMUsers                       map[string]*EnterpriseSCIMUser             `json:"scim_users,omitempty"`
	SCIMGroups                      map[string]*EnterpriseSCIMGroup            `json:"scim_groups,omitempty"`
	EnterpriseRoleTeamAssignments   map[int][]int                              `json:"enterprise_role_team_assignments,omitempty"`
	EnterpriseRoleUserAssignments   map[int][]int                              `json:"enterprise_role_user_assignments,omitempty"`
	VisualStudioSubscriptions       map[string]*VisualStudioSubscription       `json:"visual_studio_subscriptions,omitempty"`
	InnerSourceSyncJobs             map[string]*EnterpriseInnerSourceSyncJob   `json:"innersource_sync_jobs,omitempty"`
	EnterpriseCopilotSeats          map[string]*CopilotSeat                    `json:"enterprise_copilot_seats,omitempty"`
	CopilotCustomAgentsSourceOrgID  int                                        `json:"copilot_custom_agents_source_org_id,omitempty"`
	CopilotCustomAgentsRulesetID    int                                        `json:"copilot_custom_agents_ruleset_id,omitempty"`
	EnterpriseBudgets               map[string]*OrgBudget                      `json:"enterprise_budgets,omitempty"`
	EnterpriseCostCenters           map[string]*EnterpriseCostCenter           `json:"enterprise_cost_centers,omitempty"`
	EnterpriseBillingReports        map[string]*EnterpriseBillingReport        `json:"enterprise_billing_reports,omitempty"`
	GHESManagement                  *GHESManagementState                       `json:"ghes_management,omitempty"`
	GHESGlobalHooks                 []*Webhook                                 `json:"ghes_global_hooks,omitempty"`
	GHESPreReceiveEnvironments      map[int]*GHESPreReceiveEnvironment         `json:"ghes_pre_receive_environments,omitempty"`
	GHESPreReceiveHooks             map[int]*GHESPreReceiveHook                `json:"ghes_pre_receive_hooks,omitempty"`
	GHESOrgPreReceiveOverrides      map[string]map[int]*GHESPreReceiveOverride `json:"ghes_org_pre_receive_overrides,omitempty"`
	GHESRepoPreReceiveOverrides     map[string]map[int]*GHESPreReceiveOverride `json:"ghes_repo_pre_receive_overrides,omitempty"`
	NextGHESPreReceiveEnvironmentID int                                        `json:"next_ghes_pre_receive_environment_id"`
	NextGHESPreReceiveHookID        int                                        `json:"next_ghes_pre_receive_hook_id"`
	GHESLDAPUserMappings            map[string]string                          `json:"ghes_ldap_user_mappings,omitempty"`
	GHESLDAPTeamMappings            map[int]string                             `json:"ghes_ldap_team_mappings,omitempty"`

	// Dependabot repository access across organizations.
	DependabotAccessibleRepoIDs []int                  `json:"dependabot_accessible_repo_ids"`
	DependabotDefaultLevel      DependabotDefaultLevel `json:"dependabot_default_level"` // "" = never set (null); else public|internal

	// GitHub Actions cache policy. GitHub Enterprise Server ships with a
	// 14-day retention limit and a 10 GB per-repository storage limit.
	ActionsCacheRetentionDays int `json:"actions_cache_retention_days"`
	ActionsCacheSizeGB        int `json:"actions_cache_size_gb"`
	ActionsDefaultCacheSizeGB int `json:"actions_default_cache_size_gb"`

	// GitHub Actions OIDC custom property inclusions (repository custom
	// properties included in OIDC token claims), in insertion order.
	OIDCCustomProperties      []string `json:"oidc_custom_properties"`
	OIDCIncludeEnterpriseSlug bool     `json:"oidc_include_enterprise_slug"`

	// Enterprise-wide Actions policy. These settings are intentionally stored
	// independently from organization policy: organization settings may
	// narrow an enterprise policy but cannot replace its persisted source of
	// truth.
	ActionsEnabledOrganizations     string                       `json:"actions_enabled_organizations"`
	ActionsAllowedActions           string                       `json:"actions_allowed_actions"`
	ActionsSHAPinningRequired       bool                         `json:"actions_sha_pinning_required"`
	ActionsSelectedOrganizationIDs  []int                        `json:"actions_selected_organization_ids"`
	ActionsAllowed                  *ActionsAllowed              `json:"actions_allowed,omitempty"`
	ActionsWorkflowPermissions      *WorkflowPermissions         `json:"actions_workflow_permissions,omitempty"`
	ActionsArtifactRetentionDays    int                          `json:"actions_artifact_retention_days"`
	ActionsForkPRApprovalPolicy     string                       `json:"actions_fork_pr_approval_policy"`
	ActionsForkPRWorkflowsPrivate   *ForkPRWorkflowsPrivateRepos `json:"actions_fork_pr_workflows_private,omitempty"`
	ActionsDisableSelfHostedRunners bool                         `json:"actions_disable_self_hosted_runners"`

	// Copilot coding agent policy. "" = never set.
	CopilotCodingAgentPolicy string   `json:"copilot_coding_agent_policy"`
	CopilotCodingAgentOrgs   []string `json:"copilot_coding_agent_orgs"`
}

// EnterpriseAnnouncement is the enterprise-wide banner returned by the
// announcement API. ExpiresAt is kept as the caller's RFC3339 representation
// because GitHub returns an ISO-8601 string and null has distinct semantics.
type EnterpriseAnnouncement struct {
	Announcement    string  `json:"announcement"`
	ExpiresAt       *string `json:"expires_at"`
	UserDismissible bool    `json:"user_dismissible"`
}

// EnterpriseCodeSecurity is the legacy enterprise security policy. GitHub
// keeps this API for compatibility alongside code-security configurations.
type EnterpriseCodeSecurity struct {
	AdvancedSecurityEnabledForNewRepositories                  bool    `json:"advanced_security_enabled_for_new_repositories"`
	AdvancedSecurityEnabledNewUserNamespaceRepos               bool    `json:"advanced_security_enabled_new_user_namespace_repos"`
	DependabotAlertsEnabledForNewRepositories                  bool    `json:"dependabot_alerts_enabled_for_new_repositories"`
	SecretScanningEnabledForNewRepositories                    bool    `json:"secret_scanning_enabled_for_new_repositories"`
	SecretScanningPushProtectionEnabledForNewRepositories      bool    `json:"secret_scanning_push_protection_enabled_for_new_repositories"`
	SecretScanningPushProtectionCustomLink                     *string `json:"secret_scanning_push_protection_custom_link"`
	SecretScanningNonProviderPatternsEnabledForNewRepositories bool    `json:"secret_scanning_non_provider_patterns_enabled_for_new_repositories"`
}

// EnterpriseAuditLogStream is a durable audit-log delivery configuration.
// VendorSpecific contains encrypted/opaque connection settings and is never
// rendered back to clients.
type EnterpriseAuditLogStream struct {
	ID             int                    `json:"id"`
	StreamType     string                 `json:"stream_type"`
	StreamDetails  string                 `json:"stream_details"`
	Enabled        bool                   `json:"enabled"`
	VendorSpecific map[string]interface{} `json:"vendor_specific,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
	UpdatedAt      time.Time              `json:"updated_at"`
	PausedAt       *time.Time             `json:"paused_at"`
}

func defaultEnterpriseSettings() *EnterpriseSettings {
	return normalizeEnterpriseSettings(&EnterpriseSettings{
		ActionsCacheRetentionDays: 14,
		ActionsCacheSizeGB:        10,
		ActionsDefaultCacheSizeGB: 10,
	})
}

func normalizeEnterpriseSettings(settings *EnterpriseSettings) *EnterpriseSettings {
	for _, hook := range settings.GHESGlobalHooks {
		hook.Global = true
	}
	if settings.GHESPreReceiveEnvironments == nil {
		settings.GHESPreReceiveEnvironments = map[int]*GHESPreReceiveEnvironment{}
	}
	if settings.GHESPreReceiveHooks == nil {
		settings.GHESPreReceiveHooks = map[int]*GHESPreReceiveHook{}
	}
	if settings.GHESOrgPreReceiveOverrides == nil {
		settings.GHESOrgPreReceiveOverrides = map[string]map[int]*GHESPreReceiveOverride{}
	}
	if settings.GHESRepoPreReceiveOverrides == nil {
		settings.GHESRepoPreReceiveOverrides = map[string]map[int]*GHESPreReceiveOverride{}
	}
	if settings.NextGHESPreReceiveEnvironmentID == 0 {
		settings.NextGHESPreReceiveEnvironmentID = 1
	}
	if settings.NextGHESPreReceiveHookID == 0 {
		settings.NextGHESPreReceiveHookID = 1
	}
	if settings.GHESLDAPUserMappings == nil {
		settings.GHESLDAPUserMappings = map[string]string{}
	}
	if settings.GHESLDAPTeamMappings == nil {
		settings.GHESLDAPTeamMappings = map[int]string{}
	}
	if settings.NextAuditLogStreamID == 0 {
		settings.NextAuditLogStreamID = 1
	}
	if settings.RepositoryCustomProperties == nil {
		settings.RepositoryCustomProperties = map[string]*CustomProperty{}
	}
	if settings.OrganizationCustomProperties == nil {
		settings.OrganizationCustomProperties = map[string]*CustomProperty{}
	}
	if settings.OrganizationPropertyValues == nil {
		settings.OrganizationPropertyValues = map[string]map[string]interface{}{}
	}
	if settings.SCIMUsers == nil {
		settings.SCIMUsers = map[string]*EnterpriseSCIMUser{}
	}
	if settings.SCIMGroups == nil {
		settings.SCIMGroups = map[string]*EnterpriseSCIMGroup{}
	}
	if settings.EnterpriseRoleTeamAssignments == nil {
		settings.EnterpriseRoleTeamAssignments = map[int][]int{}
	}
	if settings.EnterpriseRoleUserAssignments == nil {
		settings.EnterpriseRoleUserAssignments = map[int][]int{}
	}
	if settings.VisualStudioSubscriptions == nil {
		settings.VisualStudioSubscriptions = map[string]*VisualStudioSubscription{}
	}
	if settings.InnerSourceSyncJobs == nil {
		settings.InnerSourceSyncJobs = map[string]*EnterpriseInnerSourceSyncJob{}
	}
	if settings.EnterpriseCopilotSeats == nil {
		settings.EnterpriseCopilotSeats = map[string]*CopilotSeat{}
	}
	if settings.EnterpriseBudgets == nil {
		settings.EnterpriseBudgets = map[string]*OrgBudget{}
	}
	if settings.EnterpriseCostCenters == nil {
		settings.EnterpriseCostCenters = map[string]*EnterpriseCostCenter{}
	}
	if settings.EnterpriseBillingReports == nil {
		settings.EnterpriseBillingReports = map[string]*EnterpriseBillingReport{}
	}
	if settings.GHESManagement == nil {
		settings.GHESManagement = defaultGHESManagementState()
	}
	if settings.GHESManagement.Settings == nil {
		settings.GHESManagement.Settings = map[string]interface{}{}
	}
	if settings.GHESManagement.SSHKeys == nil {
		settings.GHESManagement.SSHKeys = []string{}
	}
	if settings.ActionsCacheRetentionDays == 0 {
		settings.ActionsCacheRetentionDays = 14
	}
	if settings.ActionsCacheSizeGB == 0 {
		settings.ActionsCacheSizeGB = 10
	}
	if settings.ActionsDefaultCacheSizeGB == 0 {
		settings.ActionsDefaultCacheSizeGB = settings.ActionsCacheSizeGB
	}
	if settings.ActionsEnabledOrganizations == "" {
		settings.ActionsEnabledOrganizations = "all"
	}
	if settings.ActionsAllowedActions == "" {
		settings.ActionsAllowedActions = "all"
	}
	if settings.ActionsWorkflowPermissions == nil {
		settings.ActionsWorkflowPermissions = &WorkflowPermissions{
			DefaultWorkflowPermissions: "read",
		}
	}
	if settings.ActionsWorkflowPermissions.DefaultWorkflowPermissions == "" {
		settings.ActionsWorkflowPermissions.DefaultWorkflowPermissions = "read"
	}
	if settings.ActionsArtifactRetentionDays == 0 {
		settings.ActionsArtifactRetentionDays = 90
	}
	if settings.ActionsForkPRApprovalPolicy == "" {
		settings.ActionsForkPRApprovalPolicy = "first_time_contributors"
	}
	if settings.ActionsForkPRWorkflowsPrivate == nil {
		settings.ActionsForkPRWorkflowsPrivate = &ForkPRWorkflowsPrivateRepos{}
	}
	return settings
}

func (st *Store) persistEnterpriseSettings() {
	if st.persist != nil {
		st.persist.MustPut("enterprise_settings", "enterprise", st.EnterpriseSettings)
	}
}

func (st *Store) persistEnterpriseTeam(t *EnterpriseTeam) {
	if st.persist != nil {
		st.persist.MustPut("enterprise_teams", strconv.Itoa(t.ID), t)
	}
}

func (st *Store) persistEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration) {
	if st.persist != nil {
		st.persist.MustPut("enterprise_code_security_configs", strconv.Itoa(c.ID), c)
	}
}

// --- enterprise teams ---

// CreateEnterpriseTeam creates an enterprise team. Returns nil when a team
// with the same slug already exists.
func (st *Store) CreateEnterpriseTeam(name, description, selectionType string, groupID *string, notificationSetting string) *EnterpriseTeam {
	st.mu.Lock()
	defer st.mu.Unlock()

	slug := slugify(name)
	if _, exists := st.EnterpriseTeamsBySlug[slug]; exists {
		return nil
	}
	if selectionType == "" {
		selectionType = "disabled"
	}
	if notificationSetting == "" {
		notificationSetting = "notifications_enabled"
	}
	now := st.currentTime()
	t := &EnterpriseTeam{
		ID:                        st.NextEnterpriseTeamID,
		Name:                      name,
		Description:               description,
		Slug:                      slug,
		OrganizationSelectionType: selectionType,
		GroupID:                   groupID,
		NotificationSetting:       notificationSetting,
		MemberIDs:                 []int{},
		SelectedOrgLogins:         []string{},
		CreatedAt:                 now,
		UpdatedAt:                 now,
	}
	st.NextEnterpriseTeamID++
	st.EnterpriseTeams[t.ID] = t
	st.EnterpriseTeamsBySlug[t.Slug] = t
	st.persistEnterpriseTeam(t)
	return t
}

// GetEnterpriseTeam returns an enterprise team by slug, or nil.
// cloneEnterpriseTeam returns a deep copy safe to hand outside the store lock
// (STORE-021): GroupID, MemberIDs and SelectedOrgLogins are the reference
// fields. The mutators below re-fetch the live row by id.
func cloneEnterpriseTeam(t *EnterpriseTeam) *EnterpriseTeam {
	if t == nil {
		return nil
	}
	clone := *t
	if t.GroupID != nil {
		group := *t.GroupID
		clone.GroupID = &group
	}
	if t.MemberIDs != nil {
		clone.MemberIDs = append([]int(nil), t.MemberIDs...)
	}
	if t.SelectedOrgLogins != nil {
		clone.SelectedOrgLogins = append([]string(nil), t.SelectedOrgLogins...)
	}
	return &clone
}

// cloneEnterpriseCodeSecurityConfig deep-copies a configuration so callers hold
// a row detached from the stored one (its only reference fields are three
// optional scalars).
func cloneEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration) *EnterpriseCodeSecurityConfiguration {
	if c == nil {
		return nil
	}
	clone := *c
	if c.CodeScanningAllowAdvanced != nil {
		v := *c.CodeScanningAllowAdvanced
		clone.CodeScanningAllowAdvanced = &v
	}
	if c.CodeScanningRunnerType != nil {
		v := *c.CodeScanningRunnerType
		clone.CodeScanningRunnerType = &v
	}
	if c.CodeScanningRunnerLabel != nil {
		v := *c.CodeScanningRunnerLabel
		clone.CodeScanningRunnerLabel = &v
	}
	return &clone
}

func (st *Store) GetEnterpriseTeam(slug string) *EnterpriseTeam {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneEnterpriseTeam(st.EnterpriseTeamsBySlug[slug])
}

// ListEnterpriseTeams returns all enterprise teams sorted by ID.
func (st *Store) ListEnterpriseTeams() []*EnterpriseTeam {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*EnterpriseTeam, 0, len(st.EnterpriseTeams))
	for _, t := range st.EnterpriseTeams {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotEnterpriseTeams(out)
}

// UpdateEnterpriseTeam applies the non-nil fields. Renaming re-slugs the team
// exactly as GitHub does. Returns false when the new slug collides with a
// different existing team.
func (st *Store) UpdateEnterpriseTeam(t *EnterpriseTeam, name, description, selectionType, notificationSetting *string, groupID **string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	// Re-fetch the live row: the caller holds a detached clone from
	// GetEnterpriseTeam, so mutate the stored team and sync fresh state back
	// into the caller's pointer for rendering.
	live := st.EnterpriseTeams[t.ID]
	if live == nil {
		return false
	}

	if name != nil && *name != "" && *name != live.Name {
		newSlug := slugify(*name)
		if other, exists := st.EnterpriseTeamsBySlug[newSlug]; exists && other != live {
			return false
		}
		delete(st.EnterpriseTeamsBySlug, live.Slug)
		live.Name = *name
		live.Slug = newSlug
		st.EnterpriseTeamsBySlug[live.Slug] = live
	}
	if description != nil {
		live.Description = *description
	}
	if selectionType != nil {
		live.OrganizationSelectionType = *selectionType
	}
	if notificationSetting != nil {
		live.NotificationSetting = *notificationSetting
	}
	if groupID != nil {
		live.GroupID = *groupID
	}
	live.UpdatedAt = st.currentTime()
	st.persistEnterpriseTeam(live)
	*t = *cloneEnterpriseTeam(live)
	return true
}

// DeleteEnterpriseTeam removes an enterprise team by slug. Returns true if it
// existed.
func (st *Store) DeleteEnterpriseTeam(slug string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	t, ok := st.EnterpriseTeamsBySlug[slug]
	if !ok {
		return false
	}
	delete(st.EnterpriseTeamsBySlug, slug)
	delete(st.EnterpriseTeams, t.ID)
	if st.persist != nil {
		st.persist.MustDelete("enterprise_teams", strconv.Itoa(t.ID))
	}
	return true
}

// AddEnterpriseTeamMember adds a user to the team (idempotent).
func (st *Store) AddEnterpriseTeamMember(t *EnterpriseTeam, userID int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return
	}

	for _, id := range t.MemberIDs {
		if id == userID {
			return
		}
	}
	t.MemberIDs = append(t.MemberIDs, userID)
	t.UpdatedAt = st.currentTime()
	st.persistEnterpriseTeam(t)
}

// RemoveEnterpriseTeamMember removes a user from the team. Returns true if
// the user was a member.
func (st *Store) RemoveEnterpriseTeamMember(t *EnterpriseTeam, userID int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return false
	}

	for i, id := range t.MemberIDs {
		if id == userID {
			t.MemberIDs = append(t.MemberIDs[:i], t.MemberIDs[i+1:]...)
			t.UpdatedAt = st.currentTime()
			st.persistEnterpriseTeam(t)
			return true
		}
	}
	return false
}

// IsEnterpriseTeamMember reports whether the user belongs to the team.
func (st *Store) IsEnterpriseTeamMember(t *EnterpriseTeam, userID int) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return false
	}
	for _, id := range t.MemberIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// ListEnterpriseTeamMembers returns the team's members sorted by user ID.
func (st *Store) ListEnterpriseTeamMembers(t *EnterpriseTeam) []*User {
	st.mu.RLock()
	defer st.mu.RUnlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return nil
	}
	out := make([]*User, 0, len(t.MemberIDs))
	for _, id := range t.MemberIDs {
		if u := st.Users[id]; u != nil {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AddEnterpriseTeamOrg records an organization assignment (idempotent).
func (st *Store) AddEnterpriseTeamOrg(t *EnterpriseTeam, orgLogin string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return
	}

	for _, l := range t.SelectedOrgLogins {
		if l == orgLogin {
			return
		}
	}
	t.SelectedOrgLogins = append(t.SelectedOrgLogins, orgLogin)
	sort.Strings(t.SelectedOrgLogins)
	t.UpdatedAt = st.currentTime()
	st.persistEnterpriseTeam(t)
}

// RemoveEnterpriseTeamOrg removes an organization assignment. Returns true if
// it was assigned.
func (st *Store) RemoveEnterpriseTeamOrg(t *EnterpriseTeam, orgLogin string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return false
	}

	for i, l := range t.SelectedOrgLogins {
		if l == orgLogin {
			t.SelectedOrgLogins = append(t.SelectedOrgLogins[:i], t.SelectedOrgLogins[i+1:]...)
			t.UpdatedAt = st.currentTime()
			st.persistEnterpriseTeam(t)
			return true
		}
	}
	return false
}

// ListEnterpriseTeamOrgs resolves the team's organization assignments from
// its selection type: "all" assigns every organization on the instance,
// "selected" the recorded list, "disabled" none. Sorted by org ID.
func (st *Store) ListEnterpriseTeamOrgs(t *EnterpriseTeam) []*Org {
	st.mu.RLock()
	defer st.mu.RUnlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return nil
	}

	var out []*Org
	switch t.OrganizationSelectionType {
	case "all":
		for _, o := range st.Orgs {
			out = append(out, o)
		}
	case "selected":
		for _, l := range t.SelectedOrgLogins {
			if o := st.OrgsByLogin[l]; o != nil {
				out = append(out, o)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotOrgs(out)
}

// --- enterprise code security configurations ---

// CreateEnterpriseCodeSecurityConfig stores a new configuration.
func (st *Store) CreateEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration) *EnterpriseCodeSecurityConfiguration {
	st.mu.Lock()
	defer st.mu.Unlock()

	now := st.currentTime()
	c.ID = st.NextEnterpriseCodeSecurityConfigID
	st.NextEnterpriseCodeSecurityConfigID++
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.DefaultForNewRepos == "" {
		c.DefaultForNewRepos = "none"
	}
	st.EnterpriseCodeSecurityConfigs[c.ID] = c
	st.persistEnterpriseCodeSecurityConfig(c)
	return c
}

// GetEnterpriseCodeSecurityConfig returns a configuration by ID, or nil.
func (st *Store) GetEnterpriseCodeSecurityConfig(id int) *EnterpriseCodeSecurityConfiguration {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneEnterpriseCodeSecurityConfig(st.EnterpriseCodeSecurityConfigs[id])
}

// ListEnterpriseCodeSecurityConfigs returns all configurations sorted by ID.
func (st *Store) ListEnterpriseCodeSecurityConfigs() []*EnterpriseCodeSecurityConfiguration {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*EnterpriseCodeSecurityConfiguration, 0, len(st.EnterpriseCodeSecurityConfigs))
	for _, c := range st.EnterpriseCodeSecurityConfigs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotEnterpriseCodeSecurityConfigs(out)
}

// TouchEnterpriseCodeSecurityConfig bumps updated_at and persists after a
// caller-applied field mutation. Callers mutate under this lock via mutate.
func (st *Store) TouchEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration, mutate func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	// The caller holds a detached clone from GetEnterpriseCodeSecurityConfig;
	// mutate() applies its edits to that clone, which we then commit onto the
	// live row so concurrent readers observe the update through a stable pointer.
	live := st.EnterpriseCodeSecurityConfigs[c.ID]
	if live == nil {
		return
	}
	mutate()
	c.UpdatedAt = st.currentTime()
	*live = *cloneEnterpriseCodeSecurityConfig(c)
	st.persistEnterpriseCodeSecurityConfig(live)
}

// DeleteEnterpriseCodeSecurityConfig removes a configuration and detaches its
// repositories. Returns false when the configuration is a default for new
// repositories (GitHub refuses with 409 in that state).
func (st *Store) DeleteEnterpriseCodeSecurityConfig(id int) (deleted, conflict bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	c, ok := st.EnterpriseCodeSecurityConfigs[id]
	if !ok {
		return false, false
	}
	if c.DefaultForNewRepos != "none" {
		return false, true
	}
	// One transaction: dropping the config and detaching every repo attached to
	// it must not disagree across a crash, or a surviving attachment would point
	// at a config that no longer exists.
	batch := newPersistBatch(st.persist)
	delete(st.EnterpriseCodeSecurityConfigs, id)
	for repoID, cfgID := range st.EnterpriseCodeSecurityRepoConfigs {
		if cfgID == id {
			delete(st.EnterpriseCodeSecurityRepoConfigs, repoID)
			batch.Delete("enterprise_code_security_attachments", strconv.Itoa(repoID))
		}
	}
	batch.Delete("enterprise_code_security_configs", strconv.Itoa(id))
	if err := batch.Commit(); err != nil {
		panic(&persistenceFailure{op: "batch", bucket: "enterprise_code_security_configs", err: err})
	}
	return true, false
}

// AttachEnterpriseCodeSecurityConfig attaches the configuration to every
// organization-owned repository on the instance ("all"), or only to those
// without an attached configuration ("all_without_configurations").
func (st *Store) AttachEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration, scope string) {
	st.mu.Lock()
	defer st.mu.Unlock()

	for _, repo := range st.Repos {
		if repo.OwnerType != "Organization" {
			continue
		}
		if scope == "all_without_configurations" {
			if _, attached := st.EnterpriseCodeSecurityRepoConfigs[repo.ID]; attached {
				continue
			}
		}
		st.EnterpriseCodeSecurityRepoConfigs[repo.ID] = c.ID
		if st.persist != nil {
			st.persist.MustPut("enterprise_code_security_attachments", strconv.Itoa(repo.ID),
				&EnterpriseCodeSecurityAttachment{RepoID: repo.ID, ConfigID: c.ID})
		}
	}
}

// ListEnterpriseCodeSecurityConfigRepos returns the repositories attached to
// the configuration, sorted by repo ID.
func (st *Store) ListEnterpriseCodeSecurityConfigRepos(configID int) []*Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()

	var out []*Repo
	for repoID, cfgID := range st.EnterpriseCodeSecurityRepoConfigs {
		if cfgID != configID {
			continue
		}
		if repo := st.Repos[repoID]; repo != nil {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotRepos(out)
}

// SetEnterpriseCodeSecurityConfigDefault marks the configuration as the
// default for new repositories of the given visibility ("none" clears it).
func (st *Store) SetEnterpriseCodeSecurityConfigDefault(c *EnterpriseCodeSecurityConfiguration, defaultForNewRepos string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	// c is a detached clone; commit the change onto the live row.
	live := st.EnterpriseCodeSecurityConfigs[c.ID]
	if live == nil {
		return
	}
	c.DefaultForNewRepos = defaultForNewRepos
	c.UpdatedAt = st.currentTime()
	*live = *cloneEnterpriseCodeSecurityConfig(c)
	st.persistEnterpriseCodeSecurityConfig(live)
}

// --- enterprise settings mutators ---

// SetEnterpriseDependabotRepoAccess replaces the Dependabot accessible
// repository ID list.
func (st *Store) SetEnterpriseDependabotRepoAccess(ids []int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.EnterpriseSettings.DependabotAccessibleRepoIDs = append([]int(nil), ids...)
	st.persistEnterpriseSettings()
}

// SetEnterpriseDependabotDefaultLevel sets the Dependabot default repository
// access level (public|internal).
func (st *Store) SetEnterpriseDependabotDefaultLevel(level DependabotDefaultLevel) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.EnterpriseSettings.DependabotDefaultLevel = level
	st.persistEnterpriseSettings()
}

// SetEnterpriseActionsCacheRetentionDays sets the Actions cache retention
// limit in days.
func (st *Store) SetEnterpriseActionsCacheRetentionDays(days int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.EnterpriseSettings.ActionsCacheRetentionDays = days
	st.persistEnterpriseSettings()
}

// SetEnterpriseActionsCacheSizeGB sets the Actions cache storage limit in GB.
func (st *Store) SetEnterpriseActionsCacheSizeGB(gb int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.EnterpriseSettings.ActionsCacheSizeGB = gb
	if st.EnterpriseSettings.ActionsDefaultCacheSizeGB > gb {
		st.EnterpriseSettings.ActionsDefaultCacheSizeGB = gb
	}
	st.persistEnterpriseSettings()
}

// SetEnterpriseActionsCacheUsagePolicy atomically updates the default and
// maximum per-repository cache sizes.
func (st *Store) SetEnterpriseActionsCacheUsagePolicy(defaultGB, maxGB int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.EnterpriseSettings.ActionsDefaultCacheSizeGB = defaultGB
	st.EnterpriseSettings.ActionsCacheSizeGB = maxGB
	st.persistEnterpriseSettings()
}

// AddEnterpriseOIDCCustomProperty records an OIDC custom property inclusion.
// Returns false when the property is already included.
func (st *Store) AddEnterpriseOIDCCustomProperty(name string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, p := range st.EnterpriseSettings.OIDCCustomProperties {
		if p == name {
			return false
		}
	}
	st.EnterpriseSettings.OIDCCustomProperties = append(st.EnterpriseSettings.OIDCCustomProperties, name)
	st.persistEnterpriseSettings()
	return true
}

// RemoveEnterpriseOIDCCustomProperty removes an OIDC custom property
// inclusion. Returns true if it existed.
func (st *Store) RemoveEnterpriseOIDCCustomProperty(name string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, p := range st.EnterpriseSettings.OIDCCustomProperties {
		if p == name {
			st.EnterpriseSettings.OIDCCustomProperties = append(
				st.EnterpriseSettings.OIDCCustomProperties[:i],
				st.EnterpriseSettings.OIDCCustomProperties[i+1:]...)
			st.persistEnterpriseSettings()
			return true
		}
	}
	return false
}

// SetEnterpriseCopilotCodingAgentPolicy sets the Copilot coding agent policy
// state.
func (st *Store) SetEnterpriseCopilotCodingAgentPolicy(policyState string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.EnterpriseSettings.CopilotCodingAgentPolicy = policyState
	st.persistEnterpriseSettings()
}

// AddEnterpriseCopilotCodingAgentOrgs enables the Copilot coding agent for
// the given organization logins (idempotent, sorted).
func (st *Store) AddEnterpriseCopilotCodingAgentOrgs(logins []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	set := map[string]bool{}
	for _, l := range st.EnterpriseSettings.CopilotCodingAgentOrgs {
		set[l] = true
	}
	for _, l := range logins {
		set[l] = true
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	st.EnterpriseSettings.CopilotCodingAgentOrgs = out
	st.persistEnterpriseSettings()
}

// RemoveEnterpriseCopilotCodingAgentOrgs disables the Copilot coding agent
// for the given organization logins.
func (st *Store) RemoveEnterpriseCopilotCodingAgentOrgs(logins []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	drop := map[string]bool{}
	for _, l := range logins {
		drop[l] = true
	}
	kept := st.EnterpriseSettings.CopilotCodingAgentOrgs[:0]
	for _, l := range st.EnterpriseSettings.CopilotCodingAgentOrgs {
		if !drop[l] {
			kept = append(kept, l)
		}
	}
	st.EnterpriseSettings.CopilotCodingAgentOrgs = kept
	st.persistEnterpriseSettings()
}
