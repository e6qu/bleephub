package graphqlapi

import (
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// addModerationMutationsToSchema registers minimizeComment/unminimizeComment
// and lockLockable/unlockLockable.
func (s *Resolver) addModerationMutationsToSchema(mutationType *graphql.Object) {
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

	// The resolvers feed full commentToGQL source maps so inline-fragment
	// selections on the concrete IssueComment resolve.
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

	// lockByNodeID feeds full issueToGQL/pullRequestToGQL source maps so inline
	// fragments on the concrete types resolve.
	lockPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "LockLockablePayload",
		Fields: graphql.Fields{
			"lockedRecord": &graphql.Field{Type: s.gqlLockableInterface()},
			"actor":        s.mutationActorField(),
		},
	})

	unlockPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UnlockLockablePayload",
		Fields: graphql.Fields{
			"unlockedRecord": &graphql.Field{Type: s.gqlLockableInterface()},
			"actor":          s.mutationActorField(),
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

// graphqlToRESTLockReason maps the LockReason enum to the REST reason string.
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

// lockByNodeID applies the lock state to the Issue/PullRequest/Discussion
// nodeID names and returns its Lockable source map (bool false when not found).
// It also emits the locked/unlocked webhook the REST lock endpoint does, so the
// two surfaces stay indistinguishable.
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

// gqlLockableInterface returns the memoized Lockable interface, implemented by
// Issue, PullRequest and Discussion. ResolveType discriminates on the source's
// node-id prefix.
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

// gqlMinimizableInterface returns the memoized Minimizable interface,
// implemented by IssueComment and DiscussionComment.
func (s *Resolver) gqlMinimizableInterface() *graphql.Interface {
	if s.graphqlTypes.minimizable != nil {
		return s.graphqlTypes.minimizable
	}
	s.graphqlTypes.minimizable = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Minimizable",
		Fields: graphql.Fields{
			"isMinimized":         &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"minimizedReason":     &graphql.Field{Type: graphql.String},
			"viewerCanMinimize":   &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"viewerCanUnminimize": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
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
