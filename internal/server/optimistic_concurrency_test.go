package bleephub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// STORE-016: a mutating request may carry If-Match to demand optimistic
// concurrency — the write is rejected 412 unless the caller's ETag still matches
// the resource, so a stale editor cannot clobber a concurrent update. An absent
// If-Match stays unconditional.
func TestIssueUpdateOptimisticConcurrency(t *testing.T) {
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]any{"name": "etag-repo"}).Body.Close()
	issueResp := s.post(t, "/api/v3/repos/admin/etag-repo/issues", defaultToken, map[string]any{"title": "orig"})
	var issue map[string]any
	_ = json.NewDecoder(issueResp.Body).Decode(&issue)
	issueResp.Body.Close()
	num := int(issue["number"].(float64))
	path := fmt.Sprintf("/api/v3/repos/admin/etag-repo/issues/%d", num)

	getResp := s.get(t, path, defaultToken)
	etag := getResp.Header.Get("ETag")
	getResp.Body.Close()
	if etag == "" {
		t.Fatal("issue GET returned no ETag")
	}

	patch := func(ifMatch string, body map[string]any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPatch, s.baseURL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		req.Header.Set("Content-Type", "application/json")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// A wrong ETag is rejected without mutating.
	if code := patch(`"0000000000000000000000000000000000000000000000000000000000000000"`, map[string]any{"title": "x"}); code != http.StatusPreconditionFailed {
		t.Fatalf("wrong If-Match: status %d, want 412", code)
	}
	// The current ETag matches → the update applies.
	if code := patch(etag, map[string]any{"title": "updated"}); code != http.StatusOK {
		t.Fatalf("matching If-Match: status %d, want 200", code)
	}
	// That update changed the representation, so the same ETag is now stale — a
	// second writer holding it is rejected (the concurrency guarantee).
	if code := patch(etag, map[string]any{"title": "again"}); code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match after update: status %d, want 412", code)
	}
	// Wildcard matches any current version.
	if code := patch("*", map[string]any{"title": "wild"}); code != http.StatusOK {
		t.Fatalf("wildcard If-Match: status %d, want 200", code)
	}
	// No If-Match is unconditional.
	if code := patch("", map[string]any{"title": "unconditional"}); code != http.StatusOK {
		t.Fatalf("no If-Match: status %d, want 200", code)
	}
}
