package bleephub

import (
	"testing"
)

// TestEnterpriseStatePersistenceReload verifies every enterprise store
// surface — teams (with members and organization assignments), code security
// configurations (with attachments and next-ID counter), and the singleton
// enterprise settings — survives a persistence close/reopen cycle.
func TestEnterpriseStatePersistenceReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	// --- session 1: create enterprise state, then close ---
	p1, err := NewPersistence()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	st1.SeedDefaultUser()
	admin := st1.UsersByLogin["admin"]

	groupID := "62ab9291-fae2-468e-974b-7e45096d5021"
	team := st1.CreateEnterpriseTeam("Reload Crew", "survives restarts", "selected", &groupID, "notifications_disabled")
	if team == nil {
		t.Fatal("CreateEnterpriseTeam returned nil")
	}
	st1.AddEnterpriseTeamMember(team, admin.ID)
	st1.AddEnterpriseTeamOrg(team, "reload-org")

	cfg := st1.CreateEnterpriseCodeSecurityConfig(&EnterpriseCodeSecurityConfiguration{
		Name:            "reload-config",
		Description:     "reload coverage",
		SecretScanning:  "enabled",
		DependencyGraph: "enabled",
		Enforcement:     "enforced",
	})
	st1.SetEnterpriseCodeSecurityConfigDefault(cfg, "public")
	enterpriseRuleset := st1.CreateEnterpriseRuleset("bleephub", &Ruleset{
		Name: "reload-enterprise-ruleset", Target: "branch", Enforcement: "active",
		Rules: []Rule{{Type: "deletion"}},
	})
	enterprisePatterns := st1.CreateSecretScanningCustomPatterns("enterprise:bleephub", []secretScanningPatternCreate{{
		Name: "reload-enterprise-pattern", Pattern: `ent_[0-9a-f]{16}`,
	}})
	repo := st1.CreateRepo(admin, "ent-reload-repo", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	// Attach directly (the repo is user-owned; the association mechanism is
	// what reload must preserve).
	st1.mu.Lock()
	st1.EnterpriseCodeSecurityRepoConfigs[repo.ID] = cfg.ID
	st1.persist.MustPut("enterprise_code_security_attachments", "1",
		&EnterpriseCodeSecurityAttachment{RepoID: repo.ID, ConfigID: cfg.ID})
	st1.mu.Unlock()

	st1.SetEnterpriseDependabotRepoAccess([]int{repo.ID})
	st1.SetEnterpriseDependabotDefaultLevel("internal")
	st1.SetEnterpriseActionsCacheRetentionDays(21)
	st1.SetEnterpriseActionsCacheSizeGB(42)
	if !st1.AddEnterpriseOIDCCustomProperty("cost_center") {
		t.Fatal("AddEnterpriseOIDCCustomProperty returned false for a fresh property")
	}
	st1.SetEnterpriseCopilotCodingAgentPolicy("enabled_for_selected_orgs")
	st1.AddEnterpriseCopilotCodingAgentOrgs([]string{"reload-org"})
	st1.mu.Lock()
	expiry := "2030-01-02T03:04:05Z"
	customLink := "https://example.test/security"
	st1.EnterpriseSettings.Announcement = &EnterpriseAnnouncement{
		Announcement: "Persistent announcement", ExpiresAt: &expiry, UserDismissible: true,
	}
	st1.EnterpriseSettings.AccessRestrictionsEnabled = true
	st1.EnterpriseSettings.CodeSecurityAndAnalysis = EnterpriseCodeSecurity{
		AdvancedSecurityEnabledForNewRepositories: true,
		SecretScanningPushProtectionCustomLink:    &customLink,
	}
	pausedAt := st1.currentTime()
	st1.EnterpriseSettings.AuditLogStreams = []*EnterpriseAuditLogStream{{
		ID: 7, StreamType: "Datadog", StreamDetails: "EU1", Enabled: false,
		VendorSpecific: map[string]interface{}{"site": "EU1", "encrypted_token": "sealed"},
		CreatedAt:      pausedAt, UpdatedAt: pausedAt, PausedAt: &pausedAt,
	}}
	st1.EnterpriseSettings.NextAuditLogStreamID = 8
	st1.EnterpriseSettings.RepositoryCustomProperties["environment"] = &CustomProperty{
		PropertyName: "environment", ValueType: "single_select", AllowedValues: []string{"prod", "dev"},
	}
	st1.EnterpriseSettings.OrganizationCustomProperties["cost_center"] = &CustomProperty{
		PropertyName: "cost_center", ValueType: "string", ValuesEditableBy: "enterprise_actors",
	}
	st1.EnterpriseSettings.OrganizationPropertyValues["reload-org"] = map[string]interface{}{"cost_center": "CC-42"}
	st1.EnterpriseSettings.SCIMUsers["scim-user-reload"] = &EnterpriseSCIMUser{
		Schemas: []string{scimUserSchema}, ID: "scim-user-reload", ExternalID: "directory-user-reload",
		UserName: admin.Login, DisplayName: "Reload SCIM User", Active: true,
		UserID: admin.ID, CreatedAt: st1.currentTime(), UpdatedAt: st1.currentTime(),
	}
	st1.EnterpriseSettings.SCIMGroups["scim-group-reload"] = &EnterpriseSCIMGroup{
		Schemas: []string{scimGroupSchema}, ID: "scim-group-reload", ExternalID: "directory-group-reload",
		DisplayName: "Reload Crew", Members: []EnterpriseSCIMMember{{Value: "scim-user-reload"}},
		TeamID: team.ID, CreatedAt: st1.currentTime(), UpdatedAt: st1.currentTime(),
	}
	st1.EnterpriseSettings.EnterpriseRoleTeamAssignments[8030] = []int{team.ID}
	st1.EnterpriseSettings.EnterpriseRoleUserAssignments[8031] = []int{admin.ID}
	st1.EnterpriseSettings.VisualStudioSubscriptions["00000000-0000-0000-0000-000000000042"] = &VisualStudioSubscription{
		SubscriptionID: "00000000-0000-0000-0000-000000000042",
		Email:          admin.Email, Username: admin.Login, ManualMatch: true,
	}
	st1.EnterpriseSettings.InnerSourceSyncJobs["external-vulnerability-sync-reload"] = &EnterpriseInnerSourceSyncJob{
		ID: "external-vulnerability-sync-reload", Status: "completed", Processed: 1, Created: 1,
		Results: []EnterpriseInnerSourceSyncResult{{
			ExternalID: "MVS-reload", Status: "created", GHSAID: "GHIS-reload",
		}},
		CreatedAt: st1.currentTime(), UpdatedAt: st1.currentTime(),
	}
	st1.EnterpriseSettings.EnterpriseCopilotSeats["user:1"] = &CopilotSeat{
		OrgLogin: "enterprise:bleephub", UserID: admin.ID,
		CreatedAt: st1.currentTime(), UpdatedAt: st1.currentTime(),
	}
	st1.EnterpriseSettings.CopilotCustomAgentsSourceOrgID = 42
	st1.EnterpriseSettings.CopilotCustomAgentsRulesetID = 73
	st1.EnterpriseSettings.OIDCIncludeEnterpriseSlug = true
	st1.EnterpriseSettings.ActionsEnabledOrganizations = "selected"
	st1.EnterpriseSettings.ActionsAllowedActions = "selected"
	st1.EnterpriseSettings.ActionsSHAPinningRequired = true
	st1.EnterpriseSettings.ActionsSelectedOrganizationIDs = []int{17}
	st1.EnterpriseSettings.ActionsAllowed = &ActionsAllowed{
		GithubOwnedAllowed: true,
		PatternsAllowed:    []string{"actions/*"},
	}
	st1.EnterpriseSettings.ActionsWorkflowPermissions = &WorkflowPermissions{
		DefaultWorkflowPermissions:   "write",
		CanApprovePullRequestReviews: true,
	}
	st1.EnterpriseSettings.ActionsArtifactRetentionDays = 120
	st1.EnterpriseSettings.ActionsForkPRApprovalPolicy = "all_external_contributors"
	st1.EnterpriseSettings.ActionsForkPRWorkflowsPrivate = &ForkPRWorkflowsPrivateRepos{
		RunWorkflowsFromForkPullRequests: true,
	}
	st1.EnterpriseSettings.ActionsDisableSelfHostedRunners = true
	st1.persistEnterpriseSettings()
	st1.mu.Unlock()
	enterpriseNetworkScope := "enterprise:bleephub"
	enterpriseNetworkSettings, err := st1.CreateNetworkSettings(
		enterpriseNetworkScope, "reload-enterprise-network", "/subscriptions/reload/subnets/actions", "eastus")
	if err != nil {
		t.Fatalf("CreateNetworkSettings: %v", err)
	}
	enterpriseNetworkName := "reload_enterprise_network"
	enterpriseComputeService := "actions"
	enterpriseNetwork, err := st1.CreateNetworkConfiguration(enterpriseNetworkScope, &networkConfigurationRequest{
		Name: &enterpriseNetworkName, ComputeService: &enterpriseComputeService,
		NetworkSettingsIDs: []string{enterpriseNetworkSettings.ID},
	})
	if err != nil {
		t.Fatalf("CreateNetworkConfiguration: %v", err)
	}
	enterpriseImage := st1.CreateEnterpriseHostedRunnerCustomImage(
		"bleephub", "Reload Enterprise Image", "linux-x64")
	if !st1.AddHostedRunnerCustomImageVersion(enterpriseImage.ID, "1.0.0", 30) {
		t.Fatal("add enterprise custom image version")
	}
	st1.mu.Lock()
	hostedRunnerID := st1.NextHostedRunnerID
	st1.NextHostedRunnerID++
	st1.HostedRunners[hostedRunnerID] = &HostedRunner{
		ID: hostedRunnerID, Enterprise: "bleephub", Name: "reload-enterprise-hosted",
		RunnerGroupID: 47, ImageID: "ubuntu-24.04", ImageSource: "github",
		MachineSizeID: "4-core", MaximumRunners: 3, CreatedAt: st1.currentTime(),
	}
	st1.persistHostedRunnerLocked(st1.HostedRunners[hostedRunnerID])
	st1.mu.Unlock()

	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// --- session 2: reload and assert every surface came back ---
	p2, err := NewPersistence()
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("re-load SetPersistence: %v", err)
	}
	defer p2.Close()

	got := st2.GetEnterpriseTeam("reload-crew")
	if got == nil {
		t.Fatal("enterprise team did not persist")
	}
	if got.Name != "Reload Crew" || got.OrganizationSelectionType != "selected" || got.NotificationSetting != "notifications_disabled" {
		t.Errorf("reloaded team = %+v", got)
	}
	if got.GroupID == nil || *got.GroupID != groupID {
		t.Errorf("reloaded team group_id = %v, want %s", got.GroupID, groupID)
	}
	if !st2.IsEnterpriseTeamMember(got, admin.ID) {
		t.Error("team membership did not persist")
	}
	if len(got.SelectedOrgLogins) != 1 || got.SelectedOrgLogins[0] != "reload-org" {
		t.Errorf("team org assignments = %v", got.SelectedOrgLogins)
	}
	// The team ID counter resumes past the loaded team.
	next := st2.CreateEnterpriseTeam("Another Crew", "", "", nil, "")
	if next == nil || next.ID <= got.ID {
		t.Errorf("post-reload team ID = %v, want > %d", next, got.ID)
	}

	cfg2 := st2.GetEnterpriseCodeSecurityConfig(cfg.ID)
	if cfg2 == nil {
		t.Fatal("code security configuration did not persist")
	}
	if cfg2.Name != "reload-config" || cfg2.SecretScanning != "enabled" || cfg2.DefaultForNewRepos != "public" {
		t.Errorf("reloaded configuration = %+v", cfg2)
	}
	if st2.EnterpriseCodeSecurityRepoConfigs[repo.ID] != cfg.ID {
		t.Error("code security attachment did not persist")
	}
	if st2.NextEnterpriseCodeSecurityConfigID <= cfg.ID {
		t.Errorf("configuration ID counter = %d, want > %d", st2.NextEnterpriseCodeSecurityConfigID, cfg.ID)
	}
	if gotRuleset := st2.GetEnterpriseRuleset("bleephub", enterpriseRuleset.ID); gotRuleset == nil ||
		gotRuleset.Name != "reload-enterprise-ruleset" || len(gotRuleset.Rules) != 1 {
		t.Errorf("enterprise ruleset did not persist: %+v", gotRuleset)
	}
	if gotPatterns := st2.ListSecretScanningCustomPatterns("enterprise:bleephub"); len(gotPatterns) != 1 ||
		gotPatterns[0].ID != enterprisePatterns[0].ID || gotPatterns[0].Name != "reload-enterprise-pattern" {
		t.Errorf("enterprise secret scanning patterns did not persist: %+v", gotPatterns)
	}
	if st2.GetNetworkConfiguration(enterpriseNetworkScope, enterpriseNetwork.ID) == nil ||
		st2.GetNetworkSettings(enterpriseNetworkScope, enterpriseNetworkSettings.ID) == nil {
		t.Error("enterprise network configuration did not persist")
	}

	s := st2.EnterpriseSettings
	if s.Announcement == nil || s.Announcement.Announcement != "Persistent announcement" ||
		s.Announcement.ExpiresAt == nil || *s.Announcement.ExpiresAt != "2030-01-02T03:04:05Z" ||
		!s.Announcement.UserDismissible || !s.AccessRestrictionsEnabled ||
		!s.CodeSecurityAndAnalysis.AdvancedSecurityEnabledForNewRepositories ||
		s.CodeSecurityAndAnalysis.SecretScanningPushProtectionCustomLink == nil ||
		*s.CodeSecurityAndAnalysis.SecretScanningPushProtectionCustomLink != "https://example.test/security" {
		t.Errorf("enterprise administration settings = %+v", s)
	}
	if len(s.AuditLogStreams) != 1 || s.AuditLogStreams[0].ID != 7 ||
		s.AuditLogStreams[0].StreamType != "Datadog" ||
		s.AuditLogStreams[0].VendorSpecific["encrypted_token"] != "sealed" ||
		s.NextAuditLogStreamID != 8 {
		t.Errorf("enterprise audit streams = %+v, next=%d", s.AuditLogStreams, s.NextAuditLogStreamID)
	}
	if s.RepositoryCustomProperties["environment"] == nil ||
		len(s.RepositoryCustomProperties["environment"].AllowedValues) != 2 ||
		s.OrganizationCustomProperties["cost_center"] == nil ||
		s.OrganizationPropertyValues["reload-org"]["cost_center"] != "CC-42" {
		t.Errorf("enterprise custom properties = repo:%+v org:%+v values:%+v",
			s.RepositoryCustomProperties, s.OrganizationCustomProperties, s.OrganizationPropertyValues)
	}
	if s.SCIMUsers["scim-user-reload"] == nil ||
		s.SCIMUsers["scim-user-reload"].UserID != admin.ID ||
		s.SCIMGroups["scim-group-reload"] == nil ||
		len(s.SCIMGroups["scim-group-reload"].Members) != 1 ||
		s.SCIMGroups["scim-group-reload"].TeamID != team.ID {
		t.Errorf("enterprise SCIM resources did not persist: users=%+v groups=%+v", s.SCIMUsers, s.SCIMGroups)
	}
	if len(s.EnterpriseRoleTeamAssignments[8030]) != 1 ||
		s.EnterpriseRoleTeamAssignments[8030][0] != team.ID ||
		len(s.EnterpriseRoleUserAssignments[8031]) != 1 ||
		s.EnterpriseRoleUserAssignments[8031][0] != admin.ID {
		t.Errorf("enterprise role assignments did not persist: teams=%+v users=%+v",
			s.EnterpriseRoleTeamAssignments, s.EnterpriseRoleUserAssignments)
	}
	if s.VisualStudioSubscriptions["00000000-0000-0000-0000-000000000042"] == nil ||
		!s.VisualStudioSubscriptions["00000000-0000-0000-0000-000000000042"].ManualMatch ||
		s.InnerSourceSyncJobs["external-vulnerability-sync-reload"] == nil ||
		len(s.InnerSourceSyncJobs["external-vulnerability-sync-reload"].Results) != 1 {
		t.Errorf("enterprise licensing state did not persist: subscriptions=%+v inner_source=%+v",
			s.VisualStudioSubscriptions, s.InnerSourceSyncJobs)
	}
	if s.EnterpriseCopilotSeats["user:1"] == nil ||
		s.EnterpriseCopilotSeats["user:1"].UserID != admin.ID ||
		s.CopilotCustomAgentsSourceOrgID != 42 || s.CopilotCustomAgentsRulesetID != 73 {
		t.Errorf("enterprise Copilot state did not persist: seats=%+v source=%d ruleset=%d",
			s.EnterpriseCopilotSeats, s.CopilotCustomAgentsSourceOrgID, s.CopilotCustomAgentsRulesetID)
	}
	if len(s.DependabotAccessibleRepoIDs) != 1 || s.DependabotAccessibleRepoIDs[0] != repo.ID {
		t.Errorf("dependabot access = %v", s.DependabotAccessibleRepoIDs)
	}
	if s.DependabotDefaultLevel != "internal" {
		t.Errorf("dependabot default level = %q", s.DependabotDefaultLevel)
	}
	if s.ActionsCacheRetentionDays != 21 || s.ActionsCacheSizeGB != 42 {
		t.Errorf("actions cache limits = %d days / %d GB, want 21 / 42", s.ActionsCacheRetentionDays, s.ActionsCacheSizeGB)
	}
	if len(s.OIDCCustomProperties) != 1 || s.OIDCCustomProperties[0] != "cost_center" {
		t.Errorf("OIDC custom properties = %v", s.OIDCCustomProperties)
	}
	if !s.OIDCIncludeEnterpriseSlug || s.ActionsEnabledOrganizations != "selected" ||
		s.ActionsAllowedActions != "selected" || !s.ActionsSHAPinningRequired ||
		len(s.ActionsSelectedOrganizationIDs) != 1 || s.ActionsSelectedOrganizationIDs[0] != 17 {
		t.Errorf("enterprise Actions base policy = %+v", s)
	}
	if s.ActionsAllowed == nil || !s.ActionsAllowed.GithubOwnedAllowed ||
		len(s.ActionsAllowed.PatternsAllowed) != 1 {
		t.Errorf("enterprise selected actions = %+v", s.ActionsAllowed)
	}
	if s.ActionsWorkflowPermissions == nil ||
		s.ActionsWorkflowPermissions.DefaultWorkflowPermissions != "write" ||
		!s.ActionsWorkflowPermissions.CanApprovePullRequestReviews {
		t.Errorf("enterprise workflow permissions = %+v", s.ActionsWorkflowPermissions)
	}
	if s.ActionsArtifactRetentionDays != 120 ||
		s.ActionsForkPRApprovalPolicy != "all_external_contributors" ||
		s.ActionsForkPRWorkflowsPrivate == nil ||
		!s.ActionsForkPRWorkflowsPrivate.RunWorkflowsFromForkPullRequests ||
		!s.ActionsDisableSelfHostedRunners {
		t.Errorf("enterprise extended Actions policy = %+v", s)
	}
	if s.CopilotCodingAgentPolicy != "enabled_for_selected_orgs" {
		t.Errorf("copilot policy = %q", s.CopilotCodingAgentPolicy)
	}
	if len(s.CopilotCodingAgentOrgs) != 1 || s.CopilotCodingAgentOrgs[0] != "reload-org" {
		t.Errorf("copilot orgs = %v", s.CopilotCodingAgentOrgs)
	}
	if runner := st2.HostedRunners[hostedRunnerID]; runner == nil ||
		runner.Enterprise != "bleephub" || runner.Org != "" {
		t.Errorf("enterprise hosted runner did not retain ownership: %+v", runner)
	}
	if image := st2.HostedRunnerCustomImages[enterpriseImage.ID]; image == nil ||
		image.Enterprise != "bleephub" || image.Org != "" || len(image.Versions) != 1 {
		t.Errorf("enterprise hosted runner image did not retain ownership: %+v", image)
	}
}
