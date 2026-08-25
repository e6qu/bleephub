package graphqlapi

// Schema-fidelity shells: the miscellaneous cluster. GitHub declares a scatter
// of object types this instance never reaches through any resolved field — the
// commit/pull-request comment-thread objects, the relationship hovercard
// contexts, the agent-triage "pending suggestion" objects, a package tag, a
// member-feature-request notification, a pull-request revision marker and the
// two Projects-v2 issue-field value shells. bleephub produces none of the
// underlying data, so no query returns them; they exist only so the
// introspected schema matches GitHub's shape.
//
// Every object is built through the memoized constructor (s.mutationObject) so
// a name any other family already built is reused rather than duplicated, and
// each is published via registerExtraSchemaType. Field types reference the
// existing read-surface types read-only (User/Organization/Repository/Actor/
// PullRequest/Commit and the shared unions/enums), reached through the type
// registry, the memoized accessors, or — for the two connection objects a
// pull-request thread names — graphql.GetNamed navigation from the already-built
// PullRequestReviewThread. Signatures are matched exactly against the vendored
// SDL (third_party/github-graphql-schema.graphql.gz), nullability and arguments
// included. The hovercard-context objects carry message/octicon as GitHub's
// HovercardContext interface declares, built as plain objects (bleephub's
// HovercardContext interface carries no implementors — see sharedHovercardType).

import "github.com/graphql-go/graphql"

func init() {
	schemaShellBuilders = append(schemaShellBuilders, (*Resolver).addMiscShells)
}

func (s *Resolver) addMiscShells() {
	// --- shared scalars and enums ------------------------------------------
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")

	issueStateReason := s.sharedEnum("IssueStateReason", "COMPLETED", "DUPLICATE", "NOT_PLANNED", "REOPENED")
	diffSide := s.sharedEnum("DiffSide", "LEFT", "RIGHT")
	prReviewThreadSubjectType := s.sharedEnum("PullRequestReviewThreadSubjectType", "FILE", "LINE")

	// --- existing read-surface types referenced read-only ------------------
	commit := s.graphqlTypes.commit
	repository := s.graphqlTypes.repository
	pullRequest := s.graphqlTypes.pullRequest

	// PullRequestReviewDecision — built (unmemoized) by the pull-request family;
	// reach the one instance through PullRequest.reviewDecision rather than
	// re-minting the enum (a duplicate name would break schema assembly).
	var prReviewDecision *graphql.Enum
	if pullRequest != nil {
		if def := pullRequest.Fields()["reviewDecision"]; def != nil {
			prReviewDecision, _ = graphql.GetNamed(def.Type).(*graphql.Enum)
		}
	}
	user := s.graphqlTypes.user
	actor := s.graphqlTypes.actor
	assignee := s.graphqlTypes.assignee
	issueType := s.graphqlTypes.issueType
	issueFields := s.graphqlTypes.issueFieldsUnion
	issueOrPullRequest := s.graphqlTypes.issueOrPullRequest
	organizationConnection := s.graphqlTypes.organizationConnection
	projectV2FieldConfiguration := s.graphqlTypes.projectV2FieldConfigUnionMemo
	teamConnection := s.gqlTeamConnectionType()
	packageVersion := s.namedObject("PackageVersion")

	// OrganizationOrder — the ordering-input shell (a sibling builder) also
	// names it; mutationInput memoizes by name, so whichever builder runs first
	// mints it and the other reuses the same instance. The field set matches the
	// ordering cluster's exactly.
	organizationOrder := s.mutationInput("OrganizationOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(s.sharedEnum("OrderDirection", "ASC", "DESC")),
		"field":     gqlNonNullInputOf(s.sharedEnum("OrganizationOrderField", "CREATED_AT", "LOGIN")),
	})

	// CommitCommentConnection — built (unmemoized on the Resolver) by the account
	// surface, which memoizes it privately; reach the one instance through
	// User.commitComments rather than re-minting it (a duplicate name would break
	// schema assembly).
	var commitCommentConnection *graphql.Object
	if user != nil {
		if def := user.Fields()["commitComments"]; def != nil {
			commitCommentConnection, _ = graphql.GetNamed(def.Type).(*graphql.Object)
		}
	}

	// PullRequestReviewCommentConnection — built (unmemoized) deep inside the
	// pull-request family; reach the one instance through the memoized
	// PullRequestReviewThread's comments field rather than re-minting it (a
	// duplicate name would break schema assembly).
	var prReviewCommentConnection *graphql.Object
	if prReviewThread := s.graphqlTypes.pullRequestReviewThread; prReviewThread != nil {
		if def := prReviewThread.Fields()["comments"]; def != nil {
			prReviewCommentConnection, _ = graphql.GetNamed(def.Type).(*graphql.Object)
		}
	}

	// ProjectV2IssueFieldValues — a union over the same five IssueField*Value
	// objects as the existing IssueFieldValue union. Reuse those instances (they
	// are unmemoized locals of the issues family) via IssueFieldValue.Types() so
	// no member type is duplicated.
	var projectV2IssueFieldValues *graphql.Union
	if ifv := s.graphqlTypes.issueFieldValueUnion; ifv != nil {
		members := ifv.Types()
		projectV2IssueFieldValues = graphql.NewUnion(graphql.UnionConfig{
			Name:  "ProjectV2IssueFieldValues",
			Types: members,
			ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
				src, _ := p.Value.(map[string]interface{})
				name, _ := src["__typename"].(string)
				for _, m := range members {
					if m != nil && m.Name() == name {
						return m
					}
				}
				if len(members) > 0 {
					return members[len(members)-1]
				}
				return nil
			},
		})
	}

	// --- comment-thread objects (implements Node & RepositoryNode) ---------
	commitCommentThread := s.mutationObject("CommitCommentThread", graphql.Fields{
		"comments":   &graphql.Field{Type: graphql.NewNonNull(commitCommentConnection), Args: connectionArgs(nil)},
		"commit":     gqlField(commit),
		"id":         gqlNonNull(graphql.ID),
		"path":       gqlField(graphql.String),
		"position":   gqlField(graphql.Int),
		"repository": gqlNonNull(repository),
	})

	pullRequestCommitCommentThread := s.mutationObject("PullRequestCommitCommentThread", graphql.Fields{
		"comments":    &graphql.Field{Type: graphql.NewNonNull(commitCommentConnection), Args: connectionArgs(nil)},
		"commit":      gqlNonNull(commit),
		"id":          gqlNonNull(graphql.ID),
		"path":        gqlField(graphql.String),
		"position":    gqlField(graphql.Int),
		"pullRequest": gqlNonNull(pullRequest),
		"repository":  gqlNonNull(repository),
	})

	// --- hovercard-context objects (HovercardContext: message/octicon) -----
	genericHovercardContext := s.mutationObject("GenericHovercardContext", graphql.Fields{
		"message": gqlNonNull(graphql.String),
		"octicon": gqlNonNull(graphql.String),
	})

	organizationTeamsHovercardContext := s.mutationObject("OrganizationTeamsHovercardContext", graphql.Fields{
		"message":           gqlNonNull(graphql.String),
		"octicon":           gqlNonNull(graphql.String),
		"relevantTeams":     &graphql.Field{Type: graphql.NewNonNull(teamConnection), Args: connectionArgs(nil)},
		"teamsResourcePath": gqlNonNull(uri),
		"teamsUrl":          gqlNonNull(uri),
		"totalTeamCount":    gqlNonNull(graphql.Int),
	})

	organizationsHovercardContext := s.mutationObject("OrganizationsHovercardContext", graphql.Fields{
		"message": gqlNonNull(graphql.String),
		"octicon": gqlNonNull(graphql.String),
		"relevantOrganizations": &graphql.Field{
			Type: graphql.NewNonNull(organizationConnection),
			Args: connectionArgs(graphql.FieldConfigArgument{
				"orderBy": &graphql.ArgumentConfig{Type: organizationOrder},
			}),
		},
		"totalOrganizationCount": gqlNonNull(graphql.Int),
	})

	reviewStatusHovercardContext := s.mutationObject("ReviewStatusHovercardContext", graphql.Fields{
		"message":        gqlNonNull(graphql.String),
		"octicon":        gqlNonNull(graphql.String),
		"reviewDecision": gqlField(prReviewDecision),
	})

	viewerHovercardContext := s.mutationObject("ViewerHovercardContext", graphql.Fields{
		"message": gqlNonNull(graphql.String),
		"octicon": gqlNonNull(graphql.String),
		"viewer":  gqlNonNull(user),
	})

	// --- pending-suggestion objects ----------------------------------------
	pendingAssigneeSuggestion := s.mutationObject("PendingAssigneeSuggestion", graphql.Fields{
		"actor":     gqlField(actor),
		"assignee":  gqlField(assignee),
		"createdAt": gqlNonNull(dateTime),
		"rationale": gqlField(graphql.String),
		"updatedAt": gqlField(dateTime),
	})

	pendingCloseSuggestion := s.mutationObject("PendingCloseSuggestion", graphql.Fields{
		"actor":       gqlField(actor),
		"createdAt":   gqlNonNull(dateTime),
		"duplicateOf": gqlField(issueOrPullRequest),
		"rationale":   gqlField(graphql.String),
		"stateReason": gqlField(issueStateReason),
		"updatedAt":   gqlField(dateTime),
	})

	pendingFieldSuggestion := s.mutationObject("PendingFieldSuggestion", graphql.Fields{
		"actor":          gqlField(actor),
		"createdAt":      gqlNonNull(dateTime),
		"issueField":     gqlField(issueFields),
		"rationale":      gqlField(graphql.String),
		"suggestedValue": gqlField(graphql.String),
		"updatedAt":      gqlField(dateTime),
	})

	pendingTypeSuggestion := s.mutationObject("PendingTypeSuggestion", graphql.Fields{
		"actor":     gqlField(actor),
		"createdAt": gqlNonNull(dateTime),
		"issueType": gqlField(issueType),
		"rationale": gqlField(graphql.String),
		"updatedAt": gqlField(dateTime),
	})

	// --- remaining standalone objects --------------------------------------
	memberFeatureRequestNotification := s.mutationObject("MemberFeatureRequestNotification", graphql.Fields{
		"body":      gqlNonNull(graphql.String),
		"id":        gqlNonNull(graphql.ID),
		"title":     gqlNonNull(graphql.String),
		"updatedAt": gqlNonNull(dateTime),
	})

	packageTag := s.mutationObject("PackageTag", graphql.Fields{
		"id":      gqlNonNull(graphql.ID),
		"name":    gqlNonNull(graphql.String),
		"version": gqlField(packageVersion),
	})

	projectV2ItemIssueFieldValue := s.mutationObject("ProjectV2ItemIssueFieldValue", graphql.Fields{
		"field":           gqlNonNull(projectV2FieldConfiguration),
		"issueFieldValue": gqlField(projectV2IssueFieldValues),
	})

	pullRequestRevisionMarker := s.mutationObject("PullRequestRevisionMarker", graphql.Fields{
		"createdAt":      gqlNonNull(dateTime),
		"lastSeenCommit": gqlNonNull(commit),
		"pullRequest":    gqlNonNull(pullRequest),
	})

	pullRequestThread := s.mutationObject("PullRequestThread", graphql.Fields{
		"comments": &graphql.Field{
			Type: graphql.NewNonNull(prReviewCommentConnection),
			Args: connectionArgs(graphql.FieldConfigArgument{
				"skip": &graphql.ArgumentConfig{Type: graphql.Int},
			}),
		},
		"diffSide":           gqlNonNull(diffSide),
		"id":                 gqlNonNull(graphql.ID),
		"isCollapsed":        gqlNonNull(graphql.Boolean),
		"isOutdated":         gqlNonNull(graphql.Boolean),
		"isResolved":         gqlNonNull(graphql.Boolean),
		"line":               gqlField(graphql.Int),
		"path":               gqlNonNull(graphql.String),
		"pullRequest":        gqlNonNull(pullRequest),
		"repository":         gqlNonNull(repository),
		"resolvedBy":         gqlField(user),
		"startDiffSide":      gqlField(diffSide),
		"startLine":          gqlField(graphql.Int),
		"subjectType":        gqlNonNull(prReviewThreadSubjectType),
		"viewerCanReply":     gqlNonNull(graphql.Boolean),
		"viewerCanResolve":   gqlNonNull(graphql.Boolean),
		"viewerCanUnresolve": gqlNonNull(graphql.Boolean),
	})

	// --- publish -----------------------------------------------------------
	s.registerExtraSchemaType(
		issueStateReason,
		diffSide,
		prReviewThreadSubjectType,
		prReviewDecision,
		commitCommentConnection,
		projectV2IssueFieldValues,
		commitCommentThread,
		pullRequestCommitCommentThread,
		genericHovercardContext,
		organizationTeamsHovercardContext,
		organizationsHovercardContext,
		reviewStatusHovercardContext,
		viewerHovercardContext,
		pendingAssigneeSuggestion,
		pendingCloseSuggestion,
		pendingFieldSuggestion,
		pendingTypeSuggestion,
		memberFeatureRequestNotification,
		packageTag,
		projectV2ItemIssueFieldValue,
		pullRequestRevisionMarker,
		pullRequestThread,
	)
}
