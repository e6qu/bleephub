package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ssrfTestServer is a unit server with the webhook address gate at its secure
// default, which the shared harness deliberately turns off so its loopback
// hook receivers stay reachable.
func ssrfTestServer() *Server {
	s := newTestServer()
	s.allowPrivateOutboundTargets = false
	return s
}

func TestWebhookAddrBlockedPredicate(t *testing.T) {
	cases := []struct {
		addr    string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"127.13.99.4", true},
		{"::1", true},
		{"169.254.169.254", true}, // cloud instance metadata
		{"169.254.0.1", true},
		{"fe80::1", true},
		{"10.0.0.7", true},
		{"10.255.255.255", true},
		{"172.16.5.4", true},
		{"172.31.255.1", true},
		{"192.168.1.10", true},
		{"fd00::1", true},
		{"fdff:ffff::abcd", true},
		{"fc00::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"255.255.255.255", true},
		{"224.0.0.1", true},
		{"100.64.0.1", true},       // carrier-grade NAT
		{"::ffff:127.0.0.1", true}, // IPv4-mapped loopback
		{"64:ff9b::7f00:1", true},  // NAT64-wrapped loopback
		{"93.184.216.34", false},
		{"8.8.8.8", false},
		{"172.32.0.1", false}, // just outside 172.16/12
		{"2606:2800:220:1:248:1893:25c8:1946", false},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.addr)
		if ip == nil {
			t.Fatalf("test address %q does not parse", tc.addr)
		}
		if got := webhookAddrBlocked(ip); got != tc.blocked {
			t.Errorf("webhookAddrBlocked(%s) = %v, want %v", tc.addr, got, tc.blocked)
		}
	}
	if !webhookAddrBlocked(nil) {
		t.Error("webhookAddrBlocked(nil) = false, want true")
	}
}

func TestWebhookTargetURLValidation(t *testing.T) {
	cases := []struct {
		url      string
		rejected bool
	}{
		{"file:///etc/passwd", true},
		{"gopher://127.0.0.1:11211/_stats", true},
		{"ftp://example.com/hook", true},
		{"redis://example.com:6379", true},
		{"http://", true},
		{"https://", true},
		{"http://127.0.0.1:8080/hook", true},
		{"http://[::1]:8080/hook", true},
		{"http://169.254.169.254/latest/meta-data/", true},
		{"http://10.1.2.3/hook", true},
		{"http://192.168.0.5:9000/hook", true},
		{"http://[fd00::1]/hook", true},
		{"https://example.invalid/hook", false},
		{"http://93.184.216.34/hook", false},
	}
	for _, tc := range cases {
		_, err := parseWebhookTargetURL(tc.url, false)
		if (err != nil) != tc.rejected {
			t.Errorf("parseWebhookTargetURL(%q) error = %v, want rejected=%v", tc.url, err, tc.rejected)
		}
	}

	// A hostname resolving into blocked space is caught by the configuration
	// gate, which resolves before storing.
	if err := validateWebhookTargetURL("http://localhost:9000/hook", false); err == nil {
		t.Error("validateWebhookTargetURL admitted a hostname resolving to loopback")
	}

	// Opting in must reach the addresses, but never change the scheme rule:
	// a non-HTTP scheme is undeliverable regardless of where it points.
	if err := parseWebhookTargetURLErr("http://127.0.0.1:8080/hook", true); err != nil {
		t.Errorf("opted-in loopback target rejected: %v", err)
	}
	if err := parseWebhookTargetURLErr("file:///etc/passwd", true); err == nil {
		t.Error("opting in admitted a file:// webhook target")
	}
}

func parseWebhookTargetURLErr(raw string, allowPrivate bool) error {
	_, err := parseWebhookTargetURL(raw, allowPrivate)
	return err
}

// TestCreateHookRejectsPrivateTarget covers the configuration-time gate on the
// handler itself: a refused target must never be stored.
func TestCreateHookRejectsPrivateTarget(t *testing.T) {
	s := ssrfTestServer()
	owner := s.store.LookupUserByLogin("admin")
	if owner == nil {
		t.Fatal("seeded admin user missing")
	}
	repo := s.store.CreateRepo(owner, "ssrf-config", "", false)
	if repo == nil {
		t.Fatal("could not create the fixture repository")
	}

	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://127.0.0.1:9999/hook",
		"file:///etc/passwd",
	} {
		body := fmt.Sprintf(`{"name":"web","config":{"url":%q},"events":["push"]}`, target)
		req := httptest.NewRequest("POST", "/api/v3/repos/admin/ssrf-config/hooks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("owner", "admin")
		req.SetPathValue("repo", "ssrf-config")
		req = req.WithContext(context.WithValue(req.Context(), ctxUser, owner))
		rec := httptest.NewRecorder()
		s.handleCreateHook(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("POST hook with target %s = %d, want 422", target, rec.Code)
		}
	}
	if hooks := s.store.ListHooks(repo.FullName); len(hooks) != 0 {
		t.Fatalf("%d refused hook(s) were stored anyway", len(hooks))
	}
}

// TestWebhookDeliveryRefusesLoopbackListener is the end-to-end half: a real
// listener on loopback must never receive the delivery.
func TestWebhookDeliveryRefusesLoopbackListener(t *testing.T) {
	var hits int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	// Two spellings of the same destination: the literal address the URL check
	// sees, and a name the resolver turns into it — only the dial-time gate
	// can refuse the second.
	port := receiver.URL[strings.LastIndex(receiver.URL, ":")+1:]
	targets := []string{receiver.URL, "http://localhost:" + port + "/hook"}

	for _, target := range targets {
		s := ssrfTestServer()
		hook := s.store.CreateHook("octo/repo", target, "", "json", "0", []string{"push"}, true)
		s.deliverWebhook(hook, "push", "", []byte(`{"ref":"refs/heads/main"}`))

		deliveries := s.store.ListDeliveries(hook.ID)
		if len(deliveries) == 0 {
			t.Fatalf("target %s: refusal was not recorded as a delivery", target)
		}
		last := deliveries[len(deliveries)-1]
		if last.StatusCode != 0 {
			t.Errorf("target %s: status_code = %d, want 0 (never connected)", target, last.StatusCode)
		}
	}

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("loopback receiver was reached %d time(s); delivery must be refused", got)
	}
}

// TestWebhookDeliveryDoesNotFollowRedirects — a 3xx is the recorded outcome,
// not a hop to a second destination no address check saw.
func TestWebhookDeliveryDoesNotFollowRedirects(t *testing.T) {
	var secondHits int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/next", http.StatusFound)
	}))
	defer first.Close()

	s := newTestServer() // loopback receivers: the address gate is opted out here
	hook := s.store.CreateHook("octo/repo", first.URL, "", "json", "0", []string{"push"}, true)
	delivery := s.doDeliverAttempt(hook, "push", "", "guid-redirect", []byte(`{}`), false)

	if delivery.StatusCode != http.StatusFound {
		t.Errorf("status_code = %d, want %d (the redirect itself)", delivery.StatusCode, http.StatusFound)
	}
	if got := atomic.LoadInt32(&secondHits); got != 0 {
		t.Fatalf("redirect target was reached %d time(s); webhook delivery must not follow redirects", got)
	}
}

// TestWebhookDeliveryPreservesPerHookOrder — deliveries queued for one hook
// arrive in the order they were queued, whatever the pool is doing elsewhere.
func TestWebhookDeliveryPreservesPerHookOrder(t *testing.T) {
	const events = 25

	var mu sync.Mutex
	var seen []int
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Seq int `json:"seq"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &payload)
		mu.Lock()
		seen = append(seen, payload.Seq)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	s := newTestServer()
	hook := s.store.CreateHook("octo/ordered", receiver.URL, "", "json", "0", []string{"push"}, true)
	for i := 0; i < events; i++ {
		s.enqueueWebhookDelivery(hook, "push", "", []byte(fmt.Sprintf(`{"seq":%d}`, i)))
	}

	testEventually(20*time.Second, 20*time.Millisecond, func() bool {
		mu.Lock()
		done := len(seen) >= events
		mu.Unlock()
		return done
	})

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != events {
		t.Fatalf("received %d of %d deliveries", len(seen), events)
	}
	for i, seq := range seen {
		if seq != i {
			t.Fatalf("delivery order = %v, want ascending; first divergence at %d", seen, i)
		}
	}
}

// TestWebhookDeliveryConcurrencyIsBounded — an event storm must not put one
// goroutine per hook per event on the wire.
func TestWebhookDeliveryConcurrencyIsBounded(t *testing.T) {
	const hooks = 64

	var inFlight, peak int64
	release := make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if now <= old || atomic.CompareAndSwapInt64(&peak, old, now) {
				break
			}
		}
		<-release
		atomic.AddInt64(&inFlight, -1)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	s := newTestServer()
	for i := 0; i < hooks; i++ {
		hook := s.store.CreateHook(fmt.Sprintf("octo/bounded-%d", i), receiver.URL, "", "json", "0", []string{"push"}, true)
		s.enqueueWebhookDelivery(hook, "push", "", []byte(`{}`))
	}

	// Let the pool saturate, then let every held request finish.
	testEventually(5*time.Second, 10*time.Millisecond, func() bool {
		return atomic.LoadInt64(&peak) >= webhookDeliveryWorkers
	})
	got := atomic.LoadInt64(&peak)
	close(release)

	if got > webhookDeliveryWorkers {
		t.Fatalf("peak concurrent deliveries = %d, want at most %d", got, webhookDeliveryWorkers)
	}
	if got == 0 {
		t.Fatal("no delivery reached the receiver")
	}
}

// TestRepoHookRoutesRefuseAnUnresolvedRepository — the route gate compares the
// caller against the repository the path resolves to, so a path naming one
// that does not exist must be refused by the handler rather than acted on.
func TestRepoHookRoutesRefuseAnUnresolvedRepository(t *testing.T) {
	store := testServer.store
	now := fixedTestTime
	store.mu.Lock()
	stranger := &User{ID: store.NextUser, Login: "ssrf-stranger", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[stranger.ID] = stranger
	store.UsersByLogin[stranger.Login] = stranger
	store.NextUser++
	store.mu.Unlock()
	token := store.CreateToken(stranger.ID, "repo, workflow, read:org, admin:org, gist")
	if token == nil {
		t.Fatal("could not mint a token for the unrelated user")
	}

	const path = "/api/v3/repos/ssrf-no-such-owner/ssrf-no-such-repo/hooks"
	resp := ghPost(t, path, token.Value, map[string]interface{}{
		"name":   "web",
		"config": map[string]string{"url": "http://169.254.169.254/latest/meta-data/"},
		"events": []string{"push"},
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s = %d (%s), want 404", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if hooks := store.ListHooks("ssrf-no-such-owner/ssrf-no-such-repo"); len(hooks) != 0 {
		t.Fatalf("%d hook(s) stored against a repository that does not exist", len(hooks))
	}

	listResp := ghGet(t, path, token.Value)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET %s = %d, want 404", path, listResp.StatusCode)
	}
}

// TestDependabotSecretsRefuseAnUnresolvedRepository — same gap on the
// Dependabot secrets surface: the public-key endpoints answered without ever
// looking at the repository or organization the path named.
func TestDependabotSecretsRefuseAnUnresolvedRepository(t *testing.T) {
	store := testServer.store
	now := fixedTestTime
	store.mu.Lock()
	stranger := &User{ID: store.NextUser, Login: "ssrf-dependabot-stranger", Type: "User", CreatedAt: now, UpdatedAt: now}
	store.Users[stranger.ID] = stranger
	store.UsersByLogin[stranger.Login] = stranger
	store.NextUser++
	store.mu.Unlock()
	token := store.CreateToken(stranger.ID, "repo, workflow, read:org, admin:org, gist")
	if token == nil {
		t.Fatal("could not mint a token for the unrelated user")
	}

	for _, path := range []string{
		"/api/v3/repos/ssrf-no-such-owner/ssrf-no-such-repo/dependabot/secrets/public-key",
		"/api/v3/orgs/ssrf-no-such-org/dependabot/secrets/public-key",
	} {
		resp := ghGet(t, path, token.Value)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d (%s), want 404", path, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
}
