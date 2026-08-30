package bleephub

import (
	"net/http"
	"testing"
)

// TestOrgIssues_IncludePullRequests pins that GET /orgs/{org}/issues returns
// pull requests alongside issues, matching GitHub where every PR is an issue.
func TestOrgIssues_IncludePullRequests(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	admin := srv.store.UsersByLogin["admin"]
	org := srv.store.CreateOrg(admin, "orgpr-org", "Org PR Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	repo := srv.store.CreateOrgRepo(org, admin, "orgpr-repo", "", false)
	if repo == nil {
		t.Fatal("create org repo failed")
	}
	seedPullRequestBranches(t, srv.Server, repo, "feature")
	pr := srv.store.CreatePullRequest(repo.ID, admin.ID, "org pull request", "body",
		"feature", "main", false, nil, []int{admin.ID}, 0)
	if pr == nil {
		t.Fatal("failed to create pull request")
	}

	resp := srv.get(t, "/api/v3/orgs/orgpr-org/issues", defaultToken)
	var found map[string]interface{}
	for _, it := range decodeJSONArray(t, resp) {
		if it["number"] == float64(pr.Number) {
			found = it
			break
		}
	}
	if found == nil {
		t.Fatalf("org issues list did not include PR #%d", pr.Number)
	}
	if found["pull_request"] == nil {
		t.Fatalf("org PR row missing pull_request member: %v", found)
	}
}

func TestOrgIssues_ListForAuthenticatedUser(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	admin := srv.store.UsersByLogin["admin"]
	org := srv.store.CreateOrg(admin, "orgissues-org", "Org Issues Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	if srv.store.CreateOrgRepo(org, admin, "orgissues-repo", "", false) == nil {
		t.Fatal("create org repo failed")
	}

	// One issue assigned to the caller, one merely authored by them.
	resp := srv.post(t, "/api/v3/repos/orgissues-org/orgissues-repo/issues", defaultToken, map[string]interface{}{
		"title":     "assigned to admin",
		"assignees": []string{"admin"},
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create assigned issue: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = srv.post(t, "/api/v3/repos/orgissues-org/orgissues-repo/issues", defaultToken, map[string]interface{}{
		"title": "authored by admin",
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create authored issue: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The default filter=assigned returns only the assigned issue.
	resp = srv.get(t, "/api/v3/orgs/orgissues-org/issues", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list org issues: %d", resp.StatusCode)
	}
	assigned := decodeJSONArray(t, resp)
	if len(assigned) != 1 || assigned[0]["title"] != "assigned to admin" {
		t.Fatalf("assigned filter wrong: %v", assigned)
	}
	repoJSON, _ := assigned[0]["repository"].(map[string]interface{})
	if repoJSON == nil || repoJSON["full_name"] != "orgissues-org/orgissues-repo" {
		t.Fatalf("issue repository missing: %v", assigned[0])
	}

	resp = srv.get(t, "/api/v3/orgs/orgissues-org/issues?filter=created", defaultToken)
	created := decodeJSONArray(t, resp)
	if len(created) != 2 {
		t.Fatalf("created filter = %d issues, want 2", len(created))
	}

	resp = srv.patch(t, "/api/v3/repos/orgissues-org/orgissues-repo/issues/2", defaultToken, map[string]interface{}{"state": "closed"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("close issue: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = srv.get(t, "/api/v3/orgs/orgissues-org/issues?filter=all&state=open", defaultToken)
	open := decodeJSONArray(t, resp)
	if len(open) != 1 || open[0]["title"] != "assigned to admin" {
		t.Fatalf("open filter wrong: %v", open)
	}
	resp = srv.get(t, "/api/v3/orgs/orgissues-org/issues?filter=all&state=closed", defaultToken)
	closed := decodeJSONArray(t, resp)
	if len(closed) != 1 || closed[0]["title"] != "authored by admin" {
		t.Fatalf("closed filter wrong: %v", closed)
	}

	resp = srv.get(t, "/api/v3/orgs/no-such-issues-org/issues", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown org issues: %d, want 404", resp.StatusCode)
	}
	resp = srv.get(t, "/api/v3/orgs/orgissues-org/issues", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated org issues: %d, want 401", resp.StatusCode)
	}
}
