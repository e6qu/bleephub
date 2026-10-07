package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

// Re-requesting a review asks an already-requested reviewer again: the request
// is answered 201 with the pull request, and every named reviewer gets a
// `review_requested` event each time, where a repeated ordinary request
// changes nothing and sends nothing.
func TestRerequestReviewersNotifiesEveryNamedReviewer(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "pr-rerequest")
	s.post(t, "/api/v3/repos/admin/pr-rerequest/pulls", defaultToken, map[string]interface{}{
		"title": "Rerequest", "head": "feat", "base": "main",
	}).Body.Close()
	s.post(t, "/internal/users", defaultToken, map[string]interface{}{
		"login": "rereviewer", "name": "Re Reviewer", "email": "rr@example.com",
	}).Body.Close()

	var received atomic.Int32
	var mu sync.Mutex
	var reviewers []string
	sink, cleanup := startWebhookReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "pull_request" {
			var payload struct {
				Action            string `json:"action"`
				RequestedReviewer struct {
					Login string `json:"login"`
				} `json:"requested_reviewer"`
			}
			body, _ := io.ReadAll(r.Body)
			if json.Unmarshal(body, &payload) == nil && payload.Action == "review_requested" {
				mu.Lock()
				reviewers = append(reviewers, payload.RequestedReviewer.Login)
				mu.Unlock()
				received.Add(1)
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()
	resp := s.post(t, "/api/v3/repos/admin/pr-rerequest/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": sink + "/pr", "content_type": "json"},
		"events": []string{"pull_request"},
	})
	requireStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	body := map[string]interface{}{"reviewers": []string{"rereviewer"}}
	resp = s.post(t, "/api/v3/repos/admin/pr-rerequest/pulls/1/requested_reviewers", defaultToken, body)
	requireStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	waitForWebhookCount(t, &received, 1)

	resp = s.post(t, "/api/v3/repos/admin/pr-rerequest/pulls/1/requested_reviewers/rerequest", defaultToken, body)
	requireHTTPStatus(t, resp, http.StatusCreated)
	pr := decodeJSON(t, resp)
	if requested, _ := pr["requested_reviewers"].([]interface{}); len(requested) != 1 {
		t.Fatalf("requested_reviewers after a re-request = %v", pr["requested_reviewers"])
	}
	waitForWebhookCount(t, &received, 2)

	resp = s.post(t, "/api/v3/repos/admin/pr-rerequest/pulls/1/requested_reviewers/rerequest", defaultToken, map[string]interface{}{})
	requireStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(reviewers) != 2 || reviewers[0] != "rereviewer" || reviewers[1] != "rereviewer" {
		t.Fatalf("review_requested deliveries = %v, want two for rereviewer", reviewers)
	}
}
