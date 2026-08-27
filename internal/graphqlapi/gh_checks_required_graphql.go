package graphqlapi

import (
	"fmt"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// StatusContext.isRequired and CheckRun.isRequired back `gh pr checks`'s
// "required" column. "Required" is a property of a check relative to one pull
// request's base branch, so both resolvers answer from branch protection
// through the same seam the merge gate reads.

// requirableByPullRequestArgs is GitHub's argument pair, shared by the interface
// and both implementations so they cannot drift.
func requirableByPullRequestArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"pullRequestId":     &graphql.ArgumentConfig{Type: graphql.ID},
		"pullRequestNumber": &graphql.ArgumentConfig{Type: graphql.Int},
	}
}

// gqlRequirableByPullRequestInterface returns GitHub's RequirableByPullRequest
// interface (memoized), implemented by StatusContext and CheckRun.
func (s *Resolver) gqlRequirableByPullRequestInterface() *graphql.Interface {
	if s.graphqlTypes.requirableByPullRequest != nil {
		return s.graphqlTypes.requirableByPullRequest
	}
	s.graphqlTypes.requirableByPullRequest = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "RequirableByPullRequest",
		Fields: graphql.Fields{
			"isRequired": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Args: requirableByPullRequestArgs(),
			},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name == "StatusContext" {
				return s.graphqlTypes.statusContext
			}
			return s.graphqlTypes.checkRun
		},
	})
	return s.graphqlTypes.requirableByPullRequest
}

// gqlIsRequiredField builds the isRequired field for one rollup member. nameKey
// names the source member holding branch protection's identifier: a status
// context's `context`, a check run's `name`.
func (s *Resolver) gqlIsRequiredField(nameKey string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Args: requirableByPullRequestArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			context, _ := source[nameKey].(string)
			repoKey, _ := source["repoKey"].(string)
			repo, pullRequest, err := s.requirableSubjectPullRequest(p, repoKey)
			if err != nil {
				return nil, err
			}
			if repo == nil || pullRequest == nil {
				return false, nil
			}
			for _, required := range s.requiredStatusCheckContexts(repo, pullRequest.BaseRefName) {
				if required == context {
					return true, nil
				}
			}
			return false, nil
		},
	}
}

// requirableSubjectPullRequest resolves the pull request isRequired is asked
// about, by node id or number; one is required, since without a pull request
// there is no base branch to answer against.
func (s *Resolver) requirableSubjectPullRequest(p graphql.ResolveParams, repoKey string) (*store.Repo, *store.PullRequest, error) {
	nodeID, _ := p.Args["pullRequestId"].(string)
	number, hasNumber := intArg(p.Args, "pullRequestNumber")
	if nodeID == "" && !hasNumber {
		return nil, nil, fmt.Errorf("isRequired needs one of pullRequestId or pullRequestNumber")
	}
	owner, name, ok := store.SplitRepoFullName(repoKey)
	if !ok {
		return nil, nil, nil
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
		return nil, nil, nil
	}
	if nodeID != "" {
		pullRequest := store.FindPullRequestByNodeID(s.store, nodeID)
		// A pull request in another repository would let one repo's
		// protection rules describe another's.
		if pullRequest == nil || pullRequest.RepoID != repo.ID {
			return nil, nil, nil
		}
		return repo, s.store.GetPullRequest(pullRequest.ID), nil
	}
	return repo, s.store.GetPullRequestByNumber(repo.ID, number), nil
}
