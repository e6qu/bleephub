package graphqlapi

import (
	"context"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// viewerReactorID returns the signed-in viewer's user ID, or 0 when anonymous —
// used to compute reactionGroups.viewerHasReacted.
func (s *Resolver) viewerReactorID(ctx context.Context) int {
	if v := s.ghUserFromContext(ctx); v != nil {
		return v.ID
	}
	return 0
}

// graphQLToReactionContent maps the GraphQL ReactionContent enum back to the
// store's canonical content strings (the inverse of reactionContentToGraphQL).
func graphQLToReactionContent(enum string) string {
	switch enum {
	case "THUMBS_UP":
		return "+1"
	case "THUMBS_DOWN":
		return "-1"
	case "LAUGH":
		return "laugh"
	case "HOORAY":
		return "hooray"
	case "CONFUSED":
		return "confused"
	case "HEART":
		return "heart"
	case "ROCKET":
		return "rocket"
	case "EYES":
		return "eyes"
	}
	return ""
}

// reactableSubject is a resolved Reactable node: the store (parentType,
// databaseID) the reaction attaches to, plus the owning repo and author used
// for authorization.
type reactableSubject struct {
	parentType string
	parentID   int
	repo       *store.Repo
	authorID   int
}

// lookupReactableSubject decodes a Reactable node id to its store subject.
// GitHub's AddReactionInput.subjectId accepts CommitComment, Discussion,
// DiscussionComment, Issue, IssueComment, PullRequest, PullRequestReview,
// PullRequestReviewComment and Release; bleephub resolves the subjects that
// carry node-ID finders today (issues, pull requests, reviews, discussions and
// discussion comments). The parentType strings mirror the REST reaction
// handlers so both API surfaces key the same store rows.
func (s *Resolver) lookupReactableSubject(nodeID string) (reactableSubject, bool) {
	if d := store.FindDiscussionByNodeID(s.store, nodeID); d != nil {
		return reactableSubject{"discussion", d.ID, s.store.GetRepoByID(d.RepoID), d.AuthorID}, true
	}
	if c := store.FindDiscussionCommentByNodeID(s.store, nodeID); c != nil {
		rs := reactableSubject{parentType: "discussion_comment", parentID: c.ID, authorID: c.AuthorID}
		if d := s.store.GetDiscussion(c.DiscussionID); d != nil {
			rs.repo = s.store.GetRepoByID(d.RepoID)
		}
		return rs, true
	}
	if i := store.FindIssueByNodeID(s.store, nodeID); i != nil {
		return reactableSubject{"issue", i.ID, s.store.GetRepoByID(i.RepoID), i.AuthorID}, true
	}
	if pr := store.FindPullRequestByNodeID(s.store, nodeID); pr != nil {
		return reactableSubject{"pull_request", pr.ID, s.store.GetRepoByID(pr.RepoID), pr.AuthorID}, true
	}
	if rv := store.FindReviewByNodeID(s.store, nodeID); rv != nil {
		rs := reactableSubject{parentType: "pull_request_review", parentID: rv.ID, authorID: rv.AuthorID}
		if pr := s.store.GetPullRequest(rv.PRID); pr != nil {
			rs.repo = s.store.GetRepoByID(pr.RepoID)
		}
		return rs, true
	}
	return reactableSubject{}, false
}

// resolveReactableSubject decodes a Reactable node id to its store
// (parentType, databaseID). ok=false if the id is not a supported subject.
func (s *Resolver) resolveReactableSubject(nodeID string) (string, int, bool) {
	rs, ok := s.lookupReactableSubject(nodeID)
	return rs.parentType, rs.parentID, ok
}

// reactableSubjectScope maps a resolved reaction subject to the app permission
// scope GitHub requires: issue reactions need issues:write, pull-request and
// review reactions need pull_requests:write, discussion reactions need
// discussions:write — never one blanket scope.
func reactableSubjectScope(parentType string) store.PermScope {
	switch parentType {
	case "issue", "issue_comment":
		return store.ScopeIssues
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		return store.ScopePullRequests
	case "commit_comment":
		return store.ScopeContents
	default:
		return store.ScopeDiscussions
	}
}

// reactableScope resolves the subject in the input and returns its required
// scope, for the subject-typed scopeFor hook on the reaction authz rows.
func reactableScope(key string) func(*Resolver, map[string]interface{}) store.PermScope {
	return func(s *Resolver, input map[string]interface{}) store.PermScope {
		nodeID, _ := input[key].(string)
		if rs, ok := s.lookupReactableSubject(nodeID); ok {
			return reactableSubjectScope(rs.parentType)
		}
		return store.ScopeDiscussions
	}
}

// mutationTargetReactable authorizes a reaction against the repo owning the
// resolved subject.
func mutationTargetReactable(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		// `missing` stays set so the "can't read the repo" path in authorize
		// returns a non-nil error rather than silently authorizing.
		target := mutationTarget{missing: gqlMissingNode("Reactable", nodeID)}
		if rs, ok := s.lookupReactableSubject(nodeID); ok {
			target.repo = rs.repo
			target.authorID = rs.authorID
		}
		return target
	}
}

// addReactionMutationsToSchema registers addReaction/removeReaction. GitHub
// exposes reactions only over GraphQL; the discussion/comment reaction READ
// side (reactionGroups) already existed — this adds the write side.
func (s *Resolver) addReactionMutationsToSchema(mutationType *graphql.Object) {
	reactionContentEnum := s.graphQLEnum(
		"ReactionContent",
		"CONFUSED", "EYES", "HEART", "HOORAY", "LAUGH", "ROCKET", "THUMBS_DOWN", "THUMBS_UP",
	)
	// Payloads expose only clientMutationId (registerMutation supplies it) — a
	// GitHub-exact subset. Emitting our own reaction/subject shapes would diverge
	// from GitHub's Reaction/Reactable and trip the schema-parity ratchet; the
	// client refetches reactionGroups after the mutation instead.
	inputFields := func() graphql.InputObjectConfigFieldMap {
		return graphql.InputObjectConfigFieldMap{
			"subjectId":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"content":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(reactionContentEnum)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		}
	}
	payloadFields := func() graphql.Fields {
		return graphql.Fields{"clientMutationId": &graphql.Field{Type: graphql.String}}
	}
	addInput := graphql.NewInputObject(graphql.InputObjectConfig{Name: "AddReactionInput", Fields: inputFields()})
	addPayload := graphql.NewObject(graphql.ObjectConfig{Name: "AddReactionPayload", Fields: payloadFields()})
	removeInput := graphql.NewInputObject(graphql.InputObjectConfig{Name: "RemoveReactionInput", Fields: inputFields()})
	removePayload := graphql.NewObject(graphql.ObjectConfig{Name: "RemoveReactionPayload", Fields: payloadFields()})

	s.registerMutation(mutationType, "addReaction", &graphql.Field{
		Type: addPayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			viewer := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			subjectID, _ := input["subjectId"].(string)
			contentEnum, _ := input["content"].(string)
			parentType, parentID, ok := s.resolveReactableSubject(subjectID)
			if !ok {
				return nil, gqlMissingNode("Reactable", subjectID)
			}
			if _, _, err := s.store.Reactions.AddReaction(parentType, parentID, viewer.ID, graphQLToReactionContent(contentEnum)); err != nil {
				return nil, err
			}
			return map[string]interface{}{}, nil
		},
	})

	s.registerMutation(mutationType, "removeReaction", &graphql.Field{
		Type: removePayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(removeInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			viewer := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			subjectID, _ := input["subjectId"].(string)
			contentEnum, _ := input["content"].(string)
			parentType, parentID, ok := s.resolveReactableSubject(subjectID)
			if !ok {
				return nil, gqlMissingNode("Reactable", subjectID)
			}
			content := graphQLToReactionContent(contentEnum)
			for _, r := range s.store.Reactions.ListReactions(parentType, parentID, content) {
				if r.UserID == viewer.ID {
					s.store.Reactions.DeleteReactionByUser(parentType, parentID, r.ID, viewer.ID)
					break
				}
			}
			return map[string]interface{}{}, nil
		},
	})
}
