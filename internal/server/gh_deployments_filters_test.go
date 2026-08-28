package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func decodeDeploymentListing(t *testing.T, resp *http.Response) []map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var out []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode array: %v", err)
	}
	return out
}

// TestListDeploymentsHonoursDocumentedFilters pins the four documented filters
// on the deployments listing. They are the whole reason the listing is usable
// on a busy repository: a deploy tool asks "what is deployed to production"
// (environment) or "did this commit ship" (sha), and a listing that answers
// with every deployment ever made regardless of the filter sent reads as though
// the answer were yes.
func TestListDeploymentsHonoursDocumentedFilters(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, true)
	base := "/api/v3/repos/admin/" + repo + "/deployments"

	// Three deployments differing in exactly one filterable field each, so a
	// filter that is read narrows the listing and one that is ignored does not.
	created := make([]map[string]interface{}, 0, 3)
	for _, spec := range []map[string]interface{}{
		{"ref": "main", "environment": "production", "task": "deploy", "required_contexts": []string{}},
		{"ref": "main", "environment": "staging", "task": "deploy", "required_contexts": []string{}},
		{"ref": "main", "environment": "staging", "task": "deploy:migrations", "required_contexts": []string{}},
	} {
		created = append(created, decodeJSONWithStatus(t, s.post(t, base, defaultToken, spec), http.StatusCreated))
	}
	productionSHA, _ := created[0]["sha"].(string)
	if productionSHA == "" {
		t.Fatal("created deployment has no sha to filter on")
	}

	if all := decodeDeploymentListing(t, s.get(t, base, defaultToken)); len(all) != 3 {
		t.Fatalf("unfiltered listing = %d deployments, want 3", len(all))
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?environment=production", 1},
		{"?environment=staging", 2},
		{"?environment=nonesuch", 0},
		{"?task=deploy", 2},
		{"?task=deploy:migrations", 1},
		{"?ref=main", 3},
		{"?ref=release", 0},
		{"?sha=" + productionSHA, 3},
		{"?sha=0000000000000000000000000000000000000000", 0},
		// Filters compose: each narrows what the others left.
		{"?environment=staging&task=deploy", 1},
		{"?environment=production&task=deploy:migrations", 0},
	} {
		got := decodeDeploymentListing(t, s.get(t, base+tc.query, defaultToken))
		if len(got) != tc.want {
			t.Errorf("GET deployments%s = %d deployments, want %d", tc.query, len(got), tc.want)
		}
	}

	// A filter sent empty is not a filter for the empty string; the published
	// default reads as "no filter".
	if got := decodeDeploymentListing(t, s.get(t, base+"?environment=&task=&ref=&sha=", defaultToken)); len(got) != 3 {
		t.Errorf("empty filter values = %d deployments, want all 3", len(got))
	}
}
