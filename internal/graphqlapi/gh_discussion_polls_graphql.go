package graphqlapi

import (
	"fmt"
	"sort"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Discussion polls: the poll a discussion may carry, its options, and the one
// mutation the schema defines over them — addDiscussionPollVote. One vote per
// user per poll, revoting replaces, which the store makes structural by
// recording the option per user rather than a counter per option.

func init() {
	graphqlMutationAuthz["addDiscussionPollVote"] = discussionPollVoteRule{}
}

// discussionPollVoteRule authorizes a vote against the repository holding the
// poll's discussion: voting needs the same read standing that seeing the
// discussion needs, and a private repository's poll must answer NOT_FOUND to
// a stranger rather than confirming its existence.
type discussionPollVoteRule struct{}

func (discussionPollVoteRule) check() error { return nil }

func (discussionPollVoteRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input["pollOptionId"].(string)
	_, poll := store.FindDiscussionPollOptionByNodeID(s.store, nodeID)
	if poll == nil {
		return gqlMissingNode("DiscussionPollOption", nodeID)
	}
	discussion := s.store.GetDiscussion(poll.DiscussionID)
	if discussion == nil {
		return gqlMissingNode("DiscussionPollOption", nodeID)
	}
	repo := s.store.GetRepoByID(discussion.RepoID)
	if repo == nil || (repo.Private && !s.viewerCanReadRepo(p.Context, repo)) {
		return gqlMissingNode("DiscussionPollOption", nodeID)
	}
	if s.ghUserFromContext(p.Context) == nil {
		return fmt.Errorf("you must be signed in to vote")
	}
	return nil
}

// discussionPollOptionSource renders one option. viewer is the asking user's
// id, or zero for an anonymous read.
func discussionPollOptionSource(poll *store.DiscussionPoll, option *store.DiscussionPollOption, viewer int) map[string]interface{} {
	votes := 0
	for _, chosen := range poll.VotesByUser {
		if chosen == option.ID {
			votes++
		}
	}
	return map[string]interface{}{
		"__typename":     "DiscussionPollOption",
		"id":             option.NodeID,
		"option":         option.Option,
		"totalVoteCount": votes,
		"viewerHasVoted": viewer != 0 && poll.VotesByUser[viewer] == option.ID,
		"pollID":         poll.ID,
		// The poll is keyed in the store by its discussion id (GetDiscussionPoll
		// takes a discussion id), so the option carries it to reconstruct the
		// parent poll for the DiscussionPollOption.poll field.
		"discussionID": poll.DiscussionID,
		"viewerID":     viewer,
	}
}

// discussionPollSource renders a poll with its options in the order asked.
func (s *Resolver) discussionPollSource(poll *store.DiscussionPoll, viewer int, canVote bool) map[string]interface{} {
	if poll == nil {
		return nil
	}
	return map[string]interface{}{
		"__typename":     "DiscussionPoll",
		"id":             poll.NodeID,
		"question":       poll.Question,
		"totalVoteCount": len(poll.VotesByUser),
		"viewerCanVote":  canVote,
		"viewerHasVoted": viewer != 0 && poll.VotesByUser[viewer] != 0,
		"discussionID":   poll.DiscussionID,
		"viewerID":       viewer,
	}
}

// addDiscussionPollTypes builds the poll read surface and the vote mutation.
// discussionType is the already-built Discussion object, which gains its poll
// field here.
func (s *Resolver) addDiscussionPollTypes(mutationType, discussionType *graphql.Object) {
	optionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionPollOption",
		Fields: graphql.Fields{
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"option":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"totalVoteCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"viewerHasVoted": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
	})
	pollType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionPoll",
		Fields: graphql.Fields{
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"question":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"totalVoteCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"viewerCanVote":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"viewerHasVoted": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
	})
	optionEdge := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionPollOptionEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: optionType},
		},
	})
	optionConnection := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionPollOptionConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(optionEdge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(optionType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	orderInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DiscussionPollOptionOrder",
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.sharedEnum("OrderDirection", "ASC", "DESC"))},
			"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.graphQLEnum("DiscussionPollOptionOrderField", "AUTHORED_ORDER", "VOTE_COUNT"))},
		},
	})

	pollType.AddFieldConfig("options", &graphql.Field{
		Type: optionConnection,
		Args: graphql.FieldConfigArgument{
			"first":   &graphql.ArgumentConfig{Type: graphql.Int},
			"last":    &graphql.ArgumentConfig{Type: graphql.Int},
			"after":   &graphql.ArgumentConfig{Type: graphql.String},
			"before":  &graphql.ArgumentConfig{Type: graphql.String},
			"orderBy": &graphql.ArgumentConfig{Type: orderInput, DefaultValue: map[string]interface{}{"field": "AUTHORED_ORDER", "direction": "ASC"}},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, _ := p.Source.(map[string]interface{})
			pollID, _ := source["discussionID"].(int)
			viewer, _ := source["viewerID"].(int)
			poll := s.store.GetDiscussionPoll(pollID)
			if poll == nil {
				return nil, nil
			}
			options := append([]*store.DiscussionPollOption(nil), poll.Options...)
			if orderBy, _ := p.Args["orderBy"].(map[string]interface{}); orderBy != nil {
				field, _ := orderBy["field"].(string)
				direction, _ := orderBy["direction"].(string)
				if field == "VOTE_COUNT" {
					counts := map[int]int{}
					for _, chosen := range poll.VotesByUser {
						counts[chosen]++
					}
					sort.SliceStable(options, func(i, j int) bool {
						return counts[options[i].ID] < counts[options[j].ID]
					})
				}
				if direction == "DESC" {
					for i, j := 0, len(options)-1; i < j; i, j = i+1, j-1 {
						options[i], options[j] = options[j], options[i]
					}
				}
			}
			items := make([]gqlConnItem, 0, len(options))
			for _, o := range options {
				option := o
				items = append(items, gqlConnItem{
					identity: option.NodeID,
					render:   func() map[string]interface{} { return discussionPollOptionSource(poll, option, viewer) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// DiscussionPoll.discussion — the discussion the poll belongs to (nullable in
	// GitHub's schema). The poll source carries the discussion id.
	pollType.AddFieldConfig("discussion", &graphql.Field{
		Type: discussionType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, _ := p.Source.(map[string]interface{})
			discussionID, _ := source["discussionID"].(int)
			d := s.store.GetDiscussion(discussionID)
			return optionalObject(discussionToGQL(d, s.store)), nil
		},
	})

	// DiscussionPollOption.poll — the poll the option belongs to (nullable). The
	// option source carries the discussion id, which keys the poll in the store.
	optionType.AddFieldConfig("poll", &graphql.Field{
		Type: pollType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, _ := p.Source.(map[string]interface{})
			discussionID, _ := source["discussionID"].(int)
			poll := s.store.GetDiscussionPoll(discussionID)
			if poll == nil {
				return nil, nil
			}
			viewer, _ := source["viewerID"].(int)
			canVote := s.ghUserFromContext(p.Context) != nil
			return s.discussionPollSource(poll, viewer, canVote), nil
		},
	})

	discussionType.AddFieldConfig("poll", &graphql.Field{
		Type: pollType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, _ := p.Source.(map[string]interface{})
			discussionID, _ := source["databaseId"].(int)
			poll := s.store.GetDiscussionPoll(discussionID)
			if poll == nil {
				return nil, nil
			}
			viewer := 0
			canVote := false
			if user := s.ghUserFromContext(p.Context); user != nil {
				viewer = user.ID
				canVote = true
			}
			return s.discussionPollSource(poll, viewer, canVote), nil
		},
	})

	voteInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AddDiscussionPollVoteInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"pollOptionId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	votePayload := graphql.NewObject(graphql.ObjectConfig{
		Name: "AddDiscussionPollVotePayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"pollOption":       &graphql.Field{Type: optionType},
		},
	})
	s.registerMutation(mutationType, "addDiscussionPollVote", &graphql.Field{
		Type: votePayload,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(voteInput)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID := str(input["pollOptionId"])
			option, poll := store.FindDiscussionPollOptionByNodeID(s.store, nodeID)
			if option == nil || poll == nil {
				return nil, gqlMissingNode("DiscussionPollOption", nodeID)
			}
			if !s.store.CastDiscussionPollVote(poll.ID, option.ID, user.ID) {
				return nil, fmt.Errorf("the vote could not be recorded")
			}
			fresh := s.store.GetDiscussionPoll(poll.DiscussionID)
			var rendered interface{}
			if fresh != nil {
				for _, refreshed := range fresh.Options {
					if refreshed.ID == option.ID {
						rendered = discussionPollOptionSource(fresh, refreshed, user.ID)
					}
				}
			}
			return map[string]interface{}{
				"clientMutationId": input["clientMutationId"],
				"pollOption":       rendered,
			}, nil
		},
	})
}
