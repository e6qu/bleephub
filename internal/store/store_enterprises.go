package store

import (
	"sort"
	"strconv"
	"time"
)

// Enterprise-scoped state. Exactly one enterprise exists; its slug is
// configuration (BLEEPHUB_ENTERPRISE_SLUG), not store state.

// EnterpriseTeam is a team scoped to the enterprise, not one organization.
// OrganizationSelectionType governs org assignments: "disabled" none, "all"
// every org on the instance, "selected" exactly SelectedOrgLogins.
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
// fields hold enabled/disabled/not_set enum values.
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

// EnterpriseCodeSecurityAttachment persists one repo's config attachment. A
// repository has at most one attached configuration.
type EnterpriseCodeSecurityAttachment struct {
	RepoID   int `json:"repo_id"`
	ConfigID int `json:"config_id"`
}

// DependabotDefaultLevel is an enterprise's Dependabot default repository
// access level. Empty means "never set" (serialized as null).
type DependabotDefaultLevel string

const (
	DependabotDefaultLevelPublic   DependabotDefaultLevel = "public"
	DependabotDefaultLevelInternal DependabotDefaultLevel = "internal"
)

// EnterpriseSettings holds the singleton enterprise-level settings, persisted
// as one row under the "enterprise_settings" bucket. normalizeEnterpriseSettings
// seeds zero-value fields with defaults.
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

	// GitHub Actions cache policy. GHES defaults: 14-day retention, 10 GB
	// per-repository storage.
	ActionsCacheRetentionDays int `json:"actions_cache_retention_days"`
	ActionsCacheSizeGB        int `json:"actions_cache_size_gb"`
	ActionsDefaultCacheSizeGB int `json:"actions_default_cache_size_gb"`

	// Repository custom properties included in OIDC token claims, in insertion
	// order.
	OIDCCustomProperties      []string `json:"oidc_custom_properties"`
	OIDCIncludeEnterpriseSlug bool     `json:"oidc_include_enterprise_slug"`

	// Enterprise-wide Actions policy, stored independently of org policy: org
	// settings may narrow it but cannot replace this source of truth.
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

// EnterpriseAnnouncement is the enterprise-wide banner. ExpiresAt stays a
// string (GitHub returns ISO-8601, and null has distinct semantics).
type EnterpriseAnnouncement struct {
	Announcement    string  `json:"announcement"`
	ExpiresAt       *string `json:"expires_at"`
	UserDismissible bool    `json:"user_dismissible"`
}

// EnterpriseCodeSecurity is the legacy enterprise security policy GitHub keeps
// alongside code-security configurations.
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
// VendorSpecific holds opaque connection settings, never rendered to clients.
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

func (st *Store) PersistEnterpriseSettings() {
	if st.Persist != nil {
		st.Persist.MustPut("enterprise_settings", "enterprise", st.EnterpriseSettings)
	}
}

func (st *Store) PersistEnterpriseTeam(t *EnterpriseTeam) {
	if st.Persist != nil {
		st.Persist.MustPut("enterprise_teams", strconv.Itoa(t.ID), t)
	}
}

func (st *Store) persistEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration) {
	if st.Persist != nil {
		st.Persist.MustPut("enterprise_code_security_configs", strconv.Itoa(c.ID), c)
	}
}

// enterprise teams

// CreateEnterpriseTeam creates an enterprise team. Returns nil when a team
// with the same slug already exists.
func (st *Store) CreateEnterpriseTeam(name, description, selectionType string, groupID *string, notificationSetting string) *EnterpriseTeam {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	slug := Slugify(name)
	if _, exists := st.EnterpriseTeamsBySlug[slug]; exists {
		return nil
	}
	if selectionType == "" {
		selectionType = "disabled"
	}
	if notificationSetting == "" {
		notificationSetting = "notifications_enabled"
	}
	now := st.CurrentTime()
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
	st.PersistEnterpriseTeam(t)
	return t
}

// cloneEnterpriseTeam deep-copies a team so callers hold a row detached from
// the stored one (STORE-021); mutators re-fetch the live row by ID.
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
// a row detached from the stored one (STORE-021).
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

// GetEnterpriseTeam returns an enterprise team by slug, or nil.
func (st *Store) GetEnterpriseTeam(slug string) *EnterpriseTeam {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneEnterpriseTeam(st.EnterpriseTeamsBySlug[slug])
}

// ListEnterpriseTeams returns all enterprise teams sorted by ID.
func (st *Store) ListEnterpriseTeams() []*EnterpriseTeam {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*EnterpriseTeam, 0, len(st.EnterpriseTeams))
	for _, t := range st.EnterpriseTeams {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotEnterpriseTeams(out)
}

// ListEnterpriseCustomProperties returns detached snapshots of the
// enterprise-level repository custom property definitions, ordered by name.
func (st *Store) ListEnterpriseCustomProperties() []*CustomProperty {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*CustomProperty, 0, len(st.EnterpriseSettings.RepositoryCustomProperties))
	for _, p := range st.EnterpriseSettings.RepositoryCustomProperties {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PropertyName < out[j].PropertyName })
	return snapshotCustomProperties(out)
}

// UpdateEnterpriseTeam applies the non-nil fields, re-slugging on rename.
// Returns false when the new slug collides with a different team.
func (st *Store) UpdateEnterpriseTeam(t *EnterpriseTeam, name, description, selectionType, notificationSetting *string, groupID **string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// Caller holds a detached clone; mutate the live row and sync back into t.
	live := st.EnterpriseTeams[t.ID]
	if live == nil {
		return false
	}

	if name != nil && *name != "" && *name != live.Name {
		newSlug := Slugify(*name)
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
	live.UpdatedAt = st.CurrentTime()
	st.PersistEnterpriseTeam(live)
	*t = *cloneEnterpriseTeam(live)
	return true
}

// DeleteEnterpriseTeam removes an enterprise team by slug. Returns true if it
// existed.
func (st *Store) DeleteEnterpriseTeam(slug string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	t, ok := st.EnterpriseTeamsBySlug[slug]
	if !ok {
		return false
	}
	delete(st.EnterpriseTeamsBySlug, slug)
	delete(st.EnterpriseTeams, t.ID)
	if st.Persist != nil {
		st.Persist.MustDelete("enterprise_teams", strconv.Itoa(t.ID))
	}
	return true
}

// AddEnterpriseTeamMember adds a user to the team (idempotent).
func (st *Store) AddEnterpriseTeamMember(t *EnterpriseTeam, userID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
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
	t.UpdatedAt = st.CurrentTime()
	st.PersistEnterpriseTeam(t)
}

// RemoveEnterpriseTeamMember removes a user from the team. Returns true if
// the user was a member.
func (st *Store) RemoveEnterpriseTeamMember(t *EnterpriseTeam, userID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return false
	}

	for i, id := range t.MemberIDs {
		if id == userID {
			t.MemberIDs = append(t.MemberIDs[:i], t.MemberIDs[i+1:]...)
			t.UpdatedAt = st.CurrentTime()
			st.PersistEnterpriseTeam(t)
			return true
		}
	}
	return false
}

// IsEnterpriseTeamMember reports whether the user belongs to the team.
func (st *Store) IsEnterpriseTeamMember(t *EnterpriseTeam, userID int) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
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
	st.Mu.RLock()
	defer st.Mu.RUnlock()
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
	return snapshotUsers(out)
}

// AddEnterpriseTeamOrg records an organization assignment (idempotent).
func (st *Store) AddEnterpriseTeamOrg(t *EnterpriseTeam, orgLogin string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
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
	t.UpdatedAt = st.CurrentTime()
	st.PersistEnterpriseTeam(t)
}

// RemoveEnterpriseTeamOrg removes an organization assignment. Returns true if
// it was assigned.
func (st *Store) RemoveEnterpriseTeamOrg(t *EnterpriseTeam, orgLogin string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	t = st.EnterpriseTeams[t.ID]
	if t == nil {
		return false
	}

	for i, l := range t.SelectedOrgLogins {
		if l == orgLogin {
			t.SelectedOrgLogins = append(t.SelectedOrgLogins[:i], t.SelectedOrgLogins[i+1:]...)
			t.UpdatedAt = st.CurrentTime()
			st.PersistEnterpriseTeam(t)
			return true
		}
	}
	return false
}

// ListEnterpriseTeamOrgs resolves the team's organization assignments from
// its selection type: "all" assigns every organization on the instance,
// "selected" the recorded list, "disabled" none. Sorted by org ID.
func (st *Store) ListEnterpriseTeamOrgs(t *EnterpriseTeam) []*Org {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
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

// enterprise code security configurations

// CreateEnterpriseCodeSecurityConfig stores a new configuration.
func (st *Store) CreateEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration) *EnterpriseCodeSecurityConfiguration {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	now := st.CurrentTime()
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
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneEnterpriseCodeSecurityConfig(st.EnterpriseCodeSecurityConfigs[id])
}

// ListEnterpriseCodeSecurityConfigs returns all configurations sorted by ID.
func (st *Store) ListEnterpriseCodeSecurityConfigs() []*EnterpriseCodeSecurityConfiguration {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*EnterpriseCodeSecurityConfiguration, 0, len(st.EnterpriseCodeSecurityConfigs))
	for _, c := range st.EnterpriseCodeSecurityConfigs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotEnterpriseCodeSecurityConfigs(out)
}

// TouchEnterpriseCodeSecurityConfig runs mutate under the lock, then bumps
// updated_at and persists.
func (st *Store) TouchEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration, mutate func()) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// Caller holds a detached clone; mutate edits it, then commit onto the live row.
	live := st.EnterpriseCodeSecurityConfigs[c.ID]
	if live == nil {
		return
	}
	mutate()
	c.UpdatedAt = st.CurrentTime()
	*live = *cloneEnterpriseCodeSecurityConfig(c)
	st.persistEnterpriseCodeSecurityConfig(live)
}

// DeleteEnterpriseCodeSecurityConfig removes a configuration and detaches its
// repositories. Returns false when the configuration is a default for new
// repositories (GitHub refuses with 409 in that state).
func (st *Store) DeleteEnterpriseCodeSecurityConfig(id int) (deleted, conflict bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	c, ok := st.EnterpriseCodeSecurityConfigs[id]
	if !ok {
		return false, false
	}
	if c.DefaultForNewRepos != "none" {
		return false, true
	}
	// One transaction: a surviving attachment must never outlive its config.
	batch := NewPersistBatch(st.Persist)
	delete(st.EnterpriseCodeSecurityConfigs, id)
	for repoID, cfgID := range st.EnterpriseCodeSecurityRepoConfigs {
		if cfgID == id {
			delete(st.EnterpriseCodeSecurityRepoConfigs, repoID)
			batch.Delete("enterprise_code_security_attachments", strconv.Itoa(repoID))
		}
	}
	batch.Delete("enterprise_code_security_configs", strconv.Itoa(id))
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "enterprise_code_security_configs", Err: err})
	}
	return true, false
}

// AttachEnterpriseCodeSecurityConfig attaches the configuration to every
// organization-owned repository on the instance ("all"), or only to those
// without an attached configuration ("all_without_configurations").
func (st *Store) AttachEnterpriseCodeSecurityConfig(c *EnterpriseCodeSecurityConfiguration, scope string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

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
		if st.Persist != nil {
			st.Persist.MustPut("enterprise_code_security_attachments", strconv.Itoa(repo.ID),
				&EnterpriseCodeSecurityAttachment{RepoID: repo.ID, ConfigID: c.ID})
		}
	}
}

// ListEnterpriseCodeSecurityConfigRepos returns the repositories attached to
// the configuration, sorted by repo ID.
func (st *Store) ListEnterpriseCodeSecurityConfigRepos(configID int) []*Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

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
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// Caller holds a detached clone; commit onto the live row.
	live := st.EnterpriseCodeSecurityConfigs[c.ID]
	if live == nil {
		return
	}
	c.DefaultForNewRepos = defaultForNewRepos
	c.UpdatedAt = st.CurrentTime()
	*live = *cloneEnterpriseCodeSecurityConfig(c)
	st.persistEnterpriseCodeSecurityConfig(live)
}

// enterprise settings mutators

// SetEnterpriseDependabotRepoAccess replaces the Dependabot accessible
// repository ID list.
func (st *Store) SetEnterpriseDependabotRepoAccess(ids []int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.EnterpriseSettings.DependabotAccessibleRepoIDs = append([]int(nil), ids...)
	st.PersistEnterpriseSettings()
}

// SetEnterpriseDependabotDefaultLevel sets the Dependabot default repository
// access level (public|internal).
func (st *Store) SetEnterpriseDependabotDefaultLevel(level DependabotDefaultLevel) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.EnterpriseSettings.DependabotDefaultLevel = level
	st.PersistEnterpriseSettings()
}

// SetEnterpriseActionsCacheRetentionDays sets the Actions cache retention
// limit in days.
func (st *Store) SetEnterpriseActionsCacheRetentionDays(days int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.EnterpriseSettings.ActionsCacheRetentionDays = days
	st.PersistEnterpriseSettings()
}

// SetEnterpriseActionsCacheSizeGB sets the Actions cache storage limit in GB.
func (st *Store) SetEnterpriseActionsCacheSizeGB(gb int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.EnterpriseSettings.ActionsCacheSizeGB = gb
	if st.EnterpriseSettings.ActionsDefaultCacheSizeGB > gb {
		st.EnterpriseSettings.ActionsDefaultCacheSizeGB = gb
	}
	st.PersistEnterpriseSettings()
}

// SetEnterpriseActionsCacheUsagePolicy atomically updates the default and
// maximum per-repository cache sizes.
func (st *Store) SetEnterpriseActionsCacheUsagePolicy(defaultGB, maxGB int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.EnterpriseSettings.ActionsDefaultCacheSizeGB = defaultGB
	st.EnterpriseSettings.ActionsCacheSizeGB = maxGB
	st.PersistEnterpriseSettings()
}

// AddEnterpriseOIDCCustomProperty records an OIDC custom property inclusion.
// Returns false when the property is already included.
func (st *Store) AddEnterpriseOIDCCustomProperty(name string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, p := range st.EnterpriseSettings.OIDCCustomProperties {
		if p == name {
			return false
		}
	}
	st.EnterpriseSettings.OIDCCustomProperties = append(st.EnterpriseSettings.OIDCCustomProperties, name)
	st.PersistEnterpriseSettings()
	return true
}

// RemoveEnterpriseOIDCCustomProperty removes an OIDC custom property
// inclusion. Returns true if it existed.
func (st *Store) RemoveEnterpriseOIDCCustomProperty(name string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for i, p := range st.EnterpriseSettings.OIDCCustomProperties {
		if p == name {
			st.EnterpriseSettings.OIDCCustomProperties = append(
				st.EnterpriseSettings.OIDCCustomProperties[:i],
				st.EnterpriseSettings.OIDCCustomProperties[i+1:]...)
			st.PersistEnterpriseSettings()
			return true
		}
	}
	return false
}

// SetEnterpriseCopilotCodingAgentPolicy sets the Copilot coding agent policy
// state.
func (st *Store) SetEnterpriseCopilotCodingAgentPolicy(policyState string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.EnterpriseSettings.CopilotCodingAgentPolicy = policyState
	st.PersistEnterpriseSettings()
}

// AddEnterpriseCopilotCodingAgentOrgs enables the Copilot coding agent for
// the given organization logins (idempotent, sorted).
func (st *Store) AddEnterpriseCopilotCodingAgentOrgs(logins []string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
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
	st.PersistEnterpriseSettings()
}

// RemoveEnterpriseCopilotCodingAgentOrgs disables the Copilot coding agent
// for the given organization logins.
func (st *Store) RemoveEnterpriseCopilotCodingAgentOrgs(logins []string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
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
	st.PersistEnterpriseSettings()
}
