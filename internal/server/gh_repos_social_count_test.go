package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// GET /repos/{owner}/{repo}/stargazers/count — added to GitHub's REST
// description in d77b7dde; returns {"count": n} so a caller does not have to
// page the stargazer list, and 404s wherever the repo itself would.
func TestStargazerCountEndpoint(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	read := func(t *testing.T, resp *http.Response, want int) []byte {
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
	count := func(t *testing.T) int {
		t.Helper()
		var out struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(read(t, s.get(t, "/api/v3/repos/admin/stars/stargazers/count", defaultToken), http.StatusOK), &out); err != nil {
			t.Fatal(err)
		}
		return out.Count
	}

	read(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "stars"}), http.StatusCreated)
	if got := count(t); got != 0 {
		t.Fatalf("count on a fresh repo = %d, want 0", got)
	}
	read(t, s.put(t, "/api/v3/user/starred/admin/stars", defaultToken, nil), http.StatusNoContent)
	if got := count(t); got != 1 {
		t.Fatalf("count after starring = %d, want 1", got)
	}

	resp := s.get(t, "/api/v3/repos/admin/ghost-repo/stargazers/count", defaultToken)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown repo status = %d, want 404", resp.StatusCode)
	}
}
