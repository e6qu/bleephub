package bleephub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// GitHub's REST description gained `before`/`after` cursor parameters on the
// org and repo secret-scanning custom-pattern lists (rest-api-description
// d77b7dde); the handlers honour them like the other cursor-paginated lists.
func TestSecretScanningCustomPatternsCursorPagination(t *testing.T) {
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
	ids := func(t *testing.T, query string) []int {
		t.Helper()
		var out []struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(read(t, s.get(t, "/api/v3/repos/admin/ssp/secret-scanning/custom-patterns"+query, defaultToken), http.StatusOK), &out); err != nil {
			t.Fatal(err)
		}
		got := make([]int, 0, len(out))
		for _, p := range out {
			got = append(got, p.ID)
		}
		return got
	}
	cursor := func(id int) string {
		return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", id)))
	}

	read(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "ssp"}), http.StatusCreated)
	for _, name := range []string{"one", "two", "three"} {
		read(t, s.post(t, "/api/v3/repos/admin/ssp/secret-scanning/custom-patterns", defaultToken, map[string]interface{}{
			"patterns": []map[string]interface{}{{
				"name": name, "pattern": "[a-z]+",
			}},
		}), http.StatusCreated)
	}
	all := ids(t, "")
	if len(all) != 3 {
		t.Fatalf("patterns = %v, want three", all)
	}

	if got := ids(t, "?after="+cursor(all[0])); len(got) != 2 || got[0] != all[1] {
		t.Fatalf("after the first cursor = %v, want the last two of %v", got, all)
	}
	if got := ids(t, "?before="+cursor(all[2])); len(got) != 2 || got[len(got)-1] != all[1] {
		t.Fatalf("before the last cursor = %v, want the first two of %v", got, all)
	}
	// An unparsable cursor is a validation error, not a silently full page.
	resp := s.get(t, "/api/v3/repos/admin/ssp/secret-scanning/custom-patterns?after=not-a-cursor", defaultToken)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid cursor status = %d, want 422", resp.StatusCode)
	}
}
