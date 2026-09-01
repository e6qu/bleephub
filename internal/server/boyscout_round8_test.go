package bleephub

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
)

// TestWebhookRetryReusesDeliveryGUID pins that automatic retries of one event
// reuse a single X-GitHub-Delivery GUID (flagged as a redelivery), so a receiver
// deduping on the GUID processes the event once. The GUID was regenerated per
// attempt, making each retry look like a brand-new delivery.
func TestWebhookRetryReusesDeliveryGUID(t *testing.T) {
	var mu sync.Mutex
	var guids []string
	ln := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		guids = append(guids, r.Header.Get("X-GitHub-Delivery"))
		n := len(guids)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError) // force one retry
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ln.Close()

	s := newTestServer()
	hook := &store.Webhook{ID: 1, URL: ln.URL, Active: true, Events: []string{"push"}}
	s.deliverWebhook(hook, "push", "", []byte(`{"ref":"refs/heads/main"}`))

	testutil.TestEventually(8*time.Second, 20*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(guids) >= 2
	})
	mu.Lock()
	defer mu.Unlock()
	if len(guids) < 2 {
		t.Fatalf("expected a retry (>=2 attempts), got %d", len(guids))
	}
	if guids[0] == "" || guids[0] != guids[1] {
		t.Fatalf("retry used a different X-GitHub-Delivery: %q then %q (must be stable)", guids[0], guids[1])
	}
}

// TestDraftPullRequestCannotMerge pins that a draft PR reports mergeable_state
// "draft"/mergeable false and PUT .../merge is a 405.
func TestDraftPullRequestCannotMerge(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "draft-merge")
	s.post(t, "/api/v3/repos/admin/draft-merge/pulls", defaultToken, map[string]interface{}{
		"title": "wip", "head": "feature", "base": "main", "draft": true,
	}).Body.Close()

	got := decodeJSON(t, s.get(t, "/api/v3/repos/admin/draft-merge/pulls/1", defaultToken))
	if got["draft"] != true {
		t.Fatalf("PR draft flag = %v, want true", got["draft"])
	}
	if got["mergeable_state"] != "draft" {
		t.Fatalf("mergeable_state = %v, want draft", got["mergeable_state"])
	}
	if got["mergeable"] != false {
		t.Fatalf("mergeable = %v, want false", got["mergeable"])
	}

	merge := s.put(t, "/api/v3/repos/admin/draft-merge/pulls/1/merge", defaultToken, map[string]interface{}{})
	merge.Body.Close()
	if merge.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("merge of a draft PR = %d, want 405", merge.StatusCode)
	}
}

// TestDismissCommentedReviewRejected pins that dismissing a COMMENTED review is
// a 422 (GitHub only dismisses APPROVED / CHANGES_REQUESTED reviews).
func TestDismissCommentedReviewRejected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "dismiss-review")
	s.post(t, "/api/v3/repos/admin/dismiss-review/pulls", defaultToken, map[string]interface{}{
		"title": "x", "head": "feature", "base": "main",
	}).Body.Close()

	review := decodeJSON(t, s.post(t, "/api/v3/repos/admin/dismiss-review/pulls/1/reviews", defaultToken,
		map[string]interface{}{"event": "COMMENT", "body": "just a note"}))
	if review["state"] != "COMMENTED" {
		t.Fatalf("review state = %v, want COMMENTED", review["state"])
	}
	id, ok := review["id"].(float64)
	if !ok {
		t.Fatalf("review has no id: %#v", review)
	}

	resp := s.put(t, "/api/v3/repos/admin/dismiss-review/pulls/1/reviews/"+itoa(int(id))+"/dismissals",
		defaultToken, map[string]interface{}{"message": "no"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("dismiss of a COMMENTED review = %d, want 422", resp.StatusCode)
	}
}

// TestIssueStateReasonValidationAndReopen pins that an invalid state_reason is a
// 422 and that reopening an issue stamps state_reason "reopened".
func TestIssueStateReasonValidationAndReopen(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "issue-reason"})
	s.post(t, "/api/v3/repos/admin/issue-reason/issues", defaultToken,
		map[string]interface{}{"title": "an issue"}).Body.Close()

	// Invalid state_reason → 422.
	bad := s.patch(t, "/api/v3/repos/admin/issue-reason/issues/1", defaultToken,
		map[string]interface{}{"state_reason": "banana"})
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("state_reason=banana → %d, want 422", bad.StatusCode)
	}

	// Close then reopen; reopened issue reports state_reason "reopened".
	s.patch(t, "/api/v3/repos/admin/issue-reason/issues/1", defaultToken,
		map[string]interface{}{"state": "closed", "state_reason": "not_planned"}).Body.Close()
	reopened := decodeJSON(t, s.patch(t, "/api/v3/repos/admin/issue-reason/issues/1", defaultToken,
		map[string]interface{}{"state": "open"}))
	if reopened["state"] != "open" || reopened["state_reason"] != "reopened" {
		t.Fatalf("reopened issue state=%v state_reason=%v, want open/reopened",
			reopened["state"], reopened["state_reason"])
	}
}

// TestLFSDeclaredSizeCeiling pins that an LFS upload declaring a size above the
// 5 GB ceiling is refused (422) before any bytes are streamed to staging.
func TestLFSDeclaredSizeCeiling(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "lfs-ceiling"}).Body.Close()

	sum := sha256.Sum256([]byte("x"))
	oid := hex.EncodeToString(sum[:])
	over := int64(6) << 30 // 6 GiB, above the 5 GiB ceiling
	href := s.baseURL + "/admin/lfs-ceiling.git/info/lfs/objects/" + oid +
		"?size=" + itoa64(over)

	resp := s.lfsUpload(t, href, defaultToken, []byte("x"))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("oversize LFS upload = %d, want 422: %s", resp.StatusCode, body)
	}
}
