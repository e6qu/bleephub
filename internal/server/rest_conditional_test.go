package bleephub

import (
	"io"
	"net/http"
	"testing"
)

// getWith issues a GET against the isolated server with optional extra headers
// and returns the response (body already drained + closed).
func (s *isolatedServer) getWith(t *testing.T, path, token string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

// TestConditional304DoesNotConsumeRateLimit pins that a conditional GET which
// results in 304 Not Modified is not billed against the primary rate limit —
// GitHub's documented behaviour. bleephub consumes in middleware before the 304
// decision, so the unit must be refunded and the 304's X-RateLimit-Remaining
// must match the remaining budget from just before the conditional re-request.
func TestConditional304DoesNotConsumeRateLimit(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "cond-rl"}).Body.Close()

	// First read: consumes a unit and hands back an ETag.
	first, _ := s.getWith(t, "/api/v3/repos/admin/cond-rl", defaultToken, nil)
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("first GET carried no ETag")
	}
	remainingAfterFirst := first.Header.Get("X-RateLimit-Remaining")

	// Conditional re-read: 304, and the budget must be unchanged from after the
	// first read (the consumed unit was refunded), not decremented again.
	second, body := s.getWith(t, "/api/v3/repos/admin/cond-rl", defaultToken, map[string]string{"If-None-Match": etag})
	if second.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", second.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("304 carried a %d-byte body, want none", len(body))
	}
	if got := second.Header.Get("X-RateLimit-Remaining"); got != remainingAfterFirst {
		t.Errorf("304 X-RateLimit-Remaining = %q, want %q (a 304 must not be billed)", got, remainingAfterFirst)
	}
}

// TestActivityFeedsAdvertisePollInterval pins REST-031: the activity event
// feeds and the notifications list carry X-Poll-Interval, and unrelated
// endpoints (including the issues-events list) do not.
func TestActivityFeedsAdvertisePollInterval(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	srv.store.CreateRepo(srv.store.LookupUserByLogin("admin"), "poll-repo", "", false)

	for _, path := range []string{"/api/v3/events", "/api/v3/notifications", "/api/v3/users/admin/events"} {
		resp, _ := srv.getWith(t, path, defaultToken, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, resp.StatusCode)
		}
		if got := resp.Header.Get("X-Poll-Interval"); got != "60" {
			t.Fatalf("%s X-Poll-Interval = %q, want 60", path, got)
		}
	}
	// A non-feed endpoint and the issues-events list must not advertise polling.
	for _, path := range []string{"/api/v3/repos/admin/poll-repo", "/api/v3/repos/admin/poll-repo/issues/events"} {
		resp, _ := srv.getWith(t, path, defaultToken, nil)
		if got := resp.Header.Get("X-Poll-Interval"); got != "" {
			t.Fatalf("%s unexpectedly advertised X-Poll-Interval=%q", path, got)
		}
	}
}

// TestActivityFeedIsConditionalOnLastModified pins REST-031's Last-Modified
// on the activity feeds: the feed advertises the newest event's time, a
// matching If-Modified-Since yields a bodyless 304, and a present (even
// non-matching) If-None-Match takes precedence over If-Modified-Since.
func TestActivityFeedIsConditionalOnLastModified(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	st := srv.store
	admin := st.LookupUserByLogin("admin")
	repo := st.CreateRepo(admin, "lm-feed", "", false)
	if st.CreateIssue(repo.ID, admin.ID, "hello", "world", nil, nil, 0) == nil {
		t.Fatal("create issue")
	}

	resp, body := srv.getWith(t, "/api/v3/events", defaultToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("feed = %d, want 200", resp.StatusCode)
	}
	lastMod := resp.Header.Get("Last-Modified")
	if lastMod == "" {
		t.Fatal("feed carried no Last-Modified")
	}
	if len(body) == 0 || string(body) == "[]" {
		t.Fatalf("feed unexpectedly empty: %q", body)
	}

	// A matching If-Modified-Since yields a bodyless 304.
	resp, body = srv.getWith(t, "/api/v3/events", defaultToken, map[string]string{"If-Modified-Since": lastMod})
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional feed = %d, want 304", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("304 carried a %d-byte body, want none", len(body))
	}

	// RFC 7232 §3.3: a present (non-matching) If-None-Match takes precedence, so
	// If-Modified-Since is ignored and the full feed is returned.
	resp, _ = srv.getWith(t, "/api/v3/events", defaultToken, map[string]string{
		"If-None-Match":     `"stale"`,
		"If-Modified-Since": lastMod,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("If-None-Match precedence: feed = %d, want 200", resp.StatusCode)
	}
}

// TestReleaseAssetDownloadIsConditional pins REST-031's conditional non-JSON
// download: a binary asset download returns an ETag, a matching If-None-Match
// returns a bodyless 304, and a 304 is not counted as a download.
func TestReleaseAssetDownloadIsConditional(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	st := srv.store
	admin := st.LookupUserByLogin("admin")
	repo := st.CreateRepo(admin, "asset-cond", "", false)
	rel := st.Releases.Create(repo.ID, admin.ID, "v1", "main", "v1", "", false, false, false)
	asset, err := st.Releases.CreateReleaseAsset(rel.ID, admin.ID, "a.bin", "", "application/octet-stream", []byte("hello-bytes"))
	if err != nil {
		t.Fatalf("create asset: %v", err)
	}

	path := "/api/v3/repos/admin/asset-cond/releases/assets/" + itoa(asset.ID)
	octet := map[string]string{"Accept": "application/octet-stream"}

	resp, body := srv.getWith(t, path, defaultToken, octet)
	if resp.StatusCode != http.StatusOK || string(body) != "hello-bytes" {
		t.Fatalf("download = %d %q, want 200 hello-bytes", resp.StatusCode, body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("download response carried no ETag")
	}
	downloadsAfterFirst := st.Releases.GetReleaseAsset(asset.ID).DownloadCount

	// Conditional re-request with the ETag: 304, no body, no extra download.
	resp, body = srv.getWith(t, path, defaultToken, map[string]string{"Accept": "application/octet-stream", "If-None-Match": etag})
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional download = %d, want 304", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("304 carried a %d-byte body, want none", len(body))
	}
	if got := st.Releases.GetReleaseAsset(asset.ID).DownloadCount; got != downloadsAfterFirst {
		t.Fatalf("a 304 was counted as a download: %d -> %d", downloadsAfterFirst, got)
	}
}
