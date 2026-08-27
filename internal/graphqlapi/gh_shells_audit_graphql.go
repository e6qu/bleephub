package graphqlapi

// Introspection-only shells for the OrganizationAuditEntry concrete members
// bleephub never writes (the audit-log family builds the ones it does record).
// They exist so the schema matches GitHub's shape; no resolved field returns
// them. Every type is transcribed signature-exact from the vendored SDL.

import "github.com/graphql-go/graphql"

func init() {
	schemaShellBuilders = append(schemaShellBuilders, (*Resolver).addAuditEntryShells)
}

// --- mixin field sets the new interfaces and their implementers share ---------

func (s *Resolver) enterpriseAuditDataFields() graphql.Fields {
	uri := s.graphQLStringScalar("URI")
	return graphql.Fields{
		"enterpriseResourcePath": gqlField(uri),
		"enterpriseSlug":         gqlField(graphql.String),
		"enterpriseUrl":          gqlField(uri),
	}
}

func (s *Resolver) oauthAppAuditDataFields() graphql.Fields {
	uri := s.graphQLStringScalar("URI")
	return graphql.Fields{
		"oauthApplicationName":         gqlField(graphql.String),
		"oauthApplicationResourcePath": gqlField(uri),
		"oauthApplicationUrl":          gqlField(uri),
	}
}

func (s *Resolver) teamAuditDataFields() graphql.Fields {
	uri := s.graphQLStringScalar("URI")
	return graphql.Fields{
		"team":             gqlField(s.graphqlTypes.team),
		"teamName":         gqlField(graphql.String),
		"teamResourcePath": gqlField(uri),
		"teamUrl":          gqlField(uri),
	}
}

func (s *Resolver) topicAuditDataFields() graphql.Fields {
	return graphql.Fields{
		"topic":     gqlField(s.namedObject("Topic")),
		"topicName": gqlField(graphql.String),
	}
}

// auditShellIDField is the id: ID! every concrete audit entry declares.
func auditShellIDField() graphql.Fields {
	return graphql.Fields{"id": gqlNonNull(graphql.ID)}
}

// auditShellObject builds one audit-entry shell from mixin field maps plus any
// extras, memoized by GitHub's name. It declares no interfaces: introspection
// only needs the type to exist.
func (s *Resolver) auditShellObject(name string, parts ...graphql.Fields) *graphql.Object {
	fields := graphql.Fields{}
	for _, part := range parts {
		for k, v := range part {
			fields[k] = v
		}
	}
	return s.mutationObject(name, fields)
}

// addAuditEntryShells mints every OrganizationAuditEntry concrete-member shell,
// the four mixin interfaces, the sub-enums and the restore-member union.
func (s *Resolver) addAuditEntryShells() {
	uri := s.graphQLStringScalar("URI")

	// --- mixin interfaces (zero-implementer: the shells are plain objects) ---
	nilResolve := func(graphql.ResolveTypeParams) *graphql.Object { return nil }
	enterpriseData := s.mutationInterface("EnterpriseAuditEntryData",
		func() graphql.Fields { return s.enterpriseAuditDataFields() }, nilResolve)
	oauthAppData := s.mutationInterface("OauthApplicationAuditEntryData",
		func() graphql.Fields { return s.oauthAppAuditDataFields() }, nilResolve)
	teamData := s.mutationInterface("TeamAuditEntryData",
		func() graphql.Fields { return s.teamAuditDataFields() }, nilResolve)
	topicData := s.mutationInterface("TopicAuditEntryData",
		func() graphql.Fields { return s.topicAuditDataFields() }, nilResolve)
	s.registerExtraSchemaType(enterpriseData, oauthAppData, teamData, topicData)

	// --- sub-enums ----------------------------------------------------------
	oauthAppCreateState := s.sharedEnum("OauthApplicationCreateAuditEntryState",
		"ACTIVE", "PENDING_DELETION", "SUSPENDED")
	orgAddMemberPermission := s.sharedEnum("OrgAddMemberAuditEntryPermission",
		"ADMIN", "READ")
	orgRemoveBillingManagerReason := s.sharedEnum("OrgRemoveBillingManagerAuditEntryReason",
		"SAML_EXTERNAL_IDENTITY_MISSING", "SAML_SSO_ENFORCEMENT_REQUIRES_EXTERNAL_IDENTITY",
		"TWO_FACTOR_REQUIREMENT_NON_COMPLIANCE")
	orgRemoveMemberMembershipType := s.sharedEnum("OrgRemoveMemberAuditEntryMembershipType",
		"ADMIN", "BILLING_MANAGER", "DIRECT_MEMBER", "OUTSIDE_COLLABORATOR", "SUSPENDED", "UNAFFILIATED")
	orgRemoveMemberReason := s.sharedEnum("OrgRemoveMemberAuditEntryReason",
		"SAML_EXTERNAL_IDENTITY_MISSING", "SAML_SSO_ENFORCEMENT_REQUIRES_EXTERNAL_IDENTITY",
		"TWO_FACTOR_ACCOUNT_RECOVERY", "TWO_FACTOR_REQUIREMENT_NON_COMPLIANCE", "USER_ACCOUNT_DELETED")
	orgUpdateDefaultRepoPermission := s.sharedEnum("OrgUpdateDefaultRepositoryPermissionAuditEntryPermission",
		"ADMIN", "NONE", "READ", "WRITE")
	orgUpdateMemberPermission := s.sharedEnum("OrgUpdateMemberAuditEntryPermission",
		"ADMIN", "READ")
	orgUpdateMemberRepoCreationVisibility := s.sharedEnum("OrgUpdateMemberRepositoryCreationPermissionAuditEntryVisibility",
		"ALL", "INTERNAL", "NONE", "PRIVATE", "PRIVATE_INTERNAL", "PUBLIC", "PUBLIC_INTERNAL", "PUBLIC_PRIVATE")
	repoAccessVisibility := s.sharedEnum("RepoAccessAuditEntryVisibility",
		"INTERNAL", "PRIVATE", "PUBLIC")
	repoAddMemberVisibility := s.sharedEnum("RepoAddMemberAuditEntryVisibility",
		"INTERNAL", "PRIVATE", "PUBLIC")
	repoArchivedVisibility := s.sharedEnum("RepoArchivedAuditEntryVisibility",
		"INTERNAL", "PRIVATE", "PUBLIC")
	repoChangeMergeSettingMergeType := s.sharedEnum("RepoChangeMergeSettingAuditEntryMergeType",
		"MERGE", "REBASE", "SQUASH")
	repoRemoveMemberVisibility := s.sharedEnum("RepoRemoveMemberAuditEntryVisibility",
		"INTERNAL", "PRIVATE", "PUBLIC")
	s.registerExtraSchemaType(
		oauthAppCreateState, orgAddMemberPermission, orgRemoveBillingManagerReason,
		orgRemoveMemberMembershipType, orgRemoveMemberReason, orgUpdateDefaultRepoPermission,
		orgUpdateMemberPermission, orgUpdateMemberRepoCreationVisibility, repoAccessVisibility,
		repoAddMemberVisibility, repoArchivedVisibility, repoChangeMergeSettingMergeType,
		repoRemoveMemberVisibility)

	// --- OrgRestoreMember membership data objects + union -------------------
	restoreOrgData := s.mutationObject("OrgRestoreMemberMembershipOrganizationAuditEntryData",
		s.orgAuditDataFields())
	restoreRepoData := s.mutationObject("OrgRestoreMemberMembershipRepositoryAuditEntryData",
		s.repoAuditDataFields())
	restoreTeamData := s.mutationObject("OrgRestoreMemberMembershipTeamAuditEntryData",
		s.teamAuditDataFields())
	restoreMembership := s.mutationUnion("OrgRestoreMemberAuditEntryMembership",
		func() []*graphql.Object {
			return []*graphql.Object{restoreOrgData, restoreRepoData, restoreTeamData}
		}, nilResolve)
	s.registerExtraSchemaType(restoreOrgData, restoreRepoData, restoreTeamData, restoreMembership)

	// --- concrete member objects -------------------------------------------
	// Shared mixin sets, rebuilt per object (the field builders return fresh maps).
	base := func() graphql.Fields { return s.auditEntryBaseFields() }
	org := func() graphql.Fields { return s.orgAuditDataFields() }
	repo := func() graphql.Fields { return s.repoAuditDataFields() }
	ent := func() graphql.Fields { return s.enterpriseAuditDataFields() }
	oauthApp := func() graphql.Fields { return s.oauthAppAuditDataFields() }
	team := func() graphql.Fields { return s.teamAuditDataFields() }
	topic := func() graphql.Fields { return s.topicAuditDataFields() }
	id := auditShellIDField

	objects := []*graphql.Object{
		// MembersCanDeleteRepos* : AuditEntry & EnterpriseAuditEntryData & Node & OrganizationAuditEntryData
		s.auditShellObject("MembersCanDeleteReposClearAuditEntry", base(), id(), ent(), org()),
		s.auditShellObject("MembersCanDeleteReposDisableAuditEntry", base(), id(), ent(), org()),
		s.auditShellObject("MembersCanDeleteReposEnableAuditEntry", base(), id(), ent(), org()),

		// OauthApplicationCreateAuditEntry
		s.auditShellObject("OauthApplicationCreateAuditEntry", base(), id(), oauthApp(), org(), graphql.Fields{
			"applicationUrl": gqlField(uri),
			"callbackUrl":    gqlField(uri),
			"rateLimit":      gqlField(graphql.Int),
			"state":          gqlField(oauthAppCreateState),
		}),

		// Org* org-scoped entries
		s.auditShellObject("OrgAddBillingManagerAuditEntry", base(), id(), org(), graphql.Fields{
			"invitationEmail": gqlField(graphql.String),
		}),
		s.auditShellObject("OrgAddMemberAuditEntry", base(), id(), org(), graphql.Fields{
			"permission": gqlField(orgAddMemberPermission),
		}),
		s.auditShellObject("OrgConfigDisableCollaboratorsOnlyAuditEntry", base(), id(), org()),
		s.auditShellObject("OrgConfigEnableCollaboratorsOnlyAuditEntry", base(), id(), org()),
		s.auditShellObject("OrgDisableOauthAppRestrictionsAuditEntry", base(), id(), org()),
		s.auditShellObject("OrgDisableSamlAuditEntry", base(), id(), org(), graphql.Fields{
			"digestMethodUrl":    gqlField(uri),
			"issuerUrl":          gqlField(uri),
			"signatureMethodUrl": gqlField(uri),
			"singleSignOnUrl":    gqlField(uri),
		}),
		s.auditShellObject("OrgDisableTwoFactorRequirementAuditEntry", base(), id(), org()),
		s.auditShellObject("OrgEnableOauthAppRestrictionsAuditEntry", base(), id(), org()),
		s.auditShellObject("OrgEnableSamlAuditEntry", base(), id(), org(), graphql.Fields{
			"digestMethodUrl":    gqlField(uri),
			"issuerUrl":          gqlField(uri),
			"signatureMethodUrl": gqlField(uri),
			"singleSignOnUrl":    gqlField(uri),
		}),
		s.auditShellObject("OrgEnableTwoFactorRequirementAuditEntry", base(), id(), org()),
		// OrgInviteMemberAuditEntry is not a shell: it is a produced member of
		// the OrganizationAuditEntry union (org.invite_member is recorded), built
		// as a real interface-implementing type in gh_audit_log_graphql.go.
		s.auditShellObject("OrgInviteToBusinessAuditEntry", base(), id(), ent(), org()),
		s.auditShellObject("OrgOauthAppAccessApprovedAuditEntry", base(), id(), oauthApp(), org()),
		s.auditShellObject("OrgOauthAppAccessBlockedAuditEntry", base(), id(), oauthApp(), org()),
		s.auditShellObject("OrgOauthAppAccessDeniedAuditEntry", base(), id(), oauthApp(), org()),
		s.auditShellObject("OrgOauthAppAccessRequestedAuditEntry", base(), id(), oauthApp(), org()),
		s.auditShellObject("OrgOauthAppAccessUnblockedAuditEntry", base(), id(), oauthApp(), org()),
		s.auditShellObject("OrgRemoveBillingManagerAuditEntry", base(), id(), org(), graphql.Fields{
			"reason": gqlField(orgRemoveBillingManagerReason),
		}),
		s.auditShellObject("OrgRemoveMemberAuditEntry", base(), id(), org(), graphql.Fields{
			"membershipTypes": gqlFieldListOf(orgRemoveMemberMembershipType),
			"reason":          gqlField(orgRemoveMemberReason),
		}),
		s.auditShellObject("OrgRestoreMemberAuditEntry", base(), id(), org(), graphql.Fields{
			"restoredCustomEmailRoutingsCount": gqlField(graphql.Int),
			"restoredIssueAssignmentsCount":    gqlField(graphql.Int),
			"restoredMemberships":              gqlFieldListOf(restoreMembership),
			"restoredMembershipsCount":         gqlField(graphql.Int),
			"restoredRepositoriesCount":        gqlField(graphql.Int),
			"restoredRepositoryStarsCount":     gqlField(graphql.Int),
			"restoredRepositoryWatchesCount":   gqlField(graphql.Int),
		}),
		s.auditShellObject("OrgUpdateDefaultRepositoryPermissionAuditEntry", base(), id(), org(), graphql.Fields{
			"permission":    gqlField(orgUpdateDefaultRepoPermission),
			"permissionWas": gqlField(orgUpdateDefaultRepoPermission),
		}),
		s.auditShellObject("OrgUpdateMemberAuditEntry", base(), id(), org(), graphql.Fields{
			"permission":    gqlField(orgUpdateMemberPermission),
			"permissionWas": gqlField(orgUpdateMemberPermission),
		}),
		s.auditShellObject("OrgUpdateMemberRepositoryCreationPermissionAuditEntry", base(), id(), org(), graphql.Fields{
			"canCreateRepositories": gqlField(graphql.Boolean),
			"visibility":            gqlField(orgUpdateMemberRepoCreationVisibility),
		}),
		s.auditShellObject("OrgUpdateMemberRepositoryInvitationPermissionAuditEntry", base(), id(), org(), graphql.Fields{
			"canInviteOutsideCollaboratorsToRepositories": gqlField(graphql.Boolean),
		}),

		// PrivateRepositoryForking* : + EnterpriseAuditEntryData + RepositoryAuditEntryData
		s.auditShellObject("PrivateRepositoryForkingDisableAuditEntry", base(), id(), ent(), org(), repo()),
		s.auditShellObject("PrivateRepositoryForkingEnableAuditEntry", base(), id(), ent(), org(), repo()),

		// Repo* repo-scoped entries
		s.auditShellObject("RepoAccessAuditEntry", base(), id(), org(), repo(), graphql.Fields{
			"visibility": gqlField(repoAccessVisibility),
		}),
		s.auditShellObject("RepoAddMemberAuditEntry", base(), id(), org(), repo(), graphql.Fields{
			"visibility": gqlField(repoAddMemberVisibility),
		}),
		s.auditShellObject("RepoAddTopicAuditEntry", base(), id(), org(), repo(), topic()),
		s.auditShellObject("RepoArchivedAuditEntry", base(), id(), org(), repo(), graphql.Fields{
			"visibility": gqlField(repoArchivedVisibility),
		}),
		s.auditShellObject("RepoChangeMergeSettingAuditEntry", base(), id(), org(), repo(), graphql.Fields{
			"isEnabled": gqlField(graphql.Boolean),
			"mergeType": gqlField(repoChangeMergeSettingMergeType),
		}),
		s.auditShellObject("RepoConfigDisableAnonymousGitAccessAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigDisableCollaboratorsOnlyAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigDisableContributorsOnlyAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigDisableSockpuppetDisallowedAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigEnableAnonymousGitAccessAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigEnableCollaboratorsOnlyAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigEnableContributorsOnlyAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigEnableSockpuppetDisallowedAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigLockAnonymousGitAccessAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoConfigUnlockAnonymousGitAccessAuditEntry", base(), id(), org(), repo()),
		s.auditShellObject("RepoRemoveMemberAuditEntry", base(), id(), org(), repo(), graphql.Fields{
			"visibility": gqlField(repoRemoveMemberVisibility),
		}),
		s.auditShellObject("RepoRemoveTopicAuditEntry", base(), id(), org(), repo(), topic()),

		// RepositoryVisibilityChange* : + EnterpriseAuditEntryData (org-scoped, no repo mixin)
		s.auditShellObject("RepositoryVisibilityChangeDisableAuditEntry", base(), id(), ent(), org()),
		s.auditShellObject("RepositoryVisibilityChangeEnableAuditEntry", base(), id(), ent(), org()),

		// Team* : + TeamAuditEntryData
		s.auditShellObject("TeamAddMemberAuditEntry", base(), id(), org(), team(), graphql.Fields{
			"isLdapMapped": gqlField(graphql.Boolean),
		}),
		s.auditShellObject("TeamAddRepositoryAuditEntry", base(), id(), org(), repo(), team(), graphql.Fields{
			"isLdapMapped": gqlField(graphql.Boolean),
		}),
		s.auditShellObject("TeamChangeParentTeamAuditEntry", base(), id(), org(), team(), graphql.Fields{
			"isLdapMapped":              gqlField(graphql.Boolean),
			"parentTeam":                gqlField(s.graphqlTypes.team),
			"parentTeamName":            gqlField(graphql.String),
			"parentTeamNameWas":         gqlField(graphql.String),
			"parentTeamResourcePath":    gqlField(uri),
			"parentTeamUrl":             gqlField(uri),
			"parentTeamWas":             gqlField(s.graphqlTypes.team),
			"parentTeamWasResourcePath": gqlField(uri),
			"parentTeamWasUrl":          gqlField(uri),
		}),
		s.auditShellObject("TeamRemoveMemberAuditEntry", base(), id(), org(), team(), graphql.Fields{
			"isLdapMapped": gqlField(graphql.Boolean),
		}),
		s.auditShellObject("TeamRemoveRepositoryAuditEntry", base(), id(), org(), repo(), team(), graphql.Fields{
			"isLdapMapped": gqlField(graphql.Boolean),
		}),
	}
	for _, obj := range objects {
		s.registerExtraSchemaType(obj)
	}
}
