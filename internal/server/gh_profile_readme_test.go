package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The profile-readme wrapper must answer 200 with readme: null for the common
// no-profile-repo case (the SPA may never probe an endpoint that 404s — the
// browser logs every 4xx as a console error and the e2e harness fails on it),
// and byte-identical readme JSON to the standalone endpoint otherwise.
func TestProfileReadme_NullAndByteParity(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	readBody := func(t *testing.T, resp *http.Response, wantStatus int) []byte {
		t.Helper()
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != wantStatus {
			t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, wantStatus, body)
		}
		return body
	}
	readmeOf := func(t *testing.T, body []byte) string {
		t.Helper()
		var out struct {
			Readme json.RawMessage `json:"readme"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return string(out.Readme)
	}

	// No admin/admin repo yet: 200 with readme null.
	body := readBody(t, s.get(t, "/ui-data/users/admin/profile-readme", defaultToken), http.StatusOK)
	if readmeOf(t, body) != "null" {
		t.Fatalf("no-repo readme = %s, want null", body)
	}

	// Profile repo with a README: readme equals the standalone endpoint's body.
	readBody(t, s.post(t, "/api/v3/user/repos", defaultToken,
		map[string]interface{}{"name": "admin"}), http.StatusCreated)
	readBody(t, s.put(t, "/api/v3/repos/admin/admin/contents/README.md", defaultToken,
		map[string]interface{}{"message": "add readme", "content": "IyBoaQo="}), http.StatusCreated)

	body = readBody(t, s.get(t, "/ui-data/users/admin/profile-readme", defaultToken), http.StatusOK)
	standalone := readBody(t, s.get(t, "/api/v3/repos/admin/admin/readme", defaultToken), http.StatusOK)
	if readmeOf(t, body) != strings.TrimSpace(string(standalone)) {
		t.Fatalf("readme differs from standalone endpoint:\n%s\nvs\n%s", readmeOf(t, body), standalone)
	}

	// Organizations use the {org}/.github repo; absent → null.
	if s.store.CreateOrg(s.store.LookupUserByLogin("admin"), "readme-org", "", "") == nil {
		t.Fatal("create org")
	}
	body = readBody(t, s.get(t, "/ui-data/users/readme-org/profile-readme", defaultToken), http.StatusOK)
	if readmeOf(t, body) != "null" {
		t.Fatalf("org no-repo readme = %s, want null", body)
	}

	// Unknown login is a 404 like every account-scoped surface.
	resp := s.get(t, "/ui-data/users/ghost/profile-readme", defaultToken)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown login status = %d, want 404", resp.StatusCode)
	}
}
