package graphqlapi

// The pull-request mutation surface: review comments and threads, review
// editing and deletion, reviewer requests, the per-reviewer viewed-file marks,
// bringing a branch up to date, archival, the merge queue, the
// pull-request-creation bypass list and a team's review-assignment settings.
//
// Each writes through the store primitive its REST equivalent writes through,
// and the two that are git writes — updatePullRequestBranch and
// revertPullRequest — go through the Pulls seam so the ref moves exactly as
// PUT /pulls/{n}/update-branch moves it.

import (
	"fmt"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Resolver) addPullRequestSurfaceMutations(mutationType *graphql.Object) {
	pullRequestType := s.graphqlTypes.pullRequest
	repositoryType := s.graphqlTypes.repository
	reviewType := s.graphqlTypes.pullRequestReview
	reviewCommentType := s.graphqlTypes.pullRequestReviewComment
	reviewThreadType := s.graphqlTypes.pullRequestReviewThread
	teamType := s.graphqlTypes.team
	actorInterface := s.graphqlTypes.actor
	gitObjectID := s.graphQLStringScalar("GitObjectID")

	reviewCommentEdge := s.mutationObject("PullRequestReviewCommentEdge", graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   gqlField(reviewCommentType),
	})
	// The account surface mints GitHub's one UserEdge; the two review-request
	// payloads name that same object rather than a second of the name.
	userEdge := s.gqlUserEdgeType(s.graphqlTypes.accountSurface)
	diffSide := s.sharedEnum("DiffSide", "LEFT", "RIGHT")

	// --- review comments and threads ---------------------------------------

	s.registerMutation(mutationType, "addPullRequestReviewComment", &graphql.Field{
		Type: s.mutationPayload("AddPullRequestReviewCommentPayload", graphql.Fields{
			"comment":     gqlField(reviewCommentType),
			"commentEdge": gqlField(reviewCommentEdge),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("AddPullRequestReviewCommentInput", graphql.InputObjectConfigFieldMap{
				"pullRequestId":       gqlID(),
				"pullRequestReviewId": gqlID(),
				"inReplyTo":           gqlID(),
				"body":                gqlString(),
				"commitOID":           gqlInputOf(gitObjectID),
				"path":                gqlString(),
				"position":            gqlInt(),
			})),
		}},
		Resolve: s.resolveAddPullRequestReviewComment,
	})

	s.registerMutation(mutationType, "addPullRequestReviewThread", &graphql.Field{
		Type: s.mutationPayload("AddPullRequestReviewThreadPayload", graphql.Fields{
			"thread": gqlField(reviewThreadType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("AddPullRequestReviewThreadInput", graphql.InputObjectConfigFieldMap{
				"pullRequestId":       gqlID(),
				"pullRequestReviewId": gqlID(),
				"body":                gqlNonNullString(),
				"path":                gqlString(),
				"line":                gqlInt(),
				"startLine":           gqlInt(),
				"side":                &graphql.InputObjectFieldConfig{Type: diffSide, DefaultValue: "RIGHT"},
				"startSide":           &graphql.InputObjectFieldConfig{Type: diffSide, DefaultValue: "RIGHT"},
				"subjectType": &graphql.InputObjectFieldConfig{
					Type:         s.sharedEnum("PullRequestReviewThreadSubjectType", "FILE", "LINE"),
					DefaultValue: "LINE",
				},
			})),
		}},
		Resolve: s.resolveAddPullRequestReviewThread,
	})

	s.registerMutation(mutationType, "addPullRequestReviewThreadReply", &graphql.Field{
		Type: s.mutationPayload("AddPullRequestReviewThreadReplyPayload", graphql.Fields{
			"comment": gqlField(reviewCommentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("AddPullRequestReviewThreadReplyInput", graphql.InputObjectConfigFieldMap{
				"pullRequestReviewThreadId": gqlNonNullID(),
				"pullRequestReviewId":       gqlID(),
				"body":                      gqlNonNullString(),
			})),
		}},
		Resolve: s.resolveAddPullRequestReviewThreadReply,
	})

	s.registerMutation(mutationType, "updatePullRequestReviewComment", &graphql.Field{
		Type: s.mutationPayload("UpdatePullRequestReviewCommentPayload", graphql.Fields{
			"pullRequestReviewComment": gqlField(reviewCommentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdatePullRequestReviewCommentInput", graphql.InputObjectConfigFieldMap{
				"pullRequestReviewCommentId": gqlNonNullID(),
				"body":                       gqlNonNullString(),
			})),
		}},
		Resolve: s.resolveUpdatePullRequestReviewComment,
	})

	s.registerMutation(mutationType, "deletePullRequestReviewComment", &graphql.Field{
		Type: s.mutationPayload("DeletePullRequestReviewCommentPayload", graphql.Fields{
			"pullRequestReview":        gqlField(reviewType),
			"pullRequestReviewComment": gqlField(reviewCommentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeletePullRequestReviewCommentInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeletePullRequestReviewComment,
	})

	// --- reviews -------------------------------------------------------------

	s.registerMutation(mutationType, "updatePullRequestReview", &graphql.Field{
		Type: s.mutationPayload("UpdatePullRequestReviewPayload", graphql.Fields{
			"pullRequestReview": gqlField(reviewType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdatePullRequestReviewInput", graphql.InputObjectConfigFieldMap{
				"pullRequestReviewId": gqlNonNullID(),
				"body":                gqlNonNullString(),
			})),
		}},
		Resolve: s.resolveUpdatePullRequestReview,
	})

	s.registerMutation(mutationType, "deletePullRequestReview", &graphql.Field{
		Type: s.mutationPayload("DeletePullRequestReviewPayload", graphql.Fields{
			"pullRequestReview": gqlField(reviewType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeletePullRequestReviewInput", graphql.InputObjectConfigFieldMap{
				"pullRequestReviewId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeletePullRequestReview,
	})

	// --- reviewer requests ---------------------------------------------------

	s.registerMutation(mutationType, "requestReviews", &graphql.Field{
		Type: s.mutationPayload("RequestReviewsPayload", graphql.Fields{
			"actor":                  gqlField(actorInterface),
			"pullRequest":            gqlField(pullRequestType),
			"requestedReviewersEdge": gqlField(userEdge),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("RequestReviewsInput", graphql.InputObjectConfigFieldMap{
				"pullRequestId": gqlNonNullID(),
				"userIds":       gqlListOf(graphql.ID),
				"teamIds":       gqlListOf(graphql.ID),
				"botIds":        gqlListOf(graphql.ID),
				"union":         &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveRequestReviews(p, false)
		},
	})

	s.registerMutation(mutationType, "requestReviewsByLogin", &graphql.Field{
		Type: s.mutationPayload("RequestReviewsByLoginPayload", graphql.Fields{
			"actor":                  gqlField(actorInterface),
			"pullRequest":            gqlField(pullRequestType),
			"requestedReviewersEdge": gqlField(userEdge),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("RequestReviewsByLoginInput", graphql.InputObjectConfigFieldMap{
				"pullRequestId": gqlNonNullID(),
				"userLogins":    gqlListOf(graphql.String),
				"teamSlugs":     gqlListOf(graphql.String),
				"botLogins":     gqlListOf(graphql.String),
				"union":         &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveRequestReviews(p, true)
		},
	})

	// --- the reviewer's own diff state ---------------------------------------

	viewedFile := func(name, payloadName, inputName string, viewed bool) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				"pullRequest": gqlField(pullRequestType),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
					"pullRequestId": gqlNonNullID(),
					"path":          gqlNonNullString(),
				})),
			}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.resolveFileViewed(p, viewed)
			},
		})
	}
	viewedFile("markFileAsViewed", "MarkFileAsViewedPayload", "MarkFileAsViewedInput", true)
	viewedFile("unmarkFileAsViewed", "UnmarkFileAsViewedPayload", "UnmarkFileAsViewedInput", false)

	// --- branch state --------------------------------------------------------

	s.registerMutation(mutationType, "updatePullRequestBranch", &graphql.Field{
		Type: s.mutationPayload("UpdatePullRequestBranchPayload", graphql.Fields{
			"pullRequest": gqlField(pullRequestType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdatePullRequestBranchInput", graphql.InputObjectConfigFieldMap{
				"pullRequestId":   gqlNonNullID(),
				"expectedHeadOid": gqlInputOf(gitObjectID),
				"updateMethod":    gqlInputOf(s.sharedEnum("PullRequestBranchUpdateMethod", "MERGE", "REBASE")),
			})),
		}},
		Resolve: s.resolveUpdatePullRequestBranch,
	})

	archive := func(name, payloadName, inputName string, archived bool) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				"pullRequest": gqlField(pullRequestType),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
					"pullRequestId": gqlNonNullID(),
				})),
			}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.resolvePullRequestArchival(p, archived)
			},
		})
	}
	archive("archivePullRequest", "ArchivePullRequestPayload", "ArchivePullRequestInput", true)
	archive("unarchivePullRequest", "UnarchivePullRequestPayload", "UnarchivePullRequestInput", false)

	// --- the merge queue -----------------------------------------------------

	mergeQueueEntryType := s.gqlMergeQueueEntryType()

	s.registerMutation(mutationType, "enqueuePullRequest", &graphql.Field{
		Type: s.mutationPayload("EnqueuePullRequestPayload", graphql.Fields{
			"mergeQueueEntry": gqlField(mergeQueueEntryType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("EnqueuePullRequestInput", graphql.InputObjectConfigFieldMap{
				"pullRequestId":   gqlNonNullID(),
				"expectedHeadOid": gqlInputOf(gitObjectID),
				"jump":            gqlBool(),
			})),
		}},
		Resolve: s.resolveEnqueuePullRequest,
	})

	s.registerMutation(mutationType, "dequeuePullRequest", &graphql.Field{
		Type: s.mutationPayload("DequeuePullRequestPayload", graphql.Fields{
			"mergeQueueEntry": gqlField(mergeQueueEntryType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DequeuePullRequestInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDequeuePullRequest,
	})

	// --- the creation-cap bypass list ----------------------------------------

	bypass := func(name, payloadName, inputName string, add bool) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				"repository": gqlField(repositoryType),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
					"repositoryId": gqlNonNullID(),
					"userIds":      gqlNonNullListOf(graphql.ID),
				})),
			}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.resolveCreationCapBypass(p, add)
			},
		})
	}
	bypass("addPullRequestCreationCapBypassUsers", "AddPullRequestCreationCapBypassUsersPayload",
		"AddPullRequestCreationCapBypassUsersInput", true)
	bypass("removePullRequestCreationCapBypassUsers", "RemovePullRequestCreationCapBypassUsersPayload",
		"RemovePullRequestCreationCapBypassUsersInput", false)

	// --- team review assignment ----------------------------------------------

	s.registerMutation(mutationType, "updateTeamReviewAssignment", &graphql.Field{
		Type: s.mutationPayload("UpdateTeamReviewAssignmentPayload", graphql.Fields{
			"team": gqlField(teamType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateTeamReviewAssignmentInput", graphql.InputObjectConfigFieldMap{
				"id":      gqlNonNullID(),
				"enabled": gqlNonNullBool(),
				"algorithm": &graphql.InputObjectFieldConfig{
					Type:         s.sharedEnum("TeamReviewAssignmentAlgorithm", "LOAD_BALANCE", "ROUND_ROBIN"),
					DefaultValue: "ROUND_ROBIN",
				},
				"teamMemberCount":              &graphql.InputObjectFieldConfig{Type: graphql.Int, DefaultValue: 1},
				"notifyTeam":                   &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true},
				"includeChildTeamMembers":      &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true},
				"removeTeamRequest":            &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true},
				"countMembersAlreadyRequested": &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true},
				"excludedTeamMemberIds":        gqlListOf(graphql.ID),
			})),
		}},
		Resolve: s.resolveUpdateTeamReviewAssignment,
	})
}

// --- merge queue types --------------------------------------------------------

func (s *Resolver) gqlMergeQueueType() *graphql.Object {
	uri := s.graphQLStringScalar("URI")
	return s.mutationObjectLazy("MergeQueue", func() graphql.Fields {
		return graphql.Fields{
			"id":                            gqlNonNull(graphql.ID),
			"repository":                    gqlField(s.graphqlTypes.repository),
			"resourcePath":                  gqlNonNull(uri),
			"url":                           gqlNonNull(uri),
			"configuration":                 gqlField(s.gqlMergeQueueConfigurationType()),
			"nextEntryEstimatedTimeToMerge": gqlField(graphql.Int),
			"entries": &graphql.Field{
				Type: s.gqlMergeQueueEntryConnectionType(),
				Args: graphql.FieldConfigArgument{
					"first":  &graphql.ArgumentConfig{Type: graphql.Int},
					"last":   &graphql.ArgumentConfig{Type: graphql.Int},
					"after":  &graphql.ArgumentConfig{Type: graphql.String},
					"before": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, _ := p.Source.(map[string]interface{})
					return repaginateConnection(source["entries"], p.Args), nil
				},
			},
		}
	})
}

func (s *Resolver) gqlMergeQueueConfigurationType() *graphql.Object {
	return s.mutationObject("MergeQueueConfiguration", graphql.Fields{
		"checkResponseTimeout":          gqlField(graphql.Int),
		"maximumEntriesToBuild":         gqlField(graphql.Int),
		"maximumEntriesToMerge":         gqlField(graphql.Int),
		"minimumEntriesToMerge":         gqlField(graphql.Int),
		"minimumEntriesToMergeWaitTime": gqlField(graphql.Int),
		"mergeMethod":                   gqlField(s.sharedEnum("PullRequestMergeMethod", "MERGE", "REBASE", "SQUASH")),
		"mergingStrategy":               gqlField(s.sharedEnum("MergeQueueMergingStrategy", "ALLGREEN", "HEADGREEN")),
	})
}

func (s *Resolver) gqlMergeQueueEntryType() *graphql.Object {
	dateTime := s.graphQLStringScalar("DateTime")
	return s.mutationObjectLazy("MergeQueueEntry", func() graphql.Fields {
		return graphql.Fields{
			"id":                   gqlNonNull(graphql.ID),
			"position":             gqlNonNull(graphql.Int),
			"jump":                 gqlNonNull(graphql.Boolean),
			"solo":                 gqlNonNull(graphql.Boolean),
			"enqueuedAt":           gqlNonNull(dateTime),
			"enqueuer":             gqlNonNull(s.graphqlTypes.actor),
			"estimatedTimeToMerge": gqlField(graphql.Int),
			"state": gqlNonNull(s.sharedEnum("MergeQueueEntryState",
				"AWAITING_CHECKS", "LOCKED", "MERGEABLE", "QUEUED", "UNMERGEABLE")),
			"pullRequest": gqlField(s.graphqlTypes.pullRequest),
			"mergeQueue":  gqlField(s.gqlMergeQueueType()),
		}
	})
}

func (s *Resolver) gqlMergeQueueEntryConnectionType() *graphql.Object {
	edge := s.mutationObjectLazy("MergeQueueEntryEdge", func() graphql.Fields {
		return graphql.Fields{
			"cursor": gqlNonNull(graphql.String),
			"node":   gqlField(s.gqlMergeQueueEntryType()),
		}
	})
	return s.mutationObjectLazy("MergeQueueEntryConnection", func() graphql.Fields {
		return graphql.Fields{
			"edges":      gqlField(graphql.NewList(edge)),
			"nodes":      gqlField(graphql.NewList(s.gqlMergeQueueEntryType())),
			"totalCount": gqlNonNull(graphql.Int),
			"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
		}
	})
}

// mergeQueueNodeID identifies the queue of one base branch of one repository.
func mergeQueueNodeID(repoID int, baseRef string) string {
	return fmt.Sprintf("MQ_kwDO%08d_%s", repoID, baseRef)
}

func mergeQueueEntryNodeID(prID int) string {
	return fmt.Sprintf("MQE_kwDO%08d", prID)
}

// mergeQueueToGQL renders the queue a pull request's base branch has.
func (s *Resolver) mergeQueueToGQL(repo *store.Repo, baseRef string) map[string]interface{} {
	if repo == nil {
		return nil
	}
	resourcePath := "/" + repo.FullName + "/queue/" + baseRef
	entries := make([]map[string]interface{}, 0)
	for _, queued := range s.store.MergeQueuePullRequests(repo.ID, baseRef) {
		entries = append(entries, s.mergeQueueEntryToGQL(repo, queued, false))
	}
	return map[string]interface{}{
		"id":            mergeQueueNodeID(repo.ID, baseRef),
		"repository":    optionalObject(repoToGraphQL(s.store, repo)),
		"resourcePath":  resourcePath,
		"url":           externalURL(resourcePath),
		"configuration": optionalObject(s.mergeQueueConfigurationToGQL(repo)),
		// bleephub merges a queue entry as soon as its own checks pass rather
		// than building batches, so there is no batch wait to estimate.
		"nextEntryEstimatedTimeToMerge": nil,
		"entries":                       gqlConnectionSource(entries),
	}
}

func (s *Resolver) mergeQueueConfigurationToGQL(repo *store.Repo) map[string]interface{} {
	method := "MERGE"
	switch {
	case !repo.AllowMergeCommit && repo.AllowSquashMerge:
		method = "SQUASH"
	case !repo.AllowMergeCommit && repo.AllowRebaseMerge:
		method = "REBASE"
	}
	return map[string]interface{}{
		"checkResponseTimeout":          nil,
		"maximumEntriesToBuild":         nil,
		"maximumEntriesToMerge":         nil,
		"minimumEntriesToMerge":         1,
		"minimumEntriesToMergeWaitTime": 0,
		"mergeMethod":                   method,
		// Every queued pull request must be green on its own before it merges,
		// which is GitHub's ALLGREEN strategy.
		"mergingStrategy": "ALLGREEN",
	}
}

// mergeQueueEntryToGQL renders one queued pull request. withQueue is false
// when the entry is being rendered from inside the queue itself, so the two do
// not render each other forever.
func (s *Resolver) mergeQueueEntryToGQL(repo *store.Repo, pr *store.PullRequest, withQueue bool) map[string]interface{} {
	if pr == nil {
		return nil
	}
	enqueuedAt := pr.UpdatedAt
	if pr.MergeQueueEnqueuedAt != nil {
		enqueuedAt = *pr.MergeQueueEnqueuedAt
	}
	state := "QUEUED"
	switch {
	case pr.MergeQueuePosition == 1 && pr.Mergeable == "MERGEABLE":
		state = "MERGEABLE"
	case pr.Mergeable == "CONFLICTING":
		state = "UNMERGEABLE"
	case pr.MergeQueuePosition == 1:
		state = "AWAITING_CHECKS"
	}
	entry := map[string]interface{}{
		"id":         mergeQueueEntryNodeID(pr.ID),
		"position":   pr.MergeQueuePosition,
		"jump":       false,
		"solo":       pr.MergeQueuePosition == 1,
		"enqueuedAt": enqueuedAt.Format(time.RFC3339),
		"enqueuer":   optionalRendered(s.store.GetUserByID(pr.AuthorID), userToGraphQL),
		// The estimate is a batching figure bleephub does not compute.
		"estimatedTimeToMerge": nil,
		"state":                state,
		"pullRequest":          optionalObject(pullRequestToGQL(pr, s.store)),
	}
	if withQueue {
		entry["mergeQueue"] = optionalObject(s.mergeQueueToGQL(repo, pr.BaseRefName))
	} else {
		entry["mergeQueue"] = nil
	}
	return entry
}

// --- review comment and thread resolvers ---------------------------------------

func (s *Resolver) resolveAddPullRequestReviewComment(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	user := s.ghUserFromContext(p.Context)
	body, _ := gqlInputString(input, "body")
	if body == "" {
		return nil, fmt.Errorf("Body can't be blank")
	}

	// A reply names the comment it answers; a new comment names the pull
	// request (or the pending review it belongs to) and the line it is on.
	if replyTo, ok := gqlInputString(input, "inReplyTo"); ok && replyTo != "" {
		parent := store.FindPullRequestReviewCommentByNodeID(s.store, replyTo)
		if parent == nil {
			return nil, gqlMissingNode("PullRequestReviewComment", replyTo)
		}
		rootID := parent.ID
		if parent.InReplyToID != 0 {
			rootID = parent.InReplyToID
		}
		created := s.store.PRReviewComments.Reply(parent.PullRequestID, rootID, user.ID, body)
		if created == nil {
			return nil, gqlMissingNodeType("PullRequestReviewComment")
		}
		s.attachReviewComment(input, created.ID)
		return s.reviewCommentPayload(created), nil
	}

	pr, err := s.pullRequestFromReviewInput(input)
	if err != nil {
		return nil, err
	}
	path, _ := gqlInputString(input, "path")
	if path == "" {
		return nil, fmt.Errorf("Path can't be blank")
	}
	position, _ := gqlInputInt(input, "position")
	commitOID, _ := gqlInputString(input, "commitOID")
	created := s.store.PRReviewComments.CreateRootComment(pr.ID, user.ID, path, body, commitOID, "RIGHT", position, 0)
	if created == nil {
		return nil, gqlMissingNodeType("PullRequestReviewComment")
	}
	s.attachReviewComment(input, created.ID)
	return s.reviewCommentPayload(created), nil
}

// attachReviewComment binds a new comment to the pending review the input
// names, which is what makes it part of that review rather than a standalone
// comment.
func (s *Resolver) attachReviewComment(input map[string]interface{}, commentID int) {
	reviewNodeID, ok := gqlInputString(input, "pullRequestReviewId")
	if !ok || reviewNodeID == "" {
		return
	}
	if review := store.FindReviewByNodeID(s.store, reviewNodeID); review != nil {
		s.store.PRReviewComments.AttachToReview(commentID, review.ID)
	}
}

func (s *Resolver) reviewCommentPayload(comment *store.PRReviewComment) map[string]interface{} {
	rendered := prReviewCommentToGQL(comment, s.store)
	return map[string]interface{}{
		"comment": optionalObject(rendered),
		"commentEdge": map[string]interface{}{
			"cursor": encodeCursor(comment.ID),
			"node":   optionalObject(rendered),
		},
	}
}

func (s *Resolver) resolveAddPullRequestReviewThread(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	user := s.ghUserFromContext(p.Context)
	pr, err := s.pullRequestFromReviewInput(input)
	if err != nil {
		return nil, err
	}
	body, _ := gqlInputString(input, "body")
	path, _ := gqlInputString(input, "path")
	if path == "" {
		return nil, fmt.Errorf("Path can't be blank")
	}
	line, _ := gqlInputInt(input, "line")
	startLine, _ := gqlInputInt(input, "startLine")
	side, _ := gqlInputString(input, "side")
	if side == "" {
		side = "RIGHT"
	}
	root := s.store.PRReviewComments.CreateRootComment(pr.ID, user.ID, path, body, "", side, line, startLine)
	if root == nil {
		return nil, gqlMissingNodeType("PullRequestReviewThread")
	}
	s.attachReviewComment(input, root.ID)
	thread := s.store.PRReviewComments.GetThread(root.ID)
	if thread == nil {
		return nil, gqlMissingNodeType("PullRequestReviewThread")
	}
	rendered := reviewThreadsForGraphQL([]*store.ReviewThread{thread}, s.store)
	if len(rendered) == 0 {
		return nil, gqlMissingNodeType("PullRequestReviewThread")
	}
	return map[string]interface{}{"thread": rendered[0]}, nil
}

func (s *Resolver) resolveAddPullRequestReviewThreadReply(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	user := s.ghUserFromContext(p.Context)
	threadNodeID, _ := gqlInputString(input, "pullRequestReviewThreadId")
	rootID, ok := store.ParsePRReviewThreadNodeID(threadNodeID)
	if !ok {
		return nil, gqlMissingNode("PullRequestReviewThread", threadNodeID)
	}
	root := s.store.PRReviewComments.Get(rootID)
	if root == nil {
		return nil, gqlMissingNode("PullRequestReviewThread", threadNodeID)
	}
	body, _ := gqlInputString(input, "body")
	created := s.store.PRReviewComments.Reply(root.PullRequestID, rootID, user.ID, body)
	if created == nil {
		return nil, gqlMissingNodeType("PullRequestReviewComment")
	}
	s.attachReviewComment(input, created.ID)
	return map[string]interface{}{"comment": optionalObject(prReviewCommentToGQL(created, s.store))}, nil
}

func (s *Resolver) resolveUpdatePullRequestReviewComment(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "pullRequestReviewCommentId")
	comment := store.FindPullRequestReviewCommentByNodeID(s.store, nodeID)
	if comment == nil {
		return nil, gqlMissingNode("PullRequestReviewComment", nodeID)
	}
	body, _ := gqlInputString(input, "body")
	if !s.store.PRReviewComments.Update(comment.ID, body) {
		return nil, gqlMissingNodeType("PullRequestReviewComment")
	}
	updated := s.store.PRReviewComments.Get(comment.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("PullRequestReviewComment")
	}
	return map[string]interface{}{
		"pullRequestReviewComment": optionalObject(prReviewCommentToGQL(updated, s.store)),
	}, nil
}

func (s *Resolver) resolveDeletePullRequestReviewComment(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "id")
	comment := store.FindPullRequestReviewCommentByNodeID(s.store, nodeID)
	if comment == nil {
		return nil, gqlMissingNode("PullRequestReviewComment", nodeID)
	}
	// The payload carries the comment as it was and the review it belonged
	// to, so both are read before the row is destroyed.
	deleted := s.store.PRReviewComments.Get(comment.ID)
	if deleted == nil {
		return nil, gqlMissingNodeType("PullRequestReviewComment")
	}
	rendered := prReviewCommentToGQL(deleted, s.store)
	var review interface{}
	if deleted.ReviewID != 0 {
		review = optionalRendered(s.store.GetPullRequestReview(deleted.ReviewID), func(r *store.PullRequestReview) map[string]interface{} {
			return prReviewToGQL(r, s.store)
		})
	}
	if !s.store.PRReviewComments.Delete(deleted.ID, s.store.Reactions) {
		return nil, gqlMissingNodeType("PullRequestReviewComment")
	}
	return map[string]interface{}{
		"pullRequestReviewComment": optionalObject(rendered),
		"pullRequestReview":        review,
	}, nil
}

// --- review resolvers -----------------------------------------------------------

func (s *Resolver) resolveUpdatePullRequestReview(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "pullRequestReviewId")
	review := store.FindReviewByNodeID(s.store, nodeID)
	if review == nil {
		return nil, gqlMissingNode("PullRequestReview", nodeID)
	}
	body, _ := gqlInputString(input, "body")
	if !s.store.UpdatePullRequestReview(review.ID, body) {
		return nil, gqlMissingNodeType("PullRequestReview")
	}
	updated := s.store.GetPullRequestReview(review.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("PullRequestReview")
	}
	return map[string]interface{}{"pullRequestReview": optionalObject(prReviewToGQL(updated, s.store))}, nil
}

func (s *Resolver) resolveDeletePullRequestReview(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "pullRequestReviewId")
	review := store.FindReviewByNodeID(s.store, nodeID)
	if review == nil {
		return nil, gqlMissingNode("PullRequestReview", nodeID)
	}
	deleted := s.store.GetPullRequestReview(review.ID)
	if deleted == nil {
		return nil, gqlMissingNodeType("PullRequestReview")
	}
	// GitHub only lets a pending review be deleted; a submitted one is part of
	// the record and is dismissed instead.
	if deleted.State != "PENDING" {
		return nil, fmt.Errorf("Can not delete a submitted review")
	}
	rendered := prReviewToGQL(deleted, s.store)
	if !s.store.DeletePullRequestReview(deleted.ID) {
		return nil, gqlMissingNodeType("PullRequestReview")
	}
	return map[string]interface{}{"pullRequestReview": optionalObject(rendered)}, nil
}

// --- reviewer requests -----------------------------------------------------------

// resolveRequestReviews is the body requestReviews and requestReviewsByLogin
// share. `union: false` — GitHub's default — replaces the requested set;
// `union: true` adds to it, which is why the removal half runs first.
func (s *Resolver) resolveRequestReviews(p graphql.ResolveParams, byLogin bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	pr, repo, err := s.pullRequestAndRepoFromInput(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	user := s.ghUserFromContext(p.Context)

	userIDs, teamIDs, err := s.reviewersFromInput(input, repo, byLogin)
	if err != nil {
		return nil, err
	}
	union, _ := gqlInputBool(input, "union")
	if !union {
		if len(pr.RequestedReviewerIDs) > 0 {
			s.store.RemoveRequestedReviewers(repo.FullName, pr.Number, pr.RequestedReviewerIDs, user.ID)
		}
		if len(pr.RequestedTeamIDs) > 0 {
			s.store.RemoveRequestedTeamReviewers(repo.FullName, pr.Number, pr.RequestedTeamIDs)
		}
	}
	if len(userIDs) > 0 {
		s.store.RequestReviewers(repo.FullName, pr.Number, userIDs, user.ID)
	}
	if len(teamIDs) > 0 {
		s.store.RequestTeamReviewers(repo.FullName, pr.Number, teamIDs)
	}

	updated := s.store.GetPullRequest(pr.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("PullRequest")
	}
	s.emitPullRequestChanges(repo, updated, user, store.SubjectChange{})
	payload := map[string]interface{}{
		"actor":                  optionalRendered(user, userToGraphQL),
		"pullRequest":            optionalObject(pullRequestToGQL(updated, s.store)),
		"requestedReviewersEdge": nil,
	}
	if len(updated.RequestedReviewerIDs) > 0 {
		last := updated.RequestedReviewerIDs[len(updated.RequestedReviewerIDs)-1]
		if reviewer := s.store.GetUserByID(last); reviewer != nil {
			payload["requestedReviewersEdge"] = map[string]interface{}{
				"cursor": encodeCursor(len(updated.RequestedReviewerIDs) - 1),
				"node":   userToGraphQL(reviewer),
			}
		}
	}
	return payload, nil
}

// reviewersFromInput resolves the accounts and teams a request names, by node
// id or by login depending on which mutation is asking. A Bot reviewer is
// resolved through the same account lookup: bleephub models an app's identity
// as an account, so a bot login names a user row.
func (s *Resolver) reviewersFromInput(input map[string]interface{}, repo *store.Repo, byLogin bool) ([]int, []int, error) {
	var userIDs, teamIDs []int
	if byLogin {
		for _, key := range []string{"userLogins", "botLogins"} {
			logins, _ := gqlInputStrings(input, key)
			for _, login := range logins {
				reviewer := s.store.LookupUserByLogin(login)
				if reviewer == nil {
					return nil, nil, gqlMissingNodeType("User")
				}
				userIDs = append(userIDs, reviewer.ID)
			}
		}
		slugs, _ := gqlInputStrings(input, "teamSlugs")
		owner, _, _ := store.SplitRepoFullName(repo.FullName)
		for _, slug := range slugs {
			team := s.store.GetTeam(owner, slug)
			if team == nil {
				return nil, nil, gqlMissingNodeType("Team")
			}
			teamIDs = append(teamIDs, team.ID)
		}
		return userIDs, teamIDs, nil
	}
	for _, key := range []string{"userIds", "botIds"} {
		ids, _ := gqlInputStrings(input, key)
		for _, nodeID := range ids {
			reviewer := store.FindUserByNodeID(s.store, nodeID)
			if reviewer == nil {
				return nil, nil, gqlMissingNode("User", nodeID)
			}
			userIDs = append(userIDs, reviewer.ID)
		}
	}
	ids, _ := gqlInputStrings(input, "teamIds")
	for _, nodeID := range ids {
		team := s.teamByNodeID(nodeID)
		if team == nil {
			return nil, nil, gqlMissingNode("Team", nodeID)
		}
		teamIDs = append(teamIDs, team.ID)
	}
	return userIDs, teamIDs, nil
}

// teamByNodeID resolves a Team global node id to its row.
func (s *Resolver) teamByNodeID(nodeID string) *store.Team {
	if nodeID == "" {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, team := range s.store.Teams {
		if team.NodeID == nodeID {
			clone := *team
			return &clone
		}
	}
	return nil
}

// --- viewed files, branch state, archival ------------------------------------------

func (s *Resolver) resolveFileViewed(p graphql.ResolveParams, viewed bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	pr, _, err := s.pullRequestAndRepoFromInput(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	path, _ := gqlInputString(input, "path")
	if path == "" {
		return nil, fmt.Errorf("Path can't be blank")
	}
	user := s.ghUserFromContext(p.Context)
	if !s.store.SetPullRequestFileViewed(pr.ID, user.ID, path, viewed) {
		return nil, gqlMissingNodeType("PullRequest")
	}
	updated := s.store.GetPullRequest(pr.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("PullRequest")
	}
	return map[string]interface{}{"pullRequest": optionalObject(pullRequestToGQL(updated, s.store))}, nil
}

func (s *Resolver) resolveUpdatePullRequestBranch(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	pr, repo, err := s.pullRequestAndRepoFromInput(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	if pr.State != "OPEN" {
		return nil, fmt.Errorf("the pull request is not open")
	}
	expected, _ := gqlInputString(input, "expectedHeadOid")
	method, _ := gqlInputString(input, "updateMethod")
	if method == "" {
		method = "MERGE"
	}
	if err := s.pulls.UpdatePullRequestBranch(repo, pr, s.ghUserFromContext(p.Context), expected, method); err != nil {
		return nil, err
	}
	updated := s.store.GetPullRequest(pr.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("PullRequest")
	}
	return map[string]interface{}{"pullRequest": optionalObject(pullRequestToGQL(updated, s.store))}, nil
}

func (s *Resolver) resolvePullRequestArchival(p graphql.ResolveParams, archived bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	pr, _, err := s.pullRequestAndRepoFromInput(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	s.store.UpdatePullRequest(pr.ID, func(item *store.PullRequest) { item.Archived = archived })
	updated := s.store.GetPullRequest(pr.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("PullRequest")
	}
	return map[string]interface{}{"pullRequest": optionalObject(pullRequestToGQL(updated, s.store))}, nil
}

// --- merge queue resolvers ----------------------------------------------------------

func (s *Resolver) resolveEnqueuePullRequest(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	pr, repo, err := s.pullRequestAndRepoFromInput(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	if pr.State != "OPEN" {
		return nil, fmt.Errorf("only an open pull request can be queued")
	}
	if expected, ok := gqlInputString(input, "expectedHeadOid"); ok && expected != "" {
		if head := s.prHeadSha(repo, pr); head != "" && head != expected {
			return nil, fmt.Errorf("Head has changed since the expected oid was read")
		}
	}
	jump, _ := gqlInputBool(input, "jump")
	queued := s.store.EnqueuePullRequest(pr.ID, jump)
	if queued == nil {
		return nil, fmt.Errorf("the pull request is already queued")
	}
	entry := s.mergeQueueEntryToGQL(repo, queued, true)
	entry["jump"] = jump
	return map[string]interface{}{"mergeQueueEntry": optionalObject(entry)}, nil
}

func (s *Resolver) resolveDequeuePullRequest(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	pr, repo, err := s.pullRequestAndRepoFromInput(input, "id")
	if err != nil {
		return nil, err
	}
	removed := s.store.DequeuePullRequest(pr.ID)
	if removed == nil {
		return nil, fmt.Errorf("the pull request is not queued")
	}
	return map[string]interface{}{
		"mergeQueueEntry": optionalObject(s.mergeQueueEntryToGQL(repo, removed, true)),
	}, nil
}

// --- the creation-cap bypass list ------------------------------------------------------

func (s *Resolver) resolveCreationCapBypass(p graphql.ResolveParams, add bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	ids, _ := gqlInputStrings(input, "userIds")
	if len(ids) == 0 {
		return nil, fmt.Errorf("userIds names no account")
	}
	logins := make([]string, 0, len(ids))
	for _, nodeID := range ids {
		user := store.FindUserByNodeID(s.store, nodeID)
		if user == nil {
			return nil, gqlMissingNode("User", nodeID)
		}
		logins = append(logins, user.Login)
	}
	s.store.ChangePRCreationBypass(repo.FullName, logins, add)
	return map[string]interface{}{"repository": optionalObject(repoToGraphQL(s.store, repo))}, nil
}

// --- team review assignment ------------------------------------------------------------

func (s *Resolver) resolveUpdateTeamReviewAssignment(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "id")
	team := s.teamByNodeID(nodeID)
	if team == nil {
		return nil, gqlMissingNode("Team", nodeID)
	}
	org := s.store.GetOrgByID(team.OrgID)
	if org == nil {
		return nil, gqlMissingNodeType("Organization")
	}

	assignment := store.TeamReviewAssignment{Algorithm: "ROUND_ROBIN", TeamMemberCount: 1}
	if team.ReviewAssignment != nil {
		assignment = *team.ReviewAssignment
	}
	assignment.Enabled, _ = gqlInputBool(input, "enabled")
	if algorithm, ok := gqlInputString(input, "algorithm"); ok {
		assignment.Algorithm = algorithm
	}
	if count, ok := gqlInputInt(input, "teamMemberCount"); ok {
		assignment.TeamMemberCount = count
	}
	if value, ok := gqlInputBool(input, "notifyTeam"); ok {
		assignment.NotifyTeam = value
	}
	if value, ok := gqlInputBool(input, "includeChildTeamMembers"); ok {
		assignment.IncludeChildTeamMembers = value
	}
	if value, ok := gqlInputBool(input, "removeTeamRequest"); ok {
		assignment.RemoveTeamRequest = value
	}
	if value, ok := gqlInputBool(input, "countMembersAlreadyRequested"); ok {
		assignment.CountMembersAlreadyRequested = value
	}
	if excluded, ok := gqlInputStrings(input, "excludedTeamMemberIds"); ok {
		ids := make([]int, 0, len(excluded))
		for _, memberNodeID := range excluded {
			member := store.FindUserByNodeID(s.store, memberNodeID)
			if member == nil {
				return nil, gqlMissingNode("User", memberNodeID)
			}
			ids = append(ids, member.ID)
		}
		assignment.ExcludedTeamMemberIDs = ids
	}

	if !s.store.UpdateTeam(org.Login, team.Slug, func(t *store.Team) {
		stored := assignment
		stored.ExcludedTeamMemberIDs = append([]int(nil), assignment.ExcludedTeamMemberIDs...)
		t.ReviewAssignment = &stored
	}) {
		return nil, gqlMissingNodeType("Team")
	}
	updated := s.store.GetTeam(org.Login, team.Slug)
	if updated == nil {
		return nil, gqlMissingNodeType("Team")
	}
	return map[string]interface{}{"team": optionalObject(s.teamToGQL(updated))}, nil
}

// teamToGQL renders a team into the shared Team source shape the requested-
// reviewer union and the read surface already use.
func (s *Resolver) teamToGQL(team *store.Team) map[string]interface{} {
	if team == nil {
		return nil
	}
	out := map[string]interface{}{
		"__typename": "Team",
		"id":         team.NodeID,
		"name":       team.Name,
		"slug":       team.Slug,
	}
	if org := s.store.GetOrgByID(team.OrgID); org != nil {
		out["organization"] = orgToGraphQL(org)
	}
	return out
}

// --- shared helpers ---------------------------------------------------------------------

// pullRequestFromInput resolves the pull request a mutation input names as a
// detached snapshot.
func (s *Resolver) pullRequestFromInput(input map[string]interface{}, key string) (*store.PullRequest, error) {
	nodeID, _ := gqlInputString(input, key)
	found := store.FindPullRequestByNodeID(s.store, nodeID)
	if found == nil {
		return nil, gqlMissingNode("PullRequest", nodeID)
	}
	snapshot := s.store.GetPullRequest(found.ID)
	if snapshot == nil {
		return nil, gqlMissingNodeType("PullRequest")
	}
	return snapshot, nil
}

func (s *Resolver) pullRequestAndRepoFromInput(input map[string]interface{}, key string) (*store.PullRequest, *store.Repo, error) {
	pr, err := s.pullRequestFromInput(input, key)
	if err != nil {
		return nil, nil, err
	}
	repo := s.store.GetRepoByID(pr.RepoID)
	if repo == nil {
		return nil, nil, gqlMissingNodeType("Repository")
	}
	return pr, repo, nil
}

// pullRequestFromReviewInput resolves the pull request a review-comment input
// names, which GitHub lets a client give either directly or through the
// pending review the comment joins.
func (s *Resolver) pullRequestFromReviewInput(input map[string]interface{}) (*store.PullRequest, error) {
	if nodeID, ok := gqlInputString(input, "pullRequestId"); ok && nodeID != "" {
		return s.pullRequestFromInput(input, "pullRequestId")
	}
	reviewNodeID, ok := gqlInputString(input, "pullRequestReviewId")
	if !ok || reviewNodeID == "" {
		return nil, fmt.Errorf("one of pullRequestId or pullRequestReviewId is required")
	}
	review := store.FindReviewByNodeID(s.store, reviewNodeID)
	if review == nil {
		return nil, gqlMissingNode("PullRequestReview", reviewNodeID)
	}
	pr := s.store.GetPullRequest(review.PRID)
	if pr == nil {
		return nil, gqlMissingNodeType("PullRequest")
	}
	return pr, nil
}
