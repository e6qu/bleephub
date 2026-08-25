package graphqlapi

import (
	"context"
	"sort"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// Repository search over the GraphQL `search` connection.
//
// The official schema declares REPOSITORY in `enum SearchType`, `Repository`
// in `union SearchResultItem`, and `repositoryCount: Int!` on
// SearchResultItemConnection. octokit's own repository search sends all three
// in one query, so any one of them missing fails the whole request.

// searchRepositories evaluates a repository search query against the
// repositories the viewer may read, newest first. It understands the
// qualifiers GitHub's repository search shares with the REST surface —
// user:/org:/fork:/is: — and treats every remaining bare token as a substring
// the name, full name or description must contain.
func (s *Resolver) searchRepositories(ctx context.Context, query string, viewer *store.User) []gqlConnItem {
	var owners, keywords []string
	visibility := ""
	includeForks := true
	forksOnly := false

	for _, token := range strings.Fields(query) {
		token = strings.Trim(token, "()")
		if token == "" || strings.EqualFold(token, "OR") || strings.EqualFold(token, "AND") {
			continue
		}
		key, value, isQualifier := strings.Cut(token, ":")
		if !isQualifier {
			keywords = append(keywords, strings.Trim(token, `"`))
			continue
		}
		value = strings.Trim(value, `"`)
		switch strings.ToLower(key) {
		case "user", "org", "owner":
			owners = append(owners, value)
		case "fork":
			switch strings.ToLower(value) {
			case "true":
				includeForks = true
			case "only":
				forksOnly = true
			case "false":
				includeForks = false
			}
		case "is":
			switch strings.ToLower(value) {
			case "public", "private", "internal":
				visibility = strings.ToLower(value)
			}
		default:
			// An unrecognized qualifier is not a keyword: matching "repo:x" as
			// the substring "repo:x" would silently return nothing rather than
			// admit the qualifier is unsupported.
		}
	}

	var matches []*store.Repo
	s.store.Mu.RLock()
	for _, repo := range s.store.Repos {
		if !repositoryMatchesSearch(repo, owners, keywords, visibility, includeForks, forksOnly) {
			continue
		}
		snapshot := *repo
		if repo.Owner != nil {
			owner := *repo.Owner
			snapshot.Owner = &owner
		}
		matches = append(matches, &snapshot)
	}
	s.store.Mu.RUnlock()

	// Authorization runs off the store lock: viewerCanReadRepo takes its own
	// locks, and a private repository must never be disclosed by a search.
	readable := matches[:0]
	for _, repo := range matches {
		if repo.Private && !s.viewerCanReadRepo(ctx, repo) {
			continue
		}
		readable = append(readable, repo)
	}
	sort.Slice(readable, func(a, b int) bool {
		if !readable[a].UpdatedAt.Equal(readable[b].UpdatedAt) {
			return readable[a].UpdatedAt.After(readable[b].UpdatedAt)
		}
		return readable[a].ID > readable[b].ID
	})

	items := make([]gqlConnItem, 0, len(readable))
	for _, repo := range readable {
		items = append(items, gqlConnItem{
			identity: repo.NodeID,
			render: func() map[string]interface{} {
				node := repoToGraphQL(s.store, repo)
				node["__typename"] = "Repository"
				return node
			},
		})
	}
	return items
}

// repositoryMatchesSearch evaluates the parsed query against one repository.
// Caller must hold s.store.Mu.
func repositoryMatchesSearch(repo *store.Repo, owners, keywords []string, visibility string, includeForks, forksOnly bool) bool {
	owner, _, _ := strings.Cut(repo.FullName, "/")
	if len(owners) > 0 {
		matched := false
		for _, want := range owners {
			if strings.EqualFold(owner, want) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if forksOnly && !repo.Fork {
		return false
	}
	if !includeForks && repo.Fork {
		return false
	}
	if visibility != "" && !strings.EqualFold(repo.Visibility, visibility) {
		return false
	}
	for _, keyword := range keywords {
		lowered := strings.ToLower(keyword)
		if !strings.Contains(strings.ToLower(repo.Name), lowered) &&
			!strings.Contains(strings.ToLower(repo.FullName), lowered) &&
			!strings.Contains(strings.ToLower(repo.Description), lowered) {
			return false
		}
	}
	return true
}
