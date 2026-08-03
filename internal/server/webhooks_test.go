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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	memfs "github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

func createWebhookTestRepo(t *testing.T, name string) {
	t.Helper()
	resp := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name,
	})
	resp.Body.Close()
}

func waitForWebhookCount(t *testing.T, received *atomic.Int32, want int32) {
	t.Helper()
	if !testEventually(5*time.Second, 10*time.Millisecond, func() bool {
		return received.Load() >= want
	}) {
		t.Fatalf("webhook deliveries = %d, want at least %d", received.Load(), want)
	}
}

func TestWebhookListPagination(t *testing.T) {
	createWebhookTestRepo(t, "wh-paged")

	// An active hook fires a ping on creation; point it at an in-process sink so
	// the unit test makes no real outbound request to example.com.
	sink, cleanup := startWebhookReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()

	for i := 0; i < 2; i++ {
		resp := ghPost(t, "/api/v3/repos/admin/wh-paged/hooks", defaultToken, map[string]interface{}{
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

	resp := ghGet(t, "/api/v3/repos/admin/wh-paged/hooks?per_page=1", defaultToken)
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

	resp = ghGet(t, "/api/v3/repos/admin/wh-paged/hooks?per_page=1&page=2", defaultToken)
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
	createWebhookTestRepo(t, "wh-crud")

	// An active hook fires a ping on creation; point it at an in-process sink so
	// the unit test makes no real outbound request to example.com.
	sink, cleanup := startWebhookReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()

	// Create
	resp := ghPost(t, "/api/v3/repos/admin/wh-crud/hooks", defaultToken, map[string]interface{}{
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
	resp2 := ghGet(t, "/api/v3/repos/admin/wh-crud/hooks", defaultToken)
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
	resp3 := ghGet(t, fmt.Sprintf("/api/v3/repos/admin/wh-crud/hooks/%d", hookID), defaultToken)
	if resp3.StatusCode != 200 {
		resp3.Body.Close()
		t.Fatalf("get: expected 200, got %d", resp3.StatusCode)
	}
	data3 := decodeJSON(t, resp3)
	if int(data3["id"].(float64)) != hookID {
		t.Fatalf("expected id=%d, got %v", hookID, data3["id"])
	}

	// Update
	resp4 := ghPatch(t, fmt.Sprintf("/api/v3/repos/admin/wh-crud/hooks/%d", hookID), defaultToken, map[string]interface{}{
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
	resp5 := ghDelete(t, fmt.Sprintf("/api/v3/repos/admin/wh-crud/hooks/%d", hookID), defaultToken)
	defer resp5.Body.Close()
	if resp5.StatusCode != 204 {
		t.Fatalf("delete: expected 204, got %d", resp5.StatusCode)
	}

	// Verify deleted
	resp6 := ghGet(t, fmt.Sprintf("/api/v3/repos/admin/wh-crud/hooks/%d", hookID), defaultToken)
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

// startWebhookReceiver starts an HTTP server that records received webhook payloads.
func startWebhookReceiver(t *testing.T, handler http.HandlerFunc) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	return "http://" + ln.Addr().String(), func() { srv.Close() }
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
	var received atomic.Int32
	var mu sync.Mutex
	var lastHeaders http.Header
	var lastBody []byte

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastHeaders = r.Header.Clone()
		lastBody = body
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	createWebhookTestRepo(t, "wh-deliver")

	// Create webhook pointing to our receiver
	resp := ghPost(t, "/api/v3/repos/admin/wh-deliver/hooks", defaultToken, map[string]interface{}{
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

	pingResp := ghPost(t, fmt.Sprintf("/api/v3/repos/admin/wh-deliver/hooks/%d/pings", hookID), defaultToken, nil)
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

	createWebhookTestRepo(t, "wh-retry")

	resp := ghPost(t, "/api/v3/repos/admin/wh-retry/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{
			"url": url,
		},
		"events": []string{"issues"},
		"active": true,
	})
	resp.Body.Close()

	// Create an issue to trigger the webhook
	resp2 := ghPost(t, "/api/v3/repos/admin/wh-retry/issues", defaultToken, map[string]interface{}{
		"title": "test issue for retry",
	})
	resp2.Body.Close()

	// Wait for retries (1s + 5s backoff = ~6s, use generous timeout)
	if !testEventually(15*time.Second, 200*time.Millisecond, func() bool { return attempts.Load() >= 3 }) {
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
	var received atomic.Int32
	var mu sync.Mutex
	var lastEvent string
	var lastPayload map[string]interface{}

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastEvent = r.Header.Get("X-GitHub-Event")
		lastPayload = webhookEventJSON(t, r.Header.Get("Content-Type"), body)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	createWebhookTestRepo(t, "wh-push")

	// Create webhook
	resp := ghPost(t, "/api/v3/repos/admin/wh-push/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"push"},
		"active": true,
	})
	resp.Body.Close()

	// Push via git (use go-git)
	pushTestCommit(t, "admin", "wh-push")

	if !testEventually(5*time.Second, 10*time.Millisecond, func() bool {
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

	testEventually(5*time.Second, 20*time.Millisecond, func() bool {
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
	var received atomic.Int32
	var mu sync.Mutex
	var events []string

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		mu.Lock()
		events = append(events, r.Header.Get("X-GitHub-Event"))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	createWebhookTestRepo(t, "wh-pr")
	repo := testServer.store.GetRepo("admin", "wh-pr")
	if repo == nil {
		t.Fatal("repo wh-pr not created")
	}
	seedPullRequestBranches(t, testServer, repo, "feature")

	// Create webhook for pull_request events
	resp := ghPost(t, "/api/v3/repos/admin/wh-pr/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"pull_request"},
		"active": true,
	})
	resp.Body.Close()

	// Create a PR
	resp2 := ghPost(t, "/api/v3/repos/admin/wh-pr/pulls", defaultToken, map[string]interface{}{
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
	resp3 := ghPut(t, fmt.Sprintf("/api/v3/repos/admin/wh-pr/pulls/%d/merge", prNum), defaultToken, nil)
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

	createWebhookTestRepo(t, "wh-issues")

	// Create webhook for issues events
	resp := ghPost(t, "/api/v3/repos/admin/wh-issues/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": true,
	})
	resp.Body.Close()

	// Create issue
	resp2 := ghPost(t, "/api/v3/repos/admin/wh-issues/issues", defaultToken, map[string]interface{}{
		"title": "webhook test issue",
	})
	if resp2.StatusCode != 201 {
		resp2.Body.Close()
		t.Fatalf("create issue: expected 201, got %d", resp2.StatusCode)
	}
	issueData := decodeJSON(t, resp2)
	issueNum := int(issueData["number"].(float64))

	// Close issue
	resp3 := ghPatch(t, fmt.Sprintf("/api/v3/repos/admin/wh-issues/issues/%d", issueNum), defaultToken, map[string]interface{}{
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

	createWebhookTestRepo(t, "wh-issues-gql")
	hookResp := ghPost(t, "/api/v3/repos/admin/wh-issues-gql/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": true,
	})
	hookResp.Body.Close()

	repoData := decodeJSON(t, ghGet(t, "/api/v3/repos/admin/wh-issues-gql", defaultToken))
	repoNodeID, _ := repoData["node_id"].(string)
	if repoNodeID == "" {
		t.Fatal("repo node_id missing")
	}

	created := decodeJSON(t, ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
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

	closeResp := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query":     `mutation($input: CloseIssueInput!){ closeIssue(input:$input){ issue { state } } }`,
		"variables": map[string]interface{}{"input": map[string]interface{}{"issueId": issueNodeID}},
	})
	closeResp.Body.Close()

	if !testEventually(5*time.Second, 10*time.Millisecond, func() bool {
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

	createWebhookTestRepo(t, "wh-label")
	hook := ghPost(t, "/api/v3/repos/admin/wh-label/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"label"},
		"active": true,
	})
	hook.Body.Close()

	created := ghPost(t, "/api/v3/repos/admin/wh-label/labels", defaultToken, map[string]interface{}{
		"name": "triage", "color": "ededed",
	})
	if created.StatusCode != http.StatusCreated {
		created.Body.Close()
		t.Fatalf("create label status = %d", created.StatusCode)
	}
	created.Body.Close()
	del := ghDelete(t, "/api/v3/repos/admin/wh-label/labels/triage", defaultToken)
	del.Body.Close()

	if !testEventually(5*time.Second, 10*time.Millisecond, func() bool {
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

func TestWebhookPing(t *testing.T) {
	var received atomic.Int32
	var mu sync.Mutex
	var lastPayload map[string]interface{}

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastPayload = webhookEventJSON(t, r.Header.Get("Content-Type"), body)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer cleanup()

	createWebhookTestRepo(t, "wh-ping")

	// Create webhook
	resp := ghPost(t, "/api/v3/repos/admin/wh-ping/hooks", defaultToken, map[string]interface{}{
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
	pingResp := ghPost(t, fmt.Sprintf("/api/v3/repos/admin/wh-ping/hooks/%d/pings", hookID), defaultToken, nil)
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
	var received atomic.Int32

	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(200)
	}))
	defer cleanup()

	createWebhookTestRepo(t, "wh-log")

	// Create webhook
	resp := ghPost(t, "/api/v3/repos/admin/wh-log/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"push"},
		"active": true,
	})
	hookData := decodeJSON(t, resp)
	hookID := int(hookData["id"].(float64))

	// Ping to create a delivery
	pingResp := ghPost(t, fmt.Sprintf("/api/v3/repos/admin/wh-log/hooks/%d/pings", hookID), defaultToken, nil)
	pingResp.Body.Close()

	waitForWebhookCount(t, &received, 1)

	// List deliveries
	delResp := ghGet(t, fmt.Sprintf("/api/v3/repos/admin/wh-log/hooks/%d/deliveries", hookID), defaultToken)
	if delResp.StatusCode != 200 {
		delResp.Body.Close()
		t.Fatalf("list deliveries: expected 200, got %d", delResp.StatusCode)
	}
	defer delResp.Body.Close()

	var deliveries []map[string]interface{}
	json.NewDecoder(delResp.Body).Decode(&deliveries)
	if len(deliveries) < 1 {
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

	createWebhookTestRepo(t, "wh-inactive")

	// Create inactive webhook
	active := false
	resp := ghPost(t, "/api/v3/repos/admin/wh-inactive/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": active,
	})
	resp.Body.Close()

	// Create issue — should NOT trigger inactive webhook
	resp2 := ghPost(t, "/api/v3/repos/admin/wh-inactive/issues", defaultToken, map[string]interface{}{
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
func pushTestCommit(t *testing.T, owner, repoName string) {
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
		URLs: []string{testBaseURL + "/" + owner + "/" + repoName + ".git"},
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
	resp := ghPost(t, "/api/v3/admin/organizations", defaultToken, map[string]interface{}{
		"login": "wh-org", "admin": "admin",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create org: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = ghPost(t, "/api/v3/orgs/wh-org/repos", defaultToken, map[string]interface{}{"name": "wh-orgrepo"})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create org repo: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = ghPost(t, "/api/v3/repos/wh-org/wh-orgrepo/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": true,
	})
	resp.Body.Close()

	resp = ghPost(t, "/api/v3/repos/wh-org/wh-orgrepo/issues", defaultToken, map[string]interface{}{"title": "org evt"})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("create issue: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// User-owned repo control: same flow under admin/.
	createWebhookTestRepo(t, "wh-userrepo")
	resp = ghPost(t, "/api/v3/repos/admin/wh-userrepo/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"issues"},
		"active": true,
	})
	resp.Body.Close()
	resp = ghPost(t, "/api/v3/repos/admin/wh-userrepo/issues", defaultToken, map[string]interface{}{"title": "user evt"})
	resp.Body.Close()

	if !testEventually(5*time.Second, 10*time.Millisecond, func() bool {
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
