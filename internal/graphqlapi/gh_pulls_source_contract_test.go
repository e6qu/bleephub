package graphqlapi

import (
	"os"
	"strings"
	"testing"
)

// Source contract moved with gh_pulls_graphql.go (ARCH-003).
func TestGraphQLPullRequestConverterDoesNotReenterStoreForGitStorage(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("gh_pulls_graphql.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if strings.Contains(body, "pullRequestCommitObjects(st,") || strings.Contains(body, "pullRequestCommitObjects(s.store,") {
		t.Fatal("pull request GraphQL conversion must not call store-locking commit helpers while rendering under Store.mu")
	}
}
