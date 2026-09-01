package graphqlapi

// The authz policy for the mutation surface assembled in gh_mutations_*_graphql.go,
// merged into graphqlMutationAuthz at init. A mutation registered without a row
// fails at schema build.

import (
	"fmt"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// githubMutationAuthzRows is the policy for every mutation the extended surface
// registers, following GitHub's own gates. Repo administration (settings,
// topics, archival) is Administration at admin standing. Label CRUD is repo
// triage: Issues at push standing, no author exemption.
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

		// Account activity changes the viewer's own account, so the entitlement is
		// the credential's Metadata grant over it, at write.
		"followUser":             viewerAccountRule{scope: store.ScopeMetadata},
		"unfollowUser":           viewerAccountRule{scope: store.ScopeMetadata},
		"followOrganization":     viewerAccountRule{scope: store.ScopeMetadata},
		"unfollowOrganization":   viewerAccountRule{scope: store.ScopeMetadata},
		"changeUserStatus":       viewerAccountRule{scope: store.ScopeMetadata},
		"createUserList":         viewerAccountRule{scope: store.ScopeMetadata},
		"updateUserListsForItem": viewerAccountRule{scope: store.ScopeMetadata},
		// These additionally require the list to be the viewer's own.
		"updateUserList": userListRule{idKey: "listId"},
		"deleteUserList": userListRule{idKey: "listId"},

		"updateNotificationRestrictionSetting": notificationRestrictionRule{},

		// Issue comments: Issues scope however the subject was opened. Editing or
		// deleting admits the author; pinning does not (thread curation).
		"updateIssueComment": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssueComment("id")},
		"deleteIssueComment": repoRule{scope: store.ScopeIssues, level: mutationPushRepo, authorMayAct: true, target: mutationTargetIssueComment("id")},
		"pinIssueComment":    repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssueComment("issueCommentId")},
		"unpinIssueComment":  repoRule{scope: store.ScopeIssues, level: mutationPushRepo, target: mutationTargetIssueComment("issueCommentId")},

		// Assignment is triage: Issues at push, no author exemption. The subject
		// may be an issue or a pull request (one /issues/{n}/assignees surface).
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

		// An issue's type is repository triage; the type definitions belong to
		// the organization, so their CRUD authorizes over that org.
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

		// Pull requests. A review comment is participation: read only. Editing or
		// deleting is the author's content or a moderator's push.
		"addPullRequestReviewComment":     repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: reviewSubjectMutationTarget()},
		"addPullRequestReviewThread":      repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: reviewSubjectMutationTarget()},
		"addPullRequestReviewThreadReply": repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: mutationTargetReviewThread("pullRequestReviewThreadId")},
		"updatePullRequestReviewComment":  repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReviewComment("pullRequestReviewCommentId")},
		"deletePullRequestReviewComment":  repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReviewComment("id")},
		"updatePullRequestReview":         repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReview("pullRequestReviewId")},
		"deletePullRequestReview":         repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetReview("pullRequestReviewId")},

		// Requesting review, moving the head branch, archiving, queueing: all
		// PR maintenance, push.
		"requestReviews":          repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		"requestReviewsByLogin":   repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		"updatePullRequestBranch": repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, authorMayAct: true, target: mutationTargetPullRequest("pullRequestId")},
		"archivePullRequest":      repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		"unarchivePullRequest":    repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		// Queueing is queueing a merge, so it demands what mergePullRequest does.
		"enqueuePullRequest": repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("pullRequestId")},
		"dequeuePullRequest": repoRule{scope: store.ScopePullRequests, level: mutationPushRepo, target: mutationTargetPullRequest("id")},

		// Marking a file viewed is a note on the viewer's own review pass: read.
		"markFileAsViewed":   repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: mutationTargetPullRequest("pullRequestId")},
		"unmarkFileAsViewed": repoRule{scope: store.ScopePullRequests, level: mutationReadRepo, target: mutationTargetPullRequest("pullRequestId")},

		// The creation cap and its bypass list are repository administration.
		"addPullRequestCreationCapBypassUsers":    repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"removePullRequestCreationCapBypassUsers": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},

		// A team's review-assignment settings belong to its organization.
		"updateTeamReviewAssignment": teamOwnerRule{idKey: "id"},

		// Starring and watching record the viewer's relationship to a repo, so
		// read is the whole standing. The Metadata credential grant still applies;
		// an unreadable private repo answers as absent, not forbidden.
		"addStar":            repoRule{scope: store.ScopeMetadata, level: mutationReadRepo, target: mutationTargetRepo("starrableId")},
		"removeStar":         repoRule{scope: store.ScopeMetadata, level: mutationReadRepo, target: mutationTargetRepo("starrableId")},
		"updateSubscription": repoRule{scope: store.ScopeMetadata, level: mutationReadRepo, target: mutationTargetRepo("subscribableId")},

		// Interaction limits gate on repository admin, org admin, and the
		// account's own credential respectively.
		"setRepositoryInteractionLimit":   repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"setOrganizationInteractionLimit": orgOwnerRule{idKey: "organizationId", scope: store.ScopeOrgAdministration},
		"setUserInteractionLimit":         viewerUserRule{idKey: "userId"},

		// Removing an outside collaborator needs the owner's Members grant; the two
		// org settings are org administration.
		"removeOutsideCollaborator":                              orgOwnerRule{idKey: "organizationId", scope: store.ScopeMembers},
		"updateOrganizationAllowPrivateRepositoryForkingSetting": orgOwnerRule{idKey: "organizationId", scope: store.ScopeOrgAdministration},
		"updateOrganizationWebCommitSignoffSetting":              orgOwnerRule{idKey: "organizationId", scope: store.ScopeOrgAdministration},
	}
}

// viewerUserRule is the policy for a mutation whose subject must be the
// viewer's own account; naming another account is refused.
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
// input, which names the pull request directly or through the pending review.
func reviewSubjectMutationTarget() func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		// Mirror resolveAddPullRequestReviewComment's precedence EXACTLY: it
		// replies to inReplyTo's parent — posting onto that comment's repo —
		// before it ever consults pullRequestId. If authz resolved pullRequestId
		// first, a caller could pass an accessible pullRequestId to clear the gate
		// while the write lands on the inReplyTo comment's (possibly private)
		// repo. The key that decides the actual write must be the key authz gates.
		if nodeID, _ := input["inReplyTo"].(string); nodeID != "" {
			return mutationTargetReviewComment("inReplyTo")(s, input)
		}
		if nodeID, _ := input["pullRequestId"].(string); nodeID != "" {
			return mutationTargetPullRequest("pullRequestId")(s, input)
		}
		return mutationTargetReview("pullRequestReviewId")(s, input)
	}
}

// mutationTargetReviewComment resolves the repo a pull-request review comment
// belongs to, and the comment's author for the author exemption.
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

// orgOwnerRule is the policy for a mutation whose subject is an org definition:
// the viewer must administer the org and the credential must carry the grant
// over it. Owning one org never authorizes a write against another.
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

// authorizeOrgAdministration is the two-part org entitlement every org-scoped
// rule asks: the viewer administers the account, and the credential was granted
// the scope over it.
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

// mutationTargetLabel resolves the repo a label belongs to. A label carries no
// author, so there is no author exemption to derive from it.
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
