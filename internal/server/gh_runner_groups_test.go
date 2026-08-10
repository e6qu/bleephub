package bleephub

import (
	"fmt"
	"io"
	"testing"
)

func (s *isolatedServer) registerRunnerForLabels(t *testing.T, scope runnerScope) int {
	t.Helper()
	// Agent registration presents the scoped registration token config.sh is
	// given, not a personal access token.
	body := `{"name":"label-runner","labels":[{"name":"self-hosted","type":"system"},{"name":"linux","type":"system"}]}`
	token := mustRunnerRegistrationToken(t, scope)
	resp := runnerDo(t, "POST", s.baseURL+"/_apis/v1/Agent/1", token, body)
	agent := decodeJSONWithStatus(t, resp, 200)
	return int(agent["id"].(float64))
}

func TestRunnerLabels_Repo_ListSetDelete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t).fullName()
	agentID := s.registerRunnerForLabels(t, runnerScope{Repo: repo})

	// List labels: should include the two system labels.
	listResp := s.get(t, fmt.Sprintf("/api/v3/repos/%s/actions/runners/%d/labels", repo, agentID), defaultToken)
	listData := decodeJSONWithStatus(t, listResp, 200)
	if listData["total_count"].(float64) != 2 {
		t.Fatalf("initial labels count = %v, want 2", listData["total_count"])
	}

	// Set custom labels.
	setResp := s.put(t, fmt.Sprintf("/api/v3/repos/%s/actions/runners/%d/labels", repo, agentID), defaultToken, map[string]interface{}{
		"labels": []string{"gpu", "arm64"},
	})
	setData := decodeJSONWithStatus(t, setResp, 200)
	if setData["total_count"].(float64) != 4 {
		t.Fatalf("labels after set = %v, want 4", setData["total_count"])
	}

	// Delete all custom labels; system labels remain.
	delResp := s.delete(t, fmt.Sprintf("/api/v3/repos/%s/actions/runners/%d/labels", repo, agentID), defaultToken)
	delData := decodeJSONWithStatus(t, delResp, 200)
	if delData["total_count"].(float64) != 2 {
		t.Fatalf("labels after delete = %v, want 2", delData["total_count"])
	}
	labels, _ := delData["labels"].([]interface{})
	for _, l := range labels {
		lm := l.(map[string]interface{})
		if lm["type"] != "read-only" {
			t.Errorf("expected only read-only labels after delete, got %v", lm)
		}
	}
}

func TestRunnerLabels_Org_ListSet(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.createTestOrg(t)
	agentID := s.registerRunnerForLabels(t, runnerScope{Org: org})

	listResp := s.get(t, fmt.Sprintf("/api/v3/orgs/%s/actions/runners/%d/labels", org, agentID), defaultToken)
	listData := decodeJSONWithStatus(t, listResp, 200)
	if listData["total_count"].(float64) != 2 {
		t.Fatalf("initial org labels count = %v, want 2", listData["total_count"])
	}

	setResp := s.put(t, fmt.Sprintf("/api/v3/orgs/%s/actions/runners/%d/labels", org, agentID), defaultToken, map[string]interface{}{
		"labels": []string{"builder"},
	})
	setData := decodeJSONWithStatus(t, setResp, 200)
	if setData["total_count"].(float64) != 3 {
		t.Fatalf("org labels after set = %v, want 3", setData["total_count"])
	}
}

func TestRunnerLabels_Org_UnknownOrg(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	agentID := s.registerRunnerForLabels(t, runnerScope{Org: "different-org"})
	listResp := s.get(t, fmt.Sprintf("/api/v3/orgs/no-such-org-999/actions/runners/%d/labels", agentID), defaultToken)
	if listResp.StatusCode != 404 {
		body, _ := io.ReadAll(listResp.Body)
		listResp.Body.Close()
		t.Fatalf("expected 404 for unknown org, got %d: %s", listResp.StatusCode, body)
	}
	listResp.Body.Close()
}

func TestRunnerLabels_Repo_UnknownRunner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t).fullName()
	listResp := s.get(t, fmt.Sprintf("/api/v3/repos/%s/actions/runners/999999/labels", repo), defaultToken)
	if listResp.StatusCode != 404 {
		body, _ := io.ReadAll(listResp.Body)
		listResp.Body.Close()
		t.Fatalf("expected 404 for unknown runner, got %d: %s", listResp.StatusCode, body)
	}
	listResp.Body.Close()
}
