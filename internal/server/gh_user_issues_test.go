package bleephub

import (
	"testing"
)

func TestAuthenticatedUserIssues(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	user := srv.createTestUser(t, "issues-across-repos")
	token := srv.store.CreateToken(user.ID, "repo").Value
	repoKey := srv.createTestRepo(t)

	// An issue assigned to the user (default filter=assigned must return it).
	resp := srv.post(t, repoKey.path()+"/issues", defaultToken, map[string]interface{}{
		"title":     "assigned to target user",
		"assignees": []string{user.Login},
	})
	decodeJSONWithStatus(t, resp, 201)

	// An issue created by the user (filter=created).
	resp = srv.post(t, repoKey.path()+"/issues", token, map[string]interface{}{
		"title": "created by target user",
	})
	decodeJSONWithStatus(t, resp, 201)

	// Default filter: assigned, open.
	resp = srv.get(t, "/api/v3/issues", token)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("GET /issues status = %d", resp.StatusCode)
	}
	issues := decodeJSONArray(t, resp)
	if len(issues) != 1 || issues[0]["title"] != "assigned to target user" {
		t.Fatalf("assigned filter returned %d issues: %v", len(issues), issues)
	}
	// Cross-repo listings carry the repository member.
	repoJSON, _ := issues[0]["repository"].(map[string]interface{})
	if repoJSON == nil || repoJSON["full_name"] != repoKey.fullName() {
		t.Fatalf("issue repository = %v", issues[0]["repository"])
	}

	resp = srv.get(t, "/api/v3/issues?filter=created", token)
	issues = decodeJSONArray(t, resp)
	if len(issues) != 1 || issues[0]["title"] != "created by target user" {
		t.Fatalf("created filter returned %d issues: %v", len(issues), issues)
	}

	// filter=all returns both involvements, newest first by default.
	resp = srv.get(t, "/api/v3/issues?filter=all", token)
	issues = decodeJSONArray(t, resp)
	if len(issues) != 2 {
		t.Fatalf("all filter returned %d issues, want 2", len(issues))
	}

	// state=closed excludes the open issues.
	resp = srv.get(t, "/api/v3/issues?state=closed", token)
	issues = decodeJSONArray(t, resp)
	if len(issues) != 0 {
		t.Fatalf("closed filter returned %d issues, want 0", len(issues))
	}

	// Unauthenticated → 401.
	resp = srv.get(t, "/api/v3/issues", "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	// Invalid filter → 422.
	resp = srv.get(t, "/api/v3/issues?filter=bogus", token)
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("invalid filter status = %d, want 422", resp.StatusCode)
	}
}

// TestGlobalIssueListsIncludePullRequests pins that GET /issues and
// GET /user/issues return pull requests alongside issues — every PR is an issue
// on GitHub — each carrying the pull_request member and a repository object.
func TestGlobalIssueListsIncludePullRequests(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	user := srv.createTestUser(t, "pr-in-global-issues")
	token := srv.store.CreateToken(user.ID, "repo").Value
	repoKey := srv.createTestRepo(t)
	repo := srv.store.GetRepoByFullName(repoKey.fullName())
	if repo == nil {
		t.Fatal("repo not found")
	}
	seedPullRequestBranches(t, srv.Server, repo, "feature")

	// A pull request authored by and assigned to the user.
	pr := srv.store.CreatePullRequest(repo.ID, user.ID, "my pull request", "body",
		"feature", "main", false, nil, []int{user.ID}, 0)
	if pr == nil {
		t.Fatal("failed to create pull request")
	}

	assertPRPresent := func(path string) {
		t.Helper()
		resp := srv.get(t, path, token)
		items := decodeJSONArray(t, resp)
		var found map[string]interface{}
		for _, it := range items {
			if it["number"] == float64(pr.Number) {
				found = it
				break
			}
		}
		if found == nil {
			t.Fatalf("%s did not include PR #%d: %v", path, pr.Number, items)
		}
		if found["pull_request"] == nil {
			t.Fatalf("%s PR row missing pull_request member: %v", path, found)
		}
		if found["repository"] == nil {
			t.Fatalf("%s PR row missing repository member", path)
		}
	}
	// Default filter=assigned and filter=created both surface it, on both the
	// all-repos (/issues → handleListGlobalUserIssues) and the account-scoped
	// (/user/issues → handleListAuthUserIssues) endpoints.
	assertPRPresent("/api/v3/issues")
	assertPRPresent("/api/v3/issues?filter=created")
	assertPRPresent("/api/v3/user/issues")
	assertPRPresent("/api/v3/user/issues?filter=created")

	// An open PR is excluded by state=closed.
	resp := srv.get(t, "/api/v3/issues?state=closed", token)
	for _, it := range decodeJSONArray(t, resp) {
		if it["number"] == float64(pr.Number) {
			t.Fatalf("state=closed unexpectedly included open PR: %v", it)
		}
	}
}

func TestAuthenticatedUserIssuesLabelFilter(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	user := srv.createTestUser(t, "issues-label-filter")
	token := srv.store.CreateToken(user.ID, "repo").Value
	repoKey := srv.createTestRepo(t)

	resp := srv.post(t, repoKey.path()+"/labels", defaultToken, map[string]interface{}{
		"name": "wanted-label", "color": "d73a4a",
	})
	decodeJSONWithStatus(t, resp, 201)

	resp = srv.post(t, repoKey.path()+"/issues", token, map[string]interface{}{
		"title":  "labelled issue",
		"labels": []string{"wanted-label"},
	})
	decodeJSONWithStatus(t, resp, 201)
	resp = srv.post(t, repoKey.path()+"/issues", token, map[string]interface{}{
		"title": "unlabelled issue",
	})
	decodeJSONWithStatus(t, resp, 201)

	resp = srv.get(t, "/api/v3/issues?filter=created&labels=wanted-label", token)
	issues := decodeJSONArray(t, resp)
	if len(issues) != 1 || issues[0]["title"] != "labelled issue" {
		t.Fatalf("label filter returned %d issues: %v", len(issues), issues)
	}
}
