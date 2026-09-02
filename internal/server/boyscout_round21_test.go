package bleephub

import (
	"net/http"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestFollowRejectsNonexistentAndBlocked pins that PUT /user/following/{user}
// 404s an unknown target and 403s when the target has blocked the caller.
func TestFollowRejectsNonexistentAndBlocked(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	resp := s.put(t, "/api/v3/user/following/ghost-user", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("following an unknown user = %d, want 404", resp.StatusCode)
	}

	target, _ := s.newUser(t, "blk-target")
	admin := s.store.LookupUserByLogin("admin")
	s.store.BlockUser(target.ID, admin.ID) // target blocks admin
	resp = s.put(t, "/api/v3/user/following/blk-target", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("following a user who blocked you = %d, want 403", resp.StatusCode)
	}

	s.newUser(t, "friend-user")
	resp = s.put(t, "/api/v3/user/following/friend-user", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("following a valid, unblocking user = %d, want 204", resp.StatusCode)
	}
}

// TestGlobalAdvisoryExcludesPrivateRepoSource pins that a published advisory whose
// source is a private repository never surfaces in the public global advisory
// database (which would leak the private repo's name and vulnerability detail).
func TestGlobalAdvisoryExcludesPrivateRepoSource(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	priv := s.seedRepo(t, "sec-private", true)
	pub := s.seedRepo(t, "sec-public", false)
	published := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)

	s.store.Mu.Lock()
	s.store.SecurityAdvisories[9001] = &store.SecurityAdvisory{ID: 9001, GHSAID: "GHSA-priv-0000", RepoID: priv.ID, Severity: "high", State: "published", PublishedAt: &published}
	s.store.SecurityAdvisories[9002] = &store.SecurityAdvisory{ID: 9002, GHSAID: "GHSA-pub-0000", RepoID: pub.ID, Severity: "high", State: "published", PublishedAt: &published}
	s.store.Mu.Unlock()

	seen := map[string]bool{}
	for _, a := range decodeJSONArray(t, s.get(t, "/api/v3/advisories", defaultToken)) {
		if g, _ := a["ghsa_id"].(string); g != "" {
			seen[g] = true
		}
	}
	if seen["GHSA-priv-0000"] {
		t.Fatal("a private repo's advisory leaked into the global advisory database")
	}
	if !seen["GHSA-pub-0000"] {
		t.Fatal("a public repo's published advisory should appear in the global database")
	}

	// Directly fetching the private advisory by GHSA id 404s.
	resp := s.get(t, "/api/v3/advisories/GHSA-priv-0000", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET private advisory = %d, want 404", resp.StatusCode)
	}
}
