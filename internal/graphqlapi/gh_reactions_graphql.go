package graphqlapi

import (
	"context"
	"fmt"

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

// resolveReactableSubject decodes a Reactable node id (a Discussion or
// DiscussionComment) to its store (parentType, databaseID). ok=false if the id
// is not a supported reactable subject.
func (s *Resolver) resolveReactableSubject(nodeID string) (string, int, bool) {
	if d := store.FindDiscussionByNodeID(s.store, nodeID); d != nil {
		return "discussion", d.ID, true
	}
	if c := store.FindDiscussionCommentByNodeID(s.store, nodeID); c != nil {
		return "discussion_comment", c.ID, true
	}
	return "", 0, false
}

// mutationTargetReactable authorizes a reaction against the repo owning the
// subject (discussion or discussion comment).
func mutationTargetReactable(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		// `missing` stays set so the "can't read the repo" path in authorize
		// returns a non-nil error rather than silently authorizing.
		target := mutationTarget{missing: gqlMissingNode("Reactable", nodeID)}
		if d := store.FindDiscussionByNodeID(s.store, nodeID); d != nil {
			target.repo = s.store.GetRepoByID(d.RepoID)
			target.authorID = d.AuthorID
			return target
		}
		if c := store.FindDiscussionCommentByNodeID(s.store, nodeID); c != nil {
			target.authorID = c.AuthorID
			if d := s.store.GetDiscussion(c.DiscussionID); d != nil {
				target.repo = s.store.GetRepoByID(d.RepoID)
			}
			return target
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
				return nil, fmt.Errorf("could not resolve to a Reactable with the node id of '%s'", subjectID)
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
				return nil, fmt.Errorf("could not resolve to a Reactable with the node id of '%s'", subjectID)
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
