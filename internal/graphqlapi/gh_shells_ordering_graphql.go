package graphqlapi

// Schema-fidelity shells: the ordering-input cluster (the `*Order` inputs,
// their `*OrderField` enums, a few standalone enums, and CheckRunFilter /
// ProjectV2Filters). The fields they order aren't exposed, so no resolver
// returns them; they exist only so the introspected schema matches GitHub's.

import "github.com/graphql-go/graphql"

func init() {
	schemaShellBuilders = append(schemaShellBuilders, (*Resolver).addOrderingShells)
}

func (s *Resolver) addOrderingShells() {
	// standalone enums
	checkRunType := s.sharedEnum("CheckRunType", "ALL", "LATEST")
	discussionState := s.sharedEnum("DiscussionState", "CLOSED", "OPEN")
	environmentPinnedFilterField := s.sharedEnum("EnvironmentPinnedFilterField", "ALL", "NONE", "ONLY")
	projectV2PermissionLevel := s.sharedEnum("ProjectV2PermissionLevel", "ADMIN", "READ", "WRITE")
	projectV2State := s.sharedEnum("ProjectV2State", "CLOSED", "OPEN")
	userBlockDuration := s.sharedEnum("UserBlockDuration", "ONE_DAY", "ONE_MONTH", "ONE_WEEK", "PERMANENT", "THREE_DAYS")

	s.registerExtraSchemaType(
		checkRunType,
		discussionState,
		environmentPinnedFilterField,
		projectV2PermissionLevel,
		projectV2State,
		userBlockDuration,
	)

	// *OrderField enums
	orderDirection := s.sharedEnum("OrderDirection", "ASC", "DESC")

	enterpriseMemberOrderField := s.sharedEnum("EnterpriseMemberOrderField", "CREATED_AT", "LOGIN")
	enterpriseOrderField := s.sharedEnum("EnterpriseOrderField", "NAME")
	enterpriseServerUserAccountEmailOrderField := s.sharedEnum("EnterpriseServerUserAccountEmailOrderField", "EMAIL")
	issueDependencyOrderField := s.sharedEnum("IssueDependencyOrderField", "CREATED_AT", "DEPENDENCY_ADDED_AT")
	languageOrderField := s.sharedEnum("LanguageOrderField", "SIZE")
	mannequinOrderField := s.sharedEnum("MannequinOrderField", "CREATED_AT", "LOGIN")
	milestoneOrderField := s.sharedEnum("MilestoneOrderField", "CREATED_AT", "DUE_DATE", "NUMBER", "UPDATED_AT")
	orgEnterpriseOwnerOrderField := s.sharedEnum("OrgEnterpriseOwnerOrderField", "LOGIN")
	organizationOrderField := s.sharedEnum("OrganizationOrderField", "CREATED_AT", "LOGIN")
	packageFileOrderField := s.sharedEnum("PackageFileOrderField", "CREATED_AT")
	packageOrderField := s.sharedEnum("PackageOrderField", "CREATED_AT")
	packageVersionOrderField := s.sharedEnum("PackageVersionOrderField", "CREATED_AT")
	pinnedEnvironmentOrderField := s.sharedEnum("PinnedEnvironmentOrderField", "POSITION")
	projectOrderField := s.sharedEnum("ProjectOrderField", "CREATED_AT", "NAME", "UPDATED_AT")
	reactionOrderField := s.sharedEnum("ReactionOrderField", "CREATED_AT")
	repositoryRuleOrderField := s.sharedEnum("RepositoryRuleOrderField", "CREATED_AT", "TYPE", "UPDATED_AT")
	savedReplyOrderField := s.sharedEnum("SavedReplyOrderField", "UPDATED_AT")
	teamMemberOrderField := s.sharedEnum("TeamMemberOrderField", "CREATED_AT", "LOGIN")
	workflowRunOrderField := s.sharedEnum("WorkflowRunOrderField", "CREATED_AT")

	s.registerExtraSchemaType(
		enterpriseMemberOrderField,
		enterpriseOrderField,
		enterpriseServerUserAccountEmailOrderField,
		issueDependencyOrderField,
		languageOrderField,
		mannequinOrderField,
		milestoneOrderField,
		orgEnterpriseOwnerOrderField,
		organizationOrderField,
		packageFileOrderField,
		packageOrderField,
		packageVersionOrderField,
		pinnedEnvironmentOrderField,
		projectOrderField,
		reactionOrderField,
		repositoryRuleOrderField,
		savedReplyOrderField,
		teamMemberOrderField,
		workflowRunOrderField,
	)

	// regular `{ direction: OrderDirection!, field: <X>OrderField! }`
	enterpriseMemberOrder := s.mutationInput("EnterpriseMemberOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(enterpriseMemberOrderField),
	})
	enterpriseOrder := s.mutationInput("EnterpriseOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(enterpriseOrderField),
	})
	enterpriseServerUserAccountEmailOrder := s.mutationInput("EnterpriseServerUserAccountEmailOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(enterpriseServerUserAccountEmailOrderField),
	})
	issueDependencyOrder := s.mutationInput("IssueDependencyOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(issueDependencyOrderField),
	})
	languageOrder := s.mutationInput("LanguageOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(languageOrderField),
	})
	mannequinOrder := s.mutationInput("MannequinOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(mannequinOrderField),
	})
	milestoneOrder := s.mutationInput("MilestoneOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(milestoneOrderField),
	})
	orgEnterpriseOwnerOrder := s.mutationInput("OrgEnterpriseOwnerOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(orgEnterpriseOwnerOrderField),
	})
	organizationOrder := s.mutationInput("OrganizationOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(organizationOrderField),
	})
	pinnedEnvironmentOrder := s.mutationInput("PinnedEnvironmentOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(pinnedEnvironmentOrderField),
	})
	projectOrder := s.mutationInput("ProjectOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(projectOrderField),
	})
	reactionOrder := s.mutationInput("ReactionOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(reactionOrderField),
	})
	repositoryRuleOrder := s.mutationInput("RepositoryRuleOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(repositoryRuleOrderField),
	})
	savedReplyOrder := s.mutationInput("SavedReplyOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(savedReplyOrderField),
	})
	teamMemberOrder := s.mutationInput("TeamMemberOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(teamMemberOrderField),
	})
	workflowRunOrder := s.mutationInput("WorkflowRunOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(orderDirection),
		"field":     gqlNonNullInputOf(workflowRunOrderField),
	})

	// Package* orders: both fields NULLABLE (`OrderDirection`, `*OrderField`)
	packageFileOrder := s.mutationInput("PackageFileOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlInputOf(orderDirection),
		"field":     gqlInputOf(packageFileOrderField),
	})
	packageOrder := s.mutationInput("PackageOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlInputOf(orderDirection),
		"field":     gqlInputOf(packageOrderField),
	})
	packageVersionOrder := s.mutationInput("PackageVersionOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlInputOf(orderDirection),
		"field":     gqlInputOf(packageVersionOrderField),
	})

	// irregular filter inputs
	checkConclusionState := s.sharedEnum("CheckConclusionState",
		"ACTION_REQUIRED", "CANCELLED", "FAILURE", "NEUTRAL", "SKIPPED", "STALE",
		"STARTUP_FAILURE", "SUCCESS", "TIMED_OUT")
	checkStatusState := s.sharedEnum("CheckStatusState",
		"COMPLETED", "IN_PROGRESS", "PENDING", "QUEUED", "REQUESTED", "WAITING")
	checkRunFilter := s.mutationInput("CheckRunFilter", graphql.InputObjectConfigFieldMap{
		"appId":       gqlInt(),
		"checkName":   gqlInputOf(graphql.String),
		"checkType":   gqlInputOf(checkRunType),
		"conclusions": gqlListOf(checkConclusionState),
		"status":      gqlInputOf(checkStatusState),
		"statuses":    gqlListOf(checkStatusState),
	})

	projectV2Filters := s.mutationInput("ProjectV2Filters", graphql.InputObjectConfigFieldMap{
		"state": gqlInputOf(projectV2State),
	})

	s.registerExtraSchemaType(
		enterpriseMemberOrder,
		enterpriseOrder,
		enterpriseServerUserAccountEmailOrder,
		issueDependencyOrder,
		languageOrder,
		mannequinOrder,
		milestoneOrder,
		orgEnterpriseOwnerOrder,
		organizationOrder,
		packageFileOrder,
		packageOrder,
		packageVersionOrder,
		pinnedEnvironmentOrder,
		projectOrder,
		reactionOrder,
		repositoryRuleOrder,
		savedReplyOrder,
		teamMemberOrder,
		workflowRunOrder,
		checkRunFilter,
		projectV2Filters,
	)
}
