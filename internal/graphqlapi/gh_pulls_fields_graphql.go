package graphqlapi

import (
	"fmt"
	"html"
	"strconv"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// Completes the GraphQL surface of PullRequest, PullRequestReview and
// PullRequestReviewThread, backed by the same store/git data REST serves. Where
// bleephub models nothing (merge queue, stacked-PR graph, suggested-reviewer
// engine) the field answers the truthful zero, called out at its resolver.
// addPullRequestSurfaceFields runs once, after the three types and their
// helpers exist, adding fields additively.

// prSurfaceDeps carries the already-built type objects the surface fields name,
// passed in rather than re-minted (graphql-go rejects two types of one name).
type prSurfaceDeps struct {
	pullRequest          *graphql.Object
	review               *graphql.Object
	thread               *graphql.Object
	reviewConnection     *graphql.Object
	reviewCommentConn    *graphql.Object
	statusCheckRollup    *graphql.Object
	reviewRequest        *graphql.Object
	userType             *graphql.Object
	repoType             *graphql.Object
	uri                  *graphql.Scalar
	dateTime             *graphql.Scalar
	bigInt               *graphql.Scalar
	htmlScalar           *graphql.Scalar
	commentAuthorAssoc   *graphql.Enum
	subscriptionState    *graphql.Enum
	pullRequestMergeEnum *graphql.Enum
}

// prEmptyConnection is a well-formed empty connection, never a nil that would
// break a non-null child.
func prEmptyConnection() map[string]interface{} {
	return map[string]interface{}{
		"nodes":      []interface{}{},
		"edges":      []interface{}{},
		"totalCount": 0,
		"pageInfo": map[string]interface{}{
			"hasNextPage":     false,
			"hasPreviousPage": false,
			"startCursor":     nil,
			"endCursor":       nil,
		},
	}
}

func srcMap(p graphql.ResolveParams) map[string]interface{} {
	m, _ := p.Source.(map[string]interface{})
	return m
}

// prRepoPerms resolves the viewer's read/write/admin standing on the source's
// repo, plus the viewer and the source's authorID.
func (s *Resolver) prRepoPerms(p graphql.ResolveParams) (viewer *store.User, authorID int, read, write, admin bool) {
	src := srcMap(p)
	authorID, _ = src["authorID"].(int)
	repoID, _ := src["repoID"].(int)
	viewer = s.ghUserFromContext(p.Context)
	repo := s.store.GetRepoByID(repoID)
	if repo != nil {
		read = s.viewerCanReadRepo(p.Context, repo)
		write = s.viewerCanPushRepo(p.Context, repo)
		admin = s.viewerCanAdminRepo(p.Context, repo)
	}
	return
}

// prBoolField is a Boolean! field computed from the viewer's permissions.
func (s *Resolver) prBoolField(fn func(viewer *store.User, authorID int, read, write, admin bool) bool) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			viewer, authorID, read, write, admin := s.prRepoPerms(p)
			return fn(viewer, authorID, read, write, admin), nil
		},
	}
}

func constBoolField(v bool) *graphql.Field {
	return &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return v, nil },
	}
}

// prCannotUpdateReasons is the [CommentCannotUpdateReason!]! companion of a
// viewerCanUpdate: empty when editable, else LOGIN_REQUIRED or
// INSUFFICIENT_ACCESS.
func prCannotUpdateReasons(canUpdate bool, viewer *store.User) []interface{} {
	if canUpdate {
		return []interface{}{}
	}
	if viewer == nil {
		return []interface{}{"LOGIN_REQUIRED"}
	}
	return []interface{}{"INSUFFICIENT_ACCESS"}
}

func (s *Resolver) addPullRequestSurfaceFields(d prSurfaceDeps) {
	cannotUpdateEnum := s.sharedEnum("CommentCannotUpdateReason",
		"ARCHIVED", "DENIED", "INSUFFICIENT_ACCESS", "LOCKED", "LOGIN_REQUIRED",
		"MAINTENANCE", "VERIFIED_EMAIL_REQUIRED")
	diffSideEnum := s.sharedEnum("DiffSide", "LEFT", "RIGHT")
	subjectTypeEnum := s.sharedEnum("PullRequestReviewThreadSubjectType", "FILE", "LINE")

	assigneeConn := s.prAssigneeConnectionType(d.userType)
	suggestedReviewerActorConn := s.prSuggestedReviewerActorConnectionType()
	suggestedReviewerType := s.prSuggestedReviewerType(d.userType)
	hovercardType := s.prHovercardType()
	stackType, stackEntryType := s.prStackTypes()
	timelineConn := s.prTimelineConnectionType(d)
	projectV2Type := s.projectV2GraphQLTypes()
	projectV2Conn := s.gqlConnectionType("ProjectV2", projectV2Type)

	s.addPullRequestNodeFields(d, assigneeConn, suggestedReviewerActorConn,
		suggestedReviewerType, hovercardType, stackType, stackEntryType,
		timelineConn, projectV2Type, projectV2Conn, cannotUpdateEnum)
	s.addPullRequestReviewFields(d, cannotUpdateEnum)
	s.addPullRequestReviewThreadFields(d, diffSideEnum, subjectTypeEnum)
}

// --- PullRequest node fields -------------------------------------------------

func (s *Resolver) addPullRequestNodeFields(
	d prSurfaceDeps,
	assigneeConn, suggestedReviewerActorConn, suggestedReviewerType, hovercardType,
	stackType, stackEntryType, timelineConn, projectV2Type, projectV2Conn *graphql.Object,
	cannotUpdateEnum *graphql.Enum,
) {
	pr := d.pullRequest
	uri := d.uri
	dateTime := d.dateTime

	pr.AddFieldConfig("bodyHTML", &graphql.Field{
		Type: graphql.NewNonNull(d.htmlScalar),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			body, _ := srcMap(p)["body"].(string)
			return discussionBodyToHTML(body), nil
		},
	})
	pr.AddFieldConfig("bodyText", &graphql.Field{
		Type: graphql.NewNonNull(graphql.String),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			body, _ := srcMap(p)["body"].(string)
			return bodyText(body), nil
		},
	})
	pr.AddFieldConfig("titleHTML", &graphql.Field{
		Type: graphql.NewNonNull(d.htmlScalar),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			t, _ := srcMap(p)["title"].(string)
			return html.EscapeString(t), nil
		},
	})
	pr.AddFieldConfig("authorAssociation", &graphql.Field{Type: graphql.NewNonNull(d.commentAuthorAssoc)})

	pr.AddFieldConfig("permalink", &graphql.Field{
		Type:    graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return srcMap(p)["url"], nil },
	})
	pr.AddFieldConfig("checksResourcePath", srcPathField(uri, "resourcePath", "/checks", false))
	pr.AddFieldConfig("checksUrl", srcPathField(uri, "resourcePath", "/checks", true))
	pr.AddFieldConfig("revertResourcePath", srcPathField(uri, "resourcePath", "/revert", false))
	pr.AddFieldConfig("revertUrl", srcPathField(uri, "resourcePath", "/revert", true))

	// No per-edit history is recorded, so the edit-history fields answer "never
	// edited" and userContentEdits is a real empty connection.
	pr.AddFieldConfig("publishedAt", &graphql.Field{
		Type:    dateTime,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return srcMap(p)["createdAt"], nil },
	})
	pr.AddFieldConfig("lastEditedAt", &graphql.Field{Type: dateTime})
	pr.AddFieldConfig("includesCreatedEdit", constBoolField(false))
	pr.AddFieldConfig("editor", &graphql.Field{
		Type:    s.graphqlTypes.actor,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	pr.AddFieldConfig("userContentEdits", &graphql.Field{
		Type:    s.gqlUserContentEditConnectionType(),
		Args:    relayConnectionArgs(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return emptyUserContentEditConnection(), nil },
	})

	// No inbound-email PR path.
	pr.AddFieldConfig("createdViaEmail", constBoolField(false))
	// No merge queue is modelled.
	pr.AddFieldConfig("isInMergeQueue", constBoolField(false))
	pr.AddFieldConfig("isMergeQueueEnabled", constBoolField(false))
	pr.AddFieldConfig("mergeQueue", &graphql.Field{
		Type:    s.gqlMergeQueueType(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	pr.AddFieldConfig("mergeQueueEntry", &graphql.Field{
		Type:    s.gqlMergeQueueEntryType(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	// No stacked-PR graph is modelled.
	pr.AddFieldConfig("stack", &graphql.Field{
		Type:    stackType,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	pr.AddFieldConfig("stackEntry", &graphql.Field{
		Type:    stackEntryType,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	// No per-viewer read state is tracked; the field is nullable, so null.
	pr.AddFieldConfig("isReadByViewer", &graphql.Field{Type: graphql.Boolean})

	// canBeRebased tracks the stored mergeability, the only merge gate modelled.
	pr.AddFieldConfig("canBeRebased", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			m, _ := srcMap(p)["mergeable"].(string)
			return m == "MERGEABLE", nil
		},
	})

	// Default merge-message text: title as headline, body as message, like
	// GitHub's merge dialog.
	mergeArgs := graphql.FieldConfigArgument{"mergeType": &graphql.ArgumentConfig{Type: d.pullRequestMergeEnum}}
	pr.AddFieldConfig("viewerMergeHeadlineText", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.String),
		Args:    mergeArgs,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { t, _ := srcMap(p)["title"].(string); return t, nil },
	})
	pr.AddFieldConfig("viewerMergeBodyText", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.String),
		Args:    mergeArgs,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { b, _ := srcMap(p)["body"].(string); return b, nil },
	})

	// Computed by the renderer under the store lock; the field reads the key.
	pr.AddFieldConfig("totalCommentsCount", &graphql.Field{Type: graphql.Int})
	pr.AddFieldConfig("statusCheckRollup", &graphql.Field{Type: d.statusCheckRollup})

	pr.AddFieldConfig("headRef", &graphql.Field{
		Type: s.gqlRefType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.prRefSource(srcMap(p), "headRefName"), nil
		},
	})
	pr.AddFieldConfig("baseRepository", &graphql.Field{
		Type:    d.repoType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return srcMap(p)["repository"], nil },
	})

	// No relationship contexts are computed; the contexts list is empty.
	pr.AddFieldConfig("hovercard", &graphql.Field{
		Type: graphql.NewNonNull(hovercardType),
		Args: graphql.FieldConfigArgument{"includeNotificationContexts": &graphql.ArgumentConfig{Type: graphql.Boolean}},
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			return map[string]interface{}{"contexts": []interface{}{}}, nil
		},
	})

	pr.AddFieldConfig("assignedActors", &graphql.Field{
		Type: graphql.NewNonNull(assigneeConn),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return repaginateConnection(srcMap(p)["assignedActors"], p.Args), nil
		},
	})
	pr.AddFieldConfig("participants", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlUserConnectionType(d.userType)),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return repaginateConnection(srcMap(p)["participants"], p.Args), nil
		},
	})

	// latestOpinionatedReviews — the APPROVED/CHANGES_REQUESTED subset of
	// latestReviews.
	pr.AddFieldConfig("latestOpinionatedReviews", &graphql.Field{
		Type: d.reviewConnection,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			latest, _ := srcMap(p)["latestReviews"].(map[string]interface{})
			nodes, _ := latest["nodes"].([]map[string]interface{})
			opinionated := make([]map[string]interface{}, 0, len(nodes))
			for _, n := range nodes {
				switch n["state"] {
				case "APPROVED", "CHANGES_REQUESTED":
					opinionated = append(opinionated, n)
				}
			}
			return repaginateConnection(map[string]interface{}{"nodes": opinionated, "totalCount": len(opinionated)}, p.Args), nil
		},
	})

	// No suggestion engine, so these are empty.
	pr.AddFieldConfig("suggestedReviewers", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.NewList(suggestedReviewerType)),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return []interface{}{}, nil },
	})
	pr.AddFieldConfig("suggestedReviewerActors", &graphql.Field{
		Type:    graphql.NewNonNull(suggestedReviewerActorConn),
		Args:    relayConnectionArgs(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return prEmptyConnection(), nil },
	})
	// No suggestion ranking, so empty.
	pr.AddFieldConfig("suggestedActors", &graphql.Field{
		Type:    graphql.NewNonNull(assigneeConn),
		Args:    withArg(relayConnectionArgs(), "query", graphql.String),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return prEmptyConnection(), nil },
	})

	// timeline is GitHub's deprecated alias of timelineItems, carrying the
	// review/comment members of its older union filtered from the same entries.
	pr.AddFieldConfig("timeline", &graphql.Field{
		Type:    graphql.NewNonNull(timelineConn),
		Args:    prTimelineArgs(dateTime),
		Resolve: s.resolvePullRequestTimeline,
	})

	// No classic project cards for PRs, so empty.
	pr.AddFieldConfig("projectCards", &graphql.Field{
		Type:    graphql.NewNonNull(s.projectClassicCardConnectionType()),
		Args:    relayConnectionArgs(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return prEmptyConnection(), nil },
	})
	// A PR is a ProjectV2Owner in GitHub, but projects attach to
	// orgs/users/repos here, not PRs, so it owns none.
	pr.AddFieldConfig("projectV2", &graphql.Field{
		Type:    projectV2Type,
		Args:    graphql.FieldConfigArgument{"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)}},
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	pr.AddFieldConfig("projectsV2", &graphql.Field{
		Type:    graphql.NewNonNull(projectV2Conn),
		Args:    relayConnectionArgs(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return prEmptyConnection(), nil },
	})

	// viewer-relationship booleans, from the viewer's repo standing.
	pr.AddFieldConfig("viewerDidAuthor", s.prBoolField(func(v *store.User, authorID int, _, _, _ bool) bool {
		return v != nil && v.ID == authorID
	}))
	pr.AddFieldConfig("viewerCanUpdate", s.prBoolField(func(v *store.User, authorID int, _, write, _ bool) bool {
		return v != nil && (write || v.ID == authorID)
	}))
	pr.AddFieldConfig("viewerCanClose", s.prBoolField(func(v *store.User, authorID int, _, write, _ bool) bool {
		return v != nil && (write || v.ID == authorID)
	}))
	pr.AddFieldConfig("viewerCanReopen", s.prBoolField(func(v *store.User, authorID int, _, write, _ bool) bool {
		return v != nil && (write || v.ID == authorID)
	}))
	pr.AddFieldConfig("viewerCanAssign", s.prBoolField(func(_ *store.User, _ int, _, write, _ bool) bool { return write }))
	pr.AddFieldConfig("viewerCanLabel", s.prBoolField(func(_ *store.User, _ int, _, write, _ bool) bool { return write }))
	pr.AddFieldConfig("viewerCanApplySuggestion", s.prBoolField(func(_ *store.User, _ int, _, write, _ bool) bool { return write }))
	pr.AddFieldConfig("viewerCanEditFiles", s.prBoolField(func(_ *store.User, _ int, _, write, _ bool) bool { return write }))
	pr.AddFieldConfig("viewerCanUpdateBranch", s.prBoolField(func(_ *store.User, _ int, _, write, _ bool) bool { return write }))
	pr.AddFieldConfig("viewerCanDeleteHeadRef", s.prBoolField(func(_ *store.User, _ int, _, write, _ bool) bool { return write }))
	pr.AddFieldConfig("viewerCanEnableAutoMerge", s.prBoolField(func(_ *store.User, _ int, _, write, _ bool) bool { return write }))
	pr.AddFieldConfig("viewerCanDisableAutoMerge", s.prBoolField(func(_ *store.User, _ int, _, write, _ bool) bool { return write }))
	pr.AddFieldConfig("viewerCanMergeAsAdmin", s.prBoolField(func(_ *store.User, _ int, _, _, admin bool) bool { return admin }))
	pr.AddFieldConfig("viewerCanSubscribe", s.prBoolField(func(v *store.User, _ int, read, _, _ bool) bool { return v != nil && read }))
	pr.AddFieldConfig("viewerCannotUpdateReasons", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(cannotUpdateEnum))),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			v, authorID, _, write, _ := s.prRepoPerms(p)
			return prCannotUpdateReasons(v != nil && (write || v.ID == authorID), v), nil
		},
	})

	// viewerSubscription mirrors GitHub's auto-subscribe: a participant is
	// SUBSCRIBED, a repo-watcher inherits, else UNSUBSCRIBED; anonymous is null.
	pr.AddFieldConfig("viewerSubscription", &graphql.Field{
		Type:    d.subscriptionState,
		Resolve: s.resolvePullRequestViewerSubscription,
	})

	pr.AddFieldConfig("viewerLatestReview", &graphql.Field{
		Type:    d.review,
		Resolve: s.resolveViewerLatestReview,
	})
	pr.AddFieldConfig("viewerLatestReviewRequest", &graphql.Field{
		Type:    d.reviewRequest,
		Resolve: s.resolveViewerLatestReviewRequest,
	})
}

// srcPathField renders a URI! from a source path key plus a suffix;
// external=true wraps it as an absolute URL.
func srcPathField(uri *graphql.Scalar, key, suffix string, external bool) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			base, _ := srcMap(p)[key].(string)
			path := base + suffix
			if external {
				return externalURL(path), nil
			}
			return path, nil
		},
	}
}

func (s *Resolver) prRefSource(src map[string]interface{}, nameKey string) map[string]interface{} {
	name, _ := src[nameKey].(string)
	ref := map[string]interface{}{
		"name":          name,
		"prefix":        "refs/heads/",
		"qualifiedName": "refs/heads/" + name,
	}
	prID, _ := src["databaseId"].(int)
	if prObj := s.store.GetPullRequest(prID); prObj != nil {
		if repo := s.store.GetRepoByID(prObj.RepoID); repo != nil {
			ref["repoFullName"] = repo.FullName
			if rule := s.branchProtectionRuleForPR(repo, name); rule != nil {
				ref["branchProtectionRule"] = rule
			}
		}
	}
	return ref
}

func (s *Resolver) resolvePullRequestViewerSubscription(p graphql.ResolveParams) (interface{}, error) {
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil {
		return nil, nil
	}
	src := srcMap(p)
	prID, _ := src["databaseId"].(int)
	repoID, _ := src["repoID"].(int)
	pr := s.store.GetPullRequest(prID)
	if pr != nil {
		if pr.AuthorID == viewer.ID {
			return "SUBSCRIBED", nil
		}
		for _, aid := range pr.AssigneeIDs {
			if aid == viewer.ID {
				return "SUBSCRIBED", nil
			}
		}
	}
	if sub := s.store.GetRepoSubscription(viewer.ID, repoID); sub != nil {
		if sub.Ignored {
			return "IGNORED", nil
		}
		if sub.Subscribed {
			return "SUBSCRIBED", nil
		}
	}
	return "UNSUBSCRIBED", nil
}

func (s *Resolver) resolveViewerLatestReview(p graphql.ResolveParams) (interface{}, error) {
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil {
		return nil, nil
	}
	prID, _ := srcMap(p)["databaseId"].(int)
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	var latest *store.PullRequestReview
	for _, r := range s.store.PRReviewsByPR[prID] {
		if r.AuthorID != viewer.ID {
			continue
		}
		if latest == nil || r.CreatedAt.After(latest.CreatedAt) {
			latest = r
		}
	}
	if latest == nil {
		return nil, nil
	}
	return prReviewSourceLocked(latest, s.store), nil
}

func (s *Resolver) resolveViewerLatestReviewRequest(p graphql.ResolveParams) (interface{}, error) {
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil {
		return nil, nil
	}
	prID, _ := srcMap(p)["databaseId"].(int)
	pr := s.store.GetPullRequest(prID)
	if pr == nil {
		return nil, nil
	}
	for _, id := range pr.RequestedReviewerIDs {
		if id != viewer.ID {
			continue
		}
		reviewer := userToGraphQL(viewer)
		reviewer["__typename"] = "User"
		return map[string]interface{}{"requestedReviewer": reviewer}, nil
	}
	return nil, nil
}

// resolvePullRequestTimeline answers the deprecated PullRequest.timeline,
// reusing the live timelineItems entries filtered to the older union's members
// (reviews, review threads, issue comments).
func (s *Resolver) resolvePullRequestTimeline(p graphql.ResolveParams) (interface{}, error) {
	items, err := s.resolveTimelineItems(p, "pull_request")
	if err != nil {
		return nil, err
	}
	conn, _ := items.(map[string]interface{})
	allowed := map[string]bool{"PullRequestReview": true, "PullRequestReviewThread": true, "IssueComment": true}
	kept := []interface{}{}
	if raw, ok := conn["nodes"].([]interface{}); ok {
		for _, n := range raw {
			if m, ok := n.(map[string]interface{}); ok {
				if tn, _ := m["__typename"].(string); allowed[tn] {
					kept = append(kept, m)
				}
			}
		}
	}
	return map[string]interface{}{
		"nodes":      kept,
		"totalCount": len(kept),
		"pageInfo":   conn["pageInfo"],
	}, nil
}

// --- PullRequestReview fields ------------------------------------------------

func (s *Resolver) addPullRequestReviewFields(d prSurfaceDeps, cannotUpdateEnum *graphql.Enum) {
	rv := d.review
	dateTime := d.dateTime
	uri := d.uri

	rv.AddFieldConfig("bodyHTML", &graphql.Field{
		Type: graphql.NewNonNull(d.htmlScalar),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			b, _ := srcMap(p)["body"].(string)
			return discussionBodyToHTML(b), nil
		},
	})
	rv.AddFieldConfig("bodyText", &graphql.Field{
		Type: graphql.NewNonNull(graphql.String),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			b, _ := srcMap(p)["body"].(string)
			return bodyText(b), nil
		},
	})
	rv.AddFieldConfig("publishedAt", &graphql.Field{
		Type:    dateTime,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return srcMap(p)["submittedAt"], nil },
	})
	rv.AddFieldConfig("resourcePath", &graphql.Field{Type: graphql.NewNonNull(uri)})
	rv.AddFieldConfig("url", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			rp, _ := srcMap(p)["resourcePath"].(string)
			return externalURL(rp), nil
		},
	})
	rv.AddFieldConfig("fullDatabaseId", &graphql.Field{
		Type: d.bigInt,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id, _ := srcMap(p)["_dbID"].(int)
			return strconv.Itoa(id), nil
		},
	})
	rv.AddFieldConfig("createdViaEmail", constBoolField(false))
	rv.AddFieldConfig("includesCreatedEdit", constBoolField(false))
	rv.AddFieldConfig("isMinimized", constBoolField(false))
	rv.AddFieldConfig("lastEditedAt", &graphql.Field{Type: dateTime})
	rv.AddFieldConfig("minimizedReason", &graphql.Field{Type: graphql.String})
	rv.AddFieldConfig("editor", &graphql.Field{
		Type:    s.graphqlTypes.actor,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	rv.AddFieldConfig("userContentEdits", &graphql.Field{
		Type:    s.gqlUserContentEditConnectionType(),
		Args:    relayConnectionArgs(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return emptyUserContentEditConnection(), nil },
	})

	// authorCanPushToRepository from the author's stored association, as REST:
	// OWNER/MEMBER/COLLABORATOR can push.
	rv.AddFieldConfig("authorCanPushToRepository", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			assoc, _ := srcMap(p)["authorAssociation"].(string)
			return assoc == "OWNER" || assoc == "MEMBER" || assoc == "COLLABORATOR", nil
		},
	})

	// A review is never on behalf of a team here; empty connection.
	rv.AddFieldConfig("onBehalfOf", &graphql.Field{
		Type:    graphql.NewNonNull(s.gqlTeamConnectionType()),
		Args:    relayConnectionArgs(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return prEmptyConnection(), nil },
	})

	rv.AddFieldConfig("comments", &graphql.Field{
		Type:    graphql.NewNonNull(d.reviewCommentConn),
		Args:    relayConnectionArgs(),
		Resolve: s.resolveReviewComments,
	})

	// Resolve lazily so a review map never embeds a whole PR (and recurses).
	rv.AddFieldConfig("pullRequest", &graphql.Field{
		Type:    graphql.NewNonNull(d.pullRequest),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return s.prFromID(srcMap(p)["_prID"]) },
	})
	rv.AddFieldConfig("repository", &graphql.Field{
		Type:    graphql.NewNonNull(d.repoType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return s.repoFromID(srcMap(p)["repoID"]) },
	})

	rv.AddFieldConfig("viewerDidAuthor", s.prBoolField(func(v *store.User, authorID int, _, _, _ bool) bool {
		return v != nil && v.ID == authorID
	}))
	rv.AddFieldConfig("viewerCanUpdate", s.prBoolField(func(v *store.User, authorID int, _, _, _ bool) bool {
		return v != nil && v.ID == authorID
	}))
	rv.AddFieldConfig("viewerCanDelete", s.prBoolField(func(v *store.User, authorID int, _, _, admin bool) bool {
		return v != nil && (v.ID == authorID || admin)
	}))
	rv.AddFieldConfig("viewerCanMinimize", s.prBoolField(func(v *store.User, authorID int, _, write, _ bool) bool {
		return v != nil && (write || v.ID == authorID)
	}))
	rv.AddFieldConfig("viewerCanUnminimize", s.prBoolField(func(v *store.User, authorID int, _, write, _ bool) bool {
		return v != nil && (write || v.ID == authorID)
	}))
	rv.AddFieldConfig("viewerCannotUpdateReasons", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(cannotUpdateEnum))),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			v, authorID, _, _, _ := s.prRepoPerms(p)
			return prCannotUpdateReasons(v != nil && v.ID == authorID, v), nil
		},
	})
}

func (s *Resolver) resolveReviewComments(p graphql.ResolveParams) (interface{}, error) {
	src := srcMap(p)
	prID, _ := src["_prID"].(int)
	reviewID, _ := src["_dbID"].(int)
	comments := s.store.PRReviewComments.ListForPR(prID)
	nodes := make([]map[string]interface{}, 0, len(comments))
	for _, c := range comments {
		if c.ReviewID != reviewID {
			continue
		}
		nodes = append(nodes, prReviewCommentToGQL(c, s.store))
	}
	return repaginateConnection(map[string]interface{}{"nodes": nodes, "totalCount": len(nodes)}, p.Args), nil
}

// --- PullRequestReviewThread fields ------------------------------------------

func (s *Resolver) addPullRequestReviewThreadFields(d prSurfaceDeps, diffSideEnum, subjectTypeEnum *graphql.Enum) {
	th := d.thread

	th.AddFieldConfig("diffSide", &graphql.Field{Type: graphql.NewNonNull(diffSideEnum)})
	th.AddFieldConfig("startDiffSide", &graphql.Field{Type: diffSideEnum})
	th.AddFieldConfig("subjectType", &graphql.Field{Type: graphql.NewNonNull(subjectTypeEnum)})
	th.AddFieldConfig("isCollapsed", &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)})
	th.AddFieldConfig("originalLine", &graphql.Field{Type: graphql.Int})
	th.AddFieldConfig("originalStartLine", &graphql.Field{Type: graphql.Int})
	th.AddFieldConfig("startLine", &graphql.Field{Type: graphql.Int})

	th.AddFieldConfig("pullRequest", &graphql.Field{
		Type:    graphql.NewNonNull(d.pullRequest),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return s.prFromID(srcMap(p)["prID"]) },
	})
	th.AddFieldConfig("repository", &graphql.Field{
		Type:    graphql.NewNonNull(d.repoType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return s.repoFromID(srcMap(p)["repoID"]) },
	})

	th.AddFieldConfig("viewerCanReply", s.prBoolField(func(v *store.User, _ int, read, _, _ bool) bool { return v != nil && read }))
	th.AddFieldConfig("viewerCanResolve", s.prBoolField(func(v *store.User, authorID int, _, write, _ bool) bool {
		return v != nil && (write || v.ID == authorID)
	}))
	th.AddFieldConfig("viewerCanUnresolve", s.prBoolField(func(v *store.User, authorID int, _, write, _ bool) bool {
		return v != nil && (write || v.ID == authorID)
	}))
}

// --- shared lazy lookups -----------------------------------------------------

func (s *Resolver) prFromID(raw interface{}) (interface{}, error) {
	id, _ := raw.(int)
	pr := s.store.GetPullRequest(id)
	if pr == nil {
		return nil, fmt.Errorf("pull request %d not found", id)
	}
	return pullRequestToGQL(pr, s.store), nil
}

func (s *Resolver) repoFromID(raw interface{}) (interface{}, error) {
	id, _ := raw.(int)
	repo := s.store.GetRepoByID(id)
	if repo == nil {
		return nil, fmt.Errorf("repository %d not found", id)
	}
	return repoToGraphQL(s.store, repo), nil
}

// prParticipantsLocked builds the deduplicated set of users involved in a PR:
// author, assignees, requested reviewers, review authors and comment authors.
// Caller must hold st.Mu.RLock.
func prParticipantsLocked(pr *store.PullRequest, st *store.Store) []map[string]interface{} {
	seen := map[int]bool{}
	nodes := []map[string]interface{}{}
	add := func(id int) {
		if id == 0 || seen[id] {
			return
		}
		if u := st.Users[id]; u != nil {
			seen[id] = true
			nodes = append(nodes, userToGraphQL(u))
		}
	}
	add(pr.AuthorID)
	for _, id := range pr.AssigneeIDs {
		add(id)
	}
	for _, id := range pr.RequestedReviewerIDs {
		add(id)
	}
	for _, r := range st.PRReviewsByPR[pr.ID] {
		add(r.AuthorID)
	}
	for _, c := range st.Comments {
		if c.ParentType == "pull_request" && c.IssueID == pr.ID {
			add(c.AuthorID)
		}
	}
	return nodes
}

// prReviewThreadCommentCount sums comments across rendered review threads, for
// PullRequest.totalCommentsCount.
func prReviewThreadCommentCount(threads []map[string]interface{}) int {
	total := 0
	for _, t := range threads {
		if conn, ok := t["comments"].(map[string]interface{}); ok {
			if n, ok := conn["totalCount"].(int); ok {
				total += n
			}
		}
	}
	return total
}

// --- supporting types --------------------------------------------------------

func (s *Resolver) prAssigneeConnectionType(userType *graphql.Object) *graphql.Object {
	// AssigneeConnection is shared with Issue via sharedAssigneeConnectionType.
	return s.sharedAssigneeConnectionType(userType)
}

func (s *Resolver) prSuggestedReviewerType(userType *graphql.Object) *graphql.Object {
	return graphql.NewObject(graphql.ObjectConfig{
		Name: "SuggestedReviewer",
		Fields: graphql.Fields{
			"isAuthor":    &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isCommenter": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"reviewer":    &graphql.Field{Type: graphql.NewNonNull(userType)},
		},
	})
}

func (s *Resolver) prSuggestedReviewerActorConnectionType() *graphql.Object {
	actor := graphql.NewObject(graphql.ObjectConfig{
		Name: "SuggestedReviewerActor",
		Fields: graphql.Fields{
			"isAuthor":    &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"isCommenter": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"reviewer":    &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.actor)},
		},
	})
	edge := graphql.NewObject(graphql.ObjectConfig{
		Name: "SuggestedReviewerActorEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: actor},
		},
	})
	return graphql.NewObject(graphql.ObjectConfig{
		Name: "SuggestedReviewerActorConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(edge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(actor)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
}

func (s *Resolver) prHovercardType() *graphql.Object {
	// Hovercard is shared with Issue via sharedHovercardType.
	return s.sharedHovercardType()
}

func (s *Resolver) prStackTypes() (*graphql.Object, *graphql.Object) {
	stack := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestStack",
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"number":      &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"size":        &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"baseRefName": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	entry := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestStackEntry",
		Fields: graphql.Fields{
			"id":       &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"position": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			// No stacked-PR graph is modelled; both are nullable and answer null.
			"pullRequest": &graphql.Field{
				Type:    s.graphqlTypes.pullRequest,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) { return srcMap(p)["pullRequest"], nil },
			},
			"stack": &graphql.Field{
				Type:    stack,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) { return srcMap(p)["stack"], nil },
			},
		},
	})
	stackEntryConnection := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestStackEntryConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(entry)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"edges":      gqlEdgesField(s.simpleEdgeType("PullRequestStackEntryEdge", entry)),
			"pageInfo":   s.gqlPageInfoField(),
		},
	})
	// No stacked-PR graph, so empty.
	stack.AddFieldConfig("entries", &graphql.Field{
		Type: graphql.NewNonNull(stackEntryConnection),
		Args: relayConnectionArgs(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			return prEmptyConnection(), nil
		},
	})
	return stack, entry
}

func prTimelineArgs(dateTime *graphql.Scalar) graphql.FieldConfigArgument {
	args := relayConnectionArgs()
	args["since"] = &graphql.ArgumentConfig{Type: dateTime}
	return args
}

func (s *Resolver) prTimelineConnectionType(d prSurfaceDeps) *graphql.Object {
	// The deprecated PullRequestTimelineItem union, listing only the members the
	// live timeline emits and this alias serves.
	item := graphql.NewUnion(graphql.UnionConfig{
		Name:  "PullRequestTimelineItem",
		Types: []*graphql.Object{d.review, d.thread, s.graphqlTypes.issueComment},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			m, _ := p.Value.(map[string]interface{})
			switch m["__typename"] {
			case "PullRequestReviewThread":
				return d.thread
			case "IssueComment":
				return s.graphqlTypes.issueComment
			default:
				return d.review
			}
		},
	})
	edge := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestTimelineItemEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: item},
		},
	})
	return graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestTimelineConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(edge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(item)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
}

// projectClassicCardConnectionType returns the shared ProjectCardConnection the
// classic-project surface mints.
func (s *Resolver) projectClassicCardConnectionType() *graphql.Object {
	conn, _ := s.projectClassicConnectionPair("ProjectCard", s.projectClassicCardType())
	return conn
}
