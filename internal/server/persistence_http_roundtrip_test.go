package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TEST-011: the shared suite runs with persistence disabled and no test creates
// state over HTTP, restarts, and reads it back over HTTP. This exercises the
// full round trip: a fully persistent server (BLEEPHUB_PERSIST=true, durable git
// dir, an injected in-memory object store so no live S3 is needed) creates a
// repository over HTTP; a second server process pointed at the same durable
// state serves that repository back over HTTP.
func TestPersistenceHTTPRoundTrip(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	bs := &flakyByteStore{blobs: map[string][]byte{}, failOn: 0}
	inject := func(s *Server) { s.injectedByteStore = bs }

	// First process: create the repository over HTTP.
	s1 := NewServer("127.0.0.1:0", zerolog.Nop(), inject)
	ts1 := httptest.NewServer(s1.requestHandler())
	createBody := `{"name":"persisted-repo","description":"round-trip","private":false}`
	req, _ := http.NewRequest(http.MethodPost, ts1.URL+"/api/v3/user/repos", strings.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status=%d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
	ts1.Close()

	// Second process: a fresh server over the same durable state must serve the
	// repository it never created in memory.
	s2 := NewServer("127.0.0.1:0", zerolog.Nop(), inject)
	if s2.store.persist == nil {
		t.Fatal("second server did not enable persistence")
	}
	ts2 := httptest.NewServer(s2.requestHandler())
	defer ts2.Close()
	req2, _ := http.NewRequest(http.MethodGet, ts2.URL+"/api/v3/repos/admin/persisted-repo", nil)
	req2.Header.Set("Authorization", "Bearer "+defaultToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("read repo after restart: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("read repo after restart: status=%d, want 200", resp2.StatusCode)
	}
	var repo map[string]interface{}
	if err := json.NewDecoder(resp2.Body).Decode(&repo); err != nil {
		t.Fatalf("decode reloaded repo: %v", err)
	}
	if repo["name"] != "persisted-repo" {
		t.Errorf("reloaded repo name = %v, want persisted-repo", repo["name"])
	}
	if repo["description"] != "round-trip" {
		t.Errorf("reloaded repo description = %v, want round-trip", repo["description"])
	}
}
