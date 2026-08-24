package graphqlapi

import (
	"fmt"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// StatusContext.isRequired and CheckRun.isRequired — the two fields `gh pr
// checks` selects on every rollup context to decide which line of its table is
// marked "required". Without them the command's whole document fails
// validation and the subcommand is unusable.
//
// GitHub declares the field on the RequirableByPullRequest interface with two
// optional arguments, because "required" is not a property of a check: it is a
// property of a check with respect to one pull request's base branch. Both
// resolvers therefore answer from the repository's branch protection through
// the same seam the merge gate reads, so what the table marks required is what
// the merge would actually block on.

// requirableByPullRequestArgs is GitHub's argument pair, shared by the
// interface declaration and both implementations so they cannot drift (an
// object whose argument types differ from its interface's fails schema
// assembly).
func requirableByPullRequestArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"pullRequestId":     &graphql.ArgumentConfig{Type: graphql.ID},
		"pullRequestNumber": &graphql.ArgumentConfig{Type: graphql.Int},
	}
}

// gqlRequirableByPullRequestInterface returns GitHub's
// RequirableByPullRequest interface (memoized). StatusContext and CheckRun
// implement it; the registry entries its ResolveType reads are populated by
// the time any query executes.
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

// gqlIsRequiredField builds the isRequired field for one rollup member.
// nameKey names the source member carrying the identifier branch protection
// expresses its requirement in: a status context is required by its `context`,
// a check run by its `name` — which is the same pair requiredCheckContexts
// matches when it decides whether a merge may proceed.
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
// about. GitHub accepts either the node id or the number, and requires one of
// them: without a pull request there is no base branch, so there is no answer
// to give — which is refused rather than reported as "not required".
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
		// A pull request in another repository says nothing about this
		// commit's checks, and answering from it would let one repository's
		// protection rules describe another's.
		if pullRequest == nil || pullRequest.RepoID != repo.ID {
			return nil, nil, nil
		}
		return repo, s.store.GetPullRequest(pullRequest.ID), nil
	}
	return repo, s.store.GetPullRequestByNumber(repo.ID, number), nil
}
