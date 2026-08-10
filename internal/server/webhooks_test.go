package bleephub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
	memfs "github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

func (s *isolatedServer) createWebhookTestRepo(t *testing.T, name string) {
	t.Helper()
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name,
	})
	resp.Body.Close()
}

func waitForWebhookCount(t *testing.T, received *atomic.Int32, want int32) {
	t.Helper()
	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		return received.Load() >= want
	}) {
		t.Fatalf("webhook deliveries = %d, want at least %d", received.Load(), want)
	}
}

func TestWebhookListPagination(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createWebhookTestRepo(t, "wh-paged")

	// An active hook fires a ping on creation; point it at an in-process sink so
	// the unit test makes no real outbound request to example.com.
	sink, cleanup := startWebhookReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()

	for i := 0; i < 2; i++ {
		resp := s.post(t, "/api/v3/repos/admin/wh-paged/hooks", defaultToken, map[string]interface{}{
			"config": map[string]interface{}{
				"url": fmt.Sprintf("%s/hook-%d", sink, i),
			},
			"events": []string{"push"},
		})
		if resp.StatusCode != 201 {
			resp.Body.Close()
			t.Fatalf("create hook %d: expected 201, got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp := s.get(t, "/api/v3/repos/admin/wh-paged/hooks?per_page=1", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("page 1: expected 200, got %d", resp.StatusCode)
	}
	link := resp.Header.Get("Link")
	var page1 []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&page1)
	resp.Body.Close()
	if len(page1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(page1))
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	resp = s.get(t, "/api/v3/repos/admin/wh-paged/hooks?per_page=1&page=2", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("page 2: expected 200, got %d", resp.StatusCode)
	}
	var page2 []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&page2)
	resp.Body.Close()
	if len(page2) != 1 {
		t.Fatalf("page 2 len = %d, want 1", len(page2))
	}
	if page1[0]["id"] == page2[0]["id"] {
		t.Fatalf("page 1 and page 2 returned the same hook: %v", page1[0]["id"])
	}
}

func TestWebhookCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createWebhookTestRepo(t, "wh-crud")

	// An active hook fires a ping on creation; point it at an in-process sink so
	// the unit test makes no real outbound request to example.com.
	sink, cleanup := startWebhookReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()

	// Create
	resp := s.post(t, "/api/v3/repos/admin/wh-crud/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{
			"url":    sink + "/hook",
			"secret": "s3cret",
		},
		"events": []string{"push", "pull_request"},
		"active": true,
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	hookID := int(data["id"].(float64))
	if hookID == 0 {
		t.Fatal("hook ID should be non-zero")
	}
	if data["active"] != true {
		t.Fatalf("expected active=true, got %v", data["active"])
	}
	events := data["events"].([]interface{})
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// List
	resp2 := s.get(t, "/api/v3/repos/admin/wh-crud/hooks", defaultToken)
	if resp2.StatusCode != 200 {
		resp2.Body.Close()
		t.Fatalf("list: expected 200, got %d", resp2.StatusCode)
	}
	defer resp2.Body.Close()
	var list []map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&list)
	if len(list) < 1 {
		t.Fatal("expected at least 1 hook in list")
	}

	// Get
	resp3 := s.get(t, fmt.Sprintf("/api/v3/repos/admin/wh-crud/hooks/%d", hookID), defaultToken)
	if resp3.StatusCode != 200 {
		resp3.Body.Close()
		t.Fatalf("get: expected 200, got %d", resp3.StatusCode)
	}
	data3 := decodeJSON(t, resp3)
	if int(data3["id"].(float64)) != hookID {
		t.Fatalf("expected id=%d, got %v", hookID, data3["id"])
	}

	// Update
	resp4 := s.patch(t, fmt.Sprintf("/api/v3/repos/admin/wh-crud/hooks/%d", hookID), defaultToken, map[string]interface{}{
		"active": false,
		"events": []string{"push"},
	})
	if resp4.StatusCode != 200 {
		resp4.Body.Close()
		t.Fatalf("update: expected 200, got %d", resp4.StatusCode)
	}
	data4 := decodeJSON(t, resp4)
	if data4["active"] != false {
		t.Fatalf("expected active=false after update, got %v", data4["active"])
	}
	updatedEvents := data4["events"].([]interface{})
	if len(updatedEvents) != 1 || updatedEvents[0] != "push" {
		t.Fatalf("expected [push] events after update, got %v", updatedEvents)
	}

	// Delete
	resp5 := s.delete(t, fmt.Sprintf("/api/v3/repos/admin/wh-crud/hooks/%d", hookID), defaultToken)
	defer resp5.Body.Close()
	if resp5.StatusCode != 204 {
		t.Fatalf("delete: expected 204, got %d", resp5.StatusCode)
	}

	// Verify deleted
	resp6 := s.get(t, fmt.Sprintf("/api/v3/repos/admin/wh-crud/hooks/%d", hookID), defaultToken)
	defer resp6.Body.Close()
	if resp6.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", resp6.StatusCode)
	}
}

func TestWebhookHMACSignature(t *testing.T) {
	secret := "test-secret"
	payload := []byte(`{"action":"opened"}`)

	sig := computeHMACSignature(secret, payload)

	// Verify manually
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if sig != expected {
		t.Fatalf("signature mismatch: got %s, want %s", sig, expected)
	}

	// Verify prefix
	if len(sig) < 7 || sig[:7] != "sha256=" {
		t.Fatalf("expected sha256= prefix, got %s", sig)
	}
}

// startWebhookReceiver starts an HTTPS server that records received webhook
// payloads. Delivery is https-only; the shared httptest TLS certificate is
// trusted by the delivery transport via installWebhookTestTLSRoots.
func startWebhookReceiver(t *testing.T, handler http.HandlerFunc) (string, func()) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	return srv.URL, srv.Close
}

// webhookEventJSON extracts the JSON event payload from a received
// webhook request body, honoring the Content-Type the way a real
// receiver must: content_type=form (GitHub's default) sends the JSON as
// the `payload` field of an x-www-form-urlencoded body; content_type=json
// sends it verbatim.
// webhookEventJSON runs inside the receiver's HTTP handler goroutine, not the
// test goroutine, so it must NOT call t.Fatalf: Fatalf calls runtime.Goexit,
// which would tear down only the handler goroutine and leave the test running
// (and possibly hanging) without reliably failing. t.Errorf is documented as
// safe to call from any goroutine and records the failure; on a parse error we
// report it and return an empty (non-nil) map so callers never dereference nil.
func webhookEventJSON(t *testing.T, contentType string, body []byte) map[string]interface{} {
	t.Helper()
	raw := body
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse form webhook body: %v", err)
			return map[string]interface{}{}
		}
		raw = []byte(vals.Get("payload"))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Errorf("decode webhook payload: %v", err)
		return map[string]interface{}{}
	}
	return out
}

func TestWebhookDeliverySuccess(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var received atomic.Int32
	var mu sync.Mutex
	var lastHeaders http.Header
	var lastBody []byte

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastHeaders = r.Header.Clone()
		lastBody = body
		// Signal delivery only after the payload is recorded: a waiter that
		// observes the count then reads under the lock must see this payload,
		// not a nil/stale one.
		received.Add(1)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-deliver")

	// Create webhook pointing to our receiver
	resp := s.post(t, "/api/v3/repos/admin/wh-deliver/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{
			"url":    url,
			"secret": "delivery-secret",
		},
		"events": []string{"push"},
		"active": true,
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	hookData := decodeJSON(t, resp)
	hookID := int(hookData["id"].(float64))

	pingResp := s.post(t, fmt.Sprintf("/api/v3/repos/admin/wh-deliver/hooks/%d/pings", hookID), defaultToken, nil)
	defer pingResp.Body.Close()
	if pingResp.StatusCode != 204 {
		t.Fatalf("ping: expected 204, got %d", pingResp.StatusCode)
	}

	waitForWebhookCount(t, &received, 1)

	mu.Lock()
	defer mu.Unlock()

	// Check headers
	if lastHeaders.Get("X-GitHub-Event") != "ping" {
		t.Fatalf("expected X-GitHub-Event=ping, got %s", lastHeaders.Get("X-GitHub-Event"))
	}
	if lastHeaders.Get("X-Hub-Signature-256") == "" {
		t.Fatal("expected X-Hub-Signature-256 header")
	}
	if lastHeaders.Get("User-Agent") != "GitHub-Hookshot/bleephub" {
		t.Fatalf("expected User-Agent=GitHub-Hookshot/bleephub, got %s", lastHeaders.Get("User-Agent"))
	}
	// Repository hooks carry the installation-target headers identifying the repo.
	if tt := lastHeaders.Get("X-GitHub-Hook-Installation-Target-Type"); tt != "repository" {
		t.Errorf("Target-Type = %q, want repository", tt)
	}
	if lastHeaders.Get("X-GitHub-Hook-Installation-Target-ID") == "" {
		t.Error("repository hook must set X-GitHub-Hook-Installation-Target-ID to the repo id")
	}

	// Verify HMAC
	sig := lastHeaders.Get("X-Hub-Signature-256")
	expectedSig := computeHMACSignature("delivery-secret", lastBody)
	if sig != expectedSig {
		t.Fatalf("HMAC mismatch: got %s, want %s", sig, expectedSig)
	}
}

func TestWebhookDeliveryRetry(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var attempts atomic.Int32

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(500) // fail first 2 attempts
		} else {
			w.WriteHeader(200) // succeed on 3rd
		}
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-retry")

	resp := s.post(t, "/api/v3/repos/admin/wh-retry/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{
			"url": url,
		},
		"events": []string{"issues"},
		"active": true,
	})
	resp.Body.Close()

	// Create an issue to trigger the webhook
	resp2 := s.post(t, "/api/v3/repos/admin/wh-retry/issues", defaultToken, map[string]interface{}{
		"title": "test issue for retry",
	})
	resp2.Body.Close()

	// Wait for retries (1s + 5s backoff = ~6s, use generous timeout)
	if !testutil.TestEventually(15*time.Second, 200*time.Millisecond, func() bool { return attempts.Load() >= 3 }) {
		t.Fatalf("expected at least 3 delivery attempts, got %d", attempts.Load())
	}
}

func TestWebhookDeliveryTimeout(t *testing.T) {
	release := make(chan struct{})
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer cleanup()
	defer close(release)

	client := newWebhookClientWithTimeout(true, false, 50*time.Millisecond)
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build webhook request: %v", err)
	}
	if _, err := client.Do(request); err == nil {
		t.Fatal("webhook client accepted a response that exceeded its deadline")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("webhook timeout error = %T %v", err, err)
	}
}

func TestWebhookPushEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var received atomic.Int32
	var mu sync.Mutex
	var lastEvent string
	var lastPayload map[string]interface{}

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastEvent = r.Header.Get("X-GitHub-Event")
		lastPayload = webhookEventJSON(t, r.Header.Get("Content-Type"), body)
		// Record before signalling: see TestWebhookDeliverySuccess.
		received.Add(1)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-push")

	// Create webhook
	resp := s.post(t, "/api/v3/repos/admin/wh-push/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"push"},
		"active": true,
	})
	resp.Body.Close()

	// Push via git (use go-git)
	s.pushTestCommit(t, "admin", "wh-push")

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return lastEvent == "push" && lastPayload["ref"] != nil
	}) {
		t.Fatalf("push webhook did not arrive; deliveries = %d", received.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	if lastEvent != "push" {
		t.Fatalf("expected event=push, got %s", lastEvent)
	}
	if lastPayload["ref"] == nil {
		t.Fatal("push payload missing 'ref' field")
	}
}

func TestWebhookReleaseLifecycleActions(t *testing.T) {
	var mu sync.Mutex
	var actions []string
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		payload := webhookEventJSON(t, r.Header.Get("Content-Type"), body)
		if r.Header.Get("X-GitHub-Event") == "release" {
			// Comma-ok: webhookEventJSON may return an empty map on a decode
			// error (it no longer Fatalfs from this handler goroutine), so an
			// unchecked type assertion here could panic instead of failing.
			if action, ok := payload["action"].(string); ok {
				mu.Lock()
				actions = append(actions, action)
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cleanup()

	const repo = "wh-release-lifecycle"
	createdRepo := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": repo, "auto_init": true,
	})
	createdRepo.Body.Close()
	hook := ghPost(t, "/api/v3/repos/admin/"+repo+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"release"},
		"active": true,
	})
	hook.Body.Close()

	releaseResp := ghPost(t, "/api/v3/repos/admin/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v1.0.0", "draft": true,
	})
	if releaseResp.StatusCode != http.StatusCreated {
		releaseResp.Body.Close()
		t.Fatalf("create draft release status = %d", releaseResp.StatusCode)
	}
	release := decodeJSON(t, releaseResp)
	releaseID := int(release["id"].(float64))
	published := ghPatch(t, "/api/v3/repos/admin/"+repo+"/releases/"+itoa(releaseID), defaultToken, map[string]interface{}{
		"draft": false,
	})
	published.Body.Close()
	deleted := ghDelete(t, "/api/v3/repos/admin/"+repo+"/releases/"+itoa(releaseID), defaultToken)
	deleted.Body.Close()

	testutil.TestEventually(5*time.Second, 20*time.Millisecond, func() bool {
		mu.Lock()
		count := len(actions)
		mu.Unlock()
		return count == 3
	})
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(actions) != "[created published deleted]" {
		t.Fatalf("release webhook actions = %v", actions)
	}
}

func TestWebhookPREvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var received atomic.Int32
	var mu sync.Mutex
	var events []string

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		events = append(events, r.Header.Get("X-GitHub-Event"))
		// Record before signalling: see TestWebhookDeliverySuccess.
		received.Add(1)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-pr")
	repo := s.store.GetRepo("admin", "wh-pr")
	if repo == nil {
		t.Fatal("repo wh-pr not created")
	}
	seedPullRequestBranches(t, s.Server, repo, "feature")

	// Create webhook for pull_request events
	resp := s.post(t, "/api/v3/repos/admin/wh-pr/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"pull_request"},
		"active": true,
	})
	resp.Body.Close()

	// Create a PR
	resp2 := s.post(t, "/api/v3/repos/admin/wh-pr/pulls", defaultToken, map[string]interface{}{
		"title": "test PR",
		"head":  "feature",
		"base":  "main",
	})
	if resp2.StatusCode != 201 {
		resp2.Body.Close()
		t.Fatalf("create PR: expected 201, got %d", resp2.StatusCode)
	}
	prData := decodeJSON(t, resp2)
	prNum := int(prData["number"].(float64))

	// Merge the PR
	resp3 := s.put(t, fmt.Sprintf("/api/v3/repos/admin/wh-pr/pulls/%d/merge", prNum), defaultToken, nil)
	resp3.Body.Close()

	waitForWebhookCount(t, &received, 2)

	mu.Lock()
	defer mu.Unlock()

	if received.Load() < 2 {
		t.Fatalf("expected at least 2 PR event deliveries (opened + closed), got %d", received.Load())
	}

	hasOpened := false
	hasClosed := false
	for _, e := range events {
		if e == "pull_request" {
			hasOpened = true
			hasClosed = true
		}
	}
	if !hasOpened || !hasClosed {
		t.Fatalf("expected pull_request events, got %v", events)
	}
}

func TestWebhookIssuesEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var received atomic.Int32
	var mu sync.Mutex
	var payloads []map[string]interface{}

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count only issues deliveries: active-hook creation also fires a ping,
		// and counting it could satisfy waitForWebhookCount(2) with ping+opened
		// before the closed delivery arrives — a race under -race load.
		if r.Header.Get("X-GitHub-Event") == "issues" {
			body, _ := io.ReadAll(r.Body)
			p := webhookEventJSON(t, r.Header.Get("Content-Type"), body)
			mu.Lock()
			received.Add(1)
			payloads = append(payloads, p)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-issues")

	// Create webhook for issues events
	resp := s.post(t, "/api/v3/repos/admin/wh-issues/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": true,
	})
	resp.Body.Close()

	// Create issue
	resp2 := s.post(t, "/api/v3/repos/admin/wh-issues/issues", defaultToken, map[string]interface{}{
		"title": "webhook test issue",
	})
	if resp2.StatusCode != 201 {
		resp2.Body.Close()
		t.Fatalf("create issue: expected 201, got %d", resp2.StatusCode)
	}
	issueData := decodeJSON(t, resp2)
	issueNum := int(issueData["number"].(float64))

	// Close issue
	resp3 := s.patch(t, fmt.Sprintf("/api/v3/repos/admin/wh-issues/issues/%d", issueNum), defaultToken, map[string]interface{}{
		"state": "closed",
	})
	resp3.Body.Close()

	waitForWebhookCount(t, &received, 2)

	mu.Lock()
	defer mu.Unlock()

	if received.Load() < 2 {
		t.Fatalf("expected at least 2 issue event deliveries, got %d", received.Load())
	}

	// Verify actions
	actions := make([]string, 0, len(payloads))
	for _, p := range payloads {
		if a, ok := p["action"].(string); ok {
			actions = append(actions, a)
		}
	}
	hasOpened := false
	hasClosed := false
	for _, a := range actions {
		if a == "opened" {
			hasOpened = true
		}
		if a == "closed" {
			hasClosed = true
		}
	}
	if !hasOpened {
		t.Fatalf("missing 'opened' action in payloads, got %v", actions)
	}
	if !hasClosed {
		t.Fatalf("missing 'closed' action in payloads, got %v", actions)
	}
}

// The gh CLI opens and closes issues over GraphQL; those mutations must deliver
// the same issues webhooks the REST path does, or `on: issues` workflows never
// fire for gh-driven changes.
func TestWebhookIssuesEventFromGraphQL(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	actions := map[string]bool{}

	// Only collect issues deliveries: the hook may also receive an unrelated
	// (e.g. ping) delivery, and waiting on a raw count could be satisfied by
	// it before the closed event arrives — a delivery-ordering race under -race.
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "issues" {
			body, _ := io.ReadAll(r.Body)
			p := webhookEventJSON(t, r.Header.Get("Content-Type"), body)
			if a, ok := p["action"].(string); ok {
				mu.Lock()
				actions[a] = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-issues-gql")
	hookResp := s.post(t, "/api/v3/repos/admin/wh-issues-gql/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": true,
	})
	hookResp.Body.Close()

	repoData := decodeJSON(t, s.get(t, "/api/v3/repos/admin/wh-issues-gql", defaultToken))
	repoNodeID, _ := repoData["node_id"].(string)
	if repoNodeID == "" {
		t.Fatal("repo node_id missing")
	}

	created := decodeJSON(t, s.post(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query":     `mutation($input: CreateIssueInput!){ createIssue(input:$input){ issue { id number } } }`,
		"variables": map[string]interface{}{"input": map[string]interface{}{"repositoryId": repoNodeID, "title": "gql issue"}},
	}))
	data, _ := created["data"].(map[string]interface{})
	ci, _ := data["createIssue"].(map[string]interface{})
	iss, _ := ci["issue"].(map[string]interface{})
	issueNodeID, _ := iss["id"].(string)
	if issueNodeID == "" {
		t.Fatalf("createIssue returned no issue id: %v", created)
	}

	closeResp := s.post(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query":     `mutation($input: CloseIssueInput!){ closeIssue(input:$input){ issue { state } } }`,
		"variables": map[string]interface{}{"input": map[string]interface{}{"issueId": issueNodeID}},
	})
	closeResp.Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return actions["opened"] && actions["closed"]
	}) {
		mu.Lock()
		got := fmt.Sprint(actions)
		mu.Unlock()
		t.Fatalf("GraphQL issue mutations did not deliver opened+closed webhooks; got %s", got)
	}
}

// Label CRUD must deliver the `label` webhook so `on: label` workflows fire:
// before the fix the mutation never produced the event.
func TestWebhookLabelEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	actions := map[string]bool{}
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "label" {
			body, _ := io.ReadAll(r.Body)
			p := webhookEventJSON(t, r.Header.Get("Content-Type"), body)
			if a, ok := p["action"].(string); ok {
				mu.Lock()
				actions[a] = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-label")
	hook := s.post(t, "/api/v3/repos/admin/wh-label/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"label"},
		"active": true,
	})
	hook.Body.Close()

	created := s.post(t, "/api/v3/repos/admin/wh-label/labels", defaultToken, map[string]interface{}{
		"name": "triage", "color": "ededed",
	})
	if created.StatusCode != http.StatusCreated {
		created.Body.Close()
		t.Fatalf("create label status = %d", created.StatusCode)
	}
	created.Body.Close()
	del := s.delete(t, "/api/v3/repos/admin/wh-label/labels/triage", defaultToken)
	del.Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return actions["created"] && actions["deleted"]
	}) {
		mu.Lock()
		got := fmt.Sprint(actions)
		mu.Unlock()
		t.Fatalf("label CRUD did not deliver created+deleted webhooks; got %s", got)
	}
}

func TestWebhookMilestoneEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	actions := map[string]bool{}
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "milestone" {
			body, _ := io.ReadAll(r.Body)
			p := webhookEventJSON(t, r.Header.Get("Content-Type"), body)
			if a, ok := p["action"].(string); ok {
				mu.Lock()
				actions[a] = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-milestone")
	s.post(t, "/api/v3/repos/admin/wh-milestone/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"milestone"},
		"active": true,
	}).Body.Close()

	created := s.post(t, "/api/v3/repos/admin/wh-milestone/milestones", defaultToken, map[string]interface{}{
		"title": "v1",
	})
	if created.StatusCode != http.StatusCreated {
		created.Body.Close()
		t.Fatalf("create milestone status = %d", created.StatusCode)
	}
	created.Body.Close()
	// Closing fires `closed`; deleting fires `deleted`.
	s.patch(t, "/api/v3/repos/admin/wh-milestone/milestones/1", defaultToken, map[string]interface{}{"state": "closed"}).Body.Close()
	s.delete(t, "/api/v3/repos/admin/wh-milestone/milestones/1", defaultToken).Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return actions["created"] && actions["closed"] && actions["deleted"]
	}) {
		mu.Lock()
		got := fmt.Sprint(actions)
		mu.Unlock()
		t.Fatalf("milestone CRUD did not deliver created+closed+deleted webhooks; got %s", got)
	}
}

func TestWebhookCreateDeleteRefEvent(t *testing.T) {
	var mu sync.Mutex
	events := map[string]bool{}
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ev := r.Header.Get("X-GitHub-Event"); ev == "create" || ev == "delete" {
			mu.Lock()
			events[ev] = true
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	repoPath := "/api/v3/repos/admin/wh-refs"
	ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "wh-refs", "auto_init": true}).Body.Close()
	ghPost(t, repoPath+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"create", "delete"},
		"active": true,
	}).Body.Close()

	refData := decodeJSON(t, ghGet(t, repoPath+"/git/refs/heads/main", defaultToken))
	mainObj, _ := refData["object"].(map[string]interface{})
	mainSha, _ := mainObj["sha"].(string)
	if mainSha == "" {
		t.Fatalf("main ref sha missing: %v", refData)
	}

	created := ghPost(t, repoPath+"/git/refs", defaultToken, map[string]interface{}{
		"ref": "refs/heads/wh-feature", "sha": mainSha,
	})
	if created.StatusCode != http.StatusCreated {
		created.Body.Close()
		t.Fatalf("create ref status = %d", created.StatusCode)
	}
	created.Body.Close()
	ghDelete(t, repoPath+"/git/refs/heads/wh-feature", defaultToken).Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return events["create"] && events["delete"]
	}) {
		mu.Lock()
		got := fmt.Sprint(events)
		mu.Unlock()
		t.Fatalf("branch create/delete did not deliver create+delete webhooks; got %s", got)
	}
}

func TestWebhookCommitCommentAndForkEvents(t *testing.T) {
	s := newIsolatedServer(t)
	var mu sync.Mutex
	got := map[string]bool{}
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ev := r.Header.Get("X-GitHub-Event"); ev == "commit_comment" || ev == "fork" {
			mu.Lock()
			got[ev] = true
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	repoPath := "/api/v3/repos/admin/wh-cc-fork"
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "wh-cc-fork", "auto_init": true}).Body.Close()
	s.post(t, repoPath+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"commit_comment", "fork"},
		"active": true,
	}).Body.Close()

	refData := decodeJSON(t, s.get(t, repoPath+"/git/refs/heads/main", defaultToken))
	mainObj, _ := refData["object"].(map[string]interface{})
	mainSha, _ := mainObj["sha"].(string)
	if mainSha == "" {
		t.Fatalf("main sha missing: %v", refData)
	}
	cc := s.post(t, repoPath+"/commits/"+mainSha+"/comments", defaultToken, map[string]interface{}{"body": "nice commit"})
	if cc.StatusCode != http.StatusCreated {
		cc.Body.Close()
		t.Fatalf("create commit comment status = %d", cc.StatusCode)
	}
	cc.Body.Close()

	// A fork by another user fires `fork` on the source repo's hook.
	_, forkerToken := s.newUser(t, "wh-forker")
	fk := s.post(t, repoPath+"/forks", forkerToken, map[string]interface{}{})
	if fk.StatusCode != http.StatusAccepted {
		fk.Body.Close()
		t.Fatalf("fork status = %d", fk.StatusCode)
	}
	fk.Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got["commit_comment"] && got["fork"]
	}) {
		mu.Lock()
		g := fmt.Sprint(got)
		mu.Unlock()
		t.Fatalf("commit_comment/fork webhooks not delivered; got %s", g)
	}
}

func TestWebhookWatchEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	started := false
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "watch" {
			p := func() map[string]interface{} {
				b, _ := io.ReadAll(r.Body)
				return webhookEventJSON(t, r.Header.Get("Content-Type"), b)
			}()
			if p["action"] == "started" {
				mu.Lock()
				started = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-watch")
	s.post(t, "/api/v3/repos/admin/wh-watch/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"watch"},
		"active": true,
	}).Body.Close()

	star := s.put(t, "/api/v3/user/starred/admin/wh-watch", defaultToken, nil)
	if star.StatusCode != http.StatusNoContent {
		star.Body.Close()
		t.Fatalf("star status = %d", star.StatusCode)
	}
	star.Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return started
	}) {
		t.Fatal("starring did not deliver a watch/started webhook")
	}
}

func TestWebhookPublicEvent(t *testing.T) {
	var mu sync.Mutex
	got := false
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "public" {
			mu.Lock()
			got = true
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "wh-public", "private": true}).Body.Close()
	ghPost(t, "/api/v3/repos/admin/wh-public/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"public"},
		"active": true,
	}).Body.Close()

	patch := ghPatch(t, "/api/v3/repos/admin/wh-public", defaultToken, map[string]interface{}{"private": false})
	if patch.StatusCode != http.StatusOK {
		patch.Body.Close()
		t.Fatalf("make public status = %d", patch.StatusCode)
	}
	patch.Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got
	}) {
		t.Fatal("making a repo public did not deliver a public webhook")
	}
}

func TestWebhookProjectEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	created := false
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "project" {
			b, _ := io.ReadAll(r.Body)
			if webhookEventJSON(t, r.Header.Get("Content-Type"), b)["action"] == "created" {
				mu.Lock()
				created = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-project")
	s.post(t, "/api/v3/repos/admin/wh-project/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"project"},
		"active": true,
	}).Body.Close()

	proj := s.post(t, "/api/v3/repos/admin/wh-project/projects", defaultToken, map[string]interface{}{"name": "Roadmap"})
	if proj.StatusCode != http.StatusCreated {
		proj.Body.Close()
		t.Fatalf("create project status = %d", proj.StatusCode)
	}
	proj.Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return created
	}) {
		t.Fatal("creating a classic project did not deliver a project/created webhook")
	}
}

func TestWebhookBranchProtectionRuleEvent(t *testing.T) {
	var mu sync.Mutex
	actions := map[string]bool{}
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "branch_protection_rule" {
			b, _ := io.ReadAll(r.Body)
			if a, ok := webhookEventJSON(t, r.Header.Get("Content-Type"), b)["action"].(string); ok {
				mu.Lock()
				actions[a] = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	repoPath := "/api/v3/repos/admin/wh-bpr"
	ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "wh-bpr", "auto_init": true}).Body.Close()
	ghPost(t, repoPath+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"branch_protection_rule"},
		"active": true,
	}).Body.Close()

	put := ghPut(t, repoPath+"/branches/main/protection", defaultToken, map[string]interface{}{
		"required_status_checks":        nil,
		"enforce_admins":                true,
		"required_pull_request_reviews": nil,
		"restrictions":                  nil,
	})
	if put.StatusCode != http.StatusOK {
		put.Body.Close()
		t.Fatalf("put protection status = %d", put.StatusCode)
	}
	put.Body.Close()
	ghDelete(t, repoPath+"/branches/main/protection", defaultToken).Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return actions["created"] && actions["deleted"]
	}) {
		mu.Lock()
		got := fmt.Sprint(actions)
		mu.Unlock()
		t.Fatalf("branch protection create/delete did not deliver created+deleted; got %s", got)
	}
}

func TestWebhookMemberEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	added := false
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "member" {
			b, _ := io.ReadAll(r.Body)
			if webhookEventJSON(t, r.Header.Get("Content-Type"), b)["action"] == "added" {
				mu.Lock()
				added = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-member")
	s.post(t, "/api/v3/repos/admin/wh-member/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"member"},
		"active": true,
	}).Body.Close()

	_, collabToken := s.newUser(t, "wh-member-collab")
	inv := s.put(t, "/api/v3/repos/admin/wh-member/collaborators/wh-member-collab", defaultToken, map[string]interface{}{"permission": "push"})
	if inv.StatusCode != http.StatusCreated {
		inv.Body.Close()
		t.Fatalf("invite status = %d, want 201", inv.StatusCode)
	}
	inv.Body.Close()

	list := decodeJSONArray(t, s.get(t, "/api/v3/user/repository_invitations", collabToken))
	if len(list) == 0 {
		t.Fatal("no pending invitations for the collaborator")
	}
	invID := int(list[0]["id"].(float64))
	acc := s.patch(t, fmt.Sprintf("/api/v3/user/repository_invitations/%d", invID), collabToken, nil)
	if acc.StatusCode != http.StatusNoContent {
		acc.Body.Close()
		t.Fatalf("accept status = %d, want 204", acc.StatusCode)
	}
	acc.Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return added
	}) {
		t.Fatal("accepting an invitation did not deliver a member/added webhook")
	}
}

func TestWebhookDiscussionEvents(t *testing.T) {
	var mu sync.Mutex
	got := map[string]bool{}
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ev := r.Header.Get("X-GitHub-Event"); ev == "discussion" || ev == "discussion_comment" {
			mu.Lock()
			got[ev] = true
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	repoData := decodeJSON(t, ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "discussions-wh"}))
	login := repoData["owner"].(map[string]interface{})["login"].(string)
	name := repoData["name"].(string)
	repoNodeID := repoData["node_id"].(string)
	ghPost(t, "/api/v3/repos/"+login+"/"+name+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"discussion", "discussion_comment"},
		"active": true,
	}).Body.Close()

	cats := runDiscussionGQL(t, `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){discussionCategories(first:10){nodes{id,name}}}}`,
		map[string]interface{}{"owner": login, "name": name})
	var catID string
	for _, n := range cats["repository"].(map[string]interface{})["discussionCategories"].(map[string]interface{})["nodes"].([]interface{}) {
		c := n.(map[string]interface{})
		if c["name"] == "Q&A" {
			catID = c["id"].(string)
		}
	}
	if catID == "" {
		t.Fatal("no Q&A category")
	}

	created := runDiscussionGQL(t, `mutation($repo:ID!,$cat:ID!){createDiscussion(input:{repositoryId:$repo,categoryId:$cat,title:"Hi",body:"B"}){discussion{id}}}`,
		map[string]interface{}{"repo": repoNodeID, "cat": catID})
	discID := created["createDiscussion"].(map[string]interface{})["discussion"].(map[string]interface{})["id"].(string)
	runDiscussionGQL(t, `mutation($d:ID!){addDiscussionComment(input:{discussionId:$d,body:"C"}){comment{id}}}`,
		map[string]interface{}{"d": discID})

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got["discussion"] && got["discussion_comment"]
	}) {
		mu.Lock()
		g := fmt.Sprint(got)
		mu.Unlock()
		t.Fatalf("discussion/discussion_comment webhooks not delivered; got %s", g)
	}
}

func TestWebhookPageBuildEvent(t *testing.T) {
	var mu sync.Mutex
	got := false
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "page_build" {
			mu.Lock()
			got = true
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	repoPath := "/api/v3/repos/admin/wh-pagebuild"
	ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "wh-pagebuild", "auto_init": true}).Body.Close()
	ghPost(t, repoPath+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"page_build"},
		"active": true,
	}).Body.Close()
	requireStatus(t, ghPost(t, repoPath+"/pages", defaultToken, map[string]interface{}{
		"source": map[string]interface{}{"branch": "main", "path": "/"},
	}), 201)
	build := ghPost(t, repoPath+"/pages/builds", defaultToken, nil)
	if build.StatusCode != http.StatusCreated {
		build.Body.Close()
		t.Fatalf("trigger pages build status = %d, want 201", build.StatusCode)
	}
	build.Body.Close()

	if !testutil.TestEventually(10*time.Second, 20*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got
	}) {
		t.Fatal("a Pages build did not deliver a page_build webhook")
	}
}

func TestWebhookProjectCardColumnEvents(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	got := map[string]bool{}
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ev := r.Header.Get("X-GitHub-Event"); ev == "project_column" || ev == "project_card" {
			b, _ := io.ReadAll(r.Body)
			if a, ok := webhookEventJSON(t, r.Header.Get("Content-Type"), b)["action"].(string); ok {
				mu.Lock()
				got[ev+":"+a] = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-projcard")
	s.post(t, "/api/v3/repos/admin/wh-projcard/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"project_column", "project_card"},
		"active": true,
	}).Body.Close()

	projID := int(decodeJSON(t, s.post(t, "/api/v3/repos/admin/wh-projcard/projects", defaultToken, map[string]interface{}{"name": "Board"}))["id"].(float64))
	c1 := s.createColumn(t, projID, "Todo")
	c2 := s.createColumn(t, projID, "Done")
	card := s.createCard(t, c1, map[string]any{"note": "task"})
	cardID := int(card["id"].(float64))
	s.moveCard(t, cardID, c2, "last")

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got["project_column:created"] && got["project_card:created"] && got["project_card:moved"]
	}) {
		mu.Lock()
		g := fmt.Sprint(got)
		mu.Unlock()
		t.Fatalf("project card/column webhooks not all delivered; got %s", g)
	}
}

func TestWebhookRegistryPackageEvent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	published := false
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "registry_package" {
			b, _ := io.ReadAll(r.Body)
			if webhookEventJSON(t, r.Header.Get("Content-Type"), b)["action"] == "published" {
				mu.Lock()
				published = true
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-pkg")
	s.post(t, "/api/v3/repos/admin/wh-pkg/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"registry_package"},
		"active": true,
	}).Body.Close()

	s.seedPackageVersion(t, "repository", "admin/wh-pkg", "npm", "wh-pkg-lib", "1.0.0")

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return published
	}) {
		t.Fatal("publishing a repository package did not deliver a registry_package webhook")
	}
}

func TestPullRequestRunReportsMergeSHA(t *testing.T) {
	var mu sync.Mutex
	var mergeSHA, headSHA string
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "pull_request" {
			b, _ := io.ReadAll(r.Body)
			p := webhookEventJSON(t, r.Header.Get("Content-Type"), b)
			if p["action"] == "opened" {
				if pr, _ := p["pull_request"].(map[string]interface{}); pr != nil {
					mu.Lock()
					mergeSHA, _ = pr["merge_commit_sha"].(string)
					if head, _ := pr["head"].(map[string]interface{}); head != nil {
						headSHA, _ = head["sha"].(string)
					}
					mu.Unlock()
				}
			}
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	repoPath := "/api/v3/repos/admin/act027"
	ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "act027", "auto_init": true}).Body.Close()
	ghPost(t, repoPath+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"pull_request"},
		"active": true,
	}).Body.Close()

	mainSha := decodeJSON(t, ghGet(t, repoPath+"/git/refs/heads/main", defaultToken))["object"].(map[string]interface{})["sha"].(string)
	ghPost(t, repoPath+"/git/refs", defaultToken, map[string]interface{}{"ref": "refs/heads/feat", "sha": mainSha}).Body.Close()
	ghPut(t, repoPath+"/contents/act027.txt", defaultToken, map[string]interface{}{
		"message": "add file",
		"content": base64.StdEncoding.EncodeToString([]byte("hello")),
		"branch":  "feat",
	}).Body.Close()

	ghPost(t, repoPath+"/pulls", defaultToken, map[string]interface{}{"title": "PR", "head": "feat", "base": "main"}).Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return mergeSHA != ""
	}) {
		t.Fatal("no pull_request opened webhook delivered")
	}
	mu.Lock()
	ms, hs := mergeSHA, headSHA
	mu.Unlock()
	if ms == "" || ms == hs {
		t.Fatalf("merge_commit_sha = %q (head = %q): an open PR run must report a distinct test-merge commit, not the head SHA (ACT-027)", ms, hs)
	}
}

func TestWebhookPing(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var received atomic.Int32
	var mu sync.Mutex
	var lastPayload map[string]interface{}

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastPayload = webhookEventJSON(t, r.Header.Get("Content-Type"), body)
		// Record before signalling: see TestWebhookDeliverySuccess.
		received.Add(1)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-ping")

	// Create webhook
	resp := s.post(t, "/api/v3/repos/admin/wh-ping/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"push"},
		"active": true,
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create hook: expected 201, got %d", resp.StatusCode)
	}
	hookData := decodeJSON(t, resp)
	hookID := int(hookData["id"].(float64))

	// Ping
	pingResp := s.post(t, fmt.Sprintf("/api/v3/repos/admin/wh-ping/hooks/%d/pings", hookID), defaultToken, nil)
	defer pingResp.Body.Close()
	if pingResp.StatusCode != 204 {
		t.Fatalf("ping: expected 204, got %d", pingResp.StatusCode)
	}

	waitForWebhookCount(t, &received, 1)

	mu.Lock()
	defer mu.Unlock()
	if lastPayload["zen"] == nil {
		t.Fatal("ping payload missing 'zen' field")
	}
	if lastPayload["hook_id"] == nil {
		t.Fatal("ping payload missing 'hook_id' field")
	}
}

func TestWebhookDeliveryLog(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var received atomic.Int32

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-log")

	// Create webhook
	resp := s.post(t, "/api/v3/repos/admin/wh-log/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"push"},
		"active": true,
	})
	hookData := decodeJSON(t, resp)
	hookID := int(hookData["id"].(float64))

	// Ping to create a delivery
	pingResp := s.post(t, fmt.Sprintf("/api/v3/repos/admin/wh-log/hooks/%d/pings", hookID), defaultToken, nil)
	pingResp.Body.Close()

	waitForWebhookCount(t, &received, 1)

	// List deliveries. The delivery is recorded (AddDelivery) by the background
	// delivery goroutine AFTER the receiver's POST returns, so waitForWebhookCount
	// (which only observes the receiver) can win the race with the store write —
	// poll the endpoint until the log is populated rather than reading it once.
	var deliveries []map[string]interface{}
	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		delResp := s.get(t, fmt.Sprintf("/api/v3/repos/admin/wh-log/hooks/%d/deliveries", hookID), defaultToken)
		if delResp.StatusCode != 200 {
			delResp.Body.Close()
			t.Fatalf("list deliveries: expected 200, got %d", delResp.StatusCode)
		}
		deliveries = nil
		json.NewDecoder(delResp.Body).Decode(&deliveries)
		delResp.Body.Close()
		return len(deliveries) >= 1
	}) {
		t.Fatal("expected at least 1 delivery in log")
	}

	d := deliveries[0]
	if d["guid"] == nil {
		t.Fatal("delivery missing 'guid' field")
	}
	if d["event"] == nil {
		t.Fatal("delivery missing 'event' field")
	}
	if d["status_code"] == nil {
		t.Fatal("delivery missing 'status_code' field")
	}
}

func TestWebhookInactiveSkipped(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var received atomic.Int32
	delivered := make(chan struct{}, 1)

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		select {
		case delivered <- struct{}{}:
		default:
		}
		w.WriteHeader(200)
	}))
	defer cleanup()

	s.createWebhookTestRepo(t, "wh-inactive")

	// Create inactive webhook
	active := false
	resp := s.post(t, "/api/v3/repos/admin/wh-inactive/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": active,
	})
	resp.Body.Close()

	// Create issue — should NOT trigger inactive webhook
	resp2 := s.post(t, "/api/v3/repos/admin/wh-inactive/issues", defaultToken, map[string]interface{}{
		"title": "should not trigger",
	})
	resp2.Body.Close()

	// An absence assertion needs a bounded observation window, but it does not
	// need a blind sleep: fail immediately if a delivery arrives.
	select {
	case <-delivered:
		t.Fatal("inactive webhook received a delivery")
	case <-time.After(300 * time.Millisecond):
	}

	if received.Load() != 0 {
		t.Fatalf("expected 0 deliveries for inactive webhook, got %d", received.Load())
	}
}

// TestInstallationNodeID verifies the installation node_id is a valid base64
// GraphQL global id that round-trips to "012:Installation{id}".
func TestInstallationNodeID(t *testing.T) {
	for _, id := range []int{1, 42, 9999} {
		got := installationNodeID(id)
		raw, err := base64.StdEncoding.DecodeString(got)
		if err != nil {
			t.Fatalf("id=%d node_id %q is not valid base64: %v", id, got, err)
		}
		want := fmt.Sprintf("012:Installation%d", id)
		if string(raw) != want {
			t.Errorf("id=%d decoded = %q, want %q", id, raw, want)
		}
	}
}

// TestAttachInstallationBlockNodeID confirms the installation block carries the
// valid base64 node_id (not the old malformed concatenated form).
func TestAttachInstallationBlockNodeID(t *testing.T) {
	out := attachInstallationBlock(map[string]interface{}{}, &Installation{ID: 7})
	inst, ok := out["installation"].(map[string]interface{})
	if !ok {
		t.Fatal("installation block missing")
	}
	nodeID, _ := inst["node_id"].(string)
	if _, err := base64.StdEncoding.DecodeString(nodeID); err != nil {
		t.Errorf("installation.node_id %q is not valid base64: %v", nodeID, err)
	}
}

// TestInstallationEventHasNoTopLevelAppID verifies GitHub's installation event
// shape: action/installation/repositories/sender, with NO top-level app_id.
func TestInstallationEventHasNoTopLevelAppID(t *testing.T) {
	app := &App{ID: 99}
	inst := &Installation{ID: 7, AppID: 99}
	payload := buildInstallationEventPayload(app, "created", inst, &User{Login: "octocat", ID: 5, Type: "User"})
	if _, ok := payload["app_id"]; ok {
		t.Error("installation event must NOT have a top-level app_id")
	}
	for _, k := range []string{"action", "installation", "repositories", "sender"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("installation event missing %q", k)
		}
	}
}

// TestSenderPayloadNeverNil verifies a nil originating user yields a populated
// ghost sender object, never JSON null.
func TestSenderPayloadNeverNil(t *testing.T) {
	got := senderPayload(nil)
	if got == nil {
		t.Fatal("senderPayload(nil) returned nil; GitHub guarantees a populated sender")
	}
	if got["login"] != "ghost" {
		t.Errorf("nil sender login = %v, want ghost", got["login"])
	}
	if got["type"] != "User" {
		t.Errorf("nil sender type = %v, want User", got["type"])
	}

	// A real user is rendered faithfully.
	u := senderPayload(&User{Login: "octocat", ID: 5, Type: "User", AvatarURL: "http://a"})
	if u["login"] != "octocat" || u["id"] != 5 {
		t.Errorf("user sender = %v", u)
	}
}

// pushTestCommit creates a commit in-memory and pushes to the bleephub server via go-git.
func (s *isolatedServer) pushTestCommit(t *testing.T, owner, repoName string) {
	t.Helper()

	fs := memfs.New()
	repo, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Create a file and commit
	f, err := fs.Create("test.txt")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	f.Write([]byte("hello webhook"))
	f.Close()

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	wt.Add("test.txt")
	_, err = wt.Commit("test commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  fixedTestTime,
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{s.baseURL + "/" + owner + "/" + repoName + ".git"},
	})
	if err != nil {
		t.Fatalf("create remote: %v", err)
	}

	err = repo.Push(&git.PushOptions{
		Auth:     &githttp.BasicAuth{Username: "x-token", Password: defaultToken},
		RefSpecs: []config.RefSpec{"+refs/heads/master:refs/heads/main"},
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		t.Fatalf("push: %v", err)
	}
}

// Org-owned repos carry a top-level `organization` object on event
// payloads; user-owned repos must not.
func TestPushPayloadCarriesCommitDetails(t *testing.T) {
	s := newTestServer()
	const repoKey = "push-payload/repo"
	sha := commitWorkflowYAMLToStorage(t, s, repoKey, "docs/README.md", "payload")
	repo := s.store.GetRepoByFullName(repoKey)
	payload := buildPushPayload(s.store, repo, s.store.LookupUserByLogin("push-payload"),
		"refs/heads/main", zeroCommitSha, sha)

	if payload["created"] != true || payload["forced"] != false {
		t.Fatalf("push flags = created:%v forced:%v", payload["created"], payload["forced"])
	}
	commits, ok := payload["commits"].([]map[string]interface{})
	if !ok || len(commits) != 1 {
		t.Fatalf("commits = %#v", payload["commits"])
	}
	if commits[0]["id"] != sha || commits[0]["message"] == "" {
		t.Fatalf("commit payload = %#v", commits[0])
	}
	head, ok := payload["head_commit"].(map[string]interface{})
	if !ok || head["id"] != sha {
		t.Fatalf("head_commit = %#v", payload["head_commit"])
	}
	pusher, ok := payload["pusher"].(map[string]interface{})
	if !ok || pusher["name"] != "push-payload" {
		t.Fatalf("pusher = %#v", payload["pusher"])
	}
}

func TestWebhookOrganizationBlock(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	var mu sync.Mutex
	type recvd struct {
		event   string
		payload map[string]interface{}
	}
	var payloads []recvd

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p := webhookEventJSON(t, r.Header.Get("Content-Type"), body)
		mu.Lock()
		payloads = append(payloads, recvd{event: r.Header.Get("X-GitHub-Event"), payload: p})
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	// Org-owned repo: create the org, a repo in it, a hook, and an issue.
	resp := s.post(t, "/api/v3/admin/organizations", defaultToken, map[string]interface{}{
		"login": "wh-org", "admin": "admin",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create org: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.post(t, "/api/v3/orgs/wh-org/repos", defaultToken, map[string]interface{}{"name": "wh-orgrepo"})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create org repo: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.post(t, "/api/v3/repos/wh-org/wh-orgrepo/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": true,
	})
	resp.Body.Close()

	resp = s.post(t, "/api/v3/repos/wh-org/wh-orgrepo/issues", defaultToken, map[string]interface{}{"title": "org evt"})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create issue: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// User-owned repo control: same flow under admin/.
	s.createWebhookTestRepo(t, "wh-userrepo")
	resp = s.post(t, "/api/v3/repos/admin/wh-userrepo/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": true,
	})
	resp.Body.Close()
	resp = s.post(t, "/api/v3/repos/admin/wh-userrepo/issues", defaultToken, map[string]interface{}{"title": "user evt"})
	resp.Body.Close()

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		orgSeen, userSeen := false, false
		for _, record := range payloads {
			if record.event != "issues" {
				continue
			}
			repo, _ := record.payload["repository"].(map[string]interface{})
			switch repo["full_name"] {
			case "wh-org/wh-orgrepo":
				orgSeen = true
			case "admin/wh-userrepo":
				userSeen = true
			}
		}
		return orgSeen && userSeen
	}) {
		t.Fatal("organization and user webhook deliveries did not both arrive")
	}

	mu.Lock()
	defer mu.Unlock()
	var orgSeen, userSeen bool
	for _, rec := range payloads {
		// The organization block is asserted on real repo events; the
		// automatic create-time ping carries no organization member.
		if rec.event != "issues" {
			continue
		}
		p := rec.payload
		repo, _ := p["repository"].(map[string]interface{})
		fullName, _ := repo["full_name"].(string)
		switch fullName {
		case "wh-org/wh-orgrepo":
			orgSeen = true
			orgBlock, ok := p["organization"].(map[string]interface{})
			if !ok {
				t.Errorf("org repo event lacks organization block: %v", p)
				continue
			}
			if orgBlock["login"] != "wh-org" {
				t.Errorf("organization.login = %v, want wh-org", orgBlock["login"])
			}
			if _, ok := orgBlock["node_id"].(string); !ok {
				t.Errorf("organization.node_id missing")
			}
		case "admin/wh-userrepo":
			userSeen = true
			if _, has := p["organization"]; has {
				t.Errorf("user repo event must not carry organization block")
			}
		}
	}
	if !orgSeen || !userSeen {
		t.Fatalf("missing deliveries: orgSeen=%v userSeen=%v (got %d payloads)", orgSeen, userSeen, len(payloads))
	}
}
