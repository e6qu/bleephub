package graphqlapi

import (
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// addModerationMutationsToSchema registers comment minimization +
// issue/PR locking GraphQL mutations against the shared mutationType.
// Mirrors real GitHub's mutation surface: minimizeComment /
// unminimizeComment / lockLockable / unlockLockable.
func (s *Resolver) addModerationMutationsToSchema(mutationType *graphql.Object) {
	// --- minimizeComment / unminimizeComment ---

	classifierEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "ReportedContentClassifiers",
		Values: graphql.EnumValueConfigMap{
			"OFF_TOPIC": &graphql.EnumValueConfig{Value: "OFF_TOPIC"},
			"OUTDATED":  &graphql.EnumValueConfig{Value: "OUTDATED"},
			"RESOLVED":  &graphql.EnumValueConfig{Value: "RESOLVED"},
			"DUPLICATE": &graphql.EnumValueConfig{Value: "DUPLICATE"},
			"SPAM":      &graphql.EnumValueConfig{Value: "SPAM"},
			"ABUSE":     &graphql.EnumValueConfig{Value: "ABUSE"},
		},
	})

	minimizeInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "MinimizeCommentInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"subjectId":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"classifier": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(classifierEnum)},
		},
	})

	unminimizeInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UnminimizeCommentInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"subjectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	// The payload carries the comment behind GitHub's Minimizable interface;
	// the resolvers below feed it full commentToGQL source maps so any
	// inline-fragment selection on the concrete IssueComment resolves.
	minimizePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "MinimizeCommentPayload",
		Fields: graphql.Fields{
			"minimizedComment": &graphql.Field{Type: s.gqlMinimizableInterface()},
		},
	})

	unminimizePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UnminimizeCommentPayload",
		Fields: graphql.Fields{
			"unminimizedComment": &graphql.Field{Type: s.gqlMinimizableInterface()},
		},
	})

	s.registerMutation(mutationType, "minimizeComment", &graphql.Field{
		Type: minimizePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(minimizeInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["subjectId"].(string)
			classifier, _ := input["classifier"].(string)
			c := s.store.LookupCommentByNodeID(nodeID)
			if c == nil {
				return nil, gqlMissingNode("node", nodeID)
			}
			updated := s.store.SetCommentMinimization(c.ID, user.ID, classifier)
			if updated == nil {
				return nil, gqlMissingNode("node", nodeID)
			}
			return map[string]interface{}{
				"minimizedComment": commentToGQL(updated, s.store),
			}, nil
		},
	})

	s.registerMutation(mutationType, "unminimizeComment", &graphql.Field{
		Type: unminimizePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(unminimizeInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["subjectId"].(string)
			c := s.store.LookupCommentByNodeID(nodeID)
			if c == nil {
				return nil, gqlMissingNode("node", nodeID)
			}
			updated := s.store.SetCommentMinimization(c.ID, user.ID, "")
			if updated == nil {
				return nil, gqlMissingNode("node", nodeID)
			}
			return map[string]interface{}{
				"unminimizedComment": commentToGQL(updated, s.store),
			}, nil
		},
	})

	// --- lockLockable / unlockLockable ---

	lockReasonEnum := s.graphQLEnum("LockReason", "OFF_TOPIC", "RESOLVED", "SPAM", "TOO_HEATED")

	lockInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "LockLockableInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"lockableId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"lockReason": &graphql.InputObjectFieldConfig{Type: lockReasonEnum},
		},
	})

	unlockInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UnlockLockableInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"lockableId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	// The payloads carry the locked record behind GitHub's Lockable
	// interface; lockByNodeID feeds them full issueToGQL/pullRequestToGQL
	// source maps so inline fragments on the concrete types resolve.
	lockPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "LockLockablePayload",
		Fields: graphql.Fields{
			"lockedRecord": &graphql.Field{Type: s.gqlLockableInterface()},
		},
	})

	unlockPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UnlockLockablePayload",
		Fields: graphql.Fields{
			"unlockedRecord": &graphql.Field{Type: s.gqlLockableInterface()},
		},
	})

	s.registerMutation(mutationType, "lockLockable", &graphql.Field{
		Type: lockPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(lockInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["lockableId"].(string)
			reasonEnum, _ := input["lockReason"].(string)
			restReason := graphqlToRESTLockReason(reasonEnum)
			locked, ok := s.lockByNodeID(nodeID, true, restReason, s.ghUserFromContext(p.Context))
			if !ok {
				return nil, gqlMissingNode("node", nodeID)
			}
			return map[string]interface{}{"lockedRecord": locked}, nil
		},
	})

	s.registerMutation(mutationType, "unlockLockable", &graphql.Field{
		Type: unlockPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(unlockInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["lockableId"].(string)
			unlocked, ok := s.lockByNodeID(nodeID, false, "", s.ghUserFromContext(p.Context))
			if !ok {
				return nil, gqlMissingNode("node", nodeID)
			}
			return map[string]interface{}{"unlockedRecord": unlocked}, nil
		},
	})
}

// graphqlToRESTLockReason maps the GraphQL LockReason enum (UPPER_SNAKE)
// to the REST API's kebab-cased reason string.
func graphqlToRESTLockReason(enum string) string {
	switch enum {
	case "OFF_TOPIC":
		return "off-topic"
	case "TOO_HEATED":
		return "too heated"
	case "RESOLVED":
		return "resolved"
	case "SPAM":
		return "spam"
	}
	return ""
}

// lockByNodeID resolves nodeID to an Issue or PullRequest, applies the
// requested lock state, and returns the full GraphQL source map for the
// Lockable interface (the concrete Issue/PullRequest type resolves any
// selection, and activeLockReason serializes through the LockReason enum).
// The bool indicates whether a target was found. An issue or pull request also
// delivers the locked/unlocked webhook action the REST lock endpoint delivers,
// so the two surfaces stay indistinguishable to a consumer.
func (s *Resolver) lockByNodeID(nodeID string, locked bool, reason string, user *store.User) (map[string]interface{}, bool) {
	action := "unlocked"
	if locked {
		action = "locked"
	}
	if issue := store.FindIssueByNodeID(s.store, nodeID); issue != nil {
		s.store.SetIssueOrPRLock(issue.RepoID, issue.Number, locked, reason)
		refreshed := s.store.GetIssue(issue.ID)
		if refreshed == nil {
			refreshed = issue
		}
		if repo := s.store.GetRepoByID(issue.RepoID); repo != nil && user != nil {
			s.emitWebhookEvent(repo.FullName, "issues", action, s.buildIssuesPayload(repo, refreshed, user, action))
		}
		return issueToGQL(refreshed, s.store), true
	}
	if pr := store.FindPullRequestByNodeID(s.store, nodeID); pr != nil {
		s.store.SetIssueOrPRLock(pr.RepoID, pr.Number, locked, reason)
		refreshed := s.store.GetPullRequest(pr.ID)
		if refreshed == nil {
			refreshed = pr
		}
		s.emitPullRequestAction(refreshed, user, action, true)
		return pullRequestToGQL(refreshed, s.store), true
	}
	if d := store.FindDiscussionByNodeID(s.store, nodeID); d != nil {
		s.store.UpdateDiscussion(d.ID, func(dd *store.Discussion) {
			dd.Locked = locked
			if locked {
				dd.LockedReason = reason
			} else {
				dd.LockedReason = ""
			}
		})
		refreshed := s.store.GetDiscussion(d.ID)
		if refreshed == nil {
			refreshed = d
		}
		return discussionToGQL(refreshed, s.store), true
	}
	return nil, false
}

// gqlLockableInterface returns GitHub's Lockable interface (memoized):
// locked: Boolean! + activeLockReason: LockReason, exactly the official
// field set. Issue, PullRequest, and Discussion implement it. ResolveType
// discriminates on the source map's node id prefix — the registry entries
// are populated by the time any query executes.
func (s *Resolver) gqlLockableInterface() *graphql.Interface {
	if s.graphqlTypes.lockable != nil {
		return s.graphqlTypes.lockable
	}
	s.graphqlTypes.lockable = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Lockable",
		Fields: graphql.Fields{
			"locked":           &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"activeLockReason": &graphql.Field{Type: s.graphQLEnum("LockReason", "OFF_TOPIC", "RESOLVED", "SPAM", "TOO_HEATED")},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			nodeID, _ := source["nodeID"].(string)
			switch {
			case strings.HasPrefix(nodeID, "PR_"):
				return s.graphqlTypes.pullRequest
			case strings.HasPrefix(nodeID, "D_"):
				return s.graphqlTypes.discussion
			default:
				return s.graphqlTypes.issue
			}
		},
	})
	return s.graphqlTypes.lockable
}

// gqlMinimizableInterface returns GitHub's Minimizable interface (memoized)
// with the subset of official fields bleephub models (isMinimized,
// minimizedReason). IssueComment and DiscussionComment implement it.
func (s *Resolver) gqlMinimizableInterface() *graphql.Interface {
	if s.graphqlTypes.minimizable != nil {
		return s.graphqlTypes.minimizable
	}
	s.graphqlTypes.minimizable = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Minimizable",
		Fields: graphql.Fields{
			"isMinimized":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"minimizedReason": &graphql.Field{Type: graphql.String},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			nodeID, _ := source["nodeID"].(string)
			if strings.HasPrefix(nodeID, "DC_") {
				return s.graphqlTypes.discussionComment
			}
			return s.graphqlTypes.issueComment
		},
	})
	return s.graphqlTypes.minimizable
}
