package graphqlapi

import (
	"html"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// gh_issue_fields_graphql.go completes GitHub's field surface on the
// conversation and metadata object types — Issue, Discussion, DiscussionComment,
// Milestone, Label — and the small reaction/metadata types. Every field is
// backed by the real store where the data exists (labels, milestones,
// participants, linked branches, reactions, discussion answers, duplicate/
// dependency links) and answers GitHub's zero value (false / empty connection /
// null) only where bleephub genuinely does not model the data. The shared
// renderers (issueToGQL, labelToGQL, milestoneToGQL, discussionCommentToGQL)
// carry the store ids these resolvers read; nothing here fabricates data.
//
// The types are enriched from one entry point, enrichConversationTypes, called
// late in initGraphQLSchema so every type these fields name — ProjectV2, the
// timeline family, IssueComment, Repository — is already assembled.

// enrichConversationTypes adds the remaining GitHub fields to the conversation
// and metadata types. It runs after every family it references is built.
func (s *Resolver) enrichConversationTypes(userType, repoType *graphql.Object) {
	s.enrichIssueType(userType)
	s.enrichMilestoneType()
	s.enrichLabelType()
	s.enrichDiscussionType()
}

// --- shared small helpers --------------------------------------------------

// emptyGQLConnection is a well-formed empty Relay connection: the truthful
// answer for a connection whose subject exists but has no members.
func emptyGQLConnection() map[string]interface{} {
	return map[string]interface{}{
		"nodes":      []map[string]interface{}{},
		"edges":      []map[string]interface{}{},
		"totalCount": 0,
		"pageInfo": map[string]interface{}{
			"hasNextPage":     false,
			"hasPreviousPage": false,
			"startCursor":     nil,
			"endCursor":       nil,
		},
	}
}

func srcInt(src map[string]interface{}, key string) int {
	v, _ := src[key].(int)
	return v
}

func srcStr(src map[string]interface{}, key string) string {
	v, _ := src[key].(string)
	return v
}

// commentCannotUpdateReasonEnum is GitHub's CommentCannotUpdateReason. The
// viewerCannotUpdateReasons fields resolve to a subset of these.
func (s *Resolver) commentCannotUpdateReasonEnum() *graphql.Enum {
	return s.graphQLEnum("CommentCannotUpdateReason",
		"ARCHIVED", "DENIED", "INSUFFICIENT_ACCESS", "LOCKED",
		"LOGIN_REQUIRED", "MAINTENANCE", "VERIFIED_EMAIL_REQUIRED")
}

// sharedAssigneeConnectionType returns the AssigneeConnection type (memoized),
// shared by Issue and PullRequest. Its node union carries the User type
// (bleephub assigns only users).
// sharedAssigneeUnion returns the Assignee union (memoized). GitHub's Assignee
// is Bot|Mannequin|Organization|User; bleephub assigns users and organizations,
// the two concrete types it models. The timeline's AssignedEvent.assignee and
// the Assignable connection both name this one union.
func (s *Resolver) sharedAssigneeUnion() *graphql.Union {
	if s.graphqlTypes.assignee != nil {
		return s.graphqlTypes.assignee
	}
	s.graphqlTypes.assignee = graphql.NewUnion(graphql.UnionConfig{
		Name:  "Assignee",
		Types: []*graphql.Object{s.graphqlTypes.user, s.graphqlTypes.organization},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name == "Organization" {
				return s.graphqlTypes.organization
			}
			return s.graphqlTypes.user
		},
	})
	return s.graphqlTypes.assignee
}

func (s *Resolver) sharedAssigneeConnectionType(userType *graphql.Object) *graphql.Object {
	if s.graphqlTypes.assigneeConnection != nil {
		return s.graphqlTypes.assigneeConnection
	}
	assigneeUnion := s.sharedAssigneeUnion()
	edge := graphql.NewObject(graphql.ObjectConfig{
		Name: "AssigneeEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: assigneeUnion},
		},
	})
	s.graphqlTypes.assigneeConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "AssigneeConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(edge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(assigneeUnion)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	return s.graphqlTypes.assigneeConnection
}

// sharedHovercardType returns the Hovercard type (memoized), shared by Issue and
// PullRequest. bleephub computes no relationship contexts, so its HovercardContext
// interface carries no implementors and the contexts list is always empty.
func (s *Resolver) sharedHovercardType() *graphql.Object {
	if s.graphqlTypes.hovercard != nil {
		return s.graphqlTypes.hovercard
	}
	contextIface := graphql.NewInterface(graphql.InterfaceConfig{
		Name: "HovercardContext",
		Fields: graphql.Fields{
			"message": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"octicon": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
		ResolveType: func(graphql.ResolveTypeParams) *graphql.Object { return nil },
	})
	s.graphqlTypes.hovercard = graphql.NewObject(graphql.ObjectConfig{
		Name: "Hovercard",
		Fields: graphql.Fields{
			"contexts": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(contextIface))),
				Resolve: func(graphql.ResolveParams) (interface{}, error) { return []interface{}{}, nil },
			},
		},
	})
	return s.graphqlTypes.hovercard
}

// cannotUpdateReasons is the real reason list for a viewer that cannot update a
// subject: empty when they can, LOGIN_REQUIRED when anonymous, else
// INSUFFICIENT_ACCESS.
func cannotUpdateReasons(viewer *store.User, canUpdate bool) []interface{} {
	if canUpdate {
		return []interface{}{}
	}
	if viewer == nil {
		return []interface{}{"LOGIN_REQUIRED"}
	}
	return []interface{}{"INSUFFICIENT_ACCESS"}
}

// --- Issue -----------------------------------------------------------------

func (s *Resolver) enrichIssueType(userType *graphql.Object) {
	issueType := s.graphqlTypes.issue
	uri := s.graphQLStringScalar("URI")
	htmlScalar := s.graphQLStringScalar("HTML")
	dateTime := s.graphQLStringScalar("DateTime")
	bigInt := s.graphQLStringScalar("BigInt")
	actor := s.graphqlTypes.actor
	issueConn := s.graphqlTypes.issueConnection
	userConn := s.graphqlTypes.userConnection
	authorAssocEnum := s.graphQLEnum("CommentAuthorAssociation",
		"COLLABORATOR", "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR",
		"MANNEQUIN", "MEMBER", "NONE", "OWNER")

	// Assignee/Hovercard are shared with PullRequest; use the memoized builders
	// so the schema holds one instance of each.
	assigneeConn := s.sharedAssigneeConnectionType(userType)
	hovercardType := s.sharedHovercardType()

	// IssueDependenciesSummary — bleephub does not model issue dependencies, so
	// the counts are a truthful zero.
	depsSummary := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueDependenciesSummary",
		Fields: graphql.Fields{
			"blockedBy":      &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"blocking":       &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"totalBlockedBy": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"totalBlocking":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	// PinnedIssueComment — the type must exist because Issue.pinnedIssueComment
	// names it; bleephub does not pin issue comments, so the field resolves null.
	pinnedIssueComment := graphql.NewObject(graphql.ObjectConfig{
		Name: "PinnedIssueComment",
		Fields: graphql.Fields{
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"databaseId":     &graphql.Field{Type: graphql.Int},
			"fullDatabaseId": &graphql.Field{Type: bigInt},
			"issue":          &graphql.Field{Type: graphql.NewNonNull(issueType)},
			"issueComment":   &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.issueComment)},
			"pinnedAt":       &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"pinnedBy":       &graphql.Field{Type: graphql.NewNonNull(actor)},
		},
	})

	// PendingIssueSuggestion union + one member. bleephub has no pending
	// suggestions, so Issue.pendingSuggestions resolves null; the union must
	// still be a valid type.
	pendingLabelSuggestion := graphql.NewObject(graphql.ObjectConfig{
		Name: "PendingLabelSuggestion",
		Fields: graphql.Fields{
			"actor":     &graphql.Field{Type: actor},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"label":     &graphql.Field{Type: s.graphqlTypes.labelType},
			"rationale": &graphql.Field{Type: graphql.String},
			"updatedAt": &graphql.Field{Type: dateTime},
		},
	})
	pendingSuggestionUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "PendingIssueSuggestion",
		Types: []*graphql.Object{pendingLabelSuggestion},
		ResolveType: func(graphql.ResolveTypeParams) *graphql.Object {
			return pendingLabelSuggestion
		},
	})

	// projectCards uses the classic ProjectCardConnection the classic-project
	// surface already mints (bleephub uses ProjectsV2, so the connection is
	// empty). Sharing that type keeps one ProjectCardConnection in the schema.
	projectCardConn := s.projectClassicCardConnectionType()

	// The deprecated timeline connection. Its union names a single existing
	// member; the connection resolves empty (the modern field is timelineItems).
	timelineItem := graphql.NewUnion(graphql.UnionConfig{
		Name:  "IssueTimelineItem",
		Types: []*graphql.Object{s.graphqlTypes.issueComment},
		ResolveType: func(graphql.ResolveTypeParams) *graphql.Object {
			return s.graphqlTypes.issueComment
		},
	})
	timelineItemEdge := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueTimelineItemEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: timelineItem},
		},
	})
	timelineConn := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueTimelineConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(timelineItemEdge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(timelineItem)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	subState := s.graphQLEnum("SubscriptionState", "IGNORED", "SUBSCRIBED", "UNSUBSCRIBED")
	threadSubState := s.graphQLEnum("ThreadSubscriptionState",
		"DISABLED", "IGNORING_LIST", "IGNORING_THREAD", "NONE",
		"SUBSCRIBED_TO_LIST", "SUBSCRIBED_TO_THREAD", "SUBSCRIBED_TO_THREAD_EVENTS",
		"SUBSCRIBED_TO_THREAD_TYPE", "UNAVAILABLE")
	threadFormAction := s.graphQLEnum("ThreadSubscriptionFormAction", "NONE", "SUBSCRIBE", "UNSUBSCRIBE")

	// permission closures shared by the viewerCan* family.
	viewer := func(p graphql.ResolveParams) *store.User { return s.ghUserFromContext(p.Context) }
	repoOf := func(src map[string]interface{}) *store.Repo {
		return s.store.GetRepoByID(srcInt(src, "repoID"))
	}
	canWrite := func(p graphql.ResolveParams, src map[string]interface{}) bool {
		return s.viewerMayActOnRepo(p.Context, repoOf(src), store.ScopeIssues, store.PermWrite, store.PermAdmin)
	}
	didAuthor := func(p graphql.ResolveParams, src map[string]interface{}) bool {
		v := viewer(p)
		return v != nil && srcInt(src, "authorID") == v.ID
	}
	canUpdate := func(p graphql.ResolveParams, src map[string]interface{}) bool {
		return didAuthor(p, src) || canWrite(p, src)
	}

	boolField := func(fn func(p graphql.ResolveParams, src map[string]interface{}) bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return fn(p, src), nil
			},
		}
	}
	nullableBoolField := func(fn func(p graphql.ResolveParams, src map[string]interface{}) bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.Boolean,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return fn(p, src), nil
			},
		}
	}

	fields := graphql.Fields{
		"authorAssociation": &graphql.Field{
			Type: graphql.NewNonNull(authorAssocEnum),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				s.store.Mu.RLock()
				defer s.store.Mu.RUnlock()
				return authorAssociationForRepoLocked(s.store, srcInt(src, "repoID"), srcInt(src, "authorID")), nil
			},
		},
		"bodyHTML": &graphql.Field{
			Type: graphql.NewNonNull(htmlScalar),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return discussionBodyToHTML(srcStr(src, "body")), nil
			},
		},
		"bodyText": &graphql.Field{
			Type: graphql.NewNonNull(graphql.String),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return bodyText(srcStr(src, "body")), nil
			},
		},
		"bodyResourcePath": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return srcStr(src, "resourcePath"), nil
			},
		},
		"bodyUrl": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return srcStr(src, "url"), nil
			},
		},
		"titleHTML": &graphql.Field{
			Type: graphql.NewNonNull(graphql.String),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return html.EscapeString(srcStr(src, "title")), nil
			},
		},
		"createdViaEmail":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: falseResolver},
		"includesCreatedEdit": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: falseResolver},
		// bleephub records no per-issue edit history, so editor/lastEditedAt are
		// null and userContentEdits is a real empty connection.
		"editor":       &graphql.Field{Type: actor, Resolve: nilResolver},
		"lastEditedAt": &graphql.Field{Type: dateTime, Resolve: nilResolver},
		"publishedAt": &graphql.Field{
			Type: dateTime,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return src["createdAt"], nil
			},
		},
		"fullDatabaseId": &graphql.Field{
			Type: bigInt,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return strconv.Itoa(srcInt(src, "databaseId")), nil
			},
		},
		"resourcePath": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return srcStr(src, "resourcePath"), nil
			},
		},
		"isReadByViewer":                     &graphql.Field{Type: graphql.Boolean, Resolve: nilResolver},
		"viewerSubscription":                 &graphql.Field{Type: subState, Resolve: nilResolver},
		"viewerThreadSubscriptionStatus":     &graphql.Field{Type: threadSubState, Resolve: nilResolver},
		"viewerThreadSubscriptionFormAction": &graphql.Field{Type: threadFormAction, Resolve: nilResolver},
		"hovercard": &graphql.Field{
			Type: graphql.NewNonNull(hovercardType),
			Args: graphql.FieldConfigArgument{
				"includeNotificationContexts": &graphql.ArgumentConfig{Type: graphql.Boolean},
			},
			Resolve: func(graphql.ResolveParams) (interface{}, error) {
				return map[string]interface{}{}, nil
			},
		},
		"issueDependenciesSummary": &graphql.Field{
			Type: graphql.NewNonNull(depsSummary),
			Resolve: func(graphql.ResolveParams) (interface{}, error) {
				return map[string]interface{}{
					"blockedBy": 0, "blocking": 0, "totalBlockedBy": 0, "totalBlocking": 0,
				}, nil
			},
		},
		"eventRationales": &graphql.Field{
			Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(s.graphqlTypes.issueEventRationale))),
			Resolve: func(graphql.ResolveParams) (interface{}, error) {
				return []interface{}{}, nil
			},
		},
		"pendingSuggestions": &graphql.Field{
			Type:    graphql.NewList(graphql.NewNonNull(pendingSuggestionUnion)),
			Resolve: nilResolver,
		},
		"pinnedIssueComment": &graphql.Field{Type: pinnedIssueComment, Resolve: nilResolver},
		"duplicateOf": &graphql.Field{
			Type: issueType,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				dup := srcInt(src, "duplicateOfID")
				if dup == 0 {
					return nil, nil
				}
				return optionalObject(rendered(s.store.GetIssue(dup), func(i *store.Issue) map[string]interface{} {
					return issueToGQL(i, s.store)
				})), nil
			},
		},
		"participants": &graphql.Field{
			Type: graphql.NewNonNull(userConn),
			Args: relayConnectionArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return paginateGQLMaps(s.issueParticipants(srcInt(src, "databaseId")), p.Args), nil
			},
		},
		"assignedActors": &graphql.Field{
			Type: graphql.NewNonNull(assigneeConn),
			Args: relayConnectionArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return paginateGQLMaps(assigneeNodesFromSource(src), p.Args), nil
			},
		},
		"suggestedActors": &graphql.Field{
			Type: graphql.NewNonNull(assigneeConn),
			Args: withArg(relayConnectionArgs(), "query", graphql.String),
			Resolve: func(graphql.ResolveParams) (interface{}, error) {
				return emptyGQLConnection(), nil
			},
		},
		"blockedBy":       emptyIssueConnField(issueConn),
		"blocking":        emptyIssueConnField(issueConn),
		"trackedIssues":   emptyIssueConnField(issueConn),
		"trackedInIssues": emptyIssueConnField(issueConn),
		"trackedIssuesCount": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Int),
			Args: graphql.FieldConfigArgument{
				"states": &graphql.ArgumentConfig{Type: graphql.NewList(s.graphQLEnum("TrackedIssueStates", "CLOSED", "OPEN"))},
			},
			Resolve: func(graphql.ResolveParams) (interface{}, error) { return 0, nil },
		},
		"timeline": &graphql.Field{
			Type: graphql.NewNonNull(timelineConn),
			Args: withArg(relayConnectionArgs(), "since", dateTime),
			Resolve: func(graphql.ResolveParams) (interface{}, error) {
				return emptyGQLConnection(), nil
			},
		},
		"projectCards": &graphql.Field{
			Type: graphql.NewNonNull(projectCardConn),
			Args: withArg(relayConnectionArgs(), "archivedStates",
				graphql.NewList(s.graphQLEnum("ProjectCardArchivedState", "ARCHIVED", "NOT_ARCHIVED"))),
			Resolve: func(graphql.ResolveParams) (interface{}, error) {
				return emptyGQLConnection(), nil
			},
		},
		"userContentEdits": &graphql.Field{
			Type: s.gqlUserContentEditConnectionType(),
			Args: relayConnectionArgs(),
			Resolve: func(graphql.ResolveParams) (interface{}, error) {
				return emptyUserContentEditConnection(), nil
			},
		},
		"viewerCanUpdate":       boolField(canUpdate),
		"viewerCanClose":        boolField(canWrite),
		"viewerCanReopen":       boolField(canWrite),
		"viewerCanLabel":        boolField(canWrite),
		"viewerCanAssign":       boolField(canWrite),
		"viewerCanSetMilestone": boolField(canWrite),
		// GitHub declares these three as nullable Boolean.
		"viewerCanType":           nullableBoolField(canWrite),
		"viewerCanSetFields":      nullableBoolField(canWrite),
		"viewerCanUpdateMetadata": nullableBoolField(canWrite),
		"viewerCanDelete": boolField(func(p graphql.ResolveParams, src map[string]interface{}) bool {
			return s.viewerCanAdminRepo(p.Context, repoOf(src))
		}),
		"viewerCanSubscribe": boolField(func(p graphql.ResolveParams, src map[string]interface{}) bool {
			repo := repoOf(src)
			return viewer(p) != nil && repo != nil && s.viewerCanReadRepo(p.Context, repo)
		}),
		"viewerDidAuthor": boolField(didAuthor),
		"viewerCannotUpdateReasons": &graphql.Field{
			Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(s.commentCannotUpdateReasonEnum()))),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return cannotUpdateReasons(viewer(p), canUpdate(p, src)), nil
			},
		},
	}
	for name, f := range fields {
		issueType.AddFieldConfig(name, f)
	}

	// projectsV2 / projectV2 reach the issue's owning account's projects, the
	// same set the repository's Projects tab lists. Reuse the ProjectV2 owner
	// machinery so these resolve real projects.
	s.addIssueProjectV2Fields(issueType)
}

// addIssueProjectV2Fields wires Issue.projectsV2 and Issue.projectV2 through the
// issue's repository owner, matching how Repository.projectsV2 resolves.
func (s *Resolver) addIssueProjectV2Fields(issueType *graphql.Object) {
	projectType := s.projectV2GraphQLTypes()
	connection := s.gqlConnectionType("ProjectV2", projectType)
	owner := func(src map[string]interface{}) (int, string, bool) {
		repo := s.store.GetRepoByID(srcInt(src, "repoID"))
		if repo == nil {
			return 0, "", false
		}
		if org := s.store.GetOrgByID(repo.OwnerID); org != nil {
			return org.ID, "Organization", true
		}
		return repo.OwnerID, "User", true
	}
	issueType.AddFieldConfig("projectsV2", &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: s.projectV2ConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, _ := p.Source.(map[string]interface{})
			ownerID, ownerType, ok := owner(src)
			if !ok {
				return emptyGQLConnection(), nil
			}
			return s.projectV2Connection(p, ownerID, ownerType), nil
		},
	})
	issueType.AddFieldConfig("projectV2", &graphql.Field{
		Type: projectType,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, _ := p.Source.(map[string]interface{})
			ownerID, ownerType, ok := owner(src)
			if !ok {
				return nil, nil
			}
			number, _ := p.Args["number"].(int)
			return optionalObject(s.projectV2ByNumber(p, ownerID, ownerType, number)), nil
		},
	})
}

// issueParticipants renders the users participating in an issue conversation:
// the author, the assignees, and every comment author, in first-seen order and
// deduplicated — GitHub's participants set.
func (s *Resolver) issueParticipants(issueID int) []map[string]interface{} {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	issue := s.store.Issues[issueID]
	if issue == nil {
		return []map[string]interface{}{}
	}
	seen := map[int]bool{}
	nodes := []map[string]interface{}{}
	add := func(userID int) {
		if userID == 0 || seen[userID] {
			return
		}
		if u := s.store.Users[userID]; u != nil {
			seen[userID] = true
			nodes = append(nodes, userToGraphQL(u))
		}
	}
	add(issue.AuthorID)
	for _, c := range s.store.CommentsByParent[store.CommentCountKey("issue", issue.ID)] {
		add(c.AuthorID)
	}
	for _, aid := range issue.AssigneeIDs {
		add(aid)
	}
	return nodes
}

// assigneeNodesFromSource extracts the already-rendered assignee user maps the
// issue source carries under "assignees".
func assigneeNodesFromSource(src map[string]interface{}) []map[string]interface{} {
	conn, _ := src["assignees"].(map[string]interface{})
	nodes, _ := conn["nodes"].([]map[string]interface{})
	return nodes
}

func emptyIssueConnField(issueConn *graphql.Object) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(issueConn),
		Args: relayConnectionArgs(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			return emptyGQLConnection(), nil
		},
	}
}

func withArg(args graphql.FieldConfigArgument, name string, t graphql.Input) graphql.FieldConfigArgument {
	args[name] = &graphql.ArgumentConfig{Type: t}
	return args
}

func falseResolver(graphql.ResolveParams) (interface{}, error) { return false, nil }
func nilResolver(graphql.ResolveParams) (interface{}, error)   { return nil, nil }

// rendered renders a possibly-nil store record with a renderer that
// dereferences it, or returns a nil map when the record is absent.
func rendered[T any](record *T, render func(*T) map[string]interface{}) map[string]interface{} {
	if record == nil {
		return nil
	}
	return render(record)
}
