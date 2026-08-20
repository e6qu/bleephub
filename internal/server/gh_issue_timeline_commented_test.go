package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// A conversation comment must appear in the timeline exactly once, as the
// comment itself (whose id the reactions endpoint accepts) — never additionally
// as the stored "commented" issue event, whose id lives in the event id space
// and 404s when the UI fetches reactions for it. GitHub's issue-events surface
// likewise excludes comments entirely.
func TestIssueTimelineRendersCommentsOnceWithCommentIDs(t *testing.T) {
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

	body(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "tl"}), http.StatusCreated)
	body(t, s.post(t, "/api/v3/repos/admin/tl/issues", defaultToken, map[string]interface{}{"title": "t"}), http.StatusCreated)
	raw := body(t, s.post(t, "/api/v3/repos/admin/tl/issues/1/comments", defaultToken, map[string]interface{}{"body": "hello"}), http.StatusCreated)
	var comment struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &comment); err != nil {
		t.Fatal(err)
	}

	var timeline []map[string]interface{}
	if err := json.Unmarshal(body(t, s.get(t, "/api/v3/repos/admin/tl/issues/1/timeline", defaultToken), http.StatusOK), &timeline); err != nil {
		t.Fatal(err)
	}
	commented := 0
	for _, item := range timeline {
		if item["event"] != "commented" {
			continue
		}
		commented++
		if int(item["id"].(float64)) != comment.ID {
			t.Fatalf("commented timeline id = %v, want the comment id %d", item["id"], comment.ID)
		}
	}
	if commented != 1 {
		t.Fatalf("commented timeline entries = %d, want exactly 1", commented)
	}

	// The id the timeline carries must be a live comment id for reactions.
	body(t, s.get(t, fmt.Sprintf("/api/v3/repos/admin/tl/issues/comments/%d/reactions", comment.ID), defaultToken), http.StatusOK)

	// The events surfaces exclude comments entirely.
	for _, path := range []string{"/api/v3/repos/admin/tl/issues/1/events", "/api/v3/repos/admin/tl/issues/events"} {
		var events []map[string]interface{}
		if err := json.Unmarshal(body(t, s.get(t, path, defaultToken), http.StatusOK), &events); err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e["event"] == "commented" {
				t.Fatalf("%s includes a commented event: %v", path, e)
			}
		}
	}
}
