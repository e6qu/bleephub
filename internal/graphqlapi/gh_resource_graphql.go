package graphqlapi

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Query.resource — GitHub's URL-to-node lookup.
//
// It is how a client turns something a person pasted into the node id an API
// call needs: `gh project item-add --url https://host/owner/repo/issues/7`
// resolves the issue through this field before calling addProjectV2ItemById.
// Without it that command could not run at all, whatever the Projects v2
// surface supported.

// uniformResourceLocatableInterface is GitHub's UniformResourceLocatable.
//
// It is built before Repository, Issue and PullRequest are, because graphql-go
// reads an object's interface list once and memoizes it: an interface a type
// does not claim at construction can never gain it as a possible type, and the
// `... on Issue` spread `gh project item-add` sends would fail validation.
func (s *Resolver) uniformResourceLocatableInterface() *graphql.Interface {
	if s.graphqlTypes.uniformResourceLocatable != nil {
		return s.graphqlTypes.uniformResourceLocatable
	}
	uri := s.graphQLStringScalar("URI")
	s.graphqlTypes.uniformResourceLocatable = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "UniformResourceLocatable",
		Fields: graphql.Fields{
			"resourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			name, _ := source["__typename"].(string)
			return s.graphqlTypes.resourceNodeTypes[name]
		},
	})
	return s.graphqlTypes.uniformResourceLocatable
}

// addResourceFieldToSchema installs Query.resource over the
// UniformResourceLocatable interface, whose implementations are the node types
// a bleephub URL can name: a repository, an issue, or a pull request.
func (s *Resolver) addResourceFieldToSchema(queryType *graphql.Object, nodeTypes map[string]*graphql.Object) {
	s.graphqlTypes.resourceNodeTypes = nodeTypes
	queryType.AddFieldConfig("resource", &graphql.Field{
		Type: s.uniformResourceLocatableInterface(),
		Args: graphql.FieldConfigArgument{
			"url": &graphql.ArgumentConfig{Type: graphql.NewNonNull(s.graphQLStringScalar("URI"))},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			raw, _ := p.Args["url"].(string)
			owner, name, kind, number, ok := parseResourceURL(raw)
			if !ok {
				return nil, nil
			}
			repo := s.store.GetRepoByFullName(owner + "/" + name)
			// A repository the caller cannot read resolves to null, exactly as
			// one that does not exist does: the lookup must not become a way
			// to confirm a private repository by pasting its URL.
			if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			switch kind {
			case "":
				source := repoToGraphQL(s.store, repo)
				source["__typename"] = "Repository"
				return source, nil
			case "issues":
				issue := s.store.GetIssueByNumber(repo.ID, number)
				if issue == nil {
					// GitHub serves /issues/{n} for a pull request too, since
					// pull requests are issues underneath.
					if pr := s.store.GetPullRequestByNumber(repo.ID, number); pr != nil {
						return resourcePullRequest(s.store, pr), nil
					}
					return nil, nil
				}
				source := issueToGQL(issue, s.store)
				source["__typename"] = "Issue"
				return source, nil
			case "pull":
				pr := s.store.GetPullRequestByNumber(repo.ID, number)
				if pr == nil {
					return nil, nil
				}
				return resourcePullRequest(s.store, pr), nil
			}
			return nil, nil
		},
	})
}

func resourcePullRequest(st *store.Store, pr *store.PullRequest) map[string]interface{} {
	source := pullRequestToGQL(pr, st)
	source["__typename"] = "PullRequest"
	return source
}

// parseResourceURL picks the owner, repository, subject kind and number out of
// a bleephub web URL. It accepts the three shapes a URL can name — a
// repository, an issue and a pull request — and rejects everything else by
// reporting ok=false, which the resolver answers with null.
//
// The host is deliberately not checked: an instance is reachable under several
// names (its external URL, a proxy, localhost in tests), and refusing a URL
// whose host does not match the one this process happens to know would reject
// URLs the same instance served.
func parseResourceURL(raw string) (owner, name, kind string, number int, ok bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", "", 0, false
	}
	segments := make([]string, 0, 4)
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	if len(segments) < 2 {
		return "", "", "", 0, false
	}
	owner, name = segments[0], segments[1]
	if len(segments) == 2 {
		return owner, name, "", 0, true
	}
	if len(segments) < 4 {
		return "", "", "", 0, false
	}
	switch segments[2] {
	case "issues", "pull":
		kind = segments[2]
	default:
		return "", "", "", 0, false
	}
	number, err = strconv.Atoi(segments[3])
	if err != nil || number <= 0 {
		return "", "", "", 0, false
	}
	return owner, name, kind, number, true
}
