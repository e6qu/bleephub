package graphqlapi

// Data-layer aliases, mirroring internal/actions' store_aliases.go pattern
// (ARCH-002): the moved resolver code keeps its original spellings while
// the types themselves live in internal/store. Only names the resolver
// layer actually uses appear here.

import "github.com/e6qu/bleephub/internal/store"

type (
	CheckRun                        = store.CheckRun
	CheckSuite                      = store.CheckSuite
	Comment                         = store.Comment
	Discussion                      = store.Discussion
	DiscussionCategory              = store.DiscussionCategory
	DiscussionComment               = store.DiscussionComment
	Issue                           = store.Issue
	IssueField                      = store.IssueField
	IssueFieldOption                = store.IssueFieldOption
	IssueLabel                      = store.IssueLabel
	IssueType                       = store.IssueType
	Label                           = store.Label
	Milestone                       = store.Milestone
	MilestoneState                  = store.MilestoneState
	Org                             = store.Org
	PRReviewComment                 = store.PRReviewComment
	ProjectV2                       = store.ProjectV2
	ProjectV2Field                  = store.ProjectV2Field
	ProjectV2FieldDataType          = store.ProjectV2FieldDataType
	ProjectV2Item                   = store.ProjectV2Item
	ProjectV2ItemFieldValue         = store.ProjectV2ItemFieldValue
	ProjectV2Iteration              = store.ProjectV2Iteration
	ProjectV2IterationConfiguration = store.ProjectV2IterationConfiguration
	ProjectV2SingleSelectOption     = store.ProjectV2SingleSelectOption
	ProjectV2View                   = store.ProjectV2View
	PullRequest                     = store.PullRequest
	PullRequestOptions              = store.PullRequestOptions
	PullRequestReview               = store.PullRequestReview
	Reaction                        = store.Reaction
	ReactionStore                   = store.ReactionStore
	Release                         = store.Release
	Repo                            = store.Repo
	RepoListOptions                 = store.RepoListOptions
	ReviewThread                    = store.ReviewThread
	Store                           = store.Store
	Team                            = store.Team
	User                            = store.User
	Workflow                        = store.Workflow

	accountKind    = store.AccountKind
	permLevel      = store.PermLevel
	permScope      = store.PermScope
	projectV2Owner = store.ProjectV2Owner
)

const (
	MembershipStateActive      = store.MembershipStateActive
	ProjectV2FieldDate         = store.ProjectV2FieldDate
	ProjectV2FieldIteration    = store.ProjectV2FieldIteration
	ProjectV2FieldNumber       = store.ProjectV2FieldNumber
	ProjectV2FieldSingleSelect = store.ProjectV2FieldSingleSelect
	ProjectV2FieldText         = store.ProjectV2FieldText
	TeamPermissionPull         = store.TeamPermissionPull

	anyAccount          = store.AnyAccount
	organizationAccount = store.OrganizationAccount
	permAdmin           = store.PermAdmin
	permWrite           = store.PermWrite
	scopeAdministration = store.ScopeAdministration
	scopeDiscussions    = store.ScopeDiscussions
	scopeIssues         = store.ScopeIssues
	scopePullRequests   = store.ScopePullRequests
)

var (
	ErrOpenPullRequestExists = store.ErrOpenPullRequestExists

	actorUserLocked                     = store.ActorUserLocked
	commentCountKey                     = store.CommentCountKey
	findDiscussionByNodeID              = store.FindDiscussionByNodeID
	findDiscussionCategoryByNodeID      = store.FindDiscussionCategoryByNodeID
	findDiscussionCommentByNodeID       = store.FindDiscussionCommentByNodeID
	findIssueByNodeID                   = store.FindIssueByNodeID
	findIssueTypeByNodeID               = store.FindIssueTypeByNodeID
	findLabelByNodeID                   = store.FindLabelByNodeID
	findMilestoneByNodeID               = store.FindMilestoneByNodeID
	findPullRequestByNodeID             = store.FindPullRequestByNodeID
	findRepoByNodeID                    = store.FindRepoByNodeID
	findUserByNodeID                    = store.FindUserByNodeID
	issueHasAllLabels                   = store.IssueHasAllLabels
	licenseTemplates                    = store.LicenseTemplates
	markdownModeRenderer                = store.MarkdownModeRenderer
	membershipKey                       = store.MembershipKey
	nullableTimestamp                   = store.NullableTimestamp
	parsePRReviewThreadNodeID           = store.ParsePRReviewThreadNodeID
	permissionAtLeast                   = store.PermissionAtLeast
	prReviewThreadNodeID                = store.PRReviewThreadNodeID
	pullRequestCommitObjectsFromStorage = store.PullRequestCommitObjectsFromStorage
	pullRequestGitStorage               = store.PullRequestGitStorage
	pullRequestHeadRepoID               = store.PullRequestHeadRepoID
	pullRequestHeadRepoLocked           = store.PullRequestHeadRepoLocked
	pullRequestHeadSHALocked            = store.PullRequestHeadSHALocked
	repoHasDiscussions                  = store.RepoHasDiscussions
	resolvePullRequestHead              = store.ResolvePullRequestHead
	sshGitURL                           = store.SshGitURL
	toStringSlice                       = store.ToStringSlice
	userToJSON                          = store.UserToJSON
)
