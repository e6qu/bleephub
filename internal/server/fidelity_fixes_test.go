package bleephub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
)

// TestWebhookFormContentTypeSigning verifies that a content_type=form hook
// (GitHub's default) delivers an x-www-form-urlencoded body with the JSON
// under `payload`, and that X-Hub-Signature-256 is computed over the FORM
// body — not the raw JSON.
func TestWebhookFormContentTypeSigning(t *testing.T) {
	const secret = "s3cr3t"
	var mu sync.Mutex
	var gotCT, gotSig string
	var gotBody []byte

	ln := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotCT = r.Header.Get("Content-Type")
		gotSig = r.Header.Get("X-Hub-Signature-256")
		gotBody = b
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ln.Close()

	s := newTestServer()
	hook := &store.Webhook{
		ID:          1,
		URL:         ln.URL,
		Secret:      secret,
		ContentType: "form",
		Active:      true,
		Events:      []string{"push"},
	}
	payload := []byte(`{"ref":"refs/heads/main"}`)
	s.deliverWebhook(hook, "push", "", payload)

	// deliverWebhook is synchronous; the receiver runs in the test server.
	testutil.TestEventually(2*time.Second, 20*time.Millisecond, func() bool {
		mu.Lock()
		done := gotBody != nil
		mu.Unlock()
		return done
	})

	mu.Lock()
	defer mu.Unlock()
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", gotCT)
	}
	wantBody := url.Values{"payload": {string(payload)}}.Encode()
	if string(gotBody) != wantBody {
		t.Errorf("body = %q, want %q", gotBody, wantBody)
	}
	// Signature must be over the form body, not the JSON.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(wantBody))
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != wantSig {
		t.Errorf("X-Hub-Signature-256 = %q, want %q (signed over form body)", gotSig, wantSig)
	}
	// And it must NOT match the JSON-body signature.
	macJSON := hmac.New(sha256.New, []byte(secret))
	macJSON.Write(payload)
	jsonSig := "sha256=" + hex.EncodeToString(macJSON.Sum(nil))
	if gotSig == jsonSig {
		t.Error("signature was computed over JSON, not the form body")
	}
}

// TestWebhookJSONContentTypeSigning verifies content_type=json still sends
// the raw JSON and signs it.
func TestWebhookJSONContentTypeSigning(t *testing.T) {
	const secret = "abc"
	var mu sync.Mutex
	var gotCT string
	var gotBody []byte

	ln := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotCT = r.Header.Get("Content-Type")
		gotBody = b
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ln.Close()

	s := newTestServer()
	hook := &store.Webhook{ID: 2, URL: ln.URL, Secret: secret, ContentType: "json", Active: true, Events: []string{"push"}}
	payload := []byte(`{"ref":"refs/heads/main"}`)
	s.deliverWebhook(hook, "push", "", payload)

	testutil.TestEventually(2*time.Second, 20*time.Millisecond, func() bool {
		mu.Lock()
		done := gotBody != nil
		mu.Unlock()
		return done
	})

	mu.Lock()
	defer mu.Unlock()
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if string(gotBody) != string(payload) {
		t.Errorf("body = %q, want raw JSON %q", gotBody, payload)
	}
}

// TestJobStatusWaiting verifies a waiting (environment-gated) job reports
// "waiting", not "queued".
func TestJobStatusWaiting(t *testing.T) {
	if got := jobStatus("waiting"); got != "waiting" {
		t.Errorf("jobStatus(waiting) = %q, want waiting", got)
	}
	if got := jobStatus("queued"); got != "queued" {
		t.Errorf("jobStatus(queued) = %q, want queued", got)
	}
}

// TestJobRunAttemptReflectsRun verifies workflowJobJSON emits the run's
// attempt number rather than a hardcoded 1.
func TestJobRunAttemptReflectsRun(t *testing.T) {
	s := newTestServer()
	wf, wfJob := seedRun(t, s, "octo/repo", "running", "")
	wf.Attempt = 3 // force a non-default attempt

	jobJSON := s.workflowJobJSON(wf, wfJob, "http://example.com", "octo/repo")
	if got := jobJSON["run_attempt"]; got != 3 {
		t.Errorf("run_attempt = %v, want 3 (the run's attempt)", got)
	}
}

// TestListArtifactsScopedToRun verifies the v4 ListArtifacts Twirp call
// filters by workflow_run_backend_id so concurrent runs don't see each
// other's artifacts.
func TestListArtifactsScopedToRun(t *testing.T) {
	s := newTestServer()
	s.registerArtifactRoutes()
	token := seedRunJobToken(t, s, "octo/repo", "run-A")
	seedRunJobToken(t, s, "octo/repo", "run-B")

	add := func(id int64, name, backendID string) {
		s.artifactStore.Mu.Lock()
		s.artifactStore.Artifacts[id] = &store.Artifact{
			ID: id, Name: name, Finalized: true,
			WorkflowRunBackendID: backendID, CreatedAt: fixedTestTime,
		}
		s.artifactStore.Mu.Unlock()
	}
	add(1, "calc", "run-A")
	add(2, "other", "run-B")

	req := httptest.NewRequest("POST",
		"/twirp/github.actions.results.api.v1.ArtifactService/ListArtifacts",
		strings.NewReader(`{"workflow_run_backend_id":"run-A"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("ListArtifacts = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Artifacts []struct {
			Name string `json:"name"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0].Name != "calc" {
		t.Errorf("artifacts = %+v, want only run-A's 'calc'", resp.Artifacts)
	}
}
