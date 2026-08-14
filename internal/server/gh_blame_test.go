package bleephub

import "testing"

func TestBlameServesHunksForAFile(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":      "blame-test",
		"auto_init": true,
	})

	resp := s.get(t, "/ui-data/repos/admin/blame-test/blame/README.md", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("blame status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["path"] != "README.md" {
		t.Fatalf("path = %v, want README.md", body["path"])
	}
	if body["sha"] == nil || body["sha"] == "" {
		t.Fatalf("expected a resolved commit sha, got %v", body["sha"])
	}
	hunks, ok := body["hunks"].([]interface{})
	if !ok || len(hunks) == 0 {
		t.Fatalf("expected at least one blame hunk, got %v", body["hunks"])
	}
	h0 := hunks[0].(map[string]interface{})
	if sha, _ := h0["sha"].(string); sha == "" {
		t.Fatalf("hunk missing commit sha: %v", h0)
	}
	if short, _ := h0["short_sha"].(string); len(short) != 7 {
		t.Fatalf("hunk short_sha = %q, want 7 chars", short)
	}
	lines, ok := h0["lines"].([]interface{})
	if !ok || len(lines) == 0 {
		t.Fatalf("hunk has no lines: %v", h0)
	}
	if start, _ := h0["start_line"].(float64); start != 1 {
		t.Fatalf("first hunk start_line = %v, want 1", h0["start_line"])
	}
}

func TestBlameUnknownPathIs404(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":      "blame-404",
		"auto_init": true,
	})
	resp := s.get(t, "/ui-data/repos/admin/blame-404/blame/does-not-exist.txt", defaultToken)
	if resp.StatusCode != 404 {
		t.Fatalf("blame of missing path = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
