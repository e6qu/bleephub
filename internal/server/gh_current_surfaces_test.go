package bleephub

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func requireHTTPStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body := decodeResponseBodyForTest(resp)
		t.Fatalf("%s %s = %d, want %d; body=%s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, body)
	}
}

func decodeResponseBodyForTest(resp *http.Response) string {
	defer resp.Body.Close()
	body := make([]byte, 64<<10)
	n, _ := resp.Body.Read(body)
	return string(body[:n])
}

func TestCurrentSecretScanningCustomPatternREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "current-pattern-org", "Current pattern org", "")
	repo := s.store.CreateOrgRepo(org, admin, "current-pattern-repo", "", true)

	for _, base := range []string{
		"/api/v3/repos/" + repo.FullName + "/secret-scanning/custom-patterns",
		"/api/v3/orgs/" + org.Login + "/secret-scanning/custom-patterns",
	} {
		resp := s.post(t, base, defaultToken, map[string]interface{}{
			"patterns": []map[string]interface{}{{
				"name":    "Internal token " + base,
				"pattern": `bleep_[0-9a-f]{16}`,
			}},
		})
		requireHTTPStatus(t, resp, http.StatusCreated)
		created := decodeJSON(t, resp)
		items := created["created_patterns"].([]interface{})
		pattern := items[0].(map[string]interface{})
		id := int(pattern["id"].(float64))
		version := pattern["custom_pattern_version"].(string)

		resp = s.get(t, base, defaultToken)
		requireHTTPStatus(t, resp, http.StatusOK)
		if got := decodeJSONArray(t, resp); len(got) != 1 {
			t.Fatalf("list %s returned %d patterns, want 1", base, len(got))
		}

		resp = s.patch(t, fmt.Sprintf("%s/%d", base, id), defaultToken, map[string]interface{}{
			"start_delimiter":        `(?:\A|\s)`,
			"custom_pattern_version": version,
		})
		requireHTTPStatus(t, resp, http.StatusOK)
		updated := decodeJSON(t, resp)
		if updated["start_delimiter"] != `(?:\A|\s)` {
			t.Fatalf("updated delimiter = %v", updated["start_delimiter"])
		}

		resp = s.patch(t, fmt.Sprintf("%s/%d", base, id), defaultToken, map[string]interface{}{
			"end_delimiter":          "[",
			"custom_pattern_version": updated["custom_pattern_version"],
		})
		requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
		resp.Body.Close()

		resp = s.do(t, "DELETE", base, defaultToken, map[string]interface{}{
			"patterns": []map[string]interface{}{{
				"pattern_id":             id,
				"custom_pattern_version": updated["custom_pattern_version"],
			}},
			"post_delete_action": "resolve_alerts",
		})
		requireHTTPStatus(t, resp, http.StatusNoContent)
		resp.Body.Close()
	}
}

func TestCurrentArtifactCodeQualityIssueTypeAndCopilotREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "current-read-org", "Current read org", "")
	repo := s.store.CreateOrgRepo(org, admin, "current-read-repo", "", false)
	description, color := "Work that spans several changes", "purple"
	issueType := s.store.CreateIssueType(org.Login, "Epic", &description, &color, true)

	resp := s.get(t, "/api/v3/repos/"+repo.FullName+"/issue-types", defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	types := decodeJSONArray(t, resp)
	if len(types) != 1 || int(types[0]["id"].(float64)) != issueType.ID {
		t.Fatalf("repository issue types = %#v", types)
	}

	s.store.PutCodeQualityFinding(&store.CodeQualityFinding{
		Number: 1, RepoKey: repo.FullName, State: "open",
		Rule: store.CodeQualityFindingRule{
			ID: "go/no-dead-code", Title: "Dead code", Description: "A declaration is unreachable.",
			Severity: "warning", Category: "maintainability",
		},
		Location: store.CodeQualityFindingLocation{Path: "main.go", StartLine: 8},
		Message:  store.CodeQualityFindingMessage{Text: "Unused declaration", Markdown: "**Unused declaration**"},
	})
	findingsBase := "/api/v3/repos/" + repo.FullName + "/code-quality/findings"
	resp = s.get(t, findingsBase+"?state=open", defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if findings := decodeJSONArray(t, resp); len(findings) != 1 {
		t.Fatalf("code quality findings = %#v", findings)
	}
	resp = s.get(t, findingsBase+"/1", defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if got := decodeJSON(t, resp); got["number"] != float64(1) {
		t.Fatalf("code quality finding = %#v", got)
	}

	digest := testSubjectDigest("current-cluster-deployment")
	jobsBase := "/api/v3/orgs/" + org.Login + "/artifacts/metadata/deployment-record/cluster/cluster-a/jobs"
	resp = s.post(t, jobsBase, defaultToken, map[string]interface{}{
		"logical_environment":  "production",
		"physical_environment": "eu-central-1",
		"deployments": []map[string]interface{}{{
			"name": "service", "digest": digest, "deployment_name": "service-primary",
			"github_repository": repo.Name, "runtime_risks": []string{"internet-exposed"},
		}},
	})
	requireHTTPStatus(t, resp, http.StatusAccepted)
	job := decodeJSON(t, resp)
	jobID := int(job["job_id"].(float64))
	resp = s.get(t, fmt.Sprintf("%s/%d", jobsBase, jobID), defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if got := decodeJSON(t, resp); got["status"] != "completed" || got["total_count"] != float64(1) {
		t.Fatalf("artifact deployment job = %#v", got)
	}

	day := "2000-06-14"
	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/copilot/metrics/reports/repos-1-day?day="+day, defaultToken)
	requireHTTPStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	resp = s.get(t, "/api/v3/enterprises/bleephub/copilot/metrics/reports/repos-1-day?day="+day, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if report := decodeJSON(t, resp); report["report_day"] != day {
		t.Fatalf("enterprise Copilot repository report = %#v", report)
	}
}

func TestCurrentPullRequestControlsStacksAndSuggestionsREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "current-pull-org", "Current pull org", "")
	repo := s.store.CreateOrgRepo(org, admin, "current-pull-repo", "", false)
	seedPullRequestBranches(t, s.Server, repo, "feature-a", "feature-b", "feature-c", "cap-check")

	first := s.store.CreatePullRequest(repo.ID, admin.ID, "First", "", "feature-a", "main", false, nil, nil, 0)
	second := s.store.CreatePullRequest(repo.ID, admin.ID, "Second", "", "feature-b", "feature-a", false, nil, nil, 0)
	third := s.store.CreatePullRequest(repo.ID, admin.ID, "Third", "", "feature-c", "feature-b", false, nil, nil, 0)
	if first == nil || second == nil || third == nil {
		t.Fatal("failed to create pull request stack fixtures")
	}

	capBase := "/api/v3/repos/" + repo.FullName + "/interaction-limits/pulls"
	resp := s.get(t, capBase+"/creation-cap", defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = s.patch(t, capBase+"/creation-cap", defaultToken, map[string]interface{}{
		"enabled": true, "max_open_pull_requests": 1,
	})
	requireHTTPStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = s.put(t, capBase+"/bypass-list", defaultToken, map[string]interface{}{"users": []string{admin.Login}})
	requireHTTPStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	resp = s.get(t, capBase+"/bypass-list", defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if users := decodeJSONArray(t, resp); len(users) != 1 || users[0]["login"] != admin.Login {
		t.Fatalf("pull request bypass list = %#v", users)
	}
	resp = s.do(t, "DELETE", capBase+"/bypass-list", defaultToken, map[string]interface{}{"users": []string{admin.Login}})
	requireHTTPStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	resp = s.post(t, "/api/v3/repos/"+repo.FullName+"/pulls", defaultToken, map[string]interface{}{
		"title": "Over the cap", "head": "cap-check", "base": "main",
	})
	requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
	resp = s.patch(t, capBase+"/creation-cap", defaultToken, map[string]interface{}{"enabled": false})
	requireHTTPStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	stackBase := "/api/v3/repos/" + repo.FullName + "/stacks"
	resp = s.post(t, stackBase, defaultToken, map[string]interface{}{"pull_requests": []int{first.Number, second.Number}})
	requireHTTPStatus(t, resp, http.StatusCreated)
	stack := decodeJSON(t, resp)
	number := int(stack["number"].(float64))
	resp = s.get(t, stackBase, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if stacks := decodeJSONArray(t, resp); len(stacks) != 1 {
		t.Fatalf("pull request stacks = %#v", stacks)
	}
	resp = s.get(t, fmt.Sprintf("%s/%d", stackBase, number), defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = s.post(t, fmt.Sprintf("%s/%d/add", stackBase, number), defaultToken, map[string]interface{}{"pull_requests": []int{third.Number}})
	requireHTTPStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = s.post(t, fmt.Sprintf("%s/%d/unstack", stackBase, number), defaultToken, nil)
	requireHTTPStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	issue := s.store.CreateIssue(repo.ID, admin.ID, "Suggested issue", "", nil, nil, 0)
	approve := s.store.CreateIssueSuggestion(repo.FullName, issue.ID, store.IssueSuggestion{Action: "close_issue"})
	dismiss := s.store.CreateIssueSuggestion(repo.FullName, issue.ID, store.IssueSuggestion{Action: "close_issue"})
	suggestionsBase := fmt.Sprintf("/api/v3/repos/%s/issues/%d/suggestions", repo.FullName, issue.Number)
	resp = s.get(t, suggestionsBase, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if suggestions := decodeJSONArray(t, resp); len(suggestions) != 2 {
		t.Fatalf("issue suggestions = %#v", suggestions)
	}
	resp = s.post(t, fmt.Sprintf("%s/%d/approve", suggestionsBase, approve.ID), defaultToken, nil)
	requireHTTPStatus(t, resp, http.StatusOK)
	if got := decodeJSON(t, resp); got["state"] != "approved" {
		t.Fatalf("approved suggestion = %#v", got)
	}
	resp = s.post(t, fmt.Sprintf("%s/%d/dismiss", suggestionsBase, dismiss.ID), defaultToken, nil)
	requireHTTPStatus(t, resp, http.StatusOK)
	if got := decodeJSON(t, resp); got["state"] != "dismissed" {
		t.Fatalf("dismissed suggestion = %#v", got)
	}
}

func TestOrgPRCreationCapAndMergeAsyncREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	srv := s.Server
	st := srv.store
	admin := st.UsersByLogin["admin"]
	org := st.CreateOrg(admin, "async-merge-org", "Async merge org", "")
	orgCapBase := "/api/v3/orgs/" + org.Login + "/interaction-limits/pulls/creation-cap"

	// Default org creation cap: disabled, 10.
	resp := s.get(t, orgCapBase, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if cap := decodeJSON(t, resp); cap["enabled"] != false || cap["max_open_pull_requests"].(float64) != 10 {
		t.Fatalf("default org creation cap = %#v", cap)
	}

	resp = s.patch(t, orgCapBase, defaultToken, map[string]interface{}{"enabled": true, "max_open_pull_requests": 5})
	requireHTTPStatus(t, resp, http.StatusOK)
	if cap := decodeJSON(t, resp); cap["enabled"] != true || cap["max_open_pull_requests"].(float64) != 5 {
		t.Fatalf("updated org creation cap = %#v", cap)
	}
	resp = s.get(t, orgCapBase, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if cap := decodeJSON(t, resp); cap["max_open_pull_requests"].(float64) != 5 {
		t.Fatalf("persisted org creation cap = %#v", cap)
	}

	// Missing enabled and out-of-range max are validation errors.
	resp = s.patch(t, orgCapBase, defaultToken, map[string]interface{}{"max_open_pull_requests": 3})
	requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
	resp = s.patch(t, orgCapBase, defaultToken, map[string]interface{}{"enabled": true, "max_open_pull_requests": 5000})
	requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	owner := "asyncowner"
	repoName := "async-repo"
	repoKey := owner + "/" + repoName
	commitFilesToStorage(t, srv, repoKey, map[string]string{"README.md": "hi"})
	repo := st.GetRepo(owner, repoName)
	user := st.UsersByLogin[owner]
	stor := st.GetGitStorage(owner, repoName)
	headBranch := "main"
	if store.ResolveBranchSha(stor, "main") == "" {
		headBranch = "master"
	}
	seedStorePullRequestBranches(t, st, repo, headBranch, "base")
	pr := st.CreatePullRequest(repo.ID, user.ID, "async", "", headBranch, "base", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("PR not created")
	}
	st.UpdatePullRequest(pr.ID, func(p *store.PullRequest) { p.Mergeable = "MERGEABLE" })

	asyncBase := fmt.Sprintf("/api/v3/repos/%s/pulls/%d/merge-async", repoKey, pr.Number)
	resp = s.put(t, asyncBase, defaultToken, map[string]interface{}{"merge_method": "squash"})
	requireHTTPStatus(t, resp, http.StatusAccepted)
	enq := decodeJSON(t, resp)
	if enq["status"] != "enqueued" {
		t.Fatalf("merge-async enqueue = %#v", enq)
	}
	mergeUUID, _ := enq["details"].(map[string]interface{})["uuid"].(string)
	if mergeUUID == "" {
		t.Fatalf("merge-async enqueue missing uuid: %#v", enq)
	}

	// Poll the result: merged, with a merge commit sha.
	resp = s.get(t, asyncBase+"/"+mergeUUID, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	res := decodeJSON(t, resp)
	if res["status"] != "merged" {
		t.Fatalf("merge-async result = %#v", res)
	}
	if sha, _ := res["details"].(map[string]interface{})["sha"].(string); sha == "" {
		t.Fatalf("merge-async result missing sha: %#v", res)
	}

	// A second async merge reports already-merged (200).
	resp = s.put(t, asyncBase, defaultToken, map[string]interface{}{})
	requireHTTPStatus(t, resp, http.StatusOK)
	if again := decodeJSON(t, resp); again["status"] != "merged" {
		t.Fatalf("second merge-async = %#v", again)
	}

	// Unknown poll uuid is a 404.
	resp = s.get(t, asyncBase+"/does-not-exist", defaultToken)
	requireHTTPStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}
