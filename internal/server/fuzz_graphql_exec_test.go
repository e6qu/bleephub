package bleephub

import (
	"testing"

	"github.com/graphql-go/graphql"
)

// FuzzGraphQLExecution runs arbitrary queries through the real schema against a
// store that actually holds a repository, an issue, and a pull request, so the
// mutation exercises resolvers with data present (the path where a typed-nil or
// unchecked assertion would panic) rather than only the parser. Any query must
// resolve to a GraphQL result or error, never a panic.
func FuzzGraphQLExecution(f *testing.F) {
	s := newTestServer()
	s.graphql = s.newGraphQLResolver()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "fuzzrepo", "", false)
	if repo != nil {
		s.store.CreateIssue(repo.ID, admin.ID, "seed issue", "body @admin", nil, nil, 0)
		seedStorePullRequestBranches(f, s.store, repo, "feature")
		s.store.CreatePullRequest(repo.ID, admin.ID, "seed pr", "b", "feature", "main", false, nil, nil, 0)
	}

	f.Add(`{repository(owner:"admin",name:"fuzzrepo"){issues(first:10){nodes{title}}}}`)
	f.Add(`{repository(owner:"admin",name:"fuzzrepo"){pullRequests(first:5){nodes{number}}}}`)
	f.Add(`{search(query:"seed",type:ISSUE,first:5){nodes{__typename}}}`)
	f.Add(`{node(id:"x"){id}}`)
	f.Add(`{viewer{login repositories(first:100){nodes{name}}}}`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, q string) {
		_ = graphql.Do(graphql.Params{Schema: s.graphql.Schema(), RequestString: q})
	})
}
