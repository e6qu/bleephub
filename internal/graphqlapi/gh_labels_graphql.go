package graphqlapi

import (
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// The three label mutations GitHub exposes over its Labelable interface:
// addLabelsToLabelable, removeLabelsFromLabelable and clearLabelsFromLabelable.
// `gh issue edit --add-label` / `--remove-label` speak only these — the REST
// label routes exist, but gh does not use them for editing, so an instance
// without the mutations fails the command outright rather than degrading.
//
// All three go through the store's Set*Labels primitives, which record the
// labeled/unlabeled timeline events, and then through the same webhook
// change fan-out the REST handlers use, so a label applied through GraphQL is
// indistinguishable from one applied through REST.

// gqlLabelableInterface returns GitHub's Labelable interface (memoized), with
// the label connection Issue and PullRequest already expose. It has to exist
// before either concrete type is constructed: graphql-go reads an object's
// interface list once, so a type that does not claim Labelable at
// construction can never become one of its possible types.
func (s *Resolver) gqlLabelableInterface() *graphql.Interface {
	if s.graphqlTypes.labelable != nil {
		return s.graphqlTypes.labelable
	}
	s.graphqlTypes.labelable = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Labelable",
		Fields: graphql.Fields{
			"labels": &graphql.Field{
				Type: s.gqlLabelConnectionType(),
				Args: graphql.FieldConfigArgument{
					"first": &graphql.ArgumentConfig{Type: graphql.Int},
					"after": &graphql.ArgumentConfig{Type: graphql.String},
				},
			},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			nodeID, _ := source["nodeID"].(string)
			if strings.HasPrefix(nodeID, "PR_") {
				return s.graphqlTypes.pullRequest
			}
			return s.graphqlTypes.issue
		},
	})
	return s.graphqlTypes.labelable
}

// addLabelMutationsToSchema registers the three Labelable mutations.
func (s *Resolver) addLabelMutationsToSchema(mutationType *graphql.Object) {
	labelable := s.gqlLabelableInterface()
	labelIDList := graphql.NewList(graphql.NewNonNull(graphql.ID))

	addInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AddLabelsToLabelableInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"labelableId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"labelIds":    &graphql.InputObjectFieldConfig{Type: labelIDList},
		},
	})
	removeInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RemoveLabelsFromLabelableInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"labelableId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"labelIds":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(labelIDList)},
		},
	})
	clearInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ClearLabelsFromLabelableInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"labelableId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	payloadType := func(name string) *graphql.Object {
		return graphql.NewObject(graphql.ObjectConfig{
			Name:   name,
			Fields: graphql.Fields{"labelable": &graphql.Field{Type: labelable}},
		})
	}

	s.registerMutation(mutationType, "addLabelsToLabelable", &graphql.Field{
		Type: payloadType("AddLabelsToLabelablePayload"),
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveLabelChange(p, func(existing, named []int) []int {
				return unionLabelIDs(existing, named)
			})
		},
	})

	s.registerMutation(mutationType, "removeLabelsFromLabelable", &graphql.Field{
		Type: payloadType("RemoveLabelsFromLabelablePayload"),
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(removeInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveLabelChange(p, func(existing, named []int) []int {
				return withoutLabelIDs(existing, named)
			})
		},
	})

	s.registerMutation(mutationType, "clearLabelsFromLabelable", &graphql.Field{
		Type: payloadType("ClearLabelsFromLabelablePayload"),
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(clearInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveLabelChange(p, func([]int, []int) []int { return nil })
		},
	})
}

// resolveLabelChange is the body all three mutations share. combine receives
// the subject's current label ids and the ids the input named, and returns the
// set the subject should end up with — which is then written through the
// store's replace-the-set primitive so the labeled/unlabeled deltas are
// recorded once, whichever mutation asked.
func (s *Resolver) resolveLabelChange(p graphql.ResolveParams, combine func(existing, named []int) []int) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := input["labelableId"].(string)
	user := s.ghUserFromContext(p.Context)

	if issue := store.FindIssueByNodeID(s.store, nodeID); issue != nil {
		repo := s.store.GetRepoByID(issue.RepoID)
		if repo == nil {
			return nil, gqlMissingNodeType("Repository")
		}
		// The node finders hand back the live row; the label set the delta is
		// computed against has to be a snapshot taken before the write.
		before := s.store.GetIssue(issue.ID)
		if before == nil {
			return nil, gqlMissingNodeType("Issue")
		}
		named, err := resolveGQLLabelIDs(s.store, repo.ID, input["labelIds"])
		if err != nil {
			return nil, err
		}
		next := combine(before.LabelIDs, derefLabelIDs(named))
		s.store.SetIssueLabels(repo.ID, before.Number, next, user.ID)
		updated := s.store.GetIssue(issue.ID)
		if updated == nil {
			return nil, gqlMissingNodeType("Issue")
		}
		s.emitIssueChanges(repo, updated, user, store.SubjectChange{
			LabelsFrom: before.LabelIDs,
			LabelsTo:   &next,
		})
		return map[string]interface{}{"labelable": issueToGQL(updated, s.store)}, nil
	}

	if pullRequest := store.FindPullRequestByNodeID(s.store, nodeID); pullRequest != nil {
		repo := s.store.GetRepoByID(pullRequest.RepoID)
		if repo == nil {
			return nil, gqlMissingNodeType("Repository")
		}
		before := s.store.GetPullRequest(pullRequest.ID)
		if before == nil {
			return nil, gqlMissingNodeType("PullRequest")
		}
		named, err := resolveGQLLabelIDs(s.store, repo.ID, input["labelIds"])
		if err != nil {
			return nil, err
		}
		next := combine(before.LabelIDs, derefLabelIDs(named))
		s.store.SetPullRequestLabels(repo.ID, before.Number, next, user.ID)
		updated := s.store.GetPullRequest(pullRequest.ID)
		if updated == nil {
			return nil, gqlMissingNodeType("PullRequest")
		}
		s.emitPullRequestChanges(repo, updated, user, store.SubjectChange{
			LabelsFrom: before.LabelIDs,
			LabelsTo:   &next,
		})
		return map[string]interface{}{"labelable": pullRequestToGQL(updated, s.store)}, nil
	}

	return nil, gqlMissingNode("Labelable", nodeID)
}

// derefLabelIDs flattens resolveGQLLabelIDs's "absent means leave alone"
// pointer into the empty set: for these mutations an absent labelIds names no
// label rather than declining to say.
func derefLabelIDs(ids *[]int) []int {
	if ids == nil {
		return nil
	}
	return *ids
}

// unionLabelIDs appends the named ids the subject does not already carry,
// preserving the existing order so an add does not reshuffle the label list.
func unionLabelIDs(existing, named []int) []int {
	out := append([]int(nil), existing...)
	present := make(map[int]bool, len(existing))
	for _, id := range existing {
		present[id] = true
	}
	for _, id := range named {
		if !present[id] {
			present[id] = true
			out = append(out, id)
		}
	}
	return out
}

// withoutLabelIDs removes the named ids from the subject's set.
func withoutLabelIDs(existing, named []int) []int {
	drop := make(map[int]bool, len(named))
	for _, id := range named {
		drop[id] = true
	}
	out := make([]int, 0, len(existing))
	for _, id := range existing {
		if !drop[id] {
			out = append(out, id)
		}
	}
	return out
}

// labelableMutationTarget resolves the repository an addLabelsToLabelable /
// removeLabelsFromLabelable / clearLabelsFromLabelable input names. It is
// mutationTargetIssueOrPullRequest under the key these three use, kept
// separate only so the refusal names the interface the caller addressed.
func labelableMutationTarget(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	issueOrPR := mutationTargetIssueOrPullRequest(key)
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		target := issueOrPR(s, input)
		if target.repo == nil {
			nodeID, _ := input[key].(string)
			target.missing = gqlMissingNode("Labelable", nodeID)
		}
		return target
	}
}
