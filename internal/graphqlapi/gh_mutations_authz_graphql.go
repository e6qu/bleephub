package graphqlapi

// The authorization policy for the mutation surface assembled in
// gh_mutations_*_graphql.go.
//
// It is one table, merged into graphqlMutationAuthz at package init, so the
// coverage gate that asserts schema fields and policy rows are the same set
// sees these rows exactly as it sees the rest. A mutation registered here
// without a row still fails at schema build.

import (
	"fmt"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// githubMutationAuthzRows is the policy for every mutation the extended
// surface registers.
//
// The reasoning follows GitHub's own gates. Repository administration —
// settings, topics, archival — is Administration at write and admin standing.
// Label CRUD is repository triage, which GitHub gates on Issues at write and
// push standing; unlike editing your own issue there is no author exemption,
// because a label is the repository's vocabulary rather than anyone's content.
func githubMutationAuthzRows() map[string]mutationRule {
	return map[string]mutationRule{
		"createLabel": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		"updateLabel": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetLabel("id")},
		"deleteLabel": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetLabel("id")},

		"updateTopics":           repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"acceptTopicSuggestion":  repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"declineTopicSuggestion": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},

		"archiveRepository":                       repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"unarchiveRepository":                     repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"updateRepository":                        repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"updateRepositoryWebCommitSignoffSetting": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},

		// Account activity. Following, the profile status and the stars-page
		// lists all change the viewer's own account, so the entitlement is the
		// credential's grant over that account rather than standing on any
		// other record. GitHub groups all three under the account's Metadata
		// permission at write, which is the grant a fine-grained token
		// belonging to somebody else does not hold.
		"followUser":             viewerAccountRule{scope: store.ScopeMetadata},
		"unfollowUser":           viewerAccountRule{scope: store.ScopeMetadata},
		"followOrganization":     viewerAccountRule{scope: store.ScopeMetadata},
		"unfollowOrganization":   viewerAccountRule{scope: store.ScopeMetadata},
		"changeUserStatus":       viewerAccountRule{scope: store.ScopeMetadata},
		"createUserList":         viewerAccountRule{scope: store.ScopeMetadata},
		"updateUserListsForItem": viewerAccountRule{scope: store.ScopeMetadata},
		// The two that name an existing list additionally require it to be the
		// viewer's own, which is the cross-tenant half.
		"updateUserList": userListRule{idKey: "listId"},
		"deleteUserList": userListRule{idKey: "listId"},

		"updateNotificationRestrictionSetting": notificationRestrictionRule{},

		// Issue comments. GitHub gates the issue-comment endpoints on Issues
		// however the subject was opened, and admits the comment's own author
		// whatever their repository access — editing and deleting your own
		// comment never required push. Pinning is curation of the thread
		// rather than of your own content, so it carries no author exemption.
		"updateIssueComment": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssueComment("id")},
		"deleteIssueComment": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssueComment("id")},
		"pinIssueComment":    repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssueComment("issueCommentId")},
		"unpinIssueComment":  repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssueComment("issueCommentId")},

		// Assignment is triage: push on the repository, with no author
		// exemption, exactly as updateIssue treats its assigneeIds argument.
		// The subject may be an issue or a pull request, and GitHub serves both
		// through the one /issues/{n}/assignees surface it gates on Issues.
		"addAssigneesToAssignable":      repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: assignableMutationTarget("assignableId")},
		"removeAssigneesFromAssignable": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: assignableMutationTarget("assignableId")},
		"replaceActorsForAssignable":    repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: assignableMutationTarget("assignableId")},

		// Sub-issues, dependencies and the duplicate relation are triage of
		// the parent issue's repository.
		"addSubIssue":            repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"removeSubIssue":         repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"reprioritizeSubIssue":   repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"addBlockedBy":           repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"removeBlockedBy":        repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"unmarkIssueAsDuplicate": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("duplicateId")},

		// The issue type an issue carries is the repository's triage state;
		// the type definitions themselves belong to the organization, so their
		// CRUD authorizes over that account rather than over any repository.
		"updateIssueIssueType": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"createIssueType":      orgOwnerRule{idKey: "ownerId", scope: store.ScopeOrgAdministration},
		"updateIssueType":      issueTypeRule{idKey: "issueTypeId"},
		"deleteIssueType":      issueTypeRule{idKey: "issueTypeId"},

		// Custom issue fields are likewise organization definitions; their
		// values are per-issue triage.
		"createIssueField":      orgOwnerRule{idKey: "ownerId", scope: store.ScopeOrgAdministration},
		"updateIssueField":      issueFieldRule{idKey: "id"},
		"deleteIssueField":      issueFieldRule{idKey: "fieldId"},
		"createIssueFieldValue": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"updateIssueFieldValue": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"setIssueFieldValue":    repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"deleteIssueFieldValue": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},

		// Performing or discarding an agent's proposed triage is triage.
		"applyPendingIssueSuggestions":  repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},
		"rejectPendingIssueSuggestions": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssue("issueId")},

		// Pull requests. Leaving a review comment is how an outside
		// contributor participates, so it needs only read — the same standing
		// addPullRequestReview already carries. Editing or deleting one is the
		// author's own content or a moderator's push.
		"addPullRequestReviewComment":     repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: reviewSubjectMutationTarget()},
		"addPullRequestReviewThread":      repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: reviewSubjectMutationTarget()},
		"addPullRequestReviewThreadReply": repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: mutationTargetReviewThread("pullRequestReviewThreadId")},
		"updatePullRequestReviewComment":  repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReviewComment("pullRequestReviewCommentId")},
		"deletePullRequestReviewComment":  repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReviewComment("id")},
		"updatePullRequestReview":         repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReview("pullRequestReviewId")},
		"deletePullRequestReview":         repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReview("pullRequestReviewId")},

		// Asking somebody to review, moving the head branch, archiving and
		// queueing are all maintenance of the pull request: push.
		"requestReviews":          repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		"requestReviewsByLogin":   repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		"updatePullRequestBranch": repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
		"archivePullRequest":      repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		"unarchivePullRequest":    repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		// Queueing a pull request is queueing a merge of the base branch, so
		// it demands what mergePullRequest demands.
		"enqueuePullRequest": repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		"dequeuePullRequest": repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("id")},

		// Marking a file viewed is a note about the viewer's own review pass,
		// so anybody who can read the pull request may make one.
		"markFileAsViewed":   repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: mutationTargetPullRequest("pullRequestId")},
		"unmarkFileAsViewed": repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: mutationTargetPullRequest("pullRequestId")},

		// The creation cap and its bypass list are repository administration,
		// which is the entitlement the REST bypass-list routes demand.
		"addPullRequestCreationCapBypassUsers":    repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"removePullRequestCreationCapBypassUsers": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},

		// A team's review-assignment settings belong to its organization.
		"updateTeamReviewAssignment": teamOwnerRule{idKey: "id"},

		// Activity. Starring and watching record the viewer's own relationship
		// to a repository rather than changing it, so reading the repository is
		// the whole standing — which is how an outside contributor stars a
		// public project. The credential half still applies: a fine-grained
		// token without the account's Metadata grant may not star on its
		// bearer's behalf, and a private repository the bearer cannot read is
		// answered as absent rather than as forbidden.
		"addStar":            repoRule{scope: store.ScopeMetadata, level: mutationReadRepo, target: mutationTargetRepo("starrableId")},
		"removeStar":         repoRule{scope: store.ScopeMetadata, level: mutationReadRepo, target: mutationTargetRepo("starrableId")},
		"updateSubscription": repoRule{scope: store.ScopeMetadata, level: mutationReadRepo, target: mutationTargetRepo("subscribableId")},

		// Interaction limits are the moderation setting GitHub gates on
		// repository administration, organization administration and the
		// account's own credential respectively — exactly the three
		// interaction-limits REST routes.
		"setRepositoryInteractionLimit":   repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"setOrganizationInteractionLimit": orgOwnerRule{idKey: "organizationId", scope: store.ScopeOrgAdministration},
		"setUserInteractionLimit":         viewerUserRule{idKey: "userId"},

		// Removing an outside collaborator strips their access to every
		// repository in the organization, which is the Members grant an owner
		// holds; the two organization settings are organization
		// administration.
		"removeOutsideCollaborator":                              orgOwnerRule{idKey: "organizationId", scope: store.ScopeMembers},
		"updateOrganizationAllowPrivateRepositoryForkingSetting": orgOwnerRule{idKey: "organizationId", scope: store.ScopeOrgAdministration},
		"updateOrganizationWebCommitSignoffSetting":              orgOwnerRule{idKey: "organizationId", scope: store.ScopeOrgAdministration},
	}
}

// viewerUserRule is the policy for a mutation whose subject must be the
// viewer's own account: PUT /user/interaction-limits has no form that names
// somebody else, so naming another account is refused rather than served.
type viewerUserRule struct {
	idKey string
}

func (r viewerUserRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no user id input key")
	}
	return nil
}

func (r viewerUserRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.idKey].(string)
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil || nodeID != viewer.NodeID {
		return &ghForbiddenError{message: "You may only set an interaction limit on your own account."}
	}
	if !s.credentialGrantsAccount(p.Context, store.AnyAccount, viewer.Login, store.ScopeAdministration, store.PermWrite) {
		return &ghForbiddenError{message: "resource not accessible by integration"}
	}
	return nil
}

// reviewSubjectMutationTarget resolves the repository behind a review-comment
// input, which names the pull request either directly or through the pending
// review the comment joins.
func reviewSubjectMutationTarget() func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		if nodeID, _ := input["pullRequestId"].(string); nodeID != "" {
			return mutationTargetPullRequest("pullRequestId")(s, input)
		}
		if nodeID, _ := input["inReplyTo"].(string); nodeID != "" {
			return mutationTargetReviewComment("inReplyTo")(s, input)
		}
		return mutationTargetReview("pullRequestReviewId")(s, input)
	}
}

// mutationTargetReviewComment resolves the repository a pull-request review
// comment belongs to, and the comment's author for the author exemption.
func mutationTargetReviewComment(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("PullRequestReviewComment", nodeID)}
		comment := store.FindPullRequestReviewCommentByNodeID(s.store, nodeID)
		if comment == nil {
			return target
		}
		target.authorID = comment.AuthorID
		if pr := s.store.GetPullRequest(comment.PullRequestID); pr != nil {
			target.repo = s.store.GetRepoByID(pr.RepoID)
		}
		return target
	}
}

// teamOwnerRule is orgOwnerRule for a mutation that names a team: the
// entitlement is over the organization the team belongs to.
type teamOwnerRule struct {
	idKey string
}

func (r teamOwnerRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no team id input key")
	}
	return nil
}

func (r teamOwnerRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.idKey].(string)
	team := s.teamByNodeID(nodeID)
	if team == nil {
		return gqlMissingNode("Team", nodeID)
	}
	org := s.store.GetOrgByID(team.OrgID)
	if org == nil {
		return gqlMissingNode("Team", nodeID)
	}
	return s.authorizeOrgAdministration(p, org.Login, store.ScopeOrgAdministration)
}

// assignableMutationTarget resolves the repository behind an Assignable node
// id, which may name an issue or a pull request.
func assignableMutationTarget(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Assignable", nodeID)}
		if issue := store.FindIssueByNodeID(s.store, nodeID); issue != nil {
			target.repo = s.store.GetRepoByID(issue.RepoID)
			return target
		}
		if pr := store.FindPullRequestByNodeID(s.store, nodeID); pr != nil {
			target.repo = s.store.GetRepoByID(pr.RepoID)
		}
		return target
	}
}

// orgOwnerRule is the policy for a mutation whose subject is an organization
// definition rather than a repository record: the viewer must be able to
// administer that organization, and the credential must carry the grant over
// it. Owning one organization therefore never authorizes the write against
// another.
type orgOwnerRule struct {
	idKey string
	scope store.PermScope
}

func (r orgOwnerRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no organization id input key")
	}
	if r.scope == "" {
		return fmt.Errorf("no permission scope")
	}
	return nil
}

func (r orgOwnerRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.idKey].(string)
	org := s.orgByNodeID(nodeID)
	if org == nil {
		return gqlMissingNode("Organization", nodeID)
	}
	return s.authorizeOrgAdministration(p, org.Login, r.scope)
}

// authorizeOrgAdministration is the two-part organization entitlement every
// org-scoped rule asks: the viewer administers the account, and the credential
// was granted the scope over it.
func (s *Resolver) authorizeOrgAdministration(p graphql.ResolveParams, login string, scope store.PermScope) error {
	if !s.viewerCanAdminAccount(p.Context, login) {
		return &ghForbiddenError{message: "You must be an owner of the organization to perform this action."}
	}
	if !s.credentialGrantsAccount(p.Context, store.OrganizationAccount, login, scope, store.PermWrite) {
		return &ghForbiddenError{message: "resource not accessible by integration"}
	}
	return nil
}

// issueTypeRule is orgOwnerRule for the two mutations that name an existing
// issue type: the organization is the one that defined it.
type issueTypeRule struct {
	idKey string
}

func (r issueTypeRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no issue type id input key")
	}
	return nil
}

func (r issueTypeRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.idKey].(string)
	issueType := store.FindIssueTypeByNodeID(s.store, nodeID)
	if issueType == nil {
		return gqlMissingNode("IssueType", nodeID)
	}
	return s.authorizeOrgAdministration(p, issueType.OrgLogin, store.ScopeOrgAdministration)
}

// issueFieldRule is issueTypeRule for the custom issue-field definitions.
type issueFieldRule struct {
	idKey string
}

func (r issueFieldRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no issue field id input key")
	}
	return nil
}

func (r issueFieldRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input[r.idKey].(string)
	org, field := s.issueFieldByNodeID(nodeID)
	if field == nil {
		return gqlMissingNode("IssueFields", nodeID)
	}
	return s.authorizeOrgAdministration(p, org, store.ScopeOrgAdministration)
}

func init() {
	for name, rule := range githubMutationAuthzRows() {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic("graphql mutation " + name + " already has an authorization row")
		}
		graphqlMutationAuthz[name] = rule
	}
}

// mutationTargetLabel resolves the repository a label belongs to. A label
// carries no author, so there is no author exemption to derive from it.
func mutationTargetLabel(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Label", nodeID)}
		if label := store.FindLabelByNodeID(s.store, nodeID); label != nil {
			target.repo = s.store.GetRepoByID(label.RepoID)
		}
		return target
	}
}
