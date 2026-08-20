package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// A required status-check context is satisfied by a successful CLASSIC commit
// status, not only by a check run — most external CI reports through the
// statuses API, and mergeable_state stayed "blocked" forever after the
// required context succeeded (the UI's merge box could never converge).
func TestMergeableStateSatisfiedByCommitStatus(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	body := func(t *testing.T, resp *http.Response, want int) []byte {
		t.Helper()
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, want, raw)
		}
		return raw
	}
	prState := func(t *testing.T) (string, string) {
		t.Helper()
		var pr struct {
			MergeableState string `json:"mergeable_state"`
			Head           struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if err := json.Unmarshal(body(t, s.get(t, "/api/v3/repos/admin/ms/pulls/1", defaultToken), http.StatusOK), &pr); err != nil {
			t.Fatal(err)
		}
		return pr.MergeableState, pr.Head.SHA
	}

	repo := s.sweepRepo(t, "ms")
	seedPullRequestBranches(t, s.Server, s.store.GetRepo(repo.owner, repo.name), "feat")
	body(t, s.post(t, "/api/v3/repos/admin/ms/pulls", defaultToken,
		map[string]interface{}{"title": "t", "head": "feat", "base": "main"}), http.StatusCreated)
	body(t, s.put(t, "/api/v3/repos/admin/ms/branches/main/protection", defaultToken, map[string]interface{}{
		"required_status_checks":        map[string]interface{}{"strict": false, "contexts": []string{"ci/status"}},
		"enforce_admins":                false,
		"required_pull_request_reviews": nil,
		"restrictions":                  nil,
	}), http.StatusOK)

	got, headSHA := prState(t)
	if got != "blocked" {
		t.Fatalf("mergeable_state before the status = %q, want blocked", got)
	}
	// A pending classic status keeps it blocked (the context is not green).
	body(t, s.post(t, "/api/v3/repos/admin/ms/statuses/"+headSHA, defaultToken,
		map[string]interface{}{"state": "pending", "context": "ci/status"}), http.StatusCreated)
	if got, _ = prState(t); got != "blocked" {
		t.Fatalf("mergeable_state with pending status = %q, want blocked", got)
	}
	// The successful classic status satisfies the required context.
	body(t, s.post(t, "/api/v3/repos/admin/ms/statuses/"+headSHA, defaultToken,
		map[string]interface{}{"state": "success", "context": "ci/status"}), http.StatusCreated)
	if got, _ = prState(t); got != "clean" {
		t.Fatalf("mergeable_state with successful status = %q, want clean", got)
	}
}
