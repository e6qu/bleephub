package graphqlapi

import (
	"fmt"

	"github.com/graphql-go/graphql"
)

// GitHub's issue and pull-request timeline type graph: the object type per
// event kind, the two unions and their connections, plus the support types the
// members name. gh_timeline_graphql.go renders the sources.
//
// Members whose subject bleephub does not model (deployments, merge queues,
// projects classic, mannequins, duplicate marking) are deliberately not
// declared, so a client cannot select a member that would silently return nothing.

// timelineTypeRegistry memoizes the timeline type graph. It is built once, in
// addTimelineFieldsToSchema, after every type its members name exists.
type timelineTypeRegistry struct {
	closable        *graphql.Interface
	assignable      *graphql.Interface
	projectV2Event  *graphql.Interface
	closer          *graphql.Union
	assignee        *graphql.Union
	milestoneItem   *graphql.Union
	renamedSubject  *graphql.Union
	referencedSubj  *graphql.Union
	issueUnion      *graphql.Union
	pullUnion       *graphql.Union
	issueConn       *graphql.Object
	pullConn        *graphql.Object
	issueItemType   *graphql.Enum
	pullItemType    *graphql.Enum
	updateIntent    *graphql.Object
	eventRationale  *graphql.Object
	byName          map[string]*graphql.Object
	issueMemberSet  map[string]bool
	pullMemberSet   map[string]bool
	issueItemTypeOf map[string]string
	pullItemTypeOf  map[string]string
}

// gqlClosableInterface is GitHub's Closable, narrowed to the subset bleephub
// models on Issue and PullRequest.
func (s *Resolver) gqlClosableInterface() *graphql.Interface {
	if s.graphqlTypes.timeline == nil {
		s.graphqlTypes.timeline = &timelineTypeRegistry{}
	}
	if s.graphqlTypes.timeline.closable != nil {
		return s.graphqlTypes.timeline.closable
	}
	s.graphqlTypes.timeline.closable = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Closable",
		Fields: graphql.Fields{
			"closed":          &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"closedAt":        &graphql.Field{Type: s.graphQLStringScalar("DateTime")},
			"viewerCanClose":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"viewerCanReopen": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			return s.issueOrPullRequestObject(p.Value)
		},
	})
	return s.graphqlTypes.timeline.closable
}

// gqlAssignableInterface is GitHub's Assignable, narrowed to the assignee
// connection bleephub models.
func (s *Resolver) gqlAssignableInterface() *graphql.Interface {
	if s.graphqlTypes.timeline == nil {
		s.graphqlTypes.timeline = &timelineTypeRegistry{}
	}
	if s.graphqlTypes.timeline.assignable != nil {
		return s.graphqlTypes.timeline.assignable
	}
	s.graphqlTypes.timeline.assignable = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Assignable",
		Fields: graphql.Fields{
			"assignees": &graphql.Field{
				Type: graphql.NewNonNull(s.gqlUserConnectionType(s.graphqlTypes.user)),
				Args: relayConnectionArgs(),
			},
			"viewerCanAssign": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			return s.issueOrPullRequestObject(p.Value)
		},
	})
	return s.graphqlTypes.timeline.assignable
}

// issueOrPullRequestObject dispatches an Issue/PullRequest source map to its
// object type by node-id prefix.
func (s *Resolver) issueOrPullRequestObject(value interface{}) *graphql.Object {
	source, _ := value.(map[string]interface{})
	if name, _ := source["__typename"].(string); name == "PullRequest" {
		return s.graphqlTypes.pullRequest
	}
	if nodeID, _ := source["nodeID"].(string); len(nodeID) >= 3 && nodeID[:3] == "PR_" {
		return s.graphqlTypes.pullRequest
	}
	return s.graphqlTypes.issue
}

// addTimelineFieldsToSchema assembles the timeline type graph and installs
// Issue.timelineItems and PullRequest.timelineItems. It runs after the issue,
// pull-request, discussion and Projects v2 families because the union members
// name types from all four.
func (s *Resolver) addTimelineFieldsToSchema(nodeInterface *graphql.Interface, nodeTypes map[string]*graphql.Object) {
	if s.graphqlTypes.timeline == nil {
		s.graphqlTypes.timeline = &timelineTypeRegistry{}
	}
	reg := s.graphqlTypes.timeline
	reg.byName = map[string]*graphql.Object{}

	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	urlLocatable := s.uniformResourceLocatableInterface()
	issueType := s.graphqlTypes.issue
	pullType := s.graphqlTypes.pullRequest

	reg.closer = graphql.NewUnion(graphql.UnionConfig{
		Name:  "Closer",
		Types: []*graphql.Object{s.graphqlTypes.commit, s.graphqlTypes.projectV2Type, pullType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			switch name, _ := source["__typename"].(string); name {
			case "Commit":
				return s.graphqlTypes.commit
			case "ProjectV2":
				return s.graphqlTypes.projectV2Type
			}
			return pullType
		},
	})
	reg.milestoneItem = s.timelineSubjectUnion("MilestoneItem", issueType, pullType)
	reg.renamedSubject = s.timelineSubjectUnion("RenamedTitleSubject", issueType, pullType)
	reg.referencedSubj = s.timelineSubjectUnion("ReferencedSubject", issueType, pullType)
	// Assignee is shared with the Assignable connection (AssigneeConnection) that
	// Issue/PullRequest expose; the memoized builder owns the one instance.
	reg.assignee = s.sharedAssigneeUnion()

	reg.updateIntent = graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueUpdateIntent",
		Fields: graphql.Fields{
			"confidence": &graphql.Field{Type: s.graphQLEnum("IssueEventConfidenceLevel", "HIGH", "LOW", "MEDIUM")},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"intentId":   &graphql.Field{Type: graphql.String},
			"rationale":  &graphql.Field{Type: graphql.String},
		},
	})
	reg.eventRationale = graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueEventRationale",
		Fields: graphql.Fields{
			"actor":     &graphql.Field{Type: s.graphqlTypes.actor},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"rationale": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	// Issue.eventRationales names this same object; memoize it so both reference
	// one instance rather than minting a duplicate type name.
	s.graphqlTypes.issueEventRationale = reg.eventRationale

	// Issue and PullRequest claim the Assignable/Closable interfaces at
	// construction (graphql-go memoizes an object's interface list), so they are
	// only referenced here.
	_ = s.gqlClosableInterface()
	_ = s.gqlAssignableInterface()

	reg.projectV2Event = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "ProjectV2Event",
		Fields: graphql.Fields{
			"project":      &graphql.Field{Type: s.graphqlTypes.projectV2Type},
			"wasAutomated": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			name, _ := source["__typename"].(string)
			return reg.byName[name]
		},
	})

	// the event object types
	//
	// declare() adds id/createdAt/actor, registers the type for union and Node
	// dispatch, and returns it. `intent`/`rationale` describe a Copilot-suggested
	// update bleephub never records, so they resolve to null.
	node := []*graphql.Interface{nodeInterface}
	nodeAndURL := []*graphql.Interface{nodeInterface, urlLocatable}
	intent := &graphql.Field{Type: reg.updateIntent}
	rationale := &graphql.Field{Type: reg.eventRationale}
	declare := func(name string, interfaces []*graphql.Interface, fields graphql.Fields) *graphql.Object {
		object := s.timelineEventObject(name, interfaces, fields)
		reg.byName[name] = object
		nodeTypes[name] = object
		return object
	}
	locatable := graphql.Fields{
		"resourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
		"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
	}
	withLocatable := func(fields graphql.Fields) graphql.Fields {
		for name, field := range locatable {
			fields[name] = field
		}
		return fields
	}

	closable := &graphql.Field{
		Type:    graphql.NewNonNull(reg.closable),
		Resolve: s.timelineParentResolver,
	}
	labelable := &graphql.Field{
		Type:    graphql.NewNonNull(s.gqlLabelableInterface()),
		Resolve: s.timelineParentResolver,
	}
	lockable := &graphql.Field{
		Type:    graphql.NewNonNull(s.gqlLockableInterface()),
		Resolve: s.timelineParentResolver,
	}
	assignable := &graphql.Field{
		Type:    graphql.NewNonNull(reg.assignable),
		Resolve: s.timelineParentResolver,
	}
	issueField := &graphql.Field{
		Type:    graphql.NewNonNull(issueType),
		Resolve: s.timelineParentResolver,
	}
	pullRequestNonNull := &graphql.Field{
		Type:    graphql.NewNonNull(pullType),
		Resolve: s.timelineParentResolver,
	}
	pullRequestNullable := &graphql.Field{
		Type:    pullType,
		Resolve: s.timelineParentResolver,
	}
	milestoneSubject := &graphql.Field{
		Type:    graphql.NewNonNull(reg.milestoneItem),
		Resolve: s.timelineParentResolver,
	}
	renamedSubject := &graphql.Field{
		Type:    graphql.NewNonNull(reg.renamedSubject),
		Resolve: s.timelineParentResolver,
	}
	referencedSubject := &graphql.Field{
		Type:    graphql.NewNonNull(reg.referencedSubj),
		Resolve: s.timelineParentResolver,
	}

	declare("ClosedEvent", nodeAndURL, withLocatable(graphql.Fields{
		"closable":    closable,
		"closer":      &graphql.Field{Type: reg.closer},
		"duplicateOf": &graphql.Field{Type: s.graphqlTypes.issueOrPullRequest},
		"intent":      intent,
		"stateReason": &graphql.Field{Type: s.graphQLEnum("IssueStateReason", "COMPLETED", "NOT_PLANNED", "REOPENED")},
	}))
	declare("ReopenedEvent", node, graphql.Fields{
		"closable":    closable,
		"stateReason": &graphql.Field{Type: s.graphQLEnum("IssueStateReason", "COMPLETED", "NOT_PLANNED", "REOPENED")},
	})
	labelFields := func() graphql.Fields {
		return graphql.Fields{
			"intent":    intent,
			"label":     &graphql.Field{Type: graphql.NewNonNull(s.gqlLabelType())},
			"labelable": labelable,
			"rationale": rationale,
		}
	}
	declare("LabeledEvent", node, labelFields())
	declare("UnlabeledEvent", node, labelFields())
	declare("AssignedEvent", node, graphql.Fields{
		"assignable": assignable,
		"assignee":   &graphql.Field{Type: reg.assignee},
		"intent":     intent,
		"user":       &graphql.Field{Type: s.graphqlTypes.user},
	})
	declare("UnassignedEvent", node, graphql.Fields{
		"assignable": assignable,
		"assignee":   &graphql.Field{Type: reg.assignee},
		"user":       &graphql.Field{Type: s.graphqlTypes.user},
	})
	milestoneFields := func() graphql.Fields {
		return graphql.Fields{
			"milestoneTitle": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"subject":        milestoneSubject,
		}
	}
	declare("MilestonedEvent", node, milestoneFields())
	declare("DemilestonedEvent", node, milestoneFields())
	declare("RenamedTitleEvent", node, graphql.Fields{
		"currentTitle":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"previousTitle": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"subject":       renamedSubject,
	})
	declare("LockedEvent", node, graphql.Fields{
		"lockReason": &graphql.Field{Type: s.graphQLEnum("LockReason", "OFF_TOPIC", "RESOLVED", "SPAM", "TOO_HEATED")},
		"lockable":   lockable,
	})
	declare("UnlockedEvent", node, graphql.Fields{"lockable": lockable})
	declare("PinnedEvent", node, graphql.Fields{"issue": issueField})
	declare("UnpinnedEvent", node, graphql.Fields{"issue": issueField})
	declare("TransferredEvent", node, graphql.Fields{
		"fromRepository": &graphql.Field{Type: s.graphqlTypes.repository},
		"issue":          issueField,
	})
	declare("ConvertedToDiscussionEvent", node, graphql.Fields{
		"discussion": &graphql.Field{Type: s.graphqlTypes.discussion},
	})
	declare("MentionedEvent", node, graphql.Fields{
		"databaseId": &graphql.Field{Type: graphql.Int},
	})
	declare("CommentDeletedEvent", node, graphql.Fields{
		"databaseId":           &graphql.Field{Type: graphql.Int},
		"deletedCommentAuthor": &graphql.Field{Type: s.graphqlTypes.actor},
	})
	declare("CrossReferencedEvent", nodeAndURL, withLocatable(graphql.Fields{
		"isCrossRepository": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"referencedAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		"source":            &graphql.Field{Type: graphql.NewNonNull(reg.referencedSubj)},
		"target":            referencedSubject,
		"willCloseTarget":   &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	}))
	declare("ReferencedEvent", node, graphql.Fields{
		"commit":            &graphql.Field{Type: s.graphqlTypes.commit},
		"commitRepository":  &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.repository)},
		"isCrossRepository": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"isDirectReference": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"subject":           referencedSubject,
	})
	declare("AddedToProjectV2Event", []*graphql.Interface{nodeInterface, reg.projectV2Event}, graphql.Fields{
		"project":      &graphql.Field{Type: s.graphqlTypes.projectV2Type},
		"wasAutomated": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	})

	// pull-request-only members
	declare("MergedEvent", nodeAndURL, withLocatable(graphql.Fields{
		"commit":       &graphql.Field{Type: s.graphqlTypes.commit},
		"mergeRef":     &graphql.Field{Type: s.graphqlTypes.ref},
		"mergeRefName": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"pullRequest":  pullRequestNonNull,
	}))
	reviewRequestFields := func() graphql.Fields {
		return graphql.Fields{
			"pullRequest":       pullRequestNonNull,
			"requestedReviewer": &graphql.Field{Type: s.graphqlTypes.requestedReviewerUnion},
		}
	}
	declare("ReviewRequestedEvent", node, reviewRequestFields())
	declare("ReviewRequestRemovedEvent", node, reviewRequestFields())
	declare("ReviewDismissedEvent", nodeAndURL, withLocatable(graphql.Fields{
		"databaseId":           &graphql.Field{Type: graphql.Int},
		"dismissalMessage":     &graphql.Field{Type: graphql.String},
		"dismissalMessageHTML": &graphql.Field{Type: graphql.String},
		"previousReviewState": &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum(
			"PullRequestReviewState", "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED", "PENDING"))},
		"pullRequest": pullRequestNonNull,
		"review":      &graphql.Field{Type: s.graphqlTypes.pullRequestReview},
	}))
	declare("ReadyForReviewEvent", nodeAndURL, withLocatable(graphql.Fields{
		"pullRequest": pullRequestNonNull,
	}))
	declare("ConvertToDraftEvent", nodeAndURL, withLocatable(graphql.Fields{
		"pullRequest": pullRequestNonNull,
	}))
	declare("AutoMergeEnabledEvent", node, graphql.Fields{
		"enabler":     &graphql.Field{Type: s.graphqlTypes.user},
		"pullRequest": pullRequestNullable,
	})
	declare("AutoMergeDisabledEvent", node, graphql.Fields{
		"disabler":    &graphql.Field{Type: s.graphqlTypes.user},
		"pullRequest": pullRequestNullable,
		"reason":      &graphql.Field{Type: graphql.String},
		"reasonCode":  &graphql.Field{Type: graphql.String},
	})
	declare("HeadRefDeletedEvent", node, graphql.Fields{
		"headRef":     &graphql.Field{Type: s.graphqlTypes.ref},
		"headRefName": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"pullRequest": pullRequestNonNull,
	})
	declare("HeadRefRestoredEvent", node, graphql.Fields{
		"pullRequest": pullRequestNonNull,
	})
	declare("BaseRefChangedEvent", node, graphql.Fields{
		"currentRefName":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"databaseId":      &graphql.Field{Type: graphql.Int},
		"previousRefName": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"pullRequest":     pullRequestNonNull,
	})

	// the unions and their connections
	reg.issueItemTypeOf = issueTimelineItemTypes()
	reg.pullItemTypeOf = pullRequestTimelineItemTypes()
	reg.issueMemberSet = map[string]bool{}
	reg.pullMemberSet = map[string]bool{}

	issueMembers := []*graphql.Object{s.graphqlTypes.issueComment}
	for _, name := range issueTimelineMemberNames {
		issueMembers = append(issueMembers, reg.byName[name])
	}
	pullMembers := []*graphql.Object{
		s.graphqlTypes.issueComment,
		s.graphqlTypes.pullRequestCommit,
		s.graphqlTypes.pullRequestReview,
		s.graphqlTypes.pullRequestReviewThread,
	}
	for _, name := range pullRequestTimelineMemberNames {
		pullMembers = append(pullMembers, reg.byName[name])
	}
	// IssueEventRationale.issueEvent's union names ClosedEvent/LabeledEvent/
	// UnlabeledEvent, now present in reg.byName, plus the six agent-triage
	// events minted alongside it.
	s.addIssueEventWithRationaleUnion(reg, dateTime)
	for _, member := range issueMembers {
		reg.issueMemberSet[member.Name()] = true
	}
	for _, member := range pullMembers {
		reg.pullMemberSet[member.Name()] = true
	}
	resolveMember := func(p graphql.ResolveTypeParams) *graphql.Object {
		source, _ := p.Value.(map[string]interface{})
		name, _ := source["__typename"].(string)
		switch name {
		case "IssueComment":
			return s.graphqlTypes.issueComment
		case "PullRequestCommit":
			return s.graphqlTypes.pullRequestCommit
		case "PullRequestReview":
			return s.graphqlTypes.pullRequestReview
		case "PullRequestReviewThread":
			return s.graphqlTypes.pullRequestReviewThread
		}
		return reg.byName[name]
	}
	reg.issueUnion = graphql.NewUnion(graphql.UnionConfig{
		Name: "IssueTimelineItems", Types: issueMembers, ResolveType: resolveMember,
	})
	reg.pullUnion = graphql.NewUnion(graphql.UnionConfig{
		Name: "PullRequestTimelineItems", Types: pullMembers, ResolveType: resolveMember,
	})
	reg.issueItemType = s.graphQLEnum("IssueTimelineItemsItemType", sortedItemTypeValues(reg.issueItemTypeOf)...)
	reg.pullItemType = s.graphQLEnum("PullRequestTimelineItemsItemType", sortedItemTypeValues(reg.pullItemTypeOf)...)
	reg.issueConn = s.timelineConnectionType("IssueTimelineItems", reg.issueUnion, dateTime)
	reg.pullConn = s.timelineConnectionType("PullRequestTimelineItems", reg.pullUnion, dateTime)

	s.graphqlTypes.issue.AddFieldConfig("timelineItems", &graphql.Field{
		Type: graphql.NewNonNull(reg.issueConn),
		Args: timelineConnectionArgs(reg.issueItemType, dateTime),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveTimelineItems(p, "issue")
		},
	})
	s.graphqlTypes.pullRequest.AddFieldConfig("timelineItems", &graphql.Field{
		Type: graphql.NewNonNull(reg.pullConn),
		Args: timelineConnectionArgs(reg.pullItemType, dateTime),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveTimelineItems(p, "pull_request")
		},
	})
}

// timelineSubjectUnion builds one of GitHub's three Issue|PullRequest subject
// unions (MilestoneItem, RenamedTitleSubject, ReferencedSubject).
func (s *Resolver) timelineSubjectUnion(name string, issueType, pullType *graphql.Object) *graphql.Union {
	return graphql.NewUnion(graphql.UnionConfig{
		Name:  name,
		Types: []*graphql.Object{issueType, pullType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			return s.issueOrPullRequestObject(p.Value)
		},
	})
}

// timelineEventObject builds one timeline event type with the id/createdAt/
// actor triple every member of GitHub's event family carries.
func (s *Resolver) timelineEventObject(name string, interfaces []*graphql.Interface, fields graphql.Fields) *graphql.Object {
	fields["id"] = &graphql.Field{
		Type: graphql.NewNonNull(graphql.ID),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			event, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("timeline event source: unexpected type %T", p.Source)
			}
			return event["nodeID"], nil
		},
	}
	fields["createdAt"] = &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))}
	fields["actor"] = &graphql.Field{
		Type: s.graphqlTypes.actor,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			event, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("timeline event source: unexpected type %T", p.Source)
			}
			return event["actor"], nil
		},
	}
	return graphql.NewObject(graphql.ObjectConfig{Name: name, Interfaces: interfaces, Fields: fields})
}

// timelineConnectionType builds the timeline connections, which carry two
// members no other connection has: filteredCount (window size after
// since/before/after filtering, before slicing) and updatedAt.
func (s *Resolver) timelineConnectionType(prefix string, member *graphql.Union, dateTime *graphql.Scalar) *graphql.Object {
	edge := graphql.NewObject(graphql.ObjectConfig{
		Name: prefix + "Edge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: member},
		},
	})
	return graphql.NewObject(graphql.ObjectConfig{
		Name: prefix + "Connection",
		Fields: graphql.Fields{
			"edges":         &graphql.Field{Type: graphql.NewList(edge)},
			"filteredCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"nodes":         &graphql.Field{Type: graphql.NewList(member)},
			"pageCount":     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":      &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount":    &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"updatedAt":     &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		},
	})
}

// timelineConnectionArgs is the argument set GitHub gives both timelineItems
// fields: the four Relay arguments plus `skip`, `since` and `itemTypes`.
func timelineConnectionArgs(itemType *graphql.Enum, dateTime *graphql.Scalar) graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"after":     &graphql.ArgumentConfig{Type: graphql.String},
		"before":    &graphql.ArgumentConfig{Type: graphql.String},
		"first":     &graphql.ArgumentConfig{Type: graphql.Int},
		"itemTypes": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(itemType))},
		"last":      &graphql.ArgumentConfig{Type: graphql.Int},
		"since":     &graphql.ArgumentConfig{Type: dateTime},
		"skip":      &graphql.ArgumentConfig{Type: graphql.Int},
	}
}
