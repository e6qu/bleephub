package graphqlapi

// addGitHubMutationSurface installs the remainder of GitHub's Mutation type.
//
// It runs after every read family, because each payload here returns an
// object one of them defines: a label mutation's payload is the shared Label,
// a repository mutation's payload the shared Repository, a review mutation's
// payload the shared PullRequestReview. Registering these earlier would mint
// a second object of the same name and graphql-go would refuse the schema.
//
// Each family is one file, and each mutation in it touches the same five
// places every other mutation does: the resolver here, a row in
// graphqlMutationAuthz, a refusal case and an entitled case in the server's
// mutation tables, and the introspection snapshot.

import "github.com/graphql-go/graphql"

func (s *Resolver) addGitHubMutationSurface(mutationType *graphql.Object) {
	s.addRepositoryMetadataMutations(mutationType)
	s.addAccountActivityMutations(mutationType)
	s.addIssueSurfaceMutations(mutationType)
	s.addPullRequestSurfaceMutations(mutationType)
	s.addActivityMutations(mutationType)
}
