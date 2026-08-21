package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Watching a repository notifies about activity from that point on. Before
// this window existed, a plain subscribe retroactively surfaced every
// pre-existing issue and pull request as a "subscribed" thread, flooding the
// inbox — github.com only delivers activity that happens after you subscribe.
func TestRepoWatchNotifiesOnlyAfterSubscribing(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// The isolated server's clock is fixed, so without advancing it the
	// pre-existing issue and the subscription share an instant and the
	// "activity before the watch" case cannot be expressed.
	var clockMu sync.Mutex
	current := fixedTestTime
	s.replaceClockNow(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return current
	})
	advance := func(d time.Duration) {
		clockMu.Lock()
		current = current.Add(d)
		clockMu.Unlock()
	}

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
	watcherThreads := func(t *testing.T, token string) []string {
		t.Helper()
		var threads []struct {
			Reason  string `json:"reason"`
			Subject struct {
				Title string `json:"title"`
			} `json:"subject"`
		}
		if err := json.Unmarshal(body(t, s.get(t, "/api/v3/notifications?all=true", token), http.StatusOK), &threads); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, th := range threads {
			if th.Reason == "subscribed" {
				out = append(out, th.Subject.Title)
			}
		}
		return out
	}

	// The repo owner opens an issue BEFORE anyone watches.
	body(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "watchwin"}), http.StatusCreated)
	body(t, s.post(t, "/api/v3/repos/admin/watchwin/issues", defaultToken,
		map[string]interface{}{"title": "before watching"}), http.StatusCreated)

	advance(time.Hour)
	watcher := seedTestUser(s.Server, "watchwin-user")
	token := s.store.CreateToken(watcher.ID, "repo").Value

	// Watch: the pre-existing issue must NOT appear.
	body(t, s.put(t, "/api/v3/repos/admin/watchwin/subscription", token,
		map[string]interface{}{"subscribed": true}), http.StatusOK)
	if got := watcherThreads(t, token); len(got) != 0 {
		t.Fatalf("subscribed threads right after watching = %v, want none (no backfill)", got)
	}

	// Activity after the subscription does arrive.
	advance(time.Hour)
	body(t, s.post(t, "/api/v3/repos/admin/watchwin/issues", defaultToken,
		map[string]interface{}{"title": "after watching"}), http.StatusCreated)
	got := watcherThreads(t, token)
	if len(got) != 1 || got[0] != "after watching" {
		t.Fatalf("subscribed threads after new activity = %v, want [after watching]", got)
	}
}
