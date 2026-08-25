package graphqlapi

// Schema-fidelity shells for the timeline events GitHub declares but bleephub
// does not reach through any resolved field. These 35 event objects are the
// unmodeled entries of the IssueTimelineItems / PullRequestTimelineItems
// unions (merge-queue, classic- and v2-project moves, force-pushes,
// deployments, cross-references, sub-issue and block relations, subscription
// and block events). They exist so bleephub's introspected schema matches
// GitHub's shape; they are registered standalone (NOT added to the timeline
// unions) because this instance does not produce the underlying data, matching
// the existing design choice not to resolve them.
//
// Every type is built signature-exact against the vendored SDL. Objects use the
// memoized mutationObject so graphql-go holds one instance per GitHub name;
// their fields reference the already-assembled read surface (Issue,
// PullRequest, Commit, User, Ref, Actor, ProjectV2, classic Project/ProjectCard,
// Deployment, DeploymentStatus, MergeQueue, IssueComment, IssueOrPullRequest).
// Three support types GitHub names here but bleephub does not otherwise build —
// the ReferencedSubject union, the Subscribable interface and the
// UserBlockDuration enum — are minted here too and registered alongside.

import (
	"github.com/graphql-go/graphql"
)

func init() {
	schemaShellBuilders = append(schemaShellBuilders, (*Resolver).addEventShells)
}

// addEventShells builds every unmodeled timeline-event object plus the three
// support types they name, registering each so it appears in introspection.
func (s *Resolver) addEventShells() {
	// Existing read-surface types, reached read-only.
	actor := s.graphqlTypes.actor
	dateTime := s.graphQLStringScalar("DateTime")
	user := s.graphqlTypes.user
	issue := s.graphqlTypes.issue
	pullRequest := s.graphqlTypes.pullRequest
	commit := s.graphqlTypes.commit
	ref := s.graphqlTypes.ref
	issueComment := s.graphqlTypes.issueComment
	projectV2 := s.graphqlTypes.projectV2Type
	project := s.namedObject("Project")
	projectCard := s.namedObject("ProjectCard")
	mergeQueue := s.namedObject("MergeQueue")
	deployment := s.graphqlTypes.deployment
	deploymentStatus := s.graphqlTypes.deploymentStatus
	issueOrPullRequest := s.graphqlTypes.issueOrPullRequest

	// The three support types GitHub names here — the ReferencedSubject union
	// (Issue | PullRequest), the Subscribable interface and the UserBlockDuration
	// enum — are each already built and registered elsewhere (the timeline
	// registry, updateSubscription's payload interface, the ordering shells), so
	// reach the memoized instances rather than minting duplicates graphql-go
	// would reject.
	referencedSubject := s.graphqlTypes.timeline.referencedSubj
	subscribable := s.subscribableInterface()
	userBlockDuration := s.sharedEnum("UserBlockDuration",
		"ONE_DAY", "ONE_MONTH", "ONE_WEEK", "PERMANENT", "THREE_DAYS")

	// Only the event objects are minted here; the support types above are reused
	// instances already present in the schema.
	var built []graphql.Type
	register := func(o *graphql.Object) *graphql.Object {
		built = append(built, o)
		return o
	}

	// The `actor`, `createdAt` and `id` triple every event declares.
	base := func(extra graphql.Fields) graphql.Fields {
		f := graphql.Fields{
			"actor":     gqlField(actor),
			"createdAt": gqlNonNull(dateTime),
			"id":        gqlNonNull(graphql.ID),
		}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}

	register(s.mutationObject("AddedToMergeQueueEvent", base(graphql.Fields{
		"enqueuer":    gqlField(user),
		"mergeQueue":  gqlField(mergeQueue),
		"pullRequest": gqlField(pullRequest),
	})))
	register(s.mutationObject("AddedToProjectEvent", base(graphql.Fields{
		"databaseId":        gqlField(graphql.Int),
		"project":           gqlField(project),
		"projectCard":       gqlField(projectCard),
		"projectColumnName": gqlNonNull(graphql.String),
	})))
	register(s.mutationObject("AutoRebaseEnabledEvent", base(graphql.Fields{
		"enabler":     gqlField(user),
		"pullRequest": gqlField(pullRequest),
	})))
	register(s.mutationObject("AutoSquashEnabledEvent", base(graphql.Fields{
		"enabler":     gqlField(user),
		"pullRequest": gqlField(pullRequest),
	})))
	register(s.mutationObject("AutomaticBaseChangeFailedEvent", base(graphql.Fields{
		"newBase":     gqlNonNull(graphql.String),
		"oldBase":     gqlNonNull(graphql.String),
		"pullRequest": gqlNonNull(pullRequest),
	})))
	register(s.mutationObject("AutomaticBaseChangeSucceededEvent", base(graphql.Fields{
		"newBase":     gqlNonNull(graphql.String),
		"oldBase":     gqlNonNull(graphql.String),
		"pullRequest": gqlNonNull(pullRequest),
	})))
	register(s.mutationObject("BaseRefDeletedEvent", base(graphql.Fields{
		"baseRefName": gqlField(graphql.String),
		"pullRequest": gqlField(pullRequest),
	})))
	register(s.mutationObject("BaseRefForcePushedEvent", base(graphql.Fields{
		"afterCommit":  gqlField(commit),
		"beforeCommit": gqlField(commit),
		"pullRequest":  gqlNonNull(pullRequest),
		"ref":          gqlField(ref),
	})))
	register(s.mutationObject("BlockedByAddedEvent", base(graphql.Fields{
		"blockingIssue": gqlField(issue),
	})))
	register(s.mutationObject("BlockedByRemovedEvent", base(graphql.Fields{
		"blockingIssue": gqlField(issue),
	})))
	register(s.mutationObject("BlockingAddedEvent", base(graphql.Fields{
		"blockedIssue": gqlField(issue),
	})))
	register(s.mutationObject("BlockingRemovedEvent", base(graphql.Fields{
		"blockedIssue": gqlField(issue),
	})))
	register(s.mutationObject("ConnectedEvent", base(graphql.Fields{
		"isCrossRepository": gqlNonNull(graphql.Boolean),
		"source":            gqlNonNull(referencedSubject),
		"subject":           gqlNonNull(referencedSubject),
	})))
	register(s.mutationObject("ConvertedFromDraftEvent", base(graphql.Fields{
		"project":      gqlField(projectV2),
		"wasAutomated": gqlNonNull(graphql.Boolean),
	})))
	register(s.mutationObject("ConvertedNoteToIssueEvent", base(graphql.Fields{
		"databaseId":        gqlField(graphql.Int),
		"project":           gqlField(project),
		"projectCard":       gqlField(projectCard),
		"projectColumnName": gqlNonNull(graphql.String),
	})))
	register(s.mutationObject("DeployedEvent", base(graphql.Fields{
		"databaseId":  gqlField(graphql.Int),
		"deployment":  gqlNonNull(deployment),
		"pullRequest": gqlNonNull(pullRequest),
		"ref":         gqlField(ref),
	})))
	register(s.mutationObject("DeploymentEnvironmentChangedEvent", base(graphql.Fields{
		"deploymentStatus": gqlNonNull(deploymentStatus),
		"pullRequest":      gqlNonNull(pullRequest),
	})))
	register(s.mutationObject("DisconnectedEvent", base(graphql.Fields{
		"isCrossRepository": gqlNonNull(graphql.Boolean),
		"source":            gqlNonNull(referencedSubject),
		"subject":           gqlNonNull(referencedSubject),
	})))
	register(s.mutationObject("HeadRefForcePushedEvent", base(graphql.Fields{
		"afterCommit":  gqlField(commit),
		"beforeCommit": gqlField(commit),
		"pullRequest":  gqlNonNull(pullRequest),
		"ref":          gqlField(ref),
	})))
	register(s.mutationObject("IssueCommentPinnedEvent", base(graphql.Fields{
		"issueComment": gqlField(issueComment),
	})))
	register(s.mutationObject("IssueCommentUnpinnedEvent", base(graphql.Fields{
		"issueComment": gqlField(issueComment),
	})))
	register(s.mutationObject("MarkedAsDuplicateEvent", base(graphql.Fields{
		"canonical":         gqlField(issueOrPullRequest),
		"duplicate":         gqlField(issueOrPullRequest),
		"isCrossRepository": gqlNonNull(graphql.Boolean),
	})))
	register(s.mutationObject("MovedColumnsInProjectEvent", base(graphql.Fields{
		"databaseId":                gqlField(graphql.Int),
		"previousProjectColumnName": gqlNonNull(graphql.String),
		"project":                   gqlField(project),
		"projectCard":               gqlField(projectCard),
		"projectColumnName":         gqlNonNull(graphql.String),
	})))
	register(s.mutationObject("ParentIssueAddedEvent", base(graphql.Fields{
		"parent": gqlField(issue),
	})))
	register(s.mutationObject("ParentIssueRemovedEvent", base(graphql.Fields{
		"parent": gqlField(issue),
	})))
	register(s.mutationObject("ProjectV2ItemStatusChangedEvent", base(graphql.Fields{
		"previousStatus": gqlNonNull(graphql.String),
		"project":        gqlField(projectV2),
		"status":         gqlNonNull(graphql.String),
		"wasAutomated":   gqlNonNull(graphql.Boolean),
	})))
	register(s.mutationObject("RemovedFromMergeQueueEvent", base(graphql.Fields{
		"beforeCommit": gqlField(commit),
		"enqueuer":     gqlField(user),
		"mergeQueue":   gqlField(mergeQueue),
		"pullRequest":  gqlField(pullRequest),
		"reason":       gqlField(graphql.String),
	})))
	register(s.mutationObject("RemovedFromProjectEvent", base(graphql.Fields{
		"databaseId":        gqlField(graphql.Int),
		"project":           gqlField(project),
		"projectColumnName": gqlNonNull(graphql.String),
	})))
	register(s.mutationObject("RemovedFromProjectV2Event", base(graphql.Fields{
		"project":      gqlField(projectV2),
		"wasAutomated": gqlNonNull(graphql.Boolean),
	})))
	register(s.mutationObject("SubIssueAddedEvent", base(graphql.Fields{
		"subIssue": gqlField(issue),
	})))
	register(s.mutationObject("SubIssueRemovedEvent", base(graphql.Fields{
		"subIssue": gqlField(issue),
	})))
	register(s.mutationObject("SubscribedEvent", base(graphql.Fields{
		"subscribable": gqlNonNull(subscribable),
	})))
	register(s.mutationObject("UnmarkedAsDuplicateEvent", base(graphql.Fields{
		"canonical":         gqlField(issueOrPullRequest),
		"duplicate":         gqlField(issueOrPullRequest),
		"isCrossRepository": gqlNonNull(graphql.Boolean),
	})))
	register(s.mutationObject("UnsubscribedEvent", base(graphql.Fields{
		"subscribable": gqlNonNull(subscribable),
	})))
	register(s.mutationObject("UserBlockedEvent", base(graphql.Fields{
		"blockDuration": gqlNonNull(userBlockDuration),
		"subject":       gqlField(user),
	})))

	for _, t := range built {
		s.registerExtraSchemaType(t)
	}
}
