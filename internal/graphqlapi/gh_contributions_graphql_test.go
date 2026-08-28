package graphqlapi

import (
	"context"
	"testing"
	"time"

	"github.com/graphql-go/graphql"
)

// TestContributionsCollectionAggregate seeds real issues, a pull request, a
// created repository, and an authored commit for a user, then checks the
// aggregate the ContributionsCollection source computes against those seeds,
// and finally runs a GraphQL document over the built type to prove the whole
// type graph resolves.
func TestContributionsCollectionAggregate(t *testing.T) {
	h := newAccountHarness(t)
	user := h.user("contributor")

	repo := h.store.CreateRepo(user, "project", "", false)
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}

	// One commit authored by the user (commitRepoFiles signs with
	// <owner>@bleephub.test, which is exactly this user's email). It also gives
	// the default branch a commit, so a pull request can resolve its base SHA.
	h.commitRepoFiles(repo, map[string]string{"README.md": "hello"})

	if h.store.CreateIssue(repo.ID, user.ID, "first bug", "body", nil, nil, 0) == nil {
		t.Fatal("CreateIssue returned nil")
	}
	if h.store.CreateIssue(repo.ID, user.ID, "second bug", "body", nil, nil, 0) == nil {
		t.Fatal("CreateIssue returned nil")
	}
	// One pull request opened by the user. Head and base both resolve to the
	// seeded default branch so the store accepts it (a real branch topology is
	// not what this aggregate test exercises).
	pr := h.store.CreatePullRequest(repo.ID, user.ID, "a change", "body", "main", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("CreatePullRequest returned nil")
	}

	src := h.res.contributionsCollectionSource(user.ID, time.Time{}, time.Time{}, 0)

	if got := len(sourceNodes(src, "_issueNodes")); got != 2 {
		t.Fatalf("issue contributions = %d, want 2", got)
	}
	if got := len(sourceNodes(src, "_prNodes")); got != 1 {
		t.Fatalf("pull request contributions = %d, want 1", got)
	}
	if got := len(sourceNodes(src, "_repoNodes")); got != 1 {
		t.Fatalf("repository contributions = %d, want 1", got)
	}
	if got, _ := src["totalCommitContributions"].(int); got != 1 {
		t.Fatalf("total commit contributions = %d, want 1", got)
	}
	// Calendar total = commits + issues + PRs + reviews, all on the same day.
	calendar, _ := src["contributionCalendar"].(map[string]interface{})
	if got, _ := calendar["totalContributions"].(int); got != 4 {
		t.Fatalf("calendar total contributions = %d, want 4", got)
	}
	if !src["hasAnyContributions"].(bool) {
		t.Fatal("hasAnyContributions = false, want true")
	}

	document := `{
		cc {
			totalCommitContributions
			totalIssueContributions
			totalPullRequestContributions
			totalRepositoryContributions
			totalPullRequestReviewContributions
			hasAnyContributions
			contributionYears
			user { login }
			contributionCalendar { totalContributions weeks { contributionDays { contributionCount contributionLevel color weekday } } }
			issueContributions(first: 10) { totalCount nodes { occurredAt issue { title } } }
			repositoryContributions(first: 10) { totalCount nodes { repository { name } } }
			commitContributionsByRepository { repository { name } contributions { totalCount nodes { commitCount } } }
			firstIssueContribution { __typename ... on CreatedIssueContribution { issue { title } } }
		}
	}`

	data := runContributionsQuery(t, h.res, user.ID, document)
	cc := data["cc"].(map[string]interface{})
	if cc["totalIssueContributions"].(int) != 2 {
		t.Fatalf("graphql totalIssueContributions = %v, want 2", cc["totalIssueContributions"])
	}
	if cc["totalRepositoryContributions"].(int) != 1 {
		t.Fatalf("graphql totalRepositoryContributions = %v, want 1", cc["totalRepositoryContributions"])
	}
	issueConn := cc["issueContributions"].(map[string]interface{})
	if issueConn["totalCount"].(int) != 2 {
		t.Fatalf("graphql issueContributions.totalCount = %v, want 2", issueConn["totalCount"])
	}
	byRepo := cc["commitContributionsByRepository"].([]interface{})
	if len(byRepo) != 1 {
		t.Fatalf("commitContributionsByRepository groups = %d, want 1", len(byRepo))
	}
}

// TestContributionsCollectionTypeSignature confirms the built type carries the
// exact field set GitHub declares — no invented fields, none omitted.
func TestContributionsCollectionTypeSignature(t *testing.T) {
	h := newAccountHarness(t)
	cc := h.res.gqlContributionsCollectionType()
	if cc.Name() != "ContributionsCollection" {
		t.Fatalf("type name = %q", cc.Name())
	}
	want := []string{
		"commitContributionsByRepository", "contributionCalendar", "contributionYears",
		"doesEndInCurrentMonth", "earliestRestrictedContributionDate", "endedAt",
		"firstIssueContribution", "firstPullRequestContribution", "firstRepositoryContribution",
		"hasActivityInThePast", "hasAnyContributions", "hasAnyRestrictedContributions",
		"isSingleDay", "issueContributions", "issueContributionsByRepository",
		"joinedGitHubContribution", "latestRestrictedContributionDate",
		"mostRecentCollectionWithActivity", "mostRecentCollectionWithoutActivity",
		"popularIssueContribution", "popularPullRequestContribution",
		"pullRequestContributions", "pullRequestContributionsByRepository",
		"pullRequestReviewContributions", "pullRequestReviewContributionsByRepository",
		"repositoryContributions", "restrictedContributionsCount", "startedAt",
		"totalCommitContributions", "totalIssueContributions", "totalPullRequestContributions",
		"totalPullRequestReviewContributions", "totalRepositoriesWithContributedCommits",
		"totalRepositoriesWithContributedIssues", "totalRepositoriesWithContributedPullRequestReviews",
		"totalRepositoriesWithContributedPullRequests", "totalRepositoryContributions", "user",
	}
	fields := cc.Fields()
	if len(fields) != len(want) {
		t.Fatalf("field count = %d, want %d", len(fields), len(want))
	}
	for _, name := range want {
		if _, ok := fields[name]; !ok {
			t.Errorf("missing field %q", name)
		}
	}
}

// runContributionsQuery executes a document against a throwaway schema whose
// single root field returns the contributions source for the user. It reuses
// the resolver's already-built User/Repository/Issue/PullRequest types.
func runContributionsQuery(t *testing.T, res *Resolver, userID int, document string) map[string]interface{} {
	t.Helper()
	query := graphql.NewObject(graphql.ObjectConfig{
		Name: "ContributionsProbeQuery",
		Fields: graphql.Fields{
			"cc": &graphql.Field{
				Type: graphql.NewNonNull(res.gqlContributionsCollectionType()),
				Resolve: func(graphql.ResolveParams) (interface{}, error) {
					return res.contributionsCollectionSource(userID, time.Time{}, time.Time{}, 0), nil
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: query})
	if err != nil {
		t.Fatalf("build probe schema: %v", err)
	}
	result := graphql.Do(graphql.Params{
		Schema:        schema,
		RequestString: document,
		Context:       context.Background(),
	})
	if len(result.Errors) != 0 {
		t.Fatalf("graphql errors: %v", result.Errors)
	}
	data, _ := result.Data.(map[string]interface{})
	return data
}
