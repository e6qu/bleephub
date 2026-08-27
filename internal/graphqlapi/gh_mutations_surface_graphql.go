package graphqlapi

// addGitHubMutationSurface installs the remainder of GitHub's Mutation type.
// It must run after every read family: each payload here returns an object one
// of them defines, and registering earlier would mint a duplicate object
// graphql-go refuses.

import "github.com/graphql-go/graphql"

func (s *Resolver) addGitHubMutationSurface(mutationType *graphql.Object) {
	s.addRepositoryMetadataMutations(mutationType)
	s.addAccountActivityMutations(mutationType)
	s.addIssueSurfaceMutations(mutationType)
	s.addPullRequestSurfaceMutations(mutationType)
	s.addActivityMutations(mutationType)
}
