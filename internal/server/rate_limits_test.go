package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

var testRateLimitReset = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

func TestAuthFlowRateLimitBlocksBruteForcePerIP(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	handler := server.rateLimitAuthFlow(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	call := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/auth/local", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	// The per-IP budget admits a human's attempts, then refuses the rest.
	for i := 0; i < authFlowRateLimit; i++ {
		if rec := call("203.0.113.5:44444"); rec.Code != http.StatusOK {
			t.Fatalf("attempt %d within budget got %d, want 200", i+1, rec.Code)
		}
	}
	over := call("203.0.113.5:55555")
	if over.Code != http.StatusForbidden {
		t.Fatalf("over-budget attempt got %d, want 403", over.Code)
	}
	if over.Header().Get("Retry-After") == "" {
		t.Fatal("over-budget refusal is missing a Retry-After header")
	}

	// The budget is per client IP: a different address is unaffected.
	if rec := call("203.0.113.9:1"); rec.Code != http.StatusOK {
		t.Fatalf("distinct IP got %d, want 200 (budget must be per-IP)", rec.Code)
	}

	// A direct loopback peer with no forwarded client (local binary / dev /
	// e2e) is exempt: it never throttles however many attempts it makes.
	for i := 0; i < authFlowRateLimit+5; i++ {
		if rec := call("127.0.0.1:9999"); rec.Code != http.StatusOK {
			t.Fatalf("loopback attempt %d got %d, want 200 (must be exempt)", i+1, rec.Code)
		}
	}

	// But a loopback peer that carries a forwarded client — the reverse proxy
	// in production — is limited, keyed by that forwarded client.
	proxied := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/auth/local", nil)
		req.RemoteAddr = "127.0.0.1:1" // the proxy
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	for i := 0; i < authFlowRateLimit; i++ {
		if rec := proxied(); rec.Code != http.StatusOK {
			t.Fatalf("proxied attempt %d got %d, want 200", i+1, rec.Code)
		}
	}
	if rec := proxied(); rec.Code != http.StatusForbidden {
		t.Fatalf("proxied over-budget got %d, want 403 (forwarded client must be limited)", rec.Code)
	}
}

func TestPrimaryRateLimitsAreStatefulAndCredentialScoped(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	request := httptest.NewRequest("GET", "/api/v3/user", nil)
	request.Header.Set("Authorization", "Bearer credential-one")

	first := server.rateLimitSnapshot(request, "core", true)
	second := server.rateLimitSnapshot(request, "core", true)
	if first.Used != 1 || first.Remaining != 4999 {
		t.Fatalf("first core snapshot = %+v", first)
	}
	if second.Used != 2 || second.Remaining != 4998 || second.Reset != first.Reset {
		t.Fatalf("second core snapshot = %+v; first = %+v", second, first)
	}

	other := httptest.NewRequest("GET", "/api/v3/user", nil)
	other.Header.Set("Authorization", "Bearer credential-two")
	separate := server.rateLimitSnapshot(other, "core", true)
	if separate.Used != 1 {
		t.Fatalf("a different credential shared rate state: %+v", separate)
	}
}

func TestPrimaryRateIdentityCannotBeBypassedByChangingAuthScheme(t *testing.T) {
	bearer := httptest.NewRequest("GET", "/api/v3/user", nil)
	bearer.Header.Set("Authorization", "Bearer shared-credential")
	classic := httptest.NewRequest("GET", "/api/v3/user", nil)
	classic.Header.Set("Authorization", "TOKEN shared-credential")
	if apiRateIdentity(bearer) != apiRateIdentity(classic) {
		t.Fatal("Bearer and token schemes produced separate budgets for one credential")
	}
}

func TestPrimaryRateLimitResourceClassification(t *testing.T) {
	cases := map[string]string{
		"/api/graphql":                                         "graphql",
		"/api/v3/search/code?q=hello":                          "code_search",
		"/api/v3/search/repositories?q=hello":                  "search",
		"/api/v3/repos/o/r/actions/runners/registration-token": "actions_runner_registration",
		"/api/v3/repos/o/r/dependency-graph/snapshots":         "dependency_snapshots",
		"/api/v3/repos/o/r/code-scanning/sarifs":               "code_scanning_upload",
		"/api/v3/repos/o/r/code-scanning/alerts/1/autofix":     "code_scanning_autofix",
		"/api/v3/scim/v2/enterprises/e/Users":                  "scim",
		"/api/v3/app-manifests/code/conversions":               "integration_manifest",
		"/api/v3/user":                                         "core",
	}
	for path, want := range cases {
		if got := apiRateResource(path); got != want {
			t.Errorf("apiRateResource(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPrimaryRateLimitRejectsOnlyRequestsBeyondTheBudget(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	request := httptest.NewRequest("GET", "/api/v3/user", nil)
	request.Header.Set("Authorization", "Bearer exhausted-credential")
	key := apiRateIdentity(request) + "\x1fcore"
	server.rateLimits[key] = &apiRateWindow{
		Limit: 2,
		Used:  1,
		Reset: testRateLimitReset,
	}

	lastAllowed := server.rateLimitSnapshot(request, "core", true)
	if lastAllowed.Exceeded || lastAllowed.Used != 2 || lastAllowed.Remaining != 0 {
		t.Fatalf("last allowed request = %+v", lastAllowed)
	}
	rejected := server.rateLimitSnapshot(request, "core", true)
	if !rejected.Exceeded || rejected.Used != 2 || rejected.Remaining != 0 {
		t.Fatalf("request past the budget = %+v", rejected)
	}
}

func TestRateLimitMiddlewareReturnsGitHubShaped403(t *testing.T) {
	server := newTestServer()
	server.rateLimits = map[string]*apiRateWindow{}
	request := httptest.NewRequest("GET", "/api/v3/user", nil)
	request.Header.Set("Authorization", "Bearer "+defaultToken)
	key := apiRateIdentity(request) + "\x1fcore"
	server.rateLimits[key] = &apiRateWindow{
		Limit: 1,
		Used:  1,
		Reset: testRateLimitReset,
	}
	recorder := httptest.NewRecorder()
	server.ghHeadersMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("rate-limited request reached the handler")
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-RateLimit-Remaining") != "0" || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("rate-limit headers = %v", recorder.Header())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"message":"API rate limit exceeded"`)) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestUnauthenticatedCoreRateLimitIsSixty(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	request := httptest.NewRequest("GET", "/api/v3/users/octocat", nil)
	got := server.rateLimitSnapshot(request, "core", true)
	if got.Limit != 60 || got.Remaining != 59 {
		t.Fatalf("anonymous core snapshot = %+v", got)
	}
}

// TestUnauthenticatedSearchRateLimitIsTen pins GitHub's anonymous Search budget
// (10/minute) versus the authenticated 30 — the anonymous downgrade applies to
// search, not just core. (code_search is already 10 for everyone.)
func TestUnauthenticatedSearchRateLimitIsTen(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	anon := httptest.NewRequest("GET", "/api/v3/search/repositories?q=x", nil)
	if got := server.rateLimitSnapshot(anon, "search", true); got.Limit != 10 {
		t.Errorf("anonymous search limit = %d, want 10", got.Limit)
	}
	authed := httptest.NewRequest("GET", "/api/v3/search/repositories?q=x", nil)
	authed.Header.Set("Authorization", "Bearer "+defaultToken)
	if got := server.rateLimitSnapshot(authed, "search", true); got.Limit != 30 {
		t.Errorf("authenticated search limit = %d, want 30", got.Limit)
	}
}

func TestAnonymousRateBucketUsesForwardedClientBehindPrivateProxy(t *testing.T) {
	makeRequest := func(forwardedFor string) *http.Request {
		request := httptest.NewRequest("GET", "/api/v3/users/octocat", nil)
		request.RemoteAddr = "172.18.0.2:39104" // the reverse proxy on the container network
		request.Header.Set("X-Forwarded-For", forwardedFor)
		return request
	}
	clientA := apiRateIdentity(makeRequest("203.0.113.7"))
	clientB := apiRateIdentity(makeRequest("203.0.113.8"))
	if clientA == clientB {
		t.Fatal("distinct forwarded clients behind the proxy shared one anonymous bucket")
	}
	if clientA != "anonymous:203.0.113.7" {
		t.Fatalf("forwarded client identity = %q, want anonymous:203.0.113.7", clientA)
	}
	if again := apiRateIdentity(makeRequest("203.0.113.7")); again != clientA {
		t.Fatalf("same forwarded client resolved to a different bucket: %q vs %q", again, clientA)
	}
}

func TestAnonymousRateBucketIgnoresForwardedHeaderFromPublicPeer(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v3/users/octocat", nil)
	request.RemoteAddr = "198.51.100.9:50000" // a direct public peer, not our proxy
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := apiRateIdentity(request); got != "anonymous:198.51.100.9" {
		t.Fatalf("public peer identity = %q, want anonymous:198.51.100.9 (spoofable header must not mint budgets)", got)
	}
}

// TestBrowserSessionDoesNotConsumeCoreBudget pins the first-party UI
// exemption: a request authenticated by the browser session cookie (no
// Authorization header, principal resolved from the session) observes the
// authenticated core window read-only instead of spending from it — GitHub's
// own web UI does not bill page loads against API quota, and the SPA fires
// 16-23 calls per page.
func TestBrowserSessionDoesNotConsumeCoreBudget(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	user := &store.User{ID: 42, Login: "browser-user"}
	sessionRequest := func(target string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		return req.WithContext(contextWithUser(req.Context(), user))
	}

	// A whole page-crawl's worth of session calls leaves core untouched, with
	// honest headers: the authenticated limit, remaining pinned.
	for i := 0; i < 100; i++ {
		got := server.rateLimitSnapshot(sessionRequest("/api/v3/user"), "core", true)
		if got.Limit != 5000 || got.Used != 0 || got.Remaining != 5000 || got.Exceeded {
			t.Fatalf("session core snapshot after %d calls = %+v, want read-only 5000 window", i+1, got)
		}
	}

	// A 304 refund for a session request must not mint a unit that was never
	// consumed.
	refunded := server.refundRateLimit(sessionRequest("/api/v3/user"), "core")
	if refunded.Used != 0 || refunded.Remaining != 5000 {
		t.Fatalf("session core refund = %+v, want untouched window", refunded)
	}

	// /rate_limit for the session reflects the non-consumption.
	recorder := httptest.NewRecorder()
	server.handleGHRateLimit(recorder, sessionRequest("/api/v3/rate_limit"))
	if recorder.Code != 200 {
		t.Fatalf("rate_limit status = %d", recorder.Code)
	}
	var rateLimitBody struct {
		Rate struct {
			Limit     int `json:"limit"`
			Used      int `json:"used"`
			Remaining int `json:"remaining"`
		} `json:"rate"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &rateLimitBody); err != nil {
		t.Fatalf("decode /rate_limit body: %v", err)
	}
	if rateLimitBody.Rate.Limit != 5000 || rateLimitBody.Rate.Used != 0 || rateLimitBody.Rate.Remaining != 5000 {
		t.Fatalf("session /rate_limit core = %+v, want untouched 5000 window", rateLimitBody.Rate)
	}

	// Non-core session budgets guard expensive scans and still bill: search
	// consumes down to its authenticated limit and then refuses.
	for i := 0; i < 30; i++ {
		got := server.rateLimitSnapshot(sessionRequest("/api/v3/search/repositories?q=x"), "search", true)
		if got.Limit != 30 || got.Used != i+1 || got.Exceeded {
			t.Fatalf("session search snapshot %d = %+v", i+1, got)
		}
	}
	if got := server.rateLimitSnapshot(sessionRequest("/api/v3/search/repositories?q=x"), "search", true); !got.Exceeded {
		t.Fatalf("session search past the budget = %+v, want Exceeded", got)
	}
}

// TestBrowserSessionCoreExemptionDoesNotLeakToOtherCallers pins that the
// exemption is scoped to the session-cookie branch: a PAT presented by the
// same user and an anonymous caller both keep consuming core exactly as
// before.
func TestBrowserSessionCoreExemptionDoesNotLeakToOtherCallers(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	user := &store.User{ID: 42, Login: "browser-user"}

	// A token-authenticated request from the same principal still bills its
	// credential's window — even with the session user in context.
	pat := httptest.NewRequest(http.MethodGet, "/api/v3/user", nil)
	pat.Header.Set("Authorization", "token some-pat")
	pat = pat.WithContext(contextWithUser(pat.Context(), user))
	if got := server.rateLimitSnapshot(pat, "core", true); got.Used != 1 || got.Remaining != 4999 {
		t.Fatalf("PAT core snapshot = %+v, want consumed authenticated window", got)
	}

	// Anonymous callers keep the small IP-scoped consuming budget.
	anon := httptest.NewRequest(http.MethodGet, "/api/v3/users/octocat", nil)
	if got := server.rateLimitSnapshot(anon, "core", true); got.Limit != 60 || got.Used != 1 {
		t.Fatalf("anonymous core snapshot = %+v, want consumed 60 window", got)
	}
}

func TestRateLimitResponseContainsEveryDocumentedResource(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	request := httptest.NewRequest("GET", "/api/v3/rate_limit", nil)
	recorder := httptest.NewRecorder()
	server.handleGHRateLimit(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, resource := range apiRateResponseResources {
		if !bytes.Contains(recorder.Body.Bytes(), []byte(`"`+resource+`"`)) {
			t.Errorf("rate-limit response omitted %q", resource)
		}
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte(`"code_scanning_upload"`)) {
		t.Error("rate-limit response exposed a resource absent from GitHub's response schema")
	}
}
